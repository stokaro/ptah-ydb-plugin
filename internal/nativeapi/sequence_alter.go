package nativeapi

import (
	"context"
	"errors"
	"fmt"
	"path"
	"slices"

	native "github.com/stokaro/ptah-ydb-plugin/internal/nativeprotocol"
	"github.com/stokaro/ptah-ydb-plugin/internal/tablecreate"
	"github.com/stokaro/ptah-ydb-plugin/internal/ydbsequence"
	"github.com/stokaro/ptah-ydb-plugin/internal/ydbtype"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	ydb "github.com/ydb-platform/ydb-go-sdk/v3"
	"google.golang.org/protobuf/proto"
)

// SerialSequence changes the start and the increment of the sequence behind a
// Serial column of an existing row table, after checking the column the plan
// read, with one YQL ALTER SEQUENCE.
//
// The table service has no call that changes a sequence: measured on 25.1.4.7
// and 26.2.1.14, an AlterTable that names the column's sequence answers
// SCHEME_ERROR, resolving the sequence's name beside the table rather than in
// it. ALTER SEQUENCE names the sequence by its absolute path.
type SerialSequence struct {
	Open    func(context.Context) (DefaultSession, error)
	Execute func(context.Context, string) error
}

// NewSerialSequence binds table reads and one-shot YQL execution.
func NewSerialSequence(driver *ydb.Driver, execute func(context.Context, string) error) *SerialSequence {
	return &SerialSequence{Execute: execute, Open: func(ctx context.Context) (DefaultSession, error) { return OpenTable(ctx, driver) }}
}

// Apply refuses stale state before dispatch and never retries an uncertain
// write. capabilities are the session's: a 16-bit or 32-bit Serial's sequence
// changes only where the target keeps its range.
func (s *SerialSequence) Apply(ctx context.Context, database string, value *protocol.Envelope, capabilities map[string]bool,
	beforeWrite func(context.Context) error) (dispatched bool, err error) {
	change, err := tablecreate.DecodeSequence(value)
	if err != nil {
		return false, err
	}
	owner := proto.CloneOf(value.Owner)
	if owner.Database != "" && owner.Database != database {
		return false, fmt.Errorf("YDB sequence change targets another database")
	}
	owner.Database = database
	bound := proto.CloneOf(value)
	bound.Owner = owner
	if _, err := tablecreate.NormalizeSequence(&protocol.ValueInput{Id: "execute", Representation: protocol.ValueInput_OPERATION, Value: bound}); err != nil {
		return false, err
	}
	if serial := tablecreate.HeldSerialType(change.Held); serial != ydbtype.BigSerial && !capabilities[ydbsequence.KeepsRange] {
		return false, fmt.Errorf("YDB raises the maximum of a %s's sequence on any ALTER SEQUENCE, and this target does not keep it", serial)
	}
	session, err := s.Open(ctx)
	if err != nil {
		return false, err
	}
	defer func() { err = errors.Join(err, session.Close()) }()
	absolute := path.Join(database, owner.Schema, owner.Name)
	before, err := session.Describe(ctx, absolute)
	if err != nil {
		return false, err
	}
	if err := holdsRowTable(owner, before); err != nil {
		return false, err
	}
	at := slices.IndexFunc(before.Columns, func(column *native.ColumnMeta) bool { return column.Name == change.Held.Name })
	if at < 0 {
		return false, fmt.Errorf("YDB sequence before-state differs from the plan")
	}
	matches, err := tablecreate.EqualCapturedColumns(change.Held, before.Columns[at])
	if err != nil {
		return false, err
	}
	if !matches {
		return false, fmt.Errorf("YDB sequence before-state differs from the plan")
	}
	statement, err := tablecreate.SequenceAlterationStatement(database, tablecreate.TextPath(owner.Schema, owner.Name), change)
	if err != nil {
		return false, err
	}
	if err := beforeWrite(ctx); err != nil {
		return false, err
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if err := s.Execute(ctx, statement); err != nil {
		return true, err
	}
	after, err := session.Describe(ctx, absolute)
	if err != nil {
		return true, err
	}
	if err := holdsRowTable(owner, after); err != nil {
		return true, err
	}
	return true, confirmSequenceChange(change, before, after, at)
}

// confirmSequenceChange requires the column read back to hold a sequence with
// the requested start and increment, no restart, and every other setting it
// held, while the key and every other column stay as they were. The maximum
// may move: YDB raises a 16-bit or 32-bit Serial's to the Int64 maximum on any
// ALTER SEQUENCE, which only a target that keeps the range reaches here.
func confirmSequenceChange(change *tablecreate.SequenceAlteration, before, after *native.DescribeTableResult, at int) error {
	if len(before.Columns) != len(after.Columns) || !slices.Equal(before.PrimaryKey, after.PrimaryKey) {
		return fmt.Errorf("YDB column inventory or key changed during the sequence change")
	}
	for i, column := range before.Columns {
		actual := proto.CloneOf(after.Columns[i])
		if i == at {
			sequence := actual.GetFromSequence()
			if sequence == nil || sequence.GetSetVal() != nil || sequence.GetStartValue() != change.Settings.Start || sequence.GetIncrement() != change.Settings.Increment {
				return fmt.Errorf("YDB sequence differs from the request after the change")
			}
			held := proto.CloneOf(column.GetFromSequence())
			held.StartValue, held.Increment, held.MaxValue = sequence.StartValue, sequence.Increment, sequence.MaxValue
			if !proto.Equal(held, sequence) {
				return fmt.Errorf("YDB sequence changed outside the requested start and increment")
			}
			actual.DefaultValue = column.DefaultValue
		}
		if !proto.Equal(actual, column) {
			return fmt.Errorf("YDB column definition changed outside the requested sequence")
		}
	}
	return nil
}
