package featureflags_test

import (
	"testing"

	qt "github.com/frankban/quicktest"

	"github.com/stokaro/ptah-ydb-plugin/internal/featureflags"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
)

// A cluster that turned EnableAsyncIndexes off keeps the line's preset: the
// flag gates no capability. Ported from the host's
// TestRefine_AsyncIndexesFlagOffKeepsThePreset.
func TestRefine_AsyncIndexesFlagOffKeepsThePreset(t *testing.T) {
	for _, test := range []struct {
		page string
		line releaseLine
	}{
		{page: "local-ydb-26.2.1.14-async-indexes-off.json", line: line262},
		{page: "local-ydb-25.1.4.7-async-indexes-off.json", line: line251},
	} {
		t.Run(test.page, func(t *testing.T) {
			c := testcheck.New(t)
			flags, err := featureflags.Decode(page(c, test.page), "/local")
			c.Assert(err, qt.IsNil)
			c.Assert(flags["EnableAsyncIndexes"], qt.IsFalse)
			c.Assert(flags.Refine(linePreset(test.line)), qt.DeepEquals, linePreset(test.line))
		})
	}
}
