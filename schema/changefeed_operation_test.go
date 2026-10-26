package schema_test

import (
	"encoding/json"
	"testing"

	qt "github.com/frankban/quicktest"
	"github.com/stokaro/ptah-ydb-plugin/internal/changefeeds"
	native "github.com/stokaro/ptah-ydb-plugin/internal/nativeprotocol"
	"github.com/stokaro/ptah-ydb-plugin/schema"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb_Table"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb_Topic"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/durationpb"
)

// readFeed is a changefeed as inspection captures it, with one consumer.
func readFeed(consumers ...string) *native.ObservedChangefeed {
	feed := &native.ObservedChangefeed{Description: &native.ChangefeedDescription{Name: "f", Mode: Ydb_Table.ChangefeedMode_MODE_UPDATES,
		Format: Ydb_Table.ChangefeedFormat_FORMAT_JSON, State: native.ChangefeedDescription_STATE_ENABLED},
		Topic: &native.DescribeTopicResult{PartitioningSettings: &Ydb_Topic.PartitioningSettings{}}}
	for _, name := range consumers {
		feed.Topic.Consumers = append(feed.Topic.Consumers, &native.Consumer{Name: name})
	}
	return feed
}

// declaredFeed is a declared changefeed of the given mode, with consumers.
func declaredFeed(mode Ydb_Table.ChangefeedMode_Mode, consumers ...string) *native.ChangefeedIntent {
	feed := &native.ChangefeedIntent{Name: "f", Mode: mode, Format: Ydb_Table.ChangefeedFormat_FORMAT_JSON, Topic: &native.TopicIntent{PartitioningSettings: &Ydb_Topic.PartitioningSettings{}}}
	for _, name := range consumers {
		feed.Topic.Consumers = append(feed.Topic.Consumers, &native.ConsumerIntent{Name: name})
	}
	return feed
}

// named gives a feed another name.
func named[F interface {
	*native.ObservedChangefeed | *native.ChangefeedIntent
}](name string, feed F) F {
	switch value := any(feed).(type) {
	case *native.ObservedChangefeed:
		value.Description.Name = name
	case *native.ChangefeedIntent:
		value.Name = name
	}
	return feed
}

func TestChangefeedOperation_HappyPath(t *testing.T) {
	updates := Ydb_Table.ChangefeedMode_MODE_UPDATES
	keysOnly := Ydb_Table.ChangefeedMode_MODE_KEYS_ONLY
	longer := declaredFeed(updates)
	longer.Topic.RetentionPeriod = durationpb.New(172800e9)
	for _, test := range []struct {
		name    string
		changes []changefeeds.Change
		actions []changefeeds.Action
		impact  protocol.ChangeSemantics_Impact
		access  protocol.ChangeSemantics_Access
		reason  string
	}{
		{"an addition", []changefeeds.Change{{Name: "f", After: declaredFeed(updates, "c")}}, []changefeeds.Action{changefeeds.Add},
			protocol.ChangeSemantics_ADDITIVE, protocol.ChangeSemantics_UNCHANGED, `ADD CHANGEFEED adds a stream of the table's changes and its topic; .*`},
		{"a removal", []changefeeds.Change{{Name: "f", Before: readFeed("c")}}, []changefeeds.Action{changefeeds.Drop},
			protocol.ChangeSemantics_DESTRUCTIVE, protocol.ChangeSemantics_NARROWS, `DROP CHANGEFEED removes the change stream .*\. Changefeed f of table app/records is dropped with its topic: ` +
				`the records nobody read are lost, and so is consumer c\.`},
		{"a recreation", []changefeeds.Change{{Name: "f", Before: readFeed(), After: declaredFeed(keysOnly)}}, []changefeeds.Action{changefeeds.Recreate},
			protocol.ChangeSemantics_DESTRUCTIVE, protocol.ChangeSemantics_UNCHANGED,
			`.*Changefeed f of table app/records is dropped and added again, because YDB changes no option of a changefeed in place\..*`},
		{"a consumer added", []changefeeds.Change{{Name: "f", Before: readFeed(), After: declaredFeed(updates, "c")}}, []changefeeds.Action{changefeeds.Topic},
			protocol.ChangeSemantics_ADDITIVE, protocol.ChangeSemantics_UNCHANGED, `ALTER TOPIC adds consumers to a changefeed's topic`},
		{"a consumer dropped", []changefeeds.Change{{Name: "f", Before: readFeed("c"), After: declaredFeed(updates)}}, []changefeeds.Action{changefeeds.Topic},
			protocol.ChangeSemantics_DESTRUCTIVE, protocol.ChangeSemantics_UNCHANGED, `ALTER TOPIC can drop a consumer's position .*`},
		{"a longer retention", []changefeeds.Change{{Name: "f", Before: readFeed(), After: longer}}, []changefeeds.Action{changefeeds.Topic},
			protocol.ChangeSemantics_BEHAVIORAL, protocol.ChangeSemantics_UNCHANGED, `ALTER TOPIC can drop a consumer's position .*`},
		{"a swap of two changefeeds", []changefeeds.Change{{Name: "a", After: named("a", declaredFeed(updates))}, {Name: "z", Before: named("z", readFeed())}},
			[]changefeeds.Action{changefeeds.Add, changefeeds.Drop}, protocol.ChangeSemantics_DESTRUCTIVE, protocol.ChangeSemantics_UNCHANGED, `DROP CHANGEFEED removes .*`},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := qt.New(t)
			operation, err := schema.CaptureChangefeedOperation("op", tableOwner(), test.changes)
			c.Assert(err, qt.IsNil)
			c.Assert(operation.Value.Kind, qt.Equals, schema.ChangefeedOperation)
			c.Assert(operation.OperationSemantics.Impact, qt.Equals, test.impact)
			c.Assert(operation.OperationSemantics.Access, qt.Equals, test.access)
			c.Assert(operation.OperationSemantics.Reason, qt.Matches, test.reason)
			again, err := schema.NormalizeChangefeedOperation(&protocol.ValueInput{Id: "op", Representation: protocol.ValueInput_OPERATION, Value: operation.Value})
			c.Assert(err, qt.IsNil)
			c.Assert(proto.Equal(again, operation), qt.IsTrue)
			decoded, err := schema.DecodeChangefeedOperation(operation.Value)
			c.Assert(err, qt.IsNil)
			var actions []changefeeds.Action
			for _, change := range decoded {
				actions = append(actions, change.Action())
			}
			c.Assert(actions, qt.DeepEquals, test.actions)
		})
	}
}

func TestChangefeedOperation_FailurePath(t *testing.T) {
	updates := Ydb_Table.ChangefeedMode_MODE_UPDATES
	replicated := readFeed()
	replicated.Description.Attributes = map[string]string{"__async_replication": `{"path":"/local/repl/records","id":"1","supports_topic_autopartitioning":false}`}
	for _, test := range []struct {
		name    string
		owner   *protocol.Reference
		changes []changefeeds.Change
		wantErr string
	}{
		{"no change", tableOwner(), nil, `changefeed operation requires at least one changefeed change`},
		{"a change without an operand", tableOwner(), []changefeeds.Change{{Name: "f"}}, `changefeed operation change of "f" has no operand`},
		{"a change without a name", tableOwner(), []changefeeds.Change{{After: declaredFeed(updates)}}, `changefeed operation requires a changefeed name`},
		{"an operand of another changefeed", tableOwner(), []changefeeds.Change{{Name: "f", After: named("g", declaredFeed(updates))}},
			`changefeed operation operands name another changefeed than "f"`},
		{"two changes of one changefeed", tableOwner(), []changefeeds.Change{{Name: "f", Before: readFeed()}, {Name: "f", After: declaredFeed(updates)}},
			`changefeed operation changes must name each changefeed once, in name order`},
		{"changes out of order", tableOwner(), []changefeeds.Change{{Name: "z", After: named("z", declaredFeed(updates))}, {Name: "a", After: named("a", declaredFeed(updates))}},
			`changefeed operation changes must name each changefeed once, in name order`},
		{"a topic change that changes nothing", tableOwner(), []changefeeds.Change{{Name: "f", Before: readFeed(), After: declaredFeed(updates)}},
			`changefeed "f" of table "app/records": the changefeed operands contain no change`},
		{"a replication-managed stream", tableOwner(), []changefeeds.Change{{Name: "f", Before: replicated}},
			`changefeed "f" of table "app/records": the retained replication-managed changefeed cannot be created, removed, or changed independently of its controller`},
		{"an owner that is no table", &protocol.Reference{Engine: "ydb", Kind: "column", Schema: "app", Path: []string{"records"}, Name: "id"},
			[]changefeeds.Change{{Name: "f", After: declaredFeed(updates)}}, `changefeed operation requires a table owner`},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := qt.New(t)
			operation, err := schema.CaptureChangefeedOperation("op", test.owner, test.changes)
			c.Assert(err, qt.ErrorMatches, test.wantErr)
			c.Assert(operation, qt.IsNil)
		})
	}
}

// TestChangefeedOperation_RefusesAForgedPayload holds a payload written by hand
// to the operation's exact keys.
func TestChangefeedOperation_RefusesAForgedPayload(t *testing.T) {
	operation, err := schema.CaptureChangefeedOperation("op", tableOwner(), []changefeeds.Change{{Name: "f", Before: readFeed()}})
	qt.Assert(t, err, qt.IsNil)
	var fields map[string]json.RawMessage
	qt.Assert(t, json.Unmarshal(operation.Value.Payload, &fields), qt.IsNil)
	fields["impact"] = json.RawMessage(`"ADDITIVE"`)
	withImpact, err := json.Marshal(fields)
	qt.Assert(t, err, qt.IsNil)
	for _, test := range []struct {
		name    string
		payload string
		wantErr string
	}{
		{"an unknown key", string(withImpact), `changefeed operation has an unknown key "impact"`},
		{"an unknown key of a change", `{"changefeeds":[{"name":"f","recreate":true}]}`, `changefeed operation has an unknown key "recreate"`},
		{"a null operand", `{"changefeeds":[{"name":"f","before":null}]}`, `changefeed operation key "before" cannot be null`},
		{"not an object", `[]`, `invalid changefeed operation payload`},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := qt.New(t)
			value := proto.CloneOf(operation.Value)
			value.Payload = []byte(test.payload)
			_, err := schema.NormalizeChangefeedOperation(&protocol.ValueInput{Id: "forged", Representation: protocol.ValueInput_OPERATION, Value: value})
			c.Assert(err, qt.ErrorMatches, test.wantErr)
		})
	}
}

// changefeedSet is a changefeed set facet of app.records.
func changefeedSet(c *qt.C, representation protocol.ValueInput_Representation, payload string) *protocol.Envelope {
	c.Helper()
	model := schema.NativeDesiredModel(schema.ChangefeedProperties)
	if representation == protocol.ValueInput_OBSERVED {
		for _, candidate := range schema.ObservedModels() {
			if candidate.Kind == schema.ChangefeedProperties {
				model = candidate
			}
		}
	}
	return &protocol.Envelope{ProviderId: schema.ProviderID, Kind: schema.ChangefeedProperties, Owner: tableOwner(), SchemaVersion: 1,
		Encoding: model.Encoding, DefinitionSha256: model.DefinitionSha256, Payload: []byte(payload)}
}

// TestTablePropertiesChange_ChangefeedSets reads the impact of a changefeed set
// change from the steps that carry it out.
func TestTablePropertiesChange_ChangefeedSets(t *testing.T) {
	const (
		none     = `{"changefeeds":[]}`
		held     = `{"changefeeds":[{"description":{"name":"f","mode":"MODE_UPDATES","format":"FORMAT_JSON","state":"STATE_ENABLED"},"topic":{"partitioningSettings":{}}}]}`
		declared = `{"changefeeds":[{"name":"f","mode":"MODE_UPDATES","format":"FORMAT_JSON","topic":{"partitioningSettings":{}}}]}`
		keys     = `{"changefeeds":[{"name":"f","mode":"MODE_KEYS_ONLY","format":"FORMAT_JSON","topic":{"partitioningSettings":{}}}]}`
	)
	for _, test := range []struct {
		name          string
		before, after string
		impact        protocol.ChangeSemantics_Impact
		removesState  bool
	}{
		{"an addition", none, declared, protocol.ChangeSemantics_ADDITIVE, false},
		{"a removal", held, none, protocol.ChangeSemantics_DESTRUCTIVE, true},
		{"a recreation", held, keys, protocol.ChangeSemantics_DESTRUCTIVE, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := qt.New(t)
			change, err := schema.CaptureTablePropertiesChange("change", tableOwner(), changefeedSet(c, protocol.ValueInput_OBSERVED, test.before),
				changefeedSet(c, protocol.ValueInput_DESIRED, test.after))
			c.Assert(err, qt.IsNil)
			c.Assert(string(change.Value.Payload), qt.Contains, `"facet":"`+schema.ChangefeedProperties+`"`)
			c.Assert(change.ChangeSemantics.Impact, qt.Equals, test.impact)
			c.Assert(change.ChangeSemantics.GetRemovesState(), qt.Equals, test.removesState)
			c.Assert(change.ChangeSemantics.Access, qt.Equals, protocol.ChangeSemantics_UNCHANGED)
			again, err := schema.NormalizeTablePropertiesChange(&protocol.ValueInput{Id: "change", Representation: protocol.ValueInput_CHANGE, Value: change.Value})
			c.Assert(err, qt.IsNil)
			c.Assert(proto.Equal(again, change), qt.IsTrue)
		})
	}
}

// TestTablePropertiesChange_ChangefeedSetsOnAColumn refuses the changefeed set
// selector on a column owner.
func TestTablePropertiesChange_ChangefeedSetsOnAColumn(t *testing.T) {
	c := qt.New(t)
	change, err := schema.CaptureTablePropertiesChange("change", tableOwner(), changefeedSet(c, protocol.ValueInput_OBSERVED, `{"changefeeds":[]}`),
		changefeedSet(c, protocol.ValueInput_DESIRED, `{"changefeeds":[{"name":"f","mode":"MODE_UPDATES","format":"FORMAT_JSON","topic":{"partitioningSettings":{}}}]}`))
	c.Assert(err, qt.IsNil)
	value := proto.CloneOf(change.Value)
	value.Owner = columnOwner()
	_, err = schema.NormalizeTablePropertiesChange(&protocol.ValueInput{Id: "forged", Representation: protocol.ValueInput_CHANGE, Value: value})
	c.Assert(err, qt.ErrorMatches, "table properties change names an unsupported facet")
}
