package tablecreate

import (
	"slices"
	"strings"

	"google.golang.org/protobuf/proto"
	protocol "ptah.run/provider-sdk/protocol/experimental"
	"ptah.run/provider-sdk/provider"

	"github.com/stokaro/ptah-ydb-plugin/schema"
)

// PlacedInDefault reports whether the desired side places a column it carries
// no column properties for in the default family. A source that describes the
// column properties of every column it declares (complete desired knowledge)
// states where each column sits: one it places in no family sits in the
// default family, as the host's embedded engine reads a Go, YAML or YQL
// source. A source that could not describe them, such as HCL, states nothing,
// and the column keeps the family it holds. The comparison, the alteration
// and the rebuild read a column without a record through this one predicate:
// a comparison that reported a move the alteration did not make would never
// settle, and a rebuild that kept a family the comparison reads as left would
// replan the move. column names the column under its table.
func PlacedInDefault(coverage *protocol.ComparisonCoverage, column *protocol.ComparisonIdentity) bool {
	return FacetKnown(coverage, schema.ColumnProperties, column, DesiredSide)
}

// ColumnSubject names the column called name of table.
func ColumnSubject(table *protocol.ComparisonIdentity, name string) *protocol.ComparisonIdentity {
	subject := proto.CloneOf(table)
	subject.Kind, subject.Parent, subject.Name = "column", proto.CloneOf(table.GetName()), &protocol.ComparisonPart{Source: name, Normalized: name}
	return subject
}

// Side is the captured side of a comparison a coverage describes.
type Side int

const (
	// ObservedSide is what the database holds, as a read captured it.
	ObservedSide Side = iota
	// DesiredSide is what the declarations ask for.
	DesiredSide
)

// FacetKnown reports whether a captured side states what its facets of the
// given kind say about the subject, so that a facet the side does not carry is
// a known absence and not a gap in what the host read or decoded. A subject
// record decides for its subject. Without one, every knowledge source of the
// kind has to be complete or absent outside its regions, and no region may be
// unknown or unrepresentable. A defaulted claim is the author's request for the
// owner's defaults: it makes a missing desired facet known and says nothing
// about a missing observed one, which side selects.
//
// The answer is conservative. A region is not matched against the subject, so a
// source with an unknown region is unknown for every subject.
func FacetKnown(coverage *protocol.ComparisonCoverage, kind string, subject *protocol.ComparisonIdentity, side Side) bool {
	for _, record := range coverage.GetSubjects() {
		if record.GetKind() != kind || provider.CompareIdentities(record.GetSubject(), subject) != 0 || !forThisEngine(record.GetTargets()) {
			continue
		}
		return knowledgeKnown(record.GetKnowledge(), side)
	}
	for _, record := range coverage.GetKinds() {
		if record.GetModel().GetKind() != kind {
			continue
		}
		if len(record.GetSources()) == 0 {
			return false
		}
		for _, source := range record.GetSources() {
			if !knowledgeKnown(source.GetOutside(), side) {
				return false
			}
			for _, region := range source.GetRegions() {
				if state := region.GetKnowledge().GetState(); state == protocol.ModelKnowledge_UNINSPECTED || state == protocol.ModelKnowledge_UNREPRESENTABLE {
					return false
				}
			}
		}
		return true
	}
	return false
}

func knowledgeKnown(knowledge *protocol.ModelKnowledge, side Side) bool {
	switch knowledge.GetState() {
	case protocol.ModelKnowledge_COMPLETE, protocol.ModelKnowledge_ABSENT:
		return true
	case protocol.ModelKnowledge_DEFAULTED:
		return side == DesiredSide
	default:
		return false
	}
}

// forThisEngine reports whether a claim scoped to the given targets applies to
// YDB. A claim with no targets applies to every engine.
func forThisEngine(targets []string) bool {
	return len(targets) == 0 || slices.ContainsFunc(targets, func(target string) bool {
		target = strings.ToLower(strings.TrimSpace(target))
		return target == "ydb" || target == "ydbs"
	})
}
