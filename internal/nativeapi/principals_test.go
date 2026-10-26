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

func TestReadPrincipalsDoesNotReadPasswordState(t *testing.T) {
	c := qt.New(t)
	connector := principalFixture()
	db := sql.OpenDB(connector)
	c.Cleanup(func() { c.Check(db.Close(), qt.IsNil) })
	held, err := nativeapi.ReadPrincipals(c.Context(), db, "/local`\\tenant")
	c.Assert(err, qt.IsNil)
	c.Assert(held, qt.DeepEquals, nativeapi.Principals{
		Users: []nativeapi.User{{Name: "app", Enabled: true}}, Groups: []string{"DATA-READERS"},
		Memberships: []nativeapi.Membership{{Group: "DATA-READERS", Member: "app"}},
	})
	c.Assert(connector.queries, qt.DeepEquals, []string{
		"SELECT Sid, IsEnabled FROM `/local\\`\\\\tenant/.sys/auth_users`",
		"SELECT Sid FROM `/local\\`\\\\tenant/.sys/auth_groups`",
		"SELECT GroupSid, MemberSid FROM `/local\\`\\\\tenant/.sys/auth_group_members`",
	})
	for _, rows := range connector.results {
		c.Assert(rows.closed, qt.IsTrue)
	}
}

func TestReadPrincipalsRefusesAnIncompleteSnapshot(t *testing.T) {
	for _, test := range []struct {
		name   string
		change func(*principalConnector)
	}{
		{"missing login knowledge", func(f *principalConnector) { f.results[0].values[0][1] = nil }},
		{"missing user", func(f *principalConnector) { f.results[0].values[0][0] = nil }},
		{"duplicate user", func(f *principalConnector) { f.results[0].values = append(f.results[0].values, f.results[0].values[0]) }},
		{"shared namespace collision", func(f *principalConnector) { f.results[1].values[0][0] = "app" }},
		{"incomplete membership", func(f *principalConnector) { f.results[2].values[0][1] = nil }},
		{"duplicate membership", func(f *principalConnector) { f.results[2].values = append(f.results[2].values, f.results[2].values[0]) }},
		{"iteration failed", func(f *principalConnector) { f.results[2].end = errors.New("stream interrupted") }},
		{"close failed", func(f *principalConnector) { f.results[2].closeError = errors.New("close failed") }},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := qt.New(t)
			connector := principalFixture()
			test.change(connector)
			db := sql.OpenDB(connector)
			c.Cleanup(func() { c.Check(db.Close(), qt.IsNil) })
			held, err := nativeapi.ReadPrincipals(c.Context(), db, "/local")
			c.Assert(err, qt.IsNotNil)
			c.Assert(held, qt.DeepEquals, nativeapi.Principals{})
			c.Assert(errors.Is(err, nativeapi.ErrPrincipalsRefused), qt.IsFalse)
			for _, rows := range connector.results[:len(connector.queries)] {
				c.Assert(rows.closed, qt.IsTrue)
			}
		})
	}
}

func TestReadPrincipalsPreservesObservedSelfMembership(t *testing.T) {
	c := qt.New(t)
	connector := principalFixture()
	connector.results[2].values[0][1] = "DATA-READERS"
	db := sql.OpenDB(connector)
	c.Cleanup(func() { c.Check(db.Close(), qt.IsNil) })
	held, err := nativeapi.ReadPrincipals(c.Context(), db, "/local")
	c.Assert(err, qt.IsNil)
	c.Assert(held.Memberships, qt.DeepEquals, []nativeapi.Membership{{Group: "DATA-READERS", Member: "DATA-READERS"}})
}

func principalFixture() *principalConnector {
	return &principalConnector{results: []*principalRows{
		{columns: []string{"Sid", "IsEnabled"}, values: [][]driver.Value{{"app", true}}},
		{columns: []string{"Sid"}, values: [][]driver.Value{{"DATA-READERS"}}},
		{columns: []string{"GroupSid", "MemberSid"}, values: [][]driver.Value{{"DATA-READERS", "app"}}},
	}}
}

type principalConnector struct {
	queries []string
	results []*principalRows
}

func (c *principalConnector) Connect(context.Context) (driver.Conn, error) {
	return principalConn{c}, nil
}
func (*principalConnector) Driver() driver.Driver { return workloadReadDriver{} }

type principalConn struct{ connector *principalConnector }

func (principalConn) Prepare(string) (driver.Stmt, error) {
	return nil, errors.New("unexpected preparation")
}
func (principalConn) Close() error              { return nil }
func (principalConn) Begin() (driver.Tx, error) { return nil, errors.New("unexpected transaction") }
func (c principalConn) QueryContext(_ context.Context, query string, _ []driver.NamedValue) (driver.Rows, error) {
	i := len(c.connector.queries)
	c.connector.queries = append(c.connector.queries, query)
	return c.connector.results[i], nil
}

type principalRows struct {
	columns         []string
	values          [][]driver.Value
	end, closeError error
	closed          bool
}

func (r *principalRows) Columns() []string { return r.columns }
func (r *principalRows) Close() error      { r.closed = true; return r.closeError }
func (r *principalRows) Next(values []driver.Value) error {
	if len(r.values) == 0 {
		if r.end != nil {
			return r.end
		}
		return io.EOF
	}
	copy(values, r.values[0])
	r.values = r.values[1:]
	return nil
}
