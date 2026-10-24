package nativeapi

import (
	"context"
	"errors"
	"fmt"
	"path"
	"slices"
	"strings"

	"github.com/stokaro/ptah-ydb-plugin/internal/columnstore"
	native "github.com/stokaro/ptah-ydb-plugin/internal/nativeprotocol"
	"github.com/stokaro/ptah-ydb-plugin/internal/tablecreate"
	"github.com/stokaro/ptah-ydb-plugin/schema"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/ydb-platform/ydb-go-genproto/Ydb_Scheme_V1"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb_Scheme"
	ydb "github.com/ydb-platform/ydb-go-sdk/v3"
	"google.golang.org/protobuf/proto"
)

// ErrNoMonitoring refuses a column table's creation in a session whose address
// names no monitoring endpoint: the shard count the creation is confirmed by
// is read there.
var ErrNoMonitoring = errors.New("creating a column table requires monitoring=http(s)://host:port in the YDB address: " +
	"the table service leaves out the shard count the creation is confirmed by")

// ColumnDescriber reads a column table's descriptions from the table service.
type ColumnDescriber interface {
	DescribeColumnTable(context.Context, string) (*native.ColumnTableDescribeResult, error)
	Close() error
}

// ColumnTableCreation owns the absence check, the single dispatch of a column
// table's CREATE TABLE and its confirmation.
type ColumnTableCreation struct {
	Scheme Ydb_Scheme_V1.SchemeServiceClient
	// Open starts the table session the confirmation reads with.
	Open func(context.Context) (ColumnDescriber, error)
	// Execute runs one schema statement through the query service, once.
	Execute func(context.Context, string) error
	// Monitoring reads the monitoring description of the column table at an
	// absolute path; nil for a session whose address names no endpoint.
	Monitoring func(context.Context, string) ([]byte, error)
}

// NewColumnTableCreation binds the native clients of the session's driver, the
// statement runner and the monitoring reader.
func NewColumnTableCreation(driver *ydb.Driver, execute func(context.Context, string) error, monitoring func(context.Context, string) ([]byte, error)) *ColumnTableCreation {
	return &ColumnTableCreation{Scheme: Ydb_Scheme_V1.NewSchemeServiceClient(ydb.GRPCConn(driver)), Execute: execute, Monitoring: monitoring,
		Open: func(ctx context.Context) (ColumnDescriber, error) { return OpenTable(ctx, driver) }}
}

// Apply refuses an existing path, then creates the column table with YQL's
// CREATE TABLE and confirms it from the table service and the monitoring
// endpoint. YQL is used rather than the native CreateTable call, which writes
// another hash function than the one the host's embedded engine writes and
// reads. A failure after dispatch remains uncertain and is never retried.
func (c *ColumnTableCreation) Apply(ctx context.Context, database string, value *protocol.Envelope, beforeWrite func(context.Context) error) (dispatched bool, err error) {
	creation, err := tablecreate.ColumnTable(value)
	if err != nil {
		return false, err
	}
	if c.Monitoring == nil {
		return false, ErrNoMonitoring
	}
	owner := proto.CloneOf(value.Owner)
	if owner.Database != "" && owner.Database != database {
		return false, fmt.Errorf("YDB table creation targets another database")
	}
	owner.Database = database
	bound := proto.CloneOf(value)
	bound.Owner = owner
	if _, err := tablecreate.NormalizeColumnTable(&protocol.ValueInput{Id: "execute", Representation: protocol.ValueInput_OPERATION, Value: bound}); err != nil {
		return false, err
	}
	directory := path.Join(database, owner.Schema)
	if err := absent(ctx, c.Scheme, directory, owner); err != nil {
		return false, err
	}
	session, err := c.Open(ctx)
	if err != nil {
		return false, err
	}
	defer func() { err = errors.Join(err, session.Close()) }()
	if err := beforeWrite(ctx); err != nil {
		return false, err
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	absolute := path.Join(directory, owner.Name)
	if err := c.Execute(ctx, tablecreate.ColumnTableStatement(absolute, creation)); err != nil {
		return true, err
	}
	described, err := session.DescribeColumnTable(ctx, absolute)
	if err != nil {
		return true, err
	}
	body, err := c.Monitoring(ctx, absolute)
	if err != nil {
		return true, err
	}
	observed, err := columnstore.Decode(body, absolute, described)
	if err != nil {
		return true, err
	}
	return true, confirmColumnTable(creation, described, observed)
}

// absent lists the exact parent directory and refuses an occupied path, a
// missing parent, or a listing that is not a directory's.
func absent(ctx context.Context, scheme Ydb_Scheme_V1.SchemeServiceClient, directory string, owner *protocol.Reference) error {
	listing, err := scheme.ListDirectory(ctx, &Ydb_Scheme.ListDirectoryRequest{Path: directory})
	if err != nil {
		return fmt.Errorf("read YDB table namespace: transport failed")
	}
	var entries Ydb_Scheme.ListDirectoryResult
	if err := Operation(listing.GetOperation(), &entries); err != nil {
		return err
	}
	if entries.GetSelf().GetName() != path.Base(directory) ||
		(entries.GetSelf().GetType() != Ydb_Scheme.Entry_DIRECTORY && entries.GetSelf().GetType() != Ydb_Scheme.Entry_DATABASE) ||
		(owner.Schema != "" && entries.GetSelf().GetType() == Ydb_Scheme.Entry_DATABASE) || len(entries.Children) > 4096 {
		return fmt.Errorf("invalid YDB table namespace")
	}
	names := map[string]bool{}
	for _, entry := range entries.Children {
		if entry.GetName() == "" || strings.ContainsAny(entry.Name, "/\\\x00") || entry.Name == "." || entry.Name == ".." || names[entry.Name] {
			return fmt.Errorf("invalid YDB table namespace entry")
		}
		names[entry.Name] = true
		if entry.Name == owner.Name {
			return fmt.Errorf("YDB table creation path is already occupied")
		}
	}
	return nil
}

// confirmColumnTable requires the table read back to be the one the creation
// asked for: its columns in order with their types and nullability, its key,
// the hash key the creation states or, without one, the whole key, the shard
// count the creation states, its TTL and no local index.
func confirmColumnTable(wanted *native.ColumnTableCreation, described *native.ColumnTableDescribeResult, observed *native.ColumnStorageObservation) error {
	if !slices.Equal(wanted.PrimaryKey, described.PrimaryKey) || len(wanted.Columns) != len(described.Columns) {
		return fmt.Errorf("YDB created column table identity or structure differs from the request")
	}
	for i, column := range wanted.Columns {
		actual := described.Columns[i]
		expectedType, expectedNullable, err := schema.ColumnType(column.Type)
		if err != nil {
			return err
		}
		actualType, actualNullable, err := schema.ColumnType(actual.Type)
		if err != nil {
			return err
		}
		if actual.GetNotNull() {
			actualNullable = false
		}
		if column.Name != actual.Name || expectedType != actualType || expectedNullable != actualNullable {
			return fmt.Errorf("YDB created column differs from the request")
		}
	}
	hash := wanted.HashColumns
	if len(hash) == 0 {
		hash = wanted.PrimaryKey
	}
	if !slices.Equal(hash, observed.HashColumns) || (wanted.Partitions != 0 && wanted.Partitions != observed.Partitions) {
		return fmt.Errorf("YDB created column table layout differs from the request")
	}
	if !tablecreate.ColumnTTLEqual(wanted.Ttl, observed.Ttl) {
		return fmt.Errorf("YDB created column table TTL differs from the request")
	}
	if len(observed.LocalIndexes) != 0 {
		return fmt.Errorf("YDB created column table has unexpected local indexes")
	}
	return nil
}
