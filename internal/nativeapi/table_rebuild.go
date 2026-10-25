package nativeapi

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path"
	"slices"

	native "github.com/stokaro/ptah-ydb-plugin/internal/nativeprotocol"
	"github.com/stokaro/ptah-ydb-plugin/internal/tablecreate"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/ydb-platform/ydb-go-genproto/Ydb_Scheme_V1"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb_Scheme"
	ydb "github.com/ydb-platform/ydb-go-sdk/v3"
	"google.golang.org/protobuf/proto"
)

// TableStatements owns the steps of a table rebuild that YQL makes: the copy
// of the rows into the new table and the renames that swap it into place.
// Each checks what the plan captured before it sends its one statement.
type TableStatements struct {
	Scheme Ydb_Scheme_V1.SchemeServiceClient
	// Open starts the table session the preconditions read with.
	Open func(context.Context) (TableDescriber, error)
	// Execute runs one statement through the query service, once.
	Execute func(context.Context, string) error
	// Empty reports whether the table at an absolute path holds no row.
	Empty func(context.Context, string) (bool, error)
}

// NewTableStatements binds the native clients of the session's driver, the
// statement runner and the database the row check reads.
func NewTableStatements(driver *ydb.Driver, execute func(context.Context, string) error, db *sql.DB) *TableStatements {
	return &TableStatements{Scheme: Ydb_Scheme_V1.NewSchemeServiceClient(ydb.GRPCConn(driver)), Execute: execute,
		Open: func(ctx context.Context) (TableDescriber, error) { return OpenTable(ctx, driver) },
		Empty: func(ctx context.Context, absolute string) (bool, error) {
			rows, err := db.QueryContext(ctx, "SELECT 1 FROM "+tablecreate.QuoteIdentifier(absolute)+" LIMIT 1")
			if err != nil {
				return false, err
			}
			defer func() { _ = rows.Close() }()
			found := rows.Next()
			return !found, rows.Err()
		}}
}

// bind binds an operation's owner to the session's database and returns its
// directory and absolute path.
func bind(database string, value *protocol.Envelope) (directory, absolute string, err error) {
	owner := proto.CloneOf(value.Owner)
	if owner.Database != "" && owner.Database != database {
		return "", "", fmt.Errorf("YDB table operation targets another database")
	}
	directory = path.Join(database, owner.Schema)
	return directory, path.Join(directory, owner.Name), nil
}

// ApplyCopy copies the rows of the source into the owner, a table the plan
// created that must hold none yet. The copy is one data query, which commits
// whole or not at all; a row that does not convert fails it.
func (t *TableStatements) ApplyCopy(ctx context.Context, database string, value *protocol.Envelope, beforeWrite func(context.Context) error) (dispatched bool, err error) {
	copied, err := tablecreate.DecodeCopy(value)
	if err != nil {
		return false, err
	}
	directory, absolute, err := bind(database, value)
	if err != nil {
		return false, err
	}
	source := path.Join(directory, copied.Source)
	session, err := t.Open(ctx)
	if err != nil {
		return false, err
	}
	defer func() { err = errors.Join(err, session.Close()) }()
	held, err := session.Describe(ctx, source)
	if err != nil {
		return false, err
	}
	for _, column := range copied.Columns {
		if !slices.ContainsFunc(held.GetColumns(), func(have *native.ColumnMeta) bool { return have.GetName() == column.Name }) {
			return false, fmt.Errorf("YDB table %s holds no column %q to copy", source, column.Name)
		}
	}
	empty, err := t.Empty(ctx, absolute)
	if err != nil {
		return false, err
	}
	if !empty {
		return false, fmt.Errorf("YDB table %s holds rows already, and a rebuild copies into an empty one", absolute)
	}
	if err := beforeWrite(ctx); err != nil {
		return false, err
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	statement, err := tablecreate.CopyStatement(absolute, copied)
	if err != nil {
		return false, err
	}
	return true, t.Execute(ctx, statement)
}

// ApplyRename renames the owner once it holds the row table the plan read and
// the target path is free, and confirms the swap.
func (t *TableStatements) ApplyRename(ctx context.Context, database string, value *protocol.Envelope, beforeWrite func(context.Context) error) (dispatched bool, err error) {
	rename, err := tablecreate.DecodeRename(value)
	if err != nil {
		return false, err
	}
	directory, absolute, err := bind(database, value)
	if err != nil {
		return false, err
	}
	entry, err := occupant(ctx, t.Scheme, directory, value.Owner.Name)
	if err != nil {
		return false, err
	}
	if entry == nil || entry.GetType() != Ydb_Scheme.Entry_TABLE {
		return false, fmt.Errorf("the path of YDB table %s no longer holds the row table the plan captured", absolute)
	}
	target, err := occupant(ctx, t.Scheme, directory, rename.Target)
	if err != nil {
		return false, err
	}
	if target != nil {
		return false, fmt.Errorf("the path %s a rename moves YDB table %s to is taken", path.Join(directory, rename.Target), absolute)
	}
	session, err := t.Open(ctx)
	if err != nil {
		return false, err
	}
	defer func() { err = errors.Join(err, session.Close()) }()
	columns, key, err := heldShape(ctx, session, tablecreate.RowStore, absolute)
	if err != nil {
		return false, err
	}
	if !slices.Equal(columns, rename.Columns) || !sameMembers(key, rename.Key) {
		return false, fmt.Errorf("YDB table %s holds other columns or another key than the plan captured", absolute)
	}
	if err := beforeWrite(ctx); err != nil {
		return false, err
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if err := t.Execute(ctx, "ALTER TABLE "+tablecreate.QuoteIdentifier(absolute)+" RENAME TO "+tablecreate.QuoteIdentifier(path.Join(directory, rename.Target))); err != nil {
		return true, err
	}
	moved, err := occupant(ctx, t.Scheme, directory, rename.Target)
	if err != nil {
		return true, err
	}
	gone, err := occupant(ctx, t.Scheme, directory, value.Owner.Name)
	if err != nil {
		return true, err
	}
	if moved == nil || gone != nil {
		return true, fmt.Errorf("the rename of YDB table %s was not confirmed", absolute)
	}
	return true, nil
}
