package backend_test

import (
	"encoding/json"
	"strings"
	"testing"

	qt "github.com/frankban/quicktest"
	"github.com/stokaro/ptah-ydb-plugin/internal/backend"
	"github.com/stokaro/ptah-ydb-plugin/internal/externals"
	native "github.com/stokaro/ptah-ydb-plugin/internal/nativeprotocol"
	"github.com/stokaro/ptah-ydb-plugin/internal/objectplan"
	"github.com/stokaro/ptah-ydb-plugin/internal/testplanning"
	"github.com/stokaro/ptah-ydb-plugin/internal/ydbdecl"
	"github.com/stokaro/ptah-ydb-plugin/schema"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/stokaro/ptah/provider-sdk/provider"
)

func authoredSecret(c *qt.C) *protocol.Envelope {
	c.Helper()
	value, err := schema.CaptureSecretIntent(&protocol.Reference{Engine: "ydb", Kind: schema.SecretProperties, Schema: "app", Name: "z.pw"},
		&native.SecretIntent{ValueEnv: "PTAH_SECRET_DECLARATION_INPUT"})
	c.Assert(err, qt.IsNil)
	return value
}

func authoredSecretSource(c *qt.C, secretPath string) *protocol.Envelope {
	c.Helper()
	intent, err := externals.SourceIntent(ydbdecl.ExternalDataSource{SourceType: "PostgreSQL", Location: "pg:5432", AuthMethod: "BASIC",
		Options: map[string]string{"LOGIN": "reader", "PASSWORD_SECRET_PATH": secretPath}})
	c.Assert(err, qt.IsNil)
	value, err := schema.CaptureExternalIntent(&protocol.Reference{Engine: "ydb", Kind: schema.ExternalSourceProperties, Schema: "app", Name: "a.source"}, intent)
	c.Assert(err, qt.IsNil)
	return value
}

func TestAuthoredSecretRetainsOnlyApprovedReferenceAndDependencyEffects(t *testing.T) {
	c := qt.New(t)
	t.Setenv("PTAH_SECRET_DECLARATION_INPUT", "SECRET_VALUE_MUST_NOT_BE_CAPTURED")
	request := declarationRequest(c, authoredSecretSource(c, "app/z.pw"), authoredSecret(c))
	request.Capabilities["secrets"], request.Capabilities["external_data_source_secret_paths"] = true, true
	result, err := backend.New().PlanDeclarations(c.Context(), request)
	c.Assert(err, qt.IsNil)
	c.Assert(result.Diagnostics, qt.HasLen, 0)
	c.Assert(result.Declarations, qt.HasLen, 2)
	c.Assert(provider.ValidatePlannedDeclarations(backend.Description(), request, result), qt.IsNil)
	var reads []string
	for _, step := range result.Contributions[0].Steps {
		var capture map[string]json.RawMessage
		c.Assert(json.Unmarshal(step.Operation.Value.Value.Payload, &capture), qt.IsNil)
		c.Assert(capture, qt.HasLen, 1)
		c.Assert(capture["desired"], qt.IsNotNil)
		c.Assert(string(step.Operation.Value.Value.Payload), qt.Not(qt.Contains), "SECRET_VALUE_MUST_NOT_BE_CAPTURED")
		for _, effect := range step.Metadata.Effects {
			if effect.Action == protocol.PlanningEffect_READ {
				c.Assert(effect.Subject.Schema.Source, qt.Equals, "app")
				c.Assert(effect.Subject.Name.Source, qt.Equals, "z.pw")
				reads = append(reads, effect.Subject.Kind)
			}
		}
	}
	c.Assert(reads, qt.DeepEquals, []string{schema.SecretProperties, objectplan.PathKind})
	rendering := testplanning.RenderRequest(&protocol.PlannedFeatures{Contributions: result.Contributions}, request.Capabilities)
	rendering.TargetFeatures, rendering.Form = request.TargetFeatures, protocol.RenderRequest_ANY
	text, err := backend.New().Render(c.Context(), rendering)
	c.Assert(err, qt.IsNil)
	c.Assert(text.Units, qt.HasLen, 2)
	var statements []string
	for _, unit := range text.Units {
		c.Assert(unit.GetNative(), qt.IsNil)
		statements = append(statements, unit.GetSql())
	}
	c.Assert(strings.Join(statements, "\n"), qt.Contains, "CREATE SECRET `app/z.pw` WITH (value = $PTAH_SECRET_DECLARATION_INPUT);")
	c.Assert(strings.Join(statements, "\n"), qt.Not(qt.Contains), "SECRET_VALUE_MUST_NOT_BE_CAPTURED")
	inputs, err := backend.New().DescribeExecutionInputs(c.Context(), &protocol.DescribeExecutionInputsRequest{Engine: "ydb", Units: text.Units})
	c.Assert(err, qt.IsNil)
	c.Assert(inputs.Inputs, qt.HasLen, 1)
	c.Assert(inputs.Inputs[0].Source, qt.Equals, protocol.ExecutionInput_ENVIRONMENT)
	c.Assert(inputs.Inputs[0].Name, qt.Equals, "PTAH_SECRET_DECLARATION_INPUT")
}

func TestAuthoredSecretRefusalsDiscardAllOutput(t *testing.T) {
	for _, test := range []struct {
		name       string
		secretPath string
		capability bool
	}{
		{name: "disabled secrets", secretPath: "app/z.pw"},

		{name: "self reference", secretPath: "app/a.source", capability: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := qt.New(t)
			request := declarationRequest(c, authoredSecretSource(c, test.secretPath), authoredSecret(c))
			request.Capabilities["secrets"], request.Capabilities["external_data_source_secret_paths"] = test.capability, true
			result, err := backend.New().PlanDeclarations(c.Context(), request)
			c.Assert(err, qt.IsNil)
			c.Assert(result.Diagnostics, qt.HasLen, 1)
			c.Assert(result.Contributions, qt.HasLen, 0)
			c.Assert(result.Declarations, qt.HasLen, 0)
			c.Assert(provider.ValidatePlannedDeclarations(backend.Description(), request, result), qt.IsNil)
		})
	}
}
