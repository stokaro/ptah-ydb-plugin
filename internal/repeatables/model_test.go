package repeatables_test

import (
	"testing"

	qt "github.com/frankban/quicktest"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb_Scheme"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb_Table"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
	protocol "ptah.run/provider-sdk/protocol/experimental"

	native "github.com/stokaro/ptah-ydb-plugin/internal/nativeprotocol"
	"github.com/stokaro/ptah-ydb-plugin/internal/repeatables"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
)

// appliedRow holds every field a row can carry, at values a careless codec
// would lose: a body kept empty, the largest attempt order and the edges of
// the time range.
func appliedRow() *protocol.RepeatableRow {
	return &protocol.RepeatableRow{Identity: "user names π", State: protocol.RepeatableRow_FAILED, Checksum: "applied digest",
		AttemptChecksum: "attempt digest", AttemptOrder: 4294967295, Transactional: true, RunId: "run-2",
		AttemptedAtSeconds: -62135596800, AttemptedAtNanos: 999999999, Applied: true, AppliedAtSeconds: 253402300799, AppliedAtNanos: 1,
		Error: "line one\nline two", BodyKept: true}
}

func TestCodec_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	row := appliedRow()
	payload, err := repeatables.Encode(row)
	c.Assert(err, qt.IsNil)
	decoded, err := repeatables.Decode(row.GetIdentity(), payload)
	c.Assert(err, qt.IsNil)
	c.Assert(proto.Equal(decoded, row), qt.IsTrue)
	again, err := repeatables.Encode(decoded)
	c.Assert(err, qt.IsNil)
	c.Assert(again, qt.DeepEquals, payload)
}

func TestCodec_FailurePath(t *testing.T) {
	c := testcheck.New(t)
	row := appliedRow()
	payload, err := repeatables.Encode(row)
	c.Assert(err, qt.IsNil)
	unknown := protowire.AppendTag(append([]byte(nil), payload...), 99, protowire.VarintType)
	unknown = protowire.AppendVarint(unknown, 1)
	for _, test := range []struct {
		name, identity string
		payload        []byte
		want           string
	}{
		{"another prefix", row.GetIdentity(), append([]byte("stokaro/ptah-ydb-plugin/repeatable-v2\x00"), payload[len("stokaro/ptah-ydb-plugin/repeatable-v1\x00"):]...),
			`incompatible YDB repeatable migration row encoding`},
		{"another identity", "other", payload, `YDB repeatable migration row identity or encoding is not canonical`},
		{"an unknown field", row.GetIdentity(), unknown, `unknown fields in .*RepeatableRow`},
		{"not a message", row.GetIdentity(), append([]byte("stokaro/ptah-ydb-plugin/repeatable-v1\x00"), 0xff), `invalid YDB repeatable migration row`},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			decoded, err := repeatables.Decode(test.identity, test.payload)
			c.Assert(err, qt.ErrorMatches, test.want)
			c.Assert(decoded, qt.IsNil)
		})
	}
	t.Run("a row the SDK refuses", func(t *testing.T) {
		c := testcheck.New(t)
		invalid := appliedRow()
		invalid.Applied = false
		payload, err := repeatables.Encode(invalid)
		c.Assert(err, qt.ErrorMatches, `.*a repeatable migration that never applied keeps no digest, time or body`)
		c.Assert(payload, qt.IsNil)
	})
}

func TestResolve_FailurePath(t *testing.T) {
	for _, test := range []struct {
		name  string
		scope *protocol.RepeatableScope
		want  string
	}{
		{"a table that climbs out", &protocol.RepeatableScope{SessionId: "s", Table: ".."}, `invalid YDB store identity`},
		{"a directory that climbs out", &protocol.RepeatableScope{SessionId: "s", Schema: "app/../..", Table: "revisions_repeatable"}, `invalid YDB store directory`},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			store, err := repeatables.Resolve("/local", test.scope)
			c.Assert(err, qt.ErrorMatches, test.want)
			c.Assert(store, qt.DeepEquals, repeatables.Store{})
		})
	}
}

// described is the native description of the record Prepare creates.
func described(name string) *native.DescribeTableResult {
	column := func(name string, id Ydb.Type_PrimitiveTypeId) *native.ColumnMeta {
		return &native.ColumnMeta{Name: name, Type: &Ydb.Type{Type: &Ydb.Type_TypeId{TypeId: id}}, NotNull: new(true)}
	}
	return &native.DescribeTableResult{
		Self:       &Ydb_Scheme.Entry{Name: name, Type: Ydb_Scheme.Entry_TABLE},
		Columns:    []*native.ColumnMeta{column("identity", Ydb.Type_UTF8), column("row", Ydb.Type_STRING), column("body", Ydb.Type_STRING)},
		PrimaryKey: []string{"identity"},
		Attributes: map[string]string{"ptah.repeatable": "stokaro/ptah-ydb-plugin/repeatable-v1"},
		StoreType:  Ydb_Table.StoreType_STORE_TYPE_ROW,
	}
}

func TestOwned_HappyPath(t *testing.T) {
	t.Run("the marked record", func(t *testing.T) {
		c := testcheck.New(t)
		owned, err := repeatables.Owned("revisions_repeatable", described("revisions_repeatable"))
		c.Assert(err, qt.IsNil)
		c.Assert(owned, qt.IsTrue)
	})
	t.Run("a table without the attribute", func(t *testing.T) {
		c := testcheck.New(t)
		value := described("revisions_repeatable")
		value.Attributes = nil
		owned, err := repeatables.Owned("revisions_repeatable", value)
		c.Assert(err, qt.IsNil)
		c.Assert(owned, qt.IsFalse)
	})
}

func TestOwned_FailurePath(t *testing.T) {
	for _, test := range []struct {
		name   string
		change func(*native.DescribeTableResult)
		want   string
	}{
		{"another marker", func(value *native.DescribeTableResult) {
			value.Attributes["ptah.repeatable"] = "stokaro/ptah-ydb-plugin/repeatable-v2"
		}, `unknown YDB repeatable migration record marker`},
		{"a column the layout does not hold", func(value *native.DescribeTableResult) {
			value.Columns[2].Name = "content"
		}, `YDB storage has an incompatible column`},
		{"another key", func(value *native.DescribeTableResult) {
			value.PrimaryKey = []string{"row"}
		}, `YDB storage has incompatible layout or automatic behavior`},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			value := described("revisions_repeatable")
			test.change(value)
			owned, err := repeatables.Owned("revisions_repeatable", value)
			c.Assert(err, qt.ErrorMatches, test.want)
			c.Assert(owned, qt.IsFalse)
		})
	}
}

// The record Prepare creates is the layout Owned recognizes, marker and all.
func TestCreateRequestMatchesOwnership(t *testing.T) {
	c := testcheck.New(t)
	store, err := repeatables.Resolve("/local", &protocol.RepeatableScope{SessionId: "s", Schema: "app", Table: "revisions_repeatable"})
	c.Assert(err, qt.IsNil)
	request := store.CreateRequest()
	c.Assert(request.GetPath(), qt.Equals, "/local/app/revisions_repeatable")
	c.Assert(request.GetStoreType(), qt.Equals, Ydb_Table.StoreType_STORE_TYPE_ROW)
	c.Assert(request.GetPrimaryKey(), qt.DeepEquals, described("x").GetPrimaryKey())
	c.Assert(request.GetAttributes(), qt.DeepEquals, described("x").GetAttributes())
	names := make([]string, 0, len(request.GetColumns()))
	for _, column := range request.GetColumns() {
		names = append(names, column.GetName()+" "+column.GetType().GetTypeId().String())
	}
	c.Assert(names, qt.DeepEquals, []string{"identity UTF8", "row STRING", "body STRING"})
}
