package embedstore

import (
	"encoding/binary"
	"errors"
	"fmt"
	"maps"
	"math"
	"path"
	"slices"
	"strconv"
	"strings"

	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
)

// FloatRepresentation is the representation a YDB generation stores: the
// float element type of a vector index, which an embedding provider's float32
// answer fills. YDB's uint8, int8 and bit types hold quantized vectors, which
// nothing here produces.
const FloatRepresentation = "float"

// ColumnType is the YQL type of a generation's vector column. YDB keeps a
// vector as the bytes Knn::ToBinaryStringFloat produces, in a String column.
const ColumnType = "String"

// VectorMethod is the vector index YDB builds.
const VectorMethod = "vector_kmeans_tree"

// floatTag is the byte Knn::ToBinaryStringFloat ends a float vector with,
// after one little-endian float32 per element. Measured by the host on
// 25.1.4.7 and 26.2.1.14: [1.0, -2.5, 3.25] serializes as 0000803f 000020c0
// 00005040 01.
const floatTag = 0x01

// The columns a generation adds beside its vector, as suffixes of its name,
// and the chunk ordinal of a table of the generation's own. They are the
// host's names (internal/embedydb), so one database serves both.
const (
	generationSuffix = "_generation"
	inputHashSuffix  = "_input_hash"
	versionSuffix    = "_source_version"
	stateSuffix      = "_state"
	ordinalSuffix    = "_chunk_ordinal"
)

// The comment attributes YDB keeps a Ptah comment in, the host's keys
// (internal/ydbcomment): a table's own, and a column's on its table.
const (
	ownCommentKey    = "ptah.comment"
	columnCommentKey = "ptah.comment.column."
)

// The vector index bounds YDB refuses outside of, as the host words them.
const (
	maxVectorDimension = 16384
	maxLevels          = 16
	minClusters        = 2
	maxClusters        = 2048
	maxClusterCount    = 1 << 30
)

// metadataSuffixes lists the four Utf8 columns in the order they are created.
func metadataSuffixes() []string {
	return []string{generationSuffix, inputHashSuffix, versionSuffix, stateSuffix}
}

// generationColumns are the five columns a generation adds, with their types.
func generationColumns(column string) []RowColumn {
	columns := []RowColumn{{Name: column, Type: ColumnType, Nullable: true}}
	for _, suffix := range metadataSuffixes() {
		columns = append(columns, RowColumn{Name: column + suffix, Type: "Utf8", Nullable: true})
	}
	return columns
}

// directory is a schema as YDB names it: a directory relative to the root,
// without leading or trailing slashes.
func directory(schema string) string {
	return strings.Trim(strings.TrimSpace(schema), "/")
}

// relationPath is a relation's path relative to the root.
func relationPath(reference *protocol.Reference) string {
	return path.Join(directory(reference.GetSchema()), strings.TrimSpace(reference.GetName()))
}

// sourcePath and targetPath are a specification's two relations.
func sourcePath(spec *protocol.EmbeddingSpec) string {
	return relationPath(spec.GetSource().GetTable())
}
func targetPath(spec *protocol.EmbeddingSpec) string {
	return relationPath(spec.GetTarget().GetTable())
}

// ownsTable reports a generation in a table of its own.
func ownsTable(spec *protocol.EmbeddingSpec) bool {
	return spec.GetTarget().GetLayout() == protocol.EmbeddingTarget_OWN_TABLE
}

// oneRelation reports a specification whose two sides name one table.
func oneRelation(spec *protocol.EmbeddingSpec) bool { return sourcePath(spec) == targetPath(spec) }

// vectorSettings are a generation's vector index settings.
type vectorSettings struct {
	metric                      string
	dimension, levels, clusters uint64
}

// clause renders the WITH clause of the index's ADD INDEX, as the host does.
func (v vectorSettings) clause() string {
	return fmt.Sprintf("WITH (%s, vector_type=%s, vector_dimension=%d, levels=%d, clusters=%d)",
		v.metric, FloatRepresentation, v.dimension, v.levels, v.clusters)
}

// metricOf is the setting a vector index orders by for metric.
func metricOf(metric protocol.EmbeddingTarget_Metric) (string, bool) {
	switch metric {
	case protocol.EmbeddingTarget_COSINE:
		return "distance=cosine", true
	case protocol.EmbeddingTarget_L2:
		return "distance=euclidean", true
	case protocol.EmbeddingTarget_INNER_PRODUCT:
		return "similarity=inner_product", true
	default:
		return "", false
	}
}

// settingsOf derives the index settings from a specification, or refuses
// them. The options are levels and clusters and nothing else: a pgvector
// build parameter is refused by the method it belongs to, and another option
// by name.
func settingsOf(spec *protocol.EmbeddingSpec) (vectorSettings, error) {
	target := spec.GetTarget()
	if method := strings.ToLower(strings.TrimSpace(target.GetIndexMethod())); method != VectorMethod {
		return vectorSettings{}, refuse(protocol.EmbeddingRefusal_UNSUPPORTED,
			"index method %q is not one YDB builds; a YDB vector index is %s", target.GetIndexMethod(), VectorMethod)
	}
	metric, known := metricOf(target.GetMetric())
	if !known {
		return vectorSettings{}, refuse(protocol.EmbeddingRefusal_UNSUPPORTED, "metric %s has no YDB vector index setting", target.GetMetric())
	}
	settings := vectorSettings{metric: metric, dimension: uint64(target.GetDimension())}
	options := target.GetIndexOptions()
	for _, name := range slices.Sorted(maps.Keys(options)) {
		value := strings.TrimSpace(options[name])
		var into *uint64
		switch strings.ToLower(strings.TrimSpace(name)) {
		case "levels":
			into = &settings.levels
		case "clusters":
			into = &settings.clusters
		case "m", "ef_construction":
			return vectorSettings{}, refuse(protocol.EmbeddingRefusal_UNSUPPORTED,
				"storage parameter %q belongs to pgvector's hnsw index; a %s index is shaped by its levels and clusters", name, VectorMethod)
		case "lists":
			return vectorSettings{}, refuse(protocol.EmbeddingRefusal_UNSUPPORTED,
				"storage parameter %q belongs to pgvector's ivfflat index; a %s index is shaped by its levels and clusters", name, VectorMethod)
		default:
			return vectorSettings{}, refuse(protocol.EmbeddingRefusal_UNSUPPORTED,
				"storage parameter %q has no YDB counterpart: a %s index takes its settings as the distance or similarity, "+
					"vector_type, vector_dimension, levels and clusters attributes", name, VectorMethod)
		}
		count, err := strconv.ParseUint(value, 10, 64)
		if err != nil {
			return vectorSettings{}, refuse(protocol.EmbeddingRefusal_INVALID, "index option %s is %q, not a whole number", name, value)
		}
		*into = count
	}
	if settings.levels == 0 || settings.clusters == 0 {
		return vectorSettings{}, refuse(protocol.EmbeddingRefusal_INVALID,
			"a %s index needs index_options levels and clusters: YDB 25.3 and later refuse an index without either, "+
				"and 25.1 reports back no default for them", VectorMethod)
	}
	if reason := settings.refusal(); reason != "" {
		return vectorSettings{}, refuse(protocol.EmbeddingRefusal_INVALID, "%s", reason)
	}
	return settings, nil
}

// refusal says which of YDB's bounds the settings break, or "".
func (v vectorSettings) refusal() string {
	switch {
	case v.dimension < 1 || v.dimension > maxVectorDimension:
		return fmt.Sprintf("a vector index's vector_dimension is between 1 and %d (`Invalid vector_dimension: %d should be between 1 and %d`)",
			maxVectorDimension, v.dimension, maxVectorDimension)
	case v.levels < 1 || v.levels > maxLevels:
		return fmt.Sprintf("a vector index's levels are between 1 and %d (`Invalid levels: %d should be between 1 and %d`)", maxLevels, v.levels, maxLevels)
	case v.clusters < minClusters || v.clusters > maxClusters:
		return fmt.Sprintf("a vector index's clusters are between %d and %d (`Invalid clusters: %d should be between %d and %d`)",
			minClusters, maxClusters, v.clusters, minClusters, maxClusters)
	case math.Pow(float64(v.clusters), float64(v.levels)) > maxClusterCount:
		return fmt.Sprintf("a vector index's clusters to the power of its levels is at most %d (`Invalid clusters^levels: %d^%d should be less than %d`)",
			maxClusterCount, v.clusters, v.levels, maxClusterCount)
	}
	return ""
}

// checkTarget refuses a target YDB cannot store or search: a quantized
// representation, and a table of the generation's own that is the source.
func checkTarget(spec *protocol.EmbeddingSpec) error {
	target := spec.GetTarget()
	if !strings.EqualFold(strings.TrimSpace(target.GetRepresentation()), FloatRepresentation) {
		return refuse(protocol.EmbeddingRefusal_UNSUPPORTED,
			"representation %q is not one a YDB generation stores. YDB keeps a vector as the bytes of its float element type, "+
				"so set target.representation to %q; YDB's uint8, int8 and bit types hold quantized vectors, which an embedding "+
				"provider does not return", target.GetRepresentation(), FloatRepresentation)
	}
	if ownsTable(spec) && oneRelation(spec) {
		return refuse(protocol.EmbeddingRefusal_INVALID,
			"layout \"own_table\" names the source table %s as its target, and under this layout Ptah creates that table and "+
				"drops it when the generation is retired. Name a table of the generation's own, or use the default layout "+
				"to put the columns on the source", targetPath(spec))
	}
	return nil
}

// DescribeTarget answers offline whether YDB can hold a specification, and
// what a generation needs on it: a nullable String column with four Utf8
// columns beside it, on the target or in a table of the generation's own, and
// a vector_kmeans_tree index where the specification declares one.
func DescribeTarget(spec *protocol.EmbeddingSpec) *protocol.EmbeddingTargetObjects {
	refused := func(err error) *protocol.EmbeddingTargetObjects {
		refusal := &Refusal{Reason: protocol.EmbeddingRefusal_INVALID, Message: err.Error()}
		errors.As(err, &refusal)
		return &protocol.EmbeddingTargetObjects{Refusal: &protocol.EmbeddingRefusal{Reason: refusal.Reason, Message: oneLine(refusal.Message)}}
	}
	if err := checkTarget(spec); err != nil {
		return refused(err)
	}
	objects := &protocol.EmbeddingTargetObjects{ColumnType: ColumnType, OwnsTable: ownsTable(spec)}
	if strings.TrimSpace(spec.GetTarget().GetIndexMethod()) == "" {
		return objects
	}
	settings, err := settingsOf(spec)
	if err != nil {
		return refused(err)
	}
	objects.HasIndex, objects.IndexMethod, objects.IndexOperator = true, VectorMethod, settings.metric
	return objects
}

// EncodeVector writes a vector the way Knn::ToBinaryStringFloat does: each
// element as a little-endian float32, then the float tag. The SDK refuses a
// value that is not finite before a vector arrives.
func EncodeVector(vector []float32) []byte {
	encoded := make([]byte, 0, len(vector)*4+1)
	for _, value := range vector {
		encoded = binary.LittleEndian.AppendUint32(encoded, math.Float32bits(value))
	}
	return append(encoded, floatTag)
}
