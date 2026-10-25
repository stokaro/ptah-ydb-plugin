package ydbdecl

import (
	"fmt"
	"strconv"
	"strings"
)

// ColumnStore is the storage of a YDB column table: its hash partitioning
// columns, its column shard count and its tiered TTL.
type ColumnStore struct {
	HashColumns []string   `json:"hash_columns,omitempty"`
	Partitions  uint64     `json:"partitions,omitempty"`
	TTL         *TieredTTL `json:"ttl,omitempty"`
}

// TieredTTL is a column table's TTL: the column it reads, the unit an
// integer column counts in, and its tiers in order.
type TieredTTL struct {
	Column string    `json:"column"`
	Unit   string    `json:"unit,omitempty"`
	Tiers  []TTLTier `json:"tiers"`
}

// TTLTier is one tier of a tiered TTL: after the interval rows move to the
// external data source, or are deleted when it names none.
type TTLTier struct {
	Interval       string `json:"interval"`
	ExternalSource string `json:"external_source,omitempty"`
}

// The attributes a column table's declaration reads.
const (
	AttributeStore  = "store"
	AttributeHash   = "partition_by_hash"
	AttributeShards = "column_shards"
	AttributeTTL    = "column_ttl"
)

// ParseColumnStore reads a column table's declaration, nil for a row table.
// Storage must be selected explicitly.
func ParseColumnStore(values map[string]string) (*ColumnStore, error) {
	if value, present := values[AttributeStore]; present && strings.TrimSpace(value) == "" {
		return nil, fmt.Errorf("store must be ROW or COLUMN")
	}
	store := strings.ToUpper(strings.TrimSpace(values[AttributeStore]))
	if store != "COLUMN" {
		if store != "" && store != "ROW" {
			return nil, fmt.Errorf("store %q must be ROW or COLUMN", values[AttributeStore])
		}
		for _, name := range []string{AttributeHash, AttributeShards, AttributeTTL} {
			if _, present := values[name]; present {
				return nil, fmt.Errorf("%s requires store=column", name)
			}
		}
		return nil, nil
	}
	spec := &ColumnStore{}
	if raw, present := values[AttributeHash]; present {
		for name := range strings.SplitSeq(raw, ",") {
			name = strings.TrimSpace(name)
			if name == "" {
				return nil, fmt.Errorf("%s contains an empty column name", AttributeHash)
			}
			spec.HashColumns = append(spec.HashColumns, name)
		}
	}
	if raw, present := values[AttributeShards]; present {
		count, err := strconv.ParseUint(raw, 10, 32)
		if err != nil || count == 0 {
			return nil, fmt.Errorf("%s must be a positive 32-bit integer", AttributeShards)
		}
		spec.Partitions = count
	}
	if _, present := values[AttributeTTL]; present {
		return nil, fmt.Errorf("%s is not read from a YQL document", AttributeTTL)
	}
	if err := CheckColumnStore(*spec); err != nil {
		return nil, err
	}
	return spec, nil
}

// CheckColumnStore refuses storage YDB cannot hold.
func CheckColumnStore(s ColumnStore) error {
	seen := make(map[string]bool, len(s.HashColumns))
	for _, name := range s.HashColumns {
		if err := validText("hash partitioning column", name); err != nil {
			return err
		}
		if strings.TrimSpace(name) == "" || seen[name] {
			return fmt.Errorf("hash partitioning column %q is empty or repeated", name)
		}
		seen[name] = true
	}
	if s.Partitions > 1<<32-1 {
		return fmt.Errorf("column shard count exceeds a 32-bit integer")
	}
	return validateTieredTTL(s.TTL)
}

func validateTieredTTL(t *TieredTTL) error {
	if t == nil {
		return nil
	}
	if strings.TrimSpace(t.Column) == "" || len(t.Tiers) == 0 {
		return fmt.Errorf("tiered TTL requires a column and at least one tier")
	}
	if err := validText("TTL column", t.Column); err != nil {
		return err
	}
	unit, err := TTLUnit(t.Unit)
	if err != nil {
		return err
	}
	if unit != t.Unit {
		return fmt.Errorf("the TTL unit %q is written %q", t.Unit, unit)
	}
	var previous uint64
	moves := false
	for position, tier := range t.Tiers {
		seconds, err := IntervalSeconds(tier.Interval)
		if err != nil {
			return fmt.Errorf("TTL tier %d: %w", position+1, err)
		}
		if position > 0 && seconds <= previous {
			return fmt.Errorf("TTL tier intervals must increase strictly")
		}
		if tier.ExternalSource == "" && position != len(t.Tiers)-1 {
			return fmt.Errorf("only the last TTL tier may delete data")
		}
		if err := validText("TTL tier external source", tier.ExternalSource); err != nil {
			return err
		}
		if tier.ExternalSource != "" && !strings.HasPrefix(tier.ExternalSource, "/") {
			return fmt.Errorf("TTL tier external source must be an absolute database path")
		}
		moves = moves || tier.ExternalSource != ""
		previous = seconds
	}
	if !moves {
		return fmt.Errorf("a tiered TTL requires a tier that moves rows to an external data source; a TTL that only deletes is the table's TTL")
	}
	return nil
}
