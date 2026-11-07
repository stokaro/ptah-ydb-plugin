package devclean_test

import (
	"context"
	"errors"
	"testing"
	"testing/synctest"
	"time"

	qt "github.com/frankban/quicktest"

	"github.com/stokaro/ptah-ydb-plugin/internal/devclean"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
)

// answeringRunner answers each statement from answers, one answer a run and
// the last one from then on, and drops the object of a statement it answers
// with success from the tree. refused records the statements it answered with
// an error.
type answeringRunner struct {
	*fakeTree
	answers []error
	calls   int
	refused []string
}

func (r *answeringRunner) Execute(ctx context.Context, statement string) error {
	answer := r.answers[min(r.calls, len(r.answers)-1)]
	r.calls++
	if answer != nil {
		r.refused = append(r.refused, statement)
		return answer
	}
	return r.fakeTree.Execute(ctx, statement)
}

// probeTree is a database holding the directory probe with the one object rep
// of entryType in it.
func probeTree(c *qt.C, entryType string) *fakeTree {
	c.Helper()
	return newFakeTree(c, corpusCase{Tree: map[string][]corpusEntry{
		"/local":       {{Name: "probe", Type: "DIRECTORY"}},
		"/local/probe": {{Name: "rep", Type: entryType}},
	}}, "/local")
}

// clean walks the database tree runner holds and drops what it holds.
func clean(ctx context.Context, c *qt.C, runner *answeringRunner) error {
	c.Helper()
	entries, err := devclean.Walk(ctx, runner, devclean.Tree{Root: "/local", Database: "/local"}, "")
	c.Assert(err, qt.IsNil)
	_, err = devclean.Run(ctx, runner, runner, entries)
	return err
}

// A row table the listing named is dropped once SQL sees it: the drop runs
// again while the runner answers that the table is not visible yet, 50 ms
// after the first answer and twice as long after each next one. Ported from
// the host's TestWriter_TableDropSettles.
func TestRunWaitsForAListedTable(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		c := testcheck.New(t)
		runner := &answeringRunner{fakeTree: probeTree(c, "TABLE"), answers: []error{devclean.ErrNotVisible, devclean.ErrNotVisible, nil}}
		start := time.Now()

		err := clean(c.Context(), c, runner)

		c.Assert(err, qt.IsNil)
		c.Assert(runner.calls, qt.Equals, 3)
		c.Assert(time.Since(start), qt.Equals, 150*time.Millisecond)
		c.Assert(runner.executed, qt.DeepEquals, []string{"DROP TABLE `probe/rep`"})
		c.Assert(runner.removed, qt.DeepEquals, []string{"/local/probe"})
	})
}

// The removal of a realm waits for a listed row table as the cleanup does.
// Ported from the host's TestWriter_TableDropSettles.
func TestRemoveRealmWaitsForAListedTable(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		c := testcheck.New(t)
		tree := newFakeTree(c, corpusCase{Tree: map[string][]corpusEntry{
			"/local":             {{Name: devclean.RealmDirectory, Type: "DIRECTORY"}},
			"/local/ptah_dev":    {{Name: "r1", Type: "DIRECTORY"}},
			"/local/ptah_dev/r1": {{Name: "rep", Type: "TABLE"}},
		}}, "/local")
		runner := &answeringRunner{fakeTree: tree, answers: []error{devclean.ErrNotVisible, devclean.ErrNotVisible, nil}}
		start := time.Now()

		err := devclean.RemoveRealm(c.Context(), runner, runner, "/local", "r1")

		c.Assert(err, qt.IsNil)
		c.Assert(runner.calls, qt.Equals, 3)
		c.Assert(time.Since(start), qt.Equals, 150*time.Millisecond)
		c.Assert(runner.executed, qt.DeepEquals, []string{"DROP TABLE `ptah_dev/r1/rep`"})
		c.Assert(runner.removed, qt.DeepEquals, []string{"/local/ptah_dev/r1", "/local/ptah_dev"})
	})
}

// The wait for a listed row table ends at its budget or at the caller's
// deadline, whichever comes first, with both reasons in the error. A refusal
// other than an invisible table is the answer and is not waited on, and only
// a row table is waited for: a view and a column table answer at once.
// Nothing after the failed drop runs. Ported from the host's
// TestWriter_TableDropRefusals.
func TestRunStopsWaitingForATable_FailurePath(t *testing.T) {
	denied := errors.New("a cleanup statement failed: YDB answered UNAUTHORIZED")
	for _, test := range []struct {
		name      string
		answers   []error
		entryType string
		timeout   time.Duration
		want      error
		elapsed   time.Duration
	}{
		{name: "a table SQL never sees", answers: []error{devclean.ErrNotVisible}, entryType: "TABLE",
			timeout: time.Minute, want: context.DeadlineExceeded, elapsed: 10 * time.Second},
		{name: "the caller's deadline", answers: []error{devclean.ErrNotVisible}, entryType: "TABLE",
			timeout: 25 * time.Millisecond, want: context.DeadlineExceeded, elapsed: 25 * time.Millisecond},
		{name: "an access refusal", answers: []error{denied}, entryType: "TABLE", timeout: time.Minute, want: denied},
		{name: "a refusal after a wait", answers: []error{devclean.ErrNotVisible, denied}, entryType: "TABLE",
			timeout: time.Minute, want: denied, elapsed: 50 * time.Millisecond},
		{name: "a view", answers: []error{devclean.ErrNotVisible}, entryType: "VIEW", timeout: time.Minute, want: devclean.ErrNotVisible},
		{name: "a column table", answers: []error{devclean.ErrNotVisible}, entryType: "COLUMN_TABLE", timeout: time.Minute, want: devclean.ErrNotVisible},
	} {
		t.Run(test.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				c := testcheck.New(t)
				runner := &answeringRunner{fakeTree: probeTree(c, test.entryType), answers: test.answers}
				ctx, cancel := context.WithTimeout(c.Context(), test.timeout)
				defer cancel()
				start := time.Now()

				err := clean(ctx, c, runner)

				c.Assert(err, qt.ErrorIs, test.want)
				c.Assert(err, qt.ErrorIs, test.answers[len(test.answers)-1])
				c.Assert(time.Since(start), qt.Equals, test.elapsed)
				c.Assert(runner.executed, qt.HasLen, 0)
				c.Assert(runner.removed, qt.HasLen, 0)
			})
		})
	}
}

// A drop the server refuses stops the cleanup there: the steps before it ran,
// and no later drop and no directory removal does. Ported from the host's
// TestWriter_DropAllTables_FailurePath.
func TestRunStopsAtADropTheServerRefuses_FailurePath(t *testing.T) {
	c := testcheck.New(t)
	refusal := errors.New("a cleanup statement failed: YDB answered GENERIC_ERROR")
	tree := newFakeTree(c, corpusCase{Tree: map[string][]corpusEntry{
		"/local":   {{Name: "d", Type: "DIRECTORY"}, {Name: "t", Type: "TABLE"}, {Name: "u", Type: "TABLE"}},
		"/local/d": {{Name: "v", Type: "TABLE"}},
	}}, "/local")
	runner := &answeringRunner{fakeTree: tree, answers: []error{nil, refusal, nil}}
	entries, err := devclean.Walk(c.Context(), runner, devclean.Tree{Root: "/local", Database: "/local"}, "")
	c.Assert(err, qt.IsNil)

	started, err := devclean.Run(c.Context(), runner, runner, entries)

	c.Assert(err, qt.ErrorIs, refusal)
	c.Assert(started, qt.IsTrue)
	c.Assert(runner.executed, qt.DeepEquals, []string{"DROP TABLE `d/v`"})
	c.Assert(runner.refused, qt.DeepEquals, []string{"DROP TABLE `t`"})
	c.Assert(runner.removed, qt.HasLen, 0)
}
