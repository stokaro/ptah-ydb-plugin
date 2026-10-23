package tablecreate

import (
	"fmt"
	"slices"

	native "github.com/stokaro/ptah-ydb-plugin/internal/nativeprotocol"
	"github.com/stokaro/ptah-ydb-plugin/schema"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/stokaro/ptah/provider-sdk/provider"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

// alteration is one planned change of an existing table: its column families,
// and the host column steps this service replaces.
type alteration struct {
	index     int
	table     *protocol.PlanningTable
	change    familyChange
	operation *protocol.NormalizedValue
	id        *protocol.StepIdentity
	claims    []*columnClaim
}

func stepKey(id *protocol.StepIdentity) string { return id.GetOwner() + "\x00" + id.GetName() }

// planAlteration reads the families and column placements a captured ALTER
// carries on both sides and returns the change that gives the table what the
// declaration states. A non-empty reason refuses the whole request.
func planAlteration(owner *protocol.Reference, table *protocol.PlanningTable) (familyChange, string, error) {
	desired, err := facetEnvelopes(owner, table.Desired.GetFeatures().GetFacets(), protocol.ValueInput_DESIRED)
	if err != nil {
		return familyChange{}, "", err
	}
	observed, err := facetEnvelopes(owner, table.Current.GetFeatures().GetFacets(), protocol.ValueInput_OBSERVED)
	if err != nil {
		return familyChange{}, "", err
	}
	in := familyInput{placement: map[string]string{}, heldPlacement: map[string]string{}, existing: map[string]bool{}}
	var intent native.RowTableIntent
	if env := desired.table; env != nil {
		if err := protojson.Unmarshal(env.Payload, &intent); err != nil {
			return familyChange{}, "", err
		}
	}
	in.desired = intent.ColumnFamilies
	for name, env := range desired.columns {
		var column native.ColumnIntent
		if err := protojson.Unmarshal(env.Payload, &column); err != nil {
			return familyChange{}, "", err
		}
		in.placement[name] = column.Family
	}
	for _, column := range table.Desired.GetColumns() {
		in.columns = append(in.columns, column.Name)
		if column.Primary {
			in.key = append(in.key, column.Name)
		}
	}
	in.key = append(in.key, table.Desired.GetPrimaryKey()...)
	in.key = append(in.key, intent.PrimaryKey...)
	for _, column := range table.Current.GetColumns() {
		in.existing[column.Name] = true
	}
	var held native.DescribeTableResult
	if env := observed.table; env != nil {
		if err := protojson.Unmarshal(env.Payload, &held); err != nil {
			return familyChange{}, "", err
		}
	}
	in.held = held.ColumnFamilies
	for name, env := range observed.columns {
		var column native.ColumnMeta
		if err := protojson.Unmarshal(env.Payload, &column); err != nil {
			return familyChange{}, "", err
		}
		in.heldPlacement[name] = column.Family
	}
	stated := len(in.desired) != 0 || len(in.placement) != 0
	if stated && observed.table == nil {
		// Without the table's own read, a family the table already holds would look
		// missing, and adding it would fail after dispatch.
		return familyChange{}, "The table's current column families were not captured", nil
	}
	for name := range in.placement {
		if !slices.Contains(in.columns, name) {
			return familyChange{}, fmt.Sprintf("Column properties name column %q, which the table does not declare", name), nil
		}
	}
	change, reason := planFamilies(in)
	if reason != "" {
		return familyChange{}, reason, nil
	}
	return change, "", nil
}

// capturedFacets are the table and column facets one side of a table carries.
type capturedFacets struct {
	table   *protocol.Envelope
	columns map[string]*protocol.Envelope
}

// facetEnvelopes returns the table and column properties a captured state
// carries for owner. A facet excluded for this target, a facet bound to other
// targets and a kind this service does not own state nothing here.
func facetEnvelopes(owner *protocol.Reference, state *protocol.ComparisonState, representation protocol.ValueInput_Representation) (capturedFacets, error) {
	result := capturedFacets{columns: map[string]*protocol.Envelope{}}
	if state == nil {
		return result, nil
	}
	if state.Representation != representation {
		return result, fmt.Errorf("YDB table alteration requires %s facets", representation)
	}
	for _, record := range state.Records {
		if record.Kind != schema.TableProperties && record.Kind != schema.ColumnProperties {
			continue
		}
		if (len(record.Targets) > 0 && !slices.Contains(record.Targets, "ydb")) || record.Value == nil {
			continue
		}
		value, err := normalizedFacet(record, representation)
		if err != nil {
			return result, err
		}
		identity := proto.CloneOf(owner)
		if value.Kind == schema.ColumnProperties {
			identity.Kind, identity.Path, identity.Name = "column", []string{owner.Name}, value.Owner.Name
		}
		if !proto.Equal(identity, value.Owner) {
			return result, fmt.Errorf("YDB alteration facet belongs to a different owner")
		}
		// The SDK refuses two facets of one kind and subject before this runs.
		if value.Kind == schema.TableProperties {
			result.table = value
			continue
		}
		result.columns[value.Owner.Name] = value
	}
	return result, nil
}

// alterStepDependencies orders the family operation against the column steps of
// the same table, whoever carries them out. The families change after every
// column addition it moves a column into, since a column moves only once it
// exists, and before every column removal, so a removal never meets a family the
// plan is still reshaping. A column the plan removes is never named by the
// alteration. A claimed step is named by its native replacement, so the edge
// joins two steps of this service; any other step keeps its host identity.
func alterStepDependencies(item alteration, steps []*protocol.CommonPlanningStep) []*protocol.PlanningDependency {
	moved := map[string]bool{}
	for _, move := range item.change.move {
		moved[move.Name] = true
	}
	replacement := map[string]*protocol.StepIdentity{}
	for _, claim := range item.claims {
		replacement[stepKey(claim.source.Id)] = claim.id
	}
	identity := func(step *protocol.CommonPlanningStep) *protocol.StepIdentity {
		if claimed := replacement[stepKey(step.Id)]; claimed != nil {
			return proto.CloneOf(claimed)
		}
		return proto.CloneOf(step.Id)
	}
	var dependencies []*protocol.PlanningDependency
	for _, step := range steps {
		if step.Parent == nil || provider.CompareIdentities(step.Parent, item.table.Subject) != 0 {
			continue
		}
		switch {
		case step.AddedColumn != nil && moved[step.AddedColumn.Name]:
			dependencies = append(dependencies, &protocol.PlanningDependency{Before: identity(step), After: proto.CloneOf(item.id)})
		case step.AddedColumn == nil && dropsColumn(step):
			dependencies = append(dependencies, &protocol.PlanningDependency{Before: proto.CloneOf(item.id), After: identity(step)})
		}
	}
	return dependencies
}

// dropsColumn reports whether a host step removes a column.
func dropsColumn(step *protocol.CommonPlanningStep) bool {
	return slices.ContainsFunc(step.GetMetadata().GetEffects(), func(effect *protocol.PlanningEffect) bool {
		return effect.Action == protocol.PlanningEffect_DROP && effect.GetSubject().GetKind() == "column"
	})
}
