package connection

import (
	"context"
	"database/sql"
	sqldriver "database/sql/driver"
	"errors"
	"net/url"
	"sync"
	"time"

	ydb "github.com/ydb-platform/ydb-go-sdk/v3"
	"github.com/ydb-platform/ydb-go-sdk/v3/config"
	"google.golang.org/grpc"
)

// Connection owns the SQL pool and native scheme/table clients together.
// Closing the SQL connector alone does not close its parent YDB driver.
type Connection struct {
	DB       *sql.DB
	Driver   *ydb.Driver
	once     sync.Once
	closeErr error
	// database is the database the driver opened; a dev realm is a
	// directory inside it.
	database string
	// monitoring is the endpoint the address names, nil for none.
	monitoring *url.URL
	// secure reports a connection over TLS.
	secure bool
	// authenticated reports an address that names a credential source.
	authenticated bool
	// ticket returns the credential the monitoring pages are read with:
	// Ticket for a connection Open made.
	ticket func(context.Context) (string, error)
}

// Open opens the native query-service connection. No schema is created.
func Open(ctx context.Context, address Address) (*Connection, error) {
	credentials, err := address.credentials()
	if err != nil {
		return nil, err
	}
	driver, err := ydb.Open(ctx, address.dsn, credentials, ydb.WithApplicationName("ptah-plugin-ydb"),
		ydb.With(config.WithGrpcOptions(grpc.WithChainUnaryInterceptor(recordTicket))))
	if err != nil {
		return nil, err
	}
	c := &Connection{Driver: driver, database: address.database, monitoring: address.Monitoring(), secure: address.secure, authenticated: address.authenticated()}
	c.ticket = c.Ticket
	connector, err := ydb.Connector(driver, ydb.WithQueryService(true))
	if err != nil {
		return nil, errors.Join(err, c.Close())
	}
	var opened sqldriver.Connector = connector
	if address.realm != "" {
		opened = prefixConnector{inner: connector, prefix: pathPrefix(address.Root())}
		if err := ensureRealm(ctx, driver, address.Root()); err != nil {
			return nil, errors.Join(err, c.Close())
		}
	}
	c.DB = sql.OpenDB(opened)
	if err := c.DB.PingContext(ctx); err != nil {
		return nil, errors.Join(err, c.Close())
	}
	return c, nil
}

// Close releases both owners once. Cleanup has a separate bounded context so
// cancellation of the RPC cannot abandon the driver's sessions or sockets.
func (c *Connection) Close() error {
	c.once.Do(func() {
		var poolErr error
		if c.DB != nil {
			poolErr = c.DB.Close()
		}
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		c.closeErr = errors.Join(poolErr, c.Driver.Close(ctx))
	})
	return c.closeErr
}
