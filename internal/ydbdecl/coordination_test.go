package ydbdecl_test

import (
	"regexp"
	"testing"

	qt "github.com/frankban/quicktest"

	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
	"github.com/stokaro/ptah-ydb-plugin/internal/ydbdecl"
)

// A YAML, a Go, an HCL and a YQL declaration of a coordination node read their
// settings through ParseCoordinationNode, and a refusal says what the setting
// takes: an ISO 8601 duration, a period longer than zero in whole milliseconds
// that fits the node's field, a mode YDB takes, and settings the node would run
// as declared. Ported from the host's ydbcoordination TestParseDeclaration_FailurePath.
func TestParseCoordinationNode_FailurePath(t *testing.T) {
	tests := []struct {
		name    string
		values  map[string]string
		wantErr string
	}{
		{name: "not a duration", values: map[string]string{"self_check_period": "1s"},
			wantErr: `self_check_period "1s": a period is an ISO 8601 duration such as PT1S or PT0.5S`},
		{name: "a negative duration", values: map[string]string{"session_grace_period": "-PT5S"},
			wantErr: `session_grace_period "-PT5S": a period is longer than zero`},
		{name: "a zero duration", values: map[string]string{"self_check_period": "PT0S"},
			wantErr: `self_check_period "PT0S": a period is longer than zero`},
		{name: "a fraction of a millisecond", values: map[string]string{"self_check_period": "PT0.5005S"},
			wantErr: `self_check_period "PT0.5005S": the node keeps a period in whole milliseconds`},
		{name: "a period too long for the node", values: map[string]string{"session_grace_period": "P50D"},
			wantErr: `session_grace_period "P50D": the node keeps a period of at most 4294967295 milliseconds`},
		{name: "an empty mode", values: map[string]string{"attach_consistency_mode": " "},
			wantErr: `attach_consistency_mode is empty: leave the setting out to keep the node's default`},
		{name: "an unknown mode", values: map[string]string{"rate_limiter_counters_mode": "per-resource"},
			wantErr: `rate_limiter_counters_mode "per-resource": the mode is "aggregated" or "detailed"`},
		{name: "a combination the node would not run", values: map[string]string{"self_check_period": "PT10S"},
			wantErr: `session_grace_period PT10S: YDB runs a node with a grace period from the self-check period plus PT1S (PT11S here) to PT30S ` +
				`and moves a period outside that range into it`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)

			got, err := ydbdecl.ParseCoordinationNode(test.values)

			c.Assert(err, qt.ErrorMatches, regexp.QuoteMeta(test.wantErr))
			c.Assert(got, qt.Equals, ydbdecl.CoordinationNode{})
		})
	}
}

// The node runs each of these as declared: the bounds YDB keeps a period in,
// with the grace period at least PT1S above the self-check period, and every
// mode. Ported from the host's ydbcoordination TestValidate_HappyPath.
func TestValidateCoordinationNode_HappyPath(t *testing.T) {
	tests := []struct {
		name string
		spec ydbdecl.CoordinationNode
	}{
		{name: "nothing declared"},
		{name: "the shortest self-check period", spec: ydbdecl.CoordinationNode{SelfCheckPeriodMillis: 500}},
		{name: "the longest self-check period with a grace period above it",
			spec: ydbdecl.CoordinationNode{SelfCheckPeriodMillis: 10000, SessionGracePeriodMillis: 11000}},
		{name: "the shortest grace period after the default self-check", spec: ydbdecl.CoordinationNode{SessionGracePeriodMillis: 2000}},
		{name: "the longest grace period", spec: ydbdecl.CoordinationNode{SessionGracePeriodMillis: 30000}},
		{name: "every mode", spec: ydbdecl.CoordinationNode{ReadConsistencyMode: "strict", AttachConsistencyMode: "relaxed", RateLimiterCountersMode: "detailed"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			c.Assert(ydbdecl.ValidateCoordinationNode(test.spec), qt.IsNil)
		})
	}
}

// A period YDB would move into its range, and a mode it does not take, are
// refused with what the node takes. Ported from the host's ydbcoordination
// TestValidate_FailurePath.
func TestValidateCoordinationNode_FailurePath(t *testing.T) {
	tests := []struct {
		name    string
		spec    ydbdecl.CoordinationNode
		wantErr string
	}{
		{name: "a self-check period the node raises", spec: ydbdecl.CoordinationNode{SelfCheckPeriodMillis: 499},
			wantErr: `self_check_period PT0.499S: YDB runs a node's self-check every PT0.5S to PT10S and moves a period outside that range into it`},
		{name: "a self-check period the node lowers", spec: ydbdecl.CoordinationNode{SelfCheckPeriodMillis: 10001, SessionGracePeriodMillis: 30000},
			wantErr: `self_check_period PT10.001S: YDB runs a node's self-check every PT0.5S to PT10S and moves a period outside that range into it`},
		{name: "a self-check period that leaves the default grace period too short", spec: ydbdecl.CoordinationNode{SelfCheckPeriodMillis: 10000},
			wantErr: `session_grace_period PT10S: YDB runs a node with a grace period from the self-check period plus PT1S (PT11S here) to PT30S ` +
				`and moves a period outside that range into it`},
		{name: "a grace period too close to the self-check period", spec: ydbdecl.CoordinationNode{SelfCheckPeriodMillis: 2000, SessionGracePeriodMillis: 2999},
			wantErr: `session_grace_period PT2.999S: YDB runs a node with a grace period from the self-check period plus PT1S (PT3S here) to PT30S ` +
				`and moves a period outside that range into it`},
		{name: "a grace period the node lowers", spec: ydbdecl.CoordinationNode{SessionGracePeriodMillis: 30001},
			wantErr: `session_grace_period PT30.001S: YDB runs a node with a grace period from the self-check period plus PT1S (PT2S here) to PT30S ` +
				`and moves a period outside that range into it`},
		{name: "an unknown read mode", spec: ydbdecl.CoordinationNode{ReadConsistencyMode: "eventual"},
			wantErr: `read_consistency_mode "eventual": the mode is "strict" or "relaxed"`},
		{name: "an unknown attach mode", spec: ydbdecl.CoordinationNode{AttachConsistencyMode: "STRICT"},
			wantErr: `attach_consistency_mode "STRICT": the mode is "strict" or "relaxed"`},
		{name: "an unknown counters mode", spec: ydbdecl.CoordinationNode{RateLimiterCountersMode: "none"},
			wantErr: `rate_limiter_counters_mode "none": the mode is "aggregated" or "detailed"`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			c.Assert(ydbdecl.ValidateCoordinationNode(test.spec), qt.ErrorMatches, regexp.QuoteMeta(test.wantErr))
		})
	}
}
