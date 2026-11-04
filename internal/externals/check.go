package externals

import (
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/stokaro/ptah-ydb-plugin/internal/authored"
	"github.com/stokaro/ptah-ydb-plugin/internal/externalpath"
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

// capabilityRefusal refuses subject on a target without the capability key,
// in the host's words. subject names an object and never a value, so the
// refusal is a reason a host that negotiates errors.reasons prints.
func capabilityRefusal(subject, key string) error {
	return authored.New(capabilityText(subject, key))
}

func capabilityText(subject, key string) string {
	return fmt.Sprintf("%s, which requires target capability %s, unavailable on this ydb target", subject, key)
}

// CheckDataSource reports why the data source display names cannot be
// created as source says on a target holding capabilities, or nil.
func CheckDataSource(display string, source ydbdecl.ExternalDataSource, capabilities map[string]bool) error {
	subject := "external data source " + display
	switch {
	case !capabilities[CapabilityExternalDataSources]:
		return capabilityRefusal(subject, CapabilityExternalDataSources)
	case strings.TrimSpace(source.SourceType) == "":
		return authored.New(subject + ": it needs a source_type, such as ObjectStorage or PostgreSQL")
	case strings.TrimSpace(source.AuthMethod) == "":
		return authored.New(subject + ": it needs an auth_method, such as NONE or BASIC (`AUTH_METHOD requires key`)")
	}
	for _, option := range slices.Sorted(maps.Keys(source.Options)) {
		if externalpath.IsSecretOption(option) && !capabilities[CapabilitySecretPaths] {
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
		return authored.New(subject + ": it needs a data_source, the path of the data source it reads")
	case strings.TrimSpace(table.Location) == "":
		return authored.New(subject + ": it needs a location (`LOCATION requires key`)")
	case len(table.Columns) == 0:
		return authored.New(subject + ": it needs a column")
	}
	return nil
}

// CheckDrop reports why statement, which drops an external object, cannot
// run on a target holding capabilities, or nil. The refusal quotes the
// statement, so it is no reason a host prints.
func CheckDrop(statement string, capabilities map[string]bool) error {
	if !capabilities[CapabilityExternalDataSources] {
		return errors.New(capabilityText(statement, CapabilityExternalDataSources))
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
