//go:build integration

package integration_test

import (
	"context"
	"path"
	"slices"
	"strings"
	"testing"
	"time"

	qt "github.com/frankban/quicktest"
	"github.com/stokaro/ptah-ydb-plugin/internal/connection"
	"github.com/ydb-platform/ydb-go-sdk/v3/table"
	"github.com/ydb-platform/ydb-go-sdk/v3/table/options"
)

// feedShape is what the server holds of one changefeed: its mode, its topic's
// retention, and each consumer, as its name and whether it is important.
type feedShape struct {
	Mode      options.ChangefeedMode
	Retention time.Duration
	Consumers []string
}

// describedChangefeeds reads the changefeeds of a table back through the
// table and topic services, by name, or reports that the table is absent.
func describedChangefeeds(c *qt.C, opened *connection.Connection, absolute string) (map[string]feedShape, bool) {
	c.Helper()
	var described options.Description
	err := opened.Driver.Table().Do(c.Context(), func(ctx context.Context, s table.Session) error {
		var err error
		described, err = s.DescribeTable(ctx, absolute)
		return err
	}, table.WithIdempotent())
	if err != nil {
		return nil, false
	}
	result := map[string]feedShape{}
	for _, feed := range described.Changefeeds {
		topic, err := opened.Driver.Topic().Describe(c.Context(), path.Join(absolute, feed.Name))
		c.Assert(err, qt.IsNil)
		shape := feedShape{Mode: feed.Mode, Retention: topic.RetentionPeriod}
		for _, consumer := range topic.Consumers {
			label := consumer.Name
			if consumer.Important {
				label += " important"
			}
			shape.Consumers = append(shape.Consumers, label)
		}
		slices.Sort(shape.Consumers)
		result[feed.Name] = shape
	}
	return result, true
}

// The host plans the changefeeds of a table through this provider from each
// source that declares them: it creates the table and adds its changefeeds
// with their consumers, finds nothing to change on the next run, changes a
// topic's retention and consumers in place and drops and adds a changefeed
// whose mode changes, and drops the changefeeds once the source declares the
// table without them. Every source describes the changefeeds of each table it
// declares completely.
func TestHostAppliesChangefeeds_HappyPath(t *testing.T) {
	for _, test := range []struct {
		format                 string
		created, changed, gone string
	}{
		{format: "yaml",
			created: "tables:\n  items:\n    schema: {dir}\n    columns:\n      id: {type: Uint64, primary: true, not_null: true}\n    changefeeds:\n" +
				"      updates:\n        mode: UPDATES\n        format: JSON\n        retention_period: PT12H\n        consumers:\n          billing: {important: true}\n          audit: {}\n" +
				"      keys:\n        mode: KEYS_ONLY\n        format: JSON\n",
			changed: "tables:\n  items:\n    schema: {dir}\n    columns:\n      id: {type: Uint64, primary: true, not_null: true}\n    changefeeds:\n" +
				"      updates:\n        mode: UPDATES\n        format: JSON\n        retention_period: P2D\n        consumers:\n          billing: {}\n          fresh: {}\n" +
				"      keys:\n        mode: NEW_IMAGE\n        format: JSON\n",
			gone: "tables:\n  items:\n    schema: {dir}\n    columns:\n      id: {type: Uint64, primary: true, not_null: true}\n"},
		{format: "go",
			created: "package entities\n\n//ptah:schema:table name=\"items\" schema=\"{dir}\"\n" +
				"//ptah:schema:changefeed name=\"updates\" mode=\"UPDATES\" format=\"JSON\" retention_period=\"PT12H\"\n" +
				"//ptah:schema:changefeed:consumer name=\"billing\" changefeed=\"updates\" important=\"true\"\n" +
				"//ptah:schema:changefeed:consumer name=\"audit\" changefeed=\"updates\"\n" +
				"//ptah:schema:changefeed name=\"keys\" mode=\"KEYS_ONLY\" format=\"JSON\"\n" +
				"type Item struct {\n\t//ptah:schema:field name=\"id\" type=\"Uint64\" primary=\"true\" not_null=\"true\"\n\tID uint64\n}\n",
			changed: "package entities\n\n//ptah:schema:table name=\"items\" schema=\"{dir}\"\n" +
				"//ptah:schema:changefeed name=\"updates\" mode=\"UPDATES\" format=\"JSON\" retention_period=\"P2D\"\n" +
				"//ptah:schema:changefeed:consumer name=\"billing\" changefeed=\"updates\"\n" +
				"//ptah:schema:changefeed:consumer name=\"fresh\" changefeed=\"updates\"\n" +
				"//ptah:schema:changefeed name=\"keys\" mode=\"NEW_IMAGE\" format=\"JSON\"\n" +
				"type Item struct {\n\t//ptah:schema:field name=\"id\" type=\"Uint64\" primary=\"true\" not_null=\"true\"\n\tID uint64\n}\n",
			gone: "package entities\n\n" + goItems},
	} {
		t.Run(test.format, func(t *testing.T) {
			c := qt.New(t)
			opened, directory, database := lockFixture(c)
			absolute := path.Join(database, directory, "items")
			c.Cleanup(func() {
				cleanup, cancel := context.WithTimeout(context.Background(), time.Minute)
				defer cancel()
				c.Check(execFixtureSchema(cleanup, opened.DB, "DROP TABLE IF EXISTS `"+absolute+"`"), qt.IsNil)
			})
			apply := func(name, text, want string) {
				c.Helper()
				output, err := hostApplySource(c, directory, test.format, strings.ReplaceAll(text, "{dir}", directory))
				c.Assert(err, qt.IsNil, qt.Commentf("%s: %s", name, output))
				c.Assert(output, qt.Matches, want, qt.Commentf("%s: %s", name, output))
				c.Assert(output, qt.Not(qt.Contains), "comparison limit", qt.Commentf("%s", name))
			}
			apply("creation", test.created, `(?s).*Schema apply completed successfully.*`)
			shapes, found := describedChangefeeds(c, opened, absolute)
			c.Assert(found, qt.IsTrue)
			c.Assert(shapes, qt.DeepEquals, map[string]feedShape{
				"updates": {Mode: options.ChangefeedModeUpdates, Retention: 12 * time.Hour, Consumers: []string{"audit", "billing important"}},
				"keys":    {Mode: options.ChangefeedModeKeysOnly, Retention: 24 * time.Hour},
			})
			apply("a second run", test.created, `(?s).*Schema is synced, no changes to be made.*`)
			apply("a change", test.changed, `(?s).*Schema apply completed successfully.*`)
			shapes, found = describedChangefeeds(c, opened, absolute)
			c.Assert(found, qt.IsTrue)
			c.Assert(shapes, qt.DeepEquals, map[string]feedShape{
				"updates": {Mode: options.ChangefeedModeUpdates, Retention: 48 * time.Hour, Consumers: []string{"billing", "fresh"}},
				"keys":    {Mode: options.ChangefeedModeNewImage, Retention: 24 * time.Hour},
			})
			apply("a run after the change", test.changed, `(?s).*Schema is synced, no changes to be made.*`)
			apply("the removal", strings.ReplaceAll(test.gone, "{dir}", directory), `(?s).*Schema apply completed successfully.*`)
			shapes, found = describedChangefeeds(c, opened, absolute)
			c.Assert(found, qt.IsTrue)
			c.Assert(shapes, qt.HasLen, 0)
			apply("a run after the removal", test.gone, `(?s).*Schema is synced, no changes to be made.*`)
		})
	}
}
