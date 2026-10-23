package tablesource_test

import (
	"bytes"
	"encoding/json"
	"slices"
	"strconv"
	"strings"

	qt "github.com/frankban/quicktest"
	native "github.com/stokaro/ptah-ydb-plugin/internal/nativeprotocol"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb_Table"
	"google.golang.org/protobuf/encoding/protojson"
)

// meaning is what a table's declared partitioning and column families say,
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
	Families        []familyMeaning
}

type familyMeaning struct{ Name, Data, Compression string }

func (m meaning) empty() bool {
	return m.BySize == nil && m.PartitionSizeMB == 0 && m.ByLoad == nil && m.MinPartitions == 0 && m.MaxPartitions == 0 &&
		m.ReadReplicas == "" && m.KeyBloomFilter == nil && len(m.Families) == 0
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
// field fails the decode, so an agreement row cannot carry a starting layout,
// a cache mode or a column list that the comparison would silently ignore.
type hostPartitioning struct {
	BySize          *bool  `json:"by_size"`
	PartitionSizeMB uint64 `json:"partition_size_mb"`
	ByLoad          *bool  `json:"by_load"`
	MinPartitions   uint64 `json:"min_partitions"`
	MaxPartitions   uint64 `json:"max_partitions"`
	ReadReplicas    string `json:"read_replicas"`
	KeyBloomFilter  *bool  `json:"key_bloom_filter"`
}

type hostFamilies struct {
	Families []struct {
		Name        string `json:"name"`
		Data        string `json:"data"`
		Compression string `json:"compression"`
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
			case "ptah.run/ydb/column-families":
				var f hostFamilies
				strictJSON(c, payload, &f)
				for _, family := range f.Families {
					m.Families = append(m.Families, familyMeaning{Name: family.Name, Data: family.Data, Compression: family.Compression})
				}
			default:
				c.Fatalf("the host recorded an unexpected facet %s for %s", kind, table.Name)
			}
		}
		m.Families = families(m.Families)
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
	for _, document := range decoded.Documents {
		for _, fragment := range document.Fragments {
			for _, value := range fragment.Values {
				envelope := value.Value.Value
				c.Assert(envelope.Kind, qt.Equals, "github.com/stokaro/ptah-ydb-plugin/row-table-properties")
				c.Assert(value.Placement, qt.Equals, protocol.SourceModelValue_FACET)
				var intent native.RowTableIntent
				c.Assert(protojson.Unmarshal(envelope.Payload, &intent), qt.IsNil)
				m := meaning{BySize: switches[intent.GetPartitioningSettings().GetPartitioningBySize()],
					PartitionSizeMB: intent.GetPartitioningSettings().GetPartitionSizeMb(),
					ByLoad:          switches[intent.GetPartitioningSettings().GetPartitioningByLoad()],
					MinPartitions:   intent.GetPartitioningSettings().GetMinPartitionsCount(),
					MaxPartitions:   intent.GetPartitioningSettings().GetMaxPartitionsCount(),
					KeyBloomFilter:  switches[intent.GetKeyBloomFilter()]}
				m.ReadReplicas = replicas(intent.GetReadReplicasSettings())
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
	return result
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
