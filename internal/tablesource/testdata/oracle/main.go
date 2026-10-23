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
// frontend lexed, which the provider's test replays as captured fragments.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"ptah.run/core/annotation"
	"ptah.run/core/goschema"
	"ptah.run/core/schemaext"
	"ptah.run/core/schemamodel"
	"ptah.run/core/yamlext"
	"ptah.run/core/yamlschema"
	"ptah.run/dialect/ydb/ydbschema"
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
}

type table struct {
	Schema string                     `json:"schema"`
	Name   string                     `json:"name"`
	Struct string                     `json:"struct,omitempty"`
	Facets map[string]json.RawMessage `json:"facets"`
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
	result := golden{Format: "yaml"}
	if err != nil {
		result.Outcome, result.Error = "refused", err.Error()
		return result
	}
	result.Outcome = "accepted"
	result.Tables = tables(db.Tables)
	return result
}

var ydbDirective = regexp.MustCompile(`^\s*//ptah:schema:(columnfamily|changefeed)`)

func goCase(path string) golden {
	data, err := os.ReadFile(path)
	check(err)
	extension := ydbsource.Annotations()
	recorded := &recorder{}
	start := extension.File
	extension.File = func() annotation.FileDecoder { return &recording{FileDecoder: start(), recorder: recorded} }
	owners, err := annotation.NewSet(extension)
	check(err)
	db, err := goschema.ParseSource(owners, filepath.Base(path), string(data))
	result := golden{Format: "go", Declarations: recorded.declarations}
	if err != nil {
		result.Outcome, result.Error = "refused", err.Error()
	} else {
		result.Outcome = "accepted"
		result.Tables = tables(db.Tables)
	}
	// A refusal can come before the file's tables are known, so read them from
	// the same source without the YDB directives.
	var kept []string
	for line := range strings.SplitSeq(string(data), "\n") {
		if !ydbDirective.MatchString(line) {
			kept = append(kept, line)
		}
	}
	bare, err := annotation.NewSet(extension)
	check(err)
	probe, err := goschema.ParseSource(bare, filepath.Base(path), strings.Join(kept, "\n"))
	check(err)
	if result.Outcome == "refused" {
		result.Tables = tables(probe.Tables)
		for i := range result.Tables {
			result.Tables[i].Facets = map[string]json.RawMessage{}
		}
	}
	return result
}

type recorder struct{ declarations []declaration }

type recording struct {
	annotation.FileDecoder
	recorder *recorder
}

func (r *recording) Decode(d annotation.Declaration) ([]annotation.Contribution, error) {
	r.recorder.declarations = append(r.recorder.declarations, declaration{
		Directive: d.Directive, Attributes: d.Attributes, Struct: d.Struct, File: d.File, FileSize: d.FileSize,
		Start: position{d.Start.Offset, d.Start.Line, d.Start.Column}, End: position{d.End.Offset, d.End.Line, d.End.Column},
	})
	return r.FileDecoder.Decode(d)
}

func tables(all []schemamodel.Table) []table {
	result := make([]table, 0, len(all))
	for _, t := range all {
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
		for _, kind := range t.Facets.Kinds() {
			if kind != ydbschema.TablePartitioningKind && kind != ydbschema.ColumnFamiliesKind {
				entry.Facets[string(kind)] = json.RawMessage(`"not recorded"`)
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
