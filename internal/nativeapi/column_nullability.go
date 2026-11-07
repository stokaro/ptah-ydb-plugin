package nativeapi

import (
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

// ColumnNullability applies a column nullability operation: the DROP NOT
// NULL of a column, checked against the column the plan read before it runs
// and confirmed after.
type ColumnNullability struct {
	Open    func(context.Context) (DefaultSession, error)
	Execute func(context.Context, string) error
}

// NewColumnNullability applies through driver, running the statement with
// execute.
func NewColumnNullability(driver *ydb.Driver, execute func(context.Context, string) error) *ColumnNullability {
	return &ColumnNullability{Execute: execute, Open: func(ctx context.Context) (DefaultSession, error) { return OpenTable(ctx, driver) }}
}

// Apply runs the operation value holds in database. It sends nothing where
// the column differs from the one the plan read, and reports whether it sent
// the statement.
func (c *ColumnNullability) Apply(ctx context.Context, database string, value *protocol.Envelope, beforeWrite func(context.Context) error) (dispatched bool, err error) {
	change, err := tablecreate.DecodeNullability(value)
	if err != nil {
		return false, err
	}
	owner := proto.CloneOf(value.Owner)
	if owner.Database != "" && owner.Database != database {
		return false, fmt.Errorf("YDB column nullability change targets another database")
	}
	owner.Database = database
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
		return false, fmt.Errorf("YDB column nullability before-state differs from the plan")
	}
	matches, err := tablecreate.EqualCapturedColumns(change.Held, before.Columns[at])
	if err != nil {
		return false, err
	}
	if !matches {
		return false, fmt.Errorf("YDB column nullability before-state differs from the plan")
	}
	if err := beforeWrite(ctx); err != nil {
		return false, err
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if err := c.Execute(ctx, tablecreate.NullabilityStatement(absolute, change)); err != nil {
		return true, err
	}
	after, err := session.Describe(ctx, absolute)
	if err != nil {
		return true, err
	}
	if err := holdsRowTable(owner, after); err != nil {
		return true, err
	}
	return true, confirmColumnNullability(before, after, at)
}

// confirmColumnNullability checks that only the column at takes NULL now.
func confirmColumnNullability(before, after *native.DescribeTableResult, at int) error {
	if len(before.Columns) != len(after.Columns) || !slices.Equal(before.PrimaryKey, after.PrimaryKey) {
		return fmt.Errorf("YDB column inventory or key changed during the nullability change")
	}
	for i, column := range before.Columns {
		actual := proto.CloneOf(after.Columns[i])
		if i == at {
			if _, nullable, err := schema.ColumnType(actual.GetType()); err != nil || !nullable {
				return fmt.Errorf("YDB column %q still refuses NULL after DROP NOT NULL", column.GetName())
			}
			expected := proto.CloneOf(column)
			expected.Type, expected.NotNull = schema.Optional(column.GetType()), nil
			column = expected
			actual.NotNull = nil
		}
		if !proto.Equal(actual, column) {
			return fmt.Errorf("YDB column definition changed outside the requested nullability")
		}
	}
	return nil
}
