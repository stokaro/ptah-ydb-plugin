package yqlschema

import (
	"fmt"
	"path"
	"slices"

	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"google.golang.org/protobuf/proto"

	"github.com/stokaro/ptah-ydb-plugin/schema"
)

// unmanagedObject is the knowledge a limit that names an object leaves of it.
const unmanagedObject = "the source leaves this object unmanaged"

// LimitKind is the header kind that leaves model unmanaged in a source, and
// false for a model no header names. The kinds are the ones HeaderKinds
// lists; a YQL document, an HCL document and a Go source name them alike.
func LimitKind(model string) (string, bool) {
	for kind, limited := range limitedKinds {
		if limited.model == model {
			return kind, true
		}
	}
	return "", false
}

// LimitName is how a header names owner, an object of model: the path of an
// object a directory holds, and the name of one the database holds. A
// coordination node at the root keeps a leading slash, as the host's embedded
// engine wrote it: a node's limit without a slash reads a dot as the
// separator of a directory and a name, so `locks.v1` would read back as the
// node v1 in the directory locks.
func LimitName(owner *protocol.Reference) string {
	switch {
	case owner.GetSchema() == "" && owner.GetKind() == schema.CoordinationProperties:
		return "/" + owner.GetName()
	case owner.GetSchema() == "":
		return owner.GetName()
	}
	return path.Join(owner.GetSchema(), owner.GetName())
}

// Unmanaged reports whether reason is the knowledge a not-described limit
// leaves: of a whole kind, or of an object a limit names.
func Unmanaged(reason string) bool {
	if reason == unmanagedObject {
		return true
	}
	for _, limited := range limitedKinds {
		if limited.reason == reason {
			return true
		}
	}
	return false
}

// LimitCoverage applies a source's `ptah:not-described` limits to the coverage
// a decoding claims: a limit of a whole kind leaves its model uninspected, and
// a limit that names an object leaves that object uninspected. A limit of a
// model the coverage does not enroll changes nothing, as in a YQL document. A
// limit that names an object the coverage already accounts for, because the
// source declares it, is refused: the source cannot both declare an object
// and leave it unmanaged. A limit whose name no object of its kind can take
// is refused with the host's words, as CheckLimits refuses it.
func LimitCoverage(coverage *protocol.ModelCoverage, limits []*protocol.SchemaCoverageLimit) error {
	enrolled := make(map[string]*protocol.ModelKindCoverage, len(coverage.GetKinds()))
	for _, kind := range coverage.GetKinds() {
		enrolled[kind.GetModel().GetKind()] = kind
	}
	declared := len(coverage.GetSubjects())
	for _, limit := range limits {
		if refusal := checkLimit(limit); refusal != nil {
			return fmt.Errorf("%s", refusal.Message)
		}
		limited, known := limitedKinds[limit.GetKind()]
		kind := enrolled[limited.model]
		if !known || kind == nil {
			continue
		}
		if limit.GetName() == "" {
			kind.Knowledge = &protocol.ModelKnowledge{State: protocol.ModelKnowledge_UNINSPECTED, Reason: limited.reason}
			continue
		}
		schemaName, name := limited.path(limit.GetName())
		owner := &protocol.Reference{Engine: "ydb", Kind: limited.model, Schema: schemaName, Name: name}
		if slices.ContainsFunc(coverage.GetSubjects()[:declared], func(subject *protocol.ModelSubjectCoverage) bool {
			return subject.GetKind() == limited.model && proto.Equal(subject.GetNormalizedSubject(), owner)
		}) {
			return fmt.Errorf("the source leaves %s %q unmanaged and declares it", limit.GetKind(), limit.GetName())
		}
		if slices.ContainsFunc(coverage.GetSubjects()[declared:], func(subject *protocol.ModelSubjectCoverage) bool {
			return subject.GetKind() == limited.model && proto.Equal(subject.GetNormalizedSubject(), owner)
		}) {
			continue
		}
		coverage.Subjects = append(coverage.Subjects, &protocol.ModelSubjectCoverage{Kind: limited.model, Subject: owner, NormalizedSubject: proto.CloneOf(owner),
			Knowledge: &protocol.ModelKnowledge{State: protocol.ModelKnowledge_UNINSPECTED, Reason: unmanagedObject}})
	}
	return nil
}
