//go:build integration

package integration_test

import (
	"testing"

	qt "github.com/frankban/quicktest"
	"github.com/stokaro/ptah-ydb-plugin/internal/nativeapi"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
	"github.com/stokaro/ptah/provider-sdk/provider"
)

// A Go source whose table the provider cannot create is refused when the host
// projects the creation, and a host that negotiates errors.reasons prints the
// provider's reason after the code: the table, and what the provider does not
// create. The refused run writes nothing.
func TestHostPrintsTheReasonOfARefusedCreation_FailurePath(t *testing.T) {
	c := testcheck.New(t)
	host, manifest := hostManifest(c)
	if !hostNegotiates(c, host, manifest, provider.ErrorReasonsFeature) {
		c.Skip("the selected host did not negotiate errors.reasons")
	}
	opened, directory, absolute := itemsFixture(c)
	source := "package entities\n\n//ptah:schema:constraint name=\"ck_items_price\" type=\"CHECK\" table=\"items\" check=\"price > 0\"\n" +
		"//ptah:schema:table name=\"items\" schema=\"" + directory + "\"\ntype Item struct {\n" +
		"\t//ptah:schema:field name=\"id\" type=\"BIGINT\" primary=\"true\"\n\tID int64\n" +
		"\t//ptah:schema:field name=\"price\" type=\"BIGINT\"\n\tPrice int64\n}\n"
	output, err := hostApply(c, directory, source)
	c.Assert(err, qt.IsNotNil)
	c.Assert(output, qt.Matches, `(?s).*provider project table creations failed \(InvalidArgument\): table [^ ]+/items: `+
		`YDB table creation does not support ptah\.provider\.experimental\.DeclaredTable\.constraints.*`)
	c.Assert(output, qt.Not(qt.Contains), "price > 0")
	session, err := nativeapi.OpenTable(c.Context(), opened.Driver)
	c.Assert(err, qt.IsNil)
	c.Cleanup(func() { c.Check(session.Close(), qt.IsNil) })
	_, err = session.Describe(c.Context(), absolute)
	c.Assert(err, qt.IsNotNil)
}
