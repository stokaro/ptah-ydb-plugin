package tablecreate_test

import (
	"testing"

	qt "github.com/frankban/quicktest"
	"github.com/stokaro/ptah-ydb-plugin/internal/backend"
	"github.com/stokaro/ptah-ydb-plugin/internal/tablecreate"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

// partitionedTable is a table whose partitioning is the given JSON object.
func partitionedTable(settings string) string {
	return `{"primaryKey":["id"],"columnFamilies":[{"name":"default"}],"partitioningSettings":` + settings + `}`
}

const defaultPartitioning = `{"partitionBy":["id"],"partitioningBySize":"ENABLED","partitionSizeMb":"2048","partitioningByLoad":"DISABLED","minPartitionsCount":"1","maxPartitionsCount":"50"}`

// disabledBySize is a table that does not partition by size, so it reports no size.
const disabledBySize = `{"partitionBy":["id"],"partitioningBySize":"DISABLED","partitioningByLoad":"ENABLED","minPartitionsCount":"2","maxPartitionsCount":"8"}`

func partitioningCase(desired, held string) alterCase {
	return alterCase{desiredTable: desired, heldTable: held}
}

func TestPartitioningPlan_HappyPath(t *testing.T) {
	for _, test := range []struct {
		name string
		in   alterCase
		want string
	}{
		{"switches partitioning by load on",
			partitioningCase(partitionedTable(`{"partitioningByLoad":"ENABLED"}`), partitionedTable(defaultPartitioning)),
			`{"alterPartitioningSettings":{"partitioningByLoad":"ENABLED"}}`},
		{"switches partitioning by size off",
			partitioningCase(partitionedTable(`{"partitioningBySize":"DISABLED"}`), partitionedTable(defaultPartitioning)),
			`{"alterPartitioningSettings":{"partitioningBySize":"DISABLED"}}`},
		{"changes the partition size",
			partitioningCase(partitionedTable(`{"partitionSizeMb":"512"}`), partitionedTable(defaultPartitioning)),
			`{"alterPartitioningSettings":{"partitionSizeMb":"512"}}`},
		{"changes the minimum count",
			partitioningCase(partitionedTable(`{"minPartitionsCount":"4"}`), partitionedTable(defaultPartitioning)),
			`{"alterPartitioningSettings":{"minPartitionsCount":"4"}}`},
		{"changes the maximum count",
			partitioningCase(partitionedTable(`{"maxPartitionsCount":"8"}`), partitionedTable(defaultPartitioning)),
			`{"alterPartitioningSettings":{"maxPartitionsCount":"8"}}`},
		{"names only the settings that differ",
			partitioningCase(partitionedTable(`{"partitioningBySize":"ENABLED","partitionSizeMb":"512","maxPartitionsCount":"8"}`), partitionedTable(defaultPartitioning)),
			`{"alterPartitioningSettings":{"partitionSizeMb":"512","maxPartitionsCount":"8"}}`},
		{"changes both bounds together",
			partitioningCase(partitionedTable(`{"minPartitionsCount":"60","maxPartitionsCount":"90"}`), partitionedTable(defaultPartitioning)),
			`{"alterPartitioningSettings":{"minPartitionsCount":"60","maxPartitionsCount":"90"}}`},
		{"states the partition columns the table holds",
			partitioningCase(partitionedTable(`{"partitionBy":["id"],"maxPartitionsCount":"8"}`), partitionedTable(defaultPartitioning)),
			`{"alterPartitioningSettings":{"maxPartitionsCount":"8"}}`},
		{"names every stated setting when switching partitioning by size on",
			partitioningCase(partitionedTable(`{"partitioningBySize":"ENABLED","minPartitionsCount":"2","maxPartitionsCount":"50"}`), partitionedTable(disabledBySize)),
			`{"alterPartitioningSettings":{"partitioningBySize":"ENABLED","minPartitionsCount":"2","maxPartitionsCount":"50"}}`},
		{"states a size together with switching partitioning by size on",
			partitioningCase(partitionedTable(`{"partitioningBySize":"ENABLED","partitionSizeMb":"512"}`), partitionedTable(disabledBySize)),
			`{"alterPartitioningSettings":{"partitioningBySize":"ENABLED","partitionSizeMb":"512"}}`},
		{"plans against a table that reports no partitioning",
			partitioningCase(partitionedTable(`{"maxPartitionsCount":"8"}`), plainTable),
			`{"alterPartitioningSettings":{"maxPartitionsCount":"8"}}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := qt.New(t)
			request := alterRequest(c, test.in)
			original := proto.CloneOf(request)
			plan, err := backend.New().PlanFeatures(c.Context(), request)
			c.Assert(err, qt.IsNil)
			c.Assert(plan.Diagnostics, qt.HasLen, 0)
			c.Assert(plan.Contributions, qt.HasLen, 1)
			c.Assert(plan.Contributions[0].Steps, qt.HasLen, 1)
			c.Assert(plan.Rewrites, qt.HasLen, 0)
			step := plan.Contributions[0].Steps[0]
			c.Assert(step.Id.Name, qt.Equals, "row-table-partitioning/000000")
			c.Assert(step.Operation.Value.Value.Kind, qt.Equals, tablecreate.AlterOperationKind)
			c.Assert(step.Metadata.Transaction, qt.Equals, protocol.PlanningMetadata_FORBIDDEN)
			c.Assert(step.Metadata.Impact, qt.Equals, protocol.ChangeSemantics_BEHAVIORAL)
			c.Assert(step.Operation.Value.OperationSemantics.Impact, qt.Equals, protocol.ChangeSemantics_BEHAVIORAL)
			c.Assert(step.Metadata.Effects, qt.HasLen, 2)
			c.Assert(step.Metadata.Effects[0].Action, qt.Equals, protocol.PlanningEffect_READ)
			c.Assert(step.Metadata.Effects[1].Action, qt.Equals, protocol.PlanningEffect_ALTER)
			c.Assert(step.Metadata.Effects[1].Subject.Kind, qt.Equals, "ydb.partitioning")
			c.Assert(plan.Parents, qt.HasLen, 2)
			c.Assert(plan.Parents[0].Receipt.Steps, qt.HasLen, 1)
			c.Assert(plan.Parents[0].Receipt.Steps[0].Name, qt.Equals, "row-table-partitioning/000000")
			alteration, err := tablecreate.DecodeAlteration(step.Operation.Value.Value)
			c.Assert(err, qt.IsNil)
			got, err := protojson.Marshal(alteration.Request)
			c.Assert(err, qt.IsNil)
			c.Assert(string(got), qt.JSONEquals, jsonValue(c, test.want))
			c.Assert(proto.Equal(request, original), qt.IsTrue)
		})
	}
}

func TestPartitioningPlan_CarriesTheHeldSettings(t *testing.T) {
	c := qt.New(t)
	plan, err := backend.New().PlanFeatures(c.Context(), alterRequest(c,
		partitioningCase(partitionedTable(`{"maxPartitionsCount":"8"}`), partitionedTable(defaultPartitioning))))
	c.Assert(err, qt.IsNil)
	alteration, err := tablecreate.DecodeAlteration(plan.Contributions[0].Steps[0].Operation.Value.Value)
	c.Assert(err, qt.IsNil)
	held, err := protojson.Marshal(alteration.HeldPartitioning)
	c.Assert(err, qt.IsNil)
	c.Assert(string(held), qt.JSONEquals, jsonValue(c, defaultPartitioning))
}

func TestPartitioningPlan_NoChange(t *testing.T) {
	for _, test := range []struct {
		name string
		in   alterCase
	}{
		{"neither side states partitioning", partitioningCase(plainTable, plainTable)},
		{"the table holds the declared settings",
			partitioningCase(partitionedTable(`{"partitioningBySize":"ENABLED","maxPartitionsCount":"50"}`), partitionedTable(defaultPartitioning))},
		{"the declaration leaves out what the table reports as defaults", partitioningCase(plainTable, partitionedTable(defaultPartitioning))},
		{"a declaration without table properties leaves the partitioning alone", partitioningCase("", partitionedTable(defaultPartitioning))},
		{"the declared partition columns are the held ones", partitioningCase(partitionedTable(`{"partitionBy":["id"]}`), partitionedTable(defaultPartitioning))},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := qt.New(t)
			plan, err := backend.New().PlanFeatures(c.Context(), alterRequest(c, test.in))
			c.Assert(err, qt.IsNil)
			c.Assert(plan.Diagnostics, qt.HasLen, 0)
			c.Assert(plan.Contributions, qt.HasLen, 0)
			c.Assert(plan.Parents, qt.HasLen, 2)
			c.Assert(plan.Parents[0].Receipt.Steps, qt.HasLen, 0)
		})
	}
}

func TestPartitioningPlan_FailurePath(t *testing.T) {
	for _, test := range []struct {
		name    string
		in      alterCase
		message string
	}{
		{"minimum above the held maximum",
			partitioningCase(partitionedTable(`{"minPartitionsCount":"60"}`), partitionedTable(defaultPartitioning)),
			`AUTO_PARTITIONING_MIN_PARTITIONS_COUNT 60 exceeds AUTO_PARTITIONING_MAX_PARTITIONS_COUNT 50`},
		{"maximum below the held minimum",
			partitioningCase(partitionedTable(`{"maxPartitionsCount":"3"}`), partitionedTable(`{"partitionBy":["id"],"minPartitionsCount":"5","maxPartitionsCount":"50"}`)),
			`AUTO_PARTITIONING_MIN_PARTITIONS_COUNT 5 exceeds AUTO_PARTITIONING_MAX_PARTITIONS_COUNT 3`},
		{"a size while the table does not partition by size",
			partitioningCase(partitionedTable(`{"partitionSizeMb":"512"}`), partitionedTable(disabledBySize)),
			`AUTO_PARTITIONING_PARTITION_SIZE_MB 512 needs AUTO_PARTITIONING_BY_SIZE enabled.*`},
		{"a size beside a declared switch off",
			partitioningCase(partitionedTable(`{"partitioningBySize":"DISABLED","partitionSizeMb":"512"}`), partitionedTable(defaultPartitioning)),
			`AUTO_PARTITIONING_PARTITION_SIZE_MB 512 needs AUTO_PARTITIONING_BY_SIZE enabled.*`},
		{"other partition columns",
			partitioningCase(partitionedTable(`{"partitionBy":["id","ts"]}`), partitionedTable(defaultPartitioning)),
			`the partition columns \["id" "ts"\] differ from the \["id"\] the table holds, and YDB sets them only when the table is created`},
		{"partition columns with a setting that could change",
			partitioningCase(partitionedTable(`{"partitionBy":["ts"],"maxPartitionsCount":"8"}`), partitionedTable(defaultPartitioning)),
			`the partition columns \["ts"\] differ from the \["id"\] the table holds.*`},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := qt.New(t)
			plan, err := backend.New().PlanFeatures(c.Context(), alterRequest(c, test.in))
			c.Assert(err, qt.IsNil)
			c.Assert(plan.Diagnostics, qt.HasLen, 1)
			c.Assert(plan.Diagnostics[0].Message, qt.Matches, test.message)
			c.Assert(plan.Contributions, qt.HasLen, 0)
			c.Assert(plan.Parents, qt.HasLen, 0)
		})
	}
}

func TestPartitioningPlan_FamilyTTLAndPartitioningAreSeparateSteps(t *testing.T) {
	c := qt.New(t)
	request := alterRequest(c, alterCase{
		desiredTable: `{"primaryKey":["id"],"columnFamilies":[{"name":"archive"}],"ttlSettings":` + hourly + `,"partitioningSettings":{"maxPartitionsCount":"8"}}`,
		declared:     []string{"ts"}, types: ttlTypes, heldTable: partitionedTable(defaultPartitioning), held: []string{"ts"}})
	plan, err := backend.New().PlanFeatures(c.Context(), request)
	c.Assert(err, qt.IsNil)
	c.Assert(plan.Diagnostics, qt.HasLen, 0)
	var names []string
	for _, step := range plan.Contributions[0].Steps {
		names = append(names, step.Id.Name)
	}
	c.Assert(names, qt.DeepEquals, []string{"row-table-alter/000000", "row-table-ttl/000000", "row-table-partitioning/000000"})
	c.Assert(plan.Parents[0].Receipt.Steps, qt.HasLen, 3)
}

func TestPartitioningPlan_NumbersStepsAcrossTables(t *testing.T) {
	c := qt.New(t)
	request := alterRequest(c, partitioningCase(partitionedTable(`{"maxPartitionsCount":"8"}`), partitionedTable(defaultPartitioning)))
	accounts := accountsTable(c)
	request.Tables = append(request.Tables, accounts)
	plan, err := backend.New().PlanFeatures(c.Context(), request)
	c.Assert(err, qt.IsNil)
	c.Assert(plan.Diagnostics, qt.HasLen, 0)
	// accounts only gains a TTL; records is the one that changes partitioning.
	var names []string
	for _, step := range plan.Contributions[0].Steps {
		names = append(names, step.Id.Name+" "+step.Operation.Value.Value.Owner.Name)
	}
	c.Assert(names, qt.DeepEquals, []string{"row-table-ttl/000000 accounts", "row-table-partitioning/000000 records"})
}

func TestPartitioningPlan_RoundTrip(t *testing.T) {
	c := qt.New(t)
	service := backend.New()
	request := alterRequest(c, partitioningCase(partitionedTable(`{"maxPartitionsCount":"8"}`), partitionedTable(defaultPartitioning)))
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
	forged := proto.CloneOf(value)
	forged.OperationSemantics.Impact = protocol.ChangeSemantics_ADDITIVE
	_, err = service.Render(c.Context(), &protocol.RenderRequest{Engine: "ydb", Capabilities: request.Capabilities,
		Changes: []*protocol.Change{{Id: value.Id, Object: proto.CloneOf(value.Value.Owner),
			Operation: &protocol.Change_Native{Native: &protocol.NativeOperation{Role: protocol.NativeOperation_STATEMENT, Value: forged}}}}})
	c.Assert(err, qt.IsNotNil)
}

func TestPartitioningOperation_FailurePath(t *testing.T) {
	const held = `"held_partitioning":{"minPartitionsCount":"1","maxPartitionsCount":"50"}`
	for _, test := range []struct {
		name    string
		payload string
	}{
		{"no held partitioning", `{"request":{"alterPartitioningSettings":{"maxPartitionsCount":"8"}}}`},
		{"no setting", `{"request":{"alterPartitioningSettings":{}},` + held + `}`},
		{"a setting the table already holds", `{"request":{"alterPartitioningSettings":{"maxPartitionsCount":"50"}},` + held + `}`},
		{"partition columns", `{"request":{"alterPartitioningSettings":{"partitionBy":["id"],"maxPartitionsCount":"8"}},` + held + `}`},
		{"a size beside disabled partitioning by size", `{"request":{"alterPartitioningSettings":{"partitionSizeMb":"512"}},"held_partitioning":{"partitioningBySize":"DISABLED","maxPartitionsCount":"50"}}`},
		{"minimum above the maximum it sets", `{"request":{"alterPartitioningSettings":{"minPartitionsCount":"9","maxPartitionsCount":"8"}},` + held + `}`},
		{"minimum above the held maximum", `{"request":{"alterPartitioningSettings":{"minPartitionsCount":"60"}},` + held + `}`},
		{"an unnamed switch value", `{"request":{"alterPartitioningSettings":{"partitioningBySize":7}},` + held + `}`},
		{"an unknown switch value", `{"request":{"alterPartitioningSettings":{"partitioningBySize":"SOMETIMES"}},` + held + `}`},
		{"null settings", `{"request":{"alterPartitioningSettings":null},` + held + `}`},
		{"held partitioning without a partitioning action", `{"request":{"dropColumns":["a"]},` + held + `}`},
		{"partitioning and TTL removal", `{"request":{"alterPartitioningSettings":{"maxPartitionsCount":"8"},"dropTtlSettings":{}},` + held + `,"held_ttl":{"dateTypeColumn":{"columnName":"x","expireAfterSeconds":60}}}`},
		{"partitioning and family change", `{"request":{"alterPartitioningSettings":{"maxPartitionsCount":"8"},"addColumnFamilies":[{"name":"f"}]},` + held + `}`},
		{"partitioning and column removal", `{"request":{"alterPartitioningSettings":{"maxPartitionsCount":"8"},"dropColumns":["a"]},` + held + `}`},
		{"storage settings smuggled in", `{"request":{"alterPartitioningSettings":{"maxPartitionsCount":"8"},"alterStorageSettings":{}},` + held + `}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := qt.New(t)
			definition := tablecreate.AlterModel()
			normalized, err := tablecreate.NormalizeAlteration(&protocol.ValueInput{Id: "operation", Representation: protocol.ValueInput_OPERATION, Value: &protocol.Envelope{
				ProviderId: "github.com/stokaro/ptah-ydb-plugin", Kind: tablecreate.AlterOperationKind, Owner: owner(), SchemaVersion: 1, Encoding: definition.Encoding,
				DefinitionSha256: definition.DefinitionSha256, Payload: []byte(test.payload)}})
			c.Assert(err, qt.IsNotNil)
			c.Assert(normalized, qt.IsNil)
		})
	}
}
