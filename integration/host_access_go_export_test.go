//go:build integration

package integration_test

import (
	"context"
	"path"
	"regexp"
	"testing"
	"time"

	qt "github.com/frankban/quicktest"

	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
)

// A YDB read carries users, groups, memberships, and database and object
// grants into a Go declaration. Applying that declaration back to the same
// database must leave the access model untouched.
func TestHostGoExportCarriesYDBAccessModel_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	opened, directory, database := lockFixture(c)
	names := newAccessNames()
	removeAccess(c, opened, database, names)
	c.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		c.Check(execFixtureSchema(ctx, opened.DB, "DROP TABLE IF EXISTS `"+path.Join(database, directory, "orders")+"`"), qt.IsNil)
	})
	declared := accessDeclaration(directory, names, []string{"DATA-READERS", names.group}, false, "SELECT ROW")
	hostRun(c, directory, "the access", declared, `(?s).*Schema apply completed successfully.*`)

	source := introspectDirectory(c, directory)
	c.Assert(source, qt.Contains, `//ptah:schema:role name="`+names.group+`" group="true"`)
	c.Assert(source, qt.Contains, `//ptah:schema:role name="`+names.user+`" login="true" member_of="DATA-READERS,USERS,`+names.group+`"`)
	c.Assert(source, qt.Contains, `//ptah:schema:role name="`+names.blocked+`" member_of="USERS,`+names.group+`"`)
	c.Assert(source, qt.Contains, `//ptah:schema:grant role="`+names.user+`" privilege="YDB.DATABASE.CONNECT" on_database="true"`)
	c.Assert(source, qt.Contains, `//ptah:schema:grant role="`+names.group+`" privilege="YDB.GRANULAR.SELECT_ROW" on_table="`+directory+`.orders"`)
	c.Assert(hostPlanned(c, directory, "go", source), qt.HasLen, 0)
}

// A YDB object grant keeps the kind and qualified name of its topic or
// coordination node in a Go export.
func TestHostGoExportCarriesYDBObjectGrants_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	opened, directory, database := lockFixture(c)
	group := hclGrantGroup(c, opened.DB)
	topic := path.Join(database, directory, "events")
	node := path.Join(database, directory, "locks")
	createSchemaObject(c, opened, "CREATE TOPIC `"+topic+"`", "DROP TOPIC `"+topic+"`")
	createNode(c, opened, node)
	for _, statement := range []string{
		"GRANT 'ydb.generic.read' ON `" + topic + "` TO `" + group + "`",
		"GRANT 'ydb.generic.use' ON `" + node + "` TO `" + group + "`",
	} {
		c.Assert(execFixtureSchema(c.Context(), opened.DB, statement), qt.IsNil, qt.Commentf("%s", statement))
	}

	source := introspectDirectory(c, directory)
	c.Assert(source, qt.Contains, `//ptah:schema:grant role="`+group+`" privilege="YDB.GENERIC.READ" on_topic="`+directory+`.events"`)
	c.Assert(source, qt.Contains, `//ptah:schema:grant role="`+group+`" privilege="YDB.GENERIC.USE" on_coordination_node="`+directory+`.locks"`)
	c.Assert(hostPlanned(c, directory, "go", source), qt.HasLen, 0)
}

// The grant the server implicitly gives metadata readers when creating a
// secret is not a declaration to replay.
func TestHostGoExportLeavesImplicitSecretGrantOut_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	opened, directory, database := secretsFixture(c)
	secret := path.Join(database, directory, "pw")
	createSchemaObject(c, opened, "CREATE SECRET `"+secret+"` WITH (value = 'probe')", "DROP SECRET `"+secret+"`")

	source := introspectDirectory(c, directory)
	c.Assert(source, qt.Contains, `//ptah:schema:secret name="pw" schema="`+directory+`"`)
	c.Assert(source, qt.Not(qt.Matches), `(?m)^//ptah:schema:grant [^\n]*`+regexp.QuoteMeta(directory+`.pw`)+`[^\n]*$`)
	c.Assert(hostPlanned(c, directory, "go", source), qt.HasLen, 0)
}
