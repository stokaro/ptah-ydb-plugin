package backend

import (
	"slices"

	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"

	"github.com/stokaro/ptah-ydb-plugin/internal/objectplan"
)

// directoryConflict refuses a plan in which one planning service drops a
// native object below a scheme path and another creates a native object at
// that path (see objectplan.DirectoryConflict), or returns nil. YDB keeps the
// directory, so the creation would fail at apply after the drop had run; the
// host's embedded engine refused such a plan before it ran anything.
//
// It runs in the streaming query service's call because that service lists
// context kinds: the host asks it on every plan, and hands it the desired and
// current objects of every kind the provider models and the subject of every
// native change, which no service sees otherwise. Without this check
// `schema apply` of a coordination node at app/holder, beside a topic at
// app/holder/t the source no longer declares, plans both and YDB refuses the
// node after the topic is gone.
func directoryConflict(request *protocol.PlanFeaturesRequest) *protocol.PlannedFeatures {
	services := planningServices(Description())
	sameService := func(a, b string) bool { return services[a] != "" && services[a] == services[b] }
	if err := objectplan.DirectoryConflict(request, sameService); err != nil {
		return &protocol.PlannedFeatures{Complete: true, Diagnostics: []*protocol.PlanningDiagnostic{{
			Code: protocol.PlanningDiagnostic_INVALID_SCHEMA, Kind: objectplan.PathKind, Message: err.Error(),
		}}}
	}
	return nil
}

// planningServices maps each object kind the description's engine compares
// to the planning service that plans the changes its comparison reports.
func planningServices(description *protocol.Description) map[string]string {
	services := make(map[string]string)
	for _, engine := range description.GetEngines() {
		for _, comparison := range engine.GetComparisons() {
			for _, planning := range engine.GetPlanning() {
				if !slices.ContainsFunc(comparison.GetChangeKinds(), func(change string) bool { return slices.Contains(planning.GetChangeKinds(), change) }) {
					continue
				}
				for _, kind := range comparison.GetKinds() {
					services[kind] = planning.GetId()
				}
			}
		}
	}
	return services
}
