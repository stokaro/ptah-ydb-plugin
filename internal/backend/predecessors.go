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
	}
}
