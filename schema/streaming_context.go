package schema

import (
	"slices"

	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/stokaro/ptah/provider-sdk/provider"
)

// A host that cannot assess unchanged queries cannot safely mutate their
// inputs. Retain this requirement in both authored and inspected captures.
func requireStreamingContext(value *protocol.Envelope) {
	if value.GetKind() != StreamingQueryProperties {
		return
	}
	value.RequiredFeatures = append(value.RequiredFeatures, provider.PlanningObjectContextFeature)
	slices.Sort(value.RequiredFeatures)
	value.RequiredFeatures = slices.Compact(value.RequiredFeatures)
}
