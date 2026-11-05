// Package externalplan lowers captured changes of external data sources and
// external tables into the host's shared graph, in one batch, as Ptah's
// embedded YDB engine plans them: a data source the plan drops and creates
// again takes the external tables over it along, tables are dropped before
// the data sources they read and created after, the TTL of a column table the
// plan keeps lets go of a data source the batch replaces and is set again
// after, and a statement the target cannot take refuses the whole batch.
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

	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/stokaro/ptah/provider-sdk/provider"
	"google.golang.org/protobuf/proto"

	"github.com/stokaro/ptah-ydb-plugin/internal/externalpath"
	"github.com/stokaro/ptah-ydb-plugin/internal/externals"
	"github.com/stokaro/ptah-ydb-plugin/internal/featureflags"
	"github.com/stokaro/ptah-ydb-plugin/internal/objectplan"
	"github.com/stokaro/ptah-ydb-plugin/internal/tablecreate"
	"github.com/stokaro/ptah-ydb-plugin/internal/ydbdecl"
	"github.com/stokaro/ptah-ydb-plugin/schema"
)

// ServiceID names the external object planner.
const ServiceID = "ydb-external-objects"

// ChangeKinds are the change models the planner takes.
func ChangeKinds() []string { return []string{schema.ExternalSourceChange, schema.ExternalTableChange} }

// OperationKinds are the operation models the planner returns.
func OperationKinds() []string {
	return []string{schema.ExternalSourceOperation, schema.ExternalTableOperation, tablecreate.ColumnTTLOperationKind}
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
	changes, root, err := lower(ctx, request)
	if err != nil {
		return nil, err
	}
	if diagnostics := refusals(request.Capabilities, changes); len(diagnostics) > 0 {
		return &protocol.PlannedFeatures{Complete: true, Diagnostics: diagnostics}, nil
	}
	return contribute(ctx, request, root, changes)
}

// read is one change of the request, read.
type read struct {
	subject *protocol.ComparisonIdentity
	change  *protocol.Envelope
	specs   externals.ChangeSpecs
}

// lower reads the batch's changes and lowers each, in input order, and
// returns the database the batch runs in. An error is a request no owner
// could answer.
func lower(ctx context.Context, request *protocol.PlanFeaturesRequest) ([]lowered, string, error) {
	root := request.DatabasePath
	inputs := make([]read, 0, len(request.Changes))
	for _, input := range request.Changes {
		if err := ctx.Err(); err != nil {
			return nil, "", err
		}
		item, next, err := readInput(input, inputs, root)
		if err != nil {
			return nil, "", err
		}
		root = next
		inputs = append(inputs, item)
	}
	for i := range inputs {
		// Each change is read against the database the batch runs in, which
		// the request or one of its changes names.
		specs, err := externals.ReadChange(inputs[i].change)
		if err != nil {
			return nil, "", err
		}
		if specs.Database != root {
			if specs, err = readAgainst(inputs[i].change, root); err != nil {
				return nil, "", err
			}
		}
		inputs[i].specs = specs
	}
	replace := request.Capabilities[externals.CapabilityReplace]
	displaced := make(map[ydbdecl.Path]bool)
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
	return result, root, nil
}

// readInput checks input, one change of the request, against its normalization
// receipt and the changes read before it, and returns it read without its
// specs. root is the database the batch runs in as far as the request and the
// earlier changes name it; a change that names its database sets it, and
// readInput returns the root the next change is read against.
func readInput(input *protocol.PlanningChange, inputs []read, root string) (read, string, error) {
	kind := input.GetSubject().GetKind()
	owner, err := schema.NativeBoundOwner(kind, input.Subject)
	if err != nil {
		return read{}, root, err
	}
	if (kind == schema.ExternalSourceProperties) != (input.GetValue().GetValue().GetKind() == schema.ExternalSourceChange) ||
		!slices.Contains([]string{schema.ExternalSourceProperties, schema.ExternalTableProperties}, kind) {
		return read{}, root, fmt.Errorf("external object change names another kind of object")
	}
	captured, err := schema.NormalizeExternalChange(&protocol.ValueInput{Id: input.Value.Id, Representation: protocol.ValueInput_CHANGE, Value: input.Value.Value})
	if err != nil {
		return read{}, root, err
	}
	if !proto.Equal(captured, input.Value) || !proto.Equal(owner, captured.NormalizedOwner) {
		return read{}, root, fmt.Errorf("planning change has a forged normalization receipt or identity")
	}
	if slices.ContainsFunc(inputs, func(other read) bool { return provider.CompareIdentities(other.subject, input.Subject) == 0 }) {
		return read{}, root, fmt.Errorf("duplicate external object change")
	}
	parts, err := schema.ExternalOperands(captured.Value)
	if err != nil {
		return read{}, root, err
	}
	if database := parts.Database; database != "" {
		if root != "" && database != root {
			return read{}, root, fmt.Errorf("external object change targets another database")
		}
		root = database
	}
	if owner.Database != "" && root != "" && owner.Database != root {
		return read{}, root, fmt.Errorf("external object change has a foreign database identity")
	}
	return read{subject: input.Subject, change: captured.Value}, root, nil
}

func readAgainst(change *protocol.Envelope, root string) (externals.ChangeSpecs, error) {
	parts, err := schema.ExternalOperands(change)
	if err != nil {
		return externals.ChangeSpecs{}, err
	}
	result := externals.ChangeSpecs{Database: root, Before: parts.Before, After: parts.After}
	if change.GetKind() == schema.ExternalSourceChange {
		result.SourceBefore, result.SourceAfter, err = externals.SourcePair(parts.Before, parts.After, root)
	} else {
		result.TableBefore, result.TableAfter, err = externals.TablePair(parts.Before, parts.After, root)
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
	parts, err := schema.ExternalOperands(change)
	if err != nil {
		return nil, err
	}
	captured, err := schema.CaptureExternalChange(schema.ExternalTableProperties, "step", change.Owner, database, parts.Before, nil)
	if err != nil {
		return nil, err
	}
	return captured.Value, nil
}

func lowerSource(index int, input read, replace bool, root string) lowered {
	result := lowered{input: index, subject: input.subject, kind: schema.ExternalSourceChange}
	before, after := input.specs.SourceBefore, input.specs.SourceAfter
	secretPaths := func(source *ydbdecl.ExternalDataSource) []ydbdecl.Path {
		paths, err := externalpath.SecretPaths(source.Options, root)
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
		if source, pathErr := externalpath.SourcePath(root, before.DataSource); pathErr == nil {
			read = []ydbdecl.Path{source}
		}
	}
	if after != nil {
		if source, pathErr := externalpath.SourcePath(root, after.DataSource); pathErr == nil {
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
			// #nosec G115 -- input is a position in the request, which runtime.MaxMessageBytes bounds to 4 MiB
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
	found := make(map[int]error)
	written := make(map[ydbdecl.Path]step)
	sourceTypes := make(map[ydbdecl.Path]string)
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
// change. root is the database the batch runs in.
func contribute(ctx context.Context, request *protocol.PlanFeaturesRequest, root string, changes []lowered) (*protocol.PlannedFeatures, error) {
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
	byInput := make(map[int][]*protocol.StepIdentity)
	for index, planned := range steps {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		id := &protocol.StepIdentity{Owner: schema.ProviderID, Name: fmt.Sprintf("%s/%06d/%s", strings.ReplaceAll(noun(planned.subject), " ", "-"), index, strings.ToLower(planned.action.String()))}
		ids[index] = id
		if refusal, err := contributeStep(request, changes, planned, id, result, contribution); refusal != nil || err != nil {
			return refusal, err
		}
		byInput[planned.input] = append(byInput[planned.input], proto.CloneOf(id))
	}
	contribution.Dependencies = append(contribution.Dependencies, order(steps, ids)...)
	if refusal, err := keepTieredTTLs(request, root, steps, ids, changes, contribution, byInput); refusal != nil || err != nil {
		return refusal, err
	}
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

// contributeStep adds planned, a step of the change it carries out, to
// contribution as the step id, with its dependencies on the common steps and
// captured tables of request, and records a rewrite of the revokes it
// accounts for in result. A step ordered against access or secrets needs the
// planning.lifecycle target feature. A failure to order the step returns a
// refusal of its change.
func contributeStep(
	request *protocol.PlanFeaturesRequest,
	changes []lowered,
	planned step,
	id *protocol.StepIdentity,
	result *protocol.PlannedFeatures,
	contribution *protocol.PlanningContribution,
) (*protocol.PlannedFeatures, error) {
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
	below, err := objectplan.AfterCreationBelowDirectoryAccess(request, planned.action, planned.subject, id)
	if err != nil {
		return refused(changes[planned.input], err), nil
	}
	access.Dependencies = append(access.Dependencies, below...)
	contribution.Dependencies = append(contribution.Dependencies, edges...)
	facts := value.OperationSemantics
	// Every statement of the batch asks for the lifecycle phase, which
	// orders operations around common changes: a table dropped
	// before the data source it reads is created again after it, and the
	// batch orders its own steps there.
	step := &protocol.PlanningStep{Id: id, Operation: &protocol.PlanningOperation{Role: protocol.NativeOperation_STATEMENT, Value: value, Phase: protocol.PlanningOperation_LIFECYCLE},
		Metadata: &protocol.PlanningMetadata{Transaction: protocol.PlanningMetadata_FORBIDDEN, Placement: protocol.PlanningMetadata_EARLY, Impact: facts.Impact, Reason: facts.Reason, Effects: stepEffects(planned)}}
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
	return nil, nil
}

// stepEffects returns the effects of planned: on its object and the object's
// slot; a read of each secret it names and of the secret's slot; and, for a
// table step other than a drop, a read of each data source it reads.
func stepEffects(planned step) []*protocol.PlanningEffect {
	effects := []*protocol.PlanningEffect{{Subject: proto.CloneOf(planned.subject), Action: planned.action}, {Subject: objectplan.Slot(planned.subject), Action: planned.action}}
	for _, path := range planned.secrets {
		secret := schema.ExternalSourceSubject(path)
		secret.Kind = schema.SecretProperties
		effects = append(effects, &protocol.PlanningEffect{Subject: secret, Action: protocol.PlanningEffect_READ},
			&protocol.PlanningEffect{Subject: objectplan.Slot(secret), Action: protocol.PlanningEffect_READ})
	}
	if !planned.source() && planned.action != protocol.PlanningEffect_DROP {
		for _, source := range planned.sources {
			effects = append(effects, &protocol.PlanningEffect{Subject: schema.ExternalSourceSubject(source), Action: protocol.PlanningEffect_READ})
		}
	}
	return effects
}

func refused(change lowered, err error) *protocol.PlannedFeatures {
	// #nosec G115 -- input is a position in the request, which runtime.MaxMessageBytes bounds to 4 MiB
	return &protocol.PlannedFeatures{Complete: true, Diagnostics: []*protocol.PlanningDiagnostic{{Code: protocol.PlanningDiagnostic_UNSUPPORTED_FEATURE, Kind: change.kind, Object: display(change.subject),
		Message: err.Error(), Input: &protocol.PlanningDiagnostic_Change{Change: uint32(change.input)}}}}
}

// keepTieredTTLs carries the TTL of each column table the plan keeps past
// every data source the batch replaces that the TTL moves rows to: the TTL is
// reset before the first replacement and set again after the last, as the
// host's embedded engine carries it. YDB refuses to drop or replace a data
// source a table's TTL names (`Other entities depend on this data source`,
// measured on 25.1.4.7 and 26.2.1.14). A replacement by CREATE OR REPLACE is
// treated alike. The steps join the receipt of the first replaced source the
// TTL names, and the source's strategy says so. A table whose TTL the plan
// changes is left to the table's own steps, which read the sources their TTLs
// name, and a table the plan drops reads the sources of its TTL itself. A
// target without the tiered_ttl capability refuses the replacement, since it
// could not set the TTL again, and a table left with a TTL that names a data
// source the batch drops refuses the drop, in the embedded engine's words.
func keepTieredTTLs(request *protocol.PlanFeaturesRequest, root string, steps []step, ids []*protocol.StepIdentity, changes []lowered,
	contribution *protocol.PlanningContribution, byInput map[int][]*protocol.StepIdentity,
) (*protocol.PlannedFeatures, error) {
	replaced, dropped := sourceSteps(steps)
	if len(replaced)+len(dropped) == 0 {
		return nil, nil
	}
	if refusal, err := refuseDroppedTTLSources(request, root, steps, changes, dropped); refusal != nil || err != nil {
		return refusal, err
	}
	number := 0
	for _, table := range request.GetTables() {
		kept, err := tablecreate.KeptTieredTTL(number, root, table)
		if err != nil {
			return nil, err
		}
		around := replacedSources(kept, replaced)
		if len(around) == 0 {
			continue
		}
		if !request.GetCapabilities()[tablecreate.TieredTTLCapability] {
			return refused(changes[steps[around[0]].input], fmt.Errorf("replacing external data source %s means setting the TTL of column table %s again, "+
				"which requires target capability %s, unavailable on this ydb target%s", display(steps[around[0]].subject),
				ydbdecl.ExternalDisplay(kept.Table.GetSchema().GetSource(), kept.Table.GetName().GetSource()), tablecreate.TieredTTLCapability,
				featureflags.Clause(tablecreate.TieredTTLCapability))), nil
		}
		number++
		contribution.Steps = append(contribution.Steps, kept.Reset, kept.Set)
		for _, index := range around {
			contribution.Dependencies = append(contribution.Dependencies,
				&protocol.PlanningDependency{Before: proto.CloneOf(kept.Reset.Id), After: proto.CloneOf(ids[index])},
				&protocol.PlanningDependency{Before: proto.CloneOf(ids[index]), After: proto.CloneOf(kept.Set.Id)})
			change := &changes[steps[index].input]
			if !strings.Contains(change.strategy, keptTTLStrategy) {
				change.strategy += keptTTLStrategy
			}
		}
		input := steps[around[0]].input
		byInput[input] = append(byInput[input], proto.CloneOf(kept.Reset.Id), proto.CloneOf(kept.Set.Id))
	}
	return nil, nil
}

// sourceSteps are the data sources the batch replaces and the ones it drops,
// each by the position of its step.
func sourceSteps(steps []step) (replaced, dropped map[ydbdecl.Path]int) {
	replaced, dropped = make(map[ydbdecl.Path]int), make(map[ydbdecl.Path]int)
	for index, planned := range steps {
		switch {
		case planned.source() && planned.action == protocol.PlanningEffect_ALTER:
			replaced[planned.path()] = index
		case planned.source() && planned.action == protocol.PlanningEffect_DROP:
			dropped[planned.path()] = index
		}
	}
	return replaced, dropped
}

// replacedSources are the positions of the steps that replace a data source
// kept, the TTL of a table the plan keeps, names, in the order the TTL names
// them. A nil kept names none.
func replacedSources(kept *tablecreate.KeptTTL, replaced map[ydbdecl.Path]int) []int {
	if kept == nil {
		return nil
	}
	var around []int
	for _, source := range kept.Sources {
		if index, found := replaced[source]; found {
			around = append(around, index)
		}
	}
	return around
}

// refuseDroppedTTLSources refuses the drop of a data source that a TTL the
// plan leaves a table with names, in the embedded engine's words.
func refuseDroppedTTLSources(request *protocol.PlanFeaturesRequest, root string, steps []step, changes []lowered, dropped map[ydbdecl.Path]int) (*protocol.PlannedFeatures, error) {
	for _, table := range request.GetTables() {
		ttl, err := tablecreate.PlannedColumnTTL(table)
		if err != nil {
			return nil, err
		}
		for _, source := range tablecreate.TTLSources(root, ttl) {
			if index, found := dropped[source]; found {
				return refused(changes[steps[index].input], fmt.Errorf("table %s: its TTL reads external data source %s, which the plan drops",
					ydbdecl.ExternalDisplay(table.GetSubject().GetSchema().GetSource(), table.GetSubject().GetName().GetSource()), display(steps[index].subject))), nil
			}
		}
	}
	return nil, nil
}

// keptTTLStrategy ends the strategy of a data source replacement around which
// the plan carries a column table's TTL.
const keptTTLStrategy = "; the TTL of each column table that moves rows to it is reset before and set again after"

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
