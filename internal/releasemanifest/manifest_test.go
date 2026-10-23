package releasemanifest_test

import (
	"encoding/json"
	"testing"

	qt "github.com/frankban/quicktest"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"

	"github.com/stokaro/ptah-ydb-plugin/internal/backend"
	"github.com/stokaro/ptah-ydb-plugin/internal/releasemanifest"
)

func TestBuildDeclaredLines_HappyPath(t *testing.T) {
	c := qt.New(t)

	out, err := releasemanifest.Build(backend.Description())
	c.Assert(err, qt.IsNil)

	var got releasemanifest.Manifest
	c.Assert(json.Unmarshal(out, &got), qt.IsNil)
	c.Assert(got.Engines, qt.HasLen, 1)
	c.Assert(got.Engines[0].ID, qt.Equals, "ydb")
	lines := make([]string, len(got.Engines[0].Lines))
	for i, line := range got.Engines[0].Lines {
		lines[i] = line.Line + " " + line.Support + " " + line.Image
	}
	c.Assert(lines, qt.DeepEquals, []string{
		"26.2 certified ydbplatform/local-ydb:26.2",
		"26.1 best_effort ydbplatform/local-ydb:26.1",
		"25.4 best_effort ydbplatform/local-ydb:25.4",
		"25.3 best_effort ydbplatform/local-ydb:25.3",
		"25.2 best_effort ydbplatform/local-ydb:25.2",
		"25.1 certified ydbplatform/local-ydb:25.1",
	})
}

func TestBuildIsDeterministic_HappyPath(t *testing.T) {
	c := qt.New(t)

	first, err := releasemanifest.Build(backend.Description())
	c.Assert(err, qt.IsNil)
	second, err := releasemanifest.Build(backend.Description())
	c.Assert(err, qt.IsNil)
	c.Assert(string(first), qt.Equals, string(second))
}

func TestBuildLeavesOutEnginesWithoutLines_HappyPath(t *testing.T) {
	c := qt.New(t)

	out, err := releasemanifest.Build(&protocol.Description{ProviderId: "example.org/p", Engines: []*protocol.Engine{{Id: "bare"}}})
	c.Assert(err, qt.IsNil)

	var got releasemanifest.Manifest
	c.Assert(json.Unmarshal(out, &got), qt.IsNil)
	c.Assert(got.Provider, qt.Equals, "example.org/p")
	c.Assert(got.Engines, qt.HasLen, 0)
}
