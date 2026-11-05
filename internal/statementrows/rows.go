// Package statementrows pages a single YDB query result without buffering the
// whole response or converting an unsupported value into text.
package statementrows

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"google.golang.org/protobuf/proto"

	"github.com/stokaro/ptah-ydb-plugin/internal/authored"
	"github.com/stokaro/ptah-ydb-plugin/internal/rowdata"
)

const pageBytes = 3 << 20
const metadataBytes = 256 << 10

// Cursor owns rows. Its caller serializes reads and Close and owns the
// connection and transaction that keep the result alive.
type Cursor struct {
	rows     *sql.Rows
	columns  []*protocol.DataColumn
	pending  *protocol.Row
	closed   bool
	closeErr error
}

// Querier runs a statement: the pinned connection or the transaction a
// statement result lives on.
type Querier interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

// Query runs query on q and returns a cursor that owns its result, so the
// rows never leave this package, whose [Cursor.Page] checks their terminal
// error. statementErr is the database's answer to a statement it did not
// run; err is a result the cursor cannot page, whose rows Query closes.
func Query(ctx context.Context, q Querier, query string, args ...any) (cursor *Cursor, statementErr, err error) {
	rows, statementErr := q.QueryContext(ctx, query, args...)
	if statementErr != nil {
		return nil, statementErr, nil
	}
	cursor, err = newCursor(rows)
	if err != nil {
		_ = rows.Close()
		return nil, nil, err
	}
	return cursor, nil, nil
}

// newCursor captures the driver's column metadata. Ownership transfers only
// on success; the caller closes rows after an error.
func newCursor(rows *sql.Rows) (*Cursor, error) {
	types, err := rows.ColumnTypes()
	if err != nil {
		return nil, err
	}
	cursor := &Cursor{rows: rows}
	size := 0
	for _, column := range types {
		name := rowdata.ColumnType(column.DatabaseTypeName())
		kind := rowdata.Kind(name)
		if kind == protocol.DataKind_DATA_KIND_UNSPECIFIED {
			return nil, authored.New(fmt.Sprintf("result column %q has unsupported YDB type %s", column.Name(), name))
		}
		metadata := &protocol.DataColumn{Name: column.Name(), Type: name, Kind: kind}
		size += proto.Size(metadata) + 8
		if size > metadataBytes {
			return nil, authored.New("statement columns exceed the metadata byte limit")
		}
		cursor.columns = append(cursor.columns, metadata)
	}
	return cursor, nil
}

// Columns returns independent wire metadata for the first page.
func (c *Cursor) Columns() []*protocol.DataColumn {
	columns := make([]*protocol.DataColumn, len(c.columns))
	for i, column := range c.columns {
		columns[i] = proto.CloneOf(column)
	}
	return columns
}

// Page returns at most limit rows and reserves at most one row for the next
// page. An error returns no prefix. EOF checks that no second result set was
// hidden by the one-statement contract.
func (c *Cursor) Page(limit uint32) ([]*protocol.Row, bool, error) {
	if c.closed {
		return nil, false, errors.New("statement result is closed")
	}
	var result []*protocol.Row
	size := 0
	for {
		row, err := c.next()
		if err != nil {
			return nil, false, err
		}
		if row == nil {
			return result, false, c.Close()
		}
		rowSize := proto.Size(row) + 8
		if rowSize > pageBytes {
			return nil, false, errors.New("statement row exceeds the page byte limit")
		}
		if len(result) == int(limit) || size+rowSize > pageBytes {
			c.pending = row
			return result, true, nil
		}
		result = append(result, row)
		size += rowSize
	}
}

// next takes the row reserved by the previous page, or scans the next one. At
// the end of the result it returns no row, after checking that the driver
// reported no error and that no second result set follows.
func (c *Cursor) next() (*protocol.Row, error) {
	row := c.pending
	c.pending = nil
	if row != nil {
		return row, nil
	}
	if c.rows.Next() {
		return c.scan()
	}
	if err := c.rows.Err(); err != nil {
		return nil, err
	}
	if c.rows.NextResultSet() {
		return nil, errors.New("statement returned more than one result set")
	}
	return nil, c.rows.Err()
}

func (c *Cursor) scan() (*protocol.Row, error) {
	values := make([]any, len(c.columns))
	arguments := make([]any, len(values))
	for i := range values {
		arguments[i] = &values[i]
	}
	if err := c.rows.Scan(arguments...); err != nil {
		return nil, err
	}
	row := &protocol.Row{Values: make([]*protocol.DataValue, len(values))}
	for i, value := range values {
		var err error
		row.Values[i], err = rowdata.Scanned(c.columns[i].Type, value)
		if err != nil {
			return nil, err
		}
	}
	return row, nil
}

// Close releases the result once and retains its cleanup error.
func (c *Cursor) Close() error {
	if !c.closed {
		c.closed = true
		c.pending = nil
		c.closeErr = c.rows.Close()
	}
	return c.closeErr
}
