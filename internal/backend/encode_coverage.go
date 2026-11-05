package backend

import (
	"fmt"
	"path"
	"slices"

	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/stokaro/ptah/provider-sdk/provider"

	"github.com/stokaro/ptah-ydb-plugin/internal/tablecompare"
	"github.com/stokaro/ptah-ydb-plugin/internal/yqlschema"
)

// encodeCoverage is what a source written from request says of the knowledge
// it lacks: the ptah:not-described entries the source carries, so that reading
// the source back claims no more than the request knew. A source claims every
// model its format lists, and claims it complete; the entries are how it says
// less.
//
// A native kind the request knows completely gets no entry. A host sends a
// read that knows a kind in every namespace it listed as complete: the source
// describes what the read listed and is complete for the selection a later
// plan applies it to, as the host's embedded YDB engine writes a read that
// --schemas narrowed, so removing an object's declaration removes the object.
// A kind the request knows less, a kind no read knew anywhere or one with a
// namespace region the read did not know, is written as an entry that leaves
// the whole kind unmanaged: a header names no namespace, so leaving the kind
// unmanaged everywhere is the claim that says no more than the request knew.
// An object of a kind the source knows completely, but whose own knowledge is
// not complete, is written as an entry that names it.
//
// A facet is known where its table is: a source states the facets of the
// tables it declares, and a read reports every facet of each table it lists.
// So a facet kind's knowledge outside those tables is left as it is, and a
// table the source declares whose facet knowledge is not complete is refused.
//
// limited says whether the host negotiated source.limits. Without it, and in
// a format that writes no header, any knowledge that needs an entry is
// refused, as is a kind no header names.
func encodeCoverage(request *protocol.EncodeSourceRequest, engine *protocol.Engine, limited bool) ([]*protocol.SchemaCoverageLimit, error) {
	format := request.GetFormat()
	headers := map[protocol.SourceDocument_Format][]string{
		protocol.SourceDocument_GO_ANNOTATIONS: engine.GetGoHeaderKinds(),
		protocol.SourceDocument_HCL:            engine.GetHclSource().GetHeaderKinds(),
	}[format]
	entry := func(model, name string, knowledge *protocol.ModelKnowledge) (*protocol.SchemaCoverageLimit, error) {
		kind, named := yqlschema.LimitKind(model)
		if !limited || !named || !slices.Contains(headers, kind) {
			if name == "" {
				return nil, fmt.Errorf("YDB source encoding cannot preserve incomplete coverage of %s", model)
			}
			return nil, fmt.Errorf("YDB source encoding cannot preserve incomplete coverage of %s %s", model, name)
		}
		// Knowledge a source's own entry left is passed on as declared;
		// any other is what the request did not inspect, which a read
		// observed.
		if yqlschema.Unmanaged(knowledge.GetReason()) {
			return &protocol.SchemaCoverageLimit{Kind: kind, Name: name, Provenance: "declared"}, nil
		}
		return &protocol.SchemaCoverageLimit{Kind: kind, Name: name, Reason: "not-inspected", Provenance: "observed"}, nil
	}
	models := make(map[string]*protocol.ValueModel)
	unmanaged := make(map[string]bool)
	var limits []*protocol.SchemaCoverageLimit
	for _, kind := range request.GetCoverage().GetKinds() {
		model := kind.GetModel()
		models[model.GetKind()] = model
		if !slices.Contains(model.GetSourceFormats(), format) || slices.Contains(tablecompare.Kinds(), model.GetKind()) || completeInSource(kind) {
			continue
		}
		limit, err := entry(model.GetKind(), "", kind.GetKnowledge())
		if err != nil {
			return nil, err
		}
		limits = append(limits, limit)
		unmanaged[model.GetKind()] = true
	}
	for _, subject := range request.GetCoverage().GetSubjects() {
		model := models[subject.GetKind()]
		if known(subject.GetKnowledge()) || !slices.Contains(model.GetSourceFormats(), format) || unmanaged[subject.GetKind()] {
			continue
		}
		if slices.Contains(tablecompare.Kinds(), subject.GetKind()) {
			if table, bound := boundTable(request, subject.GetSubject()); bound {
				return nil, fmt.Errorf("YDB source encoding cannot preserve incomplete coverage of %s of table %s", subject.GetKind(), table)
			}
			continue
		}
		limit, err := entry(subject.GetKind(), yqlschema.LimitName(subject.GetSubject()), subject.GetKnowledge())
		if err != nil {
			return nil, err
		}
		if !slices.ContainsFunc(limits, func(written *protocol.SchemaCoverageLimit) bool {
			return written.GetKind() == limit.GetKind() && written.GetName() == limit.GetName()
		}) {
			limits = append(limits, limit)
		}
	}
	return limits, nil
}

// completeInSource reports whether a source can claim kind completely: the
// request knows it completely and no namespace region narrows that. The SDK
// asks for an entry wherever a region does, as a header cannot say a region.
func completeInSource(kind *protocol.ModelKindCoverage) bool {
	return kind.GetKnowledge().GetState() == protocol.ModelKnowledge_COMPLETE && len(kind.GetRegions()) == 0
}

// known reports whether knowledge leaves nothing for a source to say: a
// complete or absent subject, or one whose default the source requests.
func known(knowledge *protocol.ModelKnowledge) bool {
	switch knowledge.GetState() {
	case protocol.ModelKnowledge_COMPLETE, protocol.ModelKnowledge_ABSENT, protocol.ModelKnowledge_DEFAULTED:
		return true
	default:
		return false
	}
}

// boundTable names the table subject belongs to, a table or a column or an
// index of it, when the request binds that table. A subject is matched by its
// directory and name alone: the host names a read's table by its database in
// a binding and leaves the database out of a coverage subject.
func boundTable(request *protocol.EncodeSourceRequest, subject *protocol.Reference) (string, bool) {
	name := subject.GetName()
	if subject.GetKind() != "table" {
		if len(subject.GetPath()) != 1 {
			return "", false
		}
		name = subject.GetPath()[0]
	}
	for _, binding := range request.GetBindings() {
		if table := binding.GetObject(); table.GetSchema() == subject.GetSchema() && table.GetName() == name {
			return path.Join(table.GetSchema(), table.GetName()), true
		}
	}
	return "", false
}

// sourceLimitsNegotiated reports whether the engine of a negotiated
// description lists source.limits.
func sourceLimitsNegotiated(engine *protocol.Engine) bool {
	return slices.Contains(engine.GetFeatures(), provider.SourceLimitsFeature)
}
