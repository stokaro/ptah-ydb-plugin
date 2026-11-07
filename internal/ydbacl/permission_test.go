package ydbacl_test

import (
	"testing"

	qt "github.com/frankban/quicktest"

	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
	"github.com/stokaro/ptah-ydb-plugin/internal/ydbacl"
)

// permissionNames are the 25 permission names YDB takes in GRANT, sorted.
var permissionNames = []string{
	"ydb.access.grant",
	"ydb.database.connect", "ydb.database.create", "ydb.database.drop",
	"ydb.generic.full", "ydb.generic.full_legacy", "ydb.generic.list", "ydb.generic.manage",
	"ydb.generic.read", "ydb.generic.use", "ydb.generic.use_legacy", "ydb.generic.write",
	"ydb.granular.alter_schema", "ydb.granular.create_directory", "ydb.granular.create_queue",
	"ydb.granular.create_table", "ydb.granular.describe_schema", "ydb.granular.erase_row",
	"ydb.granular.read_attributes", "ydb.granular.remove_schema", "ydb.granular.select_row",
	"ydb.granular.update_row", "ydb.granular.write_attributes",
	"ydb.tables.modify", "ydb.tables.read",
}

// TestPermission_HappyPath pins the keyword each permission name is written
// with, measured by the host on 25.1.4.7 and 26.2.1.14: `GRANT <keyword> ...`
// stores the name beside it. A keyword is read in any case and with any
// spacing, and a name in any case. Ported from the host's
// TestPermission_HappyPath.
func TestPermission_HappyPath(t *testing.T) {
	for _, tc := range []struct {
		privilege string
		want      string
	}{
		{privilege: "CONNECT", want: "ydb.database.connect"},
		{privilege: "CREATE", want: "ydb.database.create"},
		{privilege: "DROP", want: "ydb.database.drop"},
		{privilege: "SELECT ROW", want: "ydb.granular.select_row"},
		{privilege: "UPDATE ROW", want: "ydb.granular.update_row"},
		{privilege: "ERASE ROW", want: "ydb.granular.erase_row"},
		{privilege: "SELECT ATTRIBUTES", want: "ydb.granular.read_attributes"},
		{privilege: "MODIFY ATTRIBUTES", want: "ydb.granular.write_attributes"},
		{privilege: "CREATE DIRECTORY", want: "ydb.granular.create_directory"},
		{privilege: "CREATE TABLE", want: "ydb.granular.create_table"},
		{privilege: "CREATE QUEUE", want: "ydb.granular.create_queue"},
		{privilege: "REMOVE SCHEMA", want: "ydb.granular.remove_schema"},
		{privilege: "DESCRIBE SCHEMA", want: "ydb.granular.describe_schema"},
		{privilege: "ALTER SCHEMA", want: "ydb.granular.alter_schema"},
		{privilege: "GRANT", want: "ydb.access.grant"},
		{privilege: "SELECT", want: "ydb.generic.read"},
		{privilege: "INSERT", want: "ydb.generic.write"},
		{privilege: "LIST", want: "ydb.generic.list"},
		{privilege: "USE LEGACY", want: "ydb.generic.use_legacy"},
		{privilege: "USE", want: "ydb.generic.use"},
		{privilege: "MANAGE", want: "ydb.generic.manage"},
		{privilege: "FULL LEGACY", want: "ydb.generic.full_legacy"},
		{privilege: "FULL", want: "ydb.generic.full"},
		{privilege: "MODIFY TABLES", want: "ydb.tables.modify"},
		{privilege: "SELECT TABLES", want: "ydb.tables.read"},
		{privilege: "ALL", want: "ydb.generic.full"},
		{privilege: "all  privileges", want: "ydb.generic.full"},
		{privilege: " select\trow ", want: "ydb.granular.select_row"},
		{privilege: "ydb.granular.select_row", want: "ydb.granular.select_row"},
		{privilege: "YDB.GENERIC.LIST", want: "ydb.generic.list"},
	} {
		t.Run(tc.privilege, func(t *testing.T) {
			c := testcheck.New(t)
			got, ok := ydbacl.Permission(tc.privilege)
			c.Assert(ok, qt.IsTrue)
			c.Assert(got, qt.Equals, tc.want)
		})
	}
}

// TestPermission_FailurePath pins that a privilege YDB has no permission for
// names none, PostgreSQL's spellings among them, rather than the nearest one.
// Ported from the host's TestPermission_FailurePath.
func TestPermission_FailurePath(t *testing.T) {
	for _, privilege := range []string{"", "  ", "TRUNCATE", "USAGE", "ydb.bogus.thing", "SELECTROW", "ydb.generic"} {
		t.Run(privilege, func(t *testing.T) {
			c := testcheck.New(t)
			got, ok := ydbacl.Permission(privilege)
			c.Assert(ok, qt.IsFalse)
			c.Assert(got, qt.Equals, "")
		})
	}
}

// TestPermission_EveryNameResolvesToItself pins that each of the 25
// permission names reads back as itself, both as a declared privilege and as
// a quoted permission, so a grant read from YDB and the declaration it came
// from name one permission. Ported from the host's TestPermissions; the host's
// sorted list of names had no caller outside its tests, and the plugin keeps
// none.
func TestPermission_EveryNameResolvesToItself(t *testing.T) {
	for _, name := range permissionNames {
		t.Run(name, func(t *testing.T) {
			c := testcheck.New(t)
			declared, declaredOK := ydbacl.Permission(name)
			literal, literalOK := ydbacl.LiteralPermission(name)
			c.Assert(declaredOK, qt.IsTrue)
			c.Assert(declared, qt.Equals, name)
			c.Assert(literalOK, qt.IsTrue)
			c.Assert(literal, qt.Equals, name)
		})
	}
}

// TestDescribesSchema pins the permissions whose holders a new secret gives
// ydb.granular.describe_schema, measured by the host on 26.2.1.14 by granting
// each permission alone on a directory and creating a secret in it: seven of
// the 25 do. Ported from the host's TestDescribesSchema.
func TestDescribesSchema(t *testing.T) {
	describing := map[string]bool{
		"ydb.generic.full": true, "ydb.generic.full_legacy": true, "ydb.generic.list": true, "ydb.generic.read": true,
		"ydb.generic.use": true, "ydb.generic.use_legacy": true, "ydb.granular.describe_schema": true,
	}
	for _, name := range permissionNames {
		t.Run(name, func(t *testing.T) {
			c := testcheck.New(t)
			c.Assert(ydbacl.DescribesSchema(name), qt.Equals, describing[name])
		})
	}
}

// TestLiteralPermission_HappyPath pins the quoted permissions a GRANT reads:
// a full name exactly as YDB spells it, and a short alias in any case with an
// underscore where the keyword has a space. Ported from the host's
// TestLiteralPermission.
func TestLiteralPermission_HappyPath(t *testing.T) {
	for _, tc := range []struct {
		literal, want string
	}{
		{"ydb.generic.read", "ydb.generic.read"},
		{"SELECT", "ydb.generic.read"},
		{"Select", "ydb.generic.read"},
		{"select_row", "ydb.granular.select_row"},
		{"MODIFY_TABLES", "ydb.tables.modify"},
		{"FULL", "ydb.generic.full"},
	} {
		t.Run(tc.literal, func(t *testing.T) {
			c := testcheck.New(t)
			got, ok := ydbacl.LiteralPermission(tc.literal)
			c.Assert(ok, qt.IsTrue)
			c.Assert(got, qt.Equals, tc.want)
		})
	}
}

// TestLiteralPermission_FailurePath pins the quoted permissions that name
// none: ALL, which is a keyword and no permission name, a keyword written
// with its space, a full name in another case, and a leading space. Ported
// from the host's TestLiteralPermission.
func TestLiteralPermission_FailurePath(t *testing.T) {
	for _, literal := range []string{"ALL", "ALL PRIVILEGES", "SELECT ROW", "YDB.GENERIC.READ", " select"} {
		t.Run(literal, func(t *testing.T) {
			c := testcheck.New(t)
			got, ok := ydbacl.LiteralPermission(literal)
			c.Assert(ok, qt.IsFalse)
			c.Assert(got, qt.Equals, "")
		})
	}
}
