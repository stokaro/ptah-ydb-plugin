package nativeapi

import (
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb_Table"
	"google.golang.org/protobuf/proto"

	native "github.com/stokaro/ptah-ydb-plugin/internal/nativeprotocol"
)

// sameSequence reports whether a table read back holds the sequence a creation
// asked for: every setting the request states, Int64 values, and no restart. A
// column the request gives no sequence must have none.
//
// YDB reports a Serial's sequence with a data type the vendor binding does not
// model (field 9, Int64 on 25.1.4.7 and 26.2.1.14), which the provider's own
// description reads; a restart would show as the set_val the read asks for.
func sameSequence(wanted *Ydb_Table.SequenceDescription, actual *native.SequenceDescription) bool {
	if wanted == nil || actual == nil {
		return wanted == nil && actual == nil
	}
	encoded, err := proto.Marshal(wanted)
	if err != nil {
		return false
	}
	var requested native.SequenceDescription
	if err := proto.Unmarshal(encoded, &requested); err != nil {
		return false
	}
	held := proto.CloneOf(actual)
	if held.DataType != nil {
		if !proto.Equal(held.DataType, &Ydb.Type{Type: &Ydb.Type_TypeId{TypeId: Ydb.Type_INT64}}) {
			return false
		}
		held.DataType = nil
	}
	return proto.Equal(&requested, held)
}
