package history_test

import (
	"strings"
	"testing"

	qt "github.com/frankban/quicktest"
	"github.com/stokaro/ptah-ydb-plugin/internal/history"
	native "github.com/stokaro/ptah-ydb-plugin/internal/nativeprotocol"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb_Scheme"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb_Table"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
)

func store(c *qt.C, format protocol.HistoryScope_Format) history.Store {
	c.Helper()
	value, err := history.Resolve("/local", &protocol.HistoryScope{SessionId: "session", Schema: "app", Table: "revisions", Format: format})
	c.Assert(err, qt.IsNil)
	return value
}
func nativeRow() *protocol.HistoryRevision {
	return &protocol.HistoryRevision{Version: "-9223372036854775808", Description: "exact\x00π", State: protocol.HistoryRevision_FAILED, Direction: protocol.HistoryRevision_DOWN,
		Applied: 1, Total: 2, Error: "failure", ErrorStatement: "multi\nline", ExecutionNanoseconds: 123456789,
		Checksum: "checksum", AppliedAtSeconds: -62135596800, AppliedAtNanos: 999999999, OperatorVersion: "operator"}
}
func TestHistoryCodecPreservesAllFacts(t *testing.T) {
	c := qt.New(t)
	for _, format := range []protocol.HistoryScope_Format{protocol.HistoryScope_NATIVE, protocol.HistoryScope_ATLAS} {
		s := store(c, format)
		row := nativeRow()
		if format == protocol.HistoryScope_ATLAS {
			row.Version = ""
			row.State = protocol.HistoryRevision_STATE_UNSPECIFIED
			row.Direction = protocol.HistoryRevision_DIRECTION_UNSPECIFIED
			row.PartialHashes = []string{"one", "two"}
			row.AtlasType = 4294967295
		}
		bytes, err := s.Encode(row)
		c.Assert(err, qt.IsNil)
		read, err := s.Decode(row.Version, bytes)
		c.Assert(err, qt.IsNil)
		c.Assert(proto.Equal(row, read), qt.IsTrue)
		again, err := s.Encode(read)
		c.Assert(err, qt.IsNil)
		c.Assert(again, qt.DeepEquals, bytes)
	}
}
func TestHistoryCodecRefusesWrongIdentityUnknownAndNoncanonicalData(t *testing.T) {
	c := qt.New(t)
	s := store(c, protocol.HistoryScope_NATIVE)
	row := nativeRow()
	payload, err := s.Encode(row)
	c.Assert(err, qt.IsNil)
	for _, test := range []struct {
		name, key string
		payload   []byte
	}{
		{"identity", "42", payload},
		{"prefix", row.Version, []byte("foreign")},
		{"duplicate field", row.Version, protowire.AppendString(protowire.AppendTag(append([]byte(nil), payload...), 1, protowire.BytesType), row.Version)},
		{"unknown field", row.Version, protowire.AppendVarint(protowire.AppendTag(append([]byte(nil), payload...), 99, protowire.VarintType), 1)},
		{"oversized", row.Version, make([]byte, history.MaxSnapshotBytes+1)},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := qt.New(t)
			value, err := s.Decode(test.key, test.payload)
			c.Assert(value, qt.IsNil)
			c.Assert(err, qt.IsNotNil)
		})
	}
	_, err = store(c, protocol.HistoryScope_ATLAS).Decode(row.Version, payload)
	c.Assert(err, qt.IsNotNil)
	for _, change := range []func(*protocol.HistoryRevision){
		func(v *protocol.HistoryRevision) { v.Version = "01" },
		func(v *protocol.HistoryRevision) { v.Applied = 3 },
		func(v *protocol.HistoryRevision) { v.AppliedAtNanos = 1e9 },
		func(v *protocol.HistoryRevision) { v.State = 999 },
		func(v *protocol.HistoryRevision) { v.PartialHashes = []string{"atlas"} },
		func(v *protocol.HistoryRevision) {
			v.ProtoReflect().SetUnknown(protowire.AppendVarint(protowire.AppendTag(nil, 99, protowire.VarintType), 1))
		},
	} {
		value := nativeRow()
		change(value)
		_, err := s.Encode(value)
		c.Assert(err, qt.IsNotNil)
	}
}
func TestHistoryScopeCannotEscapeDatabase(t *testing.T) {
	c := qt.New(t)
	for _, test := range []struct{ schema, table string }{
		{"../other", "revisions"}, {"app//nested", "revisions"}, {"app/../other", "revisions"}, {"/local", "revisions"}, {"app/", "revisions"}, {"app", "other/revisions"}, {"app", ".."}, {"app", "bad\\path"},
	} {
		_, err := history.Resolve("/local", &protocol.HistoryScope{SessionId: "s", Schema: test.schema, Table: test.table, Format: protocol.HistoryScope_NATIVE})
		c.Assert(err, qt.IsNotNil)
	}
	s, err := history.Resolve("/local", &protocol.HistoryScope{SessionId: "s", Schema: "app/nested", Table: "a`b", Format: protocol.HistoryScope_ATLAS})
	c.Assert(err, qt.IsNil)
	c.Assert(s.Path(), qt.Equals, "/local/app/nested/a`b")
	c.Assert(s.QuotedPath(), qt.Equals, "`/local/app/nested/a\\`b`")
	key, err := s.Key("")
	c.Assert(err, qt.IsNil)
	c.Assert(key, qt.Equals, "")
}
func layout() *native.DescribeTableResult {
	primitive := func(t Ydb.Type_PrimitiveTypeId) *Ydb.Type { return &Ydb.Type{Type: &Ydb.Type_TypeId{TypeId: t}} }
	return &native.DescribeTableResult{Self: &Ydb_Scheme.Entry{Name: "revisions", Type: Ydb_Scheme.Entry_TABLE},
		StoreType: Ydb_Table.StoreType_STORE_TYPE_ROW, PrimaryKey: []string{"version"},
		Columns:    []*native.ColumnMeta{{Name: "version", Type: primitive(Ydb.Type_INT64)}, {Name: "revision", Type: primitive(Ydb.Type_STRING)}},
		Attributes: map[string]string{"ptah.history": "stokaro/ptah-ydb-plugin/history-v1/native"}}
}
func TestHistoryOwnershipAndBehaviorAreValidated(t *testing.T) {
	c := qt.New(t)
	s := store(c, protocol.HistoryScope_NATIVE)
	c.Assert(s.ValidateLayout(layout()), qt.IsNil)
	for _, test := range []struct {
		name   string
		change func(*native.DescribeTableResult)
	}{
		{"unmarked", func(v *native.DescribeTableResult) { v.Attributes = nil }},
		{"wrong format", func(v *native.DescribeTableResult) {
			v.Attributes["ptah.history"] = strings.ReplaceAll(v.Attributes["ptah.history"], "native", "atlas")
		}},
		{"replica", func(v *native.DescribeTableResult) { v.Attributes["__async_replica"] = "true" }},
		{"wrong key", func(v *native.DescribeTableResult) { v.PrimaryKey = []string{"revision"} }},
		{"wrong table", func(v *native.DescribeTableResult) { v.Self.Name = "other" }},
		{"column store", func(v *native.DescribeTableResult) { v.StoreType = Ydb_Table.StoreType_STORE_TYPE_COLUMN }},
		{"ttl", func(v *native.DescribeTableResult) { v.TtlSettings = &Ydb_Table.TtlSettings{} }},
		{"index", func(v *native.DescribeTableResult) { v.Indexes = []*native.TableIndexDescription{{Name: "index"}} }},
		{"changefeed", func(v *native.DescribeTableResult) { v.Changefeeds = []*native.ChangefeedDescription{{Name: "stream"}} }},
		{"nullable payload", func(v *native.DescribeTableResult) {
			v.Columns[1].Type = &Ydb.Type{Type: &Ydb.Type_OptionalType{OptionalType: &Ydb.OptionalType{Item: v.Columns[1].Type}}}
		}},
		{"default", func(v *native.DescribeTableResult) {
			v.Columns[1].DefaultValue = &native.ColumnMeta_FromLiteral{FromLiteral: &Ydb.TypedValue{}}
		}},
		{"duplicate column", func(v *native.DescribeTableResult) { v.Columns[1] = proto.CloneOf(v.Columns[0]) }},
		{"extra column", func(v *native.DescribeTableResult) { v.Columns = append(v.Columns, proto.CloneOf(v.Columns[0])) }},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := qt.New(t)
			value := layout()
			test.change(value)
			c.Assert(s.ValidateLayout(value), qt.IsNotNil)
		})
	}
}

func TestHistoryClassificationRequiresMarkerAndCompleteLayout(t *testing.T) {
	for _, test := range []struct {
		name   string
		change func(*native.DescribeTableResult)
		owned  bool
		failed bool
	}{
		{"native", func(*native.DescribeTableResult) {}, true, false},
		{"atlas", func(v *native.DescribeTableResult) {
			v.Attributes["ptah.history"] = "stokaro/ptah-ydb-plugin/history-v1/atlas"
			v.Columns[0].Type = &Ydb.Type{Type: &Ydb.Type_TypeId{TypeId: Ydb.Type_UTF8}}
		}, true, false},
		{"same name and layout without marker", func(v *native.DescribeTableResult) { v.Attributes = nil }, false, false},
		{"foreign marker", func(v *native.DescribeTableResult) { v.Attributes["ptah.history"] = "foreign" }, false, true},
		{"empty marker", func(v *native.DescribeTableResult) { v.Attributes["ptah.history"] = "" }, false, true},
		{"wrong identity", func(v *native.DescribeTableResult) { v.Self.Name = "other" }, false, true},
		{"index must not be projected away", func(v *native.DescribeTableResult) { v.Indexes = []*native.TableIndexDescription{{Name: "extra"}} }, false, true},
		{"changefeed must not be projected away", func(v *native.DescribeTableResult) { v.Changefeeds = []*native.ChangefeedDescription{{Name: "extra"}} }, false, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := qt.New(t)
			value := layout()
			test.change(value)
			owned, err := history.Owned("revisions", value)
			c.Assert(err != nil, qt.Equals, test.failed)
			c.Assert(owned, qt.Equals, test.owned)
		})
	}
}
