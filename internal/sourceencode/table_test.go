package sourceencode_test

import (
	"bytes"
	"testing"

	qt "github.com/frankban/quicktest"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/testing/protocmp"

	"github.com/stokaro/ptah-ydb-plugin/internal/changefeeds"
	"github.com/stokaro/ptah-ydb-plugin/internal/changefeedsource"
	native "github.com/stokaro/ptah-ydb-plugin/internal/nativeprotocol"
	"github.com/stokaro/ptah-ydb-plugin/internal/sourceencode"
	"github.com/stokaro/ptah-ydb-plugin/internal/tablesource"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
	"github.com/stokaro/ptah-ydb-plugin/internal/ydbdecl"
	"github.com/stokaro/ptah-ydb-plugin/schema"
)

// binding is the table the facets of a table test belong to.
var binding = &protocol.SourceBinding{Id: "table-0", EntryKey: "events", StructName: "Events",
	Object: &protocol.Reference{Engine: "ydb", Kind: "table", Schema: "app", Name: "events"}}

// facet is one desired facet of the binding's table, or of its column when
// column is not empty, with the payload given.
type facet struct {
	id, kind, column, payload string
}

func facetValue(c *qt.C, spec facet) *protocol.EncodeSourceValue {
	c.Helper()
	owner := proto.CloneOf(binding.Object)
	model := schema.NativeDesiredModel(spec.kind)
	switch spec.kind {
	case schema.TableProperties:
		model = schema.TableDesiredModel()
	case schema.ColumnProperties:
		model = schema.ColumnDesiredModel()
	}
	if spec.column != "" {
		owner.Kind, owner.Path, owner.Name = "column", []string{binding.Object.Name}, spec.column
	}
	value, err := schema.NormalizeDesired(&protocol.Envelope{ProviderId: schema.ProviderID, Kind: model.Kind, SchemaVersion: model.SchemaVersion,
		Encoding: model.Encoding, DefinitionSha256: model.DefinitionSha256, Owner: owner, Payload: []byte(spec.payload)})
	c.Assert(err, qt.IsNil)
	return &protocol.EncodeSourceValue{Placement: protocol.SourceModelValue_FACET, BindingId: binding.Id, Value: &protocol.NormalizedValue{Id: spec.id,
		Value: value, NormalizedOwner: proto.CloneOf(owner), CanonicalFormat: model.CanonicalFormat, CanonicalProjection: bytes.Clone(value.Payload)}}
}

func table(c *qt.C, facets ...facet) sourceencode.Table {
	c.Helper()
	encoded := sourceencode.Table{Binding: binding}
	for _, spec := range facets {
		encoded.Values = append(encoded.Values, facetValue(c, spec))
	}
	return encoded
}

// decodedTable is what the table decoders read back from the parts.
type decodedTable struct {
	row, storage string
	columns      map[string]string
	feeds        []changefeeds.Feed
}

func decode(c *qt.C, format protocol.SourceDocument_Format, parts []*protocol.EncodedSourcePart) decodedTable {
	c.Helper()
	document := &protocol.SourceDocument{Id: "export", Format: format, Bindings: []*protocol.SourceBinding{binding}}
	for _, part := range parts {
		document.Fragments = append(document.Fragments, part.Fragment)
	}
	tables, err := tablesource.Decode(c.Context(), document)
	c.Assert(err, qt.IsNil)
	result := decodedTable{columns: make(map[string]string)}
	for _, decoded := range tables {
		payload := string(decoded.Value.Value.Payload)
		result.row, result.storage = map[bool]string{true: payload}[decoded.Value.Value.Kind == schema.TableProperties],
			map[bool]string{true: payload}[decoded.Value.Value.Kind == schema.ColumnStorageProperties]
		for _, column := range decoded.Columns {
			var intent native.ColumnIntent
			c.Assert(protojson.Unmarshal(column.Value.Payload, &intent), qt.IsNil)
			result.columns[column.Value.Owner.Name] = intent.Family
		}
	}
	feeds, err := changefeedsource.Decode(c.Context(), document)
	c.Assert(err, qt.IsNil)
	for _, decoded := range feeds {
		var set native.ChangefeedSetIntent
		c.Assert(protojson.Unmarshal(decoded.Value.Value.Payload, &set), qt.IsNil)
		for _, intent := range set.Changefeeds {
			feed, err := changefeeds.FromIntent(intent)
			c.Assert(err, qt.IsNil)
			result.feeds = append(result.feeds, feed)
		}
	}
	return result
}

// canonical is a desired row-table or column-storage payload as the model
// normalizes it.
func canonical(c *qt.C, kind, payload string) string {
	c.Helper()
	if payload == "" {
		return ""
	}
	return string(facetValue(c, facet{id: "want", kind: kind, payload: payload}).Value.Value.Payload)
}

// A row table as a read converts it: the families with the column one of
// them holds, the partitioning YDB reports, a key Bloom filter, read
// replicas and a TTL, and the facets a read reports for every table.
const (
	readRow = `{"primaryKey":["id"],"columnFamilies":[{"name":"default","compression":"COMPRESSION_NONE"},{"name":"cold","compression":"COMPRESSION_LZ4","data":{"media":"ssd"}}],` +
		`"partitioningSettings":{"partitioningBySize":"ENABLED","partitionSizeMb":"2048","partitioningByLoad":"ENABLED","minPartitionsCount":"2","maxPartitionsCount":"16"},` +
		`"keyBloomFilter":"ENABLED","readReplicasSettings":{"anyAzReadReplicasCount":"1"},"ttlSettings":{"dateTypeColumn":{"columnName":"created_at","expireAfterSeconds":86400}}}`
	// readFeeds is a changefeed set as a read reports it, with what YDB keeps
	// on the changefeed's topic and its consumer.
	readFeeds = `{"changefeeds":[{"format":"FORMAT_JSON","mode":"MODE_UPDATES","name":"updates","virtualTimestamps":true,"topic":{"attributes":{"_allow_unauthenticated_read":"true"},` +
		`"access":{"owner":"root@builtin"},"partitionWriteBurstBytes":"1048576","partitionWriteSpeedBytesPerSecond":"1048576","retentionPeriod":"172800s",` +
		`"partitioningSettings":{"autoPartitioningSettings":{"partitionWriteSpeed":{"downUtilizationPercent":20,"stabilizationWindow":"300s","upUtilizationPercent":80},` +
		`"strategy":"AUTO_PARTITIONING_STRATEGY_DISABLED"},"maxActivePartitions":"1","minActivePartitions":"1"},` +
		`"consumers":[{"name":"audit","important":true,"attributes":{"_service_type":"data-streams"}}]}}]}`
	// declaredRow is readRow without its primary key, which the common
	// columns carry.
	declaredRow = `{"columnFamilies":[{"name":"default","compression":"COMPRESSION_NONE"},{"name":"cold","compression":"COMPRESSION_LZ4","data":{"media":"ssd"}}],` +
		`"partitioningSettings":{"partitioningBySize":"ENABLED","partitionSizeMb":"2048","partitioningByLoad":"ENABLED","minPartitionsCount":"2","maxPartitionsCount":"16"},` +
		`"keyBloomFilter":"ENABLED","readReplicasSettings":{"anyAzReadReplicasCount":"1"},"ttlSettings":{"dateTypeColumn":{"columnName":"created_at","expireAfterSeconds":86400}}}`
)

// The facets of a table are written so that the table decoders give each one
// back: the row settings without the key the common columns carry, each
// column in the family that holds it, a column table's storage, and the
// changefeeds as the changefeed comparison reads them. A facet a source
// states by writing nothing is written too, and decodes to nothing.
func TestEncodeTable_HappyPath(t *testing.T) {
	updates := changefeeds.Feed{Changefeed: ydbdecl.Changefeed{Name: "updates", Mode: "UPDATES", Format: "JSON", VirtualTimestamps: true, RetentionPeriod: "P2D",
		Consumers: []ydbdecl.Consumer{{Name: "audit", Important: true}}}}
	for _, test := range []struct {
		name         string
		facets       []facet
		row, storage string
		columns      map[string]string
		feeds        []changefeeds.Feed
		written      []string
	}{{
		name: "row table read",
		facets: []facet{{"row", schema.TableProperties, "", readRow}, {"payload", schema.ColumnProperties, "payload", `{"family":"cold"}`},
			{"id", schema.ColumnProperties, "id", `{}`}, {"label", schema.ColumnProperties, "label", `{"literal":{"type":{"typeId":"UTF8"},"value":{"textValue":"x"}}}`},
			{"feeds", schema.ChangefeedProperties, "", readFeeds}, {"indexes", schema.IndexProperties, "", `{}`}},
		row: declaredRow, columns: map[string]string{"payload": "cold"}, feeds: []changefeeds.Feed{updates},
		written: []string{"row", "feeds", "payload", "id", "label", "indexes"},
	}, {
		name:    "epoch TTL and a starting layout",
		facets:  []facet{{"row", schema.TableProperties, "", `{"uniformPartitions":"4","ttlSettings":{"valueSinceUnixEpoch":{"columnName":"at","columnUnit":"UNIT_MILLISECONDS","expireAfterSeconds":3600}}}`}},
		row:     `{"uniformPartitions":"4","ttlSettings":{"valueSinceUnixEpoch":{"columnName":"at","columnUnit":"UNIT_MILLISECONDS","expireAfterSeconds":3600}}}`,
		columns: make(map[string]string), written: []string{"row"},
	}, {
		name: "family cache modes",
		facets: []facet{{"row", schema.TableProperties, "", `{"columnFamilies":[{"name":"hot","cacheMode":"CACHE_MODE_IN_MEMORY"},` +
			`{"name":"warm","compression":"COMPRESSION_LZ4","cacheMode":"CACHE_MODE_REGULAR"}]}`}, {"payload", schema.ColumnProperties, "payload", `{"family":"hot"}`}},
		row:     `{"columnFamilies":[{"name":"hot","cacheMode":"CACHE_MODE_IN_MEMORY"},{"name":"warm","compression":"COMPRESSION_LZ4","cacheMode":"CACHE_MODE_REGULAR"}]}`,
		columns: map[string]string{"payload": "hot"}, written: []string{"row", "payload"},
	}, {
		name:    "split points",
		facets:  []facet{{"row", schema.TableProperties, "", `{"partitionAtKeys":[{"values":["10"]},{"values":["it's","a,b"]}],"readReplicasSettings":{"perAzReadReplicasCount":"0"}}`}},
		row:     `{"partitionAtKeys":[{"values":["10"]},{"values":["it's","a,b"]}],"readReplicasSettings":{"perAzReadReplicasCount":"0"}}`,
		columns: make(map[string]string), written: []string{"row"},
	}, {
		name: "column table with a tiered TTL",
		facets: []facet{{"storage", schema.ColumnStorageProperties, "", `{"hashColumns":["id","at"],"partitions":"8","ttl":{"enabled":{"column":"at","tiers":[` +
			`{"applyAfterSeconds":"3600","externalStorage":"/local/app/cold"},{"applyAfterSeconds":"86400","delete":{}}]}}}`}, {"id", schema.ColumnProperties, "id", `{}`}},
		storage: `{"hashColumns":["id","at"],"partitions":"8","ttl":{"enabled":{"column":"at","tiers":[{"applyAfterSeconds":"3600","externalStorage":"/local/app/cold"},{"applyAfterSeconds":"86400","delete":{}}]}}}`,
		columns: make(map[string]string), written: []string{"storage", "id"},
	}, {
		name:    "column table with a TTL that only deletes",
		facets:  []facet{{"storage", schema.ColumnStorageProperties, "", `{"ttl":{"enabled":{"column":"at","columnUnit":"1","tiers":[{"applyAfterSeconds":"60","delete":{}}]}}}`}},
		storage: `{"ttl":{"enabled":{"column":"at","columnUnit":"1","tiers":[{"applyAfterSeconds":"60","delete":{}}]}}}`,
		columns: make(map[string]string), written: []string{"storage"},
	}, {
		name:    "only what a source states by writing nothing",
		facets:  []facet{{"feeds", schema.ChangefeedProperties, "", `{}`}, {"indexes", schema.IndexProperties, "", `{}`}, {"id", schema.ColumnProperties, "id", `{}`}},
		columns: make(map[string]string), written: []string{"feeds", "indexes", "id"},
	}, {
		// The read reports a Serial's start and increment on the common
		// column, which the host writes; the facet adds nothing.
		name:    "a Serial's sequence, which the common column writes",
		facets:  []facet{{"id", schema.ColumnProperties, "id", `{"sequence":{"startValue":"5"}}`}, {"seq", schema.ColumnProperties, "seq", `{"sequence":{}}`}},
		columns: make(map[string]string), written: []string{"id", "seq"},
	}} {
		for _, format := range []protocol.SourceDocument_Format{protocol.SourceDocument_YAML, protocol.SourceDocument_GO_ANNOTATIONS} {
			t.Run(test.name+"/"+format.String(), func(t *testing.T) {
				c := testcheck.New(t)
				input := table(c, test.facets...)
				before := proto.CloneOf(input.Values[0])
				result, err := sourceencode.EncodeTable(format, input)
				c.Assert(err, qt.IsNil)
				c.Assert(result.Losses, qt.HasLen, 0)
				c.Assert(result.Written, qt.DeepEquals, test.written)
				c.Assert(proto.Equal(input.Values[0], before), qt.IsTrue)
				decoded := decode(c, format, result.Parts)
				c.Assert(decoded.row, qt.Equals, canonical(c, schema.TableProperties, test.row))
				c.Assert(decoded.storage, qt.Equals, canonical(c, schema.ColumnStorageProperties, test.storage))
				c.Assert(decoded.columns, qt.DeepEquals, test.columns)
				c.Assert(decoded.feeds, qt.DeepEquals, test.feeds)
			})
		}
	}
}

// A Go export writes the row settings as attributes of the table's own
// directive, a family as a directive of its own, and a changefeed and its
// consumer as two directives; YAML writes each setting as a key of the table
// entry and the TTL in its ydb platform group.
func TestEncodeTable_WritesTheProvidersGrammar(t *testing.T) {
	for _, test := range []struct {
		format protocol.SourceDocument_Format
		want   []string
	}{
		{protocol.SourceDocument_GO_ANNOTATIONS, []string{
			"ptah:schema:table auto_partitioning_by_load=ENABLED key_bloom_filter=ENABLED platform.ydb.row_deletion_column=created_at platform.ydb.row_deletion_interval=P1D",
			"ptah:schema:columnfamily name=cold compression=lz4 fields=payload",
			"ptah:schema:changefeed format=JSON mode=UPDATES name=updates retention_period=P2D virtual_timestamps=true",
			"ptah:schema:changefeed:consumer changefeed=updates important=true name=audit",
		}},
		{protocol.SourceDocument_YAML, []string{
			"auto_partitioning_by_load=ENABLED", "key_bloom_filter=ENABLED", "platform.ydb=row_deletion_column=created_at row_deletion_interval=P1D",
			"column_families=cold=compression=lz4 fields=payload", "changefeeds=updates=format=JSON mode=UPDATES retention_period=P2D virtual_timestamps=true consumers=audit=important=true",
		}},
	} {
		t.Run(test.format.String(), func(t *testing.T) {
			c := testcheck.New(t)
			result, err := sourceencode.EncodeTable(test.format, table(c,
				facet{"row", schema.TableProperties, "", `{"columnFamilies":[{"name":"cold","compression":"COMPRESSION_LZ4"}],"partitioningSettings":{"partitioningByLoad":"ENABLED"},` +
					`"keyBloomFilter":"ENABLED","ttlSettings":{"dateTypeColumn":{"columnName":"created_at","expireAfterSeconds":86400}}}`},
				facet{"payload", schema.ColumnProperties, "payload", `{"family":"cold"}`}, facet{"feeds", schema.ChangefeedProperties, "", readFeeds}))
			c.Assert(err, qt.IsNil)
			c.Assert(result.Losses, qt.HasLen, 0)
			var written []string
			for _, part := range result.Parts {
				written = append(written, spelled(part.Fragment))
			}
			c.Assert(written, qt.DeepEquals, test.want)
		})
	}
}

// spelled writes a fragment in one line: a Go directive with its attributes,
// a YAML key with its value.
func spelled(fragment *protocol.SourceFragment) string {
	if directive := fragment.GetGoDirective(); directive != nil {
		return directive.GetDirective() + " " + spelledValue(fragment.GetValue())
	}
	return fragment.GetYamlKey().GetKey() + "=" + spelledValue(fragment.GetValue())
}

func spelledValue(value *protocol.SourceValue) string {
	var b bytes.Buffer
	for i, entry := range value.GetMapping().GetEntries() {
		if i > 0 {
			b.WriteString(" ")
		}
		b.WriteString(entry.Key + "=" + spelledValue(entry.Value))
	}
	for i, item := range value.GetSequence().GetValues() {
		if i > 0 {
			b.WriteString(",")
		}
		b.WriteString(spelledValue(item))
	}
	b.WriteString(value.GetText() + value.GetNumber())
	return b.String()
}

// What no declaration states is a loss for its facet, named in one line, and
// the table's other facets are still written. A setting the writer has no
// spelling for and no rule names, such as a TTL past the longest interval a
// source spells or a column table's data channels, is lost because the parts do not decode back
// to it.
func TestEncodeTable_FailurePath(t *testing.T) {
	for _, test := range []struct {
		name    string
		lost    facet
		message string
	}{
		{"access", facet{"lost", schema.TableProperties, "", `{"access":{"owner":"alice"}}`}, "table app/events: its access requirement has no source spelling"},
		{"attributes", facet{"lost", schema.TableProperties, "", `{"attributes":{"team":"core"}}`}, "table app/events: its attributes have no source spelling"},
		{"storage settings", facet{"lost", schema.TableProperties, "", `{"storageSettings":{"storeExternalBlobs":"DISABLED"}}`}, "table app/events: its storage settings have no source spelling"},
		{"tiering", facet{"lost", schema.TableProperties, "", `{"tiering":"cold"}`}, "table app/events: its tiering has no source spelling"},
		{"temporary", facet{"lost", schema.TableProperties, "", `{"temporary":true}`}, "table app/events: its temporary lifetime has no source spelling"},
		{"partition columns", facet{"lost", schema.TableProperties, "", `{"partitioningSettings":{"partitionBy":["id"]}}`}, "table app/events: its partition columns have no source spelling"},
		{"TTL run interval", facet{"lost", schema.TableProperties, "", `{"ttlSettings":{"dateTypeColumn":{"columnName":"at"},"runIntervalSeconds":60}}`}, "table app/events: its TTL run interval has no source spelling"},
		{"keep_in_memory", facet{"lost", schema.TableProperties, "", `{"columnFamilies":[{"name":"hot","keepInMemory":"ENABLED"}]}`}, "table app/events: the keep_in_memory of its column family hot has no source spelling"},
		{"TTL past what an interval spells", facet{"lost", schema.TableProperties, "", `{"ttlSettings":{"dateTypeColumn":{"columnName":"at","expireAfterSeconds":4294967295}}}`},
			"table app/events: its YDB settings cannot be written so that a source reads them back unchanged"},
		{"column table data channels", facet{"lost", schema.ColumnStorageProperties, "", `{"dataChannelCount":"4"}`},
			"table app/events: its YDB settings cannot be written so that a source reads them back unchanged"},
		{"compression", facet{"lost", schema.ColumnProperties, "id", `{"compression":{"algorithm":"ALGORITHM_ZSTD"}}`}, "column app/events/id: its compression or encoding has no source spelling"},
		{"undeclared family", facet{"lost", schema.ColumnProperties, "id", `{"family":"cold"}`}, "column app/events/id sits in family cold, which no family the source declares for the table holds"},
		{"native index", facet{"lost", schema.IndexProperties, "", nativeIndex}, "table app/events: its native indexes (by_body) have no source spelling the provider writes"},
		{"disabled changefeed", facet{"lost", schema.ChangefeedProperties, "", `{"changefeeds":[{"name":"updates","mode":"MODE_UPDATES","format":"FORMAT_JSON","disabled":true,` + feedTopic + `}]}`},
			`table app/events: its changefeeds have no source spelling: changefeed "updates" is disabled, and no statement disables one`},
		{"replication stream", facet{"lost", schema.ChangefeedProperties, "", `{"changefeeds":[{"name":"updates","mode":"MODE_UPDATES","format":"FORMAT_JSON","attributes":{"__async_replication":` + replicationBinding + `},` + feedTopic + `}]}`},
			`table app/events: its changefeeds have no source spelling: changefeed "updates" is the stream of an async replication, which a source does not declare`},
	} {
		for _, format := range []protocol.SourceDocument_Format{protocol.SourceDocument_YAML, protocol.SourceDocument_GO_ANNOTATIONS} {
			t.Run(test.name+"/"+format.String(), func(t *testing.T) {
				c := testcheck.New(t)
				result, err := sourceencode.EncodeTable(format, table(c, test.lost, facet{"kept", schema.ChangefeedProperties, "", `{}`}))
				c.Assert(err, qt.IsNil)
				c.Assert(result.Losses, qt.CmpEquals(protocmp.Transform()), []*protocol.EncodeLoss{{ValueId: "lost", Message: test.message}})
				c.Assert(result.Written, qt.DeepEquals, []string{"kept"})
				c.Assert(result.Parts, qt.Not(qt.HasLen), 0)
			})
		}
	}
}

// nativeIndex is a native index set as a read reports a global index, with
// its implementation table; no source of the provider spells one.
const nativeIndex = `{"indexes":[{"implementation":[{"columns":[{"name":"body","settings":{},"type":{"optionalType":{"item":{"typeId":"UTF8"}}}},` +
	`{"name":"id","settings":{},"type":{"typeId":"UINT64"}}],"name":"indexImplTable","settings":{"columnFamilies":[{"compression":"COMPRESSION_NONE","name":"default"}],` +
	`"partitioningSettings":{"minPartitionsCount":"1","partitionSizeMb":"2048","partitioningByLoad":"DISABLED","partitioningBySize":"ENABLED"},"primaryKey":["body","id"]}}],` +
	`"specification":{"globalIndex":{},"indexColumns":["body"],"name":"by_body"}}]}`

// feedTopic is the topic of a changefeed that states nothing.
const feedTopic = `"topic":{"partitioningSettings":{"autoPartitioningSettings":{"strategy":"AUTO_PARTITIONING_STRATEGY_DISABLED"},"minActivePartitions":"1"},"retentionPeriod":"86400s"}`

// replicationBinding is the attribute an async replication writes on the
// stream it creates, as a JSON string.
const replicationBinding = `"{\"path\":\"/local/app/replica\",\"id\":\"1\",\"supports_topic_autopartitioning\":false}"`
