// Package testcomparison constructs captured coordination exchanges for tests.
package testcomparison

import (
	"context"

	"github.com/stokaro/ptah-ydb-plugin/internal/backend"
	"github.com/stokaro/ptah-ydb-plugin/internal/coordinationcompare"
	"github.com/stokaro/ptah-ydb-plugin/schema"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb_Coordination"
	"google.golang.org/protobuf/proto"
)

// Request captures optional schema operands with independent complete coverage.
func Request(desired *schema.CoordinationIntent, observed *Ydb_Coordination.DescribeNodeResult) (*protocol.CompareModelsRequest, error) {
	subject := &protocol.ComparisonIdentity{Kind: schema.CoordinationProperties, Database: Part("/local"), Schema: Part("app"), Name: Part("node")}
	owner := &protocol.Reference{Engine: "ydb", Kind: schema.CoordinationProperties, Database: "/local", Schema: "app", Name: "node"}
	var currentModel *protocol.ValueModel
	for _, model := range schema.ObservedModels() {
		if model.Kind == schema.CoordinationProperties {
			currentModel = model
		}
	}
	desiredState, currentState := State(schema.CoordinationDesiredModel()), State(currentModel)
	request := &protocol.CompareModelsRequest{Engine: "ydb", ComparisonId: coordinationcompare.ServiceID, Kinds: []string{schema.CoordinationProperties}, Identifiers: &protocol.ComparisonIdentifiers{Rules: proto.CloneOf(backend.Description().Engines[0].Identifiers)}, Capabilities: map[string]bool{"coordination_nodes": true}, DatabasePath: "/local", Desired: desiredState, Current: currentState}
	for _, side := range []struct {
		state   *protocol.ComparisonState
		present bool
	}{{desiredState, desired != nil}, {currentState, observed != nil}} {
		if !side.present {
			continue
		}
		var value *protocol.Envelope
		var err error
		if side.state == desiredState {
			value, err = schema.CaptureCoordinationIntent(owner, *desired)
		} else {
			value, err = schema.CaptureObject(owner, observed)
		}
		if err != nil {
			return nil, err
		}
		normalized, err := backend.New().NormalizeValues(context.Background(), &protocol.NormalizeValuesRequest{Engine: "ydb", Values: []*protocol.ValueInput{{Id: "node", Representation: side.state.Representation, Value: value}}})
		if err != nil {
			return nil, err
		}
		side.state.Records = []*protocol.ComparisonRecord{{Kind: schema.CoordinationProperties, Subject: proto.CloneOf(subject), Value: normalized.Values[0]}}
	}
	return request, nil
}

// Part retains exact source and normalized spelling.
func Part(name string) *protocol.ComparisonPart {
	return &protocol.ComparisonPart{Source: name, Normalized: name}
}

// State declares complete namespace knowledge for one model representation.
func State(model *protocol.ValueModel) *protocol.ComparisonState {
	return &protocol.ComparisonState{Representation: model.Representation, Coverage: &protocol.ComparisonCoverage{Kinds: []*protocol.ComparisonKindCoverage{{Model: model, Sources: []*protocol.ComparisonKnowledgeSource{{Outside: Knowledge(protocol.ModelKnowledge_COMPLETE)}}}}}}
}

// Knowledge supplies the reason required for incomplete claims.
func Knowledge(state protocol.ModelKnowledge_State) *protocol.ModelKnowledge {
	result := &protocol.ModelKnowledge{State: state}
	if state == protocol.ModelKnowledge_UNINSPECTED || state == protocol.ModelKnowledge_UNREPRESENTABLE {
		result.Reason = "Fixture source is incomplete"
	}
	return result
}
