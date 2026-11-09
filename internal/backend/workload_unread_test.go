package backend_test

import (
	"testing"

	qt "github.com/frankban/quicktest"
	"google.golang.org/protobuf/proto"
	protocol "ptah.run/provider-sdk/protocol/experimental"

	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcomparison"
	"github.com/stokaro/ptah-ydb-plugin/internal/workload"
	"github.com/stokaro/ptah-ydb-plugin/schema"
)

// The pool default as a read of the target declares it, compared against a
// read that did not establish the pools, as a read of a dev realm does not, is
// left undecided rather than planned: nothing says whether the pool there
// matches. Ported from the host's
// TestRehearsalBaselineMatchesObservedDefaultPoolWithoutDDL, whose comparison
// against an ordinary read of the realm was refused.
func TestWorkloadComparisonLeavesTheDefaultPoolOfAnUnreadTargetUndecided(t *testing.T) {
	c := testcheck.New(t)
	request := workloadRequest(c, schema.ResourcePoolProperties, map[string]proto.Message{"default": pool(workload.PoolSpec{})}, nil)
	request.Current.Coverage.Kinds[0].Sources[0].Outside = testcomparison.Knowledge(protocol.ModelKnowledge_UNINSPECTED)

	result := compareWorkload(c, request)

	c.Assert(result.Changes, qt.HasLen, 0)
	c.Assert(result.Undecided, qt.HasLen, 1)
	c.Assert(result.Undecided[0].Subject.Name.Normalized, qt.Equals, workload.DefaultPool)
	c.Assert(result.Undecided[0].Reason, qt.Equals, "The current resource pool or its absence was not established")
}
