package tablesource

import (
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"

	native "github.com/stokaro/ptah-ydb-plugin/internal/nativeprotocol"
	"github.com/stokaro/ptah-ydb-plugin/internal/ttlgrammar"
	"github.com/stokaro/ptah-ydb-plugin/schema"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb_Table"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/emptypb"
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

// settings collects the declared parts of one table's facet: a row table's
// settings, or a column table's storage.
type settings struct {
	intent   native.RowTableIntent
	declared bool
	// placements are the columns the table's families list, in the order the
	// declarations list them.
	placements []placement
	// column is the storage of a column table, nil for a row table.
	column *native.ColumnStorageIntent
}

// placement puts one column in a declared family.
type placement struct{ column, family string }

// empty reports whether nothing was declared, so no facet is due.
func (s *settings) empty() bool { return !s.declared && s.column == nil }

// setColumnStore makes the table a column table with the storage a source
// declares.
func (s *settings) setColumnStore(storage *native.ColumnStorageIntent) error {
	if s.column != nil {
		return fmt.Errorf("it declares a column table twice")
	}
	s.column = storage
	return nil
}

// finish completes a column table's facet once every fragment of the table is
// read. A column table holds no row-table setting, so one declared beside it is
// refused with the words the host's embedded engine plans it with. A TTL that
// only deletes, written as the row_deletion properties, is the column table's
// TTL setting too: it becomes the facet's one deleting tier, and a tiered TTL
// declared beside it is refused.
func (s *settings) finish() error {
	if s.column == nil {
		return nil
	}
	ttl := s.intent.TtlSettings
	rows := proto.CloneOf(&s.intent)
	rows.TtlSettings = nil
	if !proto.Equal(rows, &native.RowTableIntent{}) || len(s.placements) != 0 {
		return errors.New(schema.ColumnTableRowSettingsRefusal)
	}
	if ttl == nil {
		return nil
	}
	if s.column.Ttl != nil {
		return errors.New(schema.ColumnTableBothTTLRefusal)
	}
	enabled := &native.ColumnTTLEnabled{}
	switch mode := ttl.GetMode().(type) {
	case *Ydb_Table.TtlSettings_DateTypeColumn:
		enabled.Column = mode.DateTypeColumn.GetColumnName()
		enabled.Tiers = []*native.ColumnTTLTier{{ApplyAfterSeconds: uint64(mode.DateTypeColumn.GetExpireAfterSeconds()), Action: &native.ColumnTTLTier_Delete{Delete: &emptypb.Empty{}}}}
	case *Ydb_Table.TtlSettings_ValueSinceUnixEpoch:
		enabled.Column = mode.ValueSinceUnixEpoch.GetColumnName()
		enabled.ColumnUnit = uint64(mode.ValueSinceUnixEpoch.GetColumnUnit())
		enabled.Tiers = []*native.ColumnTTLTier{{ApplyAfterSeconds: uint64(mode.ValueSinceUnixEpoch.GetExpireAfterSeconds()), Action: &native.ColumnTTLTier_Delete{Delete: &emptypb.Empty{}}}}
	default:
		return fmt.Errorf("YDB TTL requires an explicit mode")
	}
	s.column.Ttl = &native.ColumnTTLPolicy{State: &native.ColumnTTLPolicy_Enabled{Enabled: enabled}}
	s.intent.TtlSettings = nil
	return nil
}

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

// setLayout reads one attribute of the starting layout a new table is created
// with: uniform_partitions takes a count of at least 1, and partition_at_keys
// the split points parseSplitPoints reads. Naming both is refused where the
// facet is normalized, since YDB refuses a table with both.
func (s *settings) setLayout(attribute, raw string) error {
	switch attribute {
	case AttributeUniformPartitions:
		count, err := parseCount(attribute, strings.TrimSpace(raw))
		if err != nil {
			return err
		}
		s.intent.UniformPartitions = count
	case AttributePartitionAtKeys:
		points, err := parseSplitPoints(raw)
		if err != nil {
			return &DeclarationError{Attribute: attribute, Value: raw, Reason: err.Error()}
		}
		s.intent.PartitionAtKeys = nil
		for _, point := range points {
			s.intent.PartitionAtKeys = append(s.intent.PartitionAtKeys, &native.SplitPoint{Values: point})
		}
	default:
		return fmt.Errorf("YDB table layout reads no attribute %q", attribute)
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
	var columns []string
	if declared.fields != nil {
		var err error
		if columns, err = parseFields(*declared.fields); err != nil {
			return err
		}
		if name == defaultFamily && len(columns) > 0 {
			return &DeclarationError{Attribute: AttributeFields, Value: *declared.fields,
				Reason: "the default family holds the key and every column no other family names, so it lists none"}
		}
	}
	if slices.ContainsFunc(s.intent.ColumnFamilies, func(have *Ydb_Table.ColumnFamily) bool { return have.GetName() == name }) {
		return fmt.Errorf("it declares column family %q twice", name)
	}
	for _, column := range columns {
		if at := slices.IndexFunc(s.placements, func(have placement) bool { return have.column == column }); at >= 0 {
			return fmt.Errorf("column %q is in two column families, %q and %q", column, s.placements[at].family, name)
		}
	}
	for _, column := range columns {
		s.placements = append(s.placements, placement{column: column, family: name})
	}
	s.intent.ColumnFamilies = append(s.intent.ColumnFamilies, result)
	s.declared = true
	return nil
}

// defaultFamily is the family every row table has.
const defaultFamily = "default"

// parseFields reads the comma-separated list of column names a family holds.
// Each name is trimmed, none may be empty, and none may repeat.
func parseFields(raw string) ([]string, error) {
	var columns []string
	for part := range strings.SplitSeq(raw, ",") {
		column := strings.TrimSpace(part)
		switch {
		case column == "":
			return nil, &DeclarationError{Attribute: AttributeFields, Value: raw, Reason: "takes a comma-separated list of column names, none of them empty"}
		case slices.Contains(columns, column):
			return nil, &DeclarationError{Attribute: AttributeFields, Value: raw, Reason: fmt.Sprintf("names column %q twice", column)}
		}
		columns = append(columns, column)
	}
	return columns, nil
}

// setTTL reads the three platform properties that declare a row table's TTL:
// the column, the interval, and for an integer column the unit it counts since
// the Unix epoch. None of them declares nothing. A blank value is refused
// instead of read as no declaration, and a policy needs its column and its
// interval. The unit decides the mode: without one the column holds a date or
// a time, with one it holds an integer. Whether the column's type fits the mode
// is decided where the table's columns are known.
func (s *settings) setTTL(values map[string]string) error {
	names := ttlAttributes()
	written := false
	for _, name := range names {
		value, found := values[name]
		if !found {
			continue
		}
		written = true
		if strings.TrimSpace(value) == "" {
			return &DeclarationError{Attribute: name, Value: value, Reason: "it is empty; remove it to leave the TTL undeclared"}
		}
	}
	if !written {
		return nil
	}
	column, interval, unit := strings.TrimSpace(values[AttributeTTLColumn]), strings.TrimSpace(values[AttributeTTLInterval]), strings.TrimSpace(values[AttributeTTLUnit])
	switch {
	case column == "":
		return &DeclarationError{Attribute: AttributeTTLColumn, Reason: "a TTL needs the column its interval is measured from"}
	case interval == "":
		return &DeclarationError{Attribute: AttributeTTLInterval, Reason: "a TTL needs the interval after which a row is deleted"}
	case !utf8.ValidString(column) || strings.ContainsRune(column, 0):
		return &DeclarationError{Attribute: AttributeTTLColumn, Value: column, Reason: "the column name is not valid UTF-8 without NUL"}
	}
	canonicalUnit, err := ttlgrammar.Unit(unit)
	if err != nil {
		return &DeclarationError{Attribute: AttributeTTLUnit, Value: unit, Reason: err.Error()}
	}
	seconds, err := ttlgrammar.IntervalSeconds(interval)
	if err != nil {
		return &DeclarationError{Attribute: AttributeTTLInterval, Value: interval, Reason: err.Error()}
	}
	if s.intent.TtlSettings != nil {
		return fmt.Errorf("it declares a TTL twice")
	}
	// ttlgrammar bounds the interval by MaxSeconds, which fits 32 bits.
	expire := uint32(seconds)
	if canonicalUnit == "" {
		s.intent.TtlSettings = &Ydb_Table.TtlSettings{Mode: &Ydb_Table.TtlSettings_DateTypeColumn{
			DateTypeColumn: &Ydb_Table.DateTypeColumnModeSettings{ColumnName: column, ExpireAfterSeconds: expire}}}
	} else {
		s.intent.TtlSettings = &Ydb_Table.TtlSettings{Mode: &Ydb_Table.TtlSettings_ValueSinceUnixEpoch{
			ValueSinceUnixEpoch: &Ydb_Table.ValueSinceUnixEpochModeSettings{ColumnName: column, ColumnUnit: epochUnits[canonicalUnit], ExpireAfterSeconds: expire}}}
	}
	s.declared = true
	return nil
}

// epochUnits maps the unit as YQL spells it to the one the table service takes.
var epochUnits = map[string]Ydb_Table.ValueSinceUnixEpochModeSettings_Unit{
	ttlgrammar.Seconds:      Ydb_Table.ValueSinceUnixEpochModeSettings_UNIT_SECONDS,
	ttlgrammar.Milliseconds: Ydb_Table.ValueSinceUnixEpochModeSettings_UNIT_MILLISECONDS,
	ttlgrammar.Microseconds: Ydb_Table.ValueSinceUnixEpochModeSettings_UNIT_MICROSECONDS,
	ttlgrammar.Nanoseconds:  Ydb_Table.ValueSinceUnixEpochModeSettings_UNIT_NANOSECONDS,
}
