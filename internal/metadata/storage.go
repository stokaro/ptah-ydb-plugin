package metadata

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
)

// Runner is a pinned SQL transaction; the caller owns its commit or rollback.
type Runner interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

// Lookup reads a tag without creating storage or confusing absence with zero.
func (s Store) Lookup(ctx context.Context, runner Runner, name string) (*protocol.MigrationTagRecord, error) {
	var payload []byte
	err := runner.QueryRowContext(ctx, "DECLARE $name AS Utf8; SELECT `record` FROM "+s.QuotedPath()+" WHERE `name` = $name", sql.Named("name", name)).Scan(&payload)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read YDB metadata tag: query failed")
	}
	record, err := s.Decode(payload)
	if err != nil {
		return nil, err
	}
	tag, ok := record.(*protocol.MigrationTagRecord)
	if !ok || tag.Name != name {
		return nil, fmt.Errorf("YDB metadata tag identity changed")
	}
	return tag, nil
}

// Mutate validates any stored row before modifying it in the selected transaction.
// Deletion existence comes from the serializable read, never RowsAffected.
func (s Store) Mutate(ctx context.Context, runner Runner, request *protocol.MutateMetadataRequest, beforeWrite func(context.Context) error) (outcome protocol.MetadataResult_Outcome, deleted bool, err error) {
	switch mutation := request.Mutation.(type) {
	case *protocol.MutateMetadataRequest_AppendLog:
		row := mutation.AppendLog
		payload, err := s.Encode(row)
		if err != nil {
			return protocol.MetadataResult_REJECTED, false, err
		}
		declarations := "DECLARE $run AS Utf8; DECLARE $sequence AS Int64; "
		args := []any{sql.Named("run", row.RunId), sql.Named("sequence", row.Sequence)}
		var previous []byte
		err = runner.QueryRowContext(ctx, declarations+"SELECT `record` FROM "+s.QuotedPath()+" WHERE `run_id` = $run AND `sequence` = $sequence", args...).Scan(&previous)
		if err == nil {
			existing, decodeErr := s.Decode(previous)
			if decodeErr != nil {
				return protocol.MetadataResult_REJECTED, false, decodeErr
			}
			event, ok := existing.(*protocol.MigrationLogRecord)
			if !ok || event.RunId != row.RunId || event.Sequence != row.Sequence {
				return protocol.MetadataResult_REJECTED, false, fmt.Errorf("YDB log identity changed")
			}
			return protocol.MetadataResult_REJECTED, false, nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return protocol.MetadataResult_REJECTED, false, fmt.Errorf("read YDB log identity: query failed")
		}
		args = append(args, sql.Named("record", payload))
		if err := beforeWrite(ctx); err != nil {
			return protocol.MetadataResult_REJECTED, false, err
		}
		if err := ctx.Err(); err != nil {
			return protocol.MetadataResult_REJECTED, false, err
		}
		_, err = runner.ExecContext(ctx, declarations+"DECLARE $record AS String; INSERT INTO "+s.QuotedPath()+" (`run_id`, `sequence`, `record`) VALUES ($run, $sequence, $record)", args...)
		if err != nil {
			return protocol.MetadataResult_UNKNOWN, false, fmt.Errorf("append YDB log: query failed")
		}
	case *protocol.MutateMetadataRequest_PutTag:
		row := mutation.PutTag
		payload, err := s.Encode(row)
		if err != nil {
			return protocol.MetadataResult_REJECTED, false, err
		}
		if _, err := s.Lookup(ctx, runner, row.Name); err != nil {
			return protocol.MetadataResult_REJECTED, false, err
		}
		if err := beforeWrite(ctx); err != nil {
			return protocol.MetadataResult_REJECTED, false, err
		}
		if err := ctx.Err(); err != nil {
			return protocol.MetadataResult_REJECTED, false, err
		}
		_, err = runner.ExecContext(ctx, "DECLARE $name AS Utf8; DECLARE $record AS String; UPSERT INTO "+s.QuotedPath()+" (`name`, `record`) VALUES ($name, $record)", sql.Named("name", row.Name), sql.Named("record", payload))
		if err != nil {
			return protocol.MetadataResult_UNKNOWN, false, fmt.Errorf("write YDB tag: query failed")
		}
	case *protocol.MutateMetadataRequest_DeleteTag:
		row, err := s.Lookup(ctx, runner, mutation.DeleteTag)
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
		_, err = runner.ExecContext(ctx, "DECLARE $name AS Utf8; DELETE FROM "+s.QuotedPath()+" WHERE `name` = $name", sql.Named("name", mutation.DeleteTag))
		if err != nil {
			return protocol.MetadataResult_UNKNOWN, false, fmt.Errorf("delete YDB tag: query failed")
		}
		return protocol.MetadataResult_APPLIED, true, nil
	default:
		return protocol.MetadataResult_REJECTED, false, fmt.Errorf("invalid YDB metadata mutation")
	}
	return protocol.MetadataResult_APPLIED, false, nil
}

// Snapshot captures a complete immutable read without retaining a SQL cursor.
// Count and stream share the caller's serializable transaction, so truncation
// or an unexpected result set cannot establish a complete journal.
func (s Store) Snapshot(ctx context.Context, runner Runner) (snapshot *Snapshot, err error) {
	var expected uint64
	if err := runner.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+s.QuotedPath()).Scan(&expected); err != nil {
		return nil, fmt.Errorf("count YDB metadata: query failed")
	}
	query := "SELECT `name`, `record` FROM " + s.QuotedPath() + " ORDER BY `name`"
	if s.Kind == protocol.MetadataScope_LOG {
		query = "SELECT `run_id`, `sequence`, `record` FROM " + s.QuotedPath() + " ORDER BY `run_id`, `sequence`"
	}
	rows, err := runner.QueryContext(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("read YDB metadata: query failed")
	}
	defer func() {
		if closeErr := rows.Close(); closeErr != nil {
			err = errors.Join(err, fmt.Errorf("close YDB metadata stream failed"))
		}
	}()
	snapshot, err = NewSnapshot(s)
	if err != nil {
		return nil, err
	}
	captured := snapshot
	defer func() {
		if err != nil {
			err = errors.Join(err, captured.Close())
			snapshot = nil
		}
	}()
	var count uint64
	var lastName string
	var lastSequence int64
	for rows.Next() {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		var name string
		var sequence int64
		var payload []byte
		if s.Kind == protocol.MetadataScope_LOG {
			err = rows.Scan(&name, &sequence, &payload)
		} else {
			err = rows.Scan(&name, &payload)
		}
		if err != nil {
			return nil, fmt.Errorf("read YDB metadata: invalid row")
		}
		record, err := s.Decode(payload)
		if err != nil {
			return nil, err
		}
		switch value := record.(type) {
		case *protocol.MigrationLogRecord:
			if value.RunId != name || value.Sequence != sequence {
				return nil, fmt.Errorf("YDB log identity changed")
			}
		case *protocol.MigrationTagRecord:
			if value.Name != name {
				return nil, fmt.Errorf("YDB tag identity changed")
			}
		}
		if count > 0 && (name < lastName || (name == lastName && sequence <= lastSequence)) {
			return nil, fmt.Errorf("YDB metadata ordering changed or repeated a key")
		}
		if count >= expected {
			return nil, fmt.Errorf("YDB metadata count changed")
		}
		if err := snapshot.append(payload); err != nil {
			return nil, err
		}
		lastName, lastSequence = name, sequence
		count++
	}
	if rows.Err() != nil {
		return nil, fmt.Errorf("read YDB metadata: stream failed")
	}
	if rows.NextResultSet() || rows.Err() != nil {
		return nil, fmt.Errorf("YDB metadata returned an unexpected result set")
	}
	if count != expected {
		return nil, fmt.Errorf("YDB metadata stream was incomplete")
	}
	if err := rows.Close(); err != nil {
		return nil, fmt.Errorf("close YDB metadata stream failed")
	}
	if err := snapshot.seal(count); err != nil {
		return nil, err
	}
	return snapshot, nil
}
