//go:build integration

package integration_test

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	qt "github.com/frankban/quicktest"
	"github.com/stokaro/ptah-ydb-plugin/schema"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	providerruntime "github.com/stokaro/ptah/provider-sdk/runtime"
	"google.golang.org/protobuf/proto"
)

// This is an external binary contract test. The provider module does not import
// host packages; CI builds the pinned host in a separate checkout.
func assertHostSchemaExport(t *testing.T, client *providerruntime.Client, binary, digest string, schemas []string, inputs *protocol.NormalizeValuesRequest) {
	t.Helper()
	c := qt.New(t)
	host := os.Getenv("PTAH_HOST_TEST_BIN")
	if host == "" {
		t.Skip("PTAH_HOST_TEST_BIN is required for the external host contract")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	configuration := map[string]any{"format_version": 2, "providers": []any{map[string]any{
		"executable": binary, "sha256": digest, "provider_id": schema.ProviderID, "engines": []any{map[string]any{"id": "ydb", "aliases": []string{"ydbs"}}}, "environment": []string{},
	}}}
	encoded, err := json.Marshal(configuration)
	c.Assert(err, qt.IsNil)
	manifest := filepath.Join(t.TempDir(), "providers.json")
	c.Assert(os.WriteFile(manifest, encoded, 0o600), qt.IsNil)
	cmd := exec.CommandContext(ctx, host, "schema", "inspect", "--provider-config", manifest, "--provider-engine", "ydb", "--db-url", os.Getenv("PTAH_YDB_TEST_URL"), "--schemas", strings.Join(schemas, ","), "--format", "json")
	var diagnostics bytes.Buffer
	cmd.Stderr = &diagnostics
	output, err := cmd.Output()
	c.Assert(err, qt.IsNil, qt.Commentf("host inspect: %s", diagnostics.String()))
	var document map[string]json.RawMessage
	c.Assert(json.Unmarshal(output, &document), qt.IsNil)
	var coverage struct {
		Representation string
		Kinds          []struct {
			Model struct{ Kind, Representation string }
		}
	}
	c.Assert(json.Unmarshal(document["feature_coverage"], &coverage), qt.IsNil)
	c.Assert(coverage.Representation, qt.Equals, "desired")
	c.Assert(coverage.Kinds, qt.HasLen, len(schema.ObservedModels()))
	seenKinds := map[string]bool{}
	for _, kind := range coverage.Kinds {
		c.Assert(kind.Model.Representation, qt.Equals, "desired")
		seenKinds[kind.Model.Kind] = true
	}
	for _, kind := range schema.ObservedModels() {
		c.Assert(seenKinds[kind.Kind], qt.IsTrue, qt.Commentf("missing coverage: %s", kind.Kind))
	}
	normalized, err := client.NormalizeValues(ctx, inputs)
	c.Assert(err, qt.IsNil)
	expected, err := client.ConvertValues(ctx, &protocol.ConvertValuesRequest{Engine: "ydb", From: protocol.ValueInput_OBSERVED, To: protocol.ValueInput_DESIRED, Values: normalized.Values, TargetFeatures: inputs.TargetFeatures})
	c.Assert(err, qt.IsNil)
	var tree any
	c.Assert(json.Unmarshal(output, &tree), qt.IsNil)
	found := map[int]bool{}
	var visit func(any)
	visit = func(value any) {
		switch node := value.(type) {
		case []any:
			for _, item := range node {
				visit(item)
			}
		case map[string]any:
			if _, hasPayload := node["payload"]; hasPayload && node["representation"] != nil && node["kind"] != nil {
				var envelope struct {
					Kind, Representation string
					Payload              struct {
						Engine  string
						Subject *protocol.Reference
						Payload []byte
					}
				}
				data, err := json.Marshal(node)
				c.Assert(err, qt.IsNil)
				c.Assert(json.Unmarshal(data, &envelope), qt.IsNil)
				c.Assert(envelope.Representation, qt.Equals, "desired")
				c.Assert(envelope.Payload.Engine, qt.Equals, "ydb")
				match := -1
				for i, record := range expected.Values {
					if record.Value.Kind == envelope.Kind && proto.Equal(record.Value.Owner, envelope.Payload.Subject) {
						match = i
					}
				}
				c.Assert(match >= 0, qt.IsTrue, qt.Commentf("unexpected exported kind/owner: %s %v", envelope.Kind, envelope.Payload.Subject))
				c.Assert(found[match], qt.IsFalse)
				found[match] = true
				c.Assert(string(envelope.Payload.Payload), qt.Equals, string(expected.Values[match].Value.Payload))
				return
			}
			for _, item := range node {
				visit(item)
			}
		}
	}
	visit(tree)
	c.Assert(found, qt.HasLen, len(expected.Values))
}
