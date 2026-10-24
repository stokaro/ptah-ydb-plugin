package tablecreate

import (
	"errors"
	"fmt"

	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
)

// unsupportedError marks a declaration the provider cannot carry, as opposed
// to one that is wrong. The message is the whole text.
type unsupportedError struct{ message string }

func (e *unsupportedError) Error() string { return e.message }

func unsupported(format string, args ...any) error {
	return &unsupportedError{message: fmt.Sprintf(format, args...)}
}

// IsUnsupported reports whether err refuses a declaration for a feature the
// provider does not implement, rather than for an invalid value.
func IsUnsupported(err error) bool {
	_, found := errors.AsType[*unsupportedError](err)
	return found
}

// ValidateIdentifiers checks the exact identifier context that preparation,
// prediction, validation and planning all interpret.
func ValidateIdentifiers(facts *protocol.ComparisonIdentifiers) error {
	return validateTableIdentifiers(facts)
}

// ValidateDeclaration checks one captured row-table declaration against the
// rules CREATE compiles with, without the policy that only creation needs.
// It reads the declaration and opens nothing.
func ValidateDeclaration(subject *protocol.ComparisonIdentity, table *protocol.DeclaredTable) error {
	owner, err := boundOwner(subject)
	if err != nil {
		return err
	}
	if IsColumnTable(table) {
		_, err = CompileColumnTable(owner, table)
		return err
	}
	_, err = compile(owner, table, false)
	return err
}

// TablePath returns the database and the slash-separated scheme path a table
// identity names, after checking that the identity is a valid YDB table owner.
func TablePath(subject *protocol.ComparisonIdentity) (database, path string, err error) {
	owner, err := boundOwner(subject)
	if err != nil {
		return "", "", err
	}
	if owner.Schema != "" {
		return owner.Database, owner.Schema + "/" + owner.Name, nil
	}
	return owner.Database, owner.Name, nil
}

// ValidName reports whether name is acceptable as one element of a YDB scheme
// path, which is also how a namespace declaration is read: as a directory.
func ValidName(name string) bool { return validName(name) }
