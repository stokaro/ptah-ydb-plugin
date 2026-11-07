//go:build integration

package integration_test

import (
	"path"
	"strings"
	"testing"

	qt "github.com/frankban/quicktest"
	"github.com/stokaro/ptah/provider-sdk/provider"

	"github.com/stokaro/ptah-ydb-plugin/internal/connection"
	"github.com/stokaro/ptah-ydb-plugin/internal/nativeapi"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
)

func vectorRollbackSource(directory, clusters string) string {
	return indexRollbackSource(directory, "{fields: [emb], type: vector_kmeans_tree, distance: cosine, "+
		"vector_type: float, vector_dimension: 3, levels: 1, clusters: "+clusters+"}")
}

func indexRollbackSource(directory, index string) string {
	return "tables:\n  docs:\n    schema: " + directory + "\n    columns:\n" +
		"      id: {type: Uint64, primary: true, not_null: true}\n" +
		"      emb: {type: String}\n    indexes:\n" +
		"      by_emb: " + index + "\n"
}

func heldVectorClusters(c *qt.C, opened *connection.Connection, absolute string) uint32 {
	c.Helper()
	session, err := nativeapi.OpenTable(c.Context(), opened.Driver)
	c.Assert(err, qt.IsNil)
	defer func() { c.Check(session.Close(), qt.IsNil) }()
	read, err := session.Describe(c.Context(), absolute)
	c.Assert(err, qt.IsNil)
	c.Assert(read.GetIndexes(), qt.HasLen, 1)
	return read.GetIndexes()[0].GetGlobalVectorKmeansTreeIndex().GetVectorSettings().GetClusters()
}

// A generated down migration rebuilds a vector index with the settings the
// database held before the forward migration. Both directions drop the old
// index before adding the replacement, and the final declaration is settled
// after each run. This ports the settings-change row of the former host's
// TestPlanBidirectionalSchemaDiff_VectorIndexSettingsRebuild.
func TestHostRollsBackVectorIndexSettings_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	requireHostNegotiates(c, provider.IndexKindsFeature)
	opened, directory, database := lockFixture(c)
	emptyAtCleanup(c, opened, path.Join(database, directory))
	absolute := path.Join(database, directory, "docs")
	before := vectorRollbackSource(directory, "2")
	after := vectorRollbackSource(directory, "4")
	created, createErr := hostApplySource(c, directory, "yaml", before)
	c.Assert(createErr, qt.IsNil, qt.Commentf("%s", created))
	c.Assert(heldVectorClusters(c, opened, absolute), qt.Equals, uint32(2))

	migrations, up, down := hostGenerate(c, directory, "yaml", after)
	c.Assert(strings.Index(up, "DROP INDEX `by_emb`") < strings.Index(up, "ADD INDEX `by_emb`"), qt.IsTrue, qt.Commentf("%s", up))
	c.Assert(up, qt.Contains, "clusters=4")
	c.Assert(strings.Index(down, "DROP INDEX `by_emb`") < strings.Index(down, "ADD INDEX `by_emb`"), qt.IsTrue, qt.Commentf("%s", down))
	c.Assert(down, qt.Contains, "clusters=2")
	c.Assert(down, qt.Contains, "Recovery limit:")

	applied, upErr := hostMigrate(c, directory, migrations, "migrations", "up")
	c.Assert(upErr, qt.IsNil, qt.Commentf("%s\n%s", applied, up))
	c.Assert(heldVectorClusters(c, opened, absolute), qt.Equals, uint32(4))
	c.Assert(hostPlanned(c, directory, "yaml", after), qt.HasLen, 0)

	rolledBack, downErr := hostMigrate(c, directory, migrations, "migrations", "down", "--target", "0", "--confirm")
	c.Assert(downErr, qt.IsNil, qt.Commentf("%s\n%s", rolledBack, down))
	c.Assert(heldVectorClusters(c, opened, absolute), qt.Equals, uint32(2))
	c.Assert(hostPlanned(c, directory, "yaml", before), qt.HasLen, 0)
}

// A rollback of global-to-vector replacement restores the original global
// index under the same name. The provider and host each own one half of the
// replacement, and both the forward and reverse migrations remove the old
// index before adding the new one. This ports the kind-change row of the
// former host's TestPlanBidirectionalSchemaDiff_VectorIndexSettingsRebuild.
func TestHostRollsBackGlobalIndexReplacedByVector_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	requireHostNegotiates(c, provider.IndexKindsFeature)
	opened, directory, database := lockFixture(c)
	emptyAtCleanup(c, opened, path.Join(database, directory))
	absolute := path.Join(database, directory, "docs")
	before := indexRollbackSource(directory, "{fields: [emb]}")
	after := vectorRollbackSource(directory, "2")
	created, createErr := hostApplySource(c, directory, "yaml", before)
	c.Assert(createErr, qt.IsNil, qt.Commentf("%s", created))
	indexes, _ := describedItems(c, opened, absolute)
	c.Assert(indexes["by_emb"].GetGlobalIndex(), qt.IsNotNil)

	migrations, up, down := hostGenerate(c, directory, "yaml", after)
	c.Assert(strings.Index(up, "DROP INDEX `by_emb`") < strings.Index(up, "ADD INDEX `by_emb`"), qt.IsTrue, qt.Commentf("%s", up))
	c.Assert(up, qt.Contains, "GLOBAL USING vector_kmeans_tree")
	c.Assert(strings.Index(down, "DROP INDEX `by_emb`") < strings.Index(down, "ADD INDEX `by_emb`"), qt.IsTrue, qt.Commentf("%s", down))
	c.Assert(down, qt.Contains, "ADD INDEX `by_emb` GLOBAL SYNC ON (`emb`)")
	c.Assert(down, qt.Contains, "Recovery limit:")

	applied, upErr := hostMigrate(c, directory, migrations, "migrations", "up")
	c.Assert(upErr, qt.IsNil, qt.Commentf("%s\n%s", applied, up))
	c.Assert(heldVectorClusters(c, opened, absolute), qt.Equals, uint32(2))
	c.Assert(hostPlanned(c, directory, "yaml", after), qt.HasLen, 0)

	rolledBack, downErr := hostMigrate(c, directory, migrations, "migrations", "down", "--target", "0", "--confirm")
	c.Assert(downErr, qt.IsNil, qt.Commentf("%s\n%s", rolledBack, down))
	indexes, _ = describedItems(c, opened, absolute)
	c.Assert(indexes["by_emb"].GetGlobalIndex(), qt.IsNotNil)
	c.Assert(hostPlanned(c, directory, "yaml", before), qt.HasLen, 0)
}
