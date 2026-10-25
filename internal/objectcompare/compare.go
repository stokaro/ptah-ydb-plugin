package objectcompare

import (
	"bytes"
	"context"
	"fmt"
	"slices"
	"strconv"

	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/stokaro/ptah/provider-sdk/provider"
	"google.golang.org/protobuf/proto"
)

// Family is what one kind of standalone object brings to a comparison. Every
// function is required.
type Family struct {
	// ServiceID is the comparison the host asks for the family's kind.
	ServiceID string
	// Kind is the model kind of both representations of the object.
	Kind string
	// Capability is the session fact that a present object requires.
	Capability string
	// Noun names one object in a reason, such as "coordination node".
	Noun string
	// DesiredModel returns the desired model a preserved record carries.
	DesiredModel func() *protocol.ValueModel
	// Owner validates a subject of the family and returns its owner.
	Owner func(*protocol.ComparisonIdentity) (*protocol.Reference, error)
	// NormalizeDesired and NormalizeObserved validate one envelope of each side.
	NormalizeDesired, NormalizeObserved func(*protocol.Envelope) (*protocol.Envelope, error)
	// Reserved says why a subject belongs to the provider's runtime, or "".
	Reserved func(*protocol.ComparisonIdentity) string
	// Unsettled says why the desired object cannot be planned against the
	// current one, such as access the plan cannot establish, or nil. Either
	// side may be nil.
	Unsettled func(current, desired *protocol.Envelope) error
	// Equal reports whether the current object already holds the desired one.
	Equal func(current, desired *protocol.Envelope) (bool, error)
	// Change captures the change from current to desired, either nil.
	Change func(id string, owner *protocol.Reference, current, desired *protocol.Envelope) (*protocol.NormalizedValue, error)
	// Preserve returns the desired value that keeps the current object as it
	// is, for an object the desired source does not describe.
	Preserve func(owner *protocol.Reference, current *protocol.Envelope) (*protocol.Envelope, error)
}

type object struct {
	// subject is the object's identity in the host's terms, which pairs both
	// sides and matches coverage.
	subject *protocol.ComparisonIdentity
	// reported is the identity as the request spelled it, the declared one
	// where there is one: the host relates every change and diagnostic to an
	// identity it sent.
	reported *protocol.ComparisonIdentity
	// rank orders the sources of reported: a declared record, a read
	// record, a coverage claim.
	rank    int
	desired *protocol.Envelope
	current *protocol.Envelope
}

// Compare pairs the declared and the read objects of the family by path and
// decides, object by object, whether each needs a change. Coverage decides
// what an absence means: an object the desired source does not describe is
// kept, and one whose absence the read did not establish is undecided. The
// request is offline and is not changed.
//
// The host names an object of the session's database without a database,
// while an inspection names that database in every owner and region it reads.
// Compare reads both in the host's terms, so a declared object and the object
// read at its path are one object. A change or a diagnostic names the object
// as the request did, the declared identity where there is one.
func Compare(ctx context.Context, request *protocol.CompareModelsRequest, family Family) (*protocol.ComparedModels, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if request.GetComparisonId() != family.ServiceID || !slices.Equal(request.Kinds, []string{family.Kind}) {
		return nil, fmt.Errorf("unknown %s comparison", family.Noun)
	}
	for _, part := range request.GetIdentifiers().GetResolvedNames() {
		if part.Name != part.Key {
			return nil, fmt.Errorf("YDB comparison requires exact identifiers")
		}
	}
	if request.GetIdentifiers().GetDefaultSchema() != "" || request.GetIdentifiers().GetCatalogCollation() != "" {
		return nil, fmt.Errorf("YDB comparison has unsupported identifier defaults")
	}
	session, err := sessionDatabase(request, family.Kind)
	if err != nil {
		return nil, err
	}
	bound := bind(request, family.Kind, session)
	objects, err := capture(request, bound, family, session)
	if err != nil {
		return nil, err
	}
	if len(objects) > 0 && !request.Capabilities[family.Capability] {
		return nil, fmt.Errorf("%s capability was not established", family.Noun)
	}
	result := &protocol.ComparedModels{Complete: true, Desired: proto.CloneOf(request.Desired)}
	for index, item := range objects {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if err := compareObject(bound, result, family, item, strconv.Itoa(index)); err != nil {
			return nil, err
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return result, nil
}

// capture pairs the records and coverage claims of both sides. bound is the
// request in the host's terms, original the request as sent; their records
// and claims are in the same order.
func capture(original, bound *protocol.CompareModelsRequest, family Family, session string) ([]object, error) {
	var objects []object
	find := func(subject, reported *protocol.ComparisonIdentity, rank int) *object {
		for i := range objects {
			if provider.CompareIdentities(objects[i].subject, subject) == 0 {
				if rank < objects[i].rank {
					objects[i].reported, objects[i].rank = proto.CloneOf(reported), rank
				}
				return &objects[i]
			}
		}
		objects = append(objects, object{subject: proto.CloneOf(subject), reported: proto.CloneOf(reported), rank: rank})
		return &objects[len(objects)-1]
	}
	originals := []*protocol.ComparisonState{original.Desired, original.Current}
	for side, state := range []*protocol.ComparisonState{bound.Desired, bound.Current} {
		for index, record := range state.Records {
			if !selected(record.Targets) {
				return nil, fmt.Errorf("%s record is outside the selected target", family.Noun)
			}
			if _, err := family.Owner(record.Subject); err != nil {
				return nil, err
			}
			value, err := captureRecord(state, family, record, session)
			if err != nil {
				return nil, err
			}
			item := find(record.Subject, originals[side].Records[index].Subject, side)
			if state.Representation == protocol.ValueInput_DESIRED {
				item.desired = value
			} else {
				item.current = value
			}
		}
		for index, claim := range state.GetCoverage().GetSubjects() {
			if !selected(claim.Targets) || claim.Kind != family.Kind {
				continue
			}
			if claim.GetKnowledge().GetState() == protocol.ModelKnowledge_DEFAULTED {
				return nil, fmt.Errorf("a %s cannot be defaulted", family.Noun)
			}
			if _, err := family.Owner(claim.Subject); err != nil {
				return nil, err
			}
			find(claim.Subject, originals[side].Coverage.Subjects[index].Subject, 2)
		}
	}
	slices.SortFunc(objects, func(a, b object) int { return provider.CompareIdentities(a.subject, b.subject) })
	return objects, nil
}

// owner is the owner a subject of the family names, in the host's terms.
func owner(family Family, subject *protocol.ComparisonIdentity) *protocol.Reference {
	return &protocol.Reference{Engine: "ydb", Kind: family.Kind, Database: subject.GetDatabase().GetNormalized(), Schema: subject.GetSchema().GetNormalized(), Name: subject.GetName().GetNormalized()}
}

func captureRecord(state *protocol.ComparisonState, family Family, record *protocol.ComparisonRecord, session string) (*protocol.Envelope, error) {
	value := record.GetValue().GetValue()
	normalize, format := family.NormalizeObserved, family.Kind+"/observed-json-v1"
	if state.Representation == protocol.ValueInput_DESIRED {
		normalize, format = family.NormalizeDesired, family.DesiredModel().CanonicalFormat
	}
	normalized, err := normalize(value)
	if err != nil {
		return nil, err
	}
	expected := &protocol.NormalizedValue{Id: record.Value.Id, Value: normalized, NormalizedOwner: proto.CloneOf(normalized.Owner), CanonicalFormat: format, CanonicalProjection: bytes.Clone(normalized.Payload)}
	if !proto.Equal(expected, record.Value) {
		return nil, fmt.Errorf("%s normalization receipt differs from its payload", family.Noun)
	}
	captured := proto.CloneOf(value.Owner)
	if captured.Database == session {
		captured.Database = ""
	}
	bound := owner(family, record.Subject)
	if !proto.Equal(captured, bound) {
		return nil, fmt.Errorf("%s payload and comparison subject disagree", family.Noun)
	}
	normalized.Owner = bound
	claim := knowledge(state.Coverage, family.Kind, record.Subject)
	if claim.GetState() == protocol.ModelKnowledge_ABSENT || claim.GetState() == protocol.ModelKnowledge_DEFAULTED {
		return nil, fmt.Errorf("present %s has absent or defaulted coverage", family.Noun)
	}
	return normalized, nil
}

func undecided(result *protocol.ComparedModels, family Family, item object, reason string) {
	result.Undecided = append(result.Undecided, &protocol.ComparisonUndecided{Kind: family.Kind, Subject: proto.CloneOf(item.reported), Reason: reason})
}

// rebound returns value under owner, or nil for nil. Both operands of one
// change name the owner of the identity the change reports.
func rebound(value *protocol.Envelope, owner *protocol.Reference) *protocol.Envelope {
	if value == nil {
		return nil
	}
	value = proto.CloneOf(value)
	value.Owner = proto.CloneOf(owner)
	return value
}

func compareObject(request *protocol.CompareModelsRequest, result *protocol.ComparedModels, family Family, item object, id string) error {
	name := item.subject.GetName().GetSource()
	if namespace := item.subject.GetSchema().GetSource(); namespace != "" {
		name = namespace + "/" + name
	}
	for _, exclusion := range request.Exclusions {
		if exclusion.GetKind() == family.Kind && exclusion.GetName() == name {
			return nil
		}
	}
	if reason := family.Reserved(item.subject); reason != "" {
		// The provider's own object is left out of the comparison, as the
		// host's embedded engine leaves it out of every read. Only a
		// declaration at its path is worth a word.
		if item.desired != nil {
			undecided(result, family, item, reason)
		}
		return nil
	}
	if claim, found := subjectKnowledge(request.Desired.Coverage, family.Kind, item.subject); found && unknown(claim) && item.desired != nil {
		undecided(result, family, item, "The desired source cannot describe the complete "+family.Noun)
		return nil
	}
	if claim, found := subjectKnowledge(request.Current.Coverage, family.Kind, item.subject); (found && unknown(claim)) || (item.current == nil && unknown(knowledge(request.Current.Coverage, family.Kind, item.subject))) {
		undecided(result, family, item, "The current "+family.Noun+" or its absence was not established")
		return nil
	}
	if item.desired == nil && unknown(knowledge(request.Desired.Coverage, family.Kind, item.subject)) {
		if item.current == nil {
			return nil
		}
		return preserve(result, family, item, id)
	}
	if item.desired == nil && item.current == nil {
		return nil
	}
	if item.desired != nil {
		for _, relation := range request.DeclaredRelations {
			if relation.GetDatabase().GetNormalized() == item.subject.GetDatabase().GetNormalized() && relation.GetCatalog().GetNormalized() == "" && relation.GetSchema().GetNormalized() == item.subject.GetSchema().GetNormalized() && relation.GetName().GetNormalized() == item.subject.GetName().GetNormalized() && len(relation.GetPath()) == 0 {
				return fmt.Errorf("%s collides with a declared relation", family.Noun)
			}
		}
	}
	if item.desired == nil {
		for _, required := range request.Required {
			if provider.CompareIdentities(required, item.subject) == 0 {
				undecided(result, family, item, "A declared dependency requires the "+family.Noun)
				return nil
			}
		}
	}
	if err := family.Unsettled(item.current, item.desired); err != nil {
		undecided(result, family, item, err.Error())
		return nil
	}
	if item.desired != nil && item.current != nil {
		equal, err := family.Equal(item.current, item.desired)
		if err != nil {
			return err
		}
		if equal {
			return nil
		}
	}
	reported := owner(family, item.reported)
	change, err := family.Change("change-"+id, reported, rebound(item.current, reported), rebound(item.desired, reported))
	if err != nil {
		return err
	}
	result.Changes = append(result.Changes, &protocol.ComparedChange{Kind: family.Kind, Subject: proto.CloneOf(item.reported), Value: change})
	return nil
}

func preserve(result *protocol.ComparedModels, family Family, item object, id string) error {
	value, err := family.Preserve(owner(family, item.reported), rebound(item.current, owner(family, item.reported)))
	if err != nil {
		return err
	}
	// The preserved record gets a separate ID namespace from all authored input.
	used := func(candidate string) bool {
		return slices.ContainsFunc(result.Desired.Records, func(record *protocol.ComparisonRecord) bool { return record.GetValue().GetId() == candidate })
	}
	candidate := "preserved-" + id
	for used(candidate) {
		candidate += "-"
	}
	model := family.DesiredModel()
	result.Desired.Records = append(result.Desired.Records, &protocol.ComparisonRecord{Kind: family.Kind, Subject: proto.CloneOf(item.reported), Value: &protocol.NormalizedValue{Id: candidate, Value: value, NormalizedOwner: proto.CloneOf(value.Owner), CanonicalFormat: model.CanonicalFormat, CanonicalProjection: bytes.Clone(value.Payload)}})
	if result.Desired.Coverage == nil {
		result.Desired.Coverage = &protocol.ComparisonCoverage{}
	}
	if len(kindSources(result.Desired.Coverage, family.Kind)) == 0 && !slices.ContainsFunc(result.Desired.Coverage.Kinds, func(claim *protocol.ComparisonKindCoverage) bool { return claim.GetModel().GetKind() == family.Kind }) {
		result.Desired.Coverage.Kinds = append(result.Desired.Coverage.Kinds, &protocol.ComparisonKindCoverage{Model: model, Sources: []*protocol.ComparisonKnowledgeSource{{Outside: &protocol.ModelKnowledge{State: protocol.ModelKnowledge_UNINSPECTED, Reason: "The desired source did not describe " + family.Noun + "s"}}}})
	}
	for i, claim := range result.Desired.Coverage.Subjects {
		if claim.Kind == family.Kind && provider.CompareIdentities(claim.Subject, item.reported) == 0 {
			if !selected(claim.Targets) {
				return fmt.Errorf("preserved %s subject conflicts with another target's coverage", family.Noun)
			}
			result.Desired.Coverage.Subjects = slices.Delete(result.Desired.Coverage.Subjects, i, i+1)
			break
		}
	}
	result.Desired.Coverage.Subjects = append(result.Desired.Coverage.Subjects, &protocol.ComparisonSubjectCoverage{Kind: family.Kind, Subject: proto.CloneOf(item.reported), Knowledge: &protocol.ModelKnowledge{State: protocol.ModelKnowledge_COMPLETE}})
	return nil
}
