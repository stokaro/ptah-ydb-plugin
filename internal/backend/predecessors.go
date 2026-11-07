package backend

import (
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"

	"github.com/stokaro/ptah-ydb-plugin/schema"
)

// modelPredecessors names the identities Ptah's built-in YDB engine gave the
// models this provider owns as their predecessors
// (provider.ModelPredecessorsFeature), so the host reads a file that engine
// exported with the meaning it was written with: a claim of every
// coordination node stays a claim, and a node the file leaves out is dropped
// as the built-in engine dropped it.
//
// It also names, under provider.OwnPredecessorsFeature, each identity a
// desired model of this provider had in a release before its definition text
// changed, where the text changed without changing what a claim of the model
// means: a host names this provider's models in an HCL export's header by
// the identity the provider describes, so the export of an earlier release
// reads as the same claim. exportedModels lists every such identity, and
// TestModelIdentitiesAreDescribedOrDeclaredPredecessors fails while one of
// them is neither described nor named here. A text whose change does change
// the meaning is not named, and an earlier export that claims the model is
// then refused, as it has to be.
//
// An observed model's earlier identity is named the same way where a value
// written under it reads as it did: the observed changefeeds gained
// foreign_replication, which a value without it leaves empty, as the read
// does for a changefeed whose replication lives in this database. The
// conformance suite's models.recorded check holds the identities of the last
// release (integration/testdata).
//
// The built-in engine's HCL export records what it knows of the coordination
// nodes in a ptah:feature-coverage header, and names no other model there. It
// writes the same identity on 25.1.4.7 and 26.2.1.14, with and without
// --schemas, in every file of an --out-dir export and from ptah-compat schema
// inspect, and the codec definition and the digest computation are the same
// in every Ptah release that writes the header (testdata/builtin-hcl).
func modelPredecessors() []*protocol.ModelPredecessor {
	return []*protocol.ModelPredecessor{
		{
			Kind:             schema.CoordinationProperties,
			Representation:   protocol.ValueInput_DESIRED,
			FormerOwner:      "ptah.run/ydb",
			FormerKind:       "ptah.run/ydb/coordination-node",
			SchemaVersion:    1,
			DefinitionSha256: "4c044f15565424fd0212b50fa50c1f760e53c228028c0f0e9ac8399ad3d2183a",
		},
		{
			Kind:             schema.ChangefeedProperties,
			Representation:   protocol.ValueInput_OBSERVED,
			FormerOwner:      schema.ProviderID,
			FormerKind:       schema.ChangefeedProperties,
			SchemaVersion:    1,
			DefinitionSha256: "97bc12f14bcb467fb6ef25f27d8c79fb24361c9c0d91a6898ef72ee2fe3e9d11",
		},
	}
}
