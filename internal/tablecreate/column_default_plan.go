package tablecreate

import (
	"fmt"
	"slices"

	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"google.golang.org/protobuf/proto"

	"github.com/stokaro/ptah-ydb-plugin/internal/featureflags"
	"github.com/stokaro/ptah-ydb-plugin/schema"
)

type defaultStep struct {
	name      string
	operation *protocol.NormalizedValue
}

func planDefaults(owner *protocol.Reference, table *protocol.PlanningTable, caps map[string]bool) ([]defaultStep, string, error) {
	columns, err := compileDefinitionColumns(owner, table.Desired, caps)
	if err != nil {
		return nil, err.Error(), nil
	}
	observed, err := facetEnvelopes(owner, table.Current.GetFeatures().GetFacets(), protocol.ValueInput_OBSERVED)
	if err != nil {
		return nil, "", err
	}
	var result []defaultStep
	for _, column := range columns {
		at := slices.IndexFunc(table.Current.Columns, func(c *protocol.ObservedColumn) bool { return c.Name == column.Name })
		if at < 0 {
			continue
		}
		desired, err := defaultText(column.GetFromLiteral())
		if err != nil {
			return nil, "", err
		}
		if table.Current.Columns[at].GetDefaultExpression() == desired {
			continue
		}
		if !caps["alter_column_default"] {
			return nil, fmt.Sprintf("Changing the default of column %q requires alter_column_default, unavailable on this YDB target%s", column.Name,
				featureflags.Clause("alter_column_default")), nil
		}
		captured := observed.columns[column.Name]
		if captured == nil {
			return nil, "A column default change requires the complete native column observation", nil
		}
		held, err := capturedColumn(table.Current.Columns[at], captured)
		if err != nil {
			return nil, err.Error(), nil
		}
		operation, err := CaptureDefault("pending", owner, &DefaultAlteration{Held: held, Set: column.GetFromLiteral()})
		if err != nil {
			return nil, err.Error(), nil
		}
		result = append(result, defaultStep{name: column.Name, operation: operation})
	}
	return result, "", nil
}

func (d defaultStep) plan(table *protocol.ComparisonIdentity, number int) *protocol.PlanningStep {
	return columnAlterStep(fmt.Sprintf("column-default/%06d", number), table, d.name, d.operation)
}

// columnAlterStep is the step named name that carries out operation on the
// column called column of table: it reads the table and alters the column.
func columnAlterStep(name string, table *protocol.ComparisonIdentity, column string, operation *protocol.NormalizedValue) *protocol.PlanningStep {
	id := &protocol.StepIdentity{Owner: schema.ProviderID, Name: name}
	operation.Id = id.Name
	subject := ColumnSubject(table, column)
	facts := operation.OperationSemantics
	return &protocol.PlanningStep{Id: id, Operation: &protocol.PlanningOperation{Role: protocol.NativeOperation_STATEMENT, Value: operation},
		Metadata: &protocol.PlanningMetadata{Transaction: protocol.PlanningMetadata_FORBIDDEN, Impact: facts.Impact, Reason: facts.Reason, Effects: []*protocol.PlanningEffect{
			{Subject: proto.CloneOf(table), Action: protocol.PlanningEffect_READ}, {Subject: subject, Action: protocol.PlanningEffect_ALTER},
		}}}
}
