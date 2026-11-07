package backend_test

import (
	"testing"

	qt "github.com/frankban/quicktest"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/stokaro/ptah/provider-sdk/provider"
	"google.golang.org/protobuf/proto"

	"github.com/stokaro/ptah-ydb-plugin/internal/backend"
	native "github.com/stokaro/ptah-ydb-plugin/internal/nativeprotocol"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcomparison"
	"github.com/stokaro/ptah-ydb-plugin/internal/ydbdecl"
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
		// Ported from the host's TestTopicComparison_ALimitReadAsAPath.
		{name: "a leading dot names no directory", path: ".events", roots: []string{"events"}, drops: 2},
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
	// Ported from the host's
	// TestTopicComparison_FailurePath_ADottedLimitBesideItsDirectoryForm.
	t.Run("a topic of a nested directory", func(t *testing.T) {
		c := testcheck.New(t)
		request := topicRequest(c, nil, nil)
		addObject(c, request.Current, topicReadAt(c, "a/b", "events"))
		limited(request, schema.TopicProperties, "a", "b.events")
		result, err := backend.New().CompareModels(c.Context(), request)
		c.Assert(err, qt.ErrorMatches, `.*the topic limit "a/b\.events" names a/b\.events at the database root, .* Write the limit as "a/b/events" to keep that topic`)
		c.Assert(result, qt.IsNil)
	})
	t.Run("a secret", func(t *testing.T) {
		c := testcheck.New(t)
		request := secretRequest(c, secretPresent)
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

// limitedAs adds what the source knows of the object of kind the path name
// names at the database root, as a source header writes it.
func limitedAs(request *protocol.CompareModelsRequest, kind, name string, knowledge protocol.ModelKnowledge_State) {
	limitSubject(request.Desired, kind, "", "", name, knowledge)
}

// Ported from the host's TestTopicComparison_ADottedLimitBesideAPlannedTopic:
// beside a dotted limit only a planned drop of the object its directory form
// names is refused. The creation and the change of the declared topic at that
// path are planned, and so is its drop where the source says the root topic
// the limit names is absent rather than limiting it.
func TestDottedLimitsBesideAPlannedTopic_HappyPath(t *testing.T) {
	for _, test := range []struct {
		name      string
		knowledge protocol.ModelKnowledge_State
		declared  []ydbdecl.Topic
		read      []*native.DescribeTopicResult
		want      []string
	}{
		{name: "a creation", knowledge: protocol.ModelKnowledge_UNINSPECTED, declared: []ydbdecl.Topic{{RetentionPeriod: "PT3H"}}, want: []string{"app/events ADDITIVE"}},
		{name: "a change", knowledge: protocol.ModelKnowledge_UNINSPECTED, declared: []ydbdecl.Topic{{RetentionPeriod: "PT3H", Consumers: []ydbdecl.Consumer{{Name: "worker"}}}},
			read: []*native.DescribeTopicResult{readTopic()},
			want: []string{"app/events BEHAVIORAL"}},
		{name: "an absent root topic", knowledge: protocol.ModelKnowledge_ABSENT, read: []*native.DescribeTopicResult{readTopic()}, want: []string{"app/events DESTRUCTIVE"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			request := topicRequest(c, nil, nil)
			for _, spec := range test.declared {
				addObject(c, request.Desired, topicDeclaredAt(c, "app", "events", spec))
			}
			for range test.read {
				addObject(c, request.Current, topicReadAt(c, "app", "events"))
			}
			limitedAs(request, schema.TopicProperties, "app.events", test.knowledge)

			result, err := backend.New().CompareModels(c.Context(), request)

			c.Assert(err, qt.IsNil)
			c.Assert(provider.ValidateComparedModels(backend.Description(), request, result), qt.IsNil)
			c.Assert(changedPaths(result), qt.DeepEquals, test.want)
		})
	}
}

// Ported from the host's TestSecretComparison_ADottedLimitBesideADeclaredSecret:
// the declared secret app/pg.pw is created beside the limit "app.pg.pw", which
// names a root secret the database does not hold: only a planned drop beside
// a dotted limit is refused.
func TestDottedLimitBesideADeclaredSecret_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	request := secretRequest(c, secretAbsent)
	limited(request, schema.SecretProperties, "", "app.pg.pw")

	result, err := backend.New().CompareModels(c.Context(), request)

	c.Assert(err, qt.IsNil)
	c.Assert(changedPaths(result), qt.DeepEquals, []string{"app/pg.pw ADDITIVE"})
}
