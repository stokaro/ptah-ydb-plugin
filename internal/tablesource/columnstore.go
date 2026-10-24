package tablesource

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"strconv"
	"strings"
	"unicode/utf8"

	native "github.com/stokaro/ptah-ydb-plugin/internal/nativeprotocol"
	"github.com/stokaro/ptah-ydb-plugin/internal/ttlgrammar"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"google.golang.org/protobuf/types/known/emptypb"
)

// The YAML table key that declares a column table, and the keys of its
// mapping: the hash key, the shard count and the tiered TTL.
const (
	KeyColumnStore  = "column_store"
	keyHashColumns  = "hash_columns"
	keyPartitions   = "partitions"
	keyTTL          = "ttl"
	keyTTLColumn    = "column"
	keyTTLUnit      = "unit"
	keyTTLTiers     = "tiers"
	keyTierInterval = "interval"
	keyTierSource   = "external_source"
)

// ttlUnits are the native epoch units, numbered as the column TTL counts them.
var ttlUnits = map[string]uint64{"": 0, ttlgrammar.Seconds: 1, ttlgrammar.Milliseconds: 2, ttlgrammar.Microseconds: 3, ttlgrammar.Nanoseconds: 4}

// parseStore reads the attributes of the table directive that choose a column
// table and describe it, as the host's bundled decoder does
// (internal/ydbcolumn.Parse): the storage has to be chosen explicitly, so a
// hash key, a shard count or a tiered TTL on a table that is not a column
// table is refused rather than read as one. It returns nil for a row table.
func parseStore(values map[string]string) (*native.ColumnStorageIntent, error) {
	if value, present := values[AttributeStore]; present && strings.TrimSpace(value) == "" {
		return nil, &DeclarationError{Attribute: AttributeStore, Value: value, Reason: "write ROW or COLUMN"}
	}
	store := strings.ToUpper(strings.TrimSpace(values[AttributeStore]))
	if store != "COLUMN" {
		if store != "" && store != "ROW" {
			return nil, &DeclarationError{Attribute: AttributeStore, Value: values[AttributeStore], Reason: "write ROW or COLUMN"}
		}
		for _, name := range []string{AttributeHash, AttributeShards, AttributeColumnTTL} {
			if value, present := values[name]; present {
				return nil, &DeclarationError{Attribute: name, Value: value, Reason: "it describes a column table, so write store=COLUMN"}
			}
		}
		return nil, nil
	}
	result := &native.ColumnStorageIntent{}
	if raw, present := values[AttributeHash]; present {
		for name := range strings.SplitSeq(raw, ",") {
			name = strings.TrimSpace(name)
			if name == "" {
				return nil, &DeclarationError{Attribute: AttributeHash, Value: raw, Reason: "it names an empty column; write the key columns separated by commas"}
			}
			result.HashColumns = append(result.HashColumns, name)
		}
	}
	if raw, present := values[AttributeShards]; present {
		count, err := strconv.ParseUint(raw, 10, 32)
		if err != nil || count == 0 {
			return nil, &DeclarationError{Attribute: AttributeShards, Value: raw, Reason: "write a positive 32-bit count"}
		}
		result.Partitions = count
	}
	if raw, present := values[AttributeColumnTTL]; present {
		ttl, err := parseColumnTTL(raw)
		if err != nil {
			return nil, err
		}
		result.Ttl = ttl
	}
	if err := checkHashColumns(result.HashColumns); err != nil {
		return nil, err
	}
	return result, nil
}

// columnTTLText is the JSON object the column_ttl attribute holds.
type columnTTLText struct {
	Column string `json:"column"`
	Unit   string `json:"unit"`
	Tiers  []struct {
		Interval       string `json:"interval"`
		ExternalSource string `json:"external_source"`
	} `json:"tiers"`
}

// parseColumnTTL reads exactly one JSON object, refusing a field it does not
// name, as the bundled decoder does.
func parseColumnTTL(raw string) (*native.ColumnTTLPolicy, error) {
	var written *columnTTLText
	decoder := json.NewDecoder(strings.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&written); err != nil {
		return nil, &DeclarationError{Attribute: AttributeColumnTTL, Value: raw, Reason: "write one JSON object with a column, an optional unit and tiers: " + err.Error()}
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return nil, &DeclarationError{Attribute: AttributeColumnTTL, Value: raw, Reason: "it must hold exactly one JSON object"}
	}
	if written == nil {
		return nil, &DeclarationError{Attribute: AttributeColumnTTL, Value: raw, Reason: "it must be an object, not null"}
	}
	var tiers []tierText
	for _, tier := range written.Tiers {
		tiers = append(tiers, tierText{interval: tier.Interval, source: tier.ExternalSource})
	}
	return tieredTTL(written.Column, written.Unit, tiers)
}

// tierText is one tier as a source writes it.
type tierText struct{ interval, source string }

// tieredTTL checks a column table's tiered TTL with the bundled decoder's
// rules (ydbschema.CheckColumnStore) and returns it with each interval in
// seconds, as the server keeps it: a column and at least one tier, a unit YQL
// writes, intervals that grow strictly, deletion only in the last tier,
// external data sources named by absolute paths, and at least one tier that
// moves rows. A TTL that only deletes is the table's TTL, written with the
// row_deletion properties.
func tieredTTL(column, unit string, tiers []tierText) (*native.ColumnTTLPolicy, error) {
	if strings.TrimSpace(column) == "" || len(tiers) == 0 {
		return nil, errors.New("tiered TTL requires a column and at least one tier")
	}
	if !utf8.ValidString(column) || strings.ContainsRune(column, 0) {
		return nil, errors.New("the TTL column is not valid UTF-8 without NUL")
	}
	canonical, err := ttlgrammar.Unit(unit)
	if err != nil {
		return nil, err
	}
	enabled := &native.ColumnTTLEnabled{Column: column, ColumnUnit: ttlUnits[canonical]}
	var previous uint64
	moves := false
	for position, tier := range tiers {
		seconds, err := ttlgrammar.IntervalSeconds(tier.interval)
		if err != nil {
			return nil, fmt.Errorf("TTL tier %d: %w", position+1, err)
		}
		if position > 0 && seconds <= previous {
			return nil, errors.New("TTL tier intervals must increase strictly")
		}
		if tier.source == "" && position != len(tiers)-1 {
			return nil, errors.New("only the last TTL tier may delete data")
		}
		if !utf8.ValidString(tier.source) || strings.ContainsRune(tier.source, 0) {
			return nil, errors.New("the TTL tier external source is not valid UTF-8 without NUL")
		}
		if tier.source != "" && !strings.HasPrefix(tier.source, "/") {
			return nil, errors.New("TTL tier external source must be an absolute database path")
		}
		step := &native.ColumnTTLTier{ApplyAfterSeconds: seconds, Action: &native.ColumnTTLTier_Delete{Delete: &emptypb.Empty{}}}
		if tier.source != "" {
			step.Action = &native.ColumnTTLTier_ExternalStorage{ExternalStorage: tier.source}
			moves = true
		}
		enabled.Tiers = append(enabled.Tiers, step)
		previous = seconds
	}
	if !moves {
		return nil, errors.New("a tiered TTL requires a tier that moves rows to an external data source; a TTL that only deletes is the table's TTL")
	}
	return &native.ColumnTTLPolicy{State: &native.ColumnTTLPolicy_Enabled{Enabled: enabled}}, nil
}

// checkHashColumns refuses a hash column that is blank or named twice.
func checkHashColumns(names []string) error {
	seen := map[string]bool{}
	for _, name := range names {
		if !utf8.ValidString(name) || strings.ContainsRune(name, 0) || strings.TrimSpace(name) == "" || seen[name] {
			return fmt.Errorf("hash partitioning column %q is empty or repeated", name)
		}
		seen[name] = true
	}
	return nil
}

// applyColumnStore reads the column_store mapping of a YAML table: a hash key
// as a list of column names, a shard count and a tiered TTL. A null declares a
// row table, an empty mapping a column table that leaves everything to the
// server. The keys are the bundled decoder's, and so are the value types: the
// shard count is a number, and the hash key a list.
func applyColumnStore(target *settings, value *protocol.SourceValue) error {
	if _, null := value.GetValue().(*protocol.SourceValue_Null); null {
		return nil
	}
	mapping := value.GetMapping()
	if mapping == nil {
		return fmt.Errorf("%s: expected a mapping of %s, %s and %s", KeyColumnStore, keyHashColumns, keyPartitions, keyTTL)
	}
	result := &native.ColumnStorageIntent{}
	for _, entry := range mapping.GetEntries() {
		var err error
		switch entry.GetKey() {
		case keyHashColumns:
			result.HashColumns, err = yamlNames(entry.GetValue())
		case keyPartitions:
			result.Partitions, err = yamlShards(entry.GetValue())
		case keyTTL:
			result.Ttl, err = yamlTTL(entry.GetValue())
		default:
			err = errors.New("unknown key")
		}
		if err != nil {
			return fmt.Errorf("%s: %s: %w", KeyColumnStore, entry.GetKey(), err)
		}
	}
	if err := checkHashColumns(result.HashColumns); err != nil {
		return err
	}
	return target.setColumnStore(result)
}

// yamlNames reads a list of column names. A null is no list.
func yamlNames(value *protocol.SourceValue) ([]string, error) {
	if _, null := value.GetValue().(*protocol.SourceValue_Null); null {
		return nil, nil
	}
	sequence := value.GetSequence()
	if sequence == nil {
		return nil, errors.New("expected a list of column names")
	}
	var names []string
	for _, item := range sequence.GetValues() {
		name, err := yamlText(item)
		if err != nil {
			return nil, err
		}
		names = append(names, name)
	}
	return names, nil
}

// yamlShards reads the shard count: a whole number that fits 32 bits, where 0
// and null leave the count to the server. The bundled decoder reads the
// number into an unsigned integer and drops a fraction in silence (2.5 is two
// shards); the provider refuses it instead.
func yamlShards(value *protocol.SourceValue) (uint64, error) {
	switch content := value.GetValue().(type) {
	case *protocol.SourceValue_Null:
		return 0, nil
	case *protocol.SourceValue_Number:
		count, err := strconv.ParseUint(content.Number, 10, 64)
		if err != nil {
			return 0, errors.New("expected a whole number of shards")
		}
		if count > math.MaxUint32 {
			return 0, errors.New("column shard count exceeds a 32-bit integer")
		}
		return count, nil
	default:
		return 0, errors.New("expected a number of shards")
	}
}

// yamlTTL reads the ttl mapping: a column, an optional unit and the tiers,
// each with an interval and an optional external data source.
func yamlTTL(value *protocol.SourceValue) (*native.ColumnTTLPolicy, error) {
	if _, null := value.GetValue().(*protocol.SourceValue_Null); null {
		return nil, nil
	}
	mapping := value.GetMapping()
	if mapping == nil {
		return nil, fmt.Errorf("expected a mapping of %s, %s and %s", keyTTLColumn, keyTTLUnit, keyTTLTiers)
	}
	var column, unit string
	var tiers []tierText
	for _, entry := range mapping.GetEntries() {
		var err error
		switch entry.GetKey() {
		case keyTTLColumn:
			column, err = yamlText(entry.GetValue())
		case keyTTLUnit:
			unit, err = yamlText(entry.GetValue())
		case keyTTLTiers:
			tiers, err = yamlTiers(entry.GetValue())
		default:
			err = errors.New("unknown key")
		}
		if err != nil {
			return nil, fmt.Errorf("%s: %w", entry.GetKey(), err)
		}
	}
	return tieredTTL(column, unit, tiers)
}

func yamlTiers(value *protocol.SourceValue) ([]tierText, error) {
	if _, null := value.GetValue().(*protocol.SourceValue_Null); null {
		return nil, nil
	}
	sequence := value.GetSequence()
	if sequence == nil {
		return nil, errors.New("expected a list of tiers")
	}
	var tiers []tierText
	for position, item := range sequence.GetValues() {
		mapping := item.GetMapping()
		if mapping == nil {
			return nil, fmt.Errorf("tier %d: expected a mapping of %s and %s", position+1, keyTierInterval, keyTierSource)
		}
		var tier tierText
		for _, entry := range mapping.GetEntries() {
			var err error
			switch entry.GetKey() {
			case keyTierInterval:
				tier.interval, err = yamlText(entry.GetValue())
			case keyTierSource:
				tier.source, err = yamlText(entry.GetValue())
			default:
				err = errors.New("unknown key")
			}
			if err != nil {
				return nil, fmt.Errorf("tier %d: %s: %w", position+1, entry.GetKey(), err)
			}
		}
		tiers = append(tiers, tier)
	}
	return tiers, nil
}

// yamlText reads a scalar as the text the author wrote; a null is empty text,
// as the bundled decoder reads it into a string.
func yamlText(value *protocol.SourceValue) (string, error) {
	text, err := scalar(value)
	if errors.Is(err, errNull) {
		return "", nil
	}
	return text, err
}
