// Package migrationlint is the YDB provider's migration lint: the splitter
// that reads a YDB migration as YQL, the facts it reports about each
// statement, the annotation of a split the host made without a dialect, and
// the YD family of rules, published as data and checked once per lint run.
//
// It serves the SDK's lint.migrations service. The host owns output formats,
// severity thresholds, suppression and the rules every dialect shares; this
// package owns what only YDB's owner can know: where a YQL statement ends,
// what it does, and whether the server refuses it or runs it with an effect
// the statement does not state.
//
// Every answer follows the Ptah host's bundled YDB owner, dialect/ydb/ydblint
// and the readers it uses, and testdata/corpus holds the host's answers over a
// corpus of migration directories, which the tests compare with this
// package's finding for finding (see testdata/README.md).
//
// The rules that need to know what a table looks like before a statement read
// it from the directory itself, whether or not the run has a dev database: the
// up migrations before the analyzed version, in version order, then the
// statements of the file before the one analyzed. A table created outside the
// directory is unknown, and an unknown table reports nothing. The splitter
// reads the columns of a rebuilt table from the same history.
package migrationlint

import (
	"github.com/stokaro/ptah-ydb-plugin/internal/yqlddl"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
)

// Statement is one statement of a migration file, in the forms rules read.
type Statement struct {
	// Start and End are the statement's byte range in its source, from its
	// first token to its last; the terminating semicolon is not part of it.
	Start, End int
	// Line is the 1-based line of the statement's first token.
	Line int
	// SQL is the statement as written.
	SQL string
	// Canonical is the display form; see [canonical].
	Canonical string
	// Words and SourceWords are the token sequence rules scan, uppercased and
	// as written; see [words].
	Words, SourceWords []string
	// Comments are the comments directly above the statement.
	Comments []string
	// Effects are the facts the splitter reports about the statement.
	Effects []Effect
}

// Effect is one fact about what a statement does.
type Effect struct {
	Fact protocol.LintFact
	// Object is what the statement acts on, as written: the dropped object or
	// the unique index.
	Object string
	// Kind names what Object is in YQL's words, such as TOPIC.
	Kind string
	// Table and Columns are the table a unique index is built on and its key
	// columns, as written.
	Table   string
	Columns []string
	// Clause is the clause head that builds a unique index, and Failure the
	// server's words for the error the build fails with.
	Clause, Failure string
	// Impact and Reason are the judgment a safety effect carries, and no
	// other fact carries them.
	Impact protocol.LintImpact
	Reason string
	// Class is what a statement kind says the statement does to the schema,
	// and no other fact carries one.
	Class protocol.LintStatementClass
	// Capability is the key a required capability names, and no other fact
	// carries one.
	Capability string
}

// File is one migration file of a lint run.
type File struct {
	Name          string
	Version       int64
	Up, Down      bool
	HasPair       bool
	NoTransaction bool
	Repeatable    bool
	Statements    []Statement
}

// Target is what the server the run plans against can do: the capability map
// the request carries.
type Target struct {
	Capabilities map[string]bool
}

// Has reports a capability the target holds. A key the map does not answer is
// not held.
func (t Target) Has(key yqlddl.Capability) bool {
	return t.Capabilities[string(key)]
}

// Subject is one schema object a finding is about.
type Subject struct {
	Kind   protocol.LintSubject_Kind
	Name   string
	Parent string
}

// Violation is one violation a rule reports in a file: of the statement at a
// position, or of the file as a whole at -1.
type Violation struct {
	Statement int
	Message   string
	Subjects  []Subject
}

// Finding is one violation of a rule in a file of a run.
type Finding struct {
	Code      string
	File      int
	Statement int
	Message   string
	Subjects  []Subject
}

// checkedFile is one file of a check request as a rule reads it: the file, the
// target and the command surface the run reports on.
type checkedFile struct {
	*File
	Target Target
	// Compatibility is empty for Ptah's own surface and "atlas" for the
	// Atlas-compatible one.
	Compatibility string
}

// atlasSurface is the compatibility of the Atlas-compatible command surface.
const atlasSurface = "atlas"
