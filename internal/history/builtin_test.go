package history_test

import (
	"database/sql"
	"math"
	"testing"
	"time"

	qt "github.com/frankban/quicktest"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb_Scheme"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb_Table"
	"google.golang.org/protobuf/testing/protocmp"

	"github.com/stokaro/ptah-ydb-plugin/internal/history"
	native "github.com/stokaro/ptah-ydb-plugin/internal/nativeprotocol"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
)

// recordedAt is the applied_at of the built-in engine's recorded row of
// version 1 (integration/testdata/builtinhistory), to the microsecond YDB's
// Timestamp keeps.
var recordedAt = time.Date(2026, 11, 5, 19, 53, 19, 520812000, time.UTC)

func text(value string) sql.NullString { return sql.NullString{String: value, Valid: true} }

// A row of the built-in engine's native revision table reads as the record
// the host keeps: the state with the direction its `:down` suffix names, the
// run time in nanoseconds, and an error column's NULL as no text.
func TestNativeRowReadsAsARecord_HappyPath(t *testing.T) {
	for _, test := range []struct {
		name string
		row  history.NativeRow
		want *protocol.HistoryRevision
	}{
		{"an applied migration", history.NativeRow{Version: 1, Description: "Create", AppliedAt: recordedAt, State: "applied", Applied: 1, Total: 1,
			ExecutionMilliseconds: 103, Checksum: "dd91378e"},
			&protocol.HistoryRevision{Version: "1", Description: "Create", State: protocol.HistoryRevision_APPLIED, Direction: protocol.HistoryRevision_UP,
				Applied: 1, Total: 1, ExecutionNanoseconds: 103000000, Checksum: "dd91378e", AppliedAtSeconds: recordedAt.Unix(), AppliedAtNanos: 520812000}},
		{"a failed migration", history.NativeRow{Version: 2, Description: "More", AppliedAt: recordedAt, State: "failed", Applied: 1, Total: 2,
			Error: text("failed to execute migration SQL"), ErrorStatement: text("INSERT INTO t (id) VALUES (1)"), ExecutionMilliseconds: 40,
			Checksum: "partial:h1:vBNI"},
			&protocol.HistoryRevision{Version: "2", Description: "More", State: protocol.HistoryRevision_FAILED, Direction: protocol.HistoryRevision_UP,
				Applied: 1, Total: 2, Error: "failed to execute migration SQL", ErrorStatement: "INSERT INTO t (id) VALUES (1)",
				ExecutionNanoseconds: 40000000, Checksum: "partial:h1:vBNI", AppliedAtSeconds: recordedAt.Unix(), AppliedAtNanos: 520812000}},
		{"a rollback that stopped", history.NativeRow{Version: -3, AppliedAt: recordedAt, State: "pending:down", Applied: 1, Total: 3},
			&protocol.HistoryRevision{Version: "-3", State: protocol.HistoryRevision_PENDING, Direction: protocol.HistoryRevision_DOWN, Applied: 1, Total: 3,
				AppliedAtSeconds: recordedAt.Unix(), AppliedAtNanos: 520812000}},
		{"an up direction spelled out", history.NativeRow{Version: 4, AppliedAt: recordedAt, State: "failed:up"},
			&protocol.HistoryRevision{Version: "4", State: protocol.HistoryRevision_FAILED, Direction: protocol.HistoryRevision_UP,
				AppliedAtSeconds: recordedAt.Unix(), AppliedAtNanos: 520812000}},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			record, err := test.row.Record()
			c.Assert(err, qt.IsNil)
			c.Assert(record, qt.CmpEquals(protocmp.Transform()), test.want)
		})
	}
}

// A row whose state, direction or counts no host record holds is refused.
func TestNativeRowReadsAsARecord_FailurePath(t *testing.T) {
	for _, test := range []struct {
		name string
		row  history.NativeRow
		want string
	}{
		{"an unknown state", history.NativeRow{State: "done"}, "YDB revision row has an unknown state"},
		{"an unknown direction", history.NativeRow{State: "applied:sideways"}, "YDB revision row has an unknown direction"},
		{"a negative count", history.NativeRow{State: "applied", Applied: -1}, "YDB revision row has a negative or out of range count"},
		{"a run time beyond nanoseconds", history.NativeRow{State: "applied", ExecutionMilliseconds: math.MaxInt64},
			"YDB revision row has a negative or out of range count"},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			record, err := test.row.Record()
			c.Assert(err, qt.ErrorMatches, test.want)
			c.Assert(record, qt.IsNil)
		})
	}
}

// A record the host writes becomes the row the engine wrote for it: the
// direction as a `:down` suffix, no error as NULL, and the run time in
// milliseconds. Reading the row back gives the record with the run time the
// column keeps.
func TestNativeRowOfARecord_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	record := &protocol.HistoryRevision{Version: "7", Description: "Seven", State: protocol.HistoryRevision_FAILED, Direction: protocol.HistoryRevision_DOWN,
		Applied: 2, Total: 5, ErrorStatement: "DROP TABLE t", ExecutionNanoseconds: 1234567890, Checksum: "c",
		AppliedAtSeconds: recordedAt.Unix(), AppliedAtNanos: 520812000}

	row, err := history.NativeRowOf(record)
	c.Assert(err, qt.IsNil)
	back, err := row.Record()

	c.Assert(row, qt.DeepEquals, history.NativeRow{Version: 7, Description: "Seven", AppliedAt: recordedAt, State: "failed:down", Applied: 2, Total: 5,
		ErrorStatement: text("DROP TABLE t"), ExecutionMilliseconds: 1234, Checksum: "c"})
	c.Assert(err, qt.IsNil)
	record.ExecutionNanoseconds = 1234000000
	c.Assert(back, qt.CmpEquals(protocmp.Transform()), record)
}

// A record with a fact the engine's native layout has no column for, or with
// no state or direction, is refused before anything is written.
func TestNativeRowOfARecord_FailurePath(t *testing.T) {
	applied := func() *protocol.HistoryRevision {
		return &protocol.HistoryRevision{Version: "1", State: protocol.HistoryRevision_APPLIED, Direction: protocol.HistoryRevision_UP}
	}
	withOperator, withHashes, withType, stateless, directionless, huge, named := applied(), applied(), applied(), applied(), applied(), applied(), applied()
	withOperator.OperatorVersion = "Ptah"
	withHashes.PartialHashes = []string{"h"}
	withType.AtlasType = 2
	stateless.State = protocol.HistoryRevision_STATE_UNSPECIFIED
	directionless.Direction = protocol.HistoryRevision_DIRECTION_UNSPECIFIED
	huge.Applied = math.MaxUint64
	named.Version = "one"
	for _, test := range []struct {
		name   string
		record *protocol.HistoryRevision
		want   string
	}{
		{"an operator version", withOperator, "YDB revision has facts the built-in engine's native revision table cannot hold"},
		{"partial hashes", withHashes, "YDB revision has facts the built-in engine's native revision table cannot hold"},
		{"an Atlas type", withType, "YDB revision has facts the built-in engine's native revision table cannot hold"},
		{"no state", stateless, "YDB revision has no state the built-in engine's native revision table holds"},
		{"no direction", directionless, "YDB revision has no direction"},
		{"a count beyond Int64", huge, "YDB revision counts exceed the built-in engine's columns"},
		{"a version that is no number", named, "invalid native revision identity"},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			row, err := history.NativeRowOf(test.record)
			c.Assert(err, qt.ErrorMatches, test.want)
			c.Assert(row, qt.DeepEquals, history.NativeRow{})
		})
	}
}

// A row of the engine's Atlas revision table reads as an Atlas record with no
// state and no direction; the engine's JSON `null` and a NULL are no partial
// hashes.
func TestAtlasRowReadsAsARecord_HappyPath(t *testing.T) {
	row := func(hashes sql.NullString) history.AtlasRow {
		return history.AtlasRow{Version: "20260101000000", Description: "create", Type: 2, Applied: 1, Total: 1, ExecutedAt: recordedAt,
			ExecutionTime: 99893750, Error: text(""), ErrorStatement: text(""), Hash: "W8W1", PartialHashes: hashes, OperatorVersion: "Ptah"}
	}
	record := func(hashes ...string) *protocol.HistoryRevision {
		return &protocol.HistoryRevision{Version: "20260101000000", Description: "create", AtlasType: 2, Applied: 1, Total: 1, ExecutionNanoseconds: 99893750,
			Checksum: "W8W1", PartialHashes: hashes, OperatorVersion: "Ptah", AppliedAtSeconds: recordedAt.Unix(), AppliedAtNanos: 520812000}
	}
	for _, test := range []struct {
		name string
		row  history.AtlasRow
		want *protocol.HistoryRevision
	}{
		{"JSON null", row(text("null")), record()},
		{"NULL", row(sql.NullString{}), record()},
		{"hashes", row(text(`["a","b"]`)), record("a", "b")},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			got, err := test.row.Record()
			c.Assert(err, qt.IsNil)
			c.Assert(got, qt.CmpEquals(protocmp.Transform()), test.want)
		})
	}
}

// An Atlas row whose hashes are not a JSON array or whose type no record
// holds is refused.
func TestAtlasRowReadsAsARecord_FailurePath(t *testing.T) {
	for _, test := range []struct {
		name string
		row  history.AtlasRow
		want string
	}{
		{"hashes that are no array", history.AtlasRow{PartialHashes: text(`{"a":1}`)}, "YDB revision row has partial hashes that are not a JSON array"},
		{"a negative type", history.AtlasRow{Type: -1}, "YDB revision row has a negative or out of range count"},
		{"a type beyond 32 bits", history.AtlasRow{Type: math.MaxUint32 + 1}, "YDB revision row has a negative or out of range count"},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			got, err := test.row.Record()
			c.Assert(err, qt.ErrorMatches, test.want)
			c.Assert(got, qt.IsNil)
		})
	}
}

// An Atlas record becomes the row the engine wrote for one: an empty error as
// an empty string and no partial hashes as the JSON `null`.
func TestAtlasRowOfARecord_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	record := &protocol.HistoryRevision{Version: "20260102000000", Description: "second", AtlasType: 2, Applied: 1, Total: 1, ExecutionNanoseconds: 5,
		Checksum: "h", OperatorVersion: "Ptah", AppliedAtSeconds: recordedAt.Unix(), AppliedAtNanos: 520812000}
	hashed := &protocol.HistoryRevision{Version: "20260102000000", PartialHashes: []string{"a"}, AppliedAtSeconds: recordedAt.Unix()}

	row, err := history.AtlasRowOf(record)
	c.Assert(err, qt.IsNil)
	hashes, hashedErr := history.AtlasRowOf(hashed)

	c.Assert(row, qt.DeepEquals, history.AtlasRow{Version: "20260102000000", Description: "second", Type: 2, Applied: 1, Total: 1, ExecutedAt: recordedAt,
		ExecutionTime: 5, Error: text(""), ErrorStatement: text(""), Hash: "h", PartialHashes: text("null"), OperatorVersion: "Ptah"})
	c.Assert(hashedErr, qt.IsNil)
	c.Assert(hashes.PartialHashes, qt.Equals, text(`["a"]`))
}

// An Atlas record that states a state or a direction is refused.
func TestAtlasRowOfARecord_FailurePath(t *testing.T) {
	c := testcheck.New(t)
	row, err := history.AtlasRowOf(&protocol.HistoryRevision{Version: "1", State: protocol.HistoryRevision_APPLIED})
	c.Assert(err, qt.ErrorMatches, "an Atlas revision states no state or direction")
	c.Assert(row, qt.DeepEquals, history.AtlasRow{})
}

// builtinLayout is the description YDB gives the engine's native revision
// table named name, as its CREATE TABLE made it.
func builtinLayout(name string) *native.DescribeTableResult {
	primitive := func(t Ydb.Type_PrimitiveTypeId) *Ydb.Type { return &Ydb.Type{Type: &Ydb.Type_TypeId{TypeId: t}} }
	optional := func(t Ydb.Type_PrimitiveTypeId) *Ydb.Type {
		return &Ydb.Type{Type: &Ydb.Type_OptionalType{OptionalType: &Ydb.OptionalType{Item: primitive(t)}}}
	}
	return &native.DescribeTableResult{Self: &Ydb_Scheme.Entry{Name: name, Type: Ydb_Scheme.Entry_TABLE}, StoreType: Ydb_Table.StoreType_STORE_TYPE_ROW,
		PrimaryKey: []string{"version"}, Columns: []*native.ColumnMeta{
			{Name: "version", Type: primitive(Ydb.Type_INT64)}, {Name: "description", Type: primitive(Ydb.Type_UTF8)},
			{Name: "applied_at", Type: primitive(Ydb.Type_TIMESTAMP)}, {Name: "state", Type: primitive(Ydb.Type_UTF8)},
			{Name: "applied", Type: primitive(Ydb.Type_INT64)}, {Name: "total", Type: primitive(Ydb.Type_INT64)},
			{Name: "error", Type: optional(Ydb.Type_UTF8)}, {Name: "error_stmt", Type: optional(Ydb.Type_UTF8)},
			{Name: "execution_time_ms", Type: primitive(Ydb.Type_INT64)}, {Name: "checksum", Type: primitive(Ydb.Type_UTF8)}}}
}

// The engine's native revision table under its default name is left out of a
// read; the same layout under another name, with an attribute, or with a
// column it did not write is not its table.
func TestBuiltinOwnedRecognizesTheEnginesTable(t *testing.T) {
	marked, renamed, notNull, wide := builtinLayout("schema_migrations"), builtinLayout("schema_migrations"), builtinLayout("schema_migrations"),
		builtinLayout("schema_migrations")
	marked.Attributes = map[string]string{"ptah.history": "stokaro/ptah-ydb-plugin/history-v1/native"}
	renamed.Self.Name = "migrations"
	notNull.Columns[6].NotNull = new(true)
	wide.Columns = append(wide.Columns, &native.ColumnMeta{Name: "extra", Type: &Ydb.Type{Type: &Ydb.Type_TypeId{TypeId: Ydb.Type_UTF8}}})
	for _, test := range []struct {
		name  string
		table string
		value *native.DescribeTableResult
		want  bool
	}{
		{"the engine's table", "schema_migrations", builtinLayout("schema_migrations"), true},
		{"a marked table", "schema_migrations", marked, false},
		{"another name", "migrations", renamed, false},
		{"an error column made NOT NULL", "schema_migrations", notNull, false},
		{"a column the engine did not write", "schema_migrations", wide, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			c.Assert(history.BuiltinOwned(test.table, test.value), qt.Equals, test.want)
		})
	}
}
