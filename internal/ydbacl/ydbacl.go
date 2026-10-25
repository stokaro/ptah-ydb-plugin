// Package ydbacl holds the answers about YDB's access model that the YQL
// schema reader gives as the Ptah host's internal/ydbacl gives them: which
// YDB permission a declared privilege names, which names YDB takes for a user
// or a group, how a declared password is told from a password hash, and how
// a grant names the object it is about.
//
// YDB's access model, measured by the host on 25.1.4.7 and 26.2.1.14: a
// principal is a user or a group, and the two share one namespace; a
// permission is an entry in an object's access list, one per name GRANT
// writes; WITH GRANT OPTION adds a second entry, ydb.access.grant; and a
// permission on a directory is inherited by everything under it.
package ydbacl

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// permissionKeywords are the permission names YDB takes in GRANT, with the
// keyword spelling GRANT takes for each.
var permissionKeywords = []struct{ name, keyword string }{
	{"ydb.database.connect", "CONNECT"},
	{"ydb.database.create", "CREATE"},
	{"ydb.database.drop", "DROP"},
	{"ydb.granular.select_row", "SELECT ROW"},
	{"ydb.granular.update_row", "UPDATE ROW"},
	{"ydb.granular.erase_row", "ERASE ROW"},
	{"ydb.granular.read_attributes", "SELECT ATTRIBUTES"},
	{"ydb.granular.write_attributes", "MODIFY ATTRIBUTES"},
	{"ydb.granular.create_directory", "CREATE DIRECTORY"},
	{"ydb.granular.create_table", "CREATE TABLE"},
	{"ydb.granular.create_queue", "CREATE QUEUE"},
	{"ydb.granular.remove_schema", "REMOVE SCHEMA"},
	{"ydb.granular.describe_schema", "DESCRIBE SCHEMA"},
	{"ydb.granular.alter_schema", "ALTER SCHEMA"},
	{"ydb.access.grant", "GRANT"},
	{"ydb.generic.read", "SELECT"},
	{"ydb.generic.write", "INSERT"},
	{"ydb.generic.list", "LIST"},
	{"ydb.generic.use_legacy", "USE LEGACY"},
	{"ydb.generic.use", "USE"},
	{"ydb.generic.manage", "MANAGE"},
	{"ydb.generic.full_legacy", "FULL LEGACY"},
	{"ydb.generic.full", "FULL"},
	{"ydb.tables.modify", "MODIFY TABLES"},
	{"ydb.tables.read", "SELECT TABLES"},
}

// GrantPermission is the permission WITH GRANT OPTION adds beside the one it
// is written with.
const GrantPermission = "ydb.access.grant"

// Permission returns the YDB permission a declared privilege names, and false
// for one that names none. A privilege is a permission name, in any case, or
// a keyword GRANT takes, with any spacing; ALL and ALL PRIVILEGES name
// ydb.generic.full.
func Permission(privilege string) (string, bool) {
	normalized := strings.Join(strings.Fields(strings.ToUpper(privilege)), " ")
	switch normalized {
	case "":
		return "", false
	case "ALL", "ALL PRIVILEGES":
		return "ydb.generic.full", true
	}
	for _, permission := range permissionKeywords {
		if normalized == permission.keyword || normalized == strings.ToUpper(permission.name) {
			return permission.name, true
		}
	}
	return "", false
}

// LiteralPermission reads a quoted YQL permission name. Full permission names
// are case-sensitive; short aliases are case-insensitive and use underscores
// where keyword spellings use spaces.
func LiteralPermission(value string) (string, bool) {
	for _, permission := range permissionKeywords {
		if value == permission.name || strings.EqualFold(value, strings.ReplaceAll(permission.keyword, " ", "_")) {
			return permission.name, true
		}
	}
	return "", false
}

var errName = errors.New("YDB takes a user or group name of lower-case ASCII letters and digits only")

// CheckName returns nil when YDB takes name for a user or a group, and an
// error saying why it does not otherwise.
func CheckName(name string) error {
	if name == "" {
		return fmt.Errorf("%w, and the name is empty", errName)
	}
	for _, char := range name {
		if (char < 'a' || char > 'z') && (char < '0' || char > '9') {
			return fmt.Errorf("%w: %q holds %q", errName, name, char)
		}
	}
	return nil
}

// IsPasswordHash reports whether a declared password is a password hash YDB
// takes with CREATE USER ... HASH: a JSON object with the hash, its salt and
// its type.
func IsPasswordHash(password string) bool {
	var hash struct {
		Hash *string `json:"hash"`
		Salt *string `json:"salt"`
		Type *string `json:"type"`
	}
	trimmed := strings.TrimSpace(password)
	if !strings.HasPrefix(trimmed, "{") {
		return false
	}
	if err := json.Unmarshal([]byte(trimmed), &hash); err != nil {
		return false
	}
	return hash.Hash != nil && hash.Salt != nil && hash.Type != nil
}
