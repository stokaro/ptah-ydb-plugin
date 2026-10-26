package testsource

import (
	"encoding/json"
	"fmt"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"google.golang.org/protobuf/proto"
	"io"
	"strings"
)

// YAMLDocument captures one document-level mapping under its declared model.
func YAMLDocument(source string, key *protocol.YAMLSourceKey, model *protocol.ValueModel) (*protocol.DecodeSourceRequest, error) {
	reader := capture{source: source, decoder: json.NewDecoder(strings.NewReader(source))}
	reader.decoder.UseNumber()
	root, err := reader.value()
	if err != nil {
		return nil, err
	}
	if _, err := reader.decoder.Token(); err != io.EOF {
		return nil, fmt.Errorf("fixture has trailing input")
	}
	entries := root.GetMapping().GetEntries()
	if len(entries) != 1 || entries[0].Key != key.Key {
		return nil, fmt.Errorf("fixture requires its declared document key")
	}
	value := entries[0].Value
	return &protocol.DecodeSourceRequest{Engine: "ydb", Documents: []*protocol.SourceDocument{{Id: "doc", File: "captured.yaml", ByteLength: uint64(len(source)), Format: protocol.SourceDocument_YAML, Models: []*protocol.ValueModel{proto.CloneOf(model)}, Fragments: []*protocol.SourceFragment{{Id: "objects", Selector: key.Selector, YamlKey: proto.CloneOf(key), Span: proto.CloneOf(value.Span), Content: &protocol.SourceFragment_Value{Value: value}}}}}}, nil
}
