//go:build integration

package integration_test

import (
	"strings"
	"testing"

	qt "github.com/frankban/quicktest"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/stokaro/ptah/provider-sdk/provider"
	providerruntime "github.com/stokaro/ptah/provider-sdk/runtime"
	"google.golang.org/protobuf/proto"
)

// commonRender renders changes in form through client, each change named by
// its index.
func commonRender(c *qt.C, client *providerruntime.Client, form protocol.RenderRequest_Form, changes ...*protocol.Change) (*protocol.RenderResult, error) {
	c.Helper()
	request := &protocol.RenderRequest{Engine: "ydb", Form: form, Capabilities: map[string]bool{"transactional_ddl": false},
		TargetFeatures: client.Description().Engines[0].Features}
	for i, change := range changes {
		change = proto.CloneOf(change)
		change.Id = string(rune('a' + i))
		request.Changes = append(request.Changes, change)
	}
	return client.Render(c.Context(), request)
}

// viewChange creates the view app/v that body defines.
func viewChange(body, comment string) *protocol.Change {
	return &protocol.Change{Object: &protocol.Reference{Engine: "ydb", Kind: "view", Schema: "app", Name: "v"},
		Operation: &protocol.Change_CommonObject{CommonObject: &protocol.CommonObjectChange{Operation: &protocol.CommonObjectChange_CreateView{
			CreateView: &protocol.ViewCreation{View: &protocol.SQLView{Schema: "app", Name: "v", Body: body, Comment: comment}}}}}}
}

// commentChange sets the comment on object.
func commentChange(object *protocol.Reference, text string) *protocol.Change {
	return &protocol.Change{Object: object, Operation: &protocol.Change_CommonObject{CommonObject: &protocol.CommonObjectChange{
		Operation: &protocol.CommonObjectChange_SetComment{SetComment: &protocol.ObjectComment{Object: object, Text: text}}}}}
}

// commonIdentifiers are the identifier facts an offline common-object call
// carries.
func commonIdentifiers(client *providerruntime.Client) *protocol.ComparisonIdentifiers {
	return &protocol.ComparisonIdentifiers{Rules: proto.CloneOf(client.Description().Engines[0].Identifiers)}
}

// The common wire is a two-way addition of contract 25.3: a host that lists
// schema.common-objects gets views and comments on it, in YDB's spelling,
// rendered as the host's embedded engine writes them; a host that does not
// list it gets none of it. No database session is opened.
func TestCommonObjectsOfflineSubprocess(t *testing.T) {
	c := qt.New(t)
	listing := startProviderFor(c, provider.CommonObjectsFeature, provider.TextUnitsFeature)
	engine := listing.Description().Engines[0]
	c.Assert(engine.Features, qt.Contains, provider.CommonObjectsFeature)
	c.Assert(engine.CommonObjects.GetKinds(), qt.DeepEquals, []protocol.CommonObjectSupport_Kind{protocol.CommonObjectSupport_VIEW, protocol.CommonObjectSupport_COMMENT})

	prepared, err := listing.PrepareCommonObjects(c.Context(), &protocol.PrepareCommonObjectsRequest{Engine: "ydb", Identifiers: commonIdentifiers(listing),
		Capabilities: map[string]bool{"transactional_ddl": false}, TargetFeatures: engine.Features,
		Declared: &protocol.DeclaredCommonObjects{Views: []*protocol.SQLView{
			{Schema: "app", Name: "v", Body: "SELECT id,\n  payload -- why\nFROM `app/records` WHERE id>=1;", Comment: "a view"}}}})
	c.Assert(err, qt.IsNil)
	c.Assert(prepared.Prepared.Views[0].Body, qt.Equals, "SELECT id , payload FROM `app/records` WHERE id >= 1")
	c.Assert(prepared.Prepared.Views[0].Comment, qt.Equals, "a view")

	table := &protocol.Reference{Engine: "ydb", Kind: "table", Schema: "app", Name: "records"}
	column := &protocol.Reference{Engine: "ydb", Kind: "column", Schema: "app", Name: "payload", Path: []string{"records"}}
	index := &protocol.Reference{Engine: "ydb", Kind: "index", Schema: "app", Name: "by_ts", Path: []string{"records"}}
	view := &protocol.Reference{Engine: "ydb", Kind: "view", Schema: "app", Name: "v"}
	drop := &protocol.Change{Object: view, Operation: &protocol.Change_CommonObject{CommonObject: &protocol.CommonObjectChange{
		Operation: &protocol.CommonObjectChange_DropView{DropView: view}}}}
	want := []string{
		"CREATE VIEW `app/v` WITH (security_invoker = TRUE) AS\nSELECT id FROM `app/records` -- the last line\n;\nCOMMENT ON VIEW `app/v` IS 'a view';",
		"COMMENT ON TABLE `app/records` IS 'it\\'s';",
		"COMMENT ON COLUMN `app/records`.`payload` IS 'the text';",
		"COMMENT ON INDEX `by_ts` ON `app/records` IS NULL;",
		"COMMENT ON VIEW `app/v` IS 'line\\nbreak';",
		"DROP VIEW `app/v`;",
	}
	for _, form := range []protocol.RenderRequest_Form{protocol.RenderRequest_ANY, protocol.RenderRequest_TEXT} {
		rendered, err := commonRender(c, listing, form, viewChange("SELECT id FROM `app/records` -- the last line\n;;", "a view"),
			commentChange(table, "it's"), commentChange(column, "the text"), commentChange(index, ""), commentChange(view, "line\nbreak"), drop)
		c.Assert(err, qt.IsNil)
		var statements []string
		for _, unit := range rendered.Units {
			statements = append(statements, unit.GetSql())
		}
		c.Assert(statements, qt.DeepEquals, want, qt.Commentf("%s", form))
	}

	// What YDB cannot hold is refused before a unit is written. The client
	// redacts the provider's message; the unit tests of internal/commonobject
	// pin each one.
	for _, refused := range []struct {
		name   string
		change *protocol.Change
	}{
		{"replacement", &protocol.Change{Object: view, Operation: &protocol.Change_CommonObject{CommonObject: &protocol.CommonObjectChange{
			Operation: &protocol.CommonObjectChange_CreateView{CreateView: &protocol.ViewCreation{Replace: true,
				View: &protocol.SQLView{Schema: "app", Name: "v", Body: "SELECT 1"}}}}}}},
		{"check option", &protocol.Change{Object: view, Operation: &protocol.Change_CommonObject{CommonObject: &protocol.CommonObjectChange{
			Operation: &protocol.CommonObjectChange_CreateView{CreateView: &protocol.ViewCreation{
				View: &protocol.SQLView{Schema: "app", Name: "v", Body: "SELECT 1", WithCheck: true}}}}}}},
		{"oversized comment", commentChange(table, strings.Repeat("x", 4097))},
		{"directory comment", commentChange(&protocol.Reference{Engine: "ydb", Kind: "schema", Name: "app"}, "a directory")},
	} {
		t.Run(refused.name, func(t *testing.T) {
			c := qt.New(t)
			rendered, err := commonRender(c, listing, protocol.RenderRequest_TEXT, refused.change)
			c.Assert(err, qt.ErrorMatches, `provider render failed \(FailedPrecondition\)`)
			c.Assert(rendered, qt.IsNil)
		})
	}

	// A host of an earlier minor, or one that does not act on common objects,
	// negotiates none: the engine declares no kind, and the client refuses a
	// preparation or a common change before it is sent.
	unlisted := startProviderFor(c, provider.TextUnitsFeature)
	c.Assert(unlisted.Description().Engines[0].Features, qt.Not(qt.Contains), provider.CommonObjectsFeature)
	c.Assert(unlisted.Description().Engines[0].CommonObjects, qt.IsNil)
	refusedPreparation, err := unlisted.PrepareCommonObjects(c.Context(), &protocol.PrepareCommonObjectsRequest{Engine: "ydb", Identifiers: commonIdentifiers(unlisted),
		Capabilities: map[string]bool{"transactional_ddl": false}, Declared: &protocol.DeclaredCommonObjects{}})
	c.Assert(err, qt.ErrorMatches, `.*common objects: the wire was not negotiated.*`)
	c.Assert(refusedPreparation, qt.IsNil)
	refusedRender, err := commonRender(c, unlisted, protocol.RenderRequest_TEXT, commentChange(table, "a comment"))
	c.Assert(err, qt.ErrorMatches, `.*common objects.*`)
	c.Assert(refusedRender, qt.IsNil)
}
