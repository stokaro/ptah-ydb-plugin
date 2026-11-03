// Package buildversion says which release of the provider this binary is. The
// backend reports it in the provider description, and a host records it with
// the migrations and the provider metadata it writes.
package buildversion

import (
	"runtime/debug"
	"strings"
)

// Development is the version of a binary built from a checkout that names no
// release.
const Development = "0.0.0-dev"

// linked is set by the release build with -ldflags
// "-X github.com/stokaro/ptah-ydb-plugin/internal/buildversion.linked=<version>".
var linked string

// Version is the release this binary was built as: the version the release
// build linked in, else the module version Go recorded from the tag a build
// stood on, else [Development].
func Version() string {
	main := ""
	if info, found := debug.ReadBuildInfo(); found {
		main = info.Main.Version
	}
	return Resolve(linked, main)
}

// Resolve chooses between a linked version and the main module version Go
// recorded. A linked version wins. A module version loses its "v" prefix, which
// the provider manifest refuses; "(devel)" and the empty string mean Go
// recorded no version.
func Resolve(linked, module string) string {
	if linked != "" {
		return linked
	}
	if module == "" || module == "(devel)" {
		return Development
	}
	return strings.TrimPrefix(module, "v")
}
