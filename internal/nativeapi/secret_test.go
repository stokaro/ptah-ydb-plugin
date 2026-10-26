package nativeapi_test

import (
	"context"
	"errors"
	"path"
	"testing"

	qt "github.com/frankban/quicktest"
	"github.com/stokaro/ptah-ydb-plugin/internal/nativeapi"
	native "github.com/stokaro/ptah-ydb-plugin/internal/nativeprotocol"
	"github.com/stokaro/ptah-ydb-plugin/schema"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/ydb-platform/ydb-go-genproto/Ydb_Scheme_V1"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb_Operations"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb_Scheme"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/anypb"
)

type secretScheme struct {
	Ydb_Scheme_V1.SchemeServiceClient
	parent, present, wrongType, changedAccess bool
	calls                                     int
}

func secretResult(value proto.Message) *Ydb_Operations.Operation {
	payload, _ := anypb.New(value)
	return &Ydb_Operations.Operation{Ready: true, Status: Ydb.StatusIds_SUCCESS, Result: payload}
}
func (s *secretScheme) ListDirectory(_ context.Context, request *Ydb_Scheme.ListDirectoryRequest, _ ...grpc.CallOption) (*Ydb_Scheme.ListDirectoryResponse, error) {
	listing := &Ydb_Scheme.ListDirectoryResult{Self: &Ydb_Scheme.Entry{Name: path.Base(request.Path), Type: Ydb_Scheme.Entry_DIRECTORY}}
	if request.Path == "/local" {
		listing.Self.Type = Ydb_Scheme.Entry_DATABASE
		if s.parent {
			listing.Children = []*Ydb_Scheme.Entry{{Name: "app", Type: Ydb_Scheme.Entry_DIRECTORY}}
		}
	} else if s.present {
		kind := Ydb_Scheme.Entry_SECRET
		if s.wrongType {
			kind = Ydb_Scheme.Entry_TABLE
		}
		listing.Children = []*Ydb_Scheme.Entry{{Name: "pg.pw", Type: kind}}
	}
	return &Ydb_Scheme.ListDirectoryResponse{Operation: secretResult(listing)}, nil
}
func (s *secretScheme) DescribePath(_ context.Context, _ *Ydb_Scheme.DescribePathRequest, _ ...grpc.CallOption) (*Ydb_Scheme.DescribePathResponse, error) {
	owner := "root"
	if s.changedAccess && s.calls > 0 {
		owner = "other"
	}
	return &Ydb_Scheme.DescribePathResponse{Operation: secretResult(&Ydb_Scheme.DescribePathResult{Self: &Ydb_Scheme.Entry{Name: "pg.pw", Type: Ydb_Scheme.Entry_SECRET, Owner: owner}})}, nil
}
func secretOperation(c *qt.C, before, after bool) *protocol.Envelope {
	c.Helper()
	owner := &protocol.Reference{Engine: "ydb", Kind: schema.SecretProperties, Database: "/local", Schema: "app", Name: "pg.pw"}
	var current, desired *protocol.Envelope
	var err error
	if before {
		current, err = schema.CaptureObject(owner, &native.SecretPresence{})
		c.Assert(err, qt.IsNil)
	}
	if after {
		desired, err = schema.CaptureSecretIntent(owner, &native.SecretIntent{ValueEnv: "PTAH_SECRET_PG"})
		c.Assert(err, qt.IsNil)
	}
	change, err := schema.CaptureSecretChange("secret", owner, current, desired)
	c.Assert(err, qt.IsNil)
	operation, err := schema.SecretOperationFromChange("secret", change.Value)
	c.Assert(err, qt.IsNil)
	return operation.Value
}
func TestSecretNativeLifecycle(t *testing.T) {
	for _, test := range []struct {
		name                  string
		before, after, parent bool
		statement             string
	}{
		{name: "create with new parent", after: true, statement: "CREATE SECRET `/local/app/pg.pw` WITH (value = $PTAH_SECRET_PG);"},
		{name: "rotate", before: true, after: true, parent: true, statement: "ALTER SECRET `/local/app/pg.pw` WITH (value = $PTAH_SECRET_PG);"},
		{name: "drop", before: true, parent: true, statement: "DROP SECRET `/local/app/pg.pw`;"},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := qt.New(t)
			scheme := &secretScheme{present: test.before, parent: test.parent}
			var statements []string
			runner := &nativeapi.Secret{Scheme: scheme, Execute: func(_ context.Context, query string) error {
				scheme.calls++
				scheme.parent = true
				scheme.present = test.after
				statements = append(statements, query)
				return nil
			}}
			dispatched, err := runner.Apply(c.Context(), "/local", secretOperation(c, test.before, test.after), func(context.Context) error { return nil })
			c.Assert(err, qt.IsNil)
			c.Assert(dispatched, qt.IsTrue)
			c.Assert(statements, qt.DeepEquals, []string{test.statement})
			c.Assert(scheme.calls, qt.Equals, 1)
		})
	}
}
func TestSecretNativeRefusalsAndUnknownOutcome(t *testing.T) {
	for _, test := range []struct {
		name            string
		scheme          secretScheme
		before          bool
		guard, dispatch error
		writes          bool
	}{
		{name: "occupied creation", scheme: secretScheme{parent: true, present: true}},
		{name: "missing rotation", before: true, scheme: secretScheme{parent: true}},
		{name: "wrong object kind", before: true, scheme: secretScheme{parent: true, present: true, wrongType: true}},
		{name: "lock lost", guard: errors.New("lost lock")},
		{name: "dispatch uncertain", dispatch: errors.New("uncertain"), writes: true},
		{name: "rotation access changed", before: true, scheme: secretScheme{parent: true, present: true, changedAccess: true}, writes: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := qt.New(t)
			scheme := test.scheme
			runner := &nativeapi.Secret{Scheme: &scheme, Execute: func(context.Context, string) error {
				scheme.calls++
				scheme.present = true
				scheme.parent = true
				return test.dispatch
			}}
			dispatched, err := runner.Apply(c.Context(), "/local", secretOperation(c, test.before, true), func(context.Context) error { return test.guard })
			c.Assert(err, qt.IsNotNil)
			c.Assert(dispatched, qt.Equals, test.writes)
			c.Assert(scheme.calls > 0, qt.Equals, test.writes)
		})
	}
}
