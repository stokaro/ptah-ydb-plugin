package commonobject_test

import (
	"testing"

	qt "github.com/frankban/quicktest"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"

	"github.com/stokaro/ptah-ydb-plugin/internal/commonobject"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
)

// grantLineCapabilities are the access facts of a target that holds every
// native object a grant names. relative says whether the line resolves a
// single name against the database root, which 26.1 and later do.
func grantLineCapabilities(relative bool) map[string]bool {
	return map[string]bool{"role_management": true, "database_grants": true, "relative_grant_paths": relative,
		"topics": true, "coordination_nodes": true, "secrets": true, "external_data_sources": true,
		"async_replication": true, "transfers": true, "streaming_queries": true, "resource_pools": true}
}

// listGrant is a GRANT of LIST on target to app.
func listGrant(target *protocol.Reference) *protocol.CommonObjectChange {
	return &protocol.CommonObjectChange{Operation: &protocol.CommonObjectChange_Grant{Grant: &protocol.SQLGrant{
		Role: "app", Privileges: []string{"LIST"}, Target: target}}}
}

// TestRenderGrantPathOnEachLine_HappyPath pins the path a GRANT names its
// object by. Every line resolves a path with a directory part against the
// database root, so such a path is written relative to it; a single name is
// relative only where the line resolves one, and is written under the
// database's absolute path elsewhere. An absolute path is written as it is,
// and a dot in a name is part of the name. Ported from the host's
// TestGenerateMigrationAST_GrantPaths_HappyPath and
// TestStatementPath_HappyPath.
func TestRenderGrantPathOnEachLine_HappyPath(t *testing.T) {
	tests := []struct {
		name     string
		relative bool
		target   *protocol.Reference
		want     string
	}{
		{name: "a table in a directory, where a single name does not resolve", target: &protocol.Reference{
			Engine: "ydb", Kind: "table", Database: "/local", Schema: "shop", Name: "orders"}, want: "shop/orders"},
		{name: "a directory at the root, where a single name does not resolve", target: &protocol.Reference{
			Engine: "ydb", Kind: "schema", Database: "/local", Name: "shop"}, want: "/local/shop"},
		{name: "a directory at the root, where a single name resolves", relative: true, target: &protocol.Reference{
			Engine: "ydb", Kind: "schema", Name: "shop"}, want: "shop"},
		{name: "a directory at the root of a nested database", target: &protocol.Reference{
			Engine: "ydb", Kind: "schema", Database: "/Root/db", Name: "shop"}, want: "/Root/db/shop"},
		{name: "a nested directory, where a single name does not resolve", target: &protocol.Reference{
			Engine: "ydb", Kind: "schema", Name: "shop/eu"}, want: "shop/eu"},
		{name: "a table whose name holds a dot", relative: true, target: &protocol.Reference{
			Engine: "ydb", Kind: "table", Name: "a.b"}, want: "a.b"},
		{name: "an absolute path a plan already wrote", target: &protocol.Reference{
			Engine: "ydb", Kind: "table", Name: "/local/events"}, want: "/local/events"},
		{name: "a topic in a directory, where a single name does not resolve", target: &protocol.Reference{
			Engine: "ydb", Kind: "topic", Schema: "shop", Name: "events"}, want: "shop/events"},
		{name: "a coordination node in a nested directory", target: &protocol.Reference{
			Engine: "ydb", Kind: "coordination node", Schema: "app/eu", Name: "locks"}, want: "app/eu/locks"},
		{name: "a root secret, where a single name does not resolve", target: &protocol.Reference{
			Engine: "ydb", Kind: "secret", Database: "/local", Name: "pw"}, want: "/local/pw"},
		{name: "a root external data source, where a single name resolves", relative: true, target: &protocol.Reference{
			Engine: "ydb", Kind: "external data source", Name: "bucket"}, want: "bucket"},
		{name: "an external table whose name holds a dot", target: &protocol.Reference{
			Engine: "ydb", Kind: "external table", Schema: "ext", Name: "a.b"}, want: "ext/a.b"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			got, err := commonobject.Render(listGrant(test.target), grantLineCapabilities(test.relative))
			c.Assert(err, qt.IsNil)
			c.Assert(got, qt.Equals, "GRANT 'ydb.generic.list' ON `"+test.want+"` TO `app`;")
		})
	}
}

// TestRenderGrantPath_FailurePath pins what has no path a GRANT can write: an
// object only an absolute path reaches with no database path to write it
// from, which the database itself always is; the database named by a name;
// the cluster root; an object with no name; and a kind a YDB grant is not
// about. A path written inexactly, such as a directory with a trailing slash,
// is refused rather than rewritten: the read reports the directory without
// one, so a rewritten declaration would never match what the read reports.
// Ported from the host's TestStatementPath_FailurePath,
// TestStatementPath_NeedsTheDatabasePath and the database rows of
// TestGenerateMigrationAST_AccessChanges_FailurePath.
func TestRenderGrantPath_FailurePath(t *testing.T) {
	tests := []struct {
		name     string
		relative bool
		target   *protocol.Reference
		want     string
	}{
		{name: "the database without its path", relative: true, target: &protocol.Reference{Engine: "ydb", Kind: "database"},
			want: "YDB takes this object only by its absolute path, which begins with the database's own path; plan it against the database, which names it by that path"},
		{name: "a root topic where a single name does not resolve", target: &protocol.Reference{Engine: "ydb", Kind: "topic", Name: "events"},
			want: "YDB takes this object only by its absolute path, which begins with the database's own path; plan it against the database, which names it by that path"},
		{name: "an inexact database path", target: &protocol.Reference{Engine: "ydb", Kind: "table", Database: "/Root/db/", Name: "events"},
			want: "YDB takes this object only by its absolute path, which begins with the database's own path; plan it against the database, which names it by that path"},
		{name: "the database by a name", relative: true, target: &protocol.Reference{Engine: "ydb", Kind: "database", Database: "/local", Name: "local"},
			want: "a database grant names the database the connection opens"},
		{name: "a directory with no name", relative: true, target: &protocol.Reference{Engine: "ydb", Kind: "schema", Name: "/"},
			want: "a YDB grant target names the cluster root, which is no object of the database; " +
				"the database root is the database itself, which a database grant names"},
		{name: "a directory with a trailing slash", relative: true, target: &protocol.Reference{Engine: "ydb", Kind: "schema", Name: "shop/eu/"},
			want: "a YDB grant requires an exact object path"},
		{name: "a table with no name", relative: true, target: &protocol.Reference{Engine: "ydb", Kind: "table"},
			want: "a YDB grant requires an exact object path"},
		{name: "a topic with no name", relative: true, target: &protocol.Reference{Engine: "ydb", Kind: "topic"},
			want: "a YDB grant requires an exact object path"},
		{name: "a sequence", relative: true, target: &protocol.Reference{Engine: "ydb", Kind: "sequence", Name: "s"},
			want: `a YDB grant cannot name object kind "sequence"`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			got, err := commonobject.Render(listGrant(test.target), grantLineCapabilities(test.relative))
			c.Assert(err, qt.ErrorIs, commonobject.ErrUnsupported)
			c.Assert(err.Error(), qt.Contains, test.want)
			c.Assert(got, qt.Equals, "")
		})
	}
}
