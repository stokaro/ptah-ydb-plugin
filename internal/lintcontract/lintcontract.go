// Package lintcontract compares this module's golangci-lint configuration with
// the host's, so the provider stays held to the host's lint contract. The CI
// lint-contract job checks out the host's .golangci.yml at the revision
// scripts/resolve-host.sh names, and internal/cmd/lintcontract reports what
// Drift finds.
package lintcontract

import (
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strings"

	"gopkg.in/yaml.v3"
)

// Drift lists how module departs from host, one sentence per departure, in a
// stable order. An empty list means module keeps the host's contract.
//
// Module keeps the contract when it enables every linter and formatter host
// enables, spells every setting of a host linter or formatter as host does,
// uses host's exclusion presets, and carries exactly host's exclusion rules
// that name no path. Module may enable linters host does not, and set those
// as it likes.
//
// What names only one module is not compared: the run section, the gci
// prefix sections, the rowserrcheck packages, the depguard messages, the
// excluded paths, and every exclusion rule that names a path.
func Drift(host, module []byte) ([]string, error) {
	h, err := parse("host", host)
	if err != nil {
		return nil, err
	}
	m, err := parse("module", module)
	if err != nil {
		return nil, err
	}
	var drift []string
	drift = append(drift, differ("version", h["version"], m["version"])...)
	drift = append(drift, differ("issues", h["issues"], m["issues"])...)
	for _, section := range []string{"linters", "formatters"} {
		hs, ms := mapping(h[section]), mapping(m[section])
		drift = append(drift, differ(section+".default", hs["default"], ms["default"])...)
		drift = append(drift, missing(section+".enable", stringList(hs["enable"]), stringList(ms["enable"]))...)
		drift = append(drift, settings(section+".settings", mapping(hs["settings"]), mapping(ms["settings"]))...)
		drift = append(drift, exclusions(section+".exclusions", mapping(hs["exclusions"]), mapping(ms["exclusions"]))...)
	}
	return drift, nil
}

func parse(name string, data []byte) (map[string]any, error) {
	var config map[string]any
	if err := yaml.Unmarshal(data, &config); err != nil {
		return nil, fmt.Errorf("read the %s configuration: %w", name, err)
	}
	if config == nil {
		return nil, fmt.Errorf("the %s configuration is empty", name)
	}
	return config, nil
}

// mapping returns value as a YAML mapping, or nil when it is none.
func mapping(value any) map[string]any {
	m, _ := value.(map[string]any)
	return m
}

// stringList returns value as a list of strings, or nil when it is none.
func stringList(value any) []string {
	list, _ := value.([]any)
	items := make([]string, 0, len(list))
	for _, item := range list {
		items = append(items, fmt.Sprint(item))
	}
	return items
}

// missing reports each name of host that module lacks.
func missing(key string, host, module []string) []string {
	var drift []string
	for _, name := range host {
		if !slices.Contains(module, name) {
			drift = append(drift, fmt.Sprintf("%s: the host enables %s, and this module does not", key, name))
		}
	}
	return drift
}

// settings compares the settings of each linter or formatter host configures.
func settings(key string, host, module map[string]any) []string {
	var drift []string
	for _, name := range slices.Sorted(maps.Keys(host)) {
		drift = append(drift, differ(key+"."+name, contractOf(name, host[name]), contractOf(name, module[name]))...)
	}
	return drift
}

// contractOf leaves out of a linter's settings what names one module only.
func contractOf(linter string, value any) any {
	values := mapping(value)
	if values == nil {
		return value
	}
	values = maps.Clone(values)
	switch linter {
	case "rowserrcheck":
		delete(values, "packages")
	case "gci":
		values["sections"] = slices.DeleteFunc(stringList(values["sections"]), func(section string) bool {
			return strings.HasPrefix(section, "prefix(")
		})
	case "depguard":
		return withoutKey(values, "desc")
	case "revive":
		values["rules"] = byName(values["rules"])
	}
	return values
}

// withoutKey drops key from every mapping in value.
func withoutKey(value any, key string) any {
	switch v := value.(type) {
	case map[string]any:
		out := make(map[string]any, len(v))
		for k, item := range v {
			if k != key {
				out[k] = withoutKey(item, key)
			}
		}
		return out
	case []any:
		out := make([]any, 0, len(v))
		for _, item := range v {
			out = append(out, withoutKey(item, key))
		}
		return out
	default:
		return value
	}
}

// byName keys revive's rules on their names, so the order they are listed in
// does not count.
func byName(value any) map[string]any {
	list, _ := value.([]any)
	rules := make(map[string]any, len(list))
	for _, item := range list {
		rule := mapping(item)
		rules[fmt.Sprint(rule["name"])] = rule
	}
	return rules
}

// exclusions compares what excludes findings everywhere: the generated-code
// mode, the presets, and the rules that name no path.
func exclusions(key string, host, module map[string]any) []string {
	var drift []string
	drift = append(drift, differ(key+".generated", host["generated"], module["generated"])...)
	drift = append(drift, differ(key+".presets", sorted(stringList(host["presets"])), sorted(stringList(module["presets"])))...)
	hostRules, moduleRules := pathless(host["rules"]), pathless(module["rules"])
	for _, rule := range hostRules {
		if !slices.Contains(moduleRules, rule) {
			drift = append(drift, fmt.Sprintf("%s.rules: the host excludes %s, and this module does not", key, rule))
		}
	}
	for _, rule := range moduleRules {
		if !slices.Contains(hostRules, rule) {
			drift = append(drift, fmt.Sprintf("%s.rules: this module excludes %s, and the host does not", key, rule))
		}
	}
	return drift
}

func sorted(items []string) []string {
	slices.Sort(items)
	return items
}

// pathless renders each exclusion rule that names no path as canonical JSON,
// in sorted order.
func pathless(value any) []string {
	list, _ := value.([]any)
	rules := make([]string, 0, len(list))
	for _, item := range list {
		rule := mapping(item)
		_, path := rule["path"]
		_, except := rule["path-except"]
		if !path && !except {
			rules = append(rules, canonical(rule))
		}
	}
	return sorted(rules)
}

// differ reports key when host and module hold different values. Where both
// are mappings it reports each key of either that differs, so a departure is
// named by the setting that departs.
func differ(key string, host, module any) []string {
	if canonical(host) == canonical(module) {
		return nil
	}
	hostValues, hostMapping := host.(map[string]any)
	moduleValues, moduleMapping := module.(map[string]any)
	if !hostMapping || !moduleMapping {
		return []string{fmt.Sprintf("%s: the host has %s, and this module has %s", key, canonical(host), canonical(module))}
	}
	keys := slices.Collect(maps.Keys(hostValues))
	for name := range moduleValues {
		if _, ok := hostValues[name]; !ok {
			keys = append(keys, name)
		}
	}
	slices.Sort(keys)
	var drift []string
	for _, name := range keys {
		drift = append(drift, differ(key+"."+name, hostValues[name], moduleValues[name])...)
	}
	return drift
}

// canonical renders a value as JSON, which writes mapping keys in sorted
// order, so two values compare as text.
func canonical(value any) string {
	encoded, err := json.Marshal(value)
	if err != nil {
		return fmt.Sprintf("%#v", value)
	}
	return string(encoded)
}
