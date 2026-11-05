package tablecreate_test

import (
	"testing"

	qt "github.com/frankban/quicktest"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/stokaro/ptah-ydb-plugin/internal/backend"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
)

// statusReasons returns the reasons a status carries.
func statusReasons(c *qt.C, err error) []string {
	c.Helper()
	var reasons []string
	for _, detail := range status.Convert(err).Details() {
		diagnostic, ok := detail.(*protocol.ProviderDiagnostic)
		c.Assert(ok, qt.IsTrue, qt.Commentf("detail %T", detail))
		reasons = append(reasons, diagnostic.GetReason())
	}
	return reasons
}

// checkConstraint is a CHECK constraint, which YDB does not hold.
func checkConstraint() []*protocol.DeclaredConstraint {
	return []*protocol.DeclaredConstraint{{Name: "ck_records", Type: "CHECK", CheckExpression: "id > 0"}}
}

// A declaration the provider cannot create refuses table preparation with the
// provider's sentence as the reason, after the table it names: the sentence
// schema validation reports for the same declaration, which quotes no value.
func TestPreparationRefusesADeclarationWithAReason(t *testing.T) {
	c := testcheck.New(t)
	request := preparationRequest(c)
	request.Tables[0].Desired.Constraints = checkConstraint()
	result, err := backend.New().PrepareTables(c.Context(), request)
	c.Assert(result, qt.IsNil)
	c.Assert(status.Code(err), qt.Equals, codes.InvalidArgument)
	c.Assert(statusReasons(c, err), qt.DeepEquals, []string{"table app/records: YDB table creation does not support ptah.provider.experimental.DeclaredTable.constraints"})
}

// The creation projection refuses the same declaration with the same reason.
func TestProjectionRefusesADeclarationWithAReason(t *testing.T) {
	c := testcheck.New(t)
	request := projectionRequest(c)
	request.Tables[0].Declaration.Constraints = checkConstraint()
	result, err := backend.New().ProjectTableCreations(c.Context(), request)
	c.Assert(result, qt.IsNil)
	c.Assert(status.Code(err), qt.Equals, codes.InvalidArgument)
	c.Assert(statusReasons(c, err), qt.DeepEquals, []string{"table app/records: YDB table creation does not support ptah.provider.experimental.DeclaredTable.constraints"})
}
