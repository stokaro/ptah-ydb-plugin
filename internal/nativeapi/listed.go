package nativeapi

import (
	"context"
	"errors"
	"fmt"
	"path"
	"slices"
	"time"

	native "github.com/stokaro/ptah-ydb-plugin/internal/nativeprotocol"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb_Scheme"
)

// ListedSettleBudget is how long [DescribeListed] waits for an object that a
// listing names and its own service does not describe.
//
// The scheme listing, the table service and the topic service each follow a
// scheme change at a moment of their own. An async replication creates its
// replica table, and the changefeed on its source table, after CREATE ASYNC
// REPLICATION returns, and a read in that moment can find either named and
// not described. Measured on local-ydb 25.1.4.7 and 26.2.1.14 right after the
// statement: the directory listed the replica while DescribeTable answered
// SCHEME_ERROR, and the table service described it 50 ms later, on the first
// retry; on 25.1.4.7 the source table's description also named the new
// changefeed while DescribeTopic answered SCHEME_ERROR for its topic. No
// answer says a path is being created, so the budget bounds the wait instead.
// The host's built-in YDB reader waits the same 10 seconds for a listed table.
const ListedSettleBudget = 10 * time.Second

// ErrNoLongerListed reports an object that a listing named and a later
// listing does not: another operation dropped it while the read ran.
var ErrNoLongerListed = errors.New("the listing that named it no longer does")

// DescribeListed describes an object that a listing named. describe reads
// the object, and listed lists again and reports whether the listing still
// names it. subject names the object in an error, as "YDB table /local/t".
//
// Only a SCHEME_ERROR from describe is retried; any other answer, success or
// failure, is returned as describe gave it. After a SCHEME_ERROR it lists
// again. While the listing names the object, it waits and asks again,
// starting at 50 ms and doubling up to a second, until describe answers
// otherwise or [ListedSettleBudget] runs out; the last SCHEME_ERROR is then
// returned with subject. A listing that no longer names the object returns an
// error wrapping [ErrNoLongerListed], and a listing that fails returns its own
// error: neither reads as an object that is absent. The context ends the wait
// early, with an error wrapping the context's.
func DescribeListed[T any](
	ctx context.Context,
	subject string,
	describe func(context.Context) (T, error),
	listed func(context.Context) (bool, error),
) (T, error) {
	var none T
	deadline := time.Now().Add(ListedSettleBudget)
	wait := 50 * time.Millisecond
	for {
		described, err := describe(ctx)
		if !isSchemeError(err) {
			return described, err
		}
		still, listErr := listed(ctx)
		switch {
		case listErr != nil:
			return none, fmt.Errorf("describe %s: %w; listing it again failed: %w", subject, err, listErr)
		case !still:
			return none, fmt.Errorf("describe %s: %w: %w", subject, err, ErrNoLongerListed)
		case !time.Now().Before(deadline):
			return none, fmt.Errorf("describe %s: %w; it has been listed for %s and YDB has not described it: "+
				"another operation may still be creating it, or the session may not see it", subject, err, ListedSettleBudget)
		}
		select {
		case <-ctx.Done():
			return none, fmt.Errorf("describe %s: %w; the read stopped waiting for it: %w", subject, err, ctx.Err())
		case <-time.After(wait):
		}
		wait = min(2*wait, time.Second)
	}
}

// DescribeListedTable describes the row table name in the absolute directory,
// which a listing of directory named, through describe, a reader such as
// [Table.Describe], as [DescribeListed] describes an object. It lists the
// directory again through list, and the listing names the table while it
// holds a row table of that name.
func DescribeListedTable(
	ctx context.Context,
	describe func(context.Context, string) (*native.DescribeTableResult, error),
	list DirectoryLister,
	directory, name string,
) (*native.DescribeTableResult, error) {
	absolute := path.Join(directory, name)
	return DescribeListed(ctx, "YDB table "+absolute,
		func(ctx context.Context) (*native.DescribeTableResult, error) { return describe(ctx, absolute) },
		func(ctx context.Context) (bool, error) {
			listed, err := list(ctx, directory)
			if err != nil {
				return false, err
			}
			return slices.ContainsFunc(listed.GetChildren(), func(entry *Ydb_Scheme.Entry) bool {
				return entry.GetName() == name && entry.GetType() == Ydb_Scheme.Entry_TABLE
			}), nil
		},
	)
}
