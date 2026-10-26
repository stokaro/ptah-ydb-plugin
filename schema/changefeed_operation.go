package schema

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"github.com/stokaro/ptah-ydb-plugin/internal/changefeeds"
	native "github.com/stokaro/ptah-ydb-plugin/internal/nativeprotocol"
	"github.com/stokaro/ptah/provider-sdk/modeljson"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

// ChangefeedOperation identifies the executable change of the changefeeds of
// one row table: the changefeeds added, dropped, dropped and added again, and
// those whose topic changes.
//
// One operation carries every changefeed change of its table. The host runs
// a step that drops an object after every step that creates one, so a drop and
// an addition of two steps cannot be ordered for a swap at YDB's limit on the
// changefeeds of one table, and a changefeed dropped and added again must be
// one step.
const ChangefeedOperation = modelNamespace + "changefeed-operation"

// The reasons a changefeed operation gives for its impact, the host's words
// where the host has them.
const (
	addChangefeedReason   = "ADD CHANGEFEED adds a stream of the table's changes and its topic; it does not remove data or tighten constraints"
	dropChangefeedReason  = "DROP CHANGEFEED removes the change stream with every record nobody read, and its consumers"
	changeChangefeedTopic = "ALTER TOPIC can drop a consumer's position or shorten how long the stream keeps records"
)

// ChangefeedOperationModel defines a changefeed operation and its
// preconditions.
func ChangefeedOperationModel() *protocol.ValueModel { return proto.CloneOf(changefeedOperationModel) }

var changefeedOperationModel = buildChangefeedOperationModel()

func buildChangefeedOperationModel() *protocol.ValueModel {
	definition, err := json.Marshal(map[string]any{
		"kind": ChangefeedOperation, "representation": "operation", "schema_version": 1, "encoding": "json",
		"owner": "the row table that holds the changefeeds",
		"payload": map[string]any{"required": []string{"changefeeds"}, "additionalProperties": false,
			"changefeeds": map[string]any{"meaning": "one change per changefeed, in name order, at least one",
				"name":   "the changefeed's name",
				"before": map[string]any{"meaning": "the changefeed as the table holds it; absent for an addition", "model": json.RawMessage(model(ChangefeedProperties).DefinitionJson)},
				"after":  map[string]any{"meaning": "the declared changefeed; absent for a removal", "model": json.RawMessage(NativeDesiredModel(ChangefeedProperties).DefinitionJson)}}},
		"semantics": "additions and topic changes adding consumers only: additive, access unchanged; a removal, a recreation, or a topic change dropping or restarting a consumer: destructive, " +
			"removes the records nobody read and every consumer's position; other topic changes: behavioral; never replaces the owner",
		"comparison":   "both operands read as the host's changefeed specification; a pair that holds no change is refused; a pair whose option YDB changes in no other way drops the changefeed and adds it again",
		"execution":    "YQL ALTER TABLE ... DROP CHANGEFEED for every removal, then ALTER TABLE ... ADD CHANGEFEED with the topic's consumers for every addition, then ALTER TOPIC for every topic change, one query each, outside SQL transactions; no automatic retry",
		"precondition": "before is each changefeed as the plan read it, or its established absence; verified immediately before dispatch; the API has no atomic compare-and-set",
		"confirmation": "read back each declared changefeed with its topic's retention and consumers, or its absence after a removal",
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
	return &protocol.ValueModel{Kind: ChangefeedOperation, Representation: protocol.ValueInput_OPERATION, SchemaVersion: 1, Encoding: "json",
		DefinitionJson: canonical, DefinitionSha256: hex.EncodeToString(digest[:]), CanonicalFormat: ChangefeedOperation + "/json-v1"}
}

type changefeedEntry struct {
	Name   string          `json:"name"`
	Before json.RawMessage `json:"before,omitempty"`
	After  json.RawMessage `json:"after,omitempty"`
}

type changefeedPayload struct {
	Changefeeds []changefeedEntry `json:"changefeeds"`
}

// CaptureChangefeedOperation writes the changefeed changes of the table owner
// as one normalized operation.
func CaptureChangefeedOperation(id string, owner *protocol.Reference, changes []changefeeds.Change) (*protocol.NormalizedValue, error) {
	encoded, err := encodeChangefeedChanges(changes)
	if err != nil {
		return nil, err
	}
	definition := ChangefeedOperationModel()
	return NormalizeChangefeedOperation(&protocol.ValueInput{Id: id, Representation: protocol.ValueInput_OPERATION, Value: &protocol.Envelope{
		ProviderId: ProviderID, Kind: definition.Kind, Owner: proto.CloneOf(owner), SchemaVersion: definition.SchemaVersion,
		Encoding: definition.Encoding, DefinitionSha256: definition.DefinitionSha256, Payload: encoded}})
}

func encodeChangefeedChanges(changes []changefeeds.Change) ([]byte, error) {
	payload := changefeedPayload{Changefeeds: []changefeedEntry{}}
	for _, change := range changes {
		entry := changefeedEntry{Name: change.Name}
		var err error
		if change.Before != nil {
			if entry.Before, err = protojson.Marshal(change.Before); err != nil {
				return nil, err
			}
		}
		if change.After != nil {
			if entry.After, err = protojson.Marshal(change.After); err != nil {
				return nil, err
			}
		}
		payload.Changefeeds = append(payload.Changefeeds, entry)
	}
	return json.Marshal(payload)
}

// NormalizeChangefeedOperation validates every operand against the changefeed
// models, reconstructs the operation's safety facts and refuses a change that
// changes nothing or that the operation cannot carry out.
func NormalizeChangefeedOperation(input *protocol.ValueInput) (*protocol.NormalizedValue, error) {
	value, definition := input.GetValue(), ChangefeedOperationModel()
	if input.GetRepresentation() != protocol.ValueInput_OPERATION || value.GetProviderId() != ProviderID || value.GetKind() != definition.Kind ||
		value.GetSchemaVersion() != definition.SchemaVersion || value.GetEncoding() != definition.Encoding || value.GetDefinitionSha256() != definition.DefinitionSha256 {
		return nil, fmt.Errorf("changefeed operation does not match its negotiated definition")
	}
	owner := value.GetOwner()
	if owner.GetEngine() != "ydb" || owner.GetKind() != "table" || owner.GetName() == "" || owner.GetCatalog() != "" || len(owner.GetPath())+len(owner.GetOverload()) != 0 {
		return nil, fmt.Errorf("changefeed operation requires a table owner")
	}
	changes, err := decodeChangefeedChanges(owner, value.GetPayload())
	if err != nil {
		return nil, err
	}
	semantics, err := changefeedSemantics(owner, changes)
	if err != nil {
		return nil, err
	}
	encoded, err := encodeChangefeedChanges(changes)
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

// DecodeChangefeedOperation returns the changes a normalized changefeed
// operation carries, in name order.
func DecodeChangefeedOperation(value *protocol.Envelope) ([]changefeeds.Change, error) {
	normalized, err := NormalizeChangefeedOperation(&protocol.ValueInput{Id: "decode", Representation: protocol.ValueInput_OPERATION, Value: value})
	if err != nil {
		return nil, err
	}
	return decodeChangefeedChanges(normalized.Value.Owner, normalized.Value.Payload)
}

// exactKeys refuses a JSON object with a key outside allowed or a null value.
func exactKeys(raw json.RawMessage, allowed ...string) error {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return fmt.Errorf("invalid changefeed operation payload")
	}
	for key, field := range fields {
		if !slices.Contains(allowed, key) {
			return fmt.Errorf("changefeed operation has an unknown key %q", key)
		}
		if bytes.Equal(field, []byte("null")) {
			return fmt.Errorf("changefeed operation key %q cannot be null", key)
		}
	}
	return nil
}

// decodeChangefeedChanges reads a payload with exact keys and validates each
// operand as one changefeed of its model's set.
func decodeChangefeedChanges(owner *protocol.Reference, raw []byte) ([]changefeeds.Change, error) {
	canonical, err := modeljson.Canonical(raw)
	if err != nil {
		return nil, err
	}
	if err := exactKeys(canonical, "changefeeds"); err != nil {
		return nil, err
	}
	var payload struct {
		Changefeeds []json.RawMessage `json:"changefeeds"`
	}
	if err := json.Unmarshal(canonical, &payload); err != nil || len(payload.Changefeeds) == 0 {
		return nil, fmt.Errorf("changefeed operation requires at least one changefeed change")
	}
	var changes []changefeeds.Change
	for _, raw := range payload.Changefeeds {
		if err := exactKeys(raw, "name", "before", "after"); err != nil {
			return nil, err
		}
		var entry changefeedEntry
		if err := json.Unmarshal(raw, &entry); err != nil || entry.Name == "" {
			return nil, fmt.Errorf("changefeed operation requires a changefeed name")
		}
		change := changefeeds.Change{Name: entry.Name}
		if entry.Before != nil {
			var feed native.ObservedChangefeed
			if err := protojson.Unmarshal(entry.Before, &feed); err != nil {
				return nil, fmt.Errorf("invalid changefeed operation operand")
			}
			set := &native.ChangefeedObservations{Changefeeds: []*native.ObservedChangefeed{&feed}}
			if err := Known(set); err != nil {
				return nil, err
			}
			if err := normalizeChangefeeds(set); err != nil {
				return nil, err
			}
			change.Before = set.Changefeeds[0]
		}
		if entry.After != nil {
			var feed native.ChangefeedIntent
			if err := protojson.Unmarshal(entry.After, &feed); err != nil {
				return nil, fmt.Errorf("invalid changefeed operation operand")
			}
			encoded, err := protojson.Marshal(&native.ChangefeedSetIntent{Changefeeds: []*native.ChangefeedIntent{&feed}})
			if err != nil {
				return nil, err
			}
			model := NativeDesiredModel(ChangefeedProperties)
			normalized, err := NormalizeDesired(&protocol.Envelope{ProviderId: ProviderID, Kind: ChangefeedProperties, Owner: proto.CloneOf(owner), SchemaVersion: model.SchemaVersion,
				Encoding: model.Encoding, DefinitionSha256: model.DefinitionSha256, Payload: encoded})
			if err != nil {
				return nil, err
			}
			var set native.ChangefeedSetIntent
			if err := protojson.Unmarshal(normalized.Payload, &set); err != nil || len(set.Changefeeds) != 1 {
				return nil, fmt.Errorf("invalid changefeed operation operand")
			}
			change.After = set.Changefeeds[0]
		}
		switch {
		case change.Before == nil && change.After == nil:
			return nil, fmt.Errorf("changefeed operation change of %q has no operand", change.Name)
		case change.Before != nil && change.Before.GetDescription().GetName() != change.Name, change.After != nil && change.After.GetName() != change.Name:
			return nil, fmt.Errorf("changefeed operation operands name another changefeed than %q", change.Name)
		case len(changes) > 0 && strings.Compare(changes[len(changes)-1].Name, change.Name) >= 0:
			return nil, fmt.Errorf("changefeed operation changes must name each changefeed once, in name order")
		}
		changes = append(changes, change)
	}
	return changes, nil
}

// changefeedSemantics derives an operation's facts from its changes, and
// refuses a change that holds none or that touches a stream a replication
// controller owns.
func changefeedSemantics(owner *protocol.Reference, changes []changefeeds.Change) (*protocol.OperationSemantics, error) {
	table := owner.GetName()
	if owner.GetSchema() != "" {
		table = owner.GetSchema() + "/" + owner.GetName()
	}
	semantics := &protocol.OperationSemantics{Impact: protocol.ChangeSemantics_ADDITIVE, Access: protocol.ChangeSemantics_UNCHANGED,
		AccessReason: "Changing a changefeed preserves the table's access metadata"}
	var notes []string
	destructive, behavioral, added := false, false, false
	for _, change := range changes {
		subject := changefeeds.Subject(table, change.Name)
		var current, desired changefeeds.Feed
		var err error
		if change.Before != nil {
			if current, err = changefeeds.FromObserved(change.Before); err != nil {
				return nil, fmt.Errorf("%s: %w", subject, err)
			}
		}
		if change.After != nil {
			if desired, err = changefeeds.FromIntent(change.After); err != nil {
				return nil, err
			}
		}
		if current.Replicated || desired.Replicated {
			return nil, fmt.Errorf("%s: the retained replication-managed changefeed cannot be created, removed, or changed independently of its controller", subject)
		}
		switch change.Action() {
		case changefeeds.Add:
			added = true
		case changefeeds.Drop:
			destructive = true
			notes = append(notes, changefeeds.DroppedNote(table, current))
		case changefeeds.Recreate:
			destructive, added = true, true
			notes = append(notes, changefeeds.RecreatedNote(table, current))
		default:
			if !changefeeds.TopicChanged(desired, current) {
				return nil, fmt.Errorf("%s: the changefeed operands contain no change", subject)
			}
			_, restarted := changefeeds.TopicStatements(table, desired.Changefeed, current.Changefeed)
			if len(restarted) > 0 {
				notes = append(notes, changefeeds.RestartedConsumersNote(table, change.Name, restarted))
			}
			switch {
			case len(changefeeds.ConsumerPositionsLost(desired, current)) > 0:
				destructive = true
			case changefeeds.RetentionChanged(desired, current) || changefeeds.ConsumersChanged(desired, current):
				behavioral = true
			}
		}
	}
	switch {
	case destructive:
		semantics.Impact, semantics.Reason = protocol.ChangeSemantics_DESTRUCTIVE, dropChangefeedReason
		if !slices.ContainsFunc(changes, func(change changefeeds.Change) bool {
			return change.Action() == changefeeds.Drop || change.Action() == changefeeds.Recreate
		}) {
			semantics.Reason = changeChangefeedTopic
		}
	case behavioral:
		semantics.Impact, semantics.Reason = protocol.ChangeSemantics_BEHAVIORAL, changeChangefeedTopic
	default:
		semantics.Reason = "ALTER TOPIC adds consumers to a changefeed's topic"
		if added {
			semantics.Reason = addChangefeedReason
		}
	}
	if len(notes) > 0 {
		semantics.Reason += ". " + strings.Join(notes, " ")
	}
	dropped := slices.ContainsFunc(changes, func(change changefeeds.Change) bool { return change.Action() == changefeeds.Drop })
	switch {
	case added:
		semantics.AccessReason = "A new changefeed's topic inherits the access of its table, and the table's access stays as it is"
	case dropped:
		semantics.Access, semantics.AccessReason = protocol.ChangeSemantics_NARROWS, "Dropping a changefeed removes access to its topic"
	}
	return semantics, nil
}
