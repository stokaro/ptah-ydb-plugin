package tablecreate

import (
	"fmt"

	"github.com/stokaro/ptah-ydb-plugin/internal/columnsemantics"
	"github.com/stokaro/ptah-ydb-plugin/schema"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb_Table"
)

func validateInventoryAddition(column *Ydb_Table.ColumnMeta) error {
	if err := only(column, "name", "type", "not_null", "from_literal"); err != nil {
		return err
	}
	if !validName(column.GetName()) {
		return fmt.Errorf("column addition requires its name")
	}
	kind, nullable, err := schema.ColumnType(column.Type)
	if err != nil {
		return err
	}
	if column.NotNull == nil || column.GetNotNull() == nullable {
		return fmt.Errorf("column addition requires consistent explicit nullability")
	}
	if literal := column.GetFromLiteral(); literal != nil {
		defaultType, _, err := schema.ColumnType(literal.Type)
		if err != nil || defaultType != kind {
			return fmt.Errorf("added column default differs from its storage type")
		}
		_, err = columnsemantics.Literal(literal)
		return err
	}
	if !nullable {
		return fmt.Errorf("YDB adds a NOT NULL column only with a default")
	}
	return nil
}

// CheckCapabilities refuses a default-bearing addition when the target cannot
// fill existing rows. Planning, rendering, and execution share this boundary.
func (c *ColumnInventory) CheckCapabilities(caps map[string]bool) error {
	if c.Add.GetFromLiteral() != nil && !caps["add_column_with_default"] {
		return fmt.Errorf("adding a column with a default requires add_column_with_default")
	}
	return nil
}
