//go:build integration

package integration_test

import (
	"context"
	"crypto/rand"
	"os"
	"path"
	"slices"
	"strings"
	"testing"
	"time"

	qt "github.com/frankban/quicktest"

	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
)

// principalCreations lists the CREATE USER and CREATE GROUP statements of a
// schema render that create one of names, each as its first words and the
// principal, such as `CREATE GROUP readers`, in the order the render wrote
// them.
func principalCreations(output string, names []string) []string {
	var created []string
	for line := range strings.SplitSeq(output, "\n") {
		for _, verb := range []string{"CREATE USER", "CREATE GROUP"} {
			rest, found := strings.CutPrefix(line, verb+" `")
			name, _, _ := strings.Cut(rest, "`")
			if found && slices.Contains(names, name) {
				created = append(created, verb+" "+name)
			}
		}
	}
	return created
}

// `schema inspect --format sql` writes the users and the groups together in
// name order, as the YDB engine built into the host at 74de0b65b wrote them
// on 25.1.4.7 and 26.2.1.14. The provider wrote every user ahead of every
// group: a user named after a group came first.
func TestHostInspectsPrincipalsInNameOrder_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	opened, directory, database := inspectionFixture(c)
	prefix := "ptahnames" + strings.ToLower(rand.Text())[:6]
	users, groups := []string{prefix + "z", prefix + "a"}, []string{prefix + "y", prefix + "b"}
	c.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		emptyTree(ctx, opened, path.Join(database, directory))
		for _, user := range users {
			c.Check(execFixtureSchema(ctx, opened.DB, "DROP USER IF EXISTS `"+user+"`"), qt.IsNil)
		}
		for _, group := range groups {
			c.Check(execFixtureSchema(ctx, opened.DB, "DROP GROUP IF EXISTS `"+group+"`"), qt.IsNil)
		}
	})
	table := "`" + path.Join(database, directory, "t") + "`"
	statements := []string{"CREATE TABLE " + table + " (id Uint64 NOT NULL, PRIMARY KEY (id))",
		"CREATE USER `" + users[0] + "` PASSWORD 'unused'", "CREATE GROUP `" + groups[0] + "`",
		"CREATE USER `" + users[1] + "` PASSWORD 'unused'", "CREATE GROUP `" + groups[1] + "`",
		"ALTER GROUP `" + groups[1] + "` ADD USER `" + users[0] + "`"}
	for _, principal := range slices.Concat(users, groups) {
		statements = append(statements, "GRANT SELECT ROW ON "+table+" TO `"+principal+"`")
	}
	for _, statement := range statements {
		c.Assert(execFixtureSchema(c.Context(), opened.DB, statement), qt.IsNil, qt.Commentf("%s", statement))
	}

	output, err := hostRunAt(c, os.Getenv("PTAH_YDB_TEST_URL"), "schema", "inspect", "--schemas", directory, "--format", "sql")

	c.Assert(err, qt.IsNil, qt.Commentf("%s", output))
	c.Assert(principalCreations(output, slices.Concat(users, groups)), qt.DeepEquals, []string{
		"CREATE USER " + users[1], "CREATE GROUP " + groups[1], "CREATE GROUP " + groups[0], "CREATE USER " + users[0],
	}, qt.Commentf("%s", output))
	c.Assert(output, qt.Contains, "ALTER GROUP `"+groups[1]+"` ADD USER `"+users[0]+"`;")
}
