package devclean

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/ydb-platform/ydb-go-genproto/Ydb_Scheme_V1"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb_Scheme"
	ydb "github.com/ydb-platform/ydb-go-sdk/v3"

	"github.com/stokaro/ptah-ydb-plugin/internal/connection"
	"github.com/stokaro/ptah-ydb-plugin/internal/nativeapi"
)

// Native is the scheme service and the statement runner of an open
// connection. Each statement runs once, on a connection of its own, so
// database/sql cannot replay it on another. before runs ahead of every
// statement and drop, and refuses the step when the session's locks are not
// held.
type Native struct {
	connection *connection.Connection
	scheme     Ydb_Scheme_V1.SchemeServiceClient
	before     func(context.Context) error
}

// NewNative returns the cleanup operations of opened. A nil before checks
// nothing.
func NewNative(opened *connection.Connection, before func(context.Context) error) Native {
	if before == nil {
		before = func(context.Context) error { return nil }
	}
	return Native{connection: opened, scheme: Ydb_Scheme_V1.NewSchemeServiceClient(ydb.GRPCConn(opened.Driver)), before: before}
}

// ListDirectory lists a directory. A path that does not exist is ErrScheme.
func (n Native) ListDirectory(ctx context.Context, absolute string) ([]*Ydb_Scheme.Entry, error) {
	response, err := n.scheme.ListDirectory(ctx, &Ydb_Scheme.ListDirectoryRequest{Path: absolute})
	if err != nil {
		return nil, fmt.Errorf("list the YDB directory %s: transport failed", absolute)
	}
	if response.GetOperation().GetStatus() == Ydb.StatusIds_SCHEME_ERROR {
		return nil, fmt.Errorf("%w: list the YDB directory %s", ErrScheme, absolute)
	}
	var listed Ydb_Scheme.ListDirectoryResult
	if err := nativeapi.Operation(response.GetOperation(), &listed); err != nil {
		return nil, err
	}
	return listed.GetChildren(), nil
}

// RemoveDirectory removes an empty directory. One the server refuses, because
// it does not exist or is not empty, is ErrScheme.
func (n Native) RemoveDirectory(ctx context.Context, absolute string) error {
	if err := n.before(ctx); err != nil {
		return err
	}
	response, err := n.scheme.RemoveDirectory(ctx, &Ydb_Scheme.RemoveDirectoryRequest{Path: absolute})
	if err != nil {
		return fmt.Errorf("remove the YDB directory %s: transport failed", absolute)
	}
	if response.GetOperation().GetStatus() == Ydb.StatusIds_SCHEME_ERROR {
		return fmt.Errorf("%w: remove the YDB directory %s", ErrScheme, absolute)
	}
	return nativeapi.Status(response.GetOperation())
}

// Execute runs one schema statement. A scheme error is ErrNotVisible, which
// a drop of a table the listing named runs again for a while.
func (n Native) Execute(ctx context.Context, statement string) (err error) {
	if err := n.before(ctx); err != nil {
		return err
	}
	conn, err := n.connection.DB.Conn(ctx)
	if err != nil {
		return err
	}
	defer func() {
		if closeErr := conn.Close(); closeErr != nil && !errors.Is(closeErr, sql.ErrConnDone) {
			err = errors.Join(err, closeErr)
		}
	}()
	if _, err := conn.ExecContext(ctx, statement); err != nil {
		if ydb.IsOperationErrorSchemeError(err) {
			return fmt.Errorf("%w: YDB answered SCHEME_ERROR", ErrNotVisible)
		}
		return fmt.Errorf("a cleanup statement failed: %s", statusName(err))
	}
	return nil
}

// DropNode drops a coordination node through the coordination service.
func (n Native) DropNode(ctx context.Context, absolute string) error {
	if err := n.before(ctx); err != nil {
		return err
	}
	if err := n.connection.Driver.Coordination().DropNode(ctx, absolute); err != nil {
		return fmt.Errorf("drop the coordination node %s failed", absolute)
	}
	return nil
}

// statusName names a failure by its YDB status, never by its text, which can
// quote a statement.
func statusName(err error) string {
	if operation := ydb.OperationError(err); operation != nil {
		return "YDB answered " + operation.Name()
	}
	if transport := ydb.TransportError(err); transport != nil {
		return "the transport answered " + transport.Name()
	}
	return "the connection failed"
}
