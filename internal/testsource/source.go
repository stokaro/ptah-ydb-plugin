// Package testsource builds captured JSON-form YAML fixtures with byte spans.
// Only tests import it; production receives captures from the host frontend.
package testsource

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/stokaro/ptah-ydb-plugin/schema"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
)

// Request captures one coordination_nodes mapping without evaluating expressions.
func Request(source string) (*protocol.DecodeSourceRequest, error) {
	reader := capture{source: source, decoder: json.NewDecoder(strings.NewReader(source))}
	reader.decoder.UseNumber()
	value, err := reader.value()
	if err != nil {
		return nil, err
	}
	if _, err := reader.decoder.Token(); err != io.EOF {
		return nil, fmt.Errorf("fixture has trailing input")
	}
	if value.GetMapping() == nil || len(value.GetMapping().Entries) != 1 || value.GetMapping().Entries[0].Key != "coordination_nodes" {
		return nil, fmt.Errorf("fixture requires coordination_nodes")
	}
	value = value.GetMapping().Entries[0].Value
	return &protocol.DecodeSourceRequest{Engine: "ydb", Documents: []*protocol.SourceDocument{{Id: "doc", File: "captured.yaml", ByteLength: uint64(len(source)), Format: protocol.SourceDocument_YAML,
		Models: []*protocol.ValueModel{schema.CoordinationDesiredModel()}, Fragments: []*protocol.SourceFragment{{Id: "nodes", Selector: "coordination_nodes", YamlKey: schema.CoordinationYAMLKey(), Span: value.Span, Content: &protocol.SourceFragment_Value{Value: value}}},
	}}}, nil
}

type capture struct {
	source  string
	decoder *json.Decoder
}

func (r *capture) start() int {
	index := int(r.decoder.InputOffset())
	for index < len(r.source) && strings.ContainsRune(" \t\r\n,:", rune(r.source[index])) {
		index++
	}
	return index
}
func (r *capture) position(index int) *protocol.SourcePosition {
	prefix := r.source[:index]
	return &protocol.SourcePosition{ByteOffset: uint64(index), Line: uint32(strings.Count(prefix, "\n") + 1), Column: uint32(index - strings.LastIndex(prefix, "\n"))}
}
func (r *capture) span(start int) *protocol.SourceSpan {
	return &protocol.SourceSpan{Start: r.position(start), End: r.position(int(r.decoder.InputOffset()))}
}
func (r *capture) value() (*protocol.SourceValue, error) {
	start := r.start()
	token, err := r.decoder.Token()
	if err != nil {
		return nil, err
	}
	value := &protocol.SourceValue{}
	switch token := token.(type) {
	case nil:
		value.Value = &protocol.SourceValue_Null{Null: &protocol.Empty{}}
	case string:
		value.Value = &protocol.SourceValue_Text{Text: token}
	case bool:
		value.Value = &protocol.SourceValue_Boolean{Boolean: token}
	case json.Number:
		value.Value = &protocol.SourceValue_Number{Number: string(token)}
	case json.Delim:
		switch token {
		case '{':
			mapping := &protocol.SourceMapping{}
			for r.decoder.More() {
				keyStart := r.start()
				key, err := r.decoder.Token()
				if err != nil {
					return nil, err
				}
				keySpan := r.span(keyStart)
				child, err := r.value()
				if err != nil {
					return nil, err
				}
				mapping.Entries = append(mapping.Entries, &protocol.SourceEntry{Key: key.(string), KeySpan: keySpan, Value: child})
			}
			value.Value = &protocol.SourceValue_Mapping{Mapping: mapping}
		case '[':
			sequence := &protocol.SourceSequence{}
			for r.decoder.More() {
				child, err := r.value()
				if err != nil {
					return nil, err
				}
				sequence.Values = append(sequence.Values, child)
			}
			value.Value = &protocol.SourceValue_Sequence{Sequence: sequence}
		default:
			return nil, fmt.Errorf("unexpected fixture delimiter")
		}
		if _, err := r.decoder.Token(); err != nil {
			return nil, err
		}
	default:
		return nil, fmt.Errorf("unexpected fixture token")
	}
	value.Span = r.span(start)
	return value, nil
}
