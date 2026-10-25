package commonobject_test

import (
	"strings"
	"testing"

	qt "github.com/frankban/quicktest"
	"github.com/stokaro/ptah-ydb-plugin/internal/commonobject"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"google.golang.org/protobuf/proto"
)

func view(mutate func(*protocol.SQLView)) *protocol.SQLView {
	result := &protocol.SQLView{Schema: "app", Name: "v", Body: "SELECT id FROM `app/t`"}
	if mutate != nil {
		mutate(result)
	}
	return result
}

func createView(replace bool, declared *protocol.SQLView) *protocol.CommonObjectChange {
	return &protocol.CommonObjectChange{Operation: &protocol.CommonObjectChange_CreateView{CreateView: &protocol.ViewCreation{View: declared, Replace: replace}}}
}

func setComment(object *protocol.Reference, text string) *protocol.CommonObjectChange {
	return &protocol.CommonObjectChange{Operation: &protocol.CommonObjectChange_SetComment{SetComment: &protocol.ObjectComment{Object: object, Text: text}}}
}

var (
	table  = &protocol.Reference{Engine: "ydb", Kind: "table", Name: "t"}
	column = &protocol.Reference{Engine: "ydb", Kind: "column", Schema: "app", Name: "c", Path: []string{"t"}}
	index  = &protocol.Reference{Engine: "ydb", Kind: "index", Schema: "app", Name: "by_c", Path: []string{"t"}}
)

func TestPrepare(t *testing.T) {
	c := qt.New(t)
	declared := &protocol.DeclaredCommonObjects{Views: []*protocol.SQLView{
		view(func(v *protocol.SQLView) { v.Body, v.Comment = "select a>=1,\n $x -- c\n from `t`;", "kept" }),
		view(func(v *protocol.SQLView) { v.Name, v.Body = "w", "SELECT 1" }),
	}}
	before := proto.CloneOf(declared)
	prepared := commonobject.Prepare(declared)
	c.Assert(prepared.Views[0].Body, qt.Equals, "select a >= 1 , $ x from `t`")
	c.Assert(prepared.Views[0].Comment, qt.Equals, "kept")
	c.Assert(prepared.Views[1].Body, qt.Equals, "SELECT 1")
	c.Assert(proto.Equal(declared, before), qt.IsTrue, qt.Commentf("the request is not changed"))
	c.Assert(proto.Equal(commonobject.Prepare(nil), &protocol.DeclaredCommonObjects{}), qt.IsTrue)
}

func TestRender_HappyPath(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change *protocol.CommonObjectChange
		want   string
	}{
		{"view", createView(false, view(nil)), "CREATE VIEW `app/v` WITH (security_invoker = TRUE) AS\nSELECT id FROM `app/t`\n;"},
		{"view at the root, terminators dropped", createView(false, view(func(v *protocol.SQLView) { v.Schema, v.Body = "", "  SELECT 1 ; ;\n" })),
			"CREATE VIEW `v` WITH (security_invoker = TRUE) AS\nSELECT 1\n;"},
		{"view with a comment", createView(false, view(func(v *protocol.SQLView) { v.Comment = "a 'view'" })),
			"CREATE VIEW `app/v` WITH (security_invoker = TRUE) AS\nSELECT id FROM `app/t`\n;\nCOMMENT ON VIEW `app/v` IS 'a \\'view\\'';"},
		{"view removal", &protocol.CommonObjectChange{Operation: &protocol.CommonObjectChange_DropView{DropView: &protocol.Reference{Engine: "ydb", Kind: "view", Schema: "app", Name: "v"}}},
			"DROP VIEW `app/v`;"},
		{"table comment", setComment(table, "rows"), "COMMENT ON TABLE `t` IS 'rows';"},
		{"column comment", setComment(column, "a column"), "COMMENT ON COLUMN `app/t`.`c` IS 'a column';"},
		{"index comment removed", setComment(index, ""), "COMMENT ON INDEX `by_c` ON `app/t` IS NULL;"},
		{"view comment", setComment(&protocol.Reference{Engine: "ydb", Kind: "view", Schema: "app", Name: "v"}, "line\nbreak"),
			"COMMENT ON VIEW `app/v` IS 'line\\nbreak';"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := qt.New(t)
			got, err := commonobject.Render(tc.change)
			c.Assert(err, qt.IsNil)
			c.Assert(got, qt.Equals, tc.want)
		})
	}
}

func TestRender_FailurePath(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change *protocol.CommonObjectChange
		want   string
	}{
		{"replacement", createView(true, view(nil)), `unsupported feature: CREATE OR REPLACE VIEW app/v: YDB has no CREATE OR REPLACE VIEW`},
		{"materialized", createView(false, view(func(v *protocol.SQLView) { v.Materialized = true })), `unsupported feature: view app/v: YDB has no materialized view`},
		{"no query", createView(false, view(func(v *protocol.SQLView) { v.Body = "-- nothing\n;" })), `unsupported feature: view app/v: a YDB view needs a query`},
		{"check option", createView(false, view(func(v *protocol.SQLView) { v.WithCheck = true })),
			`unsupported feature: WITH CHECK OPTION on view app/v: a YDB view cannot be written through, so there is nothing for the option to check`},
		{"view attributes", createView(false, view(func(v *protocol.SQLView) { v.Attributes = []string{"security_barrier"} })),
			`unsupported feature: WITH security_barrier on view app/v: a YDB view takes the security_invoker option alone, which Ptah writes on every view`},
		{"oversized view comment", createView(false, view(func(v *protocol.SQLView) { v.Comment = strings.Repeat("x", 4097) })),
			`unsupported feature: the comment on view app/v: the comment is 4097 bytes long, and YDB keeps at most 4096 bytes in an attribute value`},
		{"materialized view removal", &protocol.CommonObjectChange{Operation: &protocol.CommonObjectChange_DropView{
			DropView: &protocol.Reference{Engine: "ydb", Kind: "materialized_view", Name: "m"}}}, `unsupported feature: DROP of materialized_view m: YDB has no materialized view`},
		{"directory comment", setComment(&protocol.Reference{Engine: "ydb", Kind: "schema", Name: "app"}, "x"),
			`unsupported feature: the comment on schema app: YDB keeps comments on tables, their columns and indexes, and views`},
		{"column name too long", setComment(&protocol.Reference{Engine: "ydb", Kind: "column", Name: strings.Repeat("c", 81), Path: []string{"t"}}, "x"),
			`unsupported feature: the comment on column ` + strings.Repeat("c", 81) + `: YDB keeps the comment as the table attribute .*, 101 bytes long, and an attribute key takes at most 100 bytes; a column name takes at most 80`},
		{"comment not UTF-8", setComment(table, "\xff"), `unsupported feature: the comment on table t: the comment is not UTF-8 text, which YDB keeps in an attribute value`},
		{"column without its table", setComment(&protocol.Reference{Engine: "ydb", Kind: "column", Name: "c"}, "x"), `a YDB column comment names its table`},
		{"sequence", &protocol.CommonObjectChange{Operation: &protocol.CommonObjectChange_CreateSequence{CreateSequence: &protocol.SQLSequence{Name: "s"}}},
			`unsupported feature: the YDB provider carries views and comments on the common wire, and this change is another kind`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := qt.New(t)
			got, err := commonobject.Render(tc.change)
			c.Assert(err, qt.ErrorMatches, tc.want)
			c.Assert(got, qt.Equals, "")
		})
	}
}

func TestTableCommentRefusal(t *testing.T) {
	declared := func(mutate func(*protocol.DeclaredTable)) *protocol.DeclaredTable {
		result := &protocol.DeclaredTable{Schema: "app", Name: "t", Comment: "rows",
			Columns: []*protocol.DeclaredColumn{{Name: "id", Comment: "the key"}, {Name: "c"}},
			Indexes: []*protocol.DeclaredIndex{{Name: "by_c", Comment: "lookups"}}}
		if mutate != nil {
			mutate(result)
		}
		return result
	}
	t.Run("held", func(t *testing.T) {
		c := qt.New(t)
		c.Assert(commonobject.TableCommentRefusal(declared(nil)), qt.IsNil)
	})
	for _, tc := range []struct {
		name   string
		mutate func(*protocol.DeclaredTable)
		want   string
	}{
		{"table comment too long", func(d *protocol.DeclaredTable) { d.Comment = strings.Repeat("x", 4097) },
			`unsupported feature: the comment on table app/t: the comment is 4097 bytes long, and YDB keeps at most 4096 bytes in an attribute value`},
		{"index name too long", func(d *protocol.DeclaredTable) { d.Indexes[0].Name = strings.Repeat("i", 82) },
			`unsupported feature: the comment on index "i+" of table app/t: YDB keeps the comment as the table attribute .*`},
		{"too much together", func(d *protocol.DeclaredTable) {
			d.Comment = strings.Repeat("a", 4000)
			d.Columns[0].Comment = strings.Repeat("b", 4000)
			d.Columns[1].Comment = strings.Repeat("c", 4000)
		}, `unsupported feature: the comments of table app/t take 12\d\d\d bytes as YDB table attributes, keys and text together, and YDB keeps at most 10240 bytes of attributes on one object`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := qt.New(t)
			c.Assert(commonobject.TableCommentRefusal(declared(tc.mutate)), qt.ErrorMatches, tc.want)
		})
	}
	t.Run("without comments", func(t *testing.T) {
		c := qt.New(t)
		original := declared(nil)
		stripped := commonobject.WithoutComments(original)
		c.Assert(stripped.Comment+stripped.Columns[0].Comment+stripped.Indexes[0].Comment, qt.Equals, "")
		c.Assert(original.Comment, qt.Equals, "rows", qt.Commentf("the declaration is not changed"))
		plain := declared(func(d *protocol.DeclaredTable) { d.Comment, d.Columns[0].Comment, d.Indexes[0].Comment = "", "", "" })
		c.Assert(commonobject.WithoutComments(plain), qt.Equals, plain)
	})
}

func TestSupport(t *testing.T) {
	c := qt.New(t)
	c.Assert(commonobject.Support().Kinds, qt.DeepEquals, []protocol.CommonObjectSupport_Kind{protocol.CommonObjectSupport_VIEW, protocol.CommonObjectSupport_COMMENT})
	capabilities := commonobject.Capabilities()
	c.Assert(capabilities["views"] && capabilities["view_comments"] && capabilities["comment_attributes"], qt.IsTrue)
	c.Assert(capabilities["create_or_replace_view"] || capabilities["materialized_views"] || capabilities["schema_comments"], qt.IsFalse)
}
