//go:build integration

package integration_test

import (
	"debug/buildinfo"

	qt "github.com/frankban/quicktest"
	"golang.org/x/mod/semver"
)

// sdkModule is the module of the provider SDK a host binary is built with.
const sdkModule = "github.com/stokaro/ptah/provider-sdk"

// releaseHostSDK is the provider SDK version Ptah v0.23.0 is built with. Each
// host change that requireHostChange names merged on Ptah master after
// v0.23.0 and before the next release, which pins a later SDK. A host built
// with this version or an older one can lack them; a host built with a later
// one, a master build or a later release, has them all.
const releaseHostSDK = "v0.30.0"

// requireHostChange skips a test of host behavior that change gives on a host
// built with provider SDK releaseHostSDK or an older one, such as Ptah
// v0.23.0. The version comes from the host binary's build information, not
// from what the host prints, so a host built with a later SDK always runs the
// test: Ptah master never skips it, even when a regression makes master
// answer as the old release does. A host binary whose SDK version cannot be
// read fails the test instead of skipping it.
func requireHostChange(c *qt.C, change string) {
	c.Helper()
	version := hostSDKVersion(c)
	if semver.Compare(version, releaseHostSDK) <= 0 {
		c.Skipf("the selected host is built with %s %s and lacks %s", sdkModule, version, change)
	}
}

// hostSDKVersion returns the provider SDK version the pinned host binary is
// built with, the version of its replacement when the build replaced the
// module.
func hostSDKVersion(c *qt.C) string {
	c.Helper()
	info, err := buildinfo.ReadFile(pinnedHost(c))
	c.Assert(err, qt.IsNil)
	for _, module := range info.Deps {
		if module.Path != sdkModule {
			continue
		}
		if module.Replace != nil {
			module = module.Replace
		}
		c.Assert(semver.IsValid(module.Version), qt.IsTrue,
			qt.Commentf("the host binary names %s at %q, which is no module version", sdkModule, module.Version))
		return module.Version
	}
	c.Fatalf("the host binary names no %s module", sdkModule)
	return ""
}
