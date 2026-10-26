package workloadsource_test

import (
	"context"
	"testing"

	qt "github.com/frankban/quicktest"
	"github.com/stokaro/ptah-ydb-plugin/internal/workload"
	"github.com/stokaro/ptah-ydb-plugin/internal/workloadsource"
	"github.com/stokaro/ptah-ydb-plugin/schema"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
)

func text(value string) *protocol.SourceValue {
	return &protocol.SourceValue{Value: &protocol.SourceValue_Text{Text: value}}
}

func number(value string) *protocol.SourceValue {
	return &protocol.SourceValue{Value: &protocol.SourceValue_Number{Number: value}}
}

func mapping(entries ...*protocol.SourceEntry) *protocol.SourceValue {
	return &protocol.SourceValue{Value: &protocol.SourceValue_Mapping{Mapping: &protocol.SourceMapping{Entries: entries}}}
}

func entry(key string, value *protocol.SourceValue) *protocol.SourceEntry {
	return &protocol.SourceEntry{Key: key, Value: value}
}

func models() []*protocol.ValueModel {
	return []*protocol.ValueModel{schema.NativeDesiredModel(schema.ResourcePoolProperties), schema.NativeDesiredModel(schema.ResourcePoolClassifierProperties)}
}

// yamlDocument captures a document's resource_pools and
// resource_pool_classifiers keys, either nil.
func yamlDocument(pools, classifiers *protocol.SourceValue) *protocol.SourceDocument {
	document := &protocol.SourceDocument{Id: "doc", Format: protocol.SourceDocument_YAML, Models: models()}
	keys := workloadsource.YAMLKeys()
	for i, value := range []*protocol.SourceValue{pools, classifiers} {
		if value != nil {
			document.Fragments = append(document.Fragments, &protocol.SourceFragment{Id: keys[i].Key, Selector: keys[i].Selector, YamlKey: keys[i], Content: &protocol.SourceFragment_Value{Value: value}})
		}
	}
	return document
}

// goDocument captures one Go file's directives, each a directive name and
// its text attributes in written order.
func goDocument(directives ...[]string) *protocol.SourceDocument {
	document := &protocol.SourceDocument{Id: "doc", Format: protocol.SourceDocument_GO_ANNOTATIONS, Models: models()}
	claims := map[string]*protocol.GoSourceDirective{}
	for _, claim := range workloadsource.GoDirectives() {
		claims[claim.Directive] = claim
	}
	for i, directive := range directives {
		var entries []*protocol.SourceEntry
		for j := 1; j+1 < len(directive); j += 2 {
			entries = append(entries, entry(directive[j], text(directive[j+1])))
		}
		document.Fragments = append(document.Fragments, &protocol.SourceFragment{Id: string(rune('a' + i)), Selector: directive[0], GoDirective: claims[directive[0]],
			Content: &protocol.SourceFragment_Value{Value: mapping(entries...)}})
	}
	return document
}

// declared reads every value a decode returned, by kind and name.
func declared(c *qt.C, values map[int][]*protocol.SourceModelValue) (map[string]workload.PoolSpec, map[string]workload.ClassifierSpec) {
	c.Helper()
	pools, classifiers := map[string]workload.PoolSpec{}, map[string]workload.ClassifierSpec{}
	for _, fragment := range values {
		for _, value := range fragment {
			c.Assert(value.Placement, qt.Equals, protocol.SourceModelValue_NATIVE_OBJECT)
			envelope := value.Value.Value
			switch envelope.Kind {
			case schema.ResourcePoolProperties:
				spec, err := schema.WorkloadPool(envelope)
				c.Assert(err, qt.IsNil)
				pools[envelope.Owner.Name] = spec
			default:
				spec, err := schema.WorkloadClassifier(envelope)
				c.Assert(err, qt.IsNil)
				classifiers[envelope.Owner.Name] = spec
			}
		}
	}
	return pools, classifiers
}

// A YAML document declares pools and classifiers keyed by name with the
// host's keys, the default pool among them.
func TestDecodeYAML_HappyPath(t *testing.T) {
	c := qt.New(t)
	document := yamlDocument(
		mapping(entry("reporting", mapping(entry("concurrent_query_limit", number("10")), entry("queue_size", number("20")),
			entry("query_memory_limit_percent_per_node", number("25.5")))), entry("default", mapping(entry("resource_weight", number("30")))), entry("idle", mapping())),
		mapping(entry("reporters", mapping(entry("resource_pool", text("reporting")), entry("member_name", text("analysts")), entry("rank", number("100")))),
			entry("everyone", mapping(entry("resource_pool", text("default")), entry("rank", number("0"))))),
	)
	values, err := workloadsource.Decode(context.Background(), document)
	c.Assert(err, qt.IsNil)
	pools, classifiers := declared(c, values)
	c.Assert(pools, qt.DeepEquals, map[string]workload.PoolSpec{
		"reporting": {ConcurrentQueryLimit: new(int32(10)), QueueSize: new(int32(20)), QueryMemoryLimitPercentPerNode: new(25.5)},
		"default":   {ResourceWeight: new(30.0)},
		"idle":      {},
	})
	c.Assert(classifiers, qt.DeepEquals, map[string]workload.ClassifierSpec{
		"reporters": {ResourcePool: "reporting", MemberName: "analysts", Rank: 100},
		"everyone":  {ResourcePool: "default"},
	})
}

// A Go file declares them with the host's directives and attributes: the
// fixture 054-ydb-resource-pools of the host.
func TestDecodeGo_HappyPath(t *testing.T) {
	c := qt.New(t)
	document := goDocument(
		[]string{workloadsource.DirectivePool, "name", "reporting", "concurrent_query_limit", "10", "queue_size", "20", "query_memory_limit_percent_per_node", "25.5"},
		[]string{workloadsource.DirectiveClassifier, "name", "reporting_group", "resource_pool", "reporting", "member_name", "reporters", "rank", "100"},
	)
	values, err := workloadsource.Decode(context.Background(), document)
	c.Assert(err, qt.IsNil)
	c.Assert(values, qt.HasLen, 2)
	pools, classifiers := declared(c, values)
	c.Assert(pools, qt.DeepEquals, map[string]workload.PoolSpec{
		"reporting": {ConcurrentQueryLimit: new(int32(10)), QueueSize: new(int32(20)), QueryMemoryLimitPercentPerNode: new(25.5)},
	})
	c.Assert(classifiers, qt.DeepEquals, map[string]workload.ClassifierSpec{"reporting_group": {ResourcePool: "reporting", MemberName: "reporters", Rank: 100}})
}

// What the host's source refuses is refused with its words, naming the
// object, and the whole document goes.
func TestDecode_FailurePath(t *testing.T) {
	for _, test := range []struct {
		name     string
		document *protocol.SourceDocument
		wantErr  string
	}{
		{name: "a queue with nothing to wait for", document: yamlDocument(mapping(entry("batch", mapping(entry("queue_size", number("5"))))), nil),
			wantErr: `resource pool "batch": invalid queue_size "5": a queue needs .*`},
		{name: "a classifier without a rank", document: yamlDocument(nil, mapping(entry("c", mapping(entry("resource_pool", text("default")))))),
			wantErr: `resource pool classifier "c": invalid rank "": a classifier needs a rank; .*`},
		{name: "a classifier without a pool", document: yamlDocument(nil, mapping(entry("c", mapping(entry("rank", number("1")))))),
			wantErr: `resource pool classifier "c": invalid resource_pool "": .*`},
		{name: "an empty setting", document: yamlDocument(mapping(entry("batch", mapping(entry("concurrent_query_limit", text(""))))), nil),
			wantErr: `resource pool "batch": invalid concurrent_query_limit "": .*`},
		{name: "an unknown YAML key", document: yamlDocument(mapping(entry("batch", mapping(entry("max_queries", number("3"))))), nil),
			wantErr: `resource pool "batch": unknown key "max_queries"`},
		{name: "a percentage out of range in Go", document: goDocument([]string{workloadsource.DirectivePool, "name", "p", "total_cpu_limit_percent_per_node", "101"}),
			wantErr: `invalid total_cpu_limit_percent_per_node "101": takes a percentage from 0 to 100; leave it out for no limit on //ptah:schema:resourcepool`},
		{name: "a pool with no name in Go", document: goDocument([]string{workloadsource.DirectivePool, "concurrent_query_limit", "3"}),
			wantErr: `invalid name "": a resource pool needs a name on //ptah:schema:resourcepool`},
		{name: "a pool declared twice", document: goDocument([]string{workloadsource.DirectivePool, "name", "p"}, []string{workloadsource.DirectivePool, "name", "p"}),
			wantErr: `resource pool "p" is declared twice`},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := qt.New(t)
			values, err := workloadsource.Decode(context.Background(), test.document)
			c.Assert(err, qt.ErrorMatches, test.wantErr)
			c.Assert(values, qt.IsNil)
		})
	}
}
