//go:build integration

package integration_test

import (
	"path/filepath"
	"testing"

	qt "github.com/frankban/quicktest"

	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
)

// omittingEntities declares, in Go, a row table whose columns declare types
// YDB keeps only in part, a column table with one more, and a Serial whose
// sequence starts at 100 and steps by 5.
const omittingEntities = "package entities\n\n" +
	"//ptah:schema:table name=\"accounts\" schema=\"app/sub\"\ntype Account struct {\n" +
	"\t//ptah:schema:field name=\"id\" type=\"BIGINT\" primary=\"true\"\n\tID int64\n" +
	"\t//ptah:schema:field name=\"email\" type=\"VARCHAR(255)\"\n\tEmail string\n" +
	"\t//ptah:schema:field name=\"seen\" type=\"TIMESTAMP(3)\"\n\tSeen int64\n}\n\n" +
	"//ptah:schema:table name=\"events\" schema=\"app\" store=\"COLUMN\"\ntype Event struct {\n" +
	"\t//ptah:schema:field name=\"id\" type=\"BIGINT\" primary=\"true\" not_null=\"true\"\n\tID int64\n" +
	"\t//ptah:schema:field name=\"code\" type=\"CHAR(8)\"\n\tCode string\n}\n\n" +
	"//ptah:schema:table name=\"orders\" schema=\"app\"\ntype Order struct {\n" +
	"\t//ptah:schema:field name=\"id\" type=\"BIGSERIAL\" primary=\"true\" identity_start=\"100\" identity_increment=\"5\"\n\tID int64\n" +
	"\t//ptah:schema:field name=\"note\" type=\"Utf8\"\n\tNote string\n}\n"

// `schema validate --no-skipped` reports what rendering each table loses, as
// the host's embedded engine reported it: the part of a column's declared type
// YDB does not keep (G219), and a Serial's start and increment, which an
// offline render cannot set (G220). The lines are the host's, from built-in
// 74de0b65b on the same entities, and the run exits 1. Without --no-skipped
// the schema is valid.
func TestHostValidatesWhatRenderingLeavesOut_FailurePath(t *testing.T) {
	c := testcheck.New(t)
	dir := writeFiles(c, filepath.Join(c.TempDir(), "entities"), map[string]string{"entities.go": omittingEntities})

	reported, reportErr := hostCommand(c, "schema", "validate", "--root-dir", dir, "--dialect", "ydb", "--no-skipped")
	valid, validErr := hostCommand(c, "schema", "validate", "--root-dir", dir, "--dialect", "ydb")

	c.Assert(reportErr, qt.IsNotNil, qt.Commentf("%s", reported))
	c.Assert(reported, qt.Equals, `ydb: column "app.events.code": type modifier=CHAR(8): length 8 would be skipped`+"\n"+
		`ydb: column "app.orders.id": identity increment=5 would be skipped`+"\n"+
		`ydb: column "app.orders.id": identity start=100 would be skipped`+"\n"+
		`ydb: column "app/sub.accounts.email": type modifier=VARCHAR(255): length 255 would be skipped`+"\n"+
		`ydb: column "app/sub.accounts.seen": type modifier=TIMESTAMP(3): precision 3 (YDB keeps microseconds) would be skipped`+"\n"+
		"5 problems\n")
	c.Assert(validErr, qt.IsNil, qt.Commentf("%s", valid))
}

// An offline render writes a Serial whose sequence states a start and an
// increment as its CREATE TABLE alone, as the host's embedded engine wrote
// it: the ALTER SEQUENCE that sets them names the sequence by its absolute
// path, which an offline render does not know. The render reports both
// settings as omissions (G220) rather than refusing the schema.
func TestHostRendersASerialWithSettingsOffline_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	dir := writeFiles(c, filepath.Join(c.TempDir(), "entities"), map[string]string{"entities.go": omittingEntities})

	out, err := hostCommand(c, "schema", "render", "--root-dir", dir, "--dialect", "ydb")

	c.Assert(err, qt.IsNil, qt.Commentf("%s", out))
	c.Assert(out, qt.Contains, "CREATE TABLE `app/orders` (\n    `id` BigSerial NOT NULL,\n    `note` Utf8,\n    PRIMARY KEY (`id`)\n);")
	c.Assert(out, qt.Contains, "CREATE TABLE `app/sub/accounts` (\n    `id` Int64 NOT NULL,\n    `email` Utf8,\n")
	c.Assert(out, qt.Not(qt.Contains), "ALTER SEQUENCE")
}
