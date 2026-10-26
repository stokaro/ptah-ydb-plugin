package nativeapi

import (
	"context"
	"errors"
	"fmt"
	"path"
	"slices"

	"github.com/stokaro/ptah-ydb-plugin/internal/columnsemantics"
	native "github.com/stokaro/ptah-ydb-plugin/internal/nativeprotocol"
	"github.com/stokaro/ptah-ydb-plugin/internal/tablecreate"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	ydb "github.com/ydb-platform/ydb-go-sdk/v3"
	"google.golang.org/protobuf/proto"
)

// DefaultSession reads one row table and releases its native session.
type DefaultSession interface {
	Describe(context.Context, string) (*native.DescribeTableResult, error)
	Close() error
}

// ColumnDefault executes one default change after checking its captured column.
type ColumnDefault struct {
	Open    func(context.Context) (DefaultSession, error)
	Execute func(context.Context, string) error
}

// NewColumnDefault binds column observations and one-shot YQL execution.
func NewColumnDefault(driver *ydb.Driver, execute func(context.Context, string) error) *ColumnDefault {
	return &ColumnDefault{Execute: execute, Open: func(ctx context.Context) (DefaultSession, error) { return OpenTable(ctx, driver) }}
}

// Apply refuses stale state before dispatch and never retries an uncertain write.
func (c *ColumnDefault) Apply(ctx context.Context, database string, value *protocol.Envelope, beforeWrite func(context.Context) error) (dispatched bool, err error) {
	change, err := tablecreate.DecodeDefault(value)
	if err != nil {
		return false, err
	}
	owner := proto.CloneOf(value.Owner)
	if owner.Database != "" && owner.Database != database {
		return false, fmt.Errorf("YDB column default targets another database")
	}
	owner.Database = database
	bound := proto.CloneOf(value)
	bound.Owner = owner
	if _, err := tablecreate.NormalizeDefault(&protocol.ValueInput{Id: "execute", Representation: protocol.ValueInput_OPERATION, Value: bound}); err != nil {
		return false, err
	}
	session, err := c.Open(ctx)
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
		return false, fmt.Errorf("YDB column default before-state differs from the plan")
	}
	matches, err := change.MatchesHeld(before.Columns[at])
	if err != nil {
		return false, err
	}
	if !matches {
		return false, fmt.Errorf("YDB column default before-state differs from the plan")
	}
	statement, err := tablecreate.DefaultStatement(absolute, change)
	if err != nil {
		return false, err
	}
	if err := beforeWrite(ctx); err != nil {
		return false, err
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if err := c.Execute(ctx, statement); err != nil {
		return true, err
	}
	after, err := session.Describe(ctx, absolute)
	if err != nil {
		return true, err
	}
	if err := holdsRowTable(owner, after); err != nil {
		return true, err
	}
	return true, confirmColumnDefault(change, before, after, at)
}

func confirmColumnDefault(change *tablecreate.DefaultAlteration, before, after *native.DescribeTableResult, at int) error {
	if len(before.Columns) != len(after.Columns) || !slices.Equal(before.PrimaryKey, after.PrimaryKey) {
		return fmt.Errorf("YDB column inventory or key changed during default alteration")
	}
	for i, column := range before.Columns {
		actual := proto.CloneOf(after.Columns[i])
		if i == at {
			if err := confirmDefaultValue(change, actual); err != nil {
				return err
			}
			actual.DefaultValue = column.DefaultValue
		}
		if !proto.Equal(actual, column) {
			return fmt.Errorf("YDB column definition changed outside the requested default")
		}
	}
	return nil
}

func confirmDefaultValue(change *tablecreate.DefaultAlteration, actual *native.ColumnMeta) error {
	if actual.GetFromSequence() != nil {
		return fmt.Errorf("YDB column default became sequence-backed")
	}
	got, want := "", ""
	var err error
	if actual.GetFromLiteral() != nil {
		got, err = columnsemantics.Literal(actual.GetFromLiteral())
		if err != nil {
			return err
		}
	}
	if change.Set != nil {
		want, err = columnsemantics.Literal(change.Set)
		if err != nil {
			return err
		}
	}
	if got != want {
		return fmt.Errorf("YDB column default differs from the request after alteration")
	}
	return nil
}
