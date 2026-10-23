package schemavalidation_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	qt "github.com/frankban/quicktest"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcomparison"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
)

// The corpus holds declarations and what the Ptah host's bundled YDB schema
// validation made of each, recorded by testdata/oracle. Each input is lowered
// here to the captured request the host would send this provider, so the
// provider's service and the bundled validation answer the same question
// without this module importing host code.
//
// A file name says what the row asserts:
//
//	refused-*   the host refuses the input, and so does this provider, with the same code
//	deferred-*  the host accepts the input and this provider refuses it, because
//	            the declaration needs something the provider does not carry yet
//	any other   the host accepts the input, and so does this provider; where the host
//	            reports a declaration it would skip, so does this provider

type corpusInput struct {
	Tables []struct {
		Schema     string   `json:"schema"`
		Name       string   `json:"name"`
		PrimaryKey []string `json:"primary_key"`
		Columns    []struct {
			Name     string `json:"name"`
			Type     string `json:"type"`
			Nullable bool   `json:"nullable"`
			Primary  bool   `json:"primary"`
		} `json:"columns"`
	} `json:"tables"`
	Indexes []struct {
		Table  string   `json:"table"`
		Name   string   `json:"name"`
		Fields []string `json:"fields"`
		Unique bool     `json:"unique"`
	} `json:"indexes"`
	Namespaces []struct {
		Name    string `json:"name"`
		Comment string `json:"comment"`
		Charset string `json:"charset"`
		Collate string `json:"collate"`
	} `json:"namespaces"`
	Enums []struct {
		Name   string   `json:"name"`
		Values []string `json:"values"`
	} `json:"enums"`
}

type corpusGolden struct {
	Host      string `json:"host"`
	Default   []struct{ Code string }
	NoSkipped []struct{ Code string } `json:"no_skipped"`
}

type corpusRow struct {
	name   string
	golden corpusGolden
	// request is lowered for the validation, with nothing-skipped set as asked.
	request func(noSkipped bool) *protocol.ValidateSchemaRequest
}

func readCorpus(c *qt.C, keep func(name string) bool) []corpusRow {
	c.Helper()
	paths, err := filepath.Glob(filepath.Join("testdata", "corpus", "*.golden.json"))
	c.Assert(err, qt.IsNil)
	var rows []corpusRow
	for _, path := range paths {
		name := strings.TrimSuffix(filepath.Base(path), ".golden.json")
		if !keep(name) {
			continue
		}
		raw, err := os.ReadFile(strings.TrimSuffix(path, ".golden.json") + ".json")
		c.Assert(err, qt.IsNil)
		var input corpusInput
		c.Assert(json.Unmarshal(raw, &input), qt.IsNil)
		recorded, err := os.ReadFile(path)
		c.Assert(err, qt.IsNil)
		var golden corpusGolden
		c.Assert(json.Unmarshal(recorded, &golden), qt.IsNil)
		rows = append(rows, corpusRow{name: name, golden: golden, request: func(noSkipped bool) *protocol.ValidateSchemaRequest {
			return lower(input, noSkipped)
		}})
	}
	return rows
}

func lower(input corpusInput, noSkipped bool) *protocol.ValidateSchemaRequest {
	request := newRequest()
	request.NoSkipped = noSkipped
	for _, declared := range input.Tables {
		columns := make([]*protocol.DeclaredColumn, 0, len(declared.Columns))
		for _, column := range declared.Columns {
			columns = append(columns, &protocol.DeclaredColumn{Name: column.Name, Type: column.Type, Nullable: column.Nullable, Primary: column.Primary})
		}
		converted := table(declared.Schema, declared.Name, columns...)
		converted.Declaration.PrimaryKey = declared.PrimaryKey
		for _, index := range input.Indexes {
			if index.Table == declared.Name {
				converted.Declaration.Indexes = append(converted.Declaration.Indexes, &protocol.DeclaredIndex{Name: index.Name, Fields: index.Fields, Unique: index.Unique, TableName: index.Table})
			}
		}
		request.Tables = append(request.Tables, converted)
	}
	for _, namespace := range input.Namespaces {
		request.Namespaces = append(request.Namespaces, &protocol.DeclaredNamespace{Subject: &protocol.ComparisonIdentity{Kind: "schema", Name: testcomparison.Part(namespace.Name)},
			Name: namespace.Name, Comment: namespace.Comment, Charset: namespace.Charset, Collation: namespace.Collate})
	}
	for _, enum := range input.Enums {
		request.Enums = append(request.Enums, &protocol.DeclaredEnum{Name: enum.Name, Values: enum.Values})
	}
	return request
}

// codes names the diagnostics in the host's spelling, sorted.
func codes(diagnostics []*protocol.SchemaDiagnostic) []string {
	names := map[protocol.SchemaDiagnostic_Code]string{
		protocol.SchemaDiagnostic_INVALID_SCHEMA:      "invalid-schema",
		protocol.SchemaDiagnostic_UNSUPPORTED_FEATURE: "unsupported-feature",
		protocol.SchemaDiagnostic_OMITTED_DECLARATION: "omitted-declaration",
	}
	result := []string{}
	for _, diagnostic := range diagnostics {
		result = append(result, names[diagnostic.Code])
	}
	slices.Sort(result)
	return result
}

func hostCodes(recorded []struct{ Code string }) []string {
	result := []string{}
	for _, diagnostic := range recorded {
		result = append(result, diagnostic.Code)
	}
	slices.Sort(result)
	return result
}

func isRefused(name string) bool  { return strings.HasPrefix(name, "refused-") }
func isDeferred(name string) bool { return strings.HasPrefix(name, "deferred-") }
func isAgreed(name string) bool   { return !isRefused(name) && !isDeferred(name) }

func TestCorpus_AgreesWithTheHostWhereItAccepts(t *testing.T) {
	c := qt.New(t)
	rows := readCorpus(c, isAgreed)
	c.Assert(len(rows) > 0, qt.IsTrue, qt.Commentf("the corpus holds no agreement rows"))
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			c := qt.New(t)
			c.Assert(codes(diagnostics(c, row.request(false))), qt.DeepEquals, hostCodes(row.golden.Default))
			c.Assert(codes(diagnostics(c, row.request(true))), qt.DeepEquals, hostCodes(row.golden.NoSkipped))
		})
	}
}

func TestCorpus_RefusesWhatTheHostRefuses(t *testing.T) {
	c := qt.New(t)
	rows := readCorpus(c, isRefused)
	c.Assert(len(rows) > 0, qt.IsTrue, qt.Commentf("the corpus holds no refusal rows"))
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			c := qt.New(t)
			c.Assert(hostCodes(row.golden.Default), qt.Not(qt.HasLen), 0)
			c.Assert(codes(diagnostics(c, row.request(false))), qt.DeepEquals, hostCodes(row.golden.Default))
			c.Assert(codes(diagnostics(c, row.request(true))), qt.DeepEquals, hostCodes(row.golden.NoSkipped))
		})
	}
}

// A deferred row pins a stricter answer: the host's validation would accept the
// declaration and a later stage would refuse it, so the provider says so now.
func TestCorpus_RefusesWhatOnlyALaterHostStageRefuses(t *testing.T) {
	c := qt.New(t)
	rows := readCorpus(c, isDeferred)
	c.Assert(len(rows) > 0, qt.IsTrue, qt.Commentf("the corpus holds no deferred rows"))
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			c := qt.New(t)
			c.Assert(hostCodes(row.golden.Default), qt.HasLen, 0)
			c.Assert(hostCodes(row.golden.NoSkipped), qt.HasLen, 0)
			refused := append(codes(diagnostics(c, row.request(false))), codes(diagnostics(c, row.request(true)))...)
			c.Assert(refused, qt.Not(qt.HasLen), 0)
		})
	}
}

func TestCorpus_CoversTheHostCommit(t *testing.T) {
	c := qt.New(t)
	hosts := map[string]bool{}
	for _, row := range readCorpus(c, func(string) bool { return true }) {
		hosts[row.golden.Host] = true
	}
	c.Assert(hosts, qt.HasLen, 1, qt.Commentf("every golden file comes from one host commit"))
}
