package objectcompare

import (
	"bytes"
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"

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
	// Capability is the session fact that a present object requires, or ""
	// for a family that checks its capabilities itself.
	Capability string
	// ChangeCapability says that only a change needs Capability: an object
	// the comparison keeps or leaves undecided needs none, so a target
	// without it can hold one the comparison reads around.
	ChangeCapability bool
	// KeepOmitted keeps an object the desired side leaves out, whatever the
	// coverage of its kind says, as for a database-wide object a source
	// declares only in part. An absence the desired side claims for the
	// object itself is left undecided with OmittedRemoval as the reason.
	KeepOmitted bool
	// OmittedRemoval is the reason KeepOmitted gives.
	OmittedRemoval string
	// DatabaseWide says that an object belongs to the database rather than to
	// a directory: it occupies no scheme path, so no relation collides with it.
	DatabaseWide bool
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
	// Unread says why a target holding capabilities leaves every object of
	// the family a read found unmanaged, or "". A nil Unread leaves none.
	Unread func(capabilities map[string]bool) string
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
	family *Family
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
	result, _, err := compare(ctx, request, []Family{family})
	return result, err
}

// Decided is one object a comparison of several families decided: both
// sides known and the pair compared. Subject and Owner name it as the
// request did, and Desired and Current are under Owner, either nil. Change is
// the change the comparison reported for it, or nil for an object that holds
// what is declared.
type Decided struct {
	Kind             string
	Subject          *protocol.ComparisonIdentity
	Owner            *protocol.Reference
	Desired, Current *protocol.Envelope
	Change           *protocol.NormalizedValue
}

// Families compares the objects of several families that share one
// comparison service, each as Compare compares one family's. request.Kinds
// names the kinds of some of them, sorted and each once, as the host sends
// them; the host leaves out a kind no record and no coverage claim names. Families returns
// each object it decided, family by family and each family's in identity
// order, for the rules that relate objects of two families.
func Families(ctx context.Context, request *protocol.CompareModelsRequest, families []Family) (*protocol.ComparedModels, []Decided, error) {
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	var selected []Family
	for _, family := range families {
		if request.GetComparisonId() != family.ServiceID {
			return nil, nil, fmt.Errorf("unknown %s comparison", family.Noun)
		}
		if slices.Contains(request.Kinds, family.Kind) {
			selected = append(selected, family)
		}
	}
	if len(selected) == 0 || len(selected) != len(request.Kinds) || !slices.IsSortedFunc(request.Kinds, strings.Compare) || len(slices.Compact(slices.Clone(request.Kinds))) != len(request.Kinds) {
		return nil, nil, fmt.Errorf("unknown %s comparison", families[0].Noun)
	}
	return compare(ctx, request, selected)
}

func compare(ctx context.Context, request *protocol.CompareModelsRequest, families []Family) (*protocol.ComparedModels, []Decided, error) {
	for _, part := range request.GetIdentifiers().GetResolvedNames() {
		if part.Name != part.Key {
			return nil, nil, fmt.Errorf("YDB comparison requires exact identifiers")
		}
	}
	if request.GetIdentifiers().GetDefaultSchema() != "" || request.GetIdentifiers().GetCatalogCollation() != "" {
		return nil, nil, fmt.Errorf("YDB comparison has unsupported identifier defaults")
	}
	kinds := make([]string, 0, len(families))
	for _, family := range families {
		kinds = append(kinds, family.Kind)
	}
	session, err := SessionDatabase(request, kinds...)
	if err != nil {
		return nil, nil, err
	}
	bound := bind(request, kinds, session)
	var objects []object
	for i := range families {
		captured, err := capture(request, bound, &families[i], session)
		if err != nil {
			return nil, nil, err
		}
		if len(captured) > 0 && !families[i].ChangeCapability && families[i].Capability != "" && !request.Capabilities[families[i].Capability] {
			return nil, nil, fmt.Errorf("%s capability was not established", families[i].Noun)
		}
		objects = append(objects, captured...)
	}
	result := &protocol.ComparedModels{Complete: true, Desired: proto.CloneOf(request.Desired)}
	var decided []Decided
	for index, item := range objects {
		if err := ctx.Err(); err != nil {
			return nil, nil, err
		}
		pair, err := compareObject(bound, result, *item.family, item, strconv.Itoa(index))
		if err != nil {
			return nil, nil, err
		}
		if pair != nil {
			decided = append(decided, *pair)
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	return result, decided, nil
}

// capture pairs the records and coverage claims of both sides. bound is the
// request in the host's terms, original the request as sent; their records
// and claims are in the same order.
func capture(original, bound *protocol.CompareModelsRequest, family *Family, session string) ([]object, error) {
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
		objects = append(objects, object{family: family, subject: proto.CloneOf(subject), reported: proto.CloneOf(reported), rank: rank})
		return &objects[len(objects)-1]
	}
	originals := []*protocol.ComparisonState{original.Desired, original.Current}
	for side, state := range []*protocol.ComparisonState{bound.Desired, bound.Current} {
		for index, record := range state.Records {
			if record.Kind != family.Kind {
				continue
			}
			if !selected(record.Targets) {
				return nil, fmt.Errorf("%s record is outside the selected target", family.Noun)
			}
			if _, err := family.Owner(record.Subject); err != nil {
				return nil, err
			}
			value, err := captureRecord(state, *family, record, session)
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

func compareObject(request *protocol.CompareModelsRequest, result *protocol.ComparedModels, family Family, item object, id string) (*Decided, error) {
	name := item.subject.GetName().GetSource()
	if namespace := item.subject.GetSchema().GetSource(); namespace != "" {
		name = namespace + "/" + name
	}
	for _, exclusion := range request.Exclusions {
		if exclusion.GetKind() == family.Kind && exclusion.GetName() == name {
			return nil, nil
		}
	}
	if reason := family.Reserved(item.subject); reason != "" {
		// The provider's own object is left out of the comparison, as the
		// host's embedded engine leaves it out of every read. Only a
		// declaration at its path is worth a word.
		if item.desired != nil {
			undecided(result, family, item, reason)
		}
		return nil, nil
	}
	if family.Unread != nil && item.current != nil {
		if reason := family.Unread(request.Capabilities); reason != "" {
			undecided(result, family, item, "The current "+family.Noun+" or its absence was not established: "+reason)
			return nil, nil
		}
	}
	if claim, found := subjectKnowledge(request.Desired.Coverage, family.Kind, item.subject); found && unknown(claim) && item.desired != nil {
		undecided(result, family, item, "The desired source cannot describe the complete "+family.Noun)
		return nil, nil
	}
	if claim, found := subjectKnowledge(request.Current.Coverage, family.Kind, item.subject); (found && unknown(claim)) || (item.current == nil && unknown(knowledge(request.Current.Coverage, family.Kind, item.subject))) {
		undecided(result, family, item, "The current "+family.Noun+" or its absence was not established")
		return nil, nil
	}
	if item.desired == nil && family.KeepOmitted {
		if claim, found := subjectKnowledge(request.Desired.Coverage, family.Kind, item.subject); found && claim.GetState() == protocol.ModelKnowledge_ABSENT {
			undecided(result, family, item, family.OmittedRemoval)
			return nil, nil
		}
		if item.current == nil {
			return nil, nil
		}
		return nil, preserve(result, family, item, id)
	}
	if item.desired == nil && unknown(knowledge(request.Desired.Coverage, family.Kind, item.subject)) {
		if item.current == nil {
			return nil, nil
		}
		return nil, preserve(result, family, item, id)
	}
	if item.desired == nil && item.current == nil {
		return nil, nil
	}
	if item.desired != nil && !family.DatabaseWide {
		for _, relation := range request.DeclaredRelations {
			if relation.GetDatabase().GetNormalized() == item.subject.GetDatabase().GetNormalized() && relation.GetCatalog().GetNormalized() == "" && relation.GetSchema().GetNormalized() == item.subject.GetSchema().GetNormalized() && relation.GetName().GetNormalized() == item.subject.GetName().GetNormalized() && len(relation.GetPath()) == 0 {
				return nil, fmt.Errorf("%s collides with a declared relation", family.Noun)
			}
		}
	}
	if item.desired == nil {
		for _, required := range request.Required {
			if provider.CompareIdentities(required, item.subject) == 0 {
				undecided(result, family, item, "A declared dependency requires the "+family.Noun)
				return nil, nil
			}
		}
	}
	if err := family.Unsettled(item.current, item.desired); err != nil {
		undecided(result, family, item, err.Error())
		return nil, nil
	}
	reported := owner(family, item.reported)
	pair := &Decided{Kind: family.Kind, Subject: proto.CloneOf(item.reported), Owner: reported, Desired: rebound(item.desired, reported), Current: rebound(item.current, reported)}
	if item.desired != nil && item.current != nil {
		equal, err := family.Equal(item.current, item.desired)
		if err != nil {
			return nil, err
		}
		if equal {
			return pair, nil
		}
	}
	if family.ChangeCapability && !request.Capabilities[family.Capability] {
		return nil, fmt.Errorf("changing %ss requires %s", family.Noun, family.Capability)
	}
	change, err := family.Change("change-"+id, reported, pair.Current, pair.Desired)
	if err != nil {
		return nil, err
	}
	result.Changes = append(result.Changes, &protocol.ComparedChange{Kind: family.Kind, Subject: proto.CloneOf(item.reported), Value: change})
	pair.Change = change
	return pair, nil
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
