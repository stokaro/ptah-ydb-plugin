package nativeapi

import (
	"context"
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

// TableDescriber reads a table of either store from the table service.
type TableDescriber interface {
	Describe(context.Context, string) (*native.DescribeTableResult, error)
	DescribeColumnTable(context.Context, string) (*native.ColumnTableDescribeResult, error)
	Close() error
}

// TableDrop owns the precondition, the single dispatch of a table's DROP
// TABLE and its confirmation.
type TableDrop struct {
	Scheme Ydb_Scheme_V1.SchemeServiceClient
	// Open starts the table session the precondition reads with.
	Open func(context.Context) (TableDescriber, error)
	// Execute runs one schema statement through the query service, once.
	Execute func(context.Context, string) error
}

// NewTableDrop binds the native clients of the session's driver and the
// statement runner.
func NewTableDrop(driver *ydb.Driver, execute func(context.Context, string) error) *TableDrop {
	return &TableDrop{Scheme: Ydb_Scheme_V1.NewSchemeServiceClient(ydb.GRPCConn(driver)), Execute: execute,
		Open: func(ctx context.Context) (TableDescriber, error) { return OpenTable(ctx, driver) }}
}

// Apply drops the table the removal names once it holds what the plan
// captured: a table of the captured store, with the captured columns in order
// and the captured key columns. YQL's DROP TABLE is used for either store, as
// the host's embedded engine writes it. A failure after dispatch remains
// uncertain and is never retried.
func (d *TableDrop) Apply(ctx context.Context, database string, value *protocol.Envelope, beforeWrite func(context.Context) error) (dispatched bool, err error) {
	drop, err := tablecreate.DecodeDrop(value)
	if err != nil {
		return false, err
	}
	owner := proto.CloneOf(value.Owner)
	if owner.Database != "" && owner.Database != database {
		return false, fmt.Errorf("YDB table removal targets another database")
	}
	owner.Database = database
	directory := path.Join(database, owner.Schema)
	absolute := path.Join(directory, owner.Name)
	entry, err := occupant(ctx, d.Scheme, directory, owner.Name)
	if err != nil {
		return false, err
	}
	want := map[string]Ydb_Scheme.Entry_Type{tablecreate.RowStore: Ydb_Scheme.Entry_TABLE, tablecreate.ColumnStore: Ydb_Scheme.Entry_COLUMN_TABLE}[drop.Store]
	if entry == nil || entry.GetType() != want {
		return false, fmt.Errorf("the path of YDB table %s no longer holds the %s table the plan captured", absolute, drop.Store)
	}
	session, err := d.Open(ctx)
	if err != nil {
		return false, err
	}
	defer func() { err = errors.Join(err, session.Close()) }()
	columns, key, err := heldShape(ctx, session, drop.Store, absolute)
	if err != nil {
		return false, err
	}
	if !slices.Equal(columns, drop.Columns) || !sameMembers(key, drop.Key) {
		return false, fmt.Errorf("YDB table %s holds other columns or another key than the plan captured", absolute)
	}
	if err := beforeWrite(ctx); err != nil {
		return false, err
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if err := d.Execute(ctx, "DROP TABLE "+tablecreate.QuoteIdentifier(absolute)); err != nil {
		return true, err
	}
	gone, err := occupant(ctx, d.Scheme, directory, owner.Name)
	if err != nil {
		return true, err
	}
	if gone != nil {
		return true, fmt.Errorf("the removal of YDB table %s was not confirmed", absolute)
	}
	return true, nil
}

// heldShape reads the column names, in order, and the key of a table of
// store.
func heldShape(ctx context.Context, session TableDescriber, store, absolute string) (columns, key []string, err error) {
	if store == tablecreate.ColumnStore {
		described, err := session.DescribeColumnTable(ctx, absolute)
		if err != nil {
			return nil, nil, err
		}
		for _, column := range described.GetColumns() {
			columns = append(columns, column.GetName())
		}
		return columns, described.GetPrimaryKey(), nil
	}
	described, err := session.Describe(ctx, absolute)
	if err != nil {
		return nil, nil, err
	}
	for _, column := range described.GetColumns() {
		columns = append(columns, column.GetName())
	}
	return columns, described.GetPrimaryKey(), nil
}

// sameMembers reports whether two lists hold the same names, in any order.
func sameMembers(a, b []string) bool {
	return len(a) == len(b) && !slices.ContainsFunc(a, func(name string) bool { return !slices.Contains(b, name) })
}

// occupant lists the exact parent directory and returns the entry at name,
// or nil when the path is free. A listing that is not a directory's is an
// error.
func occupant(ctx context.Context, scheme Ydb_Scheme_V1.SchemeServiceClient, directory, name string) (*Ydb_Scheme.Entry, error) {
	listing, err := scheme.ListDirectory(ctx, &Ydb_Scheme.ListDirectoryRequest{Path: directory})
	if err != nil {
		return nil, fmt.Errorf("read YDB table namespace: transport failed")
	}
	var entries Ydb_Scheme.ListDirectoryResult
	if err := Operation(listing.GetOperation(), &entries); err != nil {
		return nil, err
	}
	if entries.GetSelf().GetName() != path.Base(directory) ||
		(entries.GetSelf().GetType() != Ydb_Scheme.Entry_DIRECTORY && entries.GetSelf().GetType() != Ydb_Scheme.Entry_DATABASE) || len(entries.Children) > 4096 {
		return nil, fmt.Errorf("invalid YDB table namespace")
	}
	var found *Ydb_Scheme.Entry
	for _, entry := range entries.GetChildren() {
		if entry.GetName() != name {
			continue
		}
		if found != nil {
			return nil, fmt.Errorf("invalid YDB table namespace entry")
		}
		found = entry
	}
	return found, nil
}
