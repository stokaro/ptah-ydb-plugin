//go:build integration

package integration_test

import (
	"context"
	"path"
	"strings"
	"testing"
	"time"

	qt "github.com/frankban/quicktest"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	protocol "ptah.run/provider-sdk/protocol/experimental"

	"github.com/stokaro/ptah-ydb-plugin/internal/connection"
	native "github.com/stokaro/ptah-ydb-plugin/internal/nativeprotocol"
	"github.com/stokaro/ptah-ydb-plugin/internal/tablecreate"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
	"github.com/stokaro/ptah-ydb-plugin/internal/testplanning"
)

// The local indexes the host's TestYDBColumnStore_RoundTrip declares on 26.2:
// a bloom_filter index with a probability of 0.01 and a bloom_ngram_filter
// index with its defaults, both over the text column.
const (
	localBloom = `{"name":"bf","columns":["body"],"bloom":{"falsePositiveProbability":0.01}}`
	localNgram = `{"name":"ng","columns":["body"],"ngram":{}}`
)

// localIndexStorage is a column table hashed by id in one shard with the given
// local indexes.
func localIndexStorage(indexes ...string) string {
	return `{"hashColumns":["id"],"partitions":"1","localIndexes":[` + strings.Join(indexes, ",") + `]}`
}

// creationPlan plans the creation of records with the given storage in session
// and returns the plan.
func (f *columnTableFixture) creationPlan(c *qt.C, session *protocol.Session, storage string) *protocol.PlannedFeatures {
	c.Helper()
	subject := testplanning.Subject(f.owner)
	planning := &protocol.PlanFeaturesRequest{Engine: "ydb", TargetFeatures: negotiated(f.client), PlanningId: tablecreate.ServiceID, ParentKinds: tablecreate.ParentKinds(), DatabasePath: f.database,
		Capabilities: session.Capabilities, Identifiers: &protocol.ComparisonIdentifiers{Rules: proto.CloneOf(session.Identifiers)},
		Tables: []*protocol.PlanningTable{{Subject: subject, Action: protocol.PlanningTable_CREATE, Desired: f.declaration(c, storage)}},
		CommonSteps: []*protocol.CommonPlanningStep{{Id: &protocol.StepIdentity{Owner: "host", Name: "create"}, Parent: proto.CloneOf(subject),
			Metadata: &protocol.PlanningMetadata{Transaction: protocol.PlanningMetadata_FORBIDDEN, Impact: protocol.ChangeSemantics_ADDITIVE, Reason: "Creates a table",
				Effects: []*protocol.PlanningEffect{{Subject: proto.CloneOf(subject), Action: protocol.PlanningEffect_CREATE}}}}}}
	prepareCapturedCreation(c, f.client, planning)
	plan, err := f.client.PlanFeatures(c.Context(), planning)
	c.Assert(err, qt.IsNil)
	return plan
}

// heldLocalIndexes are the local indexes the provider reads back from records,
// by name.
func (f *columnTableFixture) heldLocalIndexes(c *qt.C) map[string]*native.LocalIndexObservation {
	c.Helper()
	result := make(map[string]*native.LocalIndexObservation)
	for _, index := range f.held(c).LocalIndexes {
		result[index.Name] = index
	}
	return result
}

// requireLocalIndexes requires the table to hold exactly the given local
// indexes, each as its declaration states it with the server's defaults.
func (f *columnTableFixture) requireLocalIndexes(c *qt.C, indexes ...string) {
	c.Helper()
	held := f.heldLocalIndexes(c)
	c.Assert(held, qt.HasLen, len(indexes))
	for _, text := range indexes {
		var want native.LocalIndexObservation
		c.Assert(protojson.Unmarshal([]byte(text), &want), qt.IsNil)
		c.Assert(held[want.Name], qt.IsNotNil, qt.Commentf("local index %s", want.Name))
		c.Assert(tablecreate.LocalIndexEqual(&want, held[want.Name]), qt.IsTrue, qt.Commentf("local index %s: %v", want.Name, held[want.Name]))
	}
}

// TestColumnTableLocalIndexesRoundTripSubprocess is the local-index part of
// the host's TestYDBColumnStore_RoundTrip: on a line that builds them, a
// column table is created with a bloom_filter and a bloom_ngram_filter index,
// reads back with both as declared, and plans nothing after.
func TestColumnTableLocalIndexesRoundTripSubprocess(t *testing.T) {
	c := testcheck.New(t)
	fixture := newColumnTableFixture(c, monitoredAddress(c))
	requireSessionCapability(c, fixture.session, tablecreate.LocalBloomIndexesCapability)
	requireSessionCapability(c, fixture.session, tablecreate.LocalNgramIndexesCapability)
	storage := localIndexStorage(localBloom, localNgram)
	execution, err := fixture.create(c, fixture.session, "create-local-indexes", storage)
	c.Assert(err, qt.IsNil)
	c.Assert(execution.Outcome, qt.Equals, protocol.Execution_APPLIED, qt.Commentf("%s", execution))
	fixture.requireLocalIndexes(c, localBloom, localNgram)
	c.Assert(fixture.heldLocalIndexes(c)["bf"].GetBloom().GetFalsePositiveProbability(), qt.Equals, 0.01)
	c.Assert(fixture.compare(c, storage).Changes, qt.HasLen, 0)
	same := fixture.alter(c, storage, nil)
	c.Assert(same.Diagnostics, qt.HasLen, 0)
	c.Assert(same.Contributions, qt.HasLen, 0)
}

// TestColumnTableLocalIndexChangesSubprocess is the host's
// TestYDBColumnStore_IndexChanges with an addition and a removal beside it: a
// renamed local bloom index is dropped under its old name and added under its
// new one, as the host's embedded engine plans it without index_rename, a
// change of its probability drops and adds it again, and an index added to the
// table and one dropped from it take one step each. Each change converges.
func TestColumnTableLocalIndexChangesSubprocess(t *testing.T) {
	c := testcheck.New(t)
	fixture := newColumnTableFixture(c, monitoredAddress(c))
	requireSessionCapability(c, fixture.session, tablecreate.LocalBloomIndexesCapability)
	requireSessionCapability(c, fixture.session, tablecreate.LocalNgramIndexesCapability)
	execution, err := fixture.create(c, fixture.session, "create-local-indexes", localIndexStorage(localBloom, localNgram))
	c.Assert(err, qt.IsNil)
	c.Assert(execution.Outcome, qt.Equals, protocol.Execution_APPLIED, qt.Commentf("%s", execution))
	renamed := `{"name":"renamed_bloom","columns":["body"],"bloom":{"falsePositiveProbability":0.01}}`
	retuned := `{"name":"renamed_bloom","columns":["body"],"bloom":{"falsePositiveProbability":0.02}}`
	onID := `{"name":"bf_id","columns":["id"],"bloom":{}}`
	for _, test := range []struct {
		name    string
		indexes []string
		steps   []string
	}{
		{"the bloom index renamed", []string{renamed, localNgram}, []string{"column-table-drop-local-index/000000", "column-table-add-local-index/000000"}},
		{"its probability changed", []string{retuned, localNgram}, []string{"column-table-drop-local-index/000000", "column-table-add-local-index/000000"}},
		{"an index added", []string{retuned, localNgram, onID}, []string{"column-table-add-local-index/000000"}},
		{"an index dropped", []string{retuned, onID}, []string{"column-table-drop-local-index/000000"}},
	} {
		storage := localIndexStorage(test.indexes...)
		plan, execution, err := fixture.change(c, "local-"+test.name, storage)
		c.Assert(err, qt.IsNil, qt.Commentf("%s", test.name))
		c.Assert(execution.Outcome, qt.Equals, protocol.Execution_APPLIED, qt.Commentf("%s: %s", test.name, execution))
		c.Assert(stepNames(plan), qt.DeepEquals, test.steps, qt.Commentf("%s", test.name))
		fixture.requireLocalIndexes(c, test.indexes...)
		c.Assert(fixture.compare(c, storage).Changes, qt.HasLen, 0, qt.Commentf("%s", test.name))
	}
}

// TestColumnTableLocalIndexStalePlanSubprocess plans the removal of an index,
// lets another plan change the index first, and runs the first plan: the table
// no longer holds the index the plan captured, so the statement is refused
// before it runs.
func TestColumnTableLocalIndexStalePlanSubprocess(t *testing.T) {
	c := testcheck.New(t)
	fixture := newColumnTableFixture(c, monitoredAddress(c))
	requireSessionCapability(c, fixture.session, tablecreate.LocalBloomIndexesCapability)
	execution, err := fixture.create(c, fixture.session, "create-local-indexes", localIndexStorage(localBloom))
	c.Assert(err, qt.IsNil)
	c.Assert(execution.Outcome, qt.Equals, protocol.Execution_APPLIED, qt.Commentf("%s", execution))
	moved := localIndexStorage(`{"name":"bf_id","columns":["id"],"bloom":{}}`)
	stale := fixture.alter(c, moved, fixture.compare(c, moved).Changes)
	c.Assert(stale.Diagnostics, qt.HasLen, 0)
	_, execution, err = fixture.change(c, "local-first", localIndexStorage(`{"name":"bf","columns":["body"],"bloom":{"falsePositiveProbability":0.05}}`))
	c.Assert(err, qt.IsNil)
	c.Assert(execution.Outcome, qt.Equals, protocol.Execution_APPLIED, qt.Commentf("%s", execution))
	rendered, err := fixture.client.Render(c.Context(), testplanning.RenderRequest(fixture.client.Description(), stale, fixture.session.Capabilities))
	c.Assert(err, qt.IsNil)
	execution, err = fixture.client.Execute(c.Context(), &protocol.ExecuteRequest{SessionId: fixture.session.Id, RequestId: "local-stale", Units: rendered.Units})
	c.Assert(err, qt.IsNotNil)
	c.Assert(execution.Outcome, qt.Equals, protocol.Execution_NOT_STARTED)
	c.Assert(fixture.heldLocalIndexes(c)["bf"].GetBloom().GetFalsePositiveProbability(), qt.Equals, 0.05)
}

// TestColumnTableLocalIndexOlderLineSubprocess holds a line without the local
// index grammar to the capability: the creation is refused before a statement
// is sent, naming the capability, and the server refuses the index itself.
func TestColumnTableLocalIndexOlderLineSubprocess(t *testing.T) {
	c := testcheck.New(t)
	fixture := newColumnTableFixture(c, monitoredAddress(c))
	requireNoSessionCapability(c, fixture.session, tablecreate.LocalBloomIndexesCapability)
	plan := fixture.creationPlan(c, fixture.session, localIndexStorage(localBloom))
	c.Assert(plan.Diagnostics, qt.HasLen, 1)
	c.Assert(plan.Diagnostics[0].Message, qt.Matches, `local index "bf" is a bloom_filter index, which requires target capability local_bloom_indexes, unavailable on this ydb target .*`)
	c.Assert(plan.Contributions, qt.HasLen, 0)
	err := execFixtureSchema(c.Context(), fixture.connection(c).DB, "CREATE TABLE `"+path.Join(fixture.database, fixture.directory, "records")+"` "+
		"(id Uint64 NOT NULL, ts Timestamp NOT NULL, body Utf8, PRIMARY KEY (ts, id), INDEX bf LOCAL USING bloom_filter ON (body)) PARTITION BY HASH (id) WITH (STORE = COLUMN)")
	c.Assert(err, qt.ErrorMatches, `(?s).*local: alternative is not implemented yet.*`)
}

// TestColumnTableLocalMinMaxIndexSubprocess holds a min_max index to its
// flag: on a cluster at its defaults the session reports no
// local_min_max_indexes, the creation is refused before a statement is sent,
// and the server refuses the index itself. With EnableLocalMinMaxIndex turned
// on, a new session reports the capability and the table is created with the
// index. A TTL on the key column the index reads, which the host's embedded
// engine takes for a column after the first in the key, is refused before a
// statement is sent, as the server refuses it with the index in place.
func TestColumnTableLocalMinMaxIndexSubprocess(t *testing.T) {
	c := testcheck.New(t)
	address := monitoredAddress(c)
	fixture := newColumnTableFixture(c, address)
	requireSessionCapability(c, fixture.session, tablecreate.LocalBloomIndexesCapability)
	c.Assert(fixture.session.Capabilities[tablecreate.LocalMinMaxIndexesCapability], qt.IsFalse)
	minMax := `{"name":"mm","columns":["id"],"minMax":{}}`
	storage := `{"hashColumns":["id"],"partitions":"1","localIndexes":[` + minMax + `]}`
	plan := fixture.creationPlan(c, fixture.session, storage)
	c.Assert(plan.Diagnostics, qt.HasLen, 1)
	c.Assert(plan.Diagnostics[0].Message, qt.Matches, `local index "mm" is a min_max index, which requires target capability local_min_max_indexes, .*EnableLocalMinMaxIndex.*`)
	opened := fixture.connection(c)
	table := "`" + path.Join(fixture.database, fixture.directory, "records") + "`"
	err := execFixtureSchema(c.Context(), opened.DB, "CREATE TABLE "+table+" (id Uint64 NOT NULL, ts Timestamp NOT NULL, body Utf8, PRIMARY KEY (ts, id), "+
		"INDEX mm LOCAL USING min_max ON (id)) PARTITION BY HASH (id) WITH (STORE = COLUMN)")
	c.Assert(err, qt.ErrorMatches, `(?s).*Local min_max index is disabled with EnableLocalMinMaxIndex feature flag.*`)

	newClusterFlags(c, opened).set(c, map[string]bool{"EnableLocalMinMaxIndex": true})
	session, err := fixture.client.Open(c.Context(), &protocol.OpenRequest{Engine: "ydb", Address: address})
	c.Assert(err, qt.IsNil)
	c.Cleanup(func() { c.Check(fixture.client.CloseSession(context.Background(), session.Id), qt.IsNil) })
	c.Assert(session.Capabilities[tablecreate.LocalMinMaxIndexesCapability], qt.IsTrue)
	fixture.session = session
	withTTL := `{"hashColumns":["id"],"partitions":"1","ttl":{"enabled":{"column":"id","columnUnit":"1","tiers":[{"applyAfterSeconds":"86400","delete":{}}]}},` +
		`"localIndexes":[` + minMax + `]}`
	_, err = fixture.client.PrepareTables(c.Context(), &protocol.PrepareTablesRequest{Engine: "ydb", Capabilities: session.Capabilities,
		Identifiers: &protocol.ComparisonIdentifiers{Rules: proto.CloneOf(session.Identifiers)},
		Tables: []*protocol.TablePreparationInput{{Subject: testplanning.Subject(fixture.owner), Desired: fixture.declaration(c, withTTL),
			CurrentKnowledge: &protocol.ModelKnowledge{State: protocol.ModelKnowledge_ABSENT}}}})
	c.Assert(err, qt.ErrorMatches, `.*\(InvalidArgument\)`)
	err = execFixtureSchema(c.Context(), opened.DB, "CREATE TABLE "+table+" (id Uint64 NOT NULL, ts Timestamp NOT NULL, body Utf8, PRIMARY KEY (ts, id), "+
		"INDEX mm LOCAL USING min_max ON (id)) PARTITION BY HASH (id) WITH (STORE = COLUMN, TTL = Interval(\"PT86400S\") DELETE ON `id` AS SECONDS)")
	c.Assert(err, qt.ErrorMatches, `(?s).*Haven't MAX-index for TTL column and TTL column is not first column in primary key.*`)
	execution, err := fixture.create(c, session, "create-min-max", storage)
	c.Assert(err, qt.IsNil)
	c.Assert(execution.Outcome, qt.Equals, protocol.Execution_APPLIED, qt.Commentf("%s", execution))
	fixture.requireLocalIndexes(c, minMax)
	c.Assert(fixture.compare(c, storage).Changes, qt.HasLen, 0)
}

// connection opens the fixture's database, closed when the test ends.
func (f *columnTableFixture) connection(c *qt.C) *connection.Connection {
	c.Helper()
	address, err := connection.Parse(monitoredAddress(c))
	c.Assert(err, qt.IsNil)
	ctx, cancel := context.WithTimeout(c.Context(), time.Minute)
	defer cancel()
	opened, err := connection.Open(ctx, address)
	c.Assert(err, qt.IsNil)
	c.Cleanup(func() { c.Check(opened.Close(), qt.IsNil) })
	return opened
}
