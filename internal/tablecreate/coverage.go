package tablecreate

import (
	"slices"
	"strings"

	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/stokaro/ptah/provider-sdk/provider"
)

// FacetKnown reports whether a captured side states what its facets of the
// given kind say about the subject, so that a facet the side does not carry is
// a known absence and not a gap in what the host read or decoded. A subject
// record decides for its subject. Without one, every knowledge source of the
// kind has to be complete or absent outside its regions, and no region may be
// unknown or unrepresentable. A defaulted claim is the author's request for the
// owner's defaults: it makes a missing desired facet known and says nothing
// about a missing observed one, which desired selects.
//
// The answer is conservative. A region is not matched against the subject, so a
// source with an unknown region is unknown for every subject.
func FacetKnown(coverage *protocol.ComparisonCoverage, kind string, subject *protocol.ComparisonIdentity, desired bool) bool {
	for _, record := range coverage.GetSubjects() {
		if record.GetKind() != kind || provider.CompareIdentities(record.GetSubject(), subject) != 0 || !forThisEngine(record.GetTargets()) {
			continue
		}
		return knowledgeKnown(record.GetKnowledge(), desired)
	}
	for _, record := range coverage.GetKinds() {
		if record.GetModel().GetKind() != kind {
			continue
		}
		if len(record.GetSources()) == 0 {
			return false
		}
		for _, source := range record.GetSources() {
			if !knowledgeKnown(source.GetOutside(), desired) {
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

func knowledgeKnown(knowledge *protocol.ModelKnowledge, desired bool) bool {
	switch knowledge.GetState() {
	case protocol.ModelKnowledge_COMPLETE, protocol.ModelKnowledge_ABSENT:
		return true
	case protocol.ModelKnowledge_DEFAULTED:
		return desired
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
