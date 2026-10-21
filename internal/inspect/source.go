// Package inspect reads native YDB descriptions without SDK type coercion.
package inspect

import (
	"context"
	"fmt"

	"github.com/stokaro/ptah-ydb-plugin/internal/nativeapi"
	native "github.com/stokaro/ptah-ydb-plugin/internal/nativeprotocol"
	"github.com/stokaro/ptah-ydb-plugin/schema"
	"github.com/ydb-platform/ydb-go-genproto/Ydb_Coordination_V1"
	"github.com/ydb-platform/ydb-go-genproto/Ydb_Scheme_V1"
	"github.com/ydb-platform/ydb-go-genproto/Ydb_Table_V1"
	"github.com/ydb-platform/ydb-go-genproto/Ydb_Topic_V1"
	"github.com/ydb-platform/ydb-go-genproto/draft/Ydb_View_V1"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb_Scheme"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb_Topic"
	ydb "github.com/ydb-platform/ydb-go-sdk/v3"
	"google.golang.org/protobuf/proto"
)

type source struct {
	scheme       Ydb_Scheme_V1.SchemeServiceClient
	table        Ydb_Table_V1.TableServiceClient
	topic        Ydb_Topic_V1.TopicServiceClient
	view         Ydb_View_V1.ViewServiceClient
	coordination Ydb_Coordination_V1.CoordinationServiceClient
	owner        *nativeapi.Table
}

func open(ctx context.Context, driver *ydb.Driver) (*source, error) {
	conn := ydb.GRPCConn(driver)
	s := &source{scheme: Ydb_Scheme_V1.NewSchemeServiceClient(conn), table: Ydb_Table_V1.NewTableServiceClient(conn), topic: Ydb_Topic_V1.NewTopicServiceClient(conn), view: Ydb_View_V1.NewViewServiceClient(conn), coordination: Ydb_Coordination_V1.NewCoordinationServiceClient(conn)}
	owner, err := nativeapi.OpenTable(ctx, driver)
	if err != nil {
		return nil, err
	}
	s.owner = owner
	return s, nil
}

func (s *source) close() error { return s.owner.Close() }

func (s *source) directory(ctx context.Context, path string) (*Ydb_Scheme.ListDirectoryResult, error) {
	response, err := s.scheme.ListDirectory(ctx, &Ydb_Scheme.ListDirectoryRequest{Path: path})
	if err != nil {
		return nil, fmt.Errorf("list YDB directory: transport failed")
	}
	var result Ydb_Scheme.ListDirectoryResult
	if err := nativeapi.Operation(response.GetOperation(), &result); err != nil {
		return nil, err
	}
	return &result, nil
}

type description struct {
	table       *native.DescribeTableResult
	indexes     []*native.TableIndexDescription
	changefeeds []*native.ChangefeedDescription
}

func (s *source) describeTable(ctx context.Context, path string) (*description, error) {
	result, err := s.owner.Describe(ctx, path)
	if err != nil {
		return nil, err
	}
	if err := schema.ValidateChangefeeds(result.Changefeeds); err != nil {
		return nil, err
	}
	feeds := result.Changefeeds
	result.Changefeeds = nil
	indexes := result.Indexes
	if err := schema.ValidateIndexes(indexes); err != nil {
		return nil, err
	}
	result.Indexes = nil
	return &description{table: result, indexes: indexes, changefeeds: feeds}, nil
}

func (s *source) describeTopic(ctx context.Context, path string) (*native.DescribeTopicResult, error) {
	response, err := s.topic.DescribeTopic(ctx, &Ydb_Topic.DescribeTopicRequest{Path: path})
	if err != nil {
		return nil, fmt.Errorf("describe YDB topic: transport failed")
	}
	if err := nativeapi.Status(response.GetOperation()); err != nil {
		return nil, err
	}
	payload := response.GetOperation().GetResult()
	if !payload.MessageIs(&Ydb_Topic.DescribeTopicResult{}) {
		return nil, fmt.Errorf("invalid YDB topic response type")
	}
	var result native.DescribeTopicResult
	if err := proto.Unmarshal(payload.GetValue(), &result); err != nil {
		return nil, fmt.Errorf("invalid YDB topic response")
	}
	if err := schema.Known(&result); err != nil {
		return nil, err
	}
	return &result, nil
}
