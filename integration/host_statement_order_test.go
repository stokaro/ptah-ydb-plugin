//go:build integration

package integration_test

import (
	"context"
	"os"
	"path"
	"slices"
	"strings"
	"testing"
	"time"

	qt "github.com/frankban/quicktest"
	"github.com/ydb-platform/ydb-go-sdk/v3/table/options"

	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
)

// statementsOn are the statements of output that create or alter an object
// below directory, each by the words that open it and the object it names, in
// output order: `CREATE TABLE a`, `ALTER TABLE a ADD CHANGEFEED`. A statement
// starts on the first line of its text that opens with CREATE or ALTER, so a
// note in front of it is no part of it.
func statementsOn(output, directory string) []string {
	var named []string
	for text := range strings.SplitSeq(output, ";") {
		lines := strings.Split(strings.TrimSpace(text), "\n")
		start := slices.IndexFunc(lines, func(line string) bool {
			return strings.HasPrefix(line, "CREATE ") || strings.HasPrefix(line, "ALTER ")
		})
		if start < 0 {
			continue
		}
		statement := strings.Join(lines[start:], "\n")
		marker := "`" + directory + "/"
		at := strings.Index(statement, marker)
		if at < 0 {
			continue
		}
		object, _, _ := strings.Cut(statement[at+len(marker):], "`")
		switch {
		case strings.HasPrefix(statement, "ALTER TABLE "):
			verb, _, _ := strings.Cut(strings.TrimSpace(statement[at+len(marker)+len(object)+1:]), " `")
			named = append(named, "ALTER TABLE "+object+" "+verb)
		default:
			verb, _, _ := strings.Cut(statement, " `")
			named = append(named, verb+" "+object)
		}
	}
	return named
}

// `schema inspect --format sql` places a read's statements as the built-in
// YDB engine at host 74de0b65b placed them, on 25.1.4.7 and 26.2.1.14: the
// topic ahead of the tables and each changefeed right after its table. The
// provider wrote the topic and the changefeeds after every table.
func TestHostInspectsStatementsInTheBuiltInOrder_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	opened, directory, database := inspectionFixture(c)
	absolute := func(name string) string { return path.Join(database, directory, name) }
	c.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		for _, statement := range []string{"DROP TABLE `" + absolute("a") + "`", "DROP TABLE `" + absolute("c") + "`", "DROP TOPIC `" + absolute("t") + "`"} {
			c.Check(execFixtureSchema(ctx, opened.DB, statement), qt.IsNil, qt.Commentf("%s", statement))
		}
	})
	for _, statement := range []string{
		"CREATE TABLE `" + absolute("a") + "` (id Uint64 NOT NULL, v Utf8, PRIMARY KEY (id))",
		"ALTER TABLE `" + absolute("a") + "` ADD CHANGEFEED feed_a WITH (MODE = 'UPDATES', FORMAT = 'JSON')",
		"CREATE TABLE `" + absolute("c") + "` (id Uint64 NOT NULL, v Utf8, PRIMARY KEY (id))",
		"ALTER TABLE `" + absolute("c") + "` ADD CHANGEFEED feed_c WITH (MODE = 'KEYS_ONLY', FORMAT = 'JSON')",
		"CREATE TOPIC `" + absolute("t") + "`",
	} {
		c.Assert(execFixtureSchema(c.Context(), opened.DB, statement), qt.IsNil, qt.Commentf("%s", statement))
	}

	output, err := hostRunAt(c, os.Getenv("PTAH_YDB_TEST_URL"), "schema", "inspect", "--schemas", directory, "--format", "sql")

	c.Assert(err, qt.IsNil, qt.Commentf("%s", output))
	c.Assert(statementsOn(output, directory), qt.DeepEquals, []string{
		"CREATE TOPIC t",
		"CREATE TABLE a", "ALTER TABLE a ADD CHANGEFEED",
		"CREATE TABLE c", "ALTER TABLE c ADD CHANGEFEED",
	}, qt.Commentf("%s", output))
}

// `schema apply` adds a new table's changefeed right after the table, as the
// built-in YDB engine at host 74de0b65b planned it on 25.1.4.7 and 26.2.1.14,
// and the server holds each table's changefeed. The provider added every
// changefeed after the last table.
func TestHostAppliesANewTablesChangefeedRightAfterIt_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	opened, directory, database := lockFixture(c)
	c.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		for _, name := range []string{"a", "c"} {
			c.Check(execFixtureSchema(ctx, opened.DB, "DROP TABLE `"+path.Join(database, directory, name)+"`"), qt.IsNil)
		}
	})
	table := func(name, mode string) string {
		return "  " + name + ":\n    schema: " + directory + "\n    columns:\n      id: {type: Uint64, primary: true}\n" +
			"    changefeeds:\n      feed_" + name + ":\n        mode: " + mode + "\n        format: JSON\n"
	}

	output, err := hostApplySource(c, directory, "yaml", "tables:\n"+table("a", "UPDATES")+table("c", "KEYS_ONLY"))

	c.Assert(err, qt.IsNil, qt.Commentf("%s", output))
	c.Assert(pluginUnits(output), qt.DeepEquals, []string{
		"row-table-create-operation " + directory + ".a", "changefeed-operation " + directory + ".a",
		"row-table-create-operation " + directory + ".c", "changefeed-operation " + directory + ".c",
	}, qt.Commentf("%s", output))
	held := make(map[string]options.ChangefeedMode)
	for _, name := range []string{"a", "c"} {
		feeds, found := describedChangefeeds(c, opened, path.Join(database, directory, name))
		c.Assert(found, qt.IsTrue)
		held[name] = feeds["feed_"+name].Mode
	}
	c.Assert(held, qt.DeepEquals, map[string]options.ChangefeedMode{"a": options.ChangefeedModeUpdates, "c": options.ChangefeedModeKeysOnly})
}
