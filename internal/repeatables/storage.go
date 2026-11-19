package repeatables

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"

	"google.golang.org/protobuf/proto"
	protocol "ptah.run/provider-sdk/protocol/experimental"
	"ptah.run/provider-sdk/provider"
)

// Runner is the SQL connection or transaction a call runs on.
type Runner interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

// Read returns every row without its body, in identity order, from one
// query, so the rows are one consistent snapshot. A record holding more than
// limit rows is refused, never truncated.
func (s Store) Read(ctx context.Context, runner Runner, limit uint32) (result []*protocol.RepeatableRow, err error) {
	rows, err := runner.QueryContext(ctx, "SELECT `identity`, `row` FROM "+s.QuotedPath()+" ORDER BY `identity` LIMIT "+strconv.FormatUint(uint64(limit)+1, 10))
	if err != nil {
		return nil, fmt.Errorf("read the YDB repeatable migration record: query failed")
	}
	defer func() {
		err = errors.Join(err, rows.Close())
		if err != nil {
			result = nil
		}
	}()
	for rows.Next() {
		if len(result) >= int(limit) {
			return nil, fmt.Errorf("the YDB repeatable migration record holds more than %d rows", limit)
		}
		var identity string
		var payload []byte
		if err := rows.Scan(&identity, &payload); err != nil {
			return nil, fmt.Errorf("read the YDB repeatable migration record: invalid row")
		}
		row, err := Decode(identity, payload)
		if err != nil {
			return nil, err
		}
		result = append(result, row)
	}
	if rows.Err() != nil {
		return nil, fmt.Errorf("read the YDB repeatable migration record: stream failed")
	}
	return result, nil
}

// StoredBody is what the record keeps of one migration's content.
type StoredBody struct {
	// Found is false when the record holds no row for the identity.
	Found bool
	// Kept is false when the row keeps no body.
	Kept bool
	// Bytes is the body when Kept holds, empty rather than nil for an empty
	// file, and nil otherwise.
	Bytes []byte
}

// Body reads the bytes the record keeps for identity.
func (s Store) Body(ctx context.Context, runner Runner, identity string) (StoredBody, error) {
	var payload, body []byte
	err := runner.QueryRowContext(ctx, "DECLARE $identity AS Utf8; SELECT `row`, `body` FROM "+s.QuotedPath()+" WHERE `identity` = $identity",
		sql.Named("identity", identity)).Scan(&payload, &body)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return StoredBody{}, nil
	case err != nil:
		return StoredBody{}, fmt.Errorf("read a YDB repeatable migration body: query failed")
	}
	row, err := Decode(identity, payload)
	if err != nil {
		return StoredBody{}, err
	}
	if !row.GetBodyKept() {
		if len(body) != 0 {
			return StoredBody{}, fmt.Errorf("a YDB repeatable migration row that keeps no body holds one")
		}
		return StoredBody{Found: true}, nil
	}
	if len(body) > provider.MaxRepeatableBodyBytes {
		return StoredBody{}, fmt.Errorf("a YDB repeatable migration body exceeds its size limit")
	}
	if body == nil {
		body = make([]byte, 0)
	}
	return StoredBody{Found: true, Kept: true, Bytes: body}, nil
}

// Result is what one write did. Holder names the run a CONFLICT met, and is
// empty when the record held no row. Removed reports a removal that deleted
// a row.
type Result struct {
	Outcome protocol.RepeatableResult_Outcome
	Holder  string
	Removed bool
}

// Mutate reads the row the request names and writes it on runner, which is
// one transaction: the read and the write commit together or not at all, and
// the caller commits. A row that is not the one the request expects is a
// CONFLICT, a completion of another digest or a row the store cannot hold is
// REFUSED, and neither writes anything. beforeWrite runs after the read and
// before the write, and its error stops the write.
func (s Store) Mutate(ctx context.Context, runner Runner, request *protocol.MutateRepeatableRequest, beforeWrite func(context.Context) error) (Result, error) {
	identity := mutationIdentity(request)
	current, err := s.lookup(ctx, runner, identity)
	if err != nil {
		return Result{}, err
	}
	if removal := request.GetRemove(); removal != nil {
		if current == nil {
			return Result{Outcome: protocol.RepeatableResult_APPLIED}, nil
		}
		if err := beforeWrite(ctx); err != nil {
			return Result{}, err
		}
		if _, err := runner.ExecContext(ctx, "DECLARE $identity AS Utf8; DELETE FROM "+s.QuotedPath()+" WHERE `identity` = $identity",
			sql.Named("identity", identity)); err != nil {
			return Result{}, fmt.Errorf("remove a YDB repeatable migration row: %w", err)
		}
		return Result{Outcome: protocol.RepeatableResult_APPLIED, Removed: true}, nil
	}
	next, write, refusal := nextRow(request, current)
	if refusal != nil {
		return *refusal, nil
	}
	payload, err := Encode(next)
	if err != nil {
		return Result{Outcome: protocol.RepeatableResult_REFUSED}, nil
	}
	if err := beforeWrite(ctx); err != nil {
		return Result{}, err
	}
	if _, err := runner.ExecContext(ctx, write.statement(s.QuotedPath()), write.arguments(identity, payload, request)...); err != nil {
		return Result{}, fmt.Errorf("write a YDB repeatable migration row: %w", err)
	}
	return Result{Outcome: protocol.RepeatableResult_APPLIED}, nil
}

// lookup reads the row of identity, nil when the record holds none.
func (s Store) lookup(ctx context.Context, runner Runner, identity string) (*protocol.RepeatableRow, error) {
	var payload []byte
	err := runner.QueryRowContext(ctx, "DECLARE $identity AS Utf8; SELECT `row` FROM "+s.QuotedPath()+" WHERE `identity` = $identity",
		sql.Named("identity", identity)).Scan(&payload)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return nil, nil
	case err != nil:
		return nil, fmt.Errorf("read a YDB repeatable migration row: %w", err)
	}
	return Decode(identity, payload)
}

func mutationIdentity(request *protocol.MutateRepeatableRequest) string {
	switch {
	case request.GetClaim() != nil:
		return request.GetClaim().GetIdentity()
	case request.GetComplete() != nil:
		return request.GetComplete().GetIdentity()
	case request.GetFail() != nil:
		return request.GetFail().GetIdentity()
	default:
		return request.GetRemove().GetIdentity()
	}
}

// write is the statement that stores a row.
type write int

const (
	// insertRow adds the row of a new identity, with no body.
	insertRow write = iota
	// updateRow replaces the encoded row and keeps the body. It is an UPDATE
	// because YDB refuses an UPSERT that leaves out a NOT NULL column, even
	// of a row that exists.
	updateRow
	// completeRow replaces the encoded row and the body.
	completeRow
)

func (w write) statement(table string) string {
	switch w {
	case insertRow:
		return "DECLARE $identity AS Utf8; DECLARE $row AS String; INSERT INTO " + table + " (`identity`, `row`, `body`) VALUES ($identity, $row, '')"
	case completeRow:
		return "DECLARE $identity AS Utf8; DECLARE $row AS String; DECLARE $body AS String; UPSERT INTO " + table +
			" (`identity`, `row`, `body`) VALUES ($identity, $row, $body)"
	default:
		return "DECLARE $identity AS Utf8; DECLARE $row AS String; UPDATE " + table + " SET `row` = $row WHERE `identity` = $identity"
	}
}

func (w write) arguments(identity string, payload []byte, request *protocol.MutateRepeatableRequest) []any {
	arguments := []any{sql.Named("identity", identity), sql.Named("row", payload)}
	if w == completeRow {
		body := request.GetComplete().GetBody()
		if body == nil {
			body = make([]byte, 0)
		}
		arguments = append(arguments, sql.Named("body", body))
	}
	return arguments
}

// nextRow is the row a claim, a completion or a failure writes over current,
// and the statement that writes it, or the CONFLICT or REFUSED that stops the
// write.
func nextRow(request *protocol.MutateRepeatableRequest, current *protocol.RepeatableRow) (*protocol.RepeatableRow, write, *Result) {
	conflict := &Result{Outcome: protocol.RepeatableResult_CONFLICT, Holder: current.GetRunId()}
	if claim := request.GetClaim(); claim != nil {
		next, statement := proto.CloneOf(current), updateRow
		switch {
		case claim.GetInsert() && current != nil, !claim.GetInsert() && (current == nil || current.GetRunId() != claim.GetExpectedRunId()):
			return nil, 0, conflict
		case current == nil:
			next, statement = &protocol.RepeatableRow{Identity: claim.GetIdentity()}, insertRow
		}
		next.State, next.AttemptChecksum, next.AttemptOrder = protocol.RepeatableRow_RUNNING, claim.GetAttemptChecksum(), claim.GetAttemptOrder()
		next.Transactional, next.RunId, next.Error = claim.GetTransactional(), claim.GetRunId(), ""
		next.AttemptedAtSeconds, next.AttemptedAtNanos = claim.GetAttemptedAtSeconds(), claim.GetAttemptedAtNanos()
		return next, statement, nil
	}
	if completion := request.GetComplete(); completion != nil {
		switch {
		case current == nil || current.GetRunId() != completion.GetRunId():
			return nil, 0, conflict
		case current.GetAttemptChecksum() != completion.GetChecksum() || len(completion.GetBody()) > provider.MaxRepeatableBodyBytes:
			return nil, 0, &Result{Outcome: protocol.RepeatableResult_REFUSED}
		}
		next := proto.CloneOf(current)
		next.State, next.Checksum, next.Error, next.BodyKept = protocol.RepeatableRow_APPLIED, completion.GetChecksum(), "", true
		next.Applied, next.AppliedAtSeconds, next.AppliedAtNanos = true, completion.GetAppliedAtSeconds(), completion.GetAppliedAtNanos()
		return next, completeRow, nil
	}
	failure := request.GetFail()
	if current == nil || current.GetRunId() != failure.GetRunId() {
		return nil, 0, conflict
	}
	next := proto.CloneOf(current)
	next.State, next.Error = protocol.RepeatableRow_FAILED, failure.GetError()
	return next, updateRow, nil
}
