package tablecreate_test

import (
	"testing"

	qt "github.com/frankban/quicktest"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	"github.com/stokaro/ptah-ydb-plugin/internal/backend"
	"github.com/stokaro/ptah-ydb-plugin/internal/tablecreate"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
)

// heldIndexDefaults are the settings YDB gives a new global index.
const heldIndexDefaults = `"partitioningSettings":{"partitioningBySize":"ENABLED","partitionSizeMb":"2048","partitioningByLoad":"DISABLED","minPartitionsCount":"1"}`

// indexWithSettings states the index by_label of the given kind with the
// given settings fields.
func indexWithSettings(kind, fields string) string {
	return `"name":"by_label","indexColumns":["label"],"` + kind + `":{"settings":{` + fields + `}}`
}

func indexSettingsCase(declared, held string) alterCase {
	return indexCase(declaredIndexes(indexWithSettings("globalIndex", declared)), heldIndexes(indexWithSettings("globalIndex", held)), "label")
}

func TestIndexSettingsPlan_HappyPath(t *testing.T) {
	for _, test := range []struct {
		name string
		in   alterCase
		set  string
		text string
	}{
		{"changes the minimum partition count",
			indexSettingsCase(`"partitioningSettings":{"minPartitionsCount":"3"}`, heldIndexDefaults),
			`{"partitioningSettings":{"minPartitionsCount":"3"}}`,
			"ALTER TABLE `app/records` ALTER INDEX `by_label` SET (AUTO_PARTITIONING_BY_SIZE = ENABLED, AUTO_PARTITIONING_PARTITION_SIZE_MB = 2048, " +
				"AUTO_PARTITIONING_BY_LOAD = DISABLED, AUTO_PARTITIONING_MIN_PARTITIONS_COUNT = 3);"},
		{"switches partitioning by load on",
			indexSettingsCase(`"partitioningSettings":{"partitioningByLoad":"ENABLED","maxPartitionsCount":"9"}`, heldIndexDefaults),
			`{"partitioningSettings":{"partitioningBySize":"ENABLED","partitionSizeMb":"2048","partitioningByLoad":"ENABLED","minPartitionsCount":"1","maxPartitionsCount":"9"}}`,
			"ALTER TABLE `app/records` ALTER INDEX `by_label` SET (AUTO_PARTITIONING_BY_SIZE = ENABLED, AUTO_PARTITIONING_PARTITION_SIZE_MB = 2048, " +
				"AUTO_PARTITIONING_BY_LOAD = ENABLED, AUTO_PARTITIONING_MIN_PARTITIONS_COUNT = 1, AUTO_PARTITIONING_MAX_PARTITIONS_COUNT = 9);"},
		{"switches partitioning by load on and keeps the minimum the index holds",
			indexSettingsCase(`"partitioningSettings":{"partitioningByLoad":"ENABLED"}`,
				`"partitioningSettings":{"partitioningBySize":"ENABLED","partitionSizeMb":"2048","partitioningByLoad":"DISABLED","minPartitionsCount":"3"}`),
			`{"partitioningSettings":{"partitioningBySize":"ENABLED","partitionSizeMb":"2048","partitioningByLoad":"ENABLED","minPartitionsCount":"3"}}`,
			"ALTER TABLE `app/records` ALTER INDEX `by_label` SET (AUTO_PARTITIONING_BY_SIZE = ENABLED, AUTO_PARTITIONING_PARTITION_SIZE_MB = 2048, " +
				"AUTO_PARTITIONING_BY_LOAD = ENABLED, AUTO_PARTITIONING_MIN_PARTITIONS_COUNT = 3);"},
		{"switches partitioning by size on and keeps the minimum the index holds",
			indexSettingsCase(`"partitioningSettings":{"partitioningBySize":"ENABLED"}`,
				`"partitioningSettings":{"partitioningBySize":"DISABLED","partitioningByLoad":"ENABLED","minPartitionsCount":"3"}`),
			`{"partitioningSettings":{"partitioningBySize":"ENABLED","partitioningByLoad":"ENABLED","minPartitionsCount":"3"}}`,
			"ALTER TABLE `app/records` ALTER INDEX `by_label` SET (AUTO_PARTITIONING_BY_SIZE = ENABLED, AUTO_PARTITIONING_PARTITION_SIZE_MB = 2048, " +
				"AUTO_PARTITIONING_BY_LOAD = ENABLED, AUTO_PARTITIONING_MIN_PARTITIONS_COUNT = 3);"},
		{"turns partitioning by size on for a size declared without the switch, as YDB does",
			indexSettingsCase(`"partitioningSettings":{"partitionSizeMb":"512","minPartitionsCount":"6"}`,
				`"partitioningSettings":{"partitioningBySize":"DISABLED","partitioningByLoad":"DISABLED","minPartitionsCount":"6"}`),
			`{"partitioningSettings":{"partitioningBySize":"ENABLED","partitionSizeMb":"512","partitioningByLoad":"DISABLED","minPartitionsCount":"6"}}`,
			"ALTER TABLE `app/records` ALTER INDEX `by_label` SET (AUTO_PARTITIONING_BY_SIZE = ENABLED, AUTO_PARTITIONING_PARTITION_SIZE_MB = 512, " +
				"AUTO_PARTITIONING_BY_LOAD = DISABLED, AUTO_PARTITIONING_MIN_PARTITIONS_COUNT = 6);"},
		{"sets read replicas",
			indexSettingsCase(`"readReplicasSettings":{"perAzReadReplicasCount":"1"}`, heldIndexDefaults),
			`{"readReplicasSettings":{"perAzReadReplicasCount":"1"}}`,
			"ALTER TABLE `app/records` ALTER INDEX `by_label` SET (READ_REPLICAS_SETTINGS = \"PER_AZ:1\");"},
		{"removes read replicas declared as ANY_AZ:0",
			indexSettingsCase(`"readReplicasSettings":{"anyAzReadReplicasCount":"0"}`, heldIndexDefaults+`,"readReplicasSettings":{"perAzReadReplicasCount":"1"}`),
			`{"readReplicasSettings":{"perAzReadReplicasCount":"0"}}`,
			"ALTER TABLE `app/records` ALTER INDEX `by_label` SET (READ_REPLICAS_SETTINGS = \"PER_AZ:0\");"},
		{"changes the partitioning and the replicas of an async index",
			indexCase(declaredIndexes(indexWithSettings("globalAsyncIndex", `"partitioningSettings":{"minPartitionsCount":"2"},"readReplicasSettings":{"anyAzReadReplicasCount":"1"}`)),
				heldIndexes(indexWithSettings("globalAsyncIndex", heldIndexDefaults)), "label"),
			`{"partitioningSettings":{"minPartitionsCount":"2"},"readReplicasSettings":{"anyAzReadReplicasCount":"1"}}`,
			"ALTER TABLE `app/records` ALTER INDEX `by_label` SET (AUTO_PARTITIONING_BY_SIZE = ENABLED, AUTO_PARTITIONING_PARTITION_SIZE_MB = 2048, " +
				"AUTO_PARTITIONING_BY_LOAD = DISABLED, AUTO_PARTITIONING_MIN_PARTITIONS_COUNT = 2, READ_REPLICAS_SETTINGS = \"ANY_AZ:1\");"},
		{"changes a unique index",
			indexCase(declaredIndexes(indexWithSettings("globalUniqueIndex", `"partitioningSettings":{"minPartitionsCount":"2"}`)),
				heldIndexes(indexWithSettings("globalUniqueIndex", heldIndexDefaults)), "label"),
			`{"partitioningSettings":{"minPartitionsCount":"2"}}`,
			"ALTER TABLE `app/records` ALTER INDEX `by_label` SET (AUTO_PARTITIONING_BY_SIZE = ENABLED, AUTO_PARTITIONING_PARTITION_SIZE_MB = 2048, " +
				"AUTO_PARTITIONING_BY_LOAD = DISABLED, AUTO_PARTITIONING_MIN_PARTITIONS_COUNT = 2);"},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			request := alterRequest(c, test.in)
			original := proto.CloneOf(request)
			plan, err := backend.New().PlanFeatures(c.Context(), request)
			c.Assert(err, qt.IsNil)
			c.Assert(plan.Diagnostics, qt.HasLen, 0)
			c.Assert(plan.Contributions, qt.HasLen, 1)
			c.Assert(plan.Contributions[0].Steps, qt.HasLen, 1)
			step := plan.Contributions[0].Steps[0]
			c.Assert(step.Id.Name, qt.Equals, "row-table-index-settings/000000")
			c.Assert(step.Operation.Value.Value.Kind, qt.Equals, tablecreate.IndexSettingsOperationKind)
			c.Assert(step.Metadata.Transaction, qt.Equals, protocol.PlanningMetadata_FORBIDDEN)
			c.Assert(step.Metadata.Impact, qt.Equals, protocol.ChangeSemantics_BEHAVIORAL)
			c.Assert(step.Metadata.Effects, qt.HasLen, 2)
			c.Assert(step.Metadata.Effects[0].Action, qt.Equals, protocol.PlanningEffect_READ)
			c.Assert(step.Metadata.Effects[1].Action, qt.Equals, protocol.PlanningEffect_ALTER)
			c.Assert(step.Metadata.Effects[1].Subject.Kind, qt.Equals, "index")
			c.Assert(step.Metadata.Effects[1].Subject.Name.Normalized, qt.Equals, "by_label")
			c.Assert(plan.Parents[0].Receipt.Steps, qt.HasLen, 1)
			settings, err := tablecreate.DecodeIndexSettings(step.Operation.Value.Value)
			c.Assert(err, qt.IsNil)
			c.Assert(settings.Index, qt.Equals, "by_label")
			got, err := protojson.Marshal(settings.Set)
			c.Assert(err, qt.IsNil)
			c.Assert(string(got), qt.JSONEquals, jsonValue(c, test.set))
			c.Assert(tablecreate.IndexSettingsStatement("app/records", settings), qt.Equals, test.text)
			c.Assert(proto.Equal(request, original), qt.IsTrue)
		})
	}
}

func TestIndexSettingsPlan_NoChange(t *testing.T) {
	for _, test := range []struct {
		name string
		in   alterCase
	}{
		{"the index holds the declared settings", indexSettingsCase(`"partitioningSettings":{"minPartitionsCount":"1","partitioningBySize":"ENABLED"}`, heldIndexDefaults)},
		{"settings the declaration leaves out", indexSettingsCase(``, heldIndexDefaults+`,"readReplicasSettings":{"perAzReadReplicasCount":"1"}`)},
		{"no replicas declared on an index read as PER_AZ:0",
			indexSettingsCase(`"readReplicasSettings":{"anyAzReadReplicasCount":"0"}`, heldIndexDefaults+`,"readReplicasSettings":{"perAzReadReplicasCount":"0"}`)},
		{"the replicas the index holds",
			indexSettingsCase(`"readReplicasSettings":{"perAzReadReplicasCount":"1"}`, heldIndexDefaults+`,"readReplicasSettings":{"perAzReadReplicasCount":"1"}`)},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			plan, err := backend.New().PlanFeatures(c.Context(), alterRequest(c, test.in))
			c.Assert(err, qt.IsNil)
			c.Assert(plan.Diagnostics, qt.HasLen, 0)
			c.Assert(plan.Contributions, qt.HasLen, 0)
			c.Assert(plan.Parents[0].Receipt.Steps, qt.HasLen, 0)
		})
	}
}

func TestIndexSettingsPlan_FailurePath(t *testing.T) {
	for _, test := range []struct {
		name    string
		in      alterCase
		message string
	}{
		{"a uniform layout", indexSettingsCase(`"uniformPartitions":"4"`, heldIndexDefaults),
			`index "by_label" declares a starting layout \(uniform_partitions or partition_at_keys\), which YDB does not take for an index the table holds`},
		{"partition columns", indexSettingsCase(`"partitioningSettings":{"partitionBy":["label"]}`, heldIndexDefaults),
			`index "by_label" declares partition columns, and YDB partitions an index on its own key`},
		{"a minimum above the maximum",
			indexSettingsCase(`"partitioningSettings":{"minPartitionsCount":"10"}`, `"partitioningSettings":{"partitioningBySize":"ENABLED","partitionSizeMb":"2048","minPartitionsCount":"1","maxPartitionsCount":"9"}`),
			`index "by_label": AUTO_PARTITIONING_MIN_PARTITIONS_COUNT 10 exceeds AUTO_PARTITIONING_MAX_PARTITIONS_COUNT 9`},
		{"a size beside the switch the declaration turns off",
			indexSettingsCase(`"partitioningSettings":{"partitioningBySize":"DISABLED","partitionSizeMb":"512"}`, heldIndexDefaults),
			`index "by_label": AUTO_PARTITIONING_PARTITION_SIZE_MB 512 needs AUTO_PARTITIONING_BY_SIZE enabled.*`},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			plan, err := backend.New().PlanFeatures(c.Context(), alterRequest(c, test.in))
			c.Assert(err, qt.IsNil)
			c.Assert(plan.Diagnostics, qt.HasLen, 1)
			c.Assert(plan.Diagnostics[0].Message, qt.Matches, test.message)
			c.Assert(plan.Contributions, qt.HasLen, 0)
		})
	}
}

func TestIndexSettingsPlan_RoundTrip(t *testing.T) {
	c := testcheck.New(t)
	service := backend.New()
	request := alterRequest(c, indexSettingsCase(`"partitioningSettings":{"minPartitionsCount":"3"}`, heldIndexDefaults))
	plan, err := service.PlanFeatures(c.Context(), request)
	c.Assert(err, qt.IsNil)
	value := plan.Contributions[0].Steps[0].Operation.Value
	normalized, err := service.NormalizeValues(c.Context(), &protocol.NormalizeValuesRequest{Engine: "ydb", Values: []*protocol.ValueInput{
		{Id: value.Id, Representation: protocol.ValueInput_OPERATION, Value: value.Value}}})
	c.Assert(err, qt.IsNil)
	c.Assert(proto.Equal(normalized.Values[0], value), qt.IsTrue)
	rendered, err := service.Render(c.Context(), &protocol.RenderRequest{Engine: "ydb", Capabilities: request.Capabilities,
		Changes: []*protocol.Change{{Id: value.Id, Object: proto.CloneOf(value.Value.Owner),
			Operation: &protocol.Change_Native{Native: &protocol.NativeOperation{Role: protocol.NativeOperation_STATEMENT, Value: proto.CloneOf(value)}}}}})
	c.Assert(err, qt.IsNil)
	c.Assert(rendered.Units, qt.HasLen, 1)
}

func TestIndexSettingsOperation_FailurePath(t *testing.T) {
	const held = `"held_index":{"name":"by_label","indexColumns":["label"],"globalIndex":{"settings":{` + heldIndexDefaults + `}},"status":"STATUS_READY"}`
	for _, test := range []struct {
		name    string
		payload string
		message string
	}{
		{"no setting", `{"index":"by_label","set":{},` + held + `}`, "YDB index settings change states no setting"},
		{"another index than the one held", `{"index":"by_other","set":{"partitioningSettings":{"minPartitionsCount":"3"}},` + held + `}`,
			"YDB index settings name another index than the one held"},
		{"a vector index", `{"index":"by_label","set":{"partitioningSettings":{"minPartitionsCount":"3"}},"held_index":{"name":"by_label","indexColumns":["label"],` +
			vectorKind + `,"status":"STATUS_READY"}}`, "YDB changes the settings of a global sync, async or unique index only"},
		{"a starting layout", `{"index":"by_label","set":{"uniformPartitions":"4"},` + held + `}`, "YDB table creation does not support ptah.ydb.native.GlobalIndexSettings.uniform_partitions"},
		{"partition columns", `{"index":"by_label","set":{"partitioningSettings":{"partitionBy":["label"],"minPartitionsCount":"3"}},` + held + `}`, "YDB table creation does not support Ydb.Table.PartitioningSettings.partition_by"},
		{"a setting the index holds", `{"index":"by_label","set":{"partitioningSettings":{"minPartitionsCount":"1"}},` + held + `}`,
			"YDB index partitioning change changes nothing"},
		{"a minimum above the maximum it sets", `{"index":"by_label","set":{"partitioningSettings":{"minPartitionsCount":"9","maxPartitionsCount":"8"}},` + held + `}`,
			"YDB index partitioning change is invalid: AUTO_PARTITIONING_MIN_PARTITIONS_COUNT 9 exceeds AUTO_PARTITIONING_MAX_PARTITIONS_COUNT 8"},
		{"replicas with no mode", `{"index":"by_label","set":{"readReplicasSettings":{}},` + held + `}`, "YDB index read replica change names no mode"},
		{"replicas the index holds", `{"index":"by_label","set":{"readReplicasSettings":{"perAzReadReplicasCount":"0"}},` + held + `}`,
			"YDB index read replica change changes nothing"},
		{"no held index", `{"index":"by_label","set":{"partitioningSettings":{"minPartitionsCount":"3"}}}`, "invalid YDB index settings JSON"},
		{"null settings", `{"index":"by_label","set":null,` + held + `}`, "YDB index settings cannot be null"},
		{"an unknown payload field", `{"index":"by_label","set":{"partitioningSettings":{"minPartitionsCount":"3"}},"extra":1,` + held + `}`, "invalid YDB index settings JSON"},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			definition := tablecreate.IndexSettingsModel()
			normalized, err := tablecreate.NormalizeIndexSettings(&protocol.ValueInput{Id: "operation", Representation: protocol.ValueInput_OPERATION, Value: &protocol.Envelope{
				ProviderId: "github.com/stokaro/ptah-ydb-plugin", Kind: tablecreate.IndexSettingsOperationKind, Owner: owner(), SchemaVersion: 1, Encoding: definition.Encoding,
				DefinitionSha256: definition.DefinitionSha256, Payload: []byte(test.payload)}})
			c.Assert(err, qt.ErrorMatches, test.message)
			c.Assert(normalized, qt.IsNil)
		})
	}
}

// A planned change of an index's settings renders in the TEXT form as the
// ALTER INDEX its execution runs.
func TestIndexSettingsRendersText(t *testing.T) {
	c := testcheck.New(t)
	rendered, err := backend.New().Render(c.Context(), textRendering(c, alterRequest(c, indexSettingsCase(`"readReplicasSettings":{"perAzReadReplicasCount":"1"}`, heldIndexDefaults))))
	c.Assert(err, qt.IsNil)
	c.Assert(rendered.Refusal, qt.IsNil)
	c.Assert(rendered.Units, qt.HasLen, 1)
	c.Assert(rendered.Units[0].GetSql(), qt.Equals, "ALTER TABLE `app/records` ALTER INDEX `by_label` SET (READ_REPLICAS_SETTINGS = \"PER_AZ:1\");")
}
