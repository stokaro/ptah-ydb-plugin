// Package reversal undoes the changes the provider's comparisons return, for
// the reverse migration the host writes beside a forward one. Each reverse
// change is planned by the same planner as a forward change of its kind; this
// package says what it asks for, what the forward change leaves, and what
// the reverse cannot recover, in the words the host's YDB engine uses.
package reversal

import (
	"context"
	"encoding/json"
	"fmt"
	"path"
	"reflect"
	"slices"
	"strings"
	"time"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/durationpb"
	protocol "ptah.run/provider-sdk/protocol/experimental"
	"ptah.run/provider-sdk/provider"

	"github.com/stokaro/ptah-ydb-plugin/internal/authored"
	"github.com/stokaro/ptah-ydb-plugin/internal/externals"
	native "github.com/stokaro/ptah-ydb-plugin/internal/nativeprotocol"
	"github.com/stokaro/ptah-ydb-plugin/internal/replication"
	"github.com/stokaro/ptah-ydb-plugin/internal/secrets"
	"github.com/stokaro/ptah-ydb-plugin/internal/streaming"
	"github.com/stokaro/ptah-ydb-plugin/internal/topics"
	"github.com/stokaro/ptah-ydb-plugin/internal/workload"
	"github.com/stokaro/ptah-ydb-plugin/internal/ydbdecl"
	"github.com/stokaro/ptah-ydb-plugin/schema"
)

// ServiceID names the reversal service of the provider's object changes.
const ServiceID = "ydb-objects"

// ChangeKinds lists the change kinds the service undoes.
func ChangeKinds() []string {
	return slices.Sorted(slices.Values(append(schema.ReversibleObjectChanges(), schema.ReversibleTableChanges()...)))
}

// capabilities names the target capability the reverse of each object change
// needs, the one its forward change needed: the host's embedded engine
// refused to reverse a change on a target without it.
var capabilities = map[string]string{
	schema.CoordinationChange: "coordination_nodes", schema.SecretChange: secrets.Capability, schema.TopicChange: topics.CapabilityTopics,
	schema.StreamingQueryChange: streaming.Capability, schema.ExternalSourceChange: externals.CapabilityExternalDataSources,
	schema.ExternalTableChange: externals.CapabilityExternalDataSources, schema.ResourcePoolChange: workload.CapabilityResourcePools,
	schema.ResourcePoolClassifierChange: workload.CapabilityResourcePools, schema.ReplicationChange: replication.CapabilityReplication,
	schema.TransferChange: replication.CapabilityTransfers,
}

// workloadLimitation is what restoring a resource pool or a classifier
// cannot undo.
const workloadLimitation = "Restoring workload configuration cannot undo queries run, queued, or rejected under the forward settings."

// Reverse returns one reverse per change of request, in order. The request
// has passed the SDK's checks. observe normalizes a predicted read under id
// as the provider normalizes an observed value; each prediction of a batch
// takes the id of the change it follows from.
func Reverse(ctx context.Context, request *protocol.ReverseChangesRequest, observe func(id string, value *protocol.Envelope) (*protocol.NormalizedValue, error)) (*protocol.ReversedChanges, error) {
	access := schema.NativeAccess
	if slices.Contains(request.GetTargetFeatures(), provider.CommonAccessFeature) {
		access = schema.CommonAccess
	}
	for _, change := range request.GetChanges() {
		if key, needed := capabilities[change.GetValue().GetValue().GetKind()]; needed && !request.GetCapabilities()[key] {
			owner := change.GetValue().GetValue().GetOwner()
			return nil, authored.New(fmt.Sprintf("the change of %s cannot be reversed: reversing it requires target capability %s, unavailable on this ydb target",
				path.Join(owner.GetSchema(), owner.GetName()), key))
		}
	}
	result := &protocol.ReversedChanges{Complete: true}
	for _, change := range request.GetChanges() {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		reverse, placement := schema.ReverseObjectChange, protocol.ForwardState_OBJECT
		if slices.Contains(schema.ReversibleTableChanges(), change.GetValue().GetValue().GetKind()) {
			reverse, placement = schema.ReverseTableChange, protocol.ForwardState_FACET
		}
		reversed, err := reverse(change.GetValue().GetId(), change.GetValue().GetValue(), access)
		if err != nil {
			owner := change.GetValue().GetValue().GetOwner()
			return nil, authored.Wrap(err, fmt.Sprintf("the change of %s cannot be reversed: %v", path.Join(owner.GetSchema(), owner.GetName()), err))
		}
		described, err := describe(change.GetValue().GetValue(), reversed)
		if err != nil {
			return nil, err
		}
		state := &protocol.ForwardState{Placement: placement, Kind: reversed.Kind}
		if reversed.Left != nil {
			state.Value, err = observe(change.GetValue().GetId(), reversed.Left)
			if err != nil {
				return nil, err
			}
		}
		described.Subject = proto.CloneOf(change.GetSubject())
		described.ForwardState = []*protocol.ForwardState{state}
		result.Changes = append(result.Changes, described)
	}
	return result, nil
}

// topicChangeLimitations names what undoing an in-place change of the topic at
// name from before to after cannot restore, in the words of the host's
// embedded engine: the partitions YDB never removes, the auto-partitioning it
// does not disable again, and the positions of the consumers the change or
// its rollback drops.
func topicChangeLimitations(name string, before, after ydbdecl.Topic) []string {
	var limitations []string
	from, to := topics.Resolve(before), topics.Resolve(after)
	if to.MinActivePartitions > from.MinActivePartitions {
		limitations = append(limitations, fmt.Sprintf("topic %s keeps the %d partitions the change gave it, since YDB never removes a partition",
			name, to.MinActivePartitions))
	}
	if to.AutoPartitioned() && !from.AutoPartitioned() {
		limitations = append(limitations, fmt.Sprintf("topic %s keeps auto-partitioning, paused, since YDB does not disable it once it is on", name))
	}
	consumers := topics.Compare(after, before)
	for _, consumer := range consumers.Removed {
		limitations = append(limitations, fmt.Sprintf("consumer %q of topic %s was dropped; the rollback adds it again without its position", consumer, name))
	}
	for _, consumer := range consumers.Added {
		limitations = append(limitations, fmt.Sprintf("consumer %q the change added to topic %s is dropped by the rollback, with its position", consumer, name))
	}
	for _, consumer := range consumers.Restarted {
		limitations = append(limitations, fmt.Sprintf("consumer %q of topic %s was dropped and added again; its earlier position is lost", consumer, name))
	}
	return limitations
}

// describe names the strategy and the limitations of one reverse, and sets
// its change. A secret's rotation has no reverse statement: the value it
// replaced was never read.
func describe(change *protocol.Envelope, reversed schema.Reversed) (*protocol.ReversedChange, error) {
	owner := change.GetOwner()
	name := path.Join(owner.GetSchema(), owner.GetName())
	created, dropped := reversed.Before == nil, reversed.After == nil
	result := &protocol.ReversedChange{Value: reversed.Value}
	switch change.GetKind() {
	case schema.SecretChange:
		if err := describeSecret(result, reversed, name); err != nil {
			return nil, err
		}
	case schema.CoordinationChange:
		result.Strategy, _ = lifecycle(created, dropped, "drop the created coordination node", "recreate the dropped coordination node", "restore coordination settings in place")
		switch {
		case created:
			result.Limitations = []string{"Dropping the new node discards its semaphores and rate limiter resources."}
		case dropped:
			result.Limitations = []string{"Recreating the configuration cannot recover semaphores or rate limiter resources lost when the node was dropped."}
		}
	case schema.TopicChange:
		if err := describeTopic(result, change, created, dropped, name); err != nil {
			return nil, err
		}
	case schema.StreamingQueryChange:
		if err := describeStreamingQuery(result, reversed); err != nil {
			return nil, err
		}
	case schema.ExternalSourceChange, schema.ExternalTableChange:
		family := map[string]string{schema.ExternalSourceChange: "data source", schema.ExternalTableChange: "external table"}[change.GetKind()]
		result.Strategy, _ = lifecycle(created, dropped, "drop the created "+family, "create the dropped "+family+" again with the definition it had", "replace the "+family+" with the definition it had")
	case schema.ResourcePoolChange:
		result.Strategy, _ = lifecycle(created, dropped, "remove the created resource pool", "recreate the removed resource pool", "restore captured resource pool settings")
		result.Limitations = []string{workloadLimitation}
	case schema.ResourcePoolClassifierChange:
		result.Strategy, _ = lifecycle(created, dropped, "remove the created resource pool classifier", "recreate the removed resource pool classifier",
			"restore captured resource pool classifier settings")
		result.Limitations = []string{workloadLimitation}
	case schema.ReplicationChange, schema.TransferChange:
		describeReplicated := map[string]func(*protocol.ReversedChange, schema.Reversed, string) error{
			schema.ReplicationChange: describeReplication, schema.TransferChange: describeTransfer}[change.GetKind()]
		if err := describeReplicated(result, reversed, name); err != nil {
			return nil, err
		}
	case schema.TablePropertiesChange, schema.ColumnStorageChange:
		if err := describeTableSettings(result, reversed, name); err != nil {
			return nil, err
		}
	default:
		return nil, authored.New("YDB change " + change.GetKind() + " has no reversal")
	}
	return result, nil
}

func describeTableSettings(result *protocol.ReversedChange, reversed schema.Reversed, name string) error {
	if reversed.Kind == schema.ChangefeedProperties {
		limitations, err := changefeedLimitations(reversed, name)
		if err != nil {
			return err
		}
		result.Strategy, result.Limitations = "restore the changefeeds the table held", limitations
		return nil
	}
	if reversed.Kind == schema.IndexSettingsProperties {
		limitations, err := indexSettingsLimitations(reversed)
		if err != nil {
			return err
		}
		result.Strategy, result.Limitations = "restore every setting the index held in place", limitations
		return nil
	}
	ttl, err := ttlTransition(reversed)
	if err != nil {
		return err
	}
	result.Strategy, result.Limitations = tableSettings(reversed.Kind, name, ttl)
	if reversed.Kind == schema.TableProperties {
		familiesChanged, retained, err := familySettingsLimitations(reversed)
		if err != nil {
			return err
		}
		if familiesChanged && ttl == ttlKept {
			result.Strategy = "move each column back to its family and restore each family setting in place"
		}
		result.Limitations = append(result.Limitations, retained...)
	}
	return nil
}

// familySettingsLimitations reports family settings the reverse cannot undo.
// YQL can change compression and column placement, but it cannot drop a family
// or remove a storage pool assigned to one. A newly added family keeps its
// settings, so it needs only one limitation entry.
func familySettingsLimitations(reversed schema.Reversed) (bool, []string, error) {
	before, left := &native.DescribeTableResult{}, &native.DescribeTableResult{}
	if err := protojson.Unmarshal(reversed.Before.GetPayload(), before); err != nil {
		return false, nil, err
	}
	if err := protojson.Unmarshal(reversed.Left.GetPayload(), left); err != nil {
		return false, nil, err
	}
	held := make(map[string]*native.ColumnFamily, len(before.GetColumnFamilies()))
	for _, family := range before.GetColumnFamilies() {
		held[family.GetName()] = family
	}
	changed := len(before.GetColumnFamilies()) != len(left.GetColumnFamilies())
	var addedFamilies, addedData []string
	for _, family := range left.GetColumnFamilies() {
		previous, found := held[family.GetName()]
		if !found {
			changed = true
			addedFamilies = append(addedFamilies, "family "+family.GetName())
			continue
		}
		changed = changed || !proto.Equal(previous, family)
		if previous.GetData().GetMedia() == "" && family.GetData().GetMedia() != "" {
			addedData = append(addedData, "the DATA of family "+family.GetName())
		}
	}
	slices.Sort(addedFamilies)
	slices.Sort(addedData)
	retained := slices.Concat(addedFamilies, addedData)
	if len(retained) == 0 {
		return changed, nil, nil
	}
	kept := retained[0]
	if len(retained) > 1 {
		kept = strings.Join(retained[:len(retained)-1], ", ") + " and " + retained[len(retained)-1]
	}
	return changed, []string{"YQL cannot drop a column family or unset its DATA pool, so the rollback keeps " + kept + ", which the forward change added."}, nil
}

// describeTopic describes the reversal of a topic change: a created topic is
// dropped with its messages, a dropped one is created again empty, and an
// in-place change names what its rollback cannot restore.
func describeTopic(result *protocol.ReversedChange, change *protocol.Envelope, created, dropped bool, name string) error {
	result.Strategy, _ = lifecycle(created, dropped, "drop the created topic", "create the dropped topic again with its settings and consumers", "restore the topic's settings and consumers in place")
	switch {
	case created:
		result.Limitations = []string{fmt.Sprintf("dropping topic %s discards every message written to it and every consumer's position", name)}
	case dropped:
		result.Limitations = []string{fmt.Sprintf("the messages topic %s held and its consumers' positions were dropped; the rollback creates it empty", name)}
	default:
		current, desired, err := schema.TopicSpecs(change)
		if err != nil {
			return err
		}
		if current != nil && desired != nil {
			result.Limitations = topicChangeLimitations(name, *current, *desired)
		}
	}
	return nil
}

// describeReplication describes the reversal of an async replication change:
// a created replication is dropped with its replica tables, a dropped one is
// created again and copies its source from the start, and one failed over
// before the change dropped it stays dropped, since its tables stayed as
// ordinary tables. A credential the change added stays.
func describeReplication(result *protocol.ReversedChange, reversed schema.Reversed, name string) error {
	created, dropped := reversed.Before == nil, reversed.After == nil
	result.Strategy, _ = lifecycle(created, dropped, "drop the created replication with its replica tables", "create the dropped replication again with its connection and items",
		"restore the replication's connection and credential in place")
	if created {
		result.Limitations = []string{fmt.Sprintf("dropping async replication %s drops the replica tables it created, with every row it copied", name)}
		return nil
	}
	held, state, err := schema.ObservedReplication(reversed.Before)
	if err != nil {
		return err
	}
	switch {
	case dropped && reversed.Value == nil:
		result.Strategy = "leave the failed-over replication dropped"
		result.Limitations = []string{fmt.Sprintf("async replication %s was failed over before it was dropped, so it is not created again: "+
			"it replicated nothing more, and its tables stayed as ordinary tables", name)}
	case dropped:
		result.Limitations = []string{fmt.Sprintf("the replica tables async replication %s dropped are created again empty, and the replication copies its source again from the start", name)}
		if state == replication.StatePaused {
			result.Limitations = append(result.Limitations, fmt.Sprintf("async replication %s was paused when it was dropped, and runs once it is created again", name))
		}
	default:
		declared, err := schema.DesiredReplication(reversed.After)
		if err != nil {
			return err
		}
		if replication.RollbackConnection(held.Connection, declared.Connection) != held.Connection {
			result.Limitations = []string{fmt.Sprintf("the credential the change gave async replication %s stays, since YDB has no statement that takes a credential away", name)}
		}
	}
	return nil
}

// describeTransfer describes the reversal of a transfer change: the rows a
// transfer wrote stay whatever its reverse does, the consumer YDB created for
// a created transfer goes with it, and a dropped one created again does not
// resume from its consumer's position. A credential the change added stays.
func describeTransfer(result *protocol.ReversedChange, reversed schema.Reversed, name string) error {
	created, dropped := reversed.Before == nil, reversed.After == nil
	result.Strategy, _ = lifecycle(created, dropped, "drop the created transfer", "create the dropped transfer again with its lambda and settings",
		"restore the transfer's lambda, batch settings and connection in place")
	var held, declared replication.TransferSpec
	var err error
	if !created {
		if held, _, err = schema.ObservedTransfer(reversed.Before); err != nil {
			return err
		}
	}
	if !dropped {
		if declared, err = schema.DesiredTransfer(reversed.After); err != nil {
			return err
		}
	}
	switch {
	case created:
		result.Limitations = []string{fmt.Sprintf("the rows transfer %s wrote into table %s stay", name, declared.Target)}
		if declared.Consumer == "" {
			result.Limitations = append(result.Limitations, fmt.Sprintf("dropping transfer %s drops the consumer YDB created for it, with its position in topic %s", name, declared.Source))
		}
	case dropped:
		result.Limitations = []string{fmt.Sprintf("dropping transfer %s removed the consumer YDB created for it, if it had one, with its position in topic %s; "+
			"the transfer created again does not resume from it", name, held.Source)}
	default:
		if replication.CompareTransfer(declared, held).Lambda {
			result.Limitations = append(result.Limitations, fmt.Sprintf("the rows transfer %s wrote with the changed lambda stay as written", name))
		}
		if replication.RollbackConnection(held.Connection, declared.Connection) != held.Connection {
			result.Limitations = append(result.Limitations, fmt.Sprintf("the credential the change gave transfer %s stays, since YDB has no statement that takes a credential away", name))
		}
	}
	return nil
}

// describeSecret sets the strategy and the limitations of a secret's reverse
// in result. A rotation is reversed by nothing: result keeps no change, and the
// rotated value stays.
func describeSecret(result *protocol.ReversedChange, reversed schema.Reversed, name string) error {
	switch {
	case reversed.Before == nil:
		result.Strategy = "drop the created secret"
	case reversed.After == nil:
		env, err := restoredValueEnv(reversed)
		if err != nil {
			return err
		}
		result.Strategy = "create the dropped secret again"
		result.Limitations = []string{fmt.Sprintf("the dropped value of secret %s was never read; the rollback takes the value %s holds when it runs", name, env)}
	default:
		result.Value = nil
		result.Strategy = "keep the secret and the value it holds"
		result.Limitations = []string{fmt.Sprintf("the value secret %s held before the rotation was never read; the rollback keeps the rotated value", name)}
	}
	return nil
}

// describeStreamingQuery sets the strategy and the limitations of a streaming
// query's reverse in result. Creating, dropping and changing the query text
// each lose the checkpoint; restoring execution settings keeps it.
func describeStreamingQuery(result *protocol.ReversedChange, reversed schema.Reversed) error {
	created, dropped := reversed.Before == nil, reversed.After == nil
	changedText, err := streamingTextChanged(reversed)
	if err != nil {
		return err
	}
	settings := "restore streaming execution settings in place"
	if changedText {
		settings = "restore the query body in place"
	}
	result.Strategy, _ = lifecycle(created, dropped, "drop the created streaming query", "recreate the dropped streaming query", settings)
	if created || dropped || changedText {
		result.Limitations = []string{streaming.CheckpointLoss}
	}
	return nil
}

// lifecycle picks the strategy of a reverse by what the forward change did.
func lifecycle(created, dropped bool, drop, recreate, restore string) (string, []string) {
	switch {
	case created:
		return drop, nil
	case dropped:
		return recreate, nil
	default:
		return restore, nil
	}
}

// restoredValueEnv is the variable the secret a reverse creates again takes
// its value from.
func restoredValueEnv(reversed schema.Reversed) (string, error) {
	intent := &native.SecretIntent{}
	if err := protojson.Unmarshal(reversed.Restored.GetPayload(), intent); err != nil {
		return "", err
	}
	return intent.GetValueEnv(), nil
}

// streamingTextChanged reports whether a streaming query change changes the
// query's text, which resets its checkpoint, rather than how it runs.
func streamingTextChanged(reversed schema.Reversed) (bool, error) {
	if reversed.Before == nil || reversed.After == nil {
		return false, nil
	}
	held, wanted := &native.StreamingQuerySettings{}, &native.StreamingQueryIntent{}
	if err := protojson.Unmarshal(reversed.Before.GetPayload(), held); err != nil {
		return false, err
	}
	if err := protojson.Unmarshal(reversed.After.GetPayload(), wanted); err != nil {
		return false, err
	}
	return held.GetText() != wanted.GetText(), nil
}

// indexSettingsLimitations is what the reverse of a change of a global index's
// settings cannot undo, in the words of the host's engine: YDB removes no
// maximum partition count, so the maximum the forward change gives an index
// that held none stays.
func indexSettingsLimitations(reversed schema.Reversed) ([]string, error) {
	held, left := &native.IndexSettingsObservation{}, &native.IndexSettingsObservation{}
	if err := protojson.Unmarshal(reversed.Before.GetPayload(), held); err != nil {
		return nil, err
	}
	if err := protojson.Unmarshal(reversed.Left.GetPayload(), left); err != nil {
		return nil, err
	}
	maximum := left.GetPartitioningSettings().GetMaxPartitionsCount()
	if held.GetPartitioningSettings().GetMaxPartitionsCount() != 0 || maximum == 0 {
		return nil, nil
	}
	return []string{fmt.Sprintf("YDB cannot remove a maximum partition count, so the rollback keeps AUTO_PARTITIONING_MAX_PARTITIONS_COUNT = %d, "+
		"which the forward change set.", maximum)}, nil
}

// changefeedRetention is the retention YDB gives a changefeed's topic that
// states none.
var changefeedRetention = durationpb.New(24 * time.Hour)

// changefeedLimitations names what the reverse of a change of the changefeed
// set of table cannot undo, in the words of the host's engine: a changefeed
// the forward change dropped, or recreated in another mode or format, starts
// a new stream when the rollback adds it again, one the forward change added
// is dropped with its unread messages, and a retention the forward change made
// shorter has expired messages the rollback cannot recover. A change the
// rollback undoes in place otherwise loses nothing. Without
// reading the set, the reverse of a retention change in place said a stream
// was added again (G234).
func changefeedLimitations(reversed schema.Reversed, table string) ([]string, error) {
	held, left := &native.ChangefeedObservations{}, &native.ChangefeedObservations{}
	if reversed.Before != nil {
		if err := protojson.Unmarshal(reversed.Before.GetPayload(), held); err != nil {
			return nil, err
		}
	}
	if reversed.Left != nil {
		if err := protojson.Unmarshal(reversed.Left.GetPayload(), left); err != nil {
			return nil, err
		}
	}
	leaves := make(map[string]*native.ObservedChangefeed, len(left.GetChangefeeds()))
	for _, feed := range left.GetChangefeeds() {
		leaves[feed.GetDescription().GetName()] = feed
	}
	retention := func(feed *native.ObservedChangefeed) *durationpb.Duration {
		if period := feed.GetTopic().GetRetentionPeriod(); period != nil {
			return period
		}
		return changefeedRetention
	}
	var restarted, expired, added bool
	found := make(map[string]bool, len(held.GetChangefeeds()))
	for _, feed := range held.GetChangefeeds() {
		found[feed.GetDescription().GetName()] = true
		after, kept := leaves[feed.GetDescription().GetName()]
		switch {
		case !kept || after.GetDescription().GetMode() != feed.GetDescription().GetMode() || after.GetDescription().GetFormat() != feed.GetDescription().GetFormat():
			restarted = true
		case retention(after).AsDuration() < retention(feed).AsDuration():
			expired = true
		}
	}
	for name := range leaves {
		added = added || !found[name]
	}
	var limitations []string
	if added {
		limitations = append(limitations, fmt.Sprintf("a changefeed the forward change added to table %s is dropped by the rollback with its topic; its unread messages and consumer positions are lost", table))
	}
	if restarted {
		limitations = append(limitations, fmt.Sprintf("a changefeed of table %s the rollback adds again starts a new stream; the messages and the consumers' positions of the dropped one are lost", table))
	}
	if expired {
		limitations = append(limitations, "Restoring retention cannot recover messages that have already expired.")
	}
	return limitations, nil
}

// ttlChange is what a change of a table's own settings does to its TTL.
type ttlChange int

const (
	ttlKept ttlChange = iota
	ttlAdded
	ttlChanged
	ttlRemoved
)

// tableSettings is the strategy and the limitations of the reverse of a
// change of a table's own settings of kind, or of a column's family, in the
// words of the host's engine. A TTL the forward change set or changed has deleted rows the
// reverse cannot bring back.
func tableSettings(kind, table string, ttl ttlChange) (string, []string) {
	lost := []string{"Restoring the prior TTL cannot recover rows YDB deleted under the forward TTL."}
	switch {
	case kind == schema.ColumnProperties:
		return "move the column back to its family", nil
	case kind == schema.IndexProperties:
		return "restore the indexes the table held", []string{fmt.Sprintf("an index of table %s the rollback adds again is built anew from the table's rows", table)}
	case ttl == ttlAdded:
		return "remove the TTL the forward change added", lost
	case ttl == ttlChanged:
		return "restore the captured TTL in place", lost
	case ttl == ttlRemoved:
		return "restore the captured TTL in place", nil
	default:
		return "restore every setting the table held in place", nil
	}
}

// ttlTransition reads what a row table's or a column table's change does to
// its TTL: the TTL settings of what the forward change found and of what it
// leaves.
func ttlTransition(reversed schema.Reversed) (ttlChange, error) {
	if reversed.Kind != schema.TableProperties && reversed.Kind != schema.ColumnStorageProperties {
		return ttlKept, nil
	}
	key := map[string]string{schema.TableProperties: "ttlSettings", schema.ColumnStorageProperties: "ttl"}[reversed.Kind]
	read := func(value *protocol.Envelope) (any, error) {
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(value.GetPayload(), &fields); err != nil || fields[key] == nil {
			return nil, err
		}
		var ttl any
		return ttl, json.Unmarshal(fields[key], &ttl)
	}
	held, err := read(reversed.Before)
	if err != nil {
		return ttlKept, err
	}
	left, err := read(reversed.Left)
	if err != nil {
		return ttlKept, err
	}
	switch {
	case reflect.DeepEqual(held, left):
		return ttlKept, nil
	case held == nil:
		return ttlAdded, nil
	case left == nil:
		return ttlRemoved, nil
	default:
		return ttlChanged, nil
	}
}
