// Package coordinationcompare compares captured YDB coordination states offline.
package coordinationcompare

import (
	"slices"
	"strings"

	"github.com/stokaro/ptah-ydb-plugin/schema"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/stokaro/ptah/provider-sdk/provider"
)

func selected(targets []string) bool {
	return len(targets) == 0 || slices.ContainsFunc(targets, func(target string) bool {
		target = strings.ToLower(strings.TrimSpace(target))
		return target == "ydb" || target == "ydbs"
	})
}

func subjectKnowledge(coverage *protocol.ComparisonCoverage, subject *protocol.ComparisonIdentity) (*protocol.ModelKnowledge, bool) {
	for _, claim := range coverage.GetSubjects() {
		if claim.Kind == schema.CoordinationProperties && selected(claim.Targets) && provider.CompareIdentities(claim.Subject, subject) == 0 {
			return claim.Knowledge, true
		}
	}
	return nil, false
}

func unknown(knowledge *protocol.ModelKnowledge) bool {
	return knowledge.GetState() != protocol.ModelKnowledge_COMPLETE && knowledge.GetState() != protocol.ModelKnowledge_ABSENT
}

func namespaceMatches(scope *protocol.ComparisonNamespace, subject *protocol.ComparisonIdentity) bool {
	if !selected(scope.Targets) || (len(scope.SubjectKinds) > 0 && !slices.Contains(scope.SubjectKinds, subject.Kind)) {
		return false
	}
	for _, pair := range [][2]*protocol.ComparisonPart{{scope.Database, subject.Database}, {scope.Catalog, subject.Catalog}, {scope.Schema, subject.Schema}, {scope.Parent, subject.Parent}} {
		if pair[0] != nil && pair[0].GetNormalized() != pair[1].GetNormalized() {
			return false
		}
	}
	if len(scope.PathPrefix) > len(subject.Path) || (scope.PathExact && len(scope.PathPrefix) != len(subject.Path)) {
		return false
	}
	for i, part := range scope.PathPrefix {
		if part.Normalized != subject.Path[i].GetNormalized() {
			return false
		}
	}
	return true
}

func kindSources(coverage *protocol.ComparisonCoverage) []*protocol.ComparisonKnowledgeSource {
	for _, claim := range coverage.GetKinds() {
		if claim.GetModel().GetKind() == schema.CoordinationProperties {
			return claim.Sources
		}
	}
	return nil
}

// Each independent source must establish absence. Matching regions intersect;
// a narrower unknown region cannot borrow a wider complete region's authority.
func knowledge(coverage *protocol.ComparisonCoverage, subject *protocol.ComparisonIdentity) *protocol.ModelKnowledge {
	if claim, found := subjectKnowledge(coverage, subject); found {
		return claim
	}
	sources := kindSources(coverage)
	if len(sources) == 0 {
		return &protocol.ModelKnowledge{State: protocol.ModelKnowledge_UNINSPECTED, Reason: "No namespace was captured"}
	}
	for _, source := range sources {
		matched := false
		for _, region := range source.Regions {
			if namespaceMatches(region.Scope, subject) {
				matched = true
				if unknown(region.Knowledge) {
					return region.Knowledge
				}
			}
		}
		if !matched && unknown(source.Outside) {
			return source.Outside
		}
	}
	return &protocol.ModelKnowledge{State: protocol.ModelKnowledge_COMPLETE}
}

func namespaceComplete(coverage *protocol.ComparisonCoverage) bool {
	sources := kindSources(coverage)
	if len(sources) == 0 {
		return false
	}
	for _, source := range sources {
		if unknown(source.Outside) {
			return false
		}
		for _, region := range source.Regions {
			if selected(region.GetScope().GetTargets()) && unknown(region.Knowledge) {
				return false
			}
		}
	}
	return true
}
