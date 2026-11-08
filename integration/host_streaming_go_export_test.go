//go:build integration

package integration_test

import (
	"path"
	"strings"
	"testing"

	qt "github.com/frankban/quicktest"

	"github.com/stokaro/ptah-ydb-plugin/internal/streaming"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
)

// The host's Go export of a streaming query must keep newlines and both
// kinds of quotes in its text. Reading the exported Go source through the
// host must describe the same query, with no replacement to plan. This ports
// the text round-trip of the former host's TestRender_StreamingQuery_RoundTrip.
func TestHostGoExportKeepsStreamingQueryText_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	opened, directory, _, held := streamingFixture(c)
	input := path.Join(directory, "input")
	output := path.Join(directory, "output")
	text := "INSERT INTO `" + output + "`\nSELECT * FROM `" + input + "`\nWHERE \"it's a stream\" = \"it's a stream\";"
	// CREATE STREAMING QUERY wraps its body in DO BEGIN and YDB keeps the
	// newline on each side of the body when it reports the stored query.
	stored := "\n" + text + "\n"
	for _, statement := range []string{
		"CREATE TOPIC `" + input + "`",
		"CREATE TOPIC `" + output + "`",
		streaming.Create(directory, "copy", streaming.Spec{Text: text}),
	} {
		c.Assert(execFixtureSchema(c.Context(), opened.DB, statement), qt.IsNil, qt.Commentf("%s", statement))
	}
	c.Assert(held().Text, qt.Equals, stored)

	source := introspectDirectory(c, directory)
	c.Assert(source, qt.Contains, `//ptah:schema:streamingquery name="copy"`, qt.Commentf("%s", source))
	c.Assert(source, qt.Contains, `it's a stream`, qt.Commentf("%s", source))
	c.Assert(source, qt.Contains, `\nSELECT`, qt.Commentf("%s", source))
	c.Assert(strings.Count(source, `//ptah:schema:streamingquery`), qt.Equals, 1)
	compared, err := hostApplySelected(c, directory, "go", source)
	c.Assert(err, qt.IsNil, qt.Commentf("%s\n%s", source, compared))
	c.Assert(compared, qt.Contains, "Schema is synced, no changes to be made")
	c.Assert(held().Text, qt.Equals, stored)
}
