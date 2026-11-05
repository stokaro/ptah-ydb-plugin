package declarationplan

import (
	"context"
	"fmt"
	"slices"
	"strings"

	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/stokaro/ptah/provider-sdk/provider"
	"google.golang.org/protobuf/proto"

	"github.com/stokaro/ptah-ydb-plugin/internal/externalpath"
	"github.com/stokaro/ptah-ydb-plugin/internal/objectplan"
	"github.com/stokaro/ptah-ydb-plugin/internal/streamingplan"
	"github.com/stokaro/ptah-ydb-plugin/schema"
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
	ranks := make(map[int64]bool)
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
		reads, refusal, err := kindReads(index, object, desired, ranks)
		if refusal != nil || err != nil {
			return refusal, err
		}
		effects = append(effects, reads...)
		secretReads, err := sourceSecretEffects(object)
		if err != nil {
			return refused(index, object.Kind, err.Error()), nil
		}
		effects = append(effects, secretReads...)
		id := &protocol.StepIdentity{Owner: schema.ProviderID, Name: operation.Id}
		facts := operation.OperationSemantics
		contribution.Steps = append(contribution.Steps, &protocol.PlanningStep{
			Id:        id,
			Operation: &protocol.PlanningOperation{Value: operation, Role: protocol.NativeOperation_STATEMENT},
			Metadata:  &protocol.PlanningMetadata{Transaction: protocol.PlanningMetadata_FORBIDDEN, Impact: facts.Impact, Reason: facts.Reason, Effects: effects},
		})
		result.Declarations[index] = &protocol.DeclarationReceipt{
			Subject: proto.CloneOf(object.Subject), Strategy: "Create the authored definition without observed-state assumptions", Steps: []*protocol.StepIdentity{proto.CloneOf(id)},
		}
	}
	if len(contribution.Steps) > 0 {
		result.Contributions = []*protocol.PlanningContribution{contribution}
	}
	return result, ctx.Err()
}

// kindReads returns the effects that make a declaration of object, the
// declaration at index, read the objects its kind depends on. A resource pool
// classifier reads its pool, an external table its data source and a
// streaming query every topic and table it reads or writes. A refusal of the
// declaration, or an error, returns no effects.
func kindReads(index int, object *protocol.ComparisonRecord, desired *protocol.Envelope, ranks map[int64]bool) ([]*protocol.PlanningEffect, *protocol.PlannedDeclarations, error) {
	switch object.Kind {
	case schema.ResourcePoolClassifierProperties:
		return classifierReads(index, object, desired, ranks)
	case schema.ExternalTableProperties:
		reads, err := externalTableReads(object, desired)
		return reads, nil, err
	case schema.StreamingQueryProperties:
		return streamingQueryReads(index, object, desired)
	}
	return nil, nil, nil
}

// classifierReads returns the read of the resource pool a classifier names,
// none when it names no pool. ranks holds the ranks of the classifiers
// planned so far; a second classifier of one rank is refused, and the
// classifier's rank joins ranks otherwise.
func classifierReads(index int, object *protocol.ComparisonRecord, desired *protocol.Envelope, ranks map[int64]bool) ([]*protocol.PlanningEffect, *protocol.PlannedDeclarations, error) {
	spec, err := schema.WorkloadClassifier(desired)
	if err != nil {
		return nil, nil, err
	}
	if ranks[spec.Rank] {
		return nil, refused(index, object.Kind, "authored resource pool classifiers share one rank"), nil
	}
	ranks[spec.Rank] = true
	if spec.ResourcePool == "" {
		return nil, nil, nil
	}
	ref := proto.CloneOf(object.Subject)
	ref.Kind = schema.ResourcePoolProperties
	ref.Name = &protocol.ComparisonPart{Source: spec.ResourcePool, Normalized: spec.ResourcePool}
	return []*protocol.PlanningEffect{{Subject: ref, Action: protocol.PlanningEffect_READ}}, nil, nil
}

// externalTableReads returns the read of the data source an external table
// names, none when its data source path does not resolve.
func externalTableReads(object *protocol.ComparisonRecord, desired *protocol.Envelope) ([]*protocol.PlanningEffect, error) {
	intent, err := schema.DesiredExternalTable(desired)
	if err != nil {
		return nil, err
	}
	path, pathErr := externalpath.SourcePath(schema.InterpretationRoot(desired), intent.GetDataSourcePath())
	if pathErr != nil {
		return nil, nil
	}
	ref := proto.CloneOf(object.Subject)
	ref.Kind = schema.ExternalSourceProperties
	ref.Schema = &protocol.ComparisonPart{Source: path.Schema, Normalized: path.Schema}
	ref.Name = &protocol.ComparisonPart{Source: path.Name, Normalized: path.Name}
	return []*protocol.PlanningEffect{{Subject: ref, Action: protocol.PlanningEffect_READ}}, nil
}

// streamingQueryReads returns the reads of the scheme slots of every topic
// and table a streaming query reads or writes. YDB compiles a streaming query
// when it creates it, so the query follows their creations. A query whose
// reads cannot be named is refused.
func streamingQueryReads(index int, object *protocol.ComparisonRecord, desired *protocol.Envelope) ([]*protocol.PlanningEffect, *protocol.PlannedDeclarations, error) {
	spec, _, err := schema.DesiredStreamingQuery(desired)
	if err != nil {
		return nil, nil, err
	}
	reads, err := streamingplan.QueryReads("", spec)
	if err != nil {
		return nil, refused(index, object.Kind, err.Error()), nil
	}
	var effects []*protocol.PlanningEffect
	for _, read := range reads {
		effects = append(effects, &protocol.PlanningEffect{Subject: read, Action: protocol.PlanningEffect_READ})
	}
	return effects, nil, nil
}

func refused(index int, kind, message string) *protocol.PlannedDeclarations {
	// #nosec G115 -- index is a position in the request, which runtime.MaxMessageBytes bounds to 4 MiB
	return &protocol.PlannedDeclarations{Complete: true, Diagnostics: []*protocol.DeclarationDiagnostic{{
		Object:  new(uint32(index)),
		Problem: &protocol.SchemaDiagnostic{Code: protocol.SchemaDiagnostic_UNSUPPORTED_FEATURE, Kind: kind, Message: message},
	}}}
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
