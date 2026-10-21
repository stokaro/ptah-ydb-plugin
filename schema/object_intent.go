package schema

import (
	"fmt"
	"maps"
	"slices"

	native "github.com/stokaro/ptah-ydb-plugin/internal/nativeprotocol"
	"github.com/ydb-platform/ydb-go-genproto/draft/protos/Ydb_View"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb_Table"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
)

func objectIntent(value proto.Message) (proto.Message, error) {
	switch observed := value.(type) {
	case *native.ViewObservation:
		// Both APIs describe the same object. Disagreement cannot become two grants.
		if !proto.Equal(observed.View.Self, observed.Properties.Self) {
			return nil, fmt.Errorf("YDB view APIs disagree about access")
		}
		remainder := proto.CloneOf(observed.Properties)
		remainder.Self = nil
		remainder.Attributes = nil
		if !proto.Equal(remainder, &native.DescribeTableResult{}) {
			var fieldName protoreflect.Name
			remainder.ProtoReflect().Range(func(field protoreflect.FieldDescriptor, _ protoreflect.Value) bool {
				fieldName = field.Name()
				return false
			})
			return nil, fmt.Errorf("YDB view has unmodeled table setting %s", fieldName)
		}
		return &native.ViewIntent{QueryText: observed.View.QueryText, Attributes: maps.Clone(observed.Properties.Attributes), Access: accessIntent(observed.View.Self)}, nil
	case *Ydb_Table.DescribeExternalDataSourceResult:
		return &native.ExternalSourceIntent{Access: accessIntent(observed.Self), SourceType: observed.SourceType, Location: observed.Location, Properties: maps.Clone(observed.Properties)}, nil
	case *native.DescribeExternalTableResult:
		columns, err := columnDeclarations(observed.Columns)
		if err != nil {
			return nil, err
		}
		return &native.ExternalTableIntent{Access: accessIntent(observed.Self), SourceType: observed.SourceType, DataSourcePath: observed.DataSourcePath, Location: observed.Location, Content: maps.Clone(observed.Content), Columns: columns}, nil
	case *native.ColumnStorageObservation:
		return &native.ColumnStorageIntent{HashColumns: slices.Clone(observed.HashColumns), Partitions: observed.Partitions, DataChannelCount: observed.DataChannelCount, LocalIndexes: cloneIndexes(observed.LocalIndexes), Ttl: proto.CloneOf(observed.Ttl)}, nil
	default:
		return nil, fmt.Errorf("unsupported native YDB observation")
	}
}

func objectObservation(value proto.Message) (proto.Message, error) {
	var result proto.Message
	var access *native.AccessIntent
	switch intent := value.(type) {
	case *native.ViewIntent:
		access = intent.Access
		result = &native.ViewObservation{View: &Ydb_View.DescribeViewResult{QueryText: intent.QueryText, Self: accessObservation(access)}, Properties: &native.DescribeTableResult{Self: accessObservation(access), Attributes: maps.Clone(intent.Attributes)}}
	case *native.ExternalSourceIntent:
		access = intent.Access
		result = &Ydb_Table.DescribeExternalDataSourceResult{Self: accessObservation(access), SourceType: intent.SourceType, Location: intent.Location, Properties: maps.Clone(intent.Properties)}
	case *native.ExternalTableIntent:
		access = intent.Access
		columns, err := declaredColumns(intent.Columns)
		if err != nil {
			return nil, err
		}
		result = &native.DescribeExternalTableResult{Self: accessObservation(access), SourceType: intent.SourceType, DataSourcePath: intent.DataSourcePath, Location: intent.Location, Content: maps.Clone(intent.Content), Columns: columns}
	case *native.ColumnStorageIntent:
		observed := &native.ColumnStorageObservation{HashColumns: slices.Clone(intent.HashColumns), Partitions: intent.Partitions, DataChannelCount: intent.DataChannelCount, LocalIndexes: cloneIndexes(intent.LocalIndexes), Ttl: proto.CloneOf(intent.Ttl)}
		if err := normalizeColumnStorage(observed); err != nil {
			return nil, err
		}
		return observed, nil
	default:
		return nil, fmt.Errorf("unsupported native YDB declaration")
	}
	if err := validateAccessIntent(access); err != nil {
		return nil, err
	}
	if err := normalizeObject(result); err != nil {
		return nil, err
	}
	return result, nil
}

func cloneIndexes(values []*native.LocalIndexObservation) []*native.LocalIndexObservation {
	result := make([]*native.LocalIndexObservation, len(values))
	for i, value := range values {
		result[i] = proto.CloneOf(value)
	}
	return result
}

func columnDeclarations(columns []*native.ColumnMeta) ([]*native.ColumnDeclaration, error) {
	var result []*native.ColumnDeclaration
	for _, column := range columns {
		settings, err := columnIntent(column)
		if err != nil {
			return nil, err
		}
		result = append(result, &native.ColumnDeclaration{Name: column.Name, Type: proto.CloneOf(column.Type), NotNull: column.NotNull, Settings: settings})
	}
	return result, nil
}

func declaredColumns(columns []*native.ColumnDeclaration) ([]*native.ColumnMeta, error) {
	var result []*native.ColumnMeta
	for _, column := range columns {
		if column.Settings == nil {
			return nil, fmt.Errorf("missing YDB column settings")
		}
		if err := validateColumnIntent(column.Settings); err != nil {
			return nil, err
		}
		observed := columnObservation(column.Settings)
		observed.Name, observed.Type, observed.NotNull = column.Name, proto.CloneOf(column.Type), column.NotNull
		result = append(result, observed)
	}
	return result, nil
}
