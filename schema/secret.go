package schema

import (
	"fmt"

	native "github.com/stokaro/ptah-ydb-plugin/internal/nativeprotocol"
	"github.com/stokaro/ptah-ydb-plugin/internal/secrets"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

const (
	// SecretProperties records a scheme secret's presence or its declared value reference.
	SecretProperties = modelNamespace + "secret"
	// SecretChange identifies a secret creation, explicit rotation, or removal.
	SecretChange = modelNamespace + "secret-change"
	// SecretOperation carries a secret change without the secret's value.
	SecretOperation = modelNamespace + "secret-operation"
)

var secretChanges = newObjectChanges(objectChanges{
	kind: SecretProperties, change: SecretChange, operation: SecretOperation, noun: "secret",
	semantics: secretSemantics,
	definition: map[string]any{
		"comparison": "presence only; value_env is an execution reference, never an observed value; both operands present requests an explicit rotation",
		"values":     "secret values are absent from every schema, change, operation, preview and saved plan",
	},
	execution: map[string]any{
		"execution":    "CREATE SECRET, ALTER SECRET or DROP SECRET outside SQL transactions; values come only from approved execution inputs; no automatic retry",
		"precondition": "presence checked before dispatch; concurrent value rotation cannot be observed or compared atomically",
		"confirmation": "presence after create or rotation, absence after drop; the server never returns the secret value",
		"access":       "rotation preserves access; common access observations and planning own grants and owners independently",
	},
})

func validateSecretOwner(owner *protocol.Reference) error {
	if owner.GetKind() != SecretProperties {
		return fmt.Errorf("invalid YDB secret kind")
	}
	if err := ValidateNativeOwner(owner); err != nil {
		return err
	}
	return secrets.ValidateIdentity(owner.GetSchema(), owner.GetName())
}

// SecretChangeModel defines complete presence and declaration operands.
func SecretChangeModel() *protocol.ValueModel { return proto.CloneOf(secretChanges.changeModel) }

// SecretOperationModel defines a secret operation and its execution constraints.
func SecretOperationModel() *protocol.ValueModel { return proto.CloneOf(secretChanges.operationModel) }

// CaptureSecretChange captures a creation, explicit rotation, or removal.
func CaptureSecretChange(id string, owner *protocol.Reference, before, after *protocol.Envelope) (*protocol.NormalizedValue, error) {
	return secretChanges.capture(id, owner, before, after)
}

// NormalizeSecretChange validates a captured secret change.
func NormalizeSecretChange(input *protocol.ValueInput) (*protocol.NormalizedValue, error) {
	return secretChanges.normalizeChange(input)
}

// SecretOperands reads the complete normalized operands, nil for absence.
func SecretOperands(value *protocol.Envelope) (*protocol.Envelope, *protocol.Envelope, error) {
	return secretChanges.operands(value)
}

// SecretOperationFromChange lowers a complete secret change.
func SecretOperationFromChange(id string, value *protocol.Envelope) (*protocol.NormalizedValue, error) {
	return secretChanges.operationFromChange(id, value)
}

// NormalizeSecretOperation validates a captured secret operation.
func NormalizeSecretOperation(input *protocol.ValueInput) (*protocol.NormalizedValue, error) {
	return secretChanges.normalizeOperation(input)
}

// SecretOperationChange restores the change carried by a secret operation.
func SecretOperationChange(value *protocol.Envelope) *protocol.Envelope {
	return secretChanges.operationChange(value)
}

// CaptureSecretIntent records a value reference under its exact secret path.
func CaptureSecretIntent(owner *protocol.Reference, intent *native.SecretIntent) (*protocol.Envelope, error) {
	return convertedEnvelope(&protocol.Envelope{ProviderId: ProviderID, Kind: SecretProperties, Owner: proto.CloneOf(owner)}, NativeDesiredModel(SecretProperties), intent, NormalizeDesired)
}

// DesiredSecret decodes the validated declaration without reading its input.
func DesiredSecret(value *protocol.Envelope) (*native.SecretIntent, error) {
	normalized, err := NormalizeDesired(value)
	if err != nil {
		return nil, err
	}
	if normalized.Kind != SecretProperties {
		return nil, fmt.Errorf("expected a YDB secret declaration")
	}
	var intent native.SecretIntent
	if err := protojson.Unmarshal(normalized.Payload, &intent); err != nil {
		return nil, err
	}
	return &intent, nil
}

func secretSemantics(before, after *protocol.Envelope) (*protocol.ChangeSemantics, error) {
	result := &protocol.ChangeSemantics{ReplacesOwner: new(false), RemovesState: new(false), Access: protocol.ChangeSemantics_UNCHANGED, AccessReason: "Secret rotation preserves access"}
	switch {
	case before == nil:
		result.Impact, result.Reason = protocol.ChangeSemantics_ADDITIVE, secrets.CreateReason
		result.Access, result.AccessReason = protocol.ChangeSemantics_ACCESS_UNKNOWN, "Secret creation inherits server access policy"
	case after == nil:
		result.Impact, result.Reason, result.RemovesState = protocol.ChangeSemantics_DESTRUCTIVE, secrets.DropReason, new(true)
		result.Access, result.AccessReason = protocol.ChangeSemantics_NARROWS, "Dropping a secret removes access to it"
	default:
		result.Impact, result.Reason = protocol.ChangeSemantics_BEHAVIORAL, secrets.RotateReason
	}
	return result, nil
}

// SecretStatement renders a captured operation using its value reference only.
func SecretStatement(operation *protocol.Envelope) (string, error) {
	normalized, err := NormalizeSecretOperation(&protocol.ValueInput{Id: "secret", Representation: protocol.ValueInput_OPERATION, Value: operation})
	if err != nil {
		return "", err
	}
	before, after, err := SecretOperands(SecretOperationChange(normalized.Value))
	if err != nil {
		return "", err
	}
	owner := normalized.Value.Owner
	if after == nil {
		return secrets.DropStatement(owner.Schema, owner.Name), nil
	}
	intent, err := DesiredSecret(after)
	if err != nil {
		return "", err
	}
	if before == nil {
		return secrets.CreateStatement(owner.Schema, owner.Name, intent.ValueEnv), nil
	}
	return secrets.AlterStatement(owner.Schema, owner.Name, intent.ValueEnv), nil
}
