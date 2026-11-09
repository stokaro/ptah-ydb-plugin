package backend_test

import (
	"testing"

	qt "github.com/frankban/quicktest"
	protocol "ptah.run/provider-sdk/protocol/experimental"

	"github.com/stokaro/ptah-ydb-plugin/internal/backend"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcomparison"
	"github.com/stokaro/ptah-ydb-plugin/schema"
)

// unrepresentableSubject is an unrepresentable claim a read makes for the
// coordination node app/node, the subject testcomparison.Request compares,
// with the reason given.
func unrepresentableSubject(reason string) *protocol.ComparisonSubjectCoverage {
	return &protocol.ComparisonSubjectCoverage{Kind: schema.CoordinationProperties,
		Subject:   &protocol.ComparisonIdentity{Kind: schema.CoordinationProperties, Database: testcomparison.Part("/local"), Schema: testcomparison.Part("app"), Name: testcomparison.Part("node")},
		Knowledge: &protocol.ModelKnowledge{State: protocol.ModelKnowledge_UNREPRESENTABLE, Reason: reason}}
}

// A reservation names its identity on every native kind, including one the
// target cannot hold. The claim is no object: a target without the kind's
// capability still compares, and the name is left out of the comparison.
func TestComparisonLeavesAReservedNameOut_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	request, err := testcomparison.Request(nil, nil)
	c.Assert(err, qt.IsNil)
	request.Capabilities = make(map[string]bool)
	request.Current.Coverage.Subjects = []*protocol.ComparisonSubjectCoverage{unrepresentableSubject(schema.ReservedNameReason)}
	result, err := backend.New().CompareModels(c.Context(), request)
	c.Assert(err, qt.IsNil)
	c.Assert(result.Changes, qt.HasLen, 0)
	c.Assert(result.Undecided, qt.HasLen, 0)
}

// What the comparison still says: a declaration at a reserved name, and a
// name the read could not represent for another reason.
func TestComparisonLeavesAReservedNameOut_Undecided(t *testing.T) {
	c := testcheck.New(t)
	declared, err := testcomparison.Request(&schema.CoordinationIntent{}, nil)
	c.Assert(err, qt.IsNil)
	declared.Current.Coverage.Subjects = []*protocol.ComparisonSubjectCoverage{unrepresentableSubject(schema.ReservedNameReason)}
	unread, err := testcomparison.Request(nil, nil)
	c.Assert(err, qt.IsNil)
	unread.Current.Coverage.Subjects = []*protocol.ComparisonSubjectCoverage{unrepresentableSubject("Fixture source is incomplete")}
	for _, test := range []struct {
		name    string
		request *protocol.CompareModelsRequest
		want    string
	}{
		{"a declaration at a reserved name", declared, schema.ReservedNameReason},
		{"a name the read could not represent", unread, "The current coordination node or its absence was not established"},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			result, err := backend.New().CompareModels(c.Context(), test.request)
			c.Assert(err, qt.IsNil)
			c.Assert(result.Changes, qt.HasLen, 0)
			c.Assert(result.Undecided, qt.HasLen, 1)
			c.Assert(result.Undecided[0].Reason, qt.Equals, test.want)
		})
	}
}

// A declared object still needs the capability its kind requires.
func TestComparisonLeavesAReservedNameOut_FailurePath(t *testing.T) {
	c := testcheck.New(t)
	request, err := testcomparison.Request(&schema.CoordinationIntent{}, nil)
	c.Assert(err, qt.IsNil)
	request.Capabilities = make(map[string]bool)
	request.Current.Coverage.Subjects = []*protocol.ComparisonSubjectCoverage{unrepresentableSubject(schema.ReservedNameReason)}
	result, err := backend.New().CompareModels(c.Context(), request)
	c.Assert(err, qt.ErrorMatches, `.*coordination node capability was not established`)
	c.Assert(result, qt.IsNil)
}
