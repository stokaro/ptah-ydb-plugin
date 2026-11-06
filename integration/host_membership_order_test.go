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

// statementsNaming are the first lines of the statements of output that name
// one of the quoted names given, in output order. A statement starts on the
// first line of its text that opens with a statement keyword, so a note in
// front of it is no part of it.
func statementsNaming(output string, names ...string) []string {
	var named []string
	for text := range strings.SplitSeq(output, ";") {
		lines := strings.Split(strings.TrimSpace(text), "\n")
		start := slices.IndexFunc(lines, func(line string) bool {
			return slices.ContainsFunc([]string{"CREATE ", "ALTER ", "GRANT ", "REVOKE ", "DROP "}, func(keyword string) bool { return strings.HasPrefix(line, keyword) })
		})
		if start < 0 || !slices.ContainsFunc(names, func(name string) bool { return strings.Contains(text, name) }) {
			continue
		}
		named = append(named, strings.TrimSpace(lines[start]))
	}
	return named
}

// `schema inspect --format sql` writes a group's membership right after the
// roles, ahead of the tables, as the built-in YDB engine at host 74de0b65b
// wrote it on 25.1.4.7 and 26.2.1.14. Through the provider the host wrote
// every `ALTER GROUP ... ADD USER` after the tables.
func TestHostInspectsAMembershipWithTheRoles_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	opened, directory, database := inspectionFixture(c)
	suffix := strings.ToLower(rand.Text())[:8]
	user, group := "ptahordu"+suffix, "ptahordg"+suffix
	table := path.Join(database, directory, "t")
	c.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		for _, statement := range []string{"DROP TABLE `" + table + "`", "DROP GROUP IF EXISTS `" + group + "`", "DROP USER IF EXISTS `" + user + "`"} {
			c.Check(execFixtureSchema(ctx, opened.DB, statement), qt.IsNil, qt.Commentf("%s", statement))
		}
	})
	for _, statement := range []string{
		"CREATE TABLE `" + table + "` (id Uint64 NOT NULL, PRIMARY KEY (id))",
		"CREATE USER `" + user + "`",
		"CREATE GROUP `" + group + "`",
		"ALTER GROUP `" + group + "` ADD USER `" + user + "`",
	} {
		c.Assert(execFixtureSchema(c.Context(), opened.DB, statement), qt.IsNil, qt.Commentf("%s", statement))
	}

	output, err := hostRunAt(c, os.Getenv("PTAH_YDB_TEST_URL"), "schema", "inspect", "--schemas", directory, "--format", "sql")

	c.Assert(err, qt.IsNil, qt.Commentf("%s", output))
	c.Assert(statementsNaming(output, "`"+group+"`", "`"+directory+"/"), qt.DeepEquals, []string{
		"CREATE GROUP `" + group + "`",
		"ALTER GROUP `" + group + "` ADD USER `" + user + "`",
		"CREATE TABLE `" + directory + "/t` (",
	}, qt.Commentf("%s", output))
}
