package buildversion_test

import (
	"testing"

	qt "github.com/frankban/quicktest"

	"github.com/stokaro/ptah-ydb-plugin/internal/buildversion"
)

func TestResolve(t *testing.T) {
	for _, test := range []struct {
		name, linked, module, want string
	}{
		{name: "the release build links the version", linked: "0.4.0", module: "v0.3.0", want: "0.4.0"},
		{name: "a build on a tag records it", module: "v0.4.0", want: "0.4.0"},
		{name: "a build between tags records a pseudo-version", module: "v0.4.1-0.20261102212024-ff23ff40361d+dirty", want: "0.4.1-0.20261102212024-ff23ff40361d+dirty"},
		{name: "a checkout without a tag", module: "(devel)", want: buildversion.Development},
		{name: "no build information", want: buildversion.Development},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := qt.New(t)
			c.Assert(buildversion.Resolve(test.linked, test.module), qt.Equals, test.want)
		})
	}
}

// A test binary records no main module version, so a binary built without the
// release flags reports itself as a development build.
func TestVersionOfADevelopmentBuild(t *testing.T) {
	c := qt.New(t)
	c.Assert(buildversion.Version(), qt.Equals, buildversion.Development)
}
