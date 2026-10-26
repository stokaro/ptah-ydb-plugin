package backend_test

import (
	"context"
	"encoding/json"
	"testing"

	qt "github.com/frankban/quicktest"
	"github.com/stokaro/ptah-ydb-plugin/internal/backend"
	native "github.com/stokaro/ptah-ydb-plugin/internal/nativeprotocol"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcomparison"
	"github.com/stokaro/ptah-ydb-plugin/schema"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/stokaro/ptah/provider-sdk/provider"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb_Coordination"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb_Scheme"
	"google.golang.org/protobuf/proto"
)

func TestCoordinationComparisonLifecycle(t *testing.T) {
	for _, test := range []struct {
		name     string
		desired  *schema.CoordinationIntent
		observed *Ydb_Coordination.DescribeNodeResult
		changes  int
		impact   protocol.ChangeSemantics_Impact
		access   protocol.ChangeSemantics_Access
		removal  bool
	}{
		{name: "defaults", desired: &schema.CoordinationIntent{}, observed: coordinationObserved()},
		{name: "source provenance", desired: &schema.CoordinationIntent{StructName: "Authored"}, observed: coordinationObserved()},
		{name: "explicit defaults", desired: &schema.CoordinationIntent{ReadConsistencyMode: new("relaxed"), AttachConsistencyMode: new("strict"), SelfCheckPeriodMillis: new(uint32(1000))}, observed: coordinationObserved()},
		{name: "create", desired: &schema.CoordinationIntent{}, changes: 1, impact: protocol.ChangeSemantics_ADDITIVE, access: protocol.ChangeSemantics_ACCESS_UNKNOWN},
		{name: "drop", observed: coordinationObserved(), changes: 1, impact: protocol.ChangeSemantics_DESTRUCTIVE, access: protocol.ChangeSemantics_NARROWS, removal: true},
		{name: "alter", desired: &schema.CoordinationIntent{ReadConsistencyMode: new("strict")}, observed: coordinationObserved(), changes: 1, impact: protocol.ChangeSemantics_BEHAVIORAL, access: protocol.ChangeSemantics_UNCHANGED},
		{name: "reset to default", desired: &schema.CoordinationIntent{}, observed: &Ydb_Coordination.DescribeNodeResult{Config: &Ydb_Coordination.Config{ReadConsistencyMode: Ydb_Coordination.ConsistencyMode_CONSISTENCY_MODE_STRICT}, Self: &Ydb_Scheme.Entry{Owner: "original-owner"}}, changes: 1, impact: protocol.ChangeSemantics_BEHAVIORAL, access: protocol.ChangeSemantics_UNCHANGED},
		{name: "empty"},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := qt.New(t)
			request, err := testcomparison.Request(test.desired, test.observed)
			c.Assert(err, qt.IsNil)
			original := proto.CloneOf(request)
			result, err := backend.New().CompareModels(t.Context(), request)
			c.Assert(err, qt.IsNil)
			c.Assert(provider.ValidateComparedModels(backend.Description(), request, result), qt.IsNil)
			c.Assert(proto.Equal(request, original), qt.IsTrue)
			c.Assert(result.Undecided, qt.HasLen, 0)
			c.Assert(result.Changes, qt.HasLen, test.changes)
			for _, change := range result.Changes {
				c.Assert(change.Value.ChangeSemantics.Impact, qt.Equals, test.impact)
				c.Assert(change.Value.ChangeSemantics.Access, qt.Equals, test.access)
				c.Assert(change.Value.ChangeSemantics.GetRemovesState(), qt.Equals, test.removal)
				c.Assert(change.Value.ChangeSemantics.GetReplacesOwner(), qt.IsFalse)
				var operands map[string]json.RawMessage
				c.Assert(json.Unmarshal(change.Value.Value.Payload, &operands), qt.IsNil)
				if test.observed != nil {
					c.Assert(string(operands["before"]), qt.Contains, "original-owner")
				}
				normalized, err := backend.New().NormalizeValues(t.Context(), &protocol.NormalizeValuesRequest{Engine: "ydb", Values: []*protocol.ValueInput{{Id: change.Value.Id, Representation: protocol.ValueInput_CHANGE, Value: change.Value.Value}}})
				c.Assert(err, qt.IsNil)
				c.Assert(proto.Equal(normalized.Values[0], change.Value), qt.IsTrue)
			}
		})
	}
}

func coordinationObserved() *Ydb_Coordination.DescribeNodeResult {
	return &Ydb_Coordination.DescribeNodeResult{Config: &Ydb_Coordination.Config{}, Self: &Ydb_Scheme.Entry{Owner: "original-owner", Permissions: []*Ydb_Scheme.Permissions{{Subject: "reader", PermissionNames: []string{"ydb.generic.read"}}}}}
}

func TestCoordinationComparisonCoverage(t *testing.T) {
	for _, test := range []struct {
		name      string
		mutate    func(*protocol.CompareModelsRequest)
		changes   int
		undecided int
		preserved int
	}{
		{name: "complete", changes: 1},
		{name: "unknown outside", mutate: func(r *protocol.CompareModelsRequest) {
			r.Current.Coverage.Kinds[0].Sources[0].Outside = testcomparison.Knowledge(protocol.ModelKnowledge_UNINSPECTED)
		}, undecided: 1},
		{name: "exact scoped absence", mutate: func(r *protocol.CompareModelsRequest) { scopeCurrent(r, "app") }, changes: 1},
		{name: "different namespace", mutate: func(r *protocol.CompareModelsRequest) { scopeCurrent(r, "application") }, undecided: 1},
		{name: "different database", mutate: func(r *protocol.CompareModelsRequest) {
			scopeCurrent(r, "app")
			r.Current.Coverage.Kinds[0].Sources[0].Regions[0].Scope.Database = testcomparison.Part("/other")
		}, undecided: 1},
		{name: "explicit empty schema", mutate: func(r *protocol.CompareModelsRequest) { scopeCurrent(r, "") }, undecided: 1},
		{name: "overlapping unknown", mutate: func(r *protocol.CompareModelsRequest) {
			scopeCurrent(r, "app")
			source := r.Current.Coverage.Kinds[0].Sources[0]
			region := proto.CloneOf(source.Regions[0])
			region.Knowledge = testcomparison.Knowledge(protocol.ModelKnowledge_UNREPRESENTABLE)
			source.Regions = append(source.Regions, region)
		}, undecided: 1},
		{name: "independent unknown", mutate: func(r *protocol.CompareModelsRequest) {
			r.Current.Coverage.Kinds[0].Sources = append(r.Current.Coverage.Kinds[0].Sources, &protocol.ComparisonKnowledgeSource{Outside: testcomparison.Knowledge(protocol.ModelKnowledge_UNINSPECTED)})
		}, undecided: 1},
		{name: "subject absence overrides unknown", mutate: func(r *protocol.CompareModelsRequest) {
			r.Current.Coverage.Kinds[0].Sources[0].Outside = testcomparison.Knowledge(protocol.ModelKnowledge_UNINSPECTED)
			r.Current.Coverage.Subjects = []*protocol.ComparisonSubjectCoverage{{Kind: schema.CoordinationProperties, Subject: proto.CloneOf(r.Desired.Records[0].Subject), Knowledge: testcomparison.Knowledge(protocol.ModelKnowledge_ABSENT)}}
		}, changes: 1},
		{name: "subject unknown overrides complete", mutate: func(r *protocol.CompareModelsRequest) {
			r.Current.Coverage.Subjects = []*protocol.ComparisonSubjectCoverage{{Kind: schema.CoordinationProperties, Subject: proto.CloneOf(r.Desired.Records[0].Subject), Knowledge: testcomparison.Knowledge(protocol.ModelKnowledge_UNREPRESENTABLE)}}
		}, undecided: 1},
		{name: "wrong target region", mutate: func(r *protocol.CompareModelsRequest) {
			scopeCurrent(r, "app")
			r.Current.Coverage.Kinds[0].Sources[0].Regions[0].Scope.Targets = []string{"postgres"}
		}, undecided: 1},
		{name: "target alias", mutate: func(r *protocol.CompareModelsRequest) {
			scopeCurrent(r, "app")
			r.Current.Coverage.Kinds[0].Sources[0].Regions[0].Scope.Targets = []string{"YDBS"}
		}, changes: 1},
		{name: "path prefix cannot match schema", mutate: func(r *protocol.CompareModelsRequest) {
			scopeCurrent(r, "app")
			r.Current.Coverage.Kinds[0].Sources[0].Regions[0].Scope.PathPrefix = []*protocol.ComparisonPart{testcomparison.Part("app")}
		}, undecided: 1},
		{name: "wrong subject kind", mutate: func(r *protocol.CompareModelsRequest) {
			scopeCurrent(r, "app")
			r.Current.Coverage.Kinds[0].Sources[0].Regions[0].Scope.SubjectKinds = []string{"table"}
		}, undecided: 1},
		{name: "exact empty path", mutate: func(r *protocol.CompareModelsRequest) {
			scopeCurrent(r, "app")
			r.Current.Coverage.Kinds[0].Sources[0].Regions[0].Scope.PathExact = true
		}, changes: 1},
		{name: "incomplete desired preserves current", mutate: func(r *protocol.CompareModelsRequest) {
			observed, err := testcomparison.Request(nil, coordinationObserved())
			if err != nil {
				panic(err)
			}
			r.Desired.Records = nil
			r.Current = observed.Current
			r.Desired.Coverage.Kinds[0].Sources[0].Outside = testcomparison.Knowledge(protocol.ModelKnowledge_UNINSPECTED)
		}, preserved: 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := qt.New(t)
			request, err := testcomparison.Request(&schema.CoordinationIntent{}, nil)
			c.Assert(err, qt.IsNil)
			if test.mutate != nil {
				test.mutate(request)
			}
			original := proto.CloneOf(request)
			result, err := backend.New().CompareModels(t.Context(), request)
			c.Assert(err, qt.IsNil)
			c.Assert(result.Changes, qt.HasLen, test.changes)
			c.Assert(result.Undecided, qt.HasLen, test.undecided)
			c.Assert(len(result.Desired.Records)-len(request.Desired.Records), qt.Equals, test.preserved)
			c.Assert(proto.Equal(request, original), qt.IsTrue)
		})
	}
}

func scopeCurrent(r *protocol.CompareModelsRequest, namespace string) {
	source := r.Current.Coverage.Kinds[0].Sources[0]
	source.Outside = testcomparison.Knowledge(protocol.ModelKnowledge_UNINSPECTED)
	source.Regions = []*protocol.ComparisonRegion{{Scope: &protocol.ComparisonNamespace{Database: testcomparison.Part("/local"), Catalog: testcomparison.Part(""), Schema: testcomparison.Part(namespace)}, Knowledge: testcomparison.Knowledge(protocol.ModelKnowledge_COMPLETE)}}
}

func TestCoordinationComparisonRejectsUntrustedInputs(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*protocol.CompareModelsRequest)
	}{
		{"owner mismatch", func(r *protocol.CompareModelsRequest) {
			r.Desired.Records[0].Subject.Name = testcomparison.Part("other")
		}},
		{"forged dependency receipt", func(r *protocol.CompareModelsRequest) {
			r.Desired.Records[0].Value.RequiredObjects = []*protocol.ComparisonIdentity{proto.CloneOf(r.Desired.Records[0].Subject)}
		}},
		{"normalized owner mismatch", func(r *protocol.CompareModelsRequest) { r.Desired.Records[0].Value.NormalizedOwner.Name = "other" }},
		{"canonical projection mismatch", func(r *protocol.CompareModelsRequest) {
			r.Desired.Records[0].Value.CanonicalProjection = []byte(`{"read_consistency_mode":"strict"}`)
		}},
		{"folded subject", func(r *protocol.CompareModelsRequest) { r.Desired.Records[0].Subject.Name.Normalized = "NODE" }},
		{"absent present object", func(r *protocol.CompareModelsRequest) {
			r.Desired.Coverage.Subjects = []*protocol.ComparisonSubjectCoverage{{Kind: schema.CoordinationProperties, Subject: proto.CloneOf(r.Desired.Records[0].Subject), Knowledge: testcomparison.Knowledge(protocol.ModelKnowledge_ABSENT)}}
		}},
		{"missing capability", func(r *protocol.CompareModelsRequest) { r.Capabilities = nil }},
		{"wrong identifier rules", func(r *protocol.CompareModelsRequest) {
			r.Identifiers.Rules.TableNames = protocol.IdentifierRules_ASCII_CASE_INSENSITIVE
		}},
		{"declared relation collision", func(r *protocol.CompareModelsRequest) {
			subject := proto.CloneOf(r.Desired.Records[0].Subject)
			subject.Kind = "table"
			r.DeclaredRelations = []*protocol.ComparisonIdentity{subject}
		}},
		{"another database", func(r *protocol.CompareModelsRequest) {
			r.Desired.Records[0].Value.Value.Owner.Database = "/other"
			r.Desired.Records[0].Value.NormalizedOwner.Database = "/other"
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := qt.New(t)
			request, err := testcomparison.Request(&schema.CoordinationIntent{}, nil)
			c.Assert(err, qt.IsNil)
			test.mutate(request)
			result, err := backend.New().CompareModels(t.Context(), request)
			c.Assert(err, qt.IsNotNil)
			c.Assert(result, qt.IsNil)
		})
	}
}

func TestCoordinationComparisonPreservesPolicy(t *testing.T) {
	c := qt.New(t)
	request, err := testcomparison.Request(&schema.CoordinationIntent{}, nil)
	c.Assert(err, qt.IsNil)
	record := request.Desired.Records[0]
	record.Value.Value.Owner.Database = ""
	record.Value.NormalizedOwner.Database = ""
	record.Subject.Database.Defaulted = true
	result, err := backend.New().CompareModels(t.Context(), request)
	c.Assert(err, qt.IsNil)
	c.Assert(result.Changes, qt.HasLen, 1)
	c.Assert(result.Changes[0].Value.Value.Owner.Database, qt.Equals, "/local")
	request.Exclusions = []*protocol.ComparisonExclusion{nil, {}}
	result, err = backend.New().CompareModels(t.Context(), request)
	c.Assert(err, qt.IsNil)
	c.Assert(result.Changes, qt.HasLen, 1)
	request.Exclusions = []*protocol.ComparisonExclusion{{Kind: schema.CoordinationProperties, Name: "app/node"}}
	result, err = backend.New().CompareModels(t.Context(), request)
	c.Assert(err, qt.IsNil)
	c.Assert(result.Changes, qt.HasLen, 0)
	c.Assert(result.Undecided, qt.HasLen, 0)
	request.Exclusions[0].Name = "app/NODE"
	result, err = backend.New().CompareModels(t.Context(), request)
	c.Assert(err, qt.IsNil)
	c.Assert(result.Changes, qt.HasLen, 1)
	request, err = testcomparison.Request(nil, coordinationObserved())
	c.Assert(err, qt.IsNil)
	request.Required = []*protocol.ComparisonIdentity{proto.CloneOf(request.Current.Records[0].Subject)}
	result, err = backend.New().CompareModels(t.Context(), request)
	c.Assert(err, qt.IsNil)
	c.Assert(result.Changes, qt.HasLen, 0)
	c.Assert(result.Undecided, qt.HasLen, 1)
	request.Required = nil
	record = request.Current.Records[0]
	record.Subject.Schema = testcomparison.Part("")
	record.Subject.Name = testcomparison.Part("ptah_locks")
	record.Value.Value.Owner.Schema = ""
	record.Value.Value.Owner.Name = "ptah_locks"
	record.Value.NormalizedOwner = proto.CloneOf(record.Value.Value.Owner)
	result, err = backend.New().CompareModels(t.Context(), request)
	c.Assert(err, qt.IsNil)
	// The runtime lock node is left out: no drop, and nothing to report.
	c.Assert(result.Changes, qt.HasLen, 0)
	c.Assert(result.Undecided, qt.HasLen, 0)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	result, err = backend.New().CompareModels(ctx, request)
	c.Assert(err, qt.IsNotNil)
	c.Assert(result, qt.IsNil)
}

func TestCoordinationAccessRequirementsCannotBecomeIgnoredChanges(t *testing.T) {
	for _, test := range []struct {
		name               string
		observed           *Ydb_Coordination.DescribeNodeResult
		owner              string
		changes, undecided int
	}{
		{"satisfied", coordinationObserved(), "original-owner", 1, 0},
		{"owner mismatch", coordinationObserved(), "other", 0, 1},
		{"creation requirements", nil, "original-owner", 0, 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := qt.New(t)
			desired := &schema.CoordinationIntent{ReadConsistencyMode: new("strict"), Access: &native.AccessIntent{Owner: new(test.owner), DirectGrants: []*Ydb_Scheme.Permissions{{Subject: "reader", PermissionNames: []string{"ydb.generic.read"}}}}}
			request, err := testcomparison.Request(desired, test.observed)
			c.Assert(err, qt.IsNil)
			result, err := backend.New().CompareModels(t.Context(), request)
			c.Assert(err, qt.IsNil)
			c.Assert(provider.ValidateComparedModels(backend.Description(), request, result), qt.IsNil)
			c.Assert(result.Changes, qt.HasLen, test.changes)
			c.Assert(result.Undecided, qt.HasLen, test.undecided)
			if test.undecided > 0 {
				var before *protocol.Envelope
				if len(request.Current.Records) > 0 {
					before = request.Current.Records[0].Value.Value
				}
				after := request.Desired.Records[0].Value.Value
				change, err := schema.CaptureCoordinationChange("forged", after.Owner, before, after)
				c.Assert(change, qt.IsNil)
				c.Assert(err, qt.IsNotNil)
			}
		})
	}
}

// hostShaped names the declared node as the host does, without a database,
// and retains the explicit database path, while the read side keeps the
// database the inspection names in its owners and regions.
func hostShaped(r *protocol.CompareModelsRequest) {
	r.DatabasePath = "/local"
	for _, record := range r.Desired.Records {
		record.Subject.Database = &protocol.ComparisonPart{}
		record.Value.Value.Owner.Database = ""
		record.Value.NormalizedOwner.Database = ""
	}
}

// The host names a declared object without a database and a read one with the
// session's. Both name one node, so a node the read holds is altered rather
// than dropped and created again, and a node the scoped read established absent
// is created. A change names the node as the request did: the declared
// identity, or the read one for a removal, which has no declaration.
func TestCoordinationComparisonReadsHostIdentities_HappyPath(t *testing.T) {
	for _, test := range []struct {
		name     string
		desired  *schema.CoordinationIntent
		observed *Ydb_Coordination.DescribeNodeResult
		impact   protocol.ChangeSemantics_Impact
		database string
	}{
		{name: "alter", desired: &schema.CoordinationIntent{ReadConsistencyMode: new("strict")}, observed: coordinationObserved(), impact: protocol.ChangeSemantics_BEHAVIORAL},
		{name: "create", desired: &schema.CoordinationIntent{ReadConsistencyMode: new("strict")}, impact: protocol.ChangeSemantics_ADDITIVE},
		{name: "drop", observed: coordinationObserved(), impact: protocol.ChangeSemantics_DESTRUCTIVE, database: "/local"},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := qt.New(t)
			request, err := testcomparison.Request(test.desired, test.observed)
			c.Assert(err, qt.IsNil)
			scopeCurrent(request, "app")
			hostShaped(request)
			result, err := backend.New().CompareModels(t.Context(), request)
			c.Assert(err, qt.IsNil)
			c.Assert(provider.ValidateComparedModels(backend.Description(), request, result), qt.IsNil)
			c.Assert(result.Undecided, qt.HasLen, 0)
			c.Assert(result.Changes, qt.HasLen, 1)
			c.Assert(result.Changes[0].Value.ChangeSemantics.Impact, qt.Equals, test.impact)
			c.Assert(result.Changes[0].Subject.GetDatabase().GetNormalized(), qt.Equals, test.database)
			c.Assert(result.Changes[0].Value.Value.Owner.Database, qt.Equals, test.database)
		})
	}
}

// Conflicting physical context receipts are refused before identity pairing.
func TestCoordinationComparisonReadsHostIdentities_FailurePath(t *testing.T) {
	c := qt.New(t)
	request, err := testcomparison.Request(&schema.CoordinationIntent{}, coordinationObserved())
	c.Assert(err, qt.IsNil)
	scopeCurrent(request, "app")
	hostShaped(request)
	request.TargetFeatures = backend.Description().Engines[0].Features
	request.Current.Records[0].Value.Value.Context = &protocol.DatabaseContext{Database: "/other", SourceDatabase: "/other"}
	result, err := backend.New().CompareModels(t.Context(), request)
	c.Assert(err, qt.ErrorMatches, `.*conflicting database contexts`)
	c.Assert(result, qt.IsNil)
}
