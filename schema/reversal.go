package schema

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"

	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb_Topic"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	"github.com/stokaro/ptah-ydb-plugin/internal/changefeeds"
	native "github.com/stokaro/ptah-ydb-plugin/internal/nativeprotocol"
)

// Reversed is the change that undoes one change of a standalone object, and
// what the forward change leaves.
type Reversed struct {
	// Value is the reverse change: its before operand is Left and its after
	// operand is the declaration of the object the forward change found. It
	// is nil where YDB reaches nothing in place that the reverse could run
	// (see rollbackReached).
	Value *protocol.NormalizedValue
	// Kind is the object model of the operands.
	Kind string
	// Before and After are the forward change's operands, either nil: what
	// the change found and what it asked for.
	Before, After *protocol.Envelope
	// Left is what the forward change leaves, as a read would report it, or
	// nil where it drops the object. It is a prediction from After, never
	// evidence that the forward change ran.
	Left *protocol.Envelope
	// Restored is the declaration the reverse asks for, the conversion of
	// Before with what YDB keeps in place, or nil where the reverse drops
	// the object or runs nothing.
	Restored *protocol.Envelope
}

// reversibleObjects are the standalone object changes ReverseObjectChange
// undoes, by change kind.
var reversibleObjects = map[string]*objectChanges{
	SecretChange: secretChanges, TopicChange: topicChanges, StreamingQueryChange: streamingChanges,
	ExternalSourceChange: externalSourceChanges, ExternalTableChange: externalTableChanges,
	ResourcePoolChange: poolChanges, ResourcePoolClassifierChange: classifierChanges,
	ReplicationChange: replicationChanges, TransferChange: transferChanges,
}

// ReversibleObjectChanges lists the change kinds ReverseObjectChange undoes.
func ReversibleObjectChanges() []string {
	return []string{CoordinationChange, ExternalSourceChange, ExternalTableChange, ReplicationChange, ResourcePoolChange, ResourcePoolClassifierChange,
		SecretChange, StreamingQueryChange, TopicChange, TransferChange}
}

// AccessWire is where a target carries the access entries of an object.
type AccessWire int

const (
	// NativeAccess carries them in the object's own value.
	NativeAccess AccessWire = iota
	// CommonAccess carries them as common grants, on a target that lists
	// provider.CommonAccessFeature.
	CommonAccess
)

// ReverseObjectChange returns the change that undoes change, a change of one
// of ReversibleObjectChanges, under id. The reverse finds what the forward
// change leaves, the conversion of its declaration, and asks for what the
// forward change found, the conversion of that read; an absent operand stays
// absent, so a creation reverses to a drop and a drop to a creation. With
// CommonAccess the restored declaration keeps no access entries, which travel
// as common grants. Each operand keeps the database context it was read or
// declared in.
func ReverseObjectChange(id string, change *protocol.Envelope, access AccessWire) (Reversed, error) {
	if change.GetKind() == CoordinationChange {
		before, after, err := CoordinationOperands(change)
		if err != nil {
			return Reversed{}, err
		}
		left, restored, err := reversedOperands(before, after, ConvertCoordination, access)
		if err != nil {
			return Reversed{}, err
		}
		value, err := CaptureCoordinationChange(id, change.Owner, left, restored)
		if err != nil {
			return Reversed{}, err
		}
		return Reversed{Value: value, Kind: CoordinationProperties, Before: before, After: after, Left: left, Restored: restored}, nil
	}
	family, found := reversibleObjects[change.GetKind()]
	if !found {
		return Reversed{}, fmt.Errorf("YDB change %s has no reversal", change.GetKind())
	}
	if _, err := family.normalizeChange(&protocol.ValueInput{Id: id, Representation: protocol.ValueInput_CHANGE, Value: change}); err != nil {
		return Reversed{}, err
	}
	parts, err := family.parts(change)
	if err != nil {
		return Reversed{}, err
	}
	left, restored, err := reversedOperands(parts.Before, parts.After, ConvertNative, access)
	if err != nil {
		return Reversed{}, err
	}
	if restored, err = keepStateReset(family, parts.After, restored); err != nil {
		return Reversed{}, err
	}
	if left, err = keepHeldConsumer(family, parts.Before, left); err != nil {
		return Reversed{}, err
	}
	nearest, err := rollbackReached(family, parts.Before, parts.After, left, restored)
	if err != nil {
		return Reversed{}, err
	}
	reversed := Reversed{Kind: family.kind, Before: parts.Before, After: parts.After, Left: nearest.left, Restored: nearest.restored}
	if !nearest.runs {
		return reversed, nil
	}
	if reversed.Value, err = family.captureIn(id, change.Owner, parts.Database, nearest.left, nearest.restored); err != nil {
		return Reversed{}, err
	}
	return reversed, nil
}

// reversedOperands converts the forward change's operands into the reverse
// change's: what the change asked for, as a read would report it, and what
// it found, as a declaration.
func reversedOperands(before, after *protocol.Envelope, convert func(*protocol.Envelope, protocol.ValueInput_Representation, protocol.ValueInput_Representation) (*protocol.Envelope, error),
	access AccessWire,
) (left, restored *protocol.Envelope, err error) {
	if after != nil {
		if left, err = convert(after, protocol.ValueInput_DESIRED, protocol.ValueInput_OBSERVED); err != nil {
			return nil, nil, err
		}
		left.Context = proto.CloneOf(after.Context)
	}
	if before != nil {
		if restored, err = convert(before, protocol.ValueInput_OBSERVED, protocol.ValueInput_DESIRED); err != nil {
			return nil, nil, err
		}
		if access == CommonAccess {
			if restored, err = WithoutAccess(restored); err != nil {
				return nil, nil, err
			}
		}
		restored.Context = proto.CloneOf(before.Context)
	}
	return left, restored, nil
}

// keepHeldConsumer carries the consumer a transfer reads through from what
// the forward change found to what it leaves, where the forward declaration
// names none. YDB keeps a transfer's consumer whatever else ALTER TRANSFER
// changes, and a declaration that names none takes the one the transfer has.
// Without it the reverse compared the consumer YDB made with none, a change
// YDB makes in no transfer in place, and refused the rollback of a lambda
// change (G233), as the host's embedded engine did.
func keepHeldConsumer(family *objectChanges, before, left *protocol.Envelope) (*protocol.Envelope, error) {
	if family != transferChanges || before == nil || left == nil {
		return left, nil
	}
	held, leaves := &native.TransferSettings{}, &native.TransferSettings{}
	if err := protojson.Unmarshal(before.GetPayload(), held); err != nil {
		return nil, err
	}
	if err := protojson.Unmarshal(left.GetPayload(), leaves); err != nil {
		return nil, err
	}
	if leaves.GetConsumer() != "" || held.GetConsumer() == "" {
		return left, nil
	}
	leaves.Consumer = held.GetConsumer()
	kept, err := convertedEnvelope(left, model(TransferProperties), leaves, Normalize)
	if err != nil {
		return nil, err
	}
	kept.Context = proto.CloneOf(left.Context)
	return kept, nil
}

// keepStateReset carries a streaming query's permission to reset its state
// from the forward declaration to the restored one. A read keeps no such
// permission, but the reverse of a text change the operator permitted resets
// the state again, and the streaming change refuses a text change without it.
func keepStateReset(family *objectChanges, after, restored *protocol.Envelope) (*protocol.Envelope, error) {
	if family != streamingChanges || after == nil || restored == nil {
		return restored, nil
	}
	forward, kept := &native.StreamingQueryIntent{}, &native.StreamingQueryIntent{}
	if err := protojson.Unmarshal(after.GetPayload(), forward); err != nil {
		return nil, err
	}
	if !forward.GetAllowStateReset() {
		return restored, nil
	}
	if err := protojson.Unmarshal(restored.GetPayload(), kept); err != nil {
		return nil, err
	}
	kept.AllowStateReset = true
	return convertedEnvelope(restored, NativeDesiredModel(StreamingQueryProperties), kept, NormalizeDesired)
}

// ReversibleTableChanges lists the change kinds ReverseTableChange undoes.
func ReversibleTableChanges() []string { return []string{ColumnStorageChange, TablePropertiesChange} }

// ReverseTableChange returns the change that undoes change, a change of a
// table's own settings, under id: the row table's properties, its index set
// or its changefeed set, or a column table's storage. Both operands of such a
// change are present, and the reverse swaps them as ReverseObjectChange does:
// what the forward change leaves, as a read would report it, and what it
// found, as a declaration. A column's change, its move to another family,
// reverses the same way under the column, and a change of a global index's
// partitioning and read replicas under the index (see reverseIndexSettings).
func ReverseTableChange(id string, change *protocol.Envelope, access AccessWire) (Reversed, error) {
	var before, after *protocol.Envelope
	var err error
	if change.GetKind() == TablePropertiesChange && change.GetOwner().GetKind() == "index" {
		return reverseIndexSettings(id, change)
	}
	switch change.GetKind() {
	case TablePropertiesChange:
		before, after, err = tablePropertiesOperands(change)
	case ColumnStorageChange:
		before, after, err = ColumnStorageOperands(change)
		if err == nil && (before == nil || after == nil) {
			err = fmt.Errorf("a change between row and column storage has no reversal")
		}
	default:
		err = fmt.Errorf("YDB change %s has no reversal", change.GetKind())
	}
	if err != nil {
		return Reversed{}, err
	}
	convert := ConvertNative
	var altered func(before, after *protocol.Envelope) (*protocol.Envelope, error)
	switch before.Kind {
	case TableProperties:
		convert, altered = restoredTable, alteredTable
	case ColumnProperties:
		convert, altered = ConvertColumn, alteredColumn
	case ChangefeedProperties:
		altered = alteredChangefeeds
	}
	left, restored, err := reversedOperands(before, after, convert, access)
	if altered != nil {
		// A table's properties are the whole table's description, and a
		// column's its family with its default and its storage settings,
		// while the declaration states only some of them: what the forward
		// change leaves is what the owner held with the declared settings in
		// place of its own. A changefeed's topic keeps what YDB derived for
		// it when no statement replaces the changefeed.
		left, restored, err = reversedOperands(before, nil, convert, access)
		if err == nil {
			left, err = altered(before, after)
		}
	}
	if err == nil && before.Kind == ChangefeedProperties {
		err = errors.Join(decidedChangefeeds(before, after), decidedChangefeeds(left, restored))
	}
	if err != nil {
		return Reversed{}, err
	}
	capture := CaptureTablePropertiesChange
	if change.GetKind() == ColumnStorageChange {
		capture = CaptureColumnStorageChange
	}
	value, err := capture(id, change.Owner, left, restored)
	if err != nil {
		return Reversed{}, err
	}
	return Reversed{Value: value, Kind: before.Kind, Before: before, After: after, Left: left, Restored: restored}, nil
}

// alteredChangefeeds is the changefeed set a table holding before reads back
// with once the forward change gives it the changefeeds after declares. A
// changefeed the change adds, or drops and adds again, reads as declared, its
// topic's partitions left to the count YDB derives from the table, which no
// declaration states. One the change keeps, its topic changed in place or not
// at all, keeps the partitions its topic held: ALTER TOPIC does not change
// them.
func alteredChangefeeds(before, after *protocol.Envelope) (*protocol.Envelope, error) {
	predicted, err := ConvertNative(after, protocol.ValueInput_DESIRED, protocol.ValueInput_OBSERVED)
	if err != nil {
		return nil, err
	}
	held, left, declared := &native.ChangefeedObservations{}, &native.ChangefeedObservations{}, &native.ChangefeedSetIntent{}
	for _, operand := range []struct {
		value  *protocol.Envelope
		target proto.Message
	}{{before, held}, {predicted, left}, {after, declared}} {
		if err := protojson.Unmarshal(operand.value.GetPayload(), operand.target); err != nil {
			return nil, err
		}
	}
	for _, feed := range left.GetChangefeeds() {
		name := feed.GetDescription().GetName()
		current, found := heldChangefeed(held, name)
		if !found {
			continue
		}
		have, err := changefeeds.FromObserved(current)
		if err != nil {
			return nil, err
		}
		want, err := changefeeds.FromIntent(declaredChangefeed(declared, name))
		if err != nil {
			return nil, err
		}
		if changefeeds.Recreated(want, have) || feed.GetTopic() == nil {
			continue
		}
		if feed.Topic.PartitioningSettings == nil {
			feed.Topic.PartitioningSettings = &Ydb_Topic.PartitioningSettings{}
		}
		feed.Topic.PartitioningSettings.MinActivePartitions = current.GetTopic().GetPartitioningSettings().GetMinActivePartitions()
	}
	altered, err := convertedEnvelope(predicted, model(ChangefeedProperties), left, Normalize)
	if err != nil {
		return nil, err
	}
	altered.Context = proto.CloneOf(after.Context)
	return altered, nil
}

// heldChangefeed returns the changefeed of set named name, and whether set
// holds one.
func heldChangefeed(set *native.ChangefeedObservations, name string) (*native.ObservedChangefeed, bool) {
	for _, feed := range set.GetChangefeeds() {
		if feed.GetDescription().GetName() == name {
			return feed, true
		}
	}
	return nil, false
}

// declaredChangefeed returns the changefeed of set named name, nil for none.
func declaredChangefeed(set *native.ChangefeedSetIntent, name string) *native.ChangefeedIntent {
	for _, feed := range set.GetChangefeeds() {
		if feed.GetName() == name {
			return feed
		}
	}
	return nil
}

// decidedChangefeeds refuses a changefeed set change, from the set a table
// holds to the one declared, whose steps cannot be decided: one that adds,
// drops or changes a stream an asynchronous replication controller owns,
// which the controller creates and removes, not the schema. A change of
// either direction that cannot be decided has no reverse.
func decidedChangefeeds(held, declared *protocol.Envelope) error {
	having, wanted := &native.ChangefeedObservations{}, &native.ChangefeedSetIntent{}
	if err := protojson.Unmarshal(held.GetPayload(), having); err != nil {
		return err
	}
	if err := protojson.Unmarshal(declared.GetPayload(), wanted); err != nil {
		return err
	}
	if _, undecided := changefeeds.Diff(wanted, having); undecided != "" {
		return fmt.Errorf("the changefeed change has no reverse: %s", undecided)
	}
	return nil
}

// reverseIndexSettings returns the change that undoes change, a change of a
// global index's partitioning and read replicas, under id. The declaration
// states only the settings it changes and keeps every other, so what the
// forward change leaves is what the index held with the declared settings in
// place of its own, and the reverse declares every setting the index held, as
// the host's embedded engine restores them: a reverse that stated only the
// settings that differ from a new index's would keep each setting the forward
// change moved away from one.
func reverseIndexSettings(id string, change *protocol.Envelope) (Reversed, error) {
	before, after, err := tablePropertiesOperands(change)
	if err != nil {
		return Reversed{}, err
	}
	held, declared := &native.IndexSettingsObservation{}, &native.IndexSettingsIntent{}
	if err := protojson.Unmarshal(before.GetPayload(), held); err != nil {
		return Reversed{}, err
	}
	if err := protojson.Unmarshal(after.GetPayload(), declared); err != nil {
		return Reversed{}, err
	}
	left, err := convertedEnvelope(before, model(IndexSettingsProperties), alteredIndexSettings(held, declared), Normalize)
	if err != nil {
		return Reversed{}, err
	}
	left.Context = proto.CloneOf(after.GetContext())
	restored, err := convertedEnvelope(after, NativeDesiredModel(IndexSettingsProperties), explicitIndexSettings(held), NormalizeDesired)
	if err != nil {
		return Reversed{}, err
	}
	restored.Context = proto.CloneOf(before.GetContext())
	value, err := CaptureTablePropertiesChange(id, change.GetOwner(), left, restored)
	if err != nil {
		return Reversed{}, err
	}
	return Reversed{Value: value, Kind: IndexSettingsProperties, Before: before, After: after, Left: left, Restored: restored}, nil
}

// TablePropertiesOperands returns the normalized operands of a table
// properties change: what the owner held, as a read reports it, and what the
// change asks it to hold, as a declaration. They are a table's properties, its
// index set or its changefeed set, a column's properties, or a global index's
// partitioning and read replicas, each under the change's owner.
func TablePropertiesOperands(change *protocol.Envelope) (before, after *protocol.Envelope, err error) {
	return tablePropertiesOperands(change)
}

// tablePropertiesOperands returns the normalized operands of a table
// properties change: a table's properties, its index set or its changefeed
// set, or a column's properties, each under the change's owner.
func tablePropertiesOperands(change *protocol.Envelope) (before, after *protocol.Envelope, err error) {
	normalized, err := NormalizeTablePropertiesChange(&protocol.ValueInput{Id: "operands", Representation: protocol.ValueInput_CHANGE, Value: change})
	if err != nil {
		return nil, nil, err
	}
	var operands map[string]json.RawMessage
	if err := json.Unmarshal(normalized.Value.Payload, &operands); err != nil {
		return nil, nil, err
	}
	kind, err := tableChangeOperandKind(change.GetOwner())
	if err != nil {
		return nil, nil, err
	}
	if selector, found := operands[facetKey]; found {
		if err := json.Unmarshal(selector, &kind); err != nil {
			return nil, nil, err
		}
	}
	operand := func(key string, definition *protocol.ValueModel) *protocol.Envelope {
		return &protocol.Envelope{ProviderId: ProviderID, Kind: kind, Owner: proto.CloneOf(change.Owner), SchemaVersion: definition.SchemaVersion, Encoding: definition.Encoding,
			DefinitionSha256: definition.DefinitionSha256, Payload: bytes.Clone(operands[key])}
	}
	return operand("before", model(kind)), operand("after", desiredModel(kind)), nil
}

// alteredTable is the description a table holding before reads back with
// once an ALTER gives it the settings after declares: the settings are the
// declaration's, and what a declaration does not state, the columns, the key
// and the indexes among them, stays as the table held it.
func alteredTable(before, after *protocol.Envelope) (*protocol.Envelope, error) {
	held, declared := &native.DescribeTableResult{}, &native.RowTableIntent{}
	if err := protojson.Unmarshal(before.GetPayload(), held); err != nil {
		return nil, err
	}
	normalized, err := normalizeTableIntent(after)
	if err != nil {
		return nil, err
	}
	if err := protojson.Unmarshal(normalized.GetPayload(), declared); err != nil {
		return nil, err
	}
	settings := tableObservation(declared)
	result := proto.CloneOf(held)
	// A table the declaration gives no TTL loses the one it holds, as the
	// comparison reads it; a setting the declaration leaves unset otherwise
	// stays as the table holds it.
	result.TtlSettings, result.Tiering = settings.TtlSettings, settings.Tiering
	if settings.StorageSettings != nil {
		result.StorageSettings = settings.StorageSettings
	}
	if settings.PartitioningSettings != nil {
		result.PartitioningSettings = settings.PartitioningSettings
	}
	if settings.KeyBloomFilter != Ydb.FeatureFlag_STATUS_UNSPECIFIED {
		result.KeyBloomFilter = settings.KeyBloomFilter
	}
	if settings.ReadReplicasSettings != nil {
		result.ReadReplicasSettings = settings.ReadReplicasSettings
	}
	if len(settings.ColumnFamilies) != 0 {
		result.ColumnFamilies = settings.ColumnFamilies
	}
	if len(settings.Attributes) != 0 {
		result.Attributes = settings.Attributes
	}
	altered, err := convertedEnvelope(before, model(TableProperties), result, Normalize)
	if err != nil {
		return nil, err
	}
	altered.Context = proto.CloneOf(after.Context)
	return altered, nil
}

// alteredColumn is the column a read reports once the forward change gives a
// column held as before what after declares: the family is the
// declaration's, since a column the declaration places in no family sits in
// the default one, and a default, a compression or an encoding the
// declaration does not state stays as the column held it.
func alteredColumn(before, after *protocol.Envelope) (*protocol.Envelope, error) {
	held, declared := &native.ColumnMeta{}, &native.ColumnMeta{}
	if err := protojson.Unmarshal(before.GetPayload(), held); err != nil {
		return nil, err
	}
	stated, err := ConvertColumn(after, protocol.ValueInput_DESIRED, protocol.ValueInput_OBSERVED)
	if err != nil {
		return nil, err
	}
	if err := protojson.Unmarshal(stated.GetPayload(), declared); err != nil {
		return nil, err
	}
	result := proto.CloneOf(held)
	result.Family = declared.Family
	if declared.DefaultValue != nil {
		result.DefaultValue = declared.DefaultValue
	}
	if declared.Compression != nil {
		result.Compression = declared.Compression
	}
	if len(declared.Encoding) != 0 {
		result.Encoding = declared.Encoding
	}
	altered, err := convertedEnvelope(before, model(ColumnProperties), result, Normalize)
	if err != nil {
		return nil, err
	}
	altered.Context = proto.CloneOf(after.Context)
	return altered, nil
}
