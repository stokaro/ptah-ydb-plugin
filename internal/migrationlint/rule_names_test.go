package migrationlint_test

import (
	"testing"

	qt "github.com/frankban/quicktest"
	"ptah.run/provider-sdk/provider"

	"github.com/stokaro/ptah-ydb-plugin/internal/migrationlint"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
)

// TestRules_AreNamedByTheSDKGrammar_HappyPath holds every published rule, and
// every rule it subsumes, to the grammar a contract 26 host accepts.
func TestRules_AreNamedByTheSDKGrammar_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	rules := migrationlint.Rules()
	c.Assert(len(rules) > 0, qt.IsTrue)
	for _, rule := range rules {
		c.Assert(provider.ValidLintRuleName(rule.GetCode()), qt.IsTrue, qt.Commentf("rule %q", rule.GetCode()))
		for _, subsumed := range rule.GetSubsumes() {
			c.Assert(provider.ValidLintRuleName(subsumed), qt.IsTrue, qt.Commentf("rule %q subsumes %q", rule.GetCode(), subsumed))
		}
	}
}

// TestRules_AreNamedByTheSDKGrammar_FailurePath is the control: the grammar
// refuses the spelling the provider published on contract 25, so the happy-path
// test cannot pass because the check accepts anything.
func TestRules_AreNamedByTheSDKGrammar_FailurePath(t *testing.T) {
	for _, name := range []string{"YD101", "MF101", "ydb", "ydb/", "/add-column-form", "Ydb/add-column-form", "ydb/add_column_form"} {
		t.Run(name, func(t *testing.T) {
			c := testcheck.New(t)
			c.Assert(provider.ValidLintRuleName(name), qt.IsFalse)
		})
	}
}
