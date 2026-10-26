package testsource

import (
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"slices"
	"strconv"
	"strings"

	"github.com/stokaro/ptah-ydb-plugin/internal/changefeedsource"
	"github.com/stokaro/ptah-ydb-plugin/internal/externalsource"
	"github.com/stokaro/ptah-ydb-plugin/internal/tablesource"
	"github.com/stokaro/ptah-ydb-plugin/schema"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"google.golang.org/protobuf/proto"
)

// desiredModels returns the desired models a source of the given format
// enrolls, as the host does for a provider that claims syntax in that format.
func desiredModels(format protocol.SourceDocument_Format) []*protocol.ValueModel {
	var models []*protocol.ValueModel
	for _, model := range schema.Models() {
		if model.Representation == protocol.ValueInput_DESIRED && slices.Contains(model.SourceFormats, format) {
			models = append(models, model)
		}
	}
	return models
}

// YAMLTables captures a whole YAML schema written as JSON, which YAML reads as
// flow syntax, the way the host captures the keys the provider claimed: one
// binding for each table of the tables mapping and one fragment for each
// claimed table key the table writes, in the order the table writes them.
func YAMLTables(source string) (*protocol.DecodeSourceRequest, error) {
	reader := capture{source: source, decoder: json.NewDecoder(strings.NewReader(source))}
	reader.decoder.UseNumber()
	root, err := reader.value()
	if err != nil {
		return nil, err
	}
	if _, err := reader.decoder.Token(); err != io.EOF {
		return nil, fmt.Errorf("fixture has trailing input")
	}
	document := &protocol.SourceDocument{Id: "doc", File: "captured.yaml", ByteLength: uint64(len(source)), Format: protocol.SourceDocument_YAML,
		Models: desiredModels(protocol.SourceDocument_YAML)}
	claims := make(map[string]*protocol.YAMLSourceKey)
	for _, claim := range append(tablesource.YAMLKeys(), changefeedsource.YAMLKey()) {
		claims[claim.Key] = claim
	}
	for _, section := range root.GetMapping().GetEntries() {
		if section.Key != "tables" {
			continue
		}
		for _, entry := range section.Value.GetMapping().GetEntries() {
			binding := &protocol.SourceBinding{Id: entry.Key, EntryKey: entry.Key, Object: &protocol.Reference{Engine: "ydb", Kind: "table", Name: entry.Key}}
			var written []*protocol.SourceEntry
			// The host hands a claimed platform group as a fragment of its own:
			// the key is platform. and the group's name, and the value is the
			// group's mapping.
			var groups []*protocol.SourceEntry
			for _, field := range entry.Value.GetMapping().GetEntries() {
				switch field.Key {
				case "name":
					binding.Object.Name = field.Value.GetText()
				case "schema":
					binding.Object.Schema = field.Value.GetText()
				}
				if _, claimed := claims[field.Key]; claimed {
					written = append(written, field)
				}
				if field.Key == "platform" {
					for _, group := range field.Value.GetMapping().GetEntries() {
						if _, claimed := claims["platform."+group.Key]; claimed {
							groups = append(groups, &protocol.SourceEntry{Key: "platform." + group.Key, KeySpan: group.KeySpan, Value: group.Value})
						}
					}
				}
			}
			document.Bindings = append(document.Bindings, binding)
			for _, field := range append(written, groups...) {
				document.Fragments = append(document.Fragments, &protocol.SourceFragment{Id: entry.Key + "/" + field.Key, Selector: field.Key,
					YamlKey: proto.CloneOf(claims[field.Key]), BindingId: entry.Key, Span: field.Value.Span, Content: &protocol.SourceFragment_Value{Value: field.Value}})
			}
		}
	}
	return &protocol.DecodeSourceRequest{Engine: "ydb", Documents: []*protocol.SourceDocument{document}}, nil
}

// GoPosition is a place in a Go source file.
type GoPosition struct{ Offset, Line, Column int }

// GoTable is a table a Go file declares.
type GoTable struct{ Schema, Name, Struct string }

// GoSubject is the part of a table a column or index directive describes, as
// the host lexed it: the written name, and the table the directive names when
// it is not the struct's own.
type GoSubject struct {
	Kind  string
	Name  string
	Table string
}

// GoDeclaration is one directive the host lexed from a Go comment.
type GoDeclaration struct {
	Directive  string
	Attributes map[string]string
	Struct     string
	File       string
	FileSize   int
	Start, End GoPosition
	// Subject is set for a directive of column or index scope.
	Subject GoSubject
}

// GoDirectives captures the declarations of one Go file as the host does for a
// provider that claims Go directives: one document, a binding for each table,
// and one fragment for each declaration whose attributes are text. A directive
// of table scope is bound to the table of the struct it is written on, and the
// provider reads a table attribute itself. A directive of document scope, such
// as a coordination node's, is bound to nothing.
func GoDirectives(tables []GoTable, declarations []GoDeclaration) (*protocol.DecodeSourceRequest, error) {
	document := &protocol.SourceDocument{Id: "document", Format: protocol.SourceDocument_GO_ANNOTATIONS, File: "<unnamed Go>",
		Models: desiredModels(protocol.SourceDocument_GO_ANNOTATIONS)}
	bindings := make([]string, len(tables))
	for i, table := range tables {
		bindings[i] = "table-" + strconv.Itoa(i)
		document.Bindings = append(document.Bindings, &protocol.SourceBinding{Id: bindings[i], StructName: table.Struct,
			Object: &protocol.Reference{Engine: "ydb", Kind: "table", Schema: table.Schema, Name: table.Name}})
	}
	for i, declaration := range declarations {
		if i == 0 {
			document.File, document.ByteLength = declaration.File, uint64(declaration.FileSize)
		}
		claims := slices.Concat(tablesource.GoDirectives(), changefeedsource.GoDirectives(), []*protocol.GoSourceDirective{schema.CoordinationGoDirective()}, externalsource.GoDirectives())
		claim := slices.IndexFunc(claims, func(claim *protocol.GoSourceDirective) bool { return claim.Directive == declaration.Directive })
		if claim < 0 {
			return nil, fmt.Errorf("the provider claims no Go directive %q", declaration.Directive)
		}
		directive := claims[claim]
		owning := slices.IndexFunc(tables, func(table GoTable) bool {
			if declaration.Subject.Table != "" {
				return table.Name == declaration.Subject.Table
			}
			return table.Struct == declaration.Struct
		})
		if owning < 0 && directive.Scope != protocol.GoSourceDirective_DOCUMENT {
			return nil, fmt.Errorf("struct %s maps to no table in this file", declaration.Struct)
		}
		span := &protocol.SourceSpan{
			Start: &protocol.SourcePosition{ByteOffset: uint64(declaration.Start.Offset), Line: uint32(declaration.Start.Line), Column: uint32(declaration.Start.Column)},
			End:   &protocol.SourcePosition{ByteOffset: uint64(declaration.End.Offset), Line: uint32(declaration.End.Line), Column: uint32(declaration.End.Column)},
		}
		mapping := &protocol.SourceMapping{}
		for _, key := range slices.Sorted(maps.Keys(declaration.Attributes)) {
			mapping.Entries = append(mapping.Entries, &protocol.SourceEntry{Key: key, KeySpan: span,
				Value: &protocol.SourceValue{Span: span, Value: &protocol.SourceValue_Text{Text: declaration.Attributes[key]}}})
		}
		fragment := &protocol.SourceFragment{Id: strconv.Itoa(i), GoDirective: directive, Selector: directive.Selector,
			Span: span, Content: &protocol.SourceFragment_Value{Value: &protocol.SourceValue{Span: span, Value: &protocol.SourceValue_Mapping{Mapping: mapping}}}}
		if directive.Scope != protocol.GoSourceDirective_DOCUMENT {
			fragment.BindingId = bindings[owning]
		}
		if directive.Scope == protocol.GoSourceDirective_COLUMN || directive.Scope == protocol.GoSourceDirective_INDEX {
			fragment.Subject = &protocol.Reference{Engine: "ydb", Kind: declaration.Subject.Kind, Schema: tables[owning].Schema, Path: []string{tables[owning].Name}, Name: declaration.Subject.Name}
		}
		document.Fragments = append(document.Fragments, fragment)
	}
	return &protocol.DecodeSourceRequest{Engine: "ydb", Documents: []*protocol.SourceDocument{document}}, nil
}
