package backend_test

import (
	"testing"

	qt "github.com/frankban/quicktest"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"google.golang.org/protobuf/proto"

	"github.com/stokaro/ptah-ydb-plugin/internal/backend"
	"github.com/stokaro/ptah-ydb-plugin/internal/streaming"
	"github.com/stokaro/ptah-ydb-plugin/internal/streamingplan"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
	"github.com/stokaro/ptah-ydb-plugin/schema"
)

// The host hands the streaming query service the desired and current objects
// of every kind and the subject of every native change, so that call sees a
// pair of changes two other services plan. These tests plan a topic the read
// holds in topicDirectory and a coordination node the source declares in
// nodeDirectory, and no streaming query.

// directoryConflictRequest is the streaming service's request of a plan that
// drops the topic topicDirectory/events and creates the coordination node
// nodeDirectory/holder.
func directoryConflictRequest(c *qt.C, topicDirectory, nodeDirectory string) *protocol.PlanFeaturesRequest {
	c.Helper()
	request := &protocol.PlanFeaturesRequest{Engine: "ydb", PlanningId: streamingplan.ServiceID, DatabasePath: "/local", ContextKinds: []string{schema.StreamingQueryProperties},
		TargetFeatures: backend.Description().Features, Capabilities: map[string]bool{streaming.Capability: true, "transactional_ddl": false},
		Identifiers:    &protocol.ComparisonIdentifiers{Rules: proto.CloneOf(backend.Description().Engines[0].Identifiers)},
		DesiredObjects: streamingContextState(c, protocol.ValueInput_DESIRED, nil), CurrentObjects: streamingContextState(c, protocol.ValueInput_OBSERVED, nil)}
	topic, err := schema.CaptureObject(&protocol.Reference{Engine: "ydb", Kind: schema.TopicProperties, Database: "/local", Schema: topicDirectory, Name: "events"}, readTopic())
	c.Assert(err, qt.IsNil)
	appendContextValue(c, request.CurrentObjects, topic)
	node, err := schema.CaptureCoordinationIntent(&protocol.Reference{Engine: "ydb", Kind: schema.CoordinationProperties, Database: "/local", Schema: nodeDirectory, Name: "holder"},
		schema.CoordinationIntent{ReadConsistencyMode: new("strict")})
	c.Assert(err, qt.IsNil)
	appendContextValue(c, request.DesiredObjects, node)
	request.ChangedSubjects = []*protocol.ComparisonIdentity{proto.CloneOf(request.DesiredObjects.Records[0].Subject), proto.CloneOf(request.CurrentObjects.Records[0].Subject)}
	return request
}

// A coordination node created at the path of a directory above a topic the
// plan drops is refused in the streaming service's call, before anything is
// planned, in the host's embedded engine's words.
func TestPlanFeaturesRefusesACreationOverAnotherServicesDrop_FailurePath(t *testing.T) {
	c := testcheck.New(t)

	result, err := backend.New().PlanFeatures(c.Context(), directoryConflictRequest(c, "app/holder", "app"))

	c.Assert(err, qt.IsNil)
	c.Assert(result.Diagnostics, qt.HasLen, 1)
	c.Assert(result.Diagnostics[0].GetMessage(), qt.Matches, "this plan drops app/holder/events and creates an object at app/holder, which is a directory in the database. .*")
	c.Assert(result.Diagnostics[0].GetCode(), qt.Equals, protocol.PlanningDiagnostic_INVALID_SCHEMA)
	c.Assert(result.Contributions, qt.HasLen, 0)
}

// A coordination node created below the path of a topic the plan drops is
// one YDB serves once the drop has run, so the streaming service plans on.
func TestPlanFeaturesKeepsACreationBelowAnotherServicesDrop_HappyPath(t *testing.T) {
	c := testcheck.New(t)

	result, err := backend.New().PlanFeatures(c.Context(), directoryConflictRequest(c, "app", "app/events"))

	c.Assert(err, qt.IsNil)
	c.Assert(result.Diagnostics, qt.HasLen, 0)
}
