package replication_test

import (
	"testing"
	"time"

	qt "github.com/frankban/quicktest"
	"github.com/ydb-platform/ydb-go-genproto/draft/protos/Ydb_Replication"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb_Issue"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/types/known/durationpb"

	ydbreplication "github.com/stokaro/ptah-ydb-plugin/internal/replication"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
)

// connectionParams is a connection to the local database with no credential.
func connectionParams() *Ydb_Replication.ConnectionParams {
	return &Ydb_Replication.ConnectionParams{Endpoint: "localhost:2136", Database: "/local"}
}

func items(pairs ...string) []*Ydb_Replication.DescribeReplicationResult_Item {
	var result []*Ydb_Replication.DescribeReplicationResult_Item
	for i := 0; i < len(pairs); i += 2 {
		result = append(result, &Ydb_Replication.DescribeReplicationResult_Item{SourcePath: pairs[i], DestinationPath: pairs[i+1], Id: uint64(i)}) // #nosec G602 -- every caller passes source and destination paths in pairs
	}
	return result
}

// TestDecodeReplication_HappyPath reads a description as the host's reader
// does: the connection string rebuilt from its parts, a secret path relative
// to the root, items relative to their databases, the global commit interval
// only where it is not YDB's default, and the state.
func TestDecodeReplication_HappyPath(t *testing.T) {
	tests := []struct {
		name      string
		described *Ydb_Replication.DescribeReplicationResult
		root      string
		want      ydbreplication.ReplicationSpec
		state     string
	}{
		{
			name: "a running row replication with a token secret name",
			described: &Ydb_Replication.DescribeReplicationResult{
				ConnectionParams: &Ydb_Replication.ConnectionParams{Endpoint: "localhost:2136", Database: "/local",
					Credentials: &Ydb_Replication.ConnectionParams_Oauth{Oauth: &Ydb_Replication.ConnectionParams_OAuth{TokenSecretName: "token"}}},
				ConsistencyLevel: &Ydb_Replication.DescribeReplicationResult_RowConsistency{RowConsistency: &Ydb_Replication.ConsistencyLevelRow{}},
				Items:            items("/local/src/accounts", "/local/replica/accounts"),
				State:            &Ydb_Replication.DescribeReplicationResult_Running{Running: &Ydb_Replication.DescribeReplicationResult_RunningState{}},
			},
			root: "/local",
			want: ydbreplication.ReplicationSpec{Connection: ydbreplication.Connection{ConnectionString: "grpc://localhost:2136/?database=/local", TokenSecretName: "token"},
				Items: []ydbreplication.Item{{Source: "src/accounts", Target: "replica/accounts"}}},
			state: ydbreplication.StateRunning,
		},
		{
			name: "a paused global replication over TLS with a password secret path and a commit interval",
			described: &Ydb_Replication.DescribeReplicationResult{
				ConnectionParams: &Ydb_Replication.ConnectionParams{Endpoint: "primary:2135", Database: "/prod", EnableSsl: true,
					Credentials: &Ydb_Replication.ConnectionParams_StaticCredentials_{StaticCredentials: &Ydb_Replication.ConnectionParams_StaticCredentials{User: "replicator", PasswordSecretName: "/local/secrets/password"}}},
				ConsistencyLevel: &Ydb_Replication.DescribeReplicationResult_GlobalConsistency{GlobalConsistency: &Ydb_Replication.ConsistencyLevelGlobal{CommitInterval: durationpb.New(1500 * time.Millisecond)}},
				Items:            items("/prod/ledger", "/local/replica/ledger", "/other/x", "/local/replica/x"),
				State:            &Ydb_Replication.DescribeReplicationResult_Paused{Paused: &Ydb_Replication.DescribeReplicationResult_PausedState{}},
			},
			root: "/local",
			want: ydbreplication.ReplicationSpec{Connection: ydbreplication.Connection{ConnectionString: "grpcs://primary:2135/?database=/prod", User: "replicator", PasswordSecretPath: "secrets/password"},
				Items:            []ydbreplication.Item{{Source: "ledger", Target: "replica/ledger"}, {Source: "/other/x", Target: "replica/x"}},
				ConsistencyLevel: ydbreplication.ConsistencyGlobal, CommitInterval: "PT1.5S"},
			state: ydbreplication.StatePaused,
		},
		{
			name: "a failed replication with no item and the default global interval, read in a realm",
			described: &Ydb_Replication.DescribeReplicationResult{
				ConnectionParams: connectionParams(),
				ConsistencyLevel: &Ydb_Replication.DescribeReplicationResult_GlobalConsistency{GlobalConsistency: &Ydb_Replication.ConsistencyLevelGlobal{CommitInterval: durationpb.New(10 * time.Second)}},
				State:            &Ydb_Replication.DescribeReplicationResult_Error{Error: &Ydb_Replication.DescribeReplicationResult_ErrorState{Issues: []*Ydb_Issue.IssueMessage{{Message: "secret not found"}}}},
			},
			root:  "/local/.realms/r",
			want:  ydbreplication.ReplicationSpec{Connection: ydbreplication.Connection{ConnectionString: "grpc://localhost:2136/?database=/local"}, ConsistencyLevel: ydbreplication.ConsistencyGlobal},
			state: ydbreplication.StateError,
		},
		{
			// Ported from the host's TestModelValidate_ObservationsTheReaderWrites:
			// a secret path outside the database is read as written.
			name: "a password secret path outside the database",
			described: &Ydb_Replication.DescribeReplicationResult{
				ConnectionParams: &Ydb_Replication.ConnectionParams{Endpoint: "primary:2135", Database: "/prod",
					Credentials: &Ydb_Replication.ConnectionParams_StaticCredentials_{StaticCredentials: &Ydb_Replication.ConnectionParams_StaticCredentials{User: "replicator", PasswordSecretName: "/other/secrets/replicator"}}},
				Items: items("/prod/ledger", "/local/replica/ledger"),
			},
			root: "/local",
			want: ydbreplication.ReplicationSpec{Connection: ydbreplication.Connection{ConnectionString: "grpc://primary:2135/?database=/prod", User: "replicator",
				PasswordSecretPath: "/other/secrets/replicator"}, Items: []ydbreplication.Item{{Source: "ledger", Target: "replica/ledger"}}},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			got, state, err := ydbreplication.DecodeReplication(test.root, test.described)
			c.Assert(err, qt.IsNil)
			c.Assert(got, qt.DeepEquals, test.want)
			c.Assert(state, qt.Equals, test.state)
		})
	}
}

// TestDecodeReplication_FailurePath refuses what a declaration could not
// say: a field the pinned protocol does not know, and a commit interval
// finer than a millisecond.
func TestDecodeReplication_FailurePath(t *testing.T) {
	unknown := &Ydb_Replication.DescribeReplicationResult{ConnectionParams: connectionParams(), Items: items("/local/a", "/local/b")}
	unknown.ConnectionParams.ProtoReflect().SetUnknown(protowire.AppendVarint(protowire.AppendTag(nil, 99, protowire.VarintType), 1))
	tests := []struct {
		name      string
		described *Ydb_Replication.DescribeReplicationResult
		wantErr   string
	}{
		{name: "an unknown connection field", described: unknown, wantErr: `its connection holds fields 99, which the provider does not read`},
		{
			name: "a fraction of a millisecond",
			described: &Ydb_Replication.DescribeReplicationResult{ConnectionParams: connectionParams(),
				ConsistencyLevel: &Ydb_Replication.DescribeReplicationResult_GlobalConsistency{GlobalConsistency: &Ydb_Replication.ConsistencyLevelGlobal{CommitInterval: durationpb.New(1500 * time.Microsecond)}}},
			wantErr: `its commit interval holds a fraction of a millisecond, which no declaration reads`,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			got, state, err := ydbreplication.DecodeReplication("/local", test.described)
			c.Assert(err, qt.ErrorMatches, test.wantErr)
			c.Assert(got, qt.DeepEquals, ydbreplication.ReplicationSpec{})
			c.Assert(state, qt.Equals, "")
		})
	}
}

// TestDecodeTransfer_HappyPath reads a transfer: the lambda out of the
// statement YDB stores, a local topic relative to the root on every line's
// spelling, batch settings only where they differ from YDB's defaults, and
// a stored text in another form read whole.
func TestDecodeTransfer_HappyPath(t *testing.T) {
	size := uint64(1 << 20)
	defaultSize := uint64(8 << 20)
	tests := []struct {
		name      string
		described *Ydb_Replication.DescribeTransferResult
		want      ydbreplication.TransferSpec
		state     string
	}{
		{
			name: "a local topic written relative, the defaults",
			described: &Ydb_Replication.DescribeTransferResult{ConnectionParams: &Ydb_Replication.ConnectionParams{}, SourcePath: "orders/feed", DestinationPath: "/local/order_log",
				TransformationLambda: "$__ydb_transfer_lambda = ($m) -> { return []; };\n", ConsumerName: "f1b2",
				BatchSettings: &Ydb_Replication.DescribeTransferResult_BatchSettings{SizeBytes: &defaultSize, FlushInterval: durationpb.New(time.Minute)},
				State:         &Ydb_Replication.DescribeTransferResult_Running{Running: &Ydb_Replication.DescribeTransferResult_RunningState{}}},
			want:  ydbreplication.TransferSpec{Source: "orders/feed", Target: "order_log", Lambda: "($m) -> { return []; }", Consumer: "f1b2"},
			state: ydbreplication.StateRunning,
		},
		{
			name: "a local topic behind a slash, as 25.1 to 25.4 keep it, with batch settings",
			described: &Ydb_Replication.DescribeTransferResult{ConnectionParams: &Ydb_Replication.ConnectionParams{}, SourcePath: "/tp", DestinationPath: "/local/t",
				TransformationLambda: "$__ydb_transfer_lambda = ($m) -> { return []; };\n", ConsumerName: "c",
				BatchSettings: &Ydb_Replication.DescribeTransferResult_BatchSettings{SizeBytes: &size, FlushInterval: durationpb.New(10 * time.Second)},
				State:         &Ydb_Replication.DescribeTransferResult_Paused{Paused: &Ydb_Replication.DescribeTransferResult_PausedState{}}},
			want:  ydbreplication.TransferSpec{Source: "tp", Target: "t", Lambda: "($m) -> { return []; }", Consumer: "c", BatchSizeBytes: size, FlushInterval: "PT10S"},
			state: ydbreplication.StatePaused,
		},
		// Ported from the host's TestResolvePath_HappyPath: an absolute topic
		// path under the session root reads relative to it, in a directory
		// and at the root.
		{
			name: "a local topic written absolute in a directory",
			described: &Ydb_Replication.DescribeTransferResult{ConnectionParams: &Ydb_Replication.ConnectionParams{}, SourcePath: "/local/app/events", DestinationPath: "/local/t",
				TransformationLambda: "$__ydb_transfer_lambda = ($m) -> { return []; };\n", ConsumerName: "c"},
			want: ydbreplication.TransferSpec{Source: "app/events", Target: "t", Lambda: "($m) -> { return []; }", Consumer: "c"},
		},
		{
			name: "a local topic written absolute at the root with a dot",
			described: &Ydb_Replication.DescribeTransferResult{ConnectionParams: &Ydb_Replication.ConnectionParams{}, SourcePath: "/local/events.v1", DestinationPath: "/local/t",
				TransformationLambda: "$__ydb_transfer_lambda = ($m) -> { return []; };\n", ConsumerName: "c"},
			want: ydbreplication.TransferSpec{Source: "events.v1", Target: "t", Lambda: "($m) -> { return []; }", Consumer: "c"},
		},
		{
			name: "a topic of another database and a lambda under a pragma",
			described: &Ydb_Replication.DescribeTransferResult{ConnectionParams: &Ydb_Replication.ConnectionParams{Endpoint: "primary:2136", Database: "/prod",
				Credentials: &Ydb_Replication.ConnectionParams_Oauth{Oauth: &Ydb_Replication.ConnectionParams_OAuth{TokenSecretName: "token"}}},
				SourcePath: "/prod/events", DestinationPath: "/local/archive", TransformationLambda: "PRAGMA TablePathPrefix = '/local';\n$__ydb_transfer_lambda = ($m) -> { return []; };\n"},
			want: ydbreplication.TransferSpec{Connection: ydbreplication.Connection{ConnectionString: "grpc://primary:2136/?database=/prod", TokenSecretName: "token"},
				Source: "events", Target: "archive", Lambda: "PRAGMA TablePathPrefix = '/local';\n$__ydb_transfer_lambda = ($m) -> { return []; };\n"},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			got, state, err := ydbreplication.DecodeTransfer("/local", test.described)
			c.Assert(err, qt.IsNil)
			c.Assert(got, qt.DeepEquals, test.want)
			c.Assert(state, qt.Equals, test.state)
		})
	}
}

// TestDecodeTransfer_FailurePath refuses a flush interval finer than a second
// and a description with no table.
func TestDecodeTransfer_FailurePath(t *testing.T) {
	tests := []struct {
		name      string
		described *Ydb_Replication.DescribeTransferResult
		wantErr   string
	}{
		{
			name: "half a second",
			described: &Ydb_Replication.DescribeTransferResult{SourcePath: "tp", DestinationPath: "/local/t", TransformationLambda: "x",
				BatchSettings: &Ydb_Replication.DescribeTransferResult_BatchSettings{FlushInterval: durationpb.New(1500 * time.Millisecond)}},
			wantErr: `its flush interval holds a fraction of a second, which no declaration reads`,
		},
		{
			name:      "no table",
			described: &Ydb_Replication.DescribeTransferResult{SourcePath: "tp", TransformationLambda: "x"},
			wantErr:   `an observed transfer names its topic and its table`,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			got, state, err := ydbreplication.DecodeTransfer("/local", test.described)
			c.Assert(err, qt.ErrorMatches, test.wantErr)
			c.Assert(got, qt.DeepEquals, ydbreplication.TransferSpec{})
			c.Assert(state, qt.Equals, "")
		})
	}
}

// TestIsReplica reads the attribute YDB gives a replica table.
func TestIsReplica(t *testing.T) {
	tests := []struct {
		name       string
		attributes map[string]string
		want       bool
	}{
		{name: "a replica", attributes: map[string]string{ydbreplication.ReplicaAttribute: "true"}, want: true},
		{name: "an ordinary table", attributes: map[string]string{"comment": "x"}},
		{name: "no attributes"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			c.Assert(ydbreplication.IsReplica(test.attributes), qt.Equals, test.want)
		})
	}
}
