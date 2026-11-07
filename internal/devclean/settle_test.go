package devclean_test

import (
	"context"
	"testing"
	"testing/synctest"
	"time"

	qt "github.com/frankban/quicktest"

	"github.com/stokaro/ptah-ydb-plugin/internal/devclean"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
)

// settlingTree answers each statement from answers, repeating the last
// answer, and drops the object once a statement succeeds.
type settlingTree struct {
	*fakeTree
	answers []error
	calls   int
}

func (s *settlingTree) Execute(ctx context.Context, statement string) error {
	answer := s.answers[min(s.calls, len(s.answers)-1)]
	s.calls++
	if answer != nil {
		return answer
	}
	return s.fakeTree.Execute(ctx, statement)
}

// settlingCleanup is a database holding one object of kind at probe/rep, whose
// first drop SQL does not see yet, and the entries a cleanup of it walks.
func settlingCleanup(c *qt.C, kind string) (*settlingTree, []devclean.Entry) {
	c.Helper()
	fake := &settlingTree{fakeTree: newFakeTree(c, corpusCase{Tree: map[string][]corpusEntry{
		"/local":       {{Name: "probe", Type: "DIRECTORY"}},
		"/local/probe": {{Name: "rep", Type: kind}},
	}}, "/local"), answers: []error{devclean.ErrNotVisible, nil}}
	entries, err := devclean.Walk(context.Background(), fake, devclean.Tree{Root: "/local", Database: "/local"}, "")
	c.Assert(err, qt.IsNil)
	return fake, entries
}

// A cleanup waits for SQL to see a row table the listing named, and drops it
// once SQL does.
func TestRunWaitsForAListedRowTable_HappyPath(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		c := testcheck.New(t)
		fake, entries := settlingCleanup(c, "TABLE")
		start := time.Now()

		_, err := devclean.Run(context.Background(), fake, fake, entries)

		c.Assert(err, qt.IsNil)
		c.Assert(fake.calls, qt.Equals, 2)
		c.Assert(time.Since(start), qt.Equals, 50*time.Millisecond)
		c.Assert(fake.executed, qt.DeepEquals, []string{"DROP TABLE `probe/rep`"})
	})
}

// The wait is the cleanup's, and for a listed row table alone: the drop of
// any other object reports at once that SQL does not see it, as a statement
// outside a cleanup does, since the provider's execution runs no statement
// through the wait. Ported from the host's
// TestWriter_TableDropRetryIsScopedToCleanup.
func TestRunWaitsForNothingElse_FailurePath(t *testing.T) {
	for _, kind := range []string{"VIEW", "COLUMN_TABLE"} {
		t.Run(kind, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				c := testcheck.New(t)
				fake, entries := settlingCleanup(c, kind)
				start := time.Now()

				_, err := devclean.Run(context.Background(), fake, fake, entries)

				c.Assert(err, qt.ErrorIs, devclean.ErrNotVisible)
				c.Assert(fake.calls, qt.Equals, 1)
				c.Assert(time.Since(start), qt.Equals, time.Duration(0))
				c.Assert(fake.removed, qt.HasLen, 0)
			})
		})
	}
}
