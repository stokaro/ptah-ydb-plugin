package nativeapi

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path"

	"github.com/stokaro/ptah-ydb-plugin/internal/streaming"
	"github.com/stokaro/ptah-ydb-plugin/internal/ydbtype"
	"github.com/stokaro/ptah-ydb-plugin/schema"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
)

// ReadStreamingQuery reads stored configuration at absolute from the database's
// system view. A nil result establishes absence only when the capability is on.
// Runtime status, checkpoints and offsets are not declaration settings.
func ReadStreamingQuery(ctx context.Context, db *sql.DB, database, absolute string) (result *streaming.Spec, err error) {
	rows, err := db.QueryContext(ctx, "SELECT Path, Text, Run, ResourcePool FROM "+ydbtype.QuoteIdentifier(path.Join(database, ".sys/streaming_queries"))+" WHERE Path = $path", sql.Named("path", absolute))
	if err != nil {
		return nil, fmt.Errorf("read streaming query %q: %w", absolute, err)
	}
	defer func() {
		err = errors.Join(err, rows.Close())
		if err != nil {
			result = nil
		}
	}()
	for rows.Next() {
		var name, text, pool sql.NullString
		var run sql.NullBool
		if err := rows.Scan(&name, &text, &run, &pool); err != nil {
			return nil, err
		}
		if result != nil || !name.Valid || name.String != absolute || !text.Valid || !run.Valid || !pool.Valid {
			return nil, fmt.Errorf("streaming query %q has an incomplete or duplicate description", absolute)
		}
		result = &streaming.Spec{Text: text.String, Run: new(run.Bool), ResourcePool: pool.String}
		if err := streaming.Validate(*result); err != nil {
			return nil, err
		}
	}
	return result, rows.Err()
}

// StreamingQuery checks a captured precondition, dispatches without retries,
// and confirms the resulting stored settings.
type StreamingQuery struct {
	Read    func(context.Context, string) (*streaming.Spec, error)
	Execute func(context.Context, string) error
}

// Apply reports dispatch even when a statement or its confirmation fails.
// YDB has no atomic schema compare-and-set between the read and the write.
func (q *StreamingQuery) Apply(ctx context.Context, database string, value *protocol.Envelope, beforeWrite func(context.Context) error) (bool, error) {
	normalized, err := schema.NormalizeStreamingQueryOperation(&protocol.ValueInput{Id: "execute", Representation: protocol.ValueInput_OPERATION, Value: value})
	if err != nil {
		return false, err
	}
	owner := normalized.NormalizedOwner
	if owner.Database != "" && owner.Database != database {
		return false, fmt.Errorf("streaming query operation targets another database")
	}
	change, err := schema.StreamingChangeOf(schema.StreamingQueryOperationChange(normalized.Value))
	if err != nil {
		return false, err
	}
	absolute := path.Join(database, owner.Schema, owner.Name)
	held, err := q.Read(ctx, absolute)
	if err != nil {
		return false, err
	}
	if !sameStreaming(held, change.Before) {
		return false, fmt.Errorf("streaming query %q changed since the plan read it", absolute)
	}
	statements, err := streaming.Statements(change)
	if err != nil {
		return false, err
	}
	if err := beforeWrite(ctx); err != nil {
		return false, err
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	for i, statement := range statements {
		if err := ctx.Err(); err != nil {
			return i > 0, err
		}
		if err := q.Execute(ctx, statement); err != nil {
			return true, err
		}
	}
	if err := ctx.Err(); err != nil {
		return true, err
	}
	held, err = q.Read(ctx, absolute)
	if err != nil {
		return true, err
	}
	if !sameStreaming(held, change.After) {
		return true, fmt.Errorf("streaming query %q was not confirmed", absolute)
	}
	return true, nil
}

func sameStreaming(a, b *streaming.Spec) bool {
	if a == nil || b == nil {
		return a == b
	}
	return streaming.Equal(*a, *b)
}
