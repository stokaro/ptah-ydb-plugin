package backend_test

import (
	"testing"

	qt "github.com/frankban/quicktest"
	protocol "ptah.run/provider-sdk/protocol/experimental"
	"ptah.run/provider-sdk/provider"

	"github.com/stokaro/ptah-ydb-plugin/internal/backend"
	"github.com/stokaro/ptah-ydb-plugin/internal/replication"
	"github.com/stokaro/ptah-ydb-plugin/internal/replicationcompare"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcomparison"
	"github.com/stokaro/ptah-ydb-plugin/schema"
)

// Ported from the host's
// TestReplicationComparison_AcceptsATargetWithoutTheKeyWhenNothingChanges: a
// comparison of transfers with nothing declared and nothing read plans
// nothing, so a target without transfers is no reason to refuse it.
func TestTransferComparisonNeedsNoCapabilityToPlanNothing(t *testing.T) {
	c := testcheck.New(t)
	request := objectRequest(c, schema.TransferProperties, replicationcompare.TransferServiceID, "ingest", nil, nil)
	request.Capabilities[replication.CapabilityTransfers] = false

	result, err := backend.New().CompareModels(c.Context(), request)

	c.Assert(err, qt.IsNil)
	c.Assert(result.Changes, qt.HasLen, 0)
	c.Assert(result.Undecided, qt.HasLen, 0)
}

// Ported from the host's TestCompare_YDBReplicationCoverage: a declared
// replication the read could not describe, as on a cluster serving no
// replication API, is neither created nor dropped; the comparison says so.
func TestReplicationComparisonWithholdsTheCreationTheReadCannotSee(t *testing.T) {
	c := testcheck.New(t)
	request := objectRequest(c, schema.ReplicationProperties, replicationcompare.ReplicationServiceID, "mirror", declaredReplication(c, new(mirror())), nil)
	limitSubject(request.Current, schema.ReplicationProperties, "/local", "app", "mirror", protocol.ModelKnowledge_UNINSPECTED)

	result, err := backend.New().CompareModels(c.Context(), request)

	c.Assert(err, qt.IsNil)
	c.Assert(provider.ValidateComparedModels(backend.Description(), request, result), qt.IsNil)
	c.Assert(result.Changes, qt.HasLen, 0)
	c.Assert(result.Undecided, qt.HasLen, 1)
}

// Ported from the host's TestCompare_YDBReplicationCoverage: a replication and
// a transfer the source does not describe are kept as YDB holds them.
func TestReplicationComparisonKeepsWhatTheSourceDoesNotDescribe(t *testing.T) {
	c := testcheck.New(t)
	for _, request := range []*protocol.CompareModelsRequest{
		objectRequest(c, schema.ReplicationProperties, replicationcompare.ReplicationServiceID, "mirror", nil, readReplication(c, new(mirror()), replication.StateRunning)),
		objectRequest(c, schema.TransferProperties, replicationcompare.TransferServiceID, "ingest", nil, readTransfer(c, new(ingest()), replication.StateRunning)),
	} {
		request.Desired.Coverage.Kinds[0].Sources[0].Outside = testcomparison.Knowledge(protocol.ModelKnowledge_UNINSPECTED)

		result, err := backend.New().CompareModels(c.Context(), request)

		c.Assert(err, qt.IsNil)
		c.Assert(result.Changes, qt.HasLen, 0)
		c.Assert(result.Undecided, qt.HasLen, 0)
		c.Assert(result.Desired.Records, qt.HasLen, 1, qt.Commentf("%s", request.Kinds[0]))
	}
}
