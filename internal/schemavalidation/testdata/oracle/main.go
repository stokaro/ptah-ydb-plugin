// Command ydbvalidationoracle records what the Ptah host's bundled YDB schema
// validation makes of each input in the corpus, so the provider's validation
// service can be compared with it without importing host code.
//
// It is not part of this module. Go ignores testdata, and the program imports
// host packages that sit behind an internal boundary, so it has to be copied
// into a checkout of the host and run from there:
//
//	cp internal/schemavalidation/testdata/oracle/main.go <host>/cmd/ydbvalidationoracle/main.go
//	(cd <host> && HOST_COMMIT=<sha> go run ./cmd/ydbvalidationoracle <plugin>/internal/schemavalidation/testdata/corpus)
//
// For every *.json input in the corpus it writes a file of the same name with
// the suffix .golden.json. The golden file holds the host commit, whether the
// bundled validation accepted the declaration, and, when it refused, whether
// it called the refusal unsupported or invalid, with the host's message.
//
// An input declares tables, indexes, SQL namespaces and enums in the form
// below. The program lowers it to the host's schema model and validates it for
// the ydb target with the target's default capabilities.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"ptah.run/core/platform/capability"
	"ptah.run/core/schemamodel"
	"ptah.run/core/schemavalidation"
	"ptah.run/engine/builtin"
)

type column struct {
	Name     string `json:"name"`
	Type     string `json:"type"`
	Nullable bool   `json:"nullable"`
	Primary  bool   `json:"primary"`
}

type table struct {
	Schema     string   `json:"schema"`
	Name       string   `json:"name"`
	PrimaryKey []string `json:"primary_key"`
	Columns    []column `json:"columns"`
}

type index struct {
	Table  string   `json:"table"`
	Name   string   `json:"name"`
	Fields []string `json:"fields"`
	Unique bool     `json:"unique"`
}

type namespace struct {
	Name    string `json:"name"`
	Comment string `json:"comment"`
	Charset string `json:"charset"`
	Collate string `json:"collate"`
}

type enum struct {
	Name   string   `json:"name"`
	Values []string `json:"values"`
}

type input struct {
	Tables     []table     `json:"tables"`
	Indexes    []index     `json:"indexes"`
	Namespaces []namespace `json:"namespaces"`
	Enums      []enum      `json:"enums"`
}

type diagnostic struct {
	Code    string `json:"code"`
	Kind    string `json:"kind"`
	Object  string `json:"object,omitempty"`
	Message string `json:"message"`
}

// golden holds the diagnostics of a validation, and of the same validation
// with NoSkipped, which also reports what the target would omit.
type golden struct {
	Host      string       `json:"host"`
	Default   []diagnostic `json:"default"`
	NoSkipped []diagnostic `json:"no_skipped"`
}

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: ydbvalidationoracle <corpus directory>")
		os.Exit(2)
	}
	entries, err := os.ReadDir(os.Args[1])
	check(err)
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasSuffix(name, ".json") || strings.HasSuffix(name, ".golden.json") {
			continue
		}
		path := filepath.Join(os.Args[1], name)
		raw, err := os.ReadFile(path)
		check(err)
		var declared input
		check(json.Unmarshal(raw, &declared))
		record := golden{Host: os.Getenv("HOST_COMMIT"), Default: validate(declared, false), NoSkipped: validate(declared, true)}
		encoded, err := json.MarshalIndent(record, "", "  ")
		check(err)
		check(os.WriteFile(strings.TrimSuffix(path, ".json")+".golden.json", append(encoded, '\n'), 0o644))
	}
}

func validate(declared input, noSkipped bool) []diagnostic {
	runtime, err := builtin.New()
	check(err)
	result, err := runtime.ValidateSchema(context.Background(), schemavalidation.Request{
		Target: "ydb", Schema: lower(declared), Capabilities: capability.ForDialect("ydb"), NoSkipped: noSkipped,
	})
	if err != nil {
		// The host reports a refusal it cannot express as a diagnostic as an error.
		return []diagnostic{{Code: "error", Kind: "schema", Message: err.Error()}}
	}
	diagnostics := []diagnostic{}
	for _, d := range result.Diagnostics {
		diagnostics = append(diagnostics, diagnostic{Code: string(d.Code), Kind: d.Kind, Object: d.Object, Message: d.Message})
	}
	return diagnostics
}

func lower(declared input) *schemamodel.Database {
	database := &schemamodel.Database{}
	for _, t := range declared.Tables {
		database.Tables = append(database.Tables, schemamodel.Table{StructName: t.Name, Schema: t.Schema, Name: t.Name, PrimaryKey: t.PrimaryKey})
		for _, c := range t.Columns {
			database.Fields = append(database.Fields, schemamodel.Field{StructName: t.Name, Name: c.Name, Type: c.Type, Nullable: c.Nullable, Primary: c.Primary})
		}
	}
	for _, i := range declared.Indexes {
		database.Indexes = append(database.Indexes, schemamodel.Index{StructName: i.Table, TableName: i.Table, Name: i.Name, Fields: i.Fields, Unique: i.Unique})
	}
	for _, n := range declared.Namespaces {
		database.Schemas = append(database.Schemas, schemamodel.Schema{Name: n.Name, Comment: n.Comment, Charset: n.Charset, Collate: n.Collate})
	}
	for _, e := range declared.Enums {
		database.Enums = append(database.Enums, schemamodel.Enum{Name: e.Name, Values: e.Values})
	}
	schemamodel.Finalize(database)
	return database
}

func check(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
