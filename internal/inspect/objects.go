package inspect

import (
	"context"
	"fmt"
	"path"

	"github.com/ydb-platform/ydb-go-genproto/draft/protos/Ydb_View"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb_Coordination"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb_Scheme"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb_Table"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"

	"github.com/stokaro/ptah-ydb-plugin/internal/nativeapi"
	native "github.com/stokaro/ptah-ydb-plugin/internal/nativeprotocol"
	"github.com/stokaro/ptah-ydb-plugin/schema"
)

// unrepresentableNode says why the description of a coordination node holds
// what the provider does not model, or "" for one it does: a field the pinned
// protocol buffers do not hold, or a consistency or counters mode they do not
// name. Read as a node it can represent, the node would be planned away, so
// the read records it as unrepresentable, as the host's embedded engine did,
// in its words.
func unrepresentableNode(node *Ydb_Coordination.DescribeNodeResult) string {
	config := node.GetConfig()
	for _, message := range []protoreflect.Message{node.ProtoReflect(), config.ProtoReflect()} {
		if !message.IsValid() || len(message.GetUnknown()) == 0 {
			continue
		}
		number, _, _ := protowire.ConsumeTag(message.GetUnknown())
		return fmt.Sprintf("its description carries field %d, which Ptah does not read", number)
	}
	if _, known := Ydb_Coordination.ConsistencyMode_name[int32(config.GetReadConsistencyMode())]; !known {
		return fmt.Sprintf("its read_consistency_mode is %d, which Ptah does not know", config.GetReadConsistencyMode())
	}
	if _, known := Ydb_Coordination.ConsistencyMode_name[int32(config.GetAttachConsistencyMode())]; !known {
		return fmt.Sprintf("its attach_consistency_mode is %d, which Ptah does not know", config.GetAttachConsistencyMode())
	}
	if _, known := Ydb_Coordination.RateLimiterCountersMode_name[int32(config.GetRateLimiterCountersMode())]; !known {
		return fmt.Sprintf("its rate_limiter_counters_mode is %d, which Ptah does not know", config.GetRateLimiterCountersMode())
	}
	return ""
}

func objectIdentity(self *Ydb_Scheme.Entry, absolute string, kind Ydb_Scheme.Entry_Type) error {
	if self.GetName() != path.Base(absolute) || self.GetType() != kind {
		return fmt.Errorf("YDB listing and native object description disagree")
	}
	return nil
}

func (s *source) readObject(ctx context.Context, absolute string, kind Ydb_Scheme.Entry_Type) (string, proto.Message, error) {
	switch kind {
	case Ydb_Scheme.Entry_SECRET:
		return schema.SecretProperties, &native.SecretPresence{}, nil
	case Ydb_Scheme.Entry_VIEW:
		return s.readView(ctx, absolute)
	case Ydb_Scheme.Entry_COORDINATION_NODE:
		response, err := s.coordination.DescribeNode(ctx, &Ydb_Coordination.DescribeNodeRequest{Path: absolute})
		if err != nil {
			return "", nil, fmt.Errorf("describe YDB coordination node: transport failed")
		}
		// The description is checked for what the provider does not model
		// once it is read (see unrepresentableNode), so that a node holding
		// it is recorded rather than refusing the read; the capture checks
		// the rest.
		if err := nativeapi.Status(response.GetOperation()); err != nil {
			return "", nil, err
		}
		var node Ydb_Coordination.DescribeNodeResult
		if err := response.GetOperation().GetResult().UnmarshalTo(&node); err != nil {
			return "", nil, fmt.Errorf("invalid YDB operation response")
		}
		if err := objectIdentity(node.Self, absolute, kind); err != nil {
			return "", nil, err
		}
		if node.GetConfig().GetPath() != "" && node.GetConfig().GetPath() != absolute {
			return "", nil, fmt.Errorf("YDB coordination config path disagrees with its identity")
		}
		return schema.CoordinationProperties, &node, nil
	case Ydb_Scheme.Entry_EXTERNAL_DATA_SOURCE:
		response, err := s.table.DescribeExternalDataSource(ctx, &Ydb_Table.DescribeExternalDataSourceRequest{Path: absolute})
		if err != nil {
			return "", nil, fmt.Errorf("describe YDB external data source: transport failed")
		}
		var source Ydb_Table.DescribeExternalDataSourceResult
		if err := nativeapi.Operation(response.GetOperation(), &source); err != nil {
			return "", nil, err
		}
		if err := objectIdentity(source.Self, absolute, kind); err != nil {
			return "", nil, err
		}
		return schema.ExternalSourceProperties, &source, nil
	case Ydb_Scheme.Entry_EXTERNAL_TABLE:
		return s.readExternalTable(ctx, absolute)
	default:
		return "", nil, fmt.Errorf("unsupported native YDB object reader")
	}
}

// readView describes the view at absolute, together with the table service's
// description of it, which must name no index or changefeed.
func (s *source) readView(ctx context.Context, absolute string) (string, proto.Message, error) {
	response, err := s.view.DescribeView(ctx, &Ydb_View.DescribeViewRequest{Path: absolute})
	if err != nil {
		return "", nil, fmt.Errorf("describe YDB view: transport failed")
	}
	var view Ydb_View.DescribeViewResult
	if err := nativeapi.Operation(response.GetOperation(), &view); err != nil {
		return "", nil, err
	}
	if err := objectIdentity(view.Self, absolute, Ydb_Scheme.Entry_VIEW); err != nil {
		return "", nil, err
	}
	properties, err := s.describeTable(ctx, absolute)
	if err != nil {
		return "", nil, err
	}
	if len(properties.indexes)+len(properties.changefeeds) != 0 {
		return "", nil, fmt.Errorf("unexpected child objects on YDB view")
	}
	if err := objectIdentity(properties.table.Self, absolute, Ydb_Scheme.Entry_VIEW); err != nil {
		return "", nil, err
	}
	return schema.ViewProperties, &native.ViewObservation{View: &view, Properties: properties.table}, nil
}

// readExternalTable describes the external table at absolute, refusing a
// description that carries a field the pinned protocol does not know.
func (s *source) readExternalTable(ctx context.Context, absolute string) (string, proto.Message, error) {
	response, err := s.table.DescribeExternalTable(ctx, &Ydb_Table.DescribeExternalTableRequest{Path: absolute})
	if err != nil {
		return "", nil, fmt.Errorf("describe YDB external table: transport failed")
	}
	if err := nativeapi.Status(response.GetOperation()); err != nil {
		return "", nil, err
	}
	payload := response.GetOperation().GetResult()
	if !payload.MessageIs(&Ydb_Table.DescribeExternalTableResult{}) {
		return "", nil, fmt.Errorf("invalid YDB external table response type")
	}
	var table native.DescribeExternalTableResult
	if err := proto.Unmarshal(payload.GetValue(), &table); err != nil {
		return "", nil, fmt.Errorf("invalid YDB external table response")
	}
	if err := schema.Known(&table); err != nil {
		return "", nil, err
	}
	if err := objectIdentity(table.Self, absolute, Ydb_Scheme.Entry_EXTERNAL_TABLE); err != nil {
		return "", nil, err
	}
	return schema.ExternalTableProperties, &table, nil
}
