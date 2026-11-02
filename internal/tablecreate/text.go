package tablecreate

import (
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/stokaro/ptah-ydb-plugin/internal/columnsemantics"
	"github.com/stokaro/ptah-ydb-plugin/internal/ydbcomment"
	"github.com/stokaro/ptah-ydb-plugin/internal/ydbtype"
	"github.com/stokaro/ptah-ydb-plugin/schema"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb_Table"
)

// ErrNoTextForm is wrapped by the error of an operation that has no YQL
// spelling: one only its native call performs. The message names the setting
// and never a value, so a refusal that carries it can be shown as written.
var ErrNoTextForm = errors.New("no YQL text form")

func noTextForm(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrNoTextForm, fmt.Sprintf(format, args...))
}

// defaultRunIntervalSeconds is the run interval YDB gives a TTL that names
// none, and the one YQL's TTL setting leaves a table with.
const defaultRunIntervalSeconds = 3600

// TextPath is the path a text unit names a table by: relative to the
// database, so that the statement runs in whatever database the migration is
// applied to, as the host's embedded engine writes it.
func TextPath(schemaPath, name string) string {
	if strings.TrimSpace(schemaPath) == "" {
		return name
	}
	return strings.TrimRight(schemaPath, "/") + "/" + name
}

// RowTableStatement writes the CREATE TABLE a valid native creation request
// makes at path, in the form the host's embedded engine writes:
//
//	CREATE TABLE `path` (
//	    `c` Type [FAMILY `f`] [NOT NULL] [DEFAULT literal],
//	    PRIMARY KEY (`k`),
//	    INDEX `i` GLOBAL SYNC ON (`c`) COVER (`d`),
//	    FAMILY `f` (DATA = 'ssd', COMPRESSION = 'lz4')
//	) WITH (TTL = ..., AUTO_PARTITIONING_..., KEY_BLOOM_FILTER = ..., UNIFORM_PARTITIONS = n);
//
// The comments Ptah keeps among the attributes, the new table's own and its
// columns' and indexes', follow as Ptah's COMMENT ON statements, as the
// embedded engine writes them after the CREATE TABLE.
//
// A request that sets something YQL has no spelling for is refused with
// [ErrNoTextForm]: storage settings, user attributes other than comments,
// tiering, a TTL run interval other than YDB's own, the partition columns, a
// family kept in memory, and a default YQL cannot write.
func RowTableStatement(path string, request *Ydb_Table.CreateTableRequest) (string, error) {
	comments, err := attributeComments(path, request)
	switch {
	case err != nil:
		return "", err
	case request.GetStorageSettings() != nil:
		return "", noTextForm("YQL has no setting for a row table's storage settings")
	case request.GetTiering() != "":
		return "", noTextForm("YQL has no setting for a row table's tiering")
	case len(request.GetPartitioningSettings().GetPartitionBy()) != 0:
		return "", noTextForm("YQL has no setting for a row table's partition columns")
	}
	lines := make([]string, 0, len(request.GetColumns())+len(request.GetColumnFamilies())+1)
	for _, column := range request.GetColumns() {
		line, err := columnDefinition(column)
		if err != nil {
			return "", err
		}
		lines = append(lines, line)
	}
	lines = append(lines, "PRIMARY KEY ("+quoteList(request.GetPrimaryKey())+")")
	for _, index := range request.GetIndexes() {
		clause, err := indexClause(index)
		if err != nil {
			return "", err
		}
		lines = append(lines, clause)
	}
	entries, err := familyEntries(request.GetColumnFamilies())
	if err != nil {
		return "", err
	}
	lines = append(lines, entries...)
	settings, err := creationSettings(request)
	if err != nil {
		return "", err
	}
	statement := "CREATE TABLE " + QuoteIdentifier(path) + " (\n    " + strings.Join(lines, ",\n    ") + "\n)"
	if len(settings) != 0 {
		statement += " WITH (" + strings.Join(settings, ", ") + ")"
	}
	return strings.Join(append([]string{statement + ";"}, comments...), "\n"), nil
}

// attributeComments writes the comments a creation's attributes hold as
// Ptah's COMMENT ON statements: the table's own, then its columns' and its
// indexes' in the order the request holds them. An attribute that is not a
// comment of the table has no text form.
func attributeComments(path string, request *Ydb_Table.CreateTableRequest) ([]string, error) {
	held := ydbcomment.Read(request.GetAttributes())
	for key := range request.GetAttributes() {
		if !ydbcomment.IsKey(key) {
			return nil, noTextForm("YQL's CREATE TABLE sets no user attribute")
		}
	}
	var statements []ydbcomment.Statement
	if held.Own != "" {
		statements = append(statements, ydbcomment.Statement{Object: ydbcomment.Table, Path: path, Comment: held.Own})
	}
	for _, column := range request.GetColumns() {
		if comment := held.Columns[column.GetName()]; comment != "" {
			statements = append(statements, ydbcomment.Statement{Object: ydbcomment.Column, Path: path, Name: column.GetName(), Comment: comment})
		}
	}
	for _, index := range request.GetIndexes() {
		if comment := held.Indexes[index.GetName()]; comment != "" {
			statements = append(statements, ydbcomment.Statement{Object: ydbcomment.Index, Path: path, Name: index.GetName(), Comment: comment})
		}
	}
	if len(statements) != len(held.Attributes()) {
		return nil, noTextForm("the table's attributes hold a comment of a column or an index it does not have")
	}
	texts := make([]string, 0, len(statements))
	for _, statement := range statements {
		text, err := statement.Text()
		if err != nil {
			return nil, noTextForm("%v", err)
		}
		texts = append(texts, text)
	}
	return texts, nil
}

// columnDefinition writes one column: its name, its type, the family it sits
// in when it is not the default one, NOT NULL, and its default.
func columnDefinition(column *Ydb_Table.ColumnMeta) (string, error) {
	spelling, _, err := schema.ColumnType(column.GetType())
	if err != nil {
		return "", noTextForm("column %q has a type YQL cannot spell", column.GetName())
	}
	line := QuoteIdentifier(column.GetName()) + " " + spelling
	if family := column.GetFamily(); family != "" && family != DefaultFamily {
		line += " FAMILY " + QuoteIdentifier(family)
	}
	if column.GetNotNull() {
		line += " NOT NULL"
	}
	switch value := column.GetDefaultValue().(type) {
	case nil:
	case *Ydb_Table.ColumnMeta_FromLiteral:
		literal, err := columnsemantics.Literal(value.FromLiteral)
		if err != nil {
			return "", noTextForm("the default of column %q has no YQL literal", column.GetName())
		}
		line += " DEFAULT " + literal
	default:
		return "", noTextForm("column %q takes its default from a sequence, which YQL's CREATE TABLE does not name", column.GetName())
	}
	return line, nil
}

// familyEntries writes the `FAMILY <name> (...)` entries of a CREATE TABLE,
// sorted by name, each with the settings it states. The default family is
// written only where it states a setting.
func familyEntries(families []*Ydb_Table.ColumnFamily) ([]string, error) {
	sorted := slices.Clone(families)
	slices.SortFunc(sorted, func(a, b *Ydb_Table.ColumnFamily) int {
		return strings.Compare(familyKey(a.GetName()), familyKey(b.GetName()))
	})
	entries := make([]string, 0, len(sorted))
	for _, family := range sorted {
		settings, err := familySettings(family)
		if err != nil {
			return nil, err
		}
		if familyKey(family.GetName()) == DefaultFamily && len(settings) == 0 {
			continue
		}
		entries = append(entries, "FAMILY "+QuoteIdentifier(familyKey(family.GetName()))+" ("+strings.Join(settings, ", ")+")")
	}
	return entries, nil
}

// familySettings are the `SETTING = value` items a family states.
func familySettings(family *Ydb_Table.ColumnFamily) ([]string, error) {
	if family.GetKeepInMemory() == Ydb.FeatureFlag_ENABLED {
		return nil, noTextForm("column family %q keeps its columns in memory, and YQL has no family setting for it", familyKey(family.GetName()))
	}
	var items []string
	if media := family.GetData().GetMedia(); media != "" {
		items = append(items, "DATA = "+ydbtype.StringLiteral(media))
	}
	if compression := compressionText(family.GetCompression()); compression != "" {
		items = append(items, "COMPRESSION = "+ydbtype.StringLiteral(compression))
	}
	return items, nil
}

// compressionText spells a family's compression as YQL takes it, or "" for
// none stated.
func compressionText(compression Ydb_Table.ColumnFamily_Compression) string {
	switch compression {
	case Ydb_Table.ColumnFamily_COMPRESSION_NONE:
		return "off"
	case Ydb_Table.ColumnFamily_COMPRESSION_LZ4:
		return "lz4"
	default:
		return ""
	}
}

// creationSettings are the settings a CREATE TABLE's WITH clause names, in
// the order the host's embedded engine writes them: the TTL, the automatic
// partitioning, the read replicas, the key bloom filter and the starting
// layout.
func creationSettings(request *Ydb_Table.CreateTableRequest) ([]string, error) {
	var settings []string
	if ttl := request.GetTtlSettings(); ttl != nil {
		setting, err := ttlSetting(ttl)
		if err != nil {
			return nil, err
		}
		settings = append(settings, "TTL = "+setting)
	}
	partitioning := request.GetPartitioningSettings()
	if flag := partitioning.GetPartitioningBySize(); flag != Ydb.FeatureFlag_STATUS_UNSPECIFIED {
		settings = append(settings, "AUTO_PARTITIONING_BY_SIZE = "+flagText(flag))
	}
	if size := partitioning.GetPartitionSizeMb(); size != 0 {
		settings = append(settings, "AUTO_PARTITIONING_PARTITION_SIZE_MB = "+strconv.FormatUint(size, 10))
	}
	if flag := partitioning.GetPartitioningByLoad(); flag != Ydb.FeatureFlag_STATUS_UNSPECIFIED {
		settings = append(settings, "AUTO_PARTITIONING_BY_LOAD = "+flagText(flag))
	}
	if count := partitioning.GetMinPartitionsCount(); count != 0 {
		settings = append(settings, "AUTO_PARTITIONING_MIN_PARTITIONS_COUNT = "+strconv.FormatUint(count, 10))
	}
	if count := partitioning.GetMaxPartitionsCount(); count != 0 {
		settings = append(settings, "AUTO_PARTITIONING_MAX_PARTITIONS_COUNT = "+strconv.FormatUint(count, 10))
	}
	if replicas := replicasText(request.GetReadReplicasSettings()); replicas != "" {
		settings = append(settings, `READ_REPLICAS_SETTINGS = "`+replicas+`"`)
	}
	if flag := request.GetKeyBloomFilter(); flag != Ydb.FeatureFlag_STATUS_UNSPECIFIED {
		settings = append(settings, "KEY_BLOOM_FILTER = "+flagText(flag))
	}
	switch partitions := request.GetPartitions().(type) {
	case nil:
	case *Ydb_Table.CreateTableRequest_UniformPartitions:
		settings = append(settings, "UNIFORM_PARTITIONS = "+strconv.FormatUint(partitions.UniformPartitions, 10))
	case *Ydb_Table.CreateTableRequest_PartitionAtKeys:
		points, err := splitPoints(partitions.PartitionAtKeys)
		if err != nil {
			return nil, err
		}
		settings = append(settings, "PARTITION_AT_KEYS = ("+points+")")
	}
	return settings, nil
}

// ttlSetting writes a row table's TTL as the value of YQL's TTL setting:
// `Interval("PT1H") ON `c“, with the unit an integer column counts in.
func ttlSetting(ttl *Ydb_Table.TtlSettings) (string, error) {
	if run := ttl.GetRunIntervalSeconds(); run != 0 && run != defaultRunIntervalSeconds {
		return "", noTextForm("the TTL runs on an interval of its own, and YQL's TTL setting has none")
	}
	switch mode := ttl.GetMode().(type) {
	case *Ydb_Table.TtlSettings_DateTypeColumn:
		return `Interval("` + intervalText(uint64(mode.DateTypeColumn.GetExpireAfterSeconds())) + `") ON ` +
			QuoteIdentifier(mode.DateTypeColumn.GetColumnName()), nil
	case *Ydb_Table.TtlSettings_ValueSinceUnixEpoch:
		unit, found := epochUnits[mode.ValueSinceUnixEpoch.GetColumnUnit()]
		if !found {
			return "", noTextForm("the TTL counts in a unit YQL does not name")
		}
		return `Interval("` + intervalText(uint64(mode.ValueSinceUnixEpoch.GetExpireAfterSeconds())) + `") ON ` +
			QuoteIdentifier(mode.ValueSinceUnixEpoch.GetColumnName()) + " AS " + unit, nil
	default:
		return "", noTextForm("the TTL reads its column in a mode YQL does not name")
	}
}

// epochUnits spell the units an integer TTL column counts in, as YQL writes
// them after AS.
var epochUnits = map[Ydb_Table.ValueSinceUnixEpochModeSettings_Unit]string{
	Ydb_Table.ValueSinceUnixEpochModeSettings_UNIT_SECONDS:      "SECONDS",
	Ydb_Table.ValueSinceUnixEpochModeSettings_UNIT_MILLISECONDS: "MILLISECONDS",
	Ydb_Table.ValueSinceUnixEpochModeSettings_UNIT_MICROSECONDS: "MICROSECONDS",
	Ydb_Table.ValueSinceUnixEpochModeSettings_UNIT_NANOSECONDS:  "NANOSECONDS",
}

// intervalText writes seconds as an ISO 8601 duration of days, hours, minutes
// and seconds.
func intervalText(seconds uint64) string {
	return ydbtype.IntervalText(int64(seconds) * 1_000_000)
}

// flagText spells a switch the way YDB's settings take it.
func flagText(flag Ydb.FeatureFlag_Status) string {
	if flag == Ydb.FeatureFlag_ENABLED {
		return "ENABLED"
	}
	return "DISABLED"
}

// replicasText spells the read replicas as READ_REPLICAS_SETTINGS takes them,
// or "" for none.
func replicasText(replicas *Ydb_Table.ReadReplicasSettings) string {
	switch settings := replicas.GetSettings().(type) {
	case *Ydb_Table.ReadReplicasSettings_PerAzReadReplicasCount:
		return "PER_AZ:" + strconv.FormatUint(settings.PerAzReadReplicasCount, 10)
	case *Ydb_Table.ReadReplicasSettings_AnyAzReadReplicasCount:
		return "ANY_AZ:" + strconv.FormatUint(settings.AnyAzReadReplicasCount, 10)
	default:
		return ""
	}
}

// splitPoints writes the split points of a starting layout as
// PARTITION_AT_KEYS lists them: each a tuple of literal numbers and strings
// over the leading key columns.
func splitPoints(explicit *Ydb_Table.ExplicitPartitions) (string, error) {
	points := make([]string, 0, len(explicit.GetSplitPoints()))
	for _, point := range explicit.GetSplitPoints() {
		elements := point.GetType().GetTupleType().GetElements()
		items := point.GetValue().GetItems()
		if len(elements) == 0 || len(elements) != len(items) {
			return "", noTextForm("a split point is not a tuple of key values")
		}
		values := make([]string, 0, len(items))
		for i, item := range items {
			text, err := splitValueText(elements[i].GetOptionalType().GetItem(), item.GetNestedValue())
			if err != nil {
				return "", err
			}
			values = append(values, text)
		}
		points = append(points, "("+strings.Join(values, ", ")+")")
	}
	return strings.Join(points, ", "), nil
}

// splitValueText writes one value of a split point: a whole number for an
// integer key column, and a quoted string for a Utf8 or String one, which is
// what the setting takes.
func splitValueText(kind *Ydb.Type, value *Ydb.Value) (string, error) {
	switch kind.GetTypeId() {
	case Ydb.Type_INT8, Ydb.Type_INT16, Ydb.Type_INT32:
		return strconv.FormatInt(int64(value.GetInt32Value()), 10), nil
	case Ydb.Type_INT64:
		return strconv.FormatInt(value.GetInt64Value(), 10), nil
	case Ydb.Type_UINT8, Ydb.Type_UINT16, Ydb.Type_UINT32:
		return strconv.FormatUint(uint64(value.GetUint32Value()), 10), nil
	case Ydb.Type_UINT64:
		return strconv.FormatUint(value.GetUint64Value(), 10), nil
	case Ydb.Type_UTF8:
		return ydbtype.StringLiteral(value.GetTextValue()), nil
	case Ydb.Type_STRING:
		return ydbtype.StringLiteral(string(value.GetBytesValue())), nil
	default:
		return "", noTextForm("a split point holds a value YQL's PARTITION_AT_KEYS does not take")
	}
}

// AlterationStatement writes the one ALTER TABLE a valid row-table alteration
// makes at path, in the spelling the host's embedded engine writes for each
// kind:
//
//   - families: `ADD FAMILY`, `ALTER FAMILY ... SET` and `ALTER COLUMN ... SET
//     FAMILY`, in that order, YDB applying the actions of one statement
//     together;
//   - additions and removals: `ADD COLUMN` and `DROP COLUMN`, one action per
//     column;
//   - the TTL: `SET (TTL = ...)` or `RESET (TTL)`;
//   - the partitioning: `SET (...)` naming every automatic partitioning
//     setting the table ends with, since a setting YQL leaves out can be reset
//     by one it names;
//   - an index: `ADD INDEX` or `DROP INDEX`.
func AlterationStatement(path string, alteration *Alteration) (string, error) {
	request := alteration.Request
	prefix := "ALTER TABLE " + QuoteIdentifier(path) + " "
	switch {
	case len(request.GetAddColumnFamilies())+len(request.GetAlterColumnFamilies()) != 0 || hasFamilyMoves(request):
		actions, err := familyActions(alteration)
		if err != nil {
			return "", err
		}
		return prefix + strings.Join(actions, ", ") + ";", nil
	case len(request.GetAddColumns()) != 0:
		actions := make([]string, 0, len(request.GetAddColumns()))
		for _, column := range request.GetAddColumns() {
			definition, err := columnDefinition(column)
			if err != nil {
				return "", err
			}
			actions = append(actions, "ADD COLUMN "+definition)
		}
		return prefix + strings.Join(actions, ", ") + ";", nil
	case len(request.GetDropColumns()) != 0:
		actions := make([]string, 0, len(request.GetDropColumns()))
		for _, column := range request.GetDropColumns() {
			actions = append(actions, "DROP COLUMN "+QuoteIdentifier(column))
		}
		return prefix + strings.Join(actions, ", ") + ";", nil
	case request.GetDropTtlSettings() != nil:
		return prefix + "RESET (TTL);", nil
	case request.GetSetTtlSettings() != nil:
		setting, err := ttlSetting(request.GetSetTtlSettings())
		if err != nil {
			return "", err
		}
		return prefix + "SET (TTL = " + setting + ");", nil
	case request.GetAlterPartitioningSettings() != nil:
		return prefix + "SET (" + strings.Join(partitioningActions(alteration), ", ") + ");", nil
	case len(request.GetAddIndexes()) != 0:
		clause, err := indexClause(request.GetAddIndexes()[0])
		if err != nil {
			return "", err
		}
		return prefix + "ADD " + clause + ";", nil
	case len(request.GetDropIndexes()) != 0:
		return prefix + "DROP INDEX " + QuoteIdentifier(request.GetDropIndexes()[0]) + ";", nil
	default:
		return "", noTextForm("the alteration changes nothing YQL names")
	}
}

// hasFamilyMoves reports an alteration that moves columns between families.
func hasFamilyMoves(request *Ydb_Table.AlterTableRequest) bool {
	return slices.ContainsFunc(request.GetAlterColumns(), func(column *Ydb_Table.ColumnMeta) bool { return column.GetFamily() != "" })
}

// familyActions are the actions of the one ALTER TABLE that changes the
// families: the families it adds, the settings it states, and the columns it
// moves, each group sorted by name as the host's embedded engine writes them.
func familyActions(alteration *Alteration) ([]string, error) {
	request := alteration.Request
	added := slices.Clone(request.GetAddColumnFamilies())
	slices.SortFunc(added, func(a, b *Ydb_Table.ColumnFamily) int { return strings.Compare(a.GetName(), b.GetName()) })
	var actions []string
	for _, family := range added {
		settings, err := familySettings(family)
		if err != nil {
			return nil, err
		}
		actions = append(actions, "ADD FAMILY "+QuoteIdentifier(familyKey(family.GetName()))+" ("+strings.Join(settings, ", ")+")")
	}
	altered := slices.Clone(request.GetAlterColumnFamilies())
	slices.SortFunc(altered, func(a, b *Ydb_Table.ColumnFamily) int {
		return strings.Compare(familyKey(a.GetName()), familyKey(b.GetName()))
	})
	for _, family := range altered {
		if family.GetKeepInMemory() != Ydb.FeatureFlag_STATUS_UNSPECIFIED {
			return nil, noTextForm("column family %q changes keep_in_memory, and YQL has no family setting for it", familyKey(family.GetName()))
		}
		set := "ALTER FAMILY " + QuoteIdentifier(familyKey(family.GetName())) + " SET "
		if media := family.GetData().GetMedia(); media != "" {
			actions = append(actions, set+"DATA "+ydbtype.StringLiteral(media))
		}
		if compression := compressionText(family.GetCompression()); compression != "" {
			actions = append(actions, set+"COMPRESSION "+ydbtype.StringLiteral(compression))
		}
	}
	moved := slices.Clone(request.GetAlterColumns())
	slices.SortFunc(moved, func(a, b *Ydb_Table.ColumnMeta) int { return strings.Compare(a.GetName(), b.GetName()) })
	for _, column := range moved {
		actions = append(actions, "ALTER COLUMN "+QuoteIdentifier(column.GetName())+" SET FAMILY "+QuoteIdentifier(familyKey(column.GetFamily())))
	}
	if len(actions) == 0 {
		return nil, noTextForm("the family alteration changes nothing YQL names")
	}
	return actions, nil
}

// partitioningActions name every automatic partitioning setting the table
// holds once the alteration's settings replace its own: by size, with the
// partition size where it splits by size, by load, the minimum count, and
// the maximum where it has one. Measured on 25.1.4.7 and 26.2.1.14, a SET that
// names one of them can reset another it leaves out, so the statement names
// the whole set, as the host's embedded engine writes it.
func partitioningActions(alteration *Alteration) []string {
	held, changes := alteration.HeldPartitioning, alteration.Request.GetAlterPartitioningSettings()
	bySize := held.GetPartitioningBySize()
	if flag := changes.GetPartitioningBySize(); flag != Ydb.FeatureFlag_STATUS_UNSPECIFIED {
		bySize = flag
	}
	size := held.GetPartitionSizeMb()
	if changes.GetPartitionSizeMb() != 0 {
		size = changes.GetPartitionSizeMb()
	}
	byLoad := held.GetPartitioningByLoad()
	if flag := changes.GetPartitioningByLoad(); flag != Ydb.FeatureFlag_STATUS_UNSPECIFIED {
		byLoad = flag
	}
	minimum := held.GetMinPartitionsCount()
	if changes.GetMinPartitionsCount() != 0 {
		minimum = changes.GetMinPartitionsCount()
	}
	maximum := held.GetMaxPartitionsCount()
	if changes.GetMaxPartitionsCount() != 0 {
		maximum = changes.GetMaxPartitionsCount()
	}
	settings := []string{"AUTO_PARTITIONING_BY_SIZE = " + flagText(bySize)}
	if bySize == Ydb.FeatureFlag_ENABLED {
		settings = append(settings, "AUTO_PARTITIONING_PARTITION_SIZE_MB = "+strconv.FormatUint(size, 10))
	}
	settings = append(settings, "AUTO_PARTITIONING_BY_LOAD = "+flagText(byLoad),
		"AUTO_PARTITIONING_MIN_PARTITIONS_COUNT = "+strconv.FormatUint(max(minimum, 1), 10))
	if maximum != 0 {
		settings = append(settings, "AUTO_PARTITIONING_MAX_PARTITIONS_COUNT = "+strconv.FormatUint(maximum, 10))
	}
	return settings
}

// indexClause writes a global index the way an ADD INDEX names it:
// `INDEX `i` GLOBAL SYNC ON (`a`) COVER (`b`)`.
func indexClause(index *Ydb_Table.TableIndex) (string, error) {
	var kind string
	switch index.GetType().(type) {
	case *Ydb_Table.TableIndex_GlobalIndex:
		kind = "GLOBAL SYNC"
	case *Ydb_Table.TableIndex_GlobalAsyncIndex:
		kind = "GLOBAL ASYNC"
	case *Ydb_Table.TableIndex_GlobalUniqueIndex:
		kind = "GLOBAL UNIQUE SYNC"
	default:
		return "", noTextForm("index %q is of a kind the native alteration does not add", index.GetName())
	}
	clause := "INDEX " + QuoteIdentifier(index.GetName()) + " " + kind + " ON (" + quoteList(index.GetIndexColumns()) + ")"
	if len(index.GetDataColumns()) != 0 {
		clause += " COVER (" + quoteList(index.GetDataColumns()) + ")"
	}
	return clause, nil
}
