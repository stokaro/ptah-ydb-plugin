//go:build integration

package integration_test

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	qt "github.com/frankban/quicktest"
	"github.com/stokaro/ptah-ydb-plugin/schema"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/stokaro/ptah/provider-sdk/provider"
	providerruntime "github.com/stokaro/ptah/provider-sdk/runtime"
	"google.golang.org/protobuf/proto"
)

// runCommon renders changes in form and executes the units in one request.
func runCommon(c *qt.C, client *providerruntime.Client, session *protocol.Session, form protocol.RenderRequest_Form, transaction string,
	changes ...*protocol.Change) (*protocol.Execution, error) {
	c.Helper()
	return runCommonIn(c.Context(), c, client, session, form, transaction, changes...)
}

// runCommonIn is runCommon under ctx, for a cleanup, which runs after the
// test's own context is done.
func runCommonIn(ctx context.Context, c *qt.C, client *providerruntime.Client, session *protocol.Session, form protocol.RenderRequest_Form, transaction string,
	changes ...*protocol.Change) (*protocol.Execution, error) {
	c.Helper()
	request := &protocol.RenderRequest{Engine: "ydb", Form: form, Capabilities: map[string]bool{"transactional_ddl": false},
		TargetFeatures: client.Description().Engines[0].Features}
	for i, change := range changes {
		change = proto.CloneOf(change)
		change.Id = string(rune('a' + i))
		request.Changes = append(request.Changes, change)
	}
	rendered, err := client.Render(ctx, request)
	c.Assert(err, qt.IsNil)
	return client.Execute(ctx, &protocol.ExecuteRequest{SessionId: session.Id, RequestId: "common", TransactionId: transaction, Units: rendered.Units})
}

// commonRead is what an inspection of directory reports of the common objects:
// each view with its query, and each comment as "kind path/name: text".
func commonRead(c *qt.C, client *providerruntime.Client, session *protocol.Session, directories ...string) (map[string]string, []string) {
	c.Helper()
	snapshot, err := client.Inspect(c.Context(), &protocol.InspectRequest{SessionId: session.Id, Schemas: directories})
	c.Assert(err, qt.IsNil)
	c.Assert(snapshot.CommonObjects.GetCoverage(), qt.HasLen, 4)
	for _, record := range snapshot.Tables {
		for _, facet := range record.Facets {
			c.Assert(string(facet.Payload), qt.Not(qt.Contains), "ptah.comment", qt.Commentf("a comment is no table property"))
		}
	}
	for _, object := range snapshot.NativeObjects {
		c.Assert(object.Kind, qt.Not(qt.Equals), schema.ViewProperties, qt.Commentf("a view the common wire carries is no native object"))
	}
	views := map[string]string{}
	for _, view := range snapshot.CommonObjects.GetViews() {
		views[view.Identity.Name] = view.Body
	}
	var comments []string
	for _, comment := range snapshot.CommonObjects.GetComments() {
		comments = append(comments, comment.Object.Kind+" "+strings.Join(append(slices.Clone(comment.Object.Path), comment.Object.Name), "/")+": "+comment.Text)
	}
	return views, comments
}

// prepared is the body PrepareCommonObjects gives the declared view v.
func prepared(c *qt.C, client *providerruntime.Client, directory, body string) string {
	c.Helper()
	result, err := client.PrepareCommonObjects(c.Context(), &protocol.PrepareCommonObjectsRequest{Engine: "ydb", Identifiers: commonIdentifiers(client),
		Capabilities: map[string]bool{"transactional_ddl": false}, TargetFeatures: client.Description().Engines[0].Features,
		Declared: &protocol.DeclaredCommonObjects{Views: []*protocol.SQLView{{Schema: directory, Name: "v", Body: body}}}})
	c.Assert(err, qt.IsNil)
	return result.Prepared.Views[0].Body
}

// TestCommonObjectsSubprocess creates a commented view and comments a table,
// one of its columns and one of its indexes through the common wire, natively
// and by the statements a migration file holds, and reads each back: the view
// as the server stores its query, which is what the preparation of its
// declaration gives, so a second plan finds nothing to change.
func TestCommonObjectsSubprocess(t *testing.T) {
	c := qt.New(t)
	f := newFacetFixtureListing(c, ", INDEX by_ts GLOBAL ON (ts)", "", provider.CommonObjectsFeature, provider.TextUnitsFeature)
	ctx, cancel := context.WithTimeout(c.Context(), 4*time.Minute)
	defer cancel()
	c.Assert(f.session.Capabilities["view_comments"], qt.IsTrue)
	dir := f.directory
	table := &protocol.Reference{Engine: "ydb", Kind: "table", Schema: dir, Name: "records"}
	column := &protocol.Reference{Engine: "ydb", Kind: "column", Schema: dir, Name: "payload", Path: []string{"records"}}
	index := &protocol.Reference{Engine: "ydb", Kind: "index", Schema: dir, Name: "by_ts", Path: []string{"records"}}
	view := &protocol.Reference{Engine: "ydb", Kind: "view", Schema: dir, Name: "v"}
	body := "SELECT id,\n  payload -- why\nFROM `" + dir + "/records` WHERE id>=1 AND payload != 'it\\'s';"
	create := viewChange(body, "a view")
	create.Object.Schema = dir
	create.GetCommonObject().GetCreateView().View.Schema = dir
	drop := &protocol.Change{Object: view, Operation: &protocol.Change_CommonObject{CommonObject: &protocol.CommonObjectChange{
		Operation: &protocol.CommonObjectChange_DropView{DropView: view}}}}

	for _, form := range []protocol.RenderRequest_Form{protocol.RenderRequest_ANY, protocol.RenderRequest_TEXT} {
		result, err := runCommon(c, f.client, f.session, form, "", create, commentChange(table, "rows"), commentChange(column, "the text"),
			commentChange(index, "lookups"))
		c.Assert(err, qt.IsNil)
		c.Assert(result.Outcome, qt.Equals, protocol.Execution_APPLIED, qt.Commentf("%s", result))
		views, comments := commonRead(c, f.client, f.session, dir)
		c.Assert(views, qt.DeepEquals, map[string]string{"v": "SELECT id , payload FROM `" + dir + "/records` WHERE id >= 1 AND payload != 'it\\'s'"})
		c.Assert(prepared(c, f.client, dir, body), qt.Equals, views["v"])
		c.Assert(comments, qt.DeepEquals, []string{"table records: rows", "view v: a view", "column records/payload: the text", "index records/by_ts: lookups"})

		result, err = runCommon(c, f.client, f.session, form, "", drop, commentChange(column, ""), commentChange(table, ""), commentChange(index, ""))
		c.Assert(err, qt.IsNil)
		c.Assert(result.Outcome, qt.Equals, protocol.Execution_APPLIED, qt.Commentf("%s", result))
		views, comments = commonRead(c, f.client, f.session, dir)
		c.Assert(views, qt.HasLen, 0)
		c.Assert(comments, qt.HasLen, 0)
	}

	// A comment on a column the table does not have is refused before
	// anything is written, and so is a comment on a column table's column,
	// which YDB would not keep.
	result, err := runCommon(c, f.client, f.session, protocol.RenderRequest_TEXT, "",
		commentChange(&protocol.Reference{Engine: "ydb", Kind: "column", Schema: dir, Name: "missing", Path: []string{"records"}}, "x"))
	c.Assert(err, qt.ErrorIs, provider.ErrExecution)
	c.Assert(result.Outcome, qt.Equals, protocol.Execution_NOT_STARTED)
	c.Assert(result.Message, qt.Matches, `invalid comment statement: table /.*/records has no column "missing"`)

	// A comment statement runs outside a transaction, as YDB runs every scheme
	// statement, and one Ptah cannot read is refused by name.
	handle, err := f.client.BeginTransaction(ctx, &protocol.BeginTransactionRequest{SessionId: f.session.Id, TransactionId: "comment",
		Isolation: protocol.TransactionIsolation_SERIALIZABLE})
	c.Assert(err, qt.IsNil)
	result, err = runCommon(c, f.client, f.session, protocol.RenderRequest_TEXT, handle.Id, commentChange(table, "rows"))
	c.Assert(err, qt.ErrorIs, provider.ErrExecution)
	c.Assert(result.DiagnosticCode, qt.Equals, "ydb.native-transaction-forbidden")
	_, err = f.client.FinishTransaction(ctx, &protocol.FinishTransactionRequest{SessionId: f.session.Id, TransactionId: handle.Id, Action: protocol.FinishTransactionRequest_ROLLBACK})
	c.Assert(err, qt.IsNil)
	result, err = f.client.Execute(ctx, &protocol.ExecuteRequest{SessionId: f.session.Id, RequestId: "bad", Units: []*protocol.Unit{
		{ChangeId: "bad", Body: &protocol.Unit_Sql{Sql: "COMMENT ON TABLE `" + dir + "/records` IS 42;"}}}})
	c.Assert(err, qt.ErrorIs, provider.ErrExecution)
	c.Assert(result.DiagnosticCode, qt.Equals, "ydb.invalid-comment-statement")
	_, comments := commonRead(c, f.client, f.session, dir)
	c.Assert(comments, qt.HasLen, 0)
}

// TestCommonObjectsInARealmSubprocess creates a view on a dev realm, where
// the session's path prefix heads every query and YDB stores it in the view:
// the read leaves that prefix out, so the view reads as its declaration.
func TestCommonObjectsInARealmSubprocess(t *testing.T) {
	c := qt.New(t)
	client := startProviderFor(c, provider.CommonObjectsFeature, provider.TextUnitsFeature)
	id, _, _ := realmSession(c, client)
	session := &protocol.Session{Id: id}
	view := &protocol.Reference{Engine: "ydb", Kind: "view", Name: "v"}
	create := viewChange("SELECT 1 AS one", "in a realm")
	create.Object.Schema = ""
	create.GetCommonObject().GetCreateView().View.Schema = ""
	result, err := runCommon(c, client, session, protocol.RenderRequest_TEXT, "", create)
	c.Assert(err, qt.IsNil)
	c.Assert(result.Outcome, qt.Equals, protocol.Execution_APPLIED, qt.Commentf("%s", result))
	c.Cleanup(func() {
		drop := &protocol.Change{Object: view, Operation: &protocol.Change_CommonObject{CommonObject: &protocol.CommonObjectChange{
			Operation: &protocol.CommonObjectChange_DropView{DropView: view}}}}
		cleanup, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		dropped, err := runCommonIn(cleanup, c, client, session, protocol.RenderRequest_TEXT, "", drop)
		c.Check(err, qt.IsNil)
		c.Check(dropped.GetOutcome(), qt.Equals, protocol.Execution_APPLIED)
	})
	views, comments := commonRead(c, client, session)
	c.Assert(views, qt.DeepEquals, map[string]string{"v": "SELECT 1 AS one"})
	c.Assert(prepared(c, client, "", "SELECT 1 AS one"), qt.Equals, views["v"])
	c.Assert(comments, qt.DeepEquals, []string{"view v: in a realm"})
}
