package schema

import (
	"cmp"
	"fmt"
	"math"
	"slices"

	native "github.com/stokaro/ptah-ydb-plugin/internal/nativeprotocol"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"google.golang.org/protobuf/proto"
)

// ColumnStorageProperties owns hash partitioning, local indexes, and column TTL.
const ColumnStorageProperties = "ydb.column-storage-properties"

// CaptureColumnStorage binds a complete column-storage observation to its table.
// Live inspection still requires both native table and monitoring descriptions.
func CaptureColumnStorage(owner *protocol.Reference, observed *native.ColumnStorageObservation) (*protocol.Envelope, error) {
	if err := Known(observed); err != nil {
		return nil, err
	}
	return capture(ColumnStorageProperties, owner, proto.CloneOf(observed))
}

func normalizeColumnStorage(value *native.ColumnStorageObservation) error {
	if value.Partitions == 0 || value.DataChannelCount == 0 || !distinctColumns(value.HashColumns) {
		return fmt.Errorf("incomplete YDB column storage settings")
	}
	names := map[string]bool{}
	for _, index := range value.LocalIndexes {
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
	slices.SortFunc(value.LocalIndexes, func(a, b *native.LocalIndexObservation) int { return cmp.Compare(a.Name, b.Name) })
	return validateColumnTTL(value.Ttl)
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
