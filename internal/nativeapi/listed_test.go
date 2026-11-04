package nativeapi_test

import (
	"context"
	"errors"
	"path"
	"testing"
	"testing/synctest"
	"time"

	qt "github.com/frankban/quicktest"
	"github.com/stokaro/ptah-ydb-plugin/internal/nativeapi"
	native "github.com/stokaro/ptah-ydb-plugin/internal/nativeprotocol"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb_Operations"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb_Scheme"
)

// directoryAnswer is one answer of a directory listing: the entries it
// names, or a failure.
type directoryAnswer struct {
	children []*Ydb_Scheme.Entry
	err      error
}

var (
	// replicaListed is a listing of /local/d that names the table rep.
	replicaListed = directoryAnswer{children: []*Ydb_Scheme.Entry{
		{Name: "mirror", Type: Ydb_Scheme.Entry_REPLICATION}, {Name: "rep", Type: Ydb_Scheme.Entry_TABLE}, {Name: "src", Type: Ydb_Scheme.Entry_TABLE},
	}}
	// replicaGone is a listing of /local/d once rep was dropped.
	replicaGone = directoryAnswer{children: []*Ydb_Scheme.Entry{{Name: "src", Type: Ydb_Scheme.Entry_TABLE}}}
)

// listedTable answers the describes and listings DescribeListedTable makes in
// order, repeating its last answer once it runs out, and records each path it
// was asked for. A describe answered SUCCESS describes the table at the path.
type listedTable struct {
	answers   []Ydb.StatusIds_StatusCode
	listings  []directoryAnswer
	described []string
	listed    []string
}

func (f *listedTable) describe(_ context.Context, absolute string) (*native.DescribeTableResult, error) {
	status := f.answers[min(len(f.described), len(f.answers)-1)]
	f.described = append(f.described, absolute)
	if err := nativeapi.Status(&Ydb_Operations.Operation{Ready: true, Status: status}); err != nil {
		return nil, err
	}
	return &native.DescribeTableResult{Self: &Ydb_Scheme.Entry{Name: path.Base(absolute), Type: Ydb_Scheme.Entry_TABLE}}, nil
}

func (f *listedTable) list(_ context.Context, absolute string) (*Ydb_Scheme.ListDirectoryResult, error) {
	answer := f.listings[min(len(f.listed), len(f.listings)-1)]
	f.listed = append(f.listed, absolute)
	if answer.err != nil {
		return nil, answer.err
	}
	return &Ydb_Scheme.ListDirectoryResult{Self: &Ydb_Scheme.Entry{Name: "d", Type: Ydb_Scheme.Entry_DIRECTORY}, Children: answer.children}, nil
}

// TestDescribeListedTable_HappyPath describes a listed table at once, and
// waits out the SCHEME_ERROR YDB answers for a replica its listing names
// before the table service describes it, listing the directory again before
// each wait.
func TestDescribeListedTable_HappyPath(t *testing.T) {
	for _, test := range []struct {
		name      string
		answers   []Ydb.StatusIds_StatusCode
		describes int
		lists     int
		waited    time.Duration
	}{
		{name: "described at once", answers: []Ydb.StatusIds_StatusCode{Ydb.StatusIds_SUCCESS}, describes: 1},
		// Measured on 25.1.4.7 and 26.2.1.14 right after CREATE ASYNC
		// REPLICATION: one SCHEME_ERROR, then the replica.
		{name: "described on the first retry", answers: []Ydb.StatusIds_StatusCode{Ydb.StatusIds_SCHEME_ERROR, Ydb.StatusIds_SUCCESS},
			describes: 2, lists: 1, waited: 50 * time.Millisecond},
		{name: "described once the waits have doubled", answers: []Ydb.StatusIds_StatusCode{Ydb.StatusIds_SCHEME_ERROR, Ydb.StatusIds_SCHEME_ERROR, Ydb.StatusIds_SCHEME_ERROR, Ydb.StatusIds_SUCCESS},
			describes: 4, lists: 3, waited: 350 * time.Millisecond},
	} {
		t.Run(test.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				c := testcheck.New(t)
				table := &listedTable{answers: test.answers, listings: []directoryAnswer{replicaListed}}
				start := time.Now()
				described, err := nativeapi.DescribeListedTable(c.Context(), table.describe, table.list, "/local/d", "rep")
				c.Assert(err, qt.IsNil)
				c.Assert(described.GetSelf().GetName(), qt.Equals, "rep")
				c.Assert(time.Since(start), qt.Equals, test.waited)
				c.Assert(table.described, qt.HasLen, test.describes)
				c.Assert(table.listed, qt.HasLen, test.lists)
				for _, absolute := range table.described {
					c.Assert(absolute, qt.Equals, "/local/d/rep")
				}
				for _, absolute := range table.listed {
					c.Assert(absolute, qt.Equals, "/local/d")
				}
			})
		})
	}
}

// TestDescribeListedTable_FailurePath returns every answer but a SCHEME_ERROR
// at once, and never reads a SCHEME_ERROR as a table that is absent: a listing
// that no longer names the table, a listing that fails and a budget that runs
// out each fail the read.
func TestDescribeListedTable_FailurePath(t *testing.T) {
	transport := errors.New("list YDB directory /local/d: transport failed")
	for _, test := range []struct {
		name      string
		answers   []Ydb.StatusIds_StatusCode
		listings  []directoryAnswer
		wantErr   string
		gone      bool
		describes int
		lists     int
		waited    time.Duration
	}{
		{name: "another status is not retried", answers: []Ydb.StatusIds_StatusCode{Ydb.StatusIds_UNAVAILABLE}, listings: []directoryAnswer{replicaListed},
			wantErr: `YDB operation failed: ready=true status=UNAVAILABLE`, describes: 1},
		{name: "the listing no longer names the table", answers: []Ydb.StatusIds_StatusCode{Ydb.StatusIds_SCHEME_ERROR}, listings: []directoryAnswer{replicaGone},
			wantErr: `describe YDB table /local/d/rep: YDB operation failed: ready=true status=SCHEME_ERROR: the listing that named it no longer does`,
			gone:    true, describes: 1, lists: 1},
		{name: "the listing names another kind of entry", answers: []Ydb.StatusIds_StatusCode{Ydb.StatusIds_SCHEME_ERROR},
			listings: []directoryAnswer{{children: []*Ydb_Scheme.Entry{{Name: "rep", Type: Ydb_Scheme.Entry_DIRECTORY}}}},
			wantErr:  `describe YDB table /local/d/rep: .*: the listing that named it no longer does`, gone: true, describes: 1, lists: 1},
		{name: "the table goes while the read waits", answers: []Ydb.StatusIds_StatusCode{Ydb.StatusIds_SCHEME_ERROR}, listings: []directoryAnswer{replicaListed, replicaListed, replicaGone},
			wantErr: `describe YDB table /local/d/rep: .*: the listing that named it no longer does`, gone: true,
			describes: 3, lists: 3, waited: 150 * time.Millisecond},
		{name: "listing the directory again fails", answers: []Ydb.StatusIds_StatusCode{Ydb.StatusIds_SCHEME_ERROR}, listings: []directoryAnswer{{err: transport}},
			wantErr:   `describe YDB table /local/d/rep: YDB operation failed: ready=true status=SCHEME_ERROR; listing it again failed: list YDB directory /local/d: transport failed`,
			describes: 1, lists: 1},
		{name: "the budget runs out", answers: []Ydb.StatusIds_StatusCode{Ydb.StatusIds_SCHEME_ERROR}, listings: []directoryAnswer{replicaListed},
			wantErr: `describe YDB table /local/d/rep: YDB operation failed: ready=true status=SCHEME_ERROR; it has been listed for 10s and YDB has not described it: ` +
				`another operation may still be creating it, or the session may not see it`,
			// 50 ms doubling to a second: 50+100+200+400+800 ms, then
			// a second at a time until a retry finds the budget spent.
			describes: 15, lists: 15, waited: 10550 * time.Millisecond},
	} {
		t.Run(test.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				c := testcheck.New(t)
				table := &listedTable{answers: test.answers, listings: test.listings}
				start := time.Now()
				described, err := nativeapi.DescribeListedTable(c.Context(), table.describe, table.list, "/local/d", "rep")
				c.Assert(err, qt.ErrorMatches, test.wantErr)
				c.Assert(errors.Is(err, nativeapi.ErrNoLongerListed), qt.Equals, test.gone)
				c.Assert(described, qt.IsNil)
				c.Assert(time.Since(start), qt.Equals, test.waited)
				c.Assert(table.described, qt.HasLen, test.describes)
				c.Assert(table.listed, qt.HasLen, test.lists)
			})
		})
	}
}

// TestDescribeListedTable_TransportFailureIsNotRetried returns a failure that
// carries no YDB status as the describe gave it, without listing.
func TestDescribeListedTable_TransportFailureIsNotRetried(t *testing.T) {
	c := testcheck.New(t)
	transport := errors.New("describe YDB table: transport failed")
	var listed int
	described, err := nativeapi.DescribeListedTable(c.Context(),
		func(context.Context, string) (*native.DescribeTableResult, error) { return nil, transport },
		func(context.Context, string) (*Ydb_Scheme.ListDirectoryResult, error) {
			listed++
			return &Ydb_Scheme.ListDirectoryResult{}, nil
		},
		"/local/d", "rep",
	)
	c.Assert(err, qt.Equals, transport)
	c.Assert(described, qt.IsNil)
	c.Assert(listed, qt.Equals, 0)
}

// TestDescribeListedTable_ContextEndsTheWait stops waiting for a listed table
// when the read's context ends, and says why.
func TestDescribeListedTable_ContextEndsTheWait(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		c := testcheck.New(t)
		ctx, cancel := context.WithTimeout(c.Context(), 120*time.Millisecond)
		defer cancel()
		table := &listedTable{answers: []Ydb.StatusIds_StatusCode{Ydb.StatusIds_SCHEME_ERROR}, listings: []directoryAnswer{replicaListed}}
		start := time.Now()
		described, err := nativeapi.DescribeListedTable(ctx, table.describe, table.list, "/local/d", "rep")
		c.Assert(err, qt.ErrorIs, context.DeadlineExceeded)
		c.Assert(err, qt.ErrorMatches, `describe YDB table /local/d/rep: YDB operation failed: ready=true status=SCHEME_ERROR; the read stopped waiting for it: context deadline exceeded`)
		c.Assert(described, qt.IsNil)
		c.Assert(time.Since(start), qt.Equals, 120*time.Millisecond)
		c.Assert(table.described, qt.HasLen, 2)
	})
}

// listedTopic answers a changefeed topic's describes in order, repeating its
// last answer, and says whether the table still names the changefeed.
type listedTopic struct {
	answers   []Ydb.StatusIds_StatusCode
	named     bool
	described int
	listed    int
}

func (f *listedTopic) describe(context.Context) (*native.DescribeTopicResult, error) {
	status := f.answers[min(f.described, len(f.answers)-1)]
	f.described++
	if err := nativeapi.Status(&Ydb_Operations.Operation{Ready: true, Status: status}); err != nil {
		return nil, err
	}
	return &native.DescribeTopicResult{Self: &Ydb_Scheme.Entry{Name: "feed", Type: Ydb_Scheme.Entry_TOPIC}}, nil
}

func (f *listedTopic) names(context.Context) (bool, error) {
	f.listed++
	return f.named, nil
}

// TestDescribeListed_HappyPath waits out the SCHEME_ERROR 25.1.4.7 answers
// for the topic of a changefeed its table already names, as for the one CREATE
// ASYNC REPLICATION adds to its source table.
func TestDescribeListed_HappyPath(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		c := testcheck.New(t)
		topic := &listedTopic{answers: []Ydb.StatusIds_StatusCode{Ydb.StatusIds_SCHEME_ERROR, Ydb.StatusIds_SUCCESS}, named: true}
		start := time.Now()
		described, err := nativeapi.DescribeListed(c.Context(), "YDB changefeed topic /local/d/src/feed", topic.describe, topic.names)
		c.Assert(err, qt.IsNil)
		c.Assert(described.GetSelf().GetName(), qt.Equals, "feed")
		c.Assert(time.Since(start), qt.Equals, 50*time.Millisecond)
		c.Assert(topic.described, qt.Equals, 2)
		c.Assert(topic.listed, qt.Equals, 1)
	})
}

// TestDescribeListed_FailurePath fails the read of a changefeed topic whose
// table no longer names the changefeed, and says which topic.
func TestDescribeListed_FailurePath(t *testing.T) {
	c := testcheck.New(t)
	topic := &listedTopic{answers: []Ydb.StatusIds_StatusCode{Ydb.StatusIds_SCHEME_ERROR}}
	described, err := nativeapi.DescribeListed(c.Context(), "YDB changefeed topic /local/d/src/feed", topic.describe, topic.names)
	c.Assert(err, qt.ErrorIs, nativeapi.ErrNoLongerListed)
	c.Assert(err, qt.ErrorMatches, `describe YDB changefeed topic /local/d/src/feed: YDB operation failed: ready=true status=SCHEME_ERROR: the listing that named it no longer does`)
	c.Assert(described, qt.IsNil)
	c.Assert(topic.described, qt.Equals, 1)
	c.Assert(topic.listed, qt.Equals, 1)
}
