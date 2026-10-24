package backend

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"

	"github.com/stokaro/ptah-ydb-plugin/internal/ownedtable"
	"github.com/stokaro/ptah-ydb-plugin/internal/rowdata"
	"github.com/stokaro/ptah-ydb-plugin/internal/ydbtype"
	"github.com/stokaro/ptah-ydb-plugin/schema"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

// pageBytes bounds the rows of one page, so the page and its envelope stay
// within the transport's 4 MiB message limit.
const pageBytes = 3 << 20

// TypeRows types the declared and live rows of one table offline; see
// rowdata.TypeRows. A value its column cannot hold refuses the request.
func (p *Provider) TypeRows(ctx context.Context, request *protocol.TypeRowsRequest) (*protocol.TypedRows, error) {
	if err := ctx.Err(); err != nil {
		return nil, status.FromContextError(err).Err()
	}
	result, err := rowdata.TypeRows(request)
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	return result, nil
}

// RenderRowWrites renders typed row writes offline; see
// rowdata.RenderRowWrites.
func (p *Provider) RenderRowWrites(ctx context.Context, request *protocol.RenderRowWritesRequest) (*protocol.RenderResult, error) {
	if err := ctx.Err(); err != nil {
		return nil, status.FromContextError(err).Err()
	}
	result, err := rowdata.RenderRowWrites(request)
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	return result, nil
}

// rowReader is what a page read needs of a connection or a transaction.
type rowReader interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

// ReadRows reads one page of a table in key order, in the session's
// transaction when the request names one. A table that does not exist is a
// complete page that says so; a table that does not hold a requested column
// is an error.
func (p *Provider) ReadRows(ctx context.Context, request *protocol.ReadRowsRequest) (result *protocol.RowPage, err error) {
	table := request.GetTable()
	if table.GetCatalog() != "" || len(table.GetPath()) != 0 || len(table.GetOverload()) != 0 {
		return nil, status.Error(codes.InvalidArgument, "a YDB table is named by its directory and name")
	}
	if err := rowdata.ValidateDirectory(table.GetSchema()); err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	s, err := p.lookup(request.GetSessionId())
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, status.Error(codes.FailedPrecondition, "YDB session is closed")
	}
	if table.GetDatabase() != "" && table.GetDatabase() != s.database {
		return nil, status.Error(codes.InvalidArgument, "the table belongs to another database than the session's")
	}
	runner, err := s.reader(request.GetTransactionId())
	if err != nil {
		return nil, err
	}
	identity, err := ownedtable.Resolve(s.database, table.GetSchema(), table.GetName())
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	described, err := identity.Find(ctx, s.connection.Driver)
	if err != nil {
		return nil, readError(ctx, err)
	}
	if described == nil {
		return &protocol.RowPage{Table: proto.CloneOf(table), Exists: false, Complete: true}, nil
	}
	types := make(map[string]string, len(described.GetColumns()))
	for _, column := range described.GetColumns() {
		kind, _, err := schema.ColumnType(column.GetType())
		if err != nil {
			return nil, status.Error(codes.FailedPrecondition, fmt.Sprintf("column %q: %v", column.GetName(), err))
		}
		types[column.GetName()] = kind
	}
	columns := make([]*protocol.DataColumn, len(request.GetColumns()))
	for i, name := range request.GetColumns() {
		kind, found := types[name]
		if !found {
			return nil, status.Error(codes.FailedPrecondition, fmt.Sprintf("the table holds no column %q", name))
		}
		columns[i] = &protocol.DataColumn{Name: name, Type: kind, Kind: rowdata.Kind(kind)}
		if columns[i].Kind == protocol.DataKind_DATA_KIND_UNSPECIFIED {
			return nil, status.Error(codes.FailedPrecondition, fmt.Sprintf("column %q: no value of type %s is read", name, kind))
		}
	}
	order := rowdata.Order(request.GetKeyColumns(), described.GetPrimaryKey())
	query := rowdata.PageQuery{Table: ydbtype.QuotePath(table.GetSchema(), table.GetName()), Order: order,
		Limit: rowdata.Limit(request.GetMaxRows()), Columns: slices.Clone(request.GetColumns())}
	for _, column := range order {
		query.OrderTypes = append(query.OrderTypes, types[column])
		if !slices.Contains(query.Columns, column) {
			query.Columns = append(query.Columns, column)
		}
	}
	if request.GetPageToken() != "" {
		if query.After, err = rowdata.DecodeToken(request.GetPageToken(), order); err != nil {
			return nil, status.Error(codes.InvalidArgument, err.Error())
		}
	}
	text, err := query.Text()
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	result = &protocol.RowPage{Table: proto.CloneOf(table), Exists: true, Columns: columns}
	last, more, err := readPage(ctx, runner, text, query, request, types, result)
	if failed := (*driverError)(nil); errors.As(err, &failed) && request.GetTransactionId() != "" {
		// A statement that failed may have ended the transaction on the
		// server, so the transaction can only be rolled back.
		s.transactions[request.GetTransactionId()].failed = true
	}
	if err != nil {
		return nil, readError(ctx, err)
	}
	if !more {
		result.Complete = true
		return result, nil
	}
	if result.NextPageToken, err = rowdata.EncodeToken(order, last); err != nil {
		return nil, status.Error(codes.FailedPrecondition, err.Error())
	}
	return result, nil
}

// readPage reads the rows of one page into result. It stops at the request's
// row bound and at the page's byte bound, and reports whether rows remain and
// the ordering values of the last row it kept.
func readPage(ctx context.Context, runner rowReader, text string, query rowdata.PageQuery, request *protocol.ReadRowsRequest,
	types map[string]string, result *protocol.RowPage) (last []*protocol.DataValue, more bool, err error) {
	rows, err := runner.QueryContext(ctx, text)
	if err != nil {
		return nil, false, &driverError{err}
	}
	defer func() {
		if closeErr := rows.Close(); closeErr != nil && err == nil {
			err = &driverError{closeErr}
		}
	}()
	size := 0
	for rows.Next() {
		if len(result.Rows) == int(request.GetMaxRows()) {
			return last, true, nil
		}
		scanned := make([]any, len(query.Columns))
		targets := make([]any, len(scanned))
		for i := range scanned {
			targets[i] = &scanned[i]
		}
		if err := rows.Scan(targets...); err != nil {
			return nil, false, &driverError{err}
		}
		values := make(map[string]*protocol.DataValue, len(query.Columns))
		for i, column := range query.Columns {
			value, err := rowdata.Scanned(types[column], scanned[i])
			if err != nil {
				return nil, false, err
			}
			values[column] = value
		}
		row := &protocol.Row{Values: make([]*protocol.DataValue, len(request.GetColumns()))}
		for i, column := range request.GetColumns() {
			row.Values[i] = values[column]
		}
		size += proto.Size(row)
		if size > pageBytes {
			if len(result.Rows) == 0 {
				return nil, false, errors.New("one row of the table is larger than a page holds")
			}
			return last, true, nil
		}
		result.Rows = append(result.Rows, row)
		last = make([]*protocol.DataValue, len(query.Order))
		for i, column := range query.Order {
			last[i] = values[column]
		}
	}
	if err := rows.Err(); err != nil {
		return nil, false, &driverError{err}
	}
	return last, false, nil
}

// driverError is a failure the driver reported. Its text can quote a value,
// so it is not repeated to the host.
type driverError struct{ err error }

func (e *driverError) Error() string { return "the YDB driver failed" }
func (e *driverError) Unwrap() error { return e.err }

// reader returns what a read in the named transaction, or outside one, runs
// on.
func (s *session) reader(transactionID string) (rowReader, error) {
	if transactionID == "" {
		if s.active != "" {
			return nil, status.Error(codes.FailedPrecondition, "YDB session has an active transaction")
		}
		return s.connection.DB, nil
	}
	record := s.transactions[transactionID]
	if record == nil || record.tx == nil || s.active != transactionID || record.failed {
		return nil, status.Error(codes.FailedPrecondition, "YDB transaction is not active")
	}
	return record.tx, nil
}

// readError reports a read that could not finish, without the driver's text,
// which can quote a value.
func readError(ctx context.Context, err error) error {
	if ctxErr := ctx.Err(); ctxErr != nil {
		return status.FromContextError(ctxErr).Err()
	}
	if failed := (*driverError)(nil); errors.As(err, &failed) {
		return status.Error(codes.Unavailable, "YDB row read failed")
	}
	return status.Error(codes.FailedPrecondition, "YDB row read failed: "+err.Error())
}
