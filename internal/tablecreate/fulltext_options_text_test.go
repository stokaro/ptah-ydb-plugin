package tablecreate_test

import (
	"maps"
	"testing"

	qt "github.com/frankban/quicktest"

	"github.com/stokaro/ptah-ydb-plugin/internal/backend"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
	"github.com/stokaro/ptah-ydb-plugin/internal/testsource"
	"github.com/stokaro/ptah-ydb-plugin/schema"
)

// decodedIndexSet decodes one ptah:schema:index directive of the Item struct
// through the provider's source service, with the attributes given, and
// returns the payload of the index set it declares.
func decodedIndexSet(c *qt.C, attributes map[string]string) string {
	c.Helper()
	request, err := testsource.GoDirectives([]testsource.GoTable{{Name: "items", Struct: "Item"}}, []testsource.GoDeclaration{{Directive: "ptah:schema:index",
		Attributes: attributes, Struct: "Item", File: "entities.go", FileSize: 1000, Start: testsource.GoPosition{Offset: 100, Line: 2, Column: 1},
		End: testsource.GoPosition{Offset: 140, Line: 2, Column: 41}, Subject: testsource.GoSubject{Kind: "index", Name: attributes["name"]}}})
	c.Assert(err, qt.IsNil)
	decoded, err := backend.New().DecodeSource(c.Context(), request)
	c.Assert(err, qt.IsNil)
	var sets []string
	for _, fragment := range decoded.Documents[0].Fragments {
		for _, value := range fragment.Values {
			if value.Value.Value.Kind == schema.IndexProperties {
				sets = append(sets, string(value.Value.Value.Payload))
			}
		}
	}
	c.Assert(sets, qt.HasLen, 1)
	return sets[0]
}

// A full-text index declared in a Go source is added with its options in the
// WITH clause as YDB reads them: a number written with a leading zero as its
// decimal value, and the language as a YQL string, a quote and a backslash in
// it escaped.
//
// Ported from the host's TestFullTextOptions.
func TestFullTextOptionsText_HappyPath(t *testing.T) {
	for _, test := range []struct {
		name    string
		options map[string]string
		with    string
	}{
		{"a leading-zero number and a quote in the language", map[string]string{"filter_ngram_min_length": "03", "language": "en'glish"},
			`filter_ngram_min_length=3, language='en\'glish', tokenizer=standard, use_filter_lowercase=true, use_filter_snowball=true`},
		{"a leading zero an octal reading would change", map[string]string{"filter_ngram_min_length": "010", "language": "english"},
			`filter_ngram_min_length=10, language='english', tokenizer=standard, use_filter_lowercase=true, use_filter_snowball=true`},
		{"a backslash in the language", map[string]string{"language": `en\glish`},
			`language='en\\glish', tokenizer=standard, use_filter_lowercase=true, use_filter_snowball=true`},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			attributes := map[string]string{"name": "ft", "fields": "body", "type": "fulltext_plain", "tokenizer": "standard",
				"use_filter_lowercase": "true", "use_filter_snowball": "true"}
			maps.Copy(attributes, test.options)
			request := alterRequest(c, documents(decodedIndexSet(c, attributes), indexSet()))
			plan, err := backend.New().PlanFeatures(c.Context(), request)
			c.Assert(err, qt.IsNil)
			c.Assert(plan.Diagnostics, qt.HasLen, 0)
			c.Assert(textUnits(c, plan, request.Capabilities), qt.DeepEquals,
				[]string{"ALTER TABLE `app/records` ADD INDEX `ft` GLOBAL USING fulltext_plain ON (`body`) WITH (" + test.with + ");"})
		})
	}
}
