package schema

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"path"
	"slices"
	"strings"
	"unicode/utf8"

	native "github.com/stokaro/ptah-ydb-plugin/internal/nativeprotocol"
	"github.com/stokaro/ptah-ydb-plugin/internal/workload"
	"github.com/stokaro/ptah/provider-sdk/modeljson"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/stokaro/ptah/provider-sdk/provider"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

const (
	// ResourcePoolProperties identifies one resource pool of a database.
	ResourcePoolProperties = modelNamespace + "resource-pool"
	// ResourcePoolClassifierProperties identifies one resource pool
	// classifier of a database.
	ResourcePoolClassifierProperties = modelNamespace + "resource-pool-classifier"
	// ResourcePoolChange identifies a complete before/after change of one
	// resource pool.
	ResourcePoolChange = modelNamespace + "resource-pool-change"
	// ResourcePoolClassifierChange identifies a complete before/after change
	// of one classifier.
	ResourcePoolClassifierChange = modelNamespace + "resource-pool-classifier-change"
	// WorkloadOperation identifies the executable change of the pools and
	// the classifiers of one database.
	//
	// One operation carries every workload change of a plan. YDB keeps one
	// classifier per rank, so two classifiers trading ranks run as a drop, a
	// move and a creation in that order, and the host runs a step that drops
	// an object after every step that creates one: the order holds only
	// inside one step.
	WorkloadOperation = modelNamespace + "workload-operation"
	// WorkloadOwnerKind is the kind of a workload operation's owner: the
	// workload management of the session's database, which owns every pool
	// and classifier of it.
	WorkloadOwnerKind = "ydb.workload"
)

// workloadDefinition is what both models of a workload object say beyond
// their message.
const workloadDefinition = "database-scoped: the owner names the database and the object, no directory; " +
	"names are printable ASCII without a slash or a space; an unset pool setting (YDB's -1) is absent and zero is a limit; " +
	"the pool default takes neither concurrent_query_limit nor database_load_cpu_threshold and is never created or dropped; " +
	"a classifier names a pool and a rank from 0; access is not read"

// The reasons a workload change gives for its impact, the host's words.
const (
	createPoolReason       = "CREATE RESOURCE POOL adds a resource pool; it does not remove data or tighten constraints"
	alterPoolReason        = "ALTER RESOURCE POOL changes the limits of running and queued queries"
	dropPoolReason         = "DROP RESOURCE POOL runs the queries a classifier sends to the pool in the pool default"
	classifierReason       = "resource pool classifier settings change which pool receives matching queries"
	dropClassifierReason   = "DROP RESOURCE POOL CLASSIFIER sends its member's queries to another classifier's pool or to the pool default"
	workloadAccessReason   = "A workload change grants and revokes no permission"
	explicitRemovalRefusal = "removing a database-wide workload object requires an explicit operation, not source omission"
)

// IsWorkloadObject reports a database-wide workload object: a resource pool
// or a classifier. Neither lives in a directory, so neither occupies a scheme
// path the way a standalone object does.
func IsWorkloadObject(kind string) bool {
	return kind == ResourcePoolProperties || kind == ResourcePoolClassifierProperties
}

// ValidateWorkloadOwner checks the owner of a workload object: the database,
// no directory, and a name YDB takes for a pool or a classifier.
func ValidateWorkloadOwner(owner *protocol.Reference) error {
	if owner.GetEngine() != "ydb" || !IsWorkloadObject(owner.GetKind()) || owner.GetCatalog() != "" || owner.GetSchema() != "" ||
		len(owner.GetPath())+len(owner.GetOverload()) != 0 || !workload.ValidName(owner.GetName()) {
		return fmt.Errorf("workload objects require exact database-scoped YDB identities")
	}
	return validDatabase(owner.GetDatabase())
}

func validDatabase(database string) error {
	if database != "" && (!utf8.ValidString(database) || !path.IsAbs(database) || path.Clean(database) != database || strings.ContainsAny(database, "\\\x00")) {
		return fmt.Errorf("invalid YDB database path")
	}
	return nil
}

// WorkloadBoundOwner returns the owner a comparison subject of a workload
// object of kind names, each part spelled exactly.
func WorkloadBoundOwner(kind string, subject *protocol.ComparisonIdentity) (*protocol.Reference, error) {
	if err := provider.ValidateComparisonIdentity(subject); err != nil {
		return nil, err
	}
	if subject.Kind != kind || subject.GetCatalog().GetSource() != "" || subject.GetSchema().GetSource() != "" || subject.GetParent().GetSource() != "" ||
		len(subject.Path)+len(subject.Signature) != 0 {
		return nil, fmt.Errorf("workload objects require exact database-scoped YDB identities")
	}
	for _, part := range []*protocol.ComparisonPart{subject.Database, subject.Name} {
		if part.GetSource() != part.GetNormalized() {
			return nil, fmt.Errorf("YDB identifiers must retain exact spelling")
		}
	}
	owner := &protocol.Reference{Engine: "ydb", Kind: kind, Database: subject.GetDatabase().GetNormalized(), Name: subject.GetName().GetNormalized()}
	return owner, ValidateWorkloadOwner(owner)
}

// PoolSpec reads the settings of a resource pool model.
func PoolSpec(settings *native.ResourcePoolSettings) workload.PoolSpec {
	return workload.PoolSpec{ConcurrentQueryLimit: settings.ConcurrentQueryLimit, QueueSize: settings.QueueSize, DatabaseLoadCPUThreshold: settings.DatabaseLoadCpuThreshold,
		QueryMemoryLimitPercentPerNode: settings.QueryMemoryLimitPercentPerNode, QueryCPULimitPercentPerNode: settings.QueryCpuLimitPercentPerNode,
		TotalCPULimitPercentPerNode: settings.TotalCpuLimitPercentPerNode, ResourceWeight: settings.ResourceWeight}
}

// PoolSettings writes a pool's settings as the resource pool model.
func PoolSettings(spec workload.PoolSpec) *native.ResourcePoolSettings {
	return &native.ResourcePoolSettings{ConcurrentQueryLimit: spec.ConcurrentQueryLimit, QueueSize: spec.QueueSize, DatabaseLoadCpuThreshold: spec.DatabaseLoadCPUThreshold,
		QueryMemoryLimitPercentPerNode: spec.QueryMemoryLimitPercentPerNode, QueryCpuLimitPercentPerNode: spec.QueryCPULimitPercentPerNode,
		TotalCpuLimitPercentPerNode: spec.TotalCPULimitPercentPerNode, ResourceWeight: spec.ResourceWeight}
}

// ClassifierSpec reads the settings of a classifier model.
func ClassifierSpec(settings *native.ResourcePoolClassifierSettings) workload.ClassifierSpec {
	return workload.ClassifierSpec{ResourcePool: settings.GetResourcePool(), MemberName: settings.GetMemberName(), Rank: settings.GetRank()}
}

// ClassifierSettings writes a classifier as the classifier model.
func ClassifierSettings(spec workload.ClassifierSpec) *native.ResourcePoolClassifierSettings {
	return &native.ResourcePoolClassifierSettings{ResourcePool: spec.ResourcePool, MemberName: spec.MemberName, Rank: spec.Rank}
}

// validateWorkload holds a workload object's settings to the rules YDB keeps
// for its name: the ranges and shapes of a pool, the pool `default`'s
// unlimited settings, and a classifier's pool and rank.
func validateWorkload(owner *protocol.Reference, value proto.Message) error {
	switch settings := value.(type) {
	case *native.ResourcePoolSettings:
		if owner.GetKind() != ResourcePoolProperties {
			return fmt.Errorf("resource pool settings belong to a resource pool")
		}
		return workload.ValidatePool(owner.GetName(), PoolSpec(settings))
	case *native.ResourcePoolClassifierSettings:
		if owner.GetKind() != ResourcePoolClassifierProperties {
			return fmt.Errorf("classifier settings belong to a classifier")
		}
		return workload.ValidateClassifier(owner.GetName(), ClassifierSpec(settings))
	default:
		return fmt.Errorf("unexpected YDB workload model")
	}
}

// CaptureWorkloadObservation writes a workload object the database holds as
// its observed model. The owner names the database it was read from.
func CaptureWorkloadObservation(owner *protocol.Reference, settings proto.Message) (*protocol.Envelope, error) {
	if !IsWorkloadObject(owner.GetKind()) {
		return nil, fmt.Errorf("unsupported YDB workload model")
	}
	return capture(owner.GetKind(), owner, settings)
}

// CaptureWorkloadIntent writes a declared workload object as its desired
// model under owner.
func CaptureWorkloadIntent(owner *protocol.Reference, settings proto.Message) (*protocol.Envelope, error) {
	if !IsWorkloadObject(owner.GetKind()) {
		return nil, fmt.Errorf("unsupported YDB workload model")
	}
	return convertedEnvelope(&protocol.Envelope{ProviderId: ProviderID, Kind: owner.GetKind(), Owner: proto.CloneOf(owner)}, NativeDesiredModel(owner.GetKind()), settings, NormalizeDesired)
}

// WorkloadPool reads a resource pool envelope of either representation.
func WorkloadPool(value *protocol.Envelope) (workload.PoolSpec, error) {
	var settings native.ResourcePoolSettings
	if err := workloadSettings(value, ResourcePoolProperties, &settings); err != nil {
		return workload.PoolSpec{}, err
	}
	return PoolSpec(&settings), nil
}

// WorkloadClassifier reads a classifier envelope of either representation.
func WorkloadClassifier(value *protocol.Envelope) (workload.ClassifierSpec, error) {
	var settings native.ResourcePoolClassifierSettings
	if err := workloadSettings(value, ResourcePoolClassifierProperties, &settings); err != nil {
		return workload.ClassifierSpec{}, err
	}
	return ClassifierSpec(&settings), nil
}

func workloadSettings(value *protocol.Envelope, kind string, target proto.Message) error {
	if value.GetKind() != kind {
		return fmt.Errorf("expected a YDB %s", modelDisplayName(kind))
	}
	normalize := Normalize
	if value.GetDefinitionSha256() == NativeDesiredModel(kind).GetDefinitionSha256() {
		normalize = NormalizeDesired
	}
	normalized, err := normalize(value)
	if err != nil {
		return err
	}
	return protojson.Unmarshal(normalized.GetPayload(), target)
}

var (
	poolChanges = newObjectChanges(objectChanges{kind: ResourcePoolProperties, change: ResourcePoolChange, noun: "resource pool",
		semantics: poolSemantics,
		definition: map[string]any{
			"semantics":  "create: additive; alter and drop: behavioral; never removes state; access unchanged; never replaces owner",
			"comparison": "every setting compared exactly; an unset setting and a zero one differ",
			"validation": "a pair that holds no change is refused; the pool default is never created or dropped",
		}})
	classifierChanges = newObjectChanges(objectChanges{kind: ResourcePoolClassifierProperties, change: ResourcePoolClassifierChange, noun: "resource pool classifier",
		semantics: classifierSemantics,
		definition: map[string]any{
			"semantics":  "create, alter and drop: behavioral; never removes state; access unchanged; never replaces owner",
			"comparison": "pool, member and rank compared exactly",
			"validation": "a pair that holds no change is refused",
		}})
)

// ResourcePoolChangeModel defines a resource pool change.
func ResourcePoolChangeModel() *protocol.ValueModel { return proto.CloneOf(poolChanges.changeModel) }

// ResourcePoolClassifierChangeModel defines a classifier change.
func ResourcePoolClassifierChangeModel() *protocol.ValueModel {
	return proto.CloneOf(classifierChanges.changeModel)
}

// CaptureResourcePoolChange retains complete operands under one owner;
// either may be nil for an established absence.
func CaptureResourcePoolChange(id string, owner *protocol.Reference, before, after *protocol.Envelope) (*protocol.NormalizedValue, error) {
	return poolChanges.capture(id, owner, before, after)
}

// CaptureResourcePoolClassifierChange retains complete operands under one
// owner; either may be nil for an established absence.
func CaptureResourcePoolClassifierChange(id string, owner *protocol.Reference, before, after *protocol.Envelope) (*protocol.NormalizedValue, error) {
	return classifierChanges.capture(id, owner, before, after)
}

// NormalizeWorkloadChange reconstructs a pool's or a classifier's change
// facts from its operands.
func NormalizeWorkloadChange(input *protocol.ValueInput) (*protocol.NormalizedValue, error) {
	if input.GetValue().GetKind() == ResourcePoolClassifierChange {
		return classifierChanges.normalizeChange(input)
	}
	return poolChanges.normalizeChange(input)
}

// PoolChangeOf reads a normalized pool change as the pool's change.
func PoolChangeOf(change *protocol.Envelope) (workload.PoolChange, error) {
	before, after, err := poolChanges.operands(change)
	if err != nil {
		return workload.PoolChange{}, err
	}
	return poolOperands(change.GetOwner().GetName(), before, after)
}

// ClassifierChangeOf reads a normalized classifier change as the
// classifier's change.
func ClassifierChangeOf(change *protocol.Envelope) (workload.ClassifierChange, error) {
	before, after, err := classifierChanges.operands(change)
	if err != nil {
		return workload.ClassifierChange{}, err
	}
	return classifierOperands(change.GetOwner().GetName(), before, after)
}

func poolOperands(name string, before, after *protocol.Envelope) (workload.PoolChange, error) {
	change := workload.PoolChange{Name: name}
	for _, operand := range []struct {
		value  *protocol.Envelope
		target **workload.PoolSpec
	}{{before, &change.Before}, {after, &change.After}} {
		if operand.value == nil {
			continue
		}
		spec, err := WorkloadPool(operand.value)
		if err != nil {
			return workload.PoolChange{}, err
		}
		*operand.target = &spec
	}
	return change, nil
}

func classifierOperands(name string, before, after *protocol.Envelope) (workload.ClassifierChange, error) {
	change := workload.ClassifierChange{Name: name}
	for _, operand := range []struct {
		value  *protocol.Envelope
		target **workload.ClassifierSpec
	}{{before, &change.Before}, {after, &change.After}} {
		if operand.value == nil {
			continue
		}
		spec, err := WorkloadClassifier(operand.value)
		if err != nil {
			return workload.ClassifierChange{}, err
		}
		*operand.target = &spec
	}
	return change, nil
}

func poolSemantics(before, after *protocol.Envelope) (*protocol.ChangeSemantics, error) {
	change, err := poolOperands(operandName(before, after), before, after)
	if err != nil {
		return nil, err
	}
	return poolImpact(change)
}

// operandName is the name of the object a change's operands describe.
func operandName(before, after *protocol.Envelope) string {
	if before != nil {
		return before.GetOwner().GetName()
	}
	return after.GetOwner().GetName()
}

func poolImpact(change workload.PoolChange) (*protocol.ChangeSemantics, error) {
	semantics := &protocol.ChangeSemantics{ReplacesOwner: new(false), RemovesState: new(false), Access: protocol.ChangeSemantics_UNCHANGED, AccessReason: workloadAccessReason}
	switch {
	case change.Name == workload.DefaultPool && (change.Before == nil || change.After == nil):
		return nil, fmt.Errorf("resource pool %q: the default pool cannot be created or dropped", change.Name)
	case change.Before == nil:
		semantics.Impact, semantics.Reason = protocol.ChangeSemantics_ADDITIVE, createPoolReason
	case change.After == nil:
		semantics.Impact, semantics.Reason = protocol.ChangeSemantics_BEHAVIORAL, dropPoolReason
	case workload.PoolsEqual(*change.After, *change.Before):
		return nil, fmt.Errorf("resource pool operands contain no change")
	default:
		semantics.Impact, semantics.Reason = protocol.ChangeSemantics_BEHAVIORAL, alterPoolReason
	}
	return semantics, nil
}

func classifierSemantics(before, after *protocol.Envelope) (*protocol.ChangeSemantics, error) {
	change, err := classifierOperands(operandName(before, after), before, after)
	if err != nil {
		return nil, err
	}
	return classifierImpact(change)
}

func classifierImpact(change workload.ClassifierChange) (*protocol.ChangeSemantics, error) {
	semantics := &protocol.ChangeSemantics{ReplacesOwner: new(false), RemovesState: new(false), Access: protocol.ChangeSemantics_UNCHANGED, AccessReason: workloadAccessReason,
		Impact: protocol.ChangeSemantics_BEHAVIORAL, Reason: classifierReason}
	switch {
	case change.After == nil:
		semantics.Reason = dropClassifierReason
	case change.Before != nil && *change.Before == *change.After:
		return nil, fmt.Errorf("resource pool classifier operands contain no change")
	}
	return semantics, nil
}

// ExplicitWorkloadRemoval says why a comparison leaves undecided a workload
// object the desired side establishes as absent: the host removes a
// database-wide workload object only through an operation that names it.
func ExplicitWorkloadRemoval() string { return explicitRemovalRefusal }

// WorkloadOperationModel defines the workload operation and its
// preconditions.
func WorkloadOperationModel() *protocol.ValueModel { return proto.CloneOf(workloadOperationModel) }

var workloadOperationModel = buildWorkloadOperationModel()

func buildWorkloadOperationModel() *protocol.ValueModel {
	definition, err := json.Marshal(map[string]any{
		"kind": WorkloadOperation, "representation": "operation", "schema_version": 1, "encoding": "json",
		"owner": "the workload management of the session's database: kind " + WorkloadOwnerKind + ", name workload, no directory",
		"payload": map[string]any{"required": []string{"pools", "classifiers"}, "additionalProperties": false,
			"pools": map[string]any{"meaning": "one change per resource pool, in name order",
				"name": "the pool's name", "before": map[string]any{"meaning": "the pool as the database holds it; absent for a creation", "model": json.RawMessage(model(ResourcePoolProperties).DefinitionJson)},
				"after": map[string]any{"meaning": "the declared pool; absent for a removal", "model": json.RawMessage(NativeDesiredModel(ResourcePoolProperties).DefinitionJson)}},
			"classifiers": map[string]any{"meaning": "one change per classifier, in name order",
				"name": "the classifier's name", "before": map[string]any{"meaning": "the classifier as the database holds it; absent for a creation", "model": json.RawMessage(model(ResourcePoolClassifierProperties).DefinitionJson)},
				"after": map[string]any{"meaning": "the declared classifier; absent for a removal", "model": json.RawMessage(NativeDesiredModel(ResourcePoolClassifierProperties).DefinitionJson)}},
			"changes": "at least one change in all"},
		"semantics":    "a pool creation alone: additive; any other change: behavioral; never removes state; access unchanged; never replaces the owner",
		"execution":    "YQL classifier removals, pool removals, pool creations and changes, classifier changes, then classifier creations, each in name order, one query each, outside SQL transactions; a classifier moving to a rank another changed classifier leaves is dropped and created; no automatic retry",
		"precondition": "before is each object as the plan read it, or its established absence; verified from .sys/resource_pools and .sys/resource_pool_classifiers immediately before dispatch; the API has no atomic compare-and-set",
		"confirmation": "read back each declared object, or its absence after a removal",
		"database":     "a nonempty owner database must equal the selected session database; empty selects that session database",
	})
	if err != nil {
		panic(err)
	}
	canonical, err := modeljson.Canonical(definition)
	if err != nil {
		panic(err)
	}
	digest := sha256.Sum256(canonical)
	return &protocol.ValueModel{Kind: WorkloadOperation, Representation: protocol.ValueInput_OPERATION, SchemaVersion: 1, Encoding: "json",
		DefinitionJson: canonical, DefinitionSha256: hex.EncodeToString(digest[:]), CanonicalFormat: WorkloadOperation + "/json-v1"}
}

// WorkloadOwner is the owner of the workload operation of database, empty
// for the session's.
func WorkloadOwner(database string) *protocol.Reference {
	return &protocol.Reference{Engine: "ydb", Kind: WorkloadOwnerKind, Database: database, Name: "workload"}
}

// WorkloadChanges is what one workload operation carries.
type WorkloadChanges struct {
	Pools       []workload.PoolChange
	Classifiers []workload.ClassifierChange
}

type workloadEntry struct {
	Name   string          `json:"name"`
	Before json.RawMessage `json:"before,omitempty"`
	After  json.RawMessage `json:"after,omitempty"`
}

type workloadPayload struct {
	Pools       []workloadEntry `json:"pools"`
	Classifiers []workloadEntry `json:"classifiers"`
}

// CaptureWorkloadOperation writes changes as one normalized workload
// operation of database, empty for the session's.
func CaptureWorkloadOperation(id, database string, changes WorkloadChanges) (*protocol.NormalizedValue, error) {
	encoded, err := encodeWorkload(changes)
	if err != nil {
		return nil, err
	}
	definition := WorkloadOperationModel()
	return NormalizeWorkloadOperation(&protocol.ValueInput{Id: id, Representation: protocol.ValueInput_OPERATION, Value: &protocol.Envelope{
		ProviderId: ProviderID, Kind: definition.Kind, Owner: WorkloadOwner(database), SchemaVersion: definition.SchemaVersion,
		Encoding: definition.Encoding, DefinitionSha256: definition.DefinitionSha256, Payload: encoded}})
}

func encodeWorkload(changes WorkloadChanges) ([]byte, error) {
	payload := workloadPayload{Pools: []workloadEntry{}, Classifiers: []workloadEntry{}}
	for _, change := range changes.Pools {
		entry := workloadEntry{Name: change.Name}
		var err error
		if change.Before != nil {
			if entry.Before, err = protojson.Marshal(PoolSettings(*change.Before)); err != nil {
				return nil, err
			}
		}
		if change.After != nil {
			if entry.After, err = protojson.Marshal(PoolSettings(*change.After)); err != nil {
				return nil, err
			}
		}
		payload.Pools = append(payload.Pools, entry)
	}
	for _, change := range changes.Classifiers {
		entry := workloadEntry{Name: change.Name}
		var err error
		if change.Before != nil {
			if entry.Before, err = protojson.Marshal(ClassifierSettings(*change.Before)); err != nil {
				return nil, err
			}
		}
		if change.After != nil {
			if entry.After, err = protojson.Marshal(ClassifierSettings(*change.After)); err != nil {
				return nil, err
			}
		}
		payload.Classifiers = append(payload.Classifiers, entry)
	}
	return json.Marshal(payload)
}

// NormalizeWorkloadOperation validates every operand against the workload
// models, reconstructs the operation's safety facts and refuses an operation
// that changes nothing or that YDB cannot carry out.
func NormalizeWorkloadOperation(input *protocol.ValueInput) (*protocol.NormalizedValue, error) {
	value, definition := input.GetValue(), WorkloadOperationModel()
	if input.GetRepresentation() != protocol.ValueInput_OPERATION || value.GetProviderId() != ProviderID || value.GetKind() != definition.Kind ||
		value.GetSchemaVersion() != definition.SchemaVersion || value.GetEncoding() != definition.Encoding || value.GetDefinitionSha256() != definition.DefinitionSha256 {
		return nil, fmt.Errorf("workload operation does not match its negotiated definition")
	}
	owner := value.GetOwner()
	if !proto.Equal(owner, WorkloadOwner(owner.GetDatabase())) {
		return nil, fmt.Errorf("workload operation requires the database's workload owner")
	}
	if err := validDatabase(owner.GetDatabase()); err != nil {
		return nil, err
	}
	changes, err := decodeWorkload(owner.GetDatabase(), value.GetPayload())
	if err != nil {
		return nil, err
	}
	if _, err := workload.Statements(changes.Pools, changes.Classifiers); err != nil {
		return nil, err
	}
	semantics, err := workloadSemantics(changes)
	if err != nil {
		return nil, err
	}
	encoded, err := encodeWorkload(changes)
	if err != nil {
		return nil, err
	}
	canonical, err := modeljson.Canonical(encoded)
	if err != nil {
		return nil, err
	}
	result := proto.CloneOf(value)
	result.Payload = canonical
	return &protocol.NormalizedValue{Id: input.GetId(), Value: result, NormalizedOwner: proto.CloneOf(result.Owner), CanonicalFormat: definition.CanonicalFormat,
		CanonicalProjection: bytes.Clone(canonical), OperationSemantics: semantics}, nil
}

// DecodeWorkloadOperation returns the changes a workload operation carries,
// each list in name order.
func DecodeWorkloadOperation(value *protocol.Envelope) (WorkloadChanges, error) {
	normalized, err := NormalizeWorkloadOperation(&protocol.ValueInput{Id: "decode", Representation: protocol.ValueInput_OPERATION, Value: value})
	if err != nil {
		return WorkloadChanges{}, err
	}
	return decodeWorkload(normalized.Value.Owner.GetDatabase(), normalized.Value.Payload)
}

func decodeWorkload(database string, raw []byte) (WorkloadChanges, error) {
	canonical, err := modeljson.Canonical(raw)
	if err != nil {
		return WorkloadChanges{}, err
	}
	if err := workloadKeys(canonical, true, "pools", "classifiers"); err != nil {
		return WorkloadChanges{}, err
	}
	var payload struct {
		Pools       []json.RawMessage `json:"pools"`
		Classifiers []json.RawMessage `json:"classifiers"`
	}
	if err := json.Unmarshal(canonical, &payload); err != nil || len(payload.Pools)+len(payload.Classifiers) == 0 {
		return WorkloadChanges{}, fmt.Errorf("workload operation requires at least one change")
	}
	var changes WorkloadChanges
	pool := func(name string, raw json.RawMessage, representation protocol.ValueInput_Representation) (*workload.PoolSpec, error) {
		settings, err := workloadOperand(database, ResourcePoolProperties, name, raw, representation, &native.ResourcePoolSettings{})
		if err != nil || settings == nil {
			return nil, err
		}
		spec := PoolSpec(settings.(*native.ResourcePoolSettings))
		return &spec, nil
	}
	classifier := func(name string, raw json.RawMessage, representation protocol.ValueInput_Representation) (*workload.ClassifierSpec, error) {
		settings, err := workloadOperand(database, ResourcePoolClassifierProperties, name, raw, representation, &native.ResourcePoolClassifierSettings{})
		if err != nil || settings == nil {
			return nil, err
		}
		spec := ClassifierSpec(settings.(*native.ResourcePoolClassifierSettings))
		return &spec, nil
	}
	names := func(entries []json.RawMessage) ([]workloadEntry, error) {
		var decoded []workloadEntry
		for _, raw := range entries {
			if err := workloadKeys(raw, false, "name", "before", "after"); err != nil {
				return nil, err
			}
			var entry workloadEntry
			if err := json.Unmarshal(raw, &entry); err != nil || entry.Name == "" {
				return nil, fmt.Errorf("workload operation requires an object name")
			}
			if entry.Before == nil && entry.After == nil {
				return nil, fmt.Errorf("workload operation change of %q has no operand", entry.Name)
			}
			if len(decoded) > 0 && strings.Compare(decoded[len(decoded)-1].Name, entry.Name) >= 0 {
				return nil, fmt.Errorf("workload operation changes must name each object once, in name order")
			}
			decoded = append(decoded, entry)
		}
		return decoded, nil
	}
	pools, err := names(payload.Pools)
	if err != nil {
		return WorkloadChanges{}, err
	}
	for _, entry := range pools {
		change := workload.PoolChange{Name: entry.Name}
		if change.Before, err = pool(entry.Name, entry.Before, protocol.ValueInput_OBSERVED); err != nil {
			return WorkloadChanges{}, err
		}
		if change.After, err = pool(entry.Name, entry.After, protocol.ValueInput_DESIRED); err != nil {
			return WorkloadChanges{}, err
		}
		changes.Pools = append(changes.Pools, change)
	}
	classifiers, err := names(payload.Classifiers)
	if err != nil {
		return WorkloadChanges{}, err
	}
	for _, entry := range classifiers {
		change := workload.ClassifierChange{Name: entry.Name}
		if change.Before, err = classifier(entry.Name, entry.Before, protocol.ValueInput_OBSERVED); err != nil {
			return WorkloadChanges{}, err
		}
		if change.After, err = classifier(entry.Name, entry.After, protocol.ValueInput_DESIRED); err != nil {
			return WorkloadChanges{}, err
		}
		changes.Classifiers = append(changes.Classifiers, change)
	}
	return changes, nil
}

// workloadOperand normalizes one operand of a workload operation through the
// model of its representation, nil for an absent one.
func workloadOperand(database, kind, name string, raw json.RawMessage, representation protocol.ValueInput_Representation, target proto.Message) (proto.Message, error) {
	if raw == nil {
		return nil, nil
	}
	definition, normalize := model(kind), Normalize
	if representation == protocol.ValueInput_DESIRED {
		definition, normalize = NativeDesiredModel(kind), NormalizeDesired
	}
	normalized, err := normalize(&protocol.Envelope{ProviderId: ProviderID, Kind: kind, Owner: &protocol.Reference{Engine: "ydb", Kind: kind, Database: database, Name: name},
		SchemaVersion: definition.SchemaVersion, Encoding: definition.Encoding, DefinitionSha256: definition.DefinitionSha256, Payload: bytes.Clone(raw)})
	if err != nil {
		return nil, err
	}
	if err := protojson.Unmarshal(normalized.Payload, target); err != nil {
		return nil, fmt.Errorf("invalid workload operation operand")
	}
	return target, nil
}

// workloadKeys refuses a JSON object with a key outside allowed or a null
// value; required asks for every allowed key.
func workloadKeys(raw json.RawMessage, required bool, allowed ...string) error {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil || fields == nil {
		return fmt.Errorf("invalid workload operation payload")
	}
	for key, field := range fields {
		if !slices.Contains(allowed, key) {
			return fmt.Errorf("workload operation has an unknown key %q", key)
		}
		if bytes.Equal(field, []byte("null")) {
			return fmt.Errorf("workload operation key %q cannot be null", key)
		}
	}
	if required && len(fields) != len(allowed) {
		return fmt.Errorf("workload operation requires the keys %s", strings.Join(allowed, " and "))
	}
	return nil
}

// workloadSemantics derives an operation's facts from its changes: a pool
// creation alone adds, anything else changes where or how queries run.
func workloadSemantics(changes WorkloadChanges) (*protocol.OperationSemantics, error) {
	semantics := &protocol.OperationSemantics{Impact: protocol.ChangeSemantics_ADDITIVE, Access: protocol.ChangeSemantics_UNCHANGED, AccessReason: workloadAccessReason}
	var reasons []string
	add := func(facts *protocol.ChangeSemantics) {
		if facts.Impact == protocol.ChangeSemantics_BEHAVIORAL {
			semantics.Impact = protocol.ChangeSemantics_BEHAVIORAL
		}
		if !slices.Contains(reasons, facts.Reason) {
			reasons = append(reasons, facts.Reason)
		}
	}
	for _, change := range changes.Pools {
		facts, err := poolImpact(change)
		if err != nil {
			return nil, err
		}
		add(facts)
	}
	for _, change := range changes.Classifiers {
		facts, err := classifierImpact(change)
		if err != nil {
			return nil, err
		}
		add(facts)
	}
	semantics.Reason = strings.Join(reasons, "; ")
	return semantics, nil
}
