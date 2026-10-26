package nativeapi_test

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"io"
	"testing"

	qt "github.com/frankban/quicktest"
	"github.com/stokaro/ptah-ydb-plugin/internal/nativeapi"
)

// The database is an identifier even when its name contains a quote or a
// backslash. Neither can terminate a system-view path and become YQL syntax.
func TestReadWorkloadQuotesDatabase(t *testing.T) {
	t.Parallel()
	c := qt.New(t)
	connector := &workloadReadConnector{}
	db := sql.OpenDB(connector)
	c.Cleanup(func() { c.Check(db.Close(), qt.IsNil) })
	held, err := nativeapi.ReadWorkload(c.Context(), db, "/local`\\tenant")
	c.Assert(err, qt.IsNil)
	c.Assert(held.Pools, qt.HasLen, 0)
	c.Assert(held.Classifiers, qt.HasLen, 0)
	c.Assert(connector.queries, qt.DeepEquals, []string{
		"SELECT Name, ConcurrentQueryLimit, QueueSize, DatabaseLoadCpuThreshold, QueryMemoryLimitPercentPerNode, QueryCpuLimitPercentPerNode, TotalCpuLimitPercentPerNode, ResourceWeight FROM `/local\\`\\\\tenant/.sys/resource_pools`",
		"SELECT Name, Rank, MemberName, ResourcePool FROM `/local\\`\\\\tenant/.sys/resource_pool_classifiers`",
	})
}

type workloadReadConnector struct{ queries []string }

func (c *workloadReadConnector) Connect(context.Context) (driver.Conn, error) {
	return workloadReadConn{connector: c}, nil
}

func (*workloadReadConnector) Driver() driver.Driver { return workloadReadDriver{} }

type workloadReadDriver struct{}

func (workloadReadDriver) Open(string) (driver.Conn, error) {
	return nil, errors.New("use the connector")
}

type workloadReadConn struct{ connector *workloadReadConnector }

func (workloadReadConn) Prepare(string) (driver.Stmt, error) {
	return nil, errors.New("prepared statements are not expected")
}

func (workloadReadConn) Close() error { return nil }

func (workloadReadConn) Begin() (driver.Tx, error) {
	return nil, errors.New("transactions are not expected")
}

func (c workloadReadConn) QueryContext(_ context.Context, query string, _ []driver.NamedValue) (driver.Rows, error) {
	c.connector.queries = append(c.connector.queries, query)
	return workloadReadRows{}, nil
}

type workloadReadRows struct{}

func (workloadReadRows) Columns() []string         { return nil }
func (workloadReadRows) Close() error              { return nil }
func (workloadReadRows) Next([]driver.Value) error { return io.EOF }
