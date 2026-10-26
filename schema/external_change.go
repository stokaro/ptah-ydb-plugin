package schema

import (
	"fmt"

	native "github.com/stokaro/ptah-ydb-plugin/internal/nativeprotocol"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb_Table"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

const (
	// ExternalSourceChange identifies a complete before/after change of one
	// external data source.
	ExternalSourceChange = modelNamespace + "external-data-source-change"
	// ExternalSourceOperation identifies the executable change of one
	// external data source.
	ExternalSourceOperation = modelNamespace + "external-data-source-operation"
	// ExternalTableChange identifies a complete before/after change of one
	// external table.
	ExternalTableChange = modelNamespace + "external-table-change"
	// ExternalTableOperation identifies the executable change of one
	// external table.
	ExternalTableOperation = modelNamespace + "external-table-operation"
)

// The ways an external object operation carries out its change.
const (
	// ExternalCreate creates the object with CREATE.
	ExternalCreate = "create"
	// ExternalReplace replaces the object in place with CREATE OR REPLACE.
	ExternalReplace = "replace"
	// ExternalRecreate drops a data source and creates it again, giving the
	// new one the owner and the access entries the old one held.
	ExternalRecreate = "recreate"
	// ExternalRestore creates an external table an earlier operation of the
	// same plan dropped, giving it the owner and the access entries the old
	// one held: the second half of a table dropped and created again around
	// a statement on the data source it reads.
	ExternalRestore = "restore"
	// ExternalDrop drops the object.
	ExternalDrop = "drop"
)

// The reasons an external object change gives for its impact, the host's
// words. Neither object holds data in YDB.
const (
	dropExternalSourceReason = "DROP EXTERNAL DATA SOURCE removes what YDB reads another system through; no data YDB stores is lost"
	dropExternalTableReason  = "DROP EXTERNAL TABLE removes the columns YDB reads files through; the files stay"
	replaceExternalReason    = "CREATE OR REPLACE changes what queries reading the external object read"
)

func externalAllowed(operation string, before, after bool) bool {
	switch operation {
	case ExternalCreate:
		return !before && after
	case ExternalDrop:
		return before && !after
	default:
		return before && after
	}
}

var externalSourceChanges = newObjectChanges(objectChanges{kind: ExternalSourceProperties, change: ExternalSourceChange, operation: ExternalSourceOperation, noun: "external data source",
	semantics: externalSemantics("CREATE EXTERNAL DATA SOURCE adds a data source", dropExternalSourceReason, "data source"),
	database:  true, operations: []string{ExternalCreate, ExternalReplace, ExternalRecreate, ExternalDrop}, allowed: externalAllowed,
	definition: map[string]any{
		"semantics":  "create: additive, access unchanged; drop: behavioral, no data YDB stores is lost, narrows access; a change of both sides: behavioral, access unchanged; never destructive; never replaces owner",
		"comparison": "both operands read as the host's data source specification: source type, location and auth method exact, options by upper-case name, a secret path relative to the database; REFERENCES and access metadata are not compared",
		"database":   "the database the comparison read, which a secret path or a data source path written absolute is read against",
	},
	execution: map[string]any{
		"execution":    "YQL CREATE, CREATE OR REPLACE or DROP EXTERNAL DATA SOURCE statements, one query each, outside SQL transactions; recreate drops the data source, creates it again and gives the new one the owner and the access entries the old one held; no automatic retry",
		"precondition": "before is the complete normalized observation, REFERENCES aside, or established absence; verified immediately before dispatch; the API has no atomic compare-and-set",
		"confirmation": "read back the declared data source, or absence after a drop",
		"owner":        "a nonempty owner database or change database must equal the selected session database; empty selects that session database",
	}})

var externalTableChanges = newObjectChanges(objectChanges{kind: ExternalTableProperties, change: ExternalTableChange, operation: ExternalTableOperation, noun: "external table",
	semantics: externalSemantics("CREATE EXTERNAL TABLE adds an external table", dropExternalTableReason, "external table"),
	database:  true, operations: []string{ExternalCreate, ExternalReplace, ExternalRestore, ExternalDrop}, allowed: externalAllowed,
	definition: map[string]any{
		"semantics":  "create: additive, access unchanged; drop: behavioral, the files stay, narrows access; a change of both sides: behavioral, access unchanged; never destructive; never replaces owner",
		"comparison": "both operands read as the host's external table specification: the data source relative to the database, the location, the columns in order with their types and nullability, the options by upper-case name with PARTITIONED_BY as the list it holds; access metadata is not compared",
		"operands":   "both operands may describe the same table, which a data source the plan drops and creates again takes along",
		"database":   "the database the comparison read, which a data source path written absolute is read against",
	},
	execution: map[string]any{
		"execution":    "YQL CREATE, CREATE OR REPLACE or DROP EXTERNAL TABLE statements, one query each, outside SQL transactions; a table dropped and created again is two operations, a drop and a restore, which creates the table and gives it the owner and the access entries the old one held; no automatic retry",
		"precondition": "before is the complete normalized observation or established absence, and absence for a restore; verified immediately before dispatch; the API has no atomic compare-and-set",
		"confirmation": "read back the declared external table, or absence after a drop",
		"owner":        "a nonempty owner database or change database must equal the selected session database; empty selects that session database",
	}})

func externalSemantics(created, dropped, noun string) func(before, after *protocol.Envelope) (*protocol.ChangeSemantics, error) {
	return func(before, after *protocol.Envelope) (*protocol.ChangeSemantics, error) {
		semantics := &protocol.ChangeSemantics{ReplacesOwner: new(false), RemovesState: new(false)}
		switch {
		case before == nil:
			semantics.Impact, semantics.Reason, semantics.Access, semantics.AccessReason = protocol.ChangeSemantics_ADDITIVE, created, protocol.ChangeSemantics_UNCHANGED,
				"A new "+noun+" takes the access its directory passes on, and the plan grants nothing"
		case after == nil:
			semantics.Impact, semantics.Reason, semantics.Access, semantics.AccessReason = protocol.ChangeSemantics_BEHAVIORAL, dropped, protocol.ChangeSemantics_NARROWS, "Dropping the "+noun+" removes access to it"
		default:
			semantics.Impact, semantics.Reason, semantics.Access, semantics.AccessReason = protocol.ChangeSemantics_BEHAVIORAL, replaceExternalReason, protocol.ChangeSemantics_UNCHANGED,
				"CREATE OR REPLACE keeps the owner and the access entries, and a "+noun+" dropped and created again is given the ones the old one held"
		}
		return semantics, nil
	}
}

func externalChanges(kind string) (*objectChanges, error) {
	switch kind {
	case ExternalSourceProperties, ExternalSourceChange, ExternalSourceOperation:
		return externalSourceChanges, nil
	case ExternalTableProperties, ExternalTableChange, ExternalTableOperation:
		return externalTableChanges, nil
	default:
		return nil, fmt.Errorf("unsupported YDB external object model")
	}
}

// ExternalSourceChangeModel defines an external data source change.
func ExternalSourceChangeModel() *protocol.ValueModel {
	return proto.CloneOf(externalSourceChanges.changeModel)
}

// ExternalSourceOperationModel defines an external data source operation.
func ExternalSourceOperationModel() *protocol.ValueModel {
	return proto.CloneOf(externalSourceChanges.operationModel)
}

// ExternalTableChangeModel defines an external table change.
func ExternalTableChangeModel() *protocol.ValueModel {
	return proto.CloneOf(externalTableChanges.changeModel)
}

// ExternalTableOperationModel defines an external table operation.
func ExternalTableOperationModel() *protocol.ValueModel {
	return proto.CloneOf(externalTableChanges.operationModel)
}

// CaptureExternalChange retains complete operands of one external object of
// kind, an object model kind, under one owner, with database, the database
// the comparison read, or "" where it is not known. Either operand may be nil
// for an established absence, and both may describe the same object.
func CaptureExternalChange(kind, id string, owner *protocol.Reference, database string, before, after *protocol.Envelope) (*protocol.NormalizedValue, error) {
	changes, err := externalChanges(kind)
	if err != nil {
		return nil, err
	}
	return changes.captureIn(id, owner, database, before, after)
}

// NormalizeExternalChange reconstructs a change's safety facts from its
// operands.
func NormalizeExternalChange(input *protocol.ValueInput) (*protocol.NormalizedValue, error) {
	changes, err := externalChanges(input.GetValue().GetKind())
	if err != nil {
		return nil, err
	}
	return changes.normalizeChange(input)
}

// ExternalOperands returns an external object change's normalized operands,
// nil for an absent one, and the database it names, "" for none.
func ExternalOperands(change *protocol.Envelope) (before, after *protocol.Envelope, database string, err error) {
	changes, err := externalChanges(change.GetKind())
	if err != nil {
		return nil, nil, "", err
	}
	if change.GetKind() != changes.change {
		return nil, nil, "", fmt.Errorf("expected a YDB external object change")
	}
	return changes.parts(change)
}

// ExternalOperation lowers a complete change to the operation that carries
// it out the way operation names: ExternalCreate, ExternalReplace,
// ExternalRecreate for a data source, ExternalRestore for an external table,
// or ExternalDrop, which the operands must allow.
func ExternalOperation(id string, change *protocol.Envelope, operation string) (*protocol.NormalizedValue, error) {
	changes, err := externalChanges(change.GetKind())
	if err != nil {
		return nil, err
	}
	if operation == "" {
		return nil, fmt.Errorf("an external object operation names how it runs")
	}
	return changes.operationAs(id, change, operation)
}

// NormalizeExternalOperation reconstructs an operation's facts and refuses
// one whose operation its operands do not allow.
func NormalizeExternalOperation(input *protocol.ValueInput) (*protocol.NormalizedValue, error) {
	changes, err := externalChanges(input.GetValue().GetKind())
	if err != nil {
		return nil, err
	}
	return changes.normalizeOperation(input)
}

// ExternalOperationChange restores the change an operation carries and the
// way the operation carries it out.
func ExternalOperationChange(operation *protocol.Envelope) (*protocol.Envelope, string, error) {
	changes, err := externalChanges(operation.GetKind())
	if err != nil {
		return nil, "", err
	}
	if operation.GetKind() != changes.operation {
		return nil, "", fmt.Errorf("expected a YDB external object operation")
	}
	return changes.split(operation)
}

// ObservedExternalSource decodes a normalized data source observation.
func ObservedExternalSource(value *protocol.Envelope) (*Ydb_Table.DescribeExternalDataSourceResult, error) {
	normalized, err := Normalize(value)
	if err != nil {
		return nil, err
	}
	if normalized.GetKind() != ExternalSourceProperties {
		return nil, fmt.Errorf("expected a YDB external data source observation")
	}
	var observed Ydb_Table.DescribeExternalDataSourceResult
	if err := protojson.Unmarshal(normalized.Payload, &observed); err != nil {
		return nil, err
	}
	return &observed, nil
}

// DesiredExternalSource decodes a normalized data source declaration.
func DesiredExternalSource(value *protocol.Envelope) (*native.ExternalSourceIntent, error) {
	normalized, err := NormalizeDesired(value)
	if err != nil {
		return nil, err
	}
	if normalized.GetKind() != ExternalSourceProperties {
		return nil, fmt.Errorf("expected a YDB external data source declaration")
	}
	var intent native.ExternalSourceIntent
	if err := protojson.Unmarshal(normalized.Payload, &intent); err != nil {
		return nil, err
	}
	return &intent, nil
}

// ObservedExternalTable decodes a normalized external table observation.
func ObservedExternalTable(value *protocol.Envelope) (*native.DescribeExternalTableResult, error) {
	normalized, err := Normalize(value)
	if err != nil {
		return nil, err
	}
	if normalized.GetKind() != ExternalTableProperties {
		return nil, fmt.Errorf("expected a YDB external table observation")
	}
	var observed native.DescribeExternalTableResult
	if err := protojson.Unmarshal(normalized.Payload, &observed); err != nil {
		return nil, err
	}
	return &observed, nil
}

// DesiredExternalTable decodes a normalized external table declaration.
func DesiredExternalTable(value *protocol.Envelope) (*native.ExternalTableIntent, error) {
	normalized, err := NormalizeDesired(value)
	if err != nil {
		return nil, err
	}
	if normalized.GetKind() != ExternalTableProperties {
		return nil, fmt.Errorf("expected a YDB external table declaration")
	}
	var intent native.ExternalTableIntent
	if err := protojson.Unmarshal(normalized.Payload, &intent); err != nil {
		return nil, err
	}
	return &intent, nil
}

// CaptureExternalIntent writes a declared external object, an
// *native.ExternalSourceIntent or an *native.ExternalTableIntent, as the
// provider's desired model under owner.
func CaptureExternalIntent(owner *protocol.Reference, intent proto.Message) (*protocol.Envelope, error) {
	var kind string
	switch intent.(type) {
	case *native.ExternalSourceIntent:
		kind = ExternalSourceProperties
	case *native.ExternalTableIntent:
		kind = ExternalTableProperties
	default:
		return nil, fmt.Errorf("unsupported YDB external object declaration")
	}
	if owner.GetKind() != kind {
		return nil, fmt.Errorf("external object declaration and owner disagree")
	}
	return convertedEnvelope(&protocol.Envelope{ProviderId: ProviderID, Kind: kind, Owner: proto.CloneOf(owner)}, NativeDesiredModel(kind), intent, NormalizeDesired)
}

// ValidateExternalAccess checks a declaration's access requirements against
// the access the object holds. CREATE OR REPLACE keeps the access entries,
// and CREATE cannot promise any.
func ValidateExternalAccess(before, after *protocol.Envelope) error {
	if after == nil {
		return nil
	}
	var required *native.AccessIntent
	switch after.GetKind() {
	case ExternalSourceProperties:
		intent, err := DesiredExternalSource(after)
		if err != nil {
			return err
		}
		required = intent.GetAccess()
	case ExternalTableProperties:
		intent, err := DesiredExternalTable(after)
		if err != nil {
			return err
		}
		required = intent.GetAccess()
	default:
		return fmt.Errorf("unsupported YDB external object declaration")
	}
	if required == nil {
		return nil
	}
	if before == nil {
		return fmt.Errorf("external object creation cannot establish requested access requirements")
	}
	var actual *native.AccessIntent
	if before.GetKind() == ExternalSourceProperties {
		observed, err := ObservedExternalSource(before)
		if err != nil {
			return err
		}
		actual = accessIntent(observed.GetSelf())
	} else {
		observed, err := ObservedExternalTable(before)
		if err != nil {
			return err
		}
		actual = accessIntent(observed.GetSelf())
	}
	if actual == nil || (required.Owner != nil && required.GetOwner() != actual.GetOwner()) ||
		!permissionSetsEqual(required.DirectGrants, actual.DirectGrants) || !permissionSetsEqual(required.RequiredEffectivePermissions, actual.RequiredEffectivePermissions) {
		return fmt.Errorf("external object access requirements differ from established access")
	}
	return nil
}
