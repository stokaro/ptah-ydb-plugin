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

// goKindCaseChange is the host change that reads the kind a Go not-described
// directive names in any case, which Ptah v0.23.0 refuses before it calls the
// provider.
const goKindCaseChange = "stokaro/ptah#5775, which reads a Go coverage kind in any case"

// goLimitRow is one not-described directive of a Go source, {dir} standing for
// the test's directory, and the objects of the directory a plan of the source
// removes.
type goLimitRow struct {
	name, directive string
	removed         []string
}

// A Go source that cannot describe all coordination nodes keeps the nodes it
// names as not described, while the rest of the namespace remains managed.
// Both dotted and slash path spellings name the same node. Ported from the
// coordination-node rows of TestGoSourceStandaloneLimitsPreventRemoval.
func TestHostGoSourceStandaloneCoordinationLimits_HappyPath(t *testing.T) {
	coordinationLimits(testcheck.New(t), t, []goLimitRow{
		{"managed omission", "", []string{"dropped", "kept"}},
		{"unmanaged namespace", `//ptah:schema:notdescribed kind="coordination_node"`, nil},
		{"unmanaged node", `//ptah:schema:notdescribed kind="coordination_node" name="{dir}.kept"`, []string{"dropped"}},
		{"path spelling", `//ptah:schema:notdescribed kind="coordination_node" name="{dir}/kept"`, []string{"dropped"}},
		{"unmanaged schema", `//ptah:schema:notdescribed kind="schema" name="{dir}"`, nil},
		{"other schema", `//ptah:schema:notdescribed kind="schema" name="other"`, []string{"dropped", "kept"}},
	})
}

// The kind of a coordination-node limit is read in any case.
func TestHostGoSourceStandaloneCoordinationLimitKindCase_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	requireHostChange(c, goKindCaseChange)
	coordinationLimits(c, t, []goLimitRow{{"kind case", `//ptah:schema:notdescribed kind="COORDINATION_NODE"`, nil}})
}

// coordinationLimits plans a Go source with each row's directive against a
// directory holding the coordination nodes kept and dropped.
func coordinationLimits(c *qt.C, t *testing.T, rows []goLimitRow) {
	c.Helper()
	opened, directory, database := lockFixture(c)
	for _, name := range []string{"kept", "dropped"} {
		createNode(c, opened, path.Join(database, directory, name))
	}
	for _, test := range rows {
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
	streamingLimits(testcheck.New(t), t, []goLimitRow{
		{"managed omission", "", []string{"dropped", "kept"}},
		{"unmanaged namespace", `//ptah:schema:notdescribed kind="streaming_query"`, nil},
		{"unmanaged query", `//ptah:schema:notdescribed kind="streaming_query" name="{dir}.kept"`, []string{"dropped"}},
		{"path spelling", `//ptah:schema:notdescribed kind="streaming_query" name="{dir}/kept"`, []string{"dropped"}},
		{"unmanaged schema", `//ptah:schema:notdescribed kind="schema" name="{dir}"`, nil},
		{"other schema", `//ptah:schema:notdescribed kind="schema" name="other"`, []string{"dropped", "kept"}},
	})
}

// The kind of a streaming-query limit is read in any case.
func TestHostGoSourceStandaloneStreamingLimitKindCase_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	requireHostChange(c, goKindCaseChange)
	streamingLimits(c, t, []goLimitRow{{"kind case", `//ptah:schema:notdescribed kind="STREAMING_QUERY"`, nil}})
}

// streamingLimits plans a Go source with each row's directive against a
// directory holding the streaming queries kept and dropped and their topics.
func streamingLimits(c *qt.C, t *testing.T, rows []goLimitRow) {
	c.Helper()
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
	for _, test := range rows {
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
