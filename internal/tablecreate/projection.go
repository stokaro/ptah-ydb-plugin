package tablecreate

import (
	"bytes"
	"context"
	"fmt"
	"slices"

	"github.com/stokaro/ptah-ydb-plugin/internal/columnsemantics"
	"github.com/stokaro/ptah-ydb-plugin/schema"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/stokaro/ptah/provider-sdk/provider"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb_Table"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

// ProjectCreations resolves the effective ordered key into the predicted desired
// table model. Unspecified native settings remain unspecified; this operation
// does not invent server configuration, access grants, or catalog observations.
func ProjectCreations(ctx context.Context, request *protocol.ProjectTableCreationsRequest) (*protocol.ProjectedTableCreations, error) {
	if err := validateTableIdentifiers(request.Identifiers); err != nil {
		return nil, err
	}
	result := &protocol.ProjectedTableCreations{Complete: true}
	for i, input := range request.Tables {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		created, err := projectCreation(input, i, request.TargetFeatures, request.Capabilities)
		if err != nil {
			return nil, err
		}
		result.Tables = append(result.Tables, created)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return result, nil
}

func projectCreation(input *protocol.TableCreationInput, index int, targetFeatures []string, caps map[string]bool) (*protocol.ProjectedTableCreation, error) {
	columns := slices.Contains(targetFeatures, provider.ColumnsFeature)
	owner, err := boundOwner(input.Subject)
	if err != nil {
		return nil, err
	}
	if IsColumnTable(input.Declaration) {
		// A column table's declared storage is its projection: the server's
		// choices the declaration leaves out stay unspecified.
		compiled, err := CompileColumnTable(owner, input.Declaration, caps)
		if err != nil {
			return nil, err
		}
		result := &protocol.ProjectedTableCreation{Subject: proto.CloneOf(input.Subject), ColumnPrimaryKeysPrepared: true, ColumnPrimaryKeys: slices.Clone(compiled.PrimaryKey)}
		if columns {
			result.Columns, err = projectedColumns(compiled.Columns, compiled.PrimaryKey)
		}
		return result, err
	}
	compiled, err := Compile(owner, input.Declaration, caps)
	if err != nil {
		return nil, err
	}
	result := &protocol.ProjectedTableCreation{Subject: proto.CloneOf(input.Subject), ColumnPrimaryKeysPrepared: true, ColumnPrimaryKeys: slices.Clone(compiled.PrimaryKey)}
	if columns {
		result.Columns, err = projectedColumns(compiled.Columns, compiled.PrimaryKey)
		if err != nil {
			return nil, err
		}
	}
	if tablePropertiesExcluded(input.Declaration) {
		return result, nil
	}
	intent, _, err := features(owner, input.Declaration.Features)
	if err != nil {
		return nil, err
	}
	intent.PrimaryKey = slices.Clone(compiled.PrimaryKey)
	payload, err := protojson.Marshal(intent)
	if err != nil {
		return nil, err
	}
	model := schema.TableDesiredModel()
	value, err := schema.NormalizeDesired(&protocol.Envelope{ProviderId: schema.ProviderID, Kind: model.Kind,
		SchemaVersion: model.SchemaVersion, Encoding: model.Encoding, DefinitionSha256: model.DefinitionSha256, Owner: owner, Payload: payload})
	if err != nil {
		return nil, err
	}
	result.DesiredFacets = []*protocol.ComparisonRecord{{Kind: model.Kind, Subject: proto.CloneOf(input.Subject),
		Value: &protocol.NormalizedValue{Id: fmt.Sprintf("create/%06d/table", index), Value: value, NormalizedOwner: proto.CloneOf(owner),
			CanonicalFormat: model.CanonicalFormat, CanonicalProjection: bytes.Clone(value.Payload)}}}
	return result, nil
}

func tablePropertiesExcluded(table *protocol.DeclaredTable) bool {
	return slices.ContainsFunc(table.GetFeatures().GetFacets().GetRecords(), func(record *protocol.ComparisonRecord) bool {
		return record.Kind == schema.TableProperties && len(record.Targets) > 0 && !slices.Contains(record.Targets, "ydb")
	})
}

// projectedColumns uses the same compiled operands as native creation and YQL
// rendering. It reports an effective definition, not an observed server state.
func projectedColumns(columns []*Ydb_Table.ColumnMeta, key []string) ([]*protocol.ObservedColumn, error) {
	result := make([]*protocol.ObservedColumn, 0, len(columns))
	for i, column := range columns {
		item, err := columnsemantics.Projected(column, i+1, slices.Contains(key, column.Name))
		if err != nil {
			return nil, err
		}
		result = append(result, item)
	}
	return result, nil
}
