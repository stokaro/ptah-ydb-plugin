package tablecreate

import (
	"fmt"

	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/stokaro/ptah/provider-sdk/provider"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	native "github.com/stokaro/ptah-ydb-plugin/internal/nativeprotocol"
	"github.com/stokaro/ptah-ydb-plugin/schema"
)

// The refusals of a column-table change no statement makes in place. YDB fixes
// a table's storage kind, hash key and shard count when it creates the table,
// and copies no column table into a new one, so each needs an explicit data
// migration. The words are the host's embedded engine's.
const (
	kindChangeRefusal    = "changing between row and column storage requires an explicit data migration"
	layoutChangeRefusal  = "changing a column table's hash key or shard count requires an explicit data migration"
	columnRebuildRefusal = "column-table rebuilds require an explicit data migration"
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
	result.known = FacetKnown(table.Desired.GetFeatures().GetFacets().GetCoverage(), schema.ColumnStorageProperties, table.Subject, DesiredSide)
	return result, nil
}

// columnTableChange is the in-place change of one column table: its local
// indexes, in the order their steps run, and its TTL, nil when it stays.
type columnTableChange struct {
	indexes []localIndexChange
	ttl     *columnTTLChange
}

// columnAlteration is what planColumnAlteration decides for a table: whether
// it is a column table on either side, the reason its change is refused or
// "", and the changes of its local indexes and TTL, nil for none.
type columnAlteration struct {
	columnTable bool
	reason      string
	change      *columnTableChange
}

// planColumnAlteration plans the ALTER of a table that is a column table on
// either side, and reports whether it is one. A column table whose storage
// already satisfies the declaration needs no step, and one whose local indexes
// or TTL differ gets the changes of each. A change of storage kind, hash key or
// shard count is refused with the reason, and so is any common step of the
// table: the provider carries out none for a column table.
func planColumnAlteration(owner *protocol.Reference, table *protocol.PlanningTable, steps []*protocol.CommonPlanningStep, capabilities map[string]bool) (columnAlteration, error) {
	table, err := columnTableTTL(table)
	if err != nil {
		return columnAlteration{}, err
	}
	sides, err := readStorage(owner, table)
	if err != nil || !sides.columnTable() {
		return columnAlteration{}, err
	}
	subject := fmt.Sprintf("table %q", tableLabel(owner))
	if reason := storageKindRefusal(sides, subject); reason != "" {
		return columnAlteration{columnTable: true, reason: reason}, nil
	}
	for _, step := range steps {
		if step.Parent != nil && provider.CompareIdentities(step.Parent, table.Subject) == 0 {
			return columnAlteration{columnTable: true, reason: columnStepRefusal}, nil
		}
	}
	if sides.desired == nil {
		// The source could not describe the table's storage, so the table
		// keeps it.
		return columnAlteration{columnTable: true}, nil
	}
	if !LayoutSatisfied(sides.desired, sides.held) {
		return columnAlteration{columnTable: true, reason: subject + ": " + layoutChangeRefusal + layoutDetail(sides.desired, sides.held)}, nil
	}
	ttlKept := ColumnTTLEqual(sides.desired.Ttl, sides.held.Ttl)
	if ttlKept && !localIndexesDiffer(sides.desired, sides.held) {
		return columnAlteration{columnTable: true}, nil
	}
	// The declaration is held to the rules of a creation, which a TTL the
	// table takes in place obeys too: its column is declared, first in the
	// key and of a type the mode reads. Each local index the plan adds needs
	// its capability; one the table keeps does not.
	creation, err := CompileColumnTable(owner, table.Desired, capabilities)
	if err != nil {
		return columnAlteration{columnTable: true, reason: err.Error()}, nil
	}
	indexes, reason := planLocalIndexes(sides.desired, sides.held, creationTypes(creation), capabilities)
	if reason != "" {
		return columnAlteration{columnTable: true, reason: reason}, nil
	}
	change := &columnTableChange{indexes: indexes}
	if !ttlKept {
		change.ttl = &columnTTLChange{held: enabledTTL(sides.held.Ttl), set: enabledTTL(sides.desired.Ttl)}
		if reason := tieredTTLRefusal(capabilities, subject, change.ttl.set); reason != "" {
			return columnAlteration{columnTable: true, reason: reason}, nil
		}
	}
	return columnAlteration{columnTable: true, change: change}, nil
}

// storageKindRefusal says why the storage of a table that is a column table
// on either side cannot be planned: a change between row and column storage,
// which needs an explicit data migration, or a held storage that was not
// read. subject names the table. It is empty when both sides are column
// tables, or when the declaration leaves the storage to the table.
func storageKindRefusal(sides storageSides, subject string) string {
	switch {
	case sides.desired != nil && sides.held == nil && sides.rowRead:
		return subject + ": " + kindChangeRefusal
	case sides.desired != nil && sides.held == nil:
		return "The table's current storage was not captured"
	case sides.desired == nil && (sides.rowStated || sides.known):
		return subject + ": " + kindChangeRefusal
	}
	return ""
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
	table, err := columnTableTTL(table)
	if err != nil {
		return false, err
	}
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

// columnTableStep is the change of one altered column table. root is the
// database the plan runs in, which the paths of the data sources its TTL
// names are read against.
type columnTableStep struct {
	index  int
	table  *protocol.PlanningTable
	owner  *protocol.Reference
	root   string
	change columnTableChange
}

// localIndexNumbers numbers the local-index steps of a plan, the removals,
// the additions and the renames apart.
type localIndexNumbers struct {
	drops, adds, renames int
}

// plan returns the steps of one column table's change and the dependencies
// between them. number orders the table among the others of the plan, and
// numbers numbers its local-index steps among theirs.
//
// The TTL changes in place. `SET (TTL = ...)` replaces the TTL a column table
// holds, whatever either one moves rows to (measured on 25.1.4.7 and
// 26.2.1.14), so a TTL that moves no row is replaced by the SET alone, as the
// host's embedded engine replaces it: no window without a TTL, and one schema
// operation. A held TTL that moves rows to an external data source is reset
// first, placed early, and the declared one set after it, as the embedded
// engine changes a tiered TTL: YDB refuses to drop or replace a data source a
// table's TTL names, so the held TTL lets go of its sources before their owner
// drops or replaces them, which the SET cannot do when it is to name the same
// source again, and the declared one is set once its sources exist. Each step
// reads the sources its TTL names, which orders it against their owner. A
// change to a TTL from none is the SET alone, and a change to none the RESET
// alone. A lone RESET drops the table's TTL setting and a lone SET creates it.
// Otherwise each step alters it: the host refuses a provider step that creates
// what another provider step drops, since it orders every creation of a phase
// before every drop, and the dependency here orders the two steps.
//
// Each local index is one step, in the lifecycle phase, so that a removal and
// the addition that replaces the index under its name can be ordered in one
// window: the removal first. No local index decides where a TTL may read,
// since YDB takes a column table's TTL on the first key column only, so the
// index steps and the TTL steps are not ordered against each other.
func (s columnTableStep) plan(number int, numbers *localIndexNumbers) ([]*protocol.PlanningStep, []*protocol.PlanningDependency, error) {
	var steps []*protocol.PlanningStep
	var dependencies []*protocol.PlanningDependency
	if ttl := s.change.ttl; ttl != nil {
		ttlSteps, ttlDependencies, err := s.ttlSteps(number, *ttl)
		if err != nil {
			return nil, nil, err
		}
		steps, dependencies = append(steps, ttlSteps...), append(dependencies, ttlDependencies...)
	}
	dropped := make(map[string]*protocol.StepIdentity)
	for _, change := range s.change.indexes {
		prefix, counter, action := "column-table-drop-local-index", &numbers.drops, protocol.PlanningEffect_DROP
		alteration := &native.ColumnTableLocalIndexAlteration{Drop: change.drop}
		switch {
		case change.rename():
			prefix, counter = "column-table-rename-local-index", &numbers.renames
			alteration = &native.ColumnTableLocalIndexAlteration{Add: change.add, Drop: change.drop}
		case change.add != nil:
			prefix, counter, action = "column-table-add-local-index", &numbers.adds, protocol.PlanningEffect_CREATE
			alteration = &native.ColumnTableLocalIndexAlteration{Add: change.add}
		}
		id := &protocol.StepIdentity{Owner: schema.ProviderID, Name: fmt.Sprintf("%s/%06d", prefix, *counter)}
		*counter++
		operation, err := CaptureColumnLocalIndex(id.Name, s.owner, alteration)
		if err != nil {
			return nil, nil, err
		}
		effects := []*protocol.PlanningEffect{{Subject: proto.CloneOf(s.table.Subject), Action: protocol.PlanningEffect_READ},
			{Subject: indexSubject(s.table.Subject, change.name()), Action: action}}
		if change.rename() {
			// A rename drops the held name and creates the declared one.
			effects = []*protocol.PlanningEffect{{Subject: proto.CloneOf(s.table.Subject), Action: protocol.PlanningEffect_READ},
				{Subject: indexSubject(s.table.Subject, change.drop.GetName()), Action: protocol.PlanningEffect_DROP},
				{Subject: indexSubject(s.table.Subject, change.add.GetName()), Action: protocol.PlanningEffect_CREATE}}
		}
		facts := operation.OperationSemantics
		steps = append(steps, &protocol.PlanningStep{Id: id,
			Operation: &protocol.PlanningOperation{Role: protocol.NativeOperation_STATEMENT, Value: operation, Phase: protocol.PlanningOperation_LIFECYCLE},
			Metadata:  &protocol.PlanningMetadata{Transaction: protocol.PlanningMetadata_FORBIDDEN, Impact: facts.Impact, Reason: facts.Reason, Effects: effects}})
		switch {
		case change.rename():
		case change.drop != nil:
			dropped[change.name()] = id
		case dropped[change.name()] != nil:
			dependencies = append(dependencies, &protocol.PlanningDependency{Before: proto.CloneOf(dropped[change.name()]), After: proto.CloneOf(id)})
		}
	}
	return steps, dependencies, nil
}

// ttlSteps are the steps of a TTL change, as plan describes them.
func (s columnTableStep) ttlSteps(number int, change columnTTLChange) ([]*protocol.PlanningStep, []*protocol.PlanningDependency, error) {
	name := fmt.Sprintf("column-table-ttl/%06d", number)
	id := func(suffix string) *protocol.StepIdentity { return &protocol.StepIdentity{Owner: schema.ProviderID, Name: name + suffix} }
	held, set := change.held, change.set
	if held != nil && set != nil && !MovesRows(held) {
		step, err := ttlStep(id("/set"), s.owner, s.table.Subject, &native.ColumnTableTTLAlteration{Held: held, Set: set}, protocol.PlanningEffect_ALTER,
			protocol.PlanningMetadata_DEFAULT, sourceReads(s.root, set))
		if err != nil {
			return nil, nil, err
		}
		return []*protocol.PlanningStep{step}, nil, nil
	}
	reset, install := protocol.PlanningEffect_DROP, protocol.PlanningEffect_CREATE
	if held != nil && set != nil {
		reset, install = protocol.PlanningEffect_ALTER, protocol.PlanningEffect_ALTER
	}
	var steps []*protocol.PlanningStep
	if held != nil {
		step, err := ttlStep(id("/reset"), s.owner, s.table.Subject, &native.ColumnTableTTLAlteration{Held: held}, reset, protocol.PlanningMetadata_EARLY, sourceReads(s.root, held))
		if err != nil {
			return nil, nil, err
		}
		steps = append(steps, step)
	}
	if set != nil {
		step, err := ttlStep(id("/set"), s.owner, s.table.Subject, &native.ColumnTableTTLAlteration{Set: set}, install, protocol.PlanningMetadata_DEFAULT, sourceReads(s.root, set))
		if err != nil {
			return nil, nil, err
		}
		steps = append(steps, step)
	}
	if len(steps) != 2 {
		return steps, nil, nil
	}
	return steps, []*protocol.PlanningDependency{{Before: proto.CloneOf(steps[0].Id), After: proto.CloneOf(steps[1].Id)}}, nil
}
