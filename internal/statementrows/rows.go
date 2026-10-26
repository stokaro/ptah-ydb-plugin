// Package statementrows pages a single YDB query result without buffering the
// whole response or converting an unsupported value into text.
package statementrows

import (
	"database/sql"
	"errors"
	"fmt"

	"github.com/stokaro/ptah-ydb-plugin/internal/rowdata"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"google.golang.org/protobuf/proto"
)

const pageBytes = 3 << 20

// Cursor owns rows. Its caller serializes reads and Close and owns the
// connection and transaction that keep the result alive.
type Cursor struct {
	rows *sql.Rows
	columns []*protocol.DataColumn
	pending *protocol.Row
	closed bool
	closeErr error
}

// New captures the driver's column metadata. Ownership transfers only on
// success; the caller closes rows after an error.
func New(rows *sql.Rows) (*Cursor, error) {
	types, err := rows.ColumnTypes()
	if err != nil { return nil, err }
	cursor := &Cursor{rows: rows}
	for _, column := range types {
		name := rowdata.ColumnType(column.DatabaseTypeName())
		kind := rowdata.Kind(name)
		if kind == protocol.DataKind_DATA_KIND_UNSPECIFIED {
			return nil, fmt.Errorf("result column %q has unsupported YDB type %s", column.Name(), name)
		}
		cursor.columns = append(cursor.columns, &protocol.DataColumn{Name: column.Name(), Type: name, Kind: kind})
	}
	return cursor, nil
}

// Columns returns independent wire metadata for the first page.
func (c *Cursor) Columns() []*protocol.DataColumn {
	columns := make([]*protocol.DataColumn, len(c.columns))
	for i, column := range c.columns { columns[i] = proto.CloneOf(column) }
	return columns
}

// Page returns at most max rows and reserves at most one row for the next
// page. An error returns no prefix. EOF checks that no second result set was
// hidden by the one-statement contract.
func (c *Cursor) Page(max uint32) ([]*protocol.Row, bool, error) {
	if c.closed { return nil, false, errors.New("statement result is closed") }
	var result []*protocol.Row
	size := 0
	for {
		row := c.pending
		c.pending = nil
		if row == nil {
			if !c.rows.Next() {
				if err := c.rows.Err(); err != nil { return nil, false, err }
				if c.rows.NextResultSet() { return nil, false, errors.New("statement returned more than one result set") }
				if err := c.rows.Err(); err != nil { return nil, false, err }
				return result, false, c.Close()
			}
			var err error
			row, err = c.scan()
			if err != nil { return nil, false, err }
		}
		rowSize := proto.Size(row) + 8
		if rowSize > pageBytes { return nil, false, errors.New("statement row exceeds the page byte limit") }
		if len(result) == int(max) || size + rowSize > pageBytes {
			c.pending = row
			return result, true, nil
		}
		result = append(result, row)
		size += rowSize
	}
}

func (c *Cursor) scan() (*protocol.Row, error) {
	values := make([]any, len(c.columns))
	arguments := make([]any, len(values))
	for i := range values { arguments[i] = &values[i] }
	if err := c.rows.Scan(arguments...); err != nil { return nil, err }
	row := &protocol.Row{Values: make([]*protocol.DataValue, len(values))}
	for i, value := range values {
		var err error
		row.Values[i], err = rowdata.Scanned(c.columns[i].Type, value)
		if err != nil { return nil, err }
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
