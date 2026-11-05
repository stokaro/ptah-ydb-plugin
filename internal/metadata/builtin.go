package metadata

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb"
	ydb "github.com/ydb-platform/ydb-go-sdk/v3"
	"github.com/ydb-platform/ydb-go-sdk/v3/types"
	"google.golang.org/protobuf/proto"

	native "github.com/stokaro/ptah-ydb-plugin/internal/nativeprotocol"
	"github.com/stokaro/ptah-ydb-plugin/internal/ownedtable"
)

// The log and tag tables Ptah's built-in YDB engine wrote, under the names it
// gave them by default (internal/revisiontable in Ptah): the log beside the
// native revision table, and the tags beside either revision table. That
// engine created them in the layouts below with no attribute. The provider
// reads and writes such a table in that layout, so the engine can still read
// it.
const (
	BuiltinLogTable  = "schema_migrations_log"
	BuiltinTagsTable = "ptah_migration_tags"
)

// builtinLogColumns are the columns of the built-in engine's log table, in
// the order its CREATE TABLE declares them.
var builtinLogColumns = []ownedtable.Column{
	{Name: "run_id", Type: Ydb.Type_UTF8},
	{Name: "seq", Type: Ydb.Type_INT64},
	{Name: "operation", Type: Ydb.Type_UTF8},
	{Name: "version", Type: Ydb.Type_INT64},
	{Name: "state", Type: Ydb.Type_UTF8},
	{Name: "actor", Type: Ydb.Type_UTF8, Nullable: true},
	{Name: "actor_source", Type: Ydb.Type_UTF8},
	{Name: "checksum", Type: Ydb.Type_UTF8, Nullable: true},
	{Name: "logged_at", Type: Ydb.Type_TIMESTAMP},
	{Name: "error", Type: Ydb.Type_UTF8, Nullable: true},
}

// builtinTagColumns are the columns of the built-in engine's tag table.
var builtinTagColumns = []ownedtable.Column{
	{Name: "tag", Type: Ydb.Type_UTF8},
	{Name: "version", Type: Ydb.Type_INT64},
	{Name: "recorded_at", Type: Ydb.Type_TIMESTAMP},
}

// builtinDefinition is the built-in engine's layout of the store's kind,
// which carries no ownership attribute.
func (s Store) builtinDefinition() ownedtable.Definition {
	if s.Kind == protocol.MetadataScope_LOG {
		return ownedtable.Definition{Identity: s.Identity, Columns: builtinLogColumns, PrimaryKey: []string{"run_id", "seq"}}
	}
	return ownedtable.Definition{Identity: s.Identity, Columns: builtinTagColumns, PrimaryKey: []string{"tag"}}
}

// Builtin reports a store in the built-in engine's layout, which
// [Store.Locate] found.
func (s Store) Builtin() bool { return s.builtin }

// layoutOf is the store that reads the described table: the provider's own
// layout when the table carries the metadata marker, and the built-in
// engine's layout of the store's kind when it carries no attribute and holds
// exactly that layout. Any other table is refused.
func (s Store) layoutOf(value *native.DescribeTableResult) (Store, error) {
	if len(value.GetAttributes()) != 0 {
		return s, s.ValidateLayout(value)
	}
	if err := s.builtinDefinition().ValidateLayout(value); err != nil {
		return s, s.ValidateLayout(value)
	}
	s.builtin = true
	return s, nil
}

// Locate establishes the store's absence, or describes it and returns the
// store that reads it in the layout it holds (see layoutOf).
func (s Store) Locate(ctx context.Context, driver *ydb.Driver) (Store, bool, error) {
	located := s
	exists, err := s.Identity.Check(ctx, driver, func(value *native.DescribeTableResult) error {
		var err error
		located, err = s.layoutOf(value)
		return err
	})
	if err != nil || !exists {
		return s, false, err
	}
	return located, true, nil
}

// Prepare creates absent storage once and confirms its layout. A table in
// the built-in engine's layout is the store already, and is kept as it is.
func (s Store) Prepare(ctx context.Context, driver *ydb.Driver, beforeCreate func(context.Context) error) (bool, error) {
	return s.Identity.Prepare(ctx, driver, s.CreateRequest(), func(value *native.DescribeTableResult) error {
		_, err := s.layoutOf(value)
		return err
	}, beforeCreate)
}

// BuiltinOwned is the purpose of a table a read leaves out as the built-in
// engine's log or tag table: one of its default names, no attribute and
// exactly its layout. The engine's reader left those names out of every
// schema it read. It is unspecified for any other table.
func BuiltinOwned(name string, value *native.DescribeTableResult) protocol.ReservedObject_Purpose {
	kinds := map[string]protocol.MetadataScope_Kind{BuiltinLogTable: protocol.MetadataScope_LOG, BuiltinTagsTable: protocol.MetadataScope_TAGS}
	kind, named := kinds[name]
	if !named || len(value.GetAttributes()) != 0 {
		return protocol.ReservedObject_PURPOSE_UNSPECIFIED
	}
	if definition(ownedtable.Identity{Name: name}, kind).builtinDefinition().ValidateLayout(value) != nil {
		return protocol.ReservedObject_PURPOSE_UNSPECIFIED
	}
	if kind == protocol.MetadataScope_LOG {
		return protocol.ReservedObject_LOG
	}
	return protocol.ReservedObject_TAG
}

// LogRow is one row of the built-in engine's log table.
type LogRow struct {
	RunID       string
	Sequence    int64
	Operation   string
	Version     int64
	State       string
	Actor       sql.NullString
	ActorSource string
	Checksum    sql.NullString
	LoggedAt    time.Time
	Error       sql.NullString
}

// Record is the row as a log record. The engine kept the time to the
// microsecond.
func (r LogRow) Record() *protocol.MigrationLogRecord {
	return &protocol.MigrationLogRecord{RunId: r.RunID, Sequence: r.Sequence, Operation: r.Operation, Version: r.Version, State: r.State,
		Actor: r.Actor.String, ActorSource: r.ActorSource, Checksum: r.Checksum.String, AtSeconds: r.LoggedAt.Unix(),
		AtNanos: int32(r.LoggedAt.Nanosecond()), Error: r.Error.String} // #nosec G115 -- Nanosecond is below 1e9
}

// LogRowOf is the row the engine's layout holds for record, with NULL for an
// empty actor, checksum or error, as the engine wrote them.
func LogRowOf(record *protocol.MigrationLogRecord) LogRow {
	return LogRow{RunID: record.RunId, Sequence: record.Sequence, Operation: record.Operation, Version: record.Version, State: record.State,
		Actor: nullable(record.Actor), ActorSource: record.ActorSource, Checksum: nullable(record.Checksum),
		LoggedAt: time.Unix(record.AtSeconds, int64(record.AtNanos)).UTC(), Error: nullable(record.Error)}
}

// TagRow is one row of the built-in engine's tag table.
type TagRow struct {
	Tag        string
	Version    int64
	RecordedAt time.Time
}

// Record is the row as a tag record. The engine kept the time to the
// microsecond.
func (r TagRow) Record() *protocol.MigrationTagRecord {
	return &protocol.MigrationTagRecord{Name: r.Tag, Version: r.Version, RecordedAtSeconds: r.RecordedAt.Unix(),
		RecordedAtNanos: int32(r.RecordedAt.Nanosecond())} // #nosec G115 -- Nanosecond is below 1e9
}

// TagRowOf is the row the engine's layout holds for record.
func TagRowOf(record *protocol.MigrationTagRecord) TagRow {
	return TagRow{Tag: record.Name, Version: record.Version, RecordedAt: time.Unix(record.RecordedAtSeconds, int64(record.RecordedAtNanos)).UTC()}
}

// nullable is text as an optional column holds it: NULL for none.
func nullable(text string) sql.NullString { return sql.NullString{String: text, Valid: text != ""} }

// optionalText is a nullable text column's value.
func optionalText(value sql.NullString) types.Value {
	if !value.Valid {
		return types.NullValue(types.TypeText)
	}
	return types.OptionalValue(types.TextValue(value.String))
}

// timestamp is t as YDB's Timestamp holds it, refused outside its range.
func timestamp(t time.Time) (types.Value, error) {
	if t.Before(time.Unix(0, 0)) || t.After(time.Date(2105, 12, 31, 23, 59, 59, 0, time.UTC)) {
		return nil, fmt.Errorf("YDB metadata time is outside the range of the built-in engine's Timestamp column")
	}
	return types.TimestampValueFromTime(t), nil
}

// scanner reads one row of a query, as *sql.Rows and *sql.Row do.
type scanner interface{ Scan(...any) error }

// columnList is the store's columns in the engine's layout, quoted.
func (s Store) columnList() string {
	columns := s.builtinDefinition().Columns
	names := make([]string, 0, len(columns))
	for _, column := range columns {
		names = append(names, "`"+column.Name+"`")
	}
	return strings.Join(names, ", ")
}

// scanBuiltin reads one row of the engine's layout as a record.
func (s Store) scanBuiltin(row scanner) (proto.Message, error) {
	if s.Kind == protocol.MetadataScope_LOG {
		var r LogRow
		if err := row.Scan(&r.RunID, &r.Sequence, &r.Operation, &r.Version, &r.State, &r.Actor, &r.ActorSource, &r.Checksum, &r.LoggedAt, &r.Error); err != nil {
			return nil, err
		}
		return r.Record(), nil
	}
	var r TagRow
	if err := row.Scan(&r.Tag, &r.Version, &r.RecordedAt); err != nil {
		return nil, err
	}
	return r.Record(), nil
}

// lookupBuiltin is [Store.Lookup] of a table in the engine's layout.
func (s Store) lookupBuiltin(ctx context.Context, runner Runner, name string) (*protocol.MigrationTagRecord, error) {
	record, err := s.scanBuiltin(runner.QueryRowContext(ctx, "DECLARE $name AS Utf8; SELECT "+s.columnList()+" FROM "+s.QuotedPath()+" WHERE `tag` = $name",
		sql.Named("name", name)))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read YDB metadata tag: query failed")
	}
	tag, ok := record.(*protocol.MigrationTagRecord)
	if !ok || tag.Name != name {
		return nil, fmt.Errorf("YDB metadata tag identity changed")
	}
	if _, err := s.Encode(tag); err != nil {
		return nil, err
	}
	return tag, nil
}

// appendBuiltinLog is [Store.appendLog] of a table in the engine's layout.
func (s Store) appendBuiltinLog(ctx context.Context, runner Runner, row *protocol.MigrationLogRecord, beforeWrite func(context.Context) error) (protocol.MetadataResult_Outcome, bool, error) {
	if _, err := s.Encode(row); err != nil {
		return protocol.MetadataResult_REJECTED, false, err
	}
	stored := LogRowOf(row)
	at, err := timestamp(stored.LoggedAt)
	if err != nil {
		return protocol.MetadataResult_REJECTED, false, err
	}
	existing, err := s.scanBuiltin(runner.QueryRowContext(ctx, "DECLARE $run AS Utf8; DECLARE $sequence AS Int64; SELECT "+s.columnList()+" FROM "+
		s.QuotedPath()+" WHERE `run_id` = $run AND `seq` = $sequence", sql.Named("run", row.RunId), sql.Named("sequence", row.Sequence)))
	switch {
	case err == nil:
		event, ok := existing.(*protocol.MigrationLogRecord)
		if !ok || event.RunId != row.RunId || event.Sequence != row.Sequence {
			return protocol.MetadataResult_REJECTED, false, fmt.Errorf("YDB log identity changed")
		}
		return protocol.MetadataResult_REJECTED, false, nil
	case !errors.Is(err, sql.ErrNoRows):
		return protocol.MetadataResult_REJECTED, false, fmt.Errorf("read YDB log identity: query failed")
	}
	if err := beforeWrite(ctx); err != nil {
		return protocol.MetadataResult_REJECTED, false, err
	}
	if err := ctx.Err(); err != nil {
		return protocol.MetadataResult_REJECTED, false, err
	}
	_, err = runner.ExecContext(ctx, "DECLARE $run_id AS Utf8; DECLARE $seq AS Int64; DECLARE $operation AS Utf8; DECLARE $version AS Int64; "+
		"DECLARE $state AS Utf8; DECLARE $actor AS Utf8?; DECLARE $actor_source AS Utf8; DECLARE $checksum AS Utf8?; DECLARE $logged_at AS Timestamp; "+
		"DECLARE $error AS Utf8?; INSERT INTO "+s.QuotedPath()+" ("+s.columnList()+") VALUES "+
		"($run_id, $seq, $operation, $version, $state, $actor, $actor_source, $checksum, $logged_at, $error)",
		sql.Named("run_id", stored.RunID), sql.Named("seq", stored.Sequence), sql.Named("operation", stored.Operation), sql.Named("version", stored.Version),
		sql.Named("state", stored.State), sql.Named("actor", optionalText(stored.Actor)), sql.Named("actor_source", stored.ActorSource),
		sql.Named("checksum", optionalText(stored.Checksum)), sql.Named("logged_at", at), sql.Named("error", optionalText(stored.Error)))
	if err != nil {
		return protocol.MetadataResult_UNKNOWN, false, fmt.Errorf("append YDB log: query failed")
	}
	return protocol.MetadataResult_APPLIED, false, nil
}

// putBuiltinTag is [Store.putTag] of a table in the engine's layout.
func (s Store) putBuiltinTag(ctx context.Context, runner Runner, row *protocol.MigrationTagRecord, beforeWrite func(context.Context) error) (protocol.MetadataResult_Outcome, bool, error) {
	if _, err := s.Encode(row); err != nil {
		return protocol.MetadataResult_REJECTED, false, err
	}
	stored := TagRowOf(row)
	at, err := timestamp(stored.RecordedAt)
	if err != nil {
		return protocol.MetadataResult_REJECTED, false, err
	}
	if _, err := s.lookupBuiltin(ctx, runner, row.Name); err != nil {
		return protocol.MetadataResult_REJECTED, false, err
	}
	if err := beforeWrite(ctx); err != nil {
		return protocol.MetadataResult_REJECTED, false, err
	}
	if err := ctx.Err(); err != nil {
		return protocol.MetadataResult_REJECTED, false, err
	}
	_, err = runner.ExecContext(ctx, "DECLARE $tag AS Utf8; DECLARE $version AS Int64; DECLARE $recorded_at AS Timestamp; UPSERT INTO "+s.QuotedPath()+
		" (`tag`, `version`, `recorded_at`) VALUES ($tag, $version, $recorded_at)",
		sql.Named("tag", stored.Tag), sql.Named("version", stored.Version), sql.Named("recorded_at", at))
	if err != nil {
		return protocol.MetadataResult_UNKNOWN, false, fmt.Errorf("write YDB tag: query failed")
	}
	return protocol.MetadataResult_APPLIED, false, nil
}

// deleteBuiltinTag is [Store.deleteTag] of a table in the engine's layout.
func (s Store) deleteBuiltinTag(ctx context.Context, runner Runner, name string, beforeWrite func(context.Context) error) (protocol.MetadataResult_Outcome, bool, error) {
	row, err := s.lookupBuiltin(ctx, runner, name)
	if err != nil {
		return protocol.MetadataResult_REJECTED, false, err
	}
	if row == nil {
		return protocol.MetadataResult_APPLIED, false, nil
	}
	if err := beforeWrite(ctx); err != nil {
		return protocol.MetadataResult_REJECTED, false, err
	}
	if err := ctx.Err(); err != nil {
		return protocol.MetadataResult_REJECTED, false, err
	}
	if _, err := runner.ExecContext(ctx, "DECLARE $name AS Utf8; DELETE FROM "+s.QuotedPath()+" WHERE `tag` = $name", sql.Named("name", name)); err != nil {
		return protocol.MetadataResult_UNKNOWN, false, fmt.Errorf("delete YDB tag: query failed")
	}
	return protocol.MetadataResult_APPLIED, true, nil
}

// captureBuiltin is [Store.capture] of a table in the engine's layout: each
// row becomes the record the provider's own layout would hold, so the
// snapshot reads it as any other.
func (s Store) captureBuiltin(ctx context.Context, rows *sql.Rows, snapshot *Snapshot, expected uint64) (uint64, error) {
	var count uint64
	for rows.Next() {
		if err := ctx.Err(); err != nil {
			return 0, err
		}
		record, err := s.scanBuiltin(rows)
		if err != nil {
			return 0, fmt.Errorf("read YDB metadata: invalid row")
		}
		payload, err := s.Encode(record)
		if err != nil {
			return 0, err
		}
		if count >= expected {
			return 0, fmt.Errorf("YDB metadata count changed")
		}
		if err := snapshot.append(payload); err != nil {
			return 0, err
		}
		count++
	}
	return count, nil
}

// builtinSnapshotQuery reads every row of the engine's layout in key order.
func (s Store) builtinSnapshotQuery() string {
	order := "`tag`"
	if s.Kind == protocol.MetadataScope_LOG {
		order = "`run_id`, `seq`"
	}
	return "SELECT " + s.columnList() + " FROM " + s.QuotedPath() + " ORDER BY " + order
}
