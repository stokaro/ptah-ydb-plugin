package tablesource

import (
	"errors"
	"fmt"
	"strconv"

	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
)

var errNull = errors.New("a null is not a value")

// scalar reads a captured scalar as the text the author wrote. A YAML number
// keeps its exact digits and a boolean reads true or false, as the bundled
// decoder reads every scalar as its text. A null, an expression, a reference,
// a sequence and a mapping are not scalars.
func scalar(value *protocol.SourceValue) (string, error) {
	switch content := value.GetValue().(type) {
	case *protocol.SourceValue_Text:
		return content.Text, nil
	case *protocol.SourceValue_Number:
		return content.Number, nil
	case *protocol.SourceValue_Boolean:
		return strconv.FormatBool(content.Boolean), nil
	case *protocol.SourceValue_Null:
		return "", errNull
	case *protocol.SourceValue_Sql, *protocol.SourceValue_Reference:
		return "", errors.New("an expression or a reference is not a value; write the value itself")
	default:
		return "", errors.New("expected a scalar")
	}
}

// optional reads a setting that may be written null, which means the author
// left it out.
func optional(value *protocol.SourceValue) (*string, error) {
	text, err := scalar(value)
	if errors.Is(err, errNull) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &text, nil
}

// apply reads one claimed fragment into the table's settings.
func apply(target *settings, fragment *protocol.SourceFragment) error {
	if claim := fragment.GetYamlKey(); claim != nil {
		return applyYAML(target, claim.GetKey(), fragment.GetValue())
	}
	if fragment.GetGoDirective().GetDirective() == DirectiveTable {
		return applyTable(target, fragment.GetValue())
	}
	return applyGo(target, fragment.GetValue())
}

func applyYAML(target *settings, key string, value *protocol.SourceValue) error {
	if key == KeyColumnFamilies {
		return applyFamilies(target, value)
	}
	text, err := scalar(value)
	if errors.Is(err, errNull) {
		text, err = "", nil
	}
	if err != nil {
		return fmt.Errorf("%s: %w", key, err)
	}
	return target.setPartitioning(key, text)
}

// applyFamilies reads a column_families mapping: family names to their
// settings. A null or an empty mapping declares no family.
func applyFamilies(target *settings, value *protocol.SourceValue) error {
	if _, null := value.GetValue().(*protocol.SourceValue_Null); null {
		return nil
	}
	mapping := value.GetMapping()
	if mapping == nil {
		return fmt.Errorf("%s: expected a mapping of family names to settings", KeyColumnFamilies)
	}
	for _, entry := range mapping.GetEntries() {
		declared := family{name: entry.GetKey()}
		if _, null := entry.GetValue().GetValue().(*protocol.SourceValue_Null); !null {
			body := entry.GetValue().GetMapping()
			if body == nil {
				return fmt.Errorf("column family %q: expected a mapping of settings", entry.GetKey())
			}
			for _, setting := range body.GetEntries() {
				var slot **string
				switch setting.GetKey() {
				case AttributeData:
					slot = &declared.data
				case AttributeCompression:
					slot = &declared.compression
				case AttributeCacheMode:
					slot = &declared.cacheMode
				case AttributeFields:
					slot = &declared.fields
				default:
					return fmt.Errorf("column family %q: unknown setting %q", entry.GetKey(), setting.GetKey())
				}
				text, err := familySetting(setting.GetKey(), setting.GetValue())
				if err != nil {
					return fmt.Errorf("column family %q: %s: %w", entry.GetKey(), setting.GetKey(), err)
				}
				*slot = text
			}
		}
		if err := target.addFamily(declared); err != nil {
			return fmt.Errorf("column family %q: %w", entry.GetKey(), err)
		}
	}
	return nil
}

// familySetting reads one setting of a YAML family. fields is a list written
// as a sequence or as one comma-separated scalar; the provider refuses it by
// name whatever it holds, so only its presence is read.
func familySetting(key string, value *protocol.SourceValue) (*string, error) {
	if key == AttributeFields {
		if _, null := value.GetValue().(*protocol.SourceValue_Null); null {
			return nil, nil
		}
		if text, isText := value.GetValue().(*protocol.SourceValue_Text); isText && text.Text == "" {
			return nil, nil
		}
		present := "fields"
		return &present, nil
	}
	return optional(value)
}

// applyGo reads the attributes of one columnfamily directive.
func applyGo(target *settings, value *protocol.SourceValue) error {
	declared := family{}
	for _, entry := range value.GetMapping().GetEntries() {
		text, err := scalar(entry.GetValue())
		if err != nil {
			return fmt.Errorf("%s: %w", entry.GetKey(), err)
		}
		switch entry.GetKey() {
		case AttributeName:
			declared.name = text
		case AttributeTable:
		case AttributeData:
			declared.data = &text
		case AttributeCompression:
			declared.compression = &text
		case AttributeCacheMode:
			declared.cacheMode = &text
		case AttributeFields:
			declared.fields = &text
		default:
			return fmt.Errorf("unknown attribute %q", entry.GetKey())
		}
	}
	if err := target.addFamily(declared); err != nil {
		return fmt.Errorf("column family %q: %w", declared.name, err)
	}
	return nil
}

// applyTable reads the attributes a Go source writes on ptah:schema:table.
// Only the attributes the provider claimed arrive, each as the text the author
// wrote.
func applyTable(target *settings, value *protocol.SourceValue) error {
	values := make(map[string]string)
	for _, entry := range value.GetMapping().GetEntries() {
		text, err := scalar(entry.GetValue())
		if err != nil {
			return fmt.Errorf("%s: %w", entry.GetKey(), err)
		}
		values[entry.GetKey()] = text
	}
	// A column table is checked before anything else is read, as the bundled
	// decoder reads the storage first, so the refusal of an input that mixes
	// row settings with column storage names the storage.
	if err := refuseColumnStore(values); err != nil {
		return err
	}
	for _, name := range partitioningKeys() {
		if text, found := values[name]; found {
			if err := target.setPartitioning(name, text); err != nil {
				return err
			}
		}
	}
	for _, name := range []string{AttributeUniformPartitions, AttributePartitionAtKeys} {
		if _, found := values[name]; found {
			return &UnsupportedError{Setting: name, Reason: "the row-table model carries no starting partition layout, which YDB takes only when it creates a table"}
		}
	}
	return target.setTTL(values)
}
