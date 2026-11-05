package backend

import (
	"cmp"
	"slices"

	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"

	"github.com/stokaro/ptah-ydb-plugin/internal/externalsource"
	"github.com/stokaro/ptah-ydb-plugin/internal/replicationsource"
	"github.com/stokaro/ptah-ydb-plugin/internal/secretsource"
	"github.com/stokaro/ptah-ydb-plugin/internal/streamingsource"
	"github.com/stokaro/ptah-ydb-plugin/internal/topicsource"
	"github.com/stokaro/ptah-ydb-plugin/internal/workloadsource"
	"github.com/stokaro/ptah-ydb-plugin/schema"
)

// goGroups ranks the Go directives of the document in the order the host's
// embedded engine wrote them in a Go export: the objects that read or move
// data after the views, topics first and streaming queries last, then the
// workload objects and the coordination nodes after the default privileges.
// A directive the map does not rank, such as one of a table, follows them.
var goGroups = map[string]int{
	topicsource.DirectiveTopic: 0, topicsource.DirectiveConsumer: 0,
	replicationsource.DirectiveReplication: 1, replicationsource.DirectiveItem: 1,
	replicationsource.DirectiveTransfer: 2,
	secretsource.Directive:              3,
	externalsource.DirectiveDataSource:  4,
	externalsource.DirectiveTable:       5,
	streamingsource.DirectiveQuery:      6,
	workloadsource.DirectivePool:        7,
	workloadsource.DirectiveClassifier:  8,
	schema.CoordinationGoDirective().GetDirective(): 9,
}

// afterPrivileges is the first group the host's embedded engine wrote after
// the default privileges.
const afterPrivileges = 7

// goRank is the group of a unit's first directive.
func goRank(unit encodedUnit) int {
	if len(unit.parts) == 0 {
		return len(goGroups)
	}
	rank, ranked := goGroups[unit.parts[0].GetFragment().GetGoDirective().GetDirective()]
	if !ranked {
		return len(goGroups)
	}
	return rank
}

// arrangeGo writes a Go export's directives of the document as the host's
// embedded engine wrote them: in its order of groups, keeping the request's
// order within a group; each with its attributes in the order its claim lists
// them; and, where the host reads placements, the workload objects and the
// coordination nodes after the default privileges. Without placements the
// host writes every one of them after the views.
func arrangeGo(units []encodedUnit, placements bool) {
	slices.SortStableFunc(units, func(a, b encodedUnit) int { return cmp.Compare(goRank(a), goRank(b)) })
	for _, unit := range units {
		for _, part := range unit.parts {
			claim := part.GetFragment().GetGoDirective()
			if claim.GetScope() != protocol.GoSourceDirective_DOCUMENT {
				continue
			}
			orderAttributes(part.GetFragment().GetValue().GetMapping(), claim.GetAttributes())
			if rank, ranked := goGroups[claim.GetDirective()]; placements && ranked && rank >= afterPrivileges {
				part.Placement = protocol.EncodedFragment_GO_AFTER_PRIVILEGES
			}
		}
	}
}

// orderAttributes sorts a directive's attributes in the order its claim
// lists them. An attribute the claim does not list cannot reach the host,
// which refuses it; it keeps its place after the listed ones.
func orderAttributes(attributes *protocol.SourceMapping, order []string) {
	position := func(entry *protocol.SourceEntry) int {
		if i := slices.Index(order, entry.GetKey()); i >= 0 {
			return i
		}
		return len(order)
	}
	slices.SortStableFunc(attributes.GetEntries(), func(a, b *protocol.SourceEntry) int { return cmp.Compare(position(a), position(b)) })
}
