package schema

import protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"

// RequiredObjects returns the native prerequisites retained when a model is
// selected. The captured database binding remains part of each prerequisite.
func RequiredObjects(value *protocol.Envelope) ([]*protocol.ComparisonIdentity, error) {
	if value.GetKind() != ResourcePoolClassifierProperties {
		return nil, nil
	}
	spec, err := WorkloadClassifier(value)
	if err != nil {
		return nil, err
	}
	if spec.ResourcePool == "" {
		return nil, nil
	}
	identity := &protocol.ComparisonIdentity{Kind: ResourcePoolProperties, Name: &protocol.ComparisonPart{Source: spec.ResourcePool, Normalized: spec.ResourcePool}}
	if database := value.GetOwner().GetDatabase(); database != "" {
		identity.Database = &protocol.ComparisonPart{Source: database, Normalized: database}
	}
	return []*protocol.ComparisonIdentity{identity}, nil
}
