package schema

import (
	"errors"

	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"

	"github.com/stokaro/ptah-ydb-plugin/internal/externalpath"
	"github.com/stokaro/ptah-ydb-plugin/internal/ydbdecl"
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
	kind := SecretProperties
	var paths []ydbdecl.Path
	var err error
	if value.GetKind() == ExternalSourceProperties {
		options, decodeErr := externalSourceOptions(value)
		if decodeErr != nil {
			return nil, false, decodeErr
		}
		paths, err = externalpath.SecretPaths(options, root)
	} else {
		kind = ExternalSourceProperties
		written, decodeErr := externalTableSourcePath(value)
		if decodeErr != nil {
			return nil, false, decodeErr
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

// externalSourceOptions returns the options of an external data source, read
// through the desired model when value is bound to it and through the observed
// one otherwise.
func externalSourceOptions(value *protocol.Envelope) (map[string]string, error) {
	if value.GetDefinitionSha256() == NativeDesiredModel(value.GetKind()).GetDefinitionSha256() {
		intent, err := DesiredExternalSource(value)
		if err != nil {
			return nil, err
		}
		return intent.Properties, nil
	}
	observed, err := ObservedExternalSource(value)
	if err != nil {
		return nil, err
	}
	return observed.Properties, nil
}

// externalTableSourcePath returns the data source path of an external table as
// written, read through the desired model when value is bound to it and
// through the observed one otherwise.
func externalTableSourcePath(value *protocol.Envelope) (string, error) {
	if value.GetDefinitionSha256() == NativeDesiredModel(value.GetKind()).GetDefinitionSha256() {
		intent, err := DesiredExternalTable(value)
		if err != nil {
			return "", err
		}
		return intent.GetDataSourcePath(), nil
	}
	observed, err := ObservedExternalTable(value)
	if err != nil {
		return "", err
	}
	return observed.GetDataSourcePath(), nil
}
