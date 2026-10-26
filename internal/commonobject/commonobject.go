// Package commonobject carries the YDB objects beyond tables that travel on
// the common provider wire: views, principals, permissions and comments. The
// host compares and orders them for every engine; this package says what YDB
// holds of each, gives a declaration in YDB's spelling, and writes the YQL
// each change is made with, as the host's embedded YDB engine writes it.
//
// YDB has views, which it creates only with the security clause
// internal/ydbview names and keeps as the query's tokens. It has no comment
// statement: Ptah keeps a comment as a user attribute of its table or view
// (internal/ydbcomment), and execution runs Ptah's COMMENT ON statement
// through the table service. Ownership and memberships travel independently
// for access analysis. Standalone sequences are unsupported.
package commonobject

import (
	"errors"
	"fmt"
	"strings"

	"github.com/stokaro/ptah-ydb-plugin/internal/ydbacl"
	"github.com/stokaro/ptah-ydb-plugin/internal/ydbcomment"
	"github.com/stokaro/ptah-ydb-plugin/internal/ydbtype"
	"github.com/stokaro/ptah-ydb-plugin/internal/ydbview"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"google.golang.org/protobuf/proto"
)

// ErrUnsupported marks a declaration or a change YDB cannot hold.
var ErrUnsupported = errors.New("unsupported feature")

func unsupported(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrUnsupported, fmt.Sprintf(format, args...))
}

// Support is the declaration of the kinds the ydb engine carries.
func Support() *protocol.CommonObjectSupport {
	return &protocol.CommonObjectSupport{Kinds: []protocol.CommonObjectSupport_Kind{protocol.CommonObjectSupport_VIEW, protocol.CommonObjectSupport_COMMENT,
		protocol.CommonObjectSupport_ROLE, protocol.CommonObjectSupport_GRANT, protocol.CommonObjectSupport_OBJECT_OWNER, protocol.CommonObjectSupport_ROLE_MEMBERSHIP}}
}

// Capabilities are the capability facts a session on a host that negotiated
// the wire reports beside its own, as the host's YDB presets state them: YDB
// keeps views and the comments of tables, columns, indexes and views, and has
// no CREATE OR REPLACE VIEW, materialized view, or comment on a schema, a
// constraint, a sequence or a role.
func Capabilities() map[string]bool {
	return map[string]bool{
		"views": true, "create_or_replace_view": false, "materialized_views": false,
		"comment_attributes": true, "view_comments": true, "index_comments": false,
		"schema_comments": false, "constraint_comments": false, "sequence_comments": false,
		"role_comments": false, "materialized_view_comments": false,
	}
}

// Prepare returns a declaration in YDB's spelling: each view's query in the
// form the server stores it, its tokens joined by single spaces without
// comments and terminators. Privileges use canonical permission names. No
// entry changes its identity.
func Prepare(declared *protocol.DeclaredCommonObjects) *protocol.DeclaredCommonObjects {
	prepared := proto.CloneOf(declared)
	if prepared == nil {
		return &protocol.DeclaredCommonObjects{}
	}
	for _, view := range prepared.GetViews() {
		view.Body = ydbview.QueryText(view.GetBody())
	}
	for _, grant := range prepared.GetGrants() {
		for i, privilege := range grant.GetPrivileges() {
			if permission, known := ydbacl.Permission(privilege); known {
				grant.Privileges[i] = permission
			}
		}
	}
	return prepared
}

// Refusal says why YDB cannot hold a declared view, or returns nil.
func Refusal(view *protocol.SQLView) error {
	subject := "view " + qualified(view.GetSchema(), view.GetName())
	switch {
	case view.GetMaterialized():
		return unsupported("%s: YDB has no materialized view", subject)
	case ydbview.QueryText(view.GetBody()) == "":
		return unsupported("%s: a YDB view needs a query", subject)
	case view.GetWithCheck():
		// Measured by the host on 25.1.4.7 and 26.2.1.14: a view cannot be
		// written through, and WITH CHECK OPTION after the query is read as a
		// table hint on its last table, which checks nothing.
		return unsupported("WITH CHECK OPTION on %s: a YDB view cannot be written through, so there is nothing for the option to check", subject)
	case len(view.GetAttributes()) > 0:
		return unsupported("WITH %s on %s: a YDB view takes the security_invoker option alone, which Ptah writes on every view",
			strings.Join(view.GetAttributes(), ", "), subject)
	}
	if reason := ydbcomment.Refusal(ydbcomment.View, "", view.GetComment()); reason != "" {
		return unsupported("the comment on %s: %s", subject, reason)
	}
	return nil
}

// Render writes the YQL of one change, terminated, as the host's embedded
// engine writes it. A view is created at its path with the security clause,
// its query on lines of its own and the semicolon on the last line, so a
// query that ends with a line comment does not swallow it; a comment the view
// declares follows as Ptah's COMMENT ON VIEW. A comment is Ptah's COMMENT ON
// statement, which execution runs through the table service. A change YDB
// cannot make is refused with [ErrUnsupported].
func Render(change *protocol.CommonObjectChange, capabilities map[string]bool) (string, error) {
	switch operation := change.GetOperation().(type) {
	case *protocol.CommonObjectChange_CreateView:
		return createView(operation.CreateView)
	case *protocol.CommonObjectChange_DropView:
		view := operation.DropView
		if view.GetKind() != "view" {
			return "", unsupported("DROP of %s %s: YDB has no materialized view", view.GetKind(), qualified(view.GetSchema(), view.GetName()))
		}
		// YDB's DROP VIEW has no CASCADE, and it needs none: YDB records no
		// dependency on a view.
		return "DROP VIEW " + quoted(view.GetSchema(), view.GetName()) + ";", nil
	case *protocol.CommonObjectChange_SetComment:
		statement, err := commentStatement(operation.SetComment)
		if err != nil {
			return "", err
		}
		return statement.Text()
	case *protocol.CommonObjectChange_CreateRole, *protocol.CommonObjectChange_AlterRole, *protocol.CommonObjectChange_DropRole,
		*protocol.CommonObjectChange_AddMember, *protocol.CommonObjectChange_RemoveMember,
		*protocol.CommonObjectChange_Grant, *protocol.CommonObjectChange_Revoke:
		return renderAccess(change, capabilities)
	default:
		return "", unsupported("the YDB provider cannot render this common object change")
	}
}

func createView(creation *protocol.ViewCreation) (string, error) {
	view := creation.GetView()
	if creation.GetReplace() {
		// YQL has neither CREATE OR REPLACE VIEW nor ALTER VIEW, so a changed
		// view is dropped and created again.
		return "", unsupported("CREATE OR REPLACE VIEW %s: YDB has no CREATE OR REPLACE VIEW", qualified(view.GetSchema(), view.GetName()))
	}
	if err := Refusal(view); err != nil {
		return "", err
	}
	body := strings.TrimSpace(view.GetBody())
	for strings.HasSuffix(body, ";") {
		body = strings.TrimSpace(strings.TrimSuffix(body, ";"))
	}
	text := "CREATE VIEW " + quoted(view.GetSchema(), view.GetName()) + " " + ydbview.SecurityClause + " AS\n" + body + "\n;"
	if view.GetComment() == "" {
		return text, nil
	}
	comment, err := ydbcomment.Statement{Object: ydbcomment.View, Path: objectPath(view.GetSchema(), view.GetName()), Comment: view.GetComment()}.Text()
	if err != nil {
		return "", err
	}
	return text + "\n" + comment, nil
}

// commentStatement is the statement that sets a comment: on a table, on a
// column or an index, which YDB keeps on its table, or on a view.
func commentStatement(comment *protocol.ObjectComment) (ydbcomment.Statement, error) {
	object := comment.GetObject()
	statement := ydbcomment.Statement{Path: objectPath(object.GetSchema(), object.GetName()), Comment: comment.GetText()}
	switch object.GetKind() {
	case "table":
		statement.Object = ydbcomment.Table
	case "view":
		statement.Object = ydbcomment.View
	case "column", "index":
		if len(object.GetPath()) != 1 {
			return ydbcomment.Statement{}, fmt.Errorf("a YDB %s comment names its table", object.GetKind())
		}
		statement.Object, statement.Path, statement.Name = ydbcomment.Column, objectPath(object.GetSchema(), object.GetPath()[0]), object.GetName()
		if object.GetKind() == "index" {
			statement.Object = ydbcomment.Index
		}
	default:
		// A YDB directory, a role and the rest hold no attribute: an index, a
		// directory and the database root answer PathNotTable.
		return ydbcomment.Statement{}, unsupported("the comment on %s %s: YDB keeps comments on tables, their columns and indexes, and views",
			object.GetKind(), qualified(object.GetSchema(), object.GetName()))
	}
	if reason := ydbcomment.Refusal(statement.Object, statement.Name, statement.Comment); reason != "" {
		return ydbcomment.Statement{}, unsupported("the comment on %s %s: %s", object.GetKind(), qualified(object.GetSchema(), object.GetName()), reason)
	}
	return statement, nil
}

// objectPath is an object's path relative to the database, as a text unit names it
// so it runs in whatever database the migration is applied to.
func objectPath(directory, name string) string {
	if strings.TrimSpace(directory) == "" {
		return name
	}
	return strings.TrimRight(directory, "/") + "/" + name
}

func quoted(directory, name string) string {
	return ydbtype.QuoteIdentifier(objectPath(directory, name))
}

// qualified names an object in a message.
func qualified(directory, name string) string { return objectPath(directory, name) }
