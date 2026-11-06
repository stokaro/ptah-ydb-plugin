package backend_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"testing"

	qt "github.com/frankban/quicktest"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/stokaro/ptah/provider-sdk/provider"

	"github.com/stokaro/ptah-ydb-plugin/internal/backend"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
	"github.com/stokaro/ptah-ydb-plugin/schema"
)

// builtinHeader matches the captured feature coverage header of an HCL file,
// whose lines end in CRLF in a Windows checkout.
var builtinHeader = regexp.MustCompile(`(?m)^// ptah:feature-coverage (\{.*\})\r?$`)

// capturedModel is a model identity as a captured header names it.
type capturedModel struct {
	Owner          string `json:"owner"`
	Kind           string `json:"kind"`
	Representation string `json:"representation"`
	Version        uint32 `json:"version"`
	Definition     string `json:"definition"`
}

// The provider names the identity every recorded export of the built-in
// engine writes in its header as the predecessor of its coordination-node
// model, and the description that does so is valid.
func TestDescriptionNamesTheBuiltinHeaderModel(t *testing.T) {
	c := testcheck.New(t)
	description := backend.Description()
	var named []capturedModel
	for _, predecessor := range description.Engines[0].ModelPredecessors {
		c.Assert(predecessor.Kind, qt.Equals, schema.CoordinationProperties)
		c.Assert(predecessor.Representation, qt.Equals, protocol.ValueInput_DESIRED)
		named = append(named, capturedModel{Owner: predecessor.FormerOwner, Kind: predecessor.FormerKind, Representation: "desired",
			Version: predecessor.SchemaVersion, Definition: "sha256:" + predecessor.DefinitionSha256})
	}
	for _, file := range []string{"inspect.hcl", "compat-inspect.hcl", "split/schemas/h1.hcl", "split/tables/h1_items.hcl",
		"split/objects/0003.hcl", "split/objects/0004.hcl"} {
		t.Run(file, func(t *testing.T) {
			c := testcheck.New(t)
			data, err := os.ReadFile(filepath.Join("testdata", "builtin-hcl", filepath.FromSlash(file)))
			c.Assert(err, qt.IsNil)
			header := builtinHeader.FindSubmatch(data)
			c.Assert(header, qt.HasLen, 2)
			var document struct {
				Kinds []struct {
					Model capturedModel `json:"model"`
				} `json:"kinds"`
			}
			c.Assert(json.Unmarshal(header[1], &document), qt.IsNil)
			c.Assert(document.Kinds, qt.HasLen, 1)
			c.Assert(named, qt.Contains, document.Kinds[0].Model)
		})
	}
	c.Assert(provider.ValidateDescription(description, []string{provider.ModelPredecessorsFeature}), qt.IsNil)
	negotiated := provider.NegotiatedDescription(description, nil)
	c.Assert(negotiated.Engines[0].ModelPredecessors, qt.HasLen, 0)
}
