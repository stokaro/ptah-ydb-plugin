//go:build integration

package integration_test

import (
	"context"
	"path"
	"testing"
	"time"

	qt "github.com/frankban/quicktest"
	"github.com/stokaro/ptah-ydb-plugin/internal/connection"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
	"github.com/ydb-platform/ydb-go-sdk/v3/table"
	"github.com/ydb-platform/ydb-go-sdk/v3/table/options"
)

// hostIndexedItems declares items in the fixture directory with index
// items_ts on ts, and the given further attributes of its index directive.
func hostIndexedItems(directory, attributes string) string {
	return "package entities\n\n//ptah:schema:table name=\"items\" schema=\"" + directory + "\"\n" +
		"type Item struct {\n\t//ptah:schema:field name=\"id\" type=\"Uint64\" primary=\"true\" not_null=\"true\"\n\tID uint64\n" +
		"\t//ptah:schema:field name=\"ts\" type=\"Timestamp\"\n\t//ptah:schema:index name=\"items_ts\" fields=\"ts\"" + attributes + "\n\tTS int64\n}\n"
}

// heldIndexKinds reads the kind of each index a table holds back from the
// table service, by name.
func heldIndexKinds(c *qt.C, opened *connection.Connection, absolute string) map[string]options.IndexType {
	c.Helper()
	var described options.Description
	err := opened.Driver.Table().Do(c.Context(), func(ctx context.Context, s table.Session) error {
		var err error
		described, err = s.DescribeTable(ctx, absolute)
		return err
	}, table.WithIdempotent())
	c.Assert(err, qt.IsNil)
	result := make(map[string]options.IndexType, len(described.Indexes))
	for _, index := range described.Indexes {
		result[index.Name] = index.Type
	}
	return result
}

// The host replaces an index whose declared kind alone changes, because the
// provider states the kind of each declared index as its inspection reports
// a held one's: a synchronous index declared async is dropped and created
// asynchronous, the next run is synced whichever spelling the source uses,
// and declaring it synchronous again replaces it once more.
func TestHostReplacesAnIndexWhoseKindChanges_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	opened, directory, database := lockFixture(c)
	absolute := path.Join(database, directory, "items")
	c.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		c.Check(execFixtureSchema(cleanup, opened.DB, "DROP TABLE IF EXISTS `"+absolute+"`"), qt.IsNil)
	})
	const (
		created = `(?s).*row-table-create-operation.*Schema apply completed successfully.*`
		// The plan prints the removal's reason, then the addition's operation.
		replaced = `(?s).*Drops a secondary index.*row-table-alter-operation.*Schema apply completed successfully.*`
		synced   = `(?s).*Schema is synced, no changes to be made.*`
	)
	for _, run := range []struct {
		name, attributes, want string
		held                   options.IndexType
	}{
		{"creation with a synchronous index", "", created, options.IndexTypeGlobal},
		{"a second run", "", synced, options.IndexTypeGlobal},
		{"the index declared asynchronous", ` type="async"`, replaced, options.IndexTypeGlobalAsync},
		{"a run after the replacement", ` type="async"`, synced, options.IndexTypeGlobalAsync},
		{"the held kind in the built-in engine's spelling", ` type="GLOBAL ASYNC"`, synced, options.IndexTypeGlobalAsync},
		{"the index declared synchronous again", ` type="GLOBAL SYNC"`, replaced, options.IndexTypeGlobal},
		{"a run after the second replacement", "", synced, options.IndexTypeGlobal},
	} {
		output, err := hostApply(c, directory, hostIndexedItems(directory, run.attributes))
		c.Assert(err, qt.IsNil, qt.Commentf("%s: %s", run.name, output))
		c.Assert(output, qt.Matches, run.want, qt.Commentf("%s", run.name))
		c.Assert(heldIndexKinds(c, opened, absolute), qt.DeepEquals, map[string]options.IndexType{"items_ts": run.held}, qt.Commentf("%s", run.name))
	}
}
