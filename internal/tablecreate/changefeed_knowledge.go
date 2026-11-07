package tablecreate

import (
	"cmp"

	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/stokaro/ptah/provider-sdk/provider"

	"github.com/stokaro/ptah-ydb-plugin/schema"
)

// undescribedChangefeeds says why the read of a table does not describe the
// changefeeds it holds, or returns "". The read carries no changefeed set for
// the table, and its coverage records the table's changefeeds, or those of
// every table, as uninspected or unrepresentable. The host records them so for
// a document that cannot describe changefeeds, such as HCL. A record of the
// table itself decides; without one, a knowledge source of the kind decides,
// and an unknown region of it counts for every table, as in FacetKnown. A read
// that records nothing about them holds none, as the alteration reads it.
//
// A rebuild drops every changefeed of the table before YDB moves it, and a
// drop takes them with the table, where a replication may read one. Neither
// can be planned from such a read, and the host's embedded engine refused
// both, in these words.
func undescribedChangefeeds(table *protocol.PlanningTable) string {
	facets := table.GetCurrent().GetFeatures().GetFacets()
	for _, record := range facets.GetRecords() {
		if record.GetKind() == schema.ChangefeedProperties && record.GetValue() != nil && forThisEngine(record.GetTargets()) {
			return ""
		}
	}
	coverage := facets.GetCoverage()
	for _, record := range coverage.GetSubjects() {
		if record.GetKind() == schema.ChangefeedProperties && provider.CompareIdentities(record.GetSubject(), table.GetSubject()) == 0 && forThisEngine(record.GetTargets()) {
			return changefeedGap(record.GetKnowledge())
		}
	}
	for _, record := range coverage.GetKinds() {
		if record.GetModel().GetKind() != schema.ChangefeedProperties {
			continue
		}
		for _, source := range record.GetSources() {
			if reason := changefeedGap(source.GetOutside()); reason != "" {
				return reason
			}
			for _, region := range source.GetRegions() {
				if reason := changefeedGap(region.GetKnowledge()); reason != "" {
					return reason
				}
			}
		}
	}
	return ""
}

// changefeedGap is the refusal of a read whose knowledge of a table's
// changefeeds is uninspected or unrepresentable, with the reason the read
// gives, or "" for any other knowledge.
func changefeedGap(knowledge *protocol.ModelKnowledge) string {
	switch knowledge.GetState() {
	case protocol.ModelKnowledge_UNINSPECTED, protocol.ModelKnowledge_UNREPRESENTABLE:
		return "the changefeed namespace is not fully described: " + cmp.Or(knowledge.GetReason(), "the read gives no reason")
	default:
		return ""
	}
}
