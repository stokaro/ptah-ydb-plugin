package objectplan

import (
	protocol "ptah.run/provider-sdk/protocol/experimental"
	"ptah.run/provider-sdk/provider"

	"github.com/stokaro/ptah-ydb-plugin/schema"
)

// DirectoryConflict refuses a plan that drops a native object below a scheme
// path and creates a native object at that path, when two planning services
// plan the two, with the words [DirectoryKept] gives. A service orders and
// refuses its own changes and the host's common steps (see Plan), but it does
// not see another service's changes, so a pair of two services passes both
// and YDB refuses the creation at apply, after the drop has run.
//
// request is a planning request that carries the object context of
// planning.object-context: the desired and current objects of every kind the
// provider models, and the subject of every native change of the plan. A
// changed subject that only the desired objects hold is a creation, and one
// that only the current objects hold is a drop. sameService reports whether
// one planning service plans the changes of both object kinds; such a pair is
// left to that service, which refuses it itself.
func DirectoryConflict(request *protocol.PlanFeaturesRequest, sameService func(a, b string) bool) error {
	var created, dropped []*protocol.ComparisonIdentity
	for _, subject := range request.GetChangedSubjects() {
		if !schema.IsNativeObject(subject.GetKind()) {
			continue
		}
		desired, current := holds(request.GetDesiredObjects(), subject), holds(request.GetCurrentObjects(), subject)
		switch {
		case desired && !current:
			created = append(created, subject)
		case current && !desired:
			dropped = append(dropped, subject)
		}
	}
	for _, drop := range dropped {
		for _, creation := range created {
			if ancestor(creation, drop) && !sameService(creation.GetKind(), drop.GetKind()) {
				return DirectoryKept(pathKey(drop), pathKey(creation))
			}
		}
	}
	return nil
}

// holds reports whether state records subject.
func holds(state *protocol.ComparisonState, subject *protocol.ComparisonIdentity) bool {
	for _, record := range state.GetRecords() {
		if provider.CompareIdentities(record.GetSubject(), subject) == 0 {
			return true
		}
	}
	return false
}
