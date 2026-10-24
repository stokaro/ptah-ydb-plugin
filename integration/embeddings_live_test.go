//go:build integration

package integration_test

import (
	"context"
	"fmt"
	"os"
	"path"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	qt "github.com/frankban/quicktest"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/stokaro/ptah/provider-sdk/provider"
	providerruntime "github.com/stokaro/ptah/provider-sdk/runtime"
	"github.com/ydb-platform/ydb-go-sdk/v3/scheme"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/testing/protocmp"

	"github.com/stokaro/ptah-ydb-plugin/internal/connection"
)

// embeddingFixture is a session on a dev realm of its own through a provider
// whose host lists embeddings.store, so the run-state tables, the source and
// the vectors are the test's. Every table the test leaves in the realm is
// dropped when it ends.
type embeddingFixture struct {
	opened  *connection.Connection
	client  *providerruntime.Client
	session string
	address string
	root    string
	version string
}

func newEmbeddingFixture(c *qt.C) *embeddingFixture {
	c.Helper()
	base, err := connection.Parse(os.Getenv("PTAH_YDB_TEST_URL"))
	c.Assert(err, qt.IsNil)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	opened, err := connection.Open(ctx, base)
	c.Assert(err, qt.IsNil)
	c.Cleanup(func() { c.Check(opened.Close(), qt.IsNil) })
	address, root := realmFixture(c, opened, base)
	c.Cleanup(func() { emptyRealm(c, opened, root) })
	client := startEmbeddingProvider(c, provider.EmbeddingStoreFeature)
	session, err := client.Open(ctx, &protocol.OpenRequest{Engine: "ydb", Address: address})
	c.Assert(err, qt.IsNil)
	c.Cleanup(func() { c.Check(client.CloseSession(context.Background(), session.Id), qt.IsNil) })
	return &embeddingFixture{opened: opened, client: client, session: session.Id, address: address, root: root, version: session.ServerVersion}
}

// startEmbeddingProvider starts the provider for a host that lists
// hostFeatures.
func startEmbeddingProvider(c *qt.C, hostFeatures ...string) *providerruntime.Client {
	c.Helper()
	binary, digest := providerBinary(c)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	client, err := providerruntime.Start(ctx, providerruntime.Options{
		Executable: binary, SHA256: digest, ProviderID: "github.com/stokaro/ptah-ydb-plugin", HostFeatures: hostFeatures,
	})
	c.Assert(err, qt.IsNil)
	c.Cleanup(client.Close)
	return client
}

// emptyRealm drops every table and the lock node at the realm's root, so the
// realm fixture can remove the directory.
func emptyRealm(c *qt.C, opened *connection.Connection, root string) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	listing, err := opened.Driver.Scheme().ListDirectory(ctx, root)
	if err != nil {
		return
	}
	for _, entry := range listing.Children {
		switch entry.Type {
		case scheme.EntryTable:
			c.Check(execFixtureSchema(ctx, opened.DB, "DROP TABLE `"+path.Join(root, entry.Name)+"`"), qt.IsNil)
		case scheme.EntryCoordinationNode:
			c.Check(opened.Driver.Coordination().DropNode(ctx, path.Join(root, entry.Name)), qt.IsNil)
		}
	}
}

func (f *embeddingFixture) exec(c *qt.C, statements ...string) {
	c.Helper()
	ctx, cancel := context.WithTimeout(c.Context(), time.Minute)
	defer cancel()
	for _, statement := range statements {
		c.Assert(execFixtureSchema(ctx, f.opened.DB, "PRAGMA TablePathPrefix(\""+f.root+"\");\n"+statement), qt.IsNil, qt.Commentf("%s", statement))
	}
}

// seedArticles creates the source table and five documents.
func (f *embeddingFixture) seedArticles(c *qt.C) {
	c.Helper()
	f.exec(c, "CREATE TABLE articles (id Int64 NOT NULL, title Utf8, body Utf8, updated_at Timestamp, PRIMARY KEY (id))",
		"UPSERT INTO articles (id, title, body, updated_at) VALUES "+
			"(1, 'Pricing', 'pricing pricing pricing support', Timestamp('2026-10-01T00:00:00Z')), "+
			"(2, 'Support', 'support support support pricing', Timestamp('2026-10-01T00:00:00Z')), "+
			"(3, 'Billing', 'billing billing billing support', Timestamp('2026-10-01T00:00:00Z')), "+
			"(4, 'Renewals', 'renewal renewal renewal billing', Timestamp('2026-10-01T00:00:00Z')), "+
			"(5, 'Costs', NULL, Timestamp('2026-10-01T00:00:00Z'))")
}

// maintainedOnWrite is what the server's line says about a vector index
// taking in rows written after its build: 25.3 and newer do.
func (f *embeddingFixture) maintainedOnWrite(c *qt.C) bool {
	c.Helper()
	version := strings.TrimPrefix(strings.ToLower(f.version), "stable-")
	parts := strings.FieldsFunc(version, func(r rune) bool { return r == '.' || r == '-' })
	c.Assert(len(parts) >= 2, qt.IsTrue, qt.Commentf("%s", f.version))
	major, err := strconv.Atoi(parts[0])
	c.Assert(err, qt.IsNil)
	minor, err := strconv.Atoi(parts[1])
	c.Assert(err, qt.IsNil)
	return major > 25 || major == 25 && minor >= 3
}

const testGeneration = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

// articlesSpec is a generation of the articles' title and body, in column
// embedding beside them or in a table of its own.
func articlesSpec(layout protocol.EmbeddingTarget_Layout, indexed bool) *protocol.EmbeddingSpec {
	target := &protocol.EmbeddingTarget{Table: &protocol.Reference{Kind: "table", Name: "articles"}, Column: "embedding", Layout: layout,
		Representation: "float", Metric: protocol.EmbeddingTarget_COSINE, Dimension: 4}
	names := &protocol.EmbeddingNames{ColumnComment: "embedding generation " + testGeneration[:12]}
	if layout == protocol.EmbeddingTarget_OWN_TABLE {
		target.Table = &protocol.Reference{Kind: "table", Name: "article_vectors"}
		names.OwnershipMark = "ptah embedding target for generation " + testGeneration
	}
	if indexed {
		target.IndexMethod, target.IndexOptions = "vector_kmeans_tree", map[string]string{"levels": "1", "clusters": "2"}
		names.Index = target.Table.Name + "_embedding_" + testGeneration[:12] + "_idx"
	}
	return &protocol.EmbeddingSpec{Generation: testGeneration, Chunked: layout == protocol.EmbeddingTarget_OWN_TABLE, Names: names,
		Source: &protocol.EmbeddingSource{Table: &protocol.Reference{Kind: "table", Name: "articles"}, KeyFields: []string{"id"},
			InputFields: []string{"title", "body"}, VersionField: "updated_at"},
		Target: target}
}

func key(components ...string) *protocol.EmbeddingKey {
	return &protocol.EmbeddingKey{Components: components}
}

func at(seconds int64, nanos int32) *protocol.TimestampValue {
	return &protocol.TimestampValue{Seconds: seconds, Nanos: nanos}
}

func testRun(id string) *protocol.EmbeddingRun {
	return &protocol.EmbeddingRun{Id: id, SpecDigest: "spec", GenerationIdentity: testGeneration, Environment: "dev", Source: "articles",
		Target: "articles.embedding", ProviderProfile: "local", PtahVersion: "test", PolicyDigest: "policy", Phase: "boundary_captured",
		Status: "running", Progress: &protocol.EmbeddingProgress{}, CreatedAt: at(1_790_000_000, 123_456_789), UpdatedAt: at(1_790_000_000, 0)}
}

func testGenerationRecord() *protocol.EmbeddingGeneration {
	return &protocol.EmbeddingGeneration{Identity: testGeneration, SpecDigest: "spec", SpecDocument: "version: 1", Reproducibility: "full",
		Dimension: 4, TargetTable: "articles", TargetColumn: "embedding", SourceTable: "articles", ConsistencyMode: "immutable",
		CreatedAt: at(1_790_000_000, 0)}
}

func everything() *protocol.EmbeddingStateScope {
	return &protocol.EmbeddingStateScope{Scope: &protocol.EmbeddingStateScope_Everything{Everything: &protocol.EmbeddingStateScope_AllRecords{}}}
}

func runScope(id string) *protocol.EmbeddingStateScope {
	return &protocol.EmbeddingStateScope{Scope: &protocol.EmbeddingStateScope_Run{Run: id}}
}

func (f *embeddingFixture) read(c *qt.C, scope *protocol.EmbeddingStateScope) *protocol.EmbeddingState {
	c.Helper()
	state, err := f.client.ReadEmbeddingState(c.Context(), &protocol.ReadEmbeddingStateRequest{SessionId: f.session, Scope: scope, MaxRecords: 100})
	c.Assert(err, qt.IsNil)
	return state
}

func (f *embeddingFixture) mutate(c *qt.C, scope *protocol.EmbeddingStateScope, token string, records *protocol.EmbeddingRecords) (*protocol.EmbeddingWriteResult, error) {
	c.Helper()
	return f.client.MutateEmbeddingState(c.Context(), &protocol.MutateEmbeddingStateRequest{SessionId: f.session, RequestId: "mutate",
		Scope: scope, ExpectedToken: token, Records: records})
}

func (f *embeddingFixture) prepareStore(c *qt.C) {
	c.Helper()
	result, err := f.client.PrepareEmbeddingStore(c.Context(), &protocol.PrepareEmbeddingStoreRequest{SessionId: f.session, RequestId: "store"})
	c.Assert(err, qt.IsNil)
	c.Assert(result.Outcome, qt.Equals, protocol.EmbeddingWriteResult_APPLIED)
}

// registered prepares the store and records the generation and its run.
func (f *embeddingFixture) registered(c *qt.C, runID string) {
	c.Helper()
	f.prepareStore(c)
	result, err := f.mutate(c, everything(), f.read(c, everything()).Token,
		&protocol.EmbeddingRecords{Generations: []*protocol.EmbeddingGeneration{testGenerationRecord()}, Runs: []*protocol.EmbeddingRun{testRun(runID)}})
	c.Assert(err, qt.IsNil)
	c.Assert(result.Outcome, qt.Equals, protocol.EmbeddingWriteResult_APPLIED)
}

// The run state is a compare-and-write store: a snapshot comes with a token,
// a mutation on that token writes the records whole and returns them as a
// later read does, to the microsecond YDB keeps, and a mutation on an old
// token writes nothing.
func TestEmbeddingRunStateSubprocess(t *testing.T) {
	c := qt.New(t)
	f := newEmbeddingFixture(c)

	before := f.read(c, everything())
	c.Assert(before.Exists, qt.IsFalse)
	absent, err := f.mutate(c, everything(), before.Token, &protocol.EmbeddingRecords{Runs: []*protocol.EmbeddingRun{testRun("r1")}})
	c.Assert(err, qt.ErrorAs, new(*provider.EmbeddingWriteError))
	c.Assert(absent.GetRefusal().GetReason(), qt.Equals, protocol.EmbeddingRefusal_ABSENT)

	f.prepareStore(c)
	f.prepareStore(c)
	empty := f.read(c, everything())
	c.Assert(empty.Exists, qt.IsTrue)
	c.Assert(empty.Token, qt.Not(qt.Equals), before.Token)

	records := &protocol.EmbeddingRecords{Generations: []*protocol.EmbeddingGeneration{testGenerationRecord()}, Runs: []*protocol.EmbeddingRun{testRun("r1")},
		Pointers: []*protocol.EmbeddingPointer{{TargetTable: "articles", ActiveGeneration: testGeneration, CutOverAt: at(1_790_000_100, 999)}}}
	applied, err := f.mutate(c, everything(), empty.Token, records)
	c.Assert(err, qt.IsNil)
	after := f.read(c, everything())
	c.Assert(applied.Written, qt.CmpEquals(protocmp.Transform()), after.Records)
	c.Assert(after.Records.Runs[0].CreatedAt, qt.CmpEquals(protocmp.Transform()), at(1_790_000_000, 123_456_000))
	c.Assert(after.Records.Pointers[0].CutOverAt, qt.CmpEquals(protocmp.Transform()), at(1_790_000_100, 0))

	stale, err := f.mutate(c, everything(), empty.Token, &protocol.EmbeddingRecords{Runs: []*protocol.EmbeddingRun{testRun("r2")}})
	c.Assert(err, qt.ErrorIs, provider.ErrEmbeddingConflict)
	c.Assert(stale.Outcome, qt.Equals, protocol.EmbeddingWriteResult_CONFLICT)
	c.Assert(f.read(c, everything()).Records.Runs, qt.HasLen, 1)

	for _, test := range []struct {
		name  string
		scope *protocol.EmbeddingStateScope
		count int
	}{
		{name: "a run", scope: runScope("r1"), count: 1},
		{name: "a run that is not there", scope: runScope("r9"), count: 0},
		{name: "a generation", scope: &protocol.EmbeddingStateScope{Scope: &protocol.EmbeddingStateScope_Generation{Generation: testGeneration}}, count: 1},
		{name: "the runs of a generation", scope: &protocol.EmbeddingStateScope{Scope: &protocol.EmbeddingStateScope_RunsOfGeneration{RunsOfGeneration: testGeneration}}, count: 1},
		{name: "a pointer", scope: &protocol.EmbeddingStateScope{Scope: &protocol.EmbeddingStateScope_Pointer{Pointer: &protocol.EmbeddingPointerKey{TargetTable: "articles"}}}, count: 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := qt.New(t)
			state := f.read(c, test.scope)
			c.Assert(len(state.Records.Generations)+len(state.Records.Runs)+len(state.Records.Pointers), qt.Equals, test.count)
		})
	}
}

// Workers that read one run's state and write it on the same token race, and
// exactly one of them wins: YDB's serializable transactions decide, and every
// other one is told the state changed.
func TestEmbeddingRunStateRacesSubprocess(t *testing.T) {
	c := qt.New(t)
	f := newEmbeddingFixture(c)
	f.registered(c, "r1")
	token := f.read(c, runScope("r1")).Token
	const workers = 6
	outcomes := make([]protocol.EmbeddingWriteResult_Outcome, workers)
	var group sync.WaitGroup
	for worker := range workers {
		group.Go(func() {
			run := testRun("r1")
			run.LeaseOwner, run.FencingToken = fmt.Sprintf("worker-%d", worker), int64(worker+1)
			result, _ := f.client.MutateEmbeddingState(context.Background(), &protocol.MutateEmbeddingStateRequest{SessionId: f.session,
				RequestId: fmt.Sprint(worker), Scope: runScope("r1"), ExpectedToken: token, Records: &protocol.EmbeddingRecords{Runs: []*protocol.EmbeddingRun{run}}})
			outcomes[worker] = result.GetOutcome()
		})
	}
	group.Wait()
	slices.Sort(outcomes)
	want := slices.Repeat([]protocol.EmbeddingWriteResult_Outcome{protocol.EmbeddingWriteResult_CONFLICT}, workers-1)
	c.Assert(outcomes, qt.DeepEquals, append([]protocol.EmbeddingWriteResult_Outcome{protocol.EmbeddingWriteResult_APPLIED}, want...))
	c.Assert(f.read(c, runScope("r1")).Records.Runs[0].LeaseOwner, qt.Matches, `worker-\d`)
}

// Two cutovers that expect no pointer move it at once: the pointer scope's
// token decides, one move lands and the other is told the state changed.
func TestEmbeddingFirstCutoverRaceSubprocess(t *testing.T) {
	c := qt.New(t)
	f := newEmbeddingFixture(c)
	f.registered(c, "r1")
	scope := &protocol.EmbeddingStateScope{Scope: &protocol.EmbeddingStateScope_Pointer{Pointer: &protocol.EmbeddingPointerKey{TargetTable: "articles"}}}
	token := f.read(c, scope).Token
	outcomes := make([]protocol.EmbeddingWriteResult_Outcome, 2)
	var group sync.WaitGroup
	for i, active := range []string{"left", "right"} {
		group.Go(func() {
			result, _ := f.client.MutateEmbeddingState(context.Background(), &protocol.MutateEmbeddingStateRequest{SessionId: f.session,
				RequestId: active, Scope: scope, ExpectedToken: token, Records: &protocol.EmbeddingRecords{
					Pointers: []*protocol.EmbeddingPointer{{TargetTable: "articles", ActiveGeneration: active, CutOverAt: at(1_790_000_000, 0)}}}})
			outcomes[i] = result.GetOutcome()
		})
	}
	group.Wait()
	slices.Sort(outcomes)
	c.Assert(outcomes, qt.DeepEquals, []protocol.EmbeddingWriteResult_Outcome{protocol.EmbeddingWriteResult_APPLIED, protocol.EmbeddingWriteResult_CONFLICT})
	c.Assert([]string{"left", "right"}, qt.Any(qt.Equals), f.read(c, scope).Records.Pointers[0].ActiveGeneration)
}

// A worker whose run another worker took over commits a batch on the token it
// read before the takeover: the commit is refused whole, and no vector of it
// is stored.
func TestEmbeddingFencedWorkerCannotCommitSubprocess(t *testing.T) {
	c := qt.New(t)
	f := newEmbeddingFixture(c)
	f.seedArticles(c)
	spec := articlesSpec(protocol.EmbeddingTarget_SOURCE_COLUMNS, false)
	prepared, err := f.client.PrepareEmbeddingTarget(c.Context(), &protocol.PrepareEmbeddingTargetRequest{SessionId: f.session, RequestId: "prepare",
		Spec: spec, Mode: protocol.PrepareEmbeddingTargetRequest_CREATE})
	c.Assert(err, qt.IsNil)
	c.Assert(prepared.Outcome, qt.Equals, protocol.EmbeddingWriteResult_APPLIED)
	f.registered(c, "r1")
	keys := []*protocol.EmbeddingKey{key("1")}
	stale, err := f.client.ReadEmbeddingBatchState(c.Context(), &protocol.ReadEmbeddingBatchStateRequest{SessionId: f.session, Spec: spec, RunId: "r1", Keys: keys})
	c.Assert(err, qt.IsNil)
	taken := proto.CloneOf(stale.Run)
	taken.LeaseOwner, taken.FencingToken = "worker-b", 2
	_, err = f.mutate(c, runScope("r1"), f.read(c, runScope("r1")).Token, &protocol.EmbeddingRecords{Runs: []*protocol.EmbeddingRun{taken}})
	c.Assert(err, qt.IsNil)

	fenced := proto.CloneOf(stale.Run)
	fenced.LeaseOwner, fenced.FencingToken = "worker-a", 1
	refused, err := f.client.CommitEmbeddingBatch(c.Context(), &protocol.CommitEmbeddingBatchRequest{SessionId: f.session, RequestId: "fenced", Spec: spec,
		RunId: "r1", Keys: keys, ExpectedToken: stale.Token, Run: fenced, Writes: []*protocol.EmbeddingTargetWrite{upsertWrite("1", 0, 1, 0, 0, 0)}})

	c.Assert(err, qt.ErrorIs, provider.ErrEmbeddingConflict)
	c.Assert(refused.Outcome, qt.Equals, protocol.EmbeddingWriteResult_CONFLICT)
	c.Assert(f.read(c, runScope("r1")).Records.Runs[0].LeaseOwner, qt.Equals, "worker-b")
	c.Assert(f.verification(c, spec, 10)[0], qt.Equals, "1 true 0:UNSPECIFIED:0")
}

// An event takes its run's next sequence, a page of events ends where its
// bound does, and a run that is not there has no events and takes none.
func TestEmbeddingEventsSubprocess(t *testing.T) {
	c := qt.New(t)
	f := newEmbeddingFixture(c)
	f.registered(c, "r1")
	var sequences []int64
	for _, kind := range []string{"prepared", "claimed", "checkpoint"} {
		result, err := f.client.AppendEmbeddingEvent(c.Context(), &protocol.AppendEmbeddingEventRequest{SessionId: f.session, RequestId: kind,
			Event: &protocol.EmbeddingEvent{RunId: "r1", Kind: kind, At: at(1_790_000_000, 0), Counts: &protocol.EmbeddingEventCounts{RowsScanned: 2}}})
		c.Assert(err, qt.IsNil)
		sequences = append(sequences, result.Sequence)
	}
	c.Assert(sequences, qt.DeepEquals, []int64{1, 2, 3})
	missing, err := f.client.AppendEmbeddingEvent(c.Context(), &protocol.AppendEmbeddingEventRequest{SessionId: f.session, RequestId: "missing",
		Event: &protocol.EmbeddingEvent{RunId: "r9", Kind: "prepared", At: at(1_790_000_000, 0)}})
	c.Assert(err, qt.ErrorAs, new(*provider.EmbeddingWriteError))
	c.Assert(missing.GetRefusal().GetReason(), qt.Equals, protocol.EmbeddingRefusal_ABSENT)

	for _, test := range []struct {
		name     string
		run      string
		after    int64
		max      uint32
		exists   bool
		kinds    []string
		complete bool
	}{
		{name: "one page of one", run: "r1", after: 1, max: 1, exists: true, kinds: []string{"claimed"}, complete: false},
		{name: "the rest", run: "r1", after: 2, max: 5, exists: true, kinds: []string{"checkpoint"}, complete: true},
		{name: "a run that is not there", run: "r9", max: 5, kinds: []string{}, complete: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := qt.New(t)
			page, err := f.client.ReadEmbeddingEvents(c.Context(), &protocol.ReadEmbeddingEventsRequest{SessionId: f.session, RunId: test.run,
				AfterSequence: test.after, MaxEvents: test.max})
			c.Assert(err, qt.IsNil)
			kinds := []string{}
			for _, event := range page.Events {
				kinds = append(kinds, event.Kind)
			}
			c.Assert([]any{page.Exists, kinds, page.Complete}, qt.DeepEquals, []any{test.exists, test.kinds, test.complete})
		})
	}
}

// commit writes a batch on the token its state was read with.
func (f *embeddingFixture) commit(c *qt.C, spec *protocol.EmbeddingSpec, keys []*protocol.EmbeddingKey, writes ...*protocol.EmbeddingTargetWrite) (*protocol.EmbeddingWriteResult, error) {
	c.Helper()
	state, err := f.client.ReadEmbeddingBatchState(c.Context(), &protocol.ReadEmbeddingBatchStateRequest{SessionId: f.session, Spec: spec, RunId: "r1", Keys: keys})
	c.Assert(err, qt.IsNil)
	run := proto.CloneOf(state.Run)
	run.Progress.BatchesCommitted++
	return f.client.CommitEmbeddingBatch(c.Context(), &protocol.CommitEmbeddingBatchRequest{SessionId: f.session, RequestId: "commit", Spec: spec,
		RunId: "r1", Keys: keys, ExpectedToken: state.Token, Run: run, Writes: writes})
}

func upsertWrite(id string, ordinal uint32, vector ...float32) *protocol.EmbeddingTargetWrite {
	return &protocol.EmbeddingTargetWrite{Key: key(id), Ordinal: ordinal, Generation: testGeneration, InputHash: "hash-" + id,
		Version: "2026-10-01T00:00:00Z", Kind: protocol.EmbeddingWriteKind_EMBEDDING_WRITE_KIND_UPSERT, Vector: vector}
}

func kindWrite(id string, kind protocol.EmbeddingWriteKind) *protocol.EmbeddingTargetWrite {
	return &protocol.EmbeddingTargetWrite{Key: key(id), Generation: testGeneration, InputHash: "hash-" + id, Version: "2026-10-01T00:00:00Z", Kind: kind}
}

// verification walks the whole of a generation's verification, page by page,
// as key:ordinal:kind:dimension, with "-" for a side that is absent.
func (f *embeddingFixture) verification(c *qt.C, spec *protocol.EmbeddingSpec, max uint32) []string {
	c.Helper()
	var walked []string
	token := ""
	for range 20 {
		page, err := f.client.ReadEmbeddingVerification(c.Context(), &protocol.ReadEmbeddingVerificationRequest{SessionId: f.session, Spec: spec,
			PageToken: token, MaxRows: max})
		c.Assert(err, qt.IsNil)
		for _, row := range page.Rows {
			stored := "-"
			if row.Stored != nil {
				stored = fmt.Sprintf("%d:%s:%d", row.Stored.Ordinal, strings.TrimPrefix(row.Stored.Kind.String(), "EMBEDDING_WRITE_KIND_"), row.Stored.Dimension)
			}
			walked = append(walked, fmt.Sprintf("%s %t %s", strings.Join(row.Key.Components, ","), row.Source != nil, stored))
		}
		token = page.NextPageToken
		if page.Complete {
			return walked
		}
	}
	c.Fatal("the verification walk did not end")
	return nil
}

// A generation in columns beside the source: the storage is added and
// verified, the source is paged by key, a batch writes its sets and its
// checkpoint together on the token it was read with, the walk pages over both
// sides, the index is built once, a search finds the nearest row through it
// and by scan, and the retirement drops what the generation added once it is
// proven the generation's.
func TestEmbeddingGenerationInSourceColumnsSubprocess(t *testing.T) {
	c := qt.New(t)
	f := newEmbeddingFixture(c)
	f.seedArticles(c)
	spec := articlesSpec(protocol.EmbeddingTarget_SOURCE_COLUMNS, true)

	described, err := f.client.DescribeEmbeddingTarget(c.Context(), &protocol.DescribeEmbeddingTargetRequest{Engine: "ydb", Spec: spec})
	c.Assert(err, qt.IsNil)
	c.Assert(described, qt.CmpEquals(protocmp.Transform()), &protocol.EmbeddingTargetObjects{ColumnType: "String", HasIndex: true,
		IndexMethod: "vector_kmeans_tree", IndexOperator: "distance=cosine"})
	measured, err := f.client.MeasureEmbedding(c.Context(), &protocol.MeasureEmbeddingRequest{SessionId: f.session, Spec: spec})
	c.Assert(err, qt.IsNil)
	c.Assert([]any{measured.SourceExists, measured.TargetColumnExists, measured.Rows, measured.Capabilities["vector_type"], measured.IndexBuildable},
		qt.DeepEquals, []any{true, false, int64(5), true, true})

	for _, mode := range []protocol.PrepareEmbeddingTargetRequest_Mode{protocol.PrepareEmbeddingTargetRequest_CREATE, protocol.PrepareEmbeddingTargetRequest_CREATE,
		protocol.PrepareEmbeddingTargetRequest_VERIFY} {
		prepared, err := f.client.PrepareEmbeddingTarget(c.Context(), &protocol.PrepareEmbeddingTargetRequest{SessionId: f.session, RequestId: "prepare",
			Spec: spec, Mode: mode})
		c.Assert(err, qt.IsNil)
		c.Assert(prepared.Outcome, qt.Equals, protocol.EmbeddingWriteResult_APPLIED)
	}

	var scanned []string
	var after *protocol.EmbeddingKey
	for range 5 {
		page, err := f.client.ScanEmbeddingSource(c.Context(), &protocol.ScanEmbeddingSourceRequest{SessionId: f.session, Source: spec.Source, After: after, MaxRows: 2})
		c.Assert(err, qt.IsNil)
		for _, row := range page.Rows {
			scanned = append(scanned, row.Key.Components[0]+":"+row.Fields[0].GetStringValue()+":"+row.Version)
		}
		after = page.Cursor
		if page.Done {
			break
		}
	}
	c.Assert(scanned, qt.DeepEquals, []string{"1:Pricing:2026-10-01T00:00:00Z", "2:Support:2026-10-01T00:00:00Z", "3:Billing:2026-10-01T00:00:00Z",
		"4:Renewals:2026-10-01T00:00:00Z", "5:Costs:2026-10-01T00:00:00Z"})
	reread, err := f.client.ReadEmbeddingSourceRows(c.Context(), &protocol.ReadEmbeddingSourceRowsRequest{SessionId: f.session, Source: spec.Source,
		Keys: []*protocol.EmbeddingKey{key("5"), key("9")}})
	c.Assert(err, qt.IsNil)
	c.Assert(reread.Rows, qt.HasLen, 1)
	c.Assert(reread.Rows[0].Fields[1].GetNull(), qt.IsNotNil)

	f.registered(c, "r1")
	keys := []*protocol.EmbeddingKey{key("1"), key("2"), key("3")}
	committed, err := f.commit(c, spec, keys, upsertWrite("1", 0, 3, 1, 0, 0), upsertWrite("2", 0, 1, 3, 0, 0),
		kindWrite("3", protocol.EmbeddingWriteKind_EMBEDDING_WRITE_KIND_SKIP))
	c.Assert(err, qt.IsNil)
	c.Assert(committed.Written.Runs[0].Progress.BatchesCommitted, qt.Equals, int64(1))
	state, err := f.client.ReadEmbeddingBatchState(c.Context(), &protocol.ReadEmbeddingBatchStateRequest{SessionId: f.session, Spec: spec, RunId: "r1", Keys: keys})
	c.Assert(err, qt.IsNil)
	c.Assert(state.Held, qt.HasLen, 3)
	c.Assert(state.Held[2].Kind, qt.Equals, protocol.EmbeddingWriteKind_EMBEDDING_WRITE_KIND_SKIP)
	stale, err := f.client.CommitEmbeddingBatch(c.Context(), &protocol.CommitEmbeddingBatchRequest{SessionId: f.session, RequestId: "stale", Spec: spec,
		RunId: "r1", Keys: keys, ExpectedToken: committed.RequestId + "-old", Run: state.Run, Writes: []*protocol.EmbeddingTargetWrite{upsertWrite("1", 0, 1, 1, 1, 1)}})
	c.Assert(err, qt.ErrorIs, provider.ErrEmbeddingConflict)
	c.Assert(stale.Outcome, qt.Equals, protocol.EmbeddingWriteResult_CONFLICT)

	c.Assert(f.verification(c, spec, 2), qt.DeepEquals, []string{"1 true 0:UPSERT:4", "2 true 0:UPSERT:4", "3 true 0:SKIP:0",
		"4 true 0:UNSPECIFIED:0", "5 true 0:UNSPECIFIED:0"})
	// The column holds this generation's vectors, so another generation is
	// refused the column before any embedding is paid for.
	another := proto.CloneOf(spec)
	another.Generation = strings.Repeat("f", 64)
	taken, err := f.client.PrepareEmbeddingTarget(c.Context(), &protocol.PrepareEmbeddingTargetRequest{SessionId: f.session, RequestId: "another",
		Spec: another, Mode: protocol.PrepareEmbeddingTargetRequest_CREATE})
	c.Assert(err, qt.ErrorAs, new(*provider.EmbeddingWriteError))
	c.Assert(taken.GetRefusal().GetReason(), qt.Equals, protocol.EmbeddingRefusal_NOT_OWNED)

	for _, want := range []protocol.EmbeddingWriteResult_IndexOutcome{protocol.EmbeddingWriteResult_BUILT, protocol.EmbeddingWriteResult_ALREADY_VALID} {
		built, err := f.client.BuildEmbeddingIndex(c.Context(), &protocol.BuildEmbeddingIndexRequest{SessionId: f.session, RequestId: "index", Spec: spec})
		c.Assert(err, qt.IsNil)
		c.Assert(built.IndexOutcome, qt.Equals, want)
	}
	location := &protocol.EmbeddingLocation{Generation: testGeneration, Table: spec.Target.Table, Column: "embedding", Dimension: 4, Names: spec.Names}
	structure, err := f.client.ReadEmbeddingStructure(c.Context(), &protocol.ReadEmbeddingStructureRequest{SessionId: f.session, Location: location, CountRows: true})
	c.Assert(err, qt.IsNil)
	c.Assert([]any{structure.ColumnType, structure.IndexValid, structure.IndexMethod, structure.IndexOperator, structure.Dimension, structure.Rows},
		qt.DeepEquals, []any{"String", true, "vector_kmeans_tree", "distance=cosine", uint32(4), int64(3)})

	found, err := f.client.SearchEmbeddings(c.Context(), &protocol.SearchEmbeddingsRequest{SessionId: f.session, Spec: spec, Vector: []float32{3, 1, 0, 0}, Depth: 1})
	c.Assert(err, qt.IsNil)
	c.Assert([]any{found.Outcome, found.IndexUsed, found.Exact}, qt.CmpEquals(protocmp.Transform()),
		[]any{protocol.EmbeddingSearchResult_COMPLETED, true, []*protocol.EmbeddingKey{key("1")}})

	// A write after the build is refused on a line whose index answers as it
	// stood at the build.
	afterIndex, _ := f.commit(c, spec, []*protocol.EmbeddingKey{key("4")}, upsertWrite("4", 0, 0, 0, 3, 1))
	wanted := map[bool]protocol.EmbeddingWriteResult_Outcome{true: protocol.EmbeddingWriteResult_APPLIED, false: protocol.EmbeddingWriteResult_REFUSED}
	c.Assert(afterIndex.Outcome, qt.Equals, wanted[f.maintainedOnWrite(c)])

	foreign := proto.CloneOf(location)
	foreign.Names.ColumnComment = "somebody else's column"
	refused, err := f.client.DropEmbeddingGeneration(c.Context(), &protocol.DropEmbeddingGenerationRequest{SessionId: f.session, RequestId: "drop",
		Location: foreign, Storage: protocol.DropEmbeddingGenerationRequest_COLUMNS})
	c.Assert(err, qt.ErrorAs, new(*provider.EmbeddingWriteError))
	c.Assert(refused.GetRefusal().GetReason(), qt.Equals, protocol.EmbeddingRefusal_NOT_OWNED)
	dropped, err := f.client.DropEmbeddingGeneration(c.Context(), &protocol.DropEmbeddingGenerationRequest{SessionId: f.session, RequestId: "drop",
		Location: location, Storage: protocol.DropEmbeddingGenerationRequest_COLUMNS})
	c.Assert(err, qt.IsNil)
	c.Assert(dropped.Outcome, qt.Equals, protocol.EmbeddingWriteResult_APPLIED)
	gone, err := f.client.ReadEmbeddingStructure(c.Context(), &protocol.ReadEmbeddingStructureRequest{SessionId: f.session, Location: location})
	c.Assert(err, qt.IsNil)
	c.Assert([]bool{gone.TableExists, gone.ColumnExists, gone.IndexExists}, qt.DeepEquals, []bool{true, false, false})
}

// A chunked generation in a table of its own: the table is created and
// marked as the generation's, a key's stored rows are made equal to its set
// whatever they were, a tombstone removes them, a write for a source row that
// is gone stores nothing, and the table is dropped only on its mark.
func TestEmbeddingGenerationInATableOfItsOwnSubprocess(t *testing.T) {
	c := qt.New(t)
	f := newEmbeddingFixture(c)
	f.seedArticles(c)
	spec := articlesSpec(protocol.EmbeddingTarget_OWN_TABLE, false)
	prepare := func(c *qt.C, spec *protocol.EmbeddingSpec) (*protocol.EmbeddingWriteResult, error) {
		return f.client.PrepareEmbeddingTarget(c.Context(), &protocol.PrepareEmbeddingTargetRequest{SessionId: f.session, RequestId: "prepare",
			Spec: spec, Mode: protocol.PrepareEmbeddingTargetRequest_CREATE})
	}
	for range 2 {
		prepared, err := prepare(c, spec)
		c.Assert(err, qt.IsNil)
		c.Assert(prepared.Outcome, qt.Equals, protocol.EmbeddingWriteResult_APPLIED)
	}
	other := proto.CloneOf(spec)
	other.Names.OwnershipMark = "ptah embedding target for generation another"
	refused, err := prepare(c, other)
	c.Assert(err, qt.ErrorAs, new(*provider.EmbeddingWriteError))
	c.Assert(refused.GetRefusal().GetReason(), qt.Equals, protocol.EmbeddingRefusal_NOT_OWNED)

	f.registered(c, "r1")
	keys := []*protocol.EmbeddingKey{key("1"), key("2"), key("99")}
	_, err = f.commit(c, spec, keys, upsertWrite("1", 0, 1, 0, 0, 0), upsertWrite("1", 1, 0, 1, 0, 0),
		upsertWrite("2", 0, 1, 1, 0, 0), upsertWrite("2", 1, 0, 1, 1, 0), upsertWrite("99", 0, 1, 1, 1, 1))
	c.Assert(err, qt.IsNil)
	// A page of three would end inside key 2's set, so it ends before it.
	c.Assert(f.verification(c, spec, 3), qt.DeepEquals, []string{"1 true 0:UPSERT:4", "1 true 1:UPSERT:4",
		"2 true 0:UPSERT:4", "2 true 1:UPSERT:4", "3 true -", "4 true -", "5 true -"})
	_, err = f.commit(c, spec, keys[:2], upsertWrite("1", 0, 1, 0, 0, 0), kindWrite("2", protocol.EmbeddingWriteKind_EMBEDDING_WRITE_KIND_TOMBSTONE))
	c.Assert(err, qt.IsNil)
	c.Assert(f.verification(c, spec, 2), qt.DeepEquals, []string{"1 true 0:UPSERT:4", "2 true -", "3 true -", "4 true -", "5 true -"})
	_, err = f.client.ReadEmbeddingVerification(c.Context(), &protocol.ReadEmbeddingVerificationRequest{SessionId: f.session, Spec: spec, MaxRows: 1,
		PageToken: "not a token"})
	c.Assert(err, qt.IsNotNil)

	location := &protocol.EmbeddingLocation{Generation: testGeneration, Table: spec.Target.Table, Column: "embedding", Dimension: 4,
		Names: &protocol.EmbeddingNames{Index: "article_vectors_embedding_" + testGeneration[:12] + "_idx", OwnershipMark: other.Names.OwnershipMark}}
	notOurs, err := f.client.DropEmbeddingGeneration(c.Context(), &protocol.DropEmbeddingGenerationRequest{SessionId: f.session, RequestId: "drop",
		Location: location, Storage: protocol.DropEmbeddingGenerationRequest_TABLE})
	c.Assert(err, qt.ErrorAs, new(*provider.EmbeddingWriteError))
	c.Assert(notOurs.GetRefusal().GetReason(), qt.Equals, protocol.EmbeddingRefusal_NOT_OWNED)
	location.Names.OwnershipMark = spec.Names.OwnershipMark
	dropped, err := f.client.DropEmbeddingGeneration(c.Context(), &protocol.DropEmbeddingGenerationRequest{SessionId: f.session, RequestId: "drop",
		Location: location, Storage: protocol.DropEmbeddingGenerationRequest_TABLE})
	c.Assert(err, qt.IsNil)
	c.Assert(dropped.Outcome, qt.Equals, protocol.EmbeddingWriteResult_APPLIED)
	structure, err := f.client.ReadEmbeddingStructure(c.Context(), &protocol.ReadEmbeddingStructureRequest{SessionId: f.session, Location: location})
	c.Assert(err, qt.IsNil)
	c.Assert(structure.TableExists, qt.IsFalse)
}

// A run-state table of another shape is not the store's: preparing the store
// refuses it whole, and leaves the table as it was.
func TestEmbeddingStoreRefusesAForeignLayoutSubprocess(t *testing.T) {
	c := qt.New(t)
	f := newEmbeddingFixture(c)
	f.exec(c, "CREATE TABLE ptah_embedding_run (id Utf8 NOT NULL, phase Utf8, PRIMARY KEY (id))")

	result, err := f.client.PrepareEmbeddingStore(c.Context(), &protocol.PrepareEmbeddingStoreRequest{SessionId: f.session, RequestId: "store"})

	c.Assert(err, qt.ErrorAs, new(*provider.EmbeddingWriteError))
	c.Assert(result.GetRefusal().GetReason(), qt.Equals, protocol.EmbeddingRefusal_NOT_OWNED)
	_, err = f.client.ReadEmbeddingState(c.Context(), &protocol.ReadEmbeddingStateRequest{SessionId: f.session, Scope: everything(), MaxRecords: 10})
	c.Assert(err, qt.IsNotNil)
}

// A host that does not list embeddings.store is offered no store: the engine
// does not declare one and the client refuses every call before sending it.
func TestEmbeddingStoreNeedsTheHostsFeatureSubprocess(t *testing.T) {
	c := qt.New(t)
	client := startEmbeddingProvider(c)

	c.Assert(client.Description().Engines[0].Features, qt.Not(qt.Contains), provider.EmbeddingStoreFeature)
	c.Assert(client.Description().Engines[0].Embeddings, qt.IsNil)
	_, err := client.DescribeEmbeddingTarget(c.Context(), &protocol.DescribeEmbeddingTargetRequest{Engine: "ydb",
		Spec: articlesSpec(protocol.EmbeddingTarget_SOURCE_COLUMNS, false)})
	c.Assert(err, qt.IsNotNil)
}
