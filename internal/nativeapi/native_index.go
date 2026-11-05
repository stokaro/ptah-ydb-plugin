package nativeapi

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"path"
	"slices"
	"time"

	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	ydb "github.com/ydb-platform/ydb-go-sdk/v3"
	"google.golang.org/protobuf/proto"

	native "github.com/stokaro/ptah-ydb-plugin/internal/nativeprotocol"
	"github.com/stokaro/ptah-ydb-plugin/internal/tablecreate"
	"github.com/stokaro/ptah-ydb-plugin/schema"
)

// NativeIndexReader reads a row table from the table service.
type NativeIndexReader interface {
	Describe(context.Context, string) (*native.DescribeTableResult, error)
	Close() error
}

// NativeIndexAddition owns the precondition read, the single dispatch of the
// YQL statement that adds one vector or full-text index, and its
// confirmation. The native AlterTable call of the pinned bindings carries
// neither kind, so the statement is YQL's ALTER TABLE ... ADD INDEX, as the
// host's embedded engine writes it.
type NativeIndexAddition struct {
	// Open starts the table session the precondition and the confirmation read with.
	Open func(context.Context) (NativeIndexReader, error)
	// Execute runs one schema statement through the query service, once.
	Execute func(context.Context, string) error
	// Poll is how long Apply waits between reads while the index is being
	// built. Zero means one second.
	Poll time.Duration
	// Timeout bounds the wait after the statement for the index to read back
	// as ready. Zero means ten minutes.
	Timeout time.Duration
}

// NewNativeIndexAddition binds the native table client of the session's
// driver and the statement runner.
func NewNativeIndexAddition(driver *ydb.Driver, execute func(context.Context, string) error) *NativeIndexAddition {
	return &NativeIndexAddition{Execute: execute, Open: func(ctx context.Context) (NativeIndexReader, error) { return OpenTable(ctx, driver) }}
}

// Apply reads the table, requires it to hold no index of the name and every
// column the index reads, runs the statement once and reads the table back
// until the index is ready. A failure before dispatch leaves the table
// untouched. A failure after dispatch is uncertain and is never retried.
func (a *NativeIndexAddition) Apply(ctx context.Context, database string, value *protocol.Envelope, beforeWrite func(context.Context) error) (dispatched bool, err error) {
	addition, err := tablecreate.DecodeNativeIndex(value)
	if err != nil {
		return false, err
	}
	owner := proto.CloneOf(value.Owner)
	if owner.Database != "" && owner.Database != database {
		return false, fmt.Errorf("YDB index addition targets another database")
	}
	owner.Database = database
	bound := proto.CloneOf(value)
	bound.Owner = owner
	if _, err := tablecreate.NormalizeNativeIndex(&protocol.ValueInput{Id: "execute", Representation: protocol.ValueInput_OPERATION, Value: bound}); err != nil {
		return false, err
	}
	session, err := a.Open(ctx)
	if err != nil {
		return false, err
	}
	defer func() { err = errors.Join(err, session.Close()) }()
	target := path.Join(database, owner.Schema, owner.Name)
	before, err := session.Describe(ctx, target)
	if err != nil {
		return false, err
	}
	if err := holdsRowTable(owner, before); err != nil {
		return false, err
	}
	index := addition.Index
	if indexByName(before.Indexes, index.GetName()) != nil {
		return false, fmt.Errorf("YDB index to add already exists")
	}
	for _, name := range slices.Concat(index.GetIndexColumns(), index.GetDataColumns()) {
		if !slices.ContainsFunc(before.Columns, func(column *native.ColumnMeta) bool { return column.GetName() == name }) {
			return false, fmt.Errorf("YDB index to add reads the absent column %q", name)
		}
	}
	if err := beforeWrite(ctx); err != nil {
		return false, err
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if err := a.Execute(ctx, tablecreate.NativeIndexStatement(target, addition)); err != nil {
		return true, err
	}
	observed, err := a.await(ctx, session, target, index.GetName())
	if err != nil {
		return true, err
	}
	return true, confirmNativeIndex(owner, index, before, observed)
}

// await reads the table until the index is ready, gone, the timeout passes or
// the context ends, and returns the last reading. YDB answers the statement
// once the build ends; the reads cover a server that answers earlier.
func (a *NativeIndexAddition) await(ctx context.Context, session NativeIndexReader, target, name string) (*native.DescribeTableResult, error) {
	deadline := time.NewTimer(cmp.Or(a.Timeout, 10*time.Minute))
	defer deadline.Stop()
	for {
		observed, err := session.Describe(ctx, target)
		if err != nil {
			return nil, err
		}
		index := indexByName(observed.Indexes, name)
		if index == nil || index.Status == native.TableIndexDescription_STATUS_READY {
			return observed, nil
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-deadline.C:
			return observed, nil
		case <-time.After(cmp.Or(a.Poll, time.Second)):
		}
	}
}

// confirmNativeIndex requires the index read back to exist, be ready and hold
// the definition the addition states, compared as the plan compares it (see
// tablecreate.SameNativeIndex), and every other index, every column and the
// key to be as they were.
func confirmNativeIndex(owner *protocol.Reference, index *native.IndexIntent, before, observed *native.DescribeTableResult) error {
	if err := holdsRowTable(owner, observed); err != nil {
		return err
	}
	if !slices.Equal(before.PrimaryKey, observed.PrimaryKey) || len(before.Columns) != len(observed.Columns) {
		return fmt.Errorf("YDB table key or columns changed with the index addition")
	}
	for i, column := range before.Columns {
		if !proto.Equal(column, observed.Columns[i]) {
			return fmt.Errorf("YDB table columns changed with the index addition")
		}
	}
	if len(observed.Indexes) != len(before.Indexes)+1 {
		return fmt.Errorf("YDB altered table holds unexpected indexes")
	}
	for _, held := range before.Indexes {
		if actual := indexByName(observed.Indexes, held.Name); actual == nil || !sameIndex(held, actual) {
			return fmt.Errorf("YDB altered table changed the index %q", held.Name)
		}
	}
	actual := indexByName(observed.Indexes, index.GetName())
	if actual == nil || actual.Status != native.TableIndexDescription_STATUS_READY || !tablecreate.SameNativeIndex(index, schema.IndexSpecification(actual)) {
		return fmt.Errorf("YDB added index differs from the request")
	}
	return nil
}
