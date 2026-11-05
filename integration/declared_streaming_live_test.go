//go:build integration

package integration_test

import (
	"context"
	"os"
	"path"
	"strings"
	"testing"
	"time"

	qt "github.com/frankban/quicktest"
	"github.com/stokaro/ptah-ydb-plugin/internal/backend"
	"github.com/stokaro/ptah-ydb-plugin/internal/declarationplan"
	"github.com/stokaro/ptah-ydb-plugin/internal/nativeapi"
	"github.com/stokaro/ptah-ydb-plugin/internal/streaming"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
	"github.com/stokaro/ptah-ydb-plugin/internal/testplanning"
	topicspec "github.com/stokaro/ptah-ydb-plugin/internal/topics"
	"github.com/stokaro/ptah-ydb-plugin/internal/ydbdecl"
	"github.com/stokaro/ptah-ydb-plugin/schema"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
)

// effectOrdered orders the steps of a declaration plan the way the host's
// plan graph does for them: a step that reads a subject after the step that
// creates it, ties kept in the order the provider listed them.
func effectOrdered(c *qt.C, steps []*protocol.PlanningStep) []*protocol.PlanningStep {
	c.Helper()
	key := func(effect *protocol.PlanningEffect) string {
		return effect.Subject.Kind + "\x00" + effect.Subject.Schema.Normalized + "\x00" + effect.Subject.Name.Normalized
	}
	creator := map[string]string{}
	for _, step := range steps {
		for _, effect := range step.Metadata.Effects {
			if effect.Action == protocol.PlanningEffect_CREATE {
				creator[key(effect)] = step.Id.Name
			}
		}
	}
	var ordered []*protocol.PlanningStep
	done := map[string]bool{}
	for len(ordered) < len(steps) {
		before := len(ordered)
		for _, step := range steps {
			if done[step.Id.Name] {
				continue
			}
			ready := true
			for _, effect := range step.Metadata.Effects {
				if name, created := creator[key(effect)]; effect.Action == protocol.PlanningEffect_READ && created && !done[name] {
					ready = false
				}
			}
			if ready {
				done[step.Id.Name] = true
				ordered = append(ordered, step)
			}
		}
		c.Assert(len(ordered) > before, qt.IsTrue, qt.Commentf("the plan's effects form a cycle"))
	}
	return ordered
}

// Ported from the host's TestYDBStreamingQueries_RenderCreatesTheTopicsFirst:
// a whole schema of two topics and a running query that reads one and writes
// the other is planned as authored declarations with the capabilities the
// live server establishes, and the script runs: YDB compiles the query when
// it creates it, so the query follows both topics. The schema has no table,
// so nothing else orders the owners. The host's `schema render` plans with
// the dialect's preset, which has no streaming queries on either engine, so
// the provider's declaration service is driven here with the session's
// capabilities, as the host's embedded engine was. A run of the host over the
// same schema afterwards plans nothing.
func TestDeclaredStreamingQueryRunsAfterItsTopicsSubprocess(t *testing.T) {
	c := testcheck.New(t)
	opened, directory, database := lockFixture(c)
	_, access := accessSession(c)
	skipWithout(c, access, streaming.Capability)
	c.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		emptyTree(ctx, opened, path.Join(database, directory))
	})
	binary, digest := providerBinary(c)
	client := startProviderListing(c, binary, digest, backend.Description().Features)
	session, err := client.Open(c.Context(), &protocol.OpenRequest{Engine: "ydb", Address: os.Getenv("PTAH_YDB_TEST_URL")})
	c.Assert(err, qt.IsNil)
	c.Cleanup(func() { c.Check(client.CloseSession(context.Background(), session.Id), qt.IsNil) })
	engine := client.Description().Engines[0]
	text := "INSERT INTO `" + directory + "/output` SELECT * FROM `" + directory + "/input`;"
	var values []*protocol.Envelope
	query, err := schema.CaptureStreamingQueryIntent(&protocol.Reference{Engine: "ydb", Kind: schema.StreamingQueryProperties, Schema: directory, Name: "copy"}, streaming.Spec{Text: text}, false)
	c.Assert(err, qt.IsNil)
	values = append(values, query)
	for _, name := range []string{"input", "output"} {
		intent, err := topicspec.Intent(ydbdecl.Topic{})
		c.Assert(err, qt.IsNil)
		topic, err := schema.CaptureTopicIntent(&protocol.Reference{Engine: "ydb", Kind: schema.TopicProperties, Schema: directory, Name: name}, intent)
		c.Assert(err, qt.IsNil)
		values = append(values, topic)
	}
	request := &protocol.PlanDeclarationsRequest{Engine: "ydb", DeclarationId: declarationplan.ServiceID, TargetFeatures: engine.Features,
		Identifiers: &protocol.ComparisonIdentifiers{Rules: engine.Identifiers}, Capabilities: session.Capabilities}
	for _, value := range values {
		normalized, err := client.NormalizeValues(c.Context(), &protocol.NormalizeValuesRequest{Engine: "ydb", TargetFeatures: engine.Features,
			Values: []*protocol.ValueInput{{Id: value.Owner.Name, Representation: protocol.ValueInput_DESIRED, Value: value}}})
		c.Assert(err, qt.IsNil)
		request.Objects = append(request.Objects, &protocol.ComparisonRecord{Kind: value.Kind, Subject: testplanning.Subject(value.Owner), Value: normalized.Values[0]})
	}

	planned, err := client.PlanDeclarations(c.Context(), request)
	c.Assert(err, qt.IsNil)
	c.Assert(planned.Diagnostics, qt.HasLen, 0)
	rendering := testplanning.RenderRequest(client.Description(), &protocol.PlannedFeatures{Contributions: []*protocol.PlanningContribution{
		{Owner: planned.Contributions[0].Owner, Steps: effectOrdered(c, planned.Contributions[0].Steps)}}}, request.Capabilities)
	rendering.Form = protocol.RenderRequest_TEXT
	rendered, err := client.Render(c.Context(), rendering)
	c.Assert(err, qt.IsNil)
	var statements []string
	for _, unit := range rendered.Units {
		statements = append(statements, strings.SplitN(unit.GetSql(), "\n", 2)[0])
	}
	execution, err := client.Execute(c.Context(), &protocol.ExecuteRequest{Engine: "ydb", SessionId: session.Id, RequestId: "script", Units: rendered.Units})
	c.Assert(err, qt.IsNil)

	c.Assert(execution.Outcome, qt.Equals, protocol.Execution_APPLIED, qt.Commentf("%s", execution))
	c.Assert(statements[:2], qt.DeepEquals, []string{"CREATE TOPIC `" + directory + "/input`;", "CREATE TOPIC `" + directory + "/output`;"})
	c.Assert(statements[2], qt.Contains, "CREATE STREAMING QUERY `"+directory+"/copy`")
	held, err := nativeapi.ReadStreamingQuery(c.Context(), opened.DB, database, path.Join(database, directory, "copy"))
	c.Assert(err, qt.IsNil)
	c.Assert(streaming.Running(*held), qt.IsTrue)
	declared := strings.ReplaceAll("topics:\n  input:\n    schema: {dir}\n  output:\n    schema: {dir}\nstreaming_queries:\n  copy:\n    schema: {dir}\n    text: \"", "{dir}", directory) +
		strings.ReplaceAll(text, `"`, `\"`) + "\"\n"
	c.Assert(hostPlanned(c, directory, "yaml", declared), qt.HasLen, 0)
}
