package yqlschema

import (
	"slices"
	"strings"

	"github.com/stokaro/ptah-ydb-plugin/internal/ydbdecl"
)

func (p *parser) coordination() *createCoordinationNode {
	p.wantWord("NODE")
	name := p.path()
	spec, err := ydbdecl.ParseCoordinationNode(p.declarationSettings(coordinationSetting))
	if err != nil {
		p.failf("%v", err)
	}
	ref, ok := parseTableRef(name)
	if !ok {
		p.failf("invalid coordination node path %q", name)
	}
	nodePath := ref.name
	if ref.schema != "" {
		nodePath = ref.schema + "/" + ref.name
	}
	schema, leaf := "", nodePath
	if slash := strings.LastIndex(nodePath, "/"); slash >= 0 {
		schema, leaf = nodePath[:slash], nodePath[slash+1:]
	}
	return &createCoordinationNode{schema: schema, name: leaf, spec: spec}
}

func coordinationSetting(key string) string {
	if !slices.Contains(ydbdecl.CoordinationSettings(), key) {
		return "unsupported"
	}
	if key == ydbdecl.SettingSelfCheckPeriod || key == ydbdecl.SettingSessionGracePeriod {
		return "Interval"
	}
	return ""
}
