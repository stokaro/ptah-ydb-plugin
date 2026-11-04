//go:build integration

package integration_test

import (
	"context"
	"path"
	"testing"
	"time"

	qt "github.com/frankban/quicktest"
	"github.com/stokaro/ptah-ydb-plugin/internal/connection"
	"github.com/stokaro/ptah-ydb-plugin/internal/nativeapi"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
)

// hostCommentedUsers declares users in directory as YAML, with a comment on
// the table, on its key and on its index users_by_label, the last one given.
func hostCommentedUsers(directory, indexComment string) string {
	index := ""
	if indexComment != "" {
		index = "    indexes:\n      users_by_label: {fields: [label], comment: " + indexComment + "}\n"
	}
	return "tables:\n  users:\n    schema: " + directory + "\n    comment: People who sign in\n    columns:\n" +
		"      id: {type: BIGINT, primary: true, comment: Key}\n      label: {type: TEXT}\n" + index
}

// heldAttributes reads the user attributes of a table, where YDB keeps
// Ptah's comments.
func heldAttributes(c *qt.C, opened *connection.Connection, absolute string) map[string]string {
	c.Helper()
	session, err := nativeapi.OpenTable(c.Context(), opened.Driver)
	c.Assert(err, qt.IsNil)
	defer func() { c.Check(session.Close(), qt.IsNil) }()
	described, err := session.Describe(c.Context(), absolute)
	c.Assert(err, qt.IsNil)
	return described.GetAttributes()
}

// The host sets the comments of a table it creates through the provider, of
// its column and of its index, in the same run, and finds the table as
// declared on the next. A changed index comment is one change, and an index
// the declaration drops takes its comment with it.
func TestHostSetsTheCommentsOfACreatedTable_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	opened, directory, database := lockFixture(c)
	absolute := path.Join(database, directory, "users")
	c.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		c.Check(execFixtureSchema(cleanup, opened.DB, "DROP TABLE IF EXISTS `"+absolute+"`"), qt.IsNil)
	})
	const (
		applied = `(?s).*Schema apply completed successfully.*`
		synced  = `(?s).*Schema is synced, no changes to be made.*`
	)
	table := map[string]string{"ptah.comment": "People who sign in", "ptah.comment.column.id": "Key"}
	withIndex := func(comment string) map[string]string {
		result := map[string]string{"ptah.comment.index.users_by_label": comment}
		for key, value := range table {
			result[key] = value
		}
		return result
	}
	for _, run := range []struct {
		name, comment, want string
		held                map[string]string
	}{
		{"creation", "Lookup by name", applied, withIndex("Lookup by name")},
		{"a second run", "Lookup by name", synced, withIndex("Lookup by name")},
		{"the index comment changed", "By label", applied, withIndex("By label")},
		{"a run after the change", "By label", synced, withIndex("By label")},
		{"the index dropped", "", applied, table},
		{"a run after the drop", "", synced, table},
	} {
		output, err := hostApplySource(c, directory, "yaml", hostCommentedUsers(directory, run.comment))
		c.Assert(err, qt.IsNil, qt.Commentf("%s: %s", run.name, output))
		c.Assert(output, qt.Matches, run.want, qt.Commentf("%s", run.name))
		c.Assert(heldAttributes(c, opened, absolute), qt.DeepEquals, run.held, qt.Commentf("%s", run.name))
	}
}

// A Go index directive's comment is set on the index of a table the host
// creates, as a YAML one is.
func TestHostSetsTheIndexCommentOfACreatedTableFromGo_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	opened, directory, database := lockFixture(c)
	absolute := path.Join(database, directory, "users")
	c.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		c.Check(execFixtureSchema(cleanup, opened.DB, "DROP TABLE IF EXISTS `"+absolute+"`"), qt.IsNil)
	})
	source := "package entities\n\n//ptah:schema:table name=\"users\" schema=\"" + directory + "\"\ntype User struct {\n" +
		"\t//ptah:schema:field name=\"id\" type=\"BIGINT\" primary=\"true\"\n\tID int64\n" +
		"\t//ptah:schema:field name=\"label\" type=\"TEXT\"\n\t//ptah:schema:index name=\"users_by_label\" fields=\"label\" comment=\"Lookup by name\"\n\tLabel string\n}\n"
	for _, run := range []struct{ name, want string }{
		{"creation", `(?s).*Schema apply completed successfully.*`},
		{"a second run", `(?s).*Schema is synced, no changes to be made.*`},
	} {
		output, err := hostApply(c, directory, source)
		c.Assert(err, qt.IsNil, qt.Commentf("%s: %s", run.name, output))
		c.Assert(output, qt.Matches, run.want, qt.Commentf("%s", run.name))
		c.Assert(heldAttributes(c, opened, absolute), qt.DeepEquals, map[string]string{"ptah.comment.index.users_by_label": "Lookup by name"}, qt.Commentf("%s", run.name))
	}
}
