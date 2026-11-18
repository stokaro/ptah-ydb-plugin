package yql_test

import (
	"testing"

	qt "github.com/frankban/quicktest"

	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
	"github.com/stokaro/ptah-ydb-plugin/internal/yql"
)

// liftedByTrust are statements a dev realm refuses only because their effects
// cannot be confined to it. A run whose operator trusts its SQL accepts each
// of them, in the realm at execution and in the replay guard of the dev
// database alike, and the strict guards refuse each one.
var liftedByTrust = []struct{ name, source string }{
	{"an absolute table", "CREATE TABLE `/local/outside` (id Int64 NOT NULL, PRIMARY KEY (id))"},
	{"a path that climbs out", "INSERT INTO `../outside` (id) VALUES (1)"},
	{"a rename out of the realm", "ALTER TABLE t RENAME TO `../renamed`"},
	{"a target named through an expression", "$p = \"/local/outside\"; INSERT INTO $p (id) VALUES (1)"},
	{"a user", "CREATE USER u1 PASSWORD 'p'"},
	{"a group", "CREATE GROUP g1"},
	{"a permission change", "GRANT SELECT ON `t` TO u1"},
	{"an action", "DEFINE ACTION $a() AS DROP TABLE `/local/a`; END DEFINE; DO $a()"},
	{"an evaluated statement", "EVALUATE FOR $i IN AsList(1) DO EMPTY_ACTION()"},
	{"a backup collection", "CREATE BACKUP COLLECTION `c` (TABLE `t`) WITH (STORAGE = 'cluster')"},
	{"an external data source", "CREATE EXTERNAL DATA SOURCE s WITH (SOURCE_TYPE = 'ObjectStorage', LOCATION = 'https://example.org/', AUTH_METHOD = 'NONE')"},
	{"an async replication", "CREATE ASYNC REPLICATION r FOR `a` AS `b` WITH (CONNECTION_STRING = 'grpc://example.org:2135/?database=/remote')"},
}

// keptUnderTrust are statements a dev realm refuses because they would change
// the session, the database's configuration or something the guard cannot
// read. Trust lifts none of them.
var keptUnderTrust = []struct{ name, source, operation string }{
	{"a pragma that moves the prefix", "PRAGMA TablePathPrefix = \"/local\"; CREATE TABLE t (id Int64 NOT NULL, PRIMARY KEY (id))", "PRAGMA TablePathPrefix"},
	{"a namespaced pragma that moves the prefix", "PRAGMA ydb.TablePathPrefix = \"/local\"", "PRAGMA TablePathPrefix"},
	{"USE", "USE ydb", "USE"},
	{"a translation setting", "--!ansi_lexer\nSELECT 1", "translation setting --!ansi_lexer"},
	{"a temporary table", "CREATE TEMPORARY TABLE t (id Int64 NOT NULL, PRIMARY KEY (id))", "temporary table"},
	{"a resource pool", "ALTER RESOURCE POOL default WITH (resource_weight = 50)", "a resource pool of the whole database"},
	{"an object of the metadata service", "CREATE OBJECT s (TYPE SECRET) WITH value = 'v'", "an object of the whole database"},
	{"a change to the whole database", "ALTER DATABASE `/local` OWNER TO u1", "a change to the whole database"},
	{"an unrecognized statement", "FROBNICATE t", "unrecognized statement FROBNICATE"},
	{"an unrecognized object", "CREATE WIDGET w", "unrecognized object CREATE WIDGET"},
	{"a kept refusal after a lifted one", "CREATE USER u1; USE ydb", "USE"},
}

func TestTrustedRealm_HappyPath(t *testing.T) {
	for _, test := range liftedByTrust {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			var strict *yql.RealmRefusal
			c.Assert(yql.ConfineToRealm(test.source), qt.ErrorAs, &strict)
			c.Assert(strict.Kept, qt.IsFalse)
			c.Assert(yql.GuardReplay(test.source), qt.ErrorAs, &strict)
			c.Assert(yql.ConfineTrustedToRealm(test.source), qt.IsNil)
			c.Assert(yql.GuardTrustedReplay(test.source), qt.IsNil)
		})
	}
}

func TestTrustedRealm_FailurePath(t *testing.T) {
	for _, test := range keptUnderTrust {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			var confined, guarded *yql.RealmRefusal

			c.Assert(yql.ConfineTrustedToRealm(test.source), qt.ErrorAs, &confined)
			c.Assert(yql.GuardTrustedReplay(test.source), qt.ErrorAs, &guarded)

			c.Assert(confined.Operation, qt.Equals, test.operation)
			c.Assert(confined.Kept, qt.IsTrue)
			c.Assert(guarded.Operation, qt.Equals, test.operation)
			c.Assert(guarded.Kept, qt.IsTrue)
		})
	}
}

// A run that owns the server and trusts its SQL keeps only the refusal of a
// translation setting: what reaches past the server, which the owned server's
// guard refuses on its own, is lifted too.
func TestTrustedServerReplay_HappyPath(t *testing.T) {
	for _, source := range []string{
		"CREATE EXTERNAL DATA SOURCE s WITH (SOURCE_TYPE = 'ObjectStorage', LOCATION = 'https://example.org/', AUTH_METHOD = 'NONE')",
		"CREATE TRANSFER x FROM `topic` TO `table` USING ($m) -> { RETURN $m; }",
		"CREATE USER u1 PASSWORD 'p'",
	} {
		t.Run(source, func(t *testing.T) {
			c := testcheck.New(t)
			c.Assert(yql.GuardTrustedServerReplay(source), qt.IsNil)
		})
	}
	c := testcheck.New(t)
	var refusal *yql.RealmRefusal
	c.Assert(yql.GuardServerReplay("CREATE EXTERNAL DATA SOURCE s WITH (SOURCE_TYPE = 'ObjectStorage')"), qt.ErrorAs, &refusal)
	c.Assert(refusal.Operation, qt.Equals, "an external data source")
}

func TestTrustedServerReplay_FailurePath(t *testing.T) {
	c := testcheck.New(t)
	var refusal *yql.RealmRefusal
	c.Assert(yql.GuardTrustedServerReplay("--!ansi_lexer\nSELECT 1"), qt.ErrorAs, &refusal)
	c.Assert(refusal.Operation, qt.Equals, "translation setting --!ansi_lexer")
	c.Assert(refusal.Kept, qt.IsTrue)
}
