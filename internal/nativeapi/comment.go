package nativeapi

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	native "github.com/stokaro/ptah-ydb-plugin/internal/nativeprotocol"
	"github.com/stokaro/ptah-ydb-plugin/internal/ydbcomment"
	"github.com/ydb-platform/ydb-go-genproto/Ydb_Scheme_V1"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb_Scheme"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb_Table"
	ydb "github.com/ydb-platform/ydb-go-sdk/v3"
)

// AttributeSession reads a table or a view and changes its user attributes.
type AttributeSession interface {
	Describe(context.Context, string) (*native.DescribeTableResult, error)
	Alter(context.Context, *Ydb_Table.AlterTableRequest) error
	Close() error
}

// Comments runs Ptah's comment statements through the table service.
type Comments struct {
	Scheme Ydb_Scheme_V1.SchemeServiceClient
	// Open starts the table session the statement reads and writes with.
	Open func(context.Context) (AttributeSession, error)
}

// NewComments binds the native clients of the session's driver.
func NewComments(driver *ydb.Driver) *Comments {
	return &Comments{Scheme: Ydb_Scheme_V1.NewSchemeServiceClient(ydb.GRPCConn(driver)),
		Open: func(ctx context.Context) (AttributeSession, error) { return OpenTable(ctx, driver) }}
}

// RunStatement runs one of Ptah's comment statements, which YQL does not
// have, as the host's YDB connection runs it: it sets the one user attribute
// of a row table or a view that [ydbcomment.Key] names, and removes it for an
// empty comment. database is the absolute path of the session's database and
// root the directory its statements are confined to: database itself, or a
// dev realm's directory under it, whose path prefix a relative path is read
// with.
//
// Before any write it refuses an object outside root, one under a directory
// whose name starts with a dot, an object of another kind than the statement
// names, and a comment on a column or an index the table does not have, which
// YDB would keep under a key nothing reads. A column table is refused by name:
// YDB accepts an attribute on one and does not keep it. Removing the comment
// of a column or an index that is gone is accepted, since YDB keeps the
// attribute after the drop. Afterward the attribute is read back.
// Dispatched is true once the write is attempted.
func (c *Comments) RunStatement(ctx context.Context, database, root string, query ydbcomment.Query, beforeWrite func(context.Context) error) (dispatched bool, err error) {
	database = "/" + strings.Trim(database, "/")
	root = "/" + strings.Trim(root, "/")
	prefix := ""
	if root != database {
		prefix = root
	}
	absolute, err := query.Absolute(database, prefix)
	if err != nil {
		return false, &StatementRefusal{Err: err}
	}
	relative, inside := strings.CutPrefix(absolute, strings.TrimSuffix(root, "/")+"/")
	if !inside {
		return false, &StatementRefusal{Err: fmt.Errorf("%w: %s is not in %s", ydbcomment.ErrStatement, absolute, root)}
	}
	if slices.ContainsFunc(strings.Split(relative, "/"), func(segment string) bool { return strings.HasPrefix(segment, ".") }) {
		return false, &StatementRefusal{Err: fmt.Errorf("%w: %s is under a directory whose name starts with a dot, which belongs to the "+
			"server or to the dev realms", ydbcomment.ErrStatement, absolute)}
	}
	subject := commentSubject(query.Statement, absolute)
	entry, err := c.describePath(ctx, absolute)
	if err != nil {
		return false, fmt.Errorf("set %s: %w", subject, err)
	}
	if err := commentTarget(query.Statement, absolute, entry.GetType()); err != nil {
		return false, &StatementRefusal{Err: err}
	}
	session, err := c.Open(ctx)
	if err != nil {
		return false, err
	}
	defer func() { err = errors.Join(err, session.Close()) }()
	if query.Comment != "" && (query.Object == ydbcomment.Column || query.Object == ydbcomment.Index) {
		described, err := session.Describe(ctx, absolute)
		if err != nil {
			return false, fmt.Errorf("set %s: %w", subject, err)
		}
		if !holds(described, query.Statement) {
			return false, &StatementRefusal{Err: fmt.Errorf("%w: table %s has no %s %q", ydbcomment.ErrStatement, absolute,
				strings.ToLower(query.Object.Keyword()), query.Name)}
		}
	}
	if err := beforeWrite(ctx); err != nil {
		return false, err
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if err := session.Alter(ctx, &Ydb_Table.AlterTableRequest{Path: absolute, AlterAttributes: map[string]string{query.Key(): query.Comment}}); err != nil {
		return true, fmt.Errorf("set %s: %w", subject, err)
	}
	described, err := session.Describe(ctx, absolute)
	if err != nil {
		return true, fmt.Errorf("confirm %s: %w", subject, err)
	}
	if held, found := described.GetAttributes()[query.Key()]; held != query.Comment || found != (query.Comment != "") {
		return true, fmt.Errorf("confirm %s: YDB does not hold the comment the statement set", subject)
	}
	return true, nil
}

// describePath reads the entry at absolute.
func (c *Comments) describePath(ctx context.Context, absolute string) (*Ydb_Scheme.Entry, error) {
	response, err := c.Scheme.DescribePath(ctx, &Ydb_Scheme.DescribePathRequest{Path: absolute})
	if err != nil {
		return nil, fmt.Errorf("describe YDB path: transport failed")
	}
	var described Ydb_Scheme.DescribePathResult
	if err := Operation(response.GetOperation(), &described); err != nil {
		return nil, err
	}
	return described.GetSelf(), nil
}

// commentSubject names the comment a statement sets, for a message.
func commentSubject(statement ydbcomment.Statement, absolute string) string {
	switch statement.Object {
	case ydbcomment.Column:
		return fmt.Sprintf("the comment on column %q of table %s", statement.Name, absolute)
	case ydbcomment.Index:
		return fmt.Sprintf("the comment on index %q of table %s", statement.Name, absolute)
	case ydbcomment.View:
		return "the comment on view " + absolute
	default:
		return "the comment on table " + absolute
	}
}

// commentTarget refuses an entry of another kind than the statement names: a
// view for COMMENT ON VIEW, a row table for every other statement.
func commentTarget(statement ydbcomment.Statement, absolute string, entryType Ydb_Scheme.Entry_Type) error {
	want := Ydb_Scheme.Entry_TABLE
	if statement.Object == ydbcomment.View {
		want = Ydb_Scheme.Entry_VIEW
	}
	switch entryType {
	case want:
		return nil
	case Ydb_Scheme.Entry_COLUMN_TABLE:
		return fmt.Errorf("%w: %s is a column table, which takes an attribute and does not keep it, so YDB "+
			"has nowhere to keep its comments", ydbcomment.ErrStatement, absolute)
	default:
		return fmt.Errorf("%w: COMMENT ON %s names %s, which is a %s", ydbcomment.ErrStatement,
			statement.Object.Keyword(), absolute, entryName(entryType))
	}
}

// entryName names a kind of scheme entry for a message.
func entryName(entryType Ydb_Scheme.Entry_Type) string {
	switch entryType {
	case Ydb_Scheme.Entry_TABLE:
		return "row table"
	case Ydb_Scheme.Entry_VIEW:
		return "view"
	case Ydb_Scheme.Entry_DIRECTORY:
		return "directory"
	default:
		return strings.ToLower(strings.ReplaceAll(entryType.String(), "_", " "))
	}
}

// holds reports whether a described table has the column or the index a
// statement names.
func holds(described *native.DescribeTableResult, statement ydbcomment.Statement) bool {
	if statement.Object == ydbcomment.Column {
		return slices.ContainsFunc(described.GetColumns(), func(column *native.ColumnMeta) bool { return column.GetName() == statement.Name })
	}
	return slices.ContainsFunc(described.GetIndexes(), func(index *native.TableIndexDescription) bool { return index.GetName() == statement.Name })
}
