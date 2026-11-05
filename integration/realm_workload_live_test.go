//go:build integration

package integration_test

import (
	"context"
	"os"
	"testing"
	"time"

	qt "github.com/frankban/quicktest"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"

	"github.com/stokaro/ptah-ydb-plugin/internal/connection"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
	"github.com/stokaro/ptah-ydb-plugin/internal/workload"
	"github.com/stokaro/ptah-ydb-plugin/schema"
)

// workloadRead is what an inspection reports of the resource pools and the
// classifiers: the names of the ones it read, and the knowledge it records
// of each kind, with the database each region it knows completely names.
type workloadRead struct {
	pools, classifiers []string
	knowledge          map[string]protocol.ModelKnowledge_State
	reasons            map[string]string
	complete           map[string][]string
}

func readWorkload(c *qt.C, snapshot *protocol.Schema) workloadRead {
	c.Helper()
	read := workloadRead{pools: make([]string, 0), classifiers: make([]string, 0), knowledge: make(map[string]protocol.ModelKnowledge_State), reasons: make(map[string]string),
		complete: make(map[string][]string)}
	for _, object := range snapshot.GetNativeObjects() {
		switch object.GetKind() {
		case schema.ResourcePoolProperties:
			read.pools = append(read.pools, object.GetOwner().GetName())
		case schema.ResourcePoolClassifierProperties:
			read.classifiers = append(read.classifiers, object.GetOwner().GetName())
		}
	}
	for _, kind := range snapshot.GetCoverage().GetKinds() {
		if schema.IsWorkloadObject(kind.GetModel().GetKind()) {
			read.knowledge[kind.GetModel().GetKind()] = kind.GetKnowledge().GetState()
			read.reasons[kind.GetModel().GetKind()] = kind.GetKnowledge().GetReason()
			read.complete[kind.GetModel().GetKind()] = make([]string, 0)
			for _, region := range kind.GetRegions() {
				if region.GetKnowledge().GetState() == protocol.ModelKnowledge_COMPLETE {
					read.complete[kind.GetModel().GetKind()] = append(read.complete[kind.GetModel().GetKind()], region.GetScope().GetDatabase().GetNormalized())
				}
			}
		}
	}
	return read
}

// A read of a dev realm reports no resource pool and no classifier, and
// records both kinds as uninspected, saying why, with no region it knows
// completely: a realm is a directory standing in for a database, and the
// pools a read would find belong to the whole database. The control is the
// same server read as the database, where the default pool is read and the
// database root is known completely.
func TestRealmInspectionLeavesResourcePoolsOutSubprocess(t *testing.T) {
	c := testcheck.New(t)
	base, err := connection.Parse(os.Getenv("PTAH_YDB_TEST_URL"))
	c.Assert(err, qt.IsNil)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	opened, err := connection.Open(ctx, base)
	c.Assert(err, qt.IsNil)
	c.Cleanup(func() { c.Check(opened.Close(), qt.IsNil) })
	enableResourcePools(c, opened)
	address, _ := realmFixture(c, opened, base)
	client := startProviderFor(c)
	realm, err := client.Open(ctx, &protocol.OpenRequest{Engine: "ydb", Address: address})
	c.Assert(err, qt.IsNil)
	c.Cleanup(func() { c.Check(client.CloseSession(context.Background(), realm.Id), qt.IsNil) })
	database, err := client.Open(ctx, &protocol.OpenRequest{Engine: "ydb", Address: os.Getenv("PTAH_YDB_TEST_URL")})
	c.Assert(err, qt.IsNil)
	c.Cleanup(func() { c.Check(client.CloseSession(context.Background(), database.Id), qt.IsNil) })

	inRealm, err := client.Inspect(ctx, &protocol.InspectRequest{SessionId: realm.Id})
	c.Assert(err, qt.IsNil)
	ofDatabase, err := client.Inspect(ctx, &protocol.InspectRequest{SessionId: database.Id, Schemas: []string{"/"}})
	c.Assert(err, qt.IsNil)

	c.Assert(realm.Capabilities[workload.CapabilityResourcePools], qt.IsTrue)
	realmRead := readWorkload(c, inRealm)
	c.Assert(realmRead.pools, qt.HasLen, 0)
	c.Assert(realmRead.classifiers, qt.HasLen, 0)
	reason := "A dev realm holds no resource pools or classifiers of its own: they belong to the whole database"
	c.Assert(realmRead.knowledge, qt.DeepEquals, map[string]protocol.ModelKnowledge_State{
		schema.ResourcePoolProperties: protocol.ModelKnowledge_UNINSPECTED, schema.ResourcePoolClassifierProperties: protocol.ModelKnowledge_UNINSPECTED,
	})
	c.Assert(realmRead.reasons, qt.DeepEquals, map[string]string{schema.ResourcePoolProperties: reason, schema.ResourcePoolClassifierProperties: reason})
	c.Assert(realmRead.complete, qt.DeepEquals, map[string][]string{schema.ResourcePoolProperties: {}, schema.ResourcePoolClassifierProperties: {}})
	databaseRead := readWorkload(c, ofDatabase)
	c.Assert(databaseRead.pools, qt.Contains, "default")
	c.Assert(databaseRead.complete, qt.DeepEquals, map[string][]string{
		schema.ResourcePoolProperties: {base.Database()}, schema.ResourcePoolClassifierProperties: {base.Database()},
	})
}
