package schema

import (
	"errors"

	"github.com/stokaro/ptah-ydb-plugin/internal/externalpath"
	"github.com/stokaro/ptah-ydb-plugin/internal/ydbdecl"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
)

// RequiredObjects returns the native prerequisites retained when a model is
// selected. An unresolved absolute reference returns no partial prerequisites.
// The identity coordinate is retained independently of the physical roots.
func RequiredObjects(value *protocol.Envelope) ([]*protocol.ComparisonIdentity, bool, error) {
	if value.GetContext() != nil {
		if err := ValidateDatabaseContext(value.Context); err != nil {
			return nil, false, err
		}
	}
	if value.GetKind() == ExternalSourceProperties || value.GetKind() == ExternalTableProperties {
		return externalRequirements(value)
	}
	var pool string
	switch value.GetKind() {
	case ResourcePoolClassifierProperties:
		spec, err := WorkloadClassifier(value)
		if err != nil {
			return nil, false, err
		}
		pool = spec.ResourcePool
	case StreamingQueryProperties:
		if value.GetDefinitionSha256() == NativeDesiredModel(StreamingQueryProperties).GetDefinitionSha256() {
			spec, _, err := DesiredStreamingQuery(value)
			if err != nil {
				return nil, false, err
			}
			pool = spec.ResourcePool
		} else {
			spec, err := ObservedStreamingQuery(value)
			if err != nil {
				return nil, false, err
			}
			pool = spec.ResourcePool
		}
		if pool == "default" {
			pool = ""
		}
	default:
		return nil, false, nil
	}
	if pool == "" {
		return nil, false, nil
	}
	identity := &protocol.ComparisonIdentity{Kind: ResourcePoolProperties, Name: &protocol.ComparisonPart{Source: pool, Normalized: pool}}
	if database := value.GetOwner().GetDatabase(); database != "" {
		identity.Database = &protocol.ComparisonPart{Source: database, Normalized: database}
	}
	return []*protocol.ComparisonIdentity{identity}, false, nil
}

func externalRequirements(value *protocol.Envelope) ([]*protocol.ComparisonIdentity, bool, error) {
	root := InterpretationRoot(value)
	desired := value.GetDefinitionSha256() == NativeDesiredModel(value.GetKind()).GetDefinitionSha256()
	kind := SecretProperties
	var paths []ydbdecl.Path
	var err error
	if value.GetKind() == ExternalSourceProperties {
		var options map[string]string
		if desired {
			intent, decodeErr := DesiredExternalSource(value)
			if decodeErr != nil {
				return nil, false, decodeErr
			}
			options = intent.Properties
		} else {
			observed, decodeErr := ObservedExternalSource(value)
			if decodeErr != nil {
				return nil, false, decodeErr
			}
			options = observed.Properties
		}
		paths, err = externalpath.SecretPaths(options, root)
	} else {
		kind = ExternalSourceProperties
		var written string
		if desired {
			intent, decodeErr := DesiredExternalTable(value)
			if decodeErr != nil {
				return nil, false, decodeErr
			}
			written = intent.GetDataSourcePath()
		} else {
			observed, decodeErr := ObservedExternalTable(value)
			if decodeErr != nil {
				return nil, false, decodeErr
			}
			written = observed.GetDataSourcePath()
		}
		var ref ydbdecl.Path
		ref, err = externalpath.SourcePath(root, written)
		paths = []ydbdecl.Path{ref}
	}
	if errors.Is(err, externalpath.ErrDatabaseContextRequired) {
		return nil, true, nil
	}
	if err != nil {
		return nil, false, err
	}
	refs := make([]*protocol.ComparisonIdentity, 0, len(paths))
	for _, ref := range paths {
		identity := &protocol.ComparisonIdentity{Kind: kind, Name: &protocol.ComparisonPart{Source: ref.Name, Normalized: ref.Name}}
		if ref.Schema != "" {
			identity.Schema = &protocol.ComparisonPart{Source: ref.Schema, Normalized: ref.Schema}
		}
		if database := value.GetOwner().GetDatabase(); database != "" {
			identity.Database = &protocol.ComparisonPart{Source: database, Normalized: database}
		}
		refs = append(refs, identity)
	}
	return refs, false, nil
}
