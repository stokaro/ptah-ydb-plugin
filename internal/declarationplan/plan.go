package declarationplan

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/stokaro/ptah-ydb-plugin/internal/externals"
	"github.com/stokaro/ptah-ydb-plugin/internal/objectplan"
	"github.com/stokaro/ptah-ydb-plugin/schema"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/stokaro/ptah/provider-sdk/provider"
	"google.golang.org/protobuf/proto"
)

// Plan accounts for every authored object and its dependencies without reading
// a catalog or manufacturing an observed-state comparison.
func Plan(ctx context.Context, request *protocol.PlanDeclarationsRequest) (*protocol.PlannedDeclarations, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if request.DeclarationId != ServiceID {
		return nil, fmt.Errorf("unknown authored declaration service")
	}
	indices := make([]int, len(request.Objects))
	for i := range indices {
		indices[i] = i
	}
	slices.SortFunc(indices, func(a, b int) int {
		return provider.CompareIdentities(request.Objects[a].Subject, request.Objects[b].Subject)
	})
	result := &protocol.PlannedDeclarations{Complete: true, Declarations: make([]*protocol.DeclarationReceipt, len(indices))}
	contribution := &protocol.PlanningContribution{Owner: schema.ProviderID}
	ranks := map[int64]bool{}
	for position, index := range indices {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		object := request.Objects[index]
		desired := object.GetValue().GetValue()
		operation, err := Capture(fmt.Sprintf("authored/%06d", position), desired)
		if err != nil {
			return nil, err
		}
		if _, err := Text(operation.Value, request.Capabilities); err != nil {
			return refused(index, object.Kind, err.Error()), nil
		}
		effects := []*protocol.PlanningEffect{{Subject: proto.CloneOf(object.Subject), Action: protocol.PlanningEffect_CREATE}}
		if !schema.IsWorkloadObject(object.Kind) {
			if err := pathAvailable(request, index); err != nil {
				return refused(index, object.Kind, err.Error()), nil
			}
			effects = append(effects, &protocol.PlanningEffect{Subject: objectplan.Slot(object.Subject), Action: protocol.PlanningEffect_CREATE})
		}
		if object.Kind == schema.ResourcePoolClassifierProperties {
			spec, err := schema.WorkloadClassifier(desired)
			if err != nil {
				return nil, err
			}
			if ranks[spec.Rank] {
				return refused(index, object.Kind, "authored resource pool classifiers share one rank"), nil
			}
			ranks[spec.Rank] = true
			if spec.ResourcePool != "" {
				ref := proto.CloneOf(object.Subject)
				ref.Kind = schema.ResourcePoolProperties
				ref.Name = &protocol.ComparisonPart{Source: spec.ResourcePool, Normalized: spec.ResourcePool}
				effects = append(effects, &protocol.PlanningEffect{Subject: ref, Action: protocol.PlanningEffect_READ})
			}
		}
		if object.Kind == schema.ExternalTableProperties {
			intent, err := schema.DesiredExternalTable(desired)
			if err != nil {
				return nil, err
			}
			if path, known := externals.ResolveSource(desired.Owner.Database, intent.GetDataSourcePath()); known {
				ref := proto.CloneOf(object.Subject)
				ref.Kind = schema.ExternalSourceProperties
				ref.Schema = &protocol.ComparisonPart{Source: path.Schema, Normalized: path.Schema}
				ref.Name = &protocol.ComparisonPart{Source: path.Name, Normalized: path.Name}
				effects = append(effects, &protocol.PlanningEffect{Subject: ref, Action: protocol.PlanningEffect_READ})
			}
		}
		secretReads, err := sourceSecretEffects(object)
		if err != nil {
			return refused(index, object.Kind, err.Error()), nil
		}
		effects = append(effects, secretReads...)
		id := &protocol.StepIdentity{Owner: schema.ProviderID, Name: operation.Id}
		facts := operation.OperationSemantics
		contribution.Steps = append(contribution.Steps, &protocol.PlanningStep{Id: id, Operation: &protocol.PlanningOperation{Value: operation, Role: protocol.NativeOperation_STATEMENT}, Metadata: &protocol.PlanningMetadata{Transaction: protocol.PlanningMetadata_FORBIDDEN, Impact: facts.Impact, Reason: facts.Reason, Effects: effects}})
		result.Declarations[index] = &protocol.DeclarationReceipt{Subject: proto.CloneOf(object.Subject), Strategy: "Create the authored definition without observed-state assumptions", Steps: []*protocol.StepIdentity{proto.CloneOf(id)}}
	}
	if len(contribution.Steps) > 0 {
		result.Contributions = []*protocol.PlanningContribution{contribution}
	}
	return result, ctx.Err()
}

func refused(index int, kind, message string) *protocol.PlannedDeclarations {
	return &protocol.PlannedDeclarations{Complete: true, Diagnostics: []*protocol.DeclarationDiagnostic{{Object: new(uint32(index)), Problem: &protocol.SchemaDiagnostic{Code: protocol.SchemaDiagnostic_UNSUPPORTED_FEATURE, Kind: kind, Message: message}}}}
}

// A table and a native object share one scheme path; no object is a directory
// under which another declaration can be placed.
func pathAvailable(request *protocol.PlanDeclarationsRequest, index int) error {
	object := request.Objects[index]
	path := schemePath(object.Subject.GetSchema().GetSource(), object.Subject.GetName().GetSource())
	overlaps := func(other string) bool {
		return path == other || strings.HasPrefix(path, other+"/") || strings.HasPrefix(other, path+"/")
	}
	for _, table := range request.Tables {
		if overlaps(schemePath(table.Declaration.Schema, table.Declaration.Name)) {
			return fmt.Errorf("authored native object conflicts with a declared table path")
		}
	}
	for i, other := range request.Objects {
		if i != index && !schema.IsWorkloadObject(other.Kind) && overlaps(schemePath(other.Subject.GetSchema().GetSource(), other.Subject.GetName().GetSource())) {
			return fmt.Errorf("authored native objects share or contain one scheme path")
		}
	}
	return nil
}
func schemePath(directory, name string) string {
	if directory == "" {
		return name
	}
	return directory + "/" + name
}
