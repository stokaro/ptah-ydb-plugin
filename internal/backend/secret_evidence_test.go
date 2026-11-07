package backend_test

import (
	"testing"

	qt "github.com/frankban/quicktest"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/stokaro/ptah/provider-sdk/provider"

	"github.com/stokaro/ptah-ydb-plugin/internal/backend"
	native "github.com/stokaro/ptah-ydb-plugin/internal/nativeprotocol"
	"github.com/stokaro/ptah-ydb-plugin/internal/secretcompare"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcomparison"
	"github.com/stokaro/ptah-ydb-plugin/schema"
)

// secretComparison is a comparison of secrets, empty on both sides, each side
// describing the kind completely, on a target with secrets.
func secretComparison() *protocol.CompareModelsRequest {
	return objectComparison(schema.SecretProperties, secretcompare.ServiceID, schema.NativeDesiredModel(schema.SecretProperties),
		map[string]bool{"secrets": true, "transactional_ddl": false})
}

// secretDeclaredAt declares the secret at dir/name, taking its value from the
// variable env.
func secretDeclaredAt(c *qt.C, dir, name, env string) *protocol.Envelope {
	c.Helper()
	value, err := schema.CaptureSecretIntent(&protocol.Reference{Engine: "ydb", Kind: schema.SecretProperties, Schema: dir, Name: name}, &native.SecretIntent{ValueEnv: env})
	c.Assert(err, qt.IsNil)
	return value
}

// secretReadAt is the secret a read finds at /local/dir/name.
func secretReadAt(c *qt.C, dir, name string) *protocol.Envelope {
	c.Helper()
	value, err := schema.CaptureObject(&protocol.Reference{Engine: "ydb", Kind: schema.SecretProperties, Database: "/local", Schema: dir, Name: name}, &native.SecretPresence{})
	c.Assert(err, qt.IsNil)
	return value
}

// Ported from the host's TestSecretComparison_ComparesByPresenceAndEvidence
// and TestSecretComparison_PlansNothingOnALineWithoutSecrets: a secret the
// read could not read is not dropped by a source that claims every secret,
// whether or not the target has secrets; the comparison says why instead.
func TestSecretComparisonWithholdsTheRemovalOfAnUnreadSecret(t *testing.T) {
	for _, test := range []struct {
		name    string
		secrets bool
	}{
		{name: "a line with secrets", secrets: true},
		{name: "a line without secrets"},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			request := secretComparison()
			request.Capabilities["secrets"] = test.secrets
			limitSubject(request.Current, schema.SecretProperties, "/local", "app", "pg.pw", protocol.ModelKnowledge_UNREPRESENTABLE)

			result, err := backend.New().CompareModels(c.Context(), request)

			c.Assert(err, qt.IsNil)
			c.Assert(provider.ValidateComparedModels(backend.Description(), request, result), qt.IsNil)
			c.Assert(result.Changes, qt.HasLen, 0)
			c.Assert(result.Undecided, qt.HasLen, 1)
		})
	}
}

// Ported from the host's TestSecretComparison_KeepsTheHeldSecretAsADeclaration
// and TestCompare_YDBSecretKeptWhereTheDesiredStateCannotNameIt: a source that
// makes no claim about secrets, as an HCL document cannot, drops no secret the
// database holds. It keeps it as a declaration of the default variable for
// the secret's path, which the host's engine selected by naming none.
func TestSecretComparisonKeepsTheHeldSecretAsADeclaration(t *testing.T) {
	c := testcheck.New(t)
	request := secretComparison()
	request.Desired.Coverage.Kinds[0].Sources[0].Outside = testcomparison.Knowledge(protocol.ModelKnowledge_UNINSPECTED)
	addObject(c, request.Current, secretReadAt(c, "app", "pg.pw"))

	result, err := backend.New().CompareModels(c.Context(), request)

	c.Assert(err, qt.IsNil)
	c.Assert(result.Changes, qt.HasLen, 0)
	c.Assert(result.Undecided, qt.HasLen, 0)
	c.Assert(result.Desired.Records, qt.HasLen, 1)
	kept, err := schema.DesiredSecret(result.Desired.Records[0].Value.Value)
	c.Assert(err, qt.IsNil)
	c.Assert(kept.ValueEnv, qt.Equals, "PTAH_SECRET_APP_PG_PW")
	c.Assert(result.Desired.Coverage.Subjects[0].Knowledge.State, qt.Equals, protocol.ModelKnowledge_COMPLETE)
}

// Ported from the host's TestCompare_YDBSecretsCompareByPresence: a secret
// pairs with a secret at the same path, so a root secret and a declared
// secret of the same name in a directory are two secrets: one dropped, the
// other created.
func TestSecretComparisonPairsSecretsByPath(t *testing.T) {
	c := testcheck.New(t)
	request := secretComparison()
	addObject(c, request.Desired, secretDeclaredAt(c, "app", "pg.pw", "PTAH_SECRET_PG"))
	addObject(c, request.Current, secretReadAt(c, "", "pg.pw"))

	result, err := backend.New().CompareModels(c.Context(), request)

	c.Assert(err, qt.IsNil)
	c.Assert(result.Undecided, qt.HasLen, 0)
	c.Assert(changedPaths(result), qt.ContentEquals, []string{"app/pg.pw ADDITIVE", "/pg.pw DESTRUCTIVE"})
}

// Ported from the host's TestCompare_YDBSecretRotatedOnlyWhenAsked: rotation
// requests that name one secret twice, beside a secret of a directory, rotate
// each requested secret once, and a declared secret the database does not
// hold is created.
func TestSecretComparisonRotatesARepeatedRequestOnce(t *testing.T) {
	c := testcheck.New(t)
	request := secretComparison()
	addObject(c, request.Desired, secretDeclaredAt(c, "", "pg.pw", "PTAH_SECRET_PG"))
	addObject(c, request.Desired, secretDeclaredAt(c, "app", "s3", "PTAH_SECRET_S3"))
	addObject(c, request.Desired, secretDeclaredAt(c, "", "fresh", "PTAH_SECRET_FRESH"))
	addObject(c, request.Current, secretReadAt(c, "", "pg.pw"))
	addObject(c, request.Current, secretReadAt(c, "app", "s3"))
	request.Requests = []*protocol.RequestedChange{{Action: "rotate", Written: "pg.pw"}, {Action: "rotate", Written: "app/s3"}, {Action: "rotate", Written: "pg.pw"}}

	result, err := backend.New().CompareModels(c.Context(), request)

	c.Assert(err, qt.IsNil)
	c.Assert(changedPaths(result), qt.ContentEquals, []string{"/fresh ADDITIVE", "/pg.pw BEHAVIORAL", "app/s3 BEHAVIORAL"})
	c.Assert(result.ResolvedRequests, qt.HasLen, 3)
}

// Ported from the host's TestSecretComparison_FailurePath: a declared secret
// on a target without secrets is refused, since CREATE SECRET would fail
// there, with no result.
func TestSecretComparisonRefusesACreationWithoutSecrets(t *testing.T) {
	c := testcheck.New(t)
	request := secretRequest(c, secretAbsent)
	request.Capabilities["secrets"] = false

	result, err := backend.New().CompareModels(c.Context(), request)

	c.Assert(err, qt.ErrorMatches, `.*changing secrets requires secrets.*`)
	c.Assert(result, qt.IsNil)
}
