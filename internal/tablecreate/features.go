package tablecreate

import (
	"bytes"
	"fmt"
	"slices"

	native "github.com/stokaro/ptah-ydb-plugin/internal/nativeprotocol"
	"github.com/stokaro/ptah-ydb-plugin/schema"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/stokaro/ptah/provider-sdk/provider"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

func features(owner *protocol.Reference, captured *protocol.CapturedFeatures) (*native.RowTableIntent, map[string]*native.ColumnIntent, error) {
	table := &native.RowTableIntent{}
	columns := make(map[string]*native.ColumnIntent)
	if captured == nil {
		return table, columns, nil
	}
	if len(captured.GetObjects().GetRecords()) != 0 {
		return nil, nil, fmt.Errorf("YDB table creation does not yet support attached objects")
	}
	state := captured.GetFacets()
	if state == nil {
		return table, columns, nil
	}
	if state.Representation != protocol.ValueInput_DESIRED {
		return nil, nil, fmt.Errorf("YDB table creation requires desired facets")
	}
	seen := make(map[string]bool)
	for _, record := range state.Records {
		if len(record.Targets) > 0 && !slices.Contains(record.Targets, "ydb") {
			continue
		}
		if record.Value == nil {
			return nil, nil, fmt.Errorf("YDB table creation cannot satisfy an excluded facet")
		}
		value, err := normalizedDesired(record)
		if err != nil {
			return nil, nil, err
		}
		identity := proto.CloneOf(owner)
		if value.Kind == schema.ColumnProperties {
			identity.Kind, identity.Path, identity.Name = "column", []string{owner.Name}, value.Owner.Name
		}
		if !proto.Equal(identity, value.Owner) {
			return nil, nil, fmt.Errorf("YDB creation facet belongs to a different owner")
		}
		slot := value.Kind + "\x00" + value.Owner.Name
		if seen[slot] {
			return nil, nil, fmt.Errorf("duplicate YDB creation facet")
		}
		seen[slot] = true
		switch value.Kind {
		case schema.TableProperties:
			if err := protojson.Unmarshal(value.Payload, table); err != nil {
				return nil, nil, err
			}
		case schema.ColumnProperties:
			var column native.ColumnIntent
			if err := protojson.Unmarshal(value.Payload, &column); err != nil {
				return nil, nil, err
			}
			columns[value.Owner.Name] = &column
		default:
			return nil, nil, fmt.Errorf("YDB table creation does not yet support facet %s", value.Kind)
		}
	}
	return table, columns, nil
}

func normalizedDesired(record *protocol.ComparisonRecord) (*protocol.Envelope, error) {
	captured := record.Value
	value, err := schema.NormalizeDesired(captured.GetValue())
	if err != nil {
		return nil, err
	}
	expected := &protocol.NormalizedValue{Id: captured.Id, Value: value, NormalizedOwner: proto.CloneOf(value.Owner),
		CanonicalFormat: value.Kind + "/desired-json-v1", CanonicalProjection: bytes.Clone(value.Payload)}
	if !proto.Equal(expected, captured) || record.Kind != value.Kind {
		return nil, fmt.Errorf("YDB creation facet has a forged normalization receipt")
	}
	if err := provider.ValidateComparisonIdentity(record.Subject); err != nil {
		return nil, err
	}
	subject, owner := record.Subject, value.Owner
	if subject.Kind != owner.Kind || subject.GetDatabase().GetSource() != owner.Database ||
		subject.GetSchema().GetSource() != owner.Schema || subject.GetName().GetSource() != owner.Name ||
		subject.GetCatalog().GetSource() != "" || len(subject.Path)+len(subject.Signature) != 0 {
		return nil, fmt.Errorf("YDB creation facet has a different bound identity")
	}
	parent := ""
	if owner.Kind == "column" && len(owner.Path) == 1 {
		parent = owner.Path[0]
	}
	if subject.GetParent().GetSource() != parent {
		return nil, fmt.Errorf("YDB creation facet has a different parent")
	}
	for _, part := range []*protocol.ComparisonPart{subject.Database, subject.Catalog, subject.Schema, subject.Parent, subject.Name} {
		if part.GetSource() != part.GetNormalized() {
			return nil, fmt.Errorf("YDB creation requires exact identifiers")
		}
	}
	return value, nil
}
