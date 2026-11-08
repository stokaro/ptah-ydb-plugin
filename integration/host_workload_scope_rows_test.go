//go:build integration

package integration_test

import (
	"fmt"
	"regexp"
	"strings"
	"testing"

	qt "github.com/frankban/quicktest"
	"github.com/stokaro/ptah/provider-sdk/provider"

	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
)

// workloadNames are the database-wide objects of a workload scope test: a
// pool whose name holds a dot, a second pool, and a classifier routing to
// each of them.
type workloadNames struct{ pool, other, classifier, analysts string }

func newWorkloadNames(directory string) workloadNames {
	return workloadNames{pool: directory + ".batch", other: directory + "_other", classifier: directory + "_etl", analysts: directory + "_analysts"}
}

// workloadSource declares the two pools and the two classifiers of names for
// directory, each classifier routing a member no user holds.
func workloadSource(directory string, names workloadNames) string {
	first, second := workloadRanks(directory)
	return fmt.Sprintf("resource_pools:\n  %q:\n    concurrent_query_limit: 5\n  %s: {}\n"+
		"resource_pool_classifiers:\n  %s:\n    resource_pool: %q\n    member_name: %s\n    rank: %d\n"+
		"  %s:\n    resource_pool: %s\n    member_name: %s\n    rank: %d\n",
		names.pool, names.other, names.classifier, names.pool, workloadMember(directory), first, names.analysts, names.other, workloadMember(directory), second)
}

// Ported from the rows of the host's TestScopeWorkloadObjects that
// TestHostScopesResourcePoolsAndClassifiers_HappyPath leaves out. An
// exclusion of every classifier keeps both pools and no classifier on both
// sides, so the diff creates the pools alone. A schema selection that names
// only an unrelated directory keeps the database-wide classifier an include
// names and the pool it routes to, so the diff creates both: the dot in the
// pool's name names no directory.
func TestHostScopesWorkloadObjectsPastSchemaSelection_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	requireHostNegotiates(c, provider.ScopeSelectionsFeature)
	opened, directory, database := inspectionFixture(c)
	enableResourcePools(c, opened)
	unrelated := outsideDirectory(c, opened, database, directory)
	names := newWorkloadNames(directory)
	source := workloadSource(directory, names)

	excluded := strings.Join(scopedStatements(c, directory+",/", source, "--exclude", "*[type=resource_pool_classifier]"), "\n")
	included := scopedStatements(c, unrelated, source, "--include", names.classifier+"[type=resource_pool_classifier]")

	c.Assert(excluded, qt.Contains, "CREATE RESOURCE POOL `"+names.pool+"`")
	c.Assert(excluded, qt.Contains, "CREATE RESOURCE POOL `"+names.other+"`")
	c.Assert(excluded, qt.Not(qt.Contains), "CLASSIFIER")
	c.Assert(included, qt.HasLen, 2)
	c.Assert(included[0], qt.Matches, "CREATE RESOURCE POOL CLASSIFIER `"+regexp.QuoteMeta(names.classifier)+"` WITH \\(RESOURCE_POOL = '"+regexp.QuoteMeta(names.pool)+"', .*")
	c.Assert(included[1], qt.Matches, "CREATE RESOURCE POOL `"+regexp.QuoteMeta(names.pool)+"` WITH \\(CONCURRENT_QUERY_LIMIT = 5\\)")
}
