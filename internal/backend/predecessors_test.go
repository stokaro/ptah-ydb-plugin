package backend_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"testing"

	qt "github.com/frankban/quicktest"
	"google.golang.org/protobuf/testing/protocmp"
	protocol "ptah.run/provider-sdk/protocol/experimental"
	"ptah.run/provider-sdk/provider"

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
	builtin := slices.DeleteFunc(slices.Clone(description.Engines[0].ModelPredecessors), func(predecessor *protocol.ModelPredecessor) bool {
		return predecessor.FormerOwner != "ptah.run/ydb"
	})
	c.Assert(builtin, qt.HasLen, 1)
	for _, predecessor := range builtin {
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

// The provider names the identity release v0.4.0 gave its observed
// changefeeds, whose definition gained foreign_replication: a value written
// under it leaves the field empty, which reads as it did. The conformance
// suite's models.recorded check reads every identity of that release.
func TestDescriptionNamesTheFormerObservedChangefeedModel(t *testing.T) {
	c := testcheck.New(t)
	description := backend.Description()

	own := slices.DeleteFunc(slices.Clone(description.Engines[0].ModelPredecessors), func(predecessor *protocol.ModelPredecessor) bool {
		return predecessor.FormerOwner != schema.ProviderID
	})

	c.Assert(own, qt.CmpEquals(protocmp.Transform()), []*protocol.ModelPredecessor{{Kind: schema.ChangefeedProperties, Representation: protocol.ValueInput_OBSERVED,
		FormerOwner: schema.ProviderID, FormerKind: schema.ChangefeedProperties, SchemaVersion: 1,
		DefinitionSha256: "97bc12f14bcb467fb6ef25f27d8c79fb24361c9c0d91a6898ef72ee2fe3e9d11"}})
	c.Assert(provider.ValidateDescription(description, []string{provider.ModelPredecessorsFeature, provider.OwnPredecessorsFeature}), qt.IsNil)
}
