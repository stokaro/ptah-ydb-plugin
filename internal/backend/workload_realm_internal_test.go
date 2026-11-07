package backend

// White-box testing required: a session opens a dev realm only against a live
// server, and which workload read the session selects is observable only in
// what that server's views answer.

import (
	"testing"

	qt "github.com/frankban/quicktest"

	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
	"github.com/stokaro/ptah-ydb-plugin/internal/workload"
)

// A session in a dev realm reads no resource pools or classifiers: they belong
// to the whole database, and a read of the realm takes none of them as its
// own, so the realm's are left unread with the reason. Ported from the host's
// TestRehearsalReadPreservesUnknownAndRefusesUnrelatedEnvironment, whose
// rehearsal read refused the workload of another environment.
func TestRealmSessionReadsNoWorkload(t *testing.T) {
	c := testcheck.New(t)
	realm := &session{realm: true, capabilities: map[string]bool{workload.CapabilityResourcePools: true}}

	read, unread := realm.workload()

	c.Assert(read, qt.IsNil)
	c.Assert(unread, qt.Equals, "A dev realm holds no resource pools or classifiers of its own: they belong to the whole database")
}

// The control: a session of the database that keeps resource pools reads
// them.
func TestDatabaseSessionReadsTheWorkload(t *testing.T) {
	c := testcheck.New(t)
	database := &session{capabilities: map[string]bool{workload.CapabilityResourcePools: true}}

	read, unread := database.workload()

	c.Assert(read, qt.IsNotNil)
	c.Assert(unread, qt.Equals, "")
}
