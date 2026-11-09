package backend_test

import (
	"testing"

	qt "github.com/frankban/quicktest"
	"google.golang.org/protobuf/proto"
	protocol "ptah.run/provider-sdk/protocol/experimental"
	"ptah.run/provider-sdk/provider"

	"github.com/stokaro/ptah-ydb-plugin/internal/backend"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
	"github.com/stokaro/ptah-ydb-plugin/schema"
)

// limitOf is a source limit of kind naming name, or the whole kind for an
// empty name.
func limitOf(kind, name string) *protocol.SchemaCoverageLimit {
	return &protocol.SchemaCoverageLimit{Kind: kind, Name: name}
}

// Each family reads the name a limit gives with its own grammar, as the
// host's embedded engine read it: a topic, a secret and a pool keep a dot in
// a segment, a coordination node named by a path keeps the dots of its
// directory and its name, and a pool and a classifier are named exactly, case
// included, at database scope. Ported
// from the host's TestCoordinationLimitsPreserveLiteralPathDots,
// TestGoTopicLimitsReadThePath, TestWorkloadLimitsKeepDatabaseScopeAndExactNames,
// TestGoSourceSecretLimitsBelongToFeatureCoverage,
// TestGoSourceWorkloadLimitsBelongToFeatureCoverage and
// TestGoSecretLimitsReadThePath.
func TestDecodeSource_LimitNames_HappyPath(t *testing.T) {
	queue := schema.CoordinationProperties + " app/queue COMPLETE"
	for _, row := range []struct {
		name   string
		kind   string
		limits []*protocol.SchemaCoverageLimit
		want   []string
	}{
		{name: "coordination nodes named by paths with dots", kind: schema.CoordinationProperties,
			limits: []*protocol.SchemaCoverageLimit{limitOf("coordination_node", "app/locks.v1"), limitOf("coordination_node", "app.v1/locks")},
			want: []string{schema.CoordinationProperties + " COMPLETE", queue, schema.CoordinationProperties + " app/locks.v1 UNINSPECTED",
				schema.CoordinationProperties + " app.v1/locks UNINSPECTED"}},
		{name: "a root coordination node with a dot", kind: schema.CoordinationProperties,
			limits: []*protocol.SchemaCoverageLimit{limitOf("coordination_node", "/locks.v1")},
			want:   []string{schema.CoordinationProperties + " COMPLETE", queue, schema.CoordinationProperties + " /locks.v1 UNINSPECTED"}},
		{name: "topics", kind: schema.TopicProperties,
			limits: []*protocol.SchemaCoverageLimit{limitOf("topic", "events.v1"), limitOf("topic", "app/events")},
			want: []string{schema.TopicProperties + " COMPLETE", queue, schema.TopicProperties + " /events.v1 UNINSPECTED",
				schema.TopicProperties + " app/events UNINSPECTED"}},
		{name: "secrets", kind: schema.SecretProperties,
			limits: []*protocol.SchemaCoverageLimit{limitOf("secret", "ext/pg.pw"), limitOf("secret", "pg.pw"), limitOf("secret", "pg/pw")},
			want: []string{schema.SecretProperties + " COMPLETE", queue, schema.SecretProperties + " ext/pg.pw UNINSPECTED",
				schema.SecretProperties + " /pg.pw UNINSPECTED", schema.SecretProperties + " pg/pw UNINSPECTED"}},
		{name: "pools by exact name", kind: schema.ResourcePoolProperties,
			limits: []*protocol.SchemaCoverageLimit{limitOf("resource_pool", "Batch.jobs"), limitOf("resource_pool", "batch.jobs")},
			want: []string{schema.ResourcePoolProperties + " COMPLETE", queue, schema.ResourcePoolProperties + " /Batch.jobs UNINSPECTED",
				schema.ResourcePoolProperties + " /batch.jobs UNINSPECTED"}},
		{name: "a classifier named like a pool", kind: schema.ResourcePoolClassifierProperties,
			limits: []*protocol.SchemaCoverageLimit{limitOf("resource_pool", "Batch.jobs"), limitOf("resource_pool_classifier", "Batch.jobs")},
			want: []string{schema.ResourcePoolClassifierProperties + " COMPLETE", queue, schema.ResourcePoolProperties + " /Batch.jobs UNINSPECTED",
				schema.ResourcePoolClassifierProperties + " /Batch.jobs UNINSPECTED"}},
		{name: "every classifier", kind: schema.ResourcePoolClassifierProperties,
			limits: []*protocol.SchemaCoverageLimit{limitOf("resource_pool", "batch.jobs"), limitOf("resource_pool_classifier", "")},
			want:   []string{schema.ResourcePoolClassifierProperties + " UNINSPECTED", queue, schema.ResourcePoolProperties + " /batch.jobs UNINSPECTED"}},
	} {
		t.Run(row.name, func(t *testing.T) {
			c := testcheck.New(t)
			request := nodeSource(c, row.limits...)

			decoded, err := backend.New().DecodeSource(limitsContext(c), request)

			c.Assert(err, qt.IsNil)
			c.Assert(decodedKnowledge(decoded.GetDocuments()[0].GetCoverage(), row.kind), qt.DeepEquals, row.want)
		})
	}
}

// unmanagedSubject is an object of kind at directory and name that a
// source's own limit left unmanaged.
func unmanagedSubject(kind, directory, name string) *protocol.ModelSubjectCoverage {
	owner := &protocol.Reference{Engine: "ydb", Kind: kind, Schema: directory, Name: name}
	return &protocol.ModelSubjectCoverage{Kind: kind, Subject: owner, NormalizedSubject: proto.CloneOf(owner),
		Knowledge: &protocol.ModelKnowledge{State: protocol.ModelKnowledge_UNINSPECTED, Reason: "the source leaves this object unmanaged"}}
}

// A source's limits on objects whose names hold dots and capitals are
// written back with the host's spelling, a root node behind a slash so its
// dot stays in its name, and read back as the same objects. Ported from the
// host's TestGoExportPreservesAuthoredSubjectLimitsInEveryFile and
// TestHCLCoordinationSourceLimitsSurviveRepeatedExport.
func TestEncodeSource_LimitNamesRoundTrip(t *testing.T) {
	for _, row := range []struct {
		format   protocol.SourceDocument_Format
		subjects []*protocol.ModelSubjectCoverage
		want     []string
	}{
		{format: protocol.SourceDocument_GO_ANNOTATIONS,
			subjects: []*protocol.ModelSubjectCoverage{
				unmanagedSubject(schema.CoordinationProperties, "", "locks.v1"), unmanagedSubject(schema.CoordinationProperties, "app.v1", "locks"),
				unmanagedSubject(schema.StreamingQueryProperties, "app.v1", "copy"), unmanagedSubject(schema.ResourcePoolProperties, "", "Batch.jobs"),
				unmanagedSubject(schema.ResourcePoolClassifierProperties, "", "route.jobs"), unmanagedSubject(schema.SecretProperties, "ext", "pg.pw"),
				unmanagedSubject(schema.SecretProperties, "", "pg.pw"),
			},
			want: []string{"coordination_node /locks.v1", "coordination_node app.v1/locks", "streaming_query app.v1/copy", "resource_pool Batch.jobs",
				"resource_pool_classifier route.jobs", "secret ext/pg.pw", "secret pg.pw"}},
		{format: protocol.SourceDocument_HCL,
			subjects: []*protocol.ModelSubjectCoverage{
				unmanagedSubject(schema.CoordinationProperties, "", "locks.v1"), unmanagedSubject(schema.CoordinationProperties, "app.v1", "locks"),
			},
			want: []string{"coordination_node /locks.v1", "coordination_node app.v1/locks"}},
	} {
		t.Run(row.format.String(), func(t *testing.T) {
			c := testcheck.New(t)
			request := encodeRequest(row.format, node(c, "queue", "app", "queue", schema.CoordinationIntent{}))
			request.Coverage = &protocol.ModelCoverage{Representation: protocol.ValueInput_DESIRED, Subjects: row.subjects,
				Kinds: []*protocol.ModelKindCoverage{kindKnowledge(schema.CoordinationDesiredModel(), protocol.ModelKnowledge_COMPLETE)}}
			for _, kind := range []string{schema.StreamingQueryProperties, schema.ResourcePoolProperties, schema.ResourcePoolClassifierProperties, schema.SecretProperties} {
				request.Coverage.Kinds = append(request.Coverage.Kinds, kindKnowledge(schema.NativeDesiredModel(kind), protocol.ModelKnowledge_COMPLETE))
			}
			encoded, err := backend.New().EncodeSource(limitsContext(c), request)
			c.Assert(err, qt.IsNil)
			c.Assert(limitTexts(encoded.GetLimits()), qt.DeepEquals, row.want)
			document := sourceDocument(row.format, request, encoded)
			document.Documents[0].Limits = encoded.GetLimits()

			decoded, err := backend.New().DecodeSource(provider.ContextWithHostFeatures(c.Context(), []string{provider.EntryKeysFeature, provider.HCLSourceFeature,
				provider.SourceLimitsFeature}), document)

			c.Assert(err, qt.IsNil)
			c.Assert(subjectTexts(decoded.GetDocuments()[0].GetCoverage().GetSubjects()), qt.DeepEquals,
				append([]string{schema.CoordinationProperties + " app/queue"}, subjectTexts(row.subjects)...))
		})
	}
}

// subjectTexts names each subject by its kind and its path, a root object
// behind a slash.
func subjectTexts(subjects []*protocol.ModelSubjectCoverage) []string {
	var texts []string
	for _, subject := range subjects {
		texts = append(texts, subject.GetKind()+" "+subject.GetSubject().GetSchema()+"/"+subject.GetSubject().GetName())
	}
	return texts
}
