// Package objectcompare compares the declared and the read standalone YDB
// objects of one model kind offline, such as coordination nodes: it pairs both
// sides by path, decides from coverage whether an absence is established, and
// asks the object's family what a pair means. It reads no database.
package objectcompare

import (
	"fmt"
	"slices"
	"strings"

	"github.com/stokaro/ptah-ydb-plugin/schema"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/stokaro/ptah/provider-sdk/provider"
	"google.golang.org/protobuf/proto"
)

// SessionDatabase returns the explicit selected database. Object identities and
// coverage coordinates do not establish a physical root. Every captured context
// must agree with the request and with the other values in the comparison.
func SessionDatabase(request *protocol.CompareModelsRequest) (string, error) {
	root := request.GetDatabasePath()
	for _, state := range []*protocol.ComparisonState{request.GetDesired(), request.GetCurrent()} {
		for _, record := range state.GetRecords() {
			captured := record.GetValue().GetValue().GetContext()
			if captured == nil {
				continue
			}
			if err := schema.ValidateDatabaseContext(captured); err != nil {
				return "", err
			}
			if root != "" && root != captured.Database {
				return "", fmt.Errorf("YDB comparison has conflicting database contexts")
			}
			root = captured.Database
		}
	}
	return root, nil
}

// bind returns a copy of the request whose subjects and regions name the
// session's database the way the host names it: not at all. A subject of
// another database keeps it, and the owner checks refuse it.
func bind(request *protocol.CompareModelsRequest, kinds []string, session string) *protocol.CompareModelsRequest {
	bound := proto.CloneOf(request)
	if session == "" {
		return bound
	}
	part := func(value *protocol.ComparisonPart) *protocol.ComparisonPart {
		if value != nil && value.GetNormalized() == session {
			return &protocol.ComparisonPart{}
		}
		return value
	}
	identity := func(subject *protocol.ComparisonIdentity) {
		if subject != nil {
			subject.Database = part(subject.Database)
		}
	}
	for _, state := range []*protocol.ComparisonState{bound.Desired, bound.Current} {
		for _, record := range state.GetRecords() {
			identity(record.Subject)
		}
		for _, claim := range state.GetCoverage().GetSubjects() {
			identity(claim.Subject)
		}
		for _, kind := range kinds {
			for _, source := range kindSources(state.GetCoverage(), kind) {
				for _, region := range source.GetRegions() {
					if region.GetScope() != nil {
						region.Scope.Database = part(region.Scope.Database)
					}
				}
			}
		}
	}
	for _, relation := range bound.DeclaredRelations {
		identity(relation)
	}
	for _, required := range bound.Required {
		identity(required)
	}
	return bound
}

func selected(targets []string) bool {
	return len(targets) == 0 || slices.ContainsFunc(targets, func(target string) bool {
		target = strings.ToLower(strings.TrimSpace(target))
		return target == "ydb" || target == "ydbs"
	})
}

func subjectKnowledge(coverage *protocol.ComparisonCoverage, kind string, subject *protocol.ComparisonIdentity) (*protocol.ModelKnowledge, bool) {
	for _, claim := range coverage.GetSubjects() {
		if claim.Kind == kind && selected(claim.Targets) && provider.CompareIdentities(claim.Subject, subject) == 0 {
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

func kindSources(coverage *protocol.ComparisonCoverage, kind string) []*protocol.ComparisonKnowledgeSource {
	for _, claim := range coverage.GetKinds() {
		if claim.GetModel().GetKind() == kind {
			return claim.Sources
		}
	}
	return nil
}

// knowledge is what the coverage establishes about one subject. Each
// independent source must establish absence. Matching regions intersect; a
// narrower unknown region cannot borrow a wider complete region's authority.
func knowledge(coverage *protocol.ComparisonCoverage, kind string, subject *protocol.ComparisonIdentity) *protocol.ModelKnowledge {
	if claim, found := subjectKnowledge(coverage, kind, subject); found {
		return claim
	}
	sources := kindSources(coverage, kind)
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
