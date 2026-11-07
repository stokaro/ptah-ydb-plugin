package yqlschema_test

import (
	"cmp"
	"encoding/json"
	"fmt"
	"path"
	"slices"
	"testing"

	qt "github.com/frankban/quicktest"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/stokaro/ptah/provider-sdk/provider"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb"
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

// schemaRequest reads one YQL document for a host that lists the given
// features of the engine.
func schemaRequest(features []string, text string) *protocol.ReadSQLSchemaRequest {
	engine := backend.Description().GetEngines()[0]
	return &protocol.ReadSQLSchemaRequest{Engine: engine.GetId(), TargetFeatures: features,
		Identifiers: &protocol.ComparisonIdentifiers{Rules: proto.CloneOf(engine.GetIdentifiers()), DefaultSchema: engine.GetDefaultSchema()},
		Documents:   []*protocol.SQLDocument{{Id: "1", Path: "schema.sql", Text: text}}}
}

// withoutFeature is the engine's features less one.
func withoutFeature(feature string) []string {
	return slices.DeleteFunc(slices.Clone(backend.Description().GetEngines()[0].GetFeatures()), func(listed string) bool { return listed == feature })
}

// declaredFacet is a facet as the formats are compared on it: its model, its
// owner and its normalized payload.
type declaredFacet struct{ Kind, Owner, Payload string }

// tableSettingKinds are the models of a table's settings, a column's and a
// column table's storage.
var tableSettingKinds = []string{schema.TableProperties, schema.ColumnProperties, schema.ColumnStorageProperties}

func declaredFacets(envelopes []*protocol.Envelope) []declaredFacet {
	var facets []declaredFacet
	for _, envelope := range envelopes {
		if !slices.Contains(tableSettingKinds, envelope.GetKind()) {
			continue
		}
		owner := envelope.GetOwner()
		parts := append(append([]string{owner.GetSchema()}, owner.GetPath()...), owner.GetName())
		facets = append(facets, declaredFacet{Kind: envelope.GetKind(), Owner: owner.GetKind() + " " + path.Join(parts...), Payload: string(envelope.GetPayload())})
	}
	slices.SortFunc(facets, func(a, b declaredFacet) int {
		return cmp.Or(cmp.Compare(a.Kind, b.Kind), cmp.Compare(a.Owner, b.Owner))
	})
	return facets
}

// yqlFacets reads text for a host that takes the facets of a reading, and
// returns the facets of the complete reply.
func yqlFacets(c *qt.C, text string) []declaredFacet {
	c.Helper()
	request := schemaRequest(backend.Description().GetEngines()[0].GetFeatures(), text)
	reply := yqlschema.Reply(request)
	c.Assert(provider.ValidateSQLSchema(backend.Description(), request, reply), qt.IsNil)
	c.Assert(reply.GetComplete(), qt.IsTrue, qt.Commentf("%v", reply.GetDiagnostics()))
	return declaredFacets(reply.GetFacets())
}

// sourceFacets decodes a YAML or Go source as the source service does and
// returns the facets it declares.
func sourceFacets(c *qt.C, request *protocol.DecodeSourceRequest) []declaredFacet {
	c.Helper()
	decoded, err := backend.New().DecodeSource(c.Context(), request)
	c.Assert(err, qt.IsNil)
	c.Assert(provider.ValidateDecodedSource(backend.Description(), request, decoded), qt.IsNil)
	var envelopes []*protocol.Envelope
	for _, fragment := range decoded.GetDocuments()[0].GetFragments() {
		for _, value := range fragment.GetValues() {
			envelopes = append(envelopes, value.GetValue().GetValue())
		}
	}
	return declaredFacets(envelopes)
}

// goTable declares app/events in a Go file with the given attributes of the
// table directive and columnfamily directives.
func goTable(c *qt.C, table map[string]string, families []map[string]string) *protocol.DecodeSourceRequest {
	c.Helper()
	declarations := []testsource.GoDeclaration{{Directive: "ptah:schema:table", Attributes: table, Struct: "Event", File: "entities.go", FileSize: 1000,
		Start: testsource.GoPosition{Offset: 0, Line: 1, Column: 1}, End: testsource.GoPosition{Offset: 40, Line: 1, Column: 41}}}
	for i, family := range families {
		declarations = append(declarations, testsource.GoDeclaration{Directive: "ptah:schema:columnfamily", Attributes: family, Struct: "Event", File: "entities.go", FileSize: 1000,
			Start: testsource.GoPosition{Offset: 100 * (i + 1), Line: i + 2, Column: 1}, End: testsource.GoPosition{Offset: 100*(i+1) + 40, Line: i + 2, Column: 41}})
	}
	request, err := testsource.GoDirectives([]testsource.GoTable{{Schema: "app", Name: "events", Struct: "Event"}}, declarations)
	c.Assert(err, qt.IsNil)
	return request
}

func yamlTable(c *qt.C, source string) *protocol.DecodeSourceRequest {
	c.Helper()
	request, err := testsource.YAMLTables(source)
	c.Assert(err, qt.IsNil)
	return request
}

// A table's YDB settings read from YQL are the facets a YAML source and a Go
// source of the same table declare, byte for byte: the row-table facet of the
// table, a column facet for each column a family holds, and a column table's
// storage facet.
func TestReply_TableFacetsAgreeWithYAMLAndGo_HappyPath(t *testing.T) {
	tests := []struct {
		name     string
		yql      string
		yaml     string
		table    map[string]string
		families []map[string]string
		kinds    []string
	}{
		{name: "a TTL and a family",
			yql: "CREATE TABLE `app/events` (id Int64 NOT NULL, ts Timestamp, body Utf8 FAMILY payload, PRIMARY KEY (id), FAMILY payload (COMPRESSION = 'lz4')) " +
				"WITH (TTL = Interval('PT1H') ON ts);",
			yaml:     `{"tables": {"events": {"schema": "app", "column_families": {"payload": {"compression": "lz4", "fields": ["body"]}}, "platform": {"ydb": {"row_deletion_column": "ts", "row_deletion_interval": "PT1H"}}}}}`,
			table:    map[string]string{"platform.ydb.row_deletion_column": "ts", "platform.ydb.row_deletion_interval": "PT1H"},
			families: []map[string]string{{"name": "payload", "compression": "lz4", "fields": "body"}},
			kinds:    []string{schema.ColumnProperties, schema.TableProperties}},
		{name: "partitioning, read replicas and the key bloom filter",
			yql: "CREATE TABLE `app/events` (id Int64 NOT NULL, PRIMARY KEY (id)) WITH (AUTO_PARTITIONING_BY_SIZE = ENABLED, AUTO_PARTITIONING_PARTITION_SIZE_MB = 2048, " +
				"AUTO_PARTITIONING_BY_LOAD = DISABLED, AUTO_PARTITIONING_MIN_PARTITIONS_COUNT = 2, AUTO_PARTITIONING_MAX_PARTITIONS_COUNT = 16, READ_REPLICAS_SETTINGS = 'any_az:2', KEY_BLOOM_FILTER = ENABLED);",
			yaml: `{"tables": {"events": {"schema": "app", "auto_partitioning_by_size": "ENABLED", "auto_partitioning_partition_size_mb": 2048, "auto_partitioning_by_load": "DISABLED", ` +
				`"auto_partitioning_min_partitions_count": 2, "auto_partitioning_max_partitions_count": 16, "read_replicas_settings": "ANY_AZ:2", "key_bloom_filter": "ENABLED"}}}`,
			table: map[string]string{"auto_partitioning_by_size": "ENABLED", "auto_partitioning_partition_size_mb": "2048", "auto_partitioning_by_load": "DISABLED",
				"auto_partitioning_min_partitions_count": "2", "auto_partitioning_max_partitions_count": "16", "read_replicas_settings": "ANY_AZ:2", "key_bloom_filter": "ENABLED"},
			kinds: []string{schema.TableProperties}},
		{name: "uniform partitions",
			yql:   "CREATE TABLE `app/events` (id Int64 NOT NULL, PRIMARY KEY (id)) WITH (UNIFORM_PARTITIONS = 4);",
			yaml:  `{"tables": {"events": {"schema": "app", "uniform_partitions": 4}}}`,
			table: map[string]string{"uniform_partitions": "4"},
			kinds: []string{schema.TableProperties}},
		{name: "split points",
			yql:   "CREATE TABLE `app/events` (id Int64 NOT NULL, tag Utf8 NOT NULL, PRIMARY KEY (id, tag)) WITH (PARTITION_AT_KEYS = ((10, 'a'), (20)));",
			yaml:  `{"tables": {"events": {"schema": "app", "partition_at_keys": "(10, 'a'), (20)"}}}`,
			table: map[string]string{"partition_at_keys": "(10, 'a'), (20)"},
			kinds: []string{schema.TableProperties}},
		{name: "a TTL on a column that counts seconds",
			yql:   "CREATE TABLE `app/events` (id Int64 NOT NULL, created Uint64, PRIMARY KEY (id)) WITH (TTL = Interval('P30D') ON created AS SECONDS);",
			yaml:  `{"tables": {"events": {"schema": "app", "platform": {"ydb": {"row_deletion_column": "created", "row_deletion_interval": "P30D", "row_deletion_unit": "SECONDS"}}}}}`,
			table: map[string]string{"platform.ydb.row_deletion_column": "created", "platform.ydb.row_deletion_interval": "P30D", "platform.ydb.row_deletion_unit": "SECONDS"},
			kinds: []string{schema.TableProperties}},
		{name: "families with storage pools and cache modes",
			yql: "CREATE TABLE `app/events` (id Int64 NOT NULL, a Utf8 FAMILY cold, b Utf8 FAMILY hot, c Utf8 FAMILY cold, PRIMARY KEY (id), " +
				"FAMILY hot (DATA = 'ssd', COMPRESSION = 'off', CACHE_MODE = 'in_memory'), FAMILY cold (COMPRESSION = 'LZ4'), FAMILY default (DATA = 'hdd'));",
			yaml: `{"tables": {"events": {"schema": "app", "column_families": {"hot": {"data": "ssd", "compression": "off", "cache_mode": "in_memory", "fields": ["b"]}, ` +
				`"cold": {"compression": "LZ4", "fields": ["a", "c"]}, "default": {"data": "hdd"}}}}}`,
			families: []map[string]string{{"name": "hot", "data": "ssd", "compression": "off", "cache_mode": "in_memory", "fields": "b"},
				{"name": "cold", "compression": "LZ4", "fields": "a, c"}, {"name": "default", "data": "hdd"}},
			kinds: []string{schema.ColumnProperties, schema.ColumnProperties, schema.ColumnProperties, schema.TableProperties}},
		{name: "a column table with a tiered TTL",
			yql: "CREATE TABLE `app/events` (ts Timestamp NOT NULL, id Uint64 NOT NULL, PRIMARY KEY (ts, id)) PARTITION BY HASH (id) WITH (STORE = COLUMN, " +
				"AUTO_PARTITIONING_MIN_PARTITIONS_COUNT = 4, TTL = Interval('PT1H') TO EXTERNAL DATA SOURCE `/local/archive/cold`, Interval('P7D') DELETE ON ts);",
			yaml: `{"tables": {"events": {"schema": "app", "column_store": {"hash_columns": ["id"], "partitions": 4, ` +
				`"ttl": {"column": "ts", "tiers": [{"interval": "PT1H", "external_source": "/local/archive/cold"}, {"interval": "P7D"}]}}}}}`,
			table: map[string]string{"store": "COLUMN", "partition_by_hash": "id", "column_shards": "4",
				"column_ttl": `{"column": "ts", "tiers": [{"interval": "PT1H", "external_source": "/local/archive/cold"}, {"interval": "P7D"}]}`},
			kinds: []string{schema.ColumnStorageProperties}},
		{name: "a column table whose TTL only deletes",
			yql:   "CREATE TABLE `app/events` (ts Timestamp NOT NULL, id Uint64 NOT NULL, PRIMARY KEY (ts, id)) WITH (STORE = COLUMN, TTL = Interval('P1D') ON ts);",
			yaml:  `{"tables": {"events": {"schema": "app", "column_store": {}, "platform": {"ydb": {"row_deletion_column": "ts", "row_deletion_interval": "P1D"}}}}}`,
			table: map[string]string{"store": "COLUMN", "platform.ydb.row_deletion_column": "ts", "platform.ydb.row_deletion_interval": "P1D"},
			kinds: []string{schema.ColumnStorageProperties}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			read := yqlFacets(c, test.yql)
			var kinds []string
			for _, facet := range read {
				kinds = append(kinds, facet.Kind)
			}
			c.Assert(kinds, qt.DeepEquals, test.kinds)
			c.Assert(read, qt.DeepEquals, sourceFacets(c, yamlTable(c, test.yaml)), qt.Commentf("YAML"))
			c.Assert(read, qt.DeepEquals, sourceFacets(c, goTable(c, test.table, test.families)), qt.Commentf("Go"))
		})
	}
}

// The facets of a table's settings reach a reply the SDK accepts, each traced
// to the CREATE TABLE that declares it, and owned by the table or by the
// column a family holds.
func TestReply_TableFacets_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	request := schemaRequest(backend.Description().GetEngines()[0].GetFeatures(), "CREATE TABLE `app/items` (id Int64 NOT NULL, PRIMARY KEY (id));\n"+
		"CREATE TABLE `app/events` (id Int64 NOT NULL, ts Timestamp, body Utf8 FAMILY payload, PRIMARY KEY (id), FAMILY payload (COMPRESSION = 'lz4')) "+
		"WITH (TTL = Interval('PT1H') ON ts);\n")

	reply := yqlschema.Reply(request)

	c.Assert(provider.ValidateSQLSchema(backend.Description(), request, reply), qt.IsNil)
	c.Assert(reply.GetComplete(), qt.IsTrue, qt.Commentf("%v", reply.GetDiagnostics()))
	c.Assert(reply.GetFacets(), qt.HasLen, 2)
	c.Assert(proto.Equal(reply.GetFacets()[0].GetOwner(), &protocol.Reference{Engine: "ydb", Kind: "table", Schema: "app", Name: "events"}), qt.IsTrue)
	var table native.RowTableIntent
	c.Assert(protojson.Unmarshal(reply.GetFacets()[0].GetPayload(), &table), qt.IsNil)
	c.Assert(proto.Equal(&table, &native.RowTableIntent{
		ColumnFamilies: []*native.ColumnFamily{{Name: "payload", Compression: Ydb_Table.ColumnFamily_COMPRESSION_LZ4}},
		TtlSettings: &Ydb_Table.TtlSettings{Mode: &Ydb_Table.TtlSettings_DateTypeColumn{
			DateTypeColumn: &Ydb_Table.DateTypeColumnModeSettings{ColumnName: "ts", ExpireAfterSeconds: 3600}}},
	}), qt.IsTrue, qt.Commentf("%s", reply.GetFacets()[0].GetPayload()))
	c.Assert(proto.Equal(reply.GetFacets()[1].GetOwner(), &protocol.Reference{Engine: "ydb", Kind: "column", Schema: "app", Path: []string{"events"}, Name: "body"}), qt.IsTrue)
	c.Assert(string(reply.GetFacets()[1].GetPayload()), qt.Equals, `{"family":"payload"}`)
	var traced []uint32
	for _, trace := range reply.GetTraces() {
		traced = append(traced, map[bool]uint32{true: trace.GetSource().GetSpan().GetStart().GetLine(), false: 0}[trace.GetCollection() == protocol.SQLObjectTrace_FACETS])
	}
	c.Assert(slices.DeleteFunc(traced, func(line uint32) bool { return line == 0 }), qt.DeepEquals, []uint32{2, 2})
	c.Assert(claimedKinds(reply), qt.Contains, schema.TableProperties)
	c.Assert(claimedKinds(reply), qt.Contains, schema.ColumnProperties)
}

// What a table's settings cannot carry is refused by name, with no schema: a
// host that does not take the facets of a reading has no home for any of
// them, and a column table holds no family. An index's settings are
// TestReply_IndexSettingsFacet_FailurePath's.
func TestReply_TableFacets_FailurePath(t *testing.T) {
	tests := []struct {
		name     string
		features []string
		text     string
		want     string
	}{
		{name: "a host without facets", features: withoutFeature(provider.SQLFacetsFeature),
			text: "CREATE TABLE `app/events` (id Int64 NOT NULL, ts Timestamp, body Utf8 FAMILY payload, PRIMARY KEY (id), FAMILY payload (COMPRESSION = 'lz4')) WITH (TTL = Interval('PT1H') ON ts);",
			want: "table `app/events` declares column families, a TTL, which a SQL schema reading has no home for: " +
				"the host does not take the facets of a table in a SQL schema reading (schema.sql-facets)"},
		{name: "a family on a column table", features: backend.Description().GetEngines()[0].GetFeatures(),
			text: "CREATE TABLE `app/events` (ts Timestamp NOT NULL, body Utf8 FAMILY payload, PRIMARY KEY (ts), FAMILY payload (COMPRESSION = 'lz4')) WITH (STORE = COLUMN);",
			want: "table `app/events`: " + schema.ColumnTableRowSettingsRefusal},
		{name: "both starting layouts", features: backend.Description().GetEngines()[0].GetFeatures(),
			text: "CREATE TABLE `app/events` (id Int64 NOT NULL, PRIMARY KEY (id)) WITH (UNIFORM_PARTITIONS = 4, PARTITION_AT_KEYS = (10, 20));",
			want: "table `app/events`: uniform_partitions and partition_at_keys are both declared, which YDB refuses " +
				"(`Uniform partitions and partitions at keys settings are mutually exclusive`)"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			request := schemaRequest(test.features, test.text)
			negotiated := provider.NegotiatedDescription(backend.Description(), slices.DeleteFunc(slices.Clone(provider.TwoWayFeatures()), func(feature string) bool {
				return feature == provider.SQLFacetsFeature && !slices.Contains(test.features, feature)
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

// hostTableFacets are the facets a complete reply gives its tables, written as
// the host's reader writes the same settings: by table, the partitioning, the
// families with the columns the column facets place in them, the TTL, and a
// column table's storage, whose TTL is the table's TTL when it only deletes.
// Intervals are written as whole seconds, as normalTableFacets writes the
// host's.
func hostTableFacets(c *qt.C, reply *protocol.SQLSchema) map[string]map[string]any {
	c.Helper()
	placed := make(map[string]map[string][]string)
	for _, facet := range reply.GetFacets() {
		if facet.GetKind() != schema.ColumnProperties {
			continue
		}
		var intent native.ColumnIntent
		c.Assert(protojson.Unmarshal(facet.GetPayload(), &intent), qt.IsNil)
		owner := facet.GetOwner()
		table := canonical(owner.GetSchema(), owner.GetPath()[0])
		if placed[table] == nil {
			placed[table] = make(map[string][]string)
		}
		placed[table][intent.GetFamily()] = append(placed[table][intent.GetFamily()], owner.GetName())
	}
	result := make(map[string]map[string]any)
	for _, facet := range reply.GetFacets() {
		owner := facet.GetOwner()
		table := canonical(owner.GetSchema(), owner.GetName())
		switch facet.GetKind() {
		case schema.TableProperties:
			var intent native.RowTableIntent
			c.Assert(protojson.Unmarshal(facet.GetPayload(), &intent), qt.IsNil)
			result[table] = hostRowSettings(c, &intent, placed[table])
		case schema.ColumnStorageProperties:
			var intent native.ColumnStorageIntent
			c.Assert(protojson.Unmarshal(facet.GetPayload(), &intent), qt.IsNil)
			result[table] = hostColumnStorage(c, &intent)
		}
	}
	return result
}

func hostRowSettings(c *qt.C, intent *native.RowTableIntent, placed map[string][]string) map[string]any {
	c.Helper()
	facets := make(map[string]any)
	settings := intent.GetPartitioningSettings()
	partitioning := ydbdecl.TablePartitioning{BySize: switchOf(settings.GetPartitioningBySize()), PartitionSizeMB: settings.GetPartitionSizeMb(),
		ByLoad: switchOf(settings.GetPartitioningByLoad()), MinPartitions: settings.GetMinPartitionsCount(), MaxPartitions: settings.GetMaxPartitionsCount(),
		KeyBloomFilter: switchOf(intent.GetKeyBloomFilter()), UniformPartitions: intent.GetUniformPartitions()}
	switch replicas := intent.GetReadReplicasSettings().GetSettings().(type) {
	case *Ydb_Table.ReadReplicasSettings_PerAzReadReplicasCount:
		partitioning.ReadReplicas = fmt.Sprintf("PER_AZ:%d", replicas.PerAzReadReplicasCount)
	case *Ydb_Table.ReadReplicasSettings_AnyAzReadReplicasCount:
		partitioning.ReadReplicas = fmt.Sprintf("ANY_AZ:%d", replicas.AnyAzReadReplicasCount)
	}
	for _, point := range intent.GetPartitionAtKeys() {
		partitioning.PartitionAtKeys = append(partitioning.PartitionAtKeys, point.GetValues())
	}
	if encoded := string(marshal(c, partitioning)); encoded != "{}" {
		facets["ptah.run/ydb/table-partitioning"] = decodeAny(c, []byte(encoded))
	}
	var families []ydbdecl.ColumnFamily
	for _, family := range intent.GetColumnFamilies() {
		families = append(families, ydbdecl.ColumnFamily{Name: family.GetName(), Data: family.GetData().GetMedia(),
			Compression: map[Ydb_Table.ColumnFamily_Compression]string{Ydb_Table.ColumnFamily_COMPRESSION_NONE: ydbdecl.CompressionOff, Ydb_Table.ColumnFamily_COMPRESSION_LZ4: ydbdecl.CompressionLZ4}[family.GetCompression()],
			CacheMode:   map[native.ColumnFamily_CacheMode]string{native.ColumnFamily_CACHE_MODE_REGULAR: ydbdecl.CacheModeRegular, native.ColumnFamily_CACHE_MODE_IN_MEMORY: ydbdecl.CacheModeInMemory}[family.GetCacheMode()],
			Columns:     placed[family.GetName()]})
	}
	if len(families) != 0 {
		facets["ptah.run/ydb/column-families"] = decodeAny(c, marshal(c, ydbdecl.ColumnFamilies{Families: families}))
	}
	switch mode := intent.GetTtlSettings().GetMode().(type) {
	case *Ydb_Table.TtlSettings_DateTypeColumn:
		facets["ptah.run/ydb/ttl"] = decodeAny(c, marshal(c, ydbdecl.TTL{Column: mode.DateTypeColumn.GetColumnName(), Interval: seconds(uint64(mode.DateTypeColumn.GetExpireAfterSeconds()))}))
	case *Ydb_Table.TtlSettings_ValueSinceUnixEpoch:
		facets["ptah.run/ydb/ttl"] = decodeAny(c, marshal(c, ydbdecl.TTL{Column: mode.ValueSinceUnixEpoch.GetColumnName(),
			Interval: seconds(uint64(mode.ValueSinceUnixEpoch.GetExpireAfterSeconds())), Unit: unitName(uint64(mode.ValueSinceUnixEpoch.GetColumnUnit()))})) // #nosec G115 -- a column unit enum number, 0 to 4
	}
	return facets
}

func hostColumnStorage(c *qt.C, intent *native.ColumnStorageIntent) map[string]any {
	c.Helper()
	facets := make(map[string]any)
	store := ydbdecl.ColumnStore{HashColumns: intent.GetHashColumns(), Partitions: intent.GetPartitions()}
	enabled := intent.GetTtl().GetEnabled()
	tiers := enabled.GetTiers()
	if len(tiers) == 1 && tiers[0].GetDelete() != nil {
		facets["ptah.run/ydb/ttl"] = decodeAny(c, marshal(c, ydbdecl.TTL{Column: enabled.GetColumn(), Interval: seconds(tiers[0].GetApplyAfterSeconds()), Unit: unitName(enabled.GetColumnUnit())}))
		tiers = nil
	}
	if len(tiers) != 0 {
		store.TTL = &ydbdecl.TieredTTL{Column: enabled.GetColumn(), Unit: unitName(enabled.GetColumnUnit())}
		for _, tier := range tiers {
			store.TTL.Tiers = append(store.TTL.Tiers, ydbdecl.TTLTier{Interval: seconds(tier.GetApplyAfterSeconds()), ExternalSource: tier.GetExternalStorage()})
		}
	}
	facets["ptah.run/ydb/column-store"] = decodeAny(c, marshal(c, store))
	return facets
}

// switchOf reads a status as the host's payload writes a switch.
func switchOf(status Ydb.FeatureFlag_Status) *bool {
	return map[Ydb.FeatureFlag_Status]*bool{Ydb.FeatureFlag_ENABLED: new(true), Ydb.FeatureFlag_DISABLED: new(false)}[status]
}

// unitName is an epoch unit as YQL spells it, empty for a date or time column.
func unitName(unit uint64) string {
	return map[uint64]string{1: ydbdecl.Seconds, 2: ydbdecl.Milliseconds, 3: ydbdecl.Microseconds, 4: ydbdecl.Nanoseconds}[unit]
}

// seconds writes an interval as its whole seconds, the one spelling both
// readings are compared in.
func seconds(n uint64) string { return fmt.Sprintf("PT%dS", n) }

// normalTableFacets writes the intervals of the host's TTLs as whole seconds,
// so a reply is held to their meaning rather than to their spelling: PT1H and
// PT60M are one interval.
func normalTableFacets(c *qt.C, m meaning) meaning {
	c.Helper()
	interval := func(written string) string {
		n, err := ydbdecl.IntervalSeconds(written)
		c.Assert(err, qt.IsNil)
		return seconds(n)
	}
	for _, facets := range m.TableFacets {
		if held, ok := facets["ptah.run/ydb/ttl"]; ok {
			var ttl ydbdecl.TTL
			c.Assert(json.Unmarshal(marshal(c, held), &ttl), qt.IsNil)
			ttl.Interval = interval(ttl.Interval)
			facets["ptah.run/ydb/ttl"] = decodeAny(c, marshal(c, ttl))
		}
		if held, ok := facets["ptah.run/ydb/column-store"]; ok {
			var store ydbdecl.ColumnStore
			c.Assert(json.Unmarshal(marshal(c, held), &store), qt.IsNil)
			if store.TTL != nil {
				for i := range store.TTL.Tiers {
					store.TTL.Tiers[i].Interval = interval(store.TTL.Tiers[i].Interval)
				}
			}
			facets["ptah.run/ydb/column-store"] = decodeAny(c, marshal(c, store))
		}
	}
	return m
}
