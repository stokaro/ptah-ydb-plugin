package streamingplan

import (
	"fmt"
	"slices"

	"github.com/stokaro/ptah-ydb-plugin/internal/objectcompare"
	"github.com/stokaro/ptah-ydb-plugin/internal/objectplan"
	"github.com/stokaro/ptah-ydb-plugin/internal/streaming"
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
// Exact-directory coverage therefore cannot prove the full dependent set. An
// unfiltered read's own coverage proves it. A filtered read's context
// inventory proves it beside the queries in the scope: it lists the queries
// outside the scope and covers the whole database (ADR 0053).
func completeQueryInventory(request *protocol.PlanFeaturesRequest) error {
	complete, reason, err := sourcesComplete(request, streamingSources(request.CurrentObjects.GetCoverage()))
	if err != nil || complete {
		return err
	}
	if inventory := request.GetContextInventory(); inventory != nil {
		complete, _, err = sourcesComplete(request, streamingSources(inventory.GetCoverage()))
		if err != nil {
			return err
		}
		if complete {
			for _, subject := range inventory.GetCoverage().GetSubjects() {
				if subject.GetKind() == schema.StreamingQueryProperties && !knownQueries(subject.GetKnowledge()) {
					return fmt.Errorf("input changes cannot exclude unknown streaming queries: %s", subject.GetKnowledge().GetReason())
				}
			}
			return nil
		}
	}
	if reason == "" {
		return fmt.Errorf("input changes require complete streaming query inventory for the selected database")
	}
	return fmt.Errorf("input changes require complete streaming query inventory for the selected database: %s", reason)
}

func knownQueries(value *protocol.ModelKnowledge) bool {
	return value.GetState() == protocol.ModelKnowledge_COMPLETE || value.GetState() == protocol.ModelKnowledge_ABSENT
}

func streamingSources(coverage *protocol.ComparisonCoverage) []*protocol.ComparisonKnowledgeSource {
	for _, kind := range coverage.GetKinds() {
		if kind.GetModel().GetKind() == schema.StreamingQueryProperties {
			return kind.Sources
		}
	}
	return nil
}

// sourcesComplete reports whether sources know every streaming query of the
// request's database, and the reason when they do not. A region of unknown
// knowledge inside it refuses.
func sourcesComplete(request *protocol.PlanFeaturesRequest, sources []*protocol.ComparisonKnowledgeSource) (bool, string, error) {
	if len(sources) == 0 {
		return false, "", nil
	}
	for _, source := range sources {
		complete := knownQueries(source.Outside)
		for _, region := range source.Regions {
			scope := region.Scope
			if !queryNamespace(request, scope) {
				continue
			}
			if !knownQueries(region.Knowledge) {
				return false, "", fmt.Errorf("input changes cannot exclude unknown streaming queries: %s", region.Knowledge.GetReason())
			}
			if scope.GetSchema() == nil && scope.GetParent().GetNormalized() == "" && len(scope.GetPathPrefix()) == 0 {
				complete = true
			}
		}
		if !complete {
			return false, source.Outside.GetReason(), nil
		}
	}
	return true, "", nil
}

// outsideReaders refuses a write of an input that a running streaming query
// outside the command's scope reads. The plan cannot stop and restart a query
// the command does not select, so the query is named instead.
func outsideReaders(request *protocol.PlanFeaturesRequest, builder *lifecycleBuilder) error {
	for _, record := range request.GetContextInventory().GetRecords() {
		if record.GetKind() != schema.StreamingQueryProperties {
			continue
		}
		spec, err := schema.ObservedStreamingQuery(record.GetValue().GetValue())
		if err != nil {
			return err
		}
		if !streaming.Running(spec) {
			continue
		}
		touched, err := inputsChanged(request, builder.paths, spec)
		if err != nil {
			return err
		}
		if touched {
			return fmt.Errorf("streaming query %q outside the command's scope reads a changed input; include its directory in the scope so the plan can stop and restart it",
				record.GetSubject().GetName().GetSource())
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
