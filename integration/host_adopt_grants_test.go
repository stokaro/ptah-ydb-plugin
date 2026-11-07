//go:build integration

package integration_test

import (
	"context"
	"crypto/rand"
	"path"
	"strings"
	"testing"
	"time"

	qt "github.com/frankban/quicktest"
	"github.com/ydb-platform/ydb-go-sdk/v3/coordination"

	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
)

// adoptedGrantsDocument declares the user {user}, and the table orders and the
// coordination node workers in the directory {dir}, with the grant of each to
// the user that the database also holds.
const adoptedGrantsDocument = `schema "{dir}" {
}

role "{user}" {
  login = true
}

table "orders" {
  schema = schema.{dir}
  column "id" {
    type = Uint64
  }
  primary_key {
    columns = [column.id]
  }
}

coordination_node "workers" {
  schema = schema.{dir}
}

permission {
  to = role.{user}
  for = table.orders
  privileges = ["YDB.GENERIC.READ"]
}

permission {
  to = role.{user}
  for = coordination_node.{dir}.workers
  privileges = ["YDB.GRANULAR.DESCRIBE_SCHEMA"]
}
`

// Ported from the host's TestAdoptObjectGrants_HappyPath. An HCL document
// cannot name a topic, so it makes no claim about the entries a role holds on
// one, and applying it back leaves them in place: the plan revokes nothing on
// the topic audit. An object the document declares is the document's to
// describe: of the two entries the user holds on the coordination node
// workers, the one the document does not name is revoked, and the one it
// names, like the table's, is kept. The host test also left out an entry YDB
// gave a secret it created; this provider never reports such an entry, so it
// has nothing to leave out here.
func TestHostHCLKeepsGrantsOnObjectsItCannotName_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	opened, directory, database := lockFixture(c)
	emptyAtCleanup(c, opened, path.Join(database, directory))
	user := "ptahadopt" + strings.ToLower(rand.Text())[:10]
	topic := path.Join(database, directory, "audit")
	table := path.Join(database, directory, "orders")
	node := path.Join(database, directory, "workers")
	c.Assert(opened.Driver.Coordination().CreateNode(c.Context(), node, coordination.NodeConfig{}), qt.IsNil)
	for _, statement := range []string{
		"CREATE USER `" + user + "` PASSWORD 'p'",
		"CREATE TABLE `" + table + "` (id Uint64 NOT NULL, PRIMARY KEY (id))",
		"CREATE TOPIC `" + topic + "`",
		"GRANT 'ydb.generic.read' ON `" + topic + "` TO `" + user + "`",
		"GRANT 'ydb.generic.read' ON `" + table + "` TO `" + user + "`",
		"GRANT 'ydb.granular.describe_schema' ON `" + node + "` TO `" + user + "`",
		"GRANT 'ydb.generic.use' ON `" + node + "` TO `" + user + "`",
	} {
		c.Assert(execFixtureSchema(c.Context(), opened.DB, statement), qt.IsNil, qt.Commentf("%s", statement))
	}
	c.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		c.Check(execFixtureSchema(ctx, opened.DB, "DROP TOPIC `"+topic+"`"), qt.IsNil)
		c.Check(execFixtureSchema(ctx, opened.DB, "DROP USER IF EXISTS `"+user+"`"), qt.IsNil)
	})
	document := strings.NewReplacer("{dir}", directory, "{user}", user).Replace(adoptedGrantsDocument)

	planned := hostPlanned(c, directory, "hcl", document)

	c.Assert(planned, qt.DeepEquals, []string{"REVOKE 'ydb.generic.use' ON `" + directory + "/workers` FROM `" + user + "`"})
}

// skipWithoutHCLGroups skips a test on a host whose HCL role block takes no
// group attribute, which a host without stokaro/ptah#5778 refuses by name.
func skipWithoutHCLGroups(c *qt.C) {
	c.Helper()
	file := compatFile(c, c.TempDir(), "group.hcl", "role \"g\" {\n  group = true\n}\n")
	out, err := hostCommand(c, "schema", "render", "--schema-file", file, "--dialect", "ydb")
	if err != nil && strings.Contains(out, `unsupported role attribute "group"`) {
		c.Skip("the host's HCL role block takes no group attribute (stokaro/ptah#5778)")
	}
	c.Assert(err, qt.IsNil, qt.Commentf("%s", out))
}

// The HCL a host exports for a YDB group writes it as a group and each user's
// groups as member_of, so the document applied back to the database it came
// from plans nothing. Without the two attributes the document read the group
// back as a user, which YDB cannot turn into a group, and dropped every
// membership (stokaro/ptah#5778). Found porting the same host test as the one
// above: an HCL round trip of a grant to a group refused before it reached the
// grant.
func TestHostHCLRoundTripsAGroupAndItsMembers_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	skipWithoutHCLGroups(c)
	opened, directory, database := lockFixture(c)
	emptyAtCleanup(c, opened, path.Join(database, directory))
	group := hclGrantGroup(c, opened.DB)
	user := group + "u"
	table := path.Join(database, directory, "orders")
	c.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		c.Check(execFixtureSchema(ctx, opened.DB, "DROP USER IF EXISTS `"+user+"`"), qt.IsNil)
	})
	for _, statement := range []string{
		"CREATE USER `" + user + "` PASSWORD 'p'",
		"ALTER GROUP `" + group + "` ADD USER `" + user + "`",
		"CREATE TABLE `" + table + "` (id Uint64 NOT NULL, PRIMARY KEY (id))",
		"GRANT 'ydb.generic.read' ON `" + table + "` TO `" + group + "`",
	} {
		c.Assert(execFixtureSchema(c.Context(), opened.DB, statement), qt.IsNil, qt.Commentf("%s", statement))
	}

	exported, _ := hostExport(c, "schema", "inspect", "--schemas", directory, "--format", "hcl")
	planned := hostPlanned(c, directory, "hcl", exported)

	c.Assert(exported, qt.Contains, "role \""+group+"\" {\n  inherit = true\n  group = true\n}\n")
	c.Assert(exported, qt.Matches, `(?s).*role "`+user+`" \{\n  login = true\n  inherit = true\n  member_of = \[[^\]]*role\.`+group+`[^\]]*\]\n\}.*`)
	c.Assert(planned, qt.HasLen, 0)
}
