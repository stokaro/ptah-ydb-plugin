package streamingplan

import (
	"context"
	"fmt"
	"slices"

	"github.com/stokaro/ptah-ydb-plugin/internal/objectcompare"
	"github.com/stokaro/ptah-ydb-plugin/internal/objectplan"
	"github.com/stokaro/ptah-ydb-plugin/internal/streaming"
	"github.com/stokaro/ptah-ydb-plugin/schema"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/stokaro/ptah/provider-sdk/provider"
	"google.golang.org/protobuf/proto"
)

func assessObjects(ctx context.Context, request *protocol.PlanFeaturesRequest, result *protocol.PlannedFeatures, builder *lifecycleBuilder) error {
	writes := dependencyWrites(request, builder.paths)
	if writes {
		if err := completeQueryInventory(request); err != nil {
			result.Diagnostics = []*protocol.PlanningDiagnostic{{Code: protocol.PlanningDiagnostic_UNSUPPORTED_FEATURE, Kind: schema.StreamingQueryProperties, Message: err.Error()}}
			return nil
		}
	}
	for index, subject := range provider.PlanningObjectSubjects(request) {
		if err := ctx.Err(); err != nil {
			return err
		}
		receipt := &protocol.PlanningReceipt{Subject: proto.CloneOf(subject), Kind: schema.StreamingQueryProperties, Strategy: "Retain the captured query and its input dependencies"}
		matched := false
		for _, changed := range result.Changes {
			if provider.CompareIdentities(changed.Subject, subject) == 0 {
				receipt.Steps = cloneSteps(changed.Steps)
				matched = true
				break
			}
		}
		if !matched {
			steps, err := assessUnchanged(request, builder, index, subject, writes)
			if err != nil {
				result.Diagnostics = []*protocol.PlanningDiagnostic{{Code: protocol.PlanningDiagnostic_UNSUPPORTED_FEATURE, Kind: schema.StreamingQueryProperties, Message: err.Error(), Input: &protocol.PlanningDiagnostic_ContextObject{ContextObject: uint32(index)}}}
				return nil
			}
			receipt.Steps = steps
		}
		result.Objects = append(result.Objects, receipt)
	}
	return nil
}

func assessUnchanged(request *protocol.PlanFeaturesRequest, builder *lifecycleBuilder, index int, subject *protocol.ComparisonIdentity, writes bool) ([]*protocol.StepIdentity, error) {
	if !localContextSubject(request, subject) {
		return nil, fmt.Errorf("streaming query context names another database")
	}
	current := contextRecord(request.CurrentObjects, subject)
	desired := contextRecord(request.DesiredObjects, subject)
	if current == nil && desired == nil {
		if !writes {
			return nil, nil
		}
		for _, state := range []*protocol.ComparisonState{request.CurrentObjects, request.DesiredObjects} {
			known := objectcompare.Knowledge(state.GetCoverage(), subject.Kind, subject, request.DatabasePath)
			if known.GetState() != protocol.ModelKnowledge_COMPLETE && known.GetState() != protocol.ModelKnowledge_ABSENT {
				return nil, fmt.Errorf("streaming query %q has no complete captured definition: %s", subject.GetName().GetSource(), known.GetReason())
			}
		}
		return nil, nil
	}
	if current == nil || desired == nil {
		return nil, fmt.Errorf("streaming query %q needs an accepted create or drop change", subject.GetName().GetSource())
	}
	before, err := schema.ObservedStreamingQuery(current.Value.Value)
	if err != nil {
		return nil, err
	}
	after, _, err := schema.DesiredStreamingQuery(desired.Value.Value)
	if err != nil {
		return nil, err
	}
	if !streaming.Equal(before, after) {
		return nil, fmt.Errorf("streaming query %q differs without an accepted change", subject.GetName().GetSource())
	}
	if !streaming.Running(before) {
		return nil, nil
	}
	touched, err := inputsChanged(request, builder.paths, before)
	if err != nil || !touched {
		return nil, err
	}
	if !request.Capabilities[streaming.Capability] || request.Capabilities["transactional_ddl"] {
		return nil, fmt.Errorf("restarting a streaming query requires established streaming_queries and nontransactional DDL")
	}
	if _, known := request.Capabilities["transactional_ddl"]; !known {
		return nil, fmt.Errorf("the DDL transaction capability was not established")
	}
	if err := survivingInputs(request, after); err != nil {
		return nil, err
	}
	owner, err := schema.NativeBoundOwner(schema.StreamingQueryProperties, subject)
	if err != nil {
		return nil, err
	}
	stopped := before
	stopped.Run = new(false)
	parts := []streaming.Change{
		{Schema: owner.Schema, Name: owner.Name, Before: &before, After: &stopped},
		{Schema: owner.Schema, Name: owner.Name, Before: &stopped, After: &after},
	}
	return builder.add(fmt.Sprintf("streaming-context/%06d", index), subject, owner, current.Value.Value.GetContext(), parts)
}

func cloneSteps(values []*protocol.StepIdentity) []*protocol.StepIdentity {
	result := make([]*protocol.StepIdentity, len(values))
	for i, value := range values {
		result[i] = proto.CloneOf(value)
	}
	return result
}

func contextRecord(state *protocol.ComparisonState, subject *protocol.ComparisonIdentity) *protocol.ComparisonRecord {
	for _, record := range state.GetRecords() {
		if provider.CompareIdentities(record.Subject, subject) == 0 {
			return record
		}
	}
	return nil
}

func queryInputKind(kind string) bool {
	return slices.Contains([]string{"table", schema.TopicProperties, schema.ExternalSourceProperties, schema.ExternalTableProperties, schema.ViewProperties}, kind)
}

func inputsChanged(request, paths *protocol.PlanFeaturesRequest, spec streaming.Spec) (bool, error) {
	names, err := streaming.NamedPaths(spec.Text)
	if err != nil {
		return false, err
	}
	for _, name := range names {
		input, err := queryPath(request.DatabasePath, name)
		if err != nil {
			return false, err
		}
		for _, changed := range request.ChangedSubjects {
			if localContextSubject(request, changed) && queryInputKind(changed.Kind) && provider.CompareIdentities(input, objectplan.Slot(changed)) == 0 {
				return true, nil
			}
		}
		for _, common := range paths.CommonSteps {
			for _, effect := range common.GetMetadata().GetEffects() {
				if effect.Action != protocol.PlanningEffect_READ && (queryInputKind(effect.Subject.Kind) || effect.Subject.Kind == objectplan.PathKind) && provider.CompareIdentities(input, objectplan.Slot(effect.Subject)) == 0 {
					return true, nil
				}
			}
		}
	}
	pool := streaming.Pool(spec)
	for _, changed := range request.ChangedSubjects {
		if localContextSubject(request, changed) && pool != "default" && changed.Kind == schema.ResourcePoolProperties && changed.GetName().GetNormalized() == pool {
			return true, nil
		}
	}
	return false, nil
}

func survivingInputs(request *protocol.PlanFeaturesRequest, spec streaming.Spec) error {
	names, err := streaming.NamedPaths(spec.Text)
	if err != nil {
		return err
	}
	for _, name := range names {
		input, err := queryPath(request.DatabasePath, name)
		if err != nil {
			return err
		}
		occupants := 0
		for _, table := range request.Tables {
			if localContextSubject(request, table.Subject) && table.Desired != nil && provider.CompareIdentities(input, objectplan.Slot(table.Subject)) == 0 {
				occupants++
			}
		}
		for _, record := range request.DesiredObjects.GetRecords() {
			if localContextSubject(request, record.Subject) && queryInputKind(record.Kind) && provider.CompareIdentities(input, objectplan.Slot(record.Subject)) == 0 {
				occupants++
			}
		}
		if occupants != 1 {
			return fmt.Errorf("streaming query input %q needs one captured surviving table, topic, view, or external object; found %d", name, occupants)
		}
	}
	if pool := streaming.Pool(spec); pool != "default" {
		found := false
		for _, record := range request.DesiredObjects.GetRecords() {
			found = found || localContextSubject(request, record.Subject) && record.Kind == schema.ResourcePoolProperties && record.Subject.GetName().GetNormalized() == pool
		}
		if !found {
			return fmt.Errorf("streaming query resource pool %q has no captured surviving definition", pool)
		}
	}
	return nil
}

func localContextSubject(request *protocol.PlanFeaturesRequest, subject *protocol.ComparisonIdentity) bool {
	database := subject.GetDatabase().GetNormalized()
	return database == "" || request.DatabasePath != "" && database == request.DatabasePath
}
