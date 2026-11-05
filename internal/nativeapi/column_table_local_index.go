package nativeapi

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"path"
	"slices"

	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	ydb "github.com/ydb-platform/ydb-go-sdk/v3"
	"google.golang.org/protobuf/proto"

	native "github.com/stokaro/ptah-ydb-plugin/internal/nativeprotocol"
	"github.com/stokaro/ptah-ydb-plugin/internal/tablecreate"
	"github.com/stokaro/ptah-ydb-plugin/schema"
)

// ColumnTableLocalIndex owns the precondition read, the single dispatch and
// the confirmation of one step of a change of a column table's local indexes.
type ColumnTableLocalIndex struct {
	// Open starts the table session the reads use.
	Open func(context.Context) (ColumnDescriber, error)
	// Execute runs one schema statement through the query service, once.
	Execute func(context.Context, string) error
	// Monitoring reads the monitoring description of the column table at an
	// absolute path; nil for a session whose address names no endpoint.
	Monitoring func(context.Context, string) ([]byte, error)
}

// NewColumnTableLocalIndex binds the native table client of the session's
// driver, the statement runner and the monitoring reader.
func NewColumnTableLocalIndex(driver *ydb.Driver, execute func(context.Context, string) error, monitoring func(context.Context, string) ([]byte, error)) *ColumnTableLocalIndex {
	return &ColumnTableLocalIndex{Execute: execute, Monitoring: monitoring,
		Open: func(ctx context.Context) (ColumnDescriber, error) { return OpenTable(ctx, driver) }}
}

// Apply reads the column table, requires it to be in the state the step was
// planned against, runs the step's ALTER TABLE once and reads the table back.
// A failure before dispatch leaves the table untouched. A failure after
// dispatch is uncertain and is never retried.
func (c *ColumnTableLocalIndex) Apply(ctx context.Context, database string, value *protocol.Envelope, beforeWrite func(context.Context) error) (dispatched bool, err error) {
	alteration, err := tablecreate.ColumnLocalIndexAlteration(value)
	if err != nil {
		return false, err
	}
	if c.Monitoring == nil {
		return false, ErrNoMonitoring
	}
	owner := proto.CloneOf(value.Owner)
	if owner.Database != "" && owner.Database != database {
		return false, fmt.Errorf("YDB column table local-index change targets another database")
	}
	owner.Database = database
	bound := proto.CloneOf(value)
	bound.Owner = owner
	if _, err := tablecreate.NormalizeColumnLocalIndex(&protocol.ValueInput{Id: "execute", Representation: protocol.ValueInput_OPERATION, Value: bound}); err != nil {
		return false, err
	}
	session, err := c.Open(ctx)
	if err != nil {
		return false, err
	}
	defer func() { err = errors.Join(err, session.Close()) }()
	absolute := path.Join(database, owner.Schema, owner.Name)
	described, before, err := readColumnTable(ctx, session, c.Monitoring, absolute)
	if err != nil {
		return false, err
	}
	if err := checkLocalIndexPrecondition(alteration, described, before); err != nil {
		return false, err
	}
	if err := beforeWrite(ctx); err != nil {
		return false, err
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if err := c.Execute(ctx, tablecreate.ColumnLocalIndexStatement(absolute, alteration)); err != nil {
		return true, err
	}
	_, after, err := readColumnTable(ctx, session, c.Monitoring, absolute)
	if err != nil {
		return true, err
	}
	return true, confirmLocalIndex(alteration, before, after)
}

// checkLocalIndexPrecondition requires the table to be the one the step was
// planned against. An addition needs the index absent and its column held,
// Utf8 for an n-gram index; a removal needs the table to hold the index as the
// plan captured it, and a rename that and no index under the new name, which
// the server refuses only after the call (`Index ... exists, but overwrite
// flag has not been set`). The server refuses an absent column or a wrong
// type only after the call too, which would leave the outcome uncertain.
func checkLocalIndexPrecondition(alteration *native.ColumnTableLocalIndexAlteration, described *native.ColumnTableDescribeResult, observed *native.ColumnStorageObservation) error {
	held := localIndexNamed(observed.LocalIndexes, alteration.GetDrop().GetName())
	if drop := alteration.GetDrop(); drop != nil {
		if held == nil || !tablecreate.LocalIndexEqual(drop, held) {
			return fmt.Errorf("YDB column table local index %q differs from the plan", drop.GetName())
		}
		if add := alteration.GetAdd(); add != nil && localIndexNamed(observed.LocalIndexes, add.GetName()) != nil {
			return fmt.Errorf("YDB column table already holds local index %q", add.GetName())
		}
		return nil
	}
	add := alteration.GetAdd()
	if localIndexNamed(observed.LocalIndexes, add.GetName()) != nil {
		return fmt.Errorf("YDB column table already holds local index %q", add.GetName())
	}
	column := add.GetColumns()[0]
	at := slices.IndexFunc(described.Columns, func(held *native.ColumnMeta) bool { return held.GetName() == column })
	if at < 0 {
		return fmt.Errorf("YDB local index column %q is absent", column)
	}
	kind, _, err := schema.ColumnType(described.Columns[at].Type)
	if err != nil {
		return err
	}
	if add.GetNgram() != nil && kind != "Utf8" {
		return fmt.Errorf("YDB n-gram index column %q must be Utf8", column)
	}
	return nil
}

// confirmLocalIndex requires the table read back after the step to hold the
// index it added as requested and no longer to hold the one it dropped, a
// renamed index both, with every other local index, the layout and the TTL as
// they were.
func confirmLocalIndex(alteration *native.ColumnTableLocalIndexAlteration, before, after *native.ColumnStorageObservation) error {
	added, dropped := alteration.GetAdd().GetName(), alteration.GetDrop().GetName()
	if add := alteration.GetAdd(); add != nil {
		if held := localIndexNamed(after.LocalIndexes, added); held == nil || !tablecreate.LocalIndexEqual(add, held) {
			return fmt.Errorf("YDB column table local index %q differs from the request after the change", added)
		}
	}
	if alteration.GetDrop() != nil && localIndexNamed(after.LocalIndexes, dropped) != nil {
		return fmt.Errorf("YDB column table still holds local index %q after the change", dropped)
	}
	name := cmp.Or(dropped, added)
	others := func(indexes []*native.LocalIndexObservation) []*native.LocalIndexObservation {
		return slices.DeleteFunc(slices.Clone(indexes), func(index *native.LocalIndexObservation) bool {
			return index.GetName() == added || index.GetName() == dropped
		})
	}
	if !sameLocalIndexes(others(before.LocalIndexes), others(after.LocalIndexes)) || !slices.Equal(before.HashColumns, after.HashColumns) ||
		before.Partitions != after.Partitions || !tablecreate.ColumnTTLEqual(before.Ttl, after.Ttl) {
		return fmt.Errorf("YDB column table changed beyond local index %q", name)
	}
	return nil
}

// localIndexNamed is the local index named name, or nil.
func localIndexNamed(indexes []*native.LocalIndexObservation, name string) *native.LocalIndexObservation {
	at := slices.IndexFunc(indexes, func(index *native.LocalIndexObservation) bool { return index.GetName() == name })
	if name == "" || at < 0 {
		return nil
	}
	return indexes[at]
}
