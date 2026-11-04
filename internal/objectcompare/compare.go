package objectcompare

import (
	"bytes"
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/stokaro/ptah-ydb-plugin/internal/authored"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/stokaro/ptah/provider-sdk/provider"
	"google.golang.org/protobuf/proto"
)

// Family is what one kind of standalone object brings to a comparison. Every
// function is required unless its comment permits nil.
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
	// DottedLimits refuses a comparison that would drop an object a dotted
	// limit of the desired source reads like; see refuseDottedLimits. It is
	// for a family whose limits name an object by its path, a dot being part
	// of a name.
	DottedLimits bool
	// DesiredModel returns the desired model a preserved record carries.
	DesiredModel func() *protocol.ValueModel
	// Owner validates a subject of the family and returns its owner.
	Owner func(*protocol.ComparisonIdentity) (*protocol.Reference, error)
	// NormalizeDesired and NormalizeObserved validate one envelope of each side.
	NormalizeDesired, NormalizeObserved func(*protocol.Envelope) (*protocol.Envelope, error)
	// RequiredObjects derives the prerequisites captured in a normalization
	// receipt. A nil function means the family has no prerequisites.
	RequiredObjects func(*protocol.Envelope) ([]*protocol.ComparisonIdentity, bool, error)
	// Reserved says why a subject belongs to the provider's runtime, or "".
	Reserved func(*protocol.ComparisonIdentity) string
	// Unread says why a target holding capabilities leaves every object of
	// the family a read found unmanaged, or "". A nil Unread leaves none.
	Unread func(capabilities map[string]bool) string
	// Unsettled says why the desired object cannot be planned against the
	// current one, such as access the plan cannot establish, or nil. Either
	// side may be nil.
	Unsettled func(current, desired *protocol.Envelope) error
	// Adopt returns the desired value with what the current object holds and
	// the declaration keeps without naming it, such as the consumer YDB made
	// for a transfer, or desired itself. The comparison and the change read
	// the result. A nil Adopt adopts nothing.
	Adopt func(current, desired *protocol.Envelope) (*protocol.Envelope, error)
	// Equal reports whether the current object already holds the desired one.
	Equal func(current, desired *protocol.Envelope) (bool, error)
	// Change captures the change from current to desired, either nil.
	Change func(id string, owner *protocol.Reference, current, desired *protocol.Envelope) (*protocol.NormalizedValue, error)
	// Preserve returns the desired value that keeps the current object as it
	// is, for an object the desired source does not describe.
	Preserve func(owner *protocol.Reference, current *protocol.Envelope) (*protocol.Envelope, error)
	// Unkeepable says why the current object has no declaration that keeps
	// it, which leaves it undecided rather than preserved, or "". A nil
	// Unkeepable keeps every object.
	Unkeepable func(current *protocol.Envelope) (string, error)
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

// present reports an object the desired or the current side holds, as opposed
// to a subject only a coverage claim names.
func present(item object) bool {
	return item.desired != nil || item.current != nil
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
	session, err := SessionDatabase(request)
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
		// A coverage claim alone is no object: a reservation states that a
		// name is taken on every native kind, including one the target cannot
		// hold, and the comparison leaves such a subject undecided.
		if slices.ContainsFunc(captured, present) && !families[i].ChangeCapability && families[i].Capability != "" && !request.Capabilities[families[i].Capability] {
			return nil, nil, authored.New(families[i].Noun + " capability was not established")
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
	for _, family := range families {
		if err := refuseDottedLimits(bound, family, objects, decided); err != nil {
			return nil, nil, err
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	return result, decided, nil
}

// refuseDottedLimits refuses a comparison that drops an object a dotted limit
// of the desired source reads like, as Ptah's embedded YDB engine refuses it.
// A limit is a path relative to the database root, so a limit written
// app.events names app.events at the root, while app/events differs from it
// only in the separator. Where the limit names nothing the database holds and
// the plan drops the object its dotted directory form names, the source most
// likely means to keep that object, and dropping it loses data nothing can
// restore, so the comparison is refused with the spelling that keeps it. A
// limit that names an object the database holds is read as written.
func refuseDottedLimits(request *protocol.CompareModelsRequest, family Family, objects []object, decided []Decided) error {
	if !family.DottedLimits {
		return nil
	}
	display := func(subject *protocol.ComparisonIdentity) string {
		if directory := subject.GetSchema().GetSource(); directory != "" {
			return strings.TrimRight(directory, "/") + "/" + subject.GetName().GetSource()
		}
		return subject.GetName().GetSource()
	}
	for _, claim := range request.GetDesired().GetCoverage().GetSubjects() {
		if claim.Kind != family.Kind || !selected(claim.Targets) || !unknown(claim.Knowledge) {
			continue
		}
		if slices.ContainsFunc(objects, func(item object) bool {
			return item.family.Kind == family.Kind && item.current != nil && provider.CompareIdentities(item.subject, claim.Subject) == 0
		}) {
			continue
		}
		written := display(claim.Subject)
		for _, pair := range decided {
			// Only a written path with a dot can equal this join, so a
			// limit without one is never refused.
			directory := pair.Subject.GetSchema().GetSource()
			if pair.Kind != family.Kind || pair.Change == nil || pair.Desired != nil || directory == "" ||
				directory+"."+pair.Subject.GetName().GetSource() != written {
				continue
			}
			dropped := display(pair.Subject)
			return fmt.Errorf("the %s limit %q names %s at the database root, which the database does not hold, "+
				"while this plan would drop %s, which the same name written with a slash names. Write the limit as %q to keep that %s",
				family.Noun, written, written, dropped, dropped, family.Noun)
		}
	}
	return nil
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
	if family.RequiredObjects != nil {
		expected.RequiredObjects, expected.RequiresDatabaseContext, err = family.RequiredObjects(normalized)
		if err != nil {
			return nil, err
		}
		if expected.RequiresDatabaseContext {
			return nil, fmt.Errorf("%s comparison requires resolved database context", family.Noun)
		}
	}
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
	if claim, found := subjectKnowledge(request.Current.Coverage, family.Kind, item.subject); found && reservedName(claim) {
		// The read reserves the name for the provider's own storage on every
		// native kind, so it holds no object of this family. It is left out as
		// Reserved leaves the family's own objects; only a declaration at the
		// name is worth a word.
		if item.desired != nil {
			undecided(result, family, item, claim.GetReason())
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
		return nil, keep(result, family, item, id)
	}
	if item.desired == nil && unknown(knowledge(request.Desired.Coverage, family.Kind, item.subject)) {
		if item.current == nil {
			return nil, nil
		}
		return nil, keep(result, family, item, id)
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
	desired := item.desired
	if family.Adopt != nil && item.desired != nil && item.current != nil {
		var err error
		if desired, err = family.Adopt(item.current, item.desired); err != nil {
			return nil, err
		}
	}
	pair := &Decided{Kind: family.Kind, Subject: proto.CloneOf(item.reported), Owner: reported, Desired: rebound(desired, reported), Current: rebound(item.current, reported)}
	if item.desired != nil && item.current != nil {
		equal, err := family.Equal(item.current, desired)
		if err != nil {
			return nil, err
		}
		if equal {
			return pair, nil
		}
	}
	if family.ChangeCapability && !request.Capabilities[family.Capability] {
		return nil, authored.New(fmt.Sprintf("changing %ss requires %s", family.Noun, family.Capability))
	}
	change, err := family.Change("change-"+id, reported, pair.Current, pair.Desired)
	if err != nil {
		return nil, err
	}
	result.Changes = append(result.Changes, &protocol.ComparedChange{Kind: family.Kind, Subject: proto.CloneOf(item.reported), Value: change})
	pair.Change = change
	return pair, nil
}

// keep preserves the current object, or leaves undecided one the family has
// no declaration for.
func keep(result *protocol.ComparedModels, family Family, item object, id string) error {
	if family.Unkeepable != nil {
		reason, err := family.Unkeepable(item.current)
		if err != nil {
			return err
		}
		if reason != "" {
			undecided(result, family, item, reason)
			return nil
		}
	}
	return preserve(result, family, item, id)
}

func preserve(result *protocol.ComparedModels, family Family, item object, id string) error {
	value, err := family.Preserve(owner(family, item.reported), rebound(item.current, owner(family, item.reported)))
	if err != nil {
		return err
	}
	value.Context = proto.CloneOf(item.current.GetContext())
	// The preserved record gets a separate ID namespace from all authored input.
	used := func(candidate string) bool {
		return slices.ContainsFunc(result.Desired.Records, func(record *protocol.ComparisonRecord) bool { return record.GetValue().GetId() == candidate })
	}
	candidate := "preserved-" + id
	for used(candidate) {
		candidate += "-"
	}
	model := family.DesiredModel()
	normalized := &protocol.NormalizedValue{Id: candidate, Value: value, NormalizedOwner: proto.CloneOf(value.Owner), CanonicalFormat: model.CanonicalFormat, CanonicalProjection: bytes.Clone(value.Payload)}
	if family.RequiredObjects != nil {
		normalized.RequiredObjects, normalized.RequiresDatabaseContext, err = family.RequiredObjects(value)
		if err != nil {
			return err
		}
		if normalized.RequiresDatabaseContext {
			return fmt.Errorf("%s preservation requires resolved database context", family.Noun)
		}
	}
	result.Desired.Records = append(result.Desired.Records, &protocol.ComparisonRecord{Kind: family.Kind, Subject: proto.CloneOf(item.reported), Value: normalized})
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
