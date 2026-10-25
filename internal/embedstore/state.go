package embedstore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"

	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/stokaro/ptah/provider-sdk/provider"
	"google.golang.org/protobuf/proto"
)

// The refusals a write ends with before anything is written.
var (
	// errConflict is a compare whose token differed.
	errConflict = errors.New("the state changed after it was read")
	// ErrStoreAbsent is a call that needs a store that was never prepared.
	ErrStoreAbsent = errors.New("the embedding store does not exist; prepare it first")
)

// Refusal is a request the store cannot meet at all, with the reason the
// contract names. Its text names objects and a reason and carries no row
// content, vector, filter or credential.
type Refusal struct {
	Reason  protocol.EmbeddingRefusal_Reason
	Message string
}

func (r *Refusal) Error() string { return r.Message }

func refuse(reason protocol.EmbeddingRefusal_Reason, format string, arguments ...any) *Refusal {
	return &Refusal{Reason: reason, Message: fmt.Sprintf(format, arguments...)}
}

// outcome is the result of a write that ended with err: APPLIED for none,
// CONFLICT for a token that differed, REFUSED for a request the store cannot
// meet, UNKNOWN for a commit nothing proves the outcome of, and NOT_APPLIED
// for any other failure, each of which happened before anything committed.
// The caller adds what an APPLIED write returns.
func outcome(requestID string, err error) *protocol.EmbeddingWriteResult {
	result := &protocol.EmbeddingWriteResult{RequestId: requestID}
	var refusal *Refusal
	switch {
	case err == nil:
		result.Outcome = protocol.EmbeddingWriteResult_APPLIED
	case errors.Is(err, errConflict):
		result.Outcome = protocol.EmbeddingWriteResult_CONFLICT
	case errors.Is(err, ErrStoreAbsent):
		result.Outcome = protocol.EmbeddingWriteResult_REFUSED
		result.Refusal = &protocol.EmbeddingRefusal{Reason: protocol.EmbeddingRefusal_ABSENT, Message: ErrStoreAbsent.Error()}
	case errors.As(err, &refusal):
		result.Outcome = protocol.EmbeddingWriteResult_REFUSED
		result.Refusal = &protocol.EmbeddingRefusal{Reason: refusal.Reason, Message: oneLine(refusal.Message)}
	case errors.Is(err, errUndetermined):
		result.Outcome = protocol.EmbeddingWriteResult_UNKNOWN
		result.Message = oneLine(err.Error())
	default:
		result.Outcome = protocol.EmbeddingWriteResult_NOT_APPLIED
		result.Message = oneLine(err.Error())
	}
	return result
}

// oneLine bounds a diagnostic to one line of at most 512 bytes.
func oneLine(text string) string {
	var line strings.Builder
	for _, r := range strings.ToValidUTF8(text, "?") {
		if unicode.IsControl(r) || r == '\u2028' || r == '\u2029' {
			r = ' '
		}
		if line.Len()+utf8.RuneLen(r) > 512 {
			break
		}
		line.WriteRune(r)
	}
	if trimmed := strings.TrimSpace(line.String()); trimmed != "" {
		return trimmed
	}
	return "the YDB embedding store refused the request"
}

// presence reports how many run-state tables exist: none, all of them, or a
// store that is only part there, which is an error.
func (s *Store) presence(ctx context.Context) (bool, error) {
	present := 0
	for _, t := range layout() {
		exists, err := s.exists(ctx, t.name)
		if err != nil {
			return false, err
		}
		if exists {
			present++
		}
	}
	switch present {
	case 0:
		return false, nil
	case len(layout()):
		return true, nil
	default:
		return false, fmt.Errorf("the embedding store holds %d of its %d tables; prepare it again", present, len(layout()))
	}
}

// PrepareStore creates the run-state tables that are missing and verifies the
// ones that exist. A table of another shape refuses the whole preparation
// with NOT_OWNED before anything is created.
func (s *Store) PrepareStore(ctx context.Context, requestID string) *protocol.EmbeddingWriteResult {
	var missing []table
	for _, t := range layout() {
		described, exists, err := s.describe(ctx, t.name)
		if err != nil {
			return outcome(requestID, err)
		}
		if !exists {
			missing = append(missing, t)
			continue
		}
		if reason := t.mismatch(described); reason != "" {
			return outcome(requestID, refuse(protocol.EmbeddingRefusal_NOT_OWNED, "%s, so it is not a store this provider recognizes", reason))
		}
	}
	for i, t := range missing {
		if _, err := s.db.ExecContext(ctx, t.createStatement(s.caps.WideTimestamps)); err != nil {
			if i == 0 {
				return outcome(requestID, fmt.Errorf("create %s: %s", t.name, statusName(err)))
			}
			return outcome(requestID, fmt.Errorf("%w: create %s: %s; the store is partly created, prepare it again", errUndetermined, t.name, statusName(err)))
		}
	}
	return outcome(requestID, nil)
}

// queryer is what a read runs on: the pool, or a transaction.
type queryer interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

// readRows reads the rows of t matching where, at most limit of them, and
// decodes each.
func readRows[T any](ctx context.Context, db queryer, t table, where string, limit int, decode func(scanned) (T, error), args ...any) ([]T, error) {
	// #nosec G201 -- the table and its columns are this package's names, and
	// where is one of its literal predicates; values are parameters.
	query := fmt.Sprintf("SELECT %s FROM %s %s LIMIT %d", t.selectList(), Quote(t.name), where, limit)
	rows, err := db.QueryContext(ctx, query, named(args...)...)
	if err != nil {
		return nil, fmt.Errorf("read %s: %s", t.name, statusName(err))
	}
	defer func() { _ = rows.Close() }()
	var decoded []T
	for rows.Next() {
		row, err := t.scan(rows)
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", t.name, err)
		}
		value, err := decode(row)
		if err != nil {
			return nil, err
		}
		decoded = append(decoded, value)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read %s: %s", t.name, statusName(err))
	}
	return decoded, nil
}

func generations(row scanned) (*protocol.EmbeddingGeneration, error) {
	return scannedGeneration(row), nil
}
func pointers(row scanned) (*protocol.EmbeddingPointer, error) { return scannedPointer(row), nil }

// readScope reads the records scope selects, in key order. A scope holding
// more than bound records is an error, never a partial answer.
func readScope(ctx context.Context, db queryer, scope *protocol.EmbeddingStateScope, bound int) (*protocol.EmbeddingRecords, error) {
	records := &protocol.EmbeddingRecords{}
	var err error
	limit := bound + 1
	byIdentity, byID := "WHERE "+Quote("identity")+" = $p1", "WHERE "+Quote("id")+" = $p1"
	switch selected := scope.GetScope().(type) {
	case *protocol.EmbeddingStateScope_Everything:
		if records.Generations, err = readRows(ctx, db, generationLayout, "ORDER BY "+Quote("identity"), limit, generations); err != nil {
			return nil, err
		}
		if records.Runs, err = readRows(ctx, db, runLayout, "ORDER BY "+Quote("id"), limit, scannedRun); err != nil {
			return nil, err
		}
		if records.Pointers, err = readRows(ctx, db, pointerLayout, "ORDER BY "+Quote("target_schema")+", "+Quote("target_table"), limit, pointers); err != nil {
			return nil, err
		}
	case *protocol.EmbeddingStateScope_Run:
		if records.Runs, err = readRows(ctx, db, runLayout, byID, limit, scannedRun, selected.Run); err != nil {
			return nil, err
		}
	case *protocol.EmbeddingStateScope_Generation:
		if records.Generations, err = readRows(ctx, db, generationLayout, byIdentity, limit, generations, selected.Generation); err != nil {
			return nil, err
		}
	case *protocol.EmbeddingStateScope_RunsOfGeneration:
		where := "WHERE " + Quote("generation_identity") + " = $p1 ORDER BY " + Quote("id")
		if records.Runs, err = readRows(ctx, db, runLayout, where, limit, scannedRun, selected.RunsOfGeneration); err != nil {
			return nil, err
		}
	case *protocol.EmbeddingStateScope_Pointer:
		where := "WHERE " + Quote("target_schema") + " = $p1 AND " + Quote("target_table") + " = $p2"
		key := selected.Pointer
		if records.Pointers, err = readRows(ctx, db, pointerLayout, where, limit, pointers, key.GetTargetSchema(), key.GetTargetTable()); err != nil {
			return nil, err
		}
	default:
		return nil, fmt.Errorf("an embedding state scope selects nothing")
	}
	if count := len(records.Generations) + len(records.Runs) + len(records.Pointers); count > bound {
		return nil, fmt.Errorf("the scope holds more than %d records", bound)
	}
	return records, nil
}

// stateToken is the token of a scope's snapshot: a digest of the scope, the
// store's presence and exactly its records.
func stateToken(scope *protocol.EmbeddingStateScope, exists bool, records *protocol.EmbeddingRecords) (string, error) {
	return provider.EmbeddingStateToken(&protocol.EmbeddingState{Scope: scope, Exists: exists, Records: records})
}

// ReadState reads one scope as one snapshot. A store that does not exist is
// exists=false with no record.
func (s *Store) ReadState(ctx context.Context, scope *protocol.EmbeddingStateScope, bound uint32) (*protocol.EmbeddingState, error) {
	exists, err := s.presence(ctx)
	if err != nil {
		return nil, err
	}
	records := &protocol.EmbeddingRecords{}
	if exists {
		err = s.inTransaction(ctx, func(tx *sql.Tx) error {
			var readErr error
			records, readErr = readScope(ctx, tx, scope, int(bound))
			return readErr
		})
		if err != nil {
			return nil, err
		}
	}
	token, err := stateToken(scope, exists, records)
	if err != nil {
		return nil, err
	}
	return &protocol.EmbeddingState{Scope: proto.CloneOf(scope), Exists: exists, Records: records, Token: token}, nil
}

// MutateState compares and writes in one transaction: it reads the scope
// again, and only when its token is the expected one writes each record
// whole. It returns the written records as a later read returns them.
func (s *Store) MutateState(ctx context.Context, request *protocol.MutateEmbeddingStateRequest) *protocol.EmbeddingWriteResult {
	exists, err := s.presence(ctx)
	switch {
	case err != nil:
		return outcome(request.GetRequestId(), err)
	case !exists:
		return outcome(request.GetRequestId(), ErrStoreAbsent)
	}
	err = s.inTransaction(ctx, func(tx *sql.Tx) error {
		before, err := readScope(ctx, tx, request.GetScope(), provider.MaxEmbeddingRecords)
		if err != nil {
			return err
		}
		token, err := stateToken(request.GetScope(), true, before)
		if err != nil {
			return err
		}
		if token != request.GetExpectedToken() {
			return errConflict
		}
		return writeRecords(ctx, tx, request.GetRecords())
	})
	result := outcome(request.GetRequestId(), err)
	if err == nil {
		result.Written = normalRecords(request.GetRecords())
	}
	return result
}

// writeRecords upserts every record whole.
func writeRecords(ctx context.Context, tx *sql.Tx, records *protocol.EmbeddingRecords) error {
	for _, generation := range records.GetGenerations() {
		if err := upsert(ctx, tx, generationLayout, generationValues(generation)); err != nil {
			return err
		}
	}
	for _, run := range records.GetRuns() {
		values, err := runValues(run)
		if err != nil {
			return err
		}
		if err := upsert(ctx, tx, runLayout, values); err != nil {
			return err
		}
	}
	for _, pointer := range records.GetPointers() {
		if err := upsert(ctx, tx, pointerLayout, pointerValues(pointer)); err != nil {
			return err
		}
	}
	return nil
}

// upsert writes one whole row.
func upsert(ctx context.Context, tx *sql.Tx, into table, values row) error {
	statement, arguments, err := into.upsert(values)
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, statement, named(arguments...)...); err != nil {
		if retryable(err) {
			return err
		}
		return fmt.Errorf("write %s: %s", into.name, statusName(err))
	}
	return nil
}

// AppendEvent gives an event of an existing run that run's next sequence, read
// in the same transaction as the insert, so two appends at once conflict and
// one of them runs again. A run that is not there is REFUSED as ABSENT.
func (s *Store) AppendEvent(ctx context.Context, requestID string, event *protocol.EmbeddingEvent) *protocol.EmbeddingWriteResult {
	exists, err := s.presence(ctx)
	switch {
	case err != nil:
		return outcome(requestID, err)
	case !exists:
		return outcome(requestID, ErrStoreAbsent)
	}
	var sequence int64
	err = s.inTransaction(ctx, func(tx *sql.Tx) error {
		runs, err := readRows(ctx, tx, runLayout, "WHERE "+Quote("id")+" = $p1", 1, scannedRun, event.GetRunId())
		if err != nil {
			return err
		}
		if len(runs) == 0 {
			return refuse(protocol.EmbeddingRefusal_ABSENT, "run %s does not exist", event.GetRunId())
		}
		var last sql.NullInt64
		// #nosec G201 -- the table and its columns are this package's names.
		query := fmt.Sprintf("SELECT MAX(%s) FROM %s WHERE %s = $p1", Quote("sequence"), Quote(EventTable), Quote("run_id"))
		if err := tx.QueryRowContext(ctx, query, named(event.GetRunId())...).Scan(&last); err != nil {
			if retryable(err) {
				return err
			}
			return fmt.Errorf("read the event sequence of run %s: %s", event.GetRunId(), statusName(err))
		}
		sequence = last.Int64 + 1
		return upsert(ctx, tx, eventLayout, eventValues(event, sequence))
	})
	result := outcome(requestID, err)
	if err == nil {
		result.Sequence = sequence
	}
	return result
}

// ReadEvents pages a run's events after a sequence, in sequence order. A run
// that is not there is exists=false.
func (s *Store) ReadEvents(ctx context.Context, request *protocol.ReadEmbeddingEventsRequest) (*protocol.EmbeddingEventPage, error) {
	page := &protocol.EmbeddingEventPage{RunId: request.GetRunId(), Complete: true}
	exists, err := s.presence(ctx)
	if err != nil || !exists {
		return page, err
	}
	err = s.inTransaction(ctx, func(tx *sql.Tx) error {
		runs, err := readRows(ctx, tx, runLayout, "WHERE "+Quote("id")+" = $p1", 1, scannedRun, request.GetRunId())
		if err != nil || len(runs) == 0 {
			return err
		}
		page.Exists = true
		bound := int(request.GetMaxEvents())
		where := "WHERE " + Quote("run_id") + " = $p1 AND " + Quote("sequence") + " > $p2 ORDER BY " + Quote("sequence")
		events, err := readRows(ctx, tx, eventLayout, where, bound+1, func(row scanned) (*protocol.EmbeddingEvent, error) {
			return scannedEvent(row), nil
		}, request.GetRunId(), request.GetAfterSequence())
		if err != nil {
			return err
		}
		page.Complete = len(events) <= bound
		page.Events = events[:min(len(events), bound)]
		return nil
	})
	return page, err
}

// normalRecords are records as a later read returns them.
func normalRecords(records *protocol.EmbeddingRecords) *protocol.EmbeddingRecords {
	normal := &protocol.EmbeddingRecords{}
	for _, generation := range records.GetGenerations() {
		normal.Generations = append(normal.Generations, normalGeneration(generation))
	}
	for _, run := range records.GetRuns() {
		normal.Runs = append(normal.Runs, normalRun(run))
	}
	for _, pointer := range records.GetPointers() {
		normal.Pointers = append(normal.Pointers, normalPointer(pointer))
	}
	return normal
}

func normalTime(value *protocol.TimestampValue) *protocol.TimestampValue {
	return wireTime(instant(value))
}

func normalGeneration(g *protocol.EmbeddingGeneration) *protocol.EmbeddingGeneration {
	normal := proto.CloneOf(g)
	normal.CreatedAt, normal.RetiredAt = normalTime(g.GetCreatedAt()), normalTime(g.GetRetiredAt())
	normal.VerifiedAt, normal.MaintainedUntil = normalTime(g.GetVerifiedAt()), normalTime(g.GetMaintainedUntil())
	return normal
}

func normalRun(r *protocol.EmbeddingRun) *protocol.EmbeddingRun {
	normal := proto.CloneOf(r)
	normal.LeaseExpires, normal.CreatedAt, normal.UpdatedAt = normalTime(r.GetLeaseExpires()), normalTime(r.GetCreatedAt()), normalTime(r.GetUpdatedAt())
	if len(r.GetCursor()) == 0 {
		normal.Cursor = nil
	}
	if normal.Progress == nil {
		normal.Progress = &protocol.EmbeddingProgress{}
	}
	normal.Progress.RetryCount = min(normal.Progress.RetryCount, 1<<31-1)
	return normal
}

func normalPointer(p *protocol.EmbeddingPointer) *protocol.EmbeddingPointer {
	normal := proto.CloneOf(p)
	normal.CutOverAt = normalTime(p.GetCutOverAt())
	return normal
}
