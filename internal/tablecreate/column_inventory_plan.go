package tablecreate

import (
	"fmt"
	"slices"
	"strings"

	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/stokaro/ptah/provider-sdk/provider"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	"github.com/stokaro/ptah-ydb-plugin/internal/columnsemantics"
	native "github.com/stokaro/ptah-ydb-plugin/internal/nativeprotocol"
	"github.com/stokaro/ptah-ydb-plugin/internal/ydbtype"
	"github.com/stokaro/ptah-ydb-plugin/schema"
)

type inventoryStep struct {
	name      string
	add       bool
	operation *protocol.NormalizedValue
	source    *protocol.CommonPlanningStep
}

func planInventory(owner *protocol.Reference, table *protocol.PlanningTable, claims []*columnClaim, steps []*protocol.CommonPlanningStep, caps map[string]bool) ([]inventoryStep, string, error) {
	columns, err := compileDefinitionColumns(owner, table.Desired, caps)
	if err != nil {
		return nil, err.Error(), nil
	}
	observed, err := facetEnvelopes(owner, table.Current.GetFeatures().GetFacets(), protocol.ValueInput_OBSERVED)
	if err != nil {
		return nil, "", err
	}
	claimed := func(name string) bool {
		return slices.ContainsFunc(claims, func(c *columnClaim) bool { return c.name == name })
	}
	var result []inventoryStep
	for _, column := range columns {
		if claimed(column.Name) || slices.ContainsFunc(table.Current.Columns, func(c *protocol.ObservedColumn) bool { return c.Name == column.Name }) {
			continue
		}
		if column.GetFromSequence() != nil {
			return nil, fmt.Sprintf("adding column %q to table %q: YDB adds no Serial column to an existing table (`Column addition with serial data type is unsupported`)",
				column.Name, tableLabel(owner)), nil
		}
		addition := proto.CloneOf(column)
		addition.Family = ""
		change := &ColumnInventory{Add: addition}
		if err := change.CheckCapabilities(caps); err != nil {
			return nil, err.Error(), nil
		}
		operation, err := CaptureInventory("pending", owner, change)
		if err != nil {
			return nil, fmt.Sprintf("Column %q addition: %v", column.Name, err), nil
		}
		result = append(result, inventoryStep{name: column.Name, add: true, operation: operation})
	}
	for _, column := range table.Current.Columns {
		if claimed(column.Name) || slices.ContainsFunc(table.Desired.Columns, func(c *protocol.DeclaredColumn) bool { return c.Name == column.Name }) {
			continue
		}
		value, err := heldColumnFacet(owner, table, observed, column, caps)
		if err != nil {
			return nil, "", err
		}
		captured, err := capturedColumn(column, value)
		if err != nil {
			return nil, err.Error(), nil
		}
		operation, err := CaptureInventory("pending", owner, &ColumnInventory{Drop: captured})
		if err != nil {
			return nil, err.Error(), nil
		}
		result = append(result, inventoryStep{name: column.Name, operation: operation})
	}
	return result, bindInventorySources(table, result, steps), nil
}

func bindInventorySources(table *protocol.PlanningTable, result []inventoryStep, steps []*protocol.CommonPlanningStep) string {
	for i := range result {
		for _, step := range steps {
			if step.Parent == nil || provider.CompareIdentities(step.Parent, table.Subject) != 0 {
				continue
			}
			name, action, exact := columnFootprint(step, table.Subject)
			if !exact || name != result[i].name {
				continue
			}
			want := protocol.PlanningEffect_DROP
			if result[i].add {
				want = protocol.PlanningEffect_CREATE
			}
			if action != want || result[i].source != nil {
				return "The host column steps disagree with the captured inventory change"
			}
			result[i].source = step
		}
	}
	return ""
}

// heldColumnFacet is the native observation of the column common that the
// read of table holds. A read that states its column facets completely and
// carries none for the column describes a column with no native setting of
// its own, no family and a default the common column states whole, and
// heldColumnFacet returns that observation: the one a declaration of the
// column with its common default as the native literal converts to. The host
// builds such a side from a schema file for a diff of two files, where only a
// column whose declaration states a native setting carries a facet; the
// host's embedded engine planned a change of such a column offline. A read
// that does not state its column facets, or a common default no literal
// reads back as, leaves the observation nil, which the callers refuse.
func heldColumnFacet(owner *protocol.Reference, table *protocol.PlanningTable, observed capturedFacets, common *protocol.ObservedColumn, caps map[string]bool) (*protocol.Envelope, error) {
	if captured := observed.columns[common.GetName()]; captured != nil {
		return captured, nil
	}
	if !FacetKnown(table.GetCurrent().GetFeatures().GetFacets().GetCoverage(), schema.ColumnProperties, ColumnSubject(table.GetSubject(), common.GetName()), ObservedSide) {
		return nil, nil
	}
	intent := &native.ColumnIntent{}
	if text := common.GetDefaultExpression(); text != "" {
		value, ok := ydbtype.LiteralValue(text)
		if !ok {
			return nil, nil
		}
		literal, err := columnsemantics.NativeDefault(&protocol.DeclaredColumn{Name: common.GetName(), Type: heldType(common), Nullable: common.GetIsNullable() == "YES",
			DefaultSet: true, DefaultValue: "'" + strings.ReplaceAll(value, "'", "''") + "'"}, heldType(common), caps)
		// A common default no literal reads back as leaves the observation
		// unknown, which the callers refuse; the error says no more.
		if err != nil || literal == nil {
			return nil, nil
		}
		intent.DefaultValue = &native.ColumnIntent_Literal{Literal: literal}
	}
	column := proto.CloneOf(owner)
	column.Kind, column.Path, column.Name = "column", []string{owner.GetName()}, common.GetName()
	declared, err := schema.CaptureColumnIntent(column, intent)
	if err != nil {
		return nil, err
	}
	return schema.ConvertColumn(declared, protocol.ValueInput_DESIRED, protocol.ValueInput_OBSERVED)
}

func capturedColumn(common *protocol.ObservedColumn, value *protocol.Envelope) (*native.ColumnMeta, error) {
	if value == nil {
		return nil, fmt.Errorf("column %q requires its native observation", common.Name)
	}
	if common.IsNullable != "YES" && common.IsNullable != "NO" {
		return nil, fmt.Errorf("column %q requires known nullability", common.Name)
	}
	result := &native.ColumnMeta{}
	if err := protojson.Unmarshal(value.Payload, result); err != nil {
		return nil, err
	}
	var err error
	result.Name = common.Name
	result.Type, err = schema.StorageType(heldType(common))
	if err != nil {
		return nil, err
	}
	if common.IsNullable == "YES" {
		result.Type = schema.Optional(result.Type)
	}
	text, err := defaultText(result.GetFromLiteral())
	if err != nil {
		return nil, err
	}
	if text != common.GetDefaultExpression() {
		return nil, fmt.Errorf("the common and native defaults of column %q disagree", common.Name)
	}
	return result, nil
}

func (i inventoryStep) plan(table *protocol.ComparisonIdentity, number int) *protocol.PlanningStep {
	id := &protocol.StepIdentity{Owner: schema.ProviderID, Name: fmt.Sprintf("column-inventory/%06d", number)}
	i.operation.Id = id.Name
	subject := proto.CloneOf(table)
	subject.Kind = "column"
	subject.Parent = proto.CloneOf(table.Name)
	subject.Name = &protocol.ComparisonPart{Source: i.name, Normalized: i.name}
	action := protocol.PlanningEffect_DROP
	if i.add {
		action = protocol.PlanningEffect_CREATE
	}
	facts := i.operation.OperationSemantics
	return &protocol.PlanningStep{Id: id, Operation: &protocol.PlanningOperation{Role: protocol.NativeOperation_STATEMENT, Value: i.operation},
		Metadata: &protocol.PlanningMetadata{Transaction: protocol.PlanningMetadata_FORBIDDEN, Impact: facts.Impact, Reason: facts.Reason,
			Effects: []*protocol.PlanningEffect{
				{Subject: proto.CloneOf(table), Action: protocol.PlanningEffect_READ}, {Subject: subject, Action: action},
			}}}
}
