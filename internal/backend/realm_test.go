package backend

// White-box testing required: a session on a dev realm is built from a live
// connection, and the confinement it applies before dispatch is observable
// without one only through the unexported session.

import (
	"testing"

	qt "github.com/frankban/quicktest"
	"github.com/stokaro/ptah-ydb-plugin/internal/connection"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
)

func realmProvider(c *qt.C, probe *driverProbe) *Provider {
	c.Helper()
	p := probeProvider(c, probe)
	p.sessions["session"].realm = true
	return p
}

func sqlUnits(statements ...string) *protocol.ExecuteRequest {
	request := &protocol.ExecuteRequest{SessionId: "session", RequestId: "write"}
	for _, statement := range statements {
		request.Units = append(request.Units, &protocol.Unit{ChangeId: statement, Body: &protocol.Unit_Sql{Sql: statement}})
	}
	return request
}

func TestRealmSessionExecutesConfinedStatements(t *testing.T) {
	c := qt.New(t)
	probe := &driverProbe{}
	p := realmProvider(c, probe)

	result, err := p.Execute(c.Context(), sqlUnits("CREATE TABLE `app/t` (id Int64 NOT NULL, PRIMARY KEY (id))", "INSERT INTO `app/t` (id) VALUES (1)"))

	c.Assert(err, qt.IsNil)
	c.Assert(result.Outcome, qt.Equals, protocol.Execution_APPLIED)
	c.Assert(result.AppliedCount, qt.Equals, uint32(2))
	c.Assert(probe.execCalls.Load(), qt.Equals, int32(2))
}

func TestRealmSessionRefusesAnEscapeBeforeAnyUnitRuns(t *testing.T) {
	for _, test := range []struct{ name, escape, message string }{
		{"absolute path", "DROP TABLE `/local/t`", "YDB dev realm session rejects the absolute path /local/t because its effects cannot be confined to the dev realm"},
		{"climbing path", "CREATE TABLE `../t` (id Int64 NOT NULL, PRIMARY KEY (id))", "YDB dev realm session rejects the path ../t, which climbs out of the dev realm because its effects cannot be confined to the dev realm"},
		{"prefix change", "PRAGMA TablePathPrefix = \"/local\"", "YDB dev realm session rejects PRAGMA TablePathPrefix because its effects cannot be confined to the dev realm"},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := qt.New(t)
			probe := &driverProbe{}
			p := realmProvider(c, probe)

			result, err := p.Execute(c.Context(), sqlUnits("SELECT 1", test.escape))

			c.Assert(err, qt.IsNil)
			c.Assert(result.Outcome, qt.Equals, protocol.Execution_NOT_STARTED)
			c.Assert(result.AppliedCount, qt.Equals, uint32(0))
			c.Assert(result.DiagnosticCode, qt.Equals, "ydb.realm-escape")
			c.Assert(result.Message, qt.Equals, test.message)
			c.Assert(probe.execCalls.Load(), qt.Equals, int32(0))
		})
	}
}

func TestSessionOutsideARealmExecutesWhatItIsGiven(t *testing.T) {
	c := qt.New(t)
	probe := &driverProbe{}
	p := probeProvider(c, probe)

	result, err := p.Execute(c.Context(), sqlUnits("DROP TABLE `/local/t`"))

	c.Assert(err, qt.IsNil)
	c.Assert(result.Outcome, qt.Equals, protocol.Execution_APPLIED)
	c.Assert(probe.execCalls.Load(), qt.Equals, int32(1))
}

func TestSessionLeavesTheRealmDirectoryOutOfWhatItReads(t *testing.T) {
	for _, test := range []struct {
		name  string
		realm bool
		want  []string
	}{
		{"database", false, []string{connection.RealmDirectory}},
		{"realm", true, nil},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := qt.New(t)
			s := &session{realm: test.realm}
			c.Assert(s.leftOut(), qt.DeepEquals, test.want)
		})
	}
}
