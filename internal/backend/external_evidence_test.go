package backend_test

import (
	"testing"

	qt "github.com/frankban/quicktest"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/stokaro/ptah/provider-sdk/provider"

	"github.com/stokaro/ptah-ydb-plugin/internal/backend"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
	"github.com/stokaro/ptah-ydb-plugin/schema"
)

// Ported from the host's
// TestExternalComparison_LeavesAnUndecidedTableOverARecreatedSource: a data
// source dropped and created again takes the declared tables over it along,
// but not a table either side cannot describe. That table is undecided, and
// the source's change is still planned rather than the whole reply refused.
func TestExternalComparisonLeavesAnUndecidedTableOverARecreatedSource(t *testing.T) {
	bucket, moved := external{name: "bucket", source: bucketAt("https://a/")}, external{name: "bucket", source: bucketAt("https://b/")}
	events := external{name: "events", table: eventsOver("app/bucket")}
	for _, test := range []struct {
		name     string
		desired  bool
		database string
	}{
		{name: "the declaration cannot describe the table", desired: true},
		{name: "the read cannot describe the table", database: "/local"},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			request := externalRequest(c, []external{moved, events}, []external{bucket, events}, withSources())
			state := map[bool]*protocol.ComparisonState{true: request.Desired, false: request.Current}[test.desired]
			limitSubject(state, schema.ExternalTableProperties, test.database, "app", "events", protocol.ModelKnowledge_UNREPRESENTABLE)

			result, err := backend.New().CompareModels(c.Context(), request)

			c.Assert(err, qt.IsNil)
			c.Assert(provider.ValidateComparedModels(backend.Description(), request, result), qt.IsNil)
			c.Assert(changedPaths(result), qt.DeepEquals, []string{"app/bucket BEHAVIORAL"})
			c.Assert(result.Undecided, qt.HasLen, 1)
			c.Assert(result.Undecided[0].Subject.Name.Normalized, qt.Equals, "events")
		})
	}
}

// Ported from the host's TestCompare_YDBExternalObjectsNotCreatedWhereTheReadDidNotLook:
// a declared object the read says it did not describe, as the read does on a
// server with external data sources off, is not created, since CREATE
// EXTERNAL has no guard; the comparison reports each, and still creates the
// declared object the read did look for.
func TestExternalComparisonCreatesOnlyWhereTheReadLooked(t *testing.T) {
	c := testcheck.New(t)
	request := externalRequest(c, []external{{name: "pg", source: bucketAt("https://pg/")}, {name: "s3", source: bucketAt("https://s3/")},
		{name: "events", table: eventsOver("app/s3")}}, nil, withSources())
	limitSubject(request.Current, schema.ExternalSourceProperties, "/local", "app", "s3", protocol.ModelKnowledge_UNINSPECTED)
	limitSubject(request.Current, schema.ExternalTableProperties, "/local", "app", "events", protocol.ModelKnowledge_UNINSPECTED)

	result, err := backend.New().CompareModels(c.Context(), request)

	c.Assert(err, qt.IsNil)
	c.Assert(changedPaths(result), qt.DeepEquals, []string{"app/pg ADDITIVE"})
	c.Assert(result.Undecided, qt.HasLen, 2)
}
