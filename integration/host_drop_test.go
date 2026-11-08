//go:build integration

package integration_test

import (
	"context"
	"path"
	"testing"
	"time"

	qt "github.com/frankban/quicktest"
	"github.com/ydb-platform/ydb-go-sdk/v3/topic/topicoptions"
	"github.com/ydb-platform/ydb-go-sdk/v3/topic/topictypes"

	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
)

// The host creates a table through the provider and drops it once the source
// no longer declares it, and a later run finds nothing to do.
func TestHostDropsATableTheSourceLeavesOut_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	opened, directory, database := lockFixture(c)
	c.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		c.Check(execFixtureSchema(cleanup, opened.DB, "DROP TABLE IF EXISTS `"+path.Join(database, directory, "items")+"`"), qt.IsNil)
	})
	output, err := hostApply(c, directory, hostItems(directory, ""))
	c.Assert(err, qt.IsNil, qt.Commentf("%s", output))
	c.Assert(childNames(c, opened, path.Join(database, directory)), qt.Contains, "items")

	output, err = hostApply(c, directory, "package entities\n")
	c.Assert(err, qt.IsNil, qt.Commentf("%s", output))
	c.Assert(output, qt.Matches, `(?s).*table-drop-operation.*Schema apply completed successfully.*`)
	c.Assert(childNames(c, opened, path.Join(database, directory)), qt.Not(qt.Contains), "items")
}

// The host refuses to drop a table that holds a changefeed the read cannot
// represent, here one whose topic has a consumer with a custom codec, which
// Ptah does not model: the read marks the changefeed namespace incomplete,
// so a plan cannot establish what dropping the table takes with it.
// The host's embedded engine refused the drop.
// The table stays. Ported from the host's
// TestPlannerRefusesDropWithUnknownCapturedChildren.
func TestHostRefusesToDropATableWithAChangefeedItCannotRepresent_FailurePath(t *testing.T) {
	c := testcheck.New(t)
	opened, directory, database := lockFixture(c)
	absolute := path.Join(database, directory, "items")
	c.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		c.Check(execFixtureSchema(cleanup, opened.DB, "DROP TABLE IF EXISTS `"+absolute+"`"), qt.IsNil)
	})
	declared := "tables:\n  items:\n    schema: " + directory + "\n    columns:\n      id: {type: Uint64, primary: true, not_null: true}\n" +
		"    changefeeds:\n      updates:\n        mode: UPDATES\n        format: JSON\n"
	created, err := hostApplySource(c, directory, "yaml", declared)
	c.Assert(err, qt.IsNil, qt.Commentf("%s", created))
	// YDB keeps a custom codec number on a consumer, which no declaration
	// names, on 25.1.4.7 and 26.2.1.14; it overwrites a consumer's
	// attributes and truncates its read_from to the second.
	c.Assert(opened.Driver.Topic().Alter(c.Context(), absolute+"/updates",
		topicoptions.AlterWithAddConsumers(topictypes.Consumer{Name: "custom", SupportedCodecs: []topictypes.Codec{10042}})), qt.IsNil)

	refused, err := hostApplySource(c, directory, "yaml", "tables: {}\n")

	c.Assert(err, qt.IsNotNil, qt.Commentf("%s", refused))
	c.Assert(refused, qt.Contains, `dropping table "`+directory+`/items": the changefeed namespace is not fully described: `+
		`changefeed "updates" has settings this provider cannot represent, so the current changefeed was not established: `+
		`consumer "custom": its codec list names codec 10042, which Ptah does not declare`)
	c.Assert(childNames(c, opened, path.Join(database, directory)), qt.Contains, "items")
}
