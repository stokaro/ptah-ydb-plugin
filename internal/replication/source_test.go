package replication_test

import (
	"testing"

	qt "github.com/frankban/quicktest"
	"github.com/stokaro/ptah-ydb-plugin/internal/replication"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
)

// mirrorSpec is a replication of /remote/source read with a token secret.
func mirrorSpec() replication.ReplicationSpec {
	return replication.ReplicationSpec{Connection: replication.Connection{ConnectionString: "grpc://source:2136/?database=/remote", TokenSecretName: "token"},
		Items: []replication.Item{{Source: "/remote/source", Target: "archive/copy"}}}
}

// TestApplyReplicationSettings_HappyPath folds the SET of an ALTER into the
// declared replication as the host's YQL reader does: ENDPOINT changes the
// address of the connection string and keeps its database, a password
// credential replaces the token, and the items stay.
func TestApplyReplicationSettings_HappyPath(t *testing.T) {
	for _, test := range []struct {
		name     string
		settings map[string]string
		want     replication.Connection
	}{
		{name: "a new endpoint", settings: map[string]string{"endpoint": "moved:2136"},
			want: replication.Connection{ConnectionString: "grpc://moved:2136/?database=/remote", TokenSecretName: "token"}},
		{name: "a secure endpoint and another database", settings: map[string]string{"endpoint": "grpcs://moved:2135", "database": "/elsewhere"},
			want: replication.Connection{ConnectionString: "grpcs://moved:2135/?database=/elsewhere", TokenSecretName: "token"}},
		{name: "a password in place of the token", settings: map[string]string{"user": "reader", "password_secret_name": "credential"},
			want: replication.Connection{ConnectionString: "grpc://source:2136/?database=/remote", User: "reader", PasswordSecretName: "credential"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)

			changed, err := replication.ApplyReplicationSettings(mirrorSpec(), test.settings)

			c.Assert(err, qt.IsNil)
			c.Assert(changed.Connection, qt.DeepEquals, test.want)
			c.Assert(changed.Items, qt.DeepEquals, mirrorSpec().Items)
		})
	}
}

// TestApplyReplicationSettings_FailurePath refuses an ALTER that names
// nothing, a setting YDB changes in no replication in place, and
// CONNECTION_STRING beside ENDPOINT.
func TestApplyReplicationSettings_FailurePath(t *testing.T) {
	for _, test := range []struct {
		name     string
		settings map[string]string
		wantErr  string
	}{
		{name: "nothing", settings: map[string]string{}, wantErr: `ALTER needs a modeled setting`},
		{name: "a consistency level", settings: map[string]string{"consistency_level": "GLOBAL"}, wantErr: `setting "consistency_level" cannot change in a desired ALTER declaration`},
		{name: "a lifecycle command", settings: map[string]string{"state": "PAUSED"}, wantErr: `setting "state" cannot change in a desired ALTER declaration`},
		{name: "a connection string beside an endpoint", settings: map[string]string{"endpoint": "moved:2136", "connection_string": "grpc://other:2136/?database=/remote"},
			wantErr: `CONNECTION_STRING cannot be combined with ENDPOINT or DATABASE`},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)

			changed, err := replication.ApplyReplicationSettings(mirrorSpec(), test.settings)

			c.Assert(err, qt.ErrorMatches, test.wantErr)
			c.Assert(changed, qt.DeepEquals, replication.ReplicationSpec{})
		})
	}
}

// TestApplyTransferSettings folds a new lambda and batch settings into the
// declared transfer and refuses a change of its consumer, which YDB makes in
// no transfer in place.
func TestApplyTransferSettings(t *testing.T) {
	c := testcheck.New(t)
	declared := replication.TransferSpec{Source: "archive/events", Target: "archive/rows", Lambda: "($msg) -> { RETURN []; }"}

	changed, err := replication.ApplyTransferSettings(declared, map[string]string{"using": "($msg) -> { RETURN [<|id:1|>]; }", "batch_size_bytes": "4096"})
	refused, refusal := replication.ApplyTransferSettings(declared, map[string]string{"consumer": "reader"})

	c.Assert(err, qt.IsNil)
	c.Assert(changed, qt.DeepEquals, replication.TransferSpec{Source: "archive/events", Target: "archive/rows", Lambda: "($msg) -> { RETURN [<|id:1|>]; }", BatchSizeBytes: 4096})
	c.Assert(refusal, qt.ErrorMatches, `setting "consumer" cannot change in a desired ALTER declaration`)
	c.Assert(refused, qt.DeepEquals, replication.TransferSpec{})
}
