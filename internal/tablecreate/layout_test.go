package tablecreate_test

import (
	"testing"

	qt "github.com/frankban/quicktest"
	"github.com/stokaro/ptah-ydb-plugin/internal/backend"
	"github.com/stokaro/ptah-ydb-plugin/internal/tablecreate"
	"github.com/stokaro/ptah-ydb-plugin/schema"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb_Table"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

// layoutDeclaration declares a table whose key is the given columns, in order,
// with the starting layout fields of its table properties.
func layoutDeclaration(c *qt.C, fields string, columns ...*protocol.DeclaredColumn) *protocol.DeclaredTable {
	c.Helper()
	table := &protocol.DeclaredTable{Name: "records", Schema: "app", Columns: columns}
	key := `"primaryKey":[`
	for i, column := range columns {
		if column.Primary {
			if i > 0 {
				key += ","
			}
			key += `"` + column.Name + `"`
		}
	}
	key += `]`
	table.Features = &protocol.CapturedFeatures{Facets: &protocol.ComparisonState{Representation: protocol.ValueInput_DESIRED, Records: []*protocol.ComparisonRecord{
		facet(c, schema.TableProperties, "", `{`+key+`,`+fields+`}`)}}}
	return table
}

// optionalTuple is a split point of one value: a tuple of one optional element.
func optionalTuple(kind, value string) string {
	return `{"type":{"tupleType":{"elements":[{"optionalType":{"item":{` + kind + `}}}]}},"value":{"items":[{"nestedValue":{` + value + `}}]}}`
}

func key(name, kind string) *protocol.DeclaredColumn {
	return &protocol.DeclaredColumn{Name: name, Type: kind, Primary: true}
}

func TestCompileStartingLayout_HappyPath(t *testing.T) {
	for _, test := range []struct {
		name    string
		fields  string
		columns []*protocol.DeclaredColumn
		want    string
	}{
		{"a uniform layout over a Uint64 key", `"uniformPartitions":"4"`, []*protocol.DeclaredColumn{key("id", "Uint64")},
			`{"uniformPartitions":"4"}`},
		{"a uniform layout over a Uint32 first key column", `"uniformPartitions":"8"`, []*protocol.DeclaredColumn{key("id", "Uint32"), key("name", "Utf8")},
			`{"uniformPartitions":"8"}`},
		{"split points over an unsigned key", `"partitionAtKeys":[{"values":["10"]},{"values":["20"]}]`, []*protocol.DeclaredColumn{key("id", "Uint64")},
			`{"partitionAtKeys":{"splitPoints":[` + optionalTuple(`"typeId":"UINT64"`, `"uint64Value":"10"`) + `,` + optionalTuple(`"typeId":"UINT64"`, `"uint64Value":"20"`) + `]}}`},
		{"split points over a signed key", `"partitionAtKeys":[{"values":["5"]}]`, []*protocol.DeclaredColumn{key("id", "Int32")},
			`{"partitionAtKeys":{"splitPoints":[` + optionalTuple(`"typeId":"INT32"`, `"int32Value":5`) + `]}}`},
		{"split points over a text key", `"partitionAtKeys":[{"values":["m"]}]`, []*protocol.DeclaredColumn{key("name", "Utf8")},
			`{"partitionAtKeys":{"splitPoints":[` + optionalTuple(`"typeId":"UTF8"`, `"textValue":"m"`) + `]}}`},
		{"a split point over the leading columns of a composite key", `"partitionAtKeys":[{"values":["10"]}]`,
			[]*protocol.DeclaredColumn{key("id", "Uint8"), key("name", "Utf8")},
			`{"partitionAtKeys":{"splitPoints":[` + optionalTuple(`"typeId":"UINT8"`, `"uint32Value":10`) + `]}}`},
		{"a split point over the whole composite key", `"partitionAtKeys":[{"values":["10","a"]}]`,
			[]*protocol.DeclaredColumn{key("id", "Uint64"), key("name", "Utf8")},
			`{"partitionAtKeys":{"splitPoints":[{"type":{"tupleType":{"elements":[{"optionalType":{"item":{"typeId":"UINT64"}}},{"optionalType":{"item":{"typeId":"UTF8"}}}]}},` +
				`"value":{"items":[{"nestedValue":{"uint64Value":"10"}},{"nestedValue":{"textValue":"a"}}]}}]}}`},
		{"no layout", `"keyBloomFilter":"ENABLED"`, []*protocol.DeclaredColumn{key("id", "Utf8")}, `{}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := qt.New(t)
			request, err := tablecreate.Compile(owner(), layoutDeclaration(c, test.fields, test.columns...))
			c.Assert(err, qt.IsNil)
			encoded, err := protojson.Marshal(&Ydb_Table.CreateTableRequest{Partitions: request.Partitions})
			c.Assert(err, qt.IsNil)
			c.Assert(string(encoded), qt.JSONEquals, jsonValue(c, test.want))
		})
	}
}

func TestCompileStartingLayout_FailurePath(t *testing.T) {
	for _, test := range []struct {
		name    string
		fields  string
		columns []*protocol.DeclaredColumn
		message string
	}{
		{"a uniform layout over a signed first key column", `"uniformPartitions":"4"`, []*protocol.DeclaredColumn{key("id", "Int64")},
			"the starting layout: uniform_partitions splits the range of the first key column, which YDB does for a Uint32 or Uint64 column only, " +
				"and this one is Int64 .`Unsupported first key column type Int64, only Uint32 and Uint64 are supported`."},
		{"a uniform layout over a text first key column", `"uniformPartitions":"4"`, []*protocol.DeclaredColumn{key("name", "Utf8"), key("id", "Uint64")},
			"the starting layout: uniform_partitions splits the range of the first key column.*this one is Utf8.*"},
		{"more values than key columns", `"partitionAtKeys":[{"values":["10"]},{"values":["20","a","b"]}]`, []*protocol.DeclaredColumn{key("id", "Uint64"), key("name", "Utf8")},
			"the starting layout: split point 2 of partition_at_keys: it holds 3 values, and the key has 2 columns .`Partition at keys has 3 key values while there are only 2 key columns`."},
		{"a value out of the column's range", `"partitionAtKeys":[{"values":["300"]}]`, []*protocol.DeclaredColumn{key("id", "Uint8")},
			`the starting layout: split point 1 of partition_at_keys: "300" is not a whole number from 0 to the largest Uint8.*`},
		{"a value beyond the largest signed integer", `"partitionAtKeys":[{"values":["128"]}]`, []*protocol.DeclaredColumn{key("id", "Int8")},
			`the starting layout: split point 1 of partition_at_keys: "128" is not a whole number from 0 to the largest Int8.*`},
		{"a negative value", `"partitionAtKeys":[{"values":["-10"]}]`, []*protocol.DeclaredColumn{key("id", "Int64")},
			`the starting layout: split point 1 of partition_at_keys: "-10" is not a whole number from 0 to the largest Int64.*`},
		{"a word for a number", `"partitionAtKeys":[{"values":["ten"]}]`, []*protocol.DeclaredColumn{key("id", "Uint64")},
			`the starting layout: split point 1 of partition_at_keys: "ten" is not a whole number.*`},
		{"a key column YDB takes no literal for", `"partitionAtKeys":[{"values":["2026-01-01"]}]`, []*protocol.DeclaredColumn{key("at", "Timestamp")},
			"the starting layout: split point 1 of partition_at_keys: its key column is Timestamp, and YDB takes only literal numbers and strings here.*"},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := qt.New(t)
			table := layoutDeclaration(c, test.fields, test.columns...)
			request, err := tablecreate.Compile(owner(), table)
			c.Assert(err, qt.ErrorMatches, test.message)
			c.Assert(request, qt.IsNil)
		})
	}
}

func TestStartingLayout_BothLayouts_FailurePath(t *testing.T) {
	c := qt.New(t)
	model := schema.TableDesiredModel()

	_, err := backend.New().NormalizeValues(c.Context(), &protocol.NormalizeValuesRequest{Engine: "ydb", Values: []*protocol.ValueInput{
		{Id: "both", Representation: protocol.ValueInput_DESIRED, Value: &protocol.Envelope{ProviderId: schema.ProviderID, Kind: schema.TableProperties, SchemaVersion: 1,
			Encoding: model.Encoding, DefinitionSha256: model.DefinitionSha256, Owner: owner(),
			Payload: []byte(`{"uniformPartitions":"4","partitionAtKeys":[{"values":["10"]}]}`)}},
	}})

	c.Assert(err, qt.ErrorMatches, ".*uniform_partitions and partition_at_keys are both declared, which YDB refuses.*")
}

func TestStartingLayout_ReachesTheCreationOperation(t *testing.T) {
	c := qt.New(t)
	table := layoutDeclaration(c, `"partitionAtKeys":[{"values":["10"]},{"values":["20"]}]`, key("id", "Uint64"))

	captured, err := tablecreate.Capture("create", owner(), table)

	c.Assert(err, qt.IsNil)
	request, err := tablecreate.Request(captured.Value)
	c.Assert(err, qt.IsNil)
	c.Assert(tablecreate.StartingPartitions(request), qt.Equals, uint64(3))
	c.Assert(tablecreate.StartingPartitions(&Ydb_Table.CreateTableRequest{}), qt.Equals, uint64(0))
}

func TestStartingLayout_ForgedOperation_FailurePath(t *testing.T) {
	good := `{"type":{"tupleType":{"elements":[{"optionalType":{"item":{"typeId":"UINT64"}}}]}},"value":{"items":[{"nestedValue":{"uint64Value":"10"}}]}}`
	for _, test := range []struct {
		name       string
		key        string
		partitions string
		message    string
	}{
		{"a split point with another element type", "UINT64",
			`"partitionAtKeys":{"splitPoints":[{"type":{"tupleType":{"elements":[{"optionalType":{"item":{"typeId":"UTF8"}}}]}},"value":{"items":[{"nestedValue":{"textValue":"10"}}]}}]}`,
			"YDB creation states a starting layout of a shape the provider does not write"},
		{"a split point that is not optional", "UINT64",
			`"partitionAtKeys":{"splitPoints":[{"type":{"tupleType":{"elements":[{"typeId":"UINT64"}]}},"value":{"items":[{"uint64Value":"10"}]}}]}`,
			`split point 1 of partition_at_keys: "" is not a whole number.*`},
		{"a uniform layout over a text key", "UTF8", `"uniformPartitions":"4"`, "uniform_partitions splits the range of the first key column.*"},
		{"a split point with too many values", "UINT64",
			`"partitionAtKeys":{"splitPoints":[{"type":{"tupleType":{"elements":[{"optionalType":{"item":{"typeId":"UINT64"}}},{"optionalType":{"item":{"typeId":"UINT64"}}}]}},` +
				`"value":{"items":[{"nestedValue":{"uint64Value":"10"}},{"nestedValue":{"uint64Value":"11"}}]}}]}`, "split point 1 of partition_at_keys: it holds 2 values.*"},
		{"no split point", "UINT64", `"partitionAtKeys":{"splitPoints":[]}`, "YDB creation states an empty starting layout"},
		{"a zero count", "UINT64", `"uniformPartitions":"0"`, "YDB creation states an empty starting layout"},
		{"a split point value of no kind", "UINT64", `"partitionAtKeys":{"splitPoints":[{"type":{"tupleType":{"elements":[{"optionalType":{"item":{"typeId":"UINT64"}}}]}},"value":{"items":[{"nestedValue":{}}]}}]}`,
			"split point 1 of partition_at_keys: \"\" is not a whole number.*"},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := qt.New(t)
			payload := `{"columns":[{"name":"id","type":{"optionalType":{"item":{"typeId":"` + test.key + `"}}},"notNull":false}],"primaryKey":["id"],"storeType":"STORE_TYPE_ROW",` + test.partitions + `}`
			_, err := tablecreate.Normalize(&protocol.ValueInput{Id: "forged", Representation: protocol.ValueInput_OPERATION, Value: operationEnvelope(payload)})
			c.Assert(err, qt.ErrorMatches, test.message)
		})
	}
	t.Run("the shape the provider writes", func(t *testing.T) {
		c := qt.New(t)
		payload := `{"columns":[{"name":"id","type":{"optionalType":{"item":{"typeId":"UINT64"}}},"notNull":false}],"primaryKey":["id"],"storeType":"STORE_TYPE_ROW",` +
			`"partitionAtKeys":{"splitPoints":[` + good + `]}}`
		_, err := tablecreate.Normalize(&protocol.ValueInput{Id: "good", Representation: protocol.ValueInput_OPERATION, Value: operationEnvelope(payload)})
		c.Assert(err, qt.IsNil)
	})
}

// operationEnvelope wraps a native creation payload for app.records.
func operationEnvelope(payload string) *protocol.Envelope {
	definition := tablecreate.Model()
	return &protocol.Envelope{ProviderId: schema.ProviderID, Kind: tablecreate.OperationKind, Owner: proto.CloneOf(owner()), SchemaVersion: 1,
		Encoding: definition.Encoding, DefinitionSha256: definition.DefinitionSha256, Payload: []byte(payload)}
}
