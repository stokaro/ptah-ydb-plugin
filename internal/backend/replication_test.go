package backend_test

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	qt "github.com/frankban/quicktest"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb_Table"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb_Topic"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/testing/protocmp"
	protocol "ptah.run/provider-sdk/protocol/experimental"
	"ptah.run/provider-sdk/provider"

	"github.com/stokaro/ptah-ydb-plugin/internal/backend"
	native "github.com/stokaro/ptah-ydb-plugin/internal/nativeprotocol"
	"github.com/stokaro/ptah-ydb-plugin/internal/objectplan"
	"github.com/stokaro/ptah-ydb-plugin/internal/replication"
	"github.com/stokaro/ptah-ydb-plugin/internal/replicationcompare"
	"github.com/stokaro/ptah-ydb-plugin/internal/replicationplan"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcomparison"
	"github.com/stokaro/ptah-ydb-plugin/internal/testplanning"
	"github.com/stokaro/ptah-ydb-plugin/schema"
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
// over. A replication whose state the read did not establish may still copy,
// so its drop takes the tables along too. Each change says what it costs in
// the host's words. Ported from the host's TestReplicationChange_Effect.
func TestReplicationComparison_HappyPath(t *testing.T) {
	byPassword := mirror()
	byPassword.Connection = replication.Connection{ConnectionString: byPassword.Connection.ConnectionString, User: "replicator", PasswordSecretName: "password"}
	for _, test := range []struct {
		name     string
		declared *replication.ReplicationSpec
		read     *replication.ReplicationSpec
		state    string
		impact   protocol.ChangeSemantics_Impact
		reason   string
		text     string
	}{
		{name: "a creation", declared: new(mirror()), impact: protocol.ChangeSemantics_ADDITIVE,
			reason: "CREATE ASYNC REPLICATION adds a replication and its replica tables",
			text:   "CREATE ASYNC REPLICATION `app/mirror` FOR `src/accounts` AS `replica/accounts` WITH (CONNECTION_STRING = 'grpc://localhost:2136/?database=/local', TOKEN_SECRET_NAME = 'token');"},
		{name: "a paused replication's credential", declared: &byPassword, read: new(mirror()), state: replication.StatePaused, impact: protocol.ChangeSemantics_BEHAVIORAL,
			reason: "ALTER ASYNC REPLICATION points the replication at another source or credential",
			text:   "ALTER ASYNC REPLICATION `app/mirror` SET (USER = 'replicator', PASSWORD_SECRET_NAME = 'password');"},
		{name: "a running replication dropped", read: new(mirror()), state: replication.StateRunning, impact: protocol.ChangeSemantics_DESTRUCTIVE,
			reason: "DROP ASYNC REPLICATION ... CASCADE drops the replica tables with the replication",
			text:   "DROP ASYNC REPLICATION `app/mirror` CASCADE;"},
		{name: "a replication of an unread state dropped", read: new(mirror()), impact: protocol.ChangeSemantics_DESTRUCTIVE,
			reason: "DROP ASYNC REPLICATION ... CASCADE drops the replica tables with the replication",
			text:   "DROP ASYNC REPLICATION `app/mirror` CASCADE;"},
		{name: "a failed-over replication dropped", read: new(mirror()), state: replication.StateDone, impact: protocol.ChangeSemantics_BEHAVIORAL,
			reason: "DROP ASYNC REPLICATION without CASCADE ends the replication and keeps its tables, read-only for good unless it was failed over first",
			text:   "DROP ASYNC REPLICATION `app/mirror`;"},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			request := objectRequest(c, schema.ReplicationProperties, replicationcompare.ReplicationServiceID, "mirror", declaredReplication(c, test.declared), readReplication(c, test.read, test.state))
			result, err := backend.New().CompareModels(t.Context(), request)
			c.Assert(err, qt.IsNil)
			c.Assert(result.Undecided, qt.HasLen, 0)
			c.Assert(result.Changes, qt.HasLen, 1)
			c.Assert(result.Changes[0].Value.ChangeSemantics.Impact, qt.Equals, test.impact)
			c.Assert(result.Changes[0].Value.ChangeSemantics.Reason, qt.Equals, test.reason)
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
// changes in place, with both batch settings, a batch size alone changes the
// rows the transfer writes as a lambda does, a paused transfer's connection
// changes in place, and a drop removes the consumer YDB created. Each change
// says what it costs in the host's words. Ported from the host's
// TestReplicationChange_Effect.
func TestTransferComparison_HappyPath(t *testing.T) {
	held := ingest()
	held.Consumer = "6f0b8c"
	relambda := ingest()
	relambda.Lambda = "($m) -> { return []; }"
	relambda.FlushInterval = "PT10S"
	rebatched := ingest()
	rebatched.BatchSizeBytes = 1024
	remote := ingest()
	remote.Connection.ConnectionString, remote.Source = "grpc://standby:2136/?database=/other", "events"
	remoteHeld := remote
	remoteHeld.Consumer = "6f0b8c"
	reconnected := remote
	reconnected.Connection.ConnectionString = "grpc://primary:2136/?database=/other"
	for _, test := range []struct {
		name     string
		declared *replication.TransferSpec
		read     *replication.TransferSpec
		state    string
		impact   protocol.ChangeSemantics_Impact
		reason   string
		text     string
	}{
		{name: "a creation", declared: new(ingest()), impact: protocol.ChangeSemantics_ADDITIVE, reason: "CREATE TRANSFER adds a transfer",
			text: "CREATE TRANSFER `app/ingest` FROM `orders/feed` TO `order_log` USING ($m) -> { return [<| id: $m._offset |>]; };"},
		{name: "a lambda and a flush interval", declared: &relambda, read: &held, state: replication.StateRunning, impact: protocol.ChangeSemantics_BEHAVIORAL,
			reason: "ALTER TRANSFER changes the rows the transfer writes from each message",
			text:   "ALTER TRANSFER `app/ingest` SET USING ($m) -> { return []; }, SET (BATCH_SIZE_BYTES = 8388608, FLUSH_INTERVAL = Interval('PT10S'));"},
		{name: "a batch size", declared: &rebatched, read: &held, state: replication.StateRunning, impact: protocol.ChangeSemantics_BEHAVIORAL,
			reason: "ALTER TRANSFER changes the rows the transfer writes from each message",
			text:   "ALTER TRANSFER `app/ingest` SET (BATCH_SIZE_BYTES = 1024, FLUSH_INTERVAL = Interval('PT1M'));"},
		{name: "a paused transfer's connection", declared: &reconnected, read: &remoteHeld, state: replication.StatePaused, impact: protocol.ChangeSemantics_BEHAVIORAL,
			reason: "ALTER TRANSFER points the transfer at another source or credential",
			text:   "ALTER TRANSFER `app/ingest` SET (CONNECTION_STRING = 'grpc://primary:2136/?database=/other');"},
		{name: "a drop", read: &held, state: replication.StateRunning, impact: protocol.ChangeSemantics_DESTRUCTIVE,
			reason: "DROP TRANSFER stops the transfer and drops the topic consumer YDB created for it, with its position in the topic", text: "DROP TRANSFER `app/ingest`;"},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			request := objectRequest(c, schema.TransferProperties, replicationcompare.TransferServiceID, "ingest", declaredTransfer(c, test.declared), readTransfer(c, test.read, test.state))
			result, err := backend.New().CompareModels(t.Context(), request)
			c.Assert(err, qt.IsNil)
			c.Assert(result.Undecided, qt.HasLen, 0)
			c.Assert(result.Changes, qt.HasLen, 1)
			c.Assert(result.Changes[0].Value.ChangeSemantics.Impact, qt.Equals, test.impact)
			c.Assert(result.Changes[0].Value.ChangeSemantics.Reason, qt.Equals, test.reason)
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
	c.Assert(err, qt.ErrorMatches, `.*changing transfers requires transfers; YDB keeps it behind feature flag EnableTopicTransfer: .*`)
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
	c.Assert(step.Metadata.Effects, qt.Any(qt.CmpEquals(protocmp.Transform())), &protocol.PlanningEffect{Subject: objectplan.PathSubject("replica", "accounts"), Action: protocol.PlanningEffect_DROP})
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

// sourceStep is a common step at src/accounts, the source mirror replicates,
// with action.
func sourceStep(name string, action protocol.PlanningEffect_Action) *protocol.CommonPlanningStep {
	return &protocol.CommonPlanningStep{Id: &protocol.StepIdentity{Owner: "host", Name: name},
		Metadata: &protocol.PlanningMetadata{Transaction: protocol.PlanningMetadata_FORBIDDEN, Impact: protocol.ChangeSemantics_ADDITIVE, Reason: "Acts on a table",
			Effects: []*protocol.PlanningEffect{{Subject: objectplan.PathSubject("src", "accounts"), Action: action}}}}
}

// edges writes a plan's dependencies as "before -> after", with the
// replication's own step as "step".
func edges(planned *protocol.PlannedFeatures) []string {
	var written []string
	for _, contribution := range planned.Contributions {
		name := func(id *protocol.StepIdentity) string {
			if proto.Equal(id, contribution.Steps[0].Id) {
				return "step"
			}
			return id.Name
		}
		for _, dependency := range contribution.Dependencies {
			written = append(written, name(dependency.Before)+" -> "+name(dependency.After))
		}
	}
	return written
}

// TestReplicationPlanningOrdersSourceTables reads the tables a replication of
// the session's database replicates: its creation comes after a common step
// that creates its source table, and its drop, with CASCADE or without, before
// one that drops it. A replication of another database reads no table here,
// and its creation still follows the last common step that needs none of the
// replication's steps, as every replication's creation does: the YDB engine
// built into the host at 74de0b65b created one after every table.
func TestReplicationPlanningOrdersSourceTables(t *testing.T) {
	absolute := mirror()
	absolute.Items[0].Source = "/local/src/accounts"
	elsewhere := mirror()
	elsewhere.Connection.ConnectionString = "grpc://standby:2136/?database=/other"
	for _, test := range []struct {
		name           string
		declared, read *replication.ReplicationSpec
		state          string
		common         *protocol.CommonPlanningStep
		want           []string
	}{
		{name: "a creation after its source table's", declared: new(mirror()), common: sourceStep("create", protocol.PlanningEffect_CREATE), want: []string{"create -> step"}},
		{name: "a source named by its absolute path", declared: &absolute, common: sourceStep("create", protocol.PlanningEffect_CREATE), want: []string{"create -> step"}},
		{name: "a creation after a change of its source table", declared: new(mirror()), common: sourceStep("alter", protocol.PlanningEffect_ALTER), want: []string{"alter -> step"}},
		{name: "a drop with CASCADE before its source table's", read: new(mirror()), state: replication.StateRunning, common: sourceStep("drop", protocol.PlanningEffect_DROP),
			want: []string{"step -> drop"}},
		{name: "a failed-over one's drop before its source table's", read: new(mirror()), state: replication.StateDone, common: sourceStep("drop", protocol.PlanningEffect_DROP),
			want: []string{"step -> drop"}},
		{name: "a replication of another database", declared: &elsewhere, common: sourceStep("create", protocol.PlanningEffect_CREATE), want: []string{"create -> step"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			request := objectRequest(c, schema.ReplicationProperties, replicationcompare.ReplicationServiceID, "mirror", declaredReplication(c, test.declared), readReplication(c, test.read, test.state))
			result, err := backend.New().CompareModels(t.Context(), request)
			c.Assert(err, qt.IsNil)
			c.Assert(result.Changes, qt.HasLen, 1)

			planned := planWith(c, result.Changes[0], nil, []*protocol.CommonPlanningStep{test.common})

			c.Assert(planned.Diagnostics, qt.HasLen, 0)
			c.Assert(planned.Contributions, qt.HasLen, 1)
			c.Assert(edges(planned), qt.DeepEquals, test.want)
		})
	}
}

// reads writes the effects a plan's one step has beyond its own object, each
// as its kind, database, directory, parent and name, and the action.
func reads(planned *protocol.PlannedFeatures) []string {
	var written []string
	step := planned.Contributions[0].Steps[0]
	for _, effect := range step.Metadata.Effects[2:] {
		subject := effect.Subject
		written = append(written, fmt.Sprintf("%s %s|%s|%s|%s %s", subject.Kind, subject.GetDatabase().GetNormalized(), subject.GetSchema().GetNormalized(),
			subject.GetParent().GetNormalized(), subject.GetName().GetNormalized(), effect.Action))
	}
	return written
}

// TestTransferPlanningReadsWhatItReads names what a transfer reads beyond its
// own path: the secret its connection names by path, by the secret's identity
// and by its path, so a plan that creates the secret creates it first; the
// table it writes; and at its source both a topic and a
// changefeed of a table, since only the plan knows which the path holds. The
// changefeed is named as the changefeed planner names one, so the host orders
// a transfer created beside its changefeed after it. A created transfer reads
// the topic by its path, and a dropped one by the topic's own identity, so a
// drop is not ordered after an object the plan creates at the path once the
// topic is gone. A source of another database is read in none of these ways.
// Last comes the directory a creation reads above its path, or the one a drop
// leaves room for at it. Ported from the host's
// TestReplicationPlan_ARemoteTransferReadsNoTopic.
func TestTransferPlanningReadsWhatItReads(t *testing.T) {
	remote := ingest()
	remote.Connection.ConnectionString = "grpc://standby:2136/?database=/other"
	remote.Source = "events"
	tokenByPath := remote
	tokenByPath.Connection.TokenSecretPath = "secrets/token" // #nosec G101 -- a secret's path, not its value
	topic := ingest()
	topic.Source = "app/events"
	for _, test := range []struct {
		name           string
		declared, read *replication.TransferSpec
		want           []string
	}{
		{name: "a creation from a changefeed", declared: new(ingest()), want: []string{
			"ydb.scheme-path |||order_log READ", "ydb.scheme-path |orders||feed READ", "ydb.changefeed ||orders|feed READ", "ydb.scheme-directory |||app READ"}},
		{name: "a creation from a topic", declared: &topic, want: []string{
			"ydb.scheme-path |||order_log READ", "ydb.scheme-path |app||events READ", "ydb.changefeed ||app|events READ", "ydb.scheme-directory |||app READ"}},
		{name: "a drop of one from a changefeed", read: new(ingest()), want: []string{
			"ydb.scheme-path |||order_log READ", schema.TopicProperties + " |orders||feed READ", schema.TopicProperties + " /local|orders||feed READ", "ydb.changefeed ||orders|feed READ",
			"ydb.scheme-directory |app||ingest CREATE"}},
		{name: "a creation from another database", declared: &remote, want: []string{"ydb.scheme-path |||order_log READ", "ydb.scheme-directory |||app READ"}},
		{name: "a creation from another database with a token secret named by path", declared: &tokenByPath, want: []string{
			schema.SecretProperties + " |secrets||token READ", "ydb.scheme-path |secrets||token READ", "ydb.scheme-path |||order_log READ", "ydb.scheme-directory |||app READ"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			request := objectRequest(c, schema.TransferProperties, replicationcompare.TransferServiceID, "ingest", declaredTransfer(c, test.declared), readTransfer(c, test.read, replication.StateRunning))
			result, err := backend.New().CompareModels(t.Context(), request)
			c.Assert(err, qt.IsNil)
			c.Assert(result.Changes, qt.HasLen, 1)

			planned := plan(c, replicationplan.TransferServiceID, result.Changes[0])

			c.Assert(planned.Diagnostics, qt.HasLen, 0)
			c.Assert(reads(planned), qt.DeepEquals, test.want)
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
// The planner of replications reads outlines, so the request carries the
// outline of table, as a host that lists planning.table-outlines sends it.
func keptReplicationRequest(c *qt.C, state, database string, table *protocol.PlanningTable) *protocol.PlanFeaturesRequest {
	c.Helper()
	read := readReplication(c, new(mirror()), state)
	read.Owner.Database = database
	return &protocol.PlanFeaturesRequest{Engine: "ydb", PlanningId: replicationplan.ReplicationServiceID, DatabasePath: "/local", ContextKinds: []string{schema.ReplicationProperties},
		TargetFeatures: backend.Description().Features, Capabilities: replicationCapabilities(), Identifiers: &protocol.ComparisonIdentifiers{Rules: proto.CloneOf(backend.Description().Engines[0].Identifiers)},
		DesiredObjects: replicationContextState(c, protocol.ValueInput_DESIRED, declaredReplication(c, new(mirror()))),
		CurrentObjects: replicationContextState(c, protocol.ValueInput_OBSERVED, read), Tables: []*protocol.PlanningTable{provider.TableOutline(table)}}
}

// A host that does not list planning.table-outlines sends the planner of
// replications the whole table, and the planner refuses the table at a live
// replica in the words it uses for the outline.
func TestKeptReplicationAssessmentOfAWholeTable_FailurePath(t *testing.T) {
	c := testcheck.New(t)
	table := &protocol.PlanningTable{Subject: replicaTable(), Action: protocol.PlanningTable_CREATE, Desired: declaredReplica()}
	request := keptReplicationRequest(c, replication.StateRunning, "", table)
	request.Tables = []*protocol.PlanningTable{table}
	request.TargetFeatures = slices.DeleteFunc(slices.Clone(request.TargetFeatures), func(feature string) bool { return feature == provider.TableOutlinesFeature })

	result, err := backend.New().PlanFeatures(t.Context(), request)

	c.Assert(err, qt.IsNil)
	c.Assert(result.Objects, qt.HasLen, 0)
	c.Assert(result.Diagnostics, qt.HasLen, 1)
	c.Assert(result.Diagnostics[0].Message, qt.Matches, "table replica/accounts: it is a replica table async replication app/mirror writes, read-only while the replication runs .*")
}

// A host that lists planning.table-outlines sends the outline, and a whole
// table is refused before the planner reads it.
func TestKeptReplicationAssessmentOfAWholeTableFromAnOutliningHost_FailurePath(t *testing.T) {
	c := testcheck.New(t)
	table := &protocol.PlanningTable{Subject: replicaTable(), Action: protocol.PlanningTable_CREATE, Desired: declaredReplica()}
	request := keptReplicationRequest(c, replication.StateRunning, "", table)
	request.Tables = []*protocol.PlanningTable{table}

	result, err := backend.New().PlanFeatures(t.Context(), request)

	c.Assert(status.Code(err), qt.Equals, codes.InvalidArgument)
	c.Assert(err, qt.ErrorMatches, `.*an outlined table carries more than its subject, action and the names of its states`)
	c.Assert(result, qt.IsNil)
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

// droppedSource is the drop of src/accounts, the table mirror reads in the
// session's database, as an outlining host sends it.
func droppedSource() *protocol.PlanningTable {
	subject := replicaTable()
	subject.Schema = testcomparison.Part("src")
	return &protocol.PlanningTable{Subject: subject, Action: protocol.PlanningTable_DROP, Current: &protocol.ObservedTable{Schema: "src", Name: "accounts"}}
}

// A plan that drops a table a replication it keeps reads in the session's
// database is refused, in any state of the replication: YDB drops the table
// and leaves a running replication failing, measured on 25.1.4.7 and
// 26.2.1.14. The host's embedded engine refused it.
func TestKeptReplicationAssessmentRefusesADroppedSource_FailurePath(t *testing.T) {
	for _, state := range []string{replication.StateRunning, replication.StatePaused, replication.StateDone} {
		t.Run(state, func(t *testing.T) {
			c := testcheck.New(t)

			result, err := backend.New().PlanFeatures(t.Context(), keptReplicationRequest(c, state, "", droppedSource()))

			c.Assert(err, qt.IsNil)
			c.Assert(result.Objects, qt.HasLen, 0)
			c.Assert(result.Diagnostics, qt.HasLen, 1)
			c.Assert(result.Diagnostics[0].Message, qt.Equals, "table src/accounts: async replication app/mirror reads it through a changefeed its "+
				"controller manages, and the schema keeps the replication; YDB drops the table and leaves the replication failing "+
				"(`Discovery for all topics failed`); drop the replication in the same plan, or keep the table")
		})
	}
}

// A table no replication of the database reads is no replication's business:
// a replication of another database reads none here, and a table elsewhere is
// not its source.
func TestKeptReplicationAssessmentLeavesADroppedTable_HappyPath(t *testing.T) {
	elsewhere := droppedSource()
	elsewhere.Subject.Schema = testcomparison.Part("other")
	for _, test := range []struct {
		name     string
		table    *protocol.PlanningTable
		database string
	}{
		{name: "a table no replication reads", table: elsewhere, database: "/local"},
		{name: "a replication of another database", table: droppedSource(), database: "/remote"},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			request := keptReplicationRequest(c, replication.StateRunning, "", test.table)
			request.DatabasePath = test.database

			result, err := backend.New().PlanFeatures(t.Context(), request)

			c.Assert(err, qt.IsNil)
			c.Assert(result.Diagnostics, qt.HasLen, 0)
			c.Assert(result.Objects, qt.HasLen, 1)
		})
	}
}

// A batch that names one replication twice, which no comparison produces, is
// refused before anything is planned, whether the two changes share a value
// identity or not. Ported from the host's
// TestReplicationPlan_RefusesAChangeNamedTwice.
func TestReplicationPlanningRefusesAChangeNamedTwice_FailurePath(t *testing.T) {
	for _, test := range []struct {
		name, secondID string
	}{
		{name: "under one value identity", secondID: "value"},
		{name: "under two value identities", secondID: "second"},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			request := objectRequest(c, schema.ReplicationProperties, replicationcompare.ReplicationServiceID, "mirror", declaredReplication(c, new(mirror())), nil)
			result, err := backend.New().CompareModels(t.Context(), request)
			c.Assert(err, qt.IsNil)
			c.Assert(result.Changes, qt.HasLen, 1)
			change := &protocol.PlanningChange{Subject: result.Changes[0].Subject, Value: result.Changes[0].Value}
			second := proto.CloneOf(change)
			second.Value.Id = test.secondID

			planned, err := backend.New().PlanFeatures(c.Context(), &protocol.PlanFeaturesRequest{Engine: "ydb", TargetFeatures: []string{provider.PlanningLifecycleFeature},
				PlanningId: replicationplan.ReplicationServiceID, DatabasePath: "/local", Identifiers: &protocol.ComparisonIdentifiers{Rules: proto.CloneOf(backend.Description().Engines[0].Identifiers)},
				Capabilities: replicationCapabilities(), Changes: []*protocol.PlanningChange{change, second}})

			c.Assert(status.Code(err), qt.Equals, codes.InvalidArgument)
			c.Assert(planned, qt.IsNil)
		})
	}
}

// TestReplicationPlanningRefusesWhatTheTargetLacks_FailurePath refuses,
// before any step is planned, a replication statement the target cannot take:
// a drop on a target without async replications, and a creation naming a
// secret by its path on a target whose replications cannot, as 25.3. Ported
// from the host's TestReplicationPlan_Refusals.
func TestReplicationPlanningRefusesWhatTheTargetLacks_FailurePath(t *testing.T) {
	byPath := mirror()
	byPath.Connection = replication.Connection{ConnectionString: byPath.Connection.ConnectionString, TokenSecretPath: "secrets/token"} // #nosec G101 -- a secret's path, not its value
	for _, test := range []struct {
		name           string
		declared, read *replication.ReplicationSpec
		withheld       string
		wantErr        string
	}{
		{name: "a drop without async replications", read: new(mirror()), withheld: replication.CapabilityReplication,
			wantErr: `Planning a async replication requires the async_replication capability and nontransactional DDL.*`},
		{name: "a secret path without replication secret paths", declared: &byPath, withheld: replication.CapabilitySecretPaths,
			wantErr: `async replication app/mirror names a secret by its path requires target capability replication_secret_paths`},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			request := objectRequest(c, schema.ReplicationProperties, replicationcompare.ReplicationServiceID, "mirror", declaredReplication(c, test.declared), readReplication(c, test.read, replication.StateRunning))
			result, err := backend.New().CompareModels(t.Context(), request)
			c.Assert(err, qt.IsNil)
			c.Assert(result.Changes, qt.HasLen, 1)
			capabilities := replicationCapabilities()
			capabilities[test.withheld] = false

			planned, err := backend.New().PlanFeatures(c.Context(), &protocol.PlanFeaturesRequest{Engine: "ydb", TargetFeatures: []string{provider.PlanningLifecycleFeature},
				PlanningId: replicationplan.ReplicationServiceID, DatabasePath: "/local", Identifiers: &protocol.ComparisonIdentifiers{Rules: proto.CloneOf(backend.Description().Engines[0].Identifiers)},
				Capabilities: capabilities, Changes: []*protocol.PlanningChange{{Subject: result.Changes[0].Subject, Value: result.Changes[0].Value}}})

			c.Assert(err, qt.IsNil)
			c.Assert(planned.Contributions, qt.HasLen, 0)
			c.Assert(planned.Diagnostics, qt.HasLen, 1)
			c.Assert(planned.Diagnostics[0].Message, qt.Matches, test.wantErr)
		})
	}
}

// TestReplicationConversion_KeepsEverySetting converts a declaration into the
// observation a read reports once it is applied, with no state, and an
// observation back into the declaration that keeps it, without its state:
// every setting survives both ways. Ported from the host's
// TestReplicationConversion_KeepsEverySetting and TestConversion_KeepsTheObject.
func TestReplicationConversion_KeepsEverySetting(t *testing.T) {
	c := testcheck.New(t)
	full := mirror()
	full.ConsistencyLevel, full.CommitInterval = replication.ConsistencyGlobal, "PT5S"
	held := ingest()
	held.Consumer, held.BatchSizeBytes, held.FlushInterval = "c", 1024, "PT5S"
	for _, test := range []struct {
		name            string
		value           *protocol.Envelope
		from, to        protocol.ValueInput_Representation
		wantReplication replication.ReplicationSpec
		wantTransfer    replication.TransferSpec
		kind            string
	}{
		{name: "a declared replication", value: declaredReplication(c, &full),
			from: protocol.ValueInput_DESIRED, to: protocol.ValueInput_OBSERVED, wantReplication: full, kind: schema.ReplicationProperties},
		{name: "a read replication", value: readReplication(c, &full, replication.StatePaused),
			from: protocol.ValueInput_OBSERVED, to: protocol.ValueInput_DESIRED, wantReplication: full, kind: schema.ReplicationProperties},
		{name: "a declared transfer", value: declaredTransfer(c, &held),
			from: protocol.ValueInput_DESIRED, to: protocol.ValueInput_OBSERVED, wantTransfer: held, kind: schema.TransferProperties},
		{name: "a read transfer", value: readTransfer(c, &held, replication.StateRunning),
			from: protocol.ValueInput_OBSERVED, to: protocol.ValueInput_DESIRED, wantTransfer: held, kind: schema.TransferProperties},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			service := backend.New()
			normalized, err := service.NormalizeValues(t.Context(), &protocol.NormalizeValuesRequest{Engine: "ydb", Values: []*protocol.ValueInput{{Id: "v", Representation: test.from, Value: test.value}}})
			c.Assert(err, qt.IsNil)

			converted, err := service.ConvertValues(t.Context(), &protocol.ConvertValuesRequest{Engine: "ydb", From: test.from, To: test.to, Values: normalized.Values})

			c.Assert(err, qt.IsNil)
			c.Assert(converted.Values, qt.HasLen, 1)
			replicationSpec, transferSpec, state := convertedSpecs(c, test.kind, test.to, converted.Values[0].Value)
			c.Assert(replicationSpec, qt.DeepEquals, test.wantReplication)
			c.Assert(transferSpec, qt.DeepEquals, test.wantTransfer)
			c.Assert(state, qt.Equals, "")
		})
	}
}

// convertedSpecs reads value, of kind in representation, as the replication or
// the transfer it holds, and the state an observation reports; the spec of the
// other kind is zero.
func convertedSpecs(c *qt.C, kind string, representation protocol.ValueInput_Representation, value *protocol.Envelope) (replication.ReplicationSpec, replication.TransferSpec, string) {
	c.Helper()
	var replicationSpec replication.ReplicationSpec
	var transferSpec replication.TransferSpec
	var state string
	var err error
	switch {
	case kind == schema.ReplicationProperties && representation == protocol.ValueInput_OBSERVED:
		replicationSpec, state, err = schema.ObservedReplication(value)
	case kind == schema.ReplicationProperties:
		replicationSpec, err = schema.DesiredReplication(value)
	case representation == protocol.ValueInput_OBSERVED:
		transferSpec, state, err = schema.ObservedTransfer(value)
	default:
		transferSpec, err = schema.DesiredTransfer(value)
	}
	c.Assert(err, qt.IsNil)
	return replicationSpec, transferSpec, state
}

// tableRelation is a declared table of the schema, as a host that lists
// models.declared-relations names it among the declared relations.
func tableRelation(directory, name string) *protocol.ComparisonIdentity {
	return &protocol.ComparisonIdentity{Kind: "table", Database: &protocol.ComparisonPart{}, Catalog: &protocol.ComparisonPart{}, Schema: testcomparison.Part(directory),
		Parent: &protocol.ComparisonPart{}, Name: testcomparison.Part(name)}
}

// transferRelationsRequest compares the declared transfer app/ingest, which
// writes table order_log, beside relations, from a host that lists every
// declared table among them.
func transferRelationsRequest(c *qt.C, relations ...*protocol.ComparisonIdentity) *protocol.CompareModelsRequest {
	c.Helper()
	request := objectRequest(c, schema.TransferProperties, replicationcompare.TransferServiceID, "ingest", declaredTransfer(c, new(ingest())), nil)
	request.TargetFeatures = append(request.TargetFeatures, provider.DeclaredRelationsFeature)
	request.DeclaredRelations = relations
	return request
}

// A declared transfer into a table the schema does not declare is refused
// before anything is planned: YDB creates no transfer into a table that is
// not there (`Path does not exist`), and the host's embedded engine refused
// it in these words. Ported from the host's
// TestGenerateMigrationAST_Replications_TransferReadsADeclaredTopic and
// TestGenerateMigrationAST_Replications_FailurePath (the undeclared table).
func TestTransferComparisonRefusesAnUndeclaredTable_FailurePath(t *testing.T) {
	for _, test := range []struct {
		name      string
		relations []*protocol.ComparisonIdentity
	}{
		{name: "no table declared"},
		{name: "a table elsewhere", relations: []*protocol.ComparisonIdentity{tableRelation("app", "order_log")}},
		{name: "a view of the name", relations: []*protocol.ComparisonIdentity{{Kind: "view", Database: &protocol.ComparisonPart{}, Catalog: &protocol.ComparisonPart{},
			Schema: &protocol.ComparisonPart{}, Parent: &protocol.ComparisonPart{}, Name: testcomparison.Part("order_log")}}},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)

			result, err := backend.New().CompareModels(c.Context(), transferRelationsRequest(c, test.relations...))

			c.Assert(err, qt.ErrorMatches, "rpc error: code = InvalidArgument desc = transfer app/ingest: it writes table order_log, which the schema does not declare, "+
				"and YDB creates no transfer into a table that is not there \\(`Path does not exist`\\); declare the table")
			c.Assert(result, qt.IsNil)
		})
	}
}

// A transfer into a declared table is compared, and so is any transfer from
// a host that names only views among the declared relations, which cannot
// say a table is undeclared.
func TestTransferComparisonRefusesAnUndeclaredTable_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	declared := transferRelationsRequest(c, tableRelation("", "order_log"))
	viewsOnly := objectRequest(c, schema.TransferProperties, replicationcompare.TransferServiceID, "ingest", declaredTransfer(c, new(ingest())), nil)

	for _, request := range []*protocol.CompareModelsRequest{declared, viewsOnly} {
		result, err := backend.New().CompareModels(c.Context(), request)

		c.Assert(err, qt.IsNil)
		c.Assert(result.Changes, qt.HasLen, 1)
	}
}

// topicObject is a declared topic of the schema, as a host that lists
// models.declared-objects names it among the declared objects of the
// transfer comparison.
func topicObject(directory, name string) *protocol.ComparisonIdentity {
	return &protocol.ComparisonIdentity{Kind: schema.TopicProperties, Database: &protocol.ComparisonPart{}, Catalog: &protocol.ComparisonPart{},
		Schema: testcomparison.Part(directory), Parent: &protocol.ComparisonPart{}, Name: testcomparison.Part(name)}
}

// transferTopicsRequest compares the declared transfer app/ingest of spec
// from a host that lists every declared table among the relations and sends
// the declared topics, with the table order_log declared beside relations
// and objects.
func transferTopicsRequest(c *qt.C, spec replication.TransferSpec, relations []*protocol.ComparisonIdentity, objects ...*protocol.ComparisonIdentity) *protocol.CompareModelsRequest {
	c.Helper()
	request := transferRelationsRequest(c, append([]*protocol.ComparisonIdentity{tableRelation("", "order_log")}, relations...)...)
	request.Desired.Records[0].Value = declaredTransferValue(c, spec)
	request.TargetFeatures = append(request.TargetFeatures, provider.DeclaredObjectsFeature)
	request.DeclaredObjects = objects
	return request
}

// declaredTransferValue normalizes the declaration of the transfer
// app/ingest of spec, as the host sends it in a comparison.
func declaredTransferValue(c *qt.C, spec replication.TransferSpec) *protocol.NormalizedValue {
	c.Helper()
	normalized, err := backend.New().NormalizeValues(c.Context(), &protocol.NormalizeValuesRequest{Engine: "ydb",
		Values: []*protocol.ValueInput{{Id: "value", Representation: protocol.ValueInput_DESIRED, Value: declaredTransfer(c, &spec)}}})
	c.Assert(err, qt.IsNil)
	return normalized.Values[0]
}

// withSource is ingest reading source.
func withSource(source string) replication.TransferSpec {
	spec := ingest()
	spec.Source = source
	return spec
}

// The transfer comparison names topics among its declared kinds, so a host
// that lists models.declared-objects sends it the topics the schema
// declares.
func TestTransferComparisonReadsTheDeclaredTopics(t *testing.T) {
	c := testcheck.New(t)

	comparisons := backend.Description().GetEngines()[0].GetComparisons()

	transfers := slices.IndexFunc(comparisons, func(comparison *protocol.ModelComparison) bool {
		return comparison.GetId() == replicationcompare.TransferServiceID
	})

	c.Assert(transfers >= 0, qt.IsTrue)
	c.Assert(comparisons[transfers].GetDeclaredKinds(), qt.DeepEquals, []string{schema.TopicProperties})
	c.Assert(backend.Description().GetFeatures(), qt.Contains, provider.DeclaredObjectsFeature)
}

// A declared transfer of a topic of its own database that the schema declares
// neither as a topic nor as a changefeed of a declared table is refused before
// anything is planned, in the words of the host's embedded engine: YDB
// accepts the transfer and then stops it (`Discovery error`). Measured with
// built-in 74de0b65b on 26.2.1.14 for a topic and for a stream of a declared
// table: both refused.
func TestTransferComparisonRefusesAnUndeclaredTopic_FailurePath(t *testing.T) {
	for _, test := range []struct {
		name      string
		source    string
		relations []*protocol.ComparisonIdentity
		objects   []*protocol.ComparisonIdentity
		topic     string
	}{
		{name: "no topic declared", source: "events", topic: "events"},
		{name: "a topic of another name", source: "events", objects: []*protocol.ComparisonIdentity{topicObject("", "other")}, topic: "events"},
		{name: "the topic in another directory", source: "app/events", objects: []*protocol.ComparisonIdentity{topicObject("ext", "events")}, topic: "app/events"},
		{name: "a stream of an undeclared table", source: "orders/feed", topic: "orders/feed"},
		{name: "a stream of a view of the table's name", source: "orders/feed", topic: "orders/feed",
			relations: []*protocol.ComparisonIdentity{{Kind: "view", Database: &protocol.ComparisonPart{}, Catalog: &protocol.ComparisonPart{},
				Schema: &protocol.ComparisonPart{}, Parent: &protocol.ComparisonPart{}, Name: testcomparison.Part("orders")}}},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)

			result, err := backend.New().CompareModels(c.Context(), transferTopicsRequest(c, withSource(test.source), test.relations, test.objects...))

			c.Assert(err, qt.ErrorMatches, "rpc error: code = InvalidArgument desc = transfer app/ingest: it reads topic "+test.topic+", which the schema "+
				"declares neither as a topic nor as a changefeed, so the plan leaves no such topic, and YDB accepts such a transfer and then stops it "+
				"\\(`Discovery error`\\); declare the topic or the changefeed")
			c.Assert(result, qt.IsNil)
		})
	}
}

// A transfer of a declared topic is compared, and so is one of a stream of a
// declared table, which the comparison takes for one of the table's
// changefeeds, and one of another database's topic, which this plan does not
// keep. Measured with built-in 74de0b65b on 26.2.1.14: a transfer of a
// declared changefeed is planned.
func TestTransferComparisonRefusesAnUndeclaredTopic_HappyPath(t *testing.T) {
	other := withSource("events")
	other.Connection = replication.Connection{ConnectionString: "grpc://localhost:2136/?database=/other", TokenSecretName: "token"}
	for _, test := range []struct {
		name      string
		spec      replication.TransferSpec
		relations []*protocol.ComparisonIdentity
		objects   []*protocol.ComparisonIdentity
	}{
		{name: "a declared topic", spec: withSource("app/events"), objects: []*protocol.ComparisonIdentity{topicObject("app", "events")}},
		{name: "a changefeed of a declared table", spec: withSource("orders/feed"), relations: []*protocol.ComparisonIdentity{tableRelation("", "orders")}},
		{name: "another database's topic", spec: other},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)

			result, err := backend.New().CompareModels(c.Context(), transferTopicsRequest(c, test.spec, test.relations, test.objects...))

			c.Assert(err, qt.IsNil)
			c.Assert(result.Changes, qt.HasLen, 1)
		})
	}
}

// changefeedFacet is the declared changefeed set of the table at directory
// and name, holding a changefeed of each name given, as a host that lists
// models.declared-facets names it among the declared facets of the transfer
// comparison.
func changefeedFacet(c *qt.C, directory, name string, changefeeds ...string) *protocol.ComparisonRecord {
	c.Helper()
	intent := &native.ChangefeedSetIntent{}
	for _, feed := range changefeeds {
		intent.Changefeeds = append(intent.Changefeeds, &native.ChangefeedIntent{Name: feed, Mode: Ydb_Table.ChangefeedMode_MODE_UPDATES,
			Format: Ydb_Table.ChangefeedFormat_FORMAT_JSON, Topic: &native.TopicIntent{PartitioningSettings: &Ydb_Topic.PartitioningSettings{}}})
	}
	value, err := schema.CaptureChangefeedIntent(&protocol.Reference{Engine: "ydb", Schema: directory, Kind: "table", Name: name}, intent)
	c.Assert(err, qt.IsNil)
	normalized, err := backend.New().NormalizeValues(c.Context(), &protocol.NormalizeValuesRequest{Engine: "ydb",
		Values: []*protocol.ValueInput{{Id: "changefeeds-" + name, Representation: protocol.ValueInput_DESIRED, Value: value}}})
	c.Assert(err, qt.IsNil)
	return &protocol.ComparisonRecord{Kind: schema.ChangefeedProperties, Subject: tableRelation(directory, name), Value: normalized.Values[0]}
}

// transferChangefeedsRequest is transferTopicsRequest from a host that also
// sends the declared changefeeds of the tables, facets.
func transferChangefeedsRequest(c *qt.C, spec replication.TransferSpec, relations []*protocol.ComparisonIdentity, facets ...*protocol.ComparisonRecord) *protocol.CompareModelsRequest {
	c.Helper()
	request := transferTopicsRequest(c, spec, relations)
	request.TargetFeatures = append(request.TargetFeatures, provider.DeclaredFacetsFeature)
	request.DeclaredFacets = facets
	return request
}

// The transfer comparison names the changefeeds of the tables among its
// declared facet kinds, so a host that lists models.declared-facets sends it
// the changefeeds the schema's tables declare.
func TestTransferComparisonReadsTheDeclaredChangefeeds(t *testing.T) {
	c := testcheck.New(t)

	comparisons := backend.Description().GetEngines()[0].GetComparisons()

	transfers := slices.IndexFunc(comparisons, func(comparison *protocol.ModelComparison) bool {
		return comparison.GetId() == replicationcompare.TransferServiceID
	})

	c.Assert(transfers >= 0, qt.IsTrue)
	c.Assert(comparisons[transfers].GetDeclaredFacetKinds(), qt.DeepEquals, []string{schema.ChangefeedProperties})
	c.Assert(backend.Description().GetFeatures(), qt.Contains, provider.DeclaredFacetsFeature)
}

// With the declared changefeeds, a transfer of a stream of a declared table
// that is none of the table's declared changefeeds is refused in the words of
// the host's embedded engine (G228). Measured with built-in 74de0b65b on
// 26.2.1.14: a transfer of orders/other beside a table orders declaring the
// changefeed feed was refused.
func TestTransferComparisonRefusesAnUndeclaredChangefeed_FailurePath(t *testing.T) {
	for _, test := range []struct {
		name   string
		facets []string
	}{
		{name: "another changefeed of the table", facets: []string{"feed"}},
		{name: "a table that declares no changefeed"},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			facets := []*protocol.ComparisonRecord{changefeedFacet(c, "", "orders", test.facets...)}

			result, err := backend.New().CompareModels(c.Context(), transferChangefeedsRequest(c, withSource("orders/other"),
				[]*protocol.ComparisonIdentity{tableRelation("", "orders")}, facets...))

			c.Assert(err, qt.ErrorMatches, "rpc error: code = InvalidArgument desc = transfer app/ingest: it reads topic orders/other, which the schema "+
				"declares neither as a topic nor as a changefeed, so the plan leaves no such topic, and YDB accepts such a transfer and then stops it "+
				"\\(`Discovery error`\\); declare the topic or the changefeed")
			c.Assert(result, qt.IsNil)
		})
	}
}

// A transfer of a declared changefeed is compared, in the table's directory
// too, and a declared topic is compared whatever the changefeeds say.
func TestTransferComparisonRefusesAnUndeclaredChangefeed_HappyPath(t *testing.T) {
	for _, test := range []struct {
		name        string
		source      string
		directory   string
		changefeeds []string
	}{
		{name: "a declared changefeed", source: "orders/feed", changefeeds: []string{"other", "feed"}},
		{name: "a declared changefeed of a table in a directory", source: "app/orders/feed", directory: "app", changefeeds: []string{"feed"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			relations := []*protocol.ComparisonIdentity{tableRelation(test.directory, "orders")}

			result, err := backend.New().CompareModels(c.Context(), transferChangefeedsRequest(c, withSource(test.source), relations,
				changefeedFacet(c, test.directory, "orders", test.changefeeds...)))

			c.Assert(err, qt.IsNil)
			c.Assert(result.Changes, qt.HasLen, 1)
		})
	}
}

// heldFeed is the read's JSON of one changefeed of a table, named name, which
// a declaration can describe.
func heldFeed(name string) string {
	return `{"description":{"name":"` + name + `","mode":"MODE_UPDATES","format":"FORMAT_JSON","state":"STATE_ENABLED"},"topic":{"partitioningSettings":{}}}`
}

// heldChangefeeds is the held changefeed set of the table at directory and
// name, holding each changefeed of feeds, the read's JSON of each, as a host
// that lists models.current-context names it in the current context of the
// transfer comparison.
func heldChangefeeds(c *qt.C, directory, name string, feeds ...string) *protocol.ComparisonRecord {
	c.Helper()
	held := &native.ChangefeedObservations{}
	c.Assert(protojson.Unmarshal([]byte(`{"changefeeds":[`+strings.Join(feeds, ",")+`]}`), held), qt.IsNil)
	value, err := schema.CaptureChangefeeds(&protocol.Reference{Engine: "ydb", Database: "/local", Schema: directory, Kind: "table", Name: name}, held)
	c.Assert(err, qt.IsNil)
	normalized, err := backend.New().NormalizeValues(c.Context(), &protocol.NormalizeValuesRequest{Engine: "ydb",
		Values: []*protocol.ValueInput{{Id: "held-" + name, Representation: protocol.ValueInput_OBSERVED, Value: value}}})
	c.Assert(err, qt.IsNil)
	subject := tableRelation(directory, name)
	subject.Database = testcomparison.Part("/local")
	return &protocol.ComparisonRecord{Kind: schema.ChangefeedProperties, Subject: subject, Value: normalized.Values[0]}
}

// changefeedsKnown is the coverage record of the changefeed set of the table
// at directory and name in state.
func changefeedsKnown(directory, name string, state protocol.ModelKnowledge_State) *protocol.ComparisonSubjectCoverage {
	subject := tableRelation(directory, name)
	subject.Database = testcomparison.Part("/local")
	return &protocol.ComparisonSubjectCoverage{Kind: schema.ChangefeedProperties, Subject: subject, Knowledge: testcomparison.Knowledge(state)}
}

// unreadChangefeeds is the changefeed set of the table at directory and name
// as the read records one holding a changefeed it cannot describe, such as one
// whose topic has a consumer with a codec Ptah does not declare: the set
// unrepresentable, and no value.
func unreadChangefeeds(directory, name string) *protocol.ComparisonSubjectCoverage {
	return changefeedsKnown(directory, name, protocol.ModelKnowledge_UNREPRESENTABLE)
}

// topicKnown is the coverage record of the topic at directory and name in
// state.
func topicKnown(directory, name string, state protocol.ModelKnowledge_State) *protocol.ComparisonSubjectCoverage {
	subject := topicObject(directory, name)
	subject.Database = testcomparison.Part("/local")
	return &protocol.ComparisonSubjectCoverage{Kind: schema.TopicProperties, Subject: subject, Knowledge: testcomparison.Knowledge(state)}
}

// recordedTopic is the topic at directory and name the read lists and does
// not read, such as a queue of the older persistent queue kind.
func recordedTopic(directory, name string) *protocol.ComparisonSubjectCoverage {
	return topicKnown(directory, name, protocol.ModelKnowledge_UNINSPECTED)
}

// transferCurrentRequest is transferChangefeedsRequest of a table orders
// that declares no changefeed, from a host that also sends what the read
// holds (models.current-context): the held changefeed sets, and the coverage
// records of the topics and changefeed sets the read recorded without
// describing them.
func transferCurrentRequest(c *qt.C, spec replication.TransferSpec, relations []*protocol.ComparisonIdentity, held []*protocol.ComparisonRecord,
	recorded ...*protocol.ComparisonSubjectCoverage,
) *protocol.CompareModelsRequest {
	c.Helper()
	request := transferChangefeedsRequest(c, spec, relations, changefeedFacet(c, "", "orders"))
	request.TargetFeatures = append(request.TargetFeatures, provider.CurrentContextFeature)
	current := testcomparison.State(observedModel(schema.ChangefeedProperties))
	current.Coverage.Kinds = append(current.Coverage.Kinds, testcomparison.State(observedModel(schema.TopicProperties)).Coverage.Kinds...)
	current.Coverage.Subjects = recorded
	current.Records = held
	request.CurrentContext = current
	return request
}

// The transfer comparison lists models.current-context, so a host that lists
// it sends the comparison what the read holds of the topics and the tables'
// changefeeds.
func TestTransferComparisonReadsTheCurrentContext(t *testing.T) {
	c := testcheck.New(t)

	description := backend.Description()

	c.Assert(description.GetFeatures(), qt.Contains, provider.CurrentContextFeature)
	c.Assert(description.GetEngines()[0].GetFeatures(), qt.Contains, provider.CurrentContextFeature)
}

// A transfer of a topic or a changefeed the read recorded without describing
// it is compared, since the plan neither drops nor changes such an object, as
// the host's embedded engine accepted it (G239). The read leaves out the whole
// changefeed set of a table that holds a changefeed it cannot describe, so any
// stream of that table is taken for one of its changefeeds. Measured with
// built-in 74de0b65b on 26.2.1.14: a transfer of a changefeed whose consumer
// names codec 10042, under a table that declares no changefeed, was planned.
func TestTransferComparisonAcceptsARecordedSource_HappyPath(t *testing.T) {
	for _, test := range []struct {
		name      string
		source    string
		directory string
		recorded  []*protocol.ComparisonSubjectCoverage
	}{
		{name: "a table whose changefeeds the read could not describe", source: "orders/feed", recorded: []*protocol.ComparisonSubjectCoverage{unreadChangefeeds("", "orders")}},
		{name: "such a table in a directory", source: "app/orders/feed", directory: "app", recorded: []*protocol.ComparisonSubjectCoverage{unreadChangefeeds("app", "orders")}},
		{name: "a topic the read lists and does not read", source: "events", recorded: []*protocol.ComparisonSubjectCoverage{recordedTopic("", "events")}},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			relations := []*protocol.ComparisonIdentity{tableRelation(test.directory, "orders")}

			result, err := backend.New().CompareModels(c.Context(), transferCurrentRequest(c, withSource(test.source), relations, nil, test.recorded...))

			c.Assert(err, qt.IsNil)
			c.Assert(result.Changes, qt.HasLen, 1)
		})
	}
}

// A changefeed the read describes is one the plan drops when the table does
// not declare it, and the changefeeds of an undeclared table go with it; an
// unread set of another table, or a recorded topic of another name, says
// nothing of the topic the transfer reads. Each transfer is refused as one of
// a missing topic.
func TestTransferComparisonAcceptsARecordedSource_FailurePath(t *testing.T) {
	for _, test := range []struct {
		name      string
		source    string
		relations []*protocol.ComparisonIdentity
		feeds     []string
		recorded  []*protocol.ComparisonSubjectCoverage
	}{
		{name: "a changefeed the read describes", source: "orders/feed", relations: []*protocol.ComparisonIdentity{tableRelation("", "orders")},
			feeds: []string{heldFeed("feed")}},
		{name: "the unread changefeeds of an undeclared table", source: "orders/feed", recorded: []*protocol.ComparisonSubjectCoverage{unreadChangefeeds("", "orders")}},
		{name: "the unread changefeeds of another table", source: "orders/feed", relations: []*protocol.ComparisonIdentity{tableRelation("", "orders")},
			recorded: []*protocol.ComparisonSubjectCoverage{unreadChangefeeds("", "items")}},
		{name: "a recorded topic of another name", source: "events", recorded: []*protocol.ComparisonSubjectCoverage{recordedTopic("", "other")}},
		{name: "a table whose changefeeds the read describes", source: "orders/feed", relations: []*protocol.ComparisonIdentity{tableRelation("", "orders")},
			recorded: []*protocol.ComparisonSubjectCoverage{changefeedsKnown("", "orders", protocol.ModelKnowledge_COMPLETE)}},
		{name: "a topic the read describes", source: "events", recorded: []*protocol.ComparisonSubjectCoverage{topicKnown("", "events", protocol.ModelKnowledge_COMPLETE)}},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			held := []*protocol.ComparisonRecord{heldChangefeeds(c, "", "orders", test.feeds...)}

			result, err := backend.New().CompareModels(c.Context(), transferCurrentRequest(c, withSource(test.source), test.relations, held, test.recorded...))

			c.Assert(err, qt.ErrorMatches, "rpc error: code = InvalidArgument desc = transfer app/ingest: it reads topic "+test.source+", which the schema "+
				"declares neither as a topic nor as a changefeed, so the plan leaves no such topic, and YDB accepts such a transfer and then stops it "+
				"\\(`Discovery error`\\); declare the topic or the changefeed")
			c.Assert(result, qt.IsNil)
		})
	}
	t.Run("a host that sends no current context", func(t *testing.T) {
		c := testcheck.New(t)
		request := transferChangefeedsRequest(c, withSource("orders/feed"), []*protocol.ComparisonIdentity{tableRelation("", "orders")}, changefeedFacet(c, "", "orders"))

		result, err := backend.New().CompareModels(c.Context(), request)

		c.Assert(err, qt.ErrorMatches, "rpc error: code = InvalidArgument desc = transfer app/ingest: it reads topic orders/feed, which the schema .*")
		c.Assert(result, qt.IsNil)
	})
}

// selectedRead is transferCurrentRequest from a host whose read selected the
// directory app alone, as a run with a schema selection reads it: the
// topics and the tables' changefeeds of app are complete, and everything
// outside it is uninspected. The changefeed region names the database, as
// the host sends it, and the topic region does not.
func selectedRead(c *qt.C, spec replication.TransferSpec) *protocol.CompareModelsRequest {
	c.Helper()
	request := transferCurrentRequest(c, spec, nil, nil)
	region := func(kind string, database *protocol.ComparisonPart) *protocol.ComparisonKindCoverage {
		return &protocol.ComparisonKindCoverage{Model: observedModel(kind), Sources: []*protocol.ComparisonKnowledgeSource{{
			Outside: &protocol.ModelKnowledge{State: protocol.ModelKnowledge_UNINSPECTED, Reason: "Knowledge is limited to returned subjects and fully listed directories"},
			Regions: []*protocol.ComparisonRegion{{
				Scope: &protocol.ComparisonNamespace{Targets: []string{"ydb"}, SubjectKinds: []string{kind}, Database: database,
					Catalog: &protocol.ComparisonPart{}, Schema: testcomparison.Part("app"), Parent: &protocol.ComparisonPart{}},
				Knowledge: testcomparison.Knowledge(protocol.ModelKnowledge_COMPLETE),
			}},
		}}}
	}
	request.CurrentContext.Coverage.Kinds = []*protocol.ComparisonKindCoverage{
		region(schema.ChangefeedProperties, testcomparison.Part("/local")),
		region(schema.TopicProperties, &protocol.ComparisonPart{}),
	}
	return request
}

// A transfer of a topic in a directory the read did not inspect, such as one
// a run's schema selection leaves out, is refused: the plan neither creates
// nor drops the topic, and the comparison cannot say whether it is there.
// The reason says so, rather than that the plan leaves no such topic, and
// asks for the directory to be selected. The topic stays refused: YDB
// accepts a transfer of a missing topic and then stops it. Measured on
// 26.2.1.14 with a master host: the topic outside the selected directory
// existed, and the read's coverage left its directory uninspected.
func TestTransferComparisonRefusesASourceTheReadDidNotInspect_FailurePath(t *testing.T) {
	for _, test := range []struct {
		name   string
		source string
	}{
		{name: "a topic in another directory", source: "ext/events"},
		{name: "a topic at the database root", source: "events"},
		{name: "a stream of a table in another directory", source: "ext/orders/feed"},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)

			result, err := backend.New().CompareModels(c.Context(), selectedRead(c, withSource(test.source)))

			c.Assert(err, qt.ErrorMatches, "rpc error: code = InvalidArgument desc = transfer app/ingest: it reads topic "+test.source+", which the schema "+
				"declares neither as a topic nor as a changefeed, and which is in a directory this run does not read, so the plan cannot tell whether "+
				"it is there, and YDB accepts a transfer of a missing topic and then stops it \\(`Discovery error`\\); select the topic's directory, "+
				"or declare the topic or the changefeed")
			c.Assert(result, qt.IsNil)
		})
	}
}

// A source in the directory the read selected is one the read knows: a topic
// there that the schema does not declare is absent or dropped, and so is a
// stream of a table there that the schema does not declare, whose table's
// changefeeds the read covered though no topic region names the table's path.
// Each is refused as a topic the plan leaves out.
func TestTransferComparisonRefusesASourceTheReadInspected_FailurePath(t *testing.T) {
	for _, test := range []struct {
		name   string
		source string
	}{
		{name: "a topic in the selected directory", source: "app/events"},
		{name: "a stream of an undeclared table in the selected directory", source: "app/orders/feed"},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)

			result, err := backend.New().CompareModels(c.Context(), selectedRead(c, withSource(test.source)))

			c.Assert(err, qt.ErrorMatches, "rpc error: code = InvalidArgument desc = transfer app/ingest: it reads topic "+test.source+", which the schema "+
				"declares neither as a topic nor as a changefeed, so the plan leaves no such topic, and YDB accepts such a transfer and then stops it "+
				"\\(`Discovery error`\\); declare the topic or the changefeed")
			c.Assert(result, qt.IsNil)
		})
	}
}

// A host that does not send the declared topics cannot say a topic is
// undeclared, so a transfer of a topic is compared as before.
func TestTransferComparisonWithoutDeclaredTopics_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	request := transferRelationsRequest(c, tableRelation("", "order_log"))
	request.Desired.Records[0].Value = declaredTransferValue(c, withSource("events"))

	result, err := backend.New().CompareModels(c.Context(), request)

	c.Assert(err, qt.IsNil)
	c.Assert(result.Changes, qt.HasLen, 1)
}

// A target without transfers refuses a declared transfer by the capability,
// before its topic is asked about, as the host's embedded engine refused it on
// 25.1.4.7 (`transfer c2533.ingest, which requires target capability
// transfers`, measured with built-in 74de0b65b).
func TestTransferComparisonRefusesWithoutTransfersFirst_FailurePath(t *testing.T) {
	c := testcheck.New(t)
	request := transferTopicsRequest(c, withSource("events"), nil)
	request.Capabilities[replication.CapabilityTransfers] = false

	result, err := backend.New().CompareModels(c.Context(), request)

	c.Assert(err, qt.ErrorMatches, "rpc error: code = InvalidArgument desc = changing transfers requires transfers.*")
	c.Assert(result, qt.IsNil)
}
