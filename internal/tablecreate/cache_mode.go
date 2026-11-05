package tablecreate

import (
	"fmt"
	"slices"

	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb_Table"

	"github.com/stokaro/ptah-ydb-plugin/internal/featureflags"
	native "github.com/stokaro/ptah-ydb-plugin/internal/nativeprotocol"
	"github.com/stokaro/ptah-ydb-plugin/schema"
)

// CacheModeCapability is a column family's cache mode: CACHE_MODE in YQL,
// and cache_mode of the table service's ColumnFamily. YDB takes it from 25.4
// on, and keeps it behind EnableTableCacheModes, off by default on 25.3. On
// 25.1.4.7, YQL answers `Unknown table setting: CACHE_MODE`, and the table
// service, which does not know the field, drops it and creates or alters the
// table without it, so no request may carry one where the capability is
// absent.
const CacheModeCapability = "column_family_cache_mode"

// cacheModeRefusal says why families cannot be written on a target without
// [CacheModeCapability], or returns "": a family that states a cache mode,
// regular included, needs the capability, as the host's embedded engine
// refuses any declared cache mode the target lacks the key for. subject names
// what writes them.
func cacheModeRefusal(subject string, families []*native.ColumnFamily, caps map[string]bool) string {
	stated := slices.ContainsFunc(families, func(family *native.ColumnFamily) bool {
		return family.GetCacheMode() != native.ColumnFamily_CACHE_MODE_UNSPECIFIED
	})
	if !stated || caps[CacheModeCapability] {
		return ""
	}
	return keyRefusal(CacheModeCapability, subject) + featureflags.Clause(CacheModeCapability)
}

// declaredCacheModeRefusal is [cacheModeRefusal] for the families the
// declaration of table states.
func declaredCacheModeRefusal(table string, families []*native.ColumnFamily, caps map[string]bool) string {
	return cacheModeRefusal(fmt.Sprintf("table %q declares column family cache mode", table), families, caps)
}

// creationCacheModeRefusal is [cacheModeRefusal] over the families a creation
// request writes.
func creationCacheModeRefusal(subject string, request *Ydb_Table.CreateTableRequest, caps map[string]bool) string {
	families := make([]*native.ColumnFamily, 0, len(request.GetColumnFamilies()))
	for _, family := range request.GetColumnFamilies() {
		read, err := schema.NativeFamily(family)
		if err != nil {
			return fmt.Sprintf("%s: %v", subject, err)
		}
		families = append(families, read)
	}
	return cacheModeRefusal(subject, families, caps)
}

// cacheModeText spells a cache mode as YQL takes it, or "" for none stated.
func cacheModeText(mode native.ColumnFamily_CacheMode) string {
	switch mode {
	case native.ColumnFamily_CACHE_MODE_REGULAR:
		return "regular"
	case native.ColumnFamily_CACHE_MODE_IN_MEMORY:
		return "in_memory"
	default:
		return ""
	}
}
