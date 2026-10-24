package migrationlint

import "slices"

// A table rebuild replaces a table with a copy that holds its rows: a new
// table is created, every row of the old one is copied into it, and the copy
// takes the old table's name while the old table is dropped. Ptah's planners
// write one where an engine cannot change a table in place, in one of two
// orders, each step a statement of its own:
//
//	SQLite:  CREATE TABLE s; INSERT INTO s ... SELECT ... FROM t; DROP TABLE t; ALTER TABLE s RENAME TO t
//	YDB:     CREATE TABLE s; INSERT INTO s ... SELECT ... FROM t; ALTER TABLE t RENAME TO r;
//	         ALTER TABLE s RENAME TO t; DROP TABLE r
//
// The rows of t are in the copy, and the copy is t once the file has run, so
// the DROP TABLE loses no row and the name deployed code reads comes back
// before the file ends. A DROP TABLE outside that shape is a drop.
//
// The shape is matched strictly, as consecutive statements: a write to t
// between the copy and the drop would be lost, a copy with a WHERE, a join,
// DISTINCT or anything after its FROM leaves rows behind, and a copy into a
// table other than the one created just before it is not a rebuild. Where the
// old table's columns are known, a copy that leaves one out is not a rebuild.
// The matcher is the host's internal/lintkit one, which the host's rules use
// where a split judges no rebuild.

// rebuildSteps are the statements of a file that belong to a table rebuild.
type rebuildSteps struct {
	// drops are the DROP TABLE statements that retire a replaced table whose
	// rows a rebuild copied.
	drops map[int]bool
	// renames are the ALTER TABLE ... RENAME TO statements that move a
	// replaced table aside or move its copy into place.
	renames map[int]bool
}

// columnsBefore answers what the matcher knows of a table's columns just
// before the statement at index runs, and whether it knows the table. Indexes
// arrive in increasing order.
type columnsBefore func(index int, table string) ([]string, bool)

// rebuilds finds the table rebuilds of a file's statements. columns, which may
// be nil, says what the old table holds.
func rebuilds(statements []Statement, columns columnsBefore) rebuildSteps {
	steps := rebuildSteps{drops: make(map[int]bool), renames: make(map[int]bool)}
	for i := range statements {
		if rebuild, ok := matchRebuild(statements, i, columns); ok {
			steps.drops[rebuild.drop] = true
			for _, index := range rebuild.renames {
				steps.renames[index] = true
			}
		}
	}
	return steps
}

// rebuildMatch is one rebuild: the statement that drops the replaced table and
// the renames that swap the copy in.
type rebuildMatch struct {
	drop    int
	renames []int
}

// matchRebuild reports whether the statements from index on are a rebuild, in
// either order.
func matchRebuild(statements []Statement, index int, columns columnsBefore) (rebuildMatch, bool) {
	if index+3 >= len(statements) {
		return rebuildMatch{}, false
	}
	scratch := createdTableRef(statements[index].Words)
	copied, ok := tableCopyOf(&statements[index+1])
	if scratch == "" || !ok || !sameTableRef(copied.into, scratch) || sameTableRef(copied.from, scratch) {
		return rebuildMatch{}, false
	}
	old := copied.from
	if len(lostColumns(index, old, copied, columns)) > 0 {
		return rebuildMatch{}, false
	}
	if dropped, ok := singleDroppedTable(&statements[index+2]); ok && sameTableRef(dropped, old) {
		if from, to, ok := tableRenameOf(&statements[index+3]); ok && sameTableRef(from, scratch) && sameTableRef(to, old) {
			return rebuildMatch{drop: index + 2, renames: []int{index + 3}}, true
		}
	}
	if index+4 >= len(statements) {
		return rebuildMatch{}, false
	}
	aside, replaced, ok := tableRenameOf(&statements[index+2])
	if !ok || !sameTableRef(aside, old) {
		return rebuildMatch{}, false
	}
	from, to, ok := tableRenameOf(&statements[index+3])
	if !ok || !sameTableRef(from, scratch) || !sameTableRef(to, old) {
		return rebuildMatch{}, false
	}
	if dropped, ok := singleDroppedTable(&statements[index+4]); !ok || !sameTableRef(dropped, replaced) {
		return rebuildMatch{}, false
	}
	return rebuildMatch{drop: index + 4, renames: []int{index + 2, index + 3}}, true
}

// tableCopy is an INSERT INTO ... SELECT ... FROM that copies every row of one
// table.
type tableCopy struct {
	into, from string
	// columns are the columns the INSERT names, folded; empty when it names
	// none.
	columns []string
	// star reports a SELECT * with no column list, which copies every column.
	star bool
}

// tableCopyOf reads `INSERT INTO s [(columns)] SELECT ... FROM t`, with
// nothing after the table and no DISTINCT or ALL.
func tableCopyOf(stmt *Statement) (tableCopy, bool) {
	w := stmt.Words
	if !hasWordPrefix(w, "INSERT", "INTO") {
		return tableCopy{}, false
	}
	into, j := tableRefAt(w, stmt.SourceWords, 2)
	if into.Normalized == "" {
		return tableCopy{}, false
	}
	copied := tableCopy{into: into.Normalized}
	copied.columns, j = copyColumns(w, j)
	if j >= len(w) || w[j] != "SELECT" || (j+1 < len(w) && (w[j+1] == "DISTINCT" || w[j+1] == "ALL")) {
		return tableCopy{}, false
	}
	copied.star = len(copied.columns) == 0 && j+2 < len(w) && w[j+1] == "*" && w[j+2] == "FROM"
	from, ok := copiedTable(stmt, j+1)
	if !ok {
		return tableCopy{}, false
	}
	copied.from = from
	return copied, true
}

// copyColumns reads the column list an INSERT names at w[j], folded, and
// returns the position past it.
func copyColumns(w []string, j int) ([]string, int) {
	if j >= len(w) || w[j] != "(" {
		return nil, j
	}
	var columns []string
	for j++; j < len(w) && w[j] != ")"; j++ {
		if w[j] != "," {
			columns = append(columns, normalizeIdent(w[j]))
		}
	}
	return columns, j + 1
}

// copiedTable reads the table the SELECT from w[j] on reads: the one after its
// FROM, outside any parentheses, with nothing after it.
func copiedTable(stmt *Statement, j int) (string, bool) {
	w := stmt.Words
	depth := 0
	for ; j < len(w); j++ {
		switch {
		case w[j] == "(":
			depth++
		case w[j] == ")":
			depth--
		case depth == 0 && w[j] == "FROM":
			from, next := tableRefAt(w, stmt.SourceWords, j+1)
			return from.Normalized, from.Normalized != "" && next == len(w)
		}
	}
	return "", false
}

// singleDroppedTable reads a DROP TABLE of one table.
func singleDroppedTable(stmt *Statement) (string, bool) {
	if !hasWordPrefix(stmt.Words, "DROP", "TABLE") {
		return "", false
	}
	tables, complete := droppedTablesNotCreated(stmt.Words, stmt.SourceWords, nil)
	if !complete || len(tables) != 1 {
		return "", false
	}
	return tables[0].Normalized, true
}

// tableRenameOf reads `ALTER TABLE from RENAME TO to`, the one clause of its
// statement.
func tableRenameOf(stmt *Statement) (from, to string, ok bool) {
	w := stmt.Words
	if !hasWordPrefix(w, "ALTER", "TABLE") {
		return "", "", false
	}
	table, j := tableRefAt(w, stmt.SourceWords, skipIfExists(w, 2))
	if table.Normalized == "" || j+1 >= len(w) || w[j] != "RENAME" || w[j+1] != "TO" {
		return "", "", false
	}
	target, next := tableRefAt(w, stmt.SourceWords, j+2)
	if target.Normalized == "" || next != len(w) {
		return "", "", false
	}
	return table.Normalized, target.Normalized, true
}

// sameTableRef reports whether two folded references name one table.
func sameTableRef(a, b string) bool {
	return refersToCreated(map[string]bool{b: true}, a)
}

// lostColumns returns the columns of the copied table that the copy leaves
// out, where its columns are known; none where they are not.
func lostColumns(index int, old string, copied tableCopy, columns columnsBefore) []string {
	if columns == nil || copied.star {
		return nil
	}
	known, ok := columns(index, old)
	if !ok {
		return nil
	}
	var lost []string
	for _, column := range known {
		if !slices.Contains(copied.columns, normalizeIdent(column)) {
			lost = append(lost, column)
		}
	}
	return lost
}
