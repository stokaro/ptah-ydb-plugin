package connection

import (
	"context"
	"database/sql/driver"
	"fmt"
	"io"

	"github.com/stokaro/ptah-ydb-plugin/internal/ydbview"
	ydb "github.com/ydb-platform/ydb-go-sdk/v3"
)

// pathPrefix is the pragma every query of a session on a dev realm starts
// with. It names the realm's directory, so a relative path in any statement
// resolves inside the realm. The pragma does not confine an absolute path, a
// path that climbs out with `..` or a table named through a `$` expression;
// internal/yql refuses those before a statement is sent.
//
// YDB stores the pragma in the query text of a view the session creates, so
// the spelling is internal/ydbview's, where the view reader removes it.
func pathPrefix(root string) string { return ydbview.RealmPrefix(root) }

// sdkConn is what database/sql uses of a ydb-go-sdk connection. The SDK's
// connection type is internal, so the set is checked when a connection is made:
// a release that drops one of them fails there instead of database/sql quietly
// taking another path.
type sdkConn interface {
	driver.Conn
	driver.ConnBeginTx
	driver.ConnPrepareContext
	driver.QueryerContext
	driver.ExecerContext
	driver.Pinger
	driver.NamedValueChecker
	driver.SessionResetter
	driver.Validator
}

// prefixConnector hands database/sql the SDK's connections with a pragma in
// front of every query.
type prefixConnector struct {
	inner  driver.Connector
	prefix string
}

var _ io.Closer = prefixConnector{}

func (c prefixConnector) Connect(ctx context.Context) (driver.Conn, error) {
	opened, err := c.inner.Connect(ctx)
	if err != nil {
		return nil, err
	}
	sdk, ok := opened.(sdkConn)
	if !ok {
		_ = opened.Close()
		return nil, fmt.Errorf("the YDB driver's connection %T does not offer what the dev realm prefix needs", opened)
	}
	return prefixConn{sdkConn: sdk, prefix: c.prefix}, nil
}

func (c prefixConnector) Driver() driver.Driver { return c.inner.Driver() }

// Close closes the SDK connector.
func (c prefixConnector) Close() error {
	if closer, ok := c.inner.(io.Closer); ok {
		return closer.Close()
	}
	return nil
}

// prefixConn is an SDK connection whose queries start with prefix.
type prefixConn struct {
	sdkConn
	prefix string
}

func (c prefixConn) ExecContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	return c.sdkConn.ExecContext(ctx, c.prefix+query, args)
}

func (c prefixConn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	return c.sdkConn.QueryContext(ctx, c.prefix+query, args)
}

func (c prefixConn) PrepareContext(ctx context.Context, query string) (driver.Stmt, error) {
	return c.sdkConn.PrepareContext(ctx, c.prefix+query)
}

func (c prefixConn) Prepare(query string) (driver.Stmt, error) {
	return c.sdkConn.Prepare(c.prefix + query)
}

// ensureRealm creates the realm's directory and the directories above it when
// they are missing. A realm is the run's own directory, so a session that
// names one creates it; creating one that exists succeeds.
func ensureRealm(ctx context.Context, driver *ydb.Driver, root string) error {
	return driver.Scheme().MakeDirectory(ctx, root)
}
