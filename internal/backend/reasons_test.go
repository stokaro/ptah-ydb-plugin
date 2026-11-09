package backend_test

import (
	"testing"

	qt "github.com/frankban/quicktest"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	protocol "ptah.run/provider-sdk/protocol/experimental"
	"ptah.run/provider-sdk/provider"

	"github.com/stokaro/ptah-ydb-plugin/internal/backend"
	"github.com/stokaro/ptah-ydb-plugin/internal/replication"
	"github.com/stokaro/ptah-ydb-plugin/internal/replicationcompare"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
	"github.com/stokaro/ptah-ydb-plugin/schema"
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

// The provider lists errors.reasons, so a host that lists it too prints the
// reasons the provider writes.
func TestDescriptionListsErrorReasons(t *testing.T) {
	c := testcheck.New(t)
	c.Assert(backend.Description().GetFeatures(), qt.Contains, provider.ErrorReasonsFeature)
}

// An address the provider refuses before it connects says why, and the reason
// repeats nothing the address carried.
func TestOpenRefusesAnAddressWithAReason(t *testing.T) {
	tests := []struct {
		name, address, reason string
	}{
		{name: "another scheme", address: "postgres://admin:hunter2@db:2136/local",
			reason: "YDB requires a ydb:// or ydbs:// address"},
		{name: "a password and a token", address: "ydb://admin:hunter2@db:2136/local?token=s3cr3t",
			reason: "provide exactly one nonempty YDB credential source"},
		{name: "no database", address: "ydb://admin:hunter2@db:2136",
			reason: "YDB URL must name a database"},
		{name: "a parameter that is not a boolean", address: "ydb://db:2136/local?use_env_credentials=hunter2",
			reason: "use_env_credentials must be a boolean"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)

			session, err := backend.New().Open(c.Context(), &protocol.OpenRequest{Engine: "ydb", Address: test.address})

			c.Assert(session, qt.IsNil)
			c.Assert(status.Code(err), qt.Equals, codes.InvalidArgument)
			c.Assert(statusReasons(c, err), qt.DeepEquals, []string{test.reason})
			c.Assert(status.Convert(err).Message(), qt.Not(qt.Contains), "hunter2")
		})
	}
}

// A comparison the target's capabilities refuse carries the provider's
// sentence as the reason.
func TestComparisonRefusesAMissingCapabilityWithAReason(t *testing.T) {
	c := testcheck.New(t)
	request := objectRequest(c, schema.TransferProperties, replicationcompare.TransferServiceID, "ingest", declaredTransfer(c, new(ingest())), nil)
	request.Capabilities[replication.CapabilityTransfers] = false

	result, err := backend.New().CompareModels(c.Context(), request)

	c.Assert(result, qt.IsNil)
	c.Assert(status.Code(err), qt.Equals, codes.InvalidArgument)
	c.Assert(statusReasons(c, err), qt.DeepEquals, []string{"changing transfers requires transfers; YDB keeps it behind feature flag EnableTopicTransfer: " +
		"turn the flag on, and name the cluster's monitoring endpoint in the URL (monitoring=http://host:8765) so Ptah reads the flags before it plans"})
}

// A refusal of a request the provider cannot read keeps its text out of the
// reason: the SDK's checks and the provider's own decoding are not reasons.
func TestComparisonRefusesAnInvalidRequestWithoutAReason(t *testing.T) {
	c := testcheck.New(t)
	result, err := backend.New().CompareModels(c.Context(), &protocol.CompareModelsRequest{Engine: "ydb"})
	c.Assert(result, qt.IsNil)
	c.Assert(status.Code(err), qt.Equals, codes.InvalidArgument)
	c.Assert(statusReasons(c, err), qt.HasLen, 0)
}

// A comparison that refuses a declared external data source on a target
// without external data sources carries the provider's sentence as the
// reason, which a host that negotiates errors.reasons prints after the code.
// Without it the host prints the code alone.
func TestComparisonRefusesAnExternalObjectWithAReason(t *testing.T) {
	c := testcheck.New(t)
	request := externalRequest(c, []external{{name: "bucket", source: bucketAt("https://a/")}}, nil, map[string]bool{"transactional_ddl": false})

	result, err := backend.New().CompareModels(c.Context(), request)

	c.Assert(result, qt.IsNil)
	c.Assert(status.Code(err), qt.Equals, codes.InvalidArgument)
	c.Assert(statusReasons(c, err), qt.DeepEquals, []string{"external data source app/bucket, which requires target capability external_data_sources, unavailable on this ydb target; YDB keeps it behind feature flag EnableExternalDataSources: turn the flag on, and name the cluster's monitoring endpoint in the URL (monitoring=http://host:8765) so Ptah reads the flags before it plans"})
}
