package schema

import (
	"cmp"
	"fmt"
	"math"
	"slices"
	"strings"
	"unicode/utf8"

	native "github.com/stokaro/ptah-ydb-plugin/internal/nativeprotocol"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"google.golang.org/protobuf/proto"
)

// The host's words for a column table that states a row table's settings, and
// for one that states a tiered TTL and a TTL that deletes rows. The host's
// bundled decoders accept both and its renderer refuses the table. The source
// decoders refuse the declaration where it is decoded, and the creation
// compiler refuses a captured table again, with the same words.
const (
	ColumnTableRowSettingsRefusal = "row-table partitioning, column families and changefeeds cannot be applied to a column table"
	ColumnTableBothTTLRefusal     = "declare either tiered TTL or a TTL that deletes rows, not both"
)

// ColumnStorageProperties owns hash partitioning, local indexes, and column TTL.
const ColumnStorageProperties = modelNamespace + "column-storage-properties"

// CaptureColumnStorage binds a complete column-storage observation to its table.
// Live inspection still requires both native table and monitoring descriptions.
func CaptureColumnStorage(owner *protocol.Reference, observed *native.ColumnStorageObservation) (*protocol.Envelope, error) {
	if err := Known(observed); err != nil {
		return nil, err
	}
	return capture(ColumnStorageProperties, owner, proto.CloneOf(observed))
}

// normalizeColumnStorage checks an observation. A live read always reports the
// hash key, the shard count and the data channel count (columnstore.Decode
// refuses a page without them); an observation projected from a declaration
// that leaves them to the server has none.
func normalizeColumnStorage(value *native.ColumnStorageObservation) error {
	if len(value.HashColumns) != 0 && !distinctColumns(value.HashColumns) {
		return fmt.Errorf("YDB hash partitioning columns must be distinct and nonempty")
	}
	if value.Partitions > math.MaxUint32 {
		return fmt.Errorf("column shard count exceeds a 32-bit integer")
	}
	if err := validateLocalIndexes(value.LocalIndexes); err != nil {
		return err
	}
	slices.SortFunc(value.LocalIndexes, func(a, b *native.LocalIndexObservation) int { return cmp.Compare(a.Name, b.Name) })
	return validateColumnTTL(value.Ttl)
}

// validateColumnStorageIntent checks a declaration of column storage. A
// declaration may leave the hash key (the server hashes by the primary key),
// the shard count and the data channel count to the server, and states the
// whole TTL of the table: none, one tier that deletes, or tiers that move rows
// to external data sources before the last may delete.
func validateColumnStorageIntent(value *native.ColumnStorageIntent) error {
	if len(value.HashColumns) != 0 && !distinctColumns(value.HashColumns) {
		return fmt.Errorf("YDB hash partitioning columns must be distinct and nonempty")
	}
	if value.Partitions > math.MaxUint32 {
		return fmt.Errorf("column shard count exceeds a 32-bit integer")
	}
	if err := validateLocalIndexes(value.LocalIndexes); err != nil {
		return err
	}
	return validateDesiredColumnTTL(value.Ttl)
}

// validateDesiredColumnTTL holds a declared TTL to what YDB keeps: a column,
// a unit for an epoch column, and tiers whose intervals grow strictly, of
// which only the last may delete and every other moves rows to an external
// data source named by its absolute path. The expiration the server derives
// from the tiers is not a declaration.
func validateDesiredColumnTTL(policy *native.ColumnTTLPolicy) error {
	enabled := policy.GetEnabled()
	if enabled == nil {
		return validateColumnTTL(policy)
	}
	if enabled.ExpireAfterSeconds != nil {
		return fmt.Errorf("a declared YDB column TTL states tiers; the server derives its expiration from them")
	}
	if !utf8.ValidString(enabled.Column) || strings.ContainsRune(enabled.Column, 0) || strings.TrimSpace(enabled.Column) == "" ||
		enabled.ColumnUnit > 4 || len(enabled.Tiers) == 0 {
		return fmt.Errorf("tiered TTL requires a column and at least one tier")
	}
	for position, tier := range enabled.Tiers {
		if tier.GetApplyAfterSeconds() > math.MaxUint32 {
			return fmt.Errorf("TTL tier %d: the interval exceeds what YDB keeps", position+1)
		}
		if position > 0 && tier.GetApplyAfterSeconds() <= enabled.Tiers[position-1].GetApplyAfterSeconds() {
			return fmt.Errorf("TTL tier intervals must increase strictly")
		}
		switch action := tier.GetAction().(type) {
		case *native.ColumnTTLTier_Delete:
			if position != len(enabled.Tiers)-1 {
				return fmt.Errorf("only the last TTL tier may delete data")
			}
		case *native.ColumnTTLTier_ExternalStorage:
			if !utf8.ValidString(action.ExternalStorage) || strings.ContainsRune(action.ExternalStorage, 0) || !strings.HasPrefix(action.ExternalStorage, "/") {
				return fmt.Errorf("TTL tier external source must be an absolute database path")
			}
		default:
			return fmt.Errorf("missing YDB TTL action")
		}
	}
	return nil
}

// validateLocalIndexes checks the local indexes of a column table, in either
// representation.
func validateLocalIndexes(indexes []*native.LocalIndexObservation) error {
	names := map[string]bool{}
	for _, index := range indexes {
		if !componentName(index.GetName()) || names[index.Name] || !distinctColumns(index.Columns) {
			return fmt.Errorf("invalid or duplicate YDB local index")
		}
		names[index.Name] = true
		var extractor, bits string
		var probability *float64
		switch kind := index.Kind.(type) {
		case *native.LocalIndexObservation_Bloom:
			extractor, bits, probability = kind.Bloom.DataExtractor, kind.Bloom.BitsStorage, kind.Bloom.FalsePositiveProbability
		case *native.LocalIndexObservation_Ngram:
			if len(index.Columns) != 1 || (kind.Ngram.NgramSize != nil && kind.Ngram.GetNgramSize() == 0) {
				return fmt.Errorf("invalid YDB ngram index")
			}
			extractor, bits, probability = kind.Ngram.DataExtractor, kind.Ngram.BitsStorage, kind.Ngram.FalsePositiveProbability
		case *native.LocalIndexObservation_MinMax:
			if len(index.Columns) != 1 {
				return fmt.Errorf("invalid YDB min/max index")
			}
			extractor = kind.MinMax.DataExtractor
		default:
			return fmt.Errorf("missing YDB local index kind")
		}
		if (extractor != "" && extractor != "DEFAULT") || (bits != "" && bits != "SIMPLE_STRING") {
			return fmt.Errorf("unsupported YDB local index encoding")
		}
		if probability != nil && (math.IsNaN(*probability) || math.IsInf(*probability, 0) || *probability <= 0 || *probability >= 1) {
			return fmt.Errorf("invalid YDB local index probability")
		}
		if index.GetMinMax() != nil {
			if index.StorageId != "__LOCAL_METADATA" || index.InheritPortionStorage {
				return fmt.Errorf("unsupported YDB min/max storage")
			}
		} else if index.StorageId != "__DEFAULT" && (index.StorageId != "" || !index.InheritPortionStorage) {
			return fmt.Errorf("unsupported YDB bloom storage")
		}
	}
	return nil
}

func distinctColumns(names []string) bool {
	if len(names) == 0 {
		return false
	}
	seen := map[string]bool{}
	for _, name := range names {
		if name == "" || seen[name] {
			return false
		}
		seen[name] = true
	}
	return true
}

func validateColumnTTL(policy *native.ColumnTTLPolicy) error {
	if policy == nil {
		return nil
	}
	switch state := policy.State.(type) {
	case *native.ColumnTTLPolicy_Disabled:
		return nil
	case *native.ColumnTTLPolicy_Enabled:
		value := state.Enabled
		if value.Column == "" || value.ColumnUnit > 4 || (value.ExpireAfterSeconds == nil && len(value.Tiers) == 0) {
			return fmt.Errorf("incomplete YDB column TTL")
		}
		for _, tier := range value.Tiers {
			switch action := tier.Action.(type) {
			case *native.ColumnTTLTier_Delete:
			case *native.ColumnTTLTier_ExternalStorage:
				if action.ExternalStorage == "" {
					return fmt.Errorf("empty YDB TTL external storage")
				}
			default:
				return fmt.Errorf("missing YDB TTL action")
			}
		}
	default:
		return fmt.Errorf("missing YDB column TTL state")
	}
	return nil
}
