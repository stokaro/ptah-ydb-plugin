package schema

import (
	"fmt"
	native "github.com/stokaro/ptah-ydb-plugin/internal/nativeprotocol"
	"google.golang.org/protobuf/proto"
	"slices"
)

func indexIntents(observed *native.IndexObservations) (*native.IndexSetIntent, error) {
	result := &native.IndexSetIntent{}
	for _, index := range observed.Indexes {
		declaration := &native.IndexDeclaration{Specification: indexSpecification(index.Description)}
		for _, table := range index.Tables {
			if len(table.Description.ShardKeyBounds) != 0 {
				return nil, fmt.Errorf("index physical shard boundaries cannot become creation settings")
			}
			requirement := &native.ImplementationRequirement{Name: table.Name, Settings: tableIntent(table.Description)}
			for _, column := range table.Description.Columns {
				settings, err := columnIntent(column)
				if err != nil {
					return nil, err
				}
				requirement.Columns = append(requirement.Columns, &native.ImplementationColumnRequirement{Name: column.Name, Type: proto.CloneOf(column.Type), NotNull: column.NotNull, Settings: settings})
			}
			declaration.Implementation = append(declaration.Implementation, requirement)
		}
		result.Indexes = append(result.Indexes, declaration)
	}
	return result, nil
}

func indexObservations(intent *native.IndexSetIntent) (*native.IndexObservations, error) {
	result := &native.IndexObservations{}
	for _, index := range intent.Indexes {
		if index.Specification == nil {
			return nil, fmt.Errorf("missing YDB index specification")
		}
		observed := &native.ObservedIndex{Description: indexDescription(index.Specification)}
		for _, table := range index.Implementation {
			if table.Settings == nil {
				return nil, fmt.Errorf("missing YDB implementation settings")
			}
			if err := validateTableIntent(table.Settings); err != nil {
				return nil, err
			}
			description := tableObservation(table.Settings)
			for _, column := range table.Columns {
				if column.Settings == nil {
					return nil, fmt.Errorf("missing YDB implementation column settings")
				}
				if err := validateColumnIntent(column.Settings); err != nil {
					return nil, err
				}
				observedColumn := columnObservation(column.Settings)
				observedColumn.Name, observedColumn.Type, observedColumn.NotNull = column.Name, proto.CloneOf(column.Type), column.NotNull
				description.Columns = append(description.Columns, observedColumn)
			}
			observed.Tables = append(observed.Tables, &native.ImplementationTable{Name: table.Name, Description: description})
		}
		result.Indexes = append(result.Indexes, observed)
	}
	if err := normalizeIndexes(result); err != nil {
		return nil, err
	}
	return result, nil
}

func indexSpecification(source *native.TableIndexDescription) *native.IndexIntent {
	result := &native.IndexIntent{Name: source.Name, IndexColumns: slices.Clone(source.IndexColumns), DataColumns: slices.Clone(source.DataColumns)}
	switch value := source.Type.(type) {
	case *native.TableIndexDescription_GlobalIndex:
		result.Type = &native.IndexIntent_GlobalIndex{GlobalIndex: proto.CloneOf(value.GlobalIndex)}
	case *native.TableIndexDescription_GlobalAsyncIndex:
		result.Type = &native.IndexIntent_GlobalAsyncIndex{GlobalAsyncIndex: proto.CloneOf(value.GlobalAsyncIndex)}
	case *native.TableIndexDescription_GlobalUniqueIndex:
		result.Type = &native.IndexIntent_GlobalUniqueIndex{GlobalUniqueIndex: proto.CloneOf(value.GlobalUniqueIndex)}
	case *native.TableIndexDescription_GlobalVectorKmeansTreeIndex:
		result.Type = &native.IndexIntent_GlobalVectorKmeansTreeIndex{GlobalVectorKmeansTreeIndex: proto.CloneOf(value.GlobalVectorKmeansTreeIndex)}
	case *native.TableIndexDescription_GlobalFulltextPlainIndex:
		result.Type = &native.IndexIntent_GlobalFulltextPlainIndex{GlobalFulltextPlainIndex: proto.CloneOf(value.GlobalFulltextPlainIndex)}
	case *native.TableIndexDescription_GlobalFulltextRelevanceIndex:
		result.Type = &native.IndexIntent_GlobalFulltextRelevanceIndex{GlobalFulltextRelevanceIndex: proto.CloneOf(value.GlobalFulltextRelevanceIndex)}
	}
	return result
}

func indexDescription(source *native.IndexIntent) *native.TableIndexDescription {
	result := &native.TableIndexDescription{Name: source.Name, IndexColumns: slices.Clone(source.IndexColumns), DataColumns: slices.Clone(source.DataColumns)}
	result.Status = native.TableIndexDescription_STATUS_READY
	switch value := source.Type.(type) {
	case *native.IndexIntent_GlobalIndex:
		result.Type = &native.TableIndexDescription_GlobalIndex{GlobalIndex: proto.CloneOf(value.GlobalIndex)}
	case *native.IndexIntent_GlobalAsyncIndex:
		result.Type = &native.TableIndexDescription_GlobalAsyncIndex{GlobalAsyncIndex: proto.CloneOf(value.GlobalAsyncIndex)}
	case *native.IndexIntent_GlobalUniqueIndex:
		result.Type = &native.TableIndexDescription_GlobalUniqueIndex{GlobalUniqueIndex: proto.CloneOf(value.GlobalUniqueIndex)}
	case *native.IndexIntent_GlobalVectorKmeansTreeIndex:
		result.Type = &native.TableIndexDescription_GlobalVectorKmeansTreeIndex{GlobalVectorKmeansTreeIndex: proto.CloneOf(value.GlobalVectorKmeansTreeIndex)}
	case *native.IndexIntent_GlobalFulltextPlainIndex:
		result.Type = &native.TableIndexDescription_GlobalFulltextPlainIndex{GlobalFulltextPlainIndex: proto.CloneOf(value.GlobalFulltextPlainIndex)}
	case *native.IndexIntent_GlobalFulltextRelevanceIndex:
		result.Type = &native.TableIndexDescription_GlobalFulltextRelevanceIndex{GlobalFulltextRelevanceIndex: proto.CloneOf(value.GlobalFulltextRelevanceIndex)}
	}
	return result
}
