// Package secrets defines YDB scheme-secret declarations without their values.
package secrets

import (
	"fmt"
	"path"
	"strings"
	"unicode/utf8"

	"github.com/stokaro/ptah-ydb-plugin/internal/ydbtype"
)

// Capability names the server's scheme-secret feature.
const Capability = "secrets"

// ValuePrefix is the namespace the host can approve for secret value bindings.
const ValuePrefix = "PTAH_SECRET_"

// Spec contains the value reference, never the value itself.
type Spec struct {
	ValueEnv string `json:"value_env"`
}

// DeclarationError reports an invalid attribute without retaining its value.
type DeclarationError struct{ Attribute, Reason string }

func (e *DeclarationError) Error() string {
	return fmt.Sprintf("invalid %s: %s", e.Attribute, e.Reason)
}

// AttributeValueEnv names the declared approved-input reference.
const AttributeValueEnv = "value_env"

// ValidateIdentity requires a relative directory and an exact leaf name.
func ValidateIdentity(directory, name string) error {
	if strings.TrimSpace(name) == "" || name == "." || name == ".." || strings.ContainsAny(name, "/\\\x00") ||
		!utf8.ValidString(name) || !utf8.ValidString(directory) || strings.ContainsAny(directory, "\\\x00") ||
		(directory != "" && (path.IsAbs(directory) || path.Clean(directory) != directory || directory == "." || directory == ".." || strings.HasPrefix(directory, "../"))) {
		return fmt.Errorf("secret requires a relative directory and an exact leaf name")
	}
	return nil
}

// Path writes a secret's exact path as one quoted YQL identifier.
func Path(directory, name string) string { return ydbtype.QuotePath(directory, name) }

// CheckValueEnv reports why name cannot be the variable a secret's value comes
// from, or nil: it starts with [ValuePrefix], has a name after it, and is
// spelled as YQL spells a named expression, so `$<name>` refers to it.
func CheckValueEnv(name string) error {
	rest, prefixed := strings.CutPrefix(name, ValuePrefix)
	switch {
	case name == "":
		return &DeclarationError{Attribute: AttributeValueEnv,
			Reason: "a secret names the environment variable that holds its value"}
	case !prefixed || rest == "":
		return &DeclarationError{Attribute: AttributeValueEnv,
			Reason: fmt.Sprintf("%q does not start with %s and a name after it; Ptah reads a secret's value only "+
				"from a variable under that prefix, so a migration file cannot copy any other variable of the "+
				"machine that applies it", name, ValuePrefix)}
	case !variableName(name):
		return &DeclarationError{Attribute: AttributeValueEnv,
			Reason: fmt.Sprintf("%q holds a character other than an ASCII letter, a digit or an underscore", name)}
	default:
		return nil
	}
}

// variableName reports a name of ASCII letters, digits and underscores that
// does not start with a digit.
func variableName(name string) bool {
	for i := range len(name) {
		c := name[i]
		switch {
		case c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z', c == '_':
		case c >= '0' && c <= '9' && i > 0:
		default:
			return false
		}
	}
	return name != ""
}

// DefaultValueEnv is the variable Ptah names for a secret it did not declare
// itself: one it read from a database and writes out as a declaration, or one
// a rollback creates again. It is [ValuePrefix] followed by the secret's path
// in upper case, with every character a variable name cannot hold written as
// an underscore.
func DefaultValueEnv(schema, name string) string {
	path := strings.Trim(schema, "/")
	if path != "" {
		path += "/"
	}
	path += name
	var b strings.Builder
	b.WriteString(ValuePrefix)
	for _, r := range strings.ToUpper(path) {
		if r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' {
			b.WriteRune(r)
			continue
		}
		b.WriteByte('_')
	}
	return b.String()
}

// Display names the secret by the path YDB writes for it, unquoted, for
// messages and reports: `dir/name`, or `name` at the database root.
func Display(schema, name string) string {
	if schema == "" {
		return name
	}
	return strings.TrimRight(schema, "/") + "/" + name
}

// Reference is the named expression a statement writes in place of the value
// valueEnv holds.
func Reference(valueEnv string) string {
	return "$" + valueEnv
}

// CreateStatement writes what creates the secret with the value valueEnv
// holds when the statement runs. The secret takes YDB's default permissions:
// it inherits only DESCRIBE SCHEMA from its directory.
func CreateStatement(schema, name, valueEnv string) string {
	return fmt.Sprintf("CREATE SECRET %s WITH (value = %s);", Path(schema, name), Reference(valueEnv))
}

// AlterStatement writes what gives the secret the value valueEnv holds when
// the statement runs.
func AlterStatement(schema, name, valueEnv string) string {
	return fmt.Sprintf("ALTER SECRET %s WITH (value = %s);", Path(schema, name), Reference(valueEnv))
}

// DropStatement writes what drops the secret and its value, which nothing can
// read back.
func DropStatement(schema, name string) string {
	return "DROP SECRET " + Path(schema, name) + ";"
}

// The consequences of the three statements, in the words every report uses:
// a change's effect, a statement's safety assessment and the classification
// of migration SQL text.
const (
	// CreateReason is why creating a secret is additive.
	CreateReason = "CREATE SECRET creates a YDB secret with the value its variable holds when the statement runs"
	// RotateReason is why rotating a secret changes behavior.
	RotateReason = "ALTER SECRET replaces the value every external data source naming the secret uses"
	// DropReason is why dropping a secret is destructive.
	DropReason = "DROP SECRET removes a YDB secret whose value nothing can read back"
)
