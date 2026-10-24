package nativeapi

import (
	"context"
	"fmt"
	"path"
	"strings"
	"time"

	"github.com/ydb-platform/ydb-go-genproto/Ydb_Operation_V1"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb_Operations"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb_Table"
)

// buildOperationKind is the kind the operation service lists index builds and
// column backfills under.
const buildOperationKind = "buildindex"

// lastPageToken is the page token the operation service ends a listing with.
const lastPageToken = "0"

// Builds finds and cancels the build operations YDB runs for a schema
// statement, as the host's embedded YDB engine does when a migration's
// statement timeout stops one (internal/dbschema/ydb).
//
// YDB runs two schema statements as builds rather than as quick changes to
// the scheme: an index added to a table that exists, and a column added with
// a default (26.1 and newer), which fills the existing rows. A client that
// stops waiting for either does not stop the build: measured by the host on
// 26.2.1.14 and 25.1.4.7, `ALTER TABLE ... ADD INDEX` whose client gave up
// after 800 ms left the index `not ready to use`, and five seconds later it
// held every row. The operation service lists the build under the kind
// `buildindex`, with the table's absolute path, and cancels it: after the
// cancellation the index is absent and the operation reports CANCELLED.
type Builds struct {
	operations Ydb_Operation_V1.OperationServiceClient
	root       string
}

// NewBuilds returns a [Builds] that asks operations about the tables under
// root, the absolute path a relative table name resolves against.
func NewBuilds(operations Ydb_Operation_V1.OperationServiceClient, root string) *Builds {
	return &Builds{operations: operations, root: root}
}

// BuildOutcome is what became of the build [Builds.CancelRunning] looked for.
type BuildOutcome uint8

const (
	// BuildNotFound means no build was running on the table while it looked.
	BuildNotFound BuildOutcome = iota
	// BuildStopped means the build was canceled, or failed, and ended without
	// completing, which leaves nothing of it applied.
	BuildStopped
	// BuildCompleted means the build completed before the cancellation could
	// stop it.
	BuildCompleted
	// BuildUnsettled means the build cannot be said to have ended either way:
	// it had not ended when the wait ran out, or more than one build was
	// running on the table and none was canceled.
	BuildUnsettled
)

// BuildWait bounds how long [Builds.CancelRunning] waits.
type BuildWait struct {
	// Appear is how long to look for a running build on the table. A
	// statement whose client gave up may not have started its build yet.
	Appear time.Duration
	// Settle is how long to wait for a canceled build to end.
	Settle time.Duration
	// Poll is the interval between two looks.
	Poll time.Duration
}

// DefaultBuildWait is the host's wait: a build is looked for for two seconds
// and given thirty to end once canceled.
var DefaultBuildWait = BuildWait{Appear: 2 * time.Second, Settle: 30 * time.Second, Poll: 100 * time.Millisecond}

// CancelRunning cancels the build running on table, a path relative to the
// root or an absolute one, and waits for it to end. It cancels only a build it
// found running on that table, and only when it found exactly one; it reports
// which way the build ended and never guesses. An error means the operation
// service could not be asked, and says nothing about the build.
func (b *Builds) CancelRunning(ctx context.Context, table string, wait BuildWait) (BuildOutcome, error) {
	target := b.absolute(table)
	id, outcome, err := b.findRunning(ctx, target, wait)
	if err != nil || id == "" {
		return outcome, err
	}
	// The answer to the cancellation is not the outcome: a build that ended a
	// moment before answers that it cannot be canceled, and one that is being
	// canceled has not ended yet. What GetOperation reports is.
	if _, err := b.operations.CancelOperation(ctx, &Ydb_Operations.CancelOperationRequest{Id: id}); err != nil {
		return BuildUnsettled, fmt.Errorf("cancel the YDB build on %s: transport failed", target)
	}
	return b.settle(ctx, id, target, wait)
}

func (b *Builds) findRunning(ctx context.Context, target string, wait BuildWait) (string, BuildOutcome, error) {
	deadline := time.Now().Add(wait.Appear)
	for {
		running, err := b.running(ctx, target)
		if err != nil {
			return "", BuildUnsettled, err
		}
		switch {
		case len(running) == 1:
			return running[0], BuildNotFound, nil
		case len(running) > 1:
			// YDB runs one scheme operation on a table at a time, so two builds
			// on one table are not this statement's alone, and canceling either
			// could stop somebody else's.
			return "", BuildUnsettled, nil
		case !time.Now().Before(deadline):
			return "", BuildNotFound, nil
		}
		if err := sleep(ctx, wait.Poll); err != nil {
			return "", BuildUnsettled, err
		}
	}
}

// running lists the ids of the builds that have not ended on target. The
// operation service ends a listing with the page token "0", not with an empty
// one, and answers a request for page "0" with the first page again, so a
// token already asked for ends the listing too.
func (b *Builds) running(ctx context.Context, target string) ([]string, error) {
	var ids []string
	asked := map[string]bool{"": true}
	request := &Ydb_Operations.ListOperationsRequest{Kind: buildOperationKind}
	for {
		response, err := b.operations.ListOperations(ctx, request)
		if err != nil {
			return nil, fmt.Errorf("list the YDB builds: transport failed")
		}
		if response.GetStatus() != Ydb.StatusIds_SUCCESS {
			return nil, fmt.Errorf("list the YDB builds: %s", response.GetStatus())
		}
		for _, operation := range response.GetOperations() {
			if !operation.GetReady() && buildPath(operation) == target {
				ids = append(ids, operation.GetId())
			}
		}
		next := response.GetNextPageToken()
		if next == lastPageToken || asked[next] {
			return ids, nil
		}
		asked[next] = true
		request = &Ydb_Operations.ListOperationsRequest{Kind: buildOperationKind, PageToken: next}
	}
}

func (b *Builds) settle(ctx context.Context, id, target string, wait BuildWait) (BuildOutcome, error) {
	deadline := time.Now().Add(wait.Settle)
	for {
		response, err := b.operations.GetOperation(ctx, &Ydb_Operations.GetOperationRequest{Id: id})
		if err != nil {
			return BuildUnsettled, fmt.Errorf("read the YDB build on %s: transport failed", target)
		}
		operation := response.GetOperation()
		switch {
		case operation.GetReady() && operation.GetStatus() == Ydb.StatusIds_SUCCESS:
			return BuildCompleted, nil
		case operation.GetReady():
			return BuildStopped, nil
		case !time.Now().Before(deadline):
			return BuildUnsettled, nil
		}
		if err := sleep(ctx, wait.Poll); err != nil {
			return BuildUnsettled, err
		}
	}
}

// buildPath is the absolute path of the table a build operation works on, or
// "" when its metadata does not say.
func buildPath(operation *Ydb_Operations.Operation) string {
	var metadata Ydb_Table.IndexBuildMetadata
	if operation.GetMetadata() == nil || operation.GetMetadata().UnmarshalTo(&metadata) != nil {
		return ""
	}
	return metadata.GetDescription().GetPath()
}

// absolute resolves table against the root.
func (b *Builds) absolute(table string) string {
	if strings.HasPrefix(table, "/") {
		return path.Clean(table)
	}
	return path.Join(b.root, table)
}

func sleep(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
