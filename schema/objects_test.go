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
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb_Scheme"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb_Table"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
)

func objectOwner(kind string) *protocol.Reference {
	owner := tableOwner()
	owner.Kind = kind
	return owner
}
func objectSelf() *Ydb_Scheme.Entry {
	return &Ydb_Scheme.Entry{Name: "records", Type: Ydb_Scheme.Entry_VIEW, Owner: "alice", SizeBytes: 23, CreatedAt: &Ydb.VirtualTimestamp{PlanStep: 7}}
}

func TestViewCaptureRetainsBothNativeSources(t *testing.T) {
	c := qt.New(t)
	input := &native.ViewObservation{View: &Ydb_View.DescribeViewResult{Self: objectSelf(), QueryText: "SELECT * FROM app.records"}, Properties: &native.DescribeTableResult{Self: objectSelf(), Attributes: map[string]string{"ptah.comment": "Query caption", "custom": "preserved"}, TableStats: &Ydb_Table.TableStats{}}}
	result, err := schema.CaptureObject(objectOwner(schema.ViewProperties), input)
	c.Assert(err, qt.IsNil)
	var view native.ViewObservation
	c.Assert(protojson.Unmarshal(result.Payload, &view), qt.IsNil)
	c.Assert(view.View.QueryText, qt.Equals, input.View.QueryText)
	c.Assert(view.Properties.Attributes, qt.DeepEquals, input.Properties.Attributes)
	c.Assert(view.View.Self.Owner, qt.Equals, "alice")
	c.Assert(view.Properties.Self.Owner, qt.Equals, "alice")
	c.Assert(view.View.Self.Name, qt.Equals, "")
	c.Assert(view.Properties.Self.Name, qt.Equals, "")
	c.Assert(view.Properties.TableStats, qt.IsNil)
	c.Assert(input.Properties.TableStats, qt.IsNotNil)
	normalized, err := schema.Normalize(result)
	c.Assert(err, qt.IsNil)
	c.Assert(proto.Equal(normalized, result), qt.IsTrue)
}

func TestCoordinationCapturePreservesConfigurationWithoutDuplicatePath(t *testing.T) {
	c := qt.New(t)
	config := &Ydb_Coordination.Config{Path: "/local/app/records", SelfCheckPeriodMillis: 2000, SessionGracePeriodMillis: 9000, ReadConsistencyMode: Ydb_Coordination.ConsistencyMode_CONSISTENCY_MODE_STRICT, AttachConsistencyMode: Ydb_Coordination.ConsistencyMode_CONSISTENCY_MODE_RELAXED, RateLimiterCountersMode: Ydb_Coordination.RateLimiterCountersMode_RATE_LIMITER_COUNTERS_MODE_DETAILED}
	input := &Ydb_Coordination.DescribeNodeResult{Self: objectSelf(), Config: config}
	result, err := schema.CaptureObject(objectOwner(schema.CoordinationProperties), input)
	c.Assert(err, qt.IsNil)
	var node Ydb_Coordination.DescribeNodeResult
	c.Assert(protojson.Unmarshal(result.Payload, &node), qt.IsNil)
	want := proto.CloneOf(config)
	want.Path = ""
	c.Assert(proto.Equal(node.Config, want), qt.IsTrue)
	c.Assert(config.Path, qt.Equals, "/local/app/records")
	c.Assert(node.GetSelf().GetName(), qt.Equals, "")
	normalized, err := schema.Normalize(result)
	c.Assert(err, qt.IsNil)
	c.Assert(proto.Equal(normalized, result), qt.IsTrue)
}

func TestExternalCapturePreservesNativeOptionsAndOrderedColumns(t *testing.T) {
	c := qt.New(t)
	source := &Ydb_Table.DescribeExternalDataSourceResult{Self: objectSelf(), SourceType: proto.String("PostgreSQL"), Location: proto.String("pg.invalid:5432"), Properties: map[string]string{"AUTH_METHOD": "BASIC", "PASSWORD_SECRET_PATH": "/local/auth/credential", "LOGIN": "reader"}}
	sourceEnvelope, err := schema.CaptureObject(objectOwner(schema.ExternalSourceProperties), source)
	c.Assert(err, qt.IsNil)
	var decodedSource Ydb_Table.DescribeExternalDataSourceResult
	c.Assert(protojson.Unmarshal(sourceEnvelope.Payload, &decodedSource), qt.IsNil)
	c.Assert(decodedSource.Properties, qt.DeepEquals, source.Properties)
	table := &native.DescribeExternalTableResult{Self: objectSelf(), SourceType: proto.String("ObjectStorage"), DataSourcePath: proto.String("/local/app/bucket"), Location: proto.String("f/"), Content: map[string]string{"format": `["json_each_row"]`}, Columns: []*native.ColumnMeta{{Name: "z", Type: primitive(Ydb.Type_INT64)}, {Name: "a", Type: optional(primitive(Ydb.Type_UTF8))}}}
	tableEnvelope, err := schema.CaptureObject(objectOwner(schema.ExternalTableProperties), table)
	c.Assert(err, qt.IsNil)
	var decodedTable native.DescribeExternalTableResult
	c.Assert(protojson.Unmarshal(tableEnvelope.Payload, &decodedTable), qt.IsNil)
	c.Assert(decodedTable.Columns[0].Name, qt.Equals, "z")
	c.Assert(decodedTable.Columns[1].Name, qt.Equals, "a")
	c.Assert(decodedTable.GetDataSourcePath(), qt.Equals, "/local/app/bucket")
	c.Assert(decodedTable.Content, qt.DeepEquals, table.Content)
	for _, value := range []*protocol.Envelope{sourceEnvelope, tableEnvelope} {
		normalized, err := schema.Normalize(value)
		c.Assert(err, qt.IsNil)
		c.Assert(proto.Equal(normalized, value), qt.IsTrue)
	}
}

func TestNativeObjectCaptureRefusesLossyOrMismatchedDescriptions(t *testing.T) {
	missingAttributes := &native.ViewObservation{View: &Ydb_View.DescribeViewResult{QueryText: "SELECT 1"}}
	wrongShape := &native.ViewObservation{View: &Ydb_View.DescribeViewResult{QueryText: "SELECT 1"}, Properties: &native.DescribeTableResult{Columns: []*native.ColumnMeta{{Name: "id", Type: primitive(Ydb.Type_INT64)}}}}
	unknownSelf := &native.ViewObservation{View: &Ydb_View.DescribeViewResult{Self: objectSelf(), QueryText: "SELECT 1"}, Properties: &native.DescribeTableResult{}}
	unknownSelf.View.Self.ProtoReflect().SetUnknown(protowire.AppendVarint(protowire.AppendTag(nil, 500, protowire.VarintType), 1))
	for _, test := range []struct {
		name, kind string
		value      proto.Message
	}{
		{"view without attributes read", schema.ViewProperties, missingAttributes},
		{"view with table children", schema.ViewProperties, wrongShape},
		{"unknown self before projection", schema.ViewProperties, unknownSelf},
		{"unknown coordination mode", schema.CoordinationProperties, &Ydb_Coordination.DescribeNodeResult{Config: &Ydb_Coordination.Config{ReadConsistencyMode: 999}}},
		{"missing coordination configuration", schema.CoordinationProperties, &Ydb_Coordination.DescribeNodeResult{}},
		{"wrong native model", schema.ViewProperties, &Ydb_Coordination.DescribeNodeResult{Config: &Ydb_Coordination.Config{}}},
		{"external table without columns", schema.ExternalTableProperties, &native.DescribeExternalTableResult{SourceType: proto.String("ObjectStorage"), DataSourcePath: proto.String("/local/s"), Location: proto.String("f/")}},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := qt.New(t)
			value, err := schema.CaptureObject(objectOwner(test.kind), test.value)
			c.Assert(value, qt.IsNil)
			c.Assert(err, qt.IsNotNil)
		})
	}
	c := qt.New(t)
	node, err := schema.CaptureObject(objectOwner(schema.CoordinationProperties), &Ydb_Coordination.DescribeNodeResult{Config: &Ydb_Coordination.Config{}})
	c.Assert(err, qt.IsNil)
	node.Payload = []byte(`{"config":{"path":"/local/wrong"}}`)
	result, err := schema.Normalize(node)
	c.Assert(result, qt.IsNil)
	c.Assert(err, qt.IsNotNil)
}
