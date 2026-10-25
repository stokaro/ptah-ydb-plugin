package yqlschema

import (
	"maps"
	"slices"
	"strings"

	"github.com/stokaro/ptah-ydb-plugin/internal/ydbdecl"
)

func (p *parser) tableSettings(table *createTable) {
	var hash []string
	if p.word("PARTITION") {
		p.pos++
		p.wantWord("BY")
		p.wantWord("HASH")
		for _, name := range p.names() {
			hash = append(hash, decodedName(name))
		}
	}
	values := p.tableOptions()
	var store *ydbdecl.ColumnStore
	var err error
	if strings.EqualFold(values["store"], "COLUMN") {
		columnValues := map[string]string{"store": "column"}
		for _, name := range slices.Sorted(maps.Keys(values)) {
			value := values[name]
			switch name {
			case "store", "ttl":
			case ydbdecl.AttributeMinPartitions:
				columnValues[ydbdecl.AttributeShards] = value
			default:
				p.failf("unsupported column-table setting %q", name)
			}
		}
		store, err = ydbdecl.ParseColumnStore(columnValues)
		if err == nil {
			store.HashColumns = hash
			err = ydbdecl.CheckColumnStore(*store)
		}
	} else {
		if len(hash) > 0 {
			p.failf("PARTITION BY HASH requires STORE = COLUMN")
		}
		if store, ok := values["store"]; ok && !strings.EqualFold(store, "ROW") {
			p.failf("STORE must be ROW or COLUMN")
		}
		err = p.applyPartitioning(table, values)
	}
	if err != nil {
		p.failf("%v", err)
		return
	}
	if ttl, ok := values["ttl"]; ok {
		p.applyTTL(table, store, ttl)
	}
	if store == nil || p.err != nil {
		return
	}
	table.facets.ColumnStore = store
}

func (p *parser) tableOptions() map[string]string {
	values := make(map[string]string)
	if p.word("WITH") {
		p.pos++
		raw := p.optionsUsing(p.tableOptionValue)
		for _, name := range slices.Sorted(maps.Keys(raw)) {
			value := raw[name]
			if name != "store" && name != "ttl" && !slices.Contains(ydbdecl.TableAttributes(), name) {
				p.failf("unsupported table setting %q", name)
			}
			if name == "partition_at_keys" {
				points, err := splitPoints(value)
				if err != nil {
					p.failf("%v", err)
				}
				values[name] = ydbdecl.FormatSplitPoints(points)
				continue
			}
			values[name] = scalar(value)
		}
	}
	return values
}

// applyPartitioning gives a row table the settings its WITH clause states,
// and nothing where it states none.
func (p *parser) applyPartitioning(table *createTable, values map[string]string) error {
	partitioning, err := ydbdecl.ParseTablePartitioning(values)
	if err != nil || partitioning == nil {
		return err
	}
	table.facets.Partitioning = partitioning
	return nil
}
