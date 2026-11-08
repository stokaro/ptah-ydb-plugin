// Package replicationcompare compares the declared and the read YDB async
// replications and transfers offline, as Ptah's embedded YDB engine compares
// them: each side resolved as YDB keeps it, so a declaration naming a default
// and one leaving it out are the same object, and a replication's items
// compare by the tables they replicate.
//
// Whether YDB can make a change in place depends on the state the object
// reported, so the planner decides it; the comparison reports the change.
package replicationcompare

import (
	"context"
	"fmt"
	"path"
	"slices"
	"strings"

	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/stokaro/ptah/provider-sdk/provider"

	"github.com/stokaro/ptah-ydb-plugin/internal/authored"
	"github.com/stokaro/ptah-ydb-plugin/internal/objectcompare"
	"github.com/stokaro/ptah-ydb-plugin/internal/replication"
	ydbtopics "github.com/stokaro/ptah-ydb-plugin/internal/topics"
	"github.com/stokaro/ptah-ydb-plugin/schema"
)

// The comparison services this package owns.
const (
	// ReplicationServiceID names the owner of async replication comparisons.
	ReplicationServiceID = "ydb-async-replications"
	// TransferServiceID names the owner of transfer comparisons.
	TransferServiceID = "ydb-transfers"
)

// CompareReplications evaluates complete operands and coverage without
// database access. A target without the async_replication capability is
// refused only where the comparison reports a change.
func CompareReplications(ctx context.Context, request *protocol.CompareModelsRequest) (*protocol.ComparedModels, error) {
	return objectcompare.Compare(ctx, request, ReplicationFamily())
}

// CompareTransfers evaluates complete transfer operands and coverage, as
// [CompareReplications] does for replications, after it refuses a transfer
// whose table or topic the schema does not declare (see
// [undeclaredDependencies]).
func CompareTransfers(ctx context.Context, request *protocol.CompareModelsRequest) (*protocol.ComparedModels, error) {
	if err := undeclaredDependencies(request); err != nil {
		return nil, err
	}
	return objectcompare.Compare(ctx, request, TransferFamily())
}

// TransferDeclaredKinds are the object models of another comparison whose
// declared objects the transfer comparison reads (models.declared-objects):
// the topics, which the topic comparison owns.
func TransferDeclaredKinds() []string {
	return []string{schema.TopicProperties}
}

// TransferDeclaredFacetKinds are the facet models whose declared facets the
// transfer comparison reads (models.declared-facets): the changefeeds of the
// tables, which the row-table comparison owns.
func TransferDeclaredFacetKinds() []string {
	return []string{schema.ChangefeedProperties}
}

// undeclaredDependencies refuses a declared transfer whose table or topic
// would not be there once the plan has run, in the words of the host's
// embedded engine, which refused both for each transfer in turn.
//
// The table has to be declared, where the host lists every declared table
// among the declared relations (models.declared-relations): YDB creates no
// transfer into a table that is not there (`Path does not exist`, measured on
// 26.2.1.14), and a plan that leaves the table out drops it or leaves it
// absent. A host that lists only views leaves the question open.
//
// A topic of the transfer's own database has to be declared as a topic, where
// the host also sends the declared topics (models.declared-objects), or be a
// changefeed of a declared table: YDB accepts a transfer of a missing topic
// and then stops it (`Discovery error`), and a topic the schema leaves out is
// one the plan drops or never creates. A table's changefeeds are a facet of
// the table another comparison owns: where the host sends them
// (models.declared-facets), a stream of a declared table has to be one of its
// declared changefeeds; a host that does not leaves the comparison to take
// any stream of a declared table for one of its changefeeds. A topic of
// another database is not this plan's to keep.
//
// A target without transfers is left to the comparison, which refuses a
// declared transfer by the capability first, as the embedded engine did.
func undeclaredDependencies(request *protocol.CompareModelsRequest) error {
	features := request.GetTargetFeatures()
	tables := slices.Contains(features, provider.DeclaredRelationsFeature)
	declaredTopics := tables && slices.Contains(features, provider.DeclaredObjectsFeature)
	declaredChangefeeds := tables && slices.Contains(features, provider.DeclaredFacetsFeature)
	if !tables || !request.GetCapabilities()[replication.CapabilityTransfers] {
		return nil
	}
	for _, record := range request.GetDesired().GetRecords() {
		if record.GetKind() != schema.TransferProperties || record.GetValue() == nil {
			continue
		}
		value := record.GetValue().GetValue()
		spec, err := schema.DesiredTransfer(value)
		if err != nil {
			return err
		}
		owner := value.GetOwner()
		subject := "transfer " + ydbtopics.Display(owner.GetSchema(), owner.GetName())
		target := strings.Trim(spec.Target, "/")
		if !declaresTable(request, target) {
			return authored.New(fmt.Sprintf("%s: it writes table %s, which the schema does not declare, and YDB creates no transfer into "+
				"a table that is not there (`Path does not exist`); declare the table", subject, target))
		}
		if !declaredTopics {
			continue
		}
		source, err := readTopic(value)
		if err != nil {
			return err
		}
		if source == nil || declaresTopic(request, source) {
			continue
		}
		if declaresTable(request, source.GetSchema().GetNormalized()) {
			if !declaredChangefeeds {
				continue
			}
			changefeed, err := declaresChangefeed(request, source)
			if err != nil {
				return err
			}
			if changefeed {
				continue
			}
		}
		return authored.New(fmt.Sprintf("%s: it reads topic %s, which the schema declares neither as a topic nor as a changefeed, so the "+
			"plan leaves no such topic, and YDB accepts such a transfer and then stops it (`Discovery error`); declare the topic or the "+
			"changefeed", subject, ydbtopics.Display(source.GetSchema().GetNormalized(), source.GetName().GetNormalized())))
	}
	return nil
}

// readTopic is the identity of the topic of this database the declared
// transfer value reads, as the transfer requires it, or nil for a transfer of
// another database's topic.
func readTopic(value *protocol.Envelope) (*protocol.ComparisonIdentity, error) {
	required, _, err := schema.RequiredObjects(value)
	if err != nil {
		return nil, err
	}
	for _, identity := range required {
		if identity.GetKind() == schema.TopicProperties {
			return identity, nil
		}
	}
	return nil, nil
}

// declaresTopic reports whether the request's declared objects hold topic.
func declaresTopic(request *protocol.CompareModelsRequest, topic *protocol.ComparisonIdentity) bool {
	return slices.ContainsFunc(request.GetDeclaredObjects(), func(object *protocol.ComparisonIdentity) bool {
		return object.GetKind() == schema.TopicProperties && object.GetSchema().GetNormalized() == topic.GetSchema().GetNormalized() &&
			object.GetName().GetNormalized() == topic.GetName().GetNormalized()
	})
}

// declaresChangefeed reports whether the declared changefeeds the host sent
// (models.declared-facets) give the table at topic's path a changefeed of
// topic's name, the stream YDB keeps for it.
func declaresChangefeed(request *protocol.CompareModelsRequest, topic *protocol.ComparisonIdentity) (bool, error) {
	directory, name := path.Split(topic.GetSchema().GetNormalized())
	directory = strings.TrimSuffix(directory, "/")
	for _, facet := range request.GetDeclaredFacets() {
		subject := facet.GetSubject()
		if facet.GetKind() != schema.ChangefeedProperties || subject.GetKind() != "table" || subject.GetSchema().GetNormalized() != directory ||
			subject.GetName().GetNormalized() != name {
			continue
		}
		names, err := schema.DeclaredChangefeedNames(facet.GetValue().GetValue())
		if err != nil {
			return false, err
		}
		return slices.Contains(names, topic.GetName().GetNormalized()), nil
	}
	return false, nil
}

// declaresTable reports whether the request's declared relations hold the
// table at tablePath, relative to the database root.
func declaresTable(request *protocol.CompareModelsRequest, tablePath string) bool {
	directory, name := path.Split(tablePath)
	directory = strings.TrimSuffix(directory, "/")
	return name != "" && slices.ContainsFunc(request.GetDeclaredRelations(), func(relation *protocol.ComparisonIdentity) bool {
		return relation.GetKind() == "table" && relation.GetSchema().GetNormalized() == directory && relation.GetName().GetNormalized() == name
	})
}

// ReplicationFamily is the async replication's part of a comparison.
func ReplicationFamily() objectcompare.Family {
	return specFamily[replication.ReplicationSpec]{
		serviceID: ReplicationServiceID, kind: schema.ReplicationProperties, changeKind: schema.ReplicationChange, capability: replication.CapabilityReplication,
		noun:     "async replication",
		observed: schema.ObservedReplication,
		desired:  schema.DesiredReplication,
		equal:    replication.ReplicationsEqual,
		change:   schema.CaptureReplicationChange,
		intent:   schema.CaptureReplicationIntent,
		// A replication that failed before it resolved its tables reports
		// none, and no declaration holds that.
		validate: replication.ValidateReplication,
	}.family()
}

// TransferFamily is the transfer's part of a comparison.
func TransferFamily() objectcompare.Family {
	return specFamily[replication.TransferSpec]{
		serviceID: TransferServiceID, kind: schema.TransferProperties, changeKind: schema.TransferChange, capability: replication.CapabilityTransfers,
		noun:     "transfer",
		observed: schema.ObservedTransfer,
		desired:  schema.DesiredTransfer,
		equal:    replication.TransfersEqual,
		change:   schema.CaptureTransferChange,
		intent:   schema.CaptureTransferIntent,
		validate: replication.ValidateTransfer,
	}.family()
}

// specFamily is what one replication object kind contributes to its
// comparison: its names, and how a spec of type S is decoded from each side,
// compared, kept and validated. Both kinds share every other part.
type specFamily[S any] struct {
	serviceID, kind, changeKind, capability, noun string
	// observed decodes the read object into its spec and reported state.
	observed func(*protocol.Envelope) (S, string, error)
	// desired decodes the declared object into its spec.
	desired func(*protocol.Envelope) (S, error)
	// equal reports whether the current spec already holds the desired one.
	equal func(desired, current S) bool
	// change captures the change from the current to the desired object.
	change func(id string, owner *protocol.Reference, before, after *protocol.Envelope) (*protocol.NormalizedValue, error)
	// intent captures the desired value that keeps a spec as it is.
	intent func(*protocol.Reference, S) (*protocol.Envelope, error)
	// validate refuses a read spec no declaration can hold.
	validate func(S) error
}

// family builds the comparison family: a change needs the capability, no
// subject is reserved or unsettled, and a read object whose spec fails
// validation is reported as having no declaration that keeps it.
func (f specFamily[S]) family() objectcompare.Family {
	return objectcompare.Family{
		ServiceID: f.serviceID, Kind: f.kind, ChangeKind: f.changeKind, Capability: f.capability, ChangeCapability: true,
		Noun:         f.noun,
		DesiredModel: func() *protocol.ValueModel { return schema.NativeDesiredModel(f.kind) },
		Owner: func(subject *protocol.ComparisonIdentity) (*protocol.Reference, error) {
			return schema.NativeBoundOwner(f.kind, subject)
		},
		NormalizeDesired: schema.NormalizeDesired, NormalizeObserved: schema.Normalize, RequiredObjects: schema.RequiredObjects,
		Reserved:  func(*protocol.ComparisonIdentity) string { return "" },
		Unsettled: func(*protocol.Envelope, *protocol.Envelope) error { return nil },
		Equal: func(current, desired *protocol.Envelope) (bool, error) {
			before, _, err := f.observed(current)
			if err != nil {
				return false, err
			}
			after, err := f.desired(desired)
			return err == nil && f.equal(after, before), err
		},
		Change: f.change,
		Preserve: func(owner *protocol.Reference, current *protocol.Envelope) (*protocol.Envelope, error) {
			spec, _, err := f.observed(current)
			if err != nil {
				return nil, err
			}
			return f.intent(owner, spec)
		},
		Unkeepable: func(current *protocol.Envelope) (string, error) {
			spec, state, err := f.observed(current)
			if err != nil {
				return "", err
			}
			if err := f.validate(spec); err != nil {
				return "The " + f.noun + " (" + stateText(state) + ") has no declaration that keeps it: " + err.Error(), nil
			}
			return "", nil
		},
	}
}

func stateText(state string) string {
	if state == "" {
		return "state not reported"
	}
	return state
}
