//go:build integration

package integration_test

import (
	"context"
	"path"
	"slices"
	"strings"
	"testing"

	qt "github.com/frankban/quicktest"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"google.golang.org/protobuf/proto"

	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
)

// atlasRevisionVersions are the revision identities the host's
// TestVersionLiteral_YDB and TestVersionLiteral_YDBReadsBackUnderTheYQLLexer
// wrote: a plain version, a quote and a backslash, a doubled quote, a
// semicolon and comment markers, a double quote, a newline, at signs, an
// escaped quote, and a letter outside ASCII.
var atlasRevisionVersions = []string{
	"20260103120000", `20260103_a\b'c`, `a''b`, "a;--b/*c", `a\b'c"d`, "a\nb", "@@x@@", `\'`, "ü",
}

// Ported from the host's TestVersionLiteral_YDB and
// TestVersionLiteral_YDBReadsBackUnderTheYQLLexer, which pinned how the
// built-in engine quoted an Atlas revision identity so the statement naming a
// revision named the one it was given, whatever it held. ptah-compat records a
// revision through the provider's history service, and only a migration file
// whose name starts with digits is a revision there, so the identities the
// host test wrote reach the provider through the history calls a host makes,
// not through a file name. Each is recorded, read back byte for byte in
// version order by a fresh session, and removed by a compare-and-swap that
// names it, which matches only if the statement identified the row exactly.
func TestNativeHistoryKeepsEveryAtlasVersionAsWritten(t *testing.T) {
	c := testcheck.New(t)
	binary, digest := providerBinary(c)
	opened, directory, database := inspectionFixture(c)
	table := path.Join(database, directory, "revisions")
	client, id, closeSession := historySession(c, binary, digest)
	scope := &protocol.HistoryScope{SessionId: id, Schema: directory, Table: "revisions", Format: protocol.HistoryScope_ATLAS}
	prepared, err := client.PrepareHistory(c.Context(), &protocol.PrepareHistoryRequest{Scope: scope, RequestId: "prepare"})
	c.Assert(err, qt.IsNil)
	c.Assert(prepared.Outcome, qt.Equals, protocol.HistoryResult_APPLIED)
	c.Cleanup(func() {
		_, err := opened.DB.ExecContext(context.Background(), "DROP TABLE `"+table+"`")
		c.Check(err, qt.IsNil)
	})
	rows := make([]*protocol.HistoryRevision, len(atlasRevisionVersions))
	for i, version := range atlasRevisionVersions {
		rows[i] = historyRow(protocol.HistoryScope_ATLAS, version)
		historyChange(c, client, scope, nil, rows[i], protocol.HistoryResult_APPLIED)
	}
	closeSession()
	client.Close()
	fresh, freshID, _ := historySession(c, binary, digest)
	freshScope := proto.CloneOf(scope)
	freshScope.SessionId = freshID

	read := historyRead(c, fresh, freshScope)
	for _, row := range rows {
		historyChange(c, fresh, freshScope, row, nil, protocol.HistoryResult_APPLIED)
	}
	emptied := historyRead(c, fresh, freshScope)

	ordered := slices.Clone(rows)
	slices.SortFunc(ordered, func(a, b *protocol.HistoryRevision) int { return strings.Compare(a.GetVersion(), b.GetVersion()) })
	c.Assert(read.Revisions, qt.HasLen, len(ordered))
	for i, want := range ordered {
		c.Assert(proto.Equal(read.Revisions[i], want), qt.IsTrue, qt.Commentf("revision %d: read %q, wrote %q", i, read.Revisions[i].GetVersion(), want.GetVersion()))
	}
	c.Assert(emptied.Revisions, qt.HasLen, 0)
}
