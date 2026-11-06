//go:build integration

package integration_test

import (
	"os"
	"path"
	"strconv"
	"strings"
	"testing"

	qt "github.com/frankban/quicktest"
	"github.com/stokaro/ptah/provider-sdk/provider"

	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
)

// exportedObjects is the comment of the struct a Go export hangs the schema's
// standalone objects on, cut down to the lines of the objects named and one
// "grants" line where the grants stand, in the order the export writes them.
func exportedObjects(source string, objects ...string) []string {
	var kept []string
	for line := range strings.SplitSeq(source, "\n") {
		switch {
		case strings.HasPrefix(line, "//ptah:schema:grant "):
			if len(kept) == 0 || kept[len(kept)-1] != "grants" {
				kept = append(kept, "grants")
			}
		case strings.HasPrefix(line, "//ptah:schema:"):
			for _, object := range objects {
				if strings.Contains(line, ` name="`+object+`"`) {
					kept = append(kept, line)
				}
			}
		}
	}
	return kept
}

// A Go export writes a directory's topic and its consumer after the views,
// and the database's resource pools, their classifiers and the directory's
// coordination node after the default privileges, below the grants, each with
// its attributes in the order the host's embedded engine wrote them. The
// lines are the ones the host at 74de0b65b wrote for the same objects on YDB
// 25.1.4.7 and 26.2.1.14 (`ptah introspect --single-file`). A host that does
// not read Go placements writes the pools, the classifier and the node before
// the grants.
func TestHostGoExportPlacesTheObjectsAsTheBuiltInDid_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	requireHostNegotiates(c, provider.GoPlacementsFeature)
	opened, directory, database := lockFixture(c)
	enableResourcePools(c, opened)
	events, workers := path.Join(database, directory, "events"), path.Join(database, directory, "workers")
	pool, classifier, member := directory+"_pool", directory+"_class", workloadMember(directory)
	rank, _ := workloadRanks(directory)
	createSchemaObject(c, opened, "CREATE TOPIC `"+events+"` (CONSUMER reader WITH (important = true))", "DROP TOPIC `"+events+"`")
	createSchemaObject(c, opened, "CREATE RESOURCE POOL `"+pool+"` WITH (CONCURRENT_QUERY_LIMIT = 5, QUEUE_SIZE = 10, RESOURCE_WEIGHT = 3)",
		"DROP RESOURCE POOL `"+pool+"`")
	createSchemaObject(c, opened, "CREATE RESOURCE POOL CLASSIFIER `"+classifier+"` WITH (RESOURCE_POOL = '"+pool+"', MEMBER_NAME = '"+member+"', RANK = "+
		strconv.FormatInt(rank, 10)+")", "DROP RESOURCE POOL CLASSIFIER `"+classifier+"`")
	createNode(c, opened, workers)

	source := introspectDirectory(c, directory)

	c.Assert(exportedObjects(source, "events", "reader", pool, classifier, "workers"), qt.DeepEquals, []string{
		`//ptah:schema:topic name="events" schema="` + directory + `" min_active_partitions="1" auto_partitioning_strategy="disabled" retention_period="P1D" ` +
			`partition_write_speed_bytes_per_second="1048576" partition_write_burst_bytes="1048576"`,
		`//ptah:schema:topic:consumer name="reader" topic="events" schema="` + directory + `" important="true"`,
		"grants",
		`//ptah:schema:resourcepool name="` + pool + `" concurrent_query_limit="5" queue_size="10" resource_weight="3"`,
		`//ptah:schema:resourcepool:classifier name="` + classifier + `" resource_pool="` + pool + `" member_name="` + member + `" rank="` +
			strconv.FormatInt(rank, 10) + `"`,
		`//ptah:schema:coordinationnode name="workers" schema="` + directory + `" self_check_period="PT2S" session_grace_period="PT10S"`,
	}, qt.Commentf("%s", source))
}

// A read leaves the users and groups the cluster made for itself out of the
// description, and the host's note names that cause in the words of the
// host's embedded engine (measured with the host at 74de0b65b on YDB 25.1.4.7
// and 26.2.1.14). Without the provider's reason the host says that nothing in
// the inspected schemas refers to them, which is not why a YDB read leaves
// them out.
func TestHostRolesNoteNamesTheBuiltInCause_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	requireHostNegotiates(c, provider.RoleScopeReasonsFeature)
	_, directory, _ := lockFixture(c)

	output, err := hostCommand(c, "db", "read", "--db-url", os.Getenv("PTAH_YDB_TEST_URL"), "--schemas", directory)

	c.Assert(err, qt.IsNil, qt.Commentf("%s", output))
	c.Assert(output, qt.Matches, `(?s).*note: \d+ roles? Ptah manages on this server (is|are) not described, because the cluster made them or they own the `+
		`database, or the read was of a dev realm, which shares its database's users and groups; comparison still treats them as present, so none of `+
		`them is planned as a CREATE ROLE\.\n.*`)
}
