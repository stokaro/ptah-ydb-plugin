package embedstore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math/rand/v2"
	"strconv"
	"time"

	"github.com/stokaro/ptah-ydb-plugin/internal/releaseline"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb"
	ydb "github.com/ydb-platform/ydb-go-sdk/v3"
)

// Store is the embedding store of one session: its database/sql pool, whose
// connections resolve a relative name against the session's root, the native
// driver, the root as an absolute path, and what the server line can do with
// vectors.
type Store struct {
	db     *sql.DB
	driver *ydb.Driver
	root   string
	caps   Capabilities
}

// New returns the store of a session.
func New(db *sql.DB, driver *ydb.Driver, root string, caps Capabilities) *Store {
	return &Store{db: db, driver: driver, root: root, caps: caps}
}

// Capabilities are what the connected server line does that the store asks
// about, by the host's capability keys.
type Capabilities struct {
	// WideTimestamps is wide_date_time_types: the run-state timestamps are
	// Timestamp64, which YDB 25.2 and newer have.
	WideTimestamps bool
	// VectorIndexes is vector_indexes: the line builds a vector_kmeans_tree
	// index, which 25.1 does only with EnableVectorIndex on.
	VectorIndexes bool
	// MaintainedOnWrite is vector_index_maintained_on_write: a vector index
	// takes in rows written after its build. 25.2 and older answer a search as
	// the table stood at the build.
	MaintainedOnWrite bool
}

// LineCapabilities are the capabilities of the server that reports version,
// the host's YDB presets (YDB251 to YDB262), with flags, the database's
// feature flags where they were read, deciding the vector index of 25.1. A
// version the store cannot read is the newest line's.
func LineCapabilities(version string, flags map[string]bool) Capabilities {
	newer := func(atMajor, atMinor int) bool { return releaseline.AtLeast(version, atMajor, atMinor) }
	caps := Capabilities{WideTimestamps: newer(25, 2), VectorIndexes: newer(25, 2), MaintainedOnWrite: newer(25, 3)}
	if enabled, listed := flags["EnableVectorIndex"]; listed {
		caps.VectorIndexes = enabled
	}
	return caps
}

// named binds arguments to the parameters $p1, $p2, ... a statement names
// them by. YQL has no positional parameter, and the driver binds an argument
// only by name.
func named(arguments ...any) []any {
	bound := make([]any, len(arguments))
	for i, argument := range arguments {
		bound[i] = sql.Named("p"+strconv.Itoa(i+1), argument)
	}
	return bound
}

// maxAttempts bounds how often a transaction YDB aborted runs again. Each
// attempt rereads what it compares, so running again is correct, and the bound
// is the host's.
const maxAttempts = 12

// errUndetermined marks a commit whose outcome nothing proves.
var errUndetermined = errors.New("the commit's outcome is unknown")

// inTransaction runs fn in a serializable read-write transaction and commits
// it, running it again when YDB aborts it in a way that proves nothing
// committed: a conflict with another transaction, an overloaded or an
// unavailable server. An error fn returns ends the attempts and is returned
// with the transaction rolled back. A commit that fails any other way wraps
// errUndetermined.
func (s *Store) inTransaction(ctx context.Context, fn func(*sql.Tx) error) error {
	var last error
	for attempt := range maxAttempts {
		if attempt > 0 {
			if err := pause(ctx, attempt); err != nil {
				return errors.Join(err, last)
			}
		}
		err := s.attempt(ctx, fn)
		if err == nil || !retryable(err) {
			return err
		}
		last = err
	}
	return fmt.Errorf("another transaction kept changing the rows this one read, %d times: %w", maxAttempts, last)
}

func (s *Store) attempt(ctx context.Context, fn func(*sql.Tx) error) error {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable})
	if err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		_ = tx.Rollback()
		return err
	}
	if err := tx.Commit(); err != nil {
		if retryable(err) {
			return err
		}
		return fmt.Errorf("%w: %s", errUndetermined, statusName(err))
	}
	return nil
}

// retryable reports a refusal that proves nothing committed and that running
// the transaction again can clear.
func retryable(err error) bool {
	return ydb.IsOperationErrorTransactionLocksInvalidated(err) ||
		ydb.IsOperationError(err, Ydb.StatusIds_ABORTED, Ydb.StatusIds_OVERLOADED, Ydb.StatusIds_UNAVAILABLE)
}

// pause waits before an attempt, longer each time and with jitter.
func pause(ctx context.Context, attempt int) error {
	base := min(10*time.Millisecond<<attempt, time.Second)
	// #nosec G404 -- jitter, not a secret.
	wait := base/2 + time.Duration(rand.Int64N(int64(base)))
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// statusName names a failure by its YDB status, never by its text, which can
// quote a statement or a value.
func statusName(err error) string {
	if operation := ydb.OperationError(err); operation != nil {
		return "YDB answered " + operation.Name()
	}
	if transport := ydb.TransportError(err); transport != nil {
		return "the transport answered " + transport.Name()
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return "the call ended before YDB answered"
	}
	return "the connection failed"
}
