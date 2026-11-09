package history

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb"
	ydb "github.com/ydb-platform/ydb-go-sdk/v3"
	"github.com/ydb-platform/ydb-go-sdk/v3/types"
	"google.golang.org/protobuf/proto"
	protocol "ptah.run/provider-sdk/protocol/experimental"

	native "github.com/stokaro/ptah-ydb-plugin/internal/nativeprotocol"
	"github.com/stokaro/ptah-ydb-plugin/internal/ownedtable"
)

// The revision tables Ptah's built-in YDB engine wrote, under the names it
// gave them by default (internal/revisiontable in Ptah): the native one and
// the Atlas-compatible one. That engine created them with CREATE TABLE IF NOT
// EXISTS in the layouts below and no attribute, and every database it
// migrated holds one. The provider reads and writes such a table in that
// layout, so the history it holds keeps its meaning and the engine can still
// read it.
const (
	BuiltinNativeTable = "schema_migrations"
	BuiltinAtlasTable  = "atlas_schema_revisions"
)

// builtinNativeColumns are the columns of the built-in engine's native
// revision table, in the order its CREATE TABLE declares them.
var builtinNativeColumns = []ownedtable.Column{
	{Name: "version", Type: Ydb.Type_INT64},
	{Name: "description", Type: Ydb.Type_UTF8},
	{Name: "applied_at", Type: Ydb.Type_TIMESTAMP},
	{Name: "state", Type: Ydb.Type_UTF8},
	{Name: "applied", Type: Ydb.Type_INT64},
	{Name: "total", Type: Ydb.Type_INT64},
	{Name: "error", Type: Ydb.Type_UTF8, Nullable: true},
	{Name: "error_stmt", Type: Ydb.Type_UTF8, Nullable: true},
	{Name: "execution_time_ms", Type: Ydb.Type_INT64},
	{Name: "checksum", Type: Ydb.Type_UTF8},
}

// builtinAtlasColumns are the columns of the built-in engine's Atlas
// revision table, in the order its CREATE TABLE declares them.
var builtinAtlasColumns = []ownedtable.Column{
	{Name: "version", Type: Ydb.Type_UTF8},
	{Name: "description", Type: Ydb.Type_UTF8},
	{Name: "type", Type: Ydb.Type_INT64},
	{Name: "applied", Type: Ydb.Type_INT64},
	{Name: "total", Type: Ydb.Type_INT64},
	{Name: "executed_at", Type: Ydb.Type_TIMESTAMP},
	{Name: "execution_time", Type: Ydb.Type_INT64},
	{Name: "error", Type: Ydb.Type_UTF8, Nullable: true},
	{Name: "error_stmt", Type: Ydb.Type_UTF8, Nullable: true},
	{Name: "hash", Type: Ydb.Type_UTF8},
	{Name: "partial_hashes", Type: Ydb.Type_UTF8, Nullable: true},
	{Name: "operator_version", Type: Ydb.Type_UTF8},
}

// builtinDefinition is the built-in engine's layout of the store's format,
// which carries no ownership attribute.
func (s Store) builtinDefinition() ownedtable.Definition {
	columns := builtinNativeColumns
	if s.Format == protocol.HistoryScope_ATLAS {
		columns = builtinAtlasColumns
	}
	return ownedtable.Definition{Identity: s.Identity, Columns: columns, PrimaryKey: []string{"version"}}
}

// Builtin reports a store in the built-in engine's layout, which [Store.Locate]
// found.
func (s Store) Builtin() bool { return s.builtin }

// layoutOf is the store that reads the described table: the provider's own
// layout when the table carries the history marker, and the built-in
// engine's layout of the store's format when it carries no attribute and
// holds exactly that layout. Any other table is refused.
func (s Store) layoutOf(value *native.DescribeTableResult) (Store, error) {
	if _, marked := value.GetAttributes()[ownershipAttribute]; marked || len(value.GetAttributes()) != 0 {
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

// BuiltinOwned reports a table a read leaves out as the built-in engine's
// revision table: one of its default names, no attribute and exactly its
// layout. The engine's reader left those names out of every schema it read,
// so a plan never dropped the record of the migrations a database ran.
func BuiltinOwned(name string, value *native.DescribeTableResult) bool {
	format := map[string]protocol.HistoryScope_Format{BuiltinNativeTable: protocol.HistoryScope_NATIVE, BuiltinAtlasTable: protocol.HistoryScope_ATLAS}
	chosen, named := format[name]
	if !named {
		return false
	}
	store := Store{Identity: ownedtable.Identity{Name: name}, Format: chosen}
	return store.builtinDefinition().ValidateLayout(value) == nil
}

// NativeRow is one row of the built-in engine's native revision table.
type NativeRow struct {
	Version     int64
	Description string
	AppliedAt   time.Time
	// State is the revision's state, `applied`, `pending` or `failed`, with
	// `:down` after it for a row a rollback wrote, as the engine encoded the
	// direction in the column.
	State                 string
	Applied, Total        int64
	Error, ErrorStatement sql.NullString
	ExecutionMilliseconds int64
	Checksum              string
}

// builtinStates map the engine's states to the record's, and back.
var builtinStates = map[string]protocol.HistoryRevision_State{
	"applied": protocol.HistoryRevision_APPLIED, "pending": protocol.HistoryRevision_PENDING, "failed": protocol.HistoryRevision_FAILED,
}

// Record is the row as a history record. The engine kept the time a
// revision was applied to the microsecond and its run time to the
// millisecond, so the record carries no finer part.
func (r NativeRow) Record() (*protocol.HistoryRevision, error) {
	stored, suffix, directed := strings.Cut(r.State, ":")
	state, known := builtinStates[stored]
	direction := protocol.HistoryRevision_UP
	switch {
	case !known:
		return nil, fmt.Errorf("YDB revision row has an unknown state")
	case directed && suffix == "down":
		direction = protocol.HistoryRevision_DOWN
	case directed && suffix != "up":
		return nil, fmt.Errorf("YDB revision row has an unknown direction")
	}
	if r.Applied < 0 || r.Total < 0 || r.ExecutionMilliseconds < 0 || r.ExecutionMilliseconds > math.MaxInt64/int64(time.Millisecond) {
		return nil, fmt.Errorf("YDB revision row has a negative or out of range count")
	}
	return &protocol.HistoryRevision{Version: strconv.FormatInt(r.Version, 10), Description: r.Description, State: state, Direction: direction,
		Applied: uint64(r.Applied), Total: uint64(r.Total), Error: r.Error.String, ErrorStatement: r.ErrorStatement.String,
		ExecutionNanoseconds: r.ExecutionMilliseconds * int64(time.Millisecond), Checksum: r.Checksum,
		AppliedAtSeconds: r.AppliedAt.Unix(), AppliedAtNanos: int32(r.AppliedAt.Nanosecond())}, nil // #nosec G115 -- Nanosecond is below 1e9
}

// NativeRowOf is the row the engine's layout holds for record. It refuses a
// fact the layout has no column for, an operator version, partial hashes or
// an Atlas type, which no native record carries. The run time is kept to the
// millisecond, as the engine kept it.
func NativeRowOf(record *protocol.HistoryRevision) (NativeRow, error) {
	if record.OperatorVersion != "" || len(record.PartialHashes) != 0 || record.AtlasType != 0 {
		return NativeRow{}, fmt.Errorf("YDB revision has facts the built-in engine's native revision table cannot hold")
	}
	version, err := strconv.ParseInt(record.Version, 10, 64)
	if err != nil {
		return NativeRow{}, fmt.Errorf("invalid native revision identity")
	}
	var state string
	for name, value := range builtinStates {
		if value == record.State {
			state = name
		}
	}
	switch {
	case state == "":
		return NativeRow{}, fmt.Errorf("YDB revision has no state the built-in engine's native revision table holds")
	case record.Direction == protocol.HistoryRevision_DOWN:
		state += ":down"
	case record.Direction != protocol.HistoryRevision_UP:
		return NativeRow{}, fmt.Errorf("YDB revision has no direction")
	}
	applied, total, err := counts(record)
	if err != nil {
		return NativeRow{}, err
	}
	return NativeRow{Version: version, Description: record.Description, AppliedAt: time.Unix(record.AppliedAtSeconds, int64(record.AppliedAtNanos)).UTC(),
		State: state, Applied: applied, Total: total, Error: nullable(record.Error), ErrorStatement: nullable(record.ErrorStatement),
		ExecutionMilliseconds: record.ExecutionNanoseconds / int64(time.Millisecond), Checksum: record.Checksum}, nil
}

// AtlasRow is one row of the built-in engine's Atlas revision table.
type AtlasRow struct {
	Version, Description string
	Type, Applied, Total int64
	ExecutedAt           time.Time
	// ExecutionTime is the run time in nanoseconds.
	ExecutionTime         int64
	Error, ErrorStatement sql.NullString
	Hash                  string
	// PartialHashes is a JSON array of the hashes, or the JSON `null` the
	// engine wrote for none.
	PartialHashes   sql.NullString
	OperatorVersion string
}

// Record is the row as an Atlas history record, which states no state and
// no direction. The engine kept the time a revision ran to the microsecond.
func (r AtlasRow) Record() (*protocol.HistoryRevision, error) {
	if r.Type < 0 || r.Type > math.MaxUint32 || r.Applied < 0 || r.Total < 0 {
		return nil, fmt.Errorf("YDB revision row has a negative or out of range count")
	}
	var hashes []string
	if r.PartialHashes.Valid {
		if err := json.Unmarshal([]byte(r.PartialHashes.String), &hashes); err != nil {
			return nil, fmt.Errorf("YDB revision row has partial hashes that are not a JSON array")
		}
	}
	return &protocol.HistoryRevision{Version: r.Version, Description: r.Description, AtlasType: uint32(r.Type), Applied: uint64(r.Applied), Total: uint64(r.Total),
		Error: r.Error.String, ErrorStatement: r.ErrorStatement.String, ExecutionNanoseconds: r.ExecutionTime, Checksum: r.Hash, PartialHashes: hashes,
		OperatorVersion: r.OperatorVersion, AppliedAtSeconds: r.ExecutedAt.Unix(), AppliedAtNanos: int32(r.ExecutedAt.Nanosecond())}, nil // #nosec G115 -- Nanosecond is below 1e9
}

// AtlasRowOf is the row the engine's Atlas layout holds for record, written
// as the engine wrote one: an empty error as an empty string, and no partial
// hashes as the JSON `null`.
func AtlasRowOf(record *protocol.HistoryRevision) (AtlasRow, error) {
	if record.State != protocol.HistoryRevision_STATE_UNSPECIFIED || record.Direction != protocol.HistoryRevision_DIRECTION_UNSPECIFIED {
		return AtlasRow{}, fmt.Errorf("an Atlas revision states no state or direction")
	}
	applied, total, err := counts(record)
	if err != nil {
		return AtlasRow{}, err
	}
	hashes := []byte("null")
	if len(record.PartialHashes) != 0 {
		hashes, err = json.Marshal(record.PartialHashes)
		if err != nil {
			return AtlasRow{}, err
		}
	}
	return AtlasRow{Version: record.Version, Description: record.Description, Type: int64(record.AtlasType), Applied: applied, Total: total,
		ExecutedAt: time.Unix(record.AppliedAtSeconds, int64(record.AppliedAtNanos)).UTC(), ExecutionTime: record.ExecutionNanoseconds,
		Error: sql.NullString{String: record.Error, Valid: true}, ErrorStatement: sql.NullString{String: record.ErrorStatement, Valid: true},
		Hash: record.Checksum, PartialHashes: sql.NullString{String: string(hashes), Valid: true}, OperatorVersion: record.OperatorVersion}, nil
}

// counts are a record's applied and total counts as the engine's Int64
// columns hold them.
func counts(record *protocol.HistoryRevision) (applied, total int64, err error) {
	if record.Applied > math.MaxInt64 || record.Total > math.MaxInt64 {
		return 0, 0, fmt.Errorf("YDB revision counts exceed the built-in engine's columns")
	}
	return int64(record.Applied), int64(record.Total), nil // #nosec G115 -- both are at most MaxInt64, checked above
}

// nullable is text as an optional column holds it: NULL for none, as the
// engine wrote its native table's error columns.
func nullable(text string) sql.NullString { return sql.NullString{String: text, Valid: text != ""} }

// timestamp is t as YDB's Timestamp holds it, refused outside its range.
func timestamp(t time.Time) (types.Value, error) {
	if t.Before(time.Unix(0, 0)) || t.After(time.Date(2105, 12, 31, 23, 59, 59, 0, time.UTC)) {
		return nil, fmt.Errorf("YDB revision time is outside the range of the built-in engine's Timestamp column")
	}
	return types.TimestampValueFromTime(t), nil
}

// optionalText is a nullable text column's value.
func optionalText(value sql.NullString) types.Value {
	if !value.Valid {
		return types.NullValue(types.TypeText)
	}
	return types.OptionalValue(types.TextValue(value.String))
}

// builtinSelect is the query that reads every column of the store's rows in
// the engine's layout, with no condition and no order.
func (s Store) builtinSelect() string {
	columns := make([]string, 0, len(builtinNativeColumns))
	for _, column := range s.builtinDefinition().Columns {
		columns = append(columns, "`"+column.Name+"`")
	}
	return "SELECT " + strings.Join(columns, ", ") + " FROM " + s.QuotedPath()
}

// builtinSelectRow is the query that reads the row of one version.
func (s Store) builtinSelectRow() string {
	return "DECLARE $version AS " + s.KeyType() + "; " + s.builtinSelect() + " WHERE `version` = $version"
}

// scanner reads one row of a query, as *sql.Rows and *sql.Row do.
type scanner interface{ Scan(...any) error }

// scanBuiltin reads one row of the engine's layout as a record.
func (s Store) scanBuiltin(row scanner) (*protocol.HistoryRevision, error) {
	if s.Format == protocol.HistoryScope_NATIVE {
		var r NativeRow
		if err := row.Scan(&r.Version, &r.Description, &r.AppliedAt, &r.State, &r.Applied, &r.Total, &r.Error, &r.ErrorStatement,
			&r.ExecutionMilliseconds, &r.Checksum); err != nil {
			return nil, err
		}
		return r.Record()
	}
	var r AtlasRow
	if err := row.Scan(&r.Version, &r.Description, &r.Type, &r.Applied, &r.Total, &r.ExecutedAt, &r.ExecutionTime, &r.Error, &r.ErrorStatement,
		&r.Hash, &r.PartialHashes, &r.OperatorVersion); err != nil {
		return nil, err
	}
	return r.Record()
}

// readBuiltin is [Store.Read] of a table in the engine's layout.
func (s Store) readBuiltin(ctx context.Context, runner Runner, limit uint32) (result []*protocol.HistoryRevision, err error) {
	rows, err := runner.QueryContext(ctx, s.builtinSelect()+" ORDER BY `version` LIMIT "+strconv.FormatUint(uint64(limit)+1, 10))
	if err != nil {
		return nil, fmt.Errorf("read YDB history: query failed")
	}
	defer func() {
		err = errors.Join(err, rows.Close())
		if err != nil {
			result = nil
		}
	}()
	size := 0
	for rows.Next() {
		if len(result) >= int(limit) {
			return nil, fmt.Errorf("YDB history exceeds revision limit")
		}
		record, err := s.scanBuiltin(rows)
		if err != nil {
			return nil, fmt.Errorf("read YDB history: invalid row: %w", err)
		}
		if _, err := s.Encode(record); err != nil {
			return nil, err
		}
		size += proto.Size(record) + 8
		if size > MaxSnapshotBytes {
			return nil, fmt.Errorf("YDB history exceeds snapshot size limit")
		}
		result = append(result, record)
	}
	if rows.Err() != nil {
		return nil, fmt.Errorf("read YDB history: stream failed")
	}
	return result, nil
}

// mutateBuiltin is [Store.Mutate] of a table in the engine's layout: the
// stored row, read as a record, must equal the expected one, and the
// replacement is written as the engine wrote a row.
func (s Store) mutateBuiltin(ctx context.Context, runner Runner, request *protocol.MutateHistoryRequest, version string, key any) (protocol.HistoryResult_Outcome, error) {
	current, err := s.scanBuiltin(runner.QueryRowContext(ctx, s.builtinSelectRow(), sql.Named("version", key)))
	switch {
	case errors.Is(err, sql.ErrNoRows):
		current = nil
	case err != nil:
		return protocol.HistoryResult_UNKNOWN, fmt.Errorf("compare YDB history: query failed")
	case current.Version != version:
		return protocol.HistoryResult_REFUSED, fmt.Errorf("YDB revision identity changed")
	}
	if !proto.Equal(current, request.Expected) {
		return protocol.HistoryResult_CONFLICT, nil
	}
	query := "DECLARE $version AS " + s.KeyType() + "; DELETE FROM " + s.QuotedPath() + " WHERE `version` = $version"
	args := []any{sql.Named("version", key)}
	if request.Replacement != nil {
		query, args, err = s.builtinUpsert(request.Replacement)
		if err != nil {
			return protocol.HistoryResult_REFUSED, err
		}
	}
	if _, err := runner.ExecContext(ctx, query, args...); err != nil {
		return protocol.HistoryResult_UNKNOWN, fmt.Errorf("write YDB history: query failed")
	}
	return protocol.HistoryResult_APPLIED, nil
}

// builtinUpsert is the statement that writes record as a row of the engine's
// layout, with its arguments.
func (s Store) builtinUpsert(record *protocol.HistoryRevision) (string, []any, error) {
	if s.Format == protocol.HistoryScope_NATIVE {
		row, err := NativeRowOf(record)
		if err != nil {
			return "", nil, err
		}
		at, err := timestamp(row.AppliedAt)
		if err != nil {
			return "", nil, err
		}
		return "DECLARE $version AS Int64; DECLARE $description AS Utf8; DECLARE $applied_at AS Timestamp; DECLARE $state AS Utf8; " +
				"DECLARE $applied AS Int64; DECLARE $total AS Int64; DECLARE $error AS Utf8?; DECLARE $error_stmt AS Utf8?; " +
				"DECLARE $execution_time_ms AS Int64; DECLARE $checksum AS Utf8; UPSERT INTO " + s.QuotedPath() +
				" (`version`, `description`, `applied_at`, `state`, `applied`, `total`, `error`, `error_stmt`, `execution_time_ms`, `checksum`) " +
				"VALUES ($version, $description, $applied_at, $state, $applied, $total, $error, $error_stmt, $execution_time_ms, $checksum)",
			[]any{sql.Named("version", row.Version), sql.Named("description", row.Description), sql.Named("applied_at", at), sql.Named("state", row.State),
				sql.Named("applied", row.Applied), sql.Named("total", row.Total), sql.Named("error", optionalText(row.Error)),
				sql.Named("error_stmt", optionalText(row.ErrorStatement)), sql.Named("execution_time_ms", row.ExecutionMilliseconds), sql.Named("checksum", row.Checksum)}, nil
	}
	row, err := AtlasRowOf(record)
	if err != nil {
		return "", nil, err
	}
	at, err := timestamp(row.ExecutedAt)
	if err != nil {
		return "", nil, err
	}
	return "DECLARE $version AS Utf8; DECLARE $description AS Utf8; DECLARE $type AS Int64; DECLARE $applied AS Int64; DECLARE $total AS Int64; " +
			"DECLARE $executed_at AS Timestamp; DECLARE $execution_time AS Int64; DECLARE $error AS Utf8?; DECLARE $error_stmt AS Utf8?; " +
			"DECLARE $hash AS Utf8; DECLARE $partial_hashes AS Utf8?; DECLARE $operator_version AS Utf8; UPSERT INTO " + s.QuotedPath() +
			" (`version`, `description`, `type`, `applied`, `total`, `executed_at`, `execution_time`, `error`, `error_stmt`, `hash`, `partial_hashes`, `operator_version`) " +
			"VALUES ($version, $description, $type, $applied, $total, $executed_at, $execution_time, $error, $error_stmt, $hash, $partial_hashes, $operator_version)",
		[]any{sql.Named("version", row.Version), sql.Named("description", row.Description), sql.Named("type", row.Type), sql.Named("applied", row.Applied),
			sql.Named("total", row.Total), sql.Named("executed_at", at), sql.Named("execution_time", row.ExecutionTime), sql.Named("error", optionalText(row.Error)),
			sql.Named("error_stmt", optionalText(row.ErrorStatement)), sql.Named("hash", row.Hash), sql.Named("partial_hashes", optionalText(row.PartialHashes)),
			sql.Named("operator_version", row.OperatorVersion)}, nil
}
