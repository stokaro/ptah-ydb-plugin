//go:build integration

package integration_test

import (
	"fmt"
	"path"
	"slices"
	"strings"
	"testing"

	qt "github.com/frankban/quicktest"

	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
)

// A Go source that cannot describe all coordination nodes keeps the nodes it
// names as not described, while the rest of the namespace remains managed.
// Both dotted and slash path spellings name the same node. Ported from the
// coordination-node rows of TestGoSourceStandaloneLimitsPreventRemoval.
func TestHostGoSourceStandaloneCoordinationLimits_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	opened, directory, database := lockFixture(c)
	for _, name := range []string{"kept", "dropped"} {
		createNode(c, opened, path.Join(database, directory, name))
	}
	for _, test := range []struct {
		name, directive string
		removed         []string
	}{
		{"managed omission", "", []string{"dropped", "kept"}},
		{"unmanaged namespace", `//ptah:schema:notdescribed kind="coordination_node"`, nil},
		{"kind case", `//ptah:schema:notdescribed kind="COORDINATION_NODE"`, nil},
		{"unmanaged node", `//ptah:schema:notdescribed kind="coordination_node" name="{dir}.kept"`, []string{"dropped"}},
		{"path spelling", `//ptah:schema:notdescribed kind="coordination_node" name="{dir}/kept"`, []string{"dropped"}},
		{"unmanaged schema", `//ptah:schema:notdescribed kind="schema" name="{dir}"`, nil},
		{"other schema", `//ptah:schema:notdescribed kind="schema" name="other"`, []string{"dropped", "kept"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			source := "package entities\n" + strings.ReplaceAll(test.directive, "{dir}", directory) + "\ntype _ struct{}\n"
			planned := hostPlanned(c, directory, "go", source)
			want := make([]string, 0, len(test.removed))
			for _, name := range test.removed {
				want = append(want, "coordination-node-operation "+directory+"."+name)
			}
			slices.Sort(planned)
			c.Assert(planned, qt.DeepEquals, want)
		})
	}
}

// The same Go coverage limits apply to standalone streaming queries. The
// source keeps all four topics, so each plan's query removals depend only on
// the not-described directive. This ports the streaming-query rows of the
// host's TestGoSourceStandaloneLimitsPreventRemoval.
func TestHostGoSourceStandaloneStreamingLimits_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	_, directory, _, _ := streamingFixture(c)
	topics := "topics:\n"
	queries := "streaming_queries:\n"
	goTopics := ""
	for _, name := range []string{"kept", "dropped"} {
		input, output := "input_"+name, "output_"+name
		topics += "  " + input + ": {schema: " + directory + "}\n  " + output + ": {schema: " + directory + "}\n"
		query := "INSERT INTO `" + directory + "/" + output + "` SELECT * FROM `" + directory + "/" + input + "`;"
		queries += fmt.Sprintf("  %s:\n    schema: %s\n    text: %q\n    run: false\n", name, directory, query)
		goTopics += fmt.Sprintf("//ptah:schema:topic name=%q schema=%q\ntype %s struct{}\n", input, directory, "Input"+name)
		goTopics += fmt.Sprintf("//ptah:schema:topic name=%q schema=%q\ntype %s struct{}\n", output, directory, "Output"+name)
	}
	applyRuns(c, directory, "yaml", topics+queries)
	for _, test := range []struct {
		name, directive string
		removed         []string
	}{
		{"managed omission", "", []string{"dropped", "kept"}},
		{"unmanaged namespace", `//ptah:schema:notdescribed kind="streaming_query"`, nil},
		{"kind case", `//ptah:schema:notdescribed kind="STREAMING_QUERY"`, nil},
		{"unmanaged query", `//ptah:schema:notdescribed kind="streaming_query" name="{dir}.kept"`, []string{"dropped"}},
		{"path spelling", `//ptah:schema:notdescribed kind="streaming_query" name="{dir}/kept"`, []string{"dropped"}},
		{"unmanaged schema", `//ptah:schema:notdescribed kind="schema" name="{dir}"`, nil},
		{"other schema", `//ptah:schema:notdescribed kind="schema" name="other"`, []string{"dropped", "kept"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			source := "package entities\n" + goTopics + strings.ReplaceAll(test.directive, "{dir}", directory) + "\ntype _ struct{}\n"
			planned := hostPlanned(c, directory, "go", source)
			want := make([]string, 0, len(test.removed))
			for _, name := range test.removed {
				want = append(want, "streaming-query-operation "+directory+"."+name)
			}
			slices.Sort(planned)
			c.Assert(planned, qt.DeepEquals, want)
		})
	}
}
