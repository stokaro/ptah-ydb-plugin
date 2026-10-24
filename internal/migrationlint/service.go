package migrationlint

import (
	"context"
	"slices"

	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
)

// The functions in this file answer the SDK's lint.migrations requests. The
// SDK validates a request before it arrives and a reply before it leaves.

// SplitRequest answers a SplitLintRequest: one file per source, in request
// order, and the facts every statement was judged for. describeStatements
// adds the statement facts, which only a host that lists
// lint.statement-facts reads.
func SplitRequest(ctx context.Context, request *protocol.SplitLintRequest, describeStatements bool) (*protocol.SplitLint, error) {
	sources := sourcesOf(request.GetSources())
	files, err := Split(ctx, sources, describeStatements)
	if err != nil {
		return nil, err
	}
	result := &protocol.SplitLint{Judged: Judged(describeStatements)}
	for i, statements := range files {
		source := request.GetSources()[i]
		result.Files = append(result.Files, &protocol.LintFile{Name: source.GetName(), Version: source.GetVersion(), Up: source.GetUp(),
			Down: source.GetDown(), Repeatable: source.GetRepeatable(), Statements: wireStatements(statements)})
	}
	return result, nil
}

// AnnotateRequest answers an AnnotateLintRequest: one annotation per
// statement the request holds.
func AnnotateRequest(ctx context.Context, request *protocol.AnnotateLintRequest) (*protocol.AnnotatedLint, error) {
	files := make([][]Statement, 0, len(request.GetFiles()))
	for _, file := range request.GetFiles() {
		files = append(files, statementsOf(file.GetStatements()))
	}
	annotations, err := Annotate(ctx, request.GetFacts(), sourcesOf(request.GetSources()), files)
	if err != nil {
		return nil, err
	}
	result := &protocol.AnnotatedLint{}
	for _, file := range annotations {
		answer := &protocol.LintAnnotations{}
		for _, annotation := range file {
			answer.Statements = append(answer.Statements, &protocol.LintAnnotation{Effects: wireEffects(annotation.Effects), Declined: annotation.Declined})
		}
		result.Files = append(result.Files, answer)
	}
	return result, nil
}

// CheckRequest answers a CheckLintRequest with every finding of the rules it
// enables over every file it holds.
func CheckRequest(ctx context.Context, request *protocol.CheckLintRequest) (*protocol.CheckedLint, error) {
	files := make([]File, 0, len(request.GetFiles()))
	for _, file := range request.GetFiles() {
		files = append(files, File{Name: file.GetName(), Version: file.GetVersion(), Up: file.GetUp(), Down: file.GetDown(), HasPair: file.GetHasPair(),
			NoTransaction: file.GetNoTransaction(), Repeatable: file.GetRepeatable(), Statements: statementsOf(file.GetStatements())})
	}
	findings, err := Check(ctx, Target{Capabilities: request.GetCapabilities()}, request.GetCompatibility(), request.GetCodes(), files)
	if err != nil {
		return nil, err
	}
	result := &protocol.CheckedLint{Complete: true}
	for _, finding := range findings {
		wire := &protocol.LintFinding{Code: finding.Code, File: int32(finding.File), Statement: int32(finding.Statement), Message: finding.Message} // #nosec G115 -- positions index the request's own lists
		for _, subject := range finding.Subjects {
			wire.Subjects = append(wire.Subjects, &protocol.LintSubject{Kind: subject.Kind, Name: subject.Name, Parent: subject.Parent})
		}
		result.Findings = append(result.Findings, wire)
	}
	return result, nil
}

func sourcesOf(sources []*protocol.LintSource) []Source {
	result := make([]Source, 0, len(sources))
	for _, source := range sources {
		result = append(result, Source{Name: source.GetName(), SQL: source.GetSql(), Version: source.GetVersion(), Up: source.GetUp(),
			Down: source.GetDown(), Repeatable: source.GetRepeatable()})
	}
	return result
}

func statementsOf(statements []*protocol.LintStatement) []Statement {
	result := make([]Statement, 0, len(statements))
	for _, statement := range statements {
		value := Statement{Start: int(statement.GetStart()), End: int(statement.GetEnd()), Line: int(statement.GetLine()), SQL: statement.GetSql(),
			Canonical: statement.GetCanonical(), Words: slices.Clone(statement.GetWords()), SourceWords: slices.Clone(statement.GetSourceWords()),
			Comments: slices.Clone(statement.GetComments())}
		for _, effect := range statement.GetEffects() {
			value.Effects = append(value.Effects, Effect{Fact: effect.GetFact(), Object: effect.GetObject(), Kind: effect.GetKind(), Table: effect.GetTable(),
				Columns: slices.Clone(effect.GetColumns()), Clause: effect.GetClause(), Failure: effect.GetFailure(), Impact: effect.GetImpact(), Reason: effect.GetReason(),
				Class: effect.GetStatementClass(), Capability: effect.GetCapability()})
		}
		result = append(result, value)
	}
	return result
}

func wireStatements(statements []Statement) []*protocol.LintStatement {
	result := make([]*protocol.LintStatement, 0, len(statements))
	for _, statement := range statements {
		result = append(result, &protocol.LintStatement{
			Start: uint32(statement.Start), End: uint32(statement.End), Line: uint32(statement.Line), // #nosec G115 -- a source the SDK accepted is far below 4 GiB
			Sql: statement.SQL, Canonical: statement.Canonical, Words: slices.Clone(statement.Words), SourceWords: slices.Clone(statement.SourceWords),
			Comments: slices.Clone(statement.Comments), Effects: wireEffects(statement.Effects),
		})
	}
	return result
}

func wireEffects(effects []Effect) []*protocol.LintEffect {
	var result []*protocol.LintEffect
	for _, effect := range effects {
		result = append(result, &protocol.LintEffect{Fact: effect.Fact, Object: effect.Object, Kind: effect.Kind, Table: effect.Table,
			Columns: slices.Clone(effect.Columns), Clause: effect.Clause, Failure: effect.Failure, Impact: effect.Impact, Reason: effect.Reason,
			StatementClass: effect.Class, Capability: effect.Capability})
	}
	return result
}
