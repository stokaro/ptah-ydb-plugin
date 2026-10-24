package connection

// White-box testing required: the prefix connector wraps the SDK's connection
// type, which is unexported, so the queries it sends are observable only
// through a stand-in for that connection.

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"testing"

	qt "github.com/frankban/quicktest"
)

// recordedQueries is what a stand-in SDK connection received.
type recordedQueries struct {
	exec, query, prepare []string
}

type fakeSDKConn struct{ seen *recordedQueries }

func (c fakeSDKConn) Prepare(query string) (driver.Stmt, error) {
	c.seen.prepare = append(c.seen.prepare, query)
	return nil, driver.ErrSkip
}
func (fakeSDKConn) Close() error              { return nil }
func (fakeSDKConn) Begin() (driver.Tx, error) { return nil, driver.ErrSkip }
func (fakeSDKConn) BeginTx(context.Context, driver.TxOptions) (driver.Tx, error) {
	return nil, driver.ErrSkip
}
func (c fakeSDKConn) PrepareContext(_ context.Context, query string) (driver.Stmt, error) {
	c.seen.prepare = append(c.seen.prepare, query)
	return nil, driver.ErrSkip
}
func (c fakeSDKConn) QueryContext(_ context.Context, query string, _ []driver.NamedValue) (driver.Rows, error) {
	c.seen.query = append(c.seen.query, query)
	return nil, driver.ErrSkip
}
func (c fakeSDKConn) ExecContext(_ context.Context, query string, _ []driver.NamedValue) (driver.Result, error) {
	c.seen.exec = append(c.seen.exec, query)
	return driver.RowsAffected(0), nil
}
func (fakeSDKConn) Ping(context.Context) error               { return nil }
func (fakeSDKConn) CheckNamedValue(*driver.NamedValue) error { return nil }
func (fakeSDKConn) ResetSession(context.Context) error       { return nil }
func (fakeSDKConn) IsValid() bool                            { return true }

type fakeConnector struct{ seen *recordedQueries }

func (c fakeConnector) Connect(context.Context) (driver.Conn, error) {
	return fakeSDKConn{seen: c.seen}, nil
}
func (fakeConnector) Driver() driver.Driver { return nil }

func TestPrefixConnector_HappyPath(t *testing.T) {
	c := qt.New(t)
	seen := &recordedQueries{}
	db := sql.OpenDB(prefixConnector{inner: fakeConnector{seen: seen}, prefix: pathPrefix("/local/ptah_dev/r1")})
	c.Cleanup(func() { c.Check(db.Close(), qt.IsNil) })

	_, err := db.ExecContext(c.Context(), "CREATE TABLE t (id Int64 NOT NULL, PRIMARY KEY (id))")
	c.Assert(err, qt.IsNil)
	_, _ = db.QueryContext(c.Context(), "SELECT 1")

	const prefix = "PRAGMA TablePathPrefix(\"/local/ptah_dev/r1\");\n"
	c.Assert(seen.exec, qt.DeepEquals, []string{prefix + "CREATE TABLE t (id Int64 NOT NULL, PRIMARY KEY (id))"})
	c.Assert(seen.query, qt.DeepEquals, []string{prefix + "SELECT 1"})
}

func TestPathPrefix(t *testing.T) {
	for _, test := range []struct{ name, root, want string }{
		{"plain", "/local/ptah_dev/r1", "PRAGMA TablePathPrefix(\"/local/ptah_dev/r1\");\n"},
		{"quote", "/lo\"cal", "PRAGMA TablePathPrefix(\"/lo\\\"cal\");\n"},
		{"backslash", "/lo\\cal", "PRAGMA TablePathPrefix(\"/lo\\\\cal\");\n"},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := qt.New(t)
			c.Assert(pathPrefix(test.root), qt.Equals, test.want)
		})
	}
}
