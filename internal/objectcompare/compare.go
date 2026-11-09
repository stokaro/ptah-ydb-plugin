package objectcompare

import (
	"bytes"
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"google.golang.org/protobuf/proto"
	protocol "ptah.run/provider-sdk/protocol/experimental"
	"ptah.run/provider-sdk/provider"

	"github.com/stokaro/ptah-ydb-plugin/internal/authored"
	"github.com/stokaro/ptah-ydb-plugin/internal/featureflags"
	"github.com/stokaro/ptah-ydb-plugin/schema"
)

// Family is what one kind of standalone object brings to a comparison. Every
// function is required unless its comment permits nil.
type Family struct {
	// ServiceID is the comparison the host asks for the family's kind.
	ServiceID string
	// Kind is the model kind of both representations of the object.
	Kind string
	// ChangeKind is the model kind of the changes Change captures, which the
	// comparison counts the family's objects under.
	ChangeKind string
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
	// RequiredRemoval refuses a comparison that would drop an object a
	// declared object requires, with this text after the family's noun and
	// the object's path. It is for a family whose object the server lets go
	// while a reader still names it, so the drop the source asks for leaves
	// the reader naming nothing. Empty leaves such an object undecided, and
	// the current one stays.
	RequiredRemoval string
	// UndeclaredRequired says why a declared object may not require an
	// object of the family that the desired side does not declare, or "".
	// It is for a family whose object a declaration has to name beside the
	// object that reads it, whatever the database holds. Nil lets a declared
	// object require any object.
	UndeclaredRequired func(required *protocol.ComparisonIdentity) string
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
	families := []Family{family}
	result, decided, err := compare(ctx, request, families)
	if err != nil {
		return nil, err
	}
	countDecided(request, result, families, decided)
	return result, nil
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
	result, decided, err := compare(ctx, request, selected)
	if err != nil {
		return nil, nil, err
	}
	countDecided(request, result, families, decided)
	return result, decided, nil
}

// countDecided states, for a request whose target features ask, how many
// objects each family of the service holds on each side of the comparison.
// Every family gets an entry, whether or not the request names its kind: a
// change kind a service does not count has no inventory entry at all, and a
// family the batch holds nothing of has zeros.
//
// An object counts as matched where both sides hold it, whether or not it
// differs and however the plan makes its change, as added where only the
// desired side holds it, and as removed where only the current side does. An
// object compareObject excludes, reserves, keeps or leaves undecided is no
// Decided, so it counts on neither side.
func countDecided(request *protocol.CompareModelsRequest, result *protocol.ComparedModels, families []Family, decided []Decided) {
	if !slices.Contains(request.GetTargetFeatures(), provider.ComparisonCountsFeature) {
		return
	}
	for _, family := range families {
		count := &protocol.ComparisonCount{Kind: family.ChangeKind}
		for _, pair := range decided {
			switch {
			case pair.Kind != family.Kind:
			case pair.Desired != nil && pair.Current != nil:
				count.Matched++
			case pair.Desired != nil:
				count.Added++
			default:
				count.Removed++
			}
		}
		result.Counts = append(result.Counts, count)
	}
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
	if err := refuseFamilies(bound, families, objects, decided); err != nil {
		return nil, nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	return result, decided, nil
}

// refuseFamilies applies the refusals that read the whole comparison of
// each family: a dotted limit that would drop an object, and an object a
// declaration requires without declaring it.
func refuseFamilies(request *protocol.CompareModelsRequest, families []Family, objects []object, decided []Decided) error {
	for _, family := range families {
		if err := refuseDottedLimits(request, family, objects, decided); err != nil {
			return err
		}
		if err := refuseUndeclaredRequired(request, family, objects); err != nil {
			return err
		}
	}
	return nil
}

// refuseUndeclaredRequired refuses a comparison where a declared object
// requires an object of family that the desired side does not declare, with
// the family's reason. A desired side that does not know whether it declares
// the object, such as a source that cannot describe the kind, is not refused.
func refuseUndeclaredRequired(request *protocol.CompareModelsRequest, family Family, objects []object) error {
	if family.UndeclaredRequired == nil {
		return nil
	}
	for _, required := range request.GetRequired() {
		if required.GetKind() != family.Kind || unknown(knowledge(request.GetDesired().GetCoverage(), family.Kind, required)) ||
			slices.ContainsFunc(objects, func(item object) bool {
				return item.family.Kind == family.Kind && item.desired != nil && provider.CompareIdentities(item.subject, required) == 0
			}) {
			continue
		}
		if reason := family.UndeclaredRequired(required); reason != "" {
			return authored.New(reason)
		}
	}
	return nil
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
	set := objectSet{family: family}
	originals := []*protocol.ComparisonState{original.Desired, original.Current}
	for side, state := range []*protocol.ComparisonState{bound.Desired, bound.Current} {
		if err := set.addRecords(state, originals[side], side, session); err != nil {
			return nil, err
		}
		if err := set.addClaims(state, originals[side]); err != nil {
			return nil, err
		}
	}
	slices.SortFunc(set.objects, func(a, b object) int { return provider.CompareIdentities(a.subject, b.subject) })
	return set.objects, nil
}

// objectSet collects the objects of one family that capture pairs, in the
// order their subjects are first seen.
type objectSet struct {
	family  *Family
	objects []object
}

// find returns the object of subject, adding one that names reported at rank
// when the set holds none. A lower rank than the object's replaces the
// identity it reports.
func (s *objectSet) find(subject, reported *protocol.ComparisonIdentity, rank int) *object {
	for i := range s.objects {
		if provider.CompareIdentities(s.objects[i].subject, subject) == 0 {
			if rank < s.objects[i].rank {
				s.objects[i].reported, s.objects[i].rank = proto.CloneOf(reported), rank
			}
			return &s.objects[i]
		}
	}
	s.objects = append(s.objects, object{family: s.family, subject: proto.CloneOf(subject), reported: proto.CloneOf(reported), rank: rank})
	return &s.objects[len(s.objects)-1]
}

// addRecords sets the records of the family's kind in state, as captureRecord
// reads them, on the objects their subjects name. original is state as sent,
// with its records in the same order, and side, 0 for the desired state and 1
// for the current one, is the rank of the identity a record reports. A record
// outside the selected target or with an invalid subject is refused.
func (s *objectSet) addRecords(state, original *protocol.ComparisonState, side int, session string) error {
	for index, record := range state.Records {
		if record.Kind != s.family.Kind {
			continue
		}
		if !selected(record.Targets) {
			return fmt.Errorf("%s record is outside the selected target", s.family.Noun)
		}
		if _, err := s.family.Owner(record.Subject); err != nil {
			return err
		}
		value, err := captureRecord(state, *s.family, record, session)
		if err != nil {
			return err
		}
		item := s.find(record.Subject, original.Records[index].Subject, side)
		if state.Representation == protocol.ValueInput_DESIRED {
			item.desired = value
		} else {
			item.current = value
		}
	}
	return nil
}

// addClaims adds an object for each subject a coverage claim of state names
// in the selected target for the family's kind. original is state as sent,
// with its claims in the same order; a claim reports its identity at rank 2,
// below every record. A defaulted claim or one with an invalid subject is
// refused.
func (s *objectSet) addClaims(state, original *protocol.ComparisonState) error {
	for index, claim := range state.GetCoverage().GetSubjects() {
		if !selected(claim.Targets) || claim.Kind != s.family.Kind {
			continue
		}
		if claim.GetKnowledge().GetState() == protocol.ModelKnowledge_DEFAULTED {
			return fmt.Errorf("a %s cannot be defaulted", s.family.Noun)
		}
		if _, err := s.family.Owner(claim.Subject); err != nil {
			return err
		}
		s.find(claim.Subject, original.Coverage.Subjects[index].Subject, 2)
	}
	return nil
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
	if screened, err := screenedOut(request, result, family, item, name); screened || err != nil {
		return nil, err
	}
	if settled, err := settleOmitted(request, result, family, item, id); settled || err != nil {
		return nil, err
	}
	if settled, err := settleRelations(request, result, family, item, name); settled || err != nil {
		return nil, err
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
		return nil, authored.New(fmt.Sprintf("changing %ss requires %s%s", family.Noun, family.Capability, featureflags.Clause(family.Capability)))
	}
	change, err := family.Change("change-"+id, reported, pair.Current, pair.Desired)
	if err != nil {
		return nil, err
	}
	result.Changes = append(result.Changes, &protocol.ComparedChange{Kind: family.Kind, Subject: proto.CloneOf(item.reported), Value: change})
	pair.Change = change
	return pair, nil
}

// leftOut reports whether the comparison leaves item out: an exclusion of
// request names it as name, the provider reserves its subject, or the read
// reserves its name for the provider's own storage. A declaration of a
// reserved subject or name is left undecided in result.
func leftOut(request *protocol.CompareModelsRequest, result *protocol.ComparedModels, family Family, item object, name string) bool {
	for _, exclusion := range request.Exclusions {
		if exclusion.GetKind() == family.Kind && exclusion.GetName() == name {
			return true
		}
	}
	if reason := family.Reserved(item.subject); reason != "" {
		// The provider's own object is left out of the comparison, as the
		// host's embedded engine leaves it out of every read. Only a
		// declaration at its path is worth a word.
		if item.desired != nil {
			undecided(result, family, item, reason)
		}
		return true
	}
	if claim, found := subjectKnowledge(request.Current.Coverage, family.Kind, item.subject); found && ReservedName(claim) {
		// The read reserves the name for the provider's own storage on every
		// native kind, so it holds no object of this family. It is left out as
		// Reserved leaves the family's own objects; only a declaration at the
		// name is worth a word.
		if item.desired != nil {
			undecided(result, family, item, claim.GetReason())
		}
		return true
	}
	return false
}

// unestablished reports whether a side of item is not established, and
// leaves item undecided in result if so: the family does not read a current
// object on this target, the desired source cannot describe the declared
// object, or the read did not establish the current object or its absence.
func unestablished(request *protocol.CompareModelsRequest, result *protocol.ComparedModels, family Family, item object) bool {
	// A current object the desired side makes no claim on is kept whatever
	// the target can read of it (see settleOmitted): nothing runs for it, so
	// the comparison is not left open. Without this, a target that lacks the
	// family's capability but holds one of its objects leaves every source
	// that does not describe the family undecided.
	unclaimed := item.desired == nil && unknown(knowledge(request.Desired.Coverage, family.Kind, item.subject))
	if family.Unread != nil && item.current != nil && !unclaimed {
		if reason := family.Unread(request.Capabilities); reason != "" {
			undecided(result, family, item, "The current "+family.Noun+" or its absence was not established: "+reason)
			return true
		}
	}
	if claim, found := subjectKnowledge(request.Desired.Coverage, family.Kind, item.subject); found && unknown(claim) && item.desired != nil {
		undecided(result, family, item, "The desired source cannot describe the complete "+family.Noun)
		return true
	}
	// A subject neither side holds is nothing to decide for a source that
	// makes no claim on it, however little the read knows of it.
	if unclaimed && item.current == nil {
		return false
	}
	if claim, found := subjectKnowledge(request.Current.Coverage, family.Kind, item.subject); (found && unknown(claim)) ||
		(item.current == nil && unknown(knowledge(request.Current.Coverage, family.Kind, item.subject))) {
		undecided(result, family, item, "The current "+family.Noun+" or its absence was not established")
		return true
	}
	return false
}

// screenedOut reports whether item, declared or read as name, is settled
// before its sides are compared: the comparison leaves it out (see leftOut),
// it is a creation inside a nested database, which is refused (see
// nestedCreation), or a side of it is not established (see unestablished).
func screenedOut(request *protocol.CompareModelsRequest, result *protocol.ComparedModels, family Family, item object, name string) (bool, error) {
	if leftOut(request, result, family, item, name) {
		return true, nil
	}
	if err := nestedCreation(request, family, item, name); err != nil {
		return true, err
	}
	return unestablished(request, result, family, item), nil
}

// nestedCreation refuses the creation of item, declared as name, inside a
// database nested in the session's database, where the read claims the kind
// with that database's reason (see schema.NestedDatabaseReason). The claim is
// complete, since this database holds nothing there, so without the refusal
// the comparison reports a creation, which YDB runs into the nested database.
func nestedCreation(request *protocol.CompareModelsRequest, family Family, item object, name string) error {
	if item.desired == nil || item.current != nil {
		return nil
	}
	root, nested := nestedRoot(request.Current.Coverage, family.Kind, item.subject)
	if !nested {
		return nil
	}
	return authored.New(schema.NestedDatabaseCreation(family.Noun, name, root, request.GetDatabasePath()))
}

// settleOmitted settles an item the desired side leaves out. A family that
// keeps omitted objects keeps the current one, or leaves undecided one whose
// absence the desired side claims; otherwise a current object whose absence
// the desired source did not establish is kept. An item neither side holds
// is settled with nothing to report. It reports whether item is settled,
// with the error of keeping it.
func settleOmitted(request *protocol.CompareModelsRequest, result *protocol.ComparedModels, family Family, item object, id string) (bool, error) {
	if item.desired == nil && family.KeepOmitted {
		if claim, found := subjectKnowledge(request.Desired.Coverage, family.Kind, item.subject); found && claim.GetState() == protocol.ModelKnowledge_ABSENT {
			undecided(result, family, item, family.OmittedRemoval)
			return true, nil
		}
		if item.current == nil {
			return true, nil
		}
		return true, keep(result, family, item, id)
	}
	if item.desired == nil && unknown(knowledge(request.Desired.Coverage, family.Kind, item.subject)) {
		if item.current == nil {
			return true, nil
		}
		return true, keep(result, family, item, id)
	}
	if item.desired == nil && item.current == nil {
		return true, nil
	}
	return false, nil
}

// settleRelations checks item against what request declares beside it. A
// declared object of a family that is not database-wide must not take the
// path of a declared relation, since YDB keeps one object at a path, as the
// host's embedded engine refused it; an object to remove that a declared dependency
// requires is refused, as name, when the family gives a reason, and left
// undecided otherwise. It reports whether item is settled.
func settleRelations(request *protocol.CompareModelsRequest, result *protocol.ComparedModels, family Family, item object, name string) (bool, error) {
	if item.desired != nil && !family.DatabaseWide {
		for _, relation := range request.DeclaredRelations {
			if relation.GetDatabase().GetNormalized() == item.subject.GetDatabase().GetNormalized() && relation.GetCatalog().GetNormalized() == "" &&
				relation.GetSchema().GetNormalized() == item.subject.GetSchema().GetNormalized() &&
				relation.GetName().GetNormalized() == item.subject.GetName().GetNormalized() && len(relation.GetPath()) == 0 {
				return false, authored.New(family.Noun + " " + name + " takes the path of a table or view the schema declares, and YDB keeps one object at a path")
			}
		}
	}
	if item.desired == nil {
		for _, required := range request.Required {
			if provider.CompareIdentities(required, item.subject) != 0 {
				continue
			}
			if family.RequiredRemoval != "" {
				return false, authored.New(family.Noun + " " + name + " " + family.RequiredRemoval)
			}
			undecided(result, family, item, "A declared dependency requires the "+family.Noun)
			return true, nil
		}
	}
	return false, nil
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
	if len(kindSources(result.Desired.Coverage, family.Kind)) == 0 &&
		!slices.ContainsFunc(result.Desired.Coverage.Kinds, func(claim *protocol.ComparisonKindCoverage) bool { return claim.GetModel().GetKind() == family.Kind }) {
		result.Desired.Coverage.Kinds = append(result.Desired.Coverage.Kinds, &protocol.ComparisonKindCoverage{
			Model: model,
			Sources: []*protocol.ComparisonKnowledgeSource{{
				Outside: &protocol.ModelKnowledge{State: protocol.ModelKnowledge_UNINSPECTED, Reason: "The desired source did not describe " + family.Noun + "s"},
			}},
		})
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
	result.Desired.Coverage.Subjects = append(result.Desired.Coverage.Subjects, &protocol.ComparisonSubjectCoverage{
		Kind: family.Kind, Subject: proto.CloneOf(item.reported), Knowledge: &protocol.ModelKnowledge{State: protocol.ModelKnowledge_COMPLETE},
	})
	return nil
}
