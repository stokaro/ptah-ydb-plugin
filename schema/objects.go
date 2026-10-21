package schema

import (
	"fmt"
	"strings"

	native "github.com/stokaro/ptah-ydb-plugin/internal/nativeprotocol"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb_Coordination"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb_Scheme"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb_Table"
	"google.golang.org/protobuf/proto"
)

const (
	// ViewProperties owns a view query, attributes, and access metadata.
	ViewProperties = modelNamespace + "view"
	// CoordinationProperties owns a coordination node's configuration.
	CoordinationProperties = modelNamespace + "coordination-node"
	// ExternalSourceProperties owns an external source's native options.
	ExternalSourceProperties = modelNamespace + "external-data-source"
	// ExternalTableProperties owns an external table's columns and options.
	ExternalTableProperties = modelNamespace + "external-table"
)

// IsNativeObject distinguishes standalone objects from table/column facets.
// Inspection coverage and owner validation must use the same classification.
func IsNativeObject(kind string) bool {
	switch kind {
	case ViewProperties, CoordinationProperties, ExternalSourceProperties, ExternalTableProperties, TopicProperties:
		return true
	default:
		return false
	}
}

// CaptureObject retains native object settings, excluding transient self
// identity and a coordination node's duplicate config path. Unknown fields
// are checked before any projection can remove them.
func CaptureObject(owner *protocol.Reference, described proto.Message) (*protocol.Envelope, error) {
	kind := owner.GetKind()
	expected := message(kind)
	if !IsNativeObject(kind) || expected == nil {
		return nil, fmt.Errorf("unsupported native YDB object model")
	}
	if err := Known(described); err != nil {
		return nil, err
	}
	if described.ProtoReflect().Descriptor() != expected.ProtoReflect().Descriptor() {
		return nil, fmt.Errorf("native YDB object description does not match its model")
	}
	projected := proto.Clone(described)
	switch value := projected.(type) {
	case *native.ViewObservation:
		if value.View == nil || value.Properties == nil {
			return nil, fmt.Errorf("incomplete YDB view observation")
		}
		projectSelf(value.View.Self)
		projectSelf(value.Properties.Self)
		value.Properties.TableStats = nil
	case *Ydb_Coordination.DescribeNodeResult:
		projectSelf(value.Self)
		if value.Config != nil {
			value.Config.Path = ""
		}
	case *Ydb_Table.DescribeExternalDataSourceResult:
		projectSelf(value.Self)
	case *native.DescribeExternalTableResult:
		projectSelf(value.Self)
	case *native.DescribeTopicResult:
		projectTopic(value)
	}
	return capture(kind, owner, projected)
}

func projectSelf(self *Ydb_Scheme.Entry) {
	if self != nil {
		self.Name = ""
		self.Type = 0
		self.SizeBytes = 0
		self.CreatedAt = nil
	}
}

func transientSelf(self *Ydb_Scheme.Entry) bool {
	return self.GetName() != "" || self.GetType() != 0 || self.GetSizeBytes() != 0 || self.GetCreatedAt() != nil
}

func normalizeObject(described proto.Message) error {
	var self *Ydb_Scheme.Entry
	switch value := described.(type) {
	case *native.ViewObservation:
		if value.View == nil || value.Properties == nil || strings.TrimSpace(value.View.QueryText) == "" {
			return fmt.Errorf("incomplete YDB view observation")
		}
		self = value.View.Self
		properties := value.Properties
		// DescribeTable returns these disabled switches for views on YDB 25.1
		// and 26.2. They describe no view partition policy. Refuse any other
		// table partition configuration instead of silently losing it.
		if partitioning := properties.PartitioningSettings; partitioning != nil {
			disabled := &Ydb_Table.PartitioningSettings{PartitioningBySize: Ydb.FeatureFlag_DISABLED, PartitioningByLoad: Ydb.FeatureFlag_DISABLED}
			if !proto.Equal(partitioning, disabled) {
				return fmt.Errorf("view table response has unsupported partitioning settings")
			}
			properties.PartitioningSettings = nil
		}
		if transientSelf(properties.Self) || properties.TableStats != nil {
			return fmt.Errorf("view properties contain transient metadata")
		}
		if len(properties.Columns)+len(properties.PrimaryKey)+len(properties.Indexes)+len(properties.Changefeeds) != 0 {
			return fmt.Errorf("view properties contain table child objects")
		}
	case *Ydb_Coordination.DescribeNodeResult:
		self = value.Self
		if value.Config == nil {
			return fmt.Errorf("missing YDB coordination configuration")
		}
		if value.Config.Path != "" {
			return fmt.Errorf("coordination configuration repeats its object path")
		}
	case *Ydb_Table.DescribeExternalDataSourceResult:
		self = value.Self
		if value.GetSourceType() == "" || value.Location == nil {
			return fmt.Errorf("incomplete YDB external data source")
		}
	case *native.DescribeExternalTableResult:
		self = value.Self
		if value.GetSourceType() == "" || value.GetDataSourcePath() == "" || value.Location == nil || len(value.Columns) == 0 {
			return fmt.Errorf("incomplete YDB external table")
		}
		names := map[string]bool{}
		for _, column := range value.Columns {
			if column.Name == "" || names[column.Name] {
				return fmt.Errorf("invalid YDB external column identity")
			}
			names[column.Name] = true
			if _, _, err := ColumnType(column.Type); err != nil {
				return err
			}
		}
	case *native.DescribeTopicResult:
		return normalizeTopic(value)
	default:
		return fmt.Errorf("unsupported native YDB object model")
	}
	if transientSelf(self) {
		return fmt.Errorf("native object properties contain transient identity")
	}
	return nil
}
