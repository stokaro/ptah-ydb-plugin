package testcheck_test

import (
	"fmt"
	"testing"

	qt "github.com/frankban/quicktest"
	protocol "ptah.run/provider-sdk/protocol/experimental"

	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
)

// The patterns allow any run of spaces after a field name: prototext adds a
// random one so that nothing depends on its output byte for byte.
func TestFormat_HappyPath(t *testing.T) {
	undecided := &protocol.ComparisonUndecided{Kind: "external-table", Reason: "absence was not established"}
	for _, test := range []struct {
		name  string
		value any
		want  string
	}{
		{"a message", undecided, `\*protocol\.ComparisonUndecided\{\n    kind: +"external-table"\n    reason: +"absence was not established"\n\}`},
		{"a slice of messages", []*protocol.ComparisonUndecided{undecided},
			`\[\]\*protocol\.ComparisonUndecided\{\n    \*protocol\.ComparisonUndecided\{\n        kind: +"external-table"\n        reason: +"absence was not established"\n    \},\n\}`},
		{"an empty slice of messages", make([]*protocol.ComparisonUndecided, 0), `\[\]\*protocol\.ComparisonUndecided\{\n\}`},
		{"a nil message in a slice", []*protocol.ComparisonUndecided{nil}, `\[\]\*protocol\.ComparisonUndecided\{\n    nil,\n\}`},
		{"a value that is no message", []int{1, 2}, `\[\]int\{1, 2\}`},
		{"a string", "text", `"text"`},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			c.Assert(testcheck.Format(test.value), qt.Matches, test.want)
		})
	}
}

// A failed check made through a checker New built reports the value as
// protobuf text rather than the message's reflection state.
func TestNew_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	recorded := &recorder{}
	checker := testcheck.New(recorded)
	checker.Check([]*protocol.ComparisonUndecided{{Kind: "external-table"}}, qt.HasLen, 0)
	c.Assert(recorded.reports, qt.HasLen, 1)
	c.Assert(recorded.reports[0], qt.Matches, `(?s).*kind: +"external-table".*`)
	c.Assert(recorded.reports[0], qt.Not(qt.Contains), "MessageState")
}

// recorder is a testing.TB that keeps what a check reports.
type recorder struct {
	testing.TB
	reports []string
}

func (r *recorder) Helper()             {}
func (r *recorder) Cleanup(func())      {}
func (r *recorder) Name() string        { return "recorder" }
func (r *recorder) Error(args ...any)   { r.reports = append(r.reports, fmt.Sprint(args...)) }
func (r *recorder) Fatal(args ...any)   { r.reports = append(r.reports, fmt.Sprint(args...)) }
func (r *recorder) Log(args ...any)     {}
func (r *recorder) Logf(string, ...any) {}
