// Package inspect reads native YDB descriptions without SDK type coercion.
package inspect

import (
	"context"
	"fmt"
	"path"

	"github.com/ydb-platform/ydb-go-genproto/Ydb_Coordination_V1"
	"github.com/ydb-platform/ydb-go-genproto/Ydb_Scheme_V1"
	"github.com/ydb-platform/ydb-go-genproto/Ydb_Table_V1"
	"github.com/ydb-platform/ydb-go-genproto/Ydb_Topic_V1"
	"github.com/ydb-platform/ydb-go-genproto/draft/Ydb_View_V1"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb_Scheme"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb_Topic"
	ydb "github.com/ydb-platform/ydb-go-sdk/v3"
	"google.golang.org/protobuf/proto"
	protocol "ptah.run/provider-sdk/protocol/experimental"

	"github.com/stokaro/ptah-ydb-plugin/internal/history"
	"github.com/stokaro/ptah-ydb-plugin/internal/metadata"
	"github.com/stokaro/ptah-ydb-plugin/internal/nativeapi"
	native "github.com/stokaro/ptah-ydb-plugin/internal/nativeprotocol"
	"github.com/stokaro/ptah-ydb-plugin/schema"
)

type source struct {
	scheme       Ydb_Scheme_V1.SchemeServiceClient
	table        Ydb_Table_V1.TableServiceClient
	topic        Ydb_Topic_V1.TopicServiceClient
	view         Ydb_View_V1.ViewServiceClient
	coordination Ydb_Coordination_V1.CoordinationServiceClient
	owner        *nativeapi.Table
	replications *nativeapi.Replications
}

func open(ctx context.Context, driver *ydb.Driver) (*source, error) {
	conn := ydb.GRPCConn(driver)
	s := &source{
		scheme:       Ydb_Scheme_V1.NewSchemeServiceClient(conn),
		table:        Ydb_Table_V1.NewTableServiceClient(conn),
		topic:        Ydb_Topic_V1.NewTopicServiceClient(conn),
		view:         Ydb_View_V1.NewViewServiceClient(conn),
		coordination: Ydb_Coordination_V1.NewCoordinationServiceClient(conn),
		replications: nativeapi.NewReplications(driver, nil),
	}
	owner, err := nativeapi.OpenTable(ctx, driver)
	if err != nil {
		return nil, err
	}
	s.owner = owner
	return s, nil
}

func (s *source) close() error { return s.owner.Close() }

func (s *source) directory(ctx context.Context, directoryPath string) (*Ydb_Scheme.ListDirectoryResult, error) {
	return nativeapi.SchemeLister(s.scheme)(ctx, directoryPath)
}

type description struct {
	reservation protocol.ReservedObject_Purpose
	table       *native.DescribeTableResult
	indexes     []*native.TableIndexDescription
	changefeeds []*native.ChangefeedDescription
}

// describeListedTable describes the row table name, which the listing of the
// absolute directory named. It waits out a table the listing names before the
// table service describes it, such as the replica an async replication is
// still creating; see [nativeapi.DescribeListedTable].
func (s *source) describeListedTable(ctx context.Context, directory, name string) (*description, error) {
	result, err := nativeapi.DescribeListedTable(ctx, s.owner.Describe, s.directory, directory, name)
	if err != nil {
		return nil, err
	}
	return classifyTable(path.Join(directory, name), result)
}

// describeTable describes the row table at absolute, which no listing named.
func (s *source) describeTable(ctx context.Context, absolute string) (*description, error) {
	result, err := s.owner.Describe(ctx, absolute)
	if err != nil {
		return nil, err
	}
	return classifyTable(absolute, result)
}

// classifyTable reads the description of the row table at absolute into its
// reservation, its own description and its child objects.
func classifyTable(absolute string, result *native.DescribeTableResult) (*description, error) {
	// Classify before projecting children away: a marked table with an index
	// or changefeed is altered storage and must not be silently hidden.
	owned, err := history.Owned(path.Base(absolute), result)
	if err != nil {
		return nil, err
	}
	purpose, err := metadata.Owned(path.Base(absolute), result)
	if err != nil {
		return nil, err
	}
	switch {
	case owned || history.BuiltinOwned(path.Base(absolute), result):
		purpose = protocol.ReservedObject_HISTORY
	case purpose == protocol.ReservedObject_PURPOSE_UNSPECIFIED:
		// Ptah's built-in YDB engine left its revision, log and tag tables
		// out of every read by name; one in its layout is still the
		// record of the migrations the database ran, and a plan must not
		// drop it.
		purpose = metadata.BuiltinOwned(path.Base(absolute), result)
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
	return &description{reservation: purpose, table: result, indexes: indexes, changefeeds: feeds}, nil
}

func (s *source) describeTopic(ctx context.Context, topicPath string) (*native.DescribeTopicResult, error) {
	response, err := s.topic.DescribeTopic(ctx, &Ydb_Topic.DescribeTopicRequest{Path: topicPath})
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
