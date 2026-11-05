package tablecreate

import (
	"fmt"

	native "github.com/stokaro/ptah-ydb-plugin/internal/nativeprotocol"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb_Table"
	"google.golang.org/protobuf/types/known/emptypb"
)

// DeletingColumnTTL is a row table's TTL as a column table's: one tier that
// deletes a row the interval after the column's value, with the unit an
// integer column counts in. A source that states a TTL with the row
// deletion properties states this policy for a column table.
func DeletingColumnTTL(ttl *Ydb_Table.TtlSettings) (*native.ColumnTTLPolicy, error) {
	enabled := &native.ColumnTTLEnabled{}
	switch mode := ttl.GetMode().(type) {
	case *Ydb_Table.TtlSettings_DateTypeColumn:
		enabled.Column = mode.DateTypeColumn.GetColumnName()
		enabled.Tiers = []*native.ColumnTTLTier{{ApplyAfterSeconds: uint64(mode.DateTypeColumn.GetExpireAfterSeconds()), Action: &native.ColumnTTLTier_Delete{Delete: &emptypb.Empty{}}}}
	case *Ydb_Table.TtlSettings_ValueSinceUnixEpoch:
		enabled.Column = mode.ValueSinceUnixEpoch.GetColumnName()
		enabled.ColumnUnit = uint64(mode.ValueSinceUnixEpoch.GetColumnUnit())
		enabled.Tiers = []*native.ColumnTTLTier{{ApplyAfterSeconds: uint64(mode.ValueSinceUnixEpoch.GetExpireAfterSeconds()), Action: &native.ColumnTTLTier_Delete{Delete: &emptypb.Empty{}}}}
	default:
		return nil, fmt.Errorf("YDB TTL requires an explicit mode")
	}
	return &native.ColumnTTLPolicy{State: &native.ColumnTTLPolicy_Enabled{Enabled: enabled}}, nil
}
