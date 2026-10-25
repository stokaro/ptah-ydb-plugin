package externals

import (
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/stokaro/ptah-ydb-plugin/internal/ydbdecl"
)

// The capability keys external objects need, as the host names them.
const (
	// CapabilityExternalDataSources is a target that keeps external data
	// sources and external tables (EnableExternalDataSources).
	CapabilityExternalDataSources = "external_data_sources"
	// CapabilityReplace is a target that replaces either object in place
	// with CREATE OR REPLACE (EnableReplaceIfExistsForExternalEntities).
	CapabilityReplace = "external_object_replace"
	// CapabilitySecretPaths is a target whose data source options name a
	// secret by its path (YDB 25.4 and later).
	CapabilitySecretPaths = "external_data_source_secret_paths"
)

// UnsupportedReason is why a target without external data sources leaves an
// external object its read finds unmanaged, the host's words.
const UnsupportedReason = "target capability external_data_sources is unavailable, so Ptah leaves the object unmanaged"

func capabilityRefusal(subject, key string) error {
	return fmt.Errorf("%s, which requires target capability %s, unavailable on this ydb target", subject, key)
}

// CheckDataSource reports why the data source display names cannot be
// created as source says on a target holding capabilities, or nil.
func CheckDataSource(display string, source ydbdecl.ExternalDataSource, capabilities map[string]bool) error {
	subject := "external data source " + display
	switch {
	case !capabilities[CapabilityExternalDataSources]:
		return capabilityRefusal(subject, CapabilityExternalDataSources)
	case strings.TrimSpace(source.SourceType) == "":
		return fmt.Errorf("%s: it needs a source_type, such as ObjectStorage or PostgreSQL", subject)
	case strings.TrimSpace(source.AuthMethod) == "":
		return fmt.Errorf("%s: it needs an auth_method, such as NONE or BASIC (`AUTH_METHOD requires key`)", subject)
	}
	for _, option := range slices.Sorted(maps.Keys(source.Options)) {
		if strings.HasSuffix(option, secretPathSuffix) && !capabilities[CapabilitySecretPaths] {
			return capabilityRefusal(subject+" option "+option, CapabilitySecretPaths)
		}
	}
	return nil
}

// CheckTable reports why the external table display names cannot be created
// as table says on a target holding capabilities, or nil.
func CheckTable(display string, table ydbdecl.ExternalTable, capabilities map[string]bool) error {
	subject := "external table " + display
	switch {
	case !capabilities[CapabilityExternalDataSources]:
		return capabilityRefusal(subject, CapabilityExternalDataSources)
	case strings.TrimSpace(table.DataSource) == "":
		return fmt.Errorf("%s: it needs a data_source, the path of the data source it reads", subject)
	case strings.TrimSpace(table.Location) == "":
		return fmt.Errorf("%s: it needs a location (`LOCATION requires key`)", subject)
	case len(table.Columns) == 0:
		return fmt.Errorf("%s: it needs a column", subject)
	}
	return nil
}

// CheckDrop reports why statement, which drops an external object, cannot
// run on a target holding capabilities, or nil.
func CheckDrop(statement string, capabilities map[string]bool) error {
	if !capabilities[CapabilityExternalDataSources] {
		return capabilityRefusal(statement, CapabilityExternalDataSources)
	}
	return nil
}

// CheckReplace reports why subject, an external object a statement replaces
// with CREATE OR REPLACE, cannot be replaced on a target holding
// capabilities, or nil.
func CheckReplace(subject string, capabilities map[string]bool) error {
	if !capabilities[CapabilityReplace] {
		return capabilityRefusal(subject+" replaced in place", CapabilityReplace)
	}
	return nil
}

// SecretPaths are the secrets the options of a data source name by their
// path, relative to root, in option order. A path outside the database is
// refused with the host's words: the data source cannot read it.
func SecretPaths(options map[string]string, root string) ([]ydbdecl.Path, error) {
	var paths []ydbdecl.Path
	for _, option := range slices.Sorted(maps.Keys(options)) {
		written := strings.TrimSpace(options[option])
		if !strings.HasSuffix(strings.ToUpper(option), secretPathSuffix) || written == "" {
			continue
		}
		relative := written
		if strings.HasPrefix(written, "/") {
			relative = RelativePath(written, root)
			if strings.HasPrefix(relative, "/") {
				return nil, fmt.Errorf("option %s names secret %q, which is outside the database /%s, so the data source cannot read it",
					option, written, strings.Trim(root, "/"))
			}
		}
		ref, err := ydbdecl.ParseExternalPath(relative)
		if err != nil || slices.Contains(paths, ref) {
			continue
		}
		paths = append(paths, ref)
	}
	return paths, nil
}
