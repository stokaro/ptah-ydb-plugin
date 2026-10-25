//go:build integration

package integration_test

import (
	"context"
	"os"
	"path"
	"testing"
	"time"

	qt "github.com/frankban/quicktest"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcomparison"
	"github.com/stokaro/ptah-ydb-plugin/internal/testplanning"
	"github.com/stokaro/ptah-ydb-plugin/internal/topiccompare"
	"github.com/stokaro/ptah-ydb-plugin/internal/topicplan"
	topicspec "github.com/stokaro/ptah-ydb-plugin/internal/topics"
	"github.com/stokaro/ptah-ydb-plugin/internal/ydbdecl"
	"github.com/stokaro/ptah-ydb-plugin/schema"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/stokaro/ptah/provider-sdk/provider"
	"github.com/ydb-platform/ydb-go-sdk/v3/topic/topictypes"
	"google.golang.org/protobuf/proto"
)

// TestTopicLifecycleSubprocess drives a topic through the provider's
// services the way the host does, and reads the topic back from the server
// after each step: a creation, a consumer YDB can change only by dropping it
// and adding it again, a change another writer made after the plan read the
// topic, and a removal.
func TestTopicLifecycleSubprocess(t *testing.T) {
	c := qt.New(t)
	opened, directory, database := inspectionFixture(c)
	ctx, cancel := context.WithTimeout(t.Context(), 4*time.Minute)
	defer cancel()
	absolute := path.Join(database, directory, "events")
	c.Cleanup(func() {
		cleanup, stop := context.WithTimeout(context.Background(), 30*time.Second)
		defer stop()
		_ = execFixtureSchema(cleanup, opened.DB, "DROP TOPIC `"+absolute+"`")
	})
	binary, digest := providerBinary(c)
	client := startProvider(c, binary, digest)
	session, err := client.Open(ctx, &protocol.OpenRequest{Engine: "ydb", Address: os.Getenv("PTAH_YDB_TEST_URL")})
	c.Assert(err, qt.IsNil)
	c.Cleanup(func() { c.Check(client.CloseSession(context.Background(), session.Id), qt.IsNil) })
	c.Assert(session.Capabilities["topics"], qt.IsTrue)
	rules := proto.CloneOf(client.Description().Engines[0].Identifiers)

	// plan compares the declaration, nil for none, with what the directory
	// holds and renders the one change that follows.
	plan := func(declared *ydbdecl.Topic) []*protocol.Unit {
		c.Helper()
		snapshot, err := client.Inspect(ctx, &protocol.InspectRequest{SessionId: session.Id, Schemas: []string{directory}})
		c.Assert(err, qt.IsNil)
		desiredModel := schema.NativeDesiredModel(schema.TopicProperties)
		request := &protocol.CompareModelsRequest{Engine: "ydb", ComparisonId: topiccompare.ServiceID, Kinds: []string{schema.TopicProperties}, Capabilities: session.Capabilities,
			Identifiers: &protocol.ComparisonIdentifiers{Rules: rules}, Desired: testcomparison.State(desiredModel),
			Current: &protocol.ComparisonState{Representation: protocol.ValueInput_OBSERVED, Coverage: capturedTopicCoverage(snapshot.Coverage)}}
		if declared != nil {
			intent, err := topicspec.Intent(*declared)
			c.Assert(err, qt.IsNil)
			value, err := schema.CaptureTopicIntent(&protocol.Reference{Engine: "ydb", Kind: schema.TopicProperties, Schema: directory, Name: "events"}, intent)
			c.Assert(err, qt.IsNil)
			normalized, err := client.NormalizeValues(ctx, &protocol.NormalizeValuesRequest{Engine: "ydb", Values: []*protocol.ValueInput{{Id: "declared", Representation: protocol.ValueInput_DESIRED, Value: value}}})
			c.Assert(err, qt.IsNil)
			subject := testplanning.Subject(value.Owner)
			request.Desired.Records = []*protocol.ComparisonRecord{{Kind: schema.TopicProperties, Subject: subject, Value: normalized.Values[0]}}
		}
		for _, value := range snapshot.NativeObjects {
			if value.Kind != schema.TopicProperties {
				continue
			}
			normalized, err := client.NormalizeValues(ctx, &protocol.NormalizeValuesRequest{Engine: "ydb", Values: []*protocol.ValueInput{{Id: "read", Representation: protocol.ValueInput_OBSERVED, Value: value}}})
			c.Assert(err, qt.IsNil)
			request.Current.Records = append(request.Current.Records, &protocol.ComparisonRecord{Kind: schema.TopicProperties, Subject: testplanning.Subject(value.Owner), Value: normalized.Values[0]})
		}
		compared, err := client.CompareModels(ctx, request)
		c.Assert(err, qt.IsNil)
		c.Assert(compared.Undecided, qt.HasLen, 0)
		if len(compared.Changes) == 0 {
			return nil
		}
		c.Assert(compared.Changes, qt.HasLen, 1)
		planned, err := client.PlanFeatures(ctx, &protocol.PlanFeaturesRequest{Engine: "ydb", PlanningId: topicplan.ServiceID, Capabilities: session.Capabilities,
			Identifiers: &protocol.ComparisonIdentifiers{Rules: rules}, Changes: []*protocol.PlanningChange{{Subject: compared.Changes[0].Subject, Value: compared.Changes[0].Value}}})
		c.Assert(err, qt.IsNil)
		c.Assert(planned.Diagnostics, qt.HasLen, 0)
		rendered, err := client.Render(ctx, testplanning.RenderRequest(planned, session.Capabilities))
		c.Assert(err, qt.IsNil)
		return rendered.Units
	}
	execute := func(expected protocol.Execution_Outcome, units ...*protocol.Unit) {
		c.Helper()
		result, err := client.Execute(ctx, &protocol.ExecuteRequest{SessionId: session.Id, RequestId: "topic", Units: units})
		if expected == protocol.Execution_APPLIED {
			c.Assert(err, qt.IsNil)
		} else {
			c.Assert(err, qt.ErrorIs, provider.ErrExecution)
		}
		c.Assert(result.Outcome, qt.Equals, expected)
	}
	describe := func() topictypes.TopicDescription {
		c.Helper()
		description, err := opened.Driver.Topic().Describe(ctx, absolute)
		c.Assert(err, qt.IsNil)
		return description
	}

	created := ydbdecl.Topic{MinActivePartitions: 2, RetentionPeriod: "PT36H", Consumers: []ydbdecl.Consumer{{Name: "worker", SupportedCodecs: []string{"raw"}}}}
	execute(protocol.Execution_APPLIED, plan(&created)...)
	description := describe()
	c.Assert(description.PartitionSettings.MinActivePartitions, qt.Equals, int64(2))
	c.Assert(description.RetentionPeriod, qt.Equals, 36*time.Hour)
	c.Assert(description.Consumers, qt.HasLen, 1)
	c.Assert(description.Consumers[0].SupportedCodecs, qt.DeepEquals, []topictypes.Codec{topictypes.CodecRaw})
	c.Assert(plan(&created), qt.HasLen, 0)

	// No statement empties a consumer's codec list, so the consumer is
	// dropped and added again in a second statement of the same unit.
	restarted := ydbdecl.Topic{MinActivePartitions: 2, RetentionPeriod: "PT36H", Consumers: []ydbdecl.Consumer{{Name: "worker"}}}
	execute(protocol.Execution_APPLIED, plan(&restarted)...)
	description = describe()
	c.Assert(description.Consumers, qt.HasLen, 1)
	c.Assert(description.Consumers[0].SupportedCodecs, qt.HasLen, 0)
	c.Assert(plan(&restarted), qt.HasLen, 0)

	// A change planned against a topic another writer changed since is
	// refused before any statement runs.
	longer := ydbdecl.Topic{MinActivePartitions: 2, RetentionPeriod: "P2D", Consumers: []ydbdecl.Consumer{{Name: "worker"}}}
	stale := plan(&longer)
	c.Assert(execFixtureSchema(ctx, opened.DB, "ALTER TOPIC `"+absolute+"` SET (retention_period = Interval('P3D'))"), qt.IsNil)
	execute(protocol.Execution_NOT_STARTED, stale...)
	c.Assert(describe().RetentionPeriod, qt.Equals, 72*time.Hour)
	execute(protocol.Execution_APPLIED, plan(&longer)...)
	c.Assert(describe().RetentionPeriod, qt.Equals, 48*time.Hour)

	execute(protocol.Execution_APPLIED, plan(nil)...)
	listing, err := opened.Driver.Scheme().ListDirectory(ctx, path.Join(database, directory))
	c.Assert(err, qt.IsNil)
	c.Assert(listing.Children, qt.HasLen, 0)
}

// capturedTopicCoverage transfers an inspection's topic coverage into the
// comparison's terms, as the host does: the regions keep the database the read
// names, and the subjects keep theirs.
func capturedTopicCoverage(coverage *protocol.ModelCoverage) *protocol.ComparisonCoverage {
	result := &protocol.ComparisonCoverage{}
	part := func(value *protocol.NamespaceComponent) *protocol.ComparisonPart {
		if value == nil {
			return nil
		}
		return &protocol.ComparisonPart{Source: value.Source, Normalized: value.Normalized}
	}
	for _, kind := range coverage.Kinds {
		if kind.Model.Kind != schema.TopicProperties {
			continue
		}
		source := &protocol.ComparisonKnowledgeSource{Outside: proto.CloneOf(kind.Knowledge)}
		for _, region := range kind.Regions {
			source.Regions = append(source.Regions, &protocol.ComparisonRegion{Scope: &protocol.ComparisonNamespace{Database: part(region.Scope.Database), Catalog: part(region.Scope.Catalog), Schema: part(region.Scope.Schema)},
				Knowledge: proto.CloneOf(region.Knowledge)})
		}
		result.Kinds = append(result.Kinds, &protocol.ComparisonKindCoverage{Model: proto.CloneOf(kind.Model), Sources: []*protocol.ComparisonKnowledgeSource{source}})
	}
	for _, subject := range coverage.Subjects {
		if subject.Kind == schema.TopicProperties {
			result.Subjects = append(result.Subjects, &protocol.ComparisonSubjectCoverage{Kind: subject.Kind, Subject: testplanning.Subject(subject.NormalizedSubject), Knowledge: proto.CloneOf(subject.Knowledge)})
		}
	}
	return result
}
