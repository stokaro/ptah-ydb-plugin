package objectcompare

import (
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"google.golang.org/protobuf/proto"
)

// Knowledge resolves a captured claim for a subject in an explicitly selected
// database. Inspection regions and localized subjects use the same namespace.
// The result shares no writable state with the captured coverage.
func Knowledge(coverage *protocol.ComparisonCoverage, kind string, subject *protocol.ComparisonIdentity, database string) *protocol.ModelKnowledge {
	bound := bind(&protocol.CompareModelsRequest{Current: &protocol.ComparisonState{Coverage: coverage}}, []string{kind}, database)
	localized := proto.CloneOf(subject)
	if database != "" && localized.GetDatabase().GetNormalized() == database {
		localized.Database = &protocol.ComparisonPart{}
	}
	return proto.CloneOf(knowledge(bound.Current.Coverage, kind, localized))
}
