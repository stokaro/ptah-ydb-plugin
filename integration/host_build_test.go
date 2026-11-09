//go:build integration

package integration_test

import (
	"debug/buildinfo"
	"slices"

	qt "github.com/frankban/quicktest"
	"golang.org/x/mod/semver"
)

// providerSDKModules are the module paths a host binary links the provider SDK
// under: ptah.run/provider-sdk, or, for Ptah releases up to v0.25.0, the GitHub
// path that SDK releases up to v0.39.0 declare. The SDK kept one version
// sequence across both, so a version read under either compares with the other.
var providerSDKModules = []string{"ptah.run/provider-sdk", "github.com/stokaro/ptah/provider-sdk"}

// releaseHostSDK is the provider SDK version Ptah v0.23.0 is built with. Each
// host change that requireHostChange names merged on Ptah master after
// v0.23.0 and before the next release, which pins a later SDK. A host built
// with this version or an older one can lack them; a host built with a later
// one, a master build or a later release, has them all.
const releaseHostSDK = "v0.30.0"

// omissionCountsHostSDK is the published SDK version whose report fields the
// host adapter carries into DBML and JSON export diagnostics.
const omissionCountsHostSDK = "v0.35.0"

// documentIndexRenameChange is the host change that pairs an index renamed
// between two SQL documents, which Ptah v0.23.0 plans as a drop and an add.
const documentIndexRenameChange = "the pairing of an index renamed between two SQL documents, which Ptah v0.23.0 plans as a drop and an add"

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
		c.Skipf("the selected host is built with provider SDK %s and lacks %s", version, change)
	}
}

// requireHostOmissionCounts runs an omission-count test only with a host
// built against the published SDK contract that carries those report fields.
// Earlier master and release hosts cannot negotiate the feature.
func requireHostOmissionCounts(c *qt.C) {
	c.Helper()
	version := hostSDKVersion(c)
	if semver.Compare(version, omissionCountsHostSDK) < 0 {
		c.Skipf("the selected host is built with provider SDK %s, before omission counts in %s", version, omissionCountsHostSDK)
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
		if !slices.Contains(providerSDKModules, module.Path) {
			continue
		}
		if module.Replace != nil {
			module = module.Replace
		}
		c.Assert(semver.IsValid(module.Version), qt.IsTrue,
			qt.Commentf("the host binary names %s at %q, which is no module version", module.Path, module.Version))
		return module.Version
	}
	c.Fatalf("the host binary names no provider SDK module (%v)", providerSDKModules)
	return ""
}
