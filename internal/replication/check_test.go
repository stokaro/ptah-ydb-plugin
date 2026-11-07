package replication_test

import (
	"testing"

	qt "github.com/frankban/quicktest"

	ydbreplication "github.com/stokaro/ptah-ydb-plugin/internal/replication"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
	"github.com/stokaro/ptah-ydb-plugin/internal/ydbtype"
)

func mirror() ydbreplication.ReplicationSpec {
	return ydbreplication.ReplicationSpec{Connection: ydbreplication.Connection{ConnectionString: "grpc://primary:2136/?database=/prod", TokenSecretName: "token"}, Items: []ydbreplication.Item{{Source: "accounts", Target: "replica/accounts"}}}
}
func ingest() ydbreplication.TransferSpec {
	return ydbreplication.TransferSpec{Source: "orders/feed", Target: "order_log", Lambda: "($m) -> { return []; }"}
}

func TestReplicationChangeRequiresPausedCapture(t *testing.T) {
	before := mirror()
	after := mirror()
	after.Connection.TokenSecretName = "rotated"
	for _, test := range []struct {
		name, state string
		refused     bool
	}{
		{name: "paused", state: ydbreplication.StatePaused}, {name: "running", state: ydbreplication.StateRunning, refused: true},
		{name: "unknown", refused: true}, {name: "failed over", state: ydbreplication.StateDone, refused: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			c.Assert(ydbreplication.ReplicationChangeRefusal("mirror", after, before, test.state) != nil, qt.Equals, test.refused)
		})
	}
}

func TestReplicationCapabilityAndImmutableState(t *testing.T) {
	c := testcheck.New(t)
	spec := mirror()
	c.Assert(ydbreplication.CheckReplication("mirror", spec, ydbtype.Capabilities{}).Key, qt.Equals, ydbreplication.CapabilityReplication)
	c.Assert(ydbreplication.CheckReplication("mirror", spec, ydbtype.Capabilities{ydbreplication.CapabilityReplication: true}), qt.Equals, (*ydbreplication.Refusal)(nil))
	changed := mirror()
	changed.Items[0].Target = "other"
	c.Assert(ydbreplication.ReplicationChangeRefusal("mirror", changed, spec, ydbreplication.StatePaused), qt.IsNotNil)
	changed = mirror()
	changed.Connection.TokenSecretName = ""
	c.Assert(ydbreplication.ReplicationChangeRefusal("mirror", changed, spec, ydbreplication.StatePaused), qt.IsNotNil)
	byPath := mirror()
	byPath.Connection.TokenSecretName = ""
	byPath.Connection.TokenSecretPath = "secrets/token"
	c.Assert(ydbreplication.CheckReplication("mirror", byPath, ydbtype.Capabilities{ydbreplication.CapabilityReplication: true}).Key, qt.Equals, ydbreplication.CapabilitySecretPaths)
}

func TestTransferChangePreservesConsumerAndInputPaths(t *testing.T) {
	before := ingest()
	for _, test := range []struct {
		name    string
		change  func(*ydbreplication.TransferSpec)
		refused bool
	}{
		{name: "lambda", change: func(s *ydbreplication.TransferSpec) { s.Lambda = "($m) -> { return [1]; }" }},
		{name: "batch", change: func(s *ydbreplication.TransferSpec) { s.BatchSizeBytes = 1024 }},
		{name: "source", change: func(s *ydbreplication.TransferSpec) { s.Source = "other/feed" }, refused: true},
		{name: "target", change: func(s *ydbreplication.TransferSpec) { s.Target = "other" }, refused: true},
		{name: "consumer", change: func(s *ydbreplication.TransferSpec) { s.Consumer = "owned" }, refused: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			after := before
			test.change(&after)
			c.Assert(ydbreplication.TransferChangeRefusal("ingest", after, before, ydbreplication.StateRunning) != nil, qt.Equals, test.refused)
		})
	}
}

func TestStatementsPreserveLiteralDotsAndEscapeIdentifiers(t *testing.T) {
	c := testcheck.New(t)
	c.Assert(ydbreplication.DropReplicationStatement("app/mirror.one", true), qt.Equals, "DROP ASYNC REPLICATION `app/mirror.one` CASCADE;")
	c.Assert(ydbreplication.DropTransferStatement("app/in`gest"), qt.Equals, "DROP TRANSFER `app/in\\`gest`;")
}

// A replication is accepted where the target has the family, a token secret
// named by its path where the target takes one. Ported from the host's
// TestCheckReplication_HappyPath, whose 26.2 and 25.4 lines hold both keys.
func TestCheckReplication_HappyPath(t *testing.T) {
	byPath := mirror()
	byPath.Connection.TokenSecretName, byPath.Connection.TokenSecretPath = "", "secrets/token"
	for _, test := range []struct {
		name string
		spec ydbreplication.ReplicationSpec
		caps ydbtype.Capabilities
	}{
		{name: "a token secret by name", spec: mirror(), caps: ydbtype.Capabilities{ydbreplication.CapabilityReplication: true}},
		{name: "a token secret by path", spec: byPath,
			caps: ydbtype.Capabilities{ydbreplication.CapabilityReplication: true, ydbreplication.CapabilitySecretPaths: true}},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			c.Assert(ydbreplication.CheckReplication("mirror", test.spec, test.caps), qt.Equals, (*ydbreplication.Refusal)(nil))
		})
	}
}

// A transfer is refused where the target lacks the family, and one naming a
// secret by its path where the target takes none. Ported from the host's
// TestCheckTransfer_FailurePath (25.1 and 25.2).
func TestCheckTransfer_FailurePath(t *testing.T) {
	remote := ingest()
	remote.Connection = ydbreplication.Connection{ConnectionString: "grpc://primary:2136/?database=/prod", TokenSecretPath: "secrets/token"}
	for _, test := range []struct {
		name string
		spec ydbreplication.TransferSpec
		caps ydbtype.Capabilities
		want ydbreplication.Refusal
	}{
		{name: "no transfers", spec: ingest(), caps: ydbtype.Capabilities{},
			want: ydbreplication.Refusal{Subject: "transfer ingest", Key: ydbreplication.CapabilityTransfers}},
		{name: "a secret path without the key", spec: remote, caps: ydbtype.Capabilities{ydbreplication.CapabilityTransfers: true},
			want: ydbreplication.Refusal{Subject: "transfer ingest names a secret by its path", Key: ydbreplication.CapabilitySecretPaths}},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			got := ydbreplication.CheckTransfer("ingest", test.spec, test.caps)
			c.Assert(got, qt.Not(qt.Equals), (*ydbreplication.Refusal)(nil))
			c.Assert(*got, qt.DeepEquals, test.want)
		})
	}
}

// A replication's change YDB makes in no replication, and a connection change
// it takes only while paused, are refused with the host's words. Ported from
// the host's TestReplicationChangeRefusal_FailurePath.
func TestReplicationChangeRefusal_FailurePath(t *testing.T) {
	moved := mirror()
	moved.Connection.ConnectionString = "grpcs://standby:2135/?database=/prod"
	global := mirror()
	global.ConsistencyLevel = ydbreplication.ConsistencyGlobal
	retargeted := mirror()
	retargeted.Items = []ydbreplication.Item{{Source: "accounts", Target: "copy/accounts"}}
	tokenless := mirror()
	tokenless.Connection.TokenSecretName = ""
	for _, test := range []struct {
		name    string
		desired ydbreplication.ReplicationSpec
		state   string
		want    string
	}{
		{name: "the level", desired: global, state: ydbreplication.StatePaused,
			want: "its consistency_level differ from the database's, and YDB changes none of them in place " +
				"(`CONSISTENCY_LEVEL is not supported in ALTER`, and ALTER takes no FOR clause); drop it with " +
				"DROP ASYNC REPLICATION ... CASCADE, which drops its replica tables, and plan again"},
		{name: "the items", desired: retargeted, state: ydbreplication.StatePaused,
			want: "its items differ from the database's, and YDB changes none of them in place " +
				"(`CONSISTENCY_LEVEL is not supported in ALTER`, and ALTER takes no FOR clause); drop it with " +
				"DROP ASYNC REPLICATION ... CASCADE, which drops its replica tables, and plan again"},
		{name: "a connection after failover", desired: moved, state: ydbreplication.StateDone,
			want: "it was failed over (STATE = 'DONE') and replicates nothing more, so its connection no longer " +
				"applies; remove it from the schema, which drops it and keeps its tables"},
		{name: "a credential taken away", desired: tokenless, state: ydbreplication.StatePaused,
			want: "it is declared with no credential and holds one, and YDB has no statement that takes a credential away"},
		{name: "a connection while running", desired: moved, state: ydbreplication.StateRunning,
			want: "its connection or credential differs, and YDB changes them only while the replication is paused " +
				"(`Modifications are not allowed in StandBy state`), which it is not (it is running); pause it with " +
				"ALTER ASYNC REPLICATION `mirror` SET (STATE = 'PAUSED'), apply, and resume it with SET (STATE = 'StandBy')"},
		{name: "a connection in error", desired: moved, state: ydbreplication.StateError,
			want: "its connection or credential differs, and YDB changes them only while the replication is paused " +
				"(`Modifications are not allowed in StandBy state`), which it is not (it is error); pause it with " +
				"ALTER ASYNC REPLICATION `mirror` SET (STATE = 'PAUSED'), apply, and resume it with SET (STATE = 'StandBy')"},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			got := ydbreplication.ReplicationChangeRefusal("mirror", test.desired, mirror(), test.state)
			c.Assert(got, qt.Not(qt.Equals), (*ydbreplication.Refusal)(nil))
			c.Assert(got.Subject, qt.Equals, "async replication mirror")
			c.Assert(got.Reason, qt.Equals, test.want)
		})
	}
}

// What YDB changes in a transfer in any state, the lambda and the batch
// settings, and a connection change of a paused transfer, pass. Ported from
// the host's TestTransferChangeRefusal_HappyPath.
func TestTransferChangeRefusal_HappyPath(t *testing.T) {
	relambda := ingest()
	relambda.Lambda = "($m) -> { return [<| id: 1 |>]; }"
	rebatched := ingest()
	rebatched.BatchSizeBytes = 1024
	remote := ingest()
	remote.Connection.ConnectionString = "grpc://primary:2136/?database=/prod"
	moved := remote
	moved.Connection.ConnectionString = "grpc://standby:2136/?database=/prod"
	for _, test := range []struct {
		name             string
		desired, current ydbreplication.TransferSpec
		state            string
	}{
		{name: "the lambda while running", desired: relambda, current: ingest(), state: ydbreplication.StateRunning},
		{name: "the batch while running", desired: rebatched, current: ingest(), state: ydbreplication.StateRunning},
		{name: "another host while paused", desired: moved, current: remote, state: ydbreplication.StatePaused},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			c.Assert(ydbreplication.TransferChangeRefusal("ingest", test.desired, test.current, test.state), qt.Equals, (*ydbreplication.Refusal)(nil))
		})
	}
}

// A transfer's target, which YDB changes in no transfer, and a connection
// change of a running transfer are refused with the host's words. Ported
// from the host's TestTransferChangeRefusal_FailurePath.
func TestTransferChangeRefusal_FailurePath(t *testing.T) {
	retargeted := ingest()
	retargeted.Target = "other_log"
	remote := ingest()
	remote.Connection.ConnectionString = "grpc://primary:2136/?database=/prod"
	moved := remote
	moved.Connection.ConnectionString = "grpc://standby:2136/?database=/prod"
	for _, test := range []struct {
		name             string
		desired, current ydbreplication.TransferSpec
		state            string
		want             string
	}{
		{name: "the target", desired: retargeted, current: ingest(), state: ydbreplication.StatePaused,
			want: "its target differ from the database's, and YDB changes none of them in place (`CONSUMER is not " +
				"supported in ALTER`, and ALTER takes no FROM or TO); drop it with DROP TRANSFER, which loses the " +
				"position of a consumer YDB created for it, and plan again"},
		{name: "a connection while running", desired: moved, current: remote, state: ydbreplication.StateRunning,
			want: "its connection or credential differs, and YDB changes them only while the transfer is paused " +
				"(`Modifications are not allowed in StandBy state`), which it is not (it is running); pause it with " +
				"ALTER TRANSFER `ingest` SET (STATE = 'PAUSED'), apply, and resume it with SET (STATE = 'StandBy')"},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			got := ydbreplication.TransferChangeRefusal("ingest", test.desired, test.current, test.state)
			c.Assert(got, qt.Not(qt.Equals), (*ydbreplication.Refusal)(nil))
			c.Assert(got.Subject, qt.Equals, "transfer ingest")
			c.Assert(got.Reason, qt.Equals, test.want)
		})
	}
}
