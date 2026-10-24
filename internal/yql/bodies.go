package yql

// Bodies tracks the YQL constructs whose body holds statements:
//
//   - DEFINE ACTION and DEFINE SUBQUERY, closed by END DEFINE;
//   - DO BEGIN, the inline action that EVALUATE FOR, EVALUATE IF, a plain DO
//     and CREATE VIEW ... AS DO all write, closed by END DO;
//   - a lambda body, ($x) -> { ...; RETURN ...; }, between braces.
//
// The grammar is SQLv1Antlr4.g.in: define_action_or_subquery_stmt,
// inline_action and lambda. Each keyword pair has to be adjacent words:
// DEFINE alone opens MATCH_RECOGNIZE's pattern definitions, and BEGIN and DO
// are ordinary names elsewhere. END alone closes a CASE, and a CASE may end
// right before the DO of an IF, as in EVALUATE IF CASE ... END DO BEGIN, so
// CASE expressions are counted and their END pairs with nothing. Measured on
// YDB 26.2.1.14, CASE and END are also names after AS and after a dot
// (SELECT 1 AS case; SELECT t.case FROM ...), where they count for nothing.
type Bodies struct {
	blocks   int
	braces   int
	cases    int
	previous string
	// named records that the last token was AS or a dot, after which a word
	// is a name.
	named bool
}

// Word records one identifier, upper-cased by the caller.
func (b *Bodies) Word(word string) {
	previous, named := b.previous, b.named
	b.previous, b.named = word, word == "AS"
	if named && (word == "CASE" || word == "END") {
		b.previous = ""
		return
	}
	switch {
	case word == "CASE":
		b.cases++
	case word == "END" && b.cases > 0:
		b.cases--
		b.previous = ""
	case previous == "DEFINE" && (word == "ACTION" || word == "SUBQUERY"):
		b.blocks++
	case previous == "DO" && word == "BEGIN":
		b.blocks++
	case previous == "END" && (word == "DEFINE" || word == "DO") && b.blocks > 0:
		b.blocks--
	}
}

// Separate records a semicolon, which ends a statement inside a body, so the
// words on either side of it pair with nothing.
func (b *Bodies) Separate() {
	b.previous = ""
	b.named = false
}

// Symbol records punctuation. It separates the words on either side of it as
// a semicolon does: measured on YDB 26.2.1.14, SELECT do, begin FROM ... is
// two columns, and SELECT end, define FROM ... inside an action closes
// nothing.
func (b *Bodies) Symbol(value string) {
	b.previous = ""
	b.named = value == "."
	switch value {
	case "{":
		b.braces++
	case "}":
		if b.braces > 0 {
			b.braces--
		}
	}
}

// Inside reports that a semicolon read now ends a statement of a body rather
// than the statement that holds the body.
func (b *Bodies) Inside() bool {
	return b.blocks > 0 || b.braces > 0
}
