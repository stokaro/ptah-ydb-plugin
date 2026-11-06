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

// grantsOn are the GRANT statements of output on an object below directory,
// each as the object and the role, in output order. A statement starts on
// the first line of its text that opens with GRANT, so a note in front of it
// is no part of it.
func grantsOn(output, directory string) []string {
	var grants []string
	for text := range strings.SplitSeq(output, ";") {
		lines := strings.Split(strings.TrimSpace(text), "\n")
		start := slices.IndexFunc(lines, func(line string) bool { return strings.HasPrefix(line, "GRANT ") })
		if start < 0 {
			continue
		}
		statement := strings.Join(lines[start:], "\n")
		_, target, found := strings.Cut(statement, " ON `"+directory+"/")
		if !found {
			continue
		}
		object, role, _ := strings.Cut(target, "` TO `")
		grants = append(grants, object+" "+strings.TrimSuffix(role, "`"))
	}
	return grants
}

// `schema inspect --format sql` writes the grants of an object in the order
// YDB describes its entries, which is the order they were granted, as the
// built-in YDB engine at host 74de0b65b wrote them on 25.1.4.7 and 26.2.1.14.
// The provider sorted them by role, so a role granted later was written
// first.
func TestHostInspectsGrantsInTheOrderYDBDescribesThem_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	opened, directory, database := inspectionFixture(c)
	suffix := strings.ToLower(rand.Text())[:8]
	first, second := "ptahordz"+suffix, "ptahorda"+suffix
	table := path.Join(database, directory, "a")
	c.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		for _, statement := range []string{"DROP TABLE `" + table + "`", "DROP USER IF EXISTS `" + first + "`", "DROP USER IF EXISTS `" + second + "`"} {
			c.Check(execFixtureSchema(ctx, opened.DB, statement), qt.IsNil, qt.Commentf("%s", statement))
		}
	})
	for _, statement := range []string{
		"CREATE TABLE `" + table + "` (id Uint64 NOT NULL, v Utf8, PRIMARY KEY (id))",
		"CREATE USER `" + first + "`",
		"CREATE USER `" + second + "`",
		"GRANT SELECT ROW ON `" + table + "` TO `" + first + "`",
		"GRANT SELECT ROW ON `" + table + "` TO `" + second + "`",
	} {
		c.Assert(execFixtureSchema(c.Context(), opened.DB, statement), qt.IsNil, qt.Commentf("%s", statement))
	}

	output, err := hostRunAt(c, os.Getenv("PTAH_YDB_TEST_URL"), "schema", "inspect", "--schemas", directory, "--format", "sql")

	c.Assert(err, qt.IsNil, qt.Commentf("%s", output))
	c.Assert(grantsOn(output, directory), qt.DeepEquals, []string{"a " + first, "a " + second}, qt.Commentf("%s", output))
}
