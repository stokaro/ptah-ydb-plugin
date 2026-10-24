package tablecreate_test

import (
	"testing"

	qt "github.com/frankban/quicktest"
	"github.com/stokaro/ptah-ydb-plugin/internal/tablecreate"
	"github.com/stokaro/ptah-ydb-plugin/schema"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
)

func knowledgeOf(state protocol.ModelKnowledge_State) *protocol.ModelKnowledge {
	return &protocol.ModelKnowledge{State: state, Reason: "fixture"}
}

func kindCoverage(sources ...*protocol.ComparisonKnowledgeSource) *protocol.ComparisonKindCoverage {
	return &protocol.ComparisonKindCoverage{Model: schema.TableDesiredModel(), Sources: sources}
}

func outside(state protocol.ModelKnowledge_State) *protocol.ComparisonKnowledgeSource {
	return &protocol.ComparisonKnowledgeSource{Outside: knowledgeOf(state)}
}

func subjectCoverage(state protocol.ModelKnowledge_State, targets ...string) *protocol.ComparisonSubjectCoverage {
	return &protocol.ComparisonSubjectCoverage{Kind: schema.TableProperties, Subject: tableSubject(), Knowledge: knowledgeOf(state), Targets: targets}
}

func TestFacetKnown_HappyPath(t *testing.T) {
	complete, absent, defaulted := protocol.ModelKnowledge_COMPLETE, protocol.ModelKnowledge_ABSENT, protocol.ModelKnowledge_DEFAULTED
	for _, test := range []struct {
		name     string
		coverage *protocol.ComparisonCoverage
		desired  bool
		want     bool
	}{
		{"complete knowledge of the kind", &protocol.ComparisonCoverage{Kinds: []*protocol.ComparisonKindCoverage{kindCoverage(outside(complete))}}, true, true},
		{"an explicit absence of the kind", &protocol.ComparisonCoverage{Kinds: []*protocol.ComparisonKindCoverage{kindCoverage(outside(absent))}}, false, true},
		{"defaults requested on the desired side", &protocol.ComparisonCoverage{Kinds: []*protocol.ComparisonKindCoverage{kindCoverage(outside(defaulted))}}, true, true},
		{"every source of the kind is complete", &protocol.ComparisonCoverage{Kinds: []*protocol.ComparisonKindCoverage{kindCoverage(outside(complete), outside(absent))}}, true, true},
		{"a subject record overrides unknown knowledge of the kind", &protocol.ComparisonCoverage{
			Kinds:    []*protocol.ComparisonKindCoverage{kindCoverage(outside(protocol.ModelKnowledge_UNINSPECTED))},
			Subjects: []*protocol.ComparisonSubjectCoverage{subjectCoverage(complete)}}, true, true},
		{"a subject record for this engine", &protocol.ComparisonCoverage{Subjects: []*protocol.ComparisonSubjectCoverage{subjectCoverage(complete, "ydb")}}, true, true},
		{"a known region beside complete knowledge", &protocol.ComparisonCoverage{Kinds: []*protocol.ComparisonKindCoverage{kindCoverage(&protocol.ComparisonKnowledgeSource{
			Outside: knowledgeOf(complete), Regions: []*protocol.ComparisonRegion{{Scope: &protocol.ComparisonNamespace{PathExact: true}, Knowledge: knowledgeOf(complete)}}})}}, true, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := qt.New(t)
			c.Assert(tablecreate.FacetKnown(test.coverage, schema.TableProperties, tableSubject(), test.desired), qt.IsTrue)
		})
	}
}

func TestFacetKnown_NotKnown(t *testing.T) {
	complete, uninspected := protocol.ModelKnowledge_COMPLETE, protocol.ModelKnowledge_UNINSPECTED
	for _, test := range []struct {
		name     string
		coverage *protocol.ComparisonCoverage
		desired  bool
	}{
		{"no coverage", nil, true},
		{"no coverage of the kind", &protocol.ComparisonCoverage{}, true},
		{"a kind with no source", &protocol.ComparisonCoverage{Kinds: []*protocol.ComparisonKindCoverage{kindCoverage()}}, true},
		{"unknown knowledge of the kind", &protocol.ComparisonCoverage{Kinds: []*protocol.ComparisonKindCoverage{kindCoverage(outside(uninspected))}}, true},
		{"unrepresentable knowledge of the kind", &protocol.ComparisonCoverage{Kinds: []*protocol.ComparisonKindCoverage{kindCoverage(outside(protocol.ModelKnowledge_UNREPRESENTABLE))}}, true},
		{"one unknown source among complete ones", &protocol.ComparisonCoverage{Kinds: []*protocol.ComparisonKindCoverage{kindCoverage(outside(complete), outside(uninspected))}}, true},
		{"defaults requested on the observed side", &protocol.ComparisonCoverage{Kinds: []*protocol.ComparisonKindCoverage{kindCoverage(outside(protocol.ModelKnowledge_DEFAULTED))}}, false},
		{"an unknown region", &protocol.ComparisonCoverage{Kinds: []*protocol.ComparisonKindCoverage{kindCoverage(&protocol.ComparisonKnowledgeSource{
			Outside: knowledgeOf(complete), Regions: []*protocol.ComparisonRegion{{Scope: &protocol.ComparisonNamespace{PathExact: true}, Knowledge: knowledgeOf(uninspected)}}})}}, true},
		{"a subject record that is unknown beside complete knowledge of the kind", &protocol.ComparisonCoverage{
			Kinds:    []*protocol.ComparisonKindCoverage{kindCoverage(outside(complete))},
			Subjects: []*protocol.ComparisonSubjectCoverage{subjectCoverage(uninspected)}}, true},
		{"complete knowledge of another kind", &protocol.ComparisonCoverage{Kinds: []*protocol.ComparisonKindCoverage{{Model: schema.ColumnDesiredModel(), Sources: []*protocol.ComparisonKnowledgeSource{outside(complete)}}}}, true},
		{"a subject record for another engine", &protocol.ComparisonCoverage{Subjects: []*protocol.ComparisonSubjectCoverage{subjectCoverage(complete, "mysql")}}, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := qt.New(t)
			c.Assert(tablecreate.FacetKnown(test.coverage, schema.TableProperties, tableSubject(), test.desired), qt.IsFalse)
		})
	}
}
