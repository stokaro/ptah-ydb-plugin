package streamingsource_test

import (
	"context"
	"testing"

	qt "github.com/frankban/quicktest"
	"github.com/stokaro/ptah-ydb-plugin/internal/streaming"
	"github.com/stokaro/ptah-ydb-plugin/internal/streamingsource"
	"github.com/stokaro/ptah-ydb-plugin/schema"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
)

func text(value string) *protocol.SourceValue {
	return &protocol.SourceValue{Value: &protocol.SourceValue_Text{Text: value}}
}

func boolean(value bool) *protocol.SourceValue {
	return &protocol.SourceValue{Value: &protocol.SourceValue_Boolean{Boolean: value}}
}

func number(value string) *protocol.SourceValue {
	return &protocol.SourceValue{Value: &protocol.SourceValue_Number{Number: value}}
}

func null() *protocol.SourceValue {
	return &protocol.SourceValue{Value: &protocol.SourceValue_Null{Null: &protocol.Empty{}}}
}

func entry(key string, value *protocol.SourceValue) *protocol.SourceEntry {
	return &protocol.SourceEntry{Key: key, Value: value}
}

func mapping(entries ...*protocol.SourceEntry) *protocol.SourceValue {
	return &protocol.SourceValue{Value: &protocol.SourceValue_Mapping{Mapping: &protocol.SourceMapping{Entries: entries}}}
}

func yamlDocument(value *protocol.SourceValue) *protocol.SourceDocument {
	claim := streamingsource.YAMLKey()
	return &protocol.SourceDocument{Id: "doc", Format: protocol.SourceDocument_YAML,
		Fragments: []*protocol.SourceFragment{{Id: "queries", Selector: claim.Selector, YamlKey: claim, Content: &protocol.SourceFragment_Value{Value: value}}}}
}

func goDocument(values ...*protocol.SourceValue) *protocol.SourceDocument {
	claim := streamingsource.GoDirectives()[0]
	document := &protocol.SourceDocument{Id: "doc", Format: protocol.SourceDocument_GO_ANNOTATIONS}
	for index, value := range values {
		document.Fragments = append(document.Fragments, &protocol.SourceFragment{Id: string(rune('a' + index)), Selector: claim.Selector,
			GoDirective: claim, Content: &protocol.SourceFragment_Value{Value: value}})
	}
	return document
}

// The body is the host's 055-ydb-streaming-queries declaration. The models
// retain exact text and distinguish omitted RUN from explicit false.
func TestDecodeDeclarations(t *testing.T) {
	body := "INSERT INTO stream_output SELECT * FROM stream_input;"
	for _, tc := range []struct {
		name     string
		document *protocol.SourceDocument
	}{
		{"YAML", yamlDocument(mapping(entry("holder", mapping(entry("name", text("stream_copy")), entry("schema", text("app/jobs")),
			entry("text", text(body)), entry("run", boolean(false)), entry("resource_pool", text("batch")), entry("allow_state_reset", boolean(true))))))},
		{"Go", goDocument(mapping(entry("name", text("stream_copy")), entry("schema", text("app/jobs")), entry("text", text(body)),
			entry("run", text("false")), entry("resource_pool", text("batch")), entry("allow_state_reset", text("true"))))},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := qt.New(t)
			values, err := streamingsource.Decode(c.Context(), tc.document)
			c.Assert(err, qt.IsNil)
			c.Assert(len(values), qt.Equals, 1)
			c.Assert(len(values[0]), qt.Equals, 1)
			value := values[0][0]
			c.Assert(value.Placement, qt.Equals, protocol.SourceModelValue_NATIVE_OBJECT)
			c.Assert(value.Value.Value.Owner.Schema, qt.Equals, "app/jobs")
			c.Assert(value.Value.Value.Owner.Name, qt.Equals, "stream_copy")
			spec, allow, err := schema.DesiredStreamingQuery(value.Value.Value)
			c.Assert(err, qt.IsNil)
			c.Assert(spec, qt.DeepEquals, streaming.Spec{Text: body, Run: new(false), ResourcePool: "batch"})
			c.Assert(allow, qt.IsTrue)
			c.Assert(value.Source.DocumentId, qt.Equals, "doc")
			c.Assert(value.Source.FragmentId, qt.Equals, tc.document.Fragments[0].Id)
			c.Assert(value.Source.Root, qt.Equals, protocol.SourceTrace_CONTENT)
			if tc.name == "YAML" {
				c.Assert(value.Source.Path[0].GetKey(), qt.Equals, "holder")
			} else {
				c.Assert(len(value.Source.Path), qt.Equals, 0)
			}
		})
	}
}

func TestDecodeDefaultsAndKeyOrder(t *testing.T) {
	c := qt.New(t)
	document := yamlDocument(mapping(entry("z", mapping(entry("text", text("SELECT 1;")))),
		entry("a", mapping(entry("text", text("SELECT 2;")), entry("run", null()), entry("allow_state_reset", null()), entry("resource_pool", number("10"))))))
	values, err := streamingsource.Decode(c.Context(), document)
	c.Assert(err, qt.IsNil)
	c.Assert(len(values[0]), qt.Equals, 2)
	c.Assert(values[0][0].Value.Value.Owner.Name, qt.Equals, "a")
	c.Assert(values[0][1].Value.Value.Owner.Name, qt.Equals, "z")
	c.Assert(values[0][0].Value.Id, qt.Not(qt.Equals), values[0][1].Value.Id)
	spec, allow, err := schema.DesiredStreamingQuery(values[0][0].Value.Value)
	c.Assert(err, qt.IsNil)
	c.Assert(spec, qt.DeepEquals, streaming.Spec{Text: "SELECT 2;", ResourcePool: "10"})
	c.Assert(allow, qt.IsFalse)
	for _, empty := range []*protocol.SourceValue{mapping(), null()} {
		values, err := streamingsource.Decode(c.Context(), yamlDocument(empty))
		c.Assert(err, qt.IsNil)
		c.Assert(len(values), qt.Equals, 1)
		c.Assert(len(values[0]), qt.Equals, 0)
	}
}

func TestDecodeBooleanGrammars(t *testing.T) {
	for _, tc := range []struct {
		name   string
		format protocol.SourceDocument_Format
		value  *protocol.SourceValue
		want   bool
	}{
		{"YAML true", protocol.SourceDocument_YAML, boolean(true), true},
		{"YAML false", protocol.SourceDocument_YAML, boolean(false), false},
		{"YAML yes", protocol.SourceDocument_YAML, text("Yes"), true},
		{"YAML off", protocol.SourceDocument_YAML, text("OFF"), false},
		{"Go one", protocol.SourceDocument_GO_ANNOTATIONS, text("1"), true},
		{"Go false", protocol.SourceDocument_GO_ANNOTATIONS, text("FALSE"), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := qt.New(t)
			fields := mapping(entry("name", text("q")), entry("text", text("SELECT 1;")), entry("run", tc.value), entry("allow_state_reset", tc.value))
			document := goDocument(fields)
			if tc.format == protocol.SourceDocument_YAML {
				document = yamlDocument(mapping(entry("q", fields)))
			}
			values, err := streamingsource.Decode(c.Context(), document)
			c.Assert(err, qt.IsNil)
			spec, allow, err := schema.DesiredStreamingQuery(values[0][0].Value.Value)
			c.Assert(err, qt.IsNil)
			c.Assert(*spec.Run, qt.Equals, tc.want)
			c.Assert(allow, qt.Equals, tc.want)
		})
	}
}

func TestDecodeRefusesInvalidDeclarations(t *testing.T) {
	valid := func(extra ...*protocol.SourceEntry) *protocol.SourceValue {
		return mapping(append([]*protocol.SourceEntry{entry("text", text("SELECT 1;"))}, extra...)...)
	}
	for _, tc := range []struct {
		name     string
		document *protocol.SourceDocument
		want     string
	}{
		{"unknown YAML key", yamlDocument(mapping(entry("q", valid(entry("checkpoint", text("ignore")))))), `.*unknown key "checkpoint".*`},
		{"unknown Go key", goDocument(valid(entry("name", text("q")), entry("checkpoint", text("ignore")))), `.*unknown key "checkpoint".*`},
		{"missing text", yamlDocument(mapping(entry("q", mapping()))), `.*text is required.*`},
		{"schema statement", yamlDocument(mapping(entry("q", mapping(entry("text", text("DROP TABLE t;")))))), `.*data statements only.*`},
		{"block escape", yamlDocument(mapping(entry("q", mapping(entry("text", text("END DO; DROP TABLE t;")))))), `.*`},
		{"YAML run type", yamlDocument(mapping(entry("q", valid(entry("run", number("1")))))), `.*run: expected a boolean.*`},
		{"YAML reset type", yamlDocument(mapping(entry("q", valid(entry("allow_state_reset", text("maybe")))))), `.*allow_state_reset: expected a boolean.*`},
		{"YAML quoted true", yamlDocument(mapping(entry("q", valid(entry("run", text("true")))))), `.*run: expected a boolean.*`},
		{"Go reset type", goDocument(valid(entry("name", text("q")), entry("allow_state_reset", text("maybe")))), `.*allow_state_reset: expected a boolean.*`},
		{"Go nontext", goDocument(valid(entry("name", text("q")), entry("run", boolean(false)))), `.*attribute run requires text.*`},
		{"nested text", yamlDocument(mapping(entry("q", mapping(entry("text", mapping()))))), `.*text: expected a scalar.*`},
		{"not a mapping", yamlDocument(text("q")), `.*requires a mapping.*`},
		{"entry not a mapping", yamlDocument(mapping(entry("q", text("SELECT 1;")))), `.*requires a mapping.*`},
		{"duplicate key", yamlDocument(mapping(entry("q", valid(entry("run", boolean(false)), entry("run", boolean(true)))))), `.*duplicate key "run".*`},
		{"duplicate Go attribute", goDocument(valid(entry("name", text("q")), entry("name", text("other")))), `.*duplicate key "name".*`},
		{"duplicate map entry", yamlDocument(mapping(entry("q", valid()), entry("q", valid(entry("name", text("other")))))), `.*duplicate key "q".*`},
		{"duplicate identity", yamlDocument(mapping(entry("a", valid(entry("name", text("q")))), entry("b", valid(entry("name", text("q")))))), `.*"q" is declared twice.*`},
		{"duplicate Go identity", goDocument(valid(entry("name", text("q"))), valid(entry("name", text("q")))), `.*"q" is declared twice.*`},
		{"no name", goDocument(valid()), `.*relative directory and an exact leaf name.*`},
		{"leaf slash", yamlDocument(mapping(entry("dir/q", valid()))), `.*relative directory and an exact leaf name.*`},
		{"parent directory", yamlDocument(mapping(entry("q", valid(entry("schema", text("../app")))))), `.*relative directory and an exact leaf name.*`},
		{"unclean directory", yamlDocument(mapping(entry("q", valid(entry("schema", text("app/../jobs")))))), `.*relative directory and an exact leaf name.*`},
		{"absolute directory", yamlDocument(mapping(entry("q", valid(entry("schema", text("/app")))))), `.*relative directory and an exact leaf name.*`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := qt.New(t)
			values, err := streamingsource.Decode(c.Context(), tc.document)
			c.Assert(err, qt.ErrorMatches, tc.want)
			c.Assert(values, qt.IsNil)
		})
	}
}

func TestDecodeRefusesAttachmentsAndFormats(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*protocol.SourceDocument)
	}{
		{"binding", func(doc *protocol.SourceDocument) { doc.Fragments[0].BindingId = "table" }},
		{"subject", func(doc *protocol.SourceDocument) { doc.Fragments[0].Subject = &protocol.Reference{Name: "q"} }},
		{"inherited", func(doc *protocol.SourceDocument) {
			doc.Fragments[0].InheritedAttributes = &protocol.SourceMapping{Entries: []*protocol.SourceEntry{entry("run", text("true"))}}
		}},
		{"HCL", func(doc *protocol.SourceDocument) { doc.Format = protocol.SourceDocument_HCL }},
		{"wrong selector", func(doc *protocol.SourceDocument) { doc.Fragments[0].Selector = "other" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := qt.New(t)
			doc := yamlDocument(mapping())
			tc.mutate(doc)
			values, err := streamingsource.Decode(c.Context(), doc)
			c.Assert(err, qt.ErrorMatches, `unsupported YDB streaming query source .*`)
			c.Assert(values, qt.IsNil)
		})
	}
}

func TestDecodeCanceled(t *testing.T) {
	c := qt.New(t)
	ctx, cancel := context.WithCancel(c.Context())
	cancel()
	values, err := streamingsource.Decode(ctx, yamlDocument(mapping()))
	c.Assert(err, qt.ErrorIs, context.Canceled)
	c.Assert(values, qt.IsNil)
}
