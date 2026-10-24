package tablesource

import (
	"bytes"
	"context"
	"fmt"
	"strconv"
	"strings"

	native "github.com/stokaro/ptah-ydb-plugin/internal/nativeprotocol"
	"github.com/stokaro/ptah-ydb-plugin/schema"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

// Table is the row-table facet a document's fragments declare for one table.
type Table struct {
	// Binding is the common table the facet belongs to.
	Binding *protocol.SourceBinding
	// Fragment is the index in the document of the first fragment that
	// declared something for the table. The facet is returned with it, and the
	// document's other fragments of the table return no value.
	Fragment int
	// Value is the normalized desired facet.
	Value *protocol.NormalizedValue
	// Columns are the column facets of the columns the table's families list,
	// in the order the declarations list them. The first fragment returns them
	// with the table's facet.
	Columns []*protocol.NormalizedValue
}

type pending struct {
	binding  *protocol.SourceBinding
	fragment int
	settings settings
}

// Decode reads every fragment of document that carries a claim of this
// package. It reads no file and no environment, and any invalid fragment
// refuses the whole document. A table whose fragments declare nothing, such as
// an empty column_families mapping, gets no facet. Tables come back in the
// order of their first declaring fragment, and ids are unique in the document.
func Decode(ctx context.Context, document *protocol.SourceDocument) ([]Table, error) {
	var order []string
	byBinding := make(map[string]*pending)
	for index, fragment := range document.GetFragments() {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if !Owns(fragment) {
			continue
		}
		if directive := fragment.GetGoDirective().GetDirective(); directive == DirectiveChangefeed || directive == DirectiveChangefeedConsumer {
			return nil, &UnsupportedError{Setting: directive, Reason: "the provider plans no changefeed yet"}
		}
		if fragment.GetGoDirective().GetDirective() == DirectiveIndex {
			return nil, indexRefusal(fragment)
		}
		binding, err := bindingOf(document, fragment)
		if err != nil {
			return nil, fmt.Errorf("fragment %q: %w", fragment.GetId(), err)
		}
		target, seen := byBinding[binding.GetId()]
		if !seen {
			target = &pending{binding: binding, fragment: index}
			byBinding[binding.GetId()] = target
			order = append(order, binding.GetId())
		}
		if err := refuseOtherTableColumns(binding, fragment); err != nil {
			return nil, fmt.Errorf("table %q: %w", tableLabel(binding.GetObject()), err)
		}
		if err := apply(&target.settings, fragment); err != nil {
			return nil, fmt.Errorf("table %q: %w", tableLabel(binding.GetObject()), err)
		}
	}
	var tables []Table
	for _, id := range order {
		target := byBinding[id]
		if err := target.settings.finish(); err != nil {
			return nil, fmt.Errorf("table %q: %w", tableLabel(target.binding.GetObject()), err)
		}
		if target.settings.empty() {
			continue
		}
		value, err := normalized(document, target)
		if err != nil {
			return nil, fmt.Errorf("table %q: %w", tableLabel(target.binding.GetObject()), err)
		}
		columns, err := placedColumns(document, target)
		if err != nil {
			return nil, fmt.Errorf("table %q: %w", tableLabel(target.binding.GetObject()), err)
		}
		tables = append(tables, Table{Binding: target.binding, Fragment: target.fragment, Value: value, Columns: columns})
	}
	return tables, nil
}

// refuseOtherTableColumns refuses a family that lists columns and names a table
// other than the one of its struct. The host places a column of the table of
// the struct the directive is written on, and refuses a column of any other
// table, so a family of another table cannot place its columns.
func refuseOtherTableColumns(binding *protocol.SourceBinding, fragment *protocol.SourceFragment) error {
	if fragment.GetGoDirective().GetDirective() != DirectiveColumnFamily || binding.GetId() == fragment.GetBindingId() {
		return nil
	}
	if _, listed := goAttribute(fragment, AttributeFields); !listed {
		return nil
	}
	return &UnsupportedError{Setting: AttributeFields, Reason: "a family places the columns of the table of the struct its directive is written on, and the table attribute names another table"}
}

// placedColumns builds the column facet of every column a family of the table
// lists. The owner is the column under the table, which must be a field of the
// struct the directive is written on; the host refuses any other.
func placedColumns(document *protocol.SourceDocument, target *pending) ([]*protocol.NormalizedValue, error) {
	var columns []*protocol.NormalizedValue
	model := schema.ColumnDesiredModel()
	for _, placed := range target.settings.placements {
		owner := proto.CloneOf(target.binding.GetObject())
		owner.Kind, owner.Path, owner.Name = "column", []string{target.binding.GetObject().GetName()}, placed.column
		payload, err := protojson.Marshal(&native.ColumnIntent{Family: placed.family})
		if err != nil {
			return nil, err
		}
		value, err := schema.NormalizeDesired(&protocol.Envelope{ProviderId: schema.ProviderID, Kind: model.Kind, SchemaVersion: model.SchemaVersion,
			Encoding: model.Encoding, DefinitionSha256: model.DefinitionSha256, Owner: owner, Payload: payload})
		if err != nil {
			return nil, fmt.Errorf("column %q: %w", placed.column, err)
		}
		columns = append(columns, &protocol.NormalizedValue{Id: "column." + document.GetId() + "." + strconv.Itoa(target.fragment) + "." + placed.column, Value: value,
			NormalizedOwner: proto.CloneOf(owner), CanonicalFormat: model.CanonicalFormat, CanonicalProjection: bytes.Clone(value.Payload)})
	}
	return columns, nil
}

func tableLabel(table *protocol.Reference) string {
	if table.GetSchema() == "" {
		return table.GetName()
	}
	return table.GetSchema() + "." + table.GetName()
}

func normalized(document *protocol.SourceDocument, target *pending) (*protocol.NormalizedValue, error) {
	owner := proto.CloneOf(target.binding.GetObject())
	var facet proto.Message = &target.settings.intent
	model := schema.TableDesiredModel()
	if target.settings.column != nil {
		facet, model = target.settings.column, schema.NativeDesiredModel(schema.ColumnStorageProperties)
	}
	payload, err := protojson.Marshal(facet)
	if err != nil {
		return nil, err
	}
	value, err := schema.NormalizeDesired(&protocol.Envelope{ProviderId: schema.ProviderID, Kind: model.Kind, SchemaVersion: model.SchemaVersion,
		Encoding: model.Encoding, DefinitionSha256: model.DefinitionSha256, Owner: owner, Payload: payload})
	if err != nil {
		return nil, err
	}
	return &protocol.NormalizedValue{Id: "table." + document.GetId() + "." + strconv.Itoa(target.fragment), Value: value, NormalizedOwner: proto.CloneOf(owner),
		CanonicalFormat: model.CanonicalFormat, CanonicalProjection: bytes.Clone(value.Payload)}, nil
}

// bindingOf resolves the common table a fragment declares for. A Go directive
// may name another table of the file with its table attribute, which wins over
// the table of the struct it is written on.
func bindingOf(document *protocol.SourceDocument, fragment *protocol.SourceFragment) (*protocol.SourceBinding, error) {
	if fragment.GetGoDirective() != nil {
		if written, named := goAttribute(fragment, AttributeTable); named {
			return namedBinding(document, written)
		}
	}
	for _, binding := range document.GetBindings() {
		if binding.GetId() == fragment.GetBindingId() {
			return binding, nil
		}
	}
	return nil, fmt.Errorf("it has no common table binding")
}

// goAttribute returns the text of a Go directive attribute and whether it was
// written.
func goAttribute(fragment *protocol.SourceFragment, name string) (string, bool) {
	for _, entry := range fragment.GetValue().GetMapping().GetEntries() {
		if entry.GetKey() == name {
			value, err := scalar(entry.GetValue())
			return value, err == nil
		}
	}
	return "", false
}

// namedBinding finds the table a table attribute names, optionally qualified
// with its schema as schema.table. A name without a schema matches the table of
// that name in any schema, and is refused when the file declares it in more
// than one.
func namedBinding(document *protocol.SourceDocument, written string) (*protocol.SourceBinding, error) {
	schemaName, name, err := splitTable(written)
	if err != nil {
		return nil, &DeclarationError{Attribute: AttributeTable, Value: written, Reason: err.Error()}
	}
	var matches []*protocol.SourceBinding
	for _, binding := range document.GetBindings() {
		table := binding.GetObject()
		if table.GetKind() == "table" && table.GetName() == name && (schemaName == "" || table.GetSchema() == schemaName) {
			matches = append(matches, binding)
		}
	}
	switch len(matches) {
	case 1:
		return matches[0], nil
	case 0:
		return nil, fmt.Errorf("table %q is not declared in this file, and a column family is declared beside its table", written)
	}
	schemas := make([]string, 0, len(matches))
	for _, match := range matches {
		schemas = append(schemas, strconv.Quote(match.GetObject().GetSchema()))
	}
	return nil, fmt.Errorf("table %q is declared in schemas %s; name the schema in the table attribute", written, strings.Join(schemas, " and "))
}

// splitTable reads schema.table, each part optionally in double quotes where
// it holds a dot.
func splitTable(written string) (schemaName, name string, err error) {
	var parts []string
	var current strings.Builder
	quoted := false
	for _, r := range strings.TrimSpace(written) {
		switch {
		case r == '"':
			quoted = !quoted
		case r == '.' && !quoted:
			parts = append(parts, current.String())
			current.Reset()
		default:
			current.WriteRune(r)
		}
	}
	if quoted {
		return "", "", fmt.Errorf("a quoted name is not closed")
	}
	parts = append(parts, current.String())
	switch len(parts) {
	case 1:
		schemaName, name = "", parts[0]
	case 2:
		schemaName, name = parts[0], parts[1]
	default:
		return "", "", fmt.Errorf("write the table as name or schema.name, quoting a part that holds a dot")
	}
	if name == "" {
		return "", "", fmt.Errorf("it names no table")
	}
	return schemaName, name, nil
}

// indexRefusal refuses the first option an index directive writes. The bundled
// decoder reads partitioning, local, full-text and vector options of an index;
// the provider adds and drops only plain global indexes and compares nothing an
// index states beyond its keys, so an option it accepted would be dropped.
func indexRefusal(fragment *protocol.SourceFragment) error {
	setting := DirectiveIndex
	if entries := fragment.GetValue().GetMapping().GetEntries(); len(entries) != 0 {
		setting = entries[0].GetKey()
	}
	return &UnsupportedError{Setting: setting, Reason: "an index states no settings of its own in the row-table model, and the provider adds only plain global indexes"}
}
