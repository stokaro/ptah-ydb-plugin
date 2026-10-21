package schema

import (
	native "github.com/stokaro/ptah-ydb-plugin/internal/nativeprotocol"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb_Scheme"
	"google.golang.org/protobuf/proto"
)

func accessIntent(self *Ydb_Scheme.Entry) *native.AccessIntent {
	if self == nil {
		return nil
	}
	result := &native.AccessIntent{}
	if self.Owner != "" {
		result.Owner = proto.String(self.Owner)
	}
	for _, grant := range self.Permissions {
		result.DirectGrants = append(result.DirectGrants, proto.CloneOf(grant))
	}
	for _, grant := range self.EffectivePermissions {
		result.RequiredEffectivePermissions = append(result.RequiredEffectivePermissions, proto.CloneOf(grant))
	}
	return result
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
