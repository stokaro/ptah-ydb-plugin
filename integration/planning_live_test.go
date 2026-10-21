//go:build integration

package integration_test

import (
	"context"
	"encoding/json"
	"os"
	"path"
	"testing"
	"time"

	qt "github.com/frankban/quicktest"
	"github.com/stokaro/ptah-ydb-plugin/internal/coordinationcompare"
	"github.com/stokaro/ptah-ydb-plugin/internal/coordinationplan"
	"github.com/stokaro/ptah-ydb-plugin/internal/testplanning"
	"github.com/stokaro/ptah-ydb-plugin/internal/testsource"
	"github.com/stokaro/ptah-ydb-plugin/schema"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/stokaro/ptah/provider-sdk/provider"
	"github.com/ydb-platform/ydb-go-genproto/Ydb_Coordination_V1"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb_Coordination"
	ydb "github.com/ydb-platform/ydb-go-sdk/v3"
	"google.golang.org/protobuf/proto"
)

// capturedCoordinationCoverage transfers the source's exact namespace bounds.
// This fixture enrolls only coordination nodes; it does not implement scheduling
// or the host's general coverage binding policy.
func capturedCoordinationCoverage(coverage *protocol.ModelCoverage, database string) *protocol.ComparisonCoverage {
	result := &protocol.ComparisonCoverage{}
	bindDatabase := func(part *protocol.ComparisonPart) *protocol.ComparisonPart {
		if part != nil && part.Normalized == "" && coverage.Representation == protocol.ValueInput_DESIRED {
			return &protocol.ComparisonPart{Source: database, Normalized: database, Defaulted: true}
		}
		return part
	}
	part := func(value *protocol.NamespaceComponent) *protocol.ComparisonPart {
		if value == nil {
			return nil
		}
		return &protocol.ComparisonPart{Source: value.Source, Normalized: value.Normalized}
	}
	for _, kind := range coverage.Kinds {
		if kind.Model.Kind != schema.CoordinationProperties {
			continue
		}
		source := &protocol.ComparisonKnowledgeSource{Outside: proto.CloneOf(kind.Knowledge)}
		for _, region := range kind.Regions {
			scope := &protocol.ComparisonNamespace{Database: bindDatabase(part(region.Scope.Database)), Catalog: part(region.Scope.Catalog), Schema: part(region.Scope.Schema)}
			for _, component := range region.Scope.PathPrefix {
				scope.PathPrefix = append(scope.PathPrefix, part(component))
			}
			source.Regions = append(source.Regions, &protocol.ComparisonRegion{Scope: scope, Knowledge: proto.CloneOf(region.Knowledge)})
		}
		result.Kinds = append(result.Kinds, &protocol.ComparisonKindCoverage{Model: proto.CloneOf(kind.Model), Sources: []*protocol.ComparisonKnowledgeSource{source}})
	}
	for _, subject := range coverage.Subjects {
		if subject.Kind == schema.CoordinationProperties {
			identity := testplanning.Subject(subject.NormalizedSubject)
			identity.Database = bindDatabase(identity.Database)
			result.Subjects = append(result.Subjects, &protocol.ComparisonSubjectCoverage{Kind: subject.Kind, Subject: identity, Knowledge: proto.CloneOf(subject.Knowledge)})
		}
	}
	return result
}

func TestCoordinationPlanningExecutionSubprocess(t *testing.T) {
	c := qt.New(t)
	opened, directory, database := inspectionFixture(c)
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	native := Ydb_Coordination_V1.NewCoordinationServiceClient(ydb.GRPCConn(opened.Driver))
	absolute := path.Join(database, directory, "node")
	// Clean up after an assertion too; DropNode on absence is intentionally ignored.
	c.Cleanup(func() {
		cleanup, stop := context.WithTimeout(context.Background(), 15*time.Second)
		defer stop()
		_, _ = native.DropNode(cleanup, &Ydb_Coordination.DropNodeRequest{Path: absolute})
	})
	binary, digest := providerBinary(c)
	client := startProvider(c, binary, digest)
	session, err := client.Open(ctx, &protocol.OpenRequest{Engine: "ydb", Address: os.Getenv("PTAH_YDB_TEST_URL")})
	c.Assert(err, qt.IsNil)
	c.Cleanup(func() { c.Check(client.CloseSession(context.Background(), session.Id), qt.IsNil) })
	c.Assert(session.Capabilities["coordination_nodes"], qt.IsTrue)
	c.Assert(session.Capabilities["transactional_ddl"], qt.IsFalse)

	compare := func(mode string, drop bool) *protocol.ComparedModels {
		c.Helper()
		nodes := map[string]any{}
		if !drop {
			fields := map[string]any{"schema": directory}
			if mode != "" {
				fields["read_consistency_mode"] = mode
			}
			nodes["node"] = fields
		}
		data, err := json.Marshal(map[string]any{"coordination_nodes": nodes})
		c.Assert(err, qt.IsNil)
		source, err := testsource.Request(string(data))
		c.Assert(err, qt.IsNil)
		decoded, err := client.DecodeSource(ctx, source)
		c.Assert(err, qt.IsNil)
		snapshot, err := client.Inspect(ctx, &protocol.InspectRequest{SessionId: session.Id, Schemas: []string{directory}})
		c.Assert(err, qt.IsNil)
		request := &protocol.CompareModelsRequest{Engine: "ydb", ComparisonId: coordinationcompare.ServiceID, Kinds: []string{schema.CoordinationProperties}, DatabasePath: database, Capabilities: session.Capabilities, Identifiers: &protocol.ComparisonIdentifiers{Rules: proto.CloneOf(client.Description().Engines[0].Identifiers)}, Desired: &protocol.ComparisonState{Representation: protocol.ValueInput_DESIRED, Coverage: capturedCoordinationCoverage(decoded.Documents[0].Coverage, database)}, Current: &protocol.ComparisonState{Representation: protocol.ValueInput_OBSERVED, Coverage: capturedCoordinationCoverage(snapshot.Coverage, database)}}
		for _, value := range decoded.Documents[0].Fragments[0].Values {
			owner := proto.CloneOf(value.Value.NormalizedOwner)
			owner.Database = database
			subject := testplanning.Subject(owner)
			subject.Database.Defaulted = true
			request.Desired.Records = append(request.Desired.Records, &protocol.ComparisonRecord{Kind: schema.CoordinationProperties, Subject: subject, Value: value.Value})
		}
		for _, value := range snapshot.NativeObjects {
			if value.Kind != schema.CoordinationProperties {
				continue
			}
			normalized, err := client.NormalizeValues(ctx, &protocol.NormalizeValuesRequest{Engine: "ydb", Values: []*protocol.ValueInput{{Id: "observed", Representation: protocol.ValueInput_OBSERVED, Value: value}}})
			c.Assert(err, qt.IsNil)
			request.Current.Records = append(request.Current.Records, &protocol.ComparisonRecord{Kind: schema.CoordinationProperties, Subject: testplanning.Subject(value.Owner), Value: normalized.Values[0]})
		}
		result, err := client.CompareModels(ctx, request)
		c.Assert(err, qt.IsNil)
		// The exact selected directory is known; the rest of the database is not.
		for _, undecided := range result.Undecided {
			c.Assert(undecided.Subject, qt.IsNil)
		}
		return result
	}
	render := func(compared *protocol.ComparedModels) *protocol.Unit {
		c.Helper()
		c.Assert(compared.Changes, qt.HasLen, 1)
		change := compared.Changes[0]
		request := &protocol.PlanFeaturesRequest{Engine: "ydb", PlanningId: coordinationplan.ServiceID, DatabasePath: database, Capabilities: session.Capabilities, Identifiers: &protocol.ComparisonIdentifiers{Rules: proto.CloneOf(client.Description().Engines[0].Identifiers)}, Changes: []*protocol.PlanningChange{{Subject: change.Subject, Value: change.Value}}}
		plan, err := client.PlanFeatures(ctx, request)
		c.Assert(err, qt.IsNil)
		c.Assert(plan.Diagnostics, qt.HasLen, 0)
		c.Assert(plan.Contributions[0].Steps[0].Metadata.Transaction, qt.Equals, protocol.PlanningMetadata_FORBIDDEN)
		rendered, err := client.Render(ctx, testplanning.RenderRequest(plan, session.Capabilities))
		c.Assert(err, qt.IsNil)
		c.Assert(rendered.Units, qt.HasLen, 1)
		c.Assert(rendered.Units[0].GetNative(), qt.IsNotNil)
		return rendered.Units[0]
	}
	execute := func(transaction string, expected protocol.Execution_Outcome, count uint32, units ...*protocol.Unit) {
		c.Helper()
		result, err := client.Execute(ctx, &protocol.ExecuteRequest{SessionId: session.Id, TransactionId: transaction, RequestId: "native", Units: units})
		if expected == protocol.Execution_APPLIED {
			c.Assert(err, qt.IsNil)
		} else {
			c.Assert(err, qt.ErrorIs, provider.ErrExecution)
		}
		c.Assert(result.Outcome, qt.Equals, expected)
		c.Assert(result.AppliedCount, qt.Equals, count)
	}
	create := render(compare("strict", false))
	execute("", protocol.Execution_APPLIED, 1, create)
	c.Assert(compare("strict", false).Changes, qt.HasLen, 0)
	execute("", protocol.Execution_NOT_STARTED, 0, create)
	// Capture a reset, then alter the server outside the provider before applying.
	reset := render(compare("", false))
	changed, err := native.AlterNode(ctx, &Ydb_Coordination.AlterNodeRequest{Path: absolute, Config: &Ydb_Coordination.Config{SessionGracePeriodMillis: 12000}})
	c.Assert(err, qt.IsNil)
	c.Assert(changed.GetOperation().GetStatus(), qt.Equals, Ydb.StatusIds_SUCCESS)
	execute("", protocol.Execution_NOT_STARTED, 0, reset)
	execute("", protocol.Execution_APPLIED, 1, render(compare("", false)))
	c.Assert(compare("", false).Changes, qt.HasLen, 0)
	execute("", protocol.Execution_APPLIED, 1, render(compare("strict", false)))
	c.Assert(compare("strict", false).Changes, qt.HasLen, 0)

	// Native transaction refusal must prevent an otherwise valid SQL prefix.
	table := path.Join(database, directory, "prefix")
	_, err = opened.DB.ExecContext(ctx, "CREATE TABLE `"+table+"` (id Uint64 NOT NULL, PRIMARY KEY(id))")
	c.Assert(err, qt.IsNil)
	c.Cleanup(func() {
		_, err := opened.DB.ExecContext(context.Background(), "DROP TABLE `"+table+"`")
		c.Check(err, qt.IsNil)
	})
	insert := &protocol.Unit{ChangeId: "insert", Body: &protocol.Unit_Sql{Sql: "UPSERT INTO `" + table + "` (id) VALUES (1u)"}}
	handle, err := client.BeginTransaction(ctx, &protocol.BeginTransactionRequest{SessionId: session.Id, TransactionId: "native-forbidden", Isolation: protocol.TransactionIsolation_SERIALIZABLE})
	c.Assert(err, qt.IsNil)
	execute(handle.Id, protocol.Execution_NOT_STARTED, 0, insert, create)
	committed, err := client.FinishTransaction(ctx, &protocol.FinishTransactionRequest{SessionId: session.Id, TransactionId: handle.Id, Action: protocol.FinishTransactionRequest_COMMIT})
	c.Assert(err, qt.IsNil)
	c.Assert(committed.Outcome, qt.Equals, protocol.TransactionResult_COMMITTED)
	assertRowCount(c, opened, table, 0)
	foreign := proto.CloneOf(create)
	foreign.GetNative().Owner.Database = "/foreign"
	execute("", protocol.Execution_NOT_STARTED, 0, insert, foreign)
	assertRowCount(c, opened, table, 0)
	malformed := proto.CloneOf(create)
	malformed.GetNative().Payload = []byte(`{"before":null,"after":{"bad":true}}`)
	execute("", protocol.Execution_NOT_STARTED, 0, insert, malformed)
	assertRowCount(c, opened, table, 0)
	execute("", protocol.Execution_PARTIAL, 1, insert, create)
	assertRowCount(c, opened, table, 1)
	execute("", protocol.Execution_APPLIED, 1, render(compare("", true)))
	c.Assert(compare("", true).Changes, qt.HasLen, 0)
}
