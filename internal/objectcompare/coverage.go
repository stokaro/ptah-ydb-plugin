// Package objectcompare compares the declared and the read standalone YDB
// objects of one model kind offline, such as coordination nodes: it pairs both
// sides by path, decides from coverage whether an absence is established, and
// asks the object's family what a pair means. It reads no database.
package objectcompare

import (
	"fmt"
	"slices"
	"strings"

	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/stokaro/ptah/provider-sdk/provider"
	"google.golang.org/protobuf/proto"
)

// sessionDatabase is the database the current side was read from, which is
// the one database a comparison holds objects of. A host that names it sends
// it as the database path; otherwise an inspection names it in every owner,
// subject and region it returns, and two databases there are refused. A region
// of another database than the path names stays another database's.
func sessionDatabase(request *protocol.CompareModelsRequest, kind string) (string, error) {
	if request.GetDatabasePath() != "" {
		return request.GetDatabasePath(), nil
	}
	var found []string
	add := func(database string) {
		if database != "" && !slices.Contains(found, database) {
			found = append(found, database)
		}
	}
	for _, record := range request.GetCurrent().GetRecords() {
		add(record.GetSubject().GetDatabase().GetNormalized())
		add(record.GetValue().GetValue().GetOwner().GetDatabase())
	}
	for _, claim := range request.GetCurrent().GetCoverage().GetSubjects() {
		if claim.GetKind() == kind {
			add(claim.GetSubject().GetDatabase().GetNormalized())
		}
	}
	for _, source := range kindSources(request.GetCurrent().GetCoverage(), kind) {
		for _, region := range source.GetRegions() {
			add(region.GetScope().GetDatabase().GetNormalized())
		}
	}
	if len(found) > 1 {
		return "", fmt.Errorf("YDB comparison reads objects of more than one database")
	}
	if len(found) == 0 {
		return "", nil
	}
	return found[0], nil
}

// bind returns a copy of the request whose subjects and regions name the
// session's database the way the host names it: not at all. A subject of
// another database keeps it, and the owner checks refuse it.
func bind(request *protocol.CompareModelsRequest, kind, session string) *protocol.CompareModelsRequest {
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
		for _, source := range kindSources(state.GetCoverage(), kind) {
			for _, region := range source.GetRegions() {
				if region.GetScope() != nil {
					region.Scope.Database = part(region.Scope.Database)
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
