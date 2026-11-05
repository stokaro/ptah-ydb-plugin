package backend_test

import (
	"testing"

	qt "github.com/frankban/quicktest"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/stokaro/ptah/provider-sdk/provider"
	"google.golang.org/protobuf/proto"

	"github.com/stokaro/ptah-ydb-plugin/internal/backend"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcomparison"
	"github.com/stokaro/ptah-ydb-plugin/schema"
)

// limited adds the limit a source header such as
// `-- ptah:not-described topic "app.events"` writes: the object the path
// names, at the database root for a path without a slash, outside the
// source's claim.
func limited(request *protocol.CompareModelsRequest, kind, directory, name string) {
	request.Desired.Coverage.Subjects = append(request.Desired.Coverage.Subjects, &protocol.ComparisonSubjectCoverage{Kind: kind,
		Subject: &protocol.ComparisonIdentity{Kind: kind, Database: &protocol.ComparisonPart{}, Catalog: &protocol.ComparisonPart{},
			Schema: testcomparison.Part(directory), Parent: &protocol.ComparisonPart{}, Name: testcomparison.Part(name)},
		Knowledge: testcomparison.Knowledge(protocol.ModelKnowledge_UNINSPECTED)})
}

// heldAtRoot adds a read topic named name at the database root, beside the
// one topicRequest reads at app/events.
func heldAtRoot(c *qt.C, request *protocol.CompareModelsRequest, name string) {
	c.Helper()
	value, err := schema.CaptureObject(&protocol.Reference{Engine: "ydb", Kind: schema.TopicProperties, Database: "/local", Name: name}, readTopic())
	c.Assert(err, qt.IsNil)
	normalized, err := backend.New().NormalizeValues(c.Context(), &protocol.NormalizeValuesRequest{Engine: "ydb",
		Values: []*protocol.ValueInput{{Id: "root", Representation: protocol.ValueInput_OBSERVED, Value: value}}})
	c.Assert(err, qt.IsNil)
	request.Current.Records = append(request.Current.Records, &protocol.ComparisonRecord{Kind: schema.TopicProperties,
		Subject: &protocol.ComparisonIdentity{Kind: schema.TopicProperties, Database: testcomparison.Part("/local"), Catalog: &protocol.ComparisonPart{},
			Schema: &protocol.ComparisonPart{}, Parent: &protocol.ComparisonPart{}, Name: testcomparison.Part(name)}, Value: normalized.Values[0]})
}

// A limit spelled with a slash, a limit without a dot, and a dotted limit
// that names a topic the database holds are each read as written, as the
// host's embedded engine reads them: the first keeps the topic at
// app/events, the others leave it to the claim, which drops it.
func TestDottedLimits_HappyPath(t *testing.T) {
	for _, test := range []struct {
		name            string
		directory, path string
		roots           []string
		drops, kept     int
	}{
		{name: "a limit with a slash keeps the topic", directory: "app", path: "events", kept: 1},
		{name: "a limit without a dot", path: "events", drops: 1},
		{name: "a dotted limit naming a topic the database holds", path: "app.events", roots: []string{"app.events"}, drops: 1, kept: 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			request := topicRequest(c, nil, readTopic())
			limited(request, schema.TopicProperties, test.directory, test.path)
			for _, name := range test.roots {
				heldAtRoot(c, request, name)
			}
			result, err := backend.New().CompareModels(c.Context(), request)
			c.Assert(err, qt.IsNil)
			c.Assert(provider.ValidateComparedModels(backend.Description(), request, result), qt.IsNil)
			c.Assert(result.Changes, qt.HasLen, test.drops)
			c.Assert(result.Desired.Records, qt.HasLen, test.kept)
		})
	}
}

// A dotted limit that names nothing the database holds, while the plan would
// drop the object the same path written with a slash names, refuses the
// comparison with the spelling that keeps the object, in the host's words.
// The request is not changed.
func TestDottedLimits_FailurePath(t *testing.T) {
	t.Run("a topic", func(t *testing.T) {
		c := testcheck.New(t)
		request := topicRequest(c, nil, readTopic())
		limited(request, schema.TopicProperties, "", "app.events")
		before := proto.CloneOf(request)
		result, err := backend.New().CompareModels(c.Context(), request)
		c.Assert(err, qt.ErrorMatches, `.*the topic limit "app\.events" names app\.events at the database root, which the database does not hold, `+
			`while this plan would drop app/events, which the same name written with a slash names\. Write the limit as "app/events" to keep that topic`)
		c.Assert(result, qt.IsNil)
		c.Assert(proto.Equal(request, before), qt.IsTrue)
	})
	t.Run("a secret", func(t *testing.T) {
		c := testcheck.New(t)
		request := secretRequest(c, true)
		request.Desired.Records = nil
		limited(request, schema.SecretProperties, "", "app.pg.pw")
		result, err := backend.New().CompareModels(c.Context(), request)
		c.Assert(err, qt.ErrorMatches, `.*the secret limit "app\.pg\.pw" names app\.pg\.pw at the database root, .* Write the limit as "app/pg\.pw" to keep that secret`)
		c.Assert(result, qt.IsNil)
	})
	t.Run("an external data source", func(t *testing.T) {
		c := testcheck.New(t)
		request := externalRequest(c, nil, []external{{name: "bucket", source: bucketAt("https://storage.example/bucket/")}}, withSources())
		limited(request, schema.ExternalSourceProperties, "", "app.bucket")
		result, err := backend.New().CompareModels(c.Context(), request)
		c.Assert(err, qt.ErrorMatches, `.*the external data source limit "app\.bucket" names app\.bucket at the database root, .* `+
			`Write the limit as "app/bucket" to keep that external data source`)
		c.Assert(result, qt.IsNil)
	})
}
