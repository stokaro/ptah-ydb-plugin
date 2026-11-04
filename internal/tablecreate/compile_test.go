package tablecreate_test

import (
	"bytes"
	"testing"

	qt "github.com/frankban/quicktest"
	"github.com/stokaro/ptah-ydb-plugin/internal/backend"
	"github.com/stokaro/ptah-ydb-plugin/internal/tablecreate"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
	"github.com/stokaro/ptah-ydb-plugin/schema"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
)

func owner() *protocol.Reference {
	return &protocol.Reference{Engine: "ydb", Kind: "table", Database: "/local", Schema: "app", Name: "records"}
}
func declaration() *protocol.DeclaredTable {
	return &protocol.DeclaredTable{Name: "records", Schema: "app", PrimaryKey: []string{"second", "first"},
		Columns: []*protocol.DeclaredColumn{{Name: "first", Type: "Uint64", Primary: true}, {Name: "second", Type: "Utf8", Primary: true},
			{Name: "value", Type: "Decimal(22,9)", Nullable: true}}}
}
func part(s string) *protocol.ComparisonPart {
	if s == "" {
		return nil
	}
	return &protocol.ComparisonPart{Source: s, Normalized: s}
}
func facet(c *qt.C, kind, name, payload string) *protocol.ComparisonRecord {
	c.Helper()
	reference := owner()
	model := schema.TableDesiredModel()
	if kind == schema.ColumnProperties {
		reference.Kind, reference.Path, reference.Name = "column", []string{reference.Name}, name
		model = schema.ColumnDesiredModel()
	}
	if kind == schema.IndexProperties || kind == schema.ChangefeedProperties {
		model = schema.NativeDesiredModel(kind)
	}
	values, err := backend.New().NormalizeValues(c.Context(), &protocol.NormalizeValuesRequest{Engine: "ydb", Values: []*protocol.ValueInput{
		{Id: kind + name, Representation: protocol.ValueInput_DESIRED, Value: &protocol.Envelope{ProviderId: schema.ProviderID,
			Kind: kind, SchemaVersion: 1, Encoding: model.Encoding, DefinitionSha256: model.DefinitionSha256, Owner: reference, Payload: []byte(payload)}},
	}})
	c.Assert(err, qt.IsNil)
	identity := &protocol.ComparisonIdentity{Kind: reference.Kind, Database: part(reference.Database), Schema: part(reference.Schema), Name: part(reference.Name)}
	if reference.Kind == "column" {
		identity.Parent = part("records")
	}
	return &protocol.ComparisonRecord{Kind: kind, Subject: identity, Value: values.Values[0]}
}
func withSettings(c *qt.C) *protocol.DeclaredTable {
	c.Helper()
	table := declaration()
	table.Features = &protocol.CapturedFeatures{Facets: &protocol.ComparisonState{Representation: protocol.ValueInput_DESIRED, Records: []*protocol.ComparisonRecord{
		facet(c, schema.TableProperties, "", `{"primaryKey":["second","first"],"attributes":{"purpose":"fixture"},"columnFamilies":[{"name":"archive"}],"keyBloomFilter":"DISABLED","partitioningSettings":{"minPartitionsCount":"1","maxPartitionsCount":"2"}}`),
		facet(c, schema.ColumnProperties, "value", `{"family":"archive","dropDefault":true}`),
	}}}
	return table
}
func TestCompileNativeCreation(t *testing.T) {
	c := testcheck.New(t)
	table := withSettings(c)
	original := proto.CloneOf(table)
	request, err := tablecreate.Compile(owner(), table, nil)
	c.Assert(err, qt.IsNil)
	c.Assert(request.Path, qt.Equals, "")
	c.Assert(request.SessionId, qt.Equals, "")
	c.Assert(request.PrimaryKey, qt.DeepEquals, []string{"second", "first"})
	c.Assert(request.KeyBloomFilter, qt.Equals, Ydb.FeatureFlag_DISABLED)
	c.Assert(request.Attributes, qt.DeepEquals, map[string]string{"purpose": "fixture"})
	c.Assert(request.Columns[2].Family, qt.Equals, "archive")
	c.Assert(request.Columns[2].GetDefaultValue(), qt.IsNil)
	spelling, nullable, err := schema.ColumnType(request.Columns[2].Type)
	c.Assert(err, qt.IsNil)
	c.Assert(spelling, qt.Equals, "Decimal(22,9)")
	c.Assert(nullable, qt.IsTrue)
	c.Assert(request.Columns[2].NotNull, qt.DeepEquals, new(false))
	c.Assert(proto.Equal(original, table), qt.IsTrue)
	request.Attributes["purpose"] = "mutated"
	request.PrimaryKey[0] = "mutated"
	c.Assert(proto.Equal(original, table), qt.IsTrue)
}
func TestCompilePrimaryKeySources(t *testing.T) {
	for _, test := range []struct {
		name    string
		change  func(*protocol.DeclaredTable)
		wanted  []string
		refused bool
	}{
		{"native order", func(d *protocol.DeclaredTable) { d.PrimaryKey = nil }, []string{"second", "first"}, false},
		{"conflicting order", func(d *protocol.DeclaredTable) { d.PrimaryKey = []string{"first", "second"} }, nil, true},
		{"single flag", func(d *protocol.DeclaredTable) { d.Features = nil; d.PrimaryKey = nil; d.Columns[1].Primary = false }, []string{"first"}, false},
		{"several flags in declaration order", func(d *protocol.DeclaredTable) { d.Features = nil; d.PrimaryKey = nil }, []string{"first", "second"}, false},
		{"no flag and no order", func(d *protocol.DeclaredTable) {
			d.Features, d.PrimaryKey, d.Columns[0].Primary, d.Columns[1].Primary = nil, nil, false, false
		}, nil, true},
		{"missing key column", func(d *protocol.DeclaredTable) { d.Features = nil; d.PrimaryKey = []string{"absent"} }, nil, true},
		{"repeated key column", func(d *protocol.DeclaredTable) { d.Features = nil; d.PrimaryKey = []string{"first", "first"} }, nil, true},
		{"extra key membership", func(d *protocol.DeclaredTable) { d.Features = nil; d.PrimaryKey = []string{"first"} }, nil, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			d := withSettings(c)
			test.change(d)
			result, err := tablecreate.Compile(owner(), d, nil)
			if test.refused {
				c.Assert(err, qt.IsNotNil)
				c.Assert(result, qt.IsNil)
				return
			}
			c.Assert(err, qt.IsNil)
			c.Assert(result.PrimaryKey, qt.DeepEquals, test.wanted)
		})
	}
}
func TestCompileRefusesUnimplementedOrForgedState(t *testing.T) {
	for _, test := range []struct {
		name   string
		change func(*qt.C, *protocol.Reference, *protocol.DeclaredTable)
	}{
		{"owner", func(_ *qt.C, o *protocol.Reference, _ *protocol.DeclaredTable) { o.Name = "other" }},
		{"path traversal", func(_ *qt.C, o *protocol.Reference, d *protocol.DeclaredTable) {
			o.Schema = "../app"
			d.Schema = o.Schema
		}},
		{"no columns", func(_ *qt.C, _ *protocol.Reference, d *protocol.DeclaredTable) { d.Columns = nil }},
		{"duplicate columns", func(_ *qt.C, _ *protocol.Reference, d *protocol.DeclaredTable) {
			d.Columns = append(d.Columns, proto.CloneOf(d.Columns[0]))
		}},
		{"invalid type", func(_ *qt.C, _ *protocol.Reference, d *protocol.DeclaredTable) {
			d.Columns[0].Type = "Uint64;DROP TABLE x"
		}},
		{"common default", func(_ *qt.C, _ *protocol.Reference, d *protocol.DeclaredTable) { d.Columns[2].DefaultExpression = "42" }},
		{"foreign key", func(_ *qt.C, _ *protocol.Reference, d *protocol.DeclaredTable) {
			d.Columns[2].ForeignReference = "x.id"
		}},
		{"checks", func(_ *qt.C, _ *protocol.Reference, d *protocol.DeclaredTable) { d.Checks = []string{"true"} }},
		{"comment", func(_ *qt.C, _ *protocol.Reference, d *protocol.DeclaredTable) { d.Comment = "keep me" }},
		{"partial index", func(_ *qt.C, _ *protocol.Reference, d *protocol.DeclaredTable) {
			d.Indexes = []*protocol.DeclaredIndex{{Name: "keep", Fields: []string{"value"}, Condition: "value > 0"}}
		}},
		{"receipt", func(_ *qt.C, _ *protocol.Reference, d *protocol.DeclaredTable) {
			d.Features.Facets.Records[0].Value.CanonicalProjection = []byte("{}")
		}},
		{"bound identity", func(_ *qt.C, _ *protocol.Reference, d *protocol.DeclaredTable) {
			d.Features.Facets.Records[0].Subject.Name = part("other")
		}},
		{"missing column", func(c *qt.C, _ *protocol.Reference, d *protocol.DeclaredTable) {
			d.Features.Facets.Records = append(d.Features.Facets.Records, facet(c, schema.ColumnProperties, "missing", `{}`))
		}},
		{"access", func(c *qt.C, _ *protocol.Reference, d *protocol.DeclaredTable) {
			d.Features.Facets.Records[0] = facet(c, schema.TableProperties, "", `{"primaryKey":["second","first"],"access":{}}`)
		}},
		{"temporary", func(c *qt.C, _ *protocol.Reference, d *protocol.DeclaredTable) {
			d.Features.Facets.Records[0] = facet(c, schema.TableProperties, "", `{"primaryKey":["second","first"],"temporary":true}`)
		}},
		{"sequence", func(c *qt.C, _ *protocol.Reference, d *protocol.DeclaredTable) {
			d.Features.Facets.Records[1] = facet(c, schema.ColumnProperties, "value", `{"sequence":{"startValue":"10"}}`)
		}},
		{"encoding", func(c *qt.C, _ *protocol.Reference, d *protocol.DeclaredTable) {
			d.Features.Facets.Records[1] = facet(c, schema.ColumnProperties, "value", `{"encoding":[{"dictionary":{}}]}`)
		}},
		{"missing family", func(c *qt.C, _ *protocol.Reference, d *protocol.DeclaredTable) {
			d.Features.Facets.Records[1] = facet(c, schema.ColumnProperties, "value", `{"family":"missing"}`)
		}},
		{"missing TTL column", func(c *qt.C, _ *protocol.Reference, d *protocol.DeclaredTable) {
			d.Features.Facets.Records[0] = facet(c, schema.TableProperties, "", `{"primaryKey":["second","first"],"ttlSettings":{"dateTypeColumn":{"columnName":"missing","expireAfterSeconds":3600}}}`)
		}},
		{"duplicate facet", func(_ *qt.C, _ *protocol.Reference, d *protocol.DeclaredTable) {
			d.Features.Facets.Records = append(d.Features.Facets.Records, d.Features.Facets.Records[0])
		}},
		{"excluded facet", func(_ *qt.C, _ *protocol.Reference, d *protocol.DeclaredTable) {
			d.Features.Facets.Records[0].Value = nil
		}},
		{"unknown wire field", func(_ *qt.C, _ *protocol.Reference, d *protocol.DeclaredTable) {
			d.ProtoReflect().SetUnknown(protowire.AppendVarint(protowire.AppendTag(nil, 500, protowire.VarintType), 1))
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			o, d := owner(), withSettings(c)
			test.change(c, o, d)
			before, _ := proto.Marshal(d)
			result, err := tablecreate.Compile(o, d, nil)
			c.Assert(err, qt.IsNotNil)
			c.Assert(result, qt.IsNil)
			after, _ := proto.Marshal(d)
			c.Assert(bytes.Equal(before, after), qt.IsTrue)
		})
	}
}

// A read of a table that holds only global indexes converts to an empty
// native index set, which states nothing to create. A vector or full-text
// index the set names is added by a step of its own after the creation (see
// TestNativeIndexCreation_HappyPath), so the creation request carries none of
// the set either way. A global index the set names is one the creation would
// leave out, and is refused.
func TestCompileNativeIndexSet(t *testing.T) {
	c := testcheck.New(t)
	plain, err := tablecreate.Compile(owner(), withSettings(c), nil)
	c.Assert(err, qt.IsNil)
	table := withSettings(c)
	table.Features.Facets.Records = append(table.Features.Facets.Records, facet(c, schema.IndexProperties, "", `{}`))
	empty, err := tablecreate.Compile(owner(), table, nil)
	c.Assert(err, qt.IsNil)
	c.Assert(proto.Equal(empty, plain), qt.IsTrue)
	table = withSettings(c)
	table.Features.Facets.Records = append(table.Features.Facets.Records, facet(c, schema.IndexProperties, "", vectorIndexes(false)))
	vector, err := tablecreate.Compile(owner(), table, nil)
	c.Assert(err, qt.IsNil)
	c.Assert(proto.Equal(vector, plain), qt.IsTrue)
	table = withSettings(c)
	table.Features.Facets.Records = append(table.Features.Facets.Records, facet(c, schema.IndexProperties, "", declaredIndexes(indexJSON("globalIndex", "by_value", []string{"value"}, nil))))
	global, err := tablecreate.Compile(owner(), table, nil)
	c.Assert(err, qt.ErrorMatches, `index "by_value": the native index set states a global index, and a new table takes its global indexes from the common declaration`)
	c.Assert(global, qt.IsNil)
}
