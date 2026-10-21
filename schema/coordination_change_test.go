package schema_test

import (
	"testing"

	qt "github.com/frankban/quicktest"
	"github.com/stokaro/ptah-ydb-plugin/schema"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb_Coordination"
	"google.golang.org/protobuf/proto"
)

func TestCoordinationChangeRefusesIncompleteAndForgedPayloads(t *testing.T) {
	c := qt.New(t)
	owner := &protocol.Reference{Engine: "ydb", Kind: schema.CoordinationProperties, Database: "/local", Schema: "app", Name: "node"}
	desired, err := schema.CaptureCoordinationIntent(owner, schema.CoordinationIntent{})
	c.Assert(err, qt.IsNil)
	change, err := schema.CaptureCoordinationChange("create", owner, nil, desired)
	c.Assert(err, qt.IsNil)
	for _, payload := range []string{
		`null`, `[]`, `{}`, `{"before":null}`, `{"before":null,"after":null}`, `{"Before":null,"after":{}}`,
		`{"before":null,"after":{},"removes_state":false}`, `{"before":null,"before":{},"after":{}}`,
		`{"before":{},"after":{}}`, `{"before":{"config":{"readConsistencyMode":999}},"after":{}}`,
		`{"before":{"config":{"selfCheckPeriodMillis":1}},"after":{}}`,
		`{"before":{"config":{"sessionGracePeriodMillis":50000}},"after":{}}`,
		`{"before":null,"after":{"self_check_period_millis":0}}`,
		`{"before":null,"after":{"READ_CONSISTENCY_MODE":"strict"}}`,
	} {
		value := proto.CloneOf(change.Value)
		value.Payload = []byte(payload)
		result, err := schema.NormalizeCoordinationChange(&protocol.ValueInput{Id: "bad", Representation: protocol.ValueInput_CHANGE, Value: value})
		c.Assert(err, qt.IsNotNil, qt.Commentf("payload %s", payload))
		c.Assert(result, qt.IsNil)
	}
	wrong := proto.CloneOf(change.Value)
	wrong.Owner.Schema = ""
	wrong.Owner.Name = "ptah_locks"
	refused, err := schema.NormalizeCoordinationChange(&protocol.ValueInput{Id: "runtime", Representation: protocol.ValueInput_CHANGE, Value: wrong})
	c.Assert(err, qt.IsNotNil)
	c.Assert(refused, qt.IsNil)
	other := proto.CloneOf(desired)
	other.Owner.Name = "different"
	refused, err = schema.CaptureCoordinationChange("owner", owner, nil, other)
	c.Assert(err, qt.IsNotNil)
	c.Assert(refused, qt.IsNil)
}

func TestCoordinationSettingsRefuseNativeClamping(t *testing.T) {
	c := qt.New(t)
	owner := &protocol.Reference{Engine: "ydb", Kind: schema.CoordinationProperties, Database: "/local", Schema: "app", Name: "node"}
	raw, err := schema.CaptureObject(owner, &Ydb_Coordination.DescribeNodeResult{Config: &Ydb_Coordination.Config{SelfCheckPeriodMillis: 1}})
	c.Assert(err, qt.IsNil)
	// Inspection preserves the server's raw setting, while comparison refuses
	// to claim equality for a value the runtime silently clamps.
	_, err = schema.CoordinationConfiguration(raw, protocol.ValueInput_OBSERVED)
	c.Assert(err, qt.IsNotNil)
	result, err := schema.CaptureCoordinationChange("drop", owner, raw, nil)
	c.Assert(err, qt.IsNotNil)
	c.Assert(result, qt.IsNil)
	defaults, err := schema.EffectiveCoordination(schema.CoordinationIntent{})
	c.Assert(err, qt.IsNil)
	c.Assert(defaults, qt.DeepEquals, schema.CoordinationSettings{SelfCheckMillis: 1000, SessionGraceMillis: 10000, ReadMode: "relaxed", AttachMode: "strict", CountersMode: "aggregated"})
}
