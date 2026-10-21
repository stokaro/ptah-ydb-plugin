package history

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"

	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"google.golang.org/protobuf/proto"
)

// Runner is the exact SQL connection or transaction selected by the session.
type Runner interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

// Read returns one consistent bounded query result, never a truncated prefix.
func (s Store) Read(ctx context.Context, runner Runner, limit uint32) (result []*protocol.HistoryRevision, err error) {
	rows, err := runner.QueryContext(ctx, "SELECT version, revision FROM "+s.QuotedPath()+" ORDER BY version LIMIT "+strconv.FormatUint(uint64(limit)+1, 10))
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
		var payload []byte
		var version string
		if s.Format == protocol.HistoryScope_NATIVE {
			var numeric int64
			err = rows.Scan(&numeric, &payload)
			version = strconv.FormatInt(numeric, 10)
		} else {
			err = rows.Scan(&version, &payload)
		}
		if err != nil {
			return nil, fmt.Errorf("read YDB history: invalid row")
		}
		record, err := s.Decode(version, payload)
		if err != nil {
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

// Mutate compares all expected facts and writes on one serializable transaction.
// The caller owns commit. RowsAffected is not an existence witness in YDB.
func (s Store) Mutate(ctx context.Context, runner Runner, request *protocol.MutateHistoryRequest) (protocol.HistoryResult_Outcome, error) {
	version := request.GetReplacement().GetVersion()
	if request.Expected != nil {
		version = request.Expected.Version
	}
	key, err := s.Key(version)
	if err != nil {
		return protocol.HistoryResult_REFUSED, err
	}
	declaration := "DECLARE $version AS " + s.KeyType() + "; "
	var payload []byte
	err = runner.QueryRowContext(ctx, declaration+"SELECT revision FROM "+s.QuotedPath()+" WHERE version = $version", sql.Named("version", key)).Scan(&payload)
	var current *protocol.HistoryRevision
	switch {
	case errors.Is(err, sql.ErrNoRows):
	case err != nil:
		return protocol.HistoryResult_UNKNOWN, fmt.Errorf("compare YDB history: query failed")
	default:
		current, err = s.Decode(version, payload)
		if err != nil {
			return protocol.HistoryResult_REFUSED, err
		}
	}
	if !proto.Equal(current, request.Expected) {
		return protocol.HistoryResult_CONFLICT, nil
	}
	query := declaration + "DELETE FROM " + s.QuotedPath() + " WHERE version = $version"
	args := []any{sql.Named("version", key)}
	if request.Replacement != nil {
		payload, err = s.Encode(request.Replacement)
		if err != nil {
			return protocol.HistoryResult_REFUSED, err
		}
		query = declaration + "DECLARE $revision AS String; UPSERT INTO " + s.QuotedPath() + " (version, revision) VALUES ($version, $revision)"
		args = append(args, sql.Named("revision", payload))
	}
	if _, err := runner.ExecContext(ctx, query, args...); err != nil {
		return protocol.HistoryResult_UNKNOWN, fmt.Errorf("write YDB history: query failed")
	}
	return protocol.HistoryResult_APPLIED, nil
}
