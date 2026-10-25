// Package seeds runs YDB seed files and keeps their ledger, the table
// schema_seeds, as the Ptah host's embedded YDB engine does
// (internal/dbschema/ydb Seeds and migration/seeder).
//
// A seed file runs as the queries YQL reads it into (internal/yqlquery), so a
// named expression or an action reaches the statements that use it, and all of
// them run in one serializable transaction with the row that records the seed:
// the file and its record commit together or not at all. YDB aborts the
// transaction of two conflicting ones that commits second, and nothing of it
// is applied, so the whole transaction runs again, at most five times. A
// scheme query, or a BATCH UPDATE or BATCH DELETE, runs on YDB only outside a
// transaction, so a file holding one is refused before anything runs. YDB has
// no savepoint, and a conflict ends the transaction it ran in, so a file that
// hits a duplicate key fails whole and nothing of it is applied.
//
// The ledger has the host's layout, so a ledger the embedded engine wrote
// stays readable here and the other way round: a row per seed path with the
// environment, the checksum and the moment the seed ran, keyed by the path.
package seeds

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb"
	ydb "github.com/ydb-platform/ydb-go-sdk/v3"

	native "github.com/stokaro/ptah-ydb-plugin/internal/nativeprotocol"
	"github.com/stokaro/ptah-ydb-plugin/internal/yqlquery"
	"github.com/stokaro/ptah-ydb-plugin/schema"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
)

// Table is the ledger's name, in the root of the session's database.
const Table = "schema_seeds"

// CreateStatement creates the ledger when it is absent and succeeds against
// one that exists. It is the host's statement: YDB declares the key in a
// clause of its own, every text column is Utf8, and the moment a seed ran is a
// Timestamp.
const CreateStatement = "CREATE TABLE IF NOT EXISTS `" + Table + "` (\n" +
	"    seed_path Utf8 NOT NULL,\n" +
	"    env Utf8 NOT NULL,\n" +
	"    checksum Utf8 NOT NULL,\n" +
	"    applied_at Timestamp NOT NULL,\n" +
	"    PRIMARY KEY (seed_path)\n" +
	")"

// RecordStatement writes the ledger row of a seed, with the path, the
// environment, the checksum and the moment as named parameters. UPSERT
// replaces the row a forced run finds, which is what the DELETE and INSERT
// pair does on the other engines, in one statement.
const RecordStatement = "DECLARE $seed_path AS Utf8; DECLARE $env AS Utf8; DECLARE $checksum AS Utf8; " +
	"DECLARE $applied_at AS Timestamp; " +
	"UPSERT INTO `" + Table + "` (seed_path, env, checksum, applied_at) VALUES ($seed_path, $env, $checksum, $applied_at)"

// ReadStatement reads the ledger in path order, at most limit rows.
func ReadStatement(limit uint32) string {
	return fmt.Sprintf("SELECT seed_path, checksum, env, applied_at FROM `%s` ORDER BY seed_path LIMIT %d", Table, uint64(limit)+1)
}

// layout is the ledger's columns and their types.
var layout = []struct{ name, kind string }{
	{"seed_path", "Utf8"}, {"env", "Utf8"}, {"checksum", "Utf8"}, {"applied_at", "Timestamp"},
}

// CheckLayout refuses a table named like the ledger that does not have its
// layout: the four columns with their types, none nullable, keyed by the path
// alone, and no index. Another layout is not adopted.
func CheckLayout(table *native.DescribeTableResult) error {
	refused := fmt.Errorf("the table %s does not have the seed ledger's layout, and it is not adopted", Table)
	if len(table.GetColumns()) != len(layout) || len(table.GetPrimaryKey()) != 1 || table.GetPrimaryKey()[0] != "seed_path" ||
		len(table.GetIndexes()) != 0 {
		return refused
	}
	for _, want := range layout {
		found := false
		for _, column := range table.GetColumns() {
			if column.GetName() != want.name {
				continue
			}
			kind, nullable, err := schema.ColumnType(column.GetType())
			if err != nil || kind != want.kind || nullable {
				return refused
			}
			found = true
		}
		if !found {
			return refused
		}
	}
	return nil
}

// Tx is what a seed run needs of a database transaction.
type Tx interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	Commit() error
	Rollback() error
}

// Begin opens a serializable transaction.
type Begin func(context.Context) (Tx, error)

// Attempts bounds how often a transaction YDB aborted for a conflicting one is
// run again.
const Attempts = 5

// Outcome is what a seed run proved.
type Outcome struct {
	Outcome protocol.SeedResult_Outcome
	// Reason is one line that names no statement or value, set for every
	// outcome but APPLIED.
	Reason string
}

// Refusal names the query of a file that YDB runs only outside a
// transaction, or the setting the file cannot be split under, and where it
// starts; nil when the file can run as a seed.
func Refusal(text string) (offset int, reason string, refused bool) {
	queries, err := yqlquery.Split(text)
	if refusal, ok := errors.AsType[*yqlquery.Refusal](err); ok {
		return refusal.Offset, refusal.Error(), true
	}
	if err != nil {
		return 0, err.Error(), true
	}
	for _, query := range queries {
		if query.Kind != yqlquery.Data {
			return query.Start, fmt.Sprintf("YDB runs a %s query only outside a transaction, and a seed runs in one; "+
				"move this statement to a migration", query.Kind), true
		}
	}
	return 0, "", false
}

// Failure is what a failed statement or commit proves about its transaction.
type Failure int

const (
	// Unproven proves nothing: a failed commit may have committed.
	Unproven Failure = iota
	// Conflict is a write that hit an existing key. Nothing of the
	// transaction stayed.
	Conflict
	// Aborted is an abort YDB answered for a conflicting transaction
	// (`Transaction locks invalidated`). Nothing of the transaction stayed,
	// and running it again may succeed.
	Aborted
	// Rejected is an abort that running the transaction again would repeat.
	// Nothing of the transaction stayed.
	Rejected
)

// Classify reads what a YDB failure proves.
func Classify(err error) Failure {
	switch {
	case KeyConflict(err):
		return Conflict
	case Retryable(err):
		return Aborted
	case ydb.IsOperationError(err, Ydb.StatusIds_ABORTED):
		return Rejected
	default:
		return Unproven
	}
}

// Apply runs the data queries of a seed file and its ledger row in one
// serializable transaction and commits it, and runs the whole transaction again
// when YDB aborts it for a conflicting one. queries are the file's queries,
// which must all be data queries. classify reads what a failure proves.
func Apply(ctx context.Context, begin Begin, classify func(error) Failure, queries []yqlquery.Query, seed *protocol.Seed, now func() time.Time) Outcome {
	return transaction(ctx, begin, classify, func(ctx context.Context, tx Tx) error {
		for _, query := range queries {
			if _, err := tx.ExecContext(ctx, query.Text); err != nil {
				return err
			}
		}
		return record(ctx, tx, seed, now)
	})
}

// Record writes the ledger row of a seed in a transaction of its own.
func Record(ctx context.Context, begin Begin, classify func(error) Failure, seed *protocol.Seed, now func() time.Time) Outcome {
	return transaction(ctx, begin, classify, func(ctx context.Context, tx Tx) error {
		return record(ctx, tx, seed, now)
	})
}

func record(ctx context.Context, tx Tx, seed *protocol.Seed, now func() time.Time) error {
	_, err := tx.ExecContext(ctx, RecordStatement, sql.Named("seed_path", seed.GetPath()), sql.Named("env", seed.GetEnv()),
		sql.Named("checksum", seed.GetChecksum()), sql.Named("applied_at", now().UTC()))
	return err
}

// transaction runs body in a serializable transaction and commits it.
//
// Nothing is committed before the commit is sent, so a failure up to it proves
// that nothing of the transaction stayed: a statement that hit an existing key
// is KEY_CONFLICT, an abort for a conflicting transaction is run again, and
// any other failure is NOT_APPLIED. A commit YDB refuses for a key conflict or
// an abort proves the same. Any other failure of the commit leaves the outcome
// unknown.
func transaction(ctx context.Context, begin Begin, classify func(error) Failure, body func(context.Context, Tx) error) Outcome {
	for attempt := range Attempts {
		outcome, again := attemptOnce(ctx, begin, classify, body)
		if !again {
			return outcome
		}
		if attempt == Attempts-1 {
			break
		}
		timer := time.NewTimer(time.Duration(attempt+1) * 50 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return Outcome{Outcome: protocol.SeedResult_NOT_APPLIED, Reason: "the request ended before the seed's transaction ran again; nothing of the seed was applied"}
		case <-timer.C:
		}
	}
	return Outcome{Outcome: protocol.SeedResult_NOT_APPLIED, Reason: fmt.Sprintf(
		"YDB aborted the seed's transaction for a conflicting one %d times; nothing of the seed was applied", Attempts)}
}

// attemptOnce runs one attempt and reports whether to run it again.
func attemptOnce(ctx context.Context, begin Begin, classify func(error) Failure, body func(context.Context, Tx) error) (Outcome, bool) {
	tx, err := begin(ctx)
	if err != nil {
		return Outcome{Outcome: protocol.SeedResult_NOT_APPLIED, Reason: "the seed's transaction could not begin: " + status(err)}, false
	}
	if err := body(ctx, tx); err != nil {
		// A failed statement may have ended the transaction on the server
		// already, and then the rollback answers `Transaction not found`;
		// the statement's failure is the one that matters.
		_ = tx.Rollback()
		return uncommitted(classify(err), err)
	}
	if err := tx.Commit(); err != nil {
		if failure := classify(err); failure != Unproven {
			return uncommitted(failure, err)
		}
		return Outcome{Outcome: protocol.SeedResult_UNKNOWN, Reason: "the seed's commit was not confirmed: " + status(err)}, false
	}
	return Outcome{Outcome: protocol.SeedResult_APPLIED}, false
}

// uncommitted is the outcome of a transaction that ended before it committed.
func uncommitted(failure Failure, err error) (Outcome, bool) {
	switch failure {
	case Conflict:
		return Outcome{Outcome: protocol.SeedResult_KEY_CONFLICT,
			Reason: "a statement of the seed hit an existing key, and nothing of the seed was applied"}, false
	case Aborted:
		return Outcome{}, true
	default:
		return Outcome{Outcome: protocol.SeedResult_NOT_APPLIED, Reason: "a statement of the seed failed: " + status(err) +
			"; nothing of the seed was applied"}, false
	}
}

// constraintViolated is the issue code YDB answers a write that hits an
// existing key with, under PRECONDITION_FAILED (`Conflict with existing key`).
const constraintViolated = 2012

// KeyConflict reports a statement or commit that hit an existing key.
func KeyConflict(err error) bool {
	if !ydb.IsOperationError(err, Ydb.StatusIds_PRECONDITION_FAILED) {
		return false
	}
	conflict := false
	ydb.IterateByIssues(err, func(_ string, code Ydb.StatusIds_StatusCode, _ uint32) {
		conflict = conflict || code == constraintViolated
	})
	return conflict
}

// deterministicAborts are the issue codes of an ABORTED answer that running
// the transaction again would repeat: a data query too large for one shard
// program answers ABORTED with issue 200509 every time.
var deterministicAborts = map[Ydb.StatusIds_StatusCode]bool{200509: true}

// Retryable reports an abort YDB answered for a conflicting transaction,
// after which nothing of the transaction stayed and running it again may
// succeed.
func Retryable(err error) bool {
	if !ydb.IsOperationError(err, Ydb.StatusIds_ABORTED) {
		return false
	}
	deterministic := false
	ydb.IterateByIssues(err, func(_ string, code Ydb.StatusIds_StatusCode, _ uint32) {
		deterministic = deterministic || deterministicAborts[code]
	})
	return !deterministic
}

// status names a failure by its YDB status, never by its text, which can
// quote a statement or a value.
func status(err error) string {
	for _, code := range []Ydb.StatusIds_StatusCode{
		Ydb.StatusIds_BAD_REQUEST, Ydb.StatusIds_UNAUTHORIZED, Ydb.StatusIds_INTERNAL_ERROR, Ydb.StatusIds_ABORTED,
		Ydb.StatusIds_UNAVAILABLE, Ydb.StatusIds_OVERLOADED, Ydb.StatusIds_SCHEME_ERROR, Ydb.StatusIds_GENERIC_ERROR,
		Ydb.StatusIds_TIMEOUT, Ydb.StatusIds_BAD_SESSION, Ydb.StatusIds_PRECONDITION_FAILED, Ydb.StatusIds_ALREADY_EXISTS,
		Ydb.StatusIds_NOT_FOUND, Ydb.StatusIds_SESSION_EXPIRED, Ydb.StatusIds_CANCELLED, Ydb.StatusIds_UNDETERMINED,
		Ydb.StatusIds_UNSUPPORTED, Ydb.StatusIds_SESSION_BUSY, Ydb.StatusIds_EXTERNAL_ERROR,
	} {
		if ydb.IsOperationError(err, code) {
			return "YDB answered " + code.String()
		}
	}
	switch {
	case errors.Is(err, context.Canceled):
		return "the request was canceled"
	case errors.Is(err, context.DeadlineExceeded):
		return "the request ran out of time"
	default:
		return "the connection failed"
	}
}
