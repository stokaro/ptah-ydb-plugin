package commonobject

import (
	"path"
	"strings"

	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
)

// grantObjectCapabilities names the capability under which each native
// object's access list exists. The common wire uses grant object types here,
// rather than the provider's model identifiers.
var grantObjectCapabilities = map[string]string{
	"topic": "topics", "coordination node": "coordination_nodes", "secret": "secrets",
	"external data source": "external_data_sources", "external table": "external_data_sources",
	"async replication": "async_replication", "transfer": "transfers",
	"streaming query": "streaming_queries", "resource pool": "resource_pools",
}

const resourcePoolDirectory = ".metadata/workload_manager/pools"

func grantPath(target *protocol.Reference, capabilities map[string]bool) (string, error) {
	if err := grantTargetRefusal(target); err != nil {
		return "", err
	}
	kind := strings.Join(strings.Fields(strings.ToLower(target.GetKind())), " ")
	if key, native := grantObjectCapabilities[kind]; native {
		if err := accessCapability(capabilities, key); err != nil {
			return "", err
		}
	}
	if kind == "database" || kind == "path" {
		if err := accessCapability(capabilities, "database_grants"); err != nil {
			return "", err
		}
	}
	name, directory := target.GetName(), target.GetSchema()
	if strings.HasPrefix(name, "/") {
		if directory != "" {
			return "", unsupported("an absolute grant target cannot also name a directory")
		}
		return name, nil
	}
	if strings.HasPrefix(directory, "/") {
		return "", unsupported("a grant target directory is relative to its database")
	}
	relative := name
	if directory != "" {
		relative = directory + "/" + name
	}
	if kind == "resource pool" {
		if directory != "" || strings.Contains(name, "/") {
			return "", unsupported("a resource pool belongs to the database and is named by its name alone")
		}
		relative = resourcePoolDirectory + "/" + name
	}
	if relative != "" && (capabilities["relative_grant_paths"] || strings.Contains(relative, "/")) {
		return relative, nil
	}
	database := target.GetDatabase()
	if !strings.HasPrefix(database, "/") || !validGrantPath(database) {
		return "", unsupported("YDB takes this grant target only by its absolute database path; plan it against the database")
	}
	if relative == "" {
		return database, nil
	}
	return strings.TrimRight(database, "/") + "/" + relative, nil
}

func validGrantPath(value string) bool {
	return accessName(value) && !strings.Contains(value, "\\") && path.Clean(value) == value && value != "." && value != ".." &&
		!strings.HasPrefix(value, "../")
}

func grantTargetRefusal(target *protocol.Reference) error {
	if target.GetEngine() != "ydb" || target.GetCatalog() != "" || len(target.GetPath()) != 0 || len(target.GetOverload()) != 0 {
		return unsupported("a YDB grant target has an engine, a directory and an object name, without a catalog, parent or routine signature")
	}
	kind := strings.Join(strings.Fields(strings.ToLower(target.GetKind())), " ")
	if _, native := grantObjectCapabilities[kind]; !native && kind != "table" && kind != "schema" && kind != "database" && kind != "path" {
		return unsupported("a YDB grant cannot name object kind %q", target.GetKind())
	}
	name, directory := target.GetName(), target.GetSchema()
	if kind == "database" {
		if name != "" || directory != "" {
			return unsupported("a database grant names the database the connection opens")
		}
		return nil
	}
	if !accessName(name) || !validGrantPath(name) || (directory != "" && !validGrantPath(directory)) {
		return unsupported("a YDB grant requires an exact object path")
	}
	if strings.HasPrefix(directory, "/") || (strings.HasPrefix(name, "/") && directory != "") {
		return unsupported("a grant target directory is relative to its database and cannot qualify an absolute path")
	}
	if kind == "resource pool" && (directory != "" || strings.Contains(name, "/")) {
		return unsupported("a resource pool belongs to the database and is named by its name alone")
	}
	return nil
}
