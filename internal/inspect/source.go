// Package inspect reads native YDB descriptions without SDK type coercion.
package inspect

import (
	"context"
	"fmt"
	"time"

	"github.com/stokaro/ptah-ydb-plugin/schema"
	"github.com/ydb-platform/ydb-go-genproto/Ydb_Scheme_V1"
	"github.com/ydb-platform/ydb-go-genproto/Ydb_Table_V1"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb_Operations"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb_Scheme"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb_Table"
	ydb "github.com/ydb-platform/ydb-go-sdk/v3"
	"google.golang.org/protobuf/proto"
)

type source struct {
	scheme  Ydb_Scheme_V1.SchemeServiceClient
	table   Ydb_Table_V1.TableServiceClient
	session string
}

func open(ctx context.Context, driver *ydb.Driver) (*source, error) {
	conn := ydb.GRPCConn(driver)
	s := &source{scheme: Ydb_Scheme_V1.NewSchemeServiceClient(conn), table: Ydb_Table_V1.NewTableServiceClient(conn)}
	response, err := s.table.CreateSession(ctx, &Ydb_Table.CreateSessionRequest{})
	if err != nil {
		return nil, fmt.Errorf("create inspection session: transport failed")
	}
	var result Ydb_Table.CreateSessionResult
	if err := operation(response.GetOperation(), &result); err != nil {
		return nil, err
	}
	if result.GetSessionId() == "" {
		return nil, fmt.Errorf("YDB returned an empty inspection session")
	}
	s.session = result.GetSessionId()
	return s, nil
}

func (s *source) close() error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	response, err := s.table.DeleteSession(ctx, &Ydb_Table.DeleteSessionRequest{SessionId: s.session})
	if err != nil {
		return fmt.Errorf("close YDB inspection session: transport failed")
	}
	return operationStatus(response.GetOperation())
}

func operationStatus(op *Ydb_Operations.Operation) error {
	if !op.GetReady() || op.GetStatus() != Ydb.StatusIds_SUCCESS {
		return fmt.Errorf("YDB schema read failed: ready=%t status=%s", op.GetReady(), op.GetStatus())
	}
	return nil
}

func operation(op *Ydb_Operations.Operation, target proto.Message) error {
	if err := operationStatus(op); err != nil {
		return err
	}
	if err := op.GetResult().UnmarshalTo(target); err != nil {
		return fmt.Errorf("invalid YDB schema response")
	}
	return schema.Known(target)
}

func (s *source) directory(ctx context.Context, path string) (*Ydb_Scheme.ListDirectoryResult, error) {
	response, err := s.scheme.ListDirectory(ctx, &Ydb_Scheme.ListDirectoryRequest{Path: path})
	if err != nil {
		return nil, fmt.Errorf("list YDB directory: transport failed")
	}
	var result Ydb_Scheme.ListDirectoryResult
	if err := operation(response.GetOperation(), &result); err != nil {
		return nil, err
	}
	return &result, nil
}

func (s *source) describeTable(ctx context.Context, path string) (*Ydb_Table.DescribeTableResult, error) {
	response, err := s.table.DescribeTable(ctx, &Ydb_Table.DescribeTableRequest{SessionId: s.session, Path: path, IncludeSetVal: true})
	if err != nil {
		return nil, fmt.Errorf("describe YDB table: transport failed")
	}
	var result Ydb_Table.DescribeTableResult
	if err := operation(response.GetOperation(), &result); err != nil {
		return nil, err
	}
	return &result, nil
}
