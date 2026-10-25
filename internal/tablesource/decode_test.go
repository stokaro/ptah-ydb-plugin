package tablesource_test

import (
	"context"
	"slices"
	"testing"

	qt "github.com/frankban/quicktest"
	"github.com/stokaro/ptah-ydb-plugin/internal/backend"
	"github.com/stokaro/ptah-ydb-plugin/internal/tablesource"
	"github.com/stokaro/ptah-ydb-plugin/internal/testsource"
	"github.com/stokaro/ptah-ydb-plugin/schema"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/stokaro/ptah/provider-sdk/provider"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

func yamlRequest(c *qt.C, source string) *protocol.DecodeSourceRequest {
	c.Helper()
	request, err := testsource.YAMLTables(source)
	c.Assert(err, qt.IsNil)
	return request
}

func TestClaims(t *testing.T) {
	c := qt.New(t)
	var keys []string
	models := map[string][]string{}
	for _, claim := range tablesource.YAMLKeys() {
		keys = append(keys, claim.Key)
		c.Assert(claim.Scope, qt.Equals, protocol.YAMLSourceKey_TABLE)
		models[claim.Key] = claim.Models
	}
	c.Assert(keys, qt.DeepEquals, []string{
		"auto_partitioning_by_size", "auto_partitioning_partition_size_mb", "auto_partitioning_by_load", "auto_partitioning_min_partitions_count",
		"auto_partitioning_max_partitions_count", "read_replicas_settings", "key_bloom_filter", "uniform_partitions", "partition_at_keys", "column_families", "platform.ydb",
		"column_store",
	})
	// column_store declares a column table, and the TTL of the platform group
	// is a column table's TTL beside it; every other key declares row-table
	// settings.
	for _, key := range keys[:10] {
		c.Assert(models[key], qt.DeepEquals, []string{schema.TableProperties}, qt.Commentf(key))
	}
	c.Assert(models["platform.ydb"], qt.DeepEquals, []string{schema.TableProperties, schema.ColumnStorageProperties})
	c.Assert(models["column_store"], qt.DeepEquals, []string{schema.ColumnStorageProperties})
	directives := tablesource.GoDirectives()
	c.Assert(len(directives), qt.Equals, 5)
	scopes := map[string]protocol.GoSourceDirective_Scope{}
	var names []string
	for _, directive := range directives {
		names = append(names, directive.Directive)
		scopes[directive.Directive] = directive.Scope
		// A family lists the columns it holds, so it answers for them too, and
		// the table directive declares a row table or a column table.
		want := []string{schema.TableProperties}
		switch directive.Directive {
		case tablesource.DirectiveColumnFamily:
			want = append(want, schema.ColumnProperties)
		case tablesource.DirectiveTable:
			want = append(want, schema.ColumnStorageProperties)
		}
		c.Assert(directive.Models, qt.DeepEquals, want, qt.Commentf(directive.Directive))
	}
	c.Assert(names, qt.DeepEquals, []string{"ptah:schema:table", "ptah:schema:index", "ptah:schema:columnfamily", "ptah:schema:changefeed", "ptah:schema:changefeed:consumer"})
	// The host's own directives take the scope of the part they declare, and
	// the provider's directives sit on a table.
	c.Assert(scopes, qt.DeepEquals, map[string]protocol.GoSourceDirective_Scope{
		"ptah:schema:table": protocol.GoSourceDirective_TABLE, "ptah:schema:index": protocol.GoSourceDirective_INDEX,
		"ptah:schema:columnfamily": protocol.GoSourceDirective_TABLE, "ptah:schema:changefeed": protocol.GoSourceDirective_TABLE,
		"ptah:schema:changefeed:consumer": protocol.GoSourceDirective_TABLE,
	})
}

// A claim on the host's own directive lists attributes only, none of which the
// directive reads itself, and the YAML keys and the Go table attributes spell
// the shared settings alike.
func TestClaims_TableAttributesAreTheYAMLKeysAndMore(t *testing.T) {
	c := qt.New(t)
	var table *protocol.GoSourceDirective
	for _, directive := range tablesource.GoDirectives() {
		if directive.Directive == tablesource.DirectiveTable {
			table = directive
		}
	}
	c.Assert(table, qt.IsNotNil)
	for _, key := range tablesource.YAMLKeys() {
		if key.Key != tablesource.KeyColumnFamilies && key.Key != tablesource.KeyPlatformYDB && key.Key != tablesource.KeyColumnStore {
			c.Assert(slices.Contains(table.Attributes, key.Key), qt.IsTrue, qt.Commentf(key.Key))
		}
	}
	c.Assert(table.Attributes[len(table.Attributes)-3:], qt.DeepEquals,
		[]string{"platform.ydb.row_deletion_column", "platform.ydb.row_deletion_interval", "platform.ydb.row_deletion_unit"})
	for _, forbidden := range []string{"name", "schema", "table", "comment", "primary_key", "custom"} {
		c.Assert(slices.Contains(table.Attributes, forbidden), qt.IsFalse, qt.Commentf(forbidden))
	}
}

func TestDescriptionAdvertisesTheTableClaims(t *testing.T) {
	c := qt.New(t)
	description := backend.Description()
	c.Assert(provider.ValidateDescription(description, nil), qt.IsNil)
	engine := description.Engines[0]
	for _, claim := range tablesource.YAMLKeys() {
		c.Assert(slices.ContainsFunc(engine.YamlSourceKeys, func(declared *protocol.YAMLSourceKey) bool { return proto.Equal(declared, claim) }), qt.IsTrue, qt.Commentf(claim.Key))
	}
	for _, directive := range tablesource.GoDirectives() {
		c.Assert(slices.ContainsFunc(engine.GoSourceDirectives, func(declared *protocol.GoSourceDirective) bool { return proto.Equal(declared, directive) }), qt.IsTrue, qt.Commentf(directive.Directive))
	}
	c.Assert(schema.TableDesiredModel().SourceFormats, qt.DeepEquals, []protocol.SourceDocument_Format{protocol.SourceDocument_YAML, protocol.SourceDocument_GO_ANNOTATIONS, protocol.SourceDocument_SQL})
	c.Assert(schema.ColumnDesiredModel().SourceFormats, qt.DeepEquals, []protocol.SourceDocument_Format{protocol.SourceDocument_GO_ANNOTATIONS, protocol.SourceDocument_SQL})
}

func TestDecode_HappyPath(t *testing.T) {
	tests := []struct {
		name   string
		source string
		want   string
	}{
		{"every setting", `{"tables":{"items":{"auto_partitioning_by_size":"disabled","auto_partitioning_by_load":"ENABLED","auto_partitioning_partition_size_mb":512,` +
			`"auto_partitioning_min_partitions_count":3,"auto_partitioning_max_partitions_count":9,"read_replicas_settings":"ANY_AZ:2","key_bloom_filter":"enabled",` +
			`"column_families":{"default":{"compression":"LZ4"},"cold":{"data":"hdd","compression":"off"}}}}}`,
			`{"columnFamilies":[{"compression":"COMPRESSION_LZ4","name":"default"},{"compression":"COMPRESSION_NONE","data":{"media":"hdd"},"name":"cold"}],` +
				`"keyBloomFilter":"ENABLED","partitioningSettings":{"maxPartitionsCount":"9","minPartitionsCount":"3","partitionSizeMb":"512","partitioningByLoad":"ENABLED",` +
				`"partitioningBySize":"DISABLED"},"readReplicasSettings":{"anyAzReadReplicasCount":"2"}}`},
		{"no replicas stays a statement", `{"tables":{"items":{"read_replicas_settings":"per_az:0"}}}`, `{"readReplicasSettings":{"perAzReadReplicasCount":"0"}}`},
		{"a TTL in the platform group", `{"tables":{"items":{"platform":{"ydb":{"row_deletion_column":"created_at","row_deletion_interval":"PT1H"}}}}}`,
			`{"ttlSettings":{"dateTypeColumn":{"columnName":"created_at","expireAfterSeconds":3600}}}`},
		{"a TTL on an integer column", `{"tables":{"items":{"platform":{"ydb":{"row_deletion_column":"created_at","row_deletion_interval":"P1D","row_deletion_unit":"milliseconds"}}}}}`,
			`{"ttlSettings":{"valueSinceUnixEpoch":{"columnName":"created_at","columnUnit":"UNIT_MILLISECONDS","expireAfterSeconds":86400}}}`},
		{"a TTL beside the groups of other engines", `{"tables":{"items":{"platform":{"mysql":{"engine":"InnoDB"},"ydb":{"row_deletion_column":"created_at","row_deletion_interval":"PT1H"}}}}}`,
			`{"ttlSettings":{"dateTypeColumn":{"columnName":"created_at","expireAfterSeconds":3600}}}`},
		{"a TTL beside partitioning and a family", `{"tables":{"items":{"key_bloom_filter":"ENABLED","column_families":{"cold":{}},` +
			`"platform":{"ydb":{"row_deletion_column":"created_at","row_deletion_interval":"PT1H"}}}}}`,
			`{"columnFamilies":[{"name":"cold"}],"keyBloomFilter":"ENABLED","ttlSettings":{"dateTypeColumn":{"columnName":"created_at","expireAfterSeconds":3600}}}`},
		{"a family that states nothing", `{"tables":{"items":{"column_families":{"default":{}}}}}`, `{"columnFamilies":[{"name":"default"}]}`},
		{"a storage pool keeps its case", `{"tables":{"items":{"column_families":{" Cold ":{"data":" HDD "}}}}}`, `{"columnFamilies":[{"data":{"media":"HDD"},"name":"Cold"}]}`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			c := qt.New(t)
			request := yamlRequest(c, test.source)
			tables, err := tablesource.Decode(c.Context(), request.Documents[0])
			c.Assert(err, qt.IsNil)
			c.Assert(len(tables), qt.Equals, 1)
			c.Assert(string(tables[0].Value.CanonicalProjection), qt.Equals, test.want)
			c.Assert(string(tables[0].Value.Value.Payload), qt.Equals, test.want)
			c.Assert(tables[0].Value.Value.Kind, qt.Equals, schema.TableProperties)
			c.Assert(tables[0].Value.CanonicalFormat, qt.Equals, schema.TableDesiredModel().CanonicalFormat)
		})
	}
}

// TestDecode_ColumnStore pins the column-storage facet the column_store key
// makes, and the TTL of the platform group beside it, whichever comes first.
func TestDecode_ColumnStore(t *testing.T) {
	tests := []struct {
		name   string
		source string
		want   string
	}{
		{"an empty mapping", `{"tables":{"items":{"column_store":{}}}}`, `{}`},
		{"every setting", `{"tables":{"items":{"column_store":{"hash_columns":["b","a"],"partitions":3,"ttl":{"column":"t","unit":"MILLISECONDS",` +
			`"tiers":[{"interval":"PT1H","external_source":"/local/ext"}]}}}}}`,
			`{"hashColumns":["b","a"],"partitions":"3","ttl":{"enabled":{"column":"t","columnUnit":"2","tiers":[{"applyAfterSeconds":"3600","externalStorage":"/local/ext"}]}}}`},
		{"null settings and zero shards leave the server's choice", `{"tables":{"items":{"column_store":{"hash_columns":null,"partitions":0,"ttl":null}}}}`, `{}`},
		{"a TTL in the platform group after the storage", `{"tables":{"items":{"column_store":{"partitions":2},"platform":{"ydb":{"row_deletion_column":"t","row_deletion_interval":"P1D"}}}}}`,
			`{"partitions":"2","ttl":{"enabled":{"column":"t","tiers":[{"applyAfterSeconds":"86400","delete":{}}]}}}`},
		{"a TTL in the platform group before the storage", `{"tables":{"items":{"platform":{"ydb":{"row_deletion_column":"t","row_deletion_interval":"P1D"}},"column_store":{}}}}`,
			`{"ttl":{"enabled":{"column":"t","tiers":[{"applyAfterSeconds":"86400","delete":{}}]}}}`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			c := qt.New(t)
			request := yamlRequest(c, test.source)
			tables, err := tablesource.Decode(c.Context(), request.Documents[0])
			c.Assert(err, qt.IsNil)
			c.Assert(len(tables), qt.Equals, 1)
			c.Assert(tables[0].Value.Value.Kind, qt.Equals, schema.ColumnStorageProperties)
			c.Assert(string(tables[0].Value.CanonicalProjection), qt.Equals, test.want)
			decoded, err := backend.New().DecodeSource(c.Context(), request)
			c.Assert(err, qt.IsNil)
			c.Assert(provider.ValidateDecodedSource(backend.Description(), request, decoded), qt.IsNil)
		})
	}
}

// TestDecode_ColumnStoreFailurePath refuses a column_store mapping whose
// values the bundled decoder refuses too.
func TestDecode_ColumnStoreFailurePath(t *testing.T) {
	tests := []struct{ name, source, wantErr string }{
		{"not a mapping", `{"tables":{"items":{"column_store":"COLUMN"}}}`, `table "items": column_store: expected a mapping of hash_columns, partitions and ttl`},
		{"an unknown key", `{"tables":{"items":{"column_store":{"shards":4}}}}`, `table "items": column_store: shards: unknown key`},
		{"shards as text", `{"tables":{"items":{"column_store":{"partitions":"4"}}}}`, `table "items": column_store: partitions: expected a number of shards`},
		{"negative shards", `{"tables":{"items":{"column_store":{"partitions":-1}}}}`, `table "items": column_store: partitions: expected a whole number of shards`},
		{"shards beyond 32 bits", `{"tables":{"items":{"column_store":{"partitions":4294967296}}}}`, `table "items": column_store: partitions: column shard count exceeds a 32-bit integer`},
		{"a hash key as one name", `{"tables":{"items":{"column_store":{"hash_columns":"id"}}}}`, `table "items": column_store: hash_columns: expected a list of column names`},
		{"a hash column twice", `{"tables":{"items":{"column_store":{"hash_columns":["id","id"]}}}}`, `table "items": hash partitioning column "id" is empty or repeated`},
		{"a TTL tier with an unknown key", `{"tables":{"items":{"column_store":{"ttl":{"column":"t","tiers":[{"interval":"P1D","delete":true}]}}}}}`,
			`table "items": column_store: ttl: tiers: tier 1: delete: unknown key`},
		{"a TTL that is a list", `{"tables":{"items":{"column_store":{"ttl":["t"]}}}}`, `table "items": column_store: ttl: expected a mapping of column, unit and tiers`},
		{"a TTL that only deletes", `{"tables":{"items":{"column_store":{"ttl":{"column":"t","tiers":[{"interval":"P1D"}]}}}}}`,
			`table "items": column_store: ttl: a tiered TTL requires a tier that moves rows to an external data source; a TTL that only deletes is the table's TTL`},
		{"row partitioning beside the storage", `{"tables":{"items":{"column_store":{},"key_bloom_filter":"ENABLED"}}}`,
			`table "items": row-table partitioning, column families and changefeeds cannot be applied to a column table`},
		{"a family beside the storage", `{"tables":{"items":{"column_store":{},"column_families":{"cold":{}}}}}`,
			`table "items": row-table partitioning, column families and changefeeds cannot be applied to a column table`},
		{"two TTLs", `{"tables":{"items":{"column_store":{"ttl":{"column":"t","tiers":[{"interval":"P1D","external_source":"/local/ext"}]}},` +
			`"platform":{"ydb":{"row_deletion_column":"t","row_deletion_interval":"P2D"}}}}}`, `table "items": declare either tiered TTL or a TTL that deletes rows, not both`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			c := qt.New(t)
			request := yamlRequest(c, test.source)
			tables, err := tablesource.Decode(c.Context(), request.Documents[0])
			c.Assert(err, qt.ErrorMatches, test.wantErr)
			c.Assert(tables, qt.IsNil)
		})
	}
}

// A document may carry one facet of a kind for a table, so the fragments of a
// table declare one together, and the first of them returns it.
func TestDecodeSource_OneFacetForAllFragmentsOfATable(t *testing.T) {
	c := qt.New(t)
	request := yamlRequest(c, `{"tables":{"orders":{"schema":"shop","key_bloom_filter":"ENABLED","auto_partitioning_by_load":"ENABLED","column_families":{"cold":{}}},`+
		`"items":{"read_replicas_settings":"ANY_AZ:1"},"plain":{}}}`)
	decoded, err := backend.New().DecodeSource(c.Context(), request)
	c.Assert(err, qt.IsNil)
	c.Assert(provider.ValidateDecodedSource(backend.Description(), request, decoded), qt.IsNil)
	fragments := decoded.Documents[0].Fragments
	c.Assert(len(fragments), qt.Equals, 4)
	var owners []string
	for _, fragment := range fragments {
		for _, value := range fragment.Values {
			owners = append(owners, fragment.Id+" "+value.Value.Value.Owner.Schema+"."+value.Value.Value.Owner.Name)
			c.Assert(value.Placement, qt.Equals, protocol.SourceModelValue_FACET)
			c.Assert(value.Source.FragmentId, qt.Equals, fragment.Id)
			c.Assert(value.Source.Path, qt.HasLen, 0)
		}
	}
	c.Assert(owners, qt.DeepEquals, []string{"orders/key_bloom_filter shop.orders", "items/read_replicas_settings .items"})
	c.Assert(len(decoded.Documents[0].Coverage.Subjects), qt.Equals, 2)
	for _, subject := range decoded.Documents[0].Coverage.Subjects {
		c.Assert(subject.Kind, qt.Equals, schema.TableProperties)
		c.Assert(subject.Knowledge.State, qt.Equals, protocol.ModelKnowledge_COMPLETE)
	}
	for _, fragment := range fragments {
		for _, value := range fragment.Values {
			normalized, err := backend.New().NormalizeValues(c.Context(), &protocol.NormalizeValuesRequest{Engine: "ydb", Values: []*protocol.ValueInput{
				{Id: value.Value.Id, Representation: protocol.ValueInput_DESIRED, Value: value.Value.Value}}})
			c.Assert(err, qt.IsNil)
			c.Assert(proto.Equal(normalized.Values[0], value.Value), qt.IsTrue)
		}
	}
}

// A table that declares nothing gets no facet. A facet would make the table's
// TTL and partitioning managed, and an empty mapping asks for nothing.
func TestDecode_DeclaringNothingGivesNoFacet(t *testing.T) {
	for _, source := range []string{
		`{"tables":{"items":{"column_families":{}}}}`,
		`{"tables":{"items":{"column_families":null}}}`,
		`{"tables":{"items":{}}}`,
	} {
		t.Run(source, func(t *testing.T) {
			c := qt.New(t)
			request := yamlRequest(c, source)
			tables, err := tablesource.Decode(c.Context(), request.Documents[0])
			c.Assert(err, qt.IsNil)
			c.Assert(len(tables), qt.Equals, 0)
			decoded, err := backend.New().DecodeSource(c.Context(), request)
			c.Assert(err, qt.IsNil)
			for _, fragment := range decoded.Documents[0].Fragments {
				c.Assert(len(fragment.Values), qt.Equals, 0)
			}
			c.Assert(len(decoded.Documents[0].Coverage.Subjects), qt.Equals, 0)
		})
	}
}

func TestDecode_FailurePath(t *testing.T) {
	tests := []struct {
		name    string
		source  string
		wantErr string
	}{
		{"a count of zero", `{"tables":{"items":{"auto_partitioning_min_partitions_count":0}}}`, `table "items": invalid auto_partitioning_min_partitions_count "0": write a whole number of at least 1.*`},
		{"a switch spelled as a boolean", `{"tables":{"items":{"key_bloom_filter":true}}}`, `table "items": invalid key_bloom_filter "true": write ENABLED or DISABLED`},
		{"a null where a setting is due", `{"tables":{"items":{"key_bloom_filter":null}}}`, `table "items": invalid key_bloom_filter "": write ENABLED or DISABLED`},
		{"replicas of two kinds", `{"tables":{"items":{"read_replicas_settings":"PER_AZ:1,ANY_AZ:1"}}}`, `table "items": invalid read_replicas_settings .*write PER_AZ:<n> for n replicas.*`},
		{"a minimum above the maximum", `{"tables":{"items":{"auto_partitioning_min_partitions_count":9,"auto_partitioning_max_partitions_count":3}}}`, `table "items": YDB minimum partitions exceed maximum`},
		{"zstd on a row table", `{"tables":{"items":{"column_families":{"cold":{"compression":"zstd"}}}}}`, `table "items": column family "cold": invalid compression "zstd": takes off or lz4: YDB keeps zstd for column-oriented tables.*`},
		{"a family setting in a list", `{"tables":{"items":{"column_families":{"cold":{"data":["hdd"]}}}}}`, `table "items": column family "cold": data: expected a scalar`},
		{"a family that is not a mapping", `{"tables":{"items":{"column_families":{"cold":"lz4"}}}}`, `table "items": column family "cold": expected a mapping of settings`},
		{"families that are not a mapping", `{"tables":{"items":{"column_families":["cold"]}}}`, `table "items": column_families: expected a mapping of family names to settings`},
		{"a setting no family has", `{"tables":{"items":{"column_families":{"cold":{"barriers":"x"}}}}}`, `table "items": column family "cold": unknown setting "barriers"`},
		{"a family without a name", `{"tables":{"items":{"column_families":{" ":{}}}}}`, `table "items": column family " ": invalid name "": a column family needs a name`},
		{"a cache mode", `{"tables":{"items":{"column_families":{"cold":{"cache_mode":"regular"}}}}}`, `table "items": column family "cold": cache_mode is not supported by the YDB provider yet: .*`},
		{"column placement", `{"tables":{"items":{"column_families":{"cold":{"fields":["body"]}}}}}`, `table "items": column family "cold": fields is not supported by the YDB provider yet: .*`},
		{"a platform property the provider does not read", `{"tables":{"items":{"platform":{"ydb":{"row_deletion_column":"c","row_deletion_interval":"PT1H","other":"x"}}}}}`,
			`table "items": platform.ydb.other is not supported by the YDB provider yet: .*`},
		{"a TTL without an interval", `{"tables":{"items":{"platform":{"ydb":{"row_deletion_column":"created_at"}}}}}`,
			`table "items": invalid platform.ydb.row_deletion_interval "": a TTL needs the interval after which a row is deleted`},
		{"a TTL without a column", `{"tables":{"items":{"platform":{"ydb":{"row_deletion_interval":"PT1H"}}}}}`,
			`table "items": invalid platform.ydb.row_deletion_column "": a TTL needs the column its interval is measured from`},
		{"a null property", `{"tables":{"items":{"platform":{"ydb":{"row_deletion_column":null,"row_deletion_interval":"PT1H"}}}}}`,
			`table "items": invalid platform.ydb.row_deletion_column "": it is empty; remove it to leave the TTL undeclared`},
		{"an interval in months", `{"tables":{"items":{"platform":{"ydb":{"row_deletion_column":"c","row_deletion_interval":"P1M"}}}}}`,
			`table "items": invalid platform.ydb.row_deletion_interval "P1M": .*`},
		{"an unknown unit", `{"tables":{"items":{"platform":{"ydb":{"row_deletion_column":"c","row_deletion_interval":"PT1H","row_deletion_unit":"hours"}}}}}`,
			`table "items": invalid platform.ydb.row_deletion_unit "hours": .*`},
		{"a group that is not a mapping", `{"tables":{"items":{"platform":{"ydb":"PT1H"}}}}`, `table "items": platform.ydb: expected a mapping of platform properties`},
		{"a value that is not a scalar", `{"tables":{"items":{"platform":{"ydb":{"row_deletion_column":["c"],"row_deletion_interval":"PT1H"}}}}}`,
			`table "items": platform.ydb.row_deletion_column: expected a scalar`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			c := qt.New(t)
			request := yamlRequest(c, test.source)
			tables, err := tablesource.Decode(c.Context(), request.Documents[0])
			c.Assert(err, qt.ErrorMatches, test.wantErr)
			c.Assert(len(tables), qt.Equals, 0)
			decoded, err := backend.New().DecodeSource(c.Context(), request)
			c.Assert(status.Code(err), qt.Equals, codes.InvalidArgument)
			c.Assert(decoded == nil, qt.IsTrue)
		})
	}
}

func TestDecode_NullFamilySettingsAreLeftOut(t *testing.T) {
	c := qt.New(t)
	request := yamlRequest(c, `{"tables":{"items":{"column_families":{"cold":{"data":null,"compression":null,"cache_mode":null,"fields":null},"warm":null}}}}`)
	tables, err := tablesource.Decode(c.Context(), request.Documents[0])
	c.Assert(err, qt.IsNil)
	c.Assert(string(tables[0].Value.CanonicalProjection), qt.Equals, `{"columnFamilies":[{"name":"cold"},{"name":"warm"}]}`)
}

func TestDecodeSource_CanceledWithoutOutput(t *testing.T) {
	c := qt.New(t)
	request := yamlRequest(c, `{"tables":{"items":{"key_bloom_filter":"ENABLED"}}}`)
	ctx, cancel := context.WithCancel(c.Context())
	cancel()
	decoded, err := backend.New().DecodeSource(ctx, request)
	c.Assert(status.Code(err), qt.Equals, codes.Canceled)
	c.Assert(decoded == nil, qt.IsTrue)
	tables, err := tablesource.Decode(ctx, request.Documents[0])
	c.Assert(err, qt.ErrorIs, ctx.Err())
	c.Assert(len(tables), qt.Equals, 0)
}

func TestDecodeSource_DoesNotChangeItsInput(t *testing.T) {
	c := qt.New(t)
	request := yamlRequest(c, `{"tables":{"items":{"key_bloom_filter":"ENABLED","column_families":{"cold":{"data":"hdd"}}}}}`)
	before := proto.CloneOf(request)
	_, err := backend.New().DecodeSource(c.Context(), request)
	c.Assert(err, qt.IsNil)
	c.Assert(proto.Equal(before, request), qt.IsTrue)
}
