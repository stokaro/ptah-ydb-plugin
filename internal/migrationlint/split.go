package migrationlint

import (
	"context"
	"slices"
	"strings"

	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
)

// Source is one migration file's text as a host hands it to the splitter.
type Source struct {
	Name       string
	SQL        string
	Version    int64
	Up, Down   bool
	Repeatable bool
}

// judged are the facts [Split] reports for every statement it returns.
var judged = []protocol.LintFact{
	protocol.LintFact_LINT_FACT_STATEFUL_DROP, protocol.LintFact_LINT_FACT_UNIQUE_INDEX,
	protocol.LintFact_LINT_FACT_REBUILD_DROP, protocol.LintFact_LINT_FACT_REBUILD_RENAME,
	protocol.LintFact_LINT_FACT_SAFETY_EFFECT,
}

// Judged returns the facts [Split] judges for every statement, with the
// statement facts where it describes statements.
func Judged(describeStatements bool) []protocol.LintFact {
	if describeStatements {
		return slices.Concat(judged, statementFacts)
	}
	return slices.Clone(judged)
}

// StatementFacts returns the facts that say what a statement is, which table
// it creates without a primary key and which capabilities it needs. They are
// contract 25.1, and only a host that lists lint.statement-facts reads them.
func StatementFacts() []protocol.LintFact {
	return slices.Clone(statementFacts)
}

// Split splits every source as YQL, in one call, and reports each statement's
// effects: a drop that loses state, YQL's unique index, the steps of a table
// rebuild, whose old table's columns it reads from the directory's own
// history, and its judgment of a statement whose effect only this owner
// knows. Where describeStatements holds, every statement also carries its
// statement facts (see [describe]). Cancellation returns the context's error
// and no statement.
func Split(ctx context.Context, sources []Source, describeStatements bool) ([][]Statement, error) {
	files := make([]File, 0, len(sources))
	for _, source := range sources {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		files = append(files, File{Name: source.Name, Version: source.Version, Up: source.Up, Down: source.Down,
			Repeatable: source.Repeatable, Statements: split(source.SQL)})
	}
	h := newHistory(files)
	result := make([][]Statement, 0, len(files))
	for i := range files {
		statements := files[i].Statements
		steps := rebuilds(statements, h.columns(files[i].Name, statements))
		for j := range statements {
			stmt := &statements[j]
			if effect, dropped := statefulDrop(stmt); dropped {
				stmt.Effects = append(stmt.Effects, effect)
			}
			stmt.Effects = append(stmt.Effects, uniqueIndexes(stmt)...)
			if effect, judged := safetyEffect(stmt); judged {
				stmt.Effects = append(stmt.Effects, effect)
			}
			if steps.drops[j] {
				stmt.Effects = append(stmt.Effects, Effect{Fact: protocol.LintFact_LINT_FACT_REBUILD_DROP})
			}
			if steps.renames[j] {
				stmt.Effects = append(stmt.Effects, Effect{Fact: protocol.LintFact_LINT_FACT_REBUILD_RENAME})
			}
			if describeStatements {
				stmt.Effects = append(stmt.Effects, describe(stmt)...)
			}
		}
		result = append(result, statements)
	}
	return result, ctx.Err()
}

// annotated are the facts [Annotate] judges, in the order it reports them.
var annotated = []protocol.LintFact{protocol.LintFact_LINT_FACT_STATEFUL_DROP, protocol.LintFact_LINT_FACT_SAFETY_EFFECT}

// Annotation is the answer about one statement another splitter produced.
type Annotation struct {
	Effects []Effect
	// Declined reports a statement this owner does not judge. It carries no
	// effect.
	Declined bool
}

// Annotate judges a drop that loses state, a statement whose effect only this
// owner knows, and the statement facts, over the split of a run that names no
// dialect. It splits each source again as YQL and judges a statement YQL
// splits the same way, from its YQL reading. It declines one whose two
// readings disagree about a fact it was asked: YQL splits it differently where
// either reading holds one, or splits it the same way and reads it otherwise.
// The dialect-blind scanner ends a string or a comment where YQL does not, and
// which reading the author meant is not this owner's to guess. Every statement
// has a kind, so where the kind is asked, a statement YQL splits differently
// is declined. It judges no other fact.
func Annotate(ctx context.Context, facts []protocol.LintFact, sources []Source, files [][]Statement) ([][]Annotation, error) {
	asked := slices.DeleteFunc(slices.Concat(annotated, statementFacts), func(fact protocol.LintFact) bool { return !slices.Contains(facts, fact) })
	result := make([][]Annotation, len(files))
	for i, statements := range files {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		result[i] = make([]Annotation, len(statements))
		if !mayHold(sources[i].SQL, asked) {
			continue
		}
		yql := split(sources[i].SQL)
		for j := range statements {
			result[i][j] = annotate(&statements[j], yql, asked)
		}
	}
	return result, ctx.Err()
}

// mayHold reports whether a source may hold a statement with one of the asked
// facts. A statement fact may belong to any statement. A statement with one of
// the other facts starts with, or holds, one of the words the fact lists, so a
// source that holds none of them in any letter case holds no such statement
// however it is lexed.
func mayHold(sql string, asked []protocol.LintFact) bool {
	if slices.ContainsFunc(asked, func(fact protocol.LintFact) bool { return slices.Contains(statementFacts, fact) }) {
		return true
	}
	upper := strings.ToUpper(sql)
	for _, fact := range asked {
		words := []string{"DROP"}
		if fact == protocol.LintFact_LINT_FACT_SAFETY_EFFECT {
			words = append(words, "ALTER", "REPLACE")
		}
		if slices.ContainsFunc(words, func(word string) bool { return strings.Contains(upper, word) }) {
			return true
		}
	}
	return false
}

// annotate judges one statement the dialect-blind scanner split, against the
// statements YQL splits its source into.
func annotate(stmt *Statement, yql []Statement, asked []protocol.LintFact) Annotation {
	var overlapping []Statement
	for _, piece := range yql {
		if piece.Start < stmt.End && stmt.Start < piece.End {
			overlapping = append(overlapping, piece)
		}
	}
	blind := read(stmt, asked)
	if len(overlapping) == 1 && overlapping[0].Start == stmt.Start && overlapping[0].End == stmt.End {
		own := read(&overlapping[0], asked)
		if !slices.EqualFunc(blind, own, func(a, b Effect) bool { return a.Fact == b.Fact && a.Impact == b.Impact }) {
			return Annotation{Declined: true}
		}
		return Annotation{Effects: own}
	}
	if len(blind) == 0 && !slices.ContainsFunc(overlapping, func(piece Statement) bool { return len(read(&piece, asked)) > 0 }) {
		return Annotation{}
	}
	return Annotation{Declined: true}
}

// read returns the statement's effects of the asked facts, in their order,
// and then its statement facts that were asked, in the order [describe] gives
// them.
func read(stmt *Statement, asked []protocol.LintFact) []Effect {
	var effects []Effect
	for _, fact := range asked {
		reader := statefulDrop
		switch {
		case slices.Contains(statementFacts, fact):
			continue
		case fact == protocol.LintFact_LINT_FACT_SAFETY_EFFECT:
			reader = safetyEffect
		}
		if effect, found := reader(stmt); found {
			effects = append(effects, effect)
		}
	}
	if slices.ContainsFunc(asked, func(fact protocol.LintFact) bool { return slices.Contains(statementFacts, fact) }) {
		for _, effect := range describe(stmt) {
			if slices.Contains(asked, effect.Fact) {
				effects = append(effects, effect)
			}
		}
	}
	return effects
}
