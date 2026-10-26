package tablecreate

import (
	"fmt"

	native "github.com/stokaro/ptah-ydb-plugin/internal/nativeprotocol"
	"github.com/stokaro/ptah-ydb-plugin/schema"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/stokaro/ptah/provider-sdk/provider"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

// The refusals of a column-table change no statement makes in place. YDB fixes
// a table's storage kind, hash key and shard count when it creates the table,
// and copies no column table into a new one, so each needs an explicit data
// migration. The words are the host's embedded engine's.
const (
	kindChangeRefusal    = "changing between row and column storage requires an explicit data migration"
	layoutChangeRefusal  = "changing a column table's hash key or shard count requires an explicit data migration"
	columnRebuildRefusal = "column-table rebuilds require an explicit data migration"
	localIndexRefusal    = "the local indexes of a column table are not added or dropped by the provider yet"
	columnStepRefusal    = "A common step is outside the supported column table lifecycle: the provider adds and drops no column of a column table yet"
)

// columnTTLChange is an in-place change of a column table's TTL: the TTL the
// table holds and the declared one, each nil for none.
type columnTTLChange struct {
	held, set *native.ColumnTTLPolicy
}

// storageSides are the column-storage facets of one table on both sides, and
// what else the sides say about its storage.
type storageSides struct {
	desired *native.ColumnStorageIntent
	held    *native.ColumnStorageObservation
	// rowRead reports a current side read as a row table.
	rowRead bool
	// rowStated reports row-table settings in the declaration.
	rowStated bool
	// known reports a declaration whose source describes the column storage of
	// the table, so a missing facet asks for a row table.
	known bool
}

// columnTable reports whether either side of the table is a column table.
func (s storageSides) columnTable() bool { return s.desired != nil || s.held != nil }

// readStorage reads the column-storage facets a captured table carries, and
// the row-table facets of both sides.
func readStorage(owner *protocol.Reference, table *protocol.PlanningTable) (storageSides, error) {
	var result storageSides
	for _, side := range []struct {
		state          *protocol.ComparisonState
		representation protocol.ValueInput_Representation
	}{{table.Desired.GetFeatures().GetFacets(), protocol.ValueInput_DESIRED}, {table.Current.GetFeatures().GetFacets(), protocol.ValueInput_OBSERVED}} {
		for _, record := range side.state.GetRecords() {
			if record.GetValue() == nil || !forThisEngine(record.GetTargets()) {
				continue
			}
			if record.Kind != schema.ColumnStorageProperties && record.Kind != schema.TableProperties {
				continue
			}
			value, err := normalizedFacet(record, side.representation)
			if err != nil {
				return storageSides{}, err
			}
			if value.Owner.GetKind() != "table" || value.Owner.GetName() != owner.Name || value.Owner.GetSchema() != owner.Schema {
				return storageSides{}, fmt.Errorf("YDB alteration facet belongs to a different owner")
			}
			switch {
			case record.Kind == schema.TableProperties && side.representation == protocol.ValueInput_DESIRED:
				result.rowStated = true
			case record.Kind == schema.TableProperties:
				result.rowRead = true
			case side.representation == protocol.ValueInput_DESIRED:
				result.desired = &native.ColumnStorageIntent{}
				if err := protojson.Unmarshal(value.Payload, result.desired); err != nil {
					return storageSides{}, err
				}
			default:
				result.held = &native.ColumnStorageObservation{}
				if err := protojson.Unmarshal(value.Payload, result.held); err != nil {
					return storageSides{}, err
				}
			}
		}
	}
	result.known = FacetKnown(table.Desired.GetFeatures().GetFacets().GetCoverage(), schema.ColumnStorageProperties, table.Subject, true)
	return result, nil
}

// planColumnAlteration plans the ALTER of a table that is a column table on
// either side, and reports whether it is one. A column table whose storage
// already satisfies the declaration needs no step, and one whose TTL differs
// gets the TTL change. A change of storage kind, hash key or shard count is
// refused with the reason, and so is any common step of the table: the
// provider carries out none for a column table.
func planColumnAlteration(owner *protocol.Reference, table *protocol.PlanningTable, steps []*protocol.CommonPlanningStep, capabilities map[string]bool) (bool, string, *columnTTLChange, error) {
	sides, err := readStorage(owner, table)
	if err != nil || !sides.columnTable() {
		return false, "", nil, err
	}
	subject := fmt.Sprintf("table %q", tableLabel(owner))
	switch {
	case sides.desired != nil && sides.held == nil && sides.rowRead:
		return true, subject + ": " + kindChangeRefusal, nil, nil
	case sides.desired != nil && sides.held == nil:
		return true, "The table's current storage was not captured", nil, nil
	case sides.desired == nil && (sides.rowStated || sides.known):
		return true, subject + ": " + kindChangeRefusal, nil, nil
	}
	for _, step := range steps {
		if step.Parent != nil && provider.CompareIdentities(step.Parent, table.Subject) == 0 {
			return true, columnStepRefusal, nil, nil
		}
	}
	if sides.desired == nil {
		// The source could not describe the table's storage, so the table
		// keeps it.
		return true, "", nil, nil
	}
	switch {
	case !LayoutSatisfied(sides.desired, sides.held):
		return true, subject + ": " + layoutChangeRefusal + layoutDetail(sides.desired, sides.held), nil, nil
	case localIndexesDiffer(sides.desired, sides.held):
		return true, subject + ": " + localIndexRefusal, nil, nil
	case ColumnTTLEqual(sides.desired.Ttl, sides.held.Ttl):
		return true, "", nil, nil
	}
	// The declaration is held to the rules of a creation, which a TTL the
	// table takes in place obeys too: its column is declared, first in the
	// key and of a type the mode reads.
	if _, err := CompileColumnTable(owner, table.Desired); err != nil {
		return true, err.Error(), nil, nil
	}
	change := &columnTTLChange{held: enabledTTL(sides.held.Ttl), set: enabledTTL(sides.desired.Ttl)}
	if reason := tieredTTLRefusal(capabilities, subject, change.set); reason != "" {
		return true, reason, nil, nil
	}
	return true, "", change, nil
}

// enabledTTL returns ttl in the terms of a declaration when it is enabled, and
// nil for none. The expiration the server derives from the tiers of a read TTL
// is left out: no declaration states it, and the comparison does not read it.
func enabledTTL(ttl *native.ColumnTTLPolicy) *native.ColumnTTLPolicy {
	if ttl.GetEnabled() == nil {
		return nil
	}
	result := proto.CloneOf(ttl)
	result.GetEnabled().ExpireAfterSeconds = nil
	return result
}

// layoutDetail names the setting a layout change would change, with both
// values.
func layoutDetail(desired *native.ColumnStorageIntent, held *native.ColumnStorageObservation) string {
	if desired.Partitions != 0 && desired.Partitions != held.Partitions {
		return fmt.Sprintf(" (the table has %d shards, and the declaration asks for %d)", held.Partitions, desired.Partitions)
	}
	return fmt.Sprintf(" (the table is hashed by %q, and the declaration asks for %q)", held.HashColumns, desired.HashColumns)
}

// columnRebuild reports a REBUILD of a table that is a column table on either
// side.
func columnRebuild(owner *protocol.Reference, table *protocol.PlanningTable) (bool, error) {
	sides, err := readStorage(owner, table)
	if err != nil {
		return false, err
	}
	return sides.columnTable(), nil
}

func tableLabel(owner *protocol.Reference) string {
	if owner.Schema == "" {
		return owner.Name
	}
	return owner.Schema + "/" + owner.Name
}

// isColumnCreation reports whether a creation operation creates a column table.
func isColumnCreation(value *protocol.NormalizedValue) bool {
	return value.GetValue().GetKind() == ColumnOperationKind
}

// columnTTLStep is the TTL change of one altered column table.
type columnTTLStep struct {
	index  int
	table  *protocol.PlanningTable
	owner  *protocol.Reference
	change columnTTLChange
}

// plan returns the steps of a TTL change, as the host's embedded engine plans
// them: a RESET of the TTL the table holds, placed early, and a SET of the
// declared one after it. YDB keeps no external data source a table's TTL
// names, so the held TTL lets go of its sources before their owner drops or
// replaces them, and the declared one is set once its sources exist. A change
// to a TTL from none is the SET alone, and a change to none the RESET alone.
// number orders the change among the others of the plan.
//
// A lone RESET drops the table's TTL setting and a lone SET creates it. When
// both run, each alters it: the host refuses a provider step that creates
// what another provider step drops, since it orders every creation of a
// phase before every drop, and the dependency here orders the two steps.
func (s columnTTLStep) plan(number int) ([]*protocol.PlanningStep, []*protocol.PlanningDependency, error) {
	name := fmt.Sprintf("column-table-ttl/%06d", number)
	slot := proto.CloneOf(s.table.Subject)
	slot.Kind = "ydb.ttl"
	var steps []*protocol.PlanningStep
	add := func(suffix string, alteration *native.ColumnTableTTLAlteration, action protocol.PlanningEffect_Action, placement protocol.PlanningMetadata_Placement) error {
		id := &protocol.StepIdentity{Owner: schema.ProviderID, Name: name + suffix}
		operation, err := CaptureColumnTTLAlteration(id.Name, s.owner, alteration)
		if err != nil {
			return err
		}
		facts := operation.OperationSemantics
		steps = append(steps, &protocol.PlanningStep{Id: id, Operation: &protocol.PlanningOperation{Role: protocol.NativeOperation_STATEMENT, Value: operation},
			Metadata: &protocol.PlanningMetadata{Transaction: protocol.PlanningMetadata_FORBIDDEN, Impact: facts.Impact, Reason: facts.Reason, Placement: placement,
				Effects: []*protocol.PlanningEffect{{Subject: proto.CloneOf(s.table.Subject), Action: protocol.PlanningEffect_READ}, {Subject: proto.CloneOf(slot), Action: action}}}})
		return nil
	}
	held, set := s.change.held, s.change.set
	reset, install := protocol.PlanningEffect_DROP, protocol.PlanningEffect_CREATE
	if held != nil && set != nil {
		reset, install = protocol.PlanningEffect_ALTER, protocol.PlanningEffect_ALTER
	}
	if held != nil {
		if err := add("/reset", &native.ColumnTableTTLAlteration{Held: held}, reset, protocol.PlanningMetadata_EARLY); err != nil {
			return nil, nil, err
		}
	}
	if set != nil {
		if err := add("/set", &native.ColumnTableTTLAlteration{Set: set}, install, protocol.PlanningMetadata_DEFAULT); err != nil {
			return nil, nil, err
		}
	}
	if len(steps) != 2 {
		return steps, nil, nil
	}
	return steps, []*protocol.PlanningDependency{{Before: proto.CloneOf(steps[0].Id), After: proto.CloneOf(steps[1].Id)}}, nil
}
