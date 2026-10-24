package migrationlint

import (
	"slices"

	"github.com/stokaro/ptah-ydb-plugin/internal/yqlddl"
)

// ydbSchema is the part of a YDB schema the splitter reads, as the
// directory's own statements leave it.
type ydbSchema struct {
	tables map[string]ydbTable
}

// ydbTable is what the splitter reads about one table.
type ydbTable struct {
	// columns are the table's columns, in order, when columnsKnown: a table
	// the directory created has them all, and one it only altered does not.
	columns      []string
	columnsKnown bool
}

func (s *ydbSchema) clone() *ydbSchema {
	cloned := &ydbSchema{tables: make(map[string]ydbTable)}
	if s == nil {
		return cloned
	}
	for name, table := range s.tables {
		cloned.tables[name] = ydbTable{columns: slices.Clone(table.columns), columnsKnown: table.columnsKnown}
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
		s.tables[read.Name] = ydbTable{columns: columns, columnsKnown: true}
	case yqlddl.AlterTable:
		table := s.table(read.Name)
		for _, action := range read.Actions {
			table = table.applyAction(action)
		}
		s.store(read, table)
	case yqlddl.DropTable:
		delete(s.tables, read.Name)
	}
}

// applyAction returns the table as one ALTER TABLE action leaves it.
func (t ydbTable) applyAction(action yqlddl.Action) ydbTable {
	switch action.Kind {
	case yqlddl.AddColumn:
		t.columns = append(slices.Clone(t.columns), action.Column.Name)
	case yqlddl.DropColumn:
		t.columns = slices.DeleteFunc(slices.Clone(t.columns), func(name string) bool { return name == action.Column.Name })
	}
	return t
}
