package changefeedsource_test

import (
	"testing"

	qt "github.com/frankban/quicktest"

	"github.com/stokaro/ptah-ydb-plugin/internal/changefeeds"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
	"github.com/stokaro/ptah-ydb-plugin/internal/testsource"
	"github.com/stokaro/ptah-ydb-plugin/internal/ydbdecl"
)

// A consumer's read_from written with an offset reads as the same instant in
// UTC. Ported from the host's TestParse_YDBChangefeed_HappyPath.
func TestDecodeYAMLReadsAReadFromInUTC(t *testing.T) {
	c := testcheck.New(t)
	request, err := testsource.YAMLTables(`{"tables": {"items": {"columns": {"id": {"type": "bigint", "primary": true}}, "changefeeds": {
		"updates": {"mode": "UPDATES", "format": "JSON", "consumers": {"audit": {"read_from": "2026-01-01T03:00:00+03:00"}}}}}}}`)
	c.Assert(err, qt.IsNil)
	c.Assert(declared(c, request), qt.DeepEquals, map[string][]changefeeds.Feed{"items": {
		feed(ydbdecl.Changefeed{Name: "updates", Mode: "UPDATES", Format: "JSON", Consumers: []ydbdecl.Consumer{{Name: "audit", ReadFrom: "2026-01-01T00:00:00Z"}}}),
	}})
}
