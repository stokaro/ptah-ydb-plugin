package schema

import protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"

// RequiredObjects returns the native prerequisites retained when a model is
// selected. The captured database binding remains part of each prerequisite.
func RequiredObjects(value *protocol.Envelope) ([]*protocol.ComparisonIdentity, error) {
	var pool string
	switch value.GetKind() {
	case ResourcePoolClassifierProperties:
		spec, err := WorkloadClassifier(value)
		if err != nil {
			return nil, err
		}
		pool = spec.ResourcePool
	case StreamingQueryProperties:
		if value.GetDefinitionSha256() == NativeDesiredModel(StreamingQueryProperties).GetDefinitionSha256() {
			spec, _, err := DesiredStreamingQuery(value)
			if err != nil {
				return nil, err
			}
			pool = spec.ResourcePool
		} else {
			spec, err := ObservedStreamingQuery(value)
			if err != nil {
				return nil, err
			}
			pool = spec.ResourcePool
		}
		if pool == "default" {
			pool = ""
		}
	default:
		return nil, nil
	}
	if pool == "" {
		return nil, nil
	}
	identity := &protocol.ComparisonIdentity{Kind: ResourcePoolProperties, Name: &protocol.ComparisonPart{Source: pool, Normalized: pool}}
	if database := value.GetOwner().GetDatabase(); database != "" {
		identity.Database = &protocol.ComparisonPart{Source: database, Normalized: database}
	}
	return []*protocol.ComparisonIdentity{identity}, nil
}
