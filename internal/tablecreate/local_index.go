package tablecreate

import (
	"bytes"
	"cmp"
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
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

// The session capabilities a column table's local index of each kind
// requires, spelled as the host's vocabulary spells them. A session reads each
// from its feature flag (see internal/featureflags).
const (
	// LocalBloomIndexesCapability is a bloom_filter index:
	// EnableLocalBloomFilterIndex, on by default from 26.2.
	LocalBloomIndexesCapability = "local_bloom_indexes"
	// LocalNgramIndexesCapability is a bloom_ngram_filter index:
	// EnableLocalBloomNgramFilterIndex, on by default from 26.2.
	LocalNgramIndexesCapability = "local_ngram_indexes"
	// LocalMinMaxIndexesCapability is a min_max index: EnableLocalMinMaxIndex,
	// off by default on 26.2.
	LocalMinMaxIndexesCapability = "local_min_max_indexes"
)

// The settings YDB gives a local index that states none. Measured on
// 26.2.1.14: a bloom_filter index created without a probability describes it
// as 0.1, and a bloom_ngram_filter index created without settings describes
// neither its probability nor its n-gram size; its constructor takes 0.1, 3
// and case-sensitive matching, as the host's embedded engine records.
const (
	defaultLocalProbability = 0.1
	defaultNgramSize        = 3
	minNgramSize            = 3
	maxNgramSize            = 8
)

// localKind is what one kind of local index is: the method YQL writes after
// USING, the capability it requires and the feature flag that decides the
// capability.
type localKind struct {
	method, capability, flag string
}

// localKindOf returns the kind of index, which a valid index has.
func localKindOf(index *native.LocalIndexObservation) localKind {
	switch index.GetKind().(type) {
	case *native.LocalIndexObservation_Ngram:
		return localKind{method: "bloom_ngram_filter", capability: LocalNgramIndexesCapability, flag: "EnableLocalBloomNgramFilterIndex"}
	case *native.LocalIndexObservation_MinMax:
		return localKind{method: "min_max", capability: LocalMinMaxIndexesCapability, flag: "EnableLocalMinMaxIndex"}
	default:
		return localKind{method: "bloom_filter", capability: LocalBloomIndexesCapability, flag: "EnableLocalBloomFilterIndex"}
	}
}

// ResolvedLocalIndex returns index in the form a comparison and a statement
// read it: its name, its columns and its kind with every setting, the server's
// default where index states none. The storage the server places it in and
// the encodings, which have one value each, are left out: YDB describes an
// index created with its table and one added later in other storage, and no
// statement names it.
func ResolvedLocalIndex(index *native.LocalIndexObservation) *native.LocalIndexObservation {
	result := &native.LocalIndexObservation{Name: index.GetName(), Columns: slices.Clone(index.GetColumns())}
	switch kind := index.GetKind().(type) {
	case *native.LocalIndexObservation_Bloom:
		result.Kind = &native.LocalIndexObservation_Bloom{Bloom: &native.LocalBloomSettings{
			FalsePositiveProbability: new(cmp.Or(kind.Bloom.GetFalsePositiveProbability(), defaultLocalProbability))}}
	case *native.LocalIndexObservation_Ngram:
		caseSensitive := true
		if kind.Ngram.CaseSensitive != nil {
			caseSensitive = kind.Ngram.GetCaseSensitive()
		}
		result.Kind = &native.LocalIndexObservation_Ngram{Ngram: &native.LocalNGramSettings{
			FalsePositiveProbability: new(cmp.Or(kind.Ngram.GetFalsePositiveProbability(), defaultLocalProbability)),
			NgramSize:                new(cmp.Or(kind.Ngram.GetNgramSize(), defaultNgramSize)), CaseSensitive: new(caseSensitive)}}
	case *native.LocalIndexObservation_MinMax:
		result.Kind = &native.LocalIndexObservation_MinMax{MinMax: &native.LocalMinMaxSettings{}}
	}
	return result
}

// LocalIndexEqual reports whether two descriptions of a local index describe
// one index: the same name, columns, kind and settings, a setting one side
// leaves to the server's default being that default.
func LocalIndexEqual(a, b *native.LocalIndexObservation) bool {
	return proto.Equal(ResolvedLocalIndex(a), ResolvedLocalIndex(b))
}

// localIndexShapeRefusal says why YDB refuses index on a column table whose
// columns have the given types, or returns the empty string. It needs no
// capability, so a creation's payload is held to it when it is decoded.
func localIndexShapeRefusal(index *native.LocalIndexObservation, types map[string]string) string {
	subject := fmt.Sprintf("local index %q", index.GetName())
	if len(index.GetColumns()) != 1 {
		// Measured on 26.2.1.14 for bloom_filter, which the host's embedded
		// engine accepts over several columns: `Local bloom index on column
		// tables requires exactly one index column`.
		return subject + ": a YDB local index reads exactly one column"
	}
	column := index.GetColumns()[0]
	kind, declared := types[column]
	switch {
	case !declared:
		return fmt.Sprintf("%s reads column %q, which the table does not declare", subject, column)
	case index.GetNgram() != nil && kind != "Utf8":
		// 26.2.1.14 answers `inappropriate column type for bloom ngramm index`
		// only after the statement runs.
		return fmt.Sprintf("%s: n-gram index column %q must be Utf8", subject, column)
	}
	resolved := ResolvedLocalIndex(index)
	if ngram := resolved.GetNgram(); ngram != nil {
		if size := ngram.GetNgramSize(); size < minNgramSize || size > maxNgramSize {
			return fmt.Sprintf("%s: ngram_size must be between %d and %d", subject, minNgramSize, maxNgramSize)
		}
		// YDB 26.2 aborts in TOlapSchemaUpdate instead of returning a YQL
		// error when the probability asks for more n-gram hashes than eight,
		// as the host's embedded engine records.
		if math.Floor(-math.Log2(ngram.GetFalsePositiveProbability())) > maxNgramSize {
			return subject + ": false_positive_probability must produce at most 8 n-gram hashes (greater than 1/512)"
		}
	}
	return ""
}

// localIndexCapabilityRefusal says why a target with capabilities cannot
// build index's kind, or returns the empty string. Planning asks it of each
// index a plan creates; an index a table keeps needs nothing of the target.
func localIndexCapabilityRefusal(index *native.LocalIndexObservation, capabilities map[string]bool) string {
	if kind := localKindOf(index); !capabilities[kind.capability] {
		return fmt.Sprintf("local index %q is a %s index, which requires target capability %s, unavailable on this ydb target "+
			"(YDB keeps it behind the %s feature flag; a session reads the flag when the address names the monitoring endpoint)",
			index.GetName(), kind.method, kind.capability, kind.flag)
	}
	return ""
}

// LocalIndexClause writes index as YQL writes it after a column table's
// columns and after ADD, as the host's embedded engine writes it: `INDEX
// `name` LOCAL USING bloom_filter ON (`c`) WITH (false_positive_probability =
// 0.1)`, every setting named, in name order. A min_max index has none.
func LocalIndexClause(index *native.LocalIndexObservation) string {
	resolved := ResolvedLocalIndex(index)
	clause := "INDEX " + QuoteIdentifier(resolved.GetName()) + " LOCAL USING " + localKindOf(resolved).method + " ON (" + quoteList(resolved.GetColumns()) + ")"
	var settings []string
	switch kind := resolved.GetKind().(type) {
	case *native.LocalIndexObservation_Bloom:
		settings = append(settings, "false_positive_probability = "+strconv.FormatFloat(kind.Bloom.GetFalsePositiveProbability(), 'g', -1, 64))
	case *native.LocalIndexObservation_Ngram:
		settings = append(settings, "case_sensitive = "+strconv.FormatBool(kind.Ngram.GetCaseSensitive()),
			"false_positive_probability = "+strconv.FormatFloat(kind.Ngram.GetFalsePositiveProbability(), 'g', -1, 64),
			"ngram_size = "+strconv.FormatUint(kind.Ngram.GetNgramSize(), 10))
	}
	if len(settings) == 0 {
		return clause
	}
	return clause + " WITH (" + strings.Join(settings, ", ") + ")"
}

// localIndexChange is one change of a column table's local indexes: an
// addition carries the declared index, a removal the index as the table holds
// it.
type localIndexChange struct {
	add, drop *native.LocalIndexObservation
}

func (c localIndexChange) name() string {
	if c.add != nil {
		return c.add.GetName()
	}
	return c.drop.GetName()
}

// localIndexesManaged reports whether a declaration manages the local
// indexes of its table: it states at least one. No source declares local
// indexes but the native column-storage facet, so one that states none leaves
// the indexes the table holds alone rather than dropping them.
func localIndexesManaged(desired *native.ColumnStorageIntent) bool {
	return len(desired.GetLocalIndexes()) != 0
}

// planLocalIndexes returns the changes that give a column table the local
// indexes the declaration states, in name order, a removal before the addition
// that replaces an index under its name. An index the table holds under a
// declared name with another kind, column or setting is dropped and added
// again, and an index the declaration renames is dropped under its old name
// and added under its new one, as the host's embedded engine plans either on a
// target without index_rename: the host compares no local index, so it pairs
// none for a rename. An index to
// add is held to the rules of its creation. A non-empty reason refuses the
// plan.
func planLocalIndexes(desired *native.ColumnStorageIntent, held *native.ColumnStorageObservation, types map[string]string, capabilities map[string]bool) ([]localIndexChange, string) {
	if !localIndexesManaged(desired) {
		return nil, ""
	}
	wanted := map[string]*native.LocalIndexObservation{}
	for _, index := range desired.GetLocalIndexes() {
		wanted[index.GetName()] = index
	}
	have := map[string]*native.LocalIndexObservation{}
	for _, index := range held.GetLocalIndexes() {
		have[index.GetName()] = index
	}
	var names []string
	for name := range wanted {
		names = append(names, name)
	}
	for name := range have {
		if wanted[name] == nil {
			names = append(names, name)
		}
	}
	slices.Sort(names)
	var changes []localIndexChange
	for _, name := range names {
		want, declared := wanted[name]
		current, present := have[name]
		if declared && present && LocalIndexEqual(want, current) {
			continue
		}
		if present {
			changes = append(changes, localIndexChange{drop: proto.CloneOf(current)})
		}
		if declared {
			if reason := cmp.Or(localIndexCapabilityRefusal(want, capabilities), localIndexShapeRefusal(want, types)); reason != "" {
				return nil, reason
			}
			changes = append(changes, localIndexChange{add: ResolvedLocalIndex(want)})
		}
	}
	return changes, ""
}

// localIndexesDiffer reports whether a declaration that manages the table's
// local indexes states other ones than the table holds.
func localIndexesDiffer(desired *native.ColumnStorageIntent, held *native.ColumnStorageObservation) bool {
	if !localIndexesManaged(desired) {
		return false
	}
	if len(desired.GetLocalIndexes()) != len(held.GetLocalIndexes()) {
		return true
	}
	for _, want := range desired.GetLocalIndexes() {
		at := slices.IndexFunc(held.GetLocalIndexes(), func(index *native.LocalIndexObservation) bool { return index.GetName() == want.GetName() })
		if at < 0 || !LocalIndexEqual(want, held.GetLocalIndexes()[at]) {
			return true
		}
	}
	return false
}

// ColumnLocalIndexOperationKind identifies one step of a change of a column
// table's local indexes.
const ColumnLocalIndexOperationKind = schema.ProviderID + "/column-table-local-index-operation"

var columnLocalIndexOperationModel = buildColumnLocalIndexOperationModel()

// ColumnLocalIndexModel binds the payload of a column table's local-index
// step, its precondition and its confirmation.
func ColumnLocalIndexModel() *protocol.ValueModel {
	return proto.CloneOf(columnLocalIndexOperationModel)
}

func buildColumnLocalIndexOperationModel() *protocol.ValueModel {
	raw, err := json.Marshal(map[string]any{
		"kind": ColumnLocalIndexOperationKind, "representation": "operation", "schema_version": 1, "encoding": "ydb-protojson",
		"message":      string((&native.ColumnTableLocalIndexAlteration{}).ProtoReflect().Descriptor().FullName()),
		"index":        "exactly one of add and drop; add names one column, its kind and every setting, defaults included, and no storage; drop is the index as the table held it",
		"render":       "YQL ALTER TABLE ... ADD INDEX `name` LOCAL USING kind ON (`column`) WITH (...) for add, ALTER TABLE ... DROP INDEX `name` for drop, as the host's embedded engine writes them",
		"execution":    "the statement once through the query service, outside SQL transactions; no retries; the absolute path is bound at dispatch",
		"precondition": "the table is a column table; for add it holds no index of that name and holds the column, Utf8 for an n-gram index; for drop it holds the index as captured; the session must name the monitoring endpoint, which the read uses",
		"confirmation": "read the table service's and the monitoring endpoint's descriptions back: the added index is held as requested or the dropped one is gone, and every other local index, the layout and the TTL are unchanged",
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
	return &protocol.ValueModel{Kind: ColumnLocalIndexOperationKind, Representation: protocol.ValueInput_OPERATION, SchemaVersion: 1, Encoding: "ydb-protojson",
		DefinitionJson: canonical, DefinitionSha256: hex.EncodeToString(sum[:]), CanonicalFormat: ColumnLocalIndexOperationKind + "/json-v1"}
}

// CaptureColumnLocalIndex builds the operation for one local-index step
// planned against a column table in owner.
func CaptureColumnLocalIndex(id string, owner *protocol.Reference, alteration *native.ColumnTableLocalIndexAlteration) (*protocol.NormalizedValue, error) {
	payload, err := protojson.Marshal(alteration)
	if err != nil {
		return nil, err
	}
	definition := ColumnLocalIndexModel()
	return NormalizeColumnLocalIndex(&protocol.ValueInput{Id: id, Representation: protocol.ValueInput_OPERATION, Value: &protocol.Envelope{
		ProviderId: schema.ProviderID, Kind: ColumnLocalIndexOperationKind, Owner: proto.CloneOf(owner), SchemaVersion: 1, Encoding: definition.Encoding,
		DefinitionSha256: definition.DefinitionSha256, Payload: payload}})
}

// NormalizeColumnLocalIndex validates a column table's local-index step and
// reconstructs its safety facts from the payload, never from the sender.
func NormalizeColumnLocalIndex(input *protocol.ValueInput) (*protocol.NormalizedValue, error) {
	value, definition := input.GetValue(), ColumnLocalIndexModel()
	if input.GetRepresentation() != protocol.ValueInput_OPERATION || value.GetProviderId() != schema.ProviderID || value.GetKind() != definition.Kind ||
		value.GetSchemaVersion() != 1 || value.GetEncoding() != definition.Encoding || value.GetDefinitionSha256() != definition.DefinitionSha256 {
		return nil, fmt.Errorf("YDB column table local-index step does not match its negotiated operation model")
	}
	if err := validateOwner(value.Owner); err != nil {
		return nil, err
	}
	alteration, err := decodeColumnLocalIndex(value.Payload)
	if err != nil {
		return nil, err
	}
	payload, err := protojson.Marshal(alteration)
	if err != nil {
		return nil, err
	}
	payload, err = modeljson.Canonical(payload)
	if err != nil {
		return nil, err
	}
	result := proto.CloneOf(value)
	result.Payload = payload
	impact, reason := protocol.ChangeSemantics_ADDITIVE, "Adds a local index to a column table; YDB builds it over the table's data"
	if alteration.Drop != nil {
		impact, reason = protocol.ChangeSemantics_DESTRUCTIVE, "Drops a local index of a column table"
	}
	return &protocol.NormalizedValue{Id: input.GetId(), Value: result, NormalizedOwner: proto.CloneOf(result.Owner),
		CanonicalFormat: definition.CanonicalFormat, CanonicalProjection: bytes.Clone(payload),
		OperationSemantics: &protocol.OperationSemantics{Impact: impact, Reason: reason,
			Access: protocol.ChangeSemantics_ACCESS_UNKNOWN, AccessReason: "Alteration inherits server access policy"}}, nil
}

// ColumnLocalIndexAlteration returns the local-index step an envelope carries
// after validating it.
func ColumnLocalIndexAlteration(value *protocol.Envelope) (*native.ColumnTableLocalIndexAlteration, error) {
	normalized, err := NormalizeColumnLocalIndex(&protocol.ValueInput{Id: "execute", Representation: protocol.ValueInput_OPERATION, Value: value})
	if err != nil {
		return nil, err
	}
	return decodeColumnLocalIndex(normalized.Value.Payload)
}

func decodeColumnLocalIndex(payload []byte) (*native.ColumnTableLocalIndexAlteration, error) {
	canonical, err := modeljson.Canonical(payload)
	if err != nil {
		return nil, err
	}
	var fields any
	if err := json.Unmarshal(canonical, &fields); err != nil {
		return nil, err
	}
	if containsNull(fields) {
		return nil, fmt.Errorf("YDB column table local-index step settings cannot be null")
	}
	var alteration native.ColumnTableLocalIndexAlteration
	if err := protojson.Unmarshal(canonical, &alteration); err != nil {
		return nil, fmt.Errorf("invalid YDB column table local-index step JSON")
	}
	if err := schema.Known(&alteration); err != nil {
		return nil, err
	}
	if (alteration.Add == nil) == (alteration.Drop == nil) {
		return nil, fmt.Errorf("a column table local-index step adds or drops one index")
	}
	if err := validateStepLocalIndex(alteration.Add, alteration.Drop); err != nil {
		return nil, err
	}
	return &alteration, nil
}

// validateStepLocalIndex holds the index a step adds to what its statement
// writes, every setting resolved and no storage, and the index a step drops
// to what a read describes.
func validateStepLocalIndex(add, drop *native.LocalIndexObservation) error {
	if err := schema.ValidateDeclaredLocalIndexes([]*native.LocalIndexObservation{cmp.Or(add, drop)}); err != nil {
		return err
	}
	if add != nil && !proto.Equal(add, ResolvedLocalIndex(add)) {
		return fmt.Errorf("a column table local-index step names every setting of the index it adds and no storage")
	}
	if add != nil && len(add.GetColumns()) != 1 {
		return fmt.Errorf("a YDB local index reads exactly one column")
	}
	return nil
}

// ColumnLocalIndexStatement writes the statement of a valid local-index step
// at an absolute path, as the host's embedded engine writes it: `ALTER TABLE
// `path` ADD INDEX ...;` or `ALTER TABLE `path` DROP INDEX `name`;`.
func ColumnLocalIndexStatement(path string, alteration *native.ColumnTableLocalIndexAlteration) string {
	prefix := "ALTER TABLE " + QuoteIdentifier(path) + " "
	if alteration.Drop != nil {
		return prefix + "DROP INDEX " + QuoteIdentifier(alteration.Drop.GetName()) + ";"
	}
	return prefix + "ADD " + LocalIndexClause(alteration.Add) + ";"
}
