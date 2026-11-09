package backend_test

import (
	"fmt"
	"testing"

	qt "github.com/frankban/quicktest"
	protocol "ptah.run/provider-sdk/protocol/experimental"
	"ptah.run/provider-sdk/provider"

	"github.com/stokaro/ptah-ydb-plugin/internal/backend"
	"github.com/stokaro/ptah-ydb-plugin/internal/coordinationplan"
	"github.com/stokaro/ptah-ydb-plugin/internal/externalplan"
	"github.com/stokaro/ptah-ydb-plugin/internal/replicationplan"
	"github.com/stokaro/ptah-ydb-plugin/internal/secretplan"
	"github.com/stokaro/ptah-ydb-plugin/internal/streamingplan"
	"github.com/stokaro/ptah-ydb-plugin/internal/tablecreate"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
	"github.com/stokaro/ptah-ydb-plugin/internal/topicplan"
	"github.com/stokaro/ptah-ydb-plugin/internal/workloadplan"
)

// TestDescriptionListsOneFeatureSet pins that the engine offers every
// service of the provider, dev realms, which the contract makes a feature of
// the engine alone, and the statement facts of its lint, which the contract
// lists beside the engine's lint.
func TestDescriptionListsOneFeatureSet(t *testing.T) {
	c := testcheck.New(t)

	description := backend.Description()

	c.Assert(description.Features, qt.Not(qt.HasLen), 0)
	c.Assert(description.Engines[0].Features, qt.DeepEquals, append(description.Features, "database.dev-realms", "lint.statement-facts"))
}

func TestDescriptionAdvertisesTheDevDatabaseService(t *testing.T) {
	c := testcheck.New(t)

	description := backend.Description()

	c.Assert(description.Features, qt.Contains, "database.dev")
	c.Assert(description.Engines[0].Features, qt.Contains, "database.dev")
}

// Database context and dependency binding require their declared contract
// minor. Later compatible minors retain both contracts.
func TestDescriptionRequiresDatabaseContextContract(t *testing.T) {
	for _, test := range []struct {
		name  string
		minor uint32
		want  error
	}{
		{"earlier minor", 6, provider.ErrProtocol},
		{"database context minor", 7, nil},
		{"later minor", 8, nil},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			host := &protocol.ContractVersion{Major: provider.Revision, Minor: test.minor}
			c.Assert(provider.CheckContract(host, provider.MinimumContract(backend.Description())), qt.ErrorIs, test.want)
		})
	}
}

// TestDescriptionRefusesAnotherMajor_FailurePath is the control: the declared
// minimum is a real version, not one every host meets.
func TestDescriptionRefusesAnotherMajor_FailurePath(t *testing.T) {
	c := testcheck.New(t)
	host := &protocol.ContractVersion{Major: provider.Revision - 1, Minor: provider.ContractMinor}

	err := provider.CheckContract(host, provider.MinimumContract(backend.Description()))

	c.Assert(err, qt.ErrorIs, provider.ErrProtocol)
}

func TestDescriptionRefusesHostsWithoutDatabaseContextContract(t *testing.T) {
	for minor := range uint32(7) {
		t.Run(fmt.Sprintf("%d.%d", provider.Revision, minor), func(t *testing.T) {
			c := testcheck.New(t)
			host := &protocol.ContractVersion{Major: provider.Revision, Minor: minor}
			c.Assert(provider.CheckContract(host, provider.MinimumContract(backend.Description())), qt.ErrorIs, provider.ErrProtocol)
		})
	}
}

// Negotiating a recent contract alone does not opt a host into complete
// column observations. Refuse before opening a database or returning a prefix.
func TestInspectionRequiresObservedColumnsFeature(t *testing.T) {
	c := testcheck.New(t)
	result, err := backend.New().Inspect(c.Context(), &protocol.InspectRequest{})
	c.Assert(result, qt.IsNil)
	c.Assert(err, qt.ErrorMatches, "rpc error: code = FailedPrecondition desc = YDB inspection requires schema.observed-columns")
	ctx := provider.ContextWithHostFeatures(c.Context(), []string{provider.ObservedColumnsFeature})
	result, err = backend.New().Inspect(ctx, &protocol.InspectRequest{})
	c.Assert(result, qt.IsNil)
	c.Assert(err, qt.ErrorMatches, "rpc error: code = NotFound desc = unknown YDB session")
}

// Every planning service but two reads the outline of each table: the
// planner of row tables plans their settings, and the planner of external
// data sources reads the TTL a column table keeps. The provider lists
// planning.table-outlines and transport.shared-coverage, and a host that
// lists neither negotiates a description whose services read whole tables.
func TestDescriptionOutlinesTheServicesThatReadNoTableSettings(t *testing.T) {
	c := testcheck.New(t)
	description := backend.Description()
	negotiated := provider.NegotiatedDescription(description, nil)
	tables, unlisted := make(map[string]protocol.PlanningTables), make(map[string]protocol.PlanningTables)
	for i, service := range description.Engines[0].Planning {
		tables[service.GetId()] = service.GetTables()
		unlisted[service.GetId()] = negotiated.Engines[0].Planning[i].GetTables()
	}

	c.Assert(provider.ValidateDescription(description, nil), qt.IsNil)
	c.Assert(description.Features, qt.Contains, provider.TableOutlinesFeature)
	c.Assert(description.Features, qt.Contains, provider.SharedCoverageFeature)
	c.Assert(tables, qt.DeepEquals, map[string]protocol.PlanningTables{
		secretplan.ServiceID:                 protocol.PlanningTables_PLANNING_TABLES_OUTLINED,
		streamingplan.ServiceID:              protocol.PlanningTables_PLANNING_TABLES_OUTLINED,
		coordinationplan.ServiceID:           protocol.PlanningTables_PLANNING_TABLES_OUTLINED,
		topicplan.ServiceID:                  protocol.PlanningTables_PLANNING_TABLES_OUTLINED,
		externalplan.ServiceID:               protocol.PlanningTables_PLANNING_TABLES_CAPTURED,
		workloadplan.ServiceID:               protocol.PlanningTables_PLANNING_TABLES_OUTLINED,
		replicationplan.ReplicationServiceID: protocol.PlanningTables_PLANNING_TABLES_OUTLINED,
		replicationplan.TransferServiceID:    protocol.PlanningTables_PLANNING_TABLES_OUTLINED,
		tablecreate.ServiceID:                protocol.PlanningTables_PLANNING_TABLES_CAPTURED,
	})
	for id := range unlisted {
		c.Assert(unlisted[id], qt.Equals, protocol.PlanningTables_PLANNING_TABLES_CAPTURED, qt.Commentf("%s", id))
	}
}
