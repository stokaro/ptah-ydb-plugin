//go:build integration

package integration_test

import (
	"context"
	"os"
	"path"
	"slices"
	"strconv"
	"testing"
	"time"

	qt "github.com/frankban/quicktest"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	providerruntime "github.com/stokaro/ptah/provider-sdk/runtime"
	"google.golang.org/protobuf/testing/protocmp"

	"github.com/stokaro/ptah-ydb-plugin/internal/backend"
	"github.com/stokaro/ptah-ydb-plugin/internal/connection"
)

// rowsFixture is a session on the test database and a table of every value
// kind, in a directory of its own.
type rowsFixture struct {
	opened    *connection.Connection
	client    *providerruntime.Client
	session   string
	directory string
	table     *protocol.Reference
	features  []string
}

// rowColumns are the table's columns, its declared types and the type the
// table holds. Decimal(22,9) is the one Decimal every line takes.
var rowColumns = []struct{ name, declared, live string }{
	{"id", "BIGINT", "Int64"}, {"name", "VARCHAR(64)", "Utf8"}, {"price", "DECIMAL(22,9)", "Decimal(22,9)"},
	{"at", "Timestamp", "Timestamp"}, {"day", "Date", "Date"}, {"blob", "BYTEA", "String"},
	{"doc", "JSONB", "JsonDocument"}, {"ok", "BOOLEAN", "Bool"}, {"n", "INT UNSIGNED", "Uint32"},
	{"wait", "Interval", "Interval"}, {"code", "UUID", "Uuid"},
}

func newRowsFixture(c *qt.C) *rowsFixture {
	c.Helper()
	opened, directory, _ := inspectionFixture(c)
	binary, digest := providerBinary(c)
	client := startProvider(c, binary, digest)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	session, err := client.Open(ctx, &protocol.OpenRequest{Engine: "ydb", Address: os.Getenv("PTAH_YDB_TEST_URL")})
	c.Assert(err, qt.IsNil)
	c.Cleanup(func() { c.Check(client.CloseSession(context.Background(), session.Id), qt.IsNil) })
	table := path.Join(directory, "rows")
	c.Assert(executeSQL(c, client, session.Id, "", "CREATE TABLE `"+table+"` (id Int64 NOT NULL, name Utf8, price Decimal(22,9), "+
		"at Timestamp, day Date, blob String, doc JsonDocument, ok Bool, n Uint32, wait Interval, code Uuid, PRIMARY KEY (id))").Outcome,
		qt.Equals, protocol.Execution_APPLIED)
	c.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		c.Check(execFixtureSchema(cleanup, opened.DB, "DROP TABLE `"+table+"`"), qt.IsNil)
	})
	return &rowsFixture{opened: opened, client: client, session: session.Id, directory: directory, features: backend.Description().GetFeatures(),
		table: &protocol.Reference{Kind: "table", Schema: directory, Name: "rows"}}
}

func (f *rowsFixture) columnNames() []string {
	names := make([]string, len(rowColumns))
	for i, column := range rowColumns {
		names[i] = column.name
	}
	return names
}

// typeRows types declared rows against the live table.
func (f *rowsFixture) typeRows(c *qt.C, declared, live []*protocol.Row) *protocol.TypedRows {
	c.Helper()
	request := &protocol.TypeRowsRequest{Engine: "ydb", TargetFeatures: f.features, Table: f.table, LiveTable: &protocol.LiveTable{},
		Declared: declared, Live: live}
	for _, column := range rowColumns {
		request.Columns = append(request.Columns, &protocol.RowColumn{Name: column.name, DeclaredType: column.declared})
		request.LiveTable.Columns = append(request.LiveTable.Columns, &protocol.Column{Name: column.name, Type: column.live})
	}
	typed, err := f.client.TypeRows(c.Context(), request)
	c.Assert(err, qt.IsNil)
	return typed
}

// write renders writes of typed rows and runs them.
func (f *rowsFixture) write(c *qt.C, typed *protocol.TypedRows, mode protocol.RowWrite_Mode, rows []*protocol.Row, columns []string) {
	c.Helper()
	types := map[string]string{}
	for _, column := range typed.GetColumns() {
		if slices.Contains(columns, column.GetName()) {
			types[column.GetName()] = column.GetType()
		}
	}
	request := &protocol.RenderRowWritesRequest{Engine: "ydb", TargetFeatures: f.features}
	for i, row := range rows {
		request.Writes = append(request.Writes, &protocol.RowWrite{ChangeId: "w" + string(rune('a'+i)), Mode: mode, Table: f.table,
			Columns: columns, KeyColumns: []string{"id"}, Values: row.GetValues(), ColumnTypes: types})
	}
	rendered, err := f.client.RenderRowWrites(c.Context(), request)
	c.Assert(err, qt.IsNil)
	c.Assert(rendered.GetComplete(), qt.IsTrue)
	execution, err := f.client.Execute(c.Context(), &protocol.ExecuteRequest{SessionId: f.session, RequestId: "rows", Units: rendered.GetUnits()})
	c.Assert(err, qt.IsNil)
	c.Assert(execution.GetOutcome(), qt.Equals, protocol.Execution_APPLIED)
}

// readAll reads the whole table in pages of size rows and returns the rows
// and the number of pages.
func (f *rowsFixture) readAll(c *qt.C, transaction string, size uint32) ([]*protocol.Row, int) {
	c.Helper()
	request := &protocol.ReadRowsRequest{SessionId: f.session, TransactionId: transaction, Table: f.table, Columns: f.columnNames(),
		KeyColumns: []string{"id"}, MaxRows: size}
	var rows []*protocol.Row
	pages := 0
	for {
		page, err := f.client.ReadRows(c.Context(), request)
		c.Assert(err, qt.IsNil)
		c.Assert(page.GetExists(), qt.IsTrue)
		rows = append(rows, page.GetRows()...)
		pages++
		if page.GetComplete() {
			return rows, pages
		}
		request.PageToken = page.GetNextPageToken()
	}
}

func dataRow(values ...*protocol.DataValue) *protocol.Row { return &protocol.Row{Values: values} }

func text(v string) *protocol.DataValue {
	return &protocol.DataValue{Value: &protocol.DataValue_StringValue{StringValue: v}}
}

func whole(v int64) *protocol.DataValue {
	return &protocol.DataValue{Value: &protocol.DataValue_Int64Value{Int64Value: v}}
}

func null() *protocol.DataValue {
	return &protocol.DataValue{Value: &protocol.DataValue_Null{Null: &protocol.Empty{}}}
}

// declaredRows are rows as a declaration writes them: text for most values,
// in spellings the server stores in another form.
func declaredRows() []*protocol.Row {
	return []*protocol.Row{
		dataRow(whole(1), text("North"), text("12.50"), text("2026-01-02 03:04:05.000001"), text("2026-01-02"),
			text("bytes"), text(`{"b": 2, "a": 1.50}`), text("yes"), whole(7), text("PT26H"), text("6F9619FF-8B86-D011-B42D-00C04FC964FF")),
		dataRow(whole(2), text("South"), null(), null(), null(), null(), null(), null(), null(), null(), null()),
		dataRow(whole(3), text("East"), text("-0.000000001"), text("1970-01-01T00:00:00Z"), text("1970-01-01"),
			&protocol.DataValue{Value: &protocol.DataValue_BytesValue{BytesValue: []byte{0, 255, '\'', '\\'}}},
			text("[]"), text("off"), whole(0), text("-PT1S"), text("00000000-0000-0000-0000-000000000000")),
	}
}

// TestRowServicesRoundTripSubprocess writes declared rows through the row
// services, reads them back in pages, and types both sides into one form, so
// a declaration the table already holds compares equal to what the table
// holds.
func TestRowServicesRoundTripSubprocess(t *testing.T) {
	c := qt.New(t)
	f := newRowsFixture(c)
	declared := f.typeRows(c, declaredRows(), nil)
	f.write(c, declared, protocol.RowWrite_INSERT, declared.GetDeclared(), f.columnNames())

	read, pages := f.readAll(c, "", 2)

	c.Assert(pages, qt.Equals, 2)
	c.Assert(read, qt.HasLen, 3)
	paired := f.typeRows(c, declaredRows(), read)
	c.Assert(paired.GetLive(), qt.CmpEquals(protocmp.Transform()), paired.GetDeclared())
	c.Assert(read, qt.CmpEquals(protocmp.Transform()), paired.GetDeclared())
}

// TestRowServicesWriteModesSubprocess runs an update, an upsert and a delete
// by key and reads the table back.
func TestRowServicesWriteModesSubprocess(t *testing.T) {
	c := qt.New(t)
	f := newRowsFixture(c)
	declared := f.typeRows(c, declaredRows(), nil)
	f.write(c, declared, protocol.RowWrite_INSERT, declared.GetDeclared(), f.columnNames())
	changed := f.typeRows(c, []*protocol.Row{dataRow(whole(2), text("Southwest"), null(), null(), null(), null(), null(), null(), null(), null(), null())}, nil)

	f.write(c, changed, protocol.RowWrite_UPDATE_BY_KEY, []*protocol.Row{dataRow(whole(2), text("Southwest"))}, []string{"id", "name"})
	f.write(c, changed, protocol.RowWrite_UPSERT, []*protocol.Row{dataRow(whole(4), text("West"))}, []string{"id", "name"})
	f.write(c, changed, protocol.RowWrite_DELETE_BY_KEY, []*protocol.Row{dataRow(whole(1))}, []string{"id"})

	read, _ := f.readAll(c, "", 10)
	names := make([]string, len(read))
	for i, row := range read {
		names[i] = row.GetValues()[1].GetStringValue()
	}
	c.Assert(names, qt.DeepEquals, []string{"Southwest", "East", "West"})
}

// TestReadRowsInATransactionSubprocess reads what an open transaction wrote
// and has not committed.
func TestReadRowsInATransactionSubprocess(t *testing.T) {
	c := qt.New(t)
	f := newRowsFixture(c)
	_, err := f.client.BeginTransaction(c.Context(), &protocol.BeginTransactionRequest{SessionId: f.session, TransactionId: "read",
		Isolation: protocol.TransactionIsolation_SERIALIZABLE})
	c.Assert(err, qt.IsNil)
	c.Assert(executeSQL(c, f.client, f.session, "read", "UPSERT INTO `"+path.Join(f.directory, "rows")+"` (id, name) VALUES (9, 'nine'u)").Outcome,
		qt.Equals, protocol.Execution_APPLIED)

	inside, _ := f.readAll(c, "read", 5)

	c.Assert(inside, qt.HasLen, 1)
	finished, err := f.client.FinishTransaction(c.Context(), &protocol.FinishTransactionRequest{SessionId: f.session, TransactionId: "read",
		Action: protocol.FinishTransactionRequest_ROLLBACK})
	c.Assert(err, qt.IsNil)
	c.Assert(finished.GetOutcome(), qt.Equals, protocol.TransactionResult_ROLLED_BACK)
	outside, _ := f.readAll(c, "", 5)
	c.Assert(outside, qt.HasLen, 0)
}

// TestReadRowsOrdersANullableKeySubprocess pages through a table whose
// ordering key holds nulls and repeats, so the cursor has to step past a null
// and past equal keys.
func TestReadRowsOrdersANullableKeySubprocess(t *testing.T) {
	c := qt.New(t)
	f := newRowsFixture(c)
	table := path.Join(f.directory, "nullable")
	c.Assert(executeSQL(c, f.client, f.session, "", "CREATE TABLE `"+table+"` (k Utf8, id Uint64 NOT NULL, PRIMARY KEY (k, id))").Outcome,
		qt.Equals, protocol.Execution_APPLIED)
	c.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		c.Check(execFixtureSchema(cleanup, f.opened.DB, "DROP TABLE `"+table+"`"), qt.IsNil)
	})
	c.Assert(executeSQL(c, f.client, f.session, "", "UPSERT INTO `"+table+"` (k, id) VALUES "+
		"(NULL, 2ul), (NULL, 1ul), ('b'u, 1ul), ('a'u, 2ul), ('a'u, 1ul)").Outcome, qt.Equals, protocol.Execution_APPLIED)
	request := &protocol.ReadRowsRequest{SessionId: f.session, Table: &protocol.Reference{Kind: "table", Schema: f.directory, Name: "nullable"},
		Columns: []string{"k", "id"}, KeyColumns: []string{"k"}, MaxRows: 1}
	var keys []string
	for range 5 {
		page, err := f.client.ReadRows(c.Context(), request)
		c.Assert(err, qt.IsNil)
		for _, row := range page.GetRows() {
			keys = append(keys, row.GetValues()[0].GetStringValue()+"/"+strconv.FormatUint(row.GetValues()[1].GetUint64Value(), 10))
		}
		request.PageToken = page.GetNextPageToken()
	}

	c.Assert(keys, qt.DeepEquals, []string{"/1", "/2", "a/1", "a/2", "b/1"})
	c.Assert(request.GetPageToken(), qt.Equals, "")
}

// TestReadRowsOfAMissingTableSubprocess reads a table that does not exist.
func TestReadRowsOfAMissingTableSubprocess(t *testing.T) {
	c := qt.New(t)
	f := newRowsFixture(c)

	page, err := f.client.ReadRows(c.Context(), &protocol.ReadRowsRequest{SessionId: f.session,
		Table: &protocol.Reference{Kind: "table", Schema: f.directory, Name: "absent"}, Columns: []string{"id"}, KeyColumns: []string{"id"}, MaxRows: 5})

	c.Assert(err, qt.IsNil)
	c.Assert(page.GetExists(), qt.IsFalse)
	c.Assert(page.GetComplete(), qt.IsTrue)
}
