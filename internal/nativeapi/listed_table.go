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

// ListedTableSettleBudget is how long [DescribeListedTable] waits for a row
// table that its directory lists and the table service does not describe.
//
// The listing and the table service each follow a scheme change at a moment of
// their own. An async replication creates its replica tables after CREATE
// ASYNC REPLICATION returns, and a read in that moment finds the replica
// listed while DescribeTable answers SCHEME_ERROR. Measured on local-ydb
// 25.1.4.7 and 26.2.1.14 right after the statement: the listing named the
// replica, DescribeTable answered SCHEME_ERROR, and it described the replica
// 50 ms later, on the first retry. Neither answer says a path is being created,
// so the budget bounds the wait instead. The host's built-in YDB reader waits
// the same 10 seconds.
const ListedTableSettleBudget = 10 * time.Second

// ErrTableNoLongerListed reports a row table that its directory listed and a
// later listing of the same directory does not name: another operation dropped
// it while the read ran.
var ErrTableNoLongerListed = errors.New("the directory no longer lists the table")

// DescribeListedTable describes the row table name in the absolute directory,
// which a listing of directory named, through describe, a reader such as
// [Table.Describe], and lists the directory again through list.
//
// Only a SCHEME_ERROR from describe is retried; any other answer, success or
// failure, is returned as describe gave it. After a SCHEME_ERROR it lists the
// directory again. While the listing names the table, it waits and asks again,
// starting at 50 ms and doubling up to a second, until describe answers
// otherwise or [ListedTableSettleBudget] runs out; the last SCHEME_ERROR is
// then returned with the table's path. A listing that no longer names the
// table returns an error wrapping [ErrTableNoLongerListed], and a listing that
// fails returns its own error: neither reads as a table that is absent. The
// context ends the wait early, with an error wrapping the context's.
func DescribeListedTable(
	ctx context.Context,
	describe func(context.Context, string) (*native.DescribeTableResult, error),
	list DirectoryLister,
	directory, name string,
) (*native.DescribeTableResult, error) {
	absolute := path.Join(directory, name)
	deadline := time.Now().Add(ListedTableSettleBudget)
	wait := 50 * time.Millisecond
	for {
		described, err := describe(ctx, absolute)
		if !isSchemeError(err) {
			return described, err
		}
		listed, listErr := list(ctx, directory)
		switch {
		case listErr != nil:
			return nil, fmt.Errorf("describe YDB table %s: %w; listing its directory again failed: %w", absolute, err, listErr)
		case !listsTable(listed, name):
			return nil, fmt.Errorf("describe YDB table %s: %w: %w", absolute, err, ErrTableNoLongerListed)
		case !time.Now().Before(deadline):
			return nil, fmt.Errorf("describe YDB table %s: %w; its directory has listed it for %s and YDB has not described it: "+
				"another operation may still be creating it, or the session may not see it", absolute, err, ListedTableSettleBudget)
		}
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("describe YDB table %s: %w; the read stopped waiting for the table its directory lists: %w", absolute, err, ctx.Err())
		case <-time.After(wait):
		}
		wait = min(2*wait, time.Second)
	}
}

// listsTable reports whether a listing names the row table name.
func listsTable(listed *Ydb_Scheme.ListDirectoryResult, name string) bool {
	return slices.ContainsFunc(listed.GetChildren(), func(entry *Ydb_Scheme.Entry) bool {
		return entry.GetName() == name && entry.GetType() == Ydb_Scheme.Entry_TABLE
	})
}
