package schema

import (
	"slices"

	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb_Scheme"
	"google.golang.org/protobuf/proto"

	native "github.com/stokaro/ptah-ydb-plugin/internal/nativeprotocol"
)

// accessIntent is the access a described entry holds, as a declaration
// states it: the owner, and one grant per subject. YDB describes a subject
// once for each access entry it keeps, and a GRANT that names two permissions
// keeps two: measured on 25.1.4.7 and 26.2.1.14, `GRANT SELECT ROW, UPDATE ROW
// ON t TO u` describes u twice, once with each permission, and a subject
// granted on a directory and on a table in it appears once more among the
// table's effective permissions. A declaration names each subject once (see
// validatePermissionSet), so the entries of one subject merge into one grant
// of every permission they name, in the order YDB described them.
func accessIntent(self *Ydb_Scheme.Entry) *native.AccessIntent {
	if self == nil {
		return nil
	}
	result := &native.AccessIntent{}
	if self.Owner != "" {
		result.Owner = new(self.Owner)
	}
	result.DirectGrants = mergedGrants(self.Permissions)
	result.RequiredEffectivePermissions = mergedGrants(self.EffectivePermissions)
	return result
}

// mergedGrants is grants with one entry per subject, in the order the subjects
// first appear, each naming every permission the subject's entries name, once
// and in the order they first appear. It is nil for no grants.
func mergedGrants(grants []*Ydb_Scheme.Permissions) []*Ydb_Scheme.Permissions {
	var merged []*Ydb_Scheme.Permissions
	bySubject := make(map[string]*Ydb_Scheme.Permissions)
	for _, grant := range grants {
		entry, seen := bySubject[grant.GetSubject()]
		if !seen {
			entry = &Ydb_Scheme.Permissions{Subject: grant.GetSubject()}
			bySubject[grant.GetSubject()] = entry
			merged = append(merged, entry)
		}
		for _, name := range grant.GetPermissionNames() {
			if !slices.Contains(entry.PermissionNames, name) {
				entry.PermissionNames = append(entry.PermissionNames, name)
			}
		}
	}
	return merged
}
func accessObservation(access *native.AccessIntent) *Ydb_Scheme.Entry {
	if access == nil {
		return nil
	}
	result := &Ydb_Scheme.Entry{Owner: access.GetOwner()}
	for _, grant := range access.DirectGrants {
		result.Permissions = append(result.Permissions, proto.CloneOf(grant))
	}
	for _, grant := range access.RequiredEffectivePermissions {
		result.EffectivePermissions = append(result.EffectivePermissions, proto.CloneOf(grant))
	}
	return result
}

// Access requirements stay separate from the permission changes an executor
// can perform. All native declarations share this validation.
func validateAccessIntent(access *native.AccessIntent) error {
	if access == nil {
		return nil
	}
	if err := Known(access); err != nil {
		return err
	}
	for _, grants := range [][]*Ydb_Scheme.Permissions{access.DirectGrants, access.RequiredEffectivePermissions} {
		if err := validatePermissionSet(grants); err != nil {
			return err
		}
	}
	return nil
}
