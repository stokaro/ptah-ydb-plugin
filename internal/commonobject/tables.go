package commonobject

import (
	"fmt"

	"github.com/stokaro/ptah-ydb-plugin/internal/ydbcomment"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"google.golang.org/protobuf/proto"
)

// TableCommentRefusal says why YDB cannot hold the comments a table
// declares on itself, its columns and its indexes, or returns nil. Each is an
// attribute of the table, so each is held to YDB's limits on one attribute
// and all of them together to its limit on one object's attributes.
func TableCommentRefusal(table *protocol.DeclaredTable) error {
	subject := "table " + qualified(table.GetSchema(), table.GetName())
	comments := ydbcomment.Comments{Own: table.GetComment(), Columns: map[string]string{}, Indexes: map[string]string{}}
	check := func(object ydbcomment.Object, name, comment, about string) error {
		if comment == "" {
			return nil
		}
		if reason := ydbcomment.Refusal(object, name, comment); reason != "" {
			return unsupported("the comment on %s: %s", about, reason)
		}
		return nil
	}
	if err := check(ydbcomment.Table, "", table.GetComment(), subject); err != nil {
		return err
	}
	for _, column := range table.GetColumns() {
		comments.Columns[column.GetName()] = column.GetComment()
		if err := check(ydbcomment.Column, column.GetName(), column.GetComment(), fmt.Sprintf("column %q of %s", column.GetName(), subject)); err != nil {
			return err
		}
	}
	for _, index := range table.GetIndexes() {
		comments.Indexes[index.GetName()] = index.GetComment()
		if err := check(ydbcomment.Index, index.GetName(), index.GetComment(), fmt.Sprintf("index %q of %s", index.GetName(), subject)); err != nil {
			return err
		}
	}
	if reason := comments.SizeRefusal(subject); reason != "" {
		return unsupported("%s", reason)
	}
	return nil
}

// WithoutComments is table without the comments it declares on itself, its
// columns and its indexes, which the common wire carries as their own
// changes, or table itself when it declares none.
func WithoutComments(table *protocol.DeclaredTable) *protocol.DeclaredTable {
	if !hasComments(table) {
		return table
	}
	result := proto.CloneOf(table)
	result.Comment = ""
	for _, column := range result.GetColumns() {
		column.Comment = ""
	}
	for _, index := range result.GetIndexes() {
		index.Comment = ""
	}
	return result
}

func hasComments(table *protocol.DeclaredTable) bool {
	if table.GetComment() != "" {
		return true
	}
	for _, column := range table.GetColumns() {
		if column.GetComment() != "" {
			return true
		}
	}
	for _, index := range table.GetIndexes() {
		if index.GetComment() != "" {
			return true
		}
	}
	return false
}
