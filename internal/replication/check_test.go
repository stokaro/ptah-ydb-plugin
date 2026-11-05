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
