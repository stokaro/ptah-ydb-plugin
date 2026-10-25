//go:build integration

package integration_test

import (
	"context"
	"testing"
	"time"

	qt "github.com/frankban/quicktest"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/stokaro/ptah/provider-sdk/provider"
	providerruntime "github.com/stokaro/ptah/provider-sdk/runtime"
)

func startProviderListing(c *qt.C, binary, digest string, hostFeatures []string) *providerruntime.Client {
	c.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	client, err := providerruntime.Start(ctx, providerruntime.Options{
		Executable: binary, SHA256: digest, ProviderID: "github.com/stokaro/ptah-ydb-plugin", HostFeatures: hostFeatures,
	})
	c.Assert(err, qt.IsNil)
	c.Cleanup(client.Close)
	return client
}

// TestRowServicesOfflineSubprocess types rows and renders their writes
// through the SDK client without a database, and declares the query-builder
// spelling to a host that lists query.dialect and to no other.
func TestRowServicesOfflineSubprocess(t *testing.T) {
	c := qt.New(t)
	binary, digest := providerBinary(c)
	listing := startProviderListing(c, binary, digest, []string{provider.QueryDialectFeature})
	silent := startProviderListing(c, binary, digest, nil)

	c.Assert(listing.Description().GetEngines()[0].GetQueryDialect().GetPlaceholder(), qt.Equals, "dollar-named")
	c.Assert(silent.Description().GetEngines()[0].GetQueryDialect(), qt.IsNil)
	c.Assert(silent.Description().GetFeatures(), qt.Contains, provider.TypeRowsFeature)

	request := &protocol.TypeRowsRequest{Engine: "ydb", TargetFeatures: silent.Description().GetFeatures(),
		Table:   &protocol.Reference{Kind: "table", Name: "regions"},
		Columns: []*protocol.RowColumn{{Name: "id", DeclaredType: "BIGINT"}, {Name: "price", DeclaredType: "DECIMAL(22,9)"}},
		Declared: []*protocol.Row{{Values: []*protocol.DataValue{
			{Value: &protocol.DataValue_StringValue{StringValue: "7"}}, {Value: &protocol.DataValue_StringValue{StringValue: "1.50"}},
		}}}}
	typed, err := silent.TypeRows(c.Context(), request)
	c.Assert(err, qt.IsNil)
	c.Assert(typed.GetColumns()[1].GetType(), qt.Equals, "Decimal(22,9)")
	c.Assert(typed.GetDeclared()[0].GetValues()[1].GetDecimalValue().GetDigits(), qt.Equals, "1500000000")

	rendered, err := silent.RenderRowWrites(c.Context(), &protocol.RenderRowWritesRequest{Engine: "ydb", TargetFeatures: silent.Description().GetFeatures(),
		Writes: []*protocol.RowWrite{{ChangeId: "w", Mode: protocol.RowWrite_UPSERT, Table: request.Table, Columns: []string{"id", "price"},
			KeyColumns: []string{"id"}, Values: typed.GetDeclared()[0].GetValues(), ColumnTypes: map[string]string{"id": "Int64", "price": "Decimal(22,9)"}}}})
	c.Assert(err, qt.IsNil)
	c.Assert(rendered.GetUnits()[0].GetSql(), qt.Equals, "UPSERT INTO `regions` (`id`, `price`) VALUES (7l, Decimal('1.5', 22, 9));")

	_, err = silent.TypeRows(c.Context(), &protocol.TypeRowsRequest{Engine: "ydb", TargetFeatures: silent.Description().GetFeatures(),
		Table: request.Table, Columns: []*protocol.RowColumn{{Name: "id", DeclaredType: "MONEY"}}})
	c.Assert(err, qt.IsNotNil)
}
