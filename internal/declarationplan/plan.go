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
	"github.com/stokaro/ptah-ydb-plugin/internal/workload"
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
	phase := authoredPhase(request)
	anchor := lateAnchor(request)
	var late []*protocol.StepIdentity
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
		text, err := Text(operation.Value, request.Capabilities)
		if err != nil {
			return refused(index, object.Kind, err.Error()), nil
		}
		if text == "" {
			// The pool default names no setting: YDB created it with the
			// database, and nothing is written for it (see Text).
			result.Declarations[index] = &protocol.DeclarationReceipt{Subject: proto.CloneOf(object.Subject), Strategy: keptDefaultPool}
			continue
		}
		effects := []*protocol.PlanningEffect{{Subject: proto.CloneOf(object.Subject), Action: creationAction(desired)}}
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
			Operation: &protocol.PlanningOperation{Value: operation, Role: protocol.NativeOperation_STATEMENT, Phase: phase},
			Metadata:  &protocol.PlanningMetadata{Transaction: protocol.PlanningMetadata_FORBIDDEN, Impact: facts.Impact, Reason: facts.Reason, Effects: effects},
		})
		if phase == protocol.PlanningOperation_LIFECYCLE && anchor != nil && !earlyKind(object.Kind) {
			late = append(late, id)
		}
		result.Declarations[index] = &protocol.DeclarationReceipt{
			Subject: proto.CloneOf(object.Subject), Strategy: "Create the authored definition without observed-state assumptions", Steps: []*protocol.StepIdentity{proto.CloneOf(id)},
		}
	}
	if len(contribution.Steps) > 0 {
		contribution.Dependencies = readsAfterCreations(contribution.Steps)
		for _, step := range late {
			contribution.Dependencies = append(contribution.Dependencies, &protocol.PlanningDependency{Before: proto.CloneOf(anchor), After: proto.CloneOf(step)})
		}
		result.Contributions = []*protocol.PlanningContribution{contribution}
	}
	return result, ctx.Err()
}

// authoredPhase is the phase of every authored creation: the lifecycle span
// where the host negotiated it, and the default window otherwise. The default
// window lies after the tables and before the views, which matches no kind's
// place in a schema render of the host's embedded engine at 74de0b65b. In the
// span a creation runs once the objects its effects read exist, and the
// dependency on lateAnchor moves a late kind's creation further down. Every
// kind this service plans states what it reads as an effect (see kindReads
// and sourceSecretEffects), which is what makes the span safe for it.
func authoredPhase(request *protocol.PlanDeclarationsRequest) protocol.PlanningOperation_Phase {
	if slices.Contains(request.GetTargetFeatures(), provider.PlanningLifecycleFeature) {
		return protocol.PlanningOperation_LIFECYCLE
	}
	return protocol.PlanningOperation_DEFAULT
}

// earlyKind reports whether the embedded engine at 74de0b65b created an
// authored object of kind as soon as what it read existed: a topic, a secret,
// an external data source and an external table, ahead of the users and the
// tables. It created every other kind this service plans, a coordination
// node, a resource pool, a classifier and a streaming query, after the
// statements of the common objects and before the grants on the objects it
// created, which it wrote last. Measured with schema render and schema
// inspect --format sql on 25.1.4.7 and 26.2.1.14.
func earlyKind(kind string) bool {
	switch kind {
	case schema.TopicProperties, schema.SecretProperties, schema.ExternalSourceProperties, schema.ExternalTableProperties:
		return true
	default:
		return false
	}
}

// lateAnchor is the common step a creation of a late kind follows: the last
// common step other than a marker ahead of the first grant or revoke on an
// object request declares, or the last of all when there is none. A host
// that writes those permissions after every scheduled statement, as the YDB
// engine built into the host did, leaves them out of the common steps, and
// the anchor is the last common step. A host that keeps them in its sequence
// among the other grants lists them, and a creation placed after the first
// of them would wait for a grant on itself. Nil places the creation by its
// effects alone.
func lateAnchor(request *protocol.PlanDeclarationsRequest) *protocol.StepIdentity {
	var anchor *protocol.StepIdentity
	for _, common := range request.GetCommonSteps() {
		if permitsDeclaredObject(request, common) {
			break
		}
		if !objectplan.Marker(common) {
			anchor = common.GetId()
		}
	}
	return anchor
}

// permitsDeclaredObject reports whether common grants or revokes a permission
// on an object request declares: it reads the object, whatever database the
// identity names.
func permitsDeclaredObject(request *protocol.PlanDeclarationsRequest, common *protocol.CommonPlanningStep) bool {
	if common.GetCommonObject().GetGrant() == nil && common.GetCommonObject().GetRevoke() == nil {
		return false
	}
	for _, effect := range common.GetMetadata().GetEffects() {
		if effect.GetAction() != protocol.PlanningEffect_READ {
			continue
		}
		read := withoutDatabase(effect.GetSubject())
		if slices.ContainsFunc(request.GetObjects(), func(object *protocol.ComparisonRecord) bool {
			return provider.CompareIdentities(withoutDatabase(object.GetSubject()), read) == 0
		}) {
			return true
		}
	}
	return false
}

// withoutDatabase is a copy of identity that names no database.
func withoutDatabase(identity *protocol.ComparisonIdentity) *protocol.ComparisonIdentity {
	identity = proto.CloneOf(identity)
	identity.Database = &protocol.ComparisonPart{}
	return identity
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
// when it creates it, so the query follows their creations. An absolute path
// names an object below the root desired names its paths in: the database a
// read took the query from, whose text YDB holds as written. A query whose
// reads cannot be named is refused, and so is one with an absolute path
// outside that root or with no root to read it below.
func streamingQueryReads(index int, object *protocol.ComparisonRecord, desired *protocol.Envelope) ([]*protocol.PlanningEffect, *protocol.PlannedDeclarations, error) {
	spec, _, err := schema.DesiredStreamingQuery(desired)
	if err != nil {
		return nil, nil, err
	}
	reads, err := streamingplan.QueryReads(schema.InterpretationRoot(desired), spec)
	if err != nil {
		return nil, refused(index, object.Kind, err.Error()), nil
	}
	var effects []*protocol.PlanningEffect
	for _, read := range reads {
		effects = append(effects, &protocol.PlanningEffect{Subject: read, Action: protocol.PlanningEffect_READ})
	}
	return effects, nil, nil
}

// keptDefaultPool is the strategy of the pool default where it names no
// setting.
const keptDefaultPool = "Write nothing for the pool default, which YDB creates with the database, since it names no setting"

// creationAction is what the step of desired does to its subject: it alters
// the pool default, which YDB creates with the database, and creates any
// other object. A classifier's read of the pool default then follows the
// change of its settings.
func creationAction(desired *protocol.Envelope) protocol.PlanningEffect_Action {
	if desired.GetKind() == schema.ResourcePoolProperties && desired.GetOwner().GetName() == workload.DefaultPool {
		return protocol.PlanningEffect_ALTER
	}
	return protocol.PlanningEffect_CREATE
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

// readsAfterCreations orders each step after the steps that create what it
// reads: a classifier after its resource pool, an external table after its
// data source, a streaming query after the topics its text names. The host
// orders the effects of two contributions on one subject, and leaves the
// steps of one contribution to the contribution's dependencies; without
// these it refuses the plan (`unordered effects on ... resource-pool`).
func readsAfterCreations(steps []*protocol.PlanningStep) []*protocol.PlanningDependency {
	type creation struct {
		subject *protocol.ComparisonIdentity
		step    *protocol.StepIdentity
	}
	var creations []creation
	for _, step := range steps {
		for _, effect := range step.GetMetadata().GetEffects() {
			if effect.GetAction() == protocol.PlanningEffect_CREATE {
				creations = append(creations, creation{subject: effect.GetSubject(), step: step.GetId()})
			}
		}
	}
	slices.SortStableFunc(creations, func(a, b creation) int { return provider.CompareIdentities(a.subject, b.subject) })
	var edges []*protocol.PlanningDependency
	for _, step := range steps {
		for _, effect := range step.GetMetadata().GetEffects() {
			if effect.GetAction() != protocol.PlanningEffect_READ {
				continue
			}
			first, _ := slices.BinarySearchFunc(creations, effect.GetSubject(), func(have creation, subject *protocol.ComparisonIdentity) int {
				return provider.CompareIdentities(have.subject, subject)
			})
			for _, created := range creations[first:] {
				if provider.CompareIdentities(created.subject, effect.GetSubject()) != 0 {
					break
				}
				edge := &protocol.PlanningDependency{Before: proto.CloneOf(created.step), After: proto.CloneOf(step.GetId())}
				if !proto.Equal(edge.Before, edge.After) && !slices.ContainsFunc(edges, func(have *protocol.PlanningDependency) bool { return proto.Equal(have, edge) }) {
					edges = append(edges, edge)
				}
			}
		}
	}
	return edges
}
