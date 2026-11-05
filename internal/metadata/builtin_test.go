package metadata_test

import (
	"database/sql"
	"testing"
	"time"

	qt "github.com/frankban/quicktest"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb_Scheme"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb_Table"
	"google.golang.org/protobuf/testing/protocmp"

	"github.com/stokaro/ptah-ydb-plugin/internal/metadata"
	native "github.com/stokaro/ptah-ydb-plugin/internal/nativeprotocol"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
)

// loggedAt is a time to the microsecond YDB's Timestamp keeps.
var loggedAt = time.Date(2026, 11, 5, 19, 53, 19, 387819000, time.UTC)

// A row of the built-in engine's log reads as the record the host keeps, a
// NULL actor, checksum or error as no text, and the record becomes the same
// row again, with NULL for each empty one, as the engine wrote it.
func TestLogRowRoundTrip_HappyPath(t *testing.T) {
	for _, test := range []struct {
		name   string
		row    metadata.LogRow
		record *protocol.MigrationLogRecord
	}{
		{"a started migration", metadata.LogRow{RunID: "17810-dmnjgdnvaq68", Sequence: 1, Operation: "up", Version: 1, State: "started",
			Actor: sql.NullString{String: "ptah", Valid: true}, ActorSource: "process-user", Checksum: sql.NullString{String: "dd91", Valid: true}, LoggedAt: loggedAt},
			&protocol.MigrationLogRecord{RunId: "17810-dmnjgdnvaq68", Sequence: 1, Operation: "up", Version: 1, State: "started", Actor: "ptah",
				ActorSource: "process-user", Checksum: "dd91", AtSeconds: loggedAt.Unix(), AtNanos: 387819000}},
		{"a failure with no actor", metadata.LogRow{RunID: "r", Sequence: 4, Operation: "up", Version: 2, State: "failed", ActorSource: "none",
			LoggedAt: loggedAt, Error: sql.NullString{String: "failed to apply migration 2", Valid: true}},
			&protocol.MigrationLogRecord{RunId: "r", Sequence: 4, Operation: "up", Version: 2, State: "failed", ActorSource: "none",
				AtSeconds: loggedAt.Unix(), AtNanos: 387819000, Error: "failed to apply migration 2"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			c.Assert(test.row.Record(), qt.CmpEquals(protocmp.Transform()), test.record)
			c.Assert(metadata.LogRowOf(test.record), qt.DeepEquals, test.row)
		})
	}
}

// A tag row reads as a tag record and back.
func TestTagRowRoundTrip_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	row := metadata.TagRow{Tag: "v1", Version: 1, RecordedAt: loggedAt}
	record := &protocol.MigrationTagRecord{Name: "v1", Version: 1, RecordedAtSeconds: loggedAt.Unix(), RecordedAtNanos: 387819000}
	c.Assert(row.Record(), qt.CmpEquals(protocmp.Transform()), record)
	c.Assert(metadata.TagRowOf(record), qt.DeepEquals, row)
}

func tagLayout(name string) *native.DescribeTableResult {
	primitive := func(t Ydb.Type_PrimitiveTypeId) *Ydb.Type { return &Ydb.Type{Type: &Ydb.Type_TypeId{TypeId: t}} }
	return &native.DescribeTableResult{Self: &Ydb_Scheme.Entry{Name: name, Type: Ydb_Scheme.Entry_TABLE}, StoreType: Ydb_Table.StoreType_STORE_TYPE_ROW,
		PrimaryKey: []string{"tag"}, Columns: []*native.ColumnMeta{{Name: "tag", Type: primitive(Ydb.Type_UTF8)}, {Name: "version", Type: primitive(Ydb.Type_INT64)},
			{Name: "recorded_at", Type: primitive(Ydb.Type_TIMESTAMP)}}}
}

// The engine's tag table under its default name is left out of a read as a
// tag store; the same layout under another name, or with an attribute, is not
// its table.
func TestBuiltinOwnedRecognizesTheEnginesTagTable(t *testing.T) {
	marked := tagLayout("ptah_migration_tags")
	marked.Attributes = map[string]string{"ptah.metadata": "stokaro/ptah-ydb-plugin/metadata-v1/tags"}
	for _, test := range []struct {
		name  string
		table string
		value *native.DescribeTableResult
		want  protocol.ReservedObject_Purpose
	}{
		{"the engine's table", "ptah_migration_tags", tagLayout("ptah_migration_tags"), protocol.ReservedObject_TAG},
		{"a marked table", "ptah_migration_tags", marked, protocol.ReservedObject_PURPOSE_UNSPECIFIED},
		{"another name", "tags", tagLayout("tags"), protocol.ReservedObject_PURPOSE_UNSPECIFIED},
		{"the log name with the tag layout", "schema_migrations_log", tagLayout("schema_migrations_log"), protocol.ReservedObject_PURPOSE_UNSPECIFIED},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			c.Assert(metadata.BuiltinOwned(test.table, test.value), qt.Equals, test.want)
		})
	}
}
