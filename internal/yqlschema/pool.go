package yqlschema

import (
	"maps"
	"slices"

	"github.com/stokaro/ptah-ydb-plugin/internal/workload"
)

// resourcePool reads `CREATE RESOURCE POOL <name> WITH (...)` and `CREATE
// RESOURCE POOL CLASSIFIER <name> WITH (...)`, after RESOURCE.
func (p *parser) resourcePool() statement {
	p.wantWord("POOL")
	if p.word("CLASSIFIER") {
		p.pos++
		return p.poolClassifier()
	}
	name := decodedName(p.identifier())
	p.wantWord("WITH")
	return p.poolSettings(name)
}

// defaultPool reads `ALTER RESOURCE POOL default SET (...)`, the spelling the
// host writes for the pool YDB creates with the database. It declares the
// pool's settings, since the server owns the pool; another pool is refused
// with the host's words.
func (p *parser) defaultPool() statement {
	p.wantWord("RESOURCE")
	p.wantWord("POOL")
	name := decodedName(p.identifier())
	if name != workload.DefaultPool {
		p.failf("only the server-owned default pool uses ALTER RESOURCE POOL in a desired schema")
	}
	p.wantWord("SET")
	return p.poolSettings(name)
}

func (p *parser) poolSettings(name string) *createPool {
	raw := p.options()
	values := map[string]string{workload.AttributeName: name}
	for _, key := range slices.Sorted(maps.Keys(raw)) {
		value := raw[key]
		if key == workload.AttributeName || !slices.Contains(workload.PoolAttributes(), key) {
			p.failf("unsupported resource pool setting %q", key)
		}
		// YQL spells an unset limit as the string "-1", and the model leaves
		// the setting out. An unquoted negative number is not YQL.
		if decoded, ok := stringLiteral(value, "String"); ok && decoded == "-1" {
			continue
		}
		values[key] = scalar(value)
	}
	parsed, spec, err := workload.ParsePool(values)
	if err != nil {
		p.failf("%v", err)
	}
	if parsed != name {
		p.failf("resource pool name %q cannot be represented exactly", name)
	}
	return &createPool{name: name, spec: spec}
}

func (p *parser) poolClassifier() *createClassifier {
	name := decodedName(p.identifier())
	if !p.word("WITH") {
		p.failf("a resource pool classifier needs WITH settings")
	}
	values := p.declarationSettings(classifierSetting)
	values[workload.AttributeName] = name
	parsed, spec, err := workload.ParseClassifier(values)
	if err != nil {
		p.failf("%v", err)
	}
	if parsed != name {
		p.failf("classifier name %q cannot be represented exactly", name)
	}
	return &createClassifier{name: name, spec: spec}
}

func classifierSetting(key string) string {
	if key == workload.AttributeName || !slices.Contains(workload.ClassifierAttributes(), key) {
		return "unsupported"
	}
	return ""
}
