//go:build integration

package integration_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path"
	"testing"
	"time"

	qt "github.com/frankban/quicktest"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/stokaro/ptah/provider-sdk/provider"
	providerruntime "github.com/stokaro/ptah/provider-sdk/runtime"
	"google.golang.org/protobuf/testing/protocmp"

	"github.com/stokaro/ptah-ydb-plugin/internal/connection"
	"github.com/stokaro/ptah-ydb-plugin/internal/seeds"
)

// seedsFixture is a session on a dev realm of its own, so the seed ledger,
// which lives at the root of the session's database, is the test's.
type seedsFixture struct {
	opened  *connection.Connection
	client  *providerruntime.Client
	session string
	root    string
}

func newSeedsFixture(c *qt.C) *seedsFixture {
	c.Helper()
	base, err := connection.Parse(os.Getenv("PTAH_YDB_TEST_URL"))
	c.Assert(err, qt.IsNil)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	opened, err := connection.Open(ctx, base)
	c.Assert(err, qt.IsNil)
	c.Cleanup(func() { c.Check(opened.Close(), qt.IsNil) })
	address, root := realmFixture(c, opened, base)
	binary, digest := providerBinary(c)
	client := startProvider(c, binary, digest)
	session, err := client.Open(ctx, &protocol.OpenRequest{Engine: "ydb", Address: address})
	c.Assert(err, qt.IsNil)
	c.Cleanup(func() { c.Check(client.CloseSession(context.Background(), session.Id), qt.IsNil) })
	f := &seedsFixture{opened: opened, client: client, session: session.Id, root: root}
	f.exec(c, "CREATE TABLE `regions` (id Int64 NOT NULL, name Utf8, PRIMARY KEY (id))")
	c.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		for _, table := range []string{"regions", seeds.Table} {
			c.Check(execFixtureSchema(cleanup, opened.DB, "DROP TABLE IF EXISTS `"+path.Join(root, table)+"`"), qt.IsNil)
		}
	})
	return f
}

func (f *seedsFixture) exec(c *qt.C, statement string) {
	c.Helper()
	c.Assert(executeSQL(c, f.client, f.session, "", statement).Outcome, qt.Equals, protocol.Execution_APPLIED)
}

func seedFile(name, text string) *protocol.Seed {
	digest := sha256.Sum256([]byte(text))
	return &protocol.Seed{Path: "seeds/" + name, Filename: name, Checksum: hex.EncodeToString(digest[:]), Text: text, Env: "dev"}
}

func (f *seedsFixture) apply(c *qt.C, seed *protocol.Seed) *protocol.SeedResult {
	c.Helper()
	result, _ := f.client.ApplySeed(c.Context(), &protocol.ApplySeedRequest{SessionId: f.session, RequestId: "apply", Seed: seed})
	c.Assert(result, qt.IsNotNil)
	return result
}

func (f *seedsFixture) ledger(c *qt.C) *protocol.SeedLedger {
	c.Helper()
	ledger, err := f.client.ReadSeedLedger(c.Context(), &protocol.ReadSeedLedgerRequest{SessionId: f.session, MaxEntries: 10})
	c.Assert(err, qt.IsNil)
	return ledger
}

func (f *seedsFixture) names(c *qt.C) []string {
	c.Helper()
	rows, err := f.opened.DB.QueryContext(c.Context(), "SELECT name FROM `"+path.Join(f.root, "regions")+"` ORDER BY id")
	c.Assert(err, qt.IsNil)
	defer func() { c.Check(rows.Close(), qt.IsNil) }()
	var names []string
	for rows.Next() {
		var name string
		c.Assert(rows.Scan(&name), qt.IsNil)
		names = append(names, name)
	}
	c.Assert(rows.Err(), qt.IsNil)
	return names
}

// TestSeedApplySubprocess runs a seed file and its ledger row together, and
// keeps nothing of a file that hits an existing key.
func TestSeedApplySubprocess(t *testing.T) {
	c := qt.New(t)
	f := newSeedsFixture(c)
	c.Assert(f.ledger(c), qt.CmpEquals(protocmp.Transform()), &protocol.SeedLedger{Exists: false, Complete: true})
	file := seedFile("regions.sql", "$north = 'North'u;\nINSERT INTO `regions` (id, name) VALUES (1, $north);\nUPSERT INTO `regions` (id, name) VALUES (2, 'South'u);\n")
	before := time.Now().Add(-time.Second)

	applied := f.apply(c, file)

	c.Assert(applied.GetOutcome(), qt.Equals, protocol.SeedResult_APPLIED)
	c.Assert(f.names(c), qt.DeepEquals, []string{"North", "South"})
	ledger := f.ledger(c)
	c.Assert(ledger.GetExists(), qt.IsTrue)
	c.Assert(ledger.GetEntries(), qt.HasLen, 1)
	entry := ledger.GetEntries()[0]
	c.Assert([]string{entry.GetPath(), entry.GetChecksum(), entry.GetEnv()}, qt.DeepEquals, []string{file.Path, file.Checksum, "dev"})
	c.Assert(time.Unix(entry.GetAppliedAtSeconds(), int64(entry.GetAppliedAtNanos())).After(before), qt.IsTrue)

	conflicting := f.apply(c, seedFile("conflict.sql", "UPSERT INTO `regions` (id, name) VALUES (3, 'East'u);\nINSERT INTO `regions` (id, name) VALUES (1, 'Again'u);\n"))

	c.Assert(conflicting.GetOutcome(), qt.Equals, protocol.SeedResult_KEY_CONFLICT)
	c.Assert(f.names(c), qt.DeepEquals, []string{"North", "South"})
	c.Assert(f.ledger(c).GetEntries(), qt.HasLen, 1)

	recorded, err := f.client.RecordSeed(c.Context(), &protocol.RecordSeedRequest{SessionId: f.session, RequestId: "record",
		Seed: &protocol.Seed{Path: "seeds/conflict.sql", Filename: "conflict.sql", Checksum: file.Checksum, Env: "dev"}})
	c.Assert(err, qt.IsNil)
	c.Assert(recorded.GetOutcome(), qt.Equals, protocol.SeedResult_APPLIED)
	c.Assert(f.ledger(c).GetEntries(), qt.HasLen, 2)
	c.Assert(f.names(c), qt.DeepEquals, []string{"North", "South"})
}

// TestSeedRefusalsSubprocess refuses, before anything runs, a seed that holds
// a scheme statement and one that writes outside the dev realm.
func TestSeedRefusalsSubprocess(t *testing.T) {
	c := qt.New(t)
	f := newSeedsFixture(c)
	for _, test := range []struct {
		name, text string
		line       uint32
	}{
		{"scheme", "UPSERT INTO `regions` (id, name) VALUES (5, 'Five'u);\nCREATE TABLE `other` (id Int64, PRIMARY KEY (id));\n", 2},
		{"outside the realm", "UPSERT INTO `/local/regions` (id, name) VALUES (5, 'Five'u);\n", 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := qt.New(t)

			refused := f.apply(c, seedFile(test.name+".sql", test.text))

			c.Assert(refused.GetOutcome(), qt.Equals, protocol.SeedResult_REFUSED)
			c.Assert(refused.GetPosition().GetLine(), qt.Equals, test.line)
			c.Assert(f.names(c), qt.HasLen, 0)
		})
	}
}

// TestSeedLedgerTheHostWroteSubprocess reads a ledger the host's embedded
// engine created and wrote, with the host's own statements, and adds to it.
func TestSeedLedgerTheHostWroteSubprocess(t *testing.T) {
	c := qt.New(t)
	f := newSeedsFixture(c)
	f.exec(c, "CREATE TABLE IF NOT EXISTS schema_seeds (\n    seed_path Utf8 NOT NULL,\n    env Utf8 NOT NULL,\n    checksum Utf8 NOT NULL,\n"+
		"    applied_at Timestamp NOT NULL,\n    PRIMARY KEY (seed_path)\n)")
	f.exec(c, "UPSERT INTO schema_seeds (seed_path, env, checksum, applied_at) VALUES "+
		"('seeds/old.sql'u, 'staging'u, '"+seedFile("old.sql", "x").Checksum+"'u, Timestamp('2026-01-02T03:04:05.000001Z'))")

	ledger := f.ledger(c)

	c.Assert(ledger.GetEntries(), qt.HasLen, 1)
	c.Assert(ledger.GetEntries()[0].GetAppliedAtSeconds(), qt.Equals, int64(1767323045))
	c.Assert(ledger.GetEntries()[0].GetAppliedAtNanos(), qt.Equals, int32(1000))
	c.Assert(f.apply(c, seedFile("new.sql", "UPSERT INTO `regions` (id, name) VALUES (1, 'One'u);")).GetOutcome(), qt.Equals, protocol.SeedResult_APPLIED)
	c.Assert(f.ledger(c).GetEntries(), qt.HasLen, 2)
}

// TestSeedLedgerOfAnotherLayoutSubprocess refuses a table named like the
// ledger that does not have its layout, and never adopts it.
func TestSeedLedgerOfAnotherLayoutSubprocess(t *testing.T) {
	c := qt.New(t)
	f := newSeedsFixture(c)
	f.exec(c, "CREATE TABLE schema_seeds (seed_path Utf8 NOT NULL, applied Bool, PRIMARY KEY (seed_path))")

	_, err := f.client.ReadSeedLedger(c.Context(), &protocol.ReadSeedLedgerRequest{SessionId: f.session, MaxEntries: 10})
	c.Assert(err, qt.IsNotNil)
	applied, err := f.client.ApplySeed(c.Context(), &protocol.ApplySeedRequest{SessionId: f.session, RequestId: "foreign",
		Seed: seedFile("one.sql", "UPSERT INTO `regions` (id, name) VALUES (1, 'One'u);")})

	c.Assert(err, qt.ErrorIs, provider.ErrExecution)
	c.Assert(applied.GetOutcome(), qt.Equals, protocol.SeedResult_NOT_APPLIED)
	c.Assert(f.names(c), qt.HasLen, 0)
}

// TestSeedTablesSubprocess lists the tables of the realm's root and of a
// directory that does not exist.
func TestSeedTablesSubprocess(t *testing.T) {
	c := qt.New(t)
	f := newSeedsFixture(c)

	listed, err := f.client.ListSeedTables(c.Context(), &protocol.ListSeedTablesRequest{SessionId: f.session})
	c.Assert(err, qt.IsNil)
	missing, err := f.client.ListSeedTables(c.Context(), &protocol.ListSeedTablesRequest{SessionId: f.session, Schema: "absent"})
	c.Assert(err, qt.IsNil)

	c.Assert(listed.GetTables(), qt.CmpEquals(protocmp.Transform()), []*protocol.Reference{{Kind: "table", Name: "regions"}})
	c.Assert(missing.GetTables(), qt.HasLen, 0)
}
