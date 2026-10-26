// Package streaming reads, checks, compares and writes YDB streaming queries
// as Ptah's embedded YDB engine does (dialect/ydb/ydbstreaming): a query that
// runs continuously over topics, its body, whether it runs, and the resource
// pool it runs in. Runtime status, retries, topic offsets and checkpoints are
// not schema state.
package streaming

import (
	"fmt"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/stokaro/ptah-ydb-plugin/internal/yql"
	"github.com/stokaro/ptah-ydb-plugin/internal/yqlquery"
	"github.com/stokaro/ptah-ydb-plugin/internal/ydbtype"
)

// Capability is a target that keeps streaming queries: one whose
// EnableStreamingQueries and EnableExternalDataSources flags are both on.
// 25.1 and 25.2 do not parse the statements, and 26.2, whose
// EnableStreamingQueries is on by default, reads a topic through the external
// data source machinery (`data source pq doesn't exist` without it).
const Capability = "streaming_queries"

// The reasons a streaming query change gives for its impact, the host's
// words.
const (
	// ExecutionChange explains the risk of changing continuous execution
	// settings.
	ExecutionChange = "ALTER STREAMING QUERY changes continuous query execution"
	// CheckpointLoss explains why a rollback cannot recover the query's state.
	CheckpointLoss = "the operation removes or resets streaming-query checkpoint state; a rollback restores the declaration, not the discarded state"
	// Creation explains a creation's impact.
	Creation = "creates a streaming query without replacing existing checkpoint state"
)

// CapabilityRefusal is what a declared query on a target without the
// capability is refused with.
func CapabilityRefusal(subject string) string {
	return subject + ": requires target capability streaming_queries (YDB needs EnableStreamingQueries and EnableExternalDataSources on the cluster)"
}

// Spec is a streaming query's persistent settings: the body inside DO BEGIN
// ... END DO, whether it runs, and its resource pool. A nil Run starts the
// query; an empty ResourcePool selects the pool `default`.
type Spec struct {
	Text         string `json:"text"`
	Run          *bool  `json:"run,omitempty"`
	ResourcePool string `json:"resource_pool,omitempty"`
}

// Running resolves the server default of RUN = TRUE.
func Running(spec Spec) bool { return spec.Run == nil || *spec.Run }

// Pool resolves the server's default resource pool.
func Pool(spec Spec) string {
	if spec.ResourcePool == "" {
		return "default"
	}
	return spec.ResourcePool
}

// Equal compares persistent settings, resolving the defaults and reading the
// bodies as tokens.
func Equal(a, b Spec) bool {
	return SameBody(a.Text, b.Text) && Running(a) == Running(b) && Pool(a) == Pool(b)
}

// SameBody compares two bodies as YQL tokens, with their contents and
// operators, ignoring whitespace and comments: YDB 26.2 removes the comments
// of a body it stores on ALTER, so comparing the text would reset the
// checkpoint over a comment on every run.
func SameBody(a, b string) bool {
	return slices.Equal(bodyTokens(a), bodyTokens(b))
}

func bodyTokens(text string) []yql.Token {
	var tokens []yql.Token
	for _, token := range yql.Tokens(text) {
		if token.Type != yql.TokenWhitespace && token.Type != yql.TokenComment {
			tokens = append(tokens, yql.Token{Type: token.Type, Value: token.Value})
		}
	}
	return tokens
}

// Validate checks that the body stays one streaming query when wrapped in
// its DO block and holds data statements only. YDB validates the data query
// itself; Ptah refuses a schema statement and a text that escapes the
// block before anything runs.
func Validate(spec Spec) error {
	if !utf8.ValidString(spec.Text) || !utf8.ValidString(spec.ResourcePool) || strings.ContainsRune(spec.ResourcePool, 0) {
		return fmt.Errorf("streaming query text and resource pool must be valid UTF-8 without a NUL in the pool name")
	}
	if strings.TrimSpace(spec.Text) == "" {
		return fmt.Errorf("streaming query text is required")
	}
	queries, err := yqlquery.Split(spec.Text)
	if err != nil {
		return err
	}
	for _, query := range queries {
		if query.Kind != yqlquery.Data {
			return fmt.Errorf("streaming query text must contain data statements only")
		}
	}
	wrapped, err := yqlquery.Split("CREATE STREAMING QUERY validation AS DO BEGIN\n" + spec.Text + "\nEND DO;")
	if err != nil {
		return err
	}
	if len(wrapped) != 1 || wrapped[0].Kind != yqlquery.Scheme {
		return fmt.Errorf("streaming query text must stay inside its DO BEGIN ... END DO block")
	}
	return nil
}

// Path writes a query's directory and name as one quoted YQL path.
func Path(schema, name string) string { return ydbtype.QuotePath(schema, name) }

// Display names a query as a message names it.
func Display(schema, name string) string {
	if schema == "" {
		return name
	}
	return schema + "/" + name
}

// Create writes the CREATE STREAMING QUERY of a query, naming whether it runs
// and its pool. orReplace replaces a query that exists at the path, which
// resets its aggregation state and keeps its topic offsets.
func Create(schema, name string, spec Spec, orReplace bool) string {
	prefix := "CREATE "
	if orReplace {
		prefix += "OR REPLACE "
	}
	return prefix + "STREAMING QUERY " + Path(schema, name) + " WITH (" + settings(spec) + ") AS DO BEGIN\n" + strings.TrimSpace(spec.Text) + "\nEND DO;"
}

// Alter writes the ALTER STREAMING QUERY that moves the query from current
// to desired in place. A changed body needs allowStateReset, because YDB
// discards the aggregation state with it; the topic offsets stay in the
// checkpoint, and no drop and creation stands in for it.
func Alter(schema, name string, desired, current Spec, allowStateReset bool) (string, error) {
	if err := ValidateAlter(schema, name, desired, current, allowStateReset); err != nil {
		return "", err
	}
	changed := !SameBody(desired.Text, current.Text)
	values := settings(desired)
	if changed {
		values += ", FORCE = TRUE"
	}
	statement := "ALTER STREAMING QUERY " + Path(schema, name) + " SET (" + values + ")"
	if changed {
		statement += " AS DO BEGIN\n" + strings.TrimSpace(desired.Text) + "\nEND DO"
	}
	return statement + ";", nil
}

// ValidateAlter refuses a body change without the permission to reset the
// query's state.
func ValidateAlter(schema, name string, desired, current Spec, allowStateReset bool) error {
	if !SameBody(desired.Text, current.Text) && !allowStateReset {
		return fmt.Errorf("streaming query %q: changing text resets aggregation state; declare allow_state_reset=true to permit it", Display(schema, name))
	}
	return nil
}

// Drop writes the DROP STREAMING QUERY of a query; YDB deletes its
// checkpoints with it.
func Drop(schema, name string) string { return "DROP STREAMING QUERY " + Path(schema, name) + ";" }

func settings(spec Spec) string {
	return "RUN = " + strings.ToUpper(strconv.FormatBool(Running(spec))) + ", RESOURCE_POOL = " + ydbtype.QuoteIdentifier(Pool(spec))
}

// Change is the change of one query. Before is the query as the database
// holds it, nil for a creation; After the declared one, nil for a removal.
// AllowStateReset is the declaration's permission to reset the query's state.
type Change struct {
	Schema, Name    string
	Before, After   *Spec
	AllowStateReset bool
}

// Statements returns the statements that carry out a change, in order. A
// running query that changes is stopped first, with its old body, as the host
// stops it, so the change runs against a stopped query; a change that only
// stops it is that one statement. A creation the declaration permits to reset
// state replaces a query that appears at the path meanwhile.
func Statements(change Change) ([]string, error) {
	switch {
	case change.Before == nil && change.After == nil:
		return nil, fmt.Errorf("streaming query %q: the change has no operand", Display(change.Schema, change.Name))
	case change.Before == nil:
		return []string{Create(change.Schema, change.Name, *change.After, change.AllowStateReset)}, nil
	case change.After == nil:
		return []string{Drop(change.Schema, change.Name)}, nil
	case Equal(*change.Before, *change.After):
		return nil, fmt.Errorf("streaming query %q: the change holds no difference", Display(change.Schema, change.Name))
	}
	if err := ValidateAlter(change.Schema, change.Name, *change.After, *change.Before, change.AllowStateReset); err != nil {
		return nil, err
	}
	current := *change.Before
	var statements []string
	if Running(current) {
		stopped := current
		stopped.Run = new(false)
		stop, err := Alter(change.Schema, change.Name, stopped, current, false)
		if err != nil {
			return nil, err
		}
		statements = append(statements, stop)
		if Equal(stopped, *change.After) {
			return statements, nil
		}
		current = stopped
	}
	statement, err := Alter(change.Schema, change.Name, *change.After, current, change.AllowStateReset)
	if err != nil {
		return nil, err
	}
	return append(statements, statement), nil
}

// LosesCheckpoint reports whether a change discards the query's state: a
// removal, a creation that may replace a query, or a changed body.
func LosesCheckpoint(change Change) bool {
	switch {
	case change.After == nil:
		return true
	case change.Before == nil:
		return change.AllowStateReset
	default:
		return !SameBody(change.Before.Text, change.After.Text)
	}
}

// NamedPaths returns the scheme paths a body names as a source or a target,
// the name after FROM, JOIN or INTO, each once, in the order the body first
// names it. A backticked name is returned without its backticks and escapes,
// a bare one as written. A name a data source qualifies, a table function, a
// named expression and a subquery name no path of the database and are left
// out, and a body that sets TablePathPrefix names none, since it resolves its
// names against a prefix Ptah does not follow.
func NamedPaths(text string) []string {
	tokens := bodyTokens(text)
	for _, token := range tokens {
		if token.Type == yql.TokenIdentifier && strings.EqualFold(token.Value, "TablePathPrefix") {
			return nil
		}
	}
	var paths []string
	for index := 0; index+1 < len(tokens); index++ {
		keyword := tokens[index]
		if !keyword.IsWord("FROM") && !keyword.IsWord("JOIN") && !keyword.IsWord("INTO") {
			continue
		}
		name := tokens[index+1]
		if name.Type != yql.TokenIdentifier || strings.HasPrefix(name.Value, "$") {
			continue
		}
		if index+2 < len(tokens) && (tokens[index+2].IsOperator(".") || tokens[index+2].IsOperator("(")) {
			continue
		}
		path := name.Value
		if decoded, ok := yql.IdentifierValue(name.Value); ok {
			path = decoded
		}
		if path != "" && !slices.Contains(paths, path) {
			paths = append(paths, path)
		}
	}
	return paths
}
