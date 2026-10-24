// Command ydboracle records what the Ptah host's bundled YDB source decoder
// (internal/ydbsource) makes of each input in the corpus, so the provider's
// decoder can be compared with it without importing host code.
//
// It is not part of this module. Go ignores testdata, and the program imports
// host packages that sit behind an internal boundary, so it has to be copied
// into a checkout of the host and run from there:
//
//	cp internal/tablesource/testdata/oracle/main.go <host>/cmd/ydboracle/main.go
//	(cd <host> && go run ./cmd/ydboracle <plugin>/internal/tablesource/testdata/corpus)
//
// For every yaml-*.json and go-*.go in the corpus directory it writes a file
// of the same name with the suffix .golden.json. The golden file holds the
// host commit, whether the bundled decoder accepted the input, the facets it
// produced for each table, and, for a Go input, the declarations the host's
// frontend lexes when a provider claims the same directives and attributes,
// which the provider's test replays as captured fragments. Those declarations
// come from a second run of the frontend with an owner that claims what the
// provider claims and decodes nothing, so they carry exactly the attributes,
// positions and subjects the host sends.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"ptah.run/core/annotation"
	"ptah.run/core/coverage"
	"ptah.run/core/goschema"
	"ptah.run/core/schemaext"
	"ptah.run/core/schemamodel"
	"ptah.run/core/schemaproperties"
	"ptah.run/core/yamlext"
	"ptah.run/core/yamlschema"
	"ptah.run/dialect/ydb/ydbschema"
	"ptah.run/engine/builtin"
	"ptah.run/internal/ydbsource"
)

type position struct {
	Offset int `json:"offset"`
	Line   int `json:"line"`
	Column int `json:"column"`
}

type declaration struct {
	Directive  string            `json:"directive"`
	Attributes map[string]string `json:"attributes"`
	Struct     string            `json:"struct"`
	File       string            `json:"file"`
	FileSize   int               `json:"file_size"`
	Start      position          `json:"start"`
	End        position          `json:"end"`
	Subject    *subject          `json:"subject,omitempty"`
}

type subject struct {
	Kind  string `json:"kind"`
	Name  string `json:"name"`
	Table string `json:"table,omitempty"`
}

type table struct {
	Schema  string                     `json:"schema"`
	Name    string                     `json:"name"`
	Struct  string                     `json:"struct,omitempty"`
	Facets  map[string]json.RawMessage `json:"facets"`
	Indexes []index                    `json:"indexes,omitempty"`
}

// index records what the bundled decoder made of the options an index
// directive writes: the facets it attached, and the WITH options it put on the
// common index.
type index struct {
	Name    string                     `json:"name"`
	Facets  map[string]json.RawMessage `json:"facets,omitempty"`
	Options map[string]string          `json:"options,omitempty"`
}

type golden struct {
	Host         string        `json:"host"`
	Format       string        `json:"format"`
	Outcome      string        `json:"outcome"`
	Error        string        `json:"error,omitempty"`
	Tables       []table       `json:"tables"`
	Declarations []declaration `json:"declarations,omitempty"`
}

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: ydboracle <corpus directory>")
		os.Exit(2)
	}
	host := os.Getenv("HOST_COMMIT")
	entries, err := os.ReadDir(os.Args[1])
	check(err)
	for _, entry := range entries {
		name := entry.Name()
		path := filepath.Join(os.Args[1], name)
		var result golden
		switch {
		case strings.HasSuffix(name, ".golden.json"):
			continue
		case strings.HasPrefix(name, "yaml-") && strings.HasSuffix(name, ".json"):
			result = yamlCase(path)
		case strings.HasPrefix(name, "go-") && strings.HasSuffix(name, ".go"):
			result = goCase(path)
		default:
			continue
		}
		result.Host = host
		encoded, err := json.MarshalIndent(result, "", " ")
		check(err)
		check(os.WriteFile(strings.TrimSuffix(path, filepath.Ext(path))+".golden.json", append(encoded, '\n'), 0o644))
	}
}

func check(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func yamlCase(path string) golden {
	data, err := os.ReadFile(path)
	check(err)
	owners, err := yamlext.NewSet(ydbsource.YAML())
	check(err)
	db, err := yamlschema.Parse(context.Background(), owners, data)
	if err == nil {
		db, err = decodeProperties(db)
	}
	result := golden{Format: "yaml"}
	if err != nil {
		result.Outcome, result.Error = "refused", err.Error()
		return result
	}
	result.Outcome = "accepted"
	result.Tables = tables(db)
	return result
}

func goCase(path string) golden {
	data, err := os.ReadFile(path)
	check(err)
	bundled, err := annotation.NewSet(ydbsource.Annotations())
	check(err)
	db, err := goschema.ParseSource(bundled, filepath.Base(path), string(data))
	if err == nil {
		var decoded *schemamodel.Database
		if decoded, err = decodeProperties(&db); err == nil {
			db = *decoded
		}
	}
	result := golden{Format: "go"}
	if err != nil {
		result.Outcome, result.Error = "refused", err.Error()
	} else {
		result.Outcome = "accepted"
	}
	// The declarations, and the tables of a file whose bundled parse refused it,
	// come from the frontend run with an owner that claims what the provider
	// claims. It decodes nothing, so it refuses nothing the provider would read.
	recorded := &recorder{}
	claims, err := annotation.NewSet(recordingOwner(recorded))
	check(err)
	probe, err := goschema.ParseSource(claims, filepath.Base(path), string(data))
	check(err)
	result.Declarations = recorded.declarations
	if result.Outcome == "accepted" {
		result.Tables = tables(&db)
		return result
	}
	result.Tables = tables(&probe)
	for i := range result.Tables {
		result.Tables[i].Facets = map[string]json.RawMessage{}
		result.Tables[i].Indexes = nil
	}
	return result
}

// recordingOwner claims the Go syntax the provider claims: the directives of a
// column family and a changefeed, and attributes of the table and index
// directives, the TTL properties of the ydb engine among them. Its file decoder
// records every declaration it is handed and returns nothing.
func recordingOwner(recorded *recorder) annotation.Extension {
	bundled := ydbsource.Annotations()
	own := map[string]bool{"ptah:schema:columnfamily": true, "ptah:schema:changefeed": true, "ptah:schema:changefeed:consumer": true}
	extension := annotation.Extension{
		Owner: "example.org/recording-owner",
		File:  func() annotation.FileDecoder { return recorded },
		Coverage: func([]coverage.Object) (schemaext.Coverage, error) {
			return schemaext.Coverage{}, nil
		},
	}
	for _, directive := range bundled.Directives {
		if own[directive.Name] {
			extension.Directives = append(extension.Directives, directive)
		}
	}
	for _, group := range bundled.Attributes {
		claim := annotation.DirectiveAttributes{Directive: group.Directive, Attributes: slices.Clone(group.Attributes), PerFile: true}
		if group.Directive == "ptah:schema:table" {
			for _, name := range []string{"column", "interval", "unit"} {
				claim.Attributes = append(claim.Attributes, annotation.Attribute{Name: "platform.ydb.row_deletion_" + name, Value: "string"})
			}
		}
		extension.Attributes = append(extension.Attributes, claim)
	}
	return extension
}

// decodeProperties moves the platform properties of the ydb target, the TTL
// among them, through the bundled owners, as the host does before it compares.
func decodeProperties(db *schemamodel.Database) (*schemamodel.Database, error) {
	runtime, err := builtin.Bundled()
	check(err)
	return schemaproperties.Decode(context.Background(), db, "ydb", runtime)
}

type recorder struct{ declarations []declaration }

func (r *recorder) Decode(d annotation.Declaration) ([]annotation.Contribution, error) {
	entry := declaration{
		Directive: d.Directive, Attributes: d.Attributes, Struct: d.Struct, File: d.File, FileSize: d.FileSize,
		Start: position{d.Start.Offset, d.Start.Line, d.Start.Column}, End: position{d.End.Offset, d.End.Line, d.End.Column},
	}
	if d.Subject.Kind != "" {
		entry.Subject = &subject{Kind: string(d.Subject.Kind), Name: d.Subject.Name, Table: d.Subject.Table}
	}
	r.declarations = append(r.declarations, entry)
	return nil, nil
}

func (r *recorder) Finish(annotation.Tables) ([]annotation.Contribution, error) { return nil, nil }

func tables(db *schemamodel.Database) []table {
	result := make([]table, 0, len(db.Tables))
	for _, t := range db.Tables {
		entry := table{Schema: t.Schema, Name: t.Name, Struct: t.StructName, Facets: map[string]json.RawMessage{}}
		if partitioning, found, err := schemaext.FacetAs[*ydbschema.DesiredTablePartitioning](t.Facets, ydbschema.TablePartitioningKind); err != nil {
			check(err)
		} else if found {
			entry.Facets[string(ydbschema.TablePartitioningKind)] = marshal(partitioning.TablePartitioning)
		}
		if families, found, err := schemaext.FacetAs[*ydbschema.DesiredColumnFamilies](t.Facets, ydbschema.ColumnFamiliesKind); err != nil {
			check(err)
		} else if found {
			entry.Facets[string(ydbschema.ColumnFamiliesKind)] = marshal(families)
		}
		if ttl, found, err := schemaext.FacetAs[*ydbschema.DesiredTTL](t.Facets, ydbschema.TTLKind); err != nil {
			check(err)
		} else if found {
			entry.Facets[string(ydbschema.TTLKind)] = marshal(ttl.Policy)
		}
		if store, found, err := schemaext.FacetAs[*ydbschema.DesiredColumnStore](t.Facets, ydbschema.ColumnStoreKind); err != nil {
			check(err)
		} else if found {
			entry.Facets[string(ydbschema.ColumnStoreKind)] = marshal(store.ColumnStore)
		}
		for _, kind := range t.Facets.Kinds() {
			if kind != ydbschema.TablePartitioningKind && kind != ydbschema.ColumnFamiliesKind && kind != ydbschema.TTLKind && kind != ydbschema.ColumnStoreKind {
				entry.Facets[string(kind)] = json.RawMessage(`"not recorded"`)
			}
		}
		for _, candidate := range db.Indexes {
			if candidate.StructName != t.StructName || (candidate.TableName != "" && candidate.TableName != t.Name) {
				continue
			}
			recorded := index{Name: candidate.Name, Facets: map[string]json.RawMessage{}}
			for _, kind := range candidate.Facets.Kinds() {
				recorded.Facets[string(kind)] = json.RawMessage(`"not recorded"`)
			}
			if len(recorded.Facets) == 0 {
				recorded.Facets = nil
			}
			recorded.Options = candidate.Overrides["ydb"]
			if len(recorded.Facets) != 0 || len(recorded.Options) != 0 {
				entry.Indexes = append(entry.Indexes, recorded)
			}
		}
		result = append(result, entry)
	}
	return result
}

func marshal(value any) json.RawMessage {
	encoded, err := json.Marshal(value)
	check(err)
	return encoded
}
