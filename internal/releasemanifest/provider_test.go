package releasemanifest_test

import (
	"encoding/json"
	"testing"

	qt "github.com/frankban/quicktest"
	"github.com/stokaro/ptah/provider-sdk/provider"

	"github.com/stokaro/ptah-ydb-plugin/internal/backend"
	"github.com/stokaro/ptah-ydb-plugin/internal/releasemanifest"
)

func TestBuildProvider_HappyPath(t *testing.T) {
	c := qt.New(t)

	out, err := releasemanifest.BuildProvider(backend.Description(), "0.2.0", "ptah-plugin-ydb", "ptah-plugin-ydb")
	c.Assert(err, qt.IsNil)

	var got releasemanifest.ProviderDocument
	c.Assert(json.Unmarshal(out, &got), qt.IsNil)
	c.Assert(got.Schema, qt.Equals, 1)
	c.Assert(got.ProviderID, qt.Equals, "github.com/stokaro/ptah-ydb-plugin")
	c.Assert(got.Version, qt.Equals, "0.2.0")
	c.Assert(got.MinContract, qt.Equals, releasemanifest.ContractVersion{Major: provider.Revision, Minor: 0})
	c.Assert(got.Engines, qt.DeepEquals, []string{"ydb"})
	c.Assert(got.Artifacts, qt.DeepEquals, []releasemanifest.Artifact{
		{OS: "darwin", Arch: "amd64", Archive: "ptah-plugin-ydb_0.2.0_darwin_amd64.tar.gz", Executable: "ptah-plugin-ydb"},
		{OS: "darwin", Arch: "arm64", Archive: "ptah-plugin-ydb_0.2.0_darwin_arm64.tar.gz", Executable: "ptah-plugin-ydb"},
		{OS: "linux", Arch: "amd64", Archive: "ptah-plugin-ydb_0.2.0_linux_amd64.tar.gz", Executable: "ptah-plugin-ydb"},
		{OS: "linux", Arch: "arm64", Archive: "ptah-plugin-ydb_0.2.0_linux_arm64.tar.gz", Executable: "ptah-plugin-ydb"},
		{OS: "windows", Arch: "amd64", Archive: "ptah-plugin-ydb_0.2.0_windows_amd64.zip", Executable: "ptah-plugin-ydb.exe"},
		{OS: "windows", Arch: "arm64", Archive: "ptah-plugin-ydb_0.2.0_windows_arm64.zip", Executable: "ptah-plugin-ydb.exe"},
	})
}

func TestBuildProviderIsDeterministic_HappyPath(t *testing.T) {
	c := qt.New(t)

	first, err := releasemanifest.BuildProvider(backend.Description(), "0.2.0", "ptah-plugin-ydb", "ptah-plugin-ydb")
	c.Assert(err, qt.IsNil)
	second, err := releasemanifest.BuildProvider(backend.Description(), "0.2.0", "ptah-plugin-ydb", "ptah-plugin-ydb")
	c.Assert(err, qt.IsNil)
	c.Assert(string(first), qt.Equals, string(second))
}

func TestBuildProvider_FailurePath(t *testing.T) {
	for _, version := range []string{"", "v0.2.0"} {
		t.Run(version, func(t *testing.T) {
			c := qt.New(t)
			out, err := releasemanifest.BuildProvider(backend.Description(), version, "ptah-plugin-ydb", "ptah-plugin-ydb")
			c.Assert(err, qt.ErrorMatches, `release version ".*" must be the tag without its leading v`)
			c.Assert(out, qt.IsNil)
		})
	}
}
