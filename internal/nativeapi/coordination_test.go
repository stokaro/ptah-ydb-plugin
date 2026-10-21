package nativeapi_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	qt "github.com/frankban/quicktest"
	"github.com/stokaro/ptah-ydb-plugin/internal/nativeapi"
	"github.com/stokaro/ptah-ydb-plugin/schema"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/ydb-platform/ydb-go-genproto/Ydb_Coordination_V1"
	"github.com/ydb-platform/ydb-go-genproto/Ydb_Scheme_V1"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb_Coordination"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb_Operations"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb_Scheme"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/anypb"
)

// The fake native transport distinguishes a failed reply from a pre-dispatch
// refusal. A live server cannot deterministically lose only the mutation reply.
type coordinationTransport struct {
	state        *Ydb_Coordination.DescribeNodeResult
	writes       int
	failWrite    bool
	ignoreWrite  bool
	changeAccess bool
	foreign      bool
}

func (f *coordinationTransport) NewStream(context.Context, *grpc.StreamDesc, string, ...grpc.CallOption) (grpc.ClientStream, error) {
	return nil, errors.New("unexpected stream")
}
func result(value proto.Message) *Ydb_Operations.Operation {
	payload, err := anypb.New(value)
	if err != nil {
		panic(err)
	}
	return &Ydb_Operations.Operation{Ready: true, Status: Ydb.StatusIds_SUCCESS, Result: payload}
}
func (f *coordinationTransport) Invoke(_ context.Context, method string, args, reply any, _ ...grpc.CallOption) error {
	switch {
	case strings.HasSuffix(method, "/ListDirectory"):
		listing := &Ydb_Scheme.ListDirectoryResult{Self: &Ydb_Scheme.Entry{Name: "app", Type: Ydb_Scheme.Entry_DIRECTORY}}
		if f.state != nil {
			listing.Children = []*Ydb_Scheme.Entry{{Name: "node", Type: Ydb_Scheme.Entry_COORDINATION_NODE}}
		}
		if f.foreign {
			listing.Children = []*Ydb_Scheme.Entry{{Name: "node", Type: Ydb_Scheme.Entry_TABLE}}
		}
		reply.(*Ydb_Scheme.ListDirectoryResponse).Operation = result(listing)
	case strings.HasSuffix(method, "/DescribeNode"):
		if f.state == nil {
			return errors.New("missing node")
		}
		reply.(*Ydb_Coordination.DescribeNodeResponse).Operation = result(f.state)
	default:
		f.writes++
		if f.failWrite {
			return errors.New("secret credential detail must not escape")
		}
		successful := &Ydb_Operations.Operation{Ready: true, Status: Ydb.StatusIds_SUCCESS}
		switch request := args.(type) {
		case *Ydb_Coordination.CreateNodeRequest:
			if !f.ignoreWrite {
				f.state = &Ydb_Coordination.DescribeNodeResult{Config: proto.CloneOf(request.Config), Self: &Ydb_Scheme.Entry{Name: "node", Type: Ydb_Scheme.Entry_COORDINATION_NODE, Owner: "owner"}}
			}
			reply.(*Ydb_Coordination.CreateNodeResponse).Operation = successful
		case *Ydb_Coordination.AlterNodeRequest:
			if !f.ignoreWrite {
				f.state.Config = proto.CloneOf(request.Config)
			}
			if f.changeAccess {
				f.state.Self.Owner = "other"
			}
			reply.(*Ydb_Coordination.AlterNodeResponse).Operation = successful
		case *Ydb_Coordination.DropNodeRequest:
			if !f.ignoreWrite {
				f.state = nil
			}
			reply.(*Ydb_Coordination.DropNodeResponse).Operation = successful
		default:
			return errors.New("unexpected native method")
		}
	}
	return nil
}

func operation(c *qt.C, action string) (*protocol.Envelope, *coordinationTransport) {
	c.Helper()
	owner := &protocol.Reference{Engine: "ydb", Kind: schema.CoordinationProperties, Database: "/local", Schema: "app", Name: "node"}
	transport := &coordinationTransport{state: &Ydb_Coordination.DescribeNodeResult{Config: &Ydb_Coordination.Config{}, Self: &Ydb_Scheme.Entry{Name: "node", Type: Ydb_Scheme.Entry_COORDINATION_NODE, Owner: "owner"}}}
	before, err := schema.CaptureObject(owner, transport.state)
	c.Assert(err, qt.IsNil)
	after, err := schema.CaptureCoordinationIntent(owner, schema.CoordinationIntent{ReadConsistencyMode: new("strict")})
	c.Assert(err, qt.IsNil)
	switch action {
	case "create":
		before = nil
		transport.state = nil
	case "drop":
		after = nil
	}
	change, err := schema.CaptureCoordinationChange("change", owner, before, after)
	c.Assert(err, qt.IsNil)
	value, err := schema.CoordinationOperationFromChange("operation", change.Value)
	c.Assert(err, qt.IsNil)
	return value.Value, transport
}

func executor(transport *coordinationTransport) *nativeapi.Coordination {
	return &nativeapi.Coordination{Scheme: Ydb_Scheme_V1.NewSchemeServiceClient(transport), Nodes: Ydb_Coordination_V1.NewCoordinationServiceClient(transport)}
}
func allow(context.Context) error { return nil }

func TestCoordinationNativeDispatchAndConfirmation(t *testing.T) {
	for _, action := range []string{"create", "alter", "drop"} {
		t.Run(action, func(t *testing.T) {
			c := qt.New(t)
			value, transport := operation(c, action)
			dispatched, err := executor(transport).Apply(t.Context(), "/local", value, allow)
			c.Assert(err, qt.IsNil)
			c.Assert(dispatched, qt.IsTrue)
			c.Assert(transport.writes, qt.Equals, 1)
			// Repeating a completed operation cannot adopt an existing node, alter a
			// different before-state, or claim a missing node was just removed.
			dispatched, err = executor(transport).Apply(t.Context(), "/local", value, allow)
			c.Assert(err, qt.IsNotNil)
			c.Assert(dispatched, qt.IsFalse)
			c.Assert(transport.writes, qt.Equals, 1)
		})
	}
}

func TestCoordinationNativePreflightRefusesBeforeDispatch(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*protocol.Envelope, *coordinationTransport)
		guard  func(context.Context) error
	}{
		{name: "drift", mutate: func(_ *protocol.Envelope, f *coordinationTransport) { f.state.Self.Owner = "changed" }},
		{name: "foreign occupant", mutate: func(_ *protocol.Envelope, f *coordinationTransport) { f.foreign = true }},
		{name: "other database", mutate: func(v *protocol.Envelope, _ *coordinationTransport) { v.Owner.Database = "/other" }},
		{name: "lost lease", guard: func(context.Context) error { return errors.New("lease lost") }},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := qt.New(t)
			value, transport := operation(c, "alter")
			if test.mutate != nil {
				test.mutate(value, transport)
			}
			guard := test.guard
			if guard == nil {
				guard = allow
			}
			dispatched, err := executor(transport).Apply(t.Context(), "/local", value, guard)
			c.Assert(err, qt.IsNotNil)
			c.Assert(dispatched, qt.IsFalse)
			c.Assert(transport.writes, qt.Equals, 0)
		})
	}
}

func TestCoordinationNativeUnconfirmedWriteNeverRetries(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*coordinationTransport)
	}{
		{"transport failure", func(f *coordinationTransport) { f.failWrite = true }},
		{"configuration mismatch", func(f *coordinationTransport) { f.ignoreWrite = true }},
		{"access changed", func(f *coordinationTransport) { f.changeAccess = true }},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := qt.New(t)
			value, transport := operation(c, "alter")
			test.mutate(transport)
			dispatched, err := executor(transport).Apply(t.Context(), "/local", value, allow)
			c.Assert(err, qt.IsNotNil)
			c.Assert(err.Error(), qt.Not(qt.Contains), "secret credential")
			c.Assert(dispatched, qt.IsTrue)
			c.Assert(transport.writes, qt.Equals, 1)
		})
	}
}

func TestCoordinationCancellationBeforeWrite(t *testing.T) {
	c := qt.New(t)
	value, transport := operation(c, "create")
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	dispatched, err := executor(transport).Apply(ctx, "/local", value, func(context.Context) error { cancel(); return nil })
	c.Assert(err, qt.ErrorIs, context.Canceled)
	c.Assert(dispatched, qt.IsFalse)
	c.Assert(transport.writes, qt.Equals, 0)
}

func TestCoordinationDropRequiresConfirmedAbsence(t *testing.T) {
	c := qt.New(t)
	value, transport := operation(c, "drop")
	transport.ignoreWrite = true
	dispatched, err := executor(transport).Apply(t.Context(), "/local", value, allow)
	c.Assert(err, qt.IsNotNil)
	c.Assert(dispatched, qt.IsTrue)
	c.Assert(transport.writes, qt.Equals, 1)
}
