//go:build integration

package integration_test

import (
	"context"
	"os"
	"path"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	qt "github.com/frankban/quicktest"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	providerruntime "github.com/stokaro/ptah/provider-sdk/runtime"
	"google.golang.org/protobuf/proto"

	"github.com/stokaro/ptah-ydb-plugin/internal/secretcompare"
	"github.com/stokaro/ptah-ydb-plugin/internal/secrets"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcomparison"
	"github.com/stokaro/ptah-ydb-plugin/internal/testplanning"
	"github.com/stokaro/ptah-ydb-plugin/internal/topiccompare"
	"github.com/stokaro/ptah-ydb-plugin/schema"
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

// Ported from the host's TestYDBSecrets_LimitNamesADottedRootSecret: a source
// that declares no secret and leaves the root secret <dir>_limit.pw out of
// its claim, a path from the database root as a `ptah:not-described secret`
// header writes it, is compared with a read of the root that holds that
// secret and the root secret <dir>_limit_other. The limit names the dotted
// root path, not pw in a directory <dir>_limit, so the secret is left alone;
// the source still describes every other secret, so the other one is
// dropped. The read covers the root of a server other tests share, so only
// the changes to the test's own secrets are compared. The test needs
// secrets, which 26.2 has and 25.1 does not.
func TestDottedLimitNamesARootSecretSubprocess(t *testing.T) {
	c := testcheck.New(t)
	opened, directory, database := inspectionFixture(c)
	client := startProviderFor(c)
	session, err := client.Open(c.Context(), &protocol.OpenRequest{Engine: "ydb", Address: os.Getenv("PTAH_YDB_TEST_URL")})
	c.Assert(err, qt.IsNil)
	c.Cleanup(func() { c.Check(client.CloseSession(context.Background(), session.Id), qt.IsNil) })
	skipWithout(c, session, secrets.Capability)
	kept, other := directory+"_limit.pw", directory+"_limit_other"
	for _, name := range []string{kept, other} {
		absolute := path.Join(database, name)
		c.Assert(execFixtureSchema(c.Context(), opened.DB, "CREATE SECRET `"+absolute+"` WITH (value = 'probe');"), qt.IsNil)
		c.Cleanup(func() {
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()
			c.Check(execFixtureSchema(ctx, opened.DB, "DROP SECRET `"+absolute+"`;"), qt.IsNil)
		})
	}
	snapshot, err := client.Inspect(c.Context(), &protocol.InspectRequest{SessionId: session.Id, Schemas: []string{"/"}})
	c.Assert(err, qt.IsNil)
	desired := testcomparison.State(schema.NativeDesiredModel(schema.SecretProperties))
	desired.Coverage.Subjects = []*protocol.ComparisonSubjectCoverage{{Kind: schema.SecretProperties,
		Subject:   testplanning.Subject(&protocol.Reference{Kind: schema.SecretProperties, Name: kept}),
		Knowledge: testcomparison.Knowledge(protocol.ModelKnowledge_UNINSPECTED)}}
	request := &protocol.CompareModelsRequest{Engine: "ydb", TargetFeatures: negotiated(client), DatabasePath: session.Database, ComparisonId: secretcompare.ServiceID,
		Kinds: []string{schema.SecretProperties}, Capabilities: session.Capabilities,
		Identifiers: &protocol.ComparisonIdentifiers{Rules: proto.CloneOf(client.Description().Engines[0].Identifiers)}, Desired: desired,
		Current: &protocol.ComparisonState{Representation: protocol.ValueInput_OBSERVED, Coverage: capturedCoverage(snapshot.Coverage, schema.SecretProperties),
			Records: observedRecords(c, client, snapshot, schema.SecretProperties)}}

	compared, err := client.CompareModels(c.Context(), request)

	c.Assert(err, qt.IsNil)
	c.Assert(changedNames(compared, kept, other), qt.DeepEquals, []string{other})
	c.Assert(compared.Desired.Records, qt.HasLen, 1)
	c.Assert(compared.Desired.Records[0].Subject.GetName().GetSource(), qt.Equals, kept)
}

// observedRecords are the comparison records of the read's native objects of
// kind, each normalized by the provider as the host normalizes it, under a
// value id of its own.
func observedRecords(c *qt.C, client *providerruntime.Client, snapshot *protocol.Schema, kind string) []*protocol.ComparisonRecord {
	c.Helper()
	var records []*protocol.ComparisonRecord
	for i, value := range snapshot.GetNativeObjects() {
		if value.GetKind() != kind {
			continue
		}
		normalized, err := client.NormalizeValues(c.Context(), &protocol.NormalizeValuesRequest{Engine: "ydb", TargetFeatures: negotiated(client),
			Values: []*protocol.ValueInput{{Id: "read-" + strconv.Itoa(i), Representation: protocol.ValueInput_OBSERVED, Value: value}}})
		c.Assert(err, qt.IsNil)
		records = append(records, &protocol.ComparisonRecord{Kind: value.GetKind(), Subject: testplanning.Subject(value.GetOwner()), Value: normalized.GetValues()[0]})
	}
	return records
}

// changedNames are the names of the subjects a comparison changes, among the
// names given, in the comparison's order.
func changedNames(compared *protocol.ComparedModels, names ...string) []string {
	var changed []string
	for _, change := range compared.GetChanges() {
		if slices.Contains(names, change.GetSubject().GetName().GetSource()) {
			changed = append(changed, change.GetSubject().GetName().GetSource())
		}
	}
	return changed
}
