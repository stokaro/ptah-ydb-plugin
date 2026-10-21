package coordinationcompare

import (
	"bytes"
	"context"
	"fmt"
	"slices"
	"strconv"

	"github.com/stokaro/ptah-ydb-plugin/schema"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/stokaro/ptah/provider-sdk/provider"
	"google.golang.org/protobuf/proto"
)

// ServiceID names the owner of coordination object comparisons.
const ServiceID = "ydb-coordination"

type node struct {
	subject *protocol.ComparisonIdentity
	desired *protocol.Envelope
	current *protocol.Envelope
}

// Compare evaluates complete operands and coverage without database access.
// The backend validates the negotiated SDK request and result around this call.
func Compare(ctx context.Context, request *protocol.CompareModelsRequest) (*protocol.ComparedModels, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if request.GetComparisonId() != ServiceID || !slices.Equal(request.Kinds, []string{schema.CoordinationProperties}) {
		return nil, fmt.Errorf("unknown coordination comparison")
	}
	for _, part := range request.GetIdentifiers().GetResolvedNames() {
		if part.Name != part.Key {
			return nil, fmt.Errorf("YDB comparison requires exact identifiers")
		}
	}
	if request.GetIdentifiers().GetDefaultSchema() != "" || request.GetIdentifiers().GetCatalogCollation() != "" {
		return nil, fmt.Errorf("YDB comparison has unsupported identifier defaults")
	}
	nodes, err := capture(request)
	if err != nil {
		return nil, err
	}
	if len(nodes) > 0 && !request.Capabilities["coordination_nodes"] {
		return nil, fmt.Errorf("coordination node capability was not established")
	}
	result := &protocol.ComparedModels{Complete: true, Desired: proto.CloneOf(request.Desired)}
	for index, item := range nodes {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if err := compareNode(request, result, item, strconv.Itoa(index)); err != nil {
			return nil, err
		}
	}
	if !namespaceComplete(request.Current.Coverage) {
		result.Undecided = append(result.Undecided, &protocol.ComparisonUndecided{Kind: schema.CoordinationProperties, Reason: "The coordination namespace was not completely inspected"})
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return result, nil
}

func capture(request *protocol.CompareModelsRequest) ([]node, error) {
	var nodes []node
	find := func(subject *protocol.ComparisonIdentity) *node {
		for i := range nodes {
			if provider.CompareIdentities(nodes[i].subject, subject) == 0 {
				return &nodes[i]
			}
		}
		nodes = append(nodes, node{subject: proto.CloneOf(subject)})
		return &nodes[len(nodes)-1]
	}
	for _, state := range []*protocol.ComparisonState{request.Desired, request.Current} {
		for _, record := range state.Records {
			if !selected(record.Targets) {
				return nil, fmt.Errorf("coordination record is outside the selected target")
			}
			if err := identity(record.Subject); err != nil {
				return nil, err
			}
			value, err := captureRecord(request, state, record)
			if err != nil {
				return nil, err
			}
			item := find(record.Subject)
			if state.Representation == protocol.ValueInput_DESIRED {
				item.desired = value
			} else {
				item.current = value
			}
		}
		for _, claim := range state.GetCoverage().GetSubjects() {
			if !selected(claim.Targets) {
				continue
			}
			if claim.GetKnowledge().GetState() == protocol.ModelKnowledge_DEFAULTED {
				return nil, fmt.Errorf("a coordination object cannot be defaulted")
			}
			if err := identity(claim.Subject); err != nil {
				return nil, err
			}
			find(claim.Subject)
		}
	}
	slices.SortFunc(nodes, func(a, b node) int { return provider.CompareIdentities(a.subject, b.subject) })
	return nodes, nil
}

func identity(subject *protocol.ComparisonIdentity) error {
	_, err := schema.CoordinationBoundOwner(subject)
	return err
}

func owner(subject *protocol.ComparisonIdentity) *protocol.Reference {
	return &protocol.Reference{Engine: "ydb", Kind: schema.CoordinationProperties, Database: subject.GetDatabase().GetNormalized(), Schema: subject.GetSchema().GetNormalized(), Name: subject.GetName().GetNormalized()}
}

func captureRecord(request *protocol.CompareModelsRequest, state *protocol.ComparisonState, record *protocol.ComparisonRecord) (*protocol.Envelope, error) {
	value := record.GetValue().GetValue()
	var normalized *protocol.Envelope
	var err error
	if state.Representation == protocol.ValueInput_DESIRED {
		normalized, err = schema.NormalizeDesired(value)
	} else {
		normalized, err = schema.Normalize(value)
	}
	if err != nil {
		return nil, err
	}
	format := schema.CoordinationProperties + "/observed-json-v1"
	if state.Representation == protocol.ValueInput_DESIRED {
		format = schema.CoordinationDesiredModel().CanonicalFormat
	}
	expected := &protocol.NormalizedValue{Id: record.Value.Id, Value: normalized, NormalizedOwner: proto.CloneOf(normalized.Owner), CanonicalFormat: format, CanonicalProjection: bytes.Clone(normalized.Payload)}
	if !proto.Equal(expected, record.Value) {
		return nil, fmt.Errorf("coordination normalization receipt differs from its payload")
	}
	bound := owner(record.Subject)
	captured := proto.CloneOf(value.Owner)
	if captured.Database == "" && record.Subject.GetDatabase().GetDefaulted() && record.Subject.GetDatabase().GetNormalized() == request.DatabasePath {
		captured.Database = request.DatabasePath
	}
	if !proto.Equal(captured, bound) {
		return nil, fmt.Errorf("coordination payload and comparison subject disagree")
	}
	normalized.Owner = bound
	claim := knowledge(state.Coverage, record.Subject)
	if claim.GetState() == protocol.ModelKnowledge_ABSENT || claim.GetState() == protocol.ModelKnowledge_DEFAULTED {
		return nil, fmt.Errorf("present coordination object has absent or defaulted coverage")
	}
	return normalized, nil
}

func undecided(result *protocol.ComparedModels, item node, reason string) {
	result.Undecided = append(result.Undecided, &protocol.ComparisonUndecided{Kind: schema.CoordinationProperties, Subject: proto.CloneOf(item.subject), Reason: reason})
}

func compareNode(request *protocol.CompareModelsRequest, result *protocol.ComparedModels, item node, id string) error {
	name := item.subject.GetName().GetSource()
	if namespace := item.subject.GetSchema().GetSource(); namespace != "" {
		name = namespace + "/" + name
	}
	for _, exclusion := range request.Exclusions {
		if exclusion.GetKind() == schema.CoordinationProperties && exclusion.GetName() == name {
			return nil
		}
	}
	if item.subject.GetSchema().GetNormalized() == "" && item.subject.GetName().GetNormalized() == "ptah_locks" {
		undecided(result, item, "The runtime lock node requires an explicit ownership and lifecycle contract")
		return nil
	}
	if claim, found := subjectKnowledge(request.Desired.Coverage, item.subject); found && unknown(claim) && item.desired != nil {
		undecided(result, item, "The desired source cannot describe the complete coordination node")
		return nil
	}
	if claim, found := subjectKnowledge(request.Current.Coverage, item.subject); (found && unknown(claim)) || (item.current == nil && unknown(knowledge(request.Current.Coverage, item.subject))) {
		undecided(result, item, "The current coordination node or its absence was not established")
		return nil
	}
	if item.desired == nil && unknown(knowledge(request.Desired.Coverage, item.subject)) {
		if item.current == nil {
			return nil
		}
		return preserve(result, item, id)
	}
	if item.desired == nil && item.current == nil {
		return nil
	}
	if item.desired != nil {
		for _, relation := range request.DeclaredRelations {
			if relation.GetDatabase().GetNormalized() == item.subject.GetDatabase().GetNormalized() && relation.GetCatalog().GetNormalized() == "" && relation.GetSchema().GetNormalized() == item.subject.GetSchema().GetNormalized() && relation.GetName().GetNormalized() == item.subject.GetName().GetNormalized() && len(relation.GetPath()) == 0 {
				return fmt.Errorf("coordination node collides with a declared relation")
			}
		}
	}
	if item.desired == nil {
		for _, required := range request.Required {
			if provider.CompareIdentities(required, item.subject) == 0 {
				undecided(result, item, "A declared dependency requires the coordination node")
				return nil
			}
		}
	}
	if item.desired != nil && item.current != nil {
		before, err := schema.CoordinationConfiguration(item.current, protocol.ValueInput_OBSERVED)
		if err != nil {
			return err
		}
		after, err := schema.CoordinationConfiguration(item.desired, protocol.ValueInput_DESIRED)
		if err != nil {
			return err
		}
		a, err := schema.EffectiveCoordination(before)
		if err != nil {
			return err
		}
		b, err := schema.EffectiveCoordination(after)
		if err != nil {
			return err
		}
		if a == b {
			return nil
		}
	}
	change, err := schema.CaptureCoordinationChange("change-"+id, owner(item.subject), item.current, item.desired)
	if err != nil {
		return err
	}
	result.Changes = append(result.Changes, &protocol.ComparedChange{Kind: schema.CoordinationProperties, Subject: proto.CloneOf(item.subject), Value: change})
	return nil
}

func preserve(result *protocol.ComparedModels, item node, id string) error {
	config, err := schema.CoordinationConfiguration(item.current, protocol.ValueInput_OBSERVED)
	if err != nil {
		return err
	}
	value, err := schema.CaptureCoordinationIntent(owner(item.subject), config)
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
	result.Desired.Records = append(result.Desired.Records, &protocol.ComparisonRecord{Kind: schema.CoordinationProperties, Subject: proto.CloneOf(item.subject), Value: &protocol.NormalizedValue{Id: candidate, Value: value, NormalizedOwner: proto.CloneOf(value.Owner), CanonicalFormat: schema.CoordinationDesiredModel().CanonicalFormat, CanonicalProjection: bytes.Clone(value.Payload)}})
	if len(kindSources(result.Desired.Coverage)) == 0 && len(result.Desired.Coverage.Kinds) == 0 {
		result.Desired.Coverage.Kinds = []*protocol.ComparisonKindCoverage{{Model: schema.CoordinationDesiredModel(), Sources: []*protocol.ComparisonKnowledgeSource{{Outside: &protocol.ModelKnowledge{State: protocol.ModelKnowledge_UNINSPECTED, Reason: "The desired source did not describe coordination nodes"}}}}}
	}
	for i, claim := range result.Desired.Coverage.Subjects {
		if provider.CompareIdentities(claim.Subject, item.subject) == 0 {
			if !selected(claim.Targets) {
				return fmt.Errorf("preserved coordination subject conflicts with another target's coverage")
			}
			result.Desired.Coverage.Subjects = slices.Delete(result.Desired.Coverage.Subjects, i, i+1)
			break
		}
	}
	result.Desired.Coverage.Subjects = append(result.Desired.Coverage.Subjects, &protocol.ComparisonSubjectCoverage{Kind: schema.CoordinationProperties, Subject: proto.CloneOf(item.subject), Knowledge: &protocol.ModelKnowledge{State: protocol.ModelKnowledge_COMPLETE}})
	return nil
}
