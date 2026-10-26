package tablecreate

import (
	"fmt"
	"slices"

	"github.com/stokaro/ptah-ydb-plugin/internal/columnsemantics"
	"github.com/stokaro/ptah-ydb-plugin/schema"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb_Table"
)

// completeColumnAlteration refuses a parent receipt that would acknowledge an
// owned column change without an operation. Common steps can be absent because
// the host delegates the complete parent definition to this planner.
func completeColumnAlteration(owner *protocol.Reference, table *protocol.PlanningTable, claims []*columnClaim, defaults []defaultStep, inventory []inventoryStep, caps map[string]bool) string {
	columns, err := compileDefinitionColumns(owner, table.Desired, caps)
	if err != nil {
		return err.Error()
	}
	current := table.GetCurrent().GetColumns()
	for _, column := range columns {
		at := slices.IndexFunc(current, func(c *protocol.ObservedColumn) bool { return c.Name == column.Name })
		if at < 0 {
			if !plannedInventory(claims, inventory, column.Name, true) {
				return fmt.Sprintf("Column %q addition requires an owned column operation", column.Name)
			}
			continue
		}
		held := current[at]
		typ, nullable, err := schema.ColumnType(column.Type)
		if err != nil {
			return err.Error()
		}
		if heldType(held) != typ || held.IsNullable == "" || (held.IsNullable == "YES") != nullable {
			return fmt.Sprintf("Column %q definition change requires an owned column operation", column.Name)
		}
		var literal string
		if column.GetFromLiteral() != nil {
			literal, err = columnsemantics.Literal(column.GetFromLiteral())
			if err != nil {
				return err.Error()
			}
		}
		if held.GetDefaultExpression() != literal && !slices.ContainsFunc(defaults, func(d defaultStep) bool { return d.name == column.Name }) {
			return fmt.Sprintf("Column %q default change requires an owned column operation", column.Name)
		}
	}
	for _, held := range current {
		if slices.ContainsFunc(columns, func(c *Ydb_Table.ColumnMeta) bool { return c.Name == held.Name }) {
			continue
		}
		if !plannedInventory(claims, inventory, held.Name, false) {
			return fmt.Sprintf("Column %q removal requires an owned column operation", held.Name)
		}
	}
	return ""
}

func compileDefinitionColumns(owner *protocol.Reference, table *protocol.DeclaredTable, caps map[string]bool) ([]*Ydb_Table.ColumnMeta, error) {
	if IsColumnTable(table) {
		compiled, err := CompileColumnTable(owner, table, caps)
		if err != nil {
			return nil, err
		}
		return compiled.Columns, nil
	}
	compiled, err := Compile(owner, table, caps)
	if err != nil {
		return nil, err
	}
	return compiled.Columns, nil
}

func plannedInventory(claims []*columnClaim, inventory []inventoryStep, name string, add bool) bool {
	return slices.ContainsFunc(claims, func(c *columnClaim) bool { return c.name == name && c.add == add }) || slices.ContainsFunc(inventory, func(i inventoryStep) bool { return i.name == name && i.add == add })
}
