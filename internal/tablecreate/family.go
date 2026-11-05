package tablecreate

import (
	"fmt"
	"slices"

	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb_Table"
	"google.golang.org/protobuf/proto"

	native "github.com/stokaro/ptah-ydb-plugin/internal/nativeprotocol"
)

// DefaultFamily names the family every row table has. It holds the key columns
// and every column no other family names; the server reads it back as an
// empty name on a column.
const DefaultFamily = "default"

// familyInput is what the family rules read: the families and column
// placements a declaration states, and the ones the table holds.
//
// A setting the declaration leaves out is not compared, because a cluster's
// table profile gives every new table settings of its own: the table keeps what
// it holds. A family the declaration leaves out stays. A column is placed by a
// declaration that names it, and by a source that describes where every column
// sits, which places a column it names in no family in the default family
// (see PlacedInDefault); any other column keeps the family it sits in.
type familyInput struct {
	// desired are the families the declaration states, with the settings it states.
	desired []*native.ColumnFamily
	// placement maps a declared column to the family the declaration puts it in.
	// An empty family stands for the default family.
	placement map[string]string
	// held are the families the table holds, and heldPlacement the family each
	// of its columns sits in.
	held          []*native.ColumnFamily
	heldPlacement map[string]string
	// columns are the columns the declaration names, key the declared key, and
	// existing the columns the table holds. A column the plan drops is in
	// neither columns nor key, so it is never moved.
	columns  []string
	key      []string
	existing map[string]bool
}

// familyChange is what one AlterTable call does to a table's families.
type familyChange struct {
	add   []*native.ColumnFamily
	alter []*native.ColumnFamily
	// move lists columns with the family they move to, the default family spelled
	// DefaultFamily.
	move []*Ydb_Table.ColumnMeta
	// heldFamilies are the touched families as the table holds them, and
	// heldColumns the family each moved column sits in now.
	heldFamilies []*native.ColumnFamily
	heldColumns  map[string]string
}

func (c familyChange) empty() bool { return len(c.add)+len(c.alter)+len(c.move) == 0 }

// familyKey spells a family name the way the rules compare it.
func familyKey(name string) string {
	if name == "" {
		return DefaultFamily
	}
	return name
}

func findFamily(families []*native.ColumnFamily, name string) *native.ColumnFamily {
	for _, family := range families {
		if familyKey(family.GetName()) == familyKey(name) {
			return family
		}
	}
	return nil
}

// sameCompression reports whether a table holding held already satisfies a
// declared compression. A server reads back a family with no compression of its
// own as uncompressed, so a declared NONE matches an unspecified held value.
func sameCompression(want, held Ydb_Table.ColumnFamily_Compression) bool {
	if want == Ydb_Table.ColumnFamily_COMPRESSION_UNSPECIFIED {
		return true
	}
	if want == Ydb_Table.ColumnFamily_COMPRESSION_NONE {
		return held == Ydb_Table.ColumnFamily_COMPRESSION_NONE || held == Ydb_Table.ColumnFamily_COMPRESSION_UNSPECIFIED
	}
	return want == held
}

// familyDelta reports which settings of the declared family want a held family
// does not satisfy: the storage pool, the compression, and the cache mode. A
// stated cache mode is compared as the table holds it, as the host's embedded
// engine compares it: a family that never had one reads back with none, so a
// declared regular cache is set once and then holds.
func familyDelta(want, held *native.ColumnFamily) (data, compression, cacheMode bool) {
	return want.GetData().GetMedia() != "" && want.GetData().GetMedia() != held.GetData().GetMedia(),
		!sameCompression(want.GetCompression(), held.GetCompression()),
		want.GetCacheMode() != native.ColumnFamily_CACHE_MODE_UNSPECIFIED && want.GetCacheMode() != held.GetCacheMode()
}

// planFamilies returns the change that gives the table what the declaration
// states, or the reason it cannot be made. An empty change means the table
// already satisfies the declaration.
func planFamilies(in familyInput) (familyChange, string) {
	change := familyChange{heldColumns: make(map[string]string)}
	if reason := planFamilySettings(in, &change); reason != "" {
		return familyChange{}, reason
	}
	if reason := planColumnMoves(in, &change); reason != "" {
		return familyChange{}, reason
	}
	return change, ""
}

// planFamilySettings adds to change, in name order, each declared family the
// table lacks and each settings change of a family it holds. It returns the
// reason a family cannot be planned.
func planFamilySettings(in familyInput, change *familyChange) string {
	names := make([]string, 0, len(in.desired))
	for _, family := range in.desired {
		names = append(names, familyKey(family.GetName()))
	}
	slices.Sort(names)
	for _, name := range names {
		want := findFamily(in.desired, name)
		if want.GetKeepInMemory() == Ydb.FeatureFlag_ENABLED && findFamily(in.held, name).GetKeepInMemory() != Ydb.FeatureFlag_ENABLED {
			return fmt.Sprintf("column family %q keeps its columns in memory (keep_in_memory) on one side only, and the table service refuses to set it", name)
		}
		held := findFamily(in.held, name)
		stated := &native.ColumnFamily{Name: name, Compression: want.GetCompression(), CacheMode: want.GetCacheMode()}
		if want.GetData().GetMedia() != "" {
			stated.Data = &Ydb_Table.StoragePool{Media: want.Data.Media}
		}
		if held == nil {
			if name == DefaultFamily {
				// A table always has its default family; a read that lacks it is incomplete.
				return "the table's default column family was not captured"
			}
			change.add = append(change.add, stated)
			continue
		}
		changed := &native.ColumnFamily{Name: name}
		data, compression, cacheMode := familyDelta(want, held)
		if data {
			changed.Data = stated.Data
		}
		if compression {
			changed.Compression = stated.Compression
		}
		if cacheMode {
			changed.CacheMode = stated.CacheMode
		}
		if data || compression || cacheMode {
			change.alter = append(change.alter, changed)
			change.heldFamilies = append(change.heldFamilies, proto.CloneOf(held))
		}
	}
	return ""
}

// planColumnMoves adds to change, in column order, the move of each column
// placed in another family than the one it sits in. It returns the reason a
// placement cannot be made: a family neither side has, or a key column
// outside the default family.
func planColumnMoves(in familyInput, change *familyChange) string {
	declared := func(name string) bool {
		return familyKey(name) == DefaultFamily || findFamily(in.desired, name) != nil || findFamily(in.held, name) != nil
	}
	for _, column := range slices.Sorted(slices.Values(in.columns)) {
		family, stated := in.placement[column]
		if !stated {
			continue
		}
		if !declared(family) {
			return fmt.Sprintf("column %q names column family %q, which neither the declaration nor the table has", column, family)
		}
		if slices.Contains(in.key, column) && familyKey(family) != DefaultFamily {
			return fmt.Sprintf("column family %q names key column %q, and YDB keeps every key column in the default family", family, column)
		}
		if familyKey(family) == familyKey(in.heldPlacement[column]) {
			continue
		}
		change.move = append(change.move, &Ydb_Table.ColumnMeta{Name: column, Family: familyKey(family)})
		change.heldColumns[column] = in.heldPlacement[column]
	}
	return ""
}
