package tablecreate_test

import (
	"encoding/json"
	"testing"

	qt "github.com/frankban/quicktest"
	"github.com/stokaro/ptah-ydb-plugin/internal/backend"
	native "github.com/stokaro/ptah-ydb-plugin/internal/nativeprotocol"
	"github.com/stokaro/ptah-ydb-plugin/internal/tablecreate"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/stokaro/ptah/provider-sdk/provider"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

func typedText(value string) *Ydb.TypedValue {
	return &Ydb.TypedValue{Type: &Ydb.Type{Type: &Ydb.Type_TypeId{TypeId: Ydb.Type_UTF8}}, Value: &Ydb.Value{Value: &Ydb.Value_TextValue{TextValue: value}}}
}
func defaultChange() *tablecreate.DefaultAlteration {
	return &tablecreate.DefaultAlteration{Held: &native.ColumnMeta{Name: "value", Type: typedText("").Type, DefaultValue: &native.ColumnMeta_FromLiteral{FromLiteral: typedText("old")}}, Set: typedText("")}
}

func TestDefaultOperationPreservesEmptyLiteralAndDrop(t *testing.T) {
	for _, test := range []struct {
		name string
		set  *Ydb.TypedValue
		sql  string
	}{{"empty", typedText(""), "SET DEFAULT ''u;"}, {"drop", nil, "DROP DEFAULT;"}} {
		t.Run(test.name, func(t *testing.T) {
			c := qt.New(t)
			change := defaultChange()
			change.Set = test.set
			operation, err := tablecreate.CaptureDefault("default", owner(), change)
			c.Assert(err, qt.IsNil)
			decoded, err := tablecreate.DecodeDefault(operation.Value)
			c.Assert(err, qt.IsNil)
			c.Assert(proto.Equal(decoded.Held, change.Held), qt.IsTrue)
			c.Assert(proto.Equal(decoded.Set, change.Set), qt.IsTrue)
			text, err := tablecreate.DefaultStatement("app/records", decoded)
			c.Assert(err, qt.IsNil)
			c.Assert(text, qt.Equals, "ALTER TABLE `app/records` ALTER COLUMN `value` "+test.sql)
			c.Assert(operation.OperationSemantics.Impact, qt.Equals, protocol.ChangeSemantics_BEHAVIORAL)
			execution, err := backend.New().Execute(c.Context(), &protocol.ExecuteRequest{SessionId: "not-open", TransactionId: "forbidden", RequestId: "request", Units: []*protocol.Unit{{ChangeId: "default", Body: &protocol.Unit_Native{Native: operation.Value}}}})
			c.Assert(err, qt.IsNil)
			c.Assert(execution.Outcome, qt.Equals, protocol.Execution_NOT_STARTED)
			c.Assert(execution.DiagnosticCode, qt.Equals, "ydb.native-transaction-forbidden")
		})
	}
}

func TestDefaultOperationRefusesInvalidCaptures(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*tablecreate.DefaultAlteration)
	}{
		{"same", func(c *tablecreate.DefaultAlteration) { c.Set = typedText("old") }},
		{"missing held", func(c *tablecreate.DefaultAlteration) { c.Held = nil }},
		{"foreign type", func(c *tablecreate.DefaultAlteration) { c.Set.Type.Type = &Ydb.Type_TypeId{TypeId: Ydb.Type_INT32} }},
		{"wrong scalar", func(c *tablecreate.DefaultAlteration) { c.Set.Value.Value = &Ydb.Value_Int32Value{Int32Value: 1} }},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := qt.New(t)
			change := defaultChange()
			test.mutate(change)
			operation, err := tablecreate.CaptureDefault("default", owner(), change)
			c.Assert(err, qt.IsNotNil)
			c.Assert(operation, qt.IsNil)
		})
	}
}

func TestDefaultChangeHasAnOwnedOperationAndCompleteEffects(t *testing.T) {
	c := qt.New(t)
	held := defaultChange().Held
	held.Name, held.Type, held.NotNull = "", nil, nil
	payload, err := protojson.Marshal(held)
	c.Assert(err, qt.IsNil)
	request := alterRequest(c, alterCase{declared: []string{"value"}, held: []string{"value"}, heldTable: plainTable, heldColumns: map[string]string{"value": string(payload)}, capabilities: map[string]bool{"alter_column_default": true}})
	request.TargetFeatures = append(request.TargetFeatures, provider.ColumnsFeature)
	table := request.Tables[0]
	table.Current.Columns[0].DataType = "Uint64"
	table.Current.Columns[0].IsNullable = "NO"
	table.Current.Columns[1].DataType = "Utf8"
	table.Current.Columns[1].IsNullable = "YES"
	table.Current.Columns[1].DefaultExpression = new("'old'u")
	table.Desired.Columns[1].DefaultSet = true
	plan, err := tablecreate.Plan(c.Context(), request)
	c.Assert(err, qt.IsNil)
	c.Assert(plan.Diagnostics, qt.HasLen, 0)
	c.Assert(plan.Contributions, qt.HasLen, 1)
	c.Assert(plan.Contributions[0].Steps, qt.HasLen, 1)
	step := plan.Contributions[0].Steps[0]
	c.Assert(step.Operation.Value.Value.Kind, qt.Equals, tablecreate.DefaultOperationKind)
	c.Assert(step.Metadata.Transaction, qt.Equals, protocol.PlanningMetadata_FORBIDDEN)
	c.Assert(step.Metadata.Effects, qt.HasLen, 2)
	c.Assert(step.Metadata.Effects[0].Action, qt.Equals, protocol.PlanningEffect_READ)
	c.Assert(step.Metadata.Effects[1].Action, qt.Equals, protocol.PlanningEffect_ALTER)
	c.Assert(step.Metadata.Effects[1].Subject.Name.Source, qt.Equals, "value")
	for _, parent := range plan.Parents {
		c.Assert(parent.Receipt.Steps, qt.HasLen, 1)
		c.Assert(proto.Equal(parent.Receipt.Steps[0], step.Id), qt.IsTrue)
	}
}

func TestDefaultOperationRefusesPayloadAmbiguity(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func([]byte) []byte
	}{
		{"explicit null", func(data []byte) []byte { return []byte(`{"held":null}`) }},
		{"null set", func(data []byte) []byte {
			var payload map[string]json.RawMessage
			if err := json.Unmarshal(data, &payload); err != nil {
				panic(err)
			}
			payload["set"] = json.RawMessage(`null`)
			result, err := json.Marshal(payload)
			if err != nil {
				panic(err)
			}
			return result
		}},
		{"unknown field", func(data []byte) []byte { return append([]byte(`{"extra":true,`), data[1:]...) }},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := qt.New(t)
			operation, err := tablecreate.CaptureDefault("default", owner(), defaultChange())
			c.Assert(err, qt.IsNil)
			operation.Value.Payload = test.mutate(operation.Value.Payload)
			_, err = tablecreate.DecodeDefault(operation.Value)
			c.Assert(err, qt.IsNotNil)
		})
	}
}
