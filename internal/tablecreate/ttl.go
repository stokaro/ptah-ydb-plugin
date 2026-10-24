package tablecreate

import (
	"fmt"
	"slices"

	"github.com/stokaro/ptah-ydb-plugin/schema"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb_Table"
	"google.golang.org/protobuf/proto"
)

// ttlChange is what one AlterTable call does to a table's TTL setting.
type ttlChange struct {
	// set is the TTL the table holds after the call. It is nil for a removal.
	set *Ydb_Table.TtlSettings
	// drop removes the table's TTL.
	drop bool
	// held is the TTL the table held when the change was planned; nil is none.
	held *Ydb_Table.TtlSettings
}

func (c ttlChange) empty() bool { return c.set == nil && !c.drop }

// ttlInput is what the TTL rule reads. managed is true when the declaration
// states table properties for this target, or when its source established the
// table properties of the table (see [FacetKnown]): a table left without a TTL
// then requests none. A table the source could not describe leaves the TTL
// alone.
type ttlInput struct {
	managed  bool
	desired  *Ydb_Table.TtlSettings
	held     *Ydb_Table.TtlSettings
	declared map[string]string // declared column name to its YDB type name
}

// ttlColumn returns the column a TTL reads.
func ttlColumn(ttl *Ydb_Table.TtlSettings) string {
	if column := ttl.GetDateTypeColumn().GetColumnName(); column != "" {
		return column
	}
	return ttl.GetValueSinceUnixEpoch().GetColumnName()
}

var (
	ttlDateTypes  = []string{"Date", "Datetime", "Timestamp", "Date32", "Datetime64", "Timestamp64"}
	ttlEpochTypes = []string{"Uint32", "Uint64", "DyNumber"}
)

// TTLColumnRefusal says why a column of the given YDB type cannot carry a TTL
// in the given mode, or returns the empty string. epoch is true for a TTL that
// counts an integer since the Unix epoch. The answers are the server's,
// measured on 25.1.4.7 and 26.2.1.14; the server reports each of them only
// after the call, which would leave the outcome uncertain.
func TTLColumnRefusal(column, ydbType string, epoch bool) string {
	switch {
	case slices.Contains(ttlDateTypes, ydbType) && !epoch:
		return ""
	case slices.Contains(ttlEpochTypes, ydbType) && epoch:
		return ""
	case slices.Contains(ttlDateTypes, ydbType):
		return fmt.Sprintf("column %q is %s, a date type, and YDB takes no unit for one", column, ydbType)
	case slices.Contains(ttlEpochTypes, ydbType):
		return fmt.Sprintf("column %q is %s, an integer type, and YDB needs the unit it counts since the Unix epoch", column, ydbType)
	default:
		return fmt.Sprintf("column %q is %s, and YDB reads a TTL from a Date, Datetime, Timestamp, Date32, Datetime64 or Timestamp64 column, or from a Uint32, Uint64 or DyNumber column with a unit", column, ydbType)
	}
}

// sameTTL reports whether a table holding held already satisfies a declared
// TTL want, which is never nil: the same mode, column, unit and seconds. A run interval the declaration
// leaves out is not compared, because it is a server schedule the call keeps.
func sameTTL(want, held *Ydb_Table.TtlSettings) bool {
	if held == nil {
		return false
	}
	return proto.Equal(want.GetDateTypeColumn(), held.GetDateTypeColumn()) &&
		proto.Equal(want.GetValueSinceUnixEpoch(), held.GetValueSinceUnixEpoch()) &&
		(want.RunIntervalSeconds == 0 || want.RunIntervalSeconds == held.RunIntervalSeconds)
}

// planTTL returns the change that gives the table the TTL the declaration
// states, or the reason it cannot be made. An empty change means the table
// already satisfies the declaration.
//
// A declared TTL is checked against the declared type of its column, so the
// plan refuses what the server would refuse after the call. A set that
// replaces a TTL keeps the run interval the table held unless the declaration
// states one, since the call resets an interval it does not carry.
func planTTL(in ttlInput) (ttlChange, string) {
	if !in.managed {
		return ttlChange{}, ""
	}
	change := ttlChange{held: proto.CloneOf(in.held)}
	if in.desired == nil {
		if in.held == nil {
			return ttlChange{}, ""
		}
		change.drop = true
		return change, ""
	}
	column := ttlColumn(in.desired)
	ydbType, declared := in.declared[column]
	if !declared {
		return ttlChange{}, fmt.Sprintf("the TTL reads column %q, which the table does not declare", column)
	}
	if reason := TTLColumnRefusal(column, ydbType, in.desired.GetValueSinceUnixEpoch() != nil); reason != "" {
		return ttlChange{}, "the TTL " + reason
	}
	if sameTTL(in.desired, in.held) {
		return ttlChange{}, ""
	}
	change.set = proto.CloneOf(in.desired)
	if change.set.RunIntervalSeconds == 0 {
		change.set.RunIntervalSeconds = in.held.GetRunIntervalSeconds()
	}
	return change, ""
}

// declaredTypes maps each declared column to its YDB type name, and reports a
// column whose type the provider cannot spell.
func declaredTypes(columns []*protocol.DeclaredColumn) map[string]string {
	result := make(map[string]string, len(columns))
	for _, column := range columns {
		kind, err := schema.StorageType(column.Type, column.Nullable)
		if err != nil {
			result[column.Name] = column.Type
			continue
		}
		name, _, err := schema.ColumnType(kind)
		if err != nil {
			name = column.Type
		}
		result[column.Name] = name
	}
	return result
}
