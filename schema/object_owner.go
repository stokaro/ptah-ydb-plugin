package schema

import (
	"fmt"
	"path"
	"strings"
	"unicode/utf8"

	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/stokaro/ptah/provider-sdk/provider"
)

// NativeBoundOwner returns the owner a comparison subject of a standalone
// object of kind names: its database, its directory relative to the database
// root and its name, each spelled exactly. A catalog, a parent, a path or a
// signature is refused, and so is a path component YDB cannot hold.
func NativeBoundOwner(kind string, subject *protocol.ComparisonIdentity) (*protocol.Reference, error) {
	if err := provider.ValidateComparisonIdentity(subject); err != nil {
		return nil, err
	}
	if subject.Kind != kind || subject.GetCatalog().GetSource() != "" || subject.GetParent().GetSource() != "" || len(subject.Path)+len(subject.Signature) != 0 {
		return nil, fmt.Errorf("invalid YDB object comparison identity")
	}
	for _, part := range []*protocol.ComparisonPart{subject.Database, subject.Catalog, subject.Schema, subject.Name} {
		if part.GetSource() != part.GetNormalized() {
			return nil, fmt.Errorf("YDB identifiers must retain exact spelling")
		}
	}
	owner := &protocol.Reference{Engine: "ydb", Kind: kind, Database: subject.GetDatabase().GetNormalized(), Schema: subject.GetSchema().GetNormalized(), Name: subject.GetName().GetNormalized()}
	return owner, ValidateNativeOwner(owner)
}

// ValidateNativeOwner checks the path components of a standalone object's
// owner. A dot inside a name is part of it, and a name may begin with one.
func ValidateNativeOwner(owner *protocol.Reference) error {
	if owner.GetEngine() != "ydb" || !IsNativeObject(owner.GetKind()) || owner.GetCatalog() != "" || len(owner.GetPath()) != 0 || len(owner.GetOverload()) != 0 {
		return fmt.Errorf("invalid YDB object identity")
	}
	if database := owner.GetDatabase(); database != "" && (!utf8.ValidString(database) || !path.IsAbs(database) || path.Clean(database) != database || strings.ContainsAny(database, "\\\x00")) {
		return fmt.Errorf("invalid YDB database path")
	}
	parts := []string{owner.GetName()}
	if namespace := owner.GetSchema(); namespace != "" {
		parts = append(parts, strings.Split(namespace, "/")...)
	}
	for _, part := range parts {
		if strings.TrimSpace(part) == "" || part == "." || part == ".." || !utf8.ValidString(part) || strings.ContainsAny(part, "/\\\x00") {
			return fmt.Errorf("invalid YDB object path")
		}
	}
	return nil
}
