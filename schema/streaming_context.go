package schema

import (
	"slices"

	protocol "ptah.run/provider-sdk/protocol/experimental"
	"ptah.run/provider-sdk/provider"
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

// UnofferedFeature returns the first value that requires a feature the host
// did not offer, and that feature. It returns nil and "" when the host offers
// every feature the values require. A reply carrying such a value breaks the
// negotiated contract, so the caller refuses the call and names the feature
// instead of sending it.
func UnofferedFeature(offered []string, values ...*protocol.Envelope) (*protocol.Envelope, string) {
	for _, value := range values {
		for _, feature := range value.GetRequiredFeatures() {
			if !slices.Contains(offered, feature) {
				return value, feature
			}
		}
	}
	return nil, ""
}
