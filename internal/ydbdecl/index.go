package ydbdecl

import (
	"fmt"
	"maps"
	"math"
	"slices"
	"strconv"
	"strings"
)

// IndexKind is the kind of a YDB secondary index.
type IndexKind int

// The kinds of index YDB builds.
const (
	IndexSync IndexKind = iota + 1
	IndexAsync
	IndexVector
	IndexFullTextPlain
	IndexFullTextRelevance
	IndexLocalBloom
	IndexLocalNgram
	IndexLocalMinMax
)

// VectorMethod is the method of a vector index.
const VectorMethod = "vector_kmeans_tree"

// IndexKindOf reads an index's method, as an index's type spells it.
func IndexKindOf(method string) (IndexKind, error) {
	normalized := strings.Join(strings.Fields(strings.ToUpper(strings.ReplaceAll(method, "_", " "))), " ")
	switch normalized {
	case "", "BTREE", "GLOBAL", "SYNC", "GLOBAL SYNC":
		return IndexSync, nil
	case "ASYNC", "GLOBAL ASYNC":
		return IndexAsync, nil
	case "VECTOR KMEANS TREE", "GLOBAL USING VECTOR KMEANS TREE":
		return IndexVector, nil
	case "FULLTEXT PLAIN", "GLOBAL USING FULLTEXT PLAIN":
		return IndexFullTextPlain, nil
	case "FULLTEXT RELEVANCE", "GLOBAL USING FULLTEXT RELEVANCE":
		return IndexFullTextRelevance, nil
	case "BLOOM FILTER", "LOCAL USING BLOOM FILTER":
		return IndexLocalBloom, nil
	case "BLOOM NGRAM FILTER", "LOCAL USING BLOOM NGRAM FILTER":
		return IndexLocalNgram, nil
	case "MIN MAX", "LOCAL USING MIN MAX":
		return IndexLocalMinMax, nil
	case "HNSW", "IVFFLAT":
		return 0, fmt.Errorf("index method %q is pgvector's and has no YDB counterpart: YDB's vector index "+
			"is %s, declared with type %q and its distance or similarity, vector_type, vector_dimension, "+
			"levels and clusters", method, VectorMethod, VectorMethod)
	default:
		return 0, fmt.Errorf("index method %q has no YDB counterpart: a row table has global indexes, "+
			"synchronous or asynchronous, vector and full-text indexes; declare type \"async\" for an asynchronous one "+
			"and type %q for a vector one", method, VectorMethod)
	}
}

// IsFullText reports a full-text index.
func (k IndexKind) IsFullText() bool { return k == IndexFullTextPlain || k == IndexFullTextRelevance }

// IsLocal reports an index stored with a column table's data portions.
func (k IndexKind) IsLocal() bool {
	return k == IndexLocalBloom || k == IndexLocalNgram || k == IndexLocalMinMax
}

// String names the kind the way a declaration may.
func (k IndexKind) String() string {
	switch k {
	case IndexSync:
		return "sync"
	case IndexAsync:
		return "async"
	case IndexVector:
		return VectorMethod
	case IndexLocalBloom:
		return "bloom_filter"
	case IndexLocalNgram:
		return "bloom_ngram_filter"
	case IndexLocalMinMax:
		return "min_max"
	case IndexFullTextPlain:
		return "fulltext_plain"
	case IndexFullTextRelevance:
		return "fulltext_relevance"
	default:
		return "unknown"
	}
}

// IndexPartitioning is how a global index's table splits into partitions,
// and its read replicas.
type IndexPartitioning struct {
	BySize          *bool  `json:"by_size,omitempty"`
	PartitionSizeMB uint64 `json:"partition_size_mb,omitempty"`
	ByLoad          *bool  `json:"by_load,omitempty"`
	MinPartitions   uint64 `json:"min_partitions,omitempty"`
	MaxPartitions   uint64 `json:"max_partitions,omitempty"`
	ReadReplicas    string `json:"read_replicas,omitempty"`
}

// ParseIndexPartitioning reads the partitioning attributes of one index
// declaration, and returns nil where none is present.
func ParseIndexPartitioning(values map[string]string) (*IndexPartitioning, error) {
	shared, present, err := parseDeclared(values)
	if err != nil {
		return nil, err
	}
	if !present {
		return nil, nil
	}
	return &IndexPartitioning{BySize: shared.bySize, PartitionSizeMB: shared.partitionSizeMB, ByLoad: shared.byLoad,
		MinPartitions: shared.minPartitions, MaxPartitions: shared.maxPartitions, ReadReplicas: shared.readReplicas}, nil
}

// The attributes that declare a vector index's settings.
const (
	AttributeDistance        = "distance"
	AttributeSimilarity      = "similarity"
	AttributeVectorType      = "vector_type"
	AttributeVectorDimension = "vector_dimension"
	AttributeLevels          = "levels"
	AttributeClusters        = "clusters"
)

// VectorAttributes lists the vector attributes in the order a declaration's
// errors are reported.
func VectorAttributes() []string {
	return []string{
		AttributeDistance, AttributeSimilarity, AttributeVectorType,
		AttributeVectorDimension, AttributeLevels, AttributeClusters,
	}
}

var (
	vectorDistances    = []string{"cosine", "euclidean", "manhattan"}
	vectorSimilarities = []string{"inner_product", "cosine"}
	vectorElementTypes = []string{"float", "uint8", "int8", "bit"}
)

// VectorIndex is the settings a vector index declares. An empty field is a
// setting the declaration leaves out.
type VectorIndex struct {
	Distance   string `json:"distance,omitempty"`
	Similarity string `json:"similarity,omitempty"`
	VectorType string `json:"vector_type,omitempty"`
	Dimension  uint64 `json:"vector_dimension,omitempty"`
	Levels     uint64 `json:"levels,omitempty"`
	Clusters   uint64 `json:"clusters,omitempty"`
}

// DeclareVector reads the vector settings of one index of method indexType:
// nil for an index that is not a vector index and states none, and an empty
// declaration for a vector index that states none.
func DeclareVector(values map[string]string, indexType string) (*VectorIndex, error) {
	var spec VectorIndex
	present := false
	for _, attribute := range VectorAttributes() {
		value, ok := values[attribute]
		if !ok {
			continue
		}
		present = true
		if err := setVectorAttribute(&spec, attribute, strings.TrimSpace(value)); err != nil {
			return nil, err
		}
	}
	if present {
		return &spec, nil
	}
	if kind, err := IndexKindOf(indexType); err != nil || kind != IndexVector {
		return nil, nil
	}
	return &VectorIndex{}, nil
}

func setVectorAttribute(spec *VectorIndex, attribute, value string) error {
	switch attribute {
	case AttributeDistance:
		return parseName(attribute, value, vectorDistances, &spec.Distance)
	case AttributeSimilarity:
		return parseName(attribute, value, vectorSimilarities, &spec.Similarity)
	case AttributeVectorType:
		return parseName(attribute, value, vectorElementTypes, &spec.VectorType)
	case AttributeVectorDimension:
		return parseCount(attribute, value, &spec.Dimension)
	case AttributeLevels:
		return parseCount(attribute, value, &spec.Levels)
	default:
		return parseCount(attribute, value, &spec.Clusters)
	}
}

func parseName(attribute, value string, names []string, target *string) error {
	lower := strings.ToLower(value)
	if !slices.Contains(names, lower) {
		return &DeclarationError{Attribute: attribute, Value: value, Reason: "write one of " + strings.Join(names, ", ")}
	}
	*target = lower
	return nil
}

// fullTextOptions names the options of a full-text index and the grammar of
// each value.
var fullTextOptions = map[string]string{
	"tokenizer": "tokenizer", "language": "language",
	"use_filter_lowercase": "bool", "use_filter_stopwords": "bool",
	"use_filter_ngram": "bool", "use_filter_edge_ngram": "bool",
	"filter_ngram_min_length": "integer", "filter_ngram_max_length": "integer",
	"use_filter_length": "bool", "filter_length_min": "integer", "filter_length_max": "integer",
	"use_filter_snowball": "bool",
}

// FullTextAttributes lists a full-text index's options in name order.
func FullTextAttributes() []string { return slices.Sorted(maps.Keys(fullTextOptions)) }

// LocalAttributes lists a local index's options.
func LocalAttributes() []string {
	return []string{"false_positive_probability", "ngram_size", "case_sensitive"}
}

// ParseIndexOptions reads the full-text or local-index options of one index
// declaration; values["type"] names the index's method. Mixing the families
// is refused.
func ParseIndexOptions(values map[string]string) (map[string]string, error) {
	text, err := parseFullText(values)
	if err != nil {
		return nil, err
	}
	local := make(map[string]string)
	for _, name := range LocalAttributes() {
		if value, ok := values[name]; ok {
			local[name] = value
		}
	}
	if len(local) == 0 {
		return text, nil
	}
	if len(text) > 0 {
		return nil, fmt.Errorf("local and full-text index options cannot be combined")
	}
	kind, err := IndexKindOf(values["type"])
	if err != nil {
		return nil, err
	}
	return resolveLocal(kind, local)
}

func parseFullText(values map[string]string) (map[string]string, error) {
	options := make(map[string]string)
	for _, name := range FullTextAttributes() {
		if value, present := values[name]; present {
			options[name] = value
		}
	}
	if len(options) == 0 {
		return nil, nil
	}
	resolved := make(map[string]string, len(options))
	for _, key := range slices.Sorted(maps.Keys(options)) {
		value, err := fullTextValue(fullTextOptions[key], options[key])
		if err != nil {
			return nil, fmt.Errorf("YDB full-text index option %s: %w", key, err)
		}
		resolved[key] = value
	}
	if resolved["tokenizer"] == "" {
		return nil, fmt.Errorf("a YDB full-text index requires tokenizer")
	}
	if resolved["use_filter_ngram"] == "true" || resolved["use_filter_edge_ngram"] == "true" {
		for _, name := range []string{"filter_ngram_min_length", "filter_ngram_max_length"} {
			if _, present := resolved[name]; !present {
				return nil, fmt.Errorf("a YDB n-gram filter requires %s", name)
			}
		}
	}
	return resolved, nil
}

func fullTextValue(grammar, value string) (string, error) {
	value = strings.TrimSpace(value)
	switch grammar {
	case "bool":
		if value == "true" || value == "false" {
			return value, nil
		}
		return "", fmt.Errorf("%q is not true or false", value)
	case "integer":
		number, err := strconv.ParseUint(value, 10, 31)
		if err != nil {
			return "", fmt.Errorf("%q is not a nonnegative 32-bit integer", value)
		}
		return strconv.FormatUint(number, 10), nil
	case "tokenizer":
		switch value {
		case "standard", "whitespace", "keyword":
			return value, nil
		default:
			return "", fmt.Errorf("unknown tokenizer %q", value)
		}
	default:
		if value == "" {
			return "", fmt.Errorf("the language is empty")
		}
		return value, nil
	}
}

// resolveLocal validates local-index settings and supplies the release
// defaults.
func resolveLocal(kind IndexKind, settings map[string]string) (map[string]string, error) {
	if !kind.IsLocal() {
		return nil, fmt.Errorf("index method %s is not local", kind)
	}
	out := make(map[string]string)
	if kind != IndexLocalMinMax {
		out["false_positive_probability"] = "0.1"
	}
	if kind == IndexLocalNgram {
		out["ngram_size"] = "3"
		out["case_sensitive"] = "true"
	}
	for _, key := range slices.Sorted(maps.Keys(settings)) {
		value := strings.TrimSpace(settings[key])
		if _, allowed := out[key]; !allowed {
			return nil, fmt.Errorf("setting %q is not supported by %s", key, kind)
		}
		switch key {
		case "false_positive_probability":
			v, err := strconv.ParseFloat(value, 64)
			if err != nil || math.IsNaN(v) || v <= 0 || v >= 1 {
				return nil, fmt.Errorf("false_positive_probability must be between 0 and 1, exclusively")
			}
			out[key] = strconv.FormatFloat(v, 'g', -1, 64)
		case "ngram_size":
			v, err := strconv.ParseUint(value, 10, 32)
			if err != nil || v < 3 || v > 8 {
				return nil, fmt.Errorf("ngram_size must be between 3 and 8")
			}
			out[key] = strconv.FormatUint(v, 10)
		case "case_sensitive":
			if value != "true" && value != "false" {
				return nil, fmt.Errorf("case_sensitive must be true or false")
			}
			out[key] = value
		}
	}
	if kind == IndexLocalNgram {
		probability, _ := strconv.ParseFloat(out["false_positive_probability"], 64)
		if math.Floor(-math.Log2(probability)) > 8 {
			return nil, fmt.Errorf("false_positive_probability must produce at most 8 n-gram hashes (greater than 1/512)")
		}
	}
	return out, nil
}
