//go:build integration

package integration_test

import (
	"path/filepath"
	"strings"
	"testing"

	qt "github.com/frankban/quicktest"

	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
)

// A UNIQUE over every key column folds even when it names them in another
// order. A UNIQUE over only part of the key still needs an index of its own.
// This ports the composite-key cases of the former host's
// TestRender_UniqueConstraint_HappyPath through authored schema rendering.
func TestHostRendersUniqueCompositeKey_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	source := `package entities

//ptah:schema:constraint name="uq_ba" type="UNIQUE" columns="b,a"
//ptah:schema:table name="pairs" schema="shop" primary_key="a,b"
type Pair struct {
	//ptah:schema:field name="a" type="BIGINT" primary="true" unique="true"
	A int64
	//ptah:schema:field name="b" type="BIGINT" primary="true"
	B int64
}
`
	dir := writeFiles(c, filepath.Join(c.TempDir(), "entities"), map[string]string{"pairs.go": source})
	for _, version := range []string{"25.1.4.7", "26.2.1.14"} {
		t.Run(version, func(t *testing.T) {
			c := testcheck.New(t)
			out, err := hostCommand(c, "schema", "render", "--root-dir", dir, "--dialect", "ydb", "--server-version", version)
			c.Assert(err, qt.IsNil, qt.Commentf("%s", out))
			c.Assert(out, qt.Contains, "PRIMARY KEY (`a`, `b`)")
			c.Assert(out, qt.Contains, "INDEX `pairs_a_key` GLOBAL UNIQUE SYNC ON (`a`)")
			c.Assert(out, qt.Not(qt.Contains), "uq_ba")
		})
	}
}

// Invalid UNIQUE declarations are refused before schema render writes a
// partial script. These are source-expressible cases of the former host's
// TestRender_UniqueConstraint_FailurePath.
func TestHostRefusesUnsupportedUniqueSource_FailurePath(t *testing.T) {
	base := uniqueAccounts("shop")
	constraint := `//ptah:schema:constraint name="uq_accounts_tenant_login" type="UNIQUE" table="accounts" columns="tenant,login"`
	field := `//ptah:schema:field name="login"`
	for _, test := range []struct {
		name, source, want string
	}{
		{"NULLS NOT DISTINCT", strings.Replace(base, constraint, constraint+` nulls_distinct="false"`, 1), "DeclaredIndex.nulls_distinct"},
		{"partial unique index", strings.Replace(base, field, `//ptah:schema:index name="uq_partial" fields="login" unique="true" condition="login IS NOT NULL"`+"\n\t"+field, 1), "DeclaredIndex.condition"},
		{"duplicate index name", strings.Replace(base, field, `//ptah:schema:index name="uq_accounts_tenant_login" fields="login"`+"\n\t"+field, 1), "two of its indexes are named"},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			dir := writeFiles(c, filepath.Join(c.TempDir(), "entities"), map[string]string{"accounts.go": test.source})
			for _, version := range []string{"25.1.4.7", "26.2.1.14"} {
				t.Run(version, func(t *testing.T) {
					c := testcheck.New(t)
					out, err := hostCommand(c, "schema", "render", "--root-dir", dir, "--dialect", "ydb", "--server-version", version)
					c.Assert(err, qt.IsNotNil, qt.Commentf("%s", out))
					c.Assert(out, qt.Contains, test.want)
					c.Assert(out, qt.Not(qt.Contains), "CREATE TABLE `shop/accounts`")
				})
			}
		})
	}
}
