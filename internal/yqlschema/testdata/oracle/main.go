// Command ydbsqlschemaoracle records what the Ptah host's bundled YDB owner
// (internal/ydbsql, with internal/yqlparse and the shared conversion it calls)
// reads from each load of YQL schema documents in the corpus, so the
// provider's ReadSQLSchema can be compared with it without importing host
// code.
//
// It is not part of this module. Go ignores testdata, and the program imports
// host packages behind an internal boundary, so it is copied into a checkout
// of the host and run from there:
//
//	mkdir -p <host>/cmd/ydbsqlschemaoracle
//	cp internal/yqlschema/testdata/oracle/main.go <host>/cmd/ydbsqlschemaoracle/main.go
//	(cd <host> && HOST_COMMIT=$(git rev-parse HEAD) go run ./cmd/ydbsqlschemaoracle <plugin>/internal/yqlschema/testdata/corpus)
//
// A case is a directory <corpus>/<case>/ whose .sql files are the documents
// of one load, in name order, and an optional <corpus>/<case>.load.json that
// names the database root the load's absolute paths resolve against. The
// program reads each load twice: through the owner's grammar alone, which is
// the call a provider's ReadSQLSchema replaces, and through
// sqlschema.ReadLoad, the code `ptah schema plan --schema-file` and a schema
// directory of .sql files run, which adds the coverage the documents claim.
// It writes <corpus>/<case>.golden.json with both answers. Feature objects,
// facets and coverage are written with the bundled runtime's codecs, which is
// the form the host's plan fingerprint takes.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"ptah.run/core/coverage"

	"ptah.run/core/schemaext"
	"ptah.run/core/sqlext"
	"ptah.run/engine/builtin"
	"ptah.run/internal/sqlschema"
	"ptah.run/internal/ydbsql"
)

type loadSpec struct {
	DatabasePath string `json:"database_path"`
}

type answer struct {
	Outcome string `json:"outcome"`
	Error   string `json:"error,omitempty"`
}

type facets struct {
	// Owner names the table, index or column the facets belong to.
	Owner  string                   `json:"owner"`
	Facets []schemaext.EncodedFacet `json:"facets"`
}

// grammarAnswer is the owner's grammar's reading: what a provider's
// ReadSQLSchema replaces.
type grammarAnswer struct {
	answer
	Database       json.RawMessage           `json:"database,omitempty"`
	FeatureObjects []schemaext.EncodedObject `json:"feature_objects,omitempty"`
	TableFacets    []facets                  `json:"table_facets,omitempty"`
	FieldFacets    []facets                  `json:"field_facets,omitempty"`
	IndexFacets    []facets                  `json:"index_facets,omitempty"`
	Files          []sqlext.FileDeclarations `json:"files,omitempty"`
}

// loadAnswer is what sqlschema.ReadLoad makes of the load: whether the
// reader accepts it once the grammar has read it, and the coverage the
// documents claim, which a provider returns with its reading.
type loadAnswer struct {
	answer
	Coverage *schemaext.CoverageDocument `json:"coverage,omitempty"`
}

// limit is one entry of a document's `ptah:not-described` header of a kind
// the YDB owner reads, as a provider-selected run sends it.
type limit struct {
	Kind       string `json:"kind"`
	Name       string `json:"name"`
	Reason     string `json:"reason,omitempty"`
	Provenance string `json:"provenance,omitempty"`
}

type golden struct {
	Host         string        `json:"host"`
	DatabasePath string        `json:"database_path,omitempty"`
	Documents    []string      `json:"documents"`
	Limits       []limit       `json:"limits,omitempty"`
	LimitsError  string        `json:"limits_error,omitempty"`
	Grammar      grammarAnswer `json:"grammar"`
	Load         loadAnswer    `json:"load"`
}

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: ydbsqlschemaoracle <corpus directory>")
		os.Exit(2)
	}
	host := os.Getenv("HOST_COMMIT")
	corpus := os.Args[1]
	entries, err := os.ReadDir(corpus)
	check(err)
	ctx := context.Background()
	runtime, err := builtin.Bundled()
	check(err)
	owners, err := sqlext.NewSet(ydbsql.SQL())
	check(err)
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		name := entry.Name()
		request := sqlext.GrammarRequest{Target: "ydb"}
		if data, err := os.ReadFile(filepath.Join(corpus, name+".load.json")); err == nil {
			var spec loadSpec
			check(json.Unmarshal(data, &spec))
			request.DatabasePath = spec.DatabasePath
		}
		documents, err := os.ReadDir(filepath.Join(corpus, name))
		check(err)
		result := golden{Host: host, DatabasePath: request.DatabasePath}
		for _, document := range documents {
			if document.IsDir() || !strings.HasSuffix(document.Name(), ".sql") {
				continue
			}
			text, err := os.ReadFile(filepath.Join(corpus, name, document.Name()))
			check(err)
			request.Files = append(request.Files, sqlext.SourceFile{Path: document.Name(), Text: string(text)})
			result.Documents = append(result.Documents, document.Name())
		}
		if len(request.Files) == 0 {
			fmt.Fprintf(os.Stderr, "%s: no documents\n", name)
			os.Exit(1)
		}
		result.Limits, result.LimitsError = headerLimits(request.Files)
		read, err := ydbsql.SQL().Grammar.ReadSQL(ctx, request)
		if err != nil {
			result.Grammar.answer = answer{Outcome: "refused", Error: err.Error()}
		} else {
			result.Grammar = record(ctx, runtime.Codecs(), read)
		}
		load, err := sqlschema.ReadLoad(ctx, request, sqlschema.Options{Owners: owners})
		if err != nil {
			result.Load.answer = answer{Outcome: "refused", Error: err.Error()}
		} else {
			result.Load.answer = answer{Outcome: "accepted"}
			document, err := runtime.Codecs().EncodeCoverage(ctx, schemaext.Desired, load.Database.FeatureCoverage)
			check(err)
			result.Load.Coverage = &document
		}
		encoded, err := json.MarshalIndent(result, "", " ")
		check(err)
		check(os.WriteFile(filepath.Join(corpus, name+".golden.json"), append(encoded, '\n'), 0o644))
	}
}

func record(ctx context.Context, codecs schemaext.Registry, read sqlext.GrammarResult) grammarAnswer {
	database := read.Database
	result := grammarAnswer{answer: answer{Outcome: "accepted"}, Files: read.Files}
	if !read.Coverage.IsZero() || !database.FeatureCoverage.IsZero() || !database.NotDescribed.IsZero() {
		fmt.Fprintln(os.Stderr, "the grammar claims coverage")
		os.Exit(1)
	}
	var err error
	result.FeatureObjects, err = codecs.EncodeObjects(ctx, schemaext.Desired, database.FeatureObjects)
	check(err)
	database.FeatureObjects = schemaext.Objects{}
	if !database.Facets.IsZero() {
		fmt.Fprintln(os.Stderr, "database facets are not recorded")
		os.Exit(1)
	}
	for i := range database.Tables {
		table := &database.Tables[i]
		if !table.Facets.IsZero() {
			encoded, err := codecs.EncodeFacets(ctx, schemaext.Desired, table.Facets)
			check(err)
			result.TableFacets = append(result.TableFacets, facets{Owner: table.StructName, Facets: encoded})
		}
		table.Facets = schemaext.Facets{}
	}
	for i := range database.Fields {
		field := &database.Fields[i]
		if !field.Facets.IsZero() {
			encoded, err := codecs.EncodeFacets(ctx, schemaext.Desired, field.Facets)
			check(err)
			result.FieldFacets = append(result.FieldFacets, facets{Owner: field.StructName + "." + field.Name, Facets: encoded})
		}
		field.Facets = schemaext.Facets{}
	}
	for i := range database.Indexes {
		index := &database.Indexes[i]
		if !index.Facets.IsZero() {
			encoded, err := codecs.EncodeFacets(ctx, schemaext.Desired, index.Facets)
			check(err)
			result.IndexFacets = append(result.IndexFacets, facets{Owner: index.StructName + "." + index.Name, Facets: encoded})
		}
		index.Facets = schemaext.Facets{}
	}
	for i := range database.Schemas {
		if !database.Schemas[i].Facets.IsZero() {
			fmt.Fprintln(os.Stderr, "schema facets are not recorded")
			os.Exit(1)
		}
	}
	encoded, err := json.Marshal(database)
	check(err)
	result.Database = encoded
	return result
}

// headerLimits are the entries of each document's header whose kind the YDB
// owner reads, in written order, as a provider-selected run sends them with
// the documents: the same reading of the header internal/providerhost makes.
func headerLimits(files []sqlext.SourceFile) ([]limit, string) {
	var kinds []string
	for _, kind := range ydbsql.SQL().Header {
		kinds = append(kinds, string(kind))
	}
	var limits []limit
	for _, file := range files {
		_, err := coverage.DecodeHeader(file.Text, coverage.Vocabulary{}, func(object coverage.Object) (bool, error) {
			index := slices.IndexFunc(kinds, func(kind string) bool { return strings.EqualFold(kind, strings.TrimSpace(string(object.Kind))) })
			if index >= 0 {
				limits = append(limits, limit{Kind: kinds[index], Name: object.Name, Reason: string(object.Reason), Provenance: string(object.Provenance)})
			}
			return true, nil
		})
		if err != nil {
			return nil, file.Path + ": " + err.Error()
		}
	}
	return limits, ""
}

func check(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
