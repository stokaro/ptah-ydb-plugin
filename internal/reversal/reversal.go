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

	"github.com/stokaro/ptah-ydb-plugin/internal/authored"
	native "github.com/stokaro/ptah-ydb-plugin/internal/nativeprotocol"
	"github.com/stokaro/ptah-ydb-plugin/internal/streaming"
	"github.com/stokaro/ptah-ydb-plugin/schema"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/stokaro/ptah/provider-sdk/provider"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

// ServiceID names the reversal service of the provider's object changes.
const ServiceID = "ydb-objects"

// ChangeKinds lists the change kinds the service undoes.
func ChangeKinds() []string {
	return slices.Sorted(slices.Values(append(schema.ReversibleObjectChanges(), schema.ReversibleTableChanges()...)))
}

// workloadLimitation is what restoring a resource pool or a classifier
// cannot undo.
const workloadLimitation = "Restoring workload configuration cannot undo queries run, queued, or rejected under the forward settings."

// Reverse returns one reverse per change of request, in order. The request
// has passed the SDK's checks. observe normalizes a predicted read under id
// as the provider normalizes an observed value; each prediction of a batch
// takes the id of the change it follows from.
func Reverse(ctx context.Context, request *protocol.ReverseChangesRequest, observe func(id string, value *protocol.Envelope) (*protocol.NormalizedValue, error)) (*protocol.ReversedChanges, error) {
	commonAccess := slices.Contains(request.GetTargetFeatures(), provider.CommonAccessFeature)
	result := &protocol.ReversedChanges{Complete: true}
	for _, change := range request.GetChanges() {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		reverse, placement := schema.ReverseObjectChange, protocol.ForwardState_OBJECT
		if slices.Contains(schema.ReversibleTableChanges(), change.GetValue().GetValue().GetKind()) {
			reverse, placement = schema.ReverseTableChange, protocol.ForwardState_FACET
		}
		reversed, err := reverse(change.GetValue().GetId(), change.GetValue().GetValue(), commonAccess)
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
		switch {
		case created:
			result.Strategy = "drop the created secret"
		case dropped:
			env, err := restoredValueEnv(reversed)
			if err != nil {
				return nil, err
			}
			result.Strategy = "create the dropped secret again"
			result.Limitations = []string{fmt.Sprintf("the dropped value of secret %s was never read; the rollback takes the value %s holds when it runs", name, env)}
		default:
			result.Value = nil
			result.Strategy = "keep the secret and the value it holds"
			result.Limitations = []string{fmt.Sprintf("the value secret %s held before the rotation was never read; the rollback keeps the rotated value", name)}
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
		result.Strategy, _ = lifecycle(created, dropped, "drop the created topic", "create the dropped topic again with its settings and consumers", "restore the topic's settings and consumers in place")
		switch {
		case created:
			result.Limitations = []string{fmt.Sprintf("dropping topic %s discards every message written to it and every consumer's position", name)}
		case dropped:
			result.Limitations = []string{fmt.Sprintf("the messages topic %s held and its consumers' positions were dropped; the rollback creates it empty", name)}
		}
	case schema.StreamingQueryChange:
		changedText, err := streamingTextChanged(reversed)
		if err != nil {
			return nil, err
		}
		settings := "restore streaming execution settings in place"
		if changedText {
			settings = "restore the query body in place"
		}
		result.Strategy, _ = lifecycle(created, dropped, "drop the created streaming query", "recreate the dropped streaming query", settings)
		if created || dropped || changedText {
			result.Limitations = []string{streaming.CheckpointLoss}
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
	case schema.ReplicationChange:
		result.Strategy, _ = lifecycle(created, dropped, "drop the created replication with its replica tables", "create the dropped replication again with its connection and items",
			"restore the replication's connection and credential in place")
		switch {
		case created:
			result.Limitations = []string{fmt.Sprintf("dropping async replication %s drops the replica tables it created, with every row it copied", name)}
		case dropped:
			result.Limitations = []string{fmt.Sprintf("the replica tables async replication %s dropped are created again empty, and the replication copies its source again from the start", name)}
		}
	case schema.TransferChange:
		result.Strategy, _ = lifecycle(created, dropped, "drop the created transfer", "create the dropped transfer again with its lambda and settings",
			"restore the transfer's lambda, batch settings and connection in place")
		switch {
		case created:
			result.Limitations = []string{fmt.Sprintf("the rows transfer %s wrote into its target table stay", name)}
		case dropped:
			result.Limitations = []string{fmt.Sprintf("dropping transfer %s removed the consumer YDB created for it, if it had one, with its position in its source topic; "+
				"the transfer created again does not resume from it", name)}
		}
	case schema.TablePropertiesChange, schema.ColumnStorageChange:
		ttl, err := ttlTransition(reversed)
		if err != nil {
			return nil, err
		}
		result.Strategy, result.Limitations = tableSettings(reversed.Kind, name, ttl)
	default:
		return nil, authored.New("YDB change " + change.GetKind() + " has no reversal")
	}
	return result, nil
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

// ttlChange is what a change of a table's own settings does to its TTL.
type ttlChange int

const (
	ttlKept ttlChange = iota
	ttlAdded
	ttlChanged
	ttlRemoved
)

// tableSettings is the strategy and the limitations of the reverse of a
// change of a table's own settings of kind, in the words of the host's
// engine. A TTL the forward change set or changed has deleted rows the
// reverse cannot bring back.
func tableSettings(kind, table string, ttl ttlChange) (string, []string) {
	lost := []string{"Restoring the prior TTL cannot recover rows YDB deleted under the forward TTL."}
	switch {
	case kind == schema.IndexProperties:
		return "restore the indexes the table held", []string{fmt.Sprintf("an index of table %s the rollback adds again is built anew from the table's rows", table)}
	case kind == schema.ChangefeedProperties:
		return "restore the changefeeds the table held", []string{fmt.Sprintf("a changefeed of table %s the rollback adds again starts a new stream; the messages and the consumers' positions of the dropped one are lost", table)}
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
