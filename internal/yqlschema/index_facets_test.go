package yqlschema_test

import (
	"fmt"
	"slices"
	"testing"

	qt "github.com/frankban/quicktest"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/stokaro/ptah/provider-sdk/provider"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb_Table"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	"github.com/stokaro/ptah-ydb-plugin/internal/backend"
	native "github.com/stokaro/ptah-ydb-plugin/internal/nativeprotocol"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
	"github.com/stokaro/ptah-ydb-plugin/internal/testsource"
	"github.com/stokaro/ptah-ydb-plugin/internal/ydbdecl"
	"github.com/stokaro/ptah-ydb-plugin/internal/yqlschema"
	"github.com/stokaro/ptah-ydb-plugin/schema"
)

// indexSettingsFacets are the facets of index settings among envelopes, each
// as its owner and its normalized payload.
func indexSettingsFacets(envelopes []*protocol.Envelope) []declaredFacet {
	var facets []declaredFacet
	for _, envelope := range envelopes {
		if envelope.GetKind() != schema.IndexSettingsProperties {
			continue
		}
		owner := envelope.GetOwner()
		facets = append(facets, declaredFacet{Kind: envelope.GetKind(), Owner: owner.GetKind() + " " + owner.GetSchema() + "/" + owner.GetPath()[0] + "/" + owner.GetName(),
			Payload: string(envelope.GetPayload())})
	}
	return facets
}

// goIndexFacets decodes the Go index directive of index by_body of app/items
// with attributes, for a host that reads an index's facets, and returns the
// facets of its index settings.
func goIndexFacets(c *qt.C, attributes map[string]string) []declaredFacet {
	c.Helper()
	request, err := testsource.GoDirectives([]testsource.GoTable{{Schema: "app", Name: "items", Struct: "Item"}}, []testsource.GoDeclaration{{
		Directive: "ptah:schema:index", Attributes: attributes, Struct: "Item", File: "entities.go", FileSize: 1000, Subject: testsource.GoSubject{Kind: "index", Name: "by_body"},
		Start: testsource.GoPosition{Offset: 0, Line: 1, Column: 1}, End: testsource.GoPosition{Offset: 40, Line: 1, Column: 41}}})
	c.Assert(err, qt.IsNil)
	ctx := provider.ContextWithHostFeatures(c.Context(), []string{provider.ObservedColumnsFeature, provider.ObservedIndexesFeature, provider.ObservedIndexFacetsFeature})
	decoded, err := backend.New().DecodeSource(ctx, request)
	c.Assert(err, qt.IsNil)
	var envelopes []*protocol.Envelope
	for _, fragment := range decoded.GetDocuments()[0].GetFragments() {
		for _, value := range fragment.GetValues() {
			envelopes = append(envelopes, value.GetValue().GetValue())
		}
	}
	return indexSettingsFacets(envelopes)
}

// The partitioning and read replicas a global index declares in its WITH
// clause reach the reply as one facet of the index, byte for byte the facet a
// Go source's index directive declares for the same settings, for a host that
// takes the facets of a reading and reads an index's own. The built-in YDB
// engine at host 74de0b65b applied such an index (the host's
// TestYDBDesiredYQL_AppliesAndSettles).
func TestReply_IndexSettingsFacetAgreesWithGo_HappyPath(t *testing.T) {
	tests := []struct {
		name, index string
		attributes  map[string]string
		payload     string
	}{
		{name: "a minimum partition count", index: "INDEX by_body GLOBAL ON (body) WITH (AUTO_PARTITIONING_MIN_PARTITIONS_COUNT = 2)",
			attributes: map[string]string{"auto_partitioning_min_partitions_count": "2"}, payload: `{"partitioningSettings":{"minPartitionsCount":"2"}}`},
		{name: "every setting of a unique index", index: "INDEX by_body GLOBAL UNIQUE SYNC ON (body) WITH (AUTO_PARTITIONING_BY_SIZE = DISABLED, " +
			"AUTO_PARTITIONING_PARTITION_SIZE_MB = 512, AUTO_PARTITIONING_BY_LOAD = ENABLED, AUTO_PARTITIONING_MIN_PARTITIONS_COUNT = 3, " +
			"AUTO_PARTITIONING_MAX_PARTITIONS_COUNT = 9, READ_REPLICAS_SETTINGS = 'ANY_AZ:2')",
			attributes: map[string]string{"auto_partitioning_by_size": "DISABLED", "auto_partitioning_partition_size_mb": "512", "auto_partitioning_by_load": "ENABLED",
				"auto_partitioning_min_partitions_count": "3", "auto_partitioning_max_partitions_count": "9", "read_replicas_settings": "any_az:2"},
			payload: `{"partitioningSettings":{"maxPartitionsCount":"9","minPartitionsCount":"3","partitionSizeMb":"512","partitioningByLoad":"ENABLED","partitioningBySize":"DISABLED"},` +
				`"readReplicasSettings":{"anyAzReadReplicasCount":"2"}}`},
		{name: "an async index's read replicas", index: "INDEX by_body GLOBAL ASYNC ON (body) WITH (READ_REPLICAS_SETTINGS = 'PER_AZ:1')",
			attributes: map[string]string{"read_replicas_settings": "PER_AZ:1"}, payload: `{"readReplicasSettings":{"perAzReadReplicasCount":"1"}}`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			request := schemaRequest(backend.Description().GetEngines()[0].GetFeatures(),
				"CREATE TABLE `app/items` (id Int64 NOT NULL, body Utf8, PRIMARY KEY (id), "+test.index+");")

			reply := yqlschema.Reply(request)

			c.Assert(provider.ValidateSQLSchema(backend.Description(), request, reply), qt.IsNil)
			c.Assert(reply.GetComplete(), qt.IsTrue, qt.Commentf("%v", reply.GetDiagnostics()))
			c.Assert(reply.GetTables()[0].GetIndexes(), qt.HasLen, 1)
			read := indexSettingsFacets(reply.GetFacets())
			c.Assert(read, qt.DeepEquals, []declaredFacet{{Kind: schema.IndexSettingsProperties, Owner: "index app/items/by_body", Payload: test.payload}})
			c.Assert(read, qt.DeepEquals, goIndexFacets(c, test.attributes))
			c.Assert(claimedKinds(reply), qt.Contains, schema.IndexSettingsProperties)
		})
	}
}

// The facet is traced to the CREATE TABLE that declares the index, and an
// index that declares no setting gets none: it keeps what YDB gives it.
func TestReply_IndexSettingsFacetIsTracedToItsIndex_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	request := schemaRequest(backend.Description().GetEngines()[0].GetFeatures(), "CREATE TABLE `app/other` (id Int64 NOT NULL, PRIMARY KEY (id));\nCREATE TABLE `app/items` (\n"+
		"    id Int64 NOT NULL, body Utf8, note Utf8, PRIMARY KEY (id),\n"+
		"    INDEX by_note GLOBAL ON (note),\n"+
		"    INDEX by_body GLOBAL ON (body) WITH (AUTO_PARTITIONING_MIN_PARTITIONS_COUNT = 2)\n);")

	reply := yqlschema.Reply(request)

	c.Assert(reply.GetComplete(), qt.IsTrue, qt.Commentf("%v", reply.GetDiagnostics()))
	c.Assert(reply.GetFacets(), qt.HasLen, 1)
	c.Assert(proto.Equal(reply.GetFacets()[0].GetOwner(), &protocol.Reference{Engine: "ydb", Kind: "index", Schema: "app", Path: []string{"items"}, Name: "by_body"}), qt.IsTrue)
	var traced []uint32
	for _, trace := range reply.GetTraces() {
		traced = append(traced, map[bool]uint32{true: trace.GetSource().GetSpan().GetStart().GetLine(), false: 0}[trace.GetCollection() == protocol.SQLObjectTrace_FACETS])
	}
	c.Assert(slices.DeleteFunc(traced, func(line uint32) bool { return line == 0 }), qt.DeepEquals, []uint32{2})
}

// An index's settings have no home where the host does not take the facet.
// The load is refused naming the index, with no schema. A vector index is one
// of its table's native indexes (see TestReply_OwnedIndexes_FailurePath).
func TestReply_IndexSettingsFacet_FailurePath(t *testing.T) {
	tests := []struct {
		name, without, text, want string
	}{
		{name: "a host without the facets of a reading", without: provider.SQLFacetsFeature,
			text: "CREATE TABLE `app/items` (id Int64 NOT NULL, body Utf8, PRIMARY KEY (id), INDEX by_body GLOBAL SYNC ON (body) WITH (AUTO_PARTITIONING_MIN_PARTITIONS_COUNT = 2));",
			want: "index `by_body` of table `app/items` declares partitioning settings, which a SQL schema reading has no home for: " +
				"the host does not take the facets of an index in a SQL schema reading (schema.sql-facets and schema.observed-index-facets)"},
		{name: "a host that reads no index's facets", without: provider.ObservedIndexFacetsFeature,
			text: "CREATE TABLE `app/items` (id Int64 NOT NULL, body Utf8, PRIMARY KEY (id), INDEX by_body GLOBAL SYNC ON (body) WITH (AUTO_PARTITIONING_MIN_PARTITIONS_COUNT = 2));",
			want: "index `by_body` of table `app/items` declares partitioning settings, which a SQL schema reading has no home for: " +
				"the host does not take the facets of an index in a SQL schema reading (schema.sql-facets and schema.observed-index-facets)"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			features := withoutFeature(test.without)
			request := schemaRequest(features, test.text)
			negotiated := provider.NegotiatedDescription(backend.Description(), slices.DeleteFunc(slices.Clone(provider.TwoWayFeatures()), func(feature string) bool {
				return feature == test.without
			}))

			reply := yqlschema.Reply(request)

			c.Assert(provider.ValidateSQLSchema(negotiated, request, reply), qt.IsNil)
			c.Assert(reply.GetComplete(), qt.IsFalse)
			c.Assert(reply.GetTables(), qt.HasLen, 0)
			c.Assert(reply.GetFacets(), qt.HasLen, 0)
			c.Assert(reply.GetDiagnostics(), qt.HasLen, 1)
			c.Assert(reply.GetDiagnostics()[0].GetMessage(), qt.Equals, test.want)
		})
	}
}

// Without the facet the reading claims nothing about an index's settings, so
// the settings an index holds stay unmanaged rather than read as none.
func TestReply_IndexSettingsClaimNeedsTheFacet_HappyPath(t *testing.T) {
	for _, without := range []string{provider.SQLFacetsFeature, provider.ObservedIndexFacetsFeature} {
		t.Run(without, func(t *testing.T) {
			c := testcheck.New(t)

			reply := yqlschema.Reply(schemaRequest(withoutFeature(without), "CREATE TABLE `app/items` (id Int64 NOT NULL, body Utf8, PRIMARY KEY (id), INDEX by_body GLOBAL ON (body));"))

			c.Assert(reply.GetComplete(), qt.IsTrue, qt.Commentf("%v", reply.GetDiagnostics()))
			c.Assert(claimedKinds(reply), qt.Not(qt.Contains), schema.IndexSettingsProperties)
		})
	}
}

// hostIndexFacets are the facets a complete reply gives its indexes, written
// as the host's reader writes the same settings: by the index's table and
// name, its partitioning with its read replicas.
func hostIndexFacets(c *qt.C, reply *protocol.SQLSchema) map[string]map[string]any {
	c.Helper()
	result := make(map[string]map[string]any)
	for _, facet := range reply.GetFacets() {
		if facet.GetKind() != schema.IndexSettingsProperties {
			continue
		}
		var intent native.IndexSettingsIntent
		c.Assert(protojson.Unmarshal(facet.GetPayload(), &intent), qt.IsNil)
		settings := intent.GetPartitioningSettings()
		partitioning := ydbdecl.IndexPartitioning{BySize: switchOf(settings.GetPartitioningBySize()), PartitionSizeMB: settings.GetPartitionSizeMb(),
			ByLoad: switchOf(settings.GetPartitioningByLoad()), MinPartitions: settings.GetMinPartitionsCount(), MaxPartitions: settings.GetMaxPartitionsCount()}
		switch replicas := intent.GetReadReplicasSettings().GetSettings().(type) {
		case *Ydb_Table.ReadReplicasSettings_PerAzReadReplicasCount:
			partitioning.ReadReplicas = fmt.Sprintf("PER_AZ:%d", replicas.PerAzReadReplicasCount)
		case *Ydb_Table.ReadReplicasSettings_AnyAzReadReplicasCount:
			partitioning.ReadReplicas = fmt.Sprintf("ANY_AZ:%d", replicas.AnyAzReadReplicasCount)
		}
		owner := facet.GetOwner()
		result[canonical(owner.GetSchema(), owner.GetPath()[0])+"\x00"+owner.GetName()] = map[string]any{"ptah.run/ydb/index-partitioning": decodeAny(c, marshal(c, partitioning))}
	}
	return result
}
