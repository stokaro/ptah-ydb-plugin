package schema_test

import (
	"testing"

	qt "github.com/frankban/quicktest"
	"github.com/stokaro/ptah-ydb-plugin/schema"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb_Coordination"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb_Scheme"
	"google.golang.org/protobuf/proto"
)

func TestRuntimeCoordinationExportRetainsObservationAndRefusesChanges(t *testing.T) {
	c := qt.New(t)
	owner := &protocol.Reference{Engine: "ydb", Kind: schema.CoordinationProperties, Database: "/local", Name: "ptah_locks"}
	observed, err := schema.CaptureObject(owner, &Ydb_Coordination.DescribeNodeResult{
		Config: &Ydb_Coordination.Config{ReadConsistencyMode: Ydb_Coordination.ConsistencyMode_CONSISTENCY_MODE_STRICT, AttachConsistencyMode: Ydb_Coordination.ConsistencyMode_CONSISTENCY_MODE_STRICT},
		Self:   &Ydb_Scheme.Entry{Owner: "foreign-owner", Permissions: []*Ydb_Scheme.Permissions{{Subject: "writer", PermissionNames: []string{"write"}}}, EffectivePermissions: []*Ydb_Scheme.Permissions{{Subject: "writer", PermissionNames: []string{"write"}}}},
	})
	c.Assert(err, qt.IsNil)
	original := proto.CloneOf(observed)
	desired, err := schema.ConvertCoordination(observed, protocol.ValueInput_OBSERVED, protocol.ValueInput_DESIRED)
	c.Assert(err, qt.IsNil)
	back, err := schema.ConvertCoordination(desired, protocol.ValueInput_DESIRED, protocol.ValueInput_OBSERVED)
	c.Assert(err, qt.IsNil)
	c.Assert(proto.Equal(back, observed), qt.IsTrue)
	c.Assert(proto.Equal(observed, original), qt.IsTrue)
	for _, test := range []struct {
		name          string
		before, after *protocol.Envelope
	}{
		{"create", nil, desired}, {"alter", observed, desired}, {"drop", observed, nil},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := qt.New(t)
			change, err := schema.CaptureCoordinationChange("refused", owner, test.before, test.after)
			c.Assert(err, qt.ErrorMatches, `database-root ptah_locks is reserved for provider runtime use`)
			c.Assert(change, qt.IsNil)
		})
	}
	owner.Schema = "app"
	c.Assert(schema.ValidateManagedCoordinationOwner(owner), qt.IsNil)
}
