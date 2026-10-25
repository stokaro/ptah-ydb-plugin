package inspect

import (
	"cmp"
	"slices"
	"strings"

	native "github.com/stokaro/ptah-ydb-plugin/internal/nativeprotocol"
	"github.com/stokaro/ptah-ydb-plugin/internal/ydbcomment"
	"github.com/stokaro/ptah-ydb-plugin/internal/ydbview"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/stokaro/ptah/provider-sdk/provider"
)

// Common names the common objects a read reports on the common wire of
// contract 25.3, beside the tables and the native objects.
type Common struct {
	// Views reports each view as an observed view, its query as the server
	// stores it, rather than as a native view object.
	Views bool
	// Comments reports Ptah's comments: a row table's, its columns' and its
	// indexes' from the table's attributes, and a view's from the view's.
	Comments bool
	// Realm is the directory of the dev realm the session reads, whose path
	// prefix YDB stores at the head of a view the session created, and empty
	// on a database.
	Realm string
}

// coverage is the common-object part of a reply before anything is read: one
// complete entry for each kind the read reports, or nil for a read of none.
func (c Common) coverage() *protocol.ObservedCommonObjects {
	if !c.Views && !c.Comments {
		return nil
	}
	result := &protocol.ObservedCommonObjects{}
	complete := &protocol.ModelKnowledge{State: protocol.ModelKnowledge_COMPLETE}
	if c.Views {
		result.Coverage = append(result.Coverage, &protocol.CommonObjectCoverage{Kind: protocol.CommonObjectSupport_VIEW, Knowledge: complete})
	}
	if c.Comments {
		result.Coverage = append(result.Coverage, &protocol.CommonObjectCoverage{Kind: protocol.CommonObjectSupport_COMMENT, Knowledge: complete})
	}
	return result
}

// tableComments adds the comments a row table's attributes hold: its own,
// and those of the columns and the indexes it has. YDB keeps a column's or an
// index's attribute after the column or the index is dropped; such a comment
// belongs to nothing and is not reported.
func (c Common) tableComments(into *protocol.ObservedCommonObjects, table *protocol.Reference, described *description) {
	if !c.Comments {
		return
	}
	comments := ydbcomment.Read(described.table.GetAttributes())
	if comments.Own != "" {
		into.Comments = append(into.Comments, &protocol.ObjectComment{Object: stripped(table), Text: comments.Own})
	}
	for _, column := range described.table.GetColumns() {
		if text := comments.Columns[column.GetName()]; text != "" {
			into.Comments = append(into.Comments, &protocol.ObjectComment{Object: stripped(provider.ColumnReference(table, column.GetName())), Text: text})
		}
	}
	for _, index := range described.indexes {
		if text := comments.Indexes[index.GetName()]; text != "" {
			reference := provider.ColumnReference(table, index.GetName())
			reference.Kind = "index"
			into.Comments = append(into.Comments, &protocol.ObjectComment{Object: stripped(reference), Text: text})
		}
	}
}

// view adds a view, with the comment its attributes hold.
func (c Common) view(into *protocol.ObservedCommonObjects, identity *protocol.Reference, observed *native.ViewObservation) {
	body := observed.GetView().GetQueryText()
	if c.Realm != "" {
		body = ydbview.RealmQueryText(body, c.Realm)
	}
	into.Views = append(into.Views, &protocol.ObservedView{Identity: identity, Body: body})
	if text := ydbcomment.Read(observed.GetProperties().GetAttributes()).Own; c.Comments && text != "" {
		into.Comments = append(into.Comments, &protocol.ObjectComment{Object: stripped(identity), Text: text})
	}
}

// stripped is a reference without the display name a table identity may
// carry.
func stripped(reference *protocol.Reference) *protocol.Reference {
	result := &protocol.Reference{Engine: reference.GetEngine(), Database: reference.GetDatabase(), Schema: reference.GetSchema(),
		Kind: reference.GetKind(), Name: reference.GetName(), Path: slices.Clone(reference.GetPath())}
	return result
}

// sort orders the views by directory and name, and the comments by their
// object, so a read of the same database answers the same.
func (Common) sort(objects *protocol.ObservedCommonObjects) {
	if objects == nil {
		return
	}
	slices.SortFunc(objects.Views, func(a, b *protocol.ObservedView) int {
		return cmp.Or(strings.Compare(a.Identity.Schema, b.Identity.Schema), strings.Compare(a.Identity.Name, b.Identity.Name))
	})
	slices.SortFunc(objects.Comments, func(a, b *protocol.ObjectComment) int {
		return cmp.Or(strings.Compare(a.Object.Schema, b.Object.Schema), slices.Compare(a.Object.Path, b.Object.Path),
			strings.Compare(a.Object.Kind, b.Object.Kind), strings.Compare(a.Object.Name, b.Object.Name))
	})
}
