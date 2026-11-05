package schema_test

import (
	"slices"
	"testing"

	qt "github.com/frankban/quicktest"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb_Table"

	native "github.com/stokaro/ptah-ydb-plugin/internal/nativeprotocol"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
	"github.com/stokaro/ptah-ydb-plugin/schema"
)

func indexOwner() *protocol.Reference {
	return &protocol.Reference{Engine: "ydb", Database: "/local", Schema: "app", Kind: "index", Path: []string{"records"}, Name: "by_label"}
}

// desiredIndexSettings is a declared index facet with payload.
func desiredIndexSettings(payload string) *protocol.Envelope {
	model := schema.NativeDesiredModel(schema.IndexSettingsProperties)
	return &protocol.Envelope{ProviderId: schema.ProviderID, Kind: model.Kind, SchemaVersion: 1, Encoding: model.Encoding,
		DefinitionSha256: model.DefinitionSha256, Owner: indexOwner(), Payload: []byte(payload)}
}

// TestIndexSettingsConversion_HappyPath writes a read of an index as the
// declaration the host's embedded engine writes, the settings that differ from
// those YDB gives a new index, and predicts what a declared index holds once
// YDB builds it: the declared settings over a new index's, a size turning
// splitting by size on and replicas of none removed.
func TestIndexSettingsConversion_HappyPath(t *testing.T) {
	for _, test := range []struct {
		name     string
		held     *native.GlobalIndexSettings
		declared string
	}{
		{"a new index's settings", &native.GlobalIndexSettings{PartitioningSettings: &Ydb_Table.PartitioningSettings{PartitioningBySize: Ydb.FeatureFlag_ENABLED,
			PartitionSizeMb: 2048, PartitioningByLoad: Ydb.FeatureFlag_DISABLED, MinPartitionsCount: 1}}, `{}`},
		{"changed partitioning", &native.GlobalIndexSettings{PartitioningSettings: &Ydb_Table.PartitioningSettings{PartitioningBySize: Ydb.FeatureFlag_ENABLED,
			PartitionSizeMb: 2048, PartitioningByLoad: Ydb.FeatureFlag_ENABLED, MinPartitionsCount: 2, MaxPartitionsCount: 9, PartitionBy: []string{"label"}}},
			`{"partitioningSettings":{"maxPartitionsCount":"9","minPartitionsCount":"2","partitioningByLoad":"ENABLED"}}`},
		{"splitting by size off", &native.GlobalIndexSettings{PartitioningSettings: &Ydb_Table.PartitioningSettings{PartitioningBySize: Ydb.FeatureFlag_DISABLED,
			PartitioningByLoad: Ydb.FeatureFlag_DISABLED, MinPartitionsCount: 1}}, `{"partitioningSettings":{"partitioningBySize":"DISABLED"}}`},
		{"read replicas", &native.GlobalIndexSettings{ReadReplicasSettings: &Ydb_Table.ReadReplicasSettings{
			Settings: &Ydb_Table.ReadReplicasSettings_PerAzReadReplicasCount{PerAzReadReplicasCount: 1}}}, `{"readReplicasSettings":{"perAzReadReplicasCount":"1"}}`},
		{"read replicas of none", &native.GlobalIndexSettings{ReadReplicasSettings: &Ydb_Table.ReadReplicasSettings{
			Settings: &Ydb_Table.ReadReplicasSettings_PerAzReadReplicasCount{PerAzReadReplicasCount: 0}}}, `{}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			observed, err := schema.CaptureIndexSettings(indexOwner(), test.held)
			c.Assert(err, qt.IsNil)
			converted, err := schema.ConvertNative(observed, protocol.ValueInput_OBSERVED, protocol.ValueInput_DESIRED)
			c.Assert(err, qt.IsNil)
			c.Assert(string(converted.Payload), qt.Equals, test.declared)
			c.Assert(converted.Owner.Name, qt.Equals, "by_label")
		})
	}
}

// TestIndexSettingsPrediction_HappyPath predicts the settings a declared
// index holds once YDB builds it.
func TestIndexSettingsPrediction_HappyPath(t *testing.T) {
	for _, test := range []struct {
		name, declared, want string
	}{
		{"nothing declared", `{}`, `{"partitioningSettings":{"minPartitionsCount":"1","partitionSizeMb":"2048","partitioningByLoad":"DISABLED","partitioningBySize":"ENABLED"}}`},
		{"a size turns splitting by size on", `{"partitioningSettings":{"partitionSizeMb":"512"}}`,
			`{"partitioningSettings":{"minPartitionsCount":"1","partitionSizeMb":"512","partitioningByLoad":"DISABLED","partitioningBySize":"ENABLED"}}`},
		{"a size beside splitting turned off", `{"partitioningSettings":{"partitionSizeMb":"512","partitioningBySize":"DISABLED"}}`,
			`{"partitioningSettings":{"minPartitionsCount":"1","partitionSizeMb":"512","partitioningByLoad":"DISABLED","partitioningBySize":"DISABLED"}}`},
		{"replicas of none", `{"readReplicasSettings":{"anyAzReadReplicasCount":"0"}}`,
			`{"partitioningSettings":{"minPartitionsCount":"1","partitionSizeMb":"2048","partitioningByLoad":"DISABLED","partitioningBySize":"ENABLED"}}`},
		{"replicas and a count", `{"partitioningSettings":{"minPartitionsCount":"3"},"readReplicasSettings":{"anyAzReadReplicasCount":"2"}}`,
			`{"partitioningSettings":{"minPartitionsCount":"3","partitionSizeMb":"2048","partitioningByLoad":"DISABLED","partitioningBySize":"ENABLED"},` +
				`"readReplicasSettings":{"anyAzReadReplicasCount":"2"}}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			predicted, err := schema.ConvertNative(desiredIndexSettings(test.declared), protocol.ValueInput_DESIRED, protocol.ValueInput_OBSERVED)
			c.Assert(err, qt.IsNil)
			c.Assert(string(predicted.Payload), qt.Equals, test.want)
		})
	}
}

// TestIndexSettings_FailurePath refuses a declaration an index does not take,
// and an owner that is not an index of one table.
func TestIndexSettings_FailurePath(t *testing.T) {
	for _, test := range []struct {
		name, payload, wantErr string
		owner                  *protocol.Reference
	}{
		{"partition columns", `{"partitioningSettings":{"partitionBy":["label"]}}`, `YDB index settings name partition columns.*`, indexOwner()},
		{"a minimum above the maximum", `{"partitioningSettings":{"minPartitionsCount":"5","maxPartitionsCount":"2"}}`, `YDB minimum partitions exceed maximum`, indexOwner()},
		{"read replicas without a mode", `{"readReplicasSettings":{}}`, `YDB index read replicas name no mode`, indexOwner()},
		{"a table owner", `{}`, `invalid YDB observation owner`, &protocol.Reference{Engine: "ydb", Database: "/local", Schema: "app", Kind: "table", Name: "records"}},
		{"an index without its table", `{}`, `invalid YDB observation owner path`, &protocol.Reference{Engine: "ydb", Database: "/local", Schema: "app", Kind: "index", Name: "by_label"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			value := desiredIndexSettings(test.payload)
			value.Owner = test.owner
			normalized, err := schema.NormalizeDesired(value)
			c.Assert(err, qt.ErrorMatches, test.wantErr)
			c.Assert(normalized, qt.IsNil)
		})
	}
}

// TestObservedIndexSettings_FailurePath refuses an observation that reports
// replicas of none, which a read reports by leaving them out.
func TestObservedIndexSettings_FailurePath(t *testing.T) {
	c := testcheck.New(t)
	models := schema.ObservedModels()
	at := slices.IndexFunc(models, func(model *protocol.ValueModel) bool { return model.Kind == schema.IndexSettingsProperties })
	c.Assert(at, qt.Not(qt.Equals), -1)
	model := models[at]
	normalized, err := schema.Normalize(&protocol.Envelope{ProviderId: schema.ProviderID, Kind: model.Kind, SchemaVersion: 1, Encoding: model.Encoding,
		DefinitionSha256: model.DefinitionSha256, Owner: indexOwner(), Payload: []byte(`{"readReplicasSettings":{"perAzReadReplicasCount":"0"}}`)})
	c.Assert(err, qt.ErrorMatches, `observed YDB index read replicas report a count of zero.*`)
	c.Assert(normalized, qt.IsNil)
}
