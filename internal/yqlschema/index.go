package yqlschema

import (
	"maps"
	"slices"
	"strings"

	"github.com/stokaro/ptah-ydb-plugin/internal/ydbdecl"
)

func (p *parser) index(table string) *index {
	index := &index{name: p.identifier(), table: table, kind: "sync"}
	p.indexKind(index)
	p.wantWord("ON")
	index.columns = p.names()
	if p.word("COVER") {
		p.pos++
		index.include = p.names()
	}
	if p.word("WITH") {
		p.pos++
		p.indexSettings(index, p.options())
	}
	return index
}

func (p *parser) indexSettings(index *index, raw map[string]string) {
	values := make(map[string]string)
	kind, kindErr := ydbdecl.IndexKindOf(index.kind)
	if kindErr != nil {
		p.failf("%v", kindErr)
		return
	}
	var allowed []string
	switch {
	case kind == ydbdecl.IndexVector:
		allowed = slices.Concat(ydbdecl.PartitioningAttributes(), ydbdecl.VectorAttributes())
	case kind.IsFullText():
		allowed = ydbdecl.FullTextAttributes()
	case kind.IsLocal():
		allowed = ydbdecl.LocalAttributes()
	default:
		allowed = ydbdecl.PartitioningAttributes()
	}
	for _, name := range slices.Sorted(maps.Keys(raw)) {
		value := raw[name]
		if !slices.Contains(allowed, name) {
			p.failf("unsupported index setting %q", name)
		}
		values[name] = scalar(value)
	}
	partitioning, err := ydbdecl.ParseIndexPartitioning(values)
	if err != nil {
		p.failf("%v", err)
	}
	index.partitioning = partitioning
	vector, err := ydbdecl.DeclareVector(values, index.kind)
	if err != nil {
		p.failf("%v", err)
	}
	index.vector = vector
	values["type"] = strings.ToLower(index.kind)
	if index.storage, err = ydbdecl.ParseIndexOptions(values); err != nil {
		p.failf("%v", err)
	}
}

func (p *parser) indexKind(index *index) {
	local := p.word("LOCAL")
	if local {
		p.pos++
	} else {
		p.wantWord("GLOBAL")
	}
	if p.word("UNIQUE") {
		p.pos++
		index.unique = true
	}
	mode := p.word("ASYNC") || p.word("SYNC")
	async := p.word("ASYNC")
	if p.word("ASYNC") {
		p.pos++
		index.kind = "async"
	} else if p.word("SYNC") {
		p.pos++
	}
	using := p.word("USING")
	if using {
		if async || index.unique || local && mode {
			p.failf("USING cannot be combined with ASYNC, UNIQUE or a local index mode")
		}
		p.pos++
		index.kind = decodedName(p.identifier())
	} else if local {
		p.failf("LOCAL indexes require USING and an index method")
	}
	kind, err := ydbdecl.IndexKindOf(index.kind)
	if err != nil {
		p.failf("%v", err)
	}
	if using && (kind == ydbdecl.IndexSync || kind == ydbdecl.IndexAsync) {
		p.failf("USING requires a YDB vector, full-text or local index method")
	}
	if kind.IsLocal() != local {
		p.failf("index method %q does not match its GLOBAL or LOCAL scope", index.kind)
	}
	if index.unique && index.kind != "sync" {
		p.failf("UNIQUE requires a synchronous global index")
	}
}
