package embedstore

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
)

// The run-state records are stored a column per field, as the host's
// embedded engine stores them (internal/embedydb, rows.go): an optional text
// is NULL when empty, a time is UTC to the microsecond YDB keeps and NULL
// when it is the zero time, and a run's cursor is a JSON array of its key
// components. A record a write returns is the record as a later read returns
// it, so what the host is handed and what it reads back are equal.

// instant is a wire time as a Go time, the zero time for an absent one.
func instant(value *protocol.TimestampValue) time.Time {
	if value == nil {
		return time.Time{}
	}
	return time.Unix(value.GetSeconds(), int64(value.GetNanos())).UTC()
}

// stored is a time as YDB stores it: UTC, to the microsecond.
func stored(value time.Time) time.Time {
	if value.IsZero() {
		return value
	}
	return value.UTC().Truncate(time.Microsecond)
}

// wireTime is a stored time on the wire, absent for the zero time.
func wireTime(value time.Time) *protocol.TimestampValue {
	if value.IsZero() {
		return nil
	}
	value = stored(value)
	return &protocol.TimestampValue{Seconds: value.Unix(), Nanos: int32(value.Nanosecond())} // #nosec G115 -- below 1e9
}

// optionalText binds text that is NULL when empty.
func optionalText(value string) *string {
	if value == "" {
		return nil
	}
	return &value
}

// optionalTime binds a time that is NULL when zero.
func optionalTime(value *protocol.TimestampValue) *time.Time {
	at := instant(value)
	if at.IsZero() {
		return nil
	}
	return new(stored(at))
}

// requiredTime binds a time a column requires.
func requiredTime(value *protocol.TimestampValue) time.Time {
	return stored(instant(value))
}

// row is one record's values by column name.
type row map[string]any

// upsert renders the statement that writes one whole row of t, and binds
// values to it in the table's column order. A column the values do not name
// is an error rather than a column left out.
func (t table) upsert(values row) (string, []any, error) {
	if len(values) != len(t.columns) {
		return "", nil, fmt.Errorf("write %s: %d values for %d columns", t.name, len(values), len(t.columns))
	}
	arguments := make([]any, 0, len(t.columns))
	placeholders := make([]string, 0, len(t.columns))
	for _, c := range t.columns {
		value, found := values[c.name]
		if !found {
			return "", nil, fmt.Errorf("write %s: no value for column %s", t.name, c.name)
		}
		arguments = append(arguments, value)
		placeholders = append(placeholders, "$p"+strconv.Itoa(len(arguments)))
	}
	statement := fmt.Sprintf("UPSERT INTO %s (%s) VALUES (%s)", Quote(t.name), t.selectList(), strings.Join(placeholders, ", "))
	return statement, arguments, nil
}

// scanned is one row read back, by column name.
type scanned map[string]any

// scan reads one row of the table's columns.
func (t table) scan(rows interface{ Scan(...any) error }) (scanned, error) {
	destinations := make([]any, len(t.columns))
	for i, c := range t.columns {
		switch c.kind {
		case integer:
			destinations[i] = new(sql.NullInt32)
		case bigint:
			destinations[i] = new(sql.NullInt64)
		case boolean:
			destinations[i] = new(sql.NullBool)
		case timestamp:
			destinations[i] = new(sql.NullTime)
		default:
			destinations[i] = new(sql.NullString)
		}
	}
	if err := rows.Scan(destinations...); err != nil {
		return nil, err
	}
	values := make(scanned, len(t.columns))
	for i, c := range t.columns {
		values[c.name] = destinations[i]
	}
	return values, nil
}

func (s scanned) text(name string) string {
	value, _ := s[name].(*sql.NullString)
	if value == nil {
		return ""
	}
	return value.String
}

func (s scanned) integer(name string) int64 {
	switch value := s[name].(type) {
	case *sql.NullInt64:
		return value.Int64
	case *sql.NullInt32:
		return int64(value.Int32)
	}
	return 0
}

func (s scanned) boolean(name string) bool {
	value, _ := s[name].(*sql.NullBool)
	return value != nil && value.Bool
}

// time reads a time, absent for NULL, in UTC: the driver hands a timestamp
// back in the process's zone.
func (s scanned) time(name string) *protocol.TimestampValue {
	value, _ := s[name].(*sql.NullTime)
	if value == nil || !value.Valid {
		return nil
	}
	return wireTime(value.Time)
}

func generationValues(g *protocol.EmbeddingGeneration) row {
	return row{
		"identity": g.GetIdentity(), "spec_digest": g.GetSpecDigest(), "spec_document": g.GetSpecDocument(),
		"name": optionalText(g.GetName()), "reproducibility": g.GetReproducibility(),
		"reproducibility_reason": optionalText(g.GetReproducibilityReason()), "resolved_model": optionalText(g.GetResolvedModel()),
		"dimension":     int32(g.GetDimension()), // #nosec G115 -- the SDK bounds a dimension at 65536
		"target_schema": g.GetTargetSchema(), "target_table": g.GetTargetTable(), "target_column": g.GetTargetColumn(),
		"source_schema": g.GetSourceSchema(), "source_table": g.GetSourceTable(), "consistency_mode": g.GetConsistencyMode(),
		"created_at": requiredTime(g.GetCreatedAt()), "retired_at": optionalTime(g.GetRetiredAt()),
		"verified_at": optionalTime(g.GetVerifiedAt()), "maintained_until": optionalTime(g.GetMaintainedUntil()),
	}
}

func scannedGeneration(s scanned) *protocol.EmbeddingGeneration {
	return &protocol.EmbeddingGeneration{
		Identity: s.text("identity"), SpecDigest: s.text("spec_digest"), SpecDocument: s.text("spec_document"), Name: s.text("name"),
		Reproducibility: s.text("reproducibility"), ReproducibilityReason: s.text("reproducibility_reason"),
		ResolvedModel: s.text("resolved_model"), Dimension: uint32(max(s.integer("dimension"), 0)), // #nosec G115 -- a stored dimension
		TargetSchema: s.text("target_schema"), TargetTable: s.text("target_table"), TargetColumn: s.text("target_column"),
		SourceSchema: s.text("source_schema"), SourceTable: s.text("source_table"), ConsistencyMode: s.text("consistency_mode"),
		CreatedAt: s.time("created_at"), RetiredAt: s.time("retired_at"), VerifiedAt: s.time("verified_at"),
		MaintainedUntil: s.time("maintained_until"),
	}
}

func runValues(r *protocol.EmbeddingRun) (row, error) {
	cursor, err := encodeCursor(r.GetCursor())
	if err != nil {
		return nil, err
	}
	p := r.GetProgress()
	return row{
		"id": r.GetId(), "spec_digest": r.GetSpecDigest(), "generation_identity": r.GetGenerationIdentity(),
		"environment": r.GetEnvironment(), "source": r.GetSource(), "target": r.GetTarget(), "provider_profile": r.GetProviderProfile(),
		"resolved_model": optionalText(r.GetResolvedModel()), "ptah_version": r.GetPtahVersion(), "policy_digest": r.GetPolicyDigest(),
		"phase": r.GetPhase(), "status": r.GetStatus(), "lease_owner": optionalText(r.GetLeaseOwner()),
		"lease_expires": optionalTime(r.GetLeaseExpires()), "fencing_token": r.GetFencingToken(), "snapshot_done": r.GetSnapshotDone(),
		"snapshot_watermark": optionalText(r.GetSnapshotWatermark()), "catch_up_watermark": optionalText(r.GetCatchUpWatermark()),
		"cursor": cursor, "rows_scanned": p.GetRowsScanned(), "rows_embedded": p.GetRowsEmbedded(), "rows_skipped": p.GetRowsSkipped(),
		"rows_deleted": p.GetRowsDeleted(), "batches_committed": p.GetBatchesCommitted(),
		"provider_prompt_tokens": p.GetProviderPromptTokens(), "provider_total_tokens": p.GetProviderTotalTokens(),
		"provider_usage_batches": p.GetProviderUsageBatches(),
		"retry_count":            int32(min(p.GetRetryCount(), 1<<31-1)), // #nosec G115 -- clamped into range on this line
		"verification_ref":       optionalText(r.GetVerificationRef()), "cutover_plan_ref": optionalText(r.GetCutoverPlanRef()),
		"approval_ref": optionalText(r.GetApprovalRef()), "active_pointer": optionalText(r.GetActivePointer()),
		"rollback_eligible": r.GetRollbackEligible(), "failure_class": optionalText(r.GetFailureClass()),
		"failure_detail": optionalText(r.GetFailureDetail()), "created_at": requiredTime(r.GetCreatedAt()),
		"updated_at": requiredTime(r.GetUpdatedAt()),
	}, nil
}

func scannedRun(s scanned) (*protocol.EmbeddingRun, error) {
	cursor, err := decodeCursor(s.text("cursor"))
	if err != nil {
		return nil, err
	}
	return &protocol.EmbeddingRun{
		Id: s.text("id"), SpecDigest: s.text("spec_digest"), GenerationIdentity: s.text("generation_identity"),
		Environment: s.text("environment"), Source: s.text("source"), Target: s.text("target"), ProviderProfile: s.text("provider_profile"),
		ResolvedModel: s.text("resolved_model"), PtahVersion: s.text("ptah_version"), PolicyDigest: s.text("policy_digest"),
		Phase: s.text("phase"), Status: s.text("status"), LeaseOwner: s.text("lease_owner"), LeaseExpires: s.time("lease_expires"),
		FencingToken: s.integer("fencing_token"), SnapshotDone: s.boolean("snapshot_done"),
		SnapshotWatermark: s.text("snapshot_watermark"), CatchUpWatermark: s.text("catch_up_watermark"), Cursor: cursor,
		Progress: &protocol.EmbeddingProgress{
			RowsScanned: s.integer("rows_scanned"), RowsEmbedded: s.integer("rows_embedded"), RowsSkipped: s.integer("rows_skipped"),
			RowsDeleted: s.integer("rows_deleted"), BatchesCommitted: s.integer("batches_committed"),
			ProviderPromptTokens: s.integer("provider_prompt_tokens"), ProviderTotalTokens: s.integer("provider_total_tokens"),
			ProviderUsageBatches: s.integer("provider_usage_batches"),
			RetryCount:           uint32(max(s.integer("retry_count"), 0)), // #nosec G115 -- a stored Int32
		},
		VerificationRef: s.text("verification_ref"), CutoverPlanRef: s.text("cutover_plan_ref"), ApprovalRef: s.text("approval_ref"),
		ActivePointer: s.text("active_pointer"), RollbackEligible: s.boolean("rollback_eligible"),
		FailureClass: s.text("failure_class"), FailureDetail: s.text("failure_detail"),
		CreatedAt: s.time("created_at"), UpdatedAt: s.time("updated_at"),
	}, nil
}

func pointerValues(p *protocol.EmbeddingPointer) row {
	return row{
		"target_schema": p.GetTargetSchema(), "target_table": p.GetTargetTable(), "active_generation": p.GetActiveGeneration(),
		"previous_generation": optionalText(p.GetPreviousGeneration()), "cut_over_at": requiredTime(p.GetCutOverAt()),
		"cut_over_by": optionalText(p.GetCutOverBy()), "plan_digest": optionalText(p.GetPlanDigest()),
	}
}

func scannedPointer(s scanned) *protocol.EmbeddingPointer {
	return &protocol.EmbeddingPointer{
		TargetSchema: s.text("target_schema"), TargetTable: s.text("target_table"), ActiveGeneration: s.text("active_generation"),
		PreviousGeneration: s.text("previous_generation"), CutOverAt: s.time("cut_over_at"), CutOverBy: s.text("cut_over_by"),
		PlanDigest: s.text("plan_digest"),
	}
}

func eventValues(e *protocol.EmbeddingEvent, sequence int64) row {
	c := e.GetCounts()
	return row{
		"sequence": sequence, "run_id": e.GetRunId(), "kind": e.GetKind(), "at": requiredTime(e.GetAt()),
		"actor": optionalText(e.GetActor()), "fencing_token": e.GetFencingToken(), "from_phase": optionalText(e.GetFromPhase()),
		"to_phase": optionalText(e.GetToPhase()), "detail": optionalText(e.GetDetail()), "rows_scanned": c.GetRowsScanned(),
		"rows_embedded": c.GetRowsEmbedded(), "rows_skipped": c.GetRowsSkipped(), "rows_deleted": c.GetRowsDeleted(),
		"batches_committed": c.GetBatchesCommitted(),
	}
}

func scannedEvent(s scanned) *protocol.EmbeddingEvent {
	return &protocol.EmbeddingEvent{
		RunId: s.text("run_id"), Sequence: s.integer("sequence"), Kind: s.text("kind"), At: s.time("at"), Actor: s.text("actor"),
		FencingToken: s.integer("fencing_token"), FromPhase: s.text("from_phase"), ToPhase: s.text("to_phase"), Detail: s.text("detail"),
		Counts: &protocol.EmbeddingEventCounts{
			RowsScanned: s.integer("rows_scanned"), RowsEmbedded: s.integer("rows_embedded"), RowsSkipped: s.integer("rows_skipped"),
			RowsDeleted: s.integer("rows_deleted"), BatchesCommitted: s.integer("batches_committed"),
		},
	}
}

// encodeCursor writes a keyset cursor as a JSON array, NULL when empty,
// because a key component may hold any character a separator could.
func encodeCursor(cursor []string) (*string, error) {
	if len(cursor) == 0 {
		return nil, nil
	}
	encoded, err := json.Marshal(cursor)
	if err != nil {
		return nil, fmt.Errorf("encode cursor: %w", err)
	}
	return new(string(encoded)), nil
}

func decodeCursor(encoded string) ([]string, error) {
	if encoded == "" {
		return nil, nil
	}
	var cursor []string
	if err := json.Unmarshal([]byte(encoded), &cursor); err != nil {
		return nil, fmt.Errorf("decode the cursor of a run: %w", err)
	}
	return cursor, nil
}
