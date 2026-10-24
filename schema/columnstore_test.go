package schema_test

import (
	"testing"

	qt "github.com/frankban/quicktest"
	native "github.com/stokaro/ptah-ydb-plugin/internal/nativeprotocol"
	"github.com/stokaro/ptah-ydb-plugin/schema"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/emptypb"
)

func storageObservation() *native.ColumnStorageObservation {
	return &native.ColumnStorageObservation{
		HashColumns: []string{"b", "a"}, Partitions: 4, DataChannelCount: 64,
		LocalIndexes: []*native.LocalIndexObservation{
			{Name: "z", Columns: []string{"a"}, StorageId: "__DEFAULT", Kind: &native.LocalIndexObservation_Ngram{Ngram: &native.LocalNGramSettings{CaseSensitive: proto.Bool(false)}}},
			{Name: "a", Columns: []string{"b", "a"}, StorageId: "__DEFAULT", Kind: &native.LocalIndexObservation_Bloom{Bloom: &native.LocalBloomSettings{}}},
		},
		Ttl: &native.ColumnTTLPolicy{State: &native.ColumnTTLPolicy_Enabled{Enabled: &native.ColumnTTLEnabled{
			Column: "at", Tiers: []*native.ColumnTTLTier{
				{ApplyAfterSeconds: 86400, Action: &native.ColumnTTLTier_ExternalStorage{ExternalStorage: "/local/archive"}},
				{ApplyAfterSeconds: 604800, Action: &native.ColumnTTLTier_Delete{Delete: &emptypb.Empty{}}},
			},
		}}},
	}
}

func TestColumnStorageCanonicalOrderAndOptionalDefaults(t *testing.T) {
	c := qt.New(t)
	input := storageObservation()
	envelope, err := schema.CaptureColumnStorage(tableOwner(), input)
	c.Assert(err, qt.IsNil)
	var value native.ColumnStorageObservation
	c.Assert(protojson.Unmarshal(envelope.Payload, &value), qt.IsNil)
	c.Assert(value.HashColumns, qt.DeepEquals, []string{"b", "a"})
	c.Assert(value.LocalIndexes[0].Name, qt.Equals, "a")
	c.Assert(value.LocalIndexes[0].Columns, qt.DeepEquals, []string{"b", "a"})
	c.Assert(value.LocalIndexes[1].GetNgram().CaseSensitive, qt.IsNotNil)
	c.Assert(value.LocalIndexes[1].GetNgram().GetCaseSensitive(), qt.IsFalse)
	c.Assert(value.LocalIndexes[1].GetNgram().NgramSize, qt.IsNil)
	c.Assert(value.LocalIndexes[1].GetNgram().FalsePositiveProbability, qt.IsNil)
	c.Assert(value.Ttl.GetEnabled().Tiers[0].GetExternalStorage(), qt.Equals, "/local/archive")
	c.Assert(value.Ttl.GetEnabled().Tiers[1].GetDelete(), qt.IsNotNil)
	c.Assert(input.LocalIndexes[0].Name, qt.Equals, "z")
	again, err := schema.Normalize(envelope)
	c.Assert(err, qt.IsNil)
	c.Assert(proto.Equal(again, envelope), qt.IsTrue)
	input.LocalIndexes[0].GetNgram().NgramSize = proto.Uint64(3)
	explicit, err := schema.CaptureColumnStorage(tableOwner(), input)
	c.Assert(err, qt.IsNil)
	c.Assert(string(explicit.Payload), qt.Not(qt.Equals), string(envelope.Payload))
}

func TestColumnStorageRejectsInvalidModels(t *testing.T) {
	for _, test := range []struct {
		name   string
		change func(*native.ColumnStorageObservation)
	}{
		{"shards beyond 32 bits", func(v *native.ColumnStorageObservation) { v.Partitions = 1 << 32 }},
		{"blank hash column", func(v *native.ColumnStorageObservation) { v.HashColumns = []string{"a", ""} }},
		{"duplicate hash", func(v *native.ColumnStorageObservation) { v.HashColumns = []string{"a", "a"} }},
		{"nil index", func(v *native.ColumnStorageObservation) { v.LocalIndexes[0] = nil }},
		{"missing kind", func(v *native.ColumnStorageObservation) { v.LocalIndexes[0].Kind = nil }},
		{"nil settings", func(v *native.ColumnStorageObservation) {
			v.LocalIndexes[0].Kind = &native.LocalIndexObservation_Ngram{}
		}},
		{"duplicate name", func(v *native.ColumnStorageObservation) { v.LocalIndexes[1].Name = "z" }},
		{"duplicate columns", func(v *native.ColumnStorageObservation) { v.LocalIndexes[1].Columns = []string{"a", "a"} }},
		{"empty columns", func(v *native.ColumnStorageObservation) { v.LocalIndexes[1].Columns = nil }},
		{"ngram size", func(v *native.ColumnStorageObservation) { v.LocalIndexes[0].GetNgram().NgramSize = proto.Uint64(0) }},
		{"probability", func(v *native.ColumnStorageObservation) {
			v.LocalIndexes[0].GetNgram().FalsePositiveProbability = proto.Float64(1)
		}},
		{"storage", func(v *native.ColumnStorageObservation) { v.LocalIndexes[0].StorageId = "other" }},
		{"ttl unit", func(v *native.ColumnStorageObservation) { v.Ttl.GetEnabled().ColumnUnit = 5 }},
		{"ttl column", func(v *native.ColumnStorageObservation) { v.Ttl.GetEnabled().Column = "" }},
		{"ttl state", func(v *native.ColumnStorageObservation) { v.Ttl.State = nil }},
		{"ttl nil state", func(v *native.ColumnStorageObservation) { v.Ttl.State = &native.ColumnTTLPolicy_Enabled{} }},
		{"ttl no policy", func(v *native.ColumnStorageObservation) { v.Ttl.GetEnabled().Tiers = nil }},
		{"ttl nil tier", func(v *native.ColumnStorageObservation) { v.Ttl.GetEnabled().Tiers[0] = nil }},
		{"ttl no action", func(v *native.ColumnStorageObservation) { v.Ttl.GetEnabled().Tiers[0].Action = nil }},
		{"ttl empty source", func(v *native.ColumnStorageObservation) {
			v.Ttl.GetEnabled().Tiers[0].Action = &native.ColumnTTLTier_ExternalStorage{}
		}},
		{"unknown native field", func(v *native.ColumnStorageObservation) {
			v.ProtoReflect().SetUnknown(protowire.AppendVarint(protowire.AppendTag(nil, 500, protowire.VarintType), 1))
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := qt.New(t)
			input := storageObservation()
			test.change(input)
			result, err := schema.CaptureColumnStorage(tableOwner(), input)
			c.Assert(result, qt.IsNil)
			c.Assert(err, qt.IsNotNil)
		})
	}
}

// TestColumnStorageProjectedFromADeclaration normalizes an observation that
// leaves the hash key, the shard count and the data channels out, as a
// prediction from a declaration that leaves them to the server does.
func TestColumnStorageProjectedFromADeclaration(t *testing.T) {
	c := qt.New(t)
	envelope, err := schema.CaptureColumnStorage(tableOwner(), &native.ColumnStorageObservation{})
	c.Assert(err, qt.IsNil)
	c.Assert(string(envelope.Payload), qt.Equals, `{}`)
}

func TestColumnStorageDisabledTTLAndAbsentTTLDiffer(t *testing.T) {
	c := qt.New(t)
	input := storageObservation()
	input.Ttl = nil
	absent, err := schema.CaptureColumnStorage(tableOwner(), input)
	c.Assert(err, qt.IsNil)
	input.Ttl = &native.ColumnTTLPolicy{State: &native.ColumnTTLPolicy_Disabled{Disabled: &emptypb.Empty{}}}
	disabled, err := schema.CaptureColumnStorage(tableOwner(), input)
	c.Assert(err, qt.IsNil)
	c.Assert(string(absent.Payload), qt.Not(qt.Equals), string(disabled.Payload))
}
