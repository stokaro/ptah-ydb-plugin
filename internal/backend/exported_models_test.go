package backend_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	qt "github.com/frankban/quicktest"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/stokaro/ptah/provider-sdk/provider"

	"github.com/stokaro/ptah-ydb-plugin/internal/backend"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
	"github.com/stokaro/ptah-ydb-plugin/schema"
)

// exportedModel is the identity of one of this provider's desired models:
// its kind, its schema version and the digest of its definition text. The
// owner is the provider and the representation is desired.
type exportedModel struct {
	kind    string
	version uint32
	digest  string
}

// exportedModels holds every identity a desired model of this provider has
// had in a release, and the one the next release ships. An HCL export's
// captured ptah:feature-coverage header has to name such an identity for a
// later release to read the file: a host reads the header only with codecs
// whose identity is the one named or a declared predecessor of it, and
// refuses any other file before anything is compared. Observed models are not
// listed: no export writes one.
//
// The host's header names a model by the identity the provider describes:
// the provider as owner, the kind, desired, the schema version and the
// definition digest, which is what an entry here holds. It moves only when a
// definition text changes, not with the release version, the features a host
// negotiated or the engine's other declarations.
//
// The list only grows. A definition text that changes moves its model's
// digest. The new identity joins the list, and the one it replaces stays and
// is declared a predecessor of the model (modelPredecessors, under
// provider.OwnPredecessorsFeature), so a file exported before still reads.
var exportedModels = []exportedModel{
	{kind: schema.ReplicationProperties, version: 1, digest: "faa84b8be10b750b58f9154e77499f7a2ce1d357ae66bf2e3ec0342cea1ca3ba"},
	{kind: schema.ColumnProperties, version: 1, digest: "6dbd837c2dc3fee253658063922d283a5d44206812d8c69b6b2da927ae836824"},
	{kind: schema.ColumnStorageProperties, version: 1, digest: "4627695b85d449a91275117597d386a81ae2afa1f0d44c6332010288325bd9b9"},
	{kind: schema.CoordinationProperties, version: 1, digest: "ed553660048298a63240dac1f471901bf38ca67a18edb9471e6f8a70d0eea912"},
	{kind: schema.ExternalSourceProperties, version: 1, digest: "1157f30e09bac8c99645b962e6cc0f95fcff2837aeea1be56aaa228fdba1d346"},
	{kind: schema.ExternalTableProperties, version: 1, digest: "be7a891bb73ed8d18a252057a7ed646705ba2b9a553238ea0475beb8b3b15bba"},
	{kind: schema.IndexSettingsProperties, version: 1, digest: "871798aa273839f894a4d366dbde23c5642ca47cd46f1770aa76944af5443e52"},
	{kind: schema.ResourcePoolProperties, version: 1, digest: "d678f6cea9521320734cffdebee647881e8eeafb34e5516ca4ec46b75fcdb6c8"},
	{kind: schema.ResourcePoolClassifierProperties, version: 1, digest: "63aa61c0de46edd1cd9ae3548ffca08f9e8abec345909894fe969bbf7b52e81a"},
	{kind: schema.TableProperties, version: 1, digest: "6360e79d5b65b9e3a4f404e72eb53a76bb8692d6f1b8dc6830b80f550217d403"},
	{kind: schema.SecretProperties, version: 1, digest: "57fbb7f5560da802b12709e85165f1b77d80b9e2b1e1fcbcb1f8b827c453b5be"},
	{kind: schema.StreamingQueryProperties, version: 1, digest: "447c2e5e18aa2690be7007e0e44c49907a4d173325f8167fa1cc9a2c92ce1d8f"},
	{kind: schema.ChangefeedProperties, version: 1, digest: "7c854b7158848a580e0afd3c80d9fc50a7e878f4c4d0e39e87c519a98b3197b1"},
	{kind: schema.IndexProperties, version: 1, digest: "01b06421a1aed46735e706443b785f1f50dbfe39c6b543d82849ea235c12f29f"},
	{kind: schema.TopicProperties, version: 1, digest: "939c4f137bcbb0e2a023b3e77eccc26f1d66ed29d8ce3158d609435d39817d93"},
	{kind: schema.TransferProperties, version: 1, digest: "24e2467ce3750c67ea83c64f9ee8989ff863599492dc93e43c8deedcd16fcbc5"},
	{kind: schema.ViewProperties, version: 1, digest: "a5149ac552ab72b6f775b69a6fccd223d4aba98a689fd8e1af26b5cc999372eb"},
}

// describedModels are the identities of the engine's desired models.
func describedModels(description *protocol.Description) []exportedModel {
	desired := slices.DeleteFunc(slices.Clone(description.GetEngines()[0].GetValueModels()), func(model *protocol.ValueModel) bool {
		return model.GetRepresentation() != protocol.ValueInput_DESIRED
	})
	var described []exportedModel
	for _, model := range desired {
		described = append(described, exportedModel{kind: model.GetKind(), version: model.GetSchemaVersion(), digest: model.GetDefinitionSha256()})
	}
	return described
}

// ownPredecessors are the former identities of the engine's desired models
// that name this provider as their owner.
func ownPredecessors(description *protocol.Description) []exportedModel {
	predecessors := slices.DeleteFunc(slices.Clone(description.GetEngines()[0].GetModelPredecessors()), func(predecessor *protocol.ModelPredecessor) bool {
		return predecessor.GetFormerOwner() != schema.ProviderID || predecessor.GetRepresentation() != protocol.ValueInput_DESIRED
	})
	var former []exportedModel
	for _, predecessor := range predecessors {
		former = append(former, exportedModel{kind: predecessor.GetFormerKind(), version: predecessor.GetSchemaVersion(), digest: predecessor.GetDefinitionSha256()})
	}
	return former
}

// missing are the identities of from that within does not hold.
func missing(from, within []exportedModel) []exportedModel {
	return slices.DeleteFunc(slices.Clone(from), func(identity exportedModel) bool { return slices.Contains(within, identity) })
}

// Every identity in exportedModels is a desired model the engine describes,
// or a predecessor the engine declares of one, and every desired model is
// listed, so a new model, or a definition whose text changed, cannot reach a
// release unrecorded. A failure of the first assertion names an identity to
// add to exportedModels; one of the second names an identity an earlier
// release had that the provider neither describes nor declares any more.
func TestModelIdentitiesAreDescribedOrDeclaredPredecessors(t *testing.T) {
	c := testcheck.New(t)
	description := backend.Description()
	described := describedModels(description)
	readable := append(slices.Clone(described), ownPredecessors(description)...)

	c.Assert(described, qt.Not(qt.HasLen), 0)
	c.Check(missing(described, exportedModels), qt.HasLen, 0,
		qt.Commentf("a desired model whose identity exportedModels does not list: a new model, or a definition text that changed"))
	c.Assert(missing(exportedModels, readable), qt.HasLen, 0,
		qt.Commentf("an identity a release had that the provider neither describes nor declares as a predecessor"))
}

// The remedy the guard asks for is open to the provider: its description
// names an identity a model had before the text changed as a predecessor of
// the model itself, which then counts as readable, and the description stays
// valid. A host whose SDK does not know such a predecessor is not sent it,
// and keeps the built-in engine's.
func TestAReplacedIdentityCanBeDeclaredAPredecessor(t *testing.T) {
	c := testcheck.New(t)
	description := backend.Description()
	engine := description.GetEngines()[0]
	model := engine.GetValueModels()[slices.IndexFunc(engine.GetValueModels(), func(model *protocol.ValueModel) bool {
		return model.GetRepresentation() == protocol.ValueInput_DESIRED
	})]
	replaced := exportedModel{kind: model.GetKind(), version: model.GetSchemaVersion(), digest: strings.Repeat("0", 64)}
	builtin := len(slices.DeleteFunc(slices.Clone(engine.GetModelPredecessors()), func(predecessor *protocol.ModelPredecessor) bool {
		return predecessor.GetFormerOwner() == schema.ProviderID
	}))
	engine.ModelPredecessors = append(engine.ModelPredecessors, &protocol.ModelPredecessor{Kind: replaced.kind, Representation: protocol.ValueInput_DESIRED,
		FormerOwner: schema.ProviderID, FormerKind: replaced.kind, SchemaVersion: replaced.version, DefinitionSha256: replaced.digest})
	hostFeatures := []string{provider.ModelPredecessorsFeature, provider.OwnPredecessorsFeature}

	c.Assert(provider.ValidateDescription(provider.NegotiatedDescription(description, hostFeatures), nil), qt.IsNil)
	c.Assert(ownPredecessors(description), qt.Contains, replaced)
	earlier := provider.NegotiatedDescription(description, []string{provider.ModelPredecessorsFeature})
	c.Assert(provider.ValidateDescription(earlier, nil), qt.IsNil)
	c.Assert(earlier.GetEngines()[0].GetModelPredecessors(), qt.HasLen, builtin)
	c.Assert(ownPredecessors(earlier), qt.HasLen, 0)
}

// The header of the export an earlier build of this provider recorded
// (testdata/provider-hcl) names each model by an identity exportedModels
// holds, with this provider as owner: the identities the guard pins are the
// ones a header carries.
func TestTheRecordedProviderHeaderNamesPinnedIdentities(t *testing.T) {
	c := testcheck.New(t)
	data, err := os.ReadFile(filepath.Join("testdata", "provider-hcl", "inspect.hcl"))
	c.Assert(err, qt.IsNil)
	header := builtinHeader.FindSubmatch(data)
	c.Assert(header, qt.HasLen, 2)
	var document struct {
		Kinds []struct {
			Model capturedModel `json:"model"`
		} `json:"kinds"`
	}
	c.Assert(json.Unmarshal(header[1], &document), qt.IsNil)
	c.Assert(document.Kinds, qt.Not(qt.HasLen), 0)
	var named []exportedModel
	for _, kind := range document.Kinds {
		c.Assert(kind.Model.Owner, qt.Equals, schema.ProviderID)
		c.Assert(kind.Model.Representation, qt.Equals, "desired")
		digest, found := strings.CutPrefix(kind.Model.Definition, "sha256:")
		c.Assert(found, qt.IsTrue)
		named = append(named, exportedModel{kind: kind.Model.Kind, version: kind.Model.Version, digest: digest})
	}
	c.Assert(missing(named, exportedModels), qt.HasLen, 0)
}
