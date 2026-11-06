package tablecreate

// White-box testing required: the index answers exactly as asking disjoint of
// every other table does, so a plan through the exported service reads the
// same either way; only the table the index returns shows whether it found the
// first conflict.

import (
	"fmt"
	"math/rand/v2"
	"slices"
	"testing"

	qt "github.com/frankban/quicktest"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/stokaro/ptah/provider-sdk/provider"

	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
)

// firstDisjointRefusal is the first table of tables, in order, that is not
// subject and that disjoint refuses beside subject, or -1: the answer the
// index must give.
func firstDisjointRefusal(tables []*protocol.PlanningTable, subject *protocol.ComparisonIdentity) int {
	for i, other := range tables {
		if provider.CompareIdentities(other.Subject, subject) == 0 {
			continue
		}
		if disjoint(subject, other.Subject) != nil {
			return i
		}
	}
	return -1
}

// overlapTables are tables at nested paths that overlap and sit side by side,
// in the databases named, an empty name standing for no binding. With unbound,
// some subjects bind to no owner.
func overlapTables(seed uint64, count int, databases []string, unbound bool) []*protocol.PlanningTable {
	// #nosec G404 -- a seeded generator makes every request the same on every run
	random := rand.New(rand.NewPCG(seed, 1))
	part := func(value string) *protocol.ComparisonPart {
		if value == "" {
			return nil
		}
		return &protocol.ComparisonPart{Source: value, Normalized: value}
	}
	schemas := []string{"", "a", "a/b", "a/b/c", "b", "t0", "a/t1"}
	names := []string{"t0", "t1", "b", "c", "x", "y", "z"}
	tables := make([]*protocol.PlanningTable, 0, count)
	for i := range count {
		subject := &protocol.ComparisonIdentity{Kind: "table", Database: part(databases[random.IntN(len(databases))]),
			Schema: part(schemas[random.IntN(len(schemas))]), Name: part(names[random.IntN(len(names))])}
		switch choice := random.IntN(12); {
		case !unbound:
		case choice == 0:
			subject.Kind = "view"
		case choice == 1:
			subject.Catalog = &protocol.ComparisonPart{Source: "c", Normalized: "c"}
		case choice == 2:
			subject.Name = &protocol.ComparisonPart{Source: "N", Normalized: "n"}
		}
		action := protocol.PlanningTable_CREATE
		if i%3 == 0 {
			action = protocol.PlanningTable_DROP
		}
		tables = append(tables, &protocol.PlanningTable{Subject: subject, Action: action})
	}
	return tables
}

// The index finds, for every table of a request, the table asking disjoint of
// each other table in turn finds first, across overlapping, side-by-side,
// unbound and differently bound tables. A request holding a table bound to
// no owner, or tables with and without a database binding, refuses every
// creation; one holding neither refuses some.
func TestOverlapIndexFindsTheFirstConflictDisjointFinds(t *testing.T) {
	for _, test := range []struct {
		name      string
		seed      uint64
		databases []string
		unbound   bool
		partial   bool
	}{
		{"one database", 1, []string{"/local"}, false, true},
		{"one database, another seed", 2, []string{"/local"}, false, true},
		{"two databases", 3, []string{"/local", "/other"}, false, true},
		{"two databases, another seed", 4, []string{"/local", "/other"}, false, true},
		{"no binding", 5, []string{""}, false, true},
		{"bound and unbound databases", 6, []string{"", "/local"}, false, false},
		{"unbound subjects", 7, []string{"/local"}, true, false},
		{"everything", 8, []string{"", "/local", "/other"}, true, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			tables := overlapTables(test.seed, 60, test.databases, test.unbound)
			index := newOverlapIndex(tables)
			found, want := make([]int, len(tables)), make([]int, len(tables))
			conflicts := 0
			for i, table := range tables {
				found[i] = index.firstConflict(table.Subject)
				want[i] = firstDisjointRefusal(tables, table.Subject)
				conflicts += min(want[i]+1, 1)
			}
			c.Assert(found, qt.DeepEquals, want)
			c.Assert(conflicts > 0, qt.IsTrue)
			c.Assert(conflicts < len(tables), qt.Equals, test.partial)
		})
	}
}

// A request of tables in one directory has no conflict for any of them.
func TestOverlapIndexFindsNoConflictBetweenSiblings(t *testing.T) {
	c := testcheck.New(t)
	tables := make([]*protocol.PlanningTable, 0, 200)
	for i := range 200 {
		name := fmt.Sprintf("t%04d", i)
		tables = append(tables, &protocol.PlanningTable{Action: protocol.PlanningTable_CREATE, Subject: &protocol.ComparisonIdentity{
			Kind: "table", Database: &protocol.ComparisonPart{Source: "/local", Normalized: "/local"},
			Schema: &protocol.ComparisonPart{Source: "app", Normalized: "app"}, Name: &protocol.ComparisonPart{Source: name, Normalized: name},
		}})
	}
	index := newOverlapIndex(tables)
	found := make([]int, 0, len(tables))
	for _, table := range tables {
		found = append(found, index.firstConflict(table.Subject))
	}
	c.Assert(found, qt.DeepEquals, slices.Repeat([]int{-1}, len(tables)))
}
