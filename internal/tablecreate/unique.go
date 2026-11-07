package tablecreate

import (
	"slices"
	"strings"

	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/stokaro/ptah/provider-sdk/provider"
)

// lowerUniqueness makes the YDB indexes that hold a table's UNIQUE
// declarations. The host can hand a complete source table to offline render
// before its comparison has lowered constraints into indexes. A table that
// comparison already lowered has no UNIQUE declarations, so both routes use
// the same native index compiler below.
func lowerUniqueness(table *protocol.DeclaredTable, key []string) (*protocol.DeclaredTable, error) {
	result := provider.CloneSharingModels(table)
	result.Constraints = nil
	for _, constraint := range table.GetConstraints() {
		if !strings.EqualFold(strings.TrimSpace(constraint.GetType()), "UNIQUE") {
			result.Constraints = append(result.Constraints, constraint)
			continue
		}
		index, err := uniqueConstraintIndex(table, key, constraint)
		if err != nil {
			return nil, err
		}
		if index != nil {
			result.Indexes = append(result.Indexes, index)
		}
	}
	for _, column := range result.GetColumns() {
		if !column.GetUnique() {
			continue
		}
		column.Unique = false
		if uniqueIsKey([]string{column.GetName()}, key) {
			continue
		}
		result.Indexes = append(result.Indexes, &protocol.DeclaredIndex{Name: uniqueIndexName(table.GetName(), "", []string{column.GetName()}),
			Fields: []string{column.GetName()}, Unique: true})
	}
	return result, nil
}

// effectiveUniqueKey uses the same key source that creation uses. A source
// may only mark primary columns, leaving their order to declaration order.
func effectiveUniqueKey(table *protocol.DeclaredTable, declared, native []string) []string {
	if len(declared) != 0 {
		return declared
	}
	if len(native) != 0 {
		return native
	}
	var marked []string
	for _, column := range table.GetColumns() {
		if column.GetPrimary() {
			marked = append(marked, column.GetName())
		}
	}
	return marked
}

func uniqueConstraintIndex(table *protocol.DeclaredTable, key []string, constraint *protocol.DeclaredConstraint) (*protocol.DeclaredIndex, error) {
	// The host renders a constraint comment as a separate COMMENT ON INDEX
	// step after the table exists. It is not part of the native CREATE request.
	if err := only(constraint, "name", "type", "table", "columns", "include_columns", "nulls_distinct", "deferrable", "not_enforced", "requires", "comment"); err != nil {
		return nil, err
	}
	name := uniqueIndexName(table.GetName(), constraint.GetName(), constraint.GetColumns())
	if !namesTable(constraint.GetTable(), table) {
		return nil, unsupported("UNIQUE constraint %q on table %q names another table", name, table.GetName())
	}
	if constraint.GetDeferrable() {
		return nil, unsupported("UNIQUE constraint %q on table %q is deferrable, which requires target capability deferrable_constraints, unavailable on this YDB target", name, table.GetName())
	}
	if constraint.GetNotEnforced() {
		return nil, unsupported("UNIQUE constraint %q on table %q: it is NOT ENFORCED, and YDB enforces every unique index", name, table.GetName())
	}
	if constraint.NullsDistinct != nil && !constraint.GetNullsDistinct() {
		return nil, unsupported("index %q treats NULLs as equal; YDB has no unique_nulls_distinct_clause", name)
	}
	if len(constraint.GetColumns()) == 0 {
		return nil, unsupported("UNIQUE constraint %q on table %q names no columns", name, table.GetName())
	}
	if uniqueIsKey(constraint.GetColumns(), key) {
		return nil, nil
	}
	return &protocol.DeclaredIndex{Name: name, Fields: slices.Clone(constraint.GetColumns()),
		IncludeColumns: slices.Clone(constraint.GetIncludeColumns()), Unique: true}, nil
}

func uniqueIndexName(table, declared string, columns []string) string {
	if strings.TrimSpace(declared) != "" {
		return declared
	}
	return table + "_" + strings.Join(columns, "_") + "_key"
}

func uniqueIsKey(columns, key []string) bool {
	if len(key) == 0 || len(columns) != len(key) {
		return false
	}
	left, right := slices.Clone(columns), slices.Clone(key)
	slices.Sort(left)
	slices.Sort(right)
	return slices.Equal(left, right)
}
