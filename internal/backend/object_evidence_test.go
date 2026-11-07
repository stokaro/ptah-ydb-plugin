package backend_test

import (
	"testing"

	qt "github.com/frankban/quicktest"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/stokaro/ptah/provider-sdk/provider"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb_Coordination"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb_Scheme"
	"google.golang.org/protobuf/proto"

	"github.com/stokaro/ptah-ydb-plugin/internal/backend"
	"github.com/stokaro/ptah-ydb-plugin/internal/coordinationcompare"
	native "github.com/stokaro/ptah-ydb-plugin/internal/nativeprotocol"
	"github.com/stokaro/ptah-ydb-plugin/internal/streaming"
	"github.com/stokaro/ptah-ydb-plugin/internal/streamingcompare"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcomparison"
	"github.com/stokaro/ptah-ydb-plugin/schema"
)

// objectSubject names the object of kind at the path dir/name of the
// database database, "" for a declaration, which names none.
func objectSubject(kind, database, dir, name string) *protocol.ComparisonIdentity {
	return &protocol.ComparisonIdentity{Kind: kind, Database: testcomparison.Part(database), Catalog: &protocol.ComparisonPart{}, Schema: testcomparison.Part(dir),
		Parent: &protocol.ComparisonPart{}, Name: testcomparison.Part(name)}
}

// addObject normalizes value as state's representation does and adds it to
// state under the subject its owner names.
func addObject(c *qt.C, state *protocol.ComparisonState, value *protocol.Envelope) {
	c.Helper()
	normalized, err := backend.New().NormalizeValues(c.Context(), &protocol.NormalizeValuesRequest{Engine: "ydb", TargetFeatures: backend.Description().Engines[0].Features,
		Values: []*protocol.ValueInput{{Id: value.Owner.Schema + "/" + value.Owner.Name, Representation: state.Representation, Value: value}}})
	c.Assert(err, qt.IsNil)
	owner := value.Owner
	state.Records = append(state.Records, &protocol.ComparisonRecord{Kind: owner.Kind, Subject: objectSubject(owner.Kind, owner.Database, owner.Schema, owner.Name), Value: normalized.Values[0]})
}

// observedModel is the OBSERVED model of kind.
func observedModel(kind string) *protocol.ValueModel {
	for _, model := range schema.ObservedModels() {
		if model.Kind == kind {
			return model
		}
	}
	return nil
}

// objectComparison is a comparison of the objects of kind by service on a
// target holding capabilities, with nothing on either side and each side
// describing the kind completely.
func objectComparison(kind, service string, desired *protocol.ValueModel, capabilities map[string]bool) *protocol.CompareModelsRequest {
	return &protocol.CompareModelsRequest{Engine: "ydb", DatabasePath: "/local", TargetFeatures: backend.Description().Engines[0].Features, ComparisonId: service, Kinds: []string{kind},
		Identifiers: &protocol.ComparisonIdentifiers{Rules: proto.CloneOf(backend.Description().Engines[0].Identifiers)}, Capabilities: capabilities,
		Desired: testcomparison.State(desired), Current: testcomparison.State(observedModel(kind))}
}

// limitSubject adds what state knows of the object of kind at dir/name.
func limitSubject(state *protocol.ComparisonState, kind, database, dir, name string, knowledge protocol.ModelKnowledge_State) {
	state.Coverage.Subjects = append(state.Coverage.Subjects, &protocol.ComparisonSubjectCoverage{Kind: kind, Subject: objectSubject(kind, database, dir, name),
		Knowledge: testcomparison.Knowledge(knowledge)})
}

// coordinationComparison is a comparison of coordination nodes, empty on both
// sides.
func coordinationComparison() *protocol.CompareModelsRequest {
	return objectComparison(schema.CoordinationProperties, coordinationcompare.ServiceID, schema.CoordinationDesiredModel(), map[string]bool{"coordination_nodes": true})
}

// nodeDeclared declares the node at dir/name with intent.
func nodeDeclared(c *qt.C, dir, name string, intent schema.CoordinationIntent) *protocol.Envelope {
	c.Helper()
	value, err := schema.CaptureCoordinationIntent(&protocol.Reference{Engine: "ydb", Kind: schema.CoordinationProperties, Schema: dir, Name: name}, intent)
	c.Assert(err, qt.IsNil)
	return value
}

// nodeRead is the node at /local/dir/name as YDB describes it.
func nodeRead(c *qt.C, dir, name string, config *Ydb_Coordination.Config) *protocol.Envelope {
	c.Helper()
	value, err := schema.CaptureObject(&protocol.Reference{Engine: "ydb", Kind: schema.CoordinationProperties, Database: "/local", Schema: dir, Name: name},
		&Ydb_Coordination.DescribeNodeResult{Config: config, Self: &Ydb_Scheme.Entry{Owner: "root@builtin"}})
	c.Assert(err, qt.IsNil)
	return value
}

// nodeReadSide is the read side of a comparison that holds the node app/node as
// observed describes it, the whole kind read.
func nodeReadSide(observed *Ydb_Coordination.DescribeNodeResult) *protocol.ComparisonState {
	return must(testcomparison.Request(nil, observed)).Current
}

// nodeKnowledge adds what state knows of the node app/node.
func nodeKnowledge(state *protocol.ComparisonState, knowledge protocol.ModelKnowledge_State) {
	limitSubject(state, schema.CoordinationProperties, "/local", "app", "node", knowledge)
}

// Ported from the host's TestCoordinationComparisonKeepsServerOwnedLimits: a
// read that says it cannot describe a node holds no record of it, and a
// source that declares nothing has nothing to keep; the comparison names the
// node undecided and plans nothing. The host's row named a node the server
// owns, a path the plugin's identity rules refuse and its read never reports.
func TestCoordinationComparisonKeepsServerOwnedLimits(t *testing.T) {
	c := testcheck.New(t)
	request := coordinationComparison()
	limitSubject(request.Current, schema.CoordinationProperties, "/local", "", "hidden", protocol.ModelKnowledge_UNREPRESENTABLE)

	result, err := backend.New().CompareModels(c.Context(), request)

	c.Assert(err, qt.IsNil)
	c.Assert(provider.ValidateComparedModels(backend.Description(), request, result), qt.IsNil)
	c.Assert(result.Changes, qt.HasLen, 0)
	c.Assert(result.Undecided, qt.HasLen, 1)
	c.Assert(result.Undecided[0].Subject.Name.Normalized, qt.Equals, "hidden")
	c.Assert(result.Desired.Records, qt.HasLen, 0)
}

// Ported from the host's TestCoordinationComparisonKeepsCaseAndDirectoryIdentity:
// a node pairs with a node at the same path, spelled the same: a node whose
// name differs in case, one in another directory, and one whose name holds a
// literal dot are another node, so each pair is one creation and one removal.
func TestCoordinationComparisonKeepsCaseAndDirectoryIdentity(t *testing.T) {
	for _, test := range []struct{ name, declaredDir, declaredName, readDir, readName string }{
		{name: "case", declaredName: "Locks", readName: "locks"},
		{name: "directory", declaredDir: "a", declaredName: "locks", readDir: "b", readName: "locks"},
		{name: "literal dot", declaredName: "app.locks", readDir: "app", readName: "locks"},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			request := coordinationComparison()
			addObject(c, request.Desired, nodeDeclared(c, test.declaredDir, test.declaredName, schema.CoordinationIntent{}))
			addObject(c, request.Current, nodeRead(c, test.readDir, test.readName, &Ydb_Coordination.Config{SelfCheckPeriodMillis: 2500,
				ReadConsistencyMode: Ydb_Coordination.ConsistencyMode_CONSISTENCY_MODE_STRICT}))

			result, err := backend.New().CompareModels(c.Context(), request)

			c.Assert(err, qt.IsNil)
			c.Assert(provider.ValidateComparedModels(backend.Description(), request, result), qt.IsNil)
			c.Assert(result.Undecided, qt.HasLen, 0)
			c.Assert(changedPaths(result), qt.ContentEquals, []string{test.declaredDir + "/" + test.declaredName + " ADDITIVE", test.readDir + "/" + test.readName + " DESTRUCTIVE"})
		})
	}
}

// changedPaths names each change of result by the path dir/name of its
// subject and its impact.
func changedPaths(result *protocol.ComparedModels) []string {
	var changed []string
	for _, change := range result.Changes {
		changed = append(changed, change.Subject.Schema.GetNormalized()+"/"+change.Subject.Name.Normalized+" "+change.Value.ChangeSemantics.Impact.String())
	}
	return changed
}

// Ported from the host's
// TestCoordinationComparisonRefusesInvalidInputsWithoutPartialOutput: a node
// lives in a directory and never under a table, so a coordination identity
// with a table parent is refused with no result.
func TestCoordinationComparisonRefusesATableParent(t *testing.T) {
	c := testcheck.New(t)
	request, err := testcomparison.Request(&schema.CoordinationIntent{}, nil)
	c.Assert(err, qt.IsNil)
	request.Desired.Records[0].Subject.Parent = testcomparison.Part("table")

	result, err := backend.New().CompareModels(c.Context(), request)

	c.Assert(err, qt.IsNotNil)
	c.Assert(result, qt.IsNil)
}

// streamingComparison is a comparison of streaming queries, empty on both
// sides, on a target with streaming queries.
func streamingComparison() *protocol.CompareModelsRequest {
	return objectComparison(schema.StreamingQueryProperties, streamingcompare.ServiceID, schema.NativeDesiredModel(schema.StreamingQueryProperties),
		map[string]bool{streaming.Capability: true})
}

// declaredQuery declares the streaming query at dir/name with spec.
func declaredQuery(c *qt.C, dir, name string, spec streaming.Spec) *protocol.Envelope {
	c.Helper()
	value, err := schema.CaptureStreamingQueryIntent(&protocol.Reference{Engine: "ydb", Kind: schema.StreamingQueryProperties, Schema: dir, Name: name}, spec, false)
	c.Assert(err, qt.IsNil)
	return value
}

// readQuery is the streaming query at /local/dir/name as YDB describes it.
func readQuery(c *qt.C, dir, name string, spec streaming.Spec) *protocol.Envelope {
	c.Helper()
	value, err := schema.CaptureObject(&protocol.Reference{Engine: "ydb", Kind: schema.StreamingQueryProperties, Database: "/local", Schema: dir, Name: name},
		&native.StreamingQuerySettings{Text: spec.Text, Run: spec.Run, ResourcePool: spec.ResourcePool})
	c.Assert(err, qt.IsNil)
	return value
}

// Ported from the host's TestStreamingComparisonNeedsNoTableAndRespectsEvidence:
// a streaming query compares by its stored settings, YDB's defaults resolved,
// and each side's knowledge decides what the comparison may plan. A creation
// needs the read to establish absence, a removal needs the source to describe
// the query, and a subject either side cannot describe is undecided. Where
// the read could not establish the namespace and holds nothing, or the same
// query, the plugin raises no undecided of the namespace, unlike the host's
// engine (integration/comparison_live_test.go).
func TestStreamingComparisonRespectsEvidence(t *testing.T) {
	text := streaming.Spec{Text: "SELECT 1;"}
	stopped := streaming.Spec{Text: "SELECT 1;", Run: new(false)}
	defaults := streaming.Spec{Text: "SELECT 1;", Run: new(true), ResourcePool: "default"}
	for _, test := range []struct {
		name                               string
		declared, read                     *streaming.Spec
		desiredKnowledge, currentKnowledge protocol.ModelKnowledge_State
		desiredLimit, currentLimit         protocol.ModelKnowledge_State
		changes, undecided, records        int
	}{
		{name: "create", declared: &text, changes: 1, records: 1},
		{name: "remove", read: &stopped, changes: 1},
		{name: "reset to default", declared: &text, read: &stopped, changes: 1, records: 1},
		{name: "explicit defaults are equivalent", declared: &defaults, read: &text, records: 1},
		{name: "unknown source adopts raw state", read: &stopped, desiredKnowledge: protocol.ModelKnowledge_UNINSPECTED, records: 1},
		{name: "unknown observation withholds creation", declared: &text, currentKnowledge: protocol.ModelKnowledge_UNINSPECTED, undecided: 1, records: 1},
		{name: "unknown empty observation", currentKnowledge: protocol.ModelKnowledge_UNINSPECTED},
		{name: "positive observation survives partial enumeration", declared: &text, read: &text, currentKnowledge: protocol.ModelKnowledge_UNINSPECTED, records: 1},
		{name: "unknown current object withholds removal", read: &stopped, currentLimit: protocol.ModelKnowledge_UNREPRESENTABLE, undecided: 1},
		{name: "unknown current object without value", currentLimit: protocol.ModelKnowledge_UNREPRESENTABLE, undecided: 1},
		{name: "incomplete explicit declaration withholds alteration", declared: &text, read: &stopped, desiredLimit: protocol.ModelKnowledge_UNREPRESENTABLE, undecided: 1, records: 1},
		{name: "explicit absence removes despite incomplete source", read: &stopped, desiredKnowledge: protocol.ModelKnowledge_UNINSPECTED, desiredLimit: protocol.ModelKnowledge_ABSENT, changes: 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			request := streamingComparison()
			for _, side := range []struct {
				state            *protocol.ComparisonState
				knowledge, limit protocol.ModelKnowledge_State
				database         string
			}{{request.Desired, test.desiredKnowledge, test.desiredLimit, ""}, {request.Current, test.currentKnowledge, test.currentLimit, "/local"}} {
				side.state.Coverage.Kinds[0].Sources[0].Outside = testcomparison.Knowledge(orComplete(side.knowledge))
				for _, limit := range stated(side.limit) {
					limitSubject(side.state, schema.StreamingQueryProperties, side.database, "app", "copy", limit)
				}
			}
			for _, spec := range nonNil(test.declared) {
				addObject(c, request.Desired, declaredQuery(c, "app", "copy", spec))
			}
			for _, spec := range nonNil(test.read) {
				addObject(c, request.Current, readQuery(c, "app", "copy", spec))
			}

			result, err := backend.New().CompareModels(c.Context(), request)

			c.Assert(err, qt.IsNil)
			c.Assert(provider.ValidateComparedModels(backend.Description(), request, result), qt.IsNil)
			c.Assert(result.Changes, qt.HasLen, test.changes)
			c.Assert(result.Undecided, qt.HasLen, test.undecided)
			c.Assert(result.Desired.Records, qt.HasLen, test.records)
		})
	}
}

// orComplete is state, or COMPLETE where a row states none.
func orComplete(state protocol.ModelKnowledge_State) protocol.ModelKnowledge_State {
	if state == protocol.ModelKnowledge_STATE_UNSPECIFIED {
		return protocol.ModelKnowledge_COMPLETE
	}
	return state
}

// stated is state as a list: none where a row states none.
func stated(state protocol.ModelKnowledge_State) []protocol.ModelKnowledge_State {
	if state == protocol.ModelKnowledge_STATE_UNSPECIFIED {
		return nil
	}
	return []protocol.ModelKnowledge_State{state}
}

// nonNil is value as a list: none for nil, value's target for any other.
func nonNil[T any](value *T) []T {
	if value == nil {
		return nil
	}
	return []T{*value}
}

// Ported from the host's TestStreamingComparisonAdoptsRawStateFromAnUnknownSource:
// a query a source without a claim leaves out is kept as YDB holds it, its
// text and its run state included.
func TestStreamingComparisonAdoptsTheHeldQuery(t *testing.T) {
	c := testcheck.New(t)
	request := streamingComparison()
	request.Desired.Coverage.Kinds[0].Sources[0].Outside = testcomparison.Knowledge(protocol.ModelKnowledge_UNINSPECTED)
	addObject(c, request.Current, readQuery(c, "app", "copy", streaming.Spec{Text: "SELECT 1;", Run: new(false)}))

	result, err := backend.New().CompareModels(c.Context(), request)

	c.Assert(err, qt.IsNil)
	c.Assert(result.Changes, qt.HasLen, 0)
	c.Assert(result.Desired.Records, qt.HasLen, 1)
	kept, _, err := schema.DesiredStreamingQuery(result.Desired.Records[0].Value.Value)
	c.Assert(err, qt.IsNil)
	c.Assert(kept.Text, qt.Equals, "SELECT 1;")
	c.Assert(kept.Run, qt.DeepEquals, new(false))
}

// Ported from the host's TestStreamingComparisonKeepsServerOwnedLimits: a
// query the read cannot describe, held nowhere and declared nowhere, is
// undecided, and nothing is planned or kept for it.
func TestStreamingComparisonKeepsServerOwnedLimits(t *testing.T) {
	c := testcheck.New(t)
	request := streamingComparison()
	limitSubject(request.Current, schema.StreamingQueryProperties, "/local", "", ".hidden", protocol.ModelKnowledge_UNREPRESENTABLE)

	result, err := backend.New().CompareModels(c.Context(), request)

	c.Assert(err, qt.IsNil)
	c.Assert(result.Changes, qt.HasLen, 0)
	c.Assert(result.Undecided, qt.HasLen, 1)
	c.Assert(result.Undecided[0].Subject.Name.Normalized, qt.Equals, ".hidden")
	c.Assert(result.Desired.Records, qt.HasLen, 0)
}

// Ported from the host's TestStreamingComparisonKeepsCaseAndDirectoryIdentity:
// a query pairs with a query at the same path spelled the same, so a name
// that differs in case, another directory and a literal dot are each one
// creation and one removal.
func TestStreamingComparisonKeepsCaseAndDirectoryIdentity(t *testing.T) {
	for _, test := range []struct{ name, declaredDir, declaredName, readDir, readName string }{
		{name: "case", declaredName: "Copy", readName: "copy"},
		{name: "directory", declaredDir: "a", declaredName: "copy", readDir: "b", readName: "copy"},
		{name: "literal dot", declaredName: "app.copy", readDir: "app", readName: "copy"},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			request := streamingComparison()
			addObject(c, request.Desired, declaredQuery(c, test.declaredDir, test.declaredName, streaming.Spec{Text: "SELECT 1;"}))
			addObject(c, request.Current, readQuery(c, test.readDir, test.readName, streaming.Spec{Text: "SELECT 1;", Run: new(false)}))

			result, err := backend.New().CompareModels(c.Context(), request)

			c.Assert(err, qt.IsNil)
			c.Assert(result.Undecided, qt.HasLen, 0)
			c.Assert(changedPaths(result), qt.ContentEquals, []string{test.declaredDir + "/" + test.declaredName + " ADDITIVE", test.readDir + "/" + test.readName + " DESTRUCTIVE"})
		})
	}
}

// Ported from the host's
// TestStreamingComparisonRefusesInvalidInputsWithoutPartialOutput: a query
// lives in a directory, never under a table, and its identity names the
// directory and the leaf apart, so an identity with a table parent and one
// whose name holds the directory are refused with no result.
func TestStreamingComparisonRefusesAnInvalidIdentity(t *testing.T) {
	for _, test := range []struct {
		name               string
		dir, parent, query string
	}{
		{name: "a table parent", dir: "app", parent: "table", query: "copy"},
		{name: "an unsplit leaf", query: "app/copy"},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			request := streamingComparison()
			addObject(c, request.Desired, declaredQuery(c, "app", "copy", streaming.Spec{Text: "SELECT 1;"}))
			record := request.Desired.Records[0]
			record.Subject.Schema, record.Subject.Parent, record.Subject.Name = testcomparison.Part(test.dir), testcomparison.Part(test.parent), testcomparison.Part(test.query)
			for _, owner := range []*protocol.Reference{record.Value.Value.Owner, record.Value.NormalizedOwner} {
				owner.Schema, owner.Name = test.dir, test.query
			}

			result, err := backend.New().CompareModels(c.Context(), request)

			c.Assert(err, qt.IsNotNil)
			c.Assert(result, qt.IsNil)
		})
	}
}

// must returns value and panics on err; it builds fixtures whose failure is
// not what a test asserts.
func must[T any](value T, err error) T {
	if err != nil {
		panic(err)
	}
	return value
}
