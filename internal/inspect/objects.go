package inspect

import (
	"context"
	"fmt"
	"path"

	"github.com/stokaro/ptah-ydb-plugin/internal/nativeapi"
	native "github.com/stokaro/ptah-ydb-plugin/internal/nativeprotocol"
	"github.com/stokaro/ptah-ydb-plugin/schema"
	"github.com/ydb-platform/ydb-go-genproto/draft/protos/Ydb_View"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb_Coordination"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb_Scheme"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb_Table"
	"google.golang.org/protobuf/proto"
)

func objectIdentity(self *Ydb_Scheme.Entry, absolute string, kind Ydb_Scheme.Entry_Type) error {
	if self.GetName() != path.Base(absolute) || self.GetType() != kind {
		return fmt.Errorf("YDB listing and native object description disagree")
	}
	return nil
}

func (s *source) readObject(ctx context.Context, absolute string, kind Ydb_Scheme.Entry_Type) (string, proto.Message, error) {
	switch kind {
	case Ydb_Scheme.Entry_VIEW:
		response, err := s.view.DescribeView(ctx, &Ydb_View.DescribeViewRequest{Path: absolute})
		if err != nil {
			return "", nil, fmt.Errorf("describe YDB view: transport failed")
		}
		var view Ydb_View.DescribeViewResult
		if err := nativeapi.Operation(response.GetOperation(), &view); err != nil {
			return "", nil, err
		}
		if err := objectIdentity(view.Self, absolute, kind); err != nil {
			return "", nil, err
		}
		properties, err := s.describeTable(ctx, absolute)
		if err != nil {
			return "", nil, err
		}
		if len(properties.indexes)+len(properties.changefeeds) != 0 {
			return "", nil, fmt.Errorf("unexpected child objects on YDB view")
		}
		if err := objectIdentity(properties.table.Self, absolute, kind); err != nil {
			return "", nil, err
		}
		return schema.ViewProperties, &native.ViewObservation{View: &view, Properties: properties.table}, nil
	case Ydb_Scheme.Entry_COORDINATION_NODE:
		response, err := s.coordination.DescribeNode(ctx, &Ydb_Coordination.DescribeNodeRequest{Path: absolute})
		if err != nil {
			return "", nil, fmt.Errorf("describe YDB coordination node: transport failed")
		}
		var node Ydb_Coordination.DescribeNodeResult
		if err := nativeapi.Operation(response.GetOperation(), &node); err != nil {
			return "", nil, err
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
		if err := objectIdentity(table.Self, absolute, kind); err != nil {
			return "", nil, err
		}
		return schema.ExternalTableProperties, &table, nil
	default:
		return "", nil, fmt.Errorf("unsupported native YDB object reader")
	}
}
