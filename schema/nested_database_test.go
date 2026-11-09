package schema_test

import (
	"testing"

	qt "github.com/frankban/quicktest"
	protocol "ptah.run/provider-sdk/protocol/experimental"

	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
	"github.com/stokaro/ptah-ydb-plugin/schema"
)

// The root a read writes into a nested database's claim reads back from it.
func TestNestedDatabase_HappyPath(t *testing.T) {
	for _, root := range []string{"db2", "app/inner"} {
		t.Run(root, func(t *testing.T) {
			c := testcheck.New(t)

			got, nested := schema.NestedDatabase(&protocol.ModelKnowledge{State: protocol.ModelKnowledge_COMPLETE, Reason: schema.NestedDatabaseReason(root)})

			c.Assert(nested, qt.IsTrue)
			c.Assert(got, qt.Equals, root)
		})
	}
}

// Knowledge that is not a nested database's claim names no root: a claim of
// another state, one without a reason, one with another reason, and one with
// no root between the reason's words.
func TestNestedDatabase_FailurePath(t *testing.T) {
	for _, test := range []struct {
		name      string
		knowledge *protocol.ModelKnowledge
	}{
		{name: "unknown knowledge", knowledge: &protocol.ModelKnowledge{State: protocol.ModelKnowledge_UNINSPECTED, Reason: schema.NestedDatabaseReason("db2")}},
		{name: "a complete claim", knowledge: &protocol.ModelKnowledge{State: protocol.ModelKnowledge_COMPLETE}},
		{name: "a reserved name", knowledge: &protocol.ModelKnowledge{State: protocol.ModelKnowledge_COMPLETE, Reason: schema.ReservedNameReason}},
		{name: "no root", knowledge: &protocol.ModelKnowledge{State: protocol.ModelKnowledge_COMPLETE, Reason: schema.NestedDatabaseReason("")}},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)

			got, nested := schema.NestedDatabase(test.knowledge)

			c.Assert(nested, qt.IsFalse)
			c.Assert(got, qt.Equals, "")
		})
	}
}
