//go:build integration

package integration_test

import (
	"testing"

	qt "github.com/frankban/quicktest"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb_Coordination"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb_Scheme"
	"google.golang.org/protobuf/proto"
	protocol "ptah.run/provider-sdk/protocol/experimental"
	"ptah.run/provider-sdk/provider"

	"github.com/stokaro/ptah-ydb-plugin/internal/coordinationplan"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcomparison"
	"github.com/stokaro/ptah-ydb-plugin/internal/testplanning"
	"github.com/stokaro/ptah-ydb-plugin/internal/testsource"
	"github.com/stokaro/ptah-ydb-plugin/schema"
)

func TestCoordinationComparisonOfflineSubprocess(t *testing.T) {
	c := testcheck.New(t)
	binary, digest := providerBinary(c)
	client := startProvider(c, binary, digest)
	source, err := testsource.Request(`{"coordination_nodes":{"node":{"schema":"app","read_consistency_mode":"strict"}}}`)
	c.Assert(err, qt.IsNil)
	decoded, err := client.DecodeSource(t.Context(), source)
	c.Assert(err, qt.IsNil)
	request, err := testcomparison.Request(&schema.CoordinationIntent{}, &Ydb_Coordination.DescribeNodeResult{Config: &Ydb_Coordination.Config{}, Self: &Ydb_Scheme.Entry{Owner: "access-owner"}})
	c.Assert(err, qt.IsNil)
	// The declaration has no database. The host supplies an explicit bound
	// default before the same process compares it with a captured observation.
	request.Desired.Records[0].Value = decoded.Documents[0].Fragments[0].Values[0].Value
	request.Desired.Records[0].Subject.Database.Defaulted = true
	compared, err := client.CompareModels(t.Context(), request)
	c.Assert(err, qt.IsNil)
	c.Assert(compared.Changes, qt.HasLen, 1)
	change := compared.Changes[0].Value
	c.Assert(string(change.Value.Payload), qt.Contains, "access-owner")
	c.Assert(change.ChangeSemantics.Access, qt.Equals, protocol.ChangeSemantics_UNCHANGED)
	normalized, err := client.NormalizeValues(t.Context(), &protocol.NormalizeValuesRequest{Engine: "ydb", Values: []*protocol.ValueInput{{Id: change.Id, Representation: protocol.ValueInput_CHANGE, Value: change.Value}}})
	c.Assert(err, qt.IsNil)
	c.Assert(proto.Equal(change, normalized.Values[0]), qt.IsTrue)

	planning := &protocol.PlanFeaturesRequest{Engine: "ydb", TargetFeatures: []string{provider.PlanningLifecycleFeature}, PlanningId: coordinationplan.ServiceID, DatabasePath: request.DatabasePath, Identifiers: request.Identifiers, Capabilities: map[string]bool{"coordination_nodes": true, "transactional_ddl": false}, Changes: []*protocol.PlanningChange{{Subject: compared.Changes[0].Subject, Value: change}}}
	plan, err := client.PlanFeatures(t.Context(), planning)
	c.Assert(err, qt.IsNil)
	c.Assert(plan.Diagnostics, qt.HasLen, 0)
	withoutLifecycle := startProviderFor(c)
	unnegotiated, err := withoutLifecycle.PlanFeatures(t.Context(), planning)
	c.Assert(err, qt.IsNotNil)
	c.Assert(unnegotiated, qt.IsNil)
	rendered, err := client.Render(t.Context(), testplanning.RenderRequest(client.Description(), plan, planning.Capabilities))
	c.Assert(err, qt.IsNil)
	c.Assert(rendered.Units, qt.HasLen, 1)
	c.Assert(rendered.Units[0].GetNative().Kind, qt.Equals, schema.CoordinationOperation)
	c.Assert(rendered.Units[0].GetSql(), qt.Equals, "")
	// A forged canonical receipt is refused across the process boundary, even
	// when its JSON and transport structure are otherwise valid.
	request.Desired.Records[0].Value.CanonicalProjection = []byte(`{}`)
	refused, err := client.CompareModels(t.Context(), request)
	c.Assert(err, qt.IsNotNil)
	c.Assert(refused, qt.IsNil)
}

// A provider process counts what it compares for a host that lists the feature,
// and the same comparison for a host that does not carries no count, which that
// host cannot ask for.
func TestComparisonCountsOfflineSubprocess(t *testing.T) {
	c := testcheck.New(t)
	request, err := testcomparison.Request(&schema.CoordinationIntent{}, &Ydb_Coordination.DescribeNodeResult{Config: &Ydb_Coordination.Config{}, Self: &Ydb_Scheme.Entry{Owner: "owner"}})
	c.Assert(err, qt.IsNil)

	counting := startProviderFor(c, provider.ComparisonCountsFeature)
	request.TargetFeatures = []string{provider.ComparisonCountsFeature}
	compared, err := counting.CompareModels(t.Context(), request)
	c.Assert(err, qt.IsNil)
	c.Assert(compared.GetCounts(), qt.HasLen, 1)
	c.Assert(compared.GetCounts()[0].GetKind(), qt.Equals, schema.CoordinationChange)
	c.Assert(compared.GetCounts()[0].GetMatched(), qt.Equals, uint32(1))

	plain := startProviderFor(c)
	refused, err := plain.CompareModels(t.Context(), request)
	c.Assert(err, qt.ErrorIs, provider.ErrProtocol)
	c.Assert(refused, qt.IsNil)
	request.TargetFeatures = nil
	compared, err = plain.CompareModels(t.Context(), request)
	c.Assert(err, qt.IsNil)
	c.Assert(compared.GetCounts(), qt.HasLen, 0)
}
