// Package locking owns YDB's coordination sessions and lease freshness checks.
package locking

import (
	"context"
	"errors"
	"fmt"
	"path"
	"time"

	"github.com/stokaro/ptah-ydb-plugin/internal/authored"
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

// ErrRelaxedNode marks a lock node that does not read and attach with strict
// consistency. Nothing was asked of its semaphores, so nothing is held. The
// message names the node and the settings it needs.
var ErrRelaxedNode = errors.New("YDB lock node is not strict")

// ErrRefused marks an acquisition the server refused this user before any
// lock was taken. The message names the step that failed and the right the
// user lacks.
var ErrRefused = errors.New("YDB refused this user Ptah's lock")

// refused is a refusal in the words of Ptah's embedded YDB engine
// (internal/dblockext/ydb). It matches ErrRefused, and its text is the reason
// a host shows: it names the coordination node and the right, never a value.
func refused(format string, args ...any) error {
	return authored.Wrap(ErrRefused, fmt.Sprintf(format, args...))
}

// Try makes a nonblocking semaphore acquisition within a new native session.
// A stream failure ends ownership; the provider never reconnects or reacquires.
// Busy is true only after a confirmed refusal and confirmed cleanup.
func Try(ctx context.Context, driver *ydb.Driver, database, name string) (*Hold, bool, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	node := path.Join(database, NodeName)
	if err := ensureNode(ctx, driver, node); err != nil {
		return nil, false, err
	}
	session, err := openSession(ctx, driver, node)
	if err != nil {
		return nil, false, err
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

// ensureNode finds the lock node, or creates it with strict consistency.
// It describes the node's path rather than listing the database root, so a
// user who may use the node but not list the root takes the lock: measured on
// 25.1.4.7 and 26.2.1.14, a user granted only on the node is refused the
// root's listing (UNAUTHORIZED), and a path that does not exist answers
// SCHEME_ERROR to any user who may connect.
func ensureNode(ctx context.Context, driver *ydb.Driver, node string) error {
	response, err := Ydb_Scheme_V1.NewSchemeServiceClient(ydb.GRPCConn(driver)).DescribePath(ctx, &Ydb_Scheme.DescribePathRequest{Path: node})
	if err != nil {
		return fmt.Errorf("describe YDB lock node failed")
	}
	if operation := response.GetOperation(); operation.GetReady() {
		switch operation.GetStatus() {
		case Ydb.StatusIds_SCHEME_ERROR:
			return createNode(ctx, driver, node)
		case Ydb.StatusIds_UNAUTHORIZED:
			return refused("this user may not use the YDB coordination node %s, which holds Ptah's locks (UNAUTHORIZED); "+
				"grant the user the right to use it", node)
		}
	}
	var described Ydb_Scheme.DescribePathResult
	if err := nativeapi.Operation(response.GetOperation(), &described); err != nil {
		return err
	}
	if described.GetSelf().GetName() != NodeName || described.GetSelf().GetType() != Ydb_Scheme.Entry_COORDINATION_NODE {
		return fmt.Errorf("YDB lock path is not a coordination node")
	}
	return validateNode(ctx, driver, node)
}

// createNode creates the lock node a description found missing. A node made
// by another run in between is taken as found.
func createNode(ctx context.Context, driver *ydb.Driver, node string) error {
	service := Ydb_Coordination_V1.NewCoordinationServiceClient(ydb.GRPCConn(driver))
	created, err := service.CreateNode(ctx, &Ydb_Coordination.CreateNodeRequest{Path: node, Config: &Ydb_Coordination.Config{
		ReadConsistencyMode:   Ydb_Coordination.ConsistencyMode_CONSISTENCY_MODE_STRICT,
		AttachConsistencyMode: Ydb_Coordination.ConsistencyMode_CONSISTENCY_MODE_STRICT,
	}})
	if err != nil {
		return fmt.Errorf("create YDB lock node failed")
	}
	operation := created.GetOperation()
	if operation.GetReady() && operation.GetStatus() == Ydb.StatusIds_UNAUTHORIZED {
		return refused("Ptah's locks live on the YDB coordination node %s, which does not exist, and this user may not "+
			"create it (UNAUTHORIZED). Run once as a user who may create a coordination node at the database root, "+
			"which creates it, and grant this user the right to use it", node)
	}
	if err := nativeapi.Status(operation); err != nil && operation.GetStatus() != Ydb.StatusIds_ALREADY_EXISTS {
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
	if description.GetSelf().GetName() != NodeName || description.GetSelf().GetType() != Ydb_Scheme.Entry_COORDINATION_NODE {
		return fmt.Errorf("YDB lock path is not a coordination node")
	}
	if description.GetConfig().GetReadConsistencyMode() != Ydb_Coordination.ConsistencyMode_CONSISTENCY_MODE_STRICT ||
		description.GetConfig().GetAttachConsistencyMode() != Ydb_Coordination.ConsistencyMode_CONSISTENCY_MODE_STRICT {
		// Ptah's built-in YDB engine creates the node with neither mode set,
		// which YDB reads as relaxed reads and strict attachment.
		return authored.Wrap(ErrRelaxedNode, fmt.Sprintf("the YDB coordination node %s, which holds Ptah's locks, reads with "+
			"read consistency %s and attach consistency %s, and a lock taken there cannot be shown to be still held. Ptah's "+
			"built-in YDB engine creates the node without either setting. YDB's CLI and YQL have no command for a coordination "+
			"node: set both modes to strict with the coordination service's AlterNode call, or remove the node with its DropNode "+
			"call while no run holds a lock on it, and the provider creates it strict", node,
			consistencyName(description.GetConfig().GetReadConsistencyMode()), consistencyName(description.GetConfig().GetAttachConsistencyMode())))
	}
	return nil
}

// consistencyName is how a refusal names a consistency mode: unset, relaxed
// or strict.
func consistencyName(mode Ydb_Coordination.ConsistencyMode) string {
	switch mode {
	case Ydb_Coordination.ConsistencyMode_CONSISTENCY_MODE_STRICT:
		return "strict"
	case Ydb_Coordination.ConsistencyMode_CONSISTENCY_MODE_RELAXED:
		return "relaxed"
	case Ydb_Coordination.ConsistencyMode_CONSISTENCY_MODE_UNSET:
		return "unset"
	default:
		return mode.String()
	}
}
