package tablecreate

import (
	"fmt"
	"slices"

	"google.golang.org/protobuf/proto"
	protocol "ptah.run/provider-sdk/protocol/experimental"

	"github.com/stokaro/ptah-ydb-plugin/internal/externalpath"
	native "github.com/stokaro/ptah-ydb-plugin/internal/nativeprotocol"
	"github.com/stokaro/ptah-ydb-plugin/internal/ydbdecl"
	"github.com/stokaro/ptah-ydb-plugin/schema"
)

// TTLSources are the external data sources the tiers of a column table's TTL
// move rows to, relative to root, the database the plan runs in, in tier
// order and each once. YDB refuses to drop a data source a table's TTL names
// (`Other entities depend on this data source`, measured on 25.1.4.7 and
// 26.2.1.14), so every step on such a source is ordered against the TTL. A
// path outside root is left out: no step of the plan writes a source there.
func TTLSources(root string, ttl *native.ColumnTTLPolicy) []ydbdecl.Path {
	var sources []ydbdecl.Path
	for _, tier := range ttl.GetEnabled().GetTiers() {
		if tier.GetExternalStorage() == "" {
			continue
		}
		source, err := externalpath.SourcePath(root, tier.GetExternalStorage())
		if err == nil && !slices.Contains(sources, source) {
			sources = append(sources, source)
		}
	}
	return sources
}

// sourceReads are the reads of the data sources a TTL names, as effects of a
// step that writes or drops the TTL.
func sourceReads(root string, ttl *native.ColumnTTLPolicy) []*protocol.PlanningEffect {
	var effects []*protocol.PlanningEffect
	for _, source := range TTLSources(root, ttl) {
		effects = append(effects, &protocol.PlanningEffect{Subject: schema.ExternalSourceSubject(source), Action: protocol.PlanningEffect_READ})
	}
	return effects
}

// ttlStep is one statement on a column table's TTL: the step id names, which
// carries out alteration on the table owner. It reads the table and the data
// sources reads names, and does action to the table's TTL setting.
func ttlStep(id *protocol.StepIdentity, owner *protocol.Reference, table *protocol.ComparisonIdentity, alteration *native.ColumnTableTTLAlteration,
	action protocol.PlanningEffect_Action, placement protocol.PlanningMetadata_Placement, reads []*protocol.PlanningEffect,
) (*protocol.PlanningStep, error) {
	operation, err := CaptureColumnTTLAlteration(id.Name, owner, alteration)
	if err != nil {
		return nil, err
	}
	slot := proto.CloneOf(table)
	slot.Kind = "ydb.ttl"
	effects := append([]*protocol.PlanningEffect{{Subject: proto.CloneOf(table), Action: protocol.PlanningEffect_READ}, {Subject: slot, Action: action}}, reads...)
	facts := operation.OperationSemantics
	return &protocol.PlanningStep{Id: id, Operation: &protocol.PlanningOperation{Role: protocol.NativeOperation_STATEMENT, Value: operation},
		Metadata: &protocol.PlanningMetadata{Transaction: protocol.PlanningMetadata_FORBIDDEN, Impact: facts.Impact, Reason: facts.Reason, Placement: placement,
			Effects: effects}}, nil
}

// KeptTTL is the TTL of a column table that the plan keeps while it changes
// a data source the TTL moves rows to, and the steps around that change.
type KeptTTL struct {
	// Table is the table, as the request names it.
	Table *protocol.ComparisonIdentity
	// Sources are the data sources the TTL names; see TTLSources.
	Sources []ydbdecl.Path
	// Reset lets the TTL go of its sources, and runs before the change of any
	// of them; Set sets the same TTL again, after every such change.
	Reset, Set *protocol.PlanningStep
}

// KeptTieredTTL returns the steps that carry the TTL of table, a table the
// plan keeps, past a change of a data source the TTL moves rows to, or nil
// when there is no such TTL: the table is not a column table read with a
// tiered TTL, the plan creates, drops or rebuilds it, or the plan changes its
// TTL, which the table's own steps then carry out (see columnTableStep.plan).
// number numbers the steps among the kept TTLs of the plan. The host's
// embedded engine plans the same pair: YDB replaces no data source a TTL
// names, so the TTL lets go of it first.
func KeptTieredTTL(number int, root string, table *protocol.PlanningTable) (*KeptTTL, error) {
	if table.GetAction() != protocol.PlanningTable_CONTEXT && table.GetAction() != protocol.PlanningTable_ALTER {
		return nil, nil
	}
	owner, err := boundOwner(table.GetSubject())
	if err != nil {
		return nil, err
	}
	converted, err := columnTableTTL(table)
	if err != nil {
		return nil, err
	}
	sides, err := readStorage(owner, converted)
	if err != nil {
		return nil, err
	}
	held := enabledTTL(sides.held.GetTtl())
	if !MovesRows(held) || table.GetAction() == protocol.PlanningTable_ALTER && sides.desired != nil && !ColumnTTLEqual(sides.desired.GetTtl(), sides.held.GetTtl()) {
		return nil, nil
	}
	kept := &KeptTTL{Table: proto.CloneOf(table.GetSubject()), Sources: TTLSources(root, held)}
	name := fmt.Sprintf("column-table-kept-ttl/%06d", number)
	reads := sourceReads(root, held)
	if kept.Reset, err = ttlStep(&protocol.StepIdentity{Owner: schema.ProviderID, Name: name + "/reset"}, owner, table.GetSubject(),
		&native.ColumnTableTTLAlteration{Held: held}, protocol.PlanningEffect_ALTER, protocol.PlanningMetadata_EARLY, reads); err != nil {
		return nil, err
	}
	if kept.Set, err = ttlStep(&protocol.StepIdentity{Owner: schema.ProviderID, Name: name + "/set"}, owner, table.GetSubject(),
		&native.ColumnTableTTLAlteration{Set: held}, protocol.PlanningEffect_ALTER, protocol.PlanningMetadata_DEFAULT, reads); err != nil {
		return nil, err
	}
	return kept, nil
}

// heldTTL is the TTL a column table holds by the read of it, or nil.
func heldTTL(table *protocol.PlanningTable) (*native.ColumnTTLPolicy, error) {
	owner, err := boundOwner(table.GetSubject())
	if err != nil {
		return nil, err
	}
	sides, err := readStorage(owner, table)
	if err != nil {
		return nil, err
	}
	return enabledTTL(sides.held.GetTtl()), nil
}

// PlannedColumnTTL is the TTL a column table holds once the plan has run: the
// declared one of a table the plan creates, the declared one of a table it
// alters, or the held one when the declaration does not describe the table's
// storage, and the held one of a table the plan keeps. It is nil for a table
// the plan drops or rebuilds, and for a row table.
func PlannedColumnTTL(table *protocol.PlanningTable) (*native.ColumnTTLPolicy, error) {
	switch table.GetAction() {
	case protocol.PlanningTable_DROP, protocol.PlanningTable_REBUILD:
		return nil, nil
	}
	owner, err := boundOwner(table.GetSubject())
	if err != nil {
		return nil, err
	}
	converted, err := columnTableTTL(table)
	if err != nil {
		return nil, err
	}
	sides, err := readStorage(owner, converted)
	if err != nil {
		return nil, err
	}
	if sides.desired != nil {
		return enabledTTL(sides.desired.GetTtl()), nil
	}
	return enabledTTL(sides.held.GetTtl()), nil
}
