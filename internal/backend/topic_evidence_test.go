package backend_test

import (
	"testing"

	qt "github.com/frankban/quicktest"
	protocol "ptah.run/provider-sdk/protocol/experimental"
	"ptah.run/provider-sdk/provider"

	"github.com/stokaro/ptah-ydb-plugin/internal/backend"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcomparison"
	"github.com/stokaro/ptah-ydb-plugin/internal/topiccompare"
	"github.com/stokaro/ptah-ydb-plugin/internal/topics"
	"github.com/stokaro/ptah-ydb-plugin/internal/ydbdecl"
	"github.com/stokaro/ptah-ydb-plugin/schema"
)

// topicComparison is a comparison of topics, empty on both sides, each side
// describing the kind completely.
func topicComparison() *protocol.CompareModelsRequest {
	return objectComparison(schema.TopicProperties, topiccompare.ServiceID, schema.NativeDesiredModel(schema.TopicProperties), map[string]bool{"topics": true})
}

// topicDeclaredAt declares the topic at dir/name with spec.
func topicDeclaredAt(c *qt.C, dir, name string, spec ydbdecl.Topic) *protocol.Envelope {
	c.Helper()
	intent, err := topics.Intent(spec)
	c.Assert(err, qt.IsNil)
	value, err := schema.CaptureTopicIntent(&protocol.Reference{Engine: "ydb", Kind: schema.TopicProperties, Schema: dir, Name: name}, intent)
	c.Assert(err, qt.IsNil)
	return value
}

// topicReadAt is readTopic at /local/dir/name.
func topicReadAt(c *qt.C, dir, name string) *protocol.Envelope {
	c.Helper()
	value, err := schema.CaptureObject(&protocol.Reference{Engine: "ydb", Kind: schema.TopicProperties, Database: "/local", Schema: dir, Name: name}, readTopic())
	c.Assert(err, qt.IsNil)
	return value
}

// Ported from the host's TestTopicComparison_ComparesResolvedSettingsAndEvidence:
// a topic the read could not read at its path is never dropped or created
// over by a source that claims it: its removal and its creation are
// undecided.
func TestTopicComparisonReadsAnUnreadTopicThroughTheClaim(t *testing.T) {
	for _, test := range []struct {
		name               string
		claim              protocol.ModelKnowledge_State
		declared           []ydbdecl.Topic
		undecided, records int
	}{
		{name: "an unread topic withholds its removal", claim: protocol.ModelKnowledge_COMPLETE, undecided: 1},
		{name: "an unread topic withholds the creation", claim: protocol.ModelKnowledge_COMPLETE, declared: []ydbdecl.Topic{{}}, undecided: 1, records: 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			request := topicComparison()
			request.Desired.Coverage.Kinds[0].Sources[0].Outside = testcomparison.Knowledge(test.claim)
			limitSubject(request.Current, schema.TopicProperties, "/local", "app", "events", protocol.ModelKnowledge_UNINSPECTED)
			for _, spec := range test.declared {
				addObject(c, request.Desired, topicDeclaredAt(c, "app", "events", spec))
			}

			result, err := backend.New().CompareModels(c.Context(), request)

			c.Assert(err, qt.IsNil)
			c.Assert(provider.ValidateComparedModels(backend.Description(), request, result), qt.IsNil)
			c.Assert(result.Changes, qt.HasLen, 0)
			c.Assert(result.Undecided, qt.HasLen, test.undecided)
			c.Assert(result.Desired.Records, qt.HasLen, test.records)
		})
	}
}

// Ported from the host's TestCompare_TopicNotCreatedWhereTheReadDidNotLook:
// a declared topic the read could not read at its path, such as a queue
// group, is not created, since CREATE TOPIC carries no guard and would fail
// over a topic that is there; the comparison says so, and still creates a
// declared topic the read did look for.
func TestTopicComparisonCreatesOnlyWhereTheReadLooked(t *testing.T) {
	c := testcheck.New(t)
	request := topicComparison()
	limitSubject(request.Current, schema.TopicProperties, "/local", "app", "events", protocol.ModelKnowledge_UNINSPECTED)
	addObject(c, request.Desired, topicDeclaredAt(c, "app", "events", ydbdecl.Topic{}))
	addObject(c, request.Desired, topicDeclaredAt(c, "app", "queue", ydbdecl.Topic{}))

	result, err := backend.New().CompareModels(c.Context(), request)

	c.Assert(err, qt.IsNil)
	c.Assert(changedPaths(result), qt.DeepEquals, []string{"app/queue ADDITIVE"})
	c.Assert(result.Undecided, qt.HasLen, 1)
	c.Assert(result.Undecided[0].Subject.Name.Normalized, qt.Equals, "events")
}

// Ported from the host's TestTopicComparison_FailurePath_CoverageSubjects: a
// source's coverage the topic comparison cannot read is refused with no
// result: a topic it calls defaulted, which no topic is, and one whose
// directory is absolute rather than a path in the database.
func TestTopicComparisonRefusesCoverageItCannotRead(t *testing.T) {
	for _, test := range []struct {
		name      string
		dir       string
		knowledge protocol.ModelKnowledge_State
	}{
		{name: "a default topic", dir: "app", knowledge: protocol.ModelKnowledge_DEFAULTED},
		{name: "an absolute directory", dir: "/app", knowledge: protocol.ModelKnowledge_UNINSPECTED},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			request := topicComparison()
			limitSubject(request.Desired, schema.TopicProperties, "", test.dir, "events", test.knowledge)

			result, err := backend.New().CompareModels(c.Context(), request)

			c.Assert(err, qt.IsNotNil)
			c.Assert(result, qt.IsNil)
		})
	}
}
