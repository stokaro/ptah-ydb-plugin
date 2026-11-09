package backend_test

import (
	"context"
	"slices"
	"testing"

	qt "github.com/frankban/quicktest"
	"google.golang.org/protobuf/proto"
	protocol "ptah.run/provider-sdk/protocol/experimental"
	"ptah.run/provider-sdk/provider"

	"github.com/stokaro/ptah-ydb-plugin/internal/backend"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
	"github.com/stokaro/ptah-ydb-plugin/schema"
)

// limitsContext is the context of a call from a host that lists source.limits
// beside the source features.
func limitsContext(c *qt.C) context.Context {
	return provider.ContextWithHostFeatures(c.Context(), []string{provider.EntryKeysFeature, provider.HCLSourceFeature, provider.SourceLimitsFeature})
}

// directory is the namespace of one directory of the database /local, as a
// read reports the directories it lists.
func directory(name string) *protocol.ModelNamespace {
	return &protocol.ModelNamespace{Database: &protocol.NamespaceComponent{Source: "/local", Normalized: "/local"}, Catalog: &protocol.NamespaceComponent{},
		Schema: &protocol.NamespaceComponent{Source: name, Normalized: name}}
}

// wholeDatabase is the namespace of the database /local, as a read reports a
// kind it knows in every directory.
func wholeDatabase() *protocol.ModelNamespace {
	return &protocol.ModelNamespace{Database: &protocol.NamespaceComponent{Source: "/local", Normalized: "/local"}}
}

func knowledge(state protocol.ModelKnowledge_State) *protocol.ModelKnowledge {
	reasons := map[protocol.ModelKnowledge_State]string{protocol.ModelKnowledge_UNINSPECTED: "Knowledge is limited to returned subjects and fully listed directories"}
	return &protocol.ModelKnowledge{State: state, Reason: reasons[state]}
}

// kindKnowledge is what a request knows of model: the knowledge outside its
// regions, and each region complete.
func kindKnowledge(model *protocol.ValueModel, outside protocol.ModelKnowledge_State, regions ...*protocol.ModelNamespace) *protocol.ModelKindCoverage {
	kind := &protocol.ModelKindCoverage{Model: model, Knowledge: knowledge(outside)}
	for _, region := range regions {
		kind.Regions = append(kind.Regions, &protocol.ModelNamespaceKnowledge{Scope: region, Knowledge: knowledge(protocol.ModelKnowledge_COMPLETE)})
	}
	return kind
}

// limitTexts writes each limit as the header entry it is.
func limitTexts(limits []*protocol.SchemaCoverageLimit) []string {
	var texts []string
	for _, limit := range limits {
		texts = append(texts, limit.GetKind()+" "+limit.GetName())
	}
	return texts
}

// unmanagedNode is the subject of the node app/legacy, which a request knows
// with state.
func unmanagedNode(state protocol.ModelKnowledge_State) *protocol.ModelSubjectCoverage {
	owner := &protocol.Reference{Engine: "ydb", Kind: schema.CoordinationProperties, Schema: "app", Name: "legacy"}
	return &protocol.ModelSubjectCoverage{Kind: schema.CoordinationProperties, Subject: owner, NormalizedSubject: proto.CloneOf(owner), Knowledge: knowledge(state)}
}

func TestEncodeSource_WritesTheKnowledgeItLacks_HappyPath(t *testing.T) {
	for _, format := range []protocol.SourceDocument_Format{protocol.SourceDocument_GO_ANNOTATIONS, protocol.SourceDocument_HCL} {
		for _, row := range []struct {
			name     string
			kinds    []*protocol.ModelKindCoverage
			subjects []*protocol.ModelSubjectCoverage
			want     []string
		}{
			{name: "a kind known in one directory only", kinds: []*protocol.ModelKindCoverage{
				kindKnowledge(schema.CoordinationDesiredModel(), protocol.ModelKnowledge_UNINSPECTED, directory("app"))}, want: []string{"coordination_node "}},
			{name: "a kind known in some directories only", kinds: []*protocol.ModelKindCoverage{
				kindKnowledge(schema.CoordinationDesiredModel(), protocol.ModelKnowledge_UNINSPECTED, directory("app"), directory("other"))}, want: []string{"coordination_node "}},
			{name: "a kind no region knows", kinds: []*protocol.ModelKindCoverage{
				kindKnowledge(schema.CoordinationDesiredModel(), protocol.ModelKnowledge_UNINSPECTED)}, want: []string{"coordination_node "}},
			{name: "a region that knows less than the rest", kinds: []*protocol.ModelKindCoverage{{Model: schema.CoordinationDesiredModel(),
				Knowledge: knowledge(protocol.ModelKnowledge_COMPLETE),
				Regions:   []*protocol.ModelNamespaceKnowledge{{Scope: directory("app"), Knowledge: knowledge(protocol.ModelKnowledge_UNINSPECTED)}}}},
				want: []string{"coordination_node "}},
			{name: "a kind known in its database only", kinds: []*protocol.ModelKindCoverage{
				kindKnowledge(schema.CoordinationDesiredModel(), protocol.ModelKnowledge_UNINSPECTED, wholeDatabase())}, want: []string{"coordination_node "}},
			{name: "complete knowledge in regions", kinds: []*protocol.ModelKindCoverage{
				kindKnowledge(schema.CoordinationDesiredModel(), protocol.ModelKnowledge_COMPLETE, directory("app"))}, want: []string{"coordination_node "}},
			{name: "a kind a narrowed read knows in every directory it listed", kinds: []*protocol.ModelKindCoverage{
				kindKnowledge(schema.CoordinationDesiredModel(), protocol.ModelKnowledge_COMPLETE)}},
			{name: "an object the request does not know", kinds: []*protocol.ModelKindCoverage{
				kindKnowledge(schema.CoordinationDesiredModel(), protocol.ModelKnowledge_COMPLETE)},
				subjects: []*protocol.ModelSubjectCoverage{unmanagedNode(protocol.ModelKnowledge_UNINSPECTED)}, want: []string{"coordination_node app/legacy"}},
			{name: "an object of a kind the source leaves unmanaged", kinds: []*protocol.ModelKindCoverage{
				kindKnowledge(schema.CoordinationDesiredModel(), protocol.ModelKnowledge_UNINSPECTED, directory("app"))},
				subjects: []*protocol.ModelSubjectCoverage{unmanagedNode(protocol.ModelKnowledge_UNINSPECTED)}, want: []string{"coordination_node "}},
			{name: "a known object", kinds: []*protocol.ModelKindCoverage{
				kindKnowledge(schema.CoordinationDesiredModel(), protocol.ModelKnowledge_COMPLETE)},
				subjects: []*protocol.ModelSubjectCoverage{unmanagedNode(protocol.ModelKnowledge_COMPLETE)}},
		} {
			t.Run(format.String()+"/"+row.name, func(t *testing.T) {
				c := testcheck.New(t)
				request := encodeRequest(format)
				request.Coverage = &protocol.ModelCoverage{Representation: protocol.ValueInput_DESIRED, Kinds: row.kinds, Subjects: row.subjects}

				encoded, err := backend.New().EncodeSource(limitsContext(c), request)

				c.Assert(err, qt.IsNil)
				c.Assert(limitTexts(encoded.GetLimits()), qt.DeepEquals, row.want)
			})
		}
	}
}

// TestEncodeSource_WritesEveryNativeKindItLacks covers each kind a Go header
// names. A read knows every one in the directories it listed only, and a
// table's facets for the tables it read, which the source declares.
func TestEncodeSource_WritesEveryNativeKindItLacks(t *testing.T) {
	c := testcheck.New(t)
	request := encodeRequest(protocol.SourceDocument_GO_ANNOTATIONS)
	request.Coverage = &protocol.ModelCoverage{Representation: protocol.ValueInput_DESIRED}
	for _, kind := range []string{schema.SecretProperties, schema.TopicProperties, schema.ExternalSourceProperties, schema.ExternalTableProperties,
		schema.ResourcePoolProperties, schema.ResourcePoolClassifierProperties, schema.StreamingQueryProperties, schema.ReplicationProperties, schema.TransferProperties} {
		request.Coverage.Kinds = append(request.Coverage.Kinds, kindKnowledge(schema.NativeDesiredModel(kind), protocol.ModelKnowledge_UNINSPECTED, directory("app")))
	}
	request.Coverage.Kinds = append(request.Coverage.Kinds, kindKnowledge(schema.CoordinationDesiredModel(), protocol.ModelKnowledge_UNINSPECTED, directory("app")),
		kindKnowledge(schema.TableDesiredModel(), protocol.ModelKnowledge_UNINSPECTED))
	table := &protocol.Reference{Engine: "ydb", Kind: "table", Schema: "app", Name: "events"}
	request.Bindings = []*protocol.SourceBinding{{Id: "events", EntryKey: "events", Object: &protocol.Reference{Engine: "ydb", Kind: "table", Database: "/local", Schema: "app", Name: "events"}}}
	request.Coverage.Subjects = []*protocol.ModelSubjectCoverage{{Kind: schema.TableProperties, Subject: table, NormalizedSubject: proto.CloneOf(table),
		Knowledge: knowledge(protocol.ModelKnowledge_COMPLETE)}}

	encoded, err := backend.New().EncodeSource(limitsContext(c), request)

	c.Assert(err, qt.IsNil)
	c.Assert(limitTexts(encoded.GetLimits()), qt.DeepEquals, []string{"secret ", "topic ", "external_data_source ", "external_table ", "resource_pool ",
		"resource_pool_classifier ", "streaming_query ", "replication ", "transfer ", "coordination_node "})
}

func TestEncodeSource_WritesTheKnowledgeItLacks_FailurePath(t *testing.T) {
	narrowed := []*protocol.ModelKindCoverage{kindKnowledge(schema.CoordinationDesiredModel(), protocol.ModelKnowledge_UNINSPECTED, directory("app"))}
	table := &protocol.Reference{Engine: "ydb", Kind: "table", Schema: "app", Name: "events"}
	for _, row := range []struct {
		name     string
		format   protocol.SourceDocument_Format
		host     []string
		kinds    []*protocol.ModelKindCoverage
		subjects []*protocol.ModelSubjectCoverage
		want     string
	}{
		{name: "a host without source.limits", format: protocol.SourceDocument_GO_ANNOTATIONS, host: []string{provider.EntryKeysFeature, provider.HCLSourceFeature},
			kinds: narrowed, want: `.*YDB source encoding cannot preserve incomplete coverage of .*/coordination-node.*`},
		{name: "an HCL source for a host without source.limits", format: protocol.SourceDocument_HCL, host: []string{provider.EntryKeysFeature, provider.HCLSourceFeature},
			kinds: narrowed, want: `.*YDB source encoding cannot preserve incomplete coverage of .*/coordination-node.*`},
		{name: "a format that writes no header", format: protocol.SourceDocument_YAML, kinds: narrowed,
			host: []string{provider.EntryKeysFeature, provider.HCLSourceFeature, provider.SourceLimitsFeature},
			want: `.*YDB source encoding cannot preserve incomplete coverage of .*/coordination-node.*`},
		{name: "an unread object in a format that writes no header", format: protocol.SourceDocument_YAML,
			host:     []string{provider.EntryKeysFeature, provider.HCLSourceFeature, provider.SourceLimitsFeature},
			kinds:    []*protocol.ModelKindCoverage{kindKnowledge(schema.CoordinationDesiredModel(), protocol.ModelKnowledge_COMPLETE)},
			subjects: []*protocol.ModelSubjectCoverage{unmanagedNode(protocol.ModelKnowledge_UNINSPECTED)},
			want:     `.*YDB source encoding cannot preserve incomplete coverage of .*/coordination-node app/legacy.*`},
		{name: "a declared table whose settings the request does not know", format: protocol.SourceDocument_GO_ANNOTATIONS,
			host:  []string{provider.EntryKeysFeature, provider.HCLSourceFeature, provider.SourceLimitsFeature},
			kinds: append([]*protocol.ModelKindCoverage{kindKnowledge(schema.TableDesiredModel(), protocol.ModelKnowledge_UNINSPECTED)}, narrowed...),
			subjects: []*protocol.ModelSubjectCoverage{{Kind: schema.TableProperties, Subject: table, NormalizedSubject: proto.CloneOf(table),
				Knowledge: knowledge(protocol.ModelKnowledge_UNINSPECTED)}},
			want: `.*YDB source encoding cannot preserve incomplete coverage of .*/row-table-properties of table app/events.*`},
	} {
		t.Run(row.name, func(t *testing.T) {
			c := testcheck.New(t)
			request := encodeRequest(row.format)
			request.Bindings = []*protocol.SourceBinding{{Id: "events", EntryKey: "events", Object: &protocol.Reference{Engine: "ydb", Kind: "table", Database: "/local", Schema: "app", Name: "events"}}}
			request.Coverage = &protocol.ModelCoverage{Representation: protocol.ValueInput_DESIRED, Kinds: row.kinds, Subjects: row.subjects}

			encoded, err := backend.New().EncodeSource(provider.ContextWithHostFeatures(c.Context(), row.host), request)

			c.Assert(err, qt.ErrorMatches, row.want)
			c.Assert(encoded, qt.IsNil)
		})
	}
}

// TestEncodeSource_ReadBackClaimsNoMoreThanTheRequest decodes what an export of
// a read of one directory wrote, with its header: the node the read held is
// declared and known, and the kind is unmanaged, so a node of another
// directory is not read as absent.
func TestEncodeSource_ReadBackClaimsNoMoreThanTheRequest(t *testing.T) {
	for _, format := range []protocol.SourceDocument_Format{protocol.SourceDocument_GO_ANNOTATIONS, protocol.SourceDocument_HCL} {
		t.Run(format.String(), func(t *testing.T) {
			c := testcheck.New(t)
			request := encodeRequest(format, node(c, "queue", "app", "queue", schema.CoordinationIntent{}))
			request.Coverage = &protocol.ModelCoverage{Representation: protocol.ValueInput_DESIRED,
				Kinds: []*protocol.ModelKindCoverage{kindKnowledge(schema.CoordinationDesiredModel(), protocol.ModelKnowledge_UNINSPECTED, directory("app"))}}
			encoded, err := backend.New().EncodeSource(limitsContext(c), request)
			c.Assert(err, qt.IsNil)
			document := sourceDocument(format, request, encoded)
			document.Documents[0].Limits = encoded.GetLimits()

			decoded, err := backend.New().DecodeSource(limitsContext(c), document)

			c.Assert(err, qt.IsNil)
			c.Assert(provider.ValidateDecodedSource(provider.NegotiatedDescription(backend.Description(), []string{provider.EntryKeysFeature, provider.HCLSourceFeature, provider.SourceLimitsFeature}),
				document, decoded), qt.IsNil)
			coverage := decoded.GetDocuments()[0].GetCoverage()
			states := make(map[string]string)
			for _, kind := range coverage.GetKinds() {
				states[kind.GetModel().GetKind()] = kind.GetKnowledge().GetState().String() + " " + kind.GetKnowledge().GetReason()
			}
			c.Assert(states[schema.CoordinationProperties], qt.Equals, "UNINSPECTED the source leaves coordination nodes unmanaged")
			c.Assert(coverage.GetSubjects(), qt.HasLen, 1)
			c.Assert(coverage.GetSubjects()[0].GetSubject().GetSchema()+"/"+coverage.GetSubjects()[0].GetSubject().GetName(), qt.Equals, "app/queue")
			c.Assert(coverage.GetSubjects()[0].GetKnowledge().GetState(), qt.Equals, protocol.ModelKnowledge_COMPLETE)
		})
	}
}

// decodedKnowledge writes what a decoding knows: each enrolled model of kinds
// with its state, and each subject with its path and state.
func decodedKnowledge(coverage *protocol.ModelCoverage, kinds ...string) []string {
	var texts []string
	for _, kind := range coverage.GetKinds() {
		if slices.Contains(kinds, kind.GetModel().GetKind()) {
			texts = append(texts, kind.GetModel().GetKind()+" "+kind.GetKnowledge().GetState().String())
		}
	}
	for _, subject := range coverage.GetSubjects() {
		texts = append(texts, subject.GetKind()+" "+subject.GetSubject().GetSchema()+"/"+subject.GetSubject().GetName()+" "+subject.GetKnowledge().GetState().String())
	}
	return texts
}

// nodeSource is the Go source of the node app/queue, with limits as its
// header, as the host hands it to the provider.
func nodeSource(c *qt.C, limits ...*protocol.SchemaCoverageLimit) *protocol.DecodeSourceRequest {
	c.Helper()
	request := encodeRequest(protocol.SourceDocument_GO_ANNOTATIONS, node(c, "queue", "app", "queue", schema.CoordinationIntent{}))
	encoded, err := backend.New().EncodeSource(limitsContext(c), request)
	c.Assert(err, qt.IsNil)
	document := sourceDocument(protocol.SourceDocument_GO_ANNOTATIONS, request, encoded)
	document.Documents[0].Limits = limits
	return document
}

func TestDecodeSource_LimitedClaim_HappyPath(t *testing.T) {
	queue := schema.CoordinationProperties + " app/queue COMPLETE"
	for _, row := range []struct {
		name   string
		host   []string
		limits []*protocol.SchemaCoverageLimit
		want   []string
	}{
		{name: "a whole kind", host: []string{provider.SourceLimitsFeature}, limits: []*protocol.SchemaCoverageLimit{{Kind: "topic"}},
			want: []string{schema.TopicProperties + " UNINSPECTED", schema.CoordinationProperties + " COMPLETE", queue}},
		{name: "one object", host: []string{provider.SourceLimitsFeature}, limits: []*protocol.SchemaCoverageLimit{{Kind: "coordination_node", Name: "app/legacy"}},
			want: []string{schema.TopicProperties + " COMPLETE", schema.CoordinationProperties + " COMPLETE", queue, schema.CoordinationProperties + " app/legacy UNINSPECTED"}},
		{name: "an object named twice", host: []string{provider.SourceLimitsFeature},
			limits: []*protocol.SchemaCoverageLimit{{Kind: "coordination_node", Name: "app/legacy"}, {Kind: "coordination_node", Name: "app.legacy"}},
			want:   []string{schema.TopicProperties + " COMPLETE", schema.CoordinationProperties + " COMPLETE", queue, schema.CoordinationProperties + " app/legacy UNINSPECTED"}},
		{name: "a host without source.limits", limits: []*protocol.SchemaCoverageLimit{{Kind: "topic"}},
			want: []string{schema.TopicProperties + " COMPLETE", schema.CoordinationProperties + " COMPLETE", queue}},
	} {
		t.Run(row.name, func(t *testing.T) {
			c := testcheck.New(t)
			request := nodeSource(c, row.limits...)

			decoded, err := backend.New().DecodeSource(provider.ContextWithHostFeatures(c.Context(), row.host), request)

			c.Assert(err, qt.IsNil)
			c.Assert(decodedKnowledge(decoded.GetDocuments()[0].GetCoverage(), schema.TopicProperties, schema.CoordinationProperties), qt.DeepEquals, row.want)
		})
	}
}

func TestDecodeSource_LimitedClaim_FailurePath(t *testing.T) {
	for _, row := range []struct {
		name   string
		limits []*protocol.SchemaCoverageLimit
		want   string
	}{
		{name: "an object the source declares", limits: []*protocol.SchemaCoverageLimit{{Kind: "coordination_node", Name: "app/queue"}},
			want: `.*the source leaves coordination_node "app/queue" unmanaged and declares it.*`},
		{name: "a name no object can take", limits: []*protocol.SchemaCoverageLimit{{Kind: "topic", Name: "/absolute/topic"}},
			want: `.*coverage of github.com/stokaro/ptah-ydb-plugin: invalid feature value: topic limit: .*`},
	} {
		t.Run(row.name, func(t *testing.T) {
			c := testcheck.New(t)
			request := nodeSource(c, row.limits...)

			decoded, err := backend.New().DecodeSource(limitsContext(c), request)

			c.Assert(err, qt.ErrorMatches, row.want)
			c.Assert(decoded, qt.IsNil)
		})
	}
}

// TestEncodeSource_SaysWhyItLacksKnowledge writes, on each entry, where the
// missing knowledge comes from: a read that did not inspect it, or a source
// whose own entry left the kind unmanaged.
func TestEncodeSource_SaysWhyItLacksKnowledge(t *testing.T) {
	for _, row := range []struct {
		name   string
		reason string
		want   string
	}{
		{name: "a read", reason: "Knowledge is limited to returned subjects and fully listed directories", want: "coordination_node not-inspected observed"},
		{name: "a source that leaves the kind unmanaged", reason: "the source leaves coordination nodes unmanaged", want: "coordination_node  declared"},
	} {
		t.Run(row.name, func(t *testing.T) {
			c := testcheck.New(t)
			request := encodeRequest(protocol.SourceDocument_GO_ANNOTATIONS)
			request.Coverage = &protocol.ModelCoverage{Representation: protocol.ValueInput_DESIRED, Kinds: []*protocol.ModelKindCoverage{{Model: schema.CoordinationDesiredModel(),
				Knowledge: &protocol.ModelKnowledge{State: protocol.ModelKnowledge_UNINSPECTED, Reason: row.reason}}}}

			encoded, err := backend.New().EncodeSource(limitsContext(c), request)

			c.Assert(err, qt.IsNil)
			c.Assert(encoded.GetLimits(), qt.HasLen, 1)
			c.Assert(encoded.GetLimits()[0].GetKind()+" "+encoded.GetLimits()[0].GetReason()+" "+encoded.GetLimits()[0].GetProvenance(), qt.Equals, row.want)
		})
	}
}
