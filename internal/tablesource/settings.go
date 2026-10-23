package tablesource

import (
	"fmt"
	"slices"
	"strconv"
	"strings"

	native "github.com/stokaro/ptah-ydb-plugin/internal/nativeprotocol"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb_Table"
)

// DeclarationError is an attribute whose value a declaration cannot carry.
type DeclarationError struct {
	// Attribute is the attribute's name.
	Attribute string
	// Value is the value it was given.
	Value string
	// Reason says what the attribute takes.
	Reason string
}

func (e *DeclarationError) Error() string {
	return fmt.Sprintf("invalid %s %q: %s", e.Attribute, e.Value, e.Reason)
}

// UnsupportedError is a setting the bundled decoder in the host reads and the
// provider cannot carry yet. It is refused by name, never dropped.
type UnsupportedError struct {
	// Setting names the key or attribute.
	Setting string
	// Reason says what is missing.
	Reason string
}

func (e *UnsupportedError) Error() string {
	return fmt.Sprintf("%s is not supported by the YDB provider yet: %s", e.Setting, e.Reason)
}

// settings collects the declared parts of one table's row-table facet.
type settings struct {
	intent   native.RowTableIntent
	declared bool
}

// empty reports whether nothing was declared, so no facet is due.
func (s *settings) empty() bool { return !s.declared }

// setPartitioning reads one partitioning attribute. A value is checked for the
// form YDB takes, so a typo is refused where it was written: ENABLED or
// DISABLED in either case for a switch, a whole number of at least 1 for a
// count, and PER_AZ:<n> or ANY_AZ:<n> for read replicas.
func (s *settings) setPartitioning(attribute, raw string) error {
	value := strings.TrimSpace(raw)
	partitioning := func() *Ydb_Table.PartitioningSettings {
		if s.intent.PartitioningSettings == nil {
			s.intent.PartitioningSettings = &Ydb_Table.PartitioningSettings{}
		}
		return s.intent.PartitioningSettings
	}
	switch attribute {
	case AttributeBySize, AttributeByLoad, AttributeKeyBloomFilter:
		status, err := parseSwitch(attribute, value)
		if err != nil {
			return err
		}
		switch attribute {
		case AttributeBySize:
			partitioning().PartitioningBySize = status
		case AttributeByLoad:
			partitioning().PartitioningByLoad = status
		default:
			s.intent.KeyBloomFilter = status
		}
	case AttributePartitionSizeMB, AttributeMinPartitions, AttributeMaxPartitions:
		count, err := parseCount(attribute, value)
		if err != nil {
			return err
		}
		switch attribute {
		case AttributePartitionSizeMB:
			partitioning().PartitionSizeMb = count
		case AttributeMinPartitions:
			partitioning().MinPartitionsCount = count
		default:
			partitioning().MaxPartitionsCount = count
		}
	case AttributeReadReplicas:
		replicas, err := parseReplicas(value)
		if err != nil {
			return err
		}
		s.intent.ReadReplicasSettings = replicas
	default:
		return fmt.Errorf("YDB table partitioning reads no attribute %q", attribute)
	}
	s.declared = true
	return nil
}

func parseSwitch(attribute, value string) (Ydb.FeatureFlag_Status, error) {
	switch strings.ToUpper(value) {
	case "ENABLED":
		return Ydb.FeatureFlag_ENABLED, nil
	case "DISABLED":
		return Ydb.FeatureFlag_DISABLED, nil
	default:
		return Ydb.FeatureFlag_STATUS_UNSPECIFIED, &DeclarationError{Attribute: attribute, Value: value, Reason: "write ENABLED or DISABLED"}
	}
}

func parseCount(attribute, value string) (uint64, error) {
	count, err := strconv.ParseUint(value, 10, 64)
	if err != nil || count == 0 {
		return 0, &DeclarationError{Attribute: attribute, Value: value, Reason: "write a whole number of at least 1; YDB refuses a count of zero"}
	}
	return count, nil
}

// parseReplicas reads exactly one of PER_AZ:<n> and ANY_AZ:<n>, in either case.
// A count of zero is a declaration of no replicas, which removes the ones a
// table holds.
func parseReplicas(value string) (*Ydb_Table.ReadReplicasSettings, error) {
	refusal := &DeclarationError{Attribute: AttributeReadReplicas, Value: value,
		Reason: "write PER_AZ:<n> for n replicas in every availability zone, or ANY_AZ:<n> for n in all of them together"}
	mode, count, found := strings.Cut(strings.TrimSpace(value), ":")
	if !found {
		return nil, refusal
	}
	n, err := strconv.ParseUint(strings.TrimSpace(count), 10, 64)
	if err != nil {
		return nil, refusal
	}
	switch strings.ToUpper(strings.TrimSpace(mode)) {
	case "PER_AZ":
		return &Ydb_Table.ReadReplicasSettings{Settings: &Ydb_Table.ReadReplicasSettings_PerAzReadReplicasCount{PerAzReadReplicasCount: n}}, nil
	case "ANY_AZ":
		return &Ydb_Table.ReadReplicasSettings{Settings: &Ydb_Table.ReadReplicasSettings_AnyAzReadReplicasCount{AnyAzReadReplicasCount: n}}, nil
	default:
		return nil, refusal
	}
}

// family is one column family as a source writes it. A nil setting is one the
// source left out: the table keeps what it holds, never a documented default.
type family struct {
	name        string
	data        *string
	compression *string
	cacheMode   *string
	fields      *string
}

// addFamily checks one family and adds it to the table's families. The checks
// follow the order in which the bundled decoder in the host makes them.
func (s *settings) addFamily(declared family) error {
	name := strings.TrimSpace(declared.name)
	if name == "" {
		return &DeclarationError{Attribute: AttributeName, Reason: "a column family needs a name"}
	}
	result := &Ydb_Table.ColumnFamily{Name: name}
	if declared.data != nil {
		data := strings.TrimSpace(*declared.data)
		if data == "" {
			return &DeclarationError{Attribute: AttributeData, Value: *declared.data,
				Reason: "names the kind of storage pool the family is kept in, such as ssd; leave it out to keep the database's own pool"}
		}
		result.Data = &Ydb_Table.StoragePool{Media: data}
	}
	if declared.compression != nil {
		switch strings.ToLower(strings.TrimSpace(*declared.compression)) {
		case "off":
			result.Compression = Ydb_Table.ColumnFamily_COMPRESSION_NONE
		case "lz4":
			result.Compression = Ydb_Table.ColumnFamily_COMPRESSION_LZ4
		case "zstd":
			return &DeclarationError{Attribute: AttributeCompression, Value: *declared.compression,
				Reason: "takes off or lz4: YDB keeps zstd for column-oriented tables, and a row table answers `Unsupported compression value 3`"}
		default:
			return &DeclarationError{Attribute: AttributeCompression, Value: *declared.compression, Reason: "takes off or lz4"}
		}
	}
	if declared.cacheMode != nil {
		return &UnsupportedError{Setting: AttributeCacheMode, Reason: "the row-table model carries no family cache mode"}
	}
	if declared.fields != nil {
		return &UnsupportedError{Setting: AttributeFields, Reason: "a source cannot place a column in a family until a facet can attach to a column"}
	}
	if slices.ContainsFunc(s.intent.ColumnFamilies, func(have *Ydb_Table.ColumnFamily) bool { return have.GetName() == name }) {
		return fmt.Errorf("it declares column family %q twice", name)
	}
	s.intent.ColumnFamilies = append(s.intent.ColumnFamilies, result)
	s.declared = true
	return nil
}
