package yqlschema_test

import (
	"encoding/json"
	"strings"

	qt "github.com/frankban/quicktest"
	"github.com/stokaro/ptah-ydb-plugin/internal/yqlschema"
)

// meaning is a reading in a form both readings write alike: each common
// object as the JSON object the host's model writes, with every field that
// holds no value and every field the host derives or keys internally left
// out, the columns and indexes of a table named by the table's qualified
// name, the YDB objects by identity with the payload the host's codec writes,
// and the settings of each table and index by kind.
type meaning struct {
	Tables        []map[string]any
	Fields        []map[string]any
	Indexes       []map[string]any
	Views         []map[string]any
	Roles         []map[string]any
	Grants        []map[string]any
	RevokedGrants []map[string]any
	Objects       []objectMeaning
	TableFacets   map[string]map[string]any
	IndexFacets   map[string]map[string]any
	Files         []fileMeaning
}

type objectMeaning struct {
	Kind, Schema, Parent, Name string
	Payload                    any
}

type fileMeaning struct {
	Objects []yqlschema.Declaration
	Guards  []yqlschema.Declaration
	Tables  []yqlschema.TableStatement
}

// internalFields are the fields of the host's model that key a table to its
// columns inside the host or that the host derives from the rest, so neither
// reading is compared on them.
var internalFields = map[string]bool{
	"StructName": true, "FieldName": true, "APIName": true, "APINames": true, "APIType": true,
	"GeneratedFromEmbedded": true, "Overrides": true,
}

// element decodes one JSON object and keeps the fields that hold a value.
func element(c *qt.C, data []byte) map[string]any {
	c.Helper()
	var object map[string]any
	c.Assert(json.Unmarshal(data, &object), qt.IsNil)
	for key, value := range object {
		if internalFields[key] || empty(value) {
			delete(object, key)
		}
	}
	return object
}

// empty reports a JSON value that holds nothing: null, false, zero, an empty
// string, array or object.
func empty(value any) bool {
	switch typed := value.(type) {
	case nil:
		return true
	case bool:
		return !typed
	case float64:
		return typed == 0
	case string:
		return typed == ""
	case []any:
		return len(typed) == 0
	case map[string]any:
		return len(typed) == 0
	default:
		return false
	}
}

func decodeAny(c *qt.C, data []byte) any {
	c.Helper()
	var value any
	c.Assert(json.Unmarshal(data, &value), qt.IsNil)
	return value
}

func marshal(c *qt.C, value any) []byte {
	c.Helper()
	data, err := json.Marshal(value)
	c.Assert(err, qt.IsNil)
	return data
}

// hostMeaning is the meaning of the host's grammar's reading.
func hostMeaning(c *qt.C, recorded goldenGrammar) meaning {
	c.Helper()
	result := meaning{TableFacets: map[string]map[string]any{}, IndexFacets: map[string]map[string]any{}}
	list := func(key string) []json.RawMessage {
		var values []json.RawMessage
		if raw, ok := recorded.Database[key]; ok {
			c.Assert(json.Unmarshal(raw, &values), qt.IsNil)
		}
		return values
	}
	tables := map[string]string{}
	for _, raw := range list("Tables") {
		var table struct{ StructName, Schema, Name string }
		c.Assert(json.Unmarshal(raw, &table), qt.IsNil)
		qualified := canonical(table.Schema, table.Name)
		tables[table.StructName] = qualified
		result.Tables = append(result.Tables, element(c, raw))
	}
	for _, raw := range list("Fields") {
		var field struct{ StructName string }
		c.Assert(json.Unmarshal(raw, &field), qt.IsNil)
		object := element(c, raw)
		object["Table"] = tables[field.StructName]
		result.Fields = append(result.Fields, object)
	}
	indexes := map[string]string{}
	for _, raw := range list("Indexes") {
		var index struct{ StructName, Name, TableName string }
		c.Assert(json.Unmarshal(raw, &index), qt.IsNil)
		indexes[index.StructName+"."+index.Name] = index.TableName + "\x00" + index.Name
		result.Indexes = append(result.Indexes, element(c, raw))
	}
	for key, target := range map[string]*[]map[string]any{"Views": &result.Views, "Roles": &result.Roles, "Grants": &result.Grants, "RevokedGrants": &result.RevokedGrants} {
		for _, raw := range list(key) {
			*target = append(*target, element(c, raw))
		}
	}
	for _, object := range recorded.FeatureObjects {
		meaning := objectMeaning{Kind: object.Ref.Kind, Name: object.Ref.Name.Source, Payload: decodeAny(c, object.Value.Payload)}
		meaning.Schema = partSource(object.Ref.Schema)
		meaning.Parent = partSource(object.Ref.Parent)
		result.Objects = append(result.Objects, meaning)
	}
	for _, owner := range recorded.TableFacets {
		result.TableFacets[tables[owner.Owner]] = facetMeaning(c, owner.Facets)
	}
	for _, owner := range recorded.IndexFacets {
		result.IndexFacets[indexes[owner.Owner]] = facetMeaning(c, owner.Facets)
	}
	c.Assert(recorded.FieldFacets, qt.HasLen, 0, qt.Commentf("a column facet has no meaning here yet"))
	for _, file := range recorded.Files {
		result.Files = append(result.Files, fileMeaning{Objects: file.Objects, Guards: file.Guards, Tables: file.Tables})
	}
	return result
}

func partSource(part *goldenPart) string {
	if part == nil {
		return ""
	}
	return part.Source
}

func facetMeaning(c *qt.C, facets []goldenFacet) map[string]any {
	c.Helper()
	result := map[string]any{}
	for _, facet := range facets {
		c.Assert(facet.Value, qt.IsNotNil, qt.Commentf("an excluded facet of kind %s", facet.Kind))
		result[facet.Kind] = decodeAny(c, facet.Value.Payload)
	}
	return result
}

// canonical writes a table's qualified name as the host's model does.
func canonical(schema, name string) string {
	part := func(value string) string {
		if !strings.ContainsAny(value, ".\"`[]") {
			return value
		}
		return `"` + strings.ReplaceAll(value, `"`, `""`) + `"`
	}
	if schema == "" {
		return part(name)
	}
	return part(schema) + "." + part(name)
}

// pluginMeaning is the meaning of this provider's reading.
func pluginMeaning(c *qt.C, reading *yqlschema.Reading) meaning {
	c.Helper()
	result := meaning{TableFacets: map[string]map[string]any{}, IndexFacets: map[string]map[string]any{}}
	for _, table := range reading.Tables {
		result.Tables = append(result.Tables, element(c, marshal(c, table)))
		facets := yqlschema.TableFacetPayloads(table)
		if len(facets) > 0 {
			result.TableFacets[table.Qualified()] = jsonValues(c, facets)
		}
	}
	for _, field := range reading.Fields {
		object := element(c, marshal(c, field))
		object["Table"] = field.Table
		result.Fields = append(result.Fields, object)
	}
	for _, index := range reading.Indexes {
		result.Indexes = append(result.Indexes, element(c, marshal(c, index)))
		facets := yqlschema.IndexFacetPayloads(index)
		if len(facets) > 0 {
			result.IndexFacets[index.TableName+"\x00"+index.Name] = jsonValues(c, facets)
		}
	}
	for _, view := range reading.Views {
		result.Views = append(result.Views, element(c, marshal(c, view)))
	}
	for _, role := range reading.Roles {
		result.Roles = append(result.Roles, element(c, marshal(c, role)))
	}
	for _, grant := range reading.Grants {
		result.Grants = append(result.Grants, element(c, marshal(c, grant)))
	}
	for _, grant := range reading.RevokedGrants {
		result.RevokedGrants = append(result.RevokedGrants, element(c, marshal(c, grant)))
	}
	for _, object := range yqlschema.SortedObjects(reading) {
		result.Objects = append(result.Objects, objectMeaning{Kind: string(object.Kind), Schema: object.Schema, Parent: object.Parent, Name: object.Name,
			Payload: decodeAny(c, marshal(c, yqlschema.ObjectPayload(object)))})
	}
	for _, file := range reading.Files {
		result.Files = append(result.Files, fileMeaning{Objects: file.Objects, Guards: file.Guards, Tables: file.Tables})
	}
	return result
}

func jsonValues(c *qt.C, values map[string]any) map[string]any {
	c.Helper()
	result := map[string]any{}
	for kind, value := range values {
		result[kind] = decodeAny(c, marshal(c, value))
	}
	return result
}
