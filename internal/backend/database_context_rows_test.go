package backend_test

import (
	"testing"

	qt "github.com/frankban/quicktest"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/testing/protocmp"

	"github.com/stokaro/ptah-ydb-plugin/internal/backend"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
)

// The database a connection URL names is its root, whether the URL names it
// by its path, by the database parameter, or by both alike. Ported from the
// host's TestYQLPrivilegeDatabaseContext, TestSourceRoot_HappyPath and
// TestSetDiffRoot_HappyPath.
func TestDatabaseContextReadsTheRootAURLNames(t *testing.T) {
	for _, test := range []struct {
		name, address, database string
	}{
		{"the database parameter", "ydb://host:2136/?database=/Root/db", "/Root/db"},
		{"a path of two segments", "ydb://host:2136/Root/db", "/Root/db"},
		{"a path and the parameter alike", "ydb://host:2136/Root/db?database=/Root/db", "/Root/db"},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			result, err := backend.New().ResolveDatabaseContext(c.Context(), &protocol.DatabaseContextRequest{Engine: "ydb", Address: test.address})
			c.Assert(err, qt.IsNil)
			c.Assert(result, qt.CmpEquals(protocmp.Transform()), &protocol.DatabaseContext{Database: test.database, SourceDatabase: test.database})
		})
	}
}

// A URL that names no host, a path that conflicts with the database
// parameter, and an address of another engine name no root. Ported from the
// host's TestYQLPrivilegeInvalidDatabaseContext, TestSourceRoot_FailurePath
// and TestSetDiffRoot_FailurePath.
func TestDatabaseContextRefusesAURLThatNamesNoRoot(t *testing.T) {
	for _, test := range []struct {
		name, address, want string
	}{
		{"no host", "ydb:///local", ".*invalid YDB connection URL"},
		{"a path the parameter contradicts", "ydb://host:2136/local?database=/Root/db", ".*YDB URL database path and parameter must identify the same nonempty database"},
		{"another engine", "postgres://host:5432/dev", ".*YDB requires a ydb:// or ydbs:// address"},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			result, err := backend.New().ResolveDatabaseContext(c.Context(), &protocol.DatabaseContextRequest{Engine: "ydb", Address: test.address})
			c.Assert(err, qt.ErrorMatches, test.want)
			c.Assert(result, qt.IsNil)
		})
	}
}

// The former host's TestLoadPath_RefusesASourceContextWithNoRoot reached the
// YDB source-root resolver with these two URLs. The provider's database
// context call now owns that decision and returns no partial context.
func TestDatabaseContextRefusesAnInvalidSourceRoot(t *testing.T) {
	for _, test := range []struct {
		name, address, want string
	}{
		{"invalid YDB port", "ydb://user:CONTEXT_SENTINEL@example:bad/local", ".*invalid YDB connection URL.*"},
		{"another Docker dialect", "docker://postgres/17/dev", ".*dev database spec names no YDB database.*"},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			context, err := backend.New().ResolveDatabaseContext(c.Context(), &protocol.DatabaseContextRequest{Engine: "ydb", Address: test.address})
			c.Assert(context, qt.IsNil)
			c.Assert(status.Code(err), qt.Equals, codes.InvalidArgument)
			c.Assert(err, qt.ErrorMatches, test.want)
			c.Assert(err.Error(), qt.Not(qt.Contains), "CONTEXT_SENTINEL")
		})
	}
}
