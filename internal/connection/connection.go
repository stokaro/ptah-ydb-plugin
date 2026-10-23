package connection

import (
	"context"
	"database/sql"
	"errors"
	"sync"
	"time"

	ydb "github.com/ydb-platform/ydb-go-sdk/v3"
)

// Connection owns the SQL pool and native scheme/table clients together.
// Closing the SQL connector alone does not close its parent YDB driver.
type Connection struct {
	DB       *sql.DB
	Driver   *ydb.Driver
	once     sync.Once
	closeErr error
}

// Open opens the native query-service connection. No schema is created.
func Open(ctx context.Context, address Address) (*Connection, error) {
	credentials, err := address.credentials()
	if err != nil {
		return nil, err
	}
	driver, err := ydb.Open(ctx, address.dsn, credentials, ydb.WithApplicationName("ptah-plugin-ydb"))
	if err != nil {
		return nil, err
	}
	c := &Connection{Driver: driver}
	connector, err := ydb.Connector(driver, ydb.WithQueryService(true))
	if err != nil {
		return nil, errors.Join(err, c.Close())
	}
	c.DB = sql.OpenDB(connector)
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
