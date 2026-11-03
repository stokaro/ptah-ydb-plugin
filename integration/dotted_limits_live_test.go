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
	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcomparison"
	"github.com/stokaro/ptah-ydb-plugin/internal/testplanning"
	"github.com/stokaro/ptah-ydb-plugin/internal/topiccompare"
	"github.com/stokaro/ptah-ydb-plugin/schema"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"google.golang.org/protobuf/proto"
)

// limitedTopicComparison creates the topic events in the fixture directory,
// reads the directory through the provider, and compares the read with a
// source that declares no topic and describes every topic but the one limit
// names, a path from the database root as a `ptah:not-described topic` header
// writes it, with {dir} standing for the fixture directory.
func limitedTopicComparison(c *qt.C, limit string) (*protocol.ComparedModels, error) {
	c.Helper()
	opened, directory, database := inspectionFixture(c)
	absolute := path.Join(database, directory, "events")
	c.Assert(execFixtureSchema(c.Context(), opened.DB, "CREATE TOPIC `"+absolute+"`"), qt.IsNil)
	c.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		c.Check(execFixtureSchema(ctx, opened.DB, "DROP TOPIC `"+absolute+"`"), qt.IsNil)
	})
	binary, digest := providerBinary(c)
	client := startProvider(c, binary, digest)
	session, err := client.Open(c.Context(), &protocol.OpenRequest{Engine: "ydb", Address: os.Getenv("PTAH_YDB_TEST_URL")})
	c.Assert(err, qt.IsNil)
	c.Cleanup(func() { c.Check(client.CloseSession(context.Background(), session.Id), qt.IsNil) })
	snapshot, err := client.Inspect(c.Context(), &protocol.InspectRequest{SessionId: session.Id, Schemas: []string{directory}})
	c.Assert(err, qt.IsNil)
	desired := testcomparison.State(schema.NativeDesiredModel(schema.TopicProperties))
	limitDirectory, limitName := path.Split(strings.ReplaceAll(limit, "{dir}", directory))
	desired.Coverage.Subjects = []*protocol.ComparisonSubjectCoverage{{Kind: schema.TopicProperties,
		Subject:   testplanning.Subject(&protocol.Reference{Kind: schema.TopicProperties, Schema: strings.TrimSuffix(limitDirectory, "/"), Name: limitName}),
		Knowledge: testcomparison.Knowledge(protocol.ModelKnowledge_UNINSPECTED)}}
	request := &protocol.CompareModelsRequest{Engine: "ydb", TargetFeatures: negotiated(client), DatabasePath: session.Database, ComparisonId: topiccompare.ServiceID,
		Kinds: []string{schema.TopicProperties}, Capabilities: session.Capabilities,
		Identifiers: &protocol.ComparisonIdentifiers{Rules: proto.CloneOf(client.Description().Engines[0].Identifiers)}, Desired: desired,
		Current: &protocol.ComparisonState{Representation: protocol.ValueInput_OBSERVED, Coverage: capturedTopicCoverage(snapshot.Coverage)}}
	for _, value := range snapshot.NativeObjects {
		normalized, err := client.NormalizeValues(c.Context(), &protocol.NormalizeValuesRequest{Engine: "ydb", TargetFeatures: negotiated(client),
			Values: []*protocol.ValueInput{{Id: "read", Representation: protocol.ValueInput_OBSERVED, Value: value}}})
		c.Assert(err, qt.IsNil)
		request.Current.Records = append(request.Current.Records, &protocol.ComparisonRecord{Kind: value.Kind, Subject: testplanning.Subject(value.Owner), Value: normalized.Values[0]})
	}
	return client.CompareModels(c.Context(), request)
}

// Ported from the host's TestYDBTopics_ALimitWithASlashKeepsTheTopic: a
// limit that names the topic by its path keeps the topic as the server holds
// it.
func TestDottedLimitWithASlashKeepsTheTopicSubprocess(t *testing.T) {
	c := testcheck.New(t)

	compared, err := limitedTopicComparison(c, "{dir}/events")

	c.Assert(err, qt.IsNil)
	c.Assert(compared.Changes, qt.HasLen, 0)
	c.Assert(compared.Desired.Records, qt.HasLen, 1)
}

// Ported from the host's
// TestYDBTopics_FailurePath_ADottedLimitBesideItsDirectoryForm: a limit
// "<dir>.events" names no topic the database holds while <dir>/events is
// read and undeclared. Compared, the source would drop the topic it most
// likely means to keep, so the comparison is refused. The SDK hands a caller
// a refusal's code and not its words, which TestDottedLimits_FailurePath
// pins.
//
// The host leaves a limit outside the directories `--schemas` selects out of
// the comparison it asks for, so this limit, at the database root, reaches the
// provider only from a run that selects the root.
func TestDottedLimitBesideItsDirectoryFormIsRefusedSubprocess(t *testing.T) {
	c := testcheck.New(t)

	compared, err := limitedTopicComparison(c, "{dir}.events")

	c.Assert(err, qt.ErrorMatches, `provider compare models failed \(InvalidArgument\)`)
	c.Assert(compared, qt.IsNil)
}
