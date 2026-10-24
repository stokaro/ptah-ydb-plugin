package tablesource_test

import (
	"bytes"
	"encoding/json"
	"slices"
	"strconv"
	"strings"

	qt "github.com/frankban/quicktest"
	native "github.com/stokaro/ptah-ydb-plugin/internal/nativeprotocol"
	"github.com/stokaro/ptah-ydb-plugin/internal/ttlgrammar"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb_Table"
	"google.golang.org/protobuf/encoding/protojson"
)

// meaning is what a table's declared partitioning, TTL and column families say,
// stripped of how a decoder spells them. Both decoders are reduced to it, so a
// difference in spelling cannot hide a difference in effect, and the other way.
type meaning struct {
	BySize          *bool
	PartitionSizeMB uint64
	ByLoad          *bool
	MinPartitions   uint64
	MaxPartitions   uint64
	ReadReplicas    string
	KeyBloomFilter  *bool
	TTL             *ttlMeaning
	Families        []familyMeaning
}

// ttlMeaning is a TTL as seconds: the host spells the interval as the author
// wrote it, the provider as the server keeps it.
type ttlMeaning struct {
	Column  string
	Seconds uint64
	Unit    string
}

// familyMeaning is a family and the columns it holds, in the order the source lists them.
type familyMeaning struct {
	Name, Data, Compression string
	Columns                 []string
}

func (m meaning) empty() bool {
	return m.BySize == nil && m.PartitionSizeMB == 0 && m.ByLoad == nil && m.MinPartitions == 0 && m.MaxPartitions == 0 &&
		m.ReadReplicas == "" && m.KeyBloomFilter == nil && m.TTL == nil && len(m.Families) == 0
}

// families sorts a family list and drops the default family when it states
// nothing: a table has that family whether or not a declaration names it.
func families(list []familyMeaning) []familyMeaning {
	list = slices.DeleteFunc(slices.Clone(list), func(f familyMeaning) bool { return f.Name == "default" && f.Data == "" && f.Compression == "" })
	slices.SortFunc(list, func(a, b familyMeaning) int { return strings.Compare(a.Name, b.Name) })
	return list
}

func tableKey(schema, name string) string { return schema + "." + name }

// hostPartitioning and hostFamilies are the host's facet payloads. An unknown
// field fails the decode, so an agreement row cannot carry a starting layout or
// a cache mode that the comparison would silently ignore.
type hostPartitioning struct {
	BySize          *bool  `json:"by_size"`
	PartitionSizeMB uint64 `json:"partition_size_mb"`
	ByLoad          *bool  `json:"by_load"`
	MinPartitions   uint64 `json:"min_partitions"`
	MaxPartitions   uint64 `json:"max_partitions"`
	ReadReplicas    string `json:"read_replicas"`
	KeyBloomFilter  *bool  `json:"key_bloom_filter"`
}

// hostTTL is the host's TTL facet payload, which has no JSON tags.
type hostTTL struct {
	Column   string
	Interval string
	Unit     string
}

type hostFamilies struct {
	Families []struct {
		Name        string   `json:"name"`
		Data        string   `json:"data"`
		Compression string   `json:"compression"`
		Columns     []string `json:"columns"`
	} `json:"families"`
}

func strictJSON(c *qt.C, data json.RawMessage, target any) {
	c.Helper()
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	c.Assert(decoder.Decode(target), qt.IsNil)
}

// hostMeaning reduces the facets the host recorded to a meaning for each table
// that declares anything.
func hostMeaning(c *qt.C, recorded golden) map[string]meaning {
	c.Helper()
	result := map[string]meaning{}
	for _, table := range recorded.Tables {
		var m meaning
		for kind, payload := range table.Facets {
			switch kind {
			case "ptah.run/ydb/table-partitioning":
				var p hostPartitioning
				strictJSON(c, payload, &p)
				m.BySize, m.PartitionSizeMB, m.ByLoad, m.MinPartitions, m.MaxPartitions = p.BySize, p.PartitionSizeMB, p.ByLoad, p.MinPartitions, p.MaxPartitions
				m.ReadReplicas, m.KeyBloomFilter = p.ReadReplicas, p.KeyBloomFilter
			case "ptah.run/ydb/ttl":
				var ttl hostTTL
				strictJSON(c, payload, &ttl)
				seconds, err := ttlgrammar.IntervalSeconds(ttl.Interval)
				c.Assert(err, qt.IsNil)
				m.TTL = &ttlMeaning{Column: ttl.Column, Seconds: seconds, Unit: ttl.Unit}
			case "ptah.run/ydb/column-families":
				var f hostFamilies
				strictJSON(c, payload, &f)
				for _, family := range f.Families {
					m.Families = append(m.Families, familyMeaning{Name: family.Name, Data: family.Data, Compression: family.Compression, Columns: family.Columns})
				}
			default:
				c.Fatalf("the host recorded an unexpected facet %s for %s", kind, table.Name)
			}
		}
		m.Families = families(m.Families)
		// A row the host and the provider are expected to agree on carries no
		// index option, which the provider cannot carry.
		c.Assert(table.Indexes, qt.HasLen, 0, qt.Commentf("table %s", table.Name))
		if !m.empty() {
			result[tableKey(table.Schema, table.Name)] = m
		}
	}
	return result
}

var switches = map[Ydb.FeatureFlag_Status]*bool{Ydb.FeatureFlag_ENABLED: new(true), Ydb.FeatureFlag_DISABLED: new(false)}

var compressions = map[Ydb_Table.ColumnFamily_Compression]string{
	Ydb_Table.ColumnFamily_COMPRESSION_NONE: "off", Ydb_Table.ColumnFamily_COMPRESSION_LZ4: "lz4",
}

// pluginMeaning reduces the facets the provider returned to a meaning for each
// table that declares anything.
func pluginMeaning(c *qt.C, decoded *protocol.DecodedSource) map[string]meaning {
	c.Helper()
	result := map[string]meaning{}
	// placed are the columns the column facets put in a family, by table.
	placed := map[string][]placement{}
	for _, document := range decoded.Documents {
		for _, fragment := range document.Fragments {
			for _, value := range fragment.Values {
				envelope := value.Value.Value
				c.Assert(value.Placement, qt.Equals, protocol.SourceModelValue_FACET)
				if envelope.Kind == "github.com/stokaro/ptah-ydb-plugin/column-properties" {
					var intent native.ColumnIntent
					c.Assert(protojson.Unmarshal(envelope.Payload, &intent), qt.IsNil)
					c.Assert(envelope.Owner.Path, qt.HasLen, 1)
					key := tableKey(envelope.Owner.Schema, envelope.Owner.Path[0])
					placed[key] = append(placed[key], placement{family: intent.GetFamily(), column: envelope.Owner.Name})
					continue
				}
				c.Assert(envelope.Kind, qt.Equals, "github.com/stokaro/ptah-ydb-plugin/row-table-properties")
				var intent native.RowTableIntent
				c.Assert(protojson.Unmarshal(envelope.Payload, &intent), qt.IsNil)
				m := meaning{BySize: switches[intent.GetPartitioningSettings().GetPartitioningBySize()],
					PartitionSizeMB: intent.GetPartitioningSettings().GetPartitionSizeMb(),
					ByLoad:          switches[intent.GetPartitioningSettings().GetPartitioningByLoad()],
					MinPartitions:   intent.GetPartitioningSettings().GetMinPartitionsCount(),
					MaxPartitions:   intent.GetPartitioningSettings().GetMaxPartitionsCount(),
					KeyBloomFilter:  switches[intent.GetKeyBloomFilter()]}
				m.ReadReplicas = replicas(intent.GetReadReplicasSettings())
				m.TTL = ttlOf(intent.GetTtlSettings())
				for _, family := range intent.GetColumnFamilies() {
					m.Families = append(m.Families, familyMeaning{Name: family.GetName(), Data: family.GetData().GetMedia(), Compression: compressions[family.GetCompression()]})
				}
				m.Families = families(m.Families)
				if !m.empty() {
					result[tableKey(envelope.Owner.Schema, envelope.Owner.Name)] = m
				}
			}
		}
	}
	for key, columns := range placed {
		m, found := result[key]
		c.Assert(found, qt.IsTrue, qt.Commentf("a column is placed in a table that declares nothing: %s", key))
		for _, column := range columns {
			at := slices.IndexFunc(m.Families, func(f familyMeaning) bool { return f.Name == column.family })
			c.Assert(at >= 0, qt.IsTrue, qt.Commentf("column %s names the family %q its table does not declare", column.column, column.family))
			m.Families[at].Columns = append(m.Families[at].Columns, column.column)
		}
		result[key] = m
	}
	return result
}

// placement is a column a column facet puts in a family.
type placement struct{ family, column string }

var epochUnits = map[Ydb_Table.ValueSinceUnixEpochModeSettings_Unit]string{
	Ydb_Table.ValueSinceUnixEpochModeSettings_UNIT_SECONDS:      "SECONDS",
	Ydb_Table.ValueSinceUnixEpochModeSettings_UNIT_MILLISECONDS: "MILLISECONDS",
	Ydb_Table.ValueSinceUnixEpochModeSettings_UNIT_MICROSECONDS: "MICROSECONDS",
	Ydb_Table.ValueSinceUnixEpochModeSettings_UNIT_NANOSECONDS:  "NANOSECONDS",
}

func ttlOf(settings *Ydb_Table.TtlSettings) *ttlMeaning {
	switch mode := settings.GetMode().(type) {
	case *Ydb_Table.TtlSettings_DateTypeColumn:
		return &ttlMeaning{Column: mode.DateTypeColumn.GetColumnName(), Seconds: uint64(mode.DateTypeColumn.GetExpireAfterSeconds())}
	case *Ydb_Table.TtlSettings_ValueSinceUnixEpoch:
		return &ttlMeaning{Column: mode.ValueSinceUnixEpoch.GetColumnName(), Seconds: uint64(mode.ValueSinceUnixEpoch.GetExpireAfterSeconds()),
			Unit: epochUnits[mode.ValueSinceUnixEpoch.GetColumnUnit()]}
	default:
		return nil
	}
}

func replicas(settings *Ydb_Table.ReadReplicasSettings) string {
	switch settings.GetSettings().(type) {
	case *Ydb_Table.ReadReplicasSettings_PerAzReadReplicasCount:
		return "PER_AZ:" + formatCount(settings.GetPerAzReadReplicasCount())
	case *Ydb_Table.ReadReplicasSettings_AnyAzReadReplicasCount:
		return "ANY_AZ:" + formatCount(settings.GetAnyAzReadReplicasCount())
	default:
		return ""
	}
}

func formatCount(n uint64) string { return strconv.FormatUint(n, 10) }
