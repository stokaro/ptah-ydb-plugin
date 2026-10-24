package migrationlint

import (
	"cmp"
	"context"
	"fmt"
	"slices"

	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
)

// ydbEngine is the engine the rules judge, as a message names it.
const ydbEngine = "ydb"

// published is what the provider publishes of a rule.
type published struct {
	Code     string
	Title    string
	Severity protocol.LintRule_Severity
	// AppliesToDown extends a statement rule to the down half of a migration.
	AppliesToDown bool
	// Subsumes names rules whose finding on the same statement this rule's
	// finding replaces.
	Subsumes []string
}

// rule is one rule and the Go function that checks it. Exactly one of
// Statement and File is set.
type rule struct {
	published
	// Statement reports whether a statement violates the rule, and the
	// message. It judges every statement of an up file, and of a down file
	// where the rule applies to down.
	Statement func(file checkedFile, stmt *Statement) (bool, string)
	// File reports the violations of one file. It decides for itself which
	// files it judges.
	File func(file checkedFile) []Violation
}

// rules are the provider's rules in code order.
func rules() []rule {
	all := append(statementRules(), readerRules()...)
	slices.SortFunc(all, func(a, b rule) int { return cmp.Compare(a.Code, b.Code) })
	return all
}

// Rules returns the rule catalog the provider publishes, in code order. Each
// call returns fresh values.
func Rules() []*protocol.LintRule {
	all := rules()
	result := make([]*protocol.LintRule, 0, len(all))
	for _, rule := range all {
		result = append(result, &protocol.LintRule{Code: rule.Code, Title: rule.Title, Severity: rule.Severity,
			AppliesToDown: rule.AppliesToDown, Subsumes: slices.Clone(rule.Subsumes)})
	}
	return result
}

// Check runs the rules codes enables over files and returns their findings:
// for each file in order, each code's findings in the order codes lists them,
// a statement rule's in statement order. A code the provider does not publish
// is refused, and cancellation returns the context's error; either returns no
// finding.
func Check(ctx context.Context, target Target, compatibility string, codes []string, files []File) ([]Finding, error) {
	all := rules()
	enabled := make([]rule, 0, len(codes))
	for _, code := range codes {
		position := slices.IndexFunc(all, func(rule rule) bool { return rule.Code == code })
		if position < 0 {
			return nil, fmt.Errorf("no YDB lint rule %s", code)
		}
		enabled = append(enabled, all[position])
	}
	var findings []Finding
	for index := range files {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		file := checkedFile{File: &files[index], Target: target, Compatibility: compatibility}
		for _, rule := range enabled {
			findings = append(findings, rule.findings(file, index)...)
		}
	}
	return findings, ctx.Err()
}

// findings is one rule over one file, the file at index of its request.
func (r rule) findings(file checkedFile, index int) []Finding {
	var findings []Finding
	if r.File != nil {
		for _, violation := range r.File(file) {
			findings = append(findings, Finding{Code: r.Code, File: index, Statement: violation.Statement,
				Message: violation.Message, Subjects: slices.Clone(violation.Subjects)})
		}
		return findings
	}
	if !file.Up && (!file.Down || !r.AppliesToDown) {
		return nil
	}
	for position := range file.Statements {
		if found, message := r.Statement(file, &file.Statements[position]); found {
			findings = append(findings, Finding{Code: r.Code, File: index, Statement: position, Message: message})
		}
	}
	return findings
}
