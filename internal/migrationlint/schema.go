package migrationlint

import (
	"cmp"
	"maps"
	"slices"
	"strconv"
	"strings"

	"github.com/stokaro/ptah-ydb-plugin/internal/ydbcomment"
	"github.com/stokaro/ptah-ydb-plugin/internal/yqlddl"
)

// ydbSchema is the part of a YDB schema the rules read, as the directory's
// own statements leave it.
type ydbSchema struct {
	tables map[string]ydbTable
	// views are the tables each view reads, by view name.
	views map[string][]string
	// failedOver are the async replications an ALTER ASYNC REPLICATION SET
	// (STATE = 'DONE') failed over, by name.
	failedOver map[string]bool
}

// ydbTable is what the rules read about one table.
type ydbTable struct {
	indexes []yqlddl.Index
	// ttl is the column the TTL reads, empty when the table has none.
	ttl string
	// columns are the table's columns, in order, when columnsKnown: a table
	// the directory created has them all, and one it only altered does not.
	columns      []string
	columnsKnown bool
	// minPartitions is the table's AUTO_PARTITIONING_MIN_PARTITIONS_COUNT
	// when minKnown: set by the CREATE TABLE the directory ran, or by an ALTER
	// TABLE that sets the count or resets it.
	minPartitions int
	minKnown      bool
	// sizeMB is the table's AUTO_PARTITIONING_PARTITION_SIZE_MB when
	// sizeKnown, and 0 for a table that does not split by size.
	sizeMB    int
	sizeKnown bool
	// serials are the YDB types of the table's Serial columns, by column:
	// Serial, BigSerial or SmallSerial, whichever the CREATE TABLE wrote.
	serials map[string]string
	// restarts are, by Serial column, the value the last RESTART of the
	// column's sequence moved it to, as the statement wrote it.
	restarts map[string]string
	// changefeeds are the table's changefeeds, in the order they were added.
	changefeeds []string
	// families are the table's column families, the default one included,
	// when familiesKnown.
	families      []string
	familiesKnown bool
	// comments are the comments Ptah's COMMENT ON statements left on the
	// table, by the attribute that holds each.
	comments map[string]string
}

func (s *ydbSchema) clone() *ydbSchema {
	cloned := &ydbSchema{tables: make(map[string]ydbTable), views: make(map[string][]string), failedOver: make(map[string]bool)}
	if s == nil {
		return cloned
	}
	maps.Copy(cloned.failedOver, s.failedOver)
	for name, table := range s.tables {
		cloned.tables[name] = ydbTable{
			indexes: slices.Clone(table.indexes), ttl: table.ttl,
			columns: slices.Clone(table.columns), columnsKnown: table.columnsKnown,
			minPartitions: table.minPartitions, minKnown: table.minKnown,
			sizeMB: table.sizeMB, sizeKnown: table.sizeKnown,
			serials: maps.Clone(table.serials), restarts: maps.Clone(table.restarts),
			changefeeds: slices.Clone(table.changefeeds), comments: maps.Clone(table.comments),
			families: slices.Clone(table.families), familiesKnown: table.familiesKnown,
		}
	}
	for name, reads := range s.views {
		cloned.views[name] = slices.Clone(reads)
	}
	return cloned
}

// table returns what the schema knows about a table, which is nothing for one
// the directory never named.
func (s *ydbSchema) table(name string) ydbTable {
	return s.tables[name]
}

// store records a table an ALTER TABLE changed, under its new name when the
// statement renamed it.
func (s *ydbSchema) store(read yqlddl.Statement, table ydbTable) {
	if read.Name == "" {
		return
	}
	name := read.Name
	for _, action := range read.Actions {
		if action.Kind == yqlddl.RenameTable && action.NewName != "" {
			delete(s.tables, name)
			name = action.NewName
		}
	}
	s.tables[name] = table
}

// apply changes the schema the way a statement does.
func (s *ydbSchema) apply(read yqlddl.Statement) {
	if read.Name == "" {
		return
	}
	switch read.Kind {
	case yqlddl.CreateTable:
		if _, exists := s.tables[read.Name]; exists && read.IfExists {
			return
		}
		columns := make([]string, 0, len(read.Columns))
		for _, column := range read.Columns {
			columns = append(columns, column.Name)
		}
		minimum, minKnown := createdMinPartitions(read.Settings)
		size, sizeKnown := createdPartitionSize(read.Settings)
		s.tables[read.Name] = ydbTable{
			indexes: slices.Clone(read.Indexes), ttl: read.TTLColumn, columns: columns, columnsKnown: true,
			minPartitions: minimum, minKnown: minKnown, sizeMB: size, sizeKnown: sizeKnown, serials: serialColumns(read),
			families: append([]string{defaultFamily}, read.Families...), familiesKnown: true,
		}
	case yqlddl.AlterTable:
		table := s.table(read.Name)
		for _, action := range read.Actions {
			table = table.applyAction(action)
		}
		table.minPartitions, table.minKnown = alteredMinPartitions(read, table)
		table.sizeMB, table.sizeKnown = alteredPartitionSize(read, table)
		s.store(read, table)
	case yqlddl.DropTable:
		delete(s.tables, read.Name)
	case yqlddl.CreateView:
		if _, exists := s.views[read.Name]; exists && read.IfExists {
			return
		}
		s.views[read.Name] = slices.Clone(read.Reads)
	case yqlddl.DropView:
		delete(s.views, read.Name)
	case yqlddl.AlterAsyncReplication:
		if failsOver(read) {
			s.failedOver[read.Name] = true
		}
	case yqlddl.DropAsyncReplication, yqlddl.CreateAsyncReplication:
		delete(s.failedOver, read.Name)
	case yqlddl.AlterSequence:
		name, column, owned := s.sequenceOwner(read.Name)
		if !owned || !read.Restart {
			return
		}
		table := s.tables[name]
		table.restarts = maps.Clone(table.restarts)
		if table.restarts == nil {
			table.restarts = make(map[string]string)
		}
		table.restarts[column] = cmp.Or(read.RestartWith, "its start")
		s.tables[name] = table
	}
}

// applyComment records the comment one of Ptah's COMMENT ON statements sets
// on a table the directory knows, or removes it. A statement that names its
// table through a path prefix or an absolute path is not followed: the rules
// that read comments stay silent about a table they cannot name.
func (s *ydbSchema) applyComment(sql string) {
	query, recognized, err := ydbcomment.Recognize(sql)
	if !recognized || err != nil || query.PathPrefix != "" || strings.HasPrefix(query.Path, "/") || query.Object == ydbcomment.View {
		return
	}
	table, known := s.tables[query.Path]
	if !known {
		return
	}
	table.comments = maps.Clone(table.comments)
	if table.comments == nil {
		table.comments = make(map[string]string)
	}
	// An empty comment is a removal, and the rules read an empty comment as
	// none.
	table.comments[query.Key()] = query.Comment
	s.tables[query.Path] = table
}

// viewsReading returns the views that read a table, sorted.
func (s *ydbSchema) viewsReading(table string) []string {
	var views []string
	for view, reads := range s.views {
		if slices.Contains(reads, table) {
			views = append(views, view)
		}
	}
	slices.Sort(views)
	return views
}

// sequenceOwner returns the table and the Serial column whose sequence path
// names. The last element of the path is `_serial_column_<column>`, and the
// elements before it end with the table: YDB takes the path from the cluster
// root, so the database's own path comes first, and the directory names its
// tables from below that. Of the tables the directory knows, the longest that
// ends the path is the owner.
func (s *ydbSchema) sequenceOwner(path string) (table, column string, ok bool) {
	slash := strings.LastIndex(path, "/")
	if slash < 0 {
		return "", "", false
	}
	column, ok = strings.CutPrefix(path[slash+1:], serialSequencePrefix)
	if !ok || column == "" {
		return "", "", false
	}
	owner := path[:slash]
	for _, name := range slices.Sorted(maps.Keys(s.tables)) {
		if (owner == name || strings.HasSuffix(owner, "/"+name)) && len(name) > len(table) {
			table = name
		}
	}
	return table, column, table != ""
}

// applyAction returns the table as one ALTER TABLE action leaves it.
func (t ydbTable) applyAction(action yqlddl.Action) ydbTable {
	indexes := slices.Clone(t.indexes)
	switch action.Kind {
	case yqlddl.AddColumn:
		t.columns = append(slices.Clone(t.columns), action.Column.Name)
		t.families = t.withFamily(action.Column.Family)
	case yqlddl.DropColumn:
		t.columns = slices.DeleteFunc(slices.Clone(t.columns), func(name string) bool { return name == action.Column.Name })
	case yqlddl.AddIndex:
		indexes = slices.DeleteFunc(indexes, func(index yqlddl.Index) bool { return index.Name == action.Index.Name })
		indexes = append(indexes, action.Index)
	case yqlddl.DropIndex:
		indexes = slices.DeleteFunc(indexes, func(index yqlddl.Index) bool { return index.Name == action.Index.Name })
	case yqlddl.RenameIndex:
		for i := range indexes {
			if indexes[i].Name == action.Index.Name {
				indexes[i].Name = action.NewName
			}
		}
	case yqlddl.SetSettings:
		for _, setting := range action.Settings {
			if setting.Name == "TTL" {
				t.ttl = setting.Column
			}
		}
	case yqlddl.ResetSettings:
		for _, setting := range action.Settings {
			if setting.Name == "TTL" {
				t.ttl = ""
			}
		}
	case yqlddl.AddFamily, yqlddl.AlterFamily, yqlddl.SetColumnFamily:
		t.families = t.withFamily(action.Family)
	case yqlddl.AddChangefeed:
		t.changefeeds = append(slices.Clone(t.changefeeds), action.Changefeed)
	case yqlddl.DropChangefeed:
		t.changefeeds = slices.DeleteFunc(slices.Clone(t.changefeeds), func(name string) bool { return name == action.Changefeed })
	}
	t.indexes = indexes
	return t
}

// withFamily returns the table's families with family among them, as a
// statement that names it leaves them: YDB creates a family an action names
// and the table does not have.
func (t ydbTable) withFamily(family string) []string {
	if family == "" || slices.Contains(t.families, family) {
		return t.families
	}
	return append(slices.Clone(t.families), family)
}

// vectorIndex returns the name of a vector index the table holds, and false
// where it holds none.
func (t ydbTable) vectorIndex() (string, bool) {
	for _, index := range t.indexes {
		if index.Vector() {
			return index.Name, true
		}
	}
	return "", false
}

// defaultFamily is the column family every table has.
const defaultFamily = "default"

// serialSequencePrefix starts the name of the sequence behind a Serial
// column, which the column's name follows.
const serialSequencePrefix = "_serial_column_"

// serialTypes are the spellings YQL takes for a Serial column, by the type
// each makes. Measured on 26.2.1.14 and 25.1.4.7, in any letter case: a
// SmallSerial or Serial2 column is Int16, a Serial or Serial4 column Int32,
// and a BigSerial or Serial8 column Int64.
var serialTypes = map[string]string{
	"smallserial": "SmallSerial", "serial2": "SmallSerial",
	"serial": "Serial", "serial4": "Serial",
	"bigserial": "BigSerial", "serial8": "BigSerial",
}

// serialColumns returns the Serial columns a CREATE TABLE declares, by column,
// with the type each makes.
func serialColumns(read yqlddl.Statement) map[string]string {
	serials := make(map[string]string)
	for _, column := range read.Columns {
		if serial, ok := serialTypes[strings.ToLower(column.Type)]; ok {
			serials[column.Name] = serial
		}
	}
	return serials
}

// The partitioning settings the reset rules read.
const (
	ydbMinPartitions = "AUTO_PARTITIONING_MIN_PARTITIONS_COUNT"
	// ydbBySize turns splitting by size on and off.
	ydbBySize = "AUTO_PARTITIONING_BY_SIZE"
	// ydbPartitionSize is the setting turning splitting by size on resets.
	ydbPartitionSize = "AUTO_PARTITIONING_PARTITION_SIZE_MB"
	// ydbDefaultPartitionSize is the partition size YDB gives a table that
	// names none, and the one turning splitting by size on resets it to.
	ydbDefaultPartitionSize = 2048
)

// createdMinPartitions is the minimum partition count a CREATE TABLE leaves,
// and whether it is known. Measured on 26.2.1.14 and 25.1.4.7: a table created
// with no partitioning setting, or with AUTO_PARTITIONING_BY_SIZE or _BY_LOAD
// alone, has a minimum of 1; UNIFORM_PARTITIONS = 4 leaves 4, also beside
// AUTO_PARTITIONING_BY_SIZE = ENABLED in either order; PARTITION_AT_KEYS with
// two split points leaves 3, and with three composite ones 4; and
// AUTO_PARTITIONING_MIN_PARTITIONS_COUNT wins over either. A value the reader
// cannot read, or both kinds of split together, leaves it unknown.
func createdMinPartitions(settings []yqlddl.Setting) (int, bool) {
	var explicit, uniform, atKeys int
	for _, setting := range settings {
		switch setting.Name {
		case ydbMinPartitions:
			value, ok := partitionCount(setting)
			if !ok {
				return 0, false
			}
			explicit = value
		case "UNIFORM_PARTITIONS":
			value, ok := partitionCount(setting)
			if !ok {
				return 0, false
			}
			uniform = value
		case "PARTITION_AT_KEYS":
			if setting.Items == 0 {
				return 0, false
			}
			atKeys = setting.Items + 1
		}
	}
	switch {
	case explicit > 0:
		return explicit, true
	case uniform > 0 && atKeys > 0:
		return 0, false
	case uniform > 0:
		return uniform, true
	case atKeys > 0:
		return atKeys, true
	default:
		return 1, true
	}
}

// partitionCount reads a setting whose value is a count of partitions.
func partitionCount(setting yqlddl.Setting) (int, bool) {
	value, err := strconv.Atoi(setting.Value)
	return value, err == nil && value > 0
}

// alteredMinPartitions is the minimum partition count an ALTER TABLE leaves on
// table: the count it sets, wherever in the statement; 1 when it turns auto
// partitioning by size or by load on without one; and the table's own
// otherwise. YDB refuses RESET (AUTO_PARTITIONING_MIN_PARTITIONS_COUNT) and a
// change of UNIFORM_PARTITIONS, measured on both lines.
func alteredMinPartitions(read yqlddl.Statement, table ydbTable) (int, bool) {
	settings := settingsSet(read)
	for _, setting := range settings {
		if setting.Name == ydbMinPartitions {
			return partitionCount(setting)
		}
	}
	for _, setting := range settings {
		if setting.Value == "ENABLED" && (setting.Name == ydbBySize || setting.Name == "AUTO_PARTITIONING_BY_LOAD") {
			return 1, true
		}
	}
	return table.minPartitions, table.minKnown
}

// createdPartitionSize is the partition size a CREATE TABLE leaves, 0 for a
// table that does not split by size, and whether it is known: the size the
// statement names, else 0 where it turns splitting by size off, else YDB's
// 2048 MB. A size the reader cannot read leaves it unknown.
func createdPartitionSize(settings []yqlddl.Setting) (int, bool) {
	size, splitting := ydbDefaultPartitionSize, true
	for _, setting := range settings {
		switch setting.Name {
		case ydbPartitionSize:
			value, ok := partitionCount(setting)
			if !ok {
				return 0, false
			}
			size = value
		case ydbBySize:
			splitting = setting.Value != "DISABLED"
		}
	}
	if !splitting {
		return 0, true
	}
	return size, true
}

// alteredPartitionSize is the partition size an ALTER TABLE leaves on table:
// the size it sets, wherever in the statement -- a size set on a table that
// does not split by size turns splitting on, measured on both lines -- else 0
// where it turns splitting by size off, 2048 MB where it turns it on, and the
// table's own otherwise.
func alteredPartitionSize(read yqlddl.Statement, table ydbTable) (int, bool) {
	settings := settingsSet(read)
	for _, setting := range settings {
		if setting.Name == ydbPartitionSize {
			return partitionCount(setting)
		}
	}
	for _, setting := range settings {
		if setting.Name == ydbBySize {
			if setting.Value == "DISABLED" {
				return 0, true
			}
			return ydbDefaultPartitionSize, true
		}
	}
	return table.sizeMB, table.sizeKnown
}

// settingsSet returns every setting the SET actions of an ALTER TABLE name.
func settingsSet(read yqlddl.Statement) []yqlddl.Setting {
	var settings []yqlddl.Setting
	for _, action := range read.Actions {
		if action.Kind == yqlddl.SetSettings {
			settings = append(settings, action.Settings...)
		}
	}
	return settings
}

// failsOver reports an ALTER ASYNC REPLICATION that sets STATE = 'DONE'.
func failsOver(read yqlddl.Statement) bool {
	for _, action := range read.Actions {
		for _, setting := range action.Settings {
			if setting.Name == "STATE" && strings.EqualFold(setting.Text, "DONE") {
				return true
			}
		}
	}
	return false
}
