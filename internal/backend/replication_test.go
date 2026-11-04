package backend_test

import (
	"testing"

	qt "github.com/frankban/quicktest"
	"github.com/stokaro/ptah-ydb-plugin/internal/backend"
	"github.com/stokaro/ptah-ydb-plugin/internal/objectplan"
	"github.com/stokaro/ptah-ydb-plugin/internal/replication"
	"github.com/stokaro/ptah-ydb-plugin/internal/replicationcompare"
	"github.com/stokaro/ptah-ydb-plugin/internal/replicationplan"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcomparison"
	"github.com/stokaro/ptah-ydb-plugin/internal/testplanning"
	"github.com/stokaro/ptah-ydb-plugin/schema"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/stokaro/ptah/provider-sdk/provider"
	"google.golang.org/protobuf/proto"
)

// replicationCapabilities are a 26.2 session's capabilities for the family.
func replicationCapabilities() map[string]bool {
	return map[string]bool{replication.CapabilityReplication: true, replication.CapabilityTransfers: true, replication.CapabilitySecretPaths: true, "transactional_ddl": false}
}

// mirror is the replication app/mirror declares: one table of the server's
// own database, read with a token secret.
func mirror() replication.ReplicationSpec {
	return replication.ReplicationSpec{Connection: replication.Connection{ConnectionString: "grpc://localhost:2136/?database=/local", TokenSecretName: "token"},
		Items: []replication.Item{{Source: "src/accounts", Target: "replica/accounts"}}}
}

// ingest is the transfer app/ingest declares.
func ingest() replication.TransferSpec {
	return replication.TransferSpec{Source: "orders/feed", Target: "order_log", Lambda: "($m) -> { return [<| id: $m._offset |>]; }"}
}

// objectRequest captures a comparison of one object at app/name of kind the
// way the host sends it: the declaration named without a database, the read
// object and the read directory with the session's. A nil side is absent.
func objectRequest(c *qt.C, kind, service, name string, declared, read *protocol.Envelope) *protocol.CompareModelsRequest {
	c.Helper()
	var observedModel *protocol.ValueModel
	for _, model := range schema.ObservedModels() {
		if model.Kind == kind {
			observedModel = model
		}
	}
	desiredState, currentState := testcomparison.State(schema.NativeDesiredModel(kind)), testcomparison.State(observedModel)
	request := &protocol.CompareModelsRequest{Engine: "ydb", DatabasePath: "/local", ComparisonId: service, Kinds: []string{kind},
		Identifiers: &protocol.ComparisonIdentifiers{Rules: proto.CloneOf(backend.Description().Engines[0].Identifiers)}, Capabilities: replicationCapabilities(), Desired: desiredState, Current: currentState}
	for _, side := range []struct {
		state          *protocol.ComparisonState
		value          *protocol.Envelope
		database       *protocol.ComparisonPart
		representation protocol.ValueInput_Representation
	}{{desiredState, declared, &protocol.ComparisonPart{}, protocol.ValueInput_DESIRED}, {currentState, read, testcomparison.Part("/local"), protocol.ValueInput_OBSERVED}} {
		if side.value == nil {
			continue
		}
		normalized, err := backend.New().NormalizeValues(c.Context(), &protocol.NormalizeValuesRequest{Engine: "ydb", Values: []*protocol.ValueInput{{Id: "value", Representation: side.representation, Value: side.value}}})
		c.Assert(err, qt.IsNil)
		side.state.Records = []*protocol.ComparisonRecord{{Kind: kind, Subject: &protocol.ComparisonIdentity{Kind: kind, Database: side.database, Catalog: &protocol.ComparisonPart{}, Schema: testcomparison.Part("app"), Parent: &protocol.ComparisonPart{}, Name: testcomparison.Part(name)}, Value: normalized.Values[0]}}
	}
	return request
}

// declaredReplication declares spec at app/mirror, or nothing for nil.
func declaredReplication(c *qt.C, spec *replication.ReplicationSpec) *protocol.Envelope {
	c.Helper()
	if spec == nil {
		return nil
	}
	value, err := schema.CaptureReplicationIntent(&protocol.Reference{Engine: "ydb", Kind: schema.ReplicationProperties, Schema: "app", Name: "mirror"}, *spec)
	c.Assert(err, qt.IsNil)
	return value
}

// readReplication reads spec in state at /local/app/mirror, or nothing for
// nil.
func readReplication(c *qt.C, spec *replication.ReplicationSpec, state string) *protocol.Envelope {
	c.Helper()
	if spec == nil {
		return nil
	}
	value, err := schema.CaptureObject(&protocol.Reference{Engine: "ydb", Kind: schema.ReplicationProperties, Database: "/local", Schema: "app", Name: "mirror"}, replication.ObservedReplication(*spec, state))
	c.Assert(err, qt.IsNil)
	return value
}

// declaredTransfer declares spec at app/ingest, or nothing for nil.
func declaredTransfer(c *qt.C, spec *replication.TransferSpec) *protocol.Envelope {
	c.Helper()
	if spec == nil {
		return nil
	}
	value, err := schema.CaptureTransferIntent(&protocol.Reference{Engine: "ydb", Kind: schema.TransferProperties, Schema: "app", Name: "ingest"}, *spec)
	c.Assert(err, qt.IsNil)
	return value
}

// readTransfer reads spec in state at /local/app/ingest, or nothing for nil.
func readTransfer(c *qt.C, spec *replication.TransferSpec, state string) *protocol.Envelope {
	c.Helper()
	if spec == nil {
		return nil
	}
	value, err := schema.CaptureObject(&protocol.Reference{Engine: "ydb", Kind: schema.TransferProperties, Database: "/local", Schema: "app", Name: "ingest"}, replication.ObservedTransfer(*spec, state))
	c.Assert(err, qt.IsNil)
	return value
}

// plan plans the one change of a comparison.
func plan(c *qt.C, service string, change *protocol.ComparedChange) *protocol.PlannedFeatures {
	c.Helper()
	planned, err := backend.New().PlanFeatures(c.Context(), &protocol.PlanFeaturesRequest{Engine: "ydb", TargetFeatures: []string{provider.PlanningLifecycleFeature}, PlanningId: service, DatabasePath: "/local",
		Identifiers: &protocol.ComparisonIdentifiers{Rules: proto.CloneOf(backend.Description().Engines[0].Identifiers)}, Capabilities: replicationCapabilities(),
		Changes: []*protocol.PlanningChange{{Subject: change.Subject, Value: change.Value}}})
	c.Assert(err, qt.IsNil)
	return planned
}

// planText plans the one change of a comparison and renders it as the YQL a
// migration file holds.
func planText(c *qt.C, service string, change *protocol.ComparedChange) string {
	c.Helper()
	planned := plan(c, service, change)
	c.Assert(planned.Diagnostics, qt.HasLen, 0)
	request := testplanning.RenderRequest(backend.Description(), planned, replicationCapabilities())
	request.Form = protocol.RenderRequest_TEXT
	rendered, err := backend.New().Render(c.Context(), request)
	c.Assert(err, qt.IsNil)
	c.Assert(rendered.Units, qt.HasLen, 1)
	return rendered.Units[0].GetSql()
}

// TestReplicationComparison_HappyPath compares and plans a replication as
// the host's embedded engine does: a creation writes its items and
// connection, a change of a paused replication's credential is an ALTER, and
// a drop takes the replica tables along unless the replication was failed
// over.
func TestReplicationComparison_HappyPath(t *testing.T) {
	byPassword := mirror()
	byPassword.Connection = replication.Connection{ConnectionString: byPassword.Connection.ConnectionString, User: "replicator", PasswordSecretName: "password"}
	for _, test := range []struct {
		name     string
		declared *replication.ReplicationSpec
		read     *replication.ReplicationSpec
		state    string
		impact   protocol.ChangeSemantics_Impact
		text     string
	}{
		{name: "a creation", declared: new(mirror()), impact: protocol.ChangeSemantics_ADDITIVE,
			text: "CREATE ASYNC REPLICATION `app/mirror` FOR `src/accounts` AS `replica/accounts` WITH (CONNECTION_STRING = 'grpc://localhost:2136/?database=/local', TOKEN_SECRET_NAME = 'token');"},
		{name: "a paused replication's credential", declared: &byPassword, read: new(mirror()), state: replication.StatePaused, impact: protocol.ChangeSemantics_BEHAVIORAL,
			text: "ALTER ASYNC REPLICATION `app/mirror` SET (USER = 'replicator', PASSWORD_SECRET_NAME = 'password');"},
		{name: "a running replication dropped", read: new(mirror()), state: replication.StateRunning, impact: protocol.ChangeSemantics_DESTRUCTIVE,
			text: "DROP ASYNC REPLICATION `app/mirror` CASCADE;"},
		{name: "a failed-over replication dropped", read: new(mirror()), state: replication.StateDone, impact: protocol.ChangeSemantics_BEHAVIORAL,
			text: "DROP ASYNC REPLICATION `app/mirror`;"},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			request := objectRequest(c, schema.ReplicationProperties, replicationcompare.ReplicationServiceID, "mirror", declaredReplication(c, test.declared), readReplication(c, test.read, test.state))
			result, err := backend.New().CompareModels(t.Context(), request)
			c.Assert(err, qt.IsNil)
			c.Assert(result.Undecided, qt.HasLen, 0)
			c.Assert(result.Changes, qt.HasLen, 1)
			c.Assert(result.Changes[0].Value.ChangeSemantics.Impact, qt.Equals, test.impact)
			c.Assert(planText(c, replicationplan.ReplicationServiceID, result.Changes[0]), qt.Equals, test.text)
		})
	}
}

// TestReplicationComparisonResolvesDefaults finds no change between a
// declaration naming YDB's default and a replication that holds it.
func TestReplicationComparisonResolvesDefaults(t *testing.T) {
	c := testcheck.New(t)
	rowLevel := mirror()
	rowLevel.ConsistencyLevel = "row"
	request := objectRequest(c, schema.ReplicationProperties, replicationcompare.ReplicationServiceID, "mirror", declaredReplication(c, &rowLevel), readReplication(c, new(mirror()), replication.StateRunning))
	result, err := backend.New().CompareModels(t.Context(), request)
	c.Assert(err, qt.IsNil)
	c.Assert(result.Undecided, qt.HasLen, 0)
	c.Assert(result.Changes, qt.HasLen, 0)
}

// TestReplicationPlanning_FailurePath refuses what the host's embedded
// engine refuses: a change YDB makes in no replication in place, and a
// connection change of a replication that is not paused.
func TestReplicationPlanning_FailurePath(t *testing.T) {
	moved := mirror()
	moved.Items = []replication.Item{{Source: "src/accounts", Target: "replica/other"}}
	byPassword := mirror()
	byPassword.Connection = replication.Connection{ConnectionString: byPassword.Connection.ConnectionString, User: "replicator", PasswordSecretName: "password"}
	for _, test := range []struct {
		name     string
		declared replication.ReplicationSpec
		state    string
		wantErr  string
	}{
		{name: "items moved", declared: moved, state: replication.StatePaused,
			wantErr: `async replication app/mirror: its items differ from the database's, and YDB changes none of them in place .*`},
		{name: "a running replication's credential", declared: byPassword, state: replication.StateRunning,
			wantErr: `async replication app/mirror: its connection or credential differs, and YDB changes them only while the replication is paused .*`},
		{name: "a failed-over replication's credential", declared: byPassword, state: replication.StateDone,
			wantErr: `async replication app/mirror: it was failed over .*`},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			request := objectRequest(c, schema.ReplicationProperties, replicationcompare.ReplicationServiceID, "mirror", declaredReplication(c, &test.declared), readReplication(c, new(mirror()), test.state))
			result, err := backend.New().CompareModels(t.Context(), request)
			c.Assert(err, qt.IsNil)
			c.Assert(result.Changes, qt.HasLen, 1)
			planned := plan(c, replicationplan.ReplicationServiceID, result.Changes[0])
			c.Assert(planned.Contributions, qt.HasLen, 0)
			c.Assert(planned.Diagnostics, qt.HasLen, 1)
			c.Assert(planned.Diagnostics[0].Message, qt.Matches, test.wantErr)
		})
	}
}

// TestReplicationComparisonLeavesAnUnkeepableOneUndecided keeps no
// declaration for a replication YDB reports with no item, one that failed
// before it resolved its tables.
func TestReplicationComparisonLeavesAnUnkeepableOneUndecided(t *testing.T) {
	c := testcheck.New(t)
	failed := mirror()
	failed.Items = nil
	request := objectRequest(c, schema.ReplicationProperties, replicationcompare.ReplicationServiceID, "mirror", nil, readReplication(c, &failed, replication.StateError))
	request.Desired.Coverage.Kinds[0].Sources[0].Outside = testcomparison.Knowledge(protocol.ModelKnowledge_UNINSPECTED)
	result, err := backend.New().CompareModels(t.Context(), request)
	c.Assert(err, qt.IsNil)
	c.Assert(result.Changes, qt.HasLen, 0)
	c.Assert(result.Undecided, qt.HasLen, 1)
	c.Assert(result.Undecided[0].Reason, qt.Matches, `The async replication \(error\) has no declaration that keeps it: it replicates no table; .*`)
}

// TestTransferComparison_HappyPath compares and plans a transfer: a lambda
// changes in place, with both batch settings, and a drop removes the
// consumer YDB created.
func TestTransferComparison_HappyPath(t *testing.T) {
	held := ingest()
	held.Consumer = "6f0b8c"
	relambda := ingest()
	relambda.Lambda = "($m) -> { return []; }"
	relambda.FlushInterval = "PT10S"
	for _, test := range []struct {
		name     string
		declared *replication.TransferSpec
		read     *replication.TransferSpec
		impact   protocol.ChangeSemantics_Impact
		text     string
	}{
		{name: "a creation", declared: new(ingest()), impact: protocol.ChangeSemantics_ADDITIVE,
			text: "CREATE TRANSFER `app/ingest` FROM `orders/feed` TO `order_log` USING ($m) -> { return [<| id: $m._offset |>]; };"},
		{name: "a lambda and a flush interval", declared: &relambda, read: &held, impact: protocol.ChangeSemantics_BEHAVIORAL,
			text: "ALTER TRANSFER `app/ingest` SET USING ($m) -> { return []; }, SET (BATCH_SIZE_BYTES = 8388608, FLUSH_INTERVAL = Interval('PT10S'));"},
		{name: "a drop", read: &held, impact: protocol.ChangeSemantics_DESTRUCTIVE, text: "DROP TRANSFER `app/ingest`;"},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			request := objectRequest(c, schema.TransferProperties, replicationcompare.TransferServiceID, "ingest", declaredTransfer(c, test.declared), readTransfer(c, test.read, replication.StateRunning))
			result, err := backend.New().CompareModels(t.Context(), request)
			c.Assert(err, qt.IsNil)
			c.Assert(result.Undecided, qt.HasLen, 0)
			c.Assert(result.Changes, qt.HasLen, 1)
			c.Assert(result.Changes[0].Value.ChangeSemantics.Impact, qt.Equals, test.impact)
			c.Assert(planText(c, replicationplan.TransferServiceID, result.Changes[0]), qt.Equals, test.text)
		})
	}
}

// TestTransferComparisonTakesTheConsumerYDBNamed finds no change between a
// declaration naming no consumer and a transfer that reads through the one
// YDB created for it.
func TestTransferComparisonTakesTheConsumerYDBNamed(t *testing.T) {
	c := testcheck.New(t)
	held := ingest()
	held.Consumer = "6f0b8c"
	request := objectRequest(c, schema.TransferProperties, replicationcompare.TransferServiceID, "ingest", declaredTransfer(c, new(ingest())), readTransfer(c, &held, replication.StateRunning))
	result, err := backend.New().CompareModels(t.Context(), request)
	c.Assert(err, qt.IsNil)
	c.Assert(result.Undecided, qt.HasLen, 0)
	c.Assert(result.Changes, qt.HasLen, 0)
}

// TestTransferPlanning_FailurePath refuses a consumer change, which YDB
// makes in no transfer in place, and a target without transfers.
func TestTransferPlanning_FailurePath(t *testing.T) {
	c := testcheck.New(t)
	held := ingest()
	held.Consumer = "a"
	other := ingest()
	other.Consumer = "b"
	request := objectRequest(c, schema.TransferProperties, replicationcompare.TransferServiceID, "ingest", declaredTransfer(c, &other), readTransfer(c, &held, replication.StateRunning))
	result, err := backend.New().CompareModels(t.Context(), request)
	c.Assert(err, qt.IsNil)
	c.Assert(result.Changes, qt.HasLen, 1)
	planned := plan(c, replicationplan.TransferServiceID, result.Changes[0])
	c.Assert(planned.Diagnostics, qt.HasLen, 1)
	c.Assert(planned.Diagnostics[0].Message, qt.Matches, `transfer app/ingest: its consumer differ from the database's, .*`)

	request = objectRequest(c, schema.TransferProperties, replicationcompare.TransferServiceID, "ingest", declaredTransfer(c, new(ingest())), nil)
	request.Capabilities[replication.CapabilityTransfers] = false
	result, err = backend.New().CompareModels(t.Context(), request)
	c.Assert(err, qt.ErrorMatches, `.*changing transfers requires transfers`)
	c.Assert(result, qt.IsNil)
}

// declaredReplica declares the table at replica/accounts, with no native
// facet.
func declaredReplica() *protocol.DeclaredTable {
	return &protocol.DeclaredTable{Schema: "replica", Name: "accounts", Features: &protocol.CapturedFeatures{
		Objects: &protocol.ComparisonState{Representation: protocol.ValueInput_DESIRED, Coverage: &protocol.ComparisonCoverage{}},
		Facets:  &protocol.ComparisonState{Representation: protocol.ValueInput_DESIRED, Coverage: &protocol.ComparisonCoverage{}}}}
}

// replicaTable is a table subject at replica/accounts, the target mirror
// replicates to, named as the host names a table of the session's database.
func replicaTable() *protocol.ComparisonIdentity {
	return &protocol.ComparisonIdentity{Kind: "table", Database: &protocol.ComparisonPart{}, Catalog: &protocol.ComparisonPart{}, Schema: testcomparison.Part("replica"), Parent: &protocol.ComparisonPart{}, Name: testcomparison.Part("accounts")}
}

// planWith plans the one change of a comparison beside tables and common
// steps.
func planWith(c *qt.C, change *protocol.ComparedChange, tables []*protocol.PlanningTable, common []*protocol.CommonPlanningStep) *protocol.PlannedFeatures {
	c.Helper()
	planned, err := backend.New().PlanFeatures(c.Context(), &protocol.PlanFeaturesRequest{Engine: "ydb", TargetFeatures: []string{provider.PlanningLifecycleFeature}, PlanningId: replicationplan.ReplicationServiceID, DatabasePath: "/local",
		Identifiers: &protocol.ComparisonIdentifiers{Rules: proto.CloneOf(backend.Description().Engines[0].Identifiers)}, Capabilities: replicationCapabilities(),
		Changes: []*protocol.PlanningChange{{Subject: change.Subject, Value: change.Value}}, Tables: tables, CommonSteps: common})
	c.Assert(err, qt.IsNil)
	return planned
}

// TestReplicationPlanningOrdersReplicaPaths names the replica paths a step
// acts on and orders it against a common step at one: a drop with CASCADE
// comes before an object created where its replica stood.
func TestReplicationPlanningOrdersReplicaPaths(t *testing.T) {
	c := testcheck.New(t)
	request := objectRequest(c, schema.ReplicationProperties, replicationcompare.ReplicationServiceID, "mirror", nil, readReplication(c, new(mirror()), replication.StateRunning))
	result, err := backend.New().CompareModels(t.Context(), request)
	c.Assert(err, qt.IsNil)
	c.Assert(result.Changes, qt.HasLen, 1)
	creation := &protocol.CommonPlanningStep{Id: &protocol.StepIdentity{Owner: "host", Name: "create"},
		Metadata: &protocol.PlanningMetadata{Transaction: protocol.PlanningMetadata_FORBIDDEN, Impact: protocol.ChangeSemantics_ADDITIVE, Reason: "Creates an object",
			Effects: []*protocol.PlanningEffect{{Subject: objectplan.PathSubject("replica", "accounts"), Action: protocol.PlanningEffect_CREATE}}}}
	planned := planWith(c, result.Changes[0], nil, []*protocol.CommonPlanningStep{creation})
	c.Assert(planned.Diagnostics, qt.HasLen, 0)
	c.Assert(planned.Contributions, qt.HasLen, 1)
	step := planned.Contributions[0].Steps[0]
	c.Assert(step.Metadata.Effects[len(step.Metadata.Effects)-1].Action, qt.Equals, protocol.PlanningEffect_DROP)
	c.Assert(step.Metadata.Effects[len(step.Metadata.Effects)-1].Subject.Name.Normalized, qt.Equals, "accounts")
	c.Assert(planned.Contributions[0].Dependencies, qt.HasLen, 1)
	c.Assert(proto.Equal(planned.Contributions[0].Dependencies[0], &protocol.PlanningDependency{Before: step.Id, After: creation.Id}), qt.IsTrue)
}

// TestReplicationPlanningRefusesReplicaTables refuses a plan its tables would
// break: a drop with CASCADE of a replication whose replica the schema
// declares, and a creation where a table stands at a target.
func TestReplicationPlanningRefusesReplicaTables(t *testing.T) {
	for _, test := range []struct {
		name     string
		declared *replication.ReplicationSpec
		read     *replication.ReplicationSpec
		table    *protocol.PlanningTable
		wantErr  string
	}{
		{name: "a dropped replication's replica declared", read: new(mirror()),
			table:   &protocol.PlanningTable{Subject: replicaTable(), Action: protocol.PlanningTable_CREATE, Desired: declaredReplica()},
			wantErr: `async replication app/mirror: the schema drops it and declares table replica/accounts, its replica, .*`},
		{name: "a table at a created replication's target", declared: new(mirror()),
			table:   &protocol.PlanningTable{Subject: replicaTable(), Action: protocol.PlanningTable_CONTEXT, Desired: declaredReplica()},
			wantErr: `async replication app/mirror: its target replica/accounts holds table replica/accounts, and YDB creates a replica only at a free path .*`},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			request := objectRequest(c, schema.ReplicationProperties, replicationcompare.ReplicationServiceID, "mirror", declaredReplication(c, test.declared), readReplication(c, test.read, replication.StateRunning))
			result, err := backend.New().CompareModels(t.Context(), request)
			c.Assert(err, qt.IsNil)
			c.Assert(result.Changes, qt.HasLen, 1)
			planned := planWith(c, result.Changes[0], []*protocol.PlanningTable{test.table}, nil)
			c.Assert(planned.Contributions, qt.HasLen, 0)
			c.Assert(planned.Diagnostics, qt.HasLen, 1)
			c.Assert(planned.Diagnostics[0].Message, qt.Matches, test.wantErr)
		})
	}
}

// TestReplicationConversion_HappyPath converts a declaration to the
// observation YDB reports for it, and an observation to the declaration
// that keeps it, as the host asks before it compares.
func TestReplicationConversion_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	ingestHeld := ingest()
	ingestHeld.Consumer = "6f0b8c"
	for _, test := range []struct {
		name     string
		value    *protocol.Envelope
		from, to protocol.ValueInput_Representation
	}{
		{name: "a declared replication", value: declaredReplication(c, new(mirror())), from: protocol.ValueInput_DESIRED, to: protocol.ValueInput_OBSERVED},
		{name: "a read replication", value: readReplication(c, new(mirror()), replication.StatePaused), from: protocol.ValueInput_OBSERVED, to: protocol.ValueInput_DESIRED},
		{name: "a declared transfer", value: declaredTransfer(c, new(ingest())), from: protocol.ValueInput_DESIRED, to: protocol.ValueInput_OBSERVED},
		{name: "a read transfer", value: readTransfer(c, &ingestHeld, replication.StateRunning), from: protocol.ValueInput_OBSERVED, to: protocol.ValueInput_DESIRED},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			service := backend.New()
			normalized, err := service.NormalizeValues(t.Context(), &protocol.NormalizeValuesRequest{Engine: "ydb", Values: []*protocol.ValueInput{{Id: "v", Representation: test.from, Value: test.value}}})
			c.Assert(err, qt.IsNil)
			converted, err := service.ConvertValues(t.Context(), &protocol.ConvertValuesRequest{Engine: "ydb", From: test.from, To: test.to, Values: normalized.Values})
			c.Assert(err, qt.IsNil)
			c.Assert(converted.Values, qt.HasLen, 1)
			c.Assert(converted.Values[0].Value.Kind, qt.Equals, test.value.Kind)
		})
	}
}

// replicationContextState is a context state holding one replication, or
// none for a nil value.
func replicationContextState(c *qt.C, representation protocol.ValueInput_Representation, value *protocol.Envelope) *protocol.ComparisonState {
	c.Helper()
	var model *protocol.ValueModel
	for _, candidate := range schema.Models() {
		if candidate.Kind == schema.ReplicationProperties && candidate.Representation == representation {
			model = candidate
		}
	}
	state := testcomparison.State(model)
	if value != nil {
		normalized, err := backend.New().NormalizeValues(c.Context(), &protocol.NormalizeValuesRequest{Engine: "ydb", TargetFeatures: backend.Description().Features, Values: []*protocol.ValueInput{{Id: "mirror", Representation: representation, Value: value}}})
		c.Assert(err, qt.IsNil)
		state.Records = []*protocol.ComparisonRecord{{Kind: value.Kind, Subject: testplanning.Subject(value.Owner), Value: normalized.Values[0]}}
	}
	return state
}

// keptReplicationRequest plans no replication change beside table, with the
// replication mirror held in state and declared as it is held. The read
// replication names database, the session's as the host sends it, or none.
func keptReplicationRequest(c *qt.C, state, database string, table *protocol.PlanningTable) *protocol.PlanFeaturesRequest {
	c.Helper()
	read := readReplication(c, new(mirror()), state)
	read.Owner.Database = database
	return &protocol.PlanFeaturesRequest{Engine: "ydb", PlanningId: replicationplan.ReplicationServiceID, DatabasePath: "/local", ContextKinds: []string{schema.ReplicationProperties},
		TargetFeatures: backend.Description().Features, Capabilities: replicationCapabilities(), Identifiers: &protocol.ComparisonIdentifiers{Rules: proto.CloneOf(backend.Description().Engines[0].Identifiers)},
		DesiredObjects: replicationContextState(c, protocol.ValueInput_DESIRED, declaredReplication(c, new(mirror()))),
		CurrentObjects: replicationContextState(c, protocol.ValueInput_OBSERVED, read), Tables: []*protocol.PlanningTable{table}}
}

// TestKeptReplicationAssessment_HappyPath acknowledges a replication the plan
// leaves alone with a receipt and no step, beside a table elsewhere.
func TestKeptReplicationAssessment_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	elsewhere := declaredReplica()
	elsewhere.Schema = "other"
	subject := replicaTable()
	subject.Schema = testcomparison.Part("other")
	result, err := backend.New().PlanFeatures(t.Context(), keptReplicationRequest(c, replication.StateRunning, "", &protocol.PlanningTable{Subject: subject, Action: protocol.PlanningTable_CREATE, Desired: elsewhere}))
	c.Assert(err, qt.IsNil)
	c.Assert(result.Diagnostics, qt.HasLen, 0)
	c.Assert(result.Objects, qt.HasLen, 1)
	c.Assert(result.Objects[0].Steps, qt.HasLen, 0)
}

// TestKeptReplicationAssessment_FailurePath refuses a table the plan creates
// at the path of a running replication's replica, and a failed-over replica
// the plan drops while the schema keeps the replication.
func TestKeptReplicationAssessment_FailurePath(t *testing.T) {
	dropped := &protocol.PlanningTable{Subject: replicaTable(), Action: protocol.PlanningTable_DROP, Current: &protocol.ObservedTable{Schema: "replica", Name: "accounts", Features: &protocol.CapturedFeatures{
		Objects: &protocol.ComparisonState{Representation: protocol.ValueInput_OBSERVED, Coverage: &protocol.ComparisonCoverage{}},
		Facets:  &protocol.ComparisonState{Representation: protocol.ValueInput_OBSERVED, Coverage: &protocol.ComparisonCoverage{}}}}}
	tests := []struct {
		name     string
		state    string
		database string
		table    *protocol.PlanningTable
		wantErr  string
	}{
		{name: "a table at a live replica", state: replication.StateRunning,
			table:   &protocol.PlanningTable{Subject: replicaTable(), Action: protocol.PlanningTable_CREATE, Desired: declaredReplica()},
			wantErr: "table replica/accounts: it is a replica table async replication app/mirror writes, read-only while the replication runs .*"},
		{name: "a failed-over replica dropped", state: replication.StateDone, table: dropped,
			wantErr: "table replica/accounts: it is a table async replication app/mirror created and failed over, which the schema keeps; .*"},
		// The host names the declared replication without its database
		// and the read one with the session's.
		{name: "a failed-over replica dropped, read in the session's database", state: replication.StateDone, database: "/local", table: dropped,
			wantErr: "table replica/accounts: it is a table async replication app/mirror created and failed over, which the schema keeps; .*"},
		{name: "a table at a live replica, read in the session's database", state: replication.StateRunning, database: "/local",
			table:   &protocol.PlanningTable{Subject: replicaTable(), Action: protocol.PlanningTable_CREATE, Desired: declaredReplica()},
			wantErr: "table replica/accounts: it is a replica table async replication app/mirror writes, read-only while the replication runs .*"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			result, err := backend.New().PlanFeatures(t.Context(), keptReplicationRequest(c, test.state, test.database, test.table))
			c.Assert(err, qt.IsNil)
			c.Assert(result.Objects, qt.HasLen, 0)
			c.Assert(result.Diagnostics, qt.HasLen, 1)
			c.Assert(result.Diagnostics[0].Message, qt.Matches, test.wantErr)
		})
	}
}
