//go:build integration

package integration_test

import (
	"context"
	"strconv"
	"strings"
	"testing"
	"time"

	qt "github.com/frankban/quicktest"

	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
)

// A plan refuses declared classifiers YDB would keep differently from what
// they say, and writes nothing: two classifiers on one rank, which YDB
// refuses for the second (`Classifier with rank ... already exists`), and a
// classifier naming a pool the schema does not declare, which YDB takes and
// then runs the member's queries in the pool default without a word. The
// host's embedded engine refused both with the database holding neither
// pool (`cannot share rank`, and `it names resource pool ..., which is not
// declared`, measured with schema apply on 26.2.1.14). Without the pool check
// the plan created the classifier. Ported from the host's
// TestRender_ResourcePool_RefusesWhatYDBKeepsDifferently.
func TestHostRefusesAClassifierYDBKeepsDifferently_FailurePath(t *testing.T) {
	for _, test := range []struct {
		name, text, want string
	}{
		{name: "two classifiers on one rank",
			text: "resource_pool_classifiers:\n  {dir}_a:\n    resource_pool: default\n    member_name: {member}\n    rank: {rank}\n" +
				"  {dir}_b:\n    resource_pool: default\n    member_name: {member}\n    rank: {rank}\n",
			want: `resource pool classifier "{dir}_b": classifiers "{dir}_a" and "{dir}_b" cannot share rank {rank}`},
		{name: "a classifier naming a pool nobody declared",
			text: "resource_pool_classifiers:\n  {dir}_a:\n    resource_pool: {dir}_batch\n    member_name: {member}\n    rank: {rank}\n",
			want: `a declared resource pool classifier names resource pool "{dir}_batch", which is not declared; ` +
				`YDB runs the queries of a classifier whose pool does not exist in the pool "default" without a word`},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			opened, directory, _ := lockFixture(c)
			enableResourcePools(c, opened)
			rank, _ := workloadRanks(directory)
			names := strings.NewReplacer("{dir}", directory, "{member}", workloadMember(directory), "{rank}", strconv.FormatInt(rank, 10))
			// A classifier belongs to the database, not to the directory, so
			// one a regression creates would outlive the fixture.
			c.Cleanup(func() {
				ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
				defer cancel()
				for _, name := range []string{directory + "_a", directory + "_b"} {
					_ = execFixtureSchema(ctx, opened.DB, "DROP RESOURCE POOL CLASSIFIER `"+name+"`")
				}
			})

			refused, err := hostApplySource(c, directory, "yaml", names.Replace(test.text))

			c.Assert(err, qt.IsNotNil, qt.Commentf("%s", refused))
			c.Assert(refused, qt.Contains, names.Replace(test.want))
			c.Assert(refused, qt.Not(qt.Contains), "CREATE RESOURCE POOL CLASSIFIER")
		})
	}
}
