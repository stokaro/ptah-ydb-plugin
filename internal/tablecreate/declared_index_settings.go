package tablecreate

import (
	"fmt"
	"maps"
	"slices"

	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb_Table"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	protocol "ptah.run/provider-sdk/protocol/experimental"
	"ptah.run/provider-sdk/provider"

	native "github.com/stokaro/ptah-ydb-plugin/internal/nativeprotocol"
	"github.com/stokaro/ptah-ydb-plugin/schema"
)

// indexSettingsFacets returns the index facets a captured state carries for
// the indexes of the table owner names, by index name. A facet excluded for
// this target or bound to other targets states nothing here.
func indexSettingsFacets(owner *protocol.Reference, state *protocol.ComparisonState, representation protocol.ValueInput_Representation) (map[string]*native.IndexSettingsIntent, map[string]*native.IndexSettingsObservation, error) {
	declared, held := make(map[string]*native.IndexSettingsIntent), make(map[string]*native.IndexSettingsObservation)
	for _, record := range state.GetRecords() {
		if record.GetKind() != schema.IndexSettingsProperties || record.GetValue() == nil || (len(record.GetTargets()) > 0 && !slices.Contains(record.GetTargets(), "ydb")) {
			continue
		}
		value, err := normalizedFacet(record, representation)
		if err != nil {
			return nil, nil, err
		}
		if value.GetOwner().GetSchema() != owner.GetSchema() || !slices.Equal(value.GetOwner().GetPath(), []string{owner.GetName()}) {
			return nil, nil, fmt.Errorf("YDB index facet belongs to another table")
		}
		name := value.GetOwner().GetName()
		if representation == protocol.ValueInput_DESIRED {
			intent := &native.IndexSettingsIntent{}
			if err := protojson.Unmarshal(value.GetPayload(), intent); err != nil {
				return nil, nil, err
			}
			declared[name] = intent
			continue
		}
		observed := &native.IndexSettingsObservation{}
		if err := protojson.Unmarshal(value.GetPayload(), observed); err != nil {
			return nil, nil, err
		}
		held[name] = observed
	}
	return declared, held, nil
}

// changedIndexSettings returns what each change of a global index's settings
// among changes asks the index to hold, by index name, for the indexes of the
// table table names: the change's after operand. A forward change asks for
// what the index's own facet declares, which the comparison found it in. The
// reverse of one asks for every setting the index held before the forward
// change, while the declaration the host plans the reverse with is the one the
// forward change started from, which states only some of them: planned from
// that declaration alone, a setting the forward change gave the index and the
// declaration leaves out would stay.
func changedIndexSettings(table *protocol.ComparisonIdentity, changes []*protocol.PlanningChange) (map[string]*native.IndexSettingsIntent, error) {
	result := make(map[string]*native.IndexSettingsIntent)
	for _, change := range changes {
		value := change.GetValue().GetValue()
		if value.GetKind() != schema.TablePropertiesChange || change.GetSubject().GetKind() != "index" ||
			provider.CompareIdentities(changedTable(change.GetSubject()), table) != 0 {
			continue
		}
		_, after, err := schema.TablePropertiesOperands(value)
		if err != nil {
			return nil, err
		}
		intent := &native.IndexSettingsIntent{}
		if err := protojson.Unmarshal(after.GetPayload(), intent); err != nil {
			return nil, err
		}
		result[change.GetSubject().GetName().GetNormalized()] = intent
	}
	return result, nil
}

// planDeclaredIndexSettings returns the changes that give each index of an
// altered table the partitioning and read replicas its own facet declares, in
// name order, or the reason one cannot be made. changes are the index changes
// the plan already makes: an index it adds takes YDB's settings for a new
// index and then the declared ones, as the host's embedded engine writes ALTER
// INDEX ... SET after ADD INDEX; an index it renames is changed under its new
// name, after the rename; and an index it drops is left alone. Any other index
// the declaration states is one the table holds, with the settings its own
// facet reports. The rules are planIndexSettings': a setting the declaration
// leaves out keeps what the index holds.
func planDeclaredIndexSettings(table *protocol.PlanningTable, declared map[string]*native.IndexSettingsIntent, held map[string]*native.IndexSettingsObservation,
	changes []indexChange, heldIndexes *native.IndexObservations) ([]indexSettingsChange, string) {
	var result []indexSettingsChange
	for _, name := range slices.Sorted(maps.Keys(declared)) {
		want := declaredIndexSettings(declared[name])
		var index *native.TableIndexDescription
		dropped := false
		for _, change := range changes {
			switch {
			case change.add != nil && change.add.GetName() == name:
				index = newIndexDescription(change.add)
			case change.rename != nil && change.rename.to == name:
				from := change.rename.held.GetName()
				if index = readIndex(table, heldIndexes, held, from); index == nil {
					return nil, fmt.Sprintf("The settings index %q holds were not read, so its declared settings cannot be compared", from)
				}
				index.Name = name
			case change.drop != nil && change.drop.GetName() == name:
				dropped = true
			}
		}
		if index == nil && dropped {
			continue
		}
		if index == nil {
			if heldIndex(table.GetCurrent(), heldIndexes, name) == nil {
				return nil, fmt.Sprintf("Index %q declares its partitioning or read replicas, and the table neither holds it nor gains it", name)
			}
			if index = readIndex(table, heldIndexes, held, name); index == nil {
				return nil, fmt.Sprintf("The settings index %q holds were not read, so its declared settings cannot be compared", name)
			}
		}
		if scope := signatureOf(schema.IndexSpecification(index)).scope(); scope != "" {
			return nil, fmt.Sprintf("Index %q declares its partitioning or read replicas, and it %s: YDB keeps the partitioning it gives one", name, scope)
		}
		change, reason := planIndexSettings(name, want, index)
		if reason != "" {
			return nil, reason
		}
		if change != nil {
			result = append(result, *change)
		}
	}
	return result, ""
}

// readIndex is the index named name the table holds, with its settings: as the
// table's index read describes it, which the operation's precondition compares
// with what the server describes, or else as the host's read of the table
// describes it. The settings are the ones the index's own facet reports
// wherever it reports them: the plan of a reverse change finds there what the
// forward change leaves, which the reverse predicts, while the table's index
// read stays as it was read. It is nil when neither read describes the index's
// settings.
func readIndex(table *protocol.PlanningTable, heldIndexes *native.IndexObservations, held map[string]*native.IndexSettingsObservation, name string) *native.TableIndexDescription {
	if at := slices.IndexFunc(heldIndexes.GetCommonIndexes(), func(index *native.TableIndexDescription) bool { return index.GetName() == name }); at >= 0 {
		index := proto.CloneOf(heldIndexes.GetCommonIndexes()[at])
		if settings := held[name]; settings != nil {
			return withIndexSettings(index, settings)
		}
		return index
	}
	index, settings := heldIndex(table.GetCurrent(), heldIndexes, name), held[name]
	if index == nil || settings == nil {
		return nil
	}
	return withIndexSettings(index, settings)
}

// withIndexSettings is index with the partitioning and read replicas settings
// reports, which the settings rule and the operation's precondition read. The
// partition columns stay as index holds them: the facet leaves them out.
func withIndexSettings(index *native.TableIndexDescription, settings *native.IndexSettingsObservation) *native.TableIndexDescription {
	result := proto.CloneOf(index)
	held := &native.GlobalIndexSettings{PartitioningSettings: proto.CloneOf(settings.GetPartitioningSettings()), ReadReplicasSettings: proto.CloneOf(settings.GetReadReplicasSettings())}
	if columns := HeldIndexSettings(index).GetPartitioningSettings().GetPartitionBy(); len(columns) != 0 && held.PartitioningSettings != nil {
		held.PartitioningSettings.PartitionBy = slices.Clone(columns)
	}
	switch kind := result.GetType().(type) {
	case *native.TableIndexDescription_GlobalIndex:
		kind.GlobalIndex.Settings = held
	case *native.TableIndexDescription_GlobalAsyncIndex:
		kind.GlobalAsyncIndex.Settings = held
	case *native.TableIndexDescription_GlobalUniqueIndex:
		kind.GlobalUniqueIndex.Settings = held
	}
	return result
}

// planningStep numbers the change and returns its step on table: it reads the
// table and alters the one index the change names.
func (s *indexSettingsStep) planningStep(number int, table *protocol.ComparisonIdentity) *protocol.PlanningStep {
	s.id = &protocol.StepIdentity{Owner: schema.ProviderID, Name: fmt.Sprintf("row-table-index-settings/%06d", number)}
	s.operation.Id = s.id.Name
	facts := s.operation.OperationSemantics
	return &protocol.PlanningStep{Id: s.id,
		Operation: &protocol.PlanningOperation{Role: protocol.NativeOperation_STATEMENT, Value: s.operation},
		Metadata: &protocol.PlanningMetadata{Transaction: protocol.PlanningMetadata_FORBIDDEN, Impact: facts.Impact, Reason: facts.Reason,
			Effects: []*protocol.PlanningEffect{{Subject: proto.CloneOf(table), Action: protocol.PlanningEffect_READ},
				{Subject: indexSubject(table, s.change.held.GetName()), Action: protocol.PlanningEffect_ALTER}}}}
}

// plannedCreationIndexSettings returns the changes that give each global index
// of a table the plan creates the partitioning and read replicas its own facet
// declares, over the settings YDB gives a new index, in name order, or the
// reason one cannot be made. An index the creation does not hold, and one YDB
// keeps the partitioning of, refuse the plan.
func plannedCreationIndexSettings(owner *protocol.Reference, desired *protocol.DeclaredTable, operation *protocol.NormalizedValue) ([]*indexSettingsStep, string, error) {
	declared, _, err := indexSettingsFacets(owner, desired.GetFeatures().GetFacets(), protocol.ValueInput_DESIRED)
	if err != nil || len(declared) == 0 {
		return nil, "", err
	}
	var created []*Ydb_Table.TableIndex
	if !isColumnCreation(operation) {
		request, err := Request(operation.Value)
		if err != nil {
			return nil, "", err
		}
		created = request.GetIndexes()
	}
	var steps []*indexSettingsStep
	for _, name := range slices.Sorted(maps.Keys(declared)) {
		at := slices.IndexFunc(created, func(index *Ydb_Table.TableIndex) bool { return index.GetName() == name })
		if at < 0 {
			return nil, fmt.Sprintf("Index %q declares its partitioning or read replicas, and table %q creates no global index of that name", name, tableLabel(owner)), nil
		}
		switch created[at].GetType().(type) {
		case *Ydb_Table.TableIndex_GlobalIndex, *Ydb_Table.TableIndex_GlobalAsyncIndex, *Ydb_Table.TableIndex_GlobalUniqueIndex:
		default:
			return nil, fmt.Sprintf("Index %q declares its partitioning or read replicas, and it is not a global sync, async or unique index: YDB keeps the partitioning it gives one", name), nil
		}
		change, reason := planIndexSettings(name, declaredIndexSettings(declared[name]), newIndexDescription(indexIntentOf(created[at])))
		if reason != "" {
			return nil, reason, nil
		}
		if change == nil {
			continue
		}
		step := &indexSettingsStep{change: *change}
		if step.operation, err = CaptureIndexSettings("pending", owner, *change); err != nil {
			return nil, err.Error(), nil
		}
		steps = append(steps, step)
	}
	return steps, "", nil
}
