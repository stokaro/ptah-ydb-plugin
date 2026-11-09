package schemavalidation_test

import (
	"testing"

	qt "github.com/frankban/quicktest"
	protocol "ptah.run/provider-sdk/protocol/experimental"
	"ptah.run/provider-sdk/provider"

	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
)

// TestValidateSchemaReportsEveryAccessProblem_FailurePath pins that a schema
// with several access mistakes is refused in one validation, naming each:
// every declared principal and every declared grant is checked, rather than
// the first refusal ending the check. Ported from the host's
// TestValidateDeclared_ReportsEveryProblem.
func TestValidateSchemaReportsEveryAccessProblem_FailurePath(t *testing.T) {
	c := testcheck.New(t)
	request := newRequest()
	request.TargetFeatures = []string{provider.CommonObjectsFeature, provider.CommonAccessFeature}
	request.CommonObjects = &protocol.DeclaredCommonObjects{
		Roles:  []*protocol.SQLRole{{Name: "b_user"}, {Name: "A"}},
		Grants: []*protocol.SQLGrant{{Role: "app", Privileges: []string{"USAGE"}, Target: &protocol.Reference{Engine: "ydb", Kind: "schema", Name: "s"}}},
	}
	var found []string
	for _, diagnostic := range diagnostics(c, request) {
		found = append(found, diagnostic.Code.String()+" "+diagnostic.Kind+" "+diagnostic.Object+": "+diagnostic.Message)
	}
	c.Assert(found, qt.DeepEquals, []string{
		`UNSUPPORTED_FEATURE grant app: unsupported feature: "USAGE" is not a YDB permission`,
		`UNSUPPORTED_FEATURE role A: unsupported feature: principal "A": YDB takes a user or group name of lower-case ASCII letters and digits only: "A" holds 'A'`,
		`UNSUPPORTED_FEATURE role b_user: unsupported feature: principal "b_user": YDB takes a user or group name of lower-case ASCII letters and digits only: "b_user" holds '_'`,
	})
}
