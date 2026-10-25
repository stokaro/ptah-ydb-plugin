package migrationlint

import (
	"fmt"
	"slices"
	"strings"

	"github.com/stokaro/ptah-ydb-plugin/internal/ydbcomment"
	"github.com/stokaro/ptah-ydb-plugin/internal/yqlddl"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
)

// The capability keys the rules read besides the ones a statement's
// requirements name; see [yqlddl.Statement.Requirements].
const (
	// serialSequenceKeepsRange is a line where ALTER SEQUENCE keeps the
	// maximum of a 16-bit or 32-bit Serial column's sequence.
	serialSequenceKeepsRange yqlddl.Capability = "serial_sequence_keeps_range"
	// vectorIndexMaintainedOnWrite is a line whose vector index takes the
	// rows written after it was built.
	vectorIndexMaintainedOnWrite yqlddl.Capability = "vector_index_maintained_on_write"
)

// quoteName quotes one YQL name verbatim in backticks, escaping a backslash
// and a backtick, as Ptah writes the names of the statements it plans.
func quoteName(name string) string {
	return "`" + strings.NewReplacer(`\`, `\\`, "`", "\\`").Replace(name) + "`"
}

// historyRules are the rules that read what a table looks like before a
// statement, from the directory's own history h (see the package
// documentation).
func historyRules(h history) []rule {
	return []rule{
		ydbDropUsedColumnRule(h),
		ydbPartitionMinimumResetRule(h),
		ydbPartitionSizeResetRule(h),
		ydbViewOrphanedRule(h),
		ydbMovedTableWithChangefeedRule(h),
		ydbNarrowSerialSequenceRule(h),
		ydbReplayedRestartRule(h),
		ydbReplicationDroppedWithoutFailoverRule(h),
		ydbUndeclaredColumnFamilyRule(h),
		ydbVectorIndexStaleRule(h),
		ydbCommentLeftBehindRule(h),
	}
}

// ydbDropUsedColumnRule reports a DROP COLUMN of a column an index keys or
// covers, or of the column the table's TTL reads. Measured on 26.2.1.14 and
// 25.1.4.7:
//
//	key column of an index   Impossible drop column because table has an index with that column
//	covered column           Impossible drop column because table index covers that column
//	TTL column               Can't drop TTL column: 'ts', disable TTL first
//
// After ALTER TABLE ... DROP INDEX, and after ALTER TABLE ... RESET (TTL),
// each drop is accepted.
func ydbDropUsedColumnRule(h history) rule {
	return rule{
		published: published{
			Code:     "YD104",
			Title:    "column dropped while an index or the TTL uses it",
			Severity: protocol.LintRule_ERROR,
		},
		File: func(file checkedFile) []Violation {
			state := h.before(file.Name)
			var findings []Violation
			for i := range file.Statements {
				stmt := &file.Statements[i]
				read := yqlddl.Read(stmt.SQL)
				if read.Kind != yqlddl.AlterTable {
					state.apply(read)
					continue
				}
				table := state.table(read.Name)
				for _, action := range read.Actions {
					if action.Kind == yqlddl.DropColumn {
						if message, used := table.dropRefusal(action.Column.Name); used {
							findings = append(findings, Violation{
								Statement: i,
								Message:   fmt.Sprintf("DROP COLUMN %s: %s", action.Column.Name, message),
								Subjects: []Subject{{
									Kind: protocol.LintSubject_COLUMN, Name: action.Column.Name, Parent: read.Name,
								}},
							})
						}
					}
					table = table.applyAction(action)
				}
				state.store(read, table)
			}
			return findings
		},
	}
}

// dropRefusal says why YDB refuses to drop a column of the table, and reports
// whether it does.
func (t ydbTable) dropRefusal(column string) (string, bool) {
	for _, index := range t.indexes {
		switch {
		case slices.Contains(index.Columns, column):
			return fmt.Sprintf("index %s keys the column, and YDB refuses to drop it "+
				"(Impossible drop column because table has an index with that column); drop the index first", index.Name), true
		case slices.Contains(index.Cover, column):
			return fmt.Sprintf("index %s covers the column, and YDB refuses to drop it "+
				"(Impossible drop column because table index covers that column); drop the index first", index.Name), true
		}
	}
	if t.ttl != "" && t.ttl == column {
		return "the table's TTL reads the column, and YDB refuses to drop it (Can't drop TTL column, disable TTL first); " +
			"run ALTER TABLE ... RESET (TTL) first", true
	}
	return "", false
}

// ydbPartitionMinimumResetRule reports an ALTER TABLE that turns
// AUTO_PARTITIONING_BY_SIZE or AUTO_PARTITIONING_BY_LOAD on without setting
// AUTO_PARTITIONING_MIN_PARTITIONS_COUNT in the same statement. Measured on
// 26.2.1.14 and 25.1.4.7 against a table whose minimum was 4:
//
//	SET (AUTO_PARTITIONING_BY_SIZE = ENABLED)        minimum 1, also when it was enabled already
//	SET (AUTO_PARTITIONING_BY_LOAD = ENABLED)        minimum 1
//	SET AUTO_PARTITIONING_BY_LOAD ENABLED            minimum 1
//	SET (..._BY_SIZE = ENABLED, ..._MIN_PARTITIONS_COUNT = 4)       minimum 4
//	SET (..._MIN_PARTITIONS_COUNT = 4), SET (..._BY_LOAD = ENABLED) minimum 4
//	SET (..._BY_LOAD = DISABLED), or a size or maximum setting      minimum 4
//
// With the minimum at 1, YDB may merge the table down to one partition. A
// table whose minimum was 1 already loses nothing, so the rule stays silent
// where the directory's own history says so (see [ydbTable.minPartitions]),
// and warns where it does not know.
func ydbPartitionMinimumResetRule(h history) rule {
	return ydbTableResetRule(h, "YD105", "partitioning change resets the minimum partition count", partitionMinimumReset)
}

// ydbTableResetRule is a warning about an ALTER TABLE that resets a setting of
// its table. reset reads each statement against the table as the directory's
// history leaves it before that statement, and says why the statement resets
// the setting; the history then takes the statement whether it was reported or
// not.
func ydbTableResetRule(h history, code, title string, reset func(yqlddl.Statement, ydbTable) (string, bool)) rule {
	return rule{
		published: published{
			Code:     code,
			Title:    title,
			Severity: protocol.LintRule_WARNING,
		},
		File: func(file checkedFile) []Violation {
			state := h.before(file.Name)
			var findings []Violation
			for i := range file.Statements {
				stmt := &file.Statements[i]
				read := yqlddl.Read(stmt.SQL)
				if message, resets := reset(read, state.table(read.Name)); resets {
					findings = append(findings, Violation{
						Statement: i,
						Message:   message,
						Subjects:  []Subject{{Kind: protocol.LintSubject_TABLE, Name: read.Name}},
					})
				}
				state.apply(read)
			}
			return findings
		},
	}
}

// partitionMinimumReset says why an ALTER TABLE of table resets its minimum
// partition count to 1, and reports whether it does. A table already at a
// minimum of 1 has nothing to lose.
func partitionMinimumReset(read yqlddl.Statement, table ydbTable) (string, bool) {
	if read.Kind != yqlddl.AlterTable || (table.minKnown && table.minPartitions == 1) {
		return "", false
	}
	enabled := ""
	for _, setting := range settingsSet(read) {
		switch {
		case setting.Name == ydbMinPartitions:
			return "", false
		case enabled == "" && setting.Value == "ENABLED" &&
			(setting.Name == "AUTO_PARTITIONING_BY_SIZE" || setting.Name == "AUTO_PARTITIONING_BY_LOAD"):
			enabled = setting.Name
		}
	}
	if enabled == "" {
		return "", false
	}
	return fmt.Sprintf(
		"setting %s = ENABLED resets %s of %s to 1, so YDB may merge its partitions down to one; "+
			"set %s in the same ALTER TABLE to keep it",
		enabled, ydbMinPartitions, read.Name, ydbMinPartitions), true
}

// ydbPartitionSizeResetRule reports an ALTER TABLE that turns
// AUTO_PARTITIONING_BY_SIZE on without setting
// AUTO_PARTITIONING_PARTITION_SIZE_MB in the same statement. Measured on
// 26.2.1.14 and 25.1.4.7 against a table whose partition size was 100 MB:
//
//	SET (AUTO_PARTITIONING_BY_SIZE = ENABLED)                         2048 MB, also when it was enabled already
//	SET (..._MIN_PARTITIONS_COUNT = 6, ..._BY_SIZE = ENABLED)         2048 MB
//	SET (..._BY_SIZE = ENABLED, ..._PARTITION_SIZE_MB = 100, ...)     100 MB
//	SET (..._BY_LOAD = ENABLED), or a minimum or maximum setting      100 MB
//
// A table that splits at 2048 MB already, or does not split by size at all,
// loses nothing, so the rule stays silent where the directory's own history
// says so (see [ydbTable.sizeMB]), and warns where it does not know. YD105
// reports the minimum the same statement resets.
func ydbPartitionSizeResetRule(h history) rule {
	return ydbTableResetRule(h, "YD118", "partitioning change resets the partition size", partitionSizeReset)
}

// partitionSizeReset says why an ALTER TABLE of table resets its partition
// size to 2048 MB, and reports whether it does. A table already at 2048 MB, or
// one that does not split by size and so holds no size, has nothing to lose.
func partitionSizeReset(read yqlddl.Statement, table ydbTable) (string, bool) {
	if read.Kind != yqlddl.AlterTable ||
		(table.sizeKnown && (table.sizeMB == ydbDefaultPartitionSize || table.sizeMB == 0)) {
		return "", false
	}
	enabled := false
	for _, setting := range settingsSet(read) {
		switch {
		case setting.Name == ydbPartitionSize:
			return "", false
		case setting.Name == ydbBySize && setting.Value == "ENABLED":
			enabled = true
		}
	}
	if !enabled {
		return "", false
	}
	return fmt.Sprintf(
		"setting %s = ENABLED resets %s of %s to %d, whatever size it held; set %s in the same ALTER TABLE to keep it",
		ydbBySize, ydbPartitionSize, read.Name, ydbDefaultPartitionSize, ydbPartitionSize), true
}

// ydbViewOrphanedRule reports a DROP TABLE of a table a view reads, and an
// ALTER TABLE ... RENAME TO of one. A view reads its table by path when it is
// read, so YDB keeps a view whose table is dropped or moved, and every read of
// the view then fails. Measured on 26.2.1.14 and 25.1.4.7: after `CREATE VIEW
// vv WITH (security_invoker = TRUE) AS SELECT id FROM vbase`, `DROP TABLE
// vbase` succeeds, and `SELECT * FROM vv` answers `Cannot find table
// 'db.[/local/vbase]' because it does not exist`. After `ALTER TABLE vbase
// RENAME TO vmoved` the read answers the same, and renaming the table back
// makes the view read it again.
//
// The renames of a table rebuild are left out (see [history.rebuilds]): the
// copy takes the table's name back in the next statement, and the view reads
// it.
func ydbViewOrphanedRule(h history) rule {
	return rule{
		published: published{
			Code:     "YD106",
			Title:    "table dropped or renamed while a view reads it",
			Severity: protocol.LintRule_ERROR,
		},
		File: func(file checkedFile) []Violation {
			rebuilds := h.rebuilds(file)
			state := h.before(file.Name)
			var findings []Violation
			for i := range file.Statements {
				stmt := &file.Statements[i]
				read := yqlddl.Read(stmt.SQL)
				views := state.viewsReading(read.Name)
				var message string
				switch {
				case len(views) == 0:
				case read.Kind == yqlddl.DropTable:
					message = fmt.Sprintf(
						"DROP TABLE %s leaves %s reading a table that does not exist: YDB keeps a view whose table is dropped, "+
							"and every read of it fails; drop or recreate %s first",
						read.Name, viewList(views), pronounFor(views))
				case read.Kind == yqlddl.AlterTable && !rebuilds.renames[i]:
					if newName, renamed := tableRenamedTo(read); renamed {
						message = fmt.Sprintf(
							"ALTER TABLE %s RENAME TO %s leaves %s reading a table that does not exist: a view reads its table "+
								"by path, so every read of it fails until a table takes the name %s again; recreate %s over %s",
							read.Name, newName, viewList(views), read.Name, pronounFor(views), newName)
					}
				}
				if message != "" {
					findings = append(findings, Violation{
						Statement: i,
						Message:   message,
						Subjects:  []Subject{{Kind: protocol.LintSubject_TABLE, Name: read.Name}},
					})
				}
				state.apply(read)
			}
			return findings
		},
	}
}

// tableRenamedTo returns the name an ALTER TABLE moves its table to, and reports
// whether it renames it.
func tableRenamedTo(read yqlddl.Statement) (string, bool) {
	for _, action := range read.Actions {
		if action.Kind == yqlddl.RenameTable && action.NewName != "" {
			return action.NewName, true
		}
	}
	return "", false
}

func viewList(views []string) string {
	if len(views) == 1 {
		return "view " + views[0]
	}
	return "views " + strings.Join(views, ", ")
}

func pronounFor(views []string) string {
	if len(views) == 1 {
		return "it"
	}
	return "them"
}

// ydbMovedTableWithChangefeedRule reports an ALTER TABLE ... RENAME TO of a
// table that carries a changefeed, which YDB refuses. Measured on 25.1.4.7,
// 25.2.1.24, 25.3.1.25, 25.4.1.15, 26.1.1.22 and 26.2.1.14:
//
//	ALTER TABLE t RENAME TO u   Cannot move table with cdc streams
//
// with one changefeed or several, and after DROP CHANGEFEED of each the same
// rename is accepted. The table's changefeeds are read from the directory's
// own migrations, as YD104 reads its indexes.
func ydbMovedTableWithChangefeedRule(h history) rule {
	return rule{
		published: published{
			Code:     "YD109",
			Title:    "table renamed while it carries a changefeed",
			Severity: protocol.LintRule_ERROR,
		},
		File: func(file checkedFile) []Violation {
			state := h.before(file.Name)
			var findings []Violation
			for i := range file.Statements {
				stmt := &file.Statements[i]
				read := yqlddl.Read(stmt.SQL)
				if read.Kind != yqlddl.AlterTable {
					state.apply(read)
					continue
				}
				table := state.table(read.Name)
				for _, action := range read.Actions {
					if action.Kind == yqlddl.RenameTable && len(table.changefeeds) > 0 {
						findings = append(findings, Violation{
							Statement: i,
							Message: fmt.Sprintf(
								"RENAME TO moves %s, which carries %s, and YDB moves no table that carries one "+
									"(Cannot move table with cdc streams); drop the changefeeds first and add them again after "+
									"the move, which restarts each stream",
								read.Name, changefeedList(table.changefeeds)),
							Subjects: []Subject{{Kind: protocol.LintSubject_TABLE, Name: read.Name}},
						})
					}
					table = table.applyAction(action)
				}
				state.store(read, table)
			}
			return findings
		},
	}
}

// changefeedList names a table's changefeeds in a finding.
func changefeedList(changefeeds []string) string {
	if len(changefeeds) == 1 {
		return "changefeed " + changefeeds[0]
	}
	return "changefeeds " + strings.Join(changefeeds, ", ")
}

// ydbNarrowSerialSequenceRule reports an ALTER SEQUENCE on the sequence of a
// Serial or SmallSerial column the directory created. Measured on 26.2.1.14
// and 25.1.4.7, any ALTER SEQUENCE, `INCREMENT BY 1` included, raises the
// sequence's maximum from the column's to the Int64 maximum, and nothing
// lowers it again:
//
//	before, SmallSerial at 32767   doesn't have any more values available
//	after,  SmallSerial at 32767   the next row is stored with id -32768
//	after,  Serial at 2147483647   the next row is stored with id -2147483648
//
// The statement succeeds, and the column stores the value past its range as a
// negative number without an error. A BigSerial's maximum is the Int64
// maximum already.
func ydbNarrowSerialSequenceRule(h history) rule {
	return rule{
		published: published{
			Code:          "YD107",
			Title:         "ALTER SEQUENCE widens a 16-bit or 32-bit Serial",
			Severity:      protocol.LintRule_ERROR,
			AppliesToDown: true,
		},
		File: func(file checkedFile) []Violation {
			if file.Target.Has(serialSequenceKeepsRange) {
				return nil
			}
			state := h.before(file.Name)
			var findings []Violation
			for i := range file.Statements {
				stmt := &file.Statements[i]
				read := yqlddl.Read(stmt.SQL)
				if read.Kind == yqlddl.AlterSequence {
					table, column, owned := state.sequenceOwner(read.Name)
					serial := state.table(table).serials[column]
					if owned && (serial == "Serial" || serial == "SmallSerial") {
						findings = append(findings, Violation{
							Statement: i,
							Message: fmt.Sprintf(
								"ALTER SEQUENCE %s raises the maximum of the sequence of %s column %s.%s to the Int64 maximum, "+
									"and the column then stores the value past its own maximum as a negative number without "+
									"an error; declare the column BigSerial to give its sequence a start or an increment",
								read.Name, serial, table, column),
							Subjects: []Subject{{Kind: protocol.LintSubject_COLUMN, Name: column, Parent: table}},
						})
					}
				}
				state.apply(read)
			}
			return findings
		},
	}
}

// ydbReplayedRestartRule reports an ALTER SEQUENCE without a RESTART of its
// own on a sequence an earlier statement restarted. Measured on 26.2.1.14 and
// 25.1.4.7: after `RESTART WITH 500` and a row holding 500, `ALTER SEQUENCE
// ... INCREMENT BY 1` succeeds and moves the next value back to 500, and the
// next insert fails with `Conflict with existing key`. A RESTART of the
// statement's own sets a new value instead.
func ydbReplayedRestartRule(h history) rule {
	return rule{
		published: published{
			Code:          "YD108",
			Title:         "ALTER SEQUENCE replays an earlier RESTART",
			Severity:      protocol.LintRule_ERROR,
			AppliesToDown: true,
		},
		File: func(file checkedFile) []Violation {
			state := h.before(file.Name)
			var findings []Violation
			for i := range file.Statements {
				stmt := &file.Statements[i]
				read := yqlddl.Read(stmt.SQL)
				if read.Kind == yqlddl.AlterSequence && !read.Restart {
					table, column, owned := state.sequenceOwner(read.Name)
					if restart, restarted := state.table(table).restarts[column]; owned && restarted {
						findings = append(findings, Violation{
							Statement: i,
							Message: fmt.Sprintf(
								"ALTER SEQUENCE %s alters a sequence an earlier statement restarted at %s, and YDB replays that "+
									"restart on every later ALTER SEQUENCE, so the next row of %s takes %s again and fails on a key "+
									"a row already holds (Conflict with existing key); restart it in this statement at a value "+
									"past every row, or leave it as it is",
								read.Name, restart, table, restart),
							Subjects: []Subject{{Kind: protocol.LintSubject_COLUMN, Name: column, Parent: table}},
						})
					}
				}
				state.apply(read)
			}
			return findings
		},
	}
}

// ydbReplicationDroppedWithoutFailoverRule reports a DROP ASYNC REPLICATION
// without CASCADE of a replication the directory has not failed over. YDB
// keeps the replica tables of such a replication, and keeps them read-only for
// good. Measured on 26.2.1.14 and 25.1.4.7 after a plain DROP of a running
// replication:
//
//	UPSERT INTO the replica     Can't execute write tx at replicated table
//	ALTER TABLE ... ADD COLUMN  path is an async replica table
//	ALTER TABLE ... RENAME TO   path is an async replica table
//
// while DROP TABLE takes it. A replication failed over first with `ALTER ASYNC
// REPLICATION ... SET (STATE = 'DONE', FAILOVER_MODE = 'FORCE')` leaves
// ordinary writable tables behind a plain DROP, and CASCADE drops them, which
// DS107 judges. The failover is read from the directory: the up migrations
// before the analyzed version, then the file's statements before the drop.
func ydbReplicationDroppedWithoutFailoverRule(h history) rule {
	return rule{
		published: published{
			Code:          "YD115",
			Title:         "async replication dropped without failover",
			Severity:      protocol.LintRule_WARNING,
			AppliesToDown: true,
		},
		File: func(file checkedFile) []Violation {
			state := h.before(file.Name)
			var findings []Violation
			for i := range file.Statements {
				stmt := &file.Statements[i]
				read := yqlddl.Read(stmt.SQL)
				if read.Kind == yqlddl.DropAsyncReplication && read.Name != "" && !read.Cascade &&
					!state.failedOver[read.Name] {
					findings = append(findings, Violation{
						Statement: i,
						Message: fmt.Sprintf("DROP ASYNC REPLICATION %s keeps its replica tables, and YDB keeps a "+
							"replica of a replication that was not failed over read-only for good (path is an async "+
							"replica table); fail it over first with ALTER ASYNC REPLICATION %s SET (STATE = 'DONE', "+
							"FAILOVER_MODE = 'FORCE') to keep writable tables, or drop it with CASCADE to drop them",
							read.Name, read.Name),
						Subjects: []Subject{{Kind: protocol.LintSubject_TABLE, Name: read.Name}},
					})
				}
				state.apply(read)
			}
			return findings
		},
	}
}

// ydbUndeclaredColumnFamilyRule reports an ALTER TABLE action that names a
// column family the table does not have. YDB does not refuse it: it creates
// the family with its own settings -- no compression, the database's default
// pool -- and the statement succeeds. Measured on 25.1.4.7 and 26.2.1.14,
// each of these created family `nope` on a table that had none:
//
//	ALTER TABLE t ALTER COLUMN c SET FAMILY nope
//	ALTER TABLE t ALTER FAMILY nope SET COMPRESSION "lz4"
//	ALTER TABLE t ADD COLUMN x Int32 FAMILY nope
//
// so a misspelled family moves a column's data into a new family nobody
// declared, and YQL has no DROP FAMILY to take it back. A CREATE TABLE that
// names an undeclared family is refused (`Unknown family nope`), and ADD
// FAMILY in the same statement declares one, so neither is reported. The
// table's families are read from the directory's own migrations, as YD104
// reads its indexes, and a table the directory did not create is not judged.
func ydbUndeclaredColumnFamilyRule(h history) rule {
	return rule{
		published: published{
			Code:     "YD119",
			Title:    "column family the table does not have",
			Severity: protocol.LintRule_WARNING,
		},
		File: func(file checkedFile) []Violation {
			state := h.before(file.Name)
			var findings []Violation
			for i := range file.Statements {
				stmt := &file.Statements[i]
				read := yqlddl.Read(stmt.SQL)
				if read.Kind != yqlddl.AlterTable {
					state.apply(read)
					continue
				}
				table := state.table(read.Name)
				for _, action := range read.Actions {
					if family, undeclared := table.undeclaredFamily(action); undeclared {
						findings = append(findings, Violation{
							Statement: i,
							Message: fmt.Sprintf(
								"%s has no column family %s, and YDB does not refuse the statement: it creates %s "+
									"with its own settings, no compression and the database's default pool, and YQL "+
									"has no DROP FAMILY; add the family with ADD FAMILY first, or name one the table has",
								read.Name, family, family),
							Subjects: []Subject{{Kind: protocol.LintSubject_TABLE, Name: read.Name}},
						})
					}
					table = table.applyAction(action)
				}
				state.store(read, table)
			}
			return findings
		},
	}
}

// undeclaredFamily names the column family an ALTER TABLE action uses that
// the table does not have, and reports whether there is one. A table whose
// families the directory did not declare has none to judge against.
func (t ydbTable) undeclaredFamily(action yqlddl.Action) (string, bool) {
	var family string
	switch action.Kind {
	case yqlddl.AlterFamily, yqlddl.SetColumnFamily:
		family = action.Family
	case yqlddl.AddColumn:
		family = action.Column.Family
	default:
		return "", false
	}
	if !t.familiesKnown || family == "" || slices.Contains(t.families, family) {
		return "", false
	}
	return family, true
}

// ydbVectorIndexStaleRule reports a statement that writes rows into a table
// holding a vector index, on a target without
// [vectorIndexMaintainedOnWrite]. Measured on local-ydb 25.1.4.7 and
// 25.2.1.24, with a vector index built over five rows: an upsert, an
// update and a delete afterwards are each accepted, a full scan sees them, and
// a search through the index answers as the table stood when the index was
// built. 25.3.1.25 and later answer each write through the index.
//
// The index is known from the directory itself, as YD104 knows a table's
// indexes; a table created outside it reports nothing. The rule reads the up
// migrations only: a down migration's writes meet the same index, and the
// state before one is not the state the directory's own history builds.
func ydbVectorIndexStaleRule(h history) rule {
	return rule{
		published: published{
			Code:     "YD131",
			Title:    "rows written past a vector index that does not take them",
			Severity: protocol.LintRule_WARNING,
		},
		File: func(file checkedFile) []Violation {
			caps := file.Target
			if !caps.Has(yqlddl.VectorIndexes) || caps.Has(vectorIndexMaintainedOnWrite) {
				return nil
			}
			state := h.before(file.Name)
			var findings []Violation
			for i := range file.Statements {
				stmt := &file.Statements[i]
				if table, writes := yqlddl.WrittenTable(stmt.SQL); writes {
					if index, held := state.table(table).vectorIndex(); held {
						findings = append(findings, Violation{
							Statement: i,
							Message: fmt.Sprintf("this statement writes rows into %s, whose vector index %s does not take "+
								"them on this target (needs target capability %s): a search through the index answers as "+
								"the table stood when the index was built; write the rows before the index is added, or "+
								"drop and add the index after them. Rows the application writes later meet the same, and "+
								"YDB 25.3 and later keep the index current",
								table, index, vectorIndexMaintainedOnWrite),
							Subjects: []Subject{{Kind: protocol.LintSubject_TABLE, Name: table}},
						})
					}
				}
				state.apply(yqlddl.Read(stmt.SQL))
			}
			return findings
		},
	}
}

// ydbCommentLeftBehindRule reports a DROP COLUMN, a DROP INDEX or a RENAME
// INDEX of a column or an index that carries a comment, when no statement
// after it in the file removes or moves the comment. YDB keeps a comment as a
// table attribute keyed by the column's or the index's name (see
// [ydbcomment.Key]), and keeps the attribute after the column or the index
// is dropped or renamed, measured on 25.1.4.7 and 26.2.1.14: it then takes up
// the table's 10240 bytes of attributes, and a column or an index created
// later under the old name reads it as its own. Ptah's plans remove and move
// it themselves; a hand-written migration has to.
func ydbCommentLeftBehindRule(h history) rule {
	return rule{
		published: published{
			Code:     "YD150",
			Title:    "comment left behind by a drop or a rename",
			Severity: protocol.LintRule_WARNING,
		},
		File: func(file checkedFile) []Violation {
			state := h.before(file.Name)
			type leftBehind struct {
				finding Violation
				table   string
				key     string
			}
			var pending []leftBehind
			for i := range file.Statements {
				stmt := &file.Statements[i]
				if query, recognized, err := ydbcomment.Recognize(stmt.SQL); recognized && err == nil {
					pending = slices.DeleteFunc(pending, func(left leftBehind) bool {
						return left.table == query.Path && left.key == query.Key()
					})
					state.applyComment(stmt.SQL)
					continue
				}
				read := yqlddl.Read(stmt.SQL)
				if read.Kind != yqlddl.AlterTable {
					state.apply(read)
					continue
				}
				table := state.table(read.Name)
				for _, action := range read.Actions {
					key, message := commentLeftBehind(read.Name, action, table.comments)
					if key != "" {
						pending = append(pending, leftBehind{table: read.Name, key: key, finding: Violation{
							Statement: i,
							Message:   message,
							Subjects: []Subject{{
								Kind: protocol.LintSubject_TABLE, Name: read.Name,
							}},
						}})
					}
					table = table.applyAction(action)
				}
				state.store(read, table)
			}
			findings := make([]Violation, 0, len(pending))
			for _, left := range pending {
				findings = append(findings, left.finding)
			}
			return findings
		},
	}
}

// commentLeftBehind returns the attribute a DROP COLUMN, a DROP INDEX or a
// RENAME INDEX of table leaves behind, from the comments the table holds,
// and what to do about it; it returns an empty key for any other action, and
// for an object without a comment.
func commentLeftBehind(table string, action yqlddl.Action, comments map[string]string) (key, message string) {
	quotedTable := quoteName(table)
	switch action.Kind {
	case yqlddl.DropColumn:
		key = ydbcomment.Key(ydbcomment.Column, action.Column.Name)
		message = fmt.Sprintf("DROP COLUMN %s leaves its comment on the table as the attribute %s, which YDB keeps "+
			"after the column is gone and a column added later under the name reads as its own; remove it with "+
			"COMMENT ON COLUMN %s.%s IS NULL", action.Column.Name, key, quotedTable,
			quoteName(action.Column.Name))
	case yqlddl.DropIndex:
		key = ydbcomment.Key(ydbcomment.Index, action.Index.Name)
		message = fmt.Sprintf("DROP INDEX %s leaves its comment on the table as the attribute %s, which YDB keeps "+
			"after the index is gone and an index added later under the name reads as its own; remove it with "+
			"COMMENT ON INDEX %s ON %s IS NULL", action.Index.Name, key, quoteName(action.Index.Name),
			quotedTable)
	case yqlddl.RenameIndex:
		key = ydbcomment.Key(ydbcomment.Index, action.Index.Name)
		message = fmt.Sprintf("RENAME INDEX %s TO %s leaves the index's comment under the old name, as the "+
			"attribute %s; move it with COMMENT ON INDEX %s ON %s and COMMENT ON INDEX %s ON %s IS NULL",
			action.Index.Name, action.NewName, key, quoteName(action.NewName), quotedTable,
			quoteName(action.Index.Name), quotedTable)
	default:
		return "", ""
	}
	if comments[key] == "" {
		return "", ""
	}
	return key, message
}
