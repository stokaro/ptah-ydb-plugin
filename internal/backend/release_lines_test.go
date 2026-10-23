package backend_test

import (
	"testing"

	qt "github.com/frankban/quicktest"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"

	"github.com/stokaro/ptah-ydb-plugin/internal/backend"
)

func TestDescriptionDeclaresReleaseLines_HappyPath(t *testing.T) {
	c := qt.New(t)
	lines := backend.Description().GetEngines()[0].GetReleaseLines()
	got := make([]string, len(lines))
	for i, line := range lines {
		got[i] = line.GetLine() + " " + line.GetSupport().String() + " " + line.GetImage()
	}
	c.Assert(got, qt.DeepEquals, []string{
		"26.2 CERTIFIED ydbplatform/local-ydb:26.2",
		"26.1 BEST_EFFORT ydbplatform/local-ydb:26.1",
		"25.4 BEST_EFFORT ydbplatform/local-ydb:25.4",
		"25.3 BEST_EFFORT ydbplatform/local-ydb:25.3",
		"25.2 BEST_EFFORT ydbplatform/local-ydb:25.2",
		"25.1 CERTIFIED ydbplatform/local-ydb:25.1",
	})
}

func TestDescriptionReleaseLinesStateWhyTheyRunNoJob_HappyPath(t *testing.T) {
	c := qt.New(t)
	for _, line := range backend.Description().GetEngines()[0].GetReleaseLines() {
		c.Assert(line.GetUnprobed() != "", qt.Equals, line.GetSupport() == protocol.ReleaseLine_BEST_EFFORT, qt.Commentf("line %s", line.GetLine()))
	}
}
