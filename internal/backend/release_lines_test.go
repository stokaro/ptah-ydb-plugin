package backend_test

import (
	"testing"

	qt "github.com/frankban/quicktest"
	protocol "ptah.run/provider-sdk/protocol/experimental"

	"github.com/stokaro/ptah-ydb-plugin/internal/backend"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
)

func TestDescriptionDeclaresReleaseLines_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	lines := backend.Description().GetEngines()[0].GetReleaseLines()
	got := make([]string, len(lines))
	for i, line := range lines {
		got[i] = line.GetLine() + " " + line.GetLabel() + " " + line.GetSupport().String() + " " + line.GetImage()
	}
	// The labels are the names the host's embedded engine gave the lines'
	// presets, which `ptah db capabilities` prints.
	c.Assert(got, qt.DeepEquals, []string{
		"26.2 YDB262 CERTIFIED ydbplatform/local-ydb:26.2",
		"26.1 YDB261 BEST_EFFORT ydbplatform/local-ydb:26.1",
		"25.4 YDB254 BEST_EFFORT ydbplatform/local-ydb:25.4",
		"25.3 YDB253 BEST_EFFORT ydbplatform/local-ydb:25.3",
		"25.2 YDB252 BEST_EFFORT ydbplatform/local-ydb:25.2",
		"25.1 YDB251 CERTIFIED ydbplatform/local-ydb:25.1",
	})
}

func TestDescriptionReleaseLinesStateWhyTheyRunNoJob_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	for _, line := range backend.Description().GetEngines()[0].GetReleaseLines() {
		c.Assert(line.GetUnprobed() != "", qt.Equals, line.GetSupport() == protocol.ReleaseLine_BEST_EFFORT, qt.Commentf("line %s", line.GetLine()))
	}
}
