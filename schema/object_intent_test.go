package schema_test

import (
	"testing"

	qt "github.com/frankban/quicktest"
	native "github.com/stokaro/ptah-ydb-plugin/internal/nativeprotocol"
	"github.com/stokaro/ptah-ydb-plugin/schema"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/ydb-platform/ydb-go-genproto/draft/protos/Ydb_View"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb_Coordination"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb_Table"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

func TestObjectConversionsPreserveNativeOptions(t *testing.T) {
	for _, test := range []struct {
		name, kind string
		value      proto.Message
	}{
		{"view", schema.ViewProperties, &native.ViewObservation{View: &Ydb_View.DescribeViewResult{Self: objectSelf(), QueryText: "SELECT 1 AS id"}, Properties: &native.DescribeTableResult{Self: objectSelf(), Attributes: map[string]string{"ptah.comment": "Caption", "custom": ""}}}},
		{"source", schema.ExternalSourceProperties, &Ydb_Table.DescribeExternalDataSourceResult{Self: objectSelf(), SourceType: proto.String("PostgreSQL"), Location: proto.String("pg.invalid:5432"), Properties: map[string]string{"AUTH_METHOD": "BASIC", "PASSWORD_SECRET_PATH": "/local/auth/credential", "LOGIN": "reader"}}},
		{"table", schema.ExternalTableProperties, &native.DescribeExternalTableResult{Self: objectSelf(), SourceType: proto.String("ObjectStorage"), DataSourcePath: proto.String("/local/App/bucket"), Location: proto.String(""), Content: map[string]string{"FORMAT": `["json_each_row"]`}, Columns: []*native.ColumnMeta{{Name: "z", Type: primitive(Ydb.Type_INT64), NotNull: proto.Bool(true)}, {Name: "a", Type: optional(primitive(Ydb.Type_UTF8)), Family: "cold", DefaultValue: &native.ColumnMeta_FromLiteral{FromLiteral: &Ydb.TypedValue{Type: primitive(Ydb.Type_UTF8), Value: &Ydb.Value{Value: &Ydb.Value_TextValue{TextValue: "Default"}}}}}}}},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := qt.New(t)
			observed, err := schema.CaptureObject(objectOwner(test.kind), test.value)
			c.Assert(err, qt.IsNil)
			observed.Context = &protocol.DatabaseContext{Database: "/local", SourceDatabase: "/local"}
			assertChildRoundTrip(c, observed)
		})
	}
	c := qt.New(t)
	observed, err := schema.CaptureColumnStorage(tableOwner(), storageObservation())
	c.Assert(err, qt.IsNil)
	desired := assertChildRoundTrip(c, observed)
	var decoded native.ColumnStorageIntent
	c.Assert(protojson.Unmarshal(desired.Payload, &decoded), qt.IsNil)
	c.Assert(decoded.HashColumns, qt.DeepEquals, []string{"b", "a"})
	c.Assert(decoded.LocalIndexes[1].GetNgram().CaseSensitive, qt.IsNotNil)
	c.Assert(decoded.LocalIndexes[1].GetNgram().GetCaseSensitive(), qt.IsFalse)
	c.Assert(decoded.Ttl.GetEnabled().Tiers[0].GetExternalStorage(), qt.Equals, "/local/archive")
}

func TestViewConversionRefusesContradictoryOrUnmodeledProperties(t *testing.T) {
	for _, test := range []struct {
		name string
		edit func(*native.ViewObservation)
	}{
		{"access mismatch", func(v *native.ViewObservation) { v.Properties.Self.Owner = "another" }},
		{"table storage", func(v *native.ViewObservation) { v.Properties.StorageSettings = &Ydb_Table.StorageSettings{} }},
		{"table topology", func(v *native.ViewObservation) {
			v.Properties.ShardKeyBounds = []*Ydb.TypedValue{{Type: primitive(Ydb.Type_UINT64)}}
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := qt.New(t)
			view := &native.ViewObservation{View: &Ydb_View.DescribeViewResult{Self: objectSelf(), QueryText: "SELECT 1"}, Properties: &native.DescribeTableResult{Self: objectSelf()}}
			test.edit(view)
			observed, err := schema.CaptureObject(objectOwner(schema.ViewProperties), view)
			c.Assert(err, qt.IsNil)
			desired, err := schema.ConvertNative(observed, protocol.ValueInput_OBSERVED, protocol.ValueInput_DESIRED)
			c.Assert(desired, qt.IsNil)
			c.Assert(err, qt.IsNotNil)
		})
	}
}

func TestDesiredObjectsRefuseInvalidValues(t *testing.T) {
	for _, test := range []struct{ name, kind, payload string }{
		{"empty view", schema.ViewProperties, `{"queryText":" "}`},
		{"view access", schema.ViewProperties, `{"queryText":"SELECT 1","access":{"directGrants":[{}]}}`},
		{"view null attribute", schema.ViewProperties, `{"queryText":"SELECT 1","attributes":{"x":null}}`},
		{"view runtime self", schema.ViewProperties, `{"queryText":"SELECT 1","self":{}}`},
		{"source missing location", schema.ExternalSourceProperties, `{"sourceType":"ObjectStorage"}`},
		{"source null option", schema.ExternalSourceProperties, `{"sourceType":"ObjectStorage","location":"","properties":{"AUTH_METHOD":null}}`},
		{"external no columns", schema.ExternalTableProperties, `{"sourceType":"ObjectStorage","dataSourcePath":"/local/bucket","location":""}`},
		{"external missing type", schema.ExternalTableProperties, `{"sourceType":"ObjectStorage","dataSourcePath":"/local/bucket","location":"","columns":[{"name":"id","settings":{}}]}`},
		{"external sequence position", schema.ExternalTableProperties, `{"sourceType":"ObjectStorage","dataSourcePath":"/local/bucket","location":"","columns":[{"name":"id","type":{"typeId":"INT64"},"settings":{"sequence":{"setVal":{}}}}]}`},
		{"storage repeated hash", schema.ColumnStorageProperties, `{"hashColumns":["id","id"]}`},
		{"storage shards beyond 32 bits", schema.ColumnStorageProperties, `{"partitions":"4294967296"}`},
		{"storage TTL expiration", schema.ColumnStorageProperties, `{"ttl":{"enabled":{"column":"at","expireAfterSeconds":"60","tiers":[{"applyAfterSeconds":"60","delete":{}}]}}}`},
		{"storage TTL without tiers", schema.ColumnStorageProperties, `{"ttl":{"enabled":{"column":"at"}}}`},
		{"storage TTL without column", schema.ColumnStorageProperties, `{"ttl":{"enabled":{"tiers":[{"applyAfterSeconds":"60","delete":{}}]}}}`},
		{"storage TTL unit", schema.ColumnStorageProperties, `{"ttl":{"enabled":{"column":"at","columnUnit":"5","tiers":[{"applyAfterSeconds":"60","delete":{}}]}}}`},
		{"storage TTL deletes first", schema.ColumnStorageProperties, `{"ttl":{"enabled":{"column":"at","tiers":[{"applyAfterSeconds":"60","delete":{}},{"applyAfterSeconds":"120","externalStorage":"/local/ext"}]}}}`},
		{"storage TTL descending", schema.ColumnStorageProperties, `{"ttl":{"enabled":{"column":"at","tiers":[{"applyAfterSeconds":"120","externalStorage":"/local/ext"},{"applyAfterSeconds":"60","delete":{}}]}}}`},
		{"storage TTL relative source", schema.ColumnStorageProperties, `{"ttl":{"enabled":{"column":"at","tiers":[{"applyAfterSeconds":"60","externalStorage":"local/ext"}]}}}`},
		{"storage TTL beyond 32 bits", schema.ColumnStorageProperties, `{"ttl":{"enabled":{"column":"at","tiers":[{"applyAfterSeconds":"4294967296","delete":{}}]}}}`},
		{"storage TTL no action", schema.ColumnStorageProperties, `{"ttl":{"enabled":{"column":"at","tiers":[{"applyAfterSeconds":"60"}]}}}`},
		{"storage runtime id", schema.ColumnStorageProperties, `{"hashColumns":["id"],"partitions":"1","dataChannelCount":"64","columnShards":["1"]}`},
		{"storage malformed TTL", schema.ColumnStorageProperties, `{"hashColumns":["id"],"partitions":"1","dataChannelCount":"64","ttl":{}}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := qt.New(t)
			definition := schema.NativeDesiredModel(test.kind)
			owner := objectOwner(test.kind)
			if test.kind == schema.ColumnStorageProperties {
				owner = tableOwner()
			}
			value := &protocol.Envelope{ProviderId: schema.ProviderID, Kind: test.kind, Owner: owner, SchemaVersion: definition.SchemaVersion, Encoding: definition.Encoding, DefinitionSha256: definition.DefinitionSha256, Payload: []byte(test.payload)}
			result, err := schema.NormalizeDesired(value)
			c.Assert(result, qt.IsNil)
			c.Assert(err, qt.IsNotNil)
		})
	}
}

// TestDesiredColumnStorageLeavesServerChoicesOut accepts declarations that
// leave the hash key, the shard count and the data channels to the server, and
// a TTL of any shape YDB keeps.
func TestDesiredColumnStorageLeavesServerChoicesOut(t *testing.T) {
	for _, payload := range []string{
		`{}`,
		`{"hashColumns":["b","a"]}`,
		`{"partitions":"4294967295"}`,
		`{"ttl":{"disabled":{}}}`,
		`{"ttl":{"enabled":{"column":"at","tiers":[{"applyAfterSeconds":"0","delete":{}}]}}}`,
		`{"ttl":{"enabled":{"column":"at","columnUnit":"4","tiers":[{"applyAfterSeconds":"60","externalStorage":"/local/a"},{"applyAfterSeconds":"120","externalStorage":"/local/b"},{"applyAfterSeconds":"4294967295","delete":{}}]}}}`,
	} {
		t.Run(payload, func(t *testing.T) {
			c := qt.New(t)
			definition := schema.NativeDesiredModel(schema.ColumnStorageProperties)
			value := &protocol.Envelope{ProviderId: schema.ProviderID, Kind: schema.ColumnStorageProperties, Owner: tableOwner(), SchemaVersion: definition.SchemaVersion,
				Encoding: definition.Encoding, DefinitionSha256: definition.DefinitionSha256, Payload: []byte(payload)}
			result, err := schema.NormalizeDesired(value)
			c.Assert(err, qt.IsNil)
			again, err := schema.NormalizeDesired(result)
			c.Assert(err, qt.IsNil)
			c.Assert(string(again.Payload), qt.Equals, string(result.Payload))
		})
	}
}

func TestCoordinationConversionPreservesOmissionAndAccess(t *testing.T) {
	for _, config := range []*Ydb_Coordination.Config{{}, {SelfCheckPeriodMillis: 2000, SessionGracePeriodMillis: 9000, ReadConsistencyMode: Ydb_Coordination.ConsistencyMode_CONSISTENCY_MODE_STRICT, AttachConsistencyMode: Ydb_Coordination.ConsistencyMode_CONSISTENCY_MODE_RELAXED, RateLimiterCountersMode: Ydb_Coordination.RateLimiterCountersMode_RATE_LIMITER_COUNTERS_MODE_DETAILED}} {
		c := qt.New(t)
		input := &Ydb_Coordination.DescribeNodeResult{Self: objectSelf(), Config: config}
		observed, err := schema.CaptureObject(objectOwner(schema.CoordinationProperties), input)
		c.Assert(err, qt.IsNil)
		desired, err := schema.ConvertCoordination(observed, protocol.ValueInput_OBSERVED, protocol.ValueInput_DESIRED)
		c.Assert(err, qt.IsNil)
		settings, err := schema.CoordinationConfiguration(desired, protocol.ValueInput_DESIRED)
		c.Assert(err, qt.IsNil)
		c.Assert(settings.Access.GetOwner(), qt.Equals, "alice")
		c.Assert(schema.ValidateCoordinationAccess(observed, desired), qt.IsNil)
		back, err := schema.ConvertCoordination(desired, protocol.ValueInput_DESIRED, protocol.ValueInput_OBSERVED)
		c.Assert(err, qt.IsNil)
		c.Assert(proto.Equal(back, observed), qt.IsTrue)
	}
}

func TestCoordinationConversionRefusesSourceHolderAndMalformedAccess(t *testing.T) {
	c := qt.New(t)
	value, err := schema.CaptureCoordinationIntent(objectOwner(schema.CoordinationProperties), schema.CoordinationIntent{StructName: "Holder"})
	c.Assert(err, qt.IsNil)
	back, err := schema.ConvertCoordination(value, protocol.ValueInput_DESIRED, protocol.ValueInput_OBSERVED)
	c.Assert(back, qt.IsNil)
	c.Assert(err, qt.IsNotNil)
	for _, payload := range []string{
		`{"access":{"Owner":"alice"}}`, `{"access":{"owner":null}}`, `{"access":{"direct_grants":[null]}}`, `{"access":{"direct_grants":[{"subject":"a","permission_names":["read","read"]}]}}`,
		`{"access":{"directGrants":[]}}`, `{"access":{"direct_grants":[{"subject":"a","permission_names":[null]}]}}`,
	} {
		value.Payload = []byte(payload)
		normalized, err := schema.NormalizeDesired(value)
		c.Assert(normalized, qt.IsNil)
		c.Assert(err, qt.IsNotNil)
	}
}

func TestEveryObservedKindHasItsOwnDesiredContract(t *testing.T) {
	c := qt.New(t)
	for _, observed := range schema.ObservedModels() {
		var found *protocol.ValueModel
		for _, model := range schema.Models() {
			if model.Kind == observed.Kind && model.Representation == protocol.ValueInput_DESIRED {
				found = model
			}
		}
		c.Assert(found, qt.IsNotNil, qt.Commentf("kind %s", observed.Kind))
		c.Assert(found.DefinitionSha256, qt.Not(qt.Equals), observed.DefinitionSha256)
		c.Assert(found.DisplayName, qt.Not(qt.Equals), "")
		c.Assert(found.DisplayName, qt.Equals, observed.DisplayName)
	}
}

func TestViewCaptureExcludesOnlyMeasuredDisabledPartitionSwitches(t *testing.T) {
	c := qt.New(t)
	view := &native.ViewObservation{View: &Ydb_View.DescribeViewResult{Self: objectSelf(), QueryText: "SELECT 1"}, Properties: &native.DescribeTableResult{Self: objectSelf(), PartitioningSettings: &Ydb_Table.PartitioningSettings{PartitioningBySize: Ydb.FeatureFlag_DISABLED, PartitioningByLoad: Ydb.FeatureFlag_DISABLED}}}
	observed, err := schema.CaptureObject(objectOwner(schema.ViewProperties), view)
	c.Assert(err, qt.IsNil)
	var result native.ViewObservation
	c.Assert(protojson.Unmarshal(observed.Payload, &result), qt.IsNil)
	c.Assert(result.Properties.PartitioningSettings, qt.IsNil)
	c.Assert(view.Properties.PartitioningSettings, qt.IsNotNil)
	assertChildRoundTrip(c, observed)
	view.Properties.PartitioningSettings.PartitioningBySize = Ydb.FeatureFlag_ENABLED
	refused, err := schema.CaptureObject(objectOwner(schema.ViewProperties), view)
	c.Assert(refused, qt.IsNil)
	c.Assert(err, qt.IsNotNil)
	view.Properties.PartitioningSettings.PartitioningBySize = Ydb.FeatureFlag_DISABLED
	view.Properties.PartitioningSettings.ProtoReflect().SetUnknown([]byte{0xa0, 0x06, 0x01})
	refused, err = schema.CaptureObject(objectOwner(schema.ViewProperties), view)
	c.Assert(refused, qt.IsNil)
	c.Assert(err, qt.IsNotNil)
}
