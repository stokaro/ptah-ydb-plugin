package schemavalidation_test

import (
	"strings"
	"testing"

	qt "github.com/frankban/quicktest"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/stokaro/ptah/provider-sdk/provider"
)

// commented is app.records with a comment on itself, a column and an index,
// on a host that negotiated the common wire when carried.
func commented(carried bool, comment string) *protocol.ValidateSchemaRequest {
	declared := keyed("app", "records")
	declared.Declaration.Comment = comment
	declared.Declaration.Columns[1].Comment = "the text"
	declared.Declaration.Indexes = []*protocol.DeclaredIndex{{Name: "by_body", Fields: []string{"body"}, Comment: "lookups"}}
	request := newRequest(declared)
	if carried {
		request.TargetFeatures = []string{provider.CommonObjectsFeature}
	}
	return request
}

func TestValidateSchema_CommentsOnTheCommonWire(t *testing.T) {
	t.Run("held", func(t *testing.T) {
		c := qt.New(t)
		c.Assert(diagnostics(c, commented(true, "rows")), qt.HasLen, 0)
	})
	t.Run("more than an attribute holds", func(t *testing.T) {
		c := qt.New(t)
		found := diagnostics(c, commented(true, strings.Repeat("x", 4097)))
		c.Assert(found, qt.HasLen, 1)
		c.Assert(found[0].Code, qt.Equals, protocol.SchemaDiagnostic_UNSUPPORTED_FEATURE)
		c.Assert(found[0].Message, qt.Equals, "unsupported feature: the comment on table app/records: the comment is 4097 bytes long, and YDB keeps at most 4096 bytes in an attribute value")
	})
	t.Run("off the wire", func(t *testing.T) {
		c := qt.New(t)
		found := diagnostics(c, commented(false, "rows"))
		c.Assert(found, qt.HasLen, 1)
		c.Assert(found[0].Message, qt.Matches, `.*YDB table creation does not support .*comment`)
	})
}

func TestValidateSchema_Views(t *testing.T) {
	c := qt.New(t)
	request := newRequest()
	request.TargetFeatures = []string{provider.CommonObjectsFeature}
	request.CommonObjects = &protocol.DeclaredCommonObjects{Views: []*protocol.SQLView{
		{Schema: "app", Name: "plain", Body: "SELECT 1", Comment: "a view"},
		{Schema: "app", Name: "checked", Body: "SELECT 1", WithCheck: true},
		{Name: "empty", Body: "-- nothing"},
	}}
	found := diagnostics(c, request)
	c.Assert(found, qt.HasLen, 2)
	c.Assert(found[0].Object+": "+found[0].Message, qt.Equals,
		"app/checked: unsupported feature: WITH CHECK OPTION on view app/checked: a YDB view cannot be written through, so there is nothing for the option to check")
	c.Assert(found[1].Object+": "+found[1].Message, qt.Equals, "empty: unsupported feature: view empty: a YDB view needs a query")
	c.Assert(found[0].Kind+found[1].Kind, qt.Equals, "viewview")
}
