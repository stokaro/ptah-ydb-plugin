//go:build integration

package integration_test

import (
	"context"
	"path"
	"strings"
	"testing"
	"time"

	qt "github.com/frankban/quicktest"

	"github.com/stokaro/ptah-ydb-plugin/internal/connection"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
)

// viewGrant is whether viewBesideTopic grants the group a privilege on the
// view.
type viewGrant int

const (
	// noViewGrant declares no grant.
	noViewGrant viewGrant = iota
	// groupViewGrant grants the group SELECT ROW on the view.
	groupViewGrant
)

// viewBesideTopic declares, in directory, a table src, a view active over it
// with the comment given, a topic events with the retention given, and the
// group of names, with the grant to the group on the view that grant names.
func viewBesideTopic(directory string, names accessNames, comment, retention string, grant viewGrant) string {
	text := "tables:\n  src:\n    schema: " + directory + "\n    columns:\n      id: {type: Int64, primary: true, not_null: true}\n" +
		"views:\n  active:\n    name: " + directory + ".active\n    body: \"SELECT id FROM `" + directory + "/src`\"\n"
	if comment != "" {
		text += "    comment: " + comment + "\n"
	}
	text += "topics:\n  events:\n    schema: " + directory + "\n    retention_period: " + retention + "\n" +
		"roles:\n  " + names.group + ":\n    group: true\n"
	if grant == groupViewGrant {
		text += "grants:\n  view:\n    role: " + names.group + "\n    privileges: [SELECT ROW]\n    on_table: " + directory + ".active\n"
	}
	return text
}

// viewTopicFixture is a locked fixture directory and the run's principals,
// with the table, the view and the topic of viewBesideTopic dropped when the
// test ends.
func viewTopicFixture(c *qt.C) (opened *connection.Connection, directory, database string, names accessNames) {
	c.Helper()
	opened, directory, database = lockFixture(c)
	names = newAccessNames()
	removeAccess(c, opened, database, names)
	c.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		emptyTree(ctx, opened, path.Join(database, directory))
	})
	return opened, directory, database, names
}

// privilegesOf are the permissions entries give role, in order; heldView
// writes an entry as the role and the permission.
func privilegesOf(entries []string, role string) []string {
	var privileges []string
	for _, entry := range entries {
		if privilege, found := strings.CutPrefix(entry, role+" "); found {
			privileges = append(privileges, privilege)
		}
	}
	return privileges
}

// The host changes a topic's retention and, in the same run, comments on a
// view or grants on it, as its engine plans both in one run (G128). The view
// is common, the topic is this provider's, and the comment or the grant
// changes a resource of the view's own, which occupies no path, so the plan
// is not refused for its footprint. Each run leaves nothing to plan, and the
// server holds the new retention and the comment or the entry.
func TestHostChangesAViewBesideATopic_HappyPath(t *testing.T) {
	for _, test := range []struct {
		name, comment string
		grant         viewGrant
		attributes    map[string]string
		privileges    []string
	}{
		{name: "a comment on the view", comment: "Active ids", attributes: map[string]string{"ptah.comment": "Active ids"}},
		{name: "a grant on the view", grant: groupViewGrant, attributes: make(map[string]string), privileges: []string{"ydb.granular.select_row"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			opened, directory, database, names := viewTopicFixture(c)
			view := path.Join(database, directory, "active")
			applyRuns(c, directory, "yaml", viewBesideTopic(directory, names, "", "PT36H", noViewGrant))

			applyRuns(c, directory, "yaml", viewBesideTopic(directory, names, test.comment, "P2D", test.grant))

			topic, found := describedTopic(c, opened, database, directory, "events")
			c.Assert(found, qt.IsTrue)
			c.Assert(topic.Retention, qt.Equals, 48*time.Hour)
			c.Assert(storedAttributes(c, opened, view), qt.DeepEquals, test.attributes)
			_, entries := heldView(c, opened, view)
			c.Assert(privilegesOf(entries, names.group), qt.DeepEquals, test.privileges)
		})
	}
}
