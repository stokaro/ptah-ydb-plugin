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

// features returns the row-table settings and the column settings a captured
// declaration of a row table states. A column-storage facet is refused: a
// column table is compiled by CompileColumnTable.
func features(owner *protocol.Reference, captured *protocol.CapturedFeatures) (*native.RowTableIntent, map[string]*native.ColumnIntent, error) {
	facets, err := tableFacets(owner, captured)
	if err != nil {
		return nil, nil, err
	}
	if facets.storage != nil {
		return nil, nil, unsupported("a column table's declaration cannot be compiled as a row table")
	}
	return facets.table, facets.columns, nil
}

// declaredFacets are the facets a captured declaration states for its table
// and columns.
type declaredFacets struct {
	// table is the row-table settings, empty when none are stated.
	table *native.RowTableIntent
	// stated reports a row-table facet in the declaration.
	stated bool
	// columns are the column settings by column name.
	columns map[string]*native.ColumnIntent
	// storage is the column-storage facet of a column table, nil for a row table.
	storage *native.ColumnStorageIntent
}

func tableFacets(owner *protocol.Reference, captured *protocol.CapturedFeatures) (declaredFacets, error) {
	result := declaredFacets{table: &native.RowTableIntent{}, columns: make(map[string]*native.ColumnIntent)}
	if captured == nil {
		return result, nil
	}
	if len(captured.GetObjects().GetRecords()) != 0 {
		return declaredFacets{}, unsupported("YDB table creation does not yet support attached objects")
	}
	state := captured.GetFacets()
	if state == nil {
		return result, nil
	}
	if state.Representation != protocol.ValueInput_DESIRED {
		return declaredFacets{}, fmt.Errorf("YDB table creation requires desired facets")
	}
	seen := make(map[string]bool)
	for _, record := range state.Records {
		if len(record.Targets) > 0 && !slices.Contains(record.Targets, "ydb") {
			continue
		}
		if record.Value == nil {
			return declaredFacets{}, fmt.Errorf("YDB table creation cannot satisfy an excluded facet")
		}
		value, err := normalizedDesired(record)
		if err != nil {
			return declaredFacets{}, err
		}
		identity := proto.CloneOf(owner)
		if value.Kind == schema.ColumnProperties {
			identity.Kind, identity.Path, identity.Name = "column", []string{owner.Name}, value.Owner.Name
		}
		if !proto.Equal(identity, value.Owner) {
			return declaredFacets{}, fmt.Errorf("YDB creation facet belongs to a different owner")
		}
		slot := value.Kind + "\x00" + value.Owner.Name
		if seen[slot] {
			return declaredFacets{}, fmt.Errorf("duplicate YDB creation facet")
		}
		seen[slot] = true
		switch value.Kind {
		case schema.TableProperties:
			if err := protojson.Unmarshal(value.Payload, result.table); err != nil {
				return declaredFacets{}, err
			}
			result.stated = true
		case schema.ColumnProperties:
			var column native.ColumnIntent
			if err := protojson.Unmarshal(value.Payload, &column); err != nil {
				return declaredFacets{}, err
			}
			result.columns[value.Owner.Name] = &column
		case schema.ColumnStorageProperties:
			result.storage = &native.ColumnStorageIntent{}
			if err := protojson.Unmarshal(value.Payload, result.storage); err != nil {
				return declaredFacets{}, err
			}
		default:
			return declaredFacets{}, unsupported("YDB table creation does not yet support facet %s", value.Kind)
		}
	}
	return result, nil
}

func normalizedDesired(record *protocol.ComparisonRecord) (*protocol.Envelope, error) {
	return normalizedFacet(record, protocol.ValueInput_DESIRED)
}

// normalizedFacet re-normalizes a captured facet and requires the host's receipt
// to match, so a forged receipt cannot smuggle a value past the owner's rules.
func normalizedFacet(record *protocol.ComparisonRecord, representation protocol.ValueInput_Representation) (*protocol.Envelope, error) {
	captured := record.Value
	normalize, format := schema.NormalizeDesired, "/desired-json-v1"
	if representation == protocol.ValueInput_OBSERVED {
		normalize, format = schema.Normalize, "/observed-json-v1"
	}
	value, err := normalize(captured.GetValue())
	if err != nil {
		return nil, err
	}
	expected := &protocol.NormalizedValue{Id: captured.Id, Value: value, NormalizedOwner: proto.CloneOf(value.Owner),
		CanonicalFormat: value.Kind + format, CanonicalProjection: bytes.Clone(value.Payload)}
	if !proto.Equal(expected, captured) || record.Kind != value.Kind {
		return nil, fmt.Errorf("YDB facet has a forged normalization receipt")
	}
	if err := provider.ValidateComparisonIdentity(record.Subject); err != nil {
		return nil, err
	}
	subject, owner := record.Subject, value.Owner
	if subject.GetDatabase().GetSource() == "" && owner.Database != "" {
		// The host names a table and its columns without a database: they live
		// in the session's, which a read owner names and a declared owner leaves
		// out. The facet is read in the subject's terms, so both sides of one
		// table name the same owner.
		value = proto.CloneOf(value)
		value.Owner.Database = ""
		owner = value.Owner
	}
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
