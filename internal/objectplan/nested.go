package objectplan

import (
	"errors"
	"slices"
	"strings"

	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"

	"github.com/stokaro/ptah-ydb-plugin/schema"
)

// NestedDatabaseCreations refuses a plan that creates an object inside a
// database nested in the session's database, such as a table at db2/items
// when /local/db2 sits below /local, with the words
// [schema.NestedDatabaseCreation] gives. A read leaves a nested database out
// and claims its directories unknown for its reason (see
// [schema.NestedDatabaseReason]), and request's current objects carry those
// claims. Without this refusal the host plans the table as a CREATE TABLE,
// because the read holds no table there: YDB runs it through the session of
// the outer database and writes the table into the nested database, where no
// later read finds it, so every later plan creates it again and fails, and a
// drop through the outer session answers `Cannot find table`.
//
// request is a planning request that carries the object context of
// planning.object-context and the outline of every table. The creations are
// the host's common steps that create a table or a view, the tables the plan
// creates, and the changed native subjects that only the desired objects hold.
func NestedDatabaseCreations(request *protocol.PlanFeaturesRequest) error {
	roots := nestedRoots(request.GetCurrentObjects().GetCoverage())
	if len(roots) == 0 {
		return nil
	}
	refuse := func(subject *protocol.ComparisonIdentity) error {
		for _, root := range roots {
			if at := pathKey(subject); at == root || strings.HasPrefix(at, root+"/") {
				return errors.New(schema.NestedDatabaseCreation(creationNoun(subject.GetKind()), at, root, request.GetDatabasePath()))
			}
		}
		return nil
	}
	for _, step := range request.GetCommonSteps() {
		for _, effect := range step.GetMetadata().GetEffects() {
			if effect.GetAction() != protocol.PlanningEffect_CREATE || !isSchemeSubject(effect.GetSubject()) {
				continue
			}
			if err := refuse(effect.GetSubject()); err != nil {
				return err
			}
		}
	}
	for _, table := range request.GetTables() {
		if table.GetAction() != protocol.PlanningTable_CREATE {
			continue
		}
		if err := refuse(table.GetSubject()); err != nil {
			return err
		}
	}
	for _, subject := range request.GetChangedSubjects() {
		if !schema.IsNativeObject(subject.GetKind()) || !holds(request.GetDesiredObjects(), subject) || holds(request.GetCurrentObjects(), subject) {
			continue
		}
		if err := refuse(subject); err != nil {
			return err
		}
	}
	return nil
}

// nestedRoots returns the roots of the nested databases coverage claims, in
// order and each once.
func nestedRoots(coverage *protocol.ComparisonCoverage) []string {
	var roots []string
	for _, kind := range coverage.GetKinds() {
		for _, source := range kind.GetSources() {
			for _, region := range source.GetRegions() {
				if root, nested := schema.NestedDatabase(region.GetKnowledge()); nested {
					roots = append(roots, root)
				}
			}
		}
	}
	slices.Sort(roots)
	return slices.Compact(roots)
}

// creationNoun is what a refusal calls an object of kind it creates.
func creationNoun(kind string) string {
	switch kind {
	case "table", "view":
		return kind
	default:
		return "object"
	}
}
