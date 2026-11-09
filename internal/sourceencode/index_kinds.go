package sourceencode

import (
	"strconv"

	"google.golang.org/protobuf/proto"
	protocol "ptah.run/provider-sdk/protocol/experimental"
	"ptah.run/provider-sdk/provider"

	native "github.com/stokaro/ptah-ydb-plugin/internal/nativeprotocol"
	"github.com/stokaro/ptah-ydb-plugin/internal/tablesource"
)

// The vector settings, as an index declaration names a metric and an element
// type.
var (
	metricSettings = map[native.VectorIndexSettings_Metric][2]string{
		native.VectorIndexSettings_SIMILARITY_INNER_PRODUCT: {"similarity", "inner_product"}, native.VectorIndexSettings_SIMILARITY_COSINE: {"similarity", "cosine"},
		native.VectorIndexSettings_DISTANCE_COSINE: {"distance", "cosine"}, native.VectorIndexSettings_DISTANCE_MANHATTAN: {"distance", "manhattan"},
		native.VectorIndexSettings_DISTANCE_EUCLIDEAN: {"distance", "euclidean"},
	}
	elementTypes = map[native.VectorIndexSettings_VectorType]string{
		native.VectorIndexSettings_VECTOR_TYPE_FLOAT: "float", native.VectorIndexSettings_VECTOR_TYPE_UINT8: "uint8",
		native.VectorIndexSettings_VECTOR_TYPE_INT8: "int8", native.VectorIndexSettings_VECTOR_TYPE_BIT: "bit",
	}
	tokenizers = map[native.FulltextIndexSettings_Tokenizer]string{
		native.FulltextIndexSettings_STANDARD: "standard", native.FulltextIndexSettings_WHITESPACE: "whitespace", native.FulltextIndexSettings_KEYWORD: "keyword",
	}
)

// ownedOption is one setting of a vector, full-text or local index as a
// declaration writes it: its text, and whether it is a count, which HCL
// writes as a number.
type ownedOption struct {
	name, text string
	count      bool
}

// ownedIndex is a vector, full-text or local index as a declaration of one of
// the provider's index kinds writes it.
type ownedIndex struct {
	selector, name, kind string
	columns, include     []string
	options              []ownedOption
}

// nativeOwned writes a vector or full-text index as its declaration: its type
// as the host's embedded engine writes it, `GLOBAL USING <method>`, its key
// and covered columns, and its settings in the order of the native
// description.
func nativeOwned(spec *native.IndexIntent) ownedIndex {
	index := ownedIndex{selector: tablesource.SelectorNativeIndex, name: spec.GetName(), columns: spec.GetIndexColumns(), include: spec.GetDataColumns()}
	add := func(name, text string, count bool) {
		index.options = append(index.options, ownedOption{name: name, text: text, count: count})
	}
	flag := func(name string, value *bool) {
		if value != nil {
			add(name, strconv.FormatBool(*value), false)
		}
	}
	length := func(name string, value *int32) {
		if value != nil {
			add(name, strconv.FormatInt(int64(*value), 10), true)
		}
	}
	if vector := spec.GetGlobalVectorKmeansTreeIndex(); vector != nil {
		index.kind = "GLOBAL USING vector_kmeans_tree"
		settings := vector.GetVectorSettings()
		if metric := settings.GetSettings().Metric; metric != nil {
			named := metricSettings[*metric]
			add(named[0], named[1], false)
		}
		if element := settings.GetSettings().VectorType; element != nil {
			add("vector_type", elementTypes[*element], false)
		}
		for _, count := range []struct {
			name  string
			value *uint32
		}{{"vector_dimension", settings.GetSettings().VectorDimension}, {"levels", settings.Levels}, {"clusters", settings.Clusters}} {
			if count.value != nil {
				add(count.name, strconv.FormatUint(uint64(*count.value), 10), true)
			}
		}
		return index
	}
	index.kind = "GLOBAL USING fulltext_relevance"
	settings := spec.GetGlobalFulltextRelevanceIndex().GetFulltextSettings()
	if plain := spec.GetGlobalFulltextPlainIndex(); plain != nil {
		index.kind, settings = "GLOBAL USING fulltext_plain", plain.GetFulltextSettings()
	}
	for _, column := range settings.GetColumns() {
		analyzers := column.GetAnalyzers()
		if tokenizer := analyzers.Tokenizer; tokenizer != nil {
			add("tokenizer", tokenizers[*tokenizer], false)
		}
		if language := analyzers.Language; language != nil {
			add("language", *language, false)
		}
		flag("use_filter_lowercase", analyzers.UseFilterLowercase)
		flag("use_filter_stopwords", analyzers.UseFilterStopwords)
		flag("use_filter_ngram", analyzers.UseFilterNgram)
		flag("use_filter_edge_ngram", analyzers.UseFilterEdgeNgram)
		length("filter_ngram_min_length", analyzers.FilterNgramMinLength)
		length("filter_ngram_max_length", analyzers.FilterNgramMaxLength)
		flag("use_filter_length", analyzers.UseFilterLength)
		length("filter_length_min", analyzers.FilterLengthMin)
		length("filter_length_max", analyzers.FilterLengthMax)
		flag("use_filter_snowball", analyzers.UseFilterSnowball)
	}
	return index
}

// localOwned writes a column table's local index as its declaration: its
// type as the host's embedded engine writes it, the method alone, its column
// and the settings it states.
func localOwned(local *native.LocalIndexObservation) ownedIndex {
	index := ownedIndex{selector: tablesource.SelectorLocalIndex, name: local.GetName(), columns: local.GetColumns()}
	probability := func(value *float64) {
		if value != nil {
			index.options = append(index.options, ownedOption{name: "false_positive_probability", text: strconv.FormatFloat(*value, 'g', -1, 64)})
		}
	}
	switch kind := local.GetKind().(type) {
	case *native.LocalIndexObservation_Bloom:
		index.kind = "bloom_filter"
		probability(kind.Bloom.FalsePositiveProbability)
	case *native.LocalIndexObservation_Ngram:
		index.kind = "bloom_ngram_filter"
		probability(kind.Ngram.FalsePositiveProbability)
		if size := kind.Ngram.NgramSize; size != nil {
			index.options = append(index.options, ownedOption{name: "ngram_size", text: strconv.FormatUint(*size, 10), count: true})
		}
		if sensitive := kind.Ngram.CaseSensitive; sensitive != nil {
			index.options = append(index.options, ownedOption{name: "case_sensitive", text: strconv.FormatBool(*sensitive)})
		}
	default:
		index.kind = "min_max"
	}
	return index
}

// part is the index-kind part that declares the index on the table of
// binding. HCL writes a count as a number; every other value, and every value
// of the other formats, is text.
func (o ownedIndex) part(binding *protocol.SourceBinding, format protocol.SourceDocument_Format) *protocol.EncodedSourcePart {
	var claim *protocol.SourceIndexKind
	for _, declared := range tablesource.IndexKinds() {
		if declared.GetSelector() == o.selector {
			claim = declared
		}
	}
	table := binding.GetObject()
	inherited := []*protocol.SourceEntry{field(provider.IndexKindType, o.kind), {Key: provider.IndexKindColumns, Value: textList(o.columns)}}
	if len(o.include) != 0 {
		inherited = append(inherited, &protocol.SourceEntry{Key: provider.IndexKindInclude, Value: textList(o.include)})
	}
	var options []*protocol.SourceEntry
	for _, option := range o.options {
		value := text(option.text)
		if option.count && format == protocol.SourceDocument_HCL {
			value = &protocol.SourceValue{Value: &protocol.SourceValue_Number{Number: option.text}}
		}
		options = append(options, &protocol.SourceEntry{Key: option.name, Value: value})
	}
	return &protocol.EncodedSourcePart{Fragment: &protocol.SourceFragment{Id: "index." + binding.GetId() + "." + o.name, Selector: claim.GetSelector(),
		IndexKind: proto.CloneOf(claim), BindingId: binding.GetId(),
		Subject: &protocol.Reference{Engine: table.GetEngine(), Kind: "index", Database: table.GetDatabase(), Catalog: table.GetCatalog(), Schema: table.GetSchema(),
			Path: []string{table.GetName()}, Name: o.name},
		InheritedAttributes: &protocol.SourceMapping{Entries: inherited}, Content: &protocol.SourceFragment_Value{Value: mapping(options...)}}}
}

// textList is a sequence of texts.
func textList(names []string) *protocol.SourceValue {
	values := make([]*protocol.SourceValue, 0, len(names))
	for _, name := range names {
		values = append(values, text(name))
	}
	return &protocol.SourceValue{Value: &protocol.SourceValue_Sequence{Sequence: &protocol.SourceSequence{Values: values}}}
}
