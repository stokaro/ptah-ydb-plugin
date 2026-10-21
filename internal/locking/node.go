// Package locking owns YDB's coordination sessions and lease freshness checks.
package locking

import (
	"context"
	"errors"
	"fmt"
	"path"
	"time"

	"github.com/stokaro/ptah-ydb-plugin/internal/nativeapi"
	"github.com/ydb-platform/ydb-go-genproto/Ydb_Coordination_V1"
	"github.com/ydb-platform/ydb-go-genproto/Ydb_Scheme_V1"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb_Coordination"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb_Scheme"
	ydb "github.com/ydb-platform/ydb-go-sdk/v3"
)

// NodeName is the database-root coordination node reserved for migration locks.
const NodeName = "ptah_locks"
const sessionTimeout = 10 * time.Second
const freshness = sessionTimeout / 2
const checkInterval = time.Second

// Try makes a nonblocking semaphore acquisition within a new native session.
// A stream failure ends ownership; the provider never reconnects or reacquires.
// Busy is true only after a confirmed refusal and confirmed cleanup.
func Try(ctx context.Context, driver *ydb.Driver, database, name string) (*Hold, bool, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	node := path.Join(database, NodeName)
	if err := ensureNode(ctx, driver, database, node); err != nil {
		return nil, false, err
	}
	session, err := openSession(ctx, driver, node)
	if err != nil {
		return nil, false, fmt.Errorf("open YDB lock session failed")
	}
	asked := time.Now()
	acquired, err := session.acquire(ctx, name)
	if err != nil || !acquired {
		cleanup, stop := context.WithTimeout(context.Background(), 10*time.Second)
		defer stop()
		closed := session.Close(cleanup)
		if err == nil && !acquired && closed == nil {
			return nil, true, nil
		}
		return nil, false, fmt.Errorf("acquire YDB semaphore or close its session failed")
	}
	h := newHold(session, session, name, asked, time.Now, func(ctx context.Context) error { return validateNode(ctx, driver, node) })
	if err := h.Check(ctx); err != nil {
		cleanup, stop := context.WithTimeout(context.Background(), 10*time.Second)
		defer stop()
		return nil, false, errors.Join(err, h.Close(cleanup))
	}
	h.start()
	return h, false, nil
}

func ensureNode(ctx context.Context, driver *ydb.Driver, database, node string) error {
	client := Ydb_Scheme_V1.NewSchemeServiceClient(ydb.GRPCConn(driver))
	response, err := client.ListDirectory(ctx, &Ydb_Scheme.ListDirectoryRequest{Path: database})
	if err != nil {
		return fmt.Errorf("list YDB lock namespace failed")
	}
	var directory Ydb_Scheme.ListDirectoryResult
	if err := nativeapi.Operation(response.GetOperation(), &directory); err != nil {
		return err
	}
	if directory.GetSelf().GetName() != path.Base(database) ||
		(directory.GetSelf().GetType() != Ydb_Scheme.Entry_DATABASE && directory.GetSelf().GetType() != Ydb_Scheme.Entry_DIRECTORY) || len(directory.Children) > 4096 {
		return fmt.Errorf("invalid YDB lock namespace")
	}
	for _, entry := range directory.Children {
		if entry.GetName() == NodeName {
			if entry.GetType() != Ydb_Scheme.Entry_COORDINATION_NODE {
				return fmt.Errorf("YDB lock path is not a coordination node")
			}
			return validateNode(ctx, driver, node)
		}
	}
	service := Ydb_Coordination_V1.NewCoordinationServiceClient(ydb.GRPCConn(driver))
	created, err := service.CreateNode(ctx, &Ydb_Coordination.CreateNodeRequest{Path: node, Config: &Ydb_Coordination.Config{
		ReadConsistencyMode:   Ydb_Coordination.ConsistencyMode_CONSISTENCY_MODE_STRICT,
		AttachConsistencyMode: Ydb_Coordination.ConsistencyMode_CONSISTENCY_MODE_STRICT,
	}})
	if err != nil {
		return fmt.Errorf("create YDB lock node failed")
	}
	if err := nativeapi.Status(created.GetOperation()); err != nil && created.GetOperation().GetStatus() != Ydb.StatusIds_ALREADY_EXISTS {
		return err
	}
	return validateNode(ctx, driver, node)
}

func validateNode(ctx context.Context, driver *ydb.Driver, node string) error {
	service := Ydb_Coordination_V1.NewCoordinationServiceClient(ydb.GRPCConn(driver))
	response, err := service.DescribeNode(ctx, &Ydb_Coordination.DescribeNodeRequest{Path: node})
	if err != nil {
		return fmt.Errorf("describe YDB lock node failed")
	}
	var description Ydb_Coordination.DescribeNodeResult
	if err := nativeapi.Operation(response.GetOperation(), &description); err != nil {
		return err
	}
	if description.GetSelf().GetName() != NodeName || description.GetSelf().GetType() != Ydb_Scheme.Entry_COORDINATION_NODE ||
		description.GetConfig().GetReadConsistencyMode() != Ydb_Coordination.ConsistencyMode_CONSISTENCY_MODE_STRICT ||
		description.GetConfig().GetAttachConsistencyMode() != Ydb_Coordination.ConsistencyMode_CONSISTENCY_MODE_STRICT {
		return fmt.Errorf("YDB lock node requires strict read and attach consistency")
	}
	return nil
}
