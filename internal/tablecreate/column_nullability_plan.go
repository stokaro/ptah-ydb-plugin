package tablecreate

import (
	"fmt"
	"slices"

	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"

	"github.com/stokaro/ptah-ydb-plugin/schema"
)

// nullabilityStep is the DROP NOT NULL of the column called name.
type nullabilityStep struct {
	name      string
	operation *protocol.NormalizedValue
}

// planNullability plans the DROP NOT NULL of each column table keeps whose
// declaration takes NULL where the table holds it NOT NULL, with its type
// unchanged. YDB changes it in place on 25.1.4.7 and 26.2.1.14, as the host's
// embedded engine planned it. A column made NOT NULL is left to a rebuild,
// and a column whose type changes too to the rebuild of a type change (see
// completeColumnAlteration).
func planNullability(owner *protocol.Reference, table *protocol.PlanningTable, caps map[string]bool) ([]nullabilityStep, string, error) {
	columns, err := compileDefinitionColumns(owner, table.Desired, caps)
	if err != nil {
		return nil, err.Error(), nil
	}
	observed, err := facetEnvelopes(owner, table.Current.GetFeatures().GetFacets(), protocol.ValueInput_OBSERVED)
	if err != nil {
		return nil, "", err
	}
	var result []nullabilityStep
	for _, column := range columns {
		at := slices.IndexFunc(table.Current.Columns, func(c *protocol.ObservedColumn) bool { return c.Name == column.Name })
		if at < 0 {
			continue
		}
		held := table.Current.Columns[at]
		typ, nullable, err := schema.ColumnType(column.Type)
		if err != nil {
			return nil, "", err
		}
		if !nullable || held.IsNullable != "NO" || heldType(held) != typ {
			continue
		}
		captured := observed.columns[column.Name]
		if captured == nil {
			return nil, "A column nullability change requires the complete native column observation", nil
		}
		meta, err := capturedColumn(held, captured)
		if err != nil {
			return nil, err.Error(), nil
		}
		operation, err := CaptureNullability("pending", owner, &NullabilityAlteration{Held: meta})
		if err != nil {
			return nil, err.Error(), nil
		}
		result = append(result, nullabilityStep{name: column.Name, operation: operation})
	}
	return result, "", nil
}

func (n nullabilityStep) plan(table *protocol.ComparisonIdentity, number int) *protocol.PlanningStep {
	return columnAlterStep(fmt.Sprintf("column-nullability/%06d", number), table, n.name, n.operation)
}
