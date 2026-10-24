// Package nativeapi preserves complete native table descriptions across readers.
package nativeapi

import (
	"cmp"
	"context"
	"fmt"
	"time"

	native "github.com/stokaro/ptah-ydb-plugin/internal/nativeprotocol"
	"github.com/stokaro/ptah-ydb-plugin/schema"
	"github.com/ydb-platform/ydb-go-genproto/Ydb_Operation_V1"
	"github.com/ydb-platform/ydb-go-genproto/Ydb_Table_V1"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb_Operations"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb_Table"
	ydb "github.com/ydb-platform/ydb-go-sdk/v3"
	"google.golang.org/protobuf/proto"
)

// Table owns one raw native table session. Calls do not retry mutations.
type Table struct {
	client     Ydb_Table_V1.TableServiceClient
	operations Ydb_Operation_V1.OperationServiceClient
	id         string
	// poll is how long await waits between reads of a running operation; zero
	// means operationPoll.
	poll time.Duration
}

// OpenTable creates the session required by native table schema operations.
func OpenTable(ctx context.Context, driver *ydb.Driver) (*Table, error) {
	conn := ydb.GRPCConn(driver)
	s := &Table{client: Ydb_Table_V1.NewTableServiceClient(conn), operations: Ydb_Operation_V1.NewOperationServiceClient(conn)}
	response, err := s.client.CreateSession(ctx, &Ydb_Table.CreateSessionRequest{})
	if err != nil {
		return nil, fmt.Errorf("create YDB table session: transport failed")
	}
	var result Ydb_Table.CreateSessionResult
	if err := Operation(response.GetOperation(), &result); err != nil {
		return nil, err
	}
	if result.SessionId == "" {
		return nil, fmt.Errorf("YDB returned an empty table session")
	}
	s.id = result.SessionId
	return s, nil
}

// Close releases the native session with a separate bounded cleanup context.
func (s *Table) Close() error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	response, err := s.client.DeleteSession(ctx, &Ydb_Table.DeleteSessionRequest{SessionId: s.id})
	if err != nil {
		return fmt.Errorf("close YDB table session: transport failed")
	}
	return Status(response.GetOperation())
}

// Status requires a completed successful operation and omits native diagnostics.
func Status(op *Ydb_Operations.Operation) error {
	if !op.GetReady() || op.GetStatus() != Ydb.StatusIds_SUCCESS {
		return fmt.Errorf("YDB operation failed: ready=%t status=%s", op.GetReady(), op.GetStatus())
	}
	return nil
}

// Operation decodes a completed operation without dropping unknown fields.
func Operation(op *Ydb_Operations.Operation, target proto.Message) error {
	if err := Status(op); err != nil {
		return err
	}
	if err := op.GetResult().UnmarshalTo(target); err != nil {
		return fmt.Errorf("invalid YDB operation response")
	}
	return schema.Known(target)
}

// Describe uses provider-owned native definitions so omitted upstream binding
// fields cannot disappear during inspection or storage ownership checks.
func (s *Table) Describe(ctx context.Context, path string) (*native.DescribeTableResult, error) {
	response, err := s.client.DescribeTable(ctx, &Ydb_Table.DescribeTableRequest{SessionId: s.id, Path: path, IncludeSetVal: true})
	if err != nil {
		return nil, fmt.Errorf("describe YDB table: transport failed")
	}
	if err := Status(response.GetOperation()); err != nil {
		return nil, err
	}
	payload := response.GetOperation().GetResult()
	if !payload.MessageIs(&Ydb_Table.DescribeTableResult{}) {
		return nil, fmt.Errorf("invalid YDB table response type")
	}
	var result native.DescribeTableResult
	if err := proto.Unmarshal(payload.GetValue(), &result); err != nil {
		return nil, fmt.Errorf("invalid YDB table response")
	}
	if err := schema.Known(&result); err != nil {
		return nil, err
	}
	return &result, nil
}

// DescribeColumnTable reads the table service's description of a column table
// into the provider's column-table definition, which carries the tiered TTL
// the pinned bindings lack. Unknown fields are refused.
func (s *Table) DescribeColumnTable(ctx context.Context, path string) (*native.ColumnTableDescribeResult, error) {
	response, err := s.client.DescribeTable(ctx, &Ydb_Table.DescribeTableRequest{SessionId: s.id, Path: path})
	if err != nil {
		return nil, fmt.Errorf("describe YDB column table: transport failed")
	}
	if err := Status(response.GetOperation()); err != nil {
		return nil, err
	}
	payload := response.GetOperation().GetResult()
	if !payload.MessageIs(&Ydb_Table.DescribeTableResult{}) {
		return nil, fmt.Errorf("invalid YDB table response type")
	}
	var result native.ColumnTableDescribeResult
	if err := proto.Unmarshal(payload.GetValue(), &result); err != nil {
		return nil, fmt.Errorf("invalid YDB table response")
	}
	if err := schema.Known(&result); err != nil {
		return nil, err
	}
	return &result, nil
}

// Create sends one native create request, including its ownership attributes.
func (s *Table) Create(ctx context.Context, request *Ydb_Table.CreateTableRequest) error {
	request = proto.CloneOf(request)
	request.SessionId = s.id
	response, err := s.client.CreateTable(ctx, request)
	if err != nil {
		return fmt.Errorf("create YDB table: transport failed")
	}
	return Status(response.GetOperation())
}

// Alter sends one native alter request. It does not retry: a failed reply leaves
// the outcome unknown to the caller, which confirms by reading the table back.
//
// The server answers the addition of an index with a running operation, because
// it builds the index in the background. Alter then reads that operation until it
// finishes and requires it to have succeeded. The wait ends with the context.
// Any other request must complete in its reply.
func (s *Table) Alter(ctx context.Context, request *Ydb_Table.AlterTableRequest) error {
	request = proto.CloneOf(request)
	request.SessionId = s.id
	response, err := s.client.AlterTable(ctx, request)
	if err != nil {
		return fmt.Errorf("alter YDB table: transport failed")
	}
	operation := response.GetOperation()
	if operation.GetReady() || len(request.AddIndexes) == 0 || operation.GetId() == "" {
		return Status(operation)
	}
	return s.await(ctx, operation.GetId())
}

// operationPoll is how long await waits between reads of a running operation.
const operationPoll = 250 * time.Millisecond

// await reads a running operation until it is ready and requires it to have
// succeeded.
func (s *Table) await(ctx context.Context, id string) error {
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(cmp.Or(s.poll, operationPoll)):
		}
		response, err := s.operations.GetOperation(ctx, &Ydb_Operations.GetOperationRequest{Id: id})
		if err != nil {
			return fmt.Errorf("read YDB operation: transport failed")
		}
		if response.GetOperation().GetReady() {
			return Status(response.GetOperation())
		}
	}
}
