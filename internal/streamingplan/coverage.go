package streamingplan

import (
	"fmt"
	"slices"

	"github.com/stokaro/ptah-ydb-plugin/internal/objectcompare"
	"github.com/stokaro/ptah-ydb-plugin/internal/objectplan"
	"github.com/stokaro/ptah-ydb-plugin/schema"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
)

// dependencyWrites reports whether the plan writes an object a streaming
// query may read. A creation writes none: no query reads an object the
// database does not hold yet, so creating one needs no inventory of the
// queries. A replacement drops the object first, and an alteration changes
// one the database holds; both count.
func dependencyWrites(request, paths *protocol.PlanFeaturesRequest) bool {
	for _, subject := range request.ChangedSubjects {
		if localContextSubject(request, subject) && (queryInputKind(subject.Kind) || subject.Kind == schema.ResourcePoolProperties) && !created(request, subject) {
			return true
		}
	}
	for _, common := range paths.CommonSteps {
		for _, effect := range common.GetMetadata().GetEffects() {
			if effect.Action != protocol.PlanningEffect_READ && effect.Action != protocol.PlanningEffect_CREATE && localContextSubject(request, effect.Subject) &&
				(queryInputKind(effect.Subject.Kind) || effect.Subject.Kind == objectplan.PathKind || effect.Subject.Kind == schema.ResourcePoolProperties) {
				return true
			}
		}
	}
	return false
}

// created reports a changed object the read established absent: it has no
// observed record, and the read covered its name completely.
func created(request *protocol.PlanFeaturesRequest, subject *protocol.ComparisonIdentity) bool {
	if contextRecord(request.CurrentObjects, subject) != nil {
		return false
	}
	known := objectcompare.Knowledge(request.CurrentObjects.GetCoverage(), subject.Kind, subject, request.DatabasePath)
	return known.GetState() == protocol.ModelKnowledge_COMPLETE || known.GetState() == protocol.ModelKnowledge_ABSENT
}

// An unknown query in another directory can still read a changed input.
// Exact-directory coverage therefore cannot prove the full dependent set.
func completeQueryInventory(request *protocol.PlanFeaturesRequest) error {
	var sources []*protocol.ComparisonKnowledgeSource
	for _, kind := range request.CurrentObjects.GetCoverage().GetKinds() {
		if kind.GetModel().GetKind() == schema.StreamingQueryProperties {
			sources = kind.Sources
		}
	}
	if len(sources) == 0 {
		return fmt.Errorf("input changes require complete streaming query inventory for the selected database")
	}
	known := func(value *protocol.ModelKnowledge) bool {
		return value.GetState() == protocol.ModelKnowledge_COMPLETE || value.GetState() == protocol.ModelKnowledge_ABSENT
	}
	for _, source := range sources {
		complete := known(source.Outside)
		for _, region := range source.Regions {
			scope := region.Scope
			if !queryNamespace(request, scope) {
				continue
			}
			if !known(region.Knowledge) {
				return fmt.Errorf("input changes cannot exclude unknown streaming queries: %s", region.Knowledge.GetReason())
			}
			if scope.GetSchema() == nil && scope.GetParent().GetNormalized() == "" && len(scope.GetPathPrefix()) == 0 {
				complete = true
			}
		}
		if !complete {
			return fmt.Errorf("input changes require complete streaming query inventory for the selected database: %s", source.Outside.GetReason())
		}
	}
	return nil
}

func queryNamespace(request *protocol.PlanFeaturesRequest, scope *protocol.ComparisonNamespace) bool {
	if len(scope.GetTargets()) > 0 && !slices.Contains(scope.Targets, "ydb") && !slices.Contains(scope.Targets, "ydbs") {
		return false
	}
	if len(scope.GetSubjectKinds()) > 0 && !slices.Contains(scope.SubjectKinds, schema.StreamingQueryProperties) {
		return false
	}
	database := scope.GetDatabase().GetNormalized()
	return (database == "" || database == request.DatabasePath) && scope.GetCatalog().GetNormalized() == ""
}
