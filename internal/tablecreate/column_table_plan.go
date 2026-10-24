package tablecreate

import (
	"fmt"

	native "github.com/stokaro/ptah-ydb-plugin/internal/nativeprotocol"
	"github.com/stokaro/ptah-ydb-plugin/schema"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/stokaro/ptah/provider-sdk/provider"
	"google.golang.org/protobuf/encoding/protojson"
)

// The refusals of a column-table change no statement makes in place. YDB fixes
// a table's storage kind, hash key and shard count when it creates the table,
// and copies no column table into a new one, so each needs an explicit data
// migration. The words are the host's embedded engine's.
const (
	kindChangeRefusal    = "changing between row and column storage requires an explicit data migration"
	layoutChangeRefusal  = "changing a column table's hash key or shard count requires an explicit data migration"
	rebuildRefusal       = "column-table rebuilds require an explicit data migration"
	localIndexRefusal    = "the local indexes of a column table are not added or dropped by the provider yet"
	columnStepRefusal    = "A common step is outside the supported column table lifecycle: the provider adds and drops no column of a column table yet"
	columnTTLStepRefusal = "changing the TTL of an existing column table is not planned by the provider yet"
)

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
// already satisfies the declaration needs no step. A change of storage kind,
// hash key or shard count is refused with the reason, and so is any common
// step of the table: the provider carries out none for a column table.
func planColumnAlteration(owner *protocol.Reference, table *protocol.PlanningTable, steps []*protocol.CommonPlanningStep) (bool, string, error) {
	sides, err := readStorage(owner, table)
	if err != nil || !sides.columnTable() {
		return false, "", err
	}
	subject := fmt.Sprintf("table %q", tableLabel(owner))
	switch {
	case sides.desired != nil && sides.held == nil && sides.rowRead:
		return true, subject + ": " + kindChangeRefusal, nil
	case sides.desired != nil && sides.held == nil:
		return true, "The table's current storage was not captured", nil
	case sides.desired == nil && (sides.rowStated || sides.known):
		return true, subject + ": " + kindChangeRefusal, nil
	}
	for _, step := range steps {
		if step.Parent != nil && provider.CompareIdentities(step.Parent, table.Subject) == 0 {
			return true, columnStepRefusal, nil
		}
	}
	if sides.desired == nil {
		// The source could not describe the table's storage, so the table
		// keeps it.
		return true, "", nil
	}
	switch {
	case !LayoutSatisfied(sides.desired, sides.held):
		return true, subject + ": " + layoutChangeRefusal + layoutDetail(sides.desired, sides.held), nil
	case localIndexesDiffer(sides.desired, sides.held):
		return true, subject + ": " + localIndexRefusal, nil
	case !ColumnTTLEqual(sides.desired.Ttl, sides.held.Ttl):
		return true, subject + ": " + columnTTLStepRefusal, nil
	}
	return true, "", nil
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
