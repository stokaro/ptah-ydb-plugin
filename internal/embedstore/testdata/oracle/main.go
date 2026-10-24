// Command ydbembedoracle records what the Ptah host's embedded YDB engine
// (internal/embedydb) answers over the provider's embedding corpus, so the
// provider's embedding store can be compared with it without importing host
// code: the run-state tables it creates on 25.1 and on 26.2, the column and
// index a specification needs or the reason YDB cannot hold it, and the bytes
// a vector is stored as.
//
// It is not part of this module. Go ignores testdata, and the program imports
// host packages behind an internal boundary, so it is copied into a checkout
// of the host and run there:
//
//	mkdir -p <host>/cmd/ydbembedoracle
//	cp internal/embedstore/testdata/oracle/main.go <host>/cmd/ydbembedoracle/main.go
//	(cd <host> && HOST_COMMIT=$(git rev-parse HEAD) go run ./cmd/ydbembedoracle <plugin>/internal/embedstore/testdata)
//
// It reads targets.json and vectors.json and writes golden.json.
package main

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"ptah.run/core/platform/capability"
	"ptah.run/engine/builtin"
	"ptah.run/internal/embedgen"
	"ptah.run/internal/embedydb"
)

type relation struct {
	Schema         string            `json:"schema"`
	Table          string            `json:"table"`
	Layout         string            `json:"layout"`
	Representation string            `json:"representation"`
	Metric         string            `json:"metric"`
	IndexMethod    string            `json:"index_method"`
	IndexOptions   map[string]string `json:"index_options"`
}

type targetCase struct {
	Name      string   `json:"name"`
	Dimension int      `json:"dimension"`
	Chunked   bool     `json:"chunked"`
	Target    relation `json:"target"`
}

type objects struct {
	Name          string `json:"name"`
	Refused       bool   `json:"refused"`
	Message       string `json:"message,omitempty"`
	ColumnType    string `json:"column_type,omitempty"`
	OwnsTable     bool   `json:"owns_table,omitempty"`
	HasIndex      bool   `json:"has_index,omitempty"`
	IndexMethod   string `json:"index_method,omitempty"`
	IndexOperator string `json:"index_operator,omitempty"`
}

type golden struct {
	HostCommit string              `json:"host_commit"`
	Schema     map[string][]string `json:"schema"`
	Targets    []objects           `json:"targets"`
	Vectors    []string            `json:"vectors"`
}

func main() {
	if len(os.Args) != 2 || os.Getenv("HOST_COMMIT") == "" {
		fmt.Fprintln(os.Stderr, "usage: HOST_COMMIT=<commit> ydbembedoracle <testdata>")
		os.Exit(2)
	}
	if err := run(os.Args[1], os.Getenv("HOST_COMMIT")); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func readJSON(file string, target any) error {
	data, err := os.ReadFile(file)
	if err != nil {
		return err
	}
	return json.Unmarshal(data, target)
}

func run(dir, commit string) error {
	out := golden{HostCommit: commit, Schema: map[string][]string{}, Targets: []objects{}, Vectors: []string{}}
	service, err := builtin.New()
	if err != nil {
		return err
	}
	for line, caps := range map[string]capability.Capabilities{"25.1": capability.YDB251(), "26.2": capability.YDB262()} {
		statements, err := embedydb.SchemaSQL(context.Background(), service, caps)
		if err != nil {
			return err
		}
		out.Schema[line] = statements
	}
	var cases []targetCase
	if err := readJSON(filepath.Join(dir, "targets.json"), &cases); err != nil {
		return err
	}
	for _, test := range cases {
		out.Targets = append(out.Targets, describe(test))
	}
	var vectors [][]float32
	if err := readJSON(filepath.Join(dir, "vectors.json"), &vectors); err != nil {
		return err
	}
	for _, vector := range vectors {
		encoded, err := embedydb.EncodeVector(vector)
		if err != nil {
			return err
		}
		out.Vectors = append(out.Vectors, hex.EncodeToString(encoded))
	}
	encoded, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "golden.json"), append(encoded, '\n'), 0o644)
}

// describe is what the host derives for one specification. Every case reads
// the table app/articles, keyed by id, with a title.
func describe(test targetCase) objects {
	representation := test.Target.Representation
	if representation == "" {
		representation = "float"
	}
	table := test.Target.Table
	if table == "" {
		table = "articles"
	}
	schema := test.Target.Schema
	if schema == "" {
		schema = "app"
	}
	spec := embedgen.Spec{
		Source: embedgen.Source{Schema: "app", Table: "articles", KeyFields: []string{"id"}, InputFields: []string{"title"}},
		Model:  embedgen.Model{Identifier: "model", Revision: "1", ReportedDimension: test.Dimension},
		Target: embedgen.Target{Schema: schema, Table: table, Column: "embedding", Representation: representation,
			Metric: embedgen.DistanceMetric(test.Target.Metric), IndexMethod: test.Target.IndexMethod, IndexOptions: test.Target.IndexOptions,
			Layout: embedgen.TargetLayout(test.Target.Layout)},
	}
	if test.Chunked {
		spec.Preprocessing.Truncate = embedgen.TruncateChunk
	}
	found, err := embedydb.TargetObjects(spec)
	if err != nil {
		return objects{Name: test.Name, Refused: true, Message: err.Error()}
	}
	result := objects{Name: test.Name, ColumnType: found.Column.Type, OwnsTable: found.OwnsTable, HasIndex: found.HasIndex}
	if found.HasIndex {
		result.IndexMethod, result.IndexOperator = found.Index.Type, found.Index.Operator
	}
	return result
}
