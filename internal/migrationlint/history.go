package migrationlint

import (
	"cmp"
	"maps"
	"slices"
	"strings"

	"github.com/stokaro/ptah-ydb-plugin/internal/yqlddl"
)

// history is, by file name, the YDB schema the directory's own up migrations
// leave before each migration file runs: for an up file, the up migrations of
// lower versions; for a down file, those and the up migration of its own
// version, which the down half undoes. Files are read in version order, as the
// migrator applies them. A file of neither half, or without a version, starts
// from nothing.
type history map[string]*ydbSchema

// newHistory reads the history of a run's files.
func newHistory(files []File) history {
	ups := make([]int, 0, len(files))
	for i := range files {
		if files[i].Up && files[i].Version > 0 && !files[i].Repeatable {
			ups = append(ups, i)
		}
	}
	slices.SortStableFunc(ups, func(a, b int) int { return cmp.Compare(files[a].Version, files[b].Version) })
	h := make(history, len(files))
	state := (*ydbSchema)(nil).clone()
	after := make(map[int64]*ydbSchema, len(ups))
	for _, index := range ups {
		file := &files[index]
		h[file.Name] = state.clone()
		for i := range file.Statements {
			state.apply(yqlddl.Read(file.Statements[i].SQL))
		}
		after[file.Version] = state.clone()
	}
	for i := range files {
		if !files[i].Down || files[i].Version <= 0 {
			continue
		}
		h[files[i].Name] = stateThrough(ups, files, after, files[i].Version)
	}
	return h
}

// stateThrough is the schema after every up migration whose version is at most
// version.
func stateThrough(ups []int, files []File, after map[int64]*ydbSchema, version int64) *ydbSchema {
	var latest *ydbSchema
	for _, index := range ups {
		if files[index].Version > version {
			break
		}
		latest = after[files[index].Version]
	}
	return latest
}

// columns says what a replaced table holds just before each statement of the
// file named name runs: the file's starting schema with the statements before
// it applied.
func (h history) columns(name string, statements []Statement) columnsBefore {
	state := h[name].clone()
	applied := 0
	return func(index int, wanted string) ([]string, bool) {
		for ; applied < index; applied++ {
			state.apply(yqlddl.Read(statements[applied].SQL))
		}
		for _, path := range slices.Sorted(maps.Keys(state.tables)) {
			if table := state.tables[path]; strings.EqualFold(path, wanted) && table.columnsKnown {
				return table.columns, true
			}
		}
		return nil, false
	}
}
