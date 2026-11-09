//go:build integration

package integration_test

import (
	"testing"

	qt "github.com/frankban/quicktest"
	protocol "ptah.run/provider-sdk/protocol/experimental"
	"ptah.run/provider-sdk/provider"

	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
)

// styledPolicy is a planning policy in style that asks for no rebuild.
func styledPolicy(style protocol.PlanningStatementStyle) *protocol.PlanningPolicy {
	return &protocol.PlanningPolicy{StatementStyle: style, TableRebuildRequest: "--allow-table-rebuild"}
}

// A host that lists planning.statement-style, as ptah-compat's migrate diff
// needs to plan in the exact style, gets the feature from the provider, and a
// creation planned in the exact style renders the statement the annotated
// style does: the provider's operations carry no descriptive comment, guard
// or CASCADE in either style. No database session is opened.
func TestStatementStyleOfflineSubprocess_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	client := startProviderFor(c, provider.PlanningLifecycleFeature, provider.TextUnitsFeature, provider.PlanningPolicyFeature,
		provider.PlanningStatementStyleFeature)
	payload := `{"primaryKey":["id"]}`
	annotated := offlineCreationUnder(c, client, payload, styledPolicy(protocol.PlanningStatementStyle_PLANNING_STATEMENT_STYLE_ANNOTATED))
	exact := offlineCreationUnder(c, client, payload, styledPolicy(protocol.PlanningStatementStyle_PLANNING_STATEMENT_STYLE_EXACT))

	renderedAnnotated, annotatedErr := client.Render(c.Context(), annotated)
	renderedExact, exactErr := client.Render(c.Context(), exact)

	c.Assert(client.Description().Features, qt.Contains, provider.PlanningStatementStyleFeature)
	c.Assert(client.Description().Engines[0].Features, qt.Contains, provider.PlanningStatementStyleFeature)
	c.Assert(annotatedErr, qt.IsNil)
	c.Assert(exactErr, qt.IsNil)
	c.Assert(renderedExact.Units, qt.HasLen, 1)
	c.Assert(renderedExact.Units[0].GetSql(), qt.Equals, "CREATE TABLE `app/records` (\n    `id` Uint64 NOT NULL,\n    `ts` Timestamp,\n    PRIMARY KEY (`id`)\n);")
	c.Assert(renderedAnnotated.Units, qt.HasLen, 1)
	c.Assert(renderedAnnotated.Units[0].GetSql(), qt.Equals, renderedExact.Units[0].GetSql())
}

// A host that does not list planning.statement-style negotiates no exact
// style: the client refuses an exact plan before the request is sent.
func TestStatementStyleOfflineSubprocess_FailurePath(t *testing.T) {
	c := testcheck.New(t)
	client := startProviderFor(c, provider.PlanningLifecycleFeature, provider.PlanningPolicyFeature)

	request := offlineCreationRequest(c, client, `{"primaryKey":["id"]}`, styledPolicy(protocol.PlanningStatementStyle_PLANNING_STATEMENT_STYLE_EXACT))

	plan, err := client.PlanFeatures(c.Context(), request)

	c.Assert(client.Description().Features, qt.Not(qt.Contains), provider.PlanningStatementStyleFeature)
	c.Assert(err, qt.ErrorMatches, `.*exact planning needs planning\.statement-style.*`)
	c.Assert(plan, qt.IsNil)
}
