package lintcontract_test

import (
	"os"
	"strings"
	"testing"

	qt "github.com/frankban/quicktest"

	"github.com/stokaro/ptah-ydb-plugin/internal/lintcontract"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
)

// host is a host configuration with one entry of each kind Drift compares or
// leaves out.
const host = `version: "2"
run:
  timeout: 5m
linters:
  default: none
  enable:
    - depguard
    - gocognit
    - revive
    - rowserrcheck
  settings:
    depguard:
      rules:
        main:
          deny:
            - pkg: github.com/stretchr/testify
              desc: the host's words
    gocognit:
      min-complexity: 30
    revive:
      rules:
        - name: atomic
          disabled: false
        - name: flag-parameter
          disabled: false
    rowserrcheck:
      packages:
        - database/sql
        - ptah.run/internal/sqlrunner
  exclusions:
    generated: lax
    presets:
      - legacy
      - std-error-handling
    rules:
      - linters:
          - gocognit
        path: '_test\.go$'
      - text: 'var-naming: avoid meaningless package names'
        linters:
          - revive
    paths:
      - 'examples$'
issues:
  max-issues-per-linter: 0
formatters:
  enable:
    - gci
    - gofmt
  settings:
    gci:
      sections:
        - standard
        - default
        - prefix(ptah.run)
`

// edited is the host configuration with each old text of edits replaced by
// the new text that follows it.
func edited(c *qt.C, edits ...string) []byte {
	c.Helper()
	for i := 0; i+1 < len(edits); i += 2 {
		c.Assert(host, qt.Contains, edits[i])
	}
	return []byte(strings.NewReplacer(edits...).Replace(host))
}

// TestDrift_HappyPath holds a module that keeps the host's contract and
// differs from it only where a setting names one module.
func TestDrift_HappyPath(t *testing.T) {
	for _, test := range []struct {
		name  string
		edits []string
	}{
		{"the same file", nil},
		{"its own gci prefix", []string{"prefix(ptah.run)", "prefix(github.com/stokaro/ptah-ydb-plugin)"}},
		{"its own rowserrcheck packages", []string{"- ptah.run/internal/sqlrunner", "- github.com/stokaro/ptah-ydb-plugin/internal/backend"}},
		{"its own depguard words", []string{"desc: the host's words", "desc: the module's words"}},
		{"its own path exclusion", []string{`path: '_test\.go$'`, `path: '(cmd|integration)/.*\.go$'`}},
		{"its own excluded paths", []string{"- 'examples$'", "- 'third_party$'"}},
		{"its own run section", []string{"timeout: 5m", "timeout: 30m"}},
		{"the revive rules in another order", []string{
			"        - name: atomic\n          disabled: false\n        - name: flag-parameter\n          disabled: false\n",
			"        - name: flag-parameter\n          disabled: false\n        - name: atomic\n          disabled: false\n",
		}},
		{"a linter beyond the host's, with settings", []string{
			"    - rowserrcheck\n  settings:\n",
			"    - rowserrcheck\n    - misspell\n  settings:\n    misspell:\n      locale: US\n",
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			drift, err := lintcontract.Drift([]byte(host), edited(c, test.edits...))
			c.Assert(err, qt.IsNil)
			c.Assert(drift, qt.HasLen, 0)
		})
	}
}

// TestDrift_FailurePath names each departure from the host's contract.
func TestDrift_FailurePath(t *testing.T) {
	for _, test := range []struct {
		name  string
		edits []string
		want  string
	}{
		{"a linter the host enables, left out", []string{"    - depguard\n    - gocognit\n", "    - depguard\n"},
			`linters.enable: the host enables gocognit, and this module does not`},
		{"a raised threshold", []string{"min-complexity: 30", "min-complexity: 40"},
			`linters.settings.gocognit.min-complexity: the host has 30, and this module has 40`},
		{"a setting the host does not make", []string{"      min-complexity: 30\n", "      min-complexity: 30\n      ignore-tests: true\n"},
			`linters.settings.gocognit.ignore-tests: the host has null, and this module has true`},
		{"a host setting left out", []string{"    gocognit:\n      min-complexity: 30\n", ""},
			`linters.settings.gocognit: the host has {"min-complexity":30}, and this module has null`},
		{"a revive rule turned off", []string{"- name: flag-parameter\n          disabled: false", "- name: flag-parameter\n          disabled: true"},
			`linters.settings.revive.rules.flag-parameter.disabled: the host has false, and this module has true`},
		{"a denied package left out", []string{"            - pkg: github.com/stretchr/testify\n              desc: the host's words\n", "            - pkg: io/ioutil\n"},
			`linters.settings.depguard.rules.main.deny: the host has [{"pkg":"github.com/stretchr/testify"}], and this module has [{"pkg":"io/ioutil"}]`},
		{"an exclusion of the module's own", []string{"    paths:\n", "      - text: 'flag-parameter:'\n        linters:\n          - revive\n    paths:\n"},
			`linters.exclusions.rules: this module excludes {"linters":["revive"],"text":"flag-parameter:"}, and the host does not`},
		{"a host exclusion left out", []string{"      - text: 'var-naming: avoid meaningless package names'\n        linters:\n          - revive\n", ""},
			`linters.exclusions.rules: the host excludes {"linters":["revive"],"text":"var-naming: avoid meaningless package names"}, and this module does not`},
		{"another preset", []string{"      - legacy\n", "      - comments\n"},
			`linters.exclusions.presets: the host has ["legacy","std-error-handling"], and this module has ["comments","std-error-handling"]`},
		{"another default set", []string{"default: none", "default: standard"},
			`linters.default: the host has "none", and this module has "standard"`},
		{"a formatter the host enables, left out", []string{"    - gci\n", ""},
			`formatters.enable: the host enables gci, and this module does not`},
		{"another gci section", []string{"        - default\n", "        - blank\n"},
			`formatters.settings.gci.sections: the host has ["standard","default"], and this module has ["standard","blank"]`},
		{"another issue limit", []string{"max-issues-per-linter: 0", "max-issues-per-linter: 50"},
			`issues.max-issues-per-linter: the host has 0, and this module has 50`},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			drift, err := lintcontract.Drift([]byte(host), edited(c, test.edits...))
			c.Assert(err, qt.IsNil)
			c.Assert(drift, qt.DeepEquals, []string{test.want})
		})
	}
}

// TestDrift_RefusesUnreadableConfiguration refuses a configuration that is
// not a YAML mapping, naming the side it read.
func TestDrift_RefusesUnreadableConfiguration(t *testing.T) {
	for _, test := range []struct {
		name         string
		host, module string
		wantErr      string
	}{
		{"a malformed host", "linters: [", host, `read the host configuration: .*`},
		{"a malformed module", host, "linters: [", `read the module configuration: .*`},
		{"an empty module", host, "", `the module configuration is empty`},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			drift, err := lintcontract.Drift([]byte(test.host), []byte(test.module))
			c.Assert(err, qt.ErrorMatches, test.wantErr)
			c.Assert(drift, qt.IsNil)
		})
	}
}

// TestDrift_ReadsTheModuleConfiguration reads this module's .golangci.yml,
// which the CI job compares with the host's.
func TestDrift_ReadsTheModuleConfiguration(t *testing.T) {
	c := testcheck.New(t)
	config, err := os.ReadFile("../../.golangci.yml")
	c.Assert(err, qt.IsNil)
	drift, err := lintcontract.Drift(config, config)
	c.Assert(err, qt.IsNil)
	c.Assert(drift, qt.HasLen, 0)
}
