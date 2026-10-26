package schema

import (
	"fmt"
	"path"
	"strings"

	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/stokaro/ptah/provider-sdk/provider"
)

// ValidateDatabaseContext checks physical roots without treating either root
// as an object's database identity.
func ValidateDatabaseContext(value *protocol.DatabaseContext) error {
	if err := provider.ValidateDatabaseContext(value); err != nil {
		return err
	}
	for _, root := range []string{value.Database, value.SourceDatabase} {
		if root == "/" || !path.IsAbs(root) || path.Clean(root) != root || strings.Contains(root, "\\") {
			return fmt.Errorf("invalid YDB database context path")
		}
	}
	return nil
}

// InterpretationRoot returns the explicit root for a model's path values.
// Declarations use the authored root; observations use the selected realm.
// An absent receipt stays unknown even when the identity names a database.
func InterpretationRoot(value *protocol.Envelope) string {
	if definition := NativeDesiredModel(value.GetKind()); definition != nil && value.GetDefinitionSha256() == definition.GetDefinitionSha256() {
		return value.GetContext().GetSourceDatabase()
	}
	return value.GetContext().GetDatabase()
}
