package tablecreate

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"

	native "github.com/stokaro/ptah-ydb-plugin/internal/nativeprotocol"
	"github.com/stokaro/ptah-ydb-plugin/schema"
	"github.com/stokaro/ptah/provider-sdk/modeljson"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb_Table"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

// NativeIndexOperationKind identifies the addition of one vector or full-text
// index to an existing row table. The pinned table bindings carry neither kind
// in an AlterTable request, so the index is added with YQL's ALTER TABLE ...
// ADD INDEX, as the host's embedded engine writes it. A removal is the native
// AlterTable call that drops any index (see AlterOperationKind).
const NativeIndexOperationKind = schema.ProviderID + "/row-table-native-index-operation"

// The capabilities a vector or full-text index needs, spelled as the host's
// vocabulary spells them.
const (
	// VectorIndexesCapability is a vector index. YDB 25.1 keeps it behind
	// EnableVectorIndex.
	VectorIndexesCapability = "vector_indexes"
	// VectorBitTypeCapability is a vector index over bit vectors, which YDB
	// builds over rows from 26.1.
	VectorBitTypeCapability = "vector_bit_type"
	// VectorMaintainedOnWriteCapability is a vector index that includes the
	// rows written after its build, from 25.3. The planner does not read it: an
	// index that does not is still the index declared.
	VectorMaintainedOnWriteCapability = "vector_index_maintained_on_write"
	// FullTextIndexesCapability is a full-text index, which YDB keeps behind
	// EnableFulltextIndex, off until 26.2.
	FullTextIndexesCapability = "full_text_indexes"
)

var nativeIndexModel = buildNativeIndexModel()

// NativeIndexModel binds the payload grammar of an addition of a vector or
// full-text index, its precondition and its confirmation.
func NativeIndexModel() *protocol.ValueModel { return proto.CloneOf(nativeIndexModel) }

func buildNativeIndexModel() *protocol.ValueModel {
	raw, err := json.Marshal(map[string]any{
		"kind": NativeIndexOperationKind, "representation": "operation", "schema_version": 1, "encoding": "ydb-protojson",
		"payload": "index: the native index specification of one vector_kmeans_tree, fulltext_plain or fulltext_relevance index: its name, ordered key columns, " +
			"covered columns and its vector or full-text settings; the settings of the tables that implement it are never stated",
		"vector":       "one metric, distance or similarity, a vector type, a dimension from 1 to 16384, levels from 1 to 16 and clusters from 2 to 2048 with clusters to the power of levels at most 2^30; no overlap settings; the last key column is the vector, the others its prefix",
		"fulltext":     "one key column, analyzed by the one column entry of the settings, with a tokenizer; an n-gram filter names both its lengths; a language goes with the snowball filter, which goes with no n-gram filter",
		"execution":    "YQL ALTER TABLE ... ADD INDEX through the query service once, outside SQL transactions; no retries; session and absolute path bound at dispatch",
		"precondition": "the table must exist as a row table, hold no index of that name, and hold every column the index reads",
		"confirmation": "read the table back until the index is ready; it must hold the requested key columns, covered columns, kind and settings; every other index, every column and the key must be unchanged",
		"semantics":    "additive: YDB builds the index over the rows the table holds; nothing else changes",
		"database":     "an explicit owner database must match the selected session; an empty database binds to that session",
	})
	if err != nil {
		panic(err)
	}
	canonical, err := modeljson.Canonical(raw)
	if err != nil {
		panic(err)
	}
	sum := sha256.Sum256(canonical)
	return &protocol.ValueModel{Kind: NativeIndexOperationKind, Representation: protocol.ValueInput_OPERATION, SchemaVersion: 1, Encoding: "ydb-protojson",
		DefinitionJson: canonical, DefinitionSha256: hex.EncodeToString(sum[:]), CanonicalFormat: NativeIndexOperationKind + "/json-v1"}
}

// nativeOnlyIndex reports a vector or full-text index: one the native index
// facet alone describes, since the common model has no place for its settings.
func nativeOnlyIndex(spec *native.IndexIntent) bool {
	switch spec.GetType().(type) {
	case *native.IndexIntent_GlobalVectorKmeansTreeIndex, *native.IndexIntent_GlobalFulltextPlainIndex, *native.IndexIntent_GlobalFulltextRelevanceIndex:
		return true
	}
	return false
}

// nativeIndexDefinition is the definition of a vector or full-text index that
// is created and compared: its name, columns, kind and vector or full-text
// settings. The settings of the tables that implement the index are left out.
// The server reports them with its defaults on every index it holds (measured
// on 25.1.4.7 and 26.2.1.14), and YDB changes none of them on a vector index
// that exists (`ALTER INDEX ... SET` answers `Only index with one impl table is
// supported`), so the host's embedded engine neither compares nor writes them
// either. Any other kind is returned unchanged.
func nativeIndexDefinition(spec *native.IndexIntent) *native.IndexIntent {
	result := proto.CloneOf(spec)
	switch kind := result.Type.(type) {
	case *native.IndexIntent_GlobalVectorKmeansTreeIndex:
		kind.GlobalVectorKmeansTreeIndex = &native.GlobalVectorKMeansTreeIndex{VectorSettings: kind.GlobalVectorKmeansTreeIndex.GetVectorSettings()}
	case *native.IndexIntent_GlobalFulltextPlainIndex:
		kind.GlobalFulltextPlainIndex = &native.GlobalFulltextPlainIndex{FulltextSettings: kind.GlobalFulltextPlainIndex.GetFulltextSettings()}
	case *native.IndexIntent_GlobalFulltextRelevanceIndex:
		kind.GlobalFulltextRelevanceIndex = &native.GlobalFulltextRelevanceIndex{FulltextSettings: kind.GlobalFulltextRelevanceIndex.GetFulltextSettings()}
	}
	return result
}

// comparableIndex is the form of an index specification a comparison reads.
// A vector or full-text index is read as its definition (see
// nativeIndexDefinition), and a full-text filter switched off is read as one
// the index does not name: YDB applies no filter it is not told to, so the
// two analyze text alike. The host's embedded engine compares them so too.
// Any other kind is returned unchanged.
func comparableIndex(spec *native.IndexIntent) *native.IndexIntent {
	if !nativeOnlyIndex(spec) {
		return spec
	}
	result := nativeIndexDefinition(spec)
	var settings *native.FulltextIndexSettings
	switch kind := result.Type.(type) {
	case *native.IndexIntent_GlobalFulltextPlainIndex:
		settings = kind.GlobalFulltextPlainIndex.GetFulltextSettings()
	case *native.IndexIntent_GlobalFulltextRelevanceIndex:
		settings = kind.GlobalFulltextRelevanceIndex.GetFulltextSettings()
	}
	for _, column := range settings.GetColumns() {
		analyzers := column.GetAnalyzers()
		if analyzers == nil {
			continue
		}
		for _, filter := range []**bool{&analyzers.UseFilterLowercase, &analyzers.UseFilterStopwords, &analyzers.UseFilterNgram,
			&analyzers.UseFilterEdgeNgram, &analyzers.UseFilterLength, &analyzers.UseFilterSnowball} {
			if *filter != nil && !**filter {
				*filter = nil
			}
		}
	}
	return result
}

// SameNativeIndex reports whether two specifications state one vector or
// full-text index as a comparison reads it (see comparableIndex).
func SameNativeIndex(a, b *native.IndexIntent) bool {
	return proto.Equal(comparableIndex(a), comparableIndex(b))
}

// nativeIndexKind names the kind of a vector or full-text index as YQL's
// USING clause spells it.
func nativeIndexKind(spec *native.IndexIntent) string {
	switch spec.GetType().(type) {
	case *native.IndexIntent_GlobalVectorKmeansTreeIndex:
		return "vector_kmeans_tree"
	case *native.IndexIntent_GlobalFulltextPlainIndex:
		return "fulltext_plain"
	case *native.IndexIntent_GlobalFulltextRelevanceIndex:
		return "fulltext_relevance"
	}
	return ""
}

// fullTextSettings returns the full-text settings of a full-text index, nil
// for any other kind.
func fullTextSettings(spec *native.IndexIntent) *native.FulltextIndexSettings {
	switch kind := spec.GetType().(type) {
	case *native.IndexIntent_GlobalFulltextPlainIndex:
		return kind.GlobalFulltextPlainIndex.GetFulltextSettings()
	case *native.IndexIntent_GlobalFulltextRelevanceIndex:
		return kind.GlobalFulltextRelevanceIndex.GetFulltextSettings()
	}
	return nil
}

// The limits YDB 25.3 and later hold a vector index to. 25.1 and 25.2 check
// none of them, and the host's embedded engine holds every line to them, so a
// declaration that applies on one line applies on every line.
const (
	maxVectorDimension   = 16384
	maxVectorLevels      = 16
	minVectorClusters    = 2
	maxVectorClusters    = 2048
	maxVectorTreeLeaves  = 1 << 30
	vectorSettingsAdvice = "declare its distance or similarity, vector_type, vector_dimension, levels and clusters"
)

// implementationDefaults are the settings YDB gives each table that
// implements a new vector or full-text index, measured on 25.1.4.7 and
// 26.2.1.14.
var implementationDefaults = &native.GlobalIndexSettings{PartitioningSettings: &Ydb_Table.PartitioningSettings{
	PartitioningBySize: Ydb.FeatureFlag_ENABLED, PartitionSizeMb: 2048, PartitioningByLoad: Ydb.FeatureFlag_DISABLED, MinPartitionsCount: 1}}

// implementationSettings returns the settings a vector or full-text index
// states for the tables that implement it.
func implementationSettings(spec *native.IndexIntent) []*native.GlobalIndexSettings {
	switch kind := spec.GetType().(type) {
	case *native.IndexIntent_GlobalVectorKmeansTreeIndex:
		index := kind.GlobalVectorKmeansTreeIndex
		return []*native.GlobalIndexSettings{index.GetLevelTableSettings(), index.GetPostingTableSettings(), index.GetPrefixTableSettings()}
	case *native.IndexIntent_GlobalFulltextPlainIndex:
		return []*native.GlobalIndexSettings{kind.GlobalFulltextPlainIndex.GetSettings()}
	case *native.IndexIntent_GlobalFulltextRelevanceIndex:
		index := kind.GlobalFulltextRelevanceIndex
		return []*native.GlobalIndexSettings{index.GetPostingTableSettings(), index.GetDictTableSettings(), index.GetDocsTableSettings(), index.GetStatsTableSettings()}
	}
	return nil
}

// nativeIndexColumns describes the table an index is added to: the columns
// the declaration states with their YDB types, and the table's key.
type nativeIndexColumns struct {
	types map[string]string
	key   []string
}

// nativeIndexRefusal returns why a vector or full-text index cannot be added
// to a table on this target, or the empty string. The rules and their words
// are the host's embedded engine's, which refuses each before a statement is
// sent; the quoted server answers are what YDB says instead.
func nativeIndexRefusal(spec *native.IndexIntent, table nativeIndexColumns, capabilities map[string]bool) string {
	subject := fmt.Sprintf("index %q", spec.GetName())
	if !validName(spec.GetName()) {
		return subject + ": YDB names every index, and this name is not one it takes"
	}
	keys, covered := spec.GetIndexColumns(), spec.GetDataColumns()
	if len(keys) == 0 {
		return subject + ": the index names no column"
	}
	for i, name := range keys {
		switch {
		case slices.Contains(keys[:i], name):
			return fmt.Sprintf("%s: it names column %q twice", subject, name)
		case table.types[name] == "":
			return fmt.Sprintf("%s reads column %q, which the declaration does not state", subject, name)
		}
	}
	for i, name := range covered {
		switch {
		case slices.Contains(keys, name):
			return fmt.Sprintf("%s: column %q is both a key and a covered column, which YDB refuses", subject, name)
		case slices.Contains(covered[:i], name):
			return fmt.Sprintf("%s: it covers column %q twice", subject, name)
		case slices.Contains(table.key, name):
			return fmt.Sprintf("%s: it covers key column %q, which every YDB index carries already", subject, name)
		case table.types[name] == "":
			return fmt.Sprintf("%s reads column %q, which the declaration does not state", subject, name)
		}
	}
	for _, settings := range implementationSettings(spec) {
		if settings != nil && proto.Size(settings) != 0 && !proto.Equal(settings, implementationDefaults) {
			return subject + ": it states the partitioning of a table that implements it, which this provider does not write; " +
				"YDB gives each table of a new vector or full-text index its default partitioning"
		}
	}
	if vector := spec.GetGlobalVectorKmeansTreeIndex(); vector != nil {
		if !capabilities[VectorIndexesCapability] {
			return fmt.Sprintf("%s declares vector settings, which requires target capability %s, unavailable on this ydb target "+
				"(YDB 25.1 keeps vector indexes behind the EnableVectorIndex feature flag; a session reads the flag when the address names the monitoring endpoint)",
				subject, VectorIndexesCapability)
		}
		if reason := vectorRefusal(vector.GetVectorSettings()); reason != "" {
			return subject + ": " + reason
		}
		if vector.GetVectorSettings().GetSettings().GetVectorType() == native.VectorIndexSettings_VECTOR_TYPE_BIT && !capabilities[VectorBitTypeCapability] {
			return fmt.Sprintf("%s stores bit vectors, which requires target capability %s, unavailable on this ydb target "+
				"(before 26.1 YDB does not build such an index over rows: `bit vector type is not supported`, `Unsupported vector_type: VECTOR_TYPE_BIT`)",
				subject, VectorBitTypeCapability)
		}
		vector, prefix := keys[len(keys)-1], keys[:len(keys)-1]
		if kind := table.types[vector]; kind != "String" {
			return fmt.Sprintf("%s: its vector column %q is %s, and a YDB vector index reads a String column (`Embedding column '%s' expected type 'String'`)",
				subject, vector, kind, vector)
		}
		for _, name := range prefix {
			if !keyComparable(table.types[name]) {
				return fmt.Sprintf("%s: column %q is %s, which YDB refuses as an index key", subject, name, table.types[name])
			}
		}
		return ""
	}
	if !capabilities[FullTextIndexesCapability] {
		return fmt.Sprintf("%s is a full-text index, which requires target capability %s, unavailable on this ydb target "+
			"(YDB keeps full-text indexes behind the EnableFulltextIndex feature flag, on from 26.2; a session reads the flag when the address names the monitoring endpoint)",
			subject, FullTextIndexesCapability)
	}
	if len(keys) != 1 {
		return subject + ": a YDB full-text index requires exactly one text column; this release does not support prefix columns"
	}
	if kind := table.types[keys[0]]; kind != "String" && kind != "Utf8" {
		return fmt.Sprintf("%s: its text column %q is %s, and a YDB full-text index reads a String or Utf8 column", subject, keys[0], kind)
	}
	if len(table.key) != 1 || table.types[table.key[0]] != "Uint64" {
		return subject + ": a YDB full-text index requires exactly one Uint64 primary key column (`Fulltext index requires primary key column ... to be of type 'Uint64'`)"
	}
	if reason := fullTextRefusal(keys[0], fullTextSettings(spec)); reason != "" {
		return subject + ": " + reason
	}
	return ""
}

// vectorRefusal checks the settings of a vector index against the limits YDB
// holds it to, in the order the host's embedded engine reports them.
func vectorRefusal(settings *native.KMeansTreeSettings) string {
	metric := settings.GetSettings().GetMetric()
	switch {
	case settings.GetSettings() == nil:
		return "a vector_kmeans_tree index declares none of its settings; " + vectorSettingsAdvice
	case metric == native.VectorIndexSettings_METRIC_UNSPECIFIED:
		return "a vector index names one metric, distance or similarity (`either distance or similarity should be set`)"
	case settings.GetSettings().GetVectorType() == native.VectorIndexSettings_VECTOR_TYPE_UNSPECIFIED:
		return "a vector index names its vector_type (`vector_type should be set`)"
	case settings.GetSettings().VectorDimension == nil || settings.GetSettings().GetVectorDimension() < 1 || settings.GetSettings().GetVectorDimension() > maxVectorDimension:
		return fmt.Sprintf("a vector index's vector_dimension is between 1 and %d (`Invalid vector_dimension`)", maxVectorDimension)
	case settings.Levels == nil || settings.GetLevels() < 1 || settings.GetLevels() > maxVectorLevels:
		return fmt.Sprintf("a vector index's levels are between 1 and %d (`levels should be set`); %s", maxVectorLevels, vectorSettingsAdvice)
	case settings.Clusters == nil || settings.GetClusters() < minVectorClusters || settings.GetClusters() > maxVectorClusters:
		return fmt.Sprintf("a vector index's clusters are between %d and %d (`clusters should be set`); %s", minVectorClusters, maxVectorClusters, vectorSettingsAdvice)
	case math.Pow(float64(settings.GetClusters()), float64(settings.GetLevels())) > maxVectorTreeLeaves:
		return fmt.Sprintf("a vector index's clusters to the power of its levels is at most %d (`Invalid clusters^levels: %d^%d should be less than %d`)",
			maxVectorTreeLeaves, settings.GetClusters(), settings.GetLevels(), maxVectorTreeLeaves)
	case settings.OverlapClusters != nil || settings.OverlapRatio != nil:
		return "this provider writes no overlap_clusters or overlap_ratio (25.1 answers `Unknown index setting: overlap_clusters`)"
	}
	return ""
}

// fullTextRefusal checks the analyzers of a full-text index on column. The
// rules after the tokenizer are what YDB 26.2.1.14 answers.
func fullTextRefusal(column string, settings *native.FulltextIndexSettings) string {
	if len(settings.GetColumns()) != 1 || settings.GetColumns()[0].GetColumn() != column || settings.GetColumns()[0].GetAnalyzers() == nil {
		return fmt.Sprintf("a YDB full-text index requires analyzer settings in WITH for its column %q", column)
	}
	analyzers := settings.GetColumns()[0].GetAnalyzers()
	ngram := analyzers.GetUseFilterNgram() || analyzers.GetUseFilterEdgeNgram()
	switch {
	case analyzers.GetTokenizer() == native.FulltextIndexSettings_TOKENIZER_UNSPECIFIED:
		return "a YDB full-text index requires tokenizer"
	case ngram && analyzers.FilterNgramMinLength == nil:
		return "a YDB n-gram filter requires filter_ngram_min_length"
	case ngram && analyzers.FilterNgramMaxLength == nil:
		return "a YDB n-gram filter requires filter_ngram_max_length"
	case analyzers.Language != nil && !analyzers.GetUseFilterSnowball():
		return "a full-text index's language goes with use_filter_snowball (`language setting is only supported with use_filter_snowball at present`)"
	case analyzers.GetUseFilterSnowball() && ngram:
		return "a full-text index takes no n-gram filter with use_filter_snowball (`cannot set use_filter_snowball with use_filter_ngram or use_filter_edge_ngram at the same time`)"
	}
	for _, length := range []*int32{analyzers.FilterNgramMinLength, analyzers.FilterNgramMaxLength, analyzers.FilterLengthMin, analyzers.FilterLengthMax} {
		if length != nil && *length < 0 {
			return "a full-text index's lengths are not negative"
		}
	}
	return ""
}

// NativeIndex is a decoded addition of a vector or full-text index.
type NativeIndex struct {
	// Index is the definition of the index to add (see nativeIndexDefinition).
	Index *native.IndexIntent
}

type nativeIndexPayload struct {
	Index json.RawMessage `json:"index"`
}

// CaptureNativeIndex builds the operation that adds one vector or full-text
// index to a table in owner.
func CaptureNativeIndex(id string, owner *protocol.Reference, spec *native.IndexIntent) (*protocol.NormalizedValue, error) {
	encoded, err := encodeNativeIndex(&NativeIndex{Index: nativeIndexDefinition(spec)})
	if err != nil {
		return nil, err
	}
	definition := NativeIndexModel()
	return NormalizeNativeIndex(&protocol.ValueInput{Id: id, Representation: protocol.ValueInput_OPERATION, Value: &protocol.Envelope{
		ProviderId: schema.ProviderID, Kind: NativeIndexOperationKind, Owner: proto.CloneOf(owner), SchemaVersion: 1, Encoding: definition.Encoding,
		DefinitionSha256: definition.DefinitionSha256, Payload: encoded}})
}

func encodeNativeIndex(index *NativeIndex) ([]byte, error) {
	encoded, err := protojson.Marshal(index.Index)
	if err != nil {
		return nil, err
	}
	raw, err := json.Marshal(nativeIndexPayload{Index: encoded})
	if err != nil {
		return nil, err
	}
	return modeljson.Canonical(raw)
}

// NormalizeNativeIndex validates the addition of a vector or full-text index
// and reconstructs its safety facts from the payload, never from the sender.
func NormalizeNativeIndex(input *protocol.ValueInput) (*protocol.NormalizedValue, error) {
	value, definition := input.GetValue(), NativeIndexModel()
	if input.GetRepresentation() != protocol.ValueInput_OPERATION || value.GetProviderId() != schema.ProviderID || value.GetKind() != definition.Kind ||
		value.GetSchemaVersion() != 1 || value.GetEncoding() != definition.Encoding || value.GetDefinitionSha256() != definition.DefinitionSha256 {
		return nil, fmt.Errorf("YDB index addition does not match its negotiated operation model")
	}
	if err := validateOwner(value.Owner); err != nil {
		return nil, err
	}
	index, err := decodeNativeIndex(value.Payload)
	if err != nil {
		return nil, err
	}
	payload, err := encodeNativeIndex(index)
	if err != nil {
		return nil, err
	}
	result := proto.CloneOf(value)
	result.Payload = payload
	return &protocol.NormalizedValue{Id: input.GetId(), Value: result, NormalizedOwner: proto.CloneOf(result.Owner),
		CanonicalFormat: definition.CanonicalFormat, CanonicalProjection: bytes.Clone(payload),
		OperationSemantics: &protocol.OperationSemantics{Impact: protocol.ChangeSemantics_ADDITIVE,
			Reason: "Adds a " + indexFamily(index.Index) + " index; YDB builds it over the rows the table holds",
			Access: protocol.ChangeSemantics_ACCESS_UNKNOWN, AccessReason: "Alteration inherits server access policy"}}, nil
}

// DecodeNativeIndex returns independent native operands after validating the
// envelope.
func DecodeNativeIndex(value *protocol.Envelope) (*NativeIndex, error) {
	normalized, err := NormalizeNativeIndex(&protocol.ValueInput{Id: "execute", Representation: protocol.ValueInput_OPERATION, Value: value})
	if err != nil {
		return nil, err
	}
	return decodeNativeIndex(normalized.Value.Payload)
}

func decodeNativeIndex(raw []byte) (*NativeIndex, error) {
	canonical, err := modeljson.Canonical(raw)
	if err != nil {
		return nil, err
	}
	var fields any
	if err := json.Unmarshal(canonical, &fields); err != nil || containsNull(fields) {
		return nil, fmt.Errorf("a YDB index addition cannot be null")
	}
	var payload nativeIndexPayload
	decoder := json.NewDecoder(bytes.NewReader(canonical))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&payload); err != nil || payload.Index == nil {
		return nil, fmt.Errorf("invalid YDB index addition JSON")
	}
	index := &native.IndexIntent{}
	if err := protojson.Unmarshal(payload.Index, index); err != nil {
		return nil, fmt.Errorf("invalid YDB index addition")
	}
	if err := schema.Known(index); err != nil {
		return nil, err
	}
	if !nativeOnlyIndex(index) || !proto.Equal(index, nativeIndexDefinition(index)) {
		return nil, fmt.Errorf("a YDB index addition adds one vector or full-text index and states no settings of the tables that implement it")
	}
	columns := nativeIndexColumns{types: map[string]string{}}
	for _, name := range slices.Concat(index.GetIndexColumns(), index.GetDataColumns()) {
		// The operation is checked without the table: every column reads as the
		// type its kind takes, and execution checks the columns the table holds.
		columns.types[name] = "String"
	}
	if fullText := fullTextSettings(index); fullText != nil {
		columns.types["\x00key"], columns.key = "Uint64", []string{"\x00key"}
	}
	all := map[string]bool{VectorIndexesCapability: true, VectorBitTypeCapability: true, FullTextIndexesCapability: true}
	if reason := nativeIndexRefusal(index, columns, all); reason != "" {
		return nil, fmt.Errorf("invalid YDB index addition: %s", reason)
	}
	return &NativeIndex{Index: index}, nil
}

// NativeIndexStatement writes the statement that adds a vector or full-text
// index to the table at path, as the host's embedded engine writes it:
// `ALTER TABLE `t` ADD INDEX `i` GLOBAL USING vector_kmeans_tree ON (...) WITH (...);`.
func NativeIndexStatement(path string, index *NativeIndex) string {
	return "ALTER TABLE " + QuoteIdentifier(path) + " ADD " + nativeIndexClause(index.Index) + ";"
}

// nativeIndexClause writes a vector or full-text index as an INDEX clause:
// `INDEX `i` GLOBAL USING <kind> ON (`a`) [COVER (`b`)] WITH (...)`. A vector
// index names its metric, vector_type, vector_dimension, levels and clusters
// in that order; a full-text index names its analyzers in name order, the
// language quoted. Both are the host's embedded engine's spellings.
func nativeIndexClause(spec *native.IndexIntent) string {
	clause := "INDEX " + QuoteIdentifier(spec.GetName()) + " GLOBAL USING " + nativeIndexKind(spec) + " ON (" + quoteList(spec.GetIndexColumns()) + ")"
	if len(spec.GetDataColumns()) != 0 {
		clause += " COVER (" + quoteList(spec.GetDataColumns()) + ")"
	}
	return clause + " WITH (" + strings.Join(nativeIndexOptions(spec), ", ") + ")"
}

func nativeIndexOptions(spec *native.IndexIntent) []string {
	if vector := spec.GetGlobalVectorKmeansTreeIndex(); vector != nil {
		settings := vector.GetVectorSettings()
		var options []string
		switch metric := settings.GetSettings().GetMetric(); metric {
		case native.VectorIndexSettings_SIMILARITY_INNER_PRODUCT:
			options = append(options, "similarity=inner_product")
		case native.VectorIndexSettings_SIMILARITY_COSINE:
			options = append(options, "similarity=cosine")
		case native.VectorIndexSettings_DISTANCE_COSINE:
			options = append(options, "distance=cosine")
		case native.VectorIndexSettings_DISTANCE_MANHATTAN:
			options = append(options, "distance=manhattan")
		case native.VectorIndexSettings_DISTANCE_EUCLIDEAN:
			options = append(options, "distance=euclidean")
		}
		vectorType := strings.ToLower(strings.TrimPrefix(settings.GetSettings().GetVectorType().String(), "VECTOR_TYPE_"))
		return append(options, "vector_type="+vectorType, "vector_dimension="+strconv.FormatUint(uint64(settings.GetSettings().GetVectorDimension()), 10),
			"levels="+strconv.FormatUint(uint64(settings.GetLevels()), 10), "clusters="+strconv.FormatUint(uint64(settings.GetClusters()), 10))
	}
	var analyzers *native.FulltextIndexSettings_Analyzers
	if columns := fullTextSettings(spec).GetColumns(); len(columns) == 1 {
		analyzers = columns[0].GetAnalyzers()
	}
	if analyzers == nil {
		return nil
	}
	options := map[string]string{}
	if analyzers.Tokenizer != nil {
		options["tokenizer"] = strings.ToLower(analyzers.GetTokenizer().String())
	}
	if analyzers.Language != nil {
		options["language"] = "'" + strings.ReplaceAll(analyzers.GetLanguage(), "'", "\\'") + "'"
	}
	for name, value := range map[string]*bool{"use_filter_lowercase": analyzers.UseFilterLowercase, "use_filter_stopwords": analyzers.UseFilterStopwords,
		"use_filter_ngram": analyzers.UseFilterNgram, "use_filter_edge_ngram": analyzers.UseFilterEdgeNgram, "use_filter_length": analyzers.UseFilterLength,
		"use_filter_snowball": analyzers.UseFilterSnowball} {
		if value != nil {
			options[name] = strconv.FormatBool(*value)
		}
	}
	for name, value := range map[string]*int32{"filter_ngram_min_length": analyzers.FilterNgramMinLength, "filter_ngram_max_length": analyzers.FilterNgramMaxLength,
		"filter_length_min": analyzers.FilterLengthMin, "filter_length_max": analyzers.FilterLengthMax} {
		if value != nil {
			options[name] = strconv.FormatInt(int64(*value), 10)
		}
	}
	names := make([]string, 0, len(options))
	for name := range options {
		names = append(names, name)
	}
	slices.Sort(names)
	result := make([]string, 0, len(names))
	for _, name := range names {
		result = append(result, name+"="+options[name])
	}
	return result
}
