package tablesource

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"maps"
	"math"
	"slices"
	"strconv"
	"strings"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	protocol "ptah.run/provider-sdk/protocol/experimental"
	"ptah.run/provider-sdk/provider"

	native "github.com/stokaro/ptah-ydb-plugin/internal/nativeprotocol"
	"github.com/stokaro/ptah-ydb-plugin/internal/ydbdecl"
	"github.com/stokaro/ptah-ydb-plugin/schema"
)

// The selectors of the provider's index-kind claims (contract 25.24, under
// source.index-kinds): the vector and full-text indexes a row table's native
// index set holds, and the local indexes a column table's storage holds.
const (
	SelectorNativeIndex = "index.native"
	SelectorLocalIndex  = "index.local"
)

// The methods of the indexes the provider reads in place of the frontend's
// common index, as YQL's USING clause spells them.
var (
	nativeMethods = []string{ydbdecl.VectorMethod, "fulltext_plain", "fulltext_relevance"}
	localMethods  = []string{"bloom_filter", "bloom_ngram_filter", "min_max"}
)

// indexTypes are the spellings of a type a declaration may write for each of
// methods, as the host's embedded engine reads them: the method, with an
// underscore or a space between its words, with or without the GLOBAL USING
// or LOCAL USING in front of it, in lower case. The host compares a type in
// lower case with single spaces.
func indexTypes(prefix string, methods []string) []string {
	var types []string
	for _, method := range methods {
		spaced := strings.ReplaceAll(method, "_", " ")
		types = append(types, method, prefix+" "+method)
		if spaced != method {
			types = append(types, spaced, prefix+" "+spaced)
		}
	}
	return types
}

// IndexKinds returns the provider's index-kind claims: a vector or full-text
// index declares the table's native index set, and a local index the column
// table's storage. For a declaration of one of these types the host builds no
// common index and sends the declaration whole.
func IndexKinds() []*protocol.SourceIndexKind {
	return []*protocol.SourceIndexKind{
		{Types: indexTypes("global using", nativeMethods), Selector: SelectorNativeIndex, Models: []string{schema.IndexProperties}},
		{Types: indexTypes("local using", localMethods), Selector: SelectorLocalIndex, Models: []string{schema.ColumnStorageProperties}},
	}
}

// indexKindFragment reports whether a fragment carries one of the provider's
// index-kind claims.
func indexKindFragment(fragment *protocol.SourceFragment) bool {
	claim := fragment.GetIndexKind()
	return claim != nil && slices.ContainsFunc(IndexKinds(), func(declared *protocol.SourceIndexKind) bool { return proto.Equal(claim, declared) })
}

// localIndexFragment reports whether a fragment declares a local index.
func localIndexFragment(fragment *protocol.SourceFragment) bool {
	return indexKindFragment(fragment) && fragment.GetIndexKind().GetSelector() == SelectorLocalIndex
}

// nativeIndexFragment reports whether a fragment declares a vector or
// full-text index.
func nativeIndexFragment(fragment *protocol.SourceFragment) bool {
	return indexKindFragment(fragment) && fragment.GetIndexKind().GetSelector() == SelectorNativeIndex
}

// ownedIndex is one index a declaration of an index-kind claim states: what
// the frontend read of it, and the options the provider claims on the
// format's index declaration, each as its text.
type ownedIndex struct {
	name             string
	kind             ydbdecl.IndexKind
	columns, include []string
	ops              string
	options          map[string]string
	// order are the options in the order the declaration writes them.
	order []string
}

// readOwnedIndex reads an index-kind fragment. A unique index and a comment
// are refused: YDB builds no unique vector, full-text or local index, and
// keeps no comment of one the provider could read back.
func readOwnedIndex(fragment *protocol.SourceFragment) (ownedIndex, error) {
	index := ownedIndex{name: fragment.GetSubject().GetName(), options: make(map[string]string)}
	written := ""
	for _, entry := range fragment.GetInheritedAttributes().GetEntries() {
		switch entry.GetKey() {
		case provider.IndexKindType:
			written = entry.GetValue().GetText()
		case provider.IndexKindColumns:
			index.columns = texts(entry.GetValue())
		case provider.IndexKindInclude:
			index.include = texts(entry.GetValue())
		case provider.IndexKindOps:
			index.ops = entry.GetValue().GetText()
		case provider.IndexKindUnique:
			return ownedIndex{}, fmt.Errorf("index %q: a YDB %s index is never unique", index.name, strings.TrimSpace(written))
		case provider.IndexKindComment:
			return ownedIndex{}, fmt.Errorf("index %q: a comment on a vector, full-text or local index has no place this provider reads back", index.name)
		}
	}
	kind, err := ydbdecl.IndexKindOf(written)
	if err != nil {
		return ownedIndex{}, fmt.Errorf("index %q: %w", index.name, err)
	}
	index.kind = kind
	for _, entry := range fragment.GetValue().GetMapping().GetEntries() {
		text, err := scalar(entry.GetValue())
		if errors.Is(err, errNull) {
			continue
		}
		if err != nil {
			return ownedIndex{}, fmt.Errorf("index %q: %s: %w", index.name, entry.GetKey(), err)
		}
		index.options[entry.GetKey()] = text
		index.order = append(index.order, entry.GetKey())
	}
	return index, index.refuseOtherOptions()
}

// texts are the texts of a sequence of texts.
func texts(value *protocol.SourceValue) []string {
	var result []string
	for _, item := range value.GetSequence().GetValues() {
		result = append(result, item.GetText())
	}
	return result
}

// refuseOtherOptions refuses an option of another kind of index than the
// declaration's, the first in the order the declaration writes them, and the
// operator class of an index other than a vector one.
func (o ownedIndex) refuseOtherOptions() error {
	var allowed []string
	switch {
	case o.kind == ydbdecl.IndexVector:
		allowed = ydbdecl.VectorAttributes()
	case o.kind.IsFullText():
		allowed = ydbdecl.FullTextAttributes()
	case o.kind == ydbdecl.IndexLocalMinMax:
		allowed = nil
	case o.kind == ydbdecl.IndexLocalBloom:
		allowed = []string{"false_positive_probability"}
	default:
		allowed = ydbdecl.LocalAttributes()
	}
	for _, name := range o.order {
		if !slices.Contains(allowed, name) {
			return fmt.Errorf("index %q: %s is not a setting of a %s index", o.name, name, o.kind)
		}
	}
	if o.ops != "" && o.kind != ydbdecl.IndexVector {
		return fmt.Errorf("index %q: an operator class names the metric of a vector index, and a %s index takes none", o.name, o.kind)
	}
	return nil
}

// spec is the native specification of a vector or full-text index.
func (o ownedIndex) spec() (*native.IndexIntent, error) {
	result := &native.IndexIntent{Name: o.name, IndexColumns: slices.Clone(o.columns), DataColumns: slices.Clone(o.include)}
	if o.kind == ydbdecl.IndexVector {
		values := cloned(o.options)
		declared, err := ydbdecl.DeclareVector(values, ydbdecl.VectorMethod)
		if err != nil {
			return nil, err
		}
		if err := ydbdecl.ResolveVectorOperator(declared, o.ops); err != nil {
			return nil, err
		}
		result.Type = &native.IndexIntent_GlobalVectorKmeansTreeIndex{GlobalVectorKmeansTreeIndex: &native.GlobalVectorKMeansTreeIndex{VectorSettings: vectorSettings(declared)}}
		return result, nil
	}
	values := cloned(o.options)
	values["type"] = o.kind.String()
	options, err := ydbdecl.ParseIndexOptions(values)
	if err != nil {
		return nil, err
	}
	settings := &native.FulltextIndexSettings{}
	if len(options) != 0 {
		column := &native.FulltextIndexSettings_ColumnAnalyzers{Analyzers: analyzers(options)}
		if len(o.columns) != 0 {
			column.Column = new(o.columns[len(o.columns)-1])
		}
		settings.Columns = []*native.FulltextIndexSettings_ColumnAnalyzers{column}
	}
	if o.kind == ydbdecl.IndexFullTextPlain {
		result.Type = &native.IndexIntent_GlobalFulltextPlainIndex{GlobalFulltextPlainIndex: &native.GlobalFulltextPlainIndex{FulltextSettings: settings}}
	} else {
		result.Type = &native.IndexIntent_GlobalFulltextRelevanceIndex{GlobalFulltextRelevanceIndex: &native.GlobalFulltextRelevanceIndex{FulltextSettings: settings}}
	}
	return result, nil
}

// NativeIndex is the native specification of the vector or full-text index
// name of kind, on columns and covering include, with the settings options
// writes, each as its text: what a YAML or Go source's declaration of the
// same index states. A YQL CREATE TABLE's index reads through it too, so an
// index means the same whichever format declares it.
func NativeIndex(name string, kind ydbdecl.IndexKind, columns, include []string, options map[string]string) (*native.IndexIntent, error) {
	return ownedIndex{name: name, kind: kind, columns: slices.Clone(columns), include: slices.Clone(include), options: cloned(options)}.spec()
}

// LocalIndex is the local index name of a column table, of kind, on columns
// and covering include, with the settings options writes, as [NativeIndex] is
// a vector or full-text index.
func LocalIndex(name string, kind ydbdecl.IndexKind, columns, include []string, options map[string]string) (*native.LocalIndexObservation, error) {
	return ownedIndex{name: name, kind: kind, columns: slices.Clone(columns), include: slices.Clone(include), options: cloned(options)}.local()
}

// cloned is a copy of options.
func cloned(options map[string]string) map[string]string {
	result := make(map[string]string, len(options))
	maps.Copy(result, options)
	return result
}

var (
	vectorMetrics = map[string]native.VectorIndexSettings_Metric{
		"similarity:inner_product": native.VectorIndexSettings_SIMILARITY_INNER_PRODUCT, "similarity:cosine": native.VectorIndexSettings_SIMILARITY_COSINE,
		"distance:cosine": native.VectorIndexSettings_DISTANCE_COSINE, "distance:manhattan": native.VectorIndexSettings_DISTANCE_MANHATTAN,
		"distance:euclidean": native.VectorIndexSettings_DISTANCE_EUCLIDEAN,
	}
	vectorTypes = map[string]native.VectorIndexSettings_VectorType{
		"float": native.VectorIndexSettings_VECTOR_TYPE_FLOAT, "uint8": native.VectorIndexSettings_VECTOR_TYPE_UINT8,
		"int8": native.VectorIndexSettings_VECTOR_TYPE_INT8, "bit": native.VectorIndexSettings_VECTOR_TYPE_BIT,
	}
)

// vectorSettings are a declared vector index's settings as YDB describes
// them. A setting the declaration leaves out stays unset, and the planner
// refuses the index naming it. A count above what a description holds is
// held at its largest value, which the planner refuses as out of range.
func vectorSettings(declared *ydbdecl.VectorIndex) *native.KMeansTreeSettings {
	settings := &native.VectorIndexSettings{}
	switch {
	case declared.Distance != "" && declared.Similarity != "":
		// Two metrics: the planner refuses the index with no metric named.
	case declared.Distance != "":
		settings.Metric = new(vectorMetrics["distance:"+declared.Distance])
	case declared.Similarity != "":
		settings.Metric = new(vectorMetrics["similarity:"+declared.Similarity])
	}
	if declared.VectorType != "" {
		settings.VectorType = new(vectorTypes[declared.VectorType])
	}
	count := func(value uint64) *uint32 {
		if value == 0 {
			return nil
		}
		return new(uint32(min(value, math.MaxUint32)))
	}
	settings.VectorDimension = count(declared.Dimension)
	result := &native.KMeansTreeSettings{Levels: count(declared.Levels), Clusters: count(declared.Clusters)}
	if proto.Size(settings) != 0 {
		result.Settings = settings
	}
	return result
}

// analyzers are a full-text index's resolved options as YDB describes them.
func analyzers(options map[string]string) *native.FulltextIndexSettings_Analyzers {
	result := &native.FulltextIndexSettings_Analyzers{}
	flag := func(name string) *bool {
		value, written := options[name]
		if !written {
			return nil
		}
		return new(value == "true")
	}
	length := func(name string) *int32 {
		value, written := options[name]
		if !written {
			return nil
		}
		parsed, _ := strconv.ParseInt(value, 10, 32)
		return new(int32(parsed))
	}
	switch options["tokenizer"] {
	case "standard":
		result.Tokenizer = new(native.FulltextIndexSettings_STANDARD)
	case "whitespace":
		result.Tokenizer = new(native.FulltextIndexSettings_WHITESPACE)
	case "keyword":
		result.Tokenizer = new(native.FulltextIndexSettings_KEYWORD)
	}
	if language, written := options["language"]; written {
		result.Language = new(language)
	}
	result.UseFilterLowercase, result.UseFilterStopwords = flag("use_filter_lowercase"), flag("use_filter_stopwords")
	result.UseFilterNgram, result.UseFilterEdgeNgram = flag("use_filter_ngram"), flag("use_filter_edge_ngram")
	result.FilterNgramMinLength, result.FilterNgramMaxLength = length("filter_ngram_min_length"), length("filter_ngram_max_length")
	result.UseFilterLength, result.FilterLengthMin, result.FilterLengthMax = flag("use_filter_length"), length("filter_length_min"), length("filter_length_max")
	result.UseFilterSnowball = flag("use_filter_snowball")
	return result
}

// local is the local index of a column table a declaration states, with the
// settings it writes. The planner reads a setting it leaves out as the
// server's default.
func (o ownedIndex) local() (*native.LocalIndexObservation, error) {
	values := cloned(o.options)
	values["type"] = o.kind.String()
	if _, err := ydbdecl.ParseIndexOptions(values); err != nil {
		return nil, err
	}
	result := &native.LocalIndexObservation{Name: o.name, Columns: slices.Clone(o.columns)}
	probability := func() *float64 {
		value, written := o.options["false_positive_probability"]
		if !written {
			return nil
		}
		parsed, _ := strconv.ParseFloat(strings.TrimSpace(value), 64)
		return new(parsed)
	}
	switch o.kind {
	case ydbdecl.IndexLocalBloom:
		result.Kind = &native.LocalIndexObservation_Bloom{Bloom: &native.LocalBloomSettings{FalsePositiveProbability: probability()}}
	case ydbdecl.IndexLocalNgram:
		ngram := &native.LocalNGramSettings{FalsePositiveProbability: probability()}
		if value, written := o.options["ngram_size"]; written {
			parsed, _ := strconv.ParseUint(strings.TrimSpace(value), 10, 64)
			ngram.NgramSize = new(parsed)
		}
		if value, written := o.options["case_sensitive"]; written {
			ngram.CaseSensitive = new(strings.TrimSpace(value) == "true")
		}
		result.Kind = &native.LocalIndexObservation_Ngram{Ngram: ngram}
	default:
		result.Kind = &native.LocalIndexObservation_MinMax{MinMax: &native.LocalMinMaxSettings{}}
	}
	if len(o.include) != 0 {
		return nil, fmt.Errorf("index %q: a YDB local index covers no column", o.name)
	}
	return result, nil
}

// NativeIndexes is the native index set a document's vector and full-text
// index declarations state for one table.
type NativeIndexes struct {
	// Binding is the table the indexes are on.
	Binding *protocol.SourceBinding
	// Fragment is the index in the document of the first fragment that
	// declares one of the indexes. The facet is returned with it.
	Fragment int
	// Value is the normalized desired facet, owned by the table.
	Value *protocol.NormalizedValue
}

type pendingNative struct {
	binding  *protocol.SourceBinding
	fragment int
	set      native.IndexSetIntent
}

// DecodeNativeIndexes reads every vector and full-text index declaration of
// document into one native index set for each table, in the order of each
// table's first declaration. tables are the facets the table decoder read: a
// vector or full-text index of a table the document declares as a column
// table is refused, since YDB keeps them on row tables, and so is an index
// name declared twice. It reads no file and no environment.
func DecodeNativeIndexes(ctx context.Context, document *protocol.SourceDocument, tables []Table) ([]NativeIndexes, error) {
	var order []string
	byBinding := make(map[string]*pendingNative)
	for position, fragment := range document.GetFragments() {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if !nativeIndexFragment(fragment) {
			continue
		}
		binding, err := Binding(document, fragment, "an index")
		if err != nil {
			return nil, fmt.Errorf("index %q: %w", fragment.GetSubject().GetName(), err)
		}
		index, err := readOwnedIndex(fragment)
		if err != nil {
			return nil, tableError(tableLabel(binding.GetObject()), err)
		}
		if columnTable(tables, binding) {
			return nil, tableError(tableLabel(binding.GetObject()), fmt.Errorf("index %q: a column table takes local indexes only; a %s index belongs to a row table",
				index.name, index.kind))
		}
		spec, err := index.spec()
		if err != nil {
			return nil, tableError(tableLabel(binding.GetObject()), fmt.Errorf("index %q: %w", index.name, err))
		}
		target, seen := byBinding[binding.GetId()]
		if !seen {
			target = &pendingNative{binding: binding, fragment: position}
			byBinding[binding.GetId()] = target
			order = append(order, binding.GetId())
		}
		if slices.ContainsFunc(target.set.Indexes, func(declared *native.IndexDeclaration) bool {
			return declared.GetSpecification().GetName() == spec.GetName()
		}) {
			return nil, tableError(tableLabel(binding.GetObject()), fmt.Errorf("index %q is declared twice", spec.GetName()))
		}
		target.set.Indexes = append(target.set.Indexes, &native.IndexDeclaration{Specification: spec})
	}
	result := make([]NativeIndexes, 0, len(order))
	for _, id := range order {
		target := byBinding[id]
		value, err := nativeSetValue(document, target)
		if err != nil {
			return nil, tableError(tableLabel(target.binding.GetObject()), err)
		}
		result = append(result, NativeIndexes{Binding: target.binding, Fragment: target.fragment, Value: value})
	}
	return result, nil
}

// columnTable reports whether the table decoder read the table of binding as
// a column table.
func columnTable(tables []Table, binding *protocol.SourceBinding) bool {
	return slices.ContainsFunc(tables, func(table Table) bool {
		return table.Binding.GetId() == binding.GetId() && table.Value.GetValue().GetKind() == schema.ColumnStorageProperties
	})
}

func nativeSetValue(document *protocol.SourceDocument, target *pendingNative) (*protocol.NormalizedValue, error) {
	owner := proto.CloneOf(target.binding.GetObject())
	payload, err := protojson.Marshal(&target.set)
	if err != nil {
		return nil, err
	}
	model := schema.NativeDesiredModel(schema.IndexProperties)
	value, err := schema.NormalizeDesired(&protocol.Envelope{ProviderId: schema.ProviderID, Kind: model.Kind, SchemaVersion: model.SchemaVersion,
		Encoding: model.Encoding, DefinitionSha256: model.DefinitionSha256, Owner: owner, Payload: payload})
	if err != nil {
		return nil, err
	}
	return &protocol.NormalizedValue{Id: "indexes." + document.GetId() + "." + strconv.Itoa(target.fragment), Value: value, NormalizedOwner: proto.CloneOf(owner),
		CanonicalFormat: model.CanonicalFormat, CanonicalProjection: bytes.Clone(value.Payload)}, nil
}

// localIndexes reads the local index declarations of document for each
// table, by binding, in the order the document writes them. An HCL document
// declares none: HCL cannot state a column table's storage, which a local
// index belongs to.
func localIndexes(document *protocol.SourceDocument) (map[string][]*native.LocalIndexObservation, error) {
	result := make(map[string][]*native.LocalIndexObservation)
	for _, fragment := range document.GetFragments() {
		if !localIndexFragment(fragment) {
			continue
		}
		binding, err := Binding(document, fragment, "an index")
		if err != nil {
			return nil, fmt.Errorf("index %q: %w", fragment.GetSubject().GetName(), err)
		}
		label := tableLabel(binding.GetObject())
		if document.GetFormat() == protocol.SourceDocument_HCL {
			return nil, tableError(label, fmt.Errorf("index %q: an HCL document declares no local index, since HCL cannot state the storage of the column table it belongs to",
				fragment.GetSubject().GetName()))
		}
		index, err := readOwnedIndex(fragment)
		if err != nil {
			return nil, tableError(label, err)
		}
		local, err := index.local()
		if err != nil {
			return nil, tableError(label, fmt.Errorf("index %q: %w", index.name, err))
		}
		if slices.ContainsFunc(result[binding.GetId()], func(declared *native.LocalIndexObservation) bool { return declared.GetName() == local.GetName() }) {
			return nil, tableError(label, fmt.Errorf("index %q is declared twice", local.GetName()))
		}
		result[binding.GetId()] = append(result[binding.GetId()], local)
	}
	return result, nil
}
