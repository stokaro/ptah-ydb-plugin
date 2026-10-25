// Package coordinationsql writes and reads the statements Ptah keeps a YDB
// coordination node with.
//
// YQL has no coordination node statement. Ptah writes `CREATE COORDINATION
// NODE`, `ALTER COORDINATION NODE ... SET` and `DROP COORDINATION NODE` into a
// migration file, and its YDB connection runs each through the coordination
// service. [Statement.Text] writes them as the host's embedded engine writes
// them, and [Recognize] reads them as its connection does, so a migration file
// either one wrote runs the same way through this provider.
package coordinationsql

import (
	"errors"
	"fmt"
	"path"
	"slices"
	"strings"

	"github.com/stokaro/ptah-ydb-plugin/internal/servicequery"
	"github.com/stokaro/ptah-ydb-plugin/internal/ydbdecl"
	"github.com/stokaro/ptah-ydb-plugin/internal/ydbtype"
	"github.com/stokaro/ptah-ydb-plugin/internal/yql"
)

// Verb is what a statement does to a node.
type Verb int

// The three statements Ptah writes for a coordination node.
const (
	// Create creates a node: `CREATE COORDINATION NODE path [WITH (...)]`.
	Create Verb = iota + 1
	// Alter changes the settings it names: `ALTER COORDINATION NODE path SET
	// (...)`.
	Alter
	// Drop drops a node: `DROP COORDINATION NODE path`.
	Drop
)

// keyword is the verb's first word.
func (v Verb) keyword() string {
	switch v {
	case Create:
		return "CREATE"
	case Alter:
		return "ALTER"
	case Drop:
		return "DROP"
	default:
		return ""
	}
}

// Statement is one of Ptah's coordination node statements.
type Statement struct {
	Verb Verb
	// Path is the node's path as the statement names it: relative to the
	// TablePathPrefix in effect, or to the database root where none is, or
	// absolute where it starts with a slash.
	Path string
	// Settings is the configuration a CREATE gives the node, or the settings
	// an ALTER changes. A DROP carries none. A zero field is a setting left
	// out.
	Settings ydbdecl.CoordinationNode
}

// ErrStatement is returned for text that is one of Ptah's coordination node
// statements and cannot be run as written.
var ErrStatement = errors.New("invalid coordination node statement")

// service is the YDB service a coordination node statement runs through, as
// a refusal names it.
const service = "coordination service"

// Text writes the statement with its terminator. The settings are written in
// the order of [ydbdecl.CoordinationSettings], and only the ones it sets.
func (s Statement) Text() (string, error) {
	keyword := s.Verb.keyword()
	if keyword == "" {
		return "", fmt.Errorf("%w: unknown verb %d", ErrStatement, s.Verb)
	}
	text := keyword + " COORDINATION NODE " + ydbtype.QuoteIdentifier(s.Path)
	options := optionList(s.Settings)
	switch {
	case s.Verb == Create && options != "":
		text += " WITH (" + options + ")"
	case s.Verb == Alter && options == "":
		return "", fmt.Errorf("%w: ALTER COORDINATION NODE %s names no setting to change", ErrStatement, s.Path)
	case s.Verb == Alter:
		text += " SET (" + options + ")"
	case s.Verb == Drop && options != "":
		return "", fmt.Errorf("%w: DROP COORDINATION NODE %s takes no setting", ErrStatement, s.Path)
	}
	return text + ";", nil
}

// optionList writes the settings spec sets as `name = value` pairs: a period
// as an Interval, a mode as a string.
func optionList(spec ydbdecl.CoordinationNode) string {
	var options []string
	if spec.SelfCheckPeriodMillis != 0 {
		options = append(options, ydbdecl.SettingSelfCheckPeriod+" = Interval("+
			ydbtype.StringLiteral(ydbdecl.PeriodText(spec.SelfCheckPeriodMillis))+")")
	}
	if spec.SessionGracePeriodMillis != 0 {
		options = append(options, ydbdecl.SettingSessionGracePeriod+" = Interval("+
			ydbtype.StringLiteral(ydbdecl.PeriodText(spec.SessionGracePeriodMillis))+")")
	}
	for _, mode := range []struct{ setting, value string }{
		{ydbdecl.SettingReadConsistencyMode, spec.ReadConsistencyMode},
		{ydbdecl.SettingAttachConsistencyMode, spec.AttachConsistencyMode},
		{ydbdecl.SettingRateLimiterCountersMode, spec.RateLimiterCountersMode},
	} {
		if mode.value != "" {
			options = append(options, mode.setting+" = "+ydbtype.StringLiteral(mode.value))
		}
	}
	return strings.Join(options, ", ")
}

// Query is a query that runs one of Ptah's coordination node statements.
type Query struct {
	Statement
	// PathPrefix is the path the query's `PRAGMA TablePathPrefix` sets, or
	// empty where it sets none.
	PathPrefix string
}

// Absolute is the absolute path of the node the query names, on the
// database whose absolute path is database, with prefix in effect where the
// query sets no TablePathPrefix of its own.
//
// A relative TablePathPrefix is refused, as YDB refuses one: `PRAGMA
// TablePathPrefix("relp"); CREATE TABLE t (...)` answers `Table path not in
// database, path: relp/t, database: /local` on 25.1.4.7 and 26.2.1.14.
func (q Query) Absolute(database, prefix string) (string, error) {
	if strings.HasPrefix(q.Path, "/") {
		return path.Clean(q.Path), nil
	}
	if q.PathPrefix != "" {
		prefix = q.PathPrefix
	}
	switch {
	case prefix == "":
		return path.Join("/"+strings.Trim(database, "/"), q.Path), nil
	case strings.HasPrefix(prefix, "/"):
		return path.Join(prefix, q.Path), nil
	default:
		return "", fmt.Errorf("%w: path %s is not in database %s: the TablePathPrefix %q is relative, and YDB "+
			"reads a prefix only as an absolute path", ErrStatement, path.Join(prefix, q.Path),
			"/"+strings.Trim(database, "/"), prefix)
	}
}

// Recognize reports whether text is a query running one of Ptah's
// coordination node statements, and reads it.
//
// Such a query is the statement, preceded by nothing but definitions: the
// translation setting at its head, PRAGMA, DECLARE, DEFINE and IMPORT
// statements and named expressions. Of those, only `PRAGMA TablePathPrefix`
// changes what the statement names. A statement mixed with any other, or not
// last in its query, is refused with [ErrStatement], since the coordination
// service runs the one statement and nothing else; so is one Ptah cannot read.
// Text that holds none of the three statements, a string or a comment that
// merely mentions one included, is not recognized and reports no error.
func Recognize(text string) (Query, bool, error) {
	if !servicequery.ContainsFold(text, "coordination") {
		return Query{}, false, nil
	}
	located, recognized, err := servicequery.Locate(text, opens, service)
	switch {
	case err != nil:
		return Query{}, true, fmt.Errorf("%w: %w", ErrStatement, err)
	case !recognized:
		return Query{}, false, nil
	}
	statement, err := parse(located.Tokens)
	if err != nil {
		return Query{}, true, err
	}
	return Query{Statement: statement, PathPrefix: located.PathPrefix}, true, nil
}

// opens reports whether tokens open one of the three statements.
func opens(tokens []yql.Token) bool {
	if len(tokens) < 3 || !tokens[1].IsWord("COORDINATION") || !tokens[2].IsWord("NODE") {
		return false
	}
	return tokens[0].IsWord("CREATE") || tokens[0].IsWord("ALTER") || tokens[0].IsWord("DROP")
}

// parse reads one of the three statements.
func parse(tokens []yql.Token) (Statement, error) {
	statement := Statement{Verb: Drop}
	switch {
	case tokens[0].IsWord("CREATE"):
		statement.Verb = Create
	case tokens[0].IsWord("ALTER"):
		statement.Verb = Alter
	}
	opening := servicequery.FirstWords(tokens, 3)
	if len(tokens) < 4 {
		return Statement{}, fmt.Errorf("%w: %s names no node", ErrStatement, opening)
	}
	name := tokens[3]
	nodePath, ok := yql.IdentifierValue(name.Value)
	if name.Type != yql.TokenIdentifier || !ok || nodePath == "" || strings.HasPrefix(nodePath, "$") {
		return Statement{}, fmt.Errorf("%w: %s %s does not name a node; write its path in backticks", ErrStatement,
			opening, name.Value)
	}
	statement.Path = nodePath
	rest := tokens[4:]
	clause := map[Verb]string{Create: "WITH", Alter: "SET"}[statement.Verb]
	switch {
	case len(rest) == 0 && statement.Verb == Alter:
		return Statement{}, fmt.Errorf("%w: ALTER COORDINATION NODE %s names no setting; write SET (...)",
			ErrStatement, nodePath)
	case len(rest) == 0:
		return statement, nil
	case clause == "" || !rest[0].IsWord(clause):
		return Statement{}, fmt.Errorf("%w: %s %s is followed by %s, which Ptah does not read", ErrStatement,
			opening, nodePath, rest[0].Value)
	}
	settings, err := parseOptions(rest[1:], opening+" "+nodePath)
	if err != nil {
		return Statement{}, err
	}
	statement.Settings = settings
	if statement.Verb == Create {
		if err := ydbdecl.ValidateCoordinationNode(settings); err != nil {
			return Statement{}, fmt.Errorf("%w: %s %s: %w", ErrStatement, opening, nodePath, err)
		}
	}
	return statement, nil
}

// parseOptions reads `(name = value, ...)` and nothing after it.
func parseOptions(tokens []yql.Token, subject string) (ydbdecl.CoordinationNode, error) {
	var spec ydbdecl.CoordinationNode
	refuse := func(format string, args ...any) (ydbdecl.CoordinationNode, error) {
		return ydbdecl.CoordinationNode{}, fmt.Errorf("%w: %s: %s", ErrStatement, subject, fmt.Sprintf(format, args...))
	}
	if len(tokens) == 0 || !tokens[0].IsOperator("(") {
		return refuse("the settings are written in parentheses")
	}
	seen := make(map[string]bool)
	i := 1
	for {
		if i+2 >= len(tokens) {
			return refuse("the settings end early; write name = value, separated by commas, in parentheses")
		}
		setting := strings.ToLower(tokens[i].Value)
		if tokens[i].Type != yql.TokenIdentifier || !slices.Contains(ydbdecl.CoordinationSettings(), setting) {
			return refuse("%s is not a setting; the settings are %s", tokens[i].Value,
				strings.Join(ydbdecl.CoordinationSettings(), ", "))
		}
		if seen[setting] {
			return refuse("%s is set twice", setting)
		}
		seen[setting] = true
		if !tokens[i+1].IsOperator("=") {
			return refuse("%s is followed by %s, not =", setting, tokens[i+1].Value)
		}
		value, next, err := optionValue(tokens, i+2, setting)
		if err != nil {
			return refuse("%v", err)
		}
		if err := ydbdecl.SetCoordinationSetting(&spec, setting, value); err != nil {
			return refuse("%v", err)
		}
		if err := validateSetting(spec, setting); err != nil {
			return refuse("%v", err)
		}
		switch {
		case next < len(tokens) && tokens[next].IsOperator(","):
			i = next + 1
		case next == len(tokens)-1 && tokens[next].IsOperator(")"):
			return spec, nil
		default:
			return refuse("the settings end with ) and nothing follows it")
		}
	}
}

// optionValue reads the value of setting starting at tokens[at], and returns
// the index after it. A period is `Interval('...')`, a mode a string.
func optionValue(tokens []yql.Token, at int, setting string) (string, int, error) {
	if setting != ydbdecl.SettingSelfCheckPeriod && setting != ydbdecl.SettingSessionGracePeriod {
		value, ok := plainString(tokens[at])
		if !ok {
			return "", 0, fmt.Errorf("%s takes a string such as 'strict', not %s", setting, tokens[at].Value)
		}
		return value, at + 1, nil
	}
	if at+3 >= len(tokens) || !tokens[at].IsWord("Interval") || !tokens[at+1].IsOperator("(") ||
		!tokens[at+3].IsOperator(")") {
		return "", 0, fmt.Errorf("%s takes an interval such as Interval('PT1S')", setting)
	}
	value, ok := plainString(tokens[at+2])
	if !ok {
		return "", 0, fmt.Errorf("%s takes an interval such as Interval('PT1S'), not %s", setting, tokens[at+2].Value)
	}
	return value, at + 4, nil
}

// plainString reads a YQL string token that holds no escape, with or without
// the Utf8 suffix. A coordination node statement holds nothing but paths,
// durations and mode names, none of which needs one.
func plainString(token yql.Token) (string, bool) {
	if token.Type != yql.TokenString {
		return "", false
	}
	text := strings.TrimSuffix(token.Value, "u")
	if len(text) < 2 || (text[0] != '\'' && text[0] != '"') || text[len(text)-1] != text[0] {
		return "", false
	}
	inner := text[1 : len(text)-1]
	if strings.ContainsAny(inner, `\'"`) {
		return "", false
	}
	return inner, true
}

// validateSetting checks the one setting just read on its own: a mode's
// value, and a period's bounds that do not depend on another setting. The
// combination is checked where the node's other settings are known.
func validateSetting(spec ydbdecl.CoordinationNode, setting string) error {
	switch setting {
	case ydbdecl.SettingSelfCheckPeriod:
		return ydbdecl.ValidateCoordinationNode(ydbdecl.CoordinationNode{SelfCheckPeriodMillis: spec.SelfCheckPeriodMillis,
			SessionGracePeriodMillis: ydbdecl.MaxSessionGracePeriodMillis})
	case ydbdecl.SettingSessionGracePeriod:
		if spec.SessionGracePeriodMillis > ydbdecl.MaxSessionGracePeriodMillis {
			return ydbdecl.ValidateCoordinationNode(ydbdecl.CoordinationNode{SessionGracePeriodMillis: spec.SessionGracePeriodMillis})
		}
		return nil
	default:
		return ydbdecl.ValidateCoordinationNode(ydbdecl.CoordinationNode{
			ReadConsistencyMode:     spec.ReadConsistencyMode,
			AttachConsistencyMode:   spec.AttachConsistencyMode,
			RateLimiterCountersMode: spec.RateLimiterCountersMode,
		})
	}
}

// Changes is what an ALTER has to send to take a node configured as current
// to desired: each setting whose value the node runs with differs, at its
// desired value, and nothing else. Both sides are read with the node's
// defaults, so a setting left out on one side and set to its default on the
// other is no change. YDB keeps every setting an AlterNode leaves out, so
// naming only what changed is safe (measured on 25.1.4.7 and 26.2.1.14).
func Changes(desired, current ydbdecl.CoordinationNode) ydbdecl.CoordinationNode {
	want, have := Effective(desired), Effective(current)
	var changes ydbdecl.CoordinationNode
	if want.SelfCheckPeriodMillis != have.SelfCheckPeriodMillis {
		changes.SelfCheckPeriodMillis = want.SelfCheckPeriodMillis
	}
	if want.SessionGracePeriodMillis != have.SessionGracePeriodMillis {
		changes.SessionGracePeriodMillis = want.SessionGracePeriodMillis
	}
	if want.ReadConsistencyMode != have.ReadConsistencyMode {
		changes.ReadConsistencyMode = want.ReadConsistencyMode
	}
	if want.AttachConsistencyMode != have.AttachConsistencyMode {
		changes.AttachConsistencyMode = want.AttachConsistencyMode
	}
	if want.RateLimiterCountersMode != have.RateLimiterCountersMode {
		changes.RateLimiterCountersMode = want.RateLimiterCountersMode
	}
	return changes
}

// Merge is current with every setting changes names replaced: the
// configuration a node holds after an ALTER that sends changes.
func Merge(current, changes ydbdecl.CoordinationNode) ydbdecl.CoordinationNode {
	if changes.SelfCheckPeriodMillis != 0 {
		current.SelfCheckPeriodMillis = changes.SelfCheckPeriodMillis
	}
	if changes.SessionGracePeriodMillis != 0 {
		current.SessionGracePeriodMillis = changes.SessionGracePeriodMillis
	}
	if changes.ReadConsistencyMode != "" {
		current.ReadConsistencyMode = changes.ReadConsistencyMode
	}
	if changes.AttachConsistencyMode != "" {
		current.AttachConsistencyMode = changes.AttachConsistencyMode
	}
	if changes.RateLimiterCountersMode != "" {
		current.RateLimiterCountersMode = changes.RateLimiterCountersMode
	}
	return current
}

// Effective is spec with every setting it leaves out taken from the node's
// defaults: a self-check every second, a ten-second grace period, relaxed
// reads, strict attaches and aggregated counters.
func Effective(spec ydbdecl.CoordinationNode) ydbdecl.CoordinationNode {
	return Merge(ydbdecl.CoordinationNode{SelfCheckPeriodMillis: 1_000, SessionGracePeriodMillis: 10_000,
		ReadConsistencyMode: "relaxed", AttachConsistencyMode: "strict", RateLimiterCountersMode: "aggregated"}, spec)
}
