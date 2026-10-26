// Package externalplan lowers captured changes of external data sources and
// external tables into the host's shared graph, in one batch, as Ptah's
// embedded YDB engine plans them: a data source the plan drops and creates
// again takes the external tables over it along, tables are dropped before
// the data sources they read and created after, and a statement the target
// cannot take refuses the whole batch.
//
// YDB alters neither object (`Alter operation for EXTERNAL_DATA_SOURCE
// objects is not implemented`), so a changed one is replaced: by CREATE OR
// REPLACE on a target with the external_object_replace capability, and
// otherwise by a DROP and a CREATE.
package externalplan

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/stokaro/ptah-ydb-plugin/internal/externals"
	"github.com/stokaro/ptah-ydb-plugin/internal/objectplan"
	"github.com/stokaro/ptah-ydb-plugin/internal/ydbdecl"
	"github.com/stokaro/ptah-ydb-plugin/schema"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/stokaro/ptah/provider-sdk/provider"
	"google.golang.org/protobuf/proto"
)

// ServiceID names the external object planner.
const ServiceID = "ydb-external-objects"

// ChangeKinds are the change models the planner takes.
func ChangeKinds() []string { return []string{schema.ExternalSourceChange, schema.ExternalTableChange} }

// OperationKinds are the operation models the planner returns.
func OperationKinds() []string {
	return []string{schema.ExternalSourceOperation, schema.ExternalTableOperation}
}

// group orders the statements of one batch: tables are dropped before the
// data sources they read, and created after.
type group int

const (
	tableDrops group = iota
	sourceDrops
	sourceWrites
	tableWrites
)

// step is one statement the batch plans.
type step struct {
	input     int
	subject   *protocol.ComparisonIdentity
	group     group
	action    protocol.PlanningEffect_Action
	operation string
	// change is the change the step's operation carries out.
	change *protocol.Envelope
	// sourceType is the source type a data source write declares.
	sourceType string
	// written is the data source a table write declares, as written.
	written string
	// sources are the data sources a table statement reads or read, to order
	// it against their statements.
	sources []ydbdecl.Path
	// secrets are the scheme secrets read before or after this operation.
	secrets []ydbdecl.Path
}

func (s step) path() ydbdecl.Path {
	return ydbdecl.Path{Schema: s.subject.GetSchema().GetNormalized(), Name: s.subject.GetName().GetNormalized()}
}

func (s step) source() bool { return s.subject.GetKind() == schema.ExternalSourceProperties }

// lowered is one change of the batch with the statements it needs.
type lowered struct {
	input    int
	subject  *protocol.ComparisonIdentity
	kind     string
	strategy string
	steps    []step
	// refused is why the batch cannot take the change.
	refused error
}

// Plan contributes operations, footprints, dependencies, and a receipt per
// input. A refused change returns no operation from the batch.
func Plan(ctx context.Context, request *protocol.PlanFeaturesRequest) (*protocol.PlannedFeatures, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if request.PlanningId != ServiceID || len(request.ParentKinds) != 0 {
		return nil, fmt.Errorf("invalid external object planning assignment")
	}
	if request.Identifiers.GetDefaultSchema() != "" || request.Identifiers.GetCatalogCollation() != "" {
		return nil, fmt.Errorf("invalid YDB planning identifier context")
	}
	for _, name := range request.Identifiers.GetResolvedNames() {
		if name.Name != name.Key {
			return nil, fmt.Errorf("YDB planning requires exact identifiers")
		}
	}
	if transactional, known := request.Capabilities["transactional_ddl"]; len(request.Changes) > 0 && (!known || transactional) {
		return &protocol.PlannedFeatures{Complete: true, Diagnostics: []*protocol.PlanningDiagnostic{{Code: protocol.PlanningDiagnostic_UNSUPPORTED_FEATURE,
			Kind: request.Changes[0].GetValue().GetValue().GetKind(), Message: "Planning an external object requires established nontransactional DDL",
			Input: &protocol.PlanningDiagnostic_Change{Change: 0}}}}, nil
	}
	changes, err := lower(ctx, request)
	if err != nil {
		return nil, err
	}
	if diagnostics := refusals(request.Capabilities, changes); len(diagnostics) > 0 {
		return &protocol.PlannedFeatures{Complete: true, Diagnostics: diagnostics}, nil
	}
	return contribute(ctx, request, changes)
}

// read is one change of the request, read.
type read struct {
	subject *protocol.ComparisonIdentity
	change  *protocol.Envelope
	specs   externals.ChangeSpecs
}

// lower reads the batch's changes and lowers each, in input order. An error
// is a request no owner could answer.
func lower(ctx context.Context, request *protocol.PlanFeaturesRequest) ([]lowered, error) {
	root := request.DatabasePath
	inputs := make([]read, 0, len(request.Changes))
	for _, input := range request.Changes {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		kind := input.GetSubject().GetKind()
		owner, err := schema.NativeBoundOwner(kind, input.Subject)
		if err != nil {
			return nil, err
		}
		if (kind == schema.ExternalSourceProperties) != (input.GetValue().GetValue().GetKind() == schema.ExternalSourceChange) || !slices.Contains([]string{schema.ExternalSourceProperties, schema.ExternalTableProperties}, kind) {
			return nil, fmt.Errorf("external object change names another kind of object")
		}
		captured, err := schema.NormalizeExternalChange(&protocol.ValueInput{Id: input.Value.Id, Representation: protocol.ValueInput_CHANGE, Value: input.Value.Value})
		if err != nil {
			return nil, err
		}
		if !proto.Equal(captured, input.Value) || !proto.Equal(owner, captured.NormalizedOwner) {
			return nil, fmt.Errorf("planning change has a forged normalization receipt or identity")
		}
		if slices.ContainsFunc(inputs, func(other read) bool { return provider.CompareIdentities(other.subject, input.Subject) == 0 }) {
			return nil, fmt.Errorf("duplicate external object change")
		}
		_, _, database, err := schema.ExternalOperands(captured.Value)
		if err != nil {
			return nil, err
		}
		for _, named := range []string{database, owner.Database} {
			if named == "" {
				continue
			}
			if root != "" && named != root {
				return nil, fmt.Errorf("external object change targets another database")
			}
			root = named
		}
		inputs = append(inputs, read{subject: input.Subject, change: captured.Value})
	}
	for i := range inputs {
		// Each change is read against the database the batch runs in, which
		// the request or one of its changes names.
		specs, err := externals.ReadChange(inputs[i].change)
		if err != nil {
			return nil, err
		}
		if specs.Database != root {
			if specs, err = readAgainst(inputs[i].change, root); err != nil {
				return nil, err
			}
		}
		inputs[i].specs = specs
	}
	replace := request.Capabilities[externals.CapabilityReplace]
	displaced := map[ydbdecl.Path]bool{}
	for _, input := range inputs {
		if input.specs.SourceBefore != nil && externals.Displaces(input.specs.SourceBefore, input.specs.SourceAfter, replace) {
			displaced[path(input.subject)] = true
		}
	}
	result := make([]lowered, 0, len(inputs))
	for index, input := range inputs {
		if input.subject.GetKind() == schema.ExternalSourceProperties {
			result = append(result, lowerSource(index, input, replace, root))
			continue
		}
		result = append(result, lowerTable(index, input, replace, displaced, root))
	}
	return result, nil
}

func readAgainst(change *protocol.Envelope, root string) (externals.ChangeSpecs, error) {
	before, after, _, err := schema.ExternalOperands(change)
	if err != nil {
		return externals.ChangeSpecs{}, err
	}
	result := externals.ChangeSpecs{Database: root, Before: before, After: after}
	if change.GetKind() == schema.ExternalSourceChange {
		result.SourceBefore, result.SourceAfter, err = externals.SourcePair(before, after, root)
	} else {
		result.TableBefore, result.TableAfter, err = externals.TablePair(before, after, root)
	}
	return result, err
}

func path(subject *protocol.ComparisonIdentity) ydbdecl.Path {
	return ydbdecl.Path{Schema: subject.GetSchema().GetNormalized(), Name: subject.GetName().GetNormalized()}
}

func display(subject *protocol.ComparisonIdentity) string {
	return ydbdecl.ExternalDisplay(subject.GetSchema().GetSource(), subject.GetName().GetSource())
}

// dropHalf is change with only its before operand: the change the first step
// of a table dropped and created again carries out. The second step restores
// the table from the whole change.
func dropHalf(change *protocol.Envelope, database string) (*protocol.Envelope, error) {
	before, _, _, err := schema.ExternalOperands(change)
	if err != nil {
		return nil, err
	}
	captured, err := schema.CaptureExternalChange(schema.ExternalTableProperties, "step", change.Owner, database, before, nil)
	if err != nil {
		return nil, err
	}
	return captured.Value, nil
}

func lowerSource(index int, input read, replace bool, root string) lowered {
	result := lowered{input: index, subject: input.subject, kind: schema.ExternalSourceChange}
	before, after := input.specs.SourceBefore, input.specs.SourceAfter
	secretPaths := func(source *ydbdecl.ExternalDataSource) []ydbdecl.Path {
		paths, err := externals.SecretPaths(source.Options, root)
		if err != nil {
			result.refused = fmt.Errorf("external data source %s: %w", display(input.subject), err)
		}
		return paths
	}
	write := func(operation string, action protocol.PlanningEffect_Action) step {
		paths := secretPaths(after)
		if before != nil {
			// Removing a reference must finish before the old secret drops.
			for _, path := range secretPaths(before) {
				if !slices.Contains(paths, path) {
					paths = append(paths, path)
				}
			}
		}
		return step{input: index, subject: input.subject, group: sourceWrites, action: action, operation: operation, change: input.change, sourceType: after.SourceType, secrets: paths}
	}
	switch {
	case before == nil:
		result.strategy = "create the data source"
		result.steps = []step{write(schema.ExternalCreate, protocol.PlanningEffect_CREATE)}
	case after == nil:
		result.strategy = "drop the data source; the system it reads is untouched"
		result.steps = []step{{input: index, subject: input.subject, group: sourceDrops, action: protocol.PlanningEffect_DROP, operation: schema.ExternalDrop, change: input.change, secrets: secretPaths(before)}}
	case !replace:
		result.strategy = "drop the data source and create it again, with the external tables over it"
		result.steps = []step{write(schema.ExternalRecreate, protocol.PlanningEffect_ALTER)}
	case externals.Displaces(before, after, replace):
		result.strategy = "replace the data source in place with CREATE OR REPLACE, with the external tables over it dropped first"
		result.steps = []step{write(schema.ExternalReplace, protocol.PlanningEffect_ALTER)}
	default:
		result.strategy = "replace the data source in place with CREATE OR REPLACE"
		result.steps = []step{write(schema.ExternalReplace, protocol.PlanningEffect_ALTER)}
	}
	return result
}

func lowerTable(index int, input read, replace bool, displaced map[ydbdecl.Path]bool, root string) lowered {
	result := lowered{input: index, subject: input.subject, kind: schema.ExternalTableChange}
	before, after := input.specs.TableBefore, input.specs.TableAfter
	var read, wrote []ydbdecl.Path
	if before != nil {
		if source, ok := externals.ResolveSource(root, before.DataSource); ok {
			read = []ydbdecl.Path{source}
		}
	}
	if after != nil {
		if source, ok := externals.ResolveSource(root, after.DataSource); ok {
			wrote = []ydbdecl.Path{source}
		}
	}
	drop := func(change *protocol.Envelope) step {
		return step{input: index, subject: input.subject, group: tableDrops, action: protocol.PlanningEffect_DROP, operation: schema.ExternalDrop, change: change, sources: read}
	}
	write := func(operation string, action protocol.PlanningEffect_Action, change *protocol.Envelope) step {
		return step{input: index, subject: input.subject, group: tableWrites, action: action, operation: operation, change: change, sources: wrote, written: after.DataSource}
	}
	recreates := before != nil && after != nil && (!replace || slices.ContainsFunc(append(slices.Clone(read), wrote...), func(source ydbdecl.Path) bool { return displaced[source] }))
	switch {
	case before == nil:
		result.strategy = "create the external table"
		result.steps = []step{write(schema.ExternalCreate, protocol.PlanningEffect_CREATE, input.change)}
	case after == nil:
		result.strategy = "drop the external table; the files it reads stay"
		result.steps = []step{drop(input.change)}
	case recreates:
		result.strategy = "drop the external table and create it again"
		dropped, err := dropHalf(input.change, input.specs.Database)
		if err != nil {
			result.refused = err
			return result
		}
		result.steps = []step{drop(dropped), write(schema.ExternalRestore, protocol.PlanningEffect_CREATE, input.change)}
	default:
		result.strategy = "replace the external table in place with CREATE OR REPLACE"
		result.steps = []step{write(schema.ExternalReplace, protocol.PlanningEffect_ALTER, input.change)}
	}
	return result
}

// refusals refuses, before any operation is returned, a statement the target
// cannot take: every one on a target without the external_data_sources
// capability, a declaration the line refuses, two objects written at one
// path, and an external table over a data source the batch writes that is not
// object storage.
func refusals(capabilities map[string]bool, changes []lowered) []*protocol.PlanningDiagnostic {
	var diagnostics []*protocol.PlanningDiagnostic
	conflicts := conflicts(changes)
	for _, change := range changes {
		refused := change.refused
		if refused == nil {
			refused = conflicts[change.input]
		}
		for _, planned := range change.steps {
			if refused != nil {
				break
			}
			refused = refusal(capabilities, change, planned)
		}
		if refused != nil {
			diagnostics = append(diagnostics, &protocol.PlanningDiagnostic{Code: protocol.PlanningDiagnostic_UNSUPPORTED_FEATURE, Kind: change.kind, Object: display(change.subject),
				Message: refused.Error(), Input: &protocol.PlanningDiagnostic_Change{Change: uint32(change.input)}})
		}
	}
	return diagnostics
}

// conflicts finds, by change, the objects the batch cannot write together:
// an object written at the path another one is written at, since YDB keeps
// one object at a path (`unexpected path type`), and an external table over a
// data source the batch writes with a source type other than ObjectStorage,
// since an external table reads files (`Only ObjectStorage source type
// supported but got PostgreSQL`), both measured by the host on 25.1.4.7 and
// 26.2.1.14.
func conflicts(changes []lowered) map[int]error {
	found := map[int]error{}
	written := map[ydbdecl.Path]step{}
	sourceTypes := map[ydbdecl.Path]string{}
	for _, change := range changes {
		for _, planned := range change.steps {
			if planned.action == protocol.PlanningEffect_DROP && len(planned.secrets) != 0 {
				continue
			}
			if planned.source() {
				sourceTypes[planned.path()] = planned.sourceType
			}
			if other, taken := written[planned.path()]; taken && other.subject.GetKind() != planned.subject.GetKind() {
				found[change.input] = fmt.Errorf("%s %s has the path of %s %s, and YDB keeps one object at a path (`unexpected path type`)",
					noun(planned.subject), display(planned.subject), noun(other.subject), display(other.subject))
				continue
			}
			written[planned.path()] = planned
		}
	}
	for _, change := range changes {
		for _, planned := range change.steps {
			if planned.source() || planned.action == protocol.PlanningEffect_DROP || len(planned.sources) == 0 || found[change.input] != nil {
				continue
			}
			if sourceType, declared := sourceTypes[planned.sources[0]]; declared && sourceType != "ObjectStorage" {
				found[change.input] = fmt.Errorf("external table %s reads data source %s, a %s source; an external table reads files, "+
					"from an ObjectStorage source (`Only ObjectStorage source type supported`)", display(planned.subject), planned.written, sourceType)
			}
		}
	}
	return found
}

func noun(subject *protocol.ComparisonIdentity) string {
	if subject.GetKind() == schema.ExternalSourceProperties {
		return "external data source"
	}
	return "external table"
}

func refusal(capabilities map[string]bool, change lowered, planned step) error {
	name := display(planned.subject)
	specs, err := externals.ReadChange(planned.change)
	if err != nil {
		return err
	}
	if planned.source() {
		if planned.operation == schema.ExternalDrop {
			return externals.CheckDrop("DROP EXTERNAL DATA SOURCE "+name, capabilities)
		}
		return cmp.Or(externals.CheckDataSource(name, *specs.SourceAfter, capabilities), replaceRefusal(planned, "external data source "+name, capabilities))
	}
	if planned.operation == schema.ExternalDrop {
		return externals.CheckDrop("DROP EXTERNAL TABLE "+name, capabilities)
	}
	return cmp.Or(externals.CheckTable(name, *specs.TableAfter, capabilities), replaceRefusal(planned, "external table "+name, capabilities))
}

func replaceRefusal(planned step, subject string, capabilities map[string]bool) error {
	if planned.operation != schema.ExternalReplace {
		return nil
	}
	return externals.CheckReplace(subject, capabilities)
}

// contribute orders the batch's statements and returns one receipt per
// change.
func contribute(ctx context.Context, request *protocol.PlanFeaturesRequest, changes []lowered) (*protocol.PlannedFeatures, error) {
	var steps []step
	for _, change := range changes {
		steps = append(steps, change.steps...)
	}
	slices.SortStableFunc(steps, func(a, b step) int {
		return cmp.Or(cmp.Compare(a.group, b.group), provider.CompareIdentities(a.subject, b.subject))
	})
	result := &protocol.PlannedFeatures{Complete: true, Changes: make([]*protocol.PlanningReceipt, len(changes))}
	contribution := &protocol.PlanningContribution{Owner: schema.ProviderID}
	ids := make([]*protocol.StepIdentity, len(steps))
	byInput := map[int][]*protocol.StepIdentity{}
	for index, planned := range steps {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		id := &protocol.StepIdentity{Owner: schema.ProviderID, Name: fmt.Sprintf("%s/%06d/%s", strings.ReplaceAll(noun(planned.subject), " ", "-"), index, strings.ToLower(planned.action.String()))}
		ids[index] = id
		value, err := schema.ExternalOperation(id.Name, planned.change, planned.operation)
		if err != nil {
			return nil, err
		}
		pathRequest, err := objectplan.PathContext(request)
		if err != nil {
			return refused(changes[planned.input], err), nil
		}
		edges, err := objectplan.PathDependencies(pathRequest, family(planned.subject.GetKind()), planned.subject, planned.action, id)
		if err != nil {
			return refused(changes[planned.input], err), nil
		}
		accessAction := planned.action
		if len(changes[planned.input].steps) > 1 {
			// A temporary drop must preserve a revoke until the replacement
			// object exists and its captured access has been restored.
			accessAction = protocol.PlanningEffect_ALTER
		}
		access, err := objectplan.OrderAccess(request, family(planned.subject.GetKind()), planned.subject, accessAction, id)
		if err != nil {
			return refused(changes[planned.input], err), nil
		}
		access.Dependencies = append(access.Dependencies, objectplan.BeforePrincipalChanges(request, accessAction, id)...)
		contribution.Dependencies = append(contribution.Dependencies, edges...)
		effects := []*protocol.PlanningEffect{{Subject: proto.CloneOf(planned.subject), Action: planned.action}, {Subject: objectplan.Slot(planned.subject), Action: planned.action}}
		for _, path := range planned.secrets {
			secret := sourceSubject(path)
			secret.Kind = schema.SecretProperties
			effects = append(effects, &protocol.PlanningEffect{Subject: secret, Action: protocol.PlanningEffect_READ},
				&protocol.PlanningEffect{Subject: objectplan.Slot(secret), Action: protocol.PlanningEffect_READ})
		}
		if !planned.source() && planned.action != protocol.PlanningEffect_DROP {
			for _, source := range planned.sources {
				effects = append(effects, &protocol.PlanningEffect{Subject: sourceSubject(source), Action: protocol.PlanningEffect_READ})
			}
		}
		facts := value.OperationSemantics
		// Every statement of the batch asks for the lifecycle phase, which
		// orders operations around common changes: a table dropped
		// before the data source it reads is created again after it, and the
		// batch orders its own steps there.
		step := &protocol.PlanningStep{Id: id, Operation: &protocol.PlanningOperation{Role: protocol.NativeOperation_STATEMENT, Value: value, Phase: protocol.PlanningOperation_LIFECYCLE},
			Metadata: &protocol.PlanningMetadata{Transaction: protocol.PlanningMetadata_FORBIDDEN, Placement: protocol.PlanningMetadata_EARLY, Impact: facts.Impact, Reason: facts.Reason, Effects: effects}}
		if len(access.Dependencies)+len(access.Sources)+len(planned.secrets) != 0 {
			if !slices.Contains(request.GetTargetFeatures(), provider.PlanningLifecycleFeature) {
				return refused(changes[planned.input], fmt.Errorf("native dependency ordering requires planning.lifecycle")), nil
			}
			step.Operation.Phase = protocol.PlanningOperation_LIFECYCLE
			step.Metadata.Placement = protocol.PlanningMetadata_DEFAULT
			if planned.action == protocol.PlanningEffect_DROP {
				step.Metadata.Placement = protocol.PlanningMetadata_EARLY
			}
			step.Metadata.Effects = append(step.Metadata.Effects, access.Effects...)
			contribution.Dependencies = append(contribution.Dependencies, access.Dependencies...)
			if len(access.Sources) != 0 {
				result.Rewrites = append(result.Rewrites, &protocol.PlanningRewrite{Sources: access.Sources, Replacement: proto.CloneOf(id)})
			}
		}
		contribution.Steps = append(contribution.Steps, step)
		byInput[planned.input] = append(byInput[planned.input], proto.CloneOf(id))
	}
	contribution.Dependencies = append(contribution.Dependencies, order(steps, ids)...)
	for _, change := range changes {
		result.Changes[change.input] = &protocol.PlanningReceipt{Subject: proto.CloneOf(change.subject), Kind: change.kind, Strategy: change.strategy, Steps: byInput[change.input]}
	}
	if len(contribution.Steps) > 0 {
		result.Contributions = []*protocol.PlanningContribution{contribution}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return result, nil
}

func refused(change lowered, err error) *protocol.PlannedFeatures {
	return &protocol.PlannedFeatures{Complete: true, Diagnostics: []*protocol.PlanningDiagnostic{{Code: protocol.PlanningDiagnostic_UNSUPPORTED_FEATURE, Kind: change.kind, Object: display(change.subject),
		Message: err.Error(), Input: &protocol.PlanningDiagnostic_Change{Change: uint32(change.input)}}}}
}

// family is what objectplan needs to order a step on an object of kind
// against the common steps at its path.
func family(kind string) objectplan.Family {
	noun := "external data source"
	if kind == schema.ExternalTableProperties {
		noun = "external table"
	}
	return objectplan.Family{ServiceID: ServiceID, Kind: kind, Noun: noun, Owner: func(subject *protocol.ComparisonIdentity) (*protocol.Reference, error) {
		return schema.NativeBoundOwner(kind, subject)
	}}
}

// sourceSubject names the data source at a path as a step's effect names it.
func sourceSubject(source ydbdecl.Path) *protocol.ComparisonIdentity {
	return &protocol.ComparisonIdentity{Kind: schema.ExternalSourceProperties, Database: &protocol.ComparisonPart{},
		Schema: &protocol.ComparisonPart{Source: source.Schema, Normalized: source.Schema}, Name: &protocol.ComparisonPart{Source: source.Name, Normalized: source.Name}}
}

// order orders the batch's own statements: a drop before a creation at its
// path, an external table's drop before any statement on the data source it
// read, and a data source's creation or replacement before the external
// tables that read it are created or replaced.
func order(steps []step, ids []*protocol.StepIdentity) []*protocol.PlanningDependency {
	var edges []*protocol.PlanningDependency
	reads := func(planned step, source ydbdecl.Path) bool { return slices.Contains(planned.sources, source) }
	for i, first := range steps {
		for j, second := range steps {
			if first.group >= second.group {
				continue
			}
			samePath := first.path() == second.path()
			switch {
			case first.action == protocol.PlanningEffect_DROP && second.action == protocol.PlanningEffect_CREATE && samePath,
				first.group == tableDrops && second.group <= sourceWrites && reads(first, second.path()),
				first.group == sourceWrites && second.group == tableWrites && reads(second, first.path()):
				edges = append(edges, &protocol.PlanningDependency{Before: proto.CloneOf(ids[i]), After: proto.CloneOf(ids[j])})
			}
		}
	}
	return edges
}
