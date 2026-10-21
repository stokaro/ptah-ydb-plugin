package nativeapi

import (
	"bytes"
	"context"
	"fmt"
	"path"

	"github.com/stokaro/ptah-ydb-plugin/schema"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/ydb-platform/ydb-go-genproto/Ydb_Coordination_V1"
	"github.com/ydb-platform/ydb-go-genproto/Ydb_Scheme_V1"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb_Coordination"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb_Scheme"
	ydb "github.com/ydb-platform/ydb-go-sdk/v3"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

// Coordination owns raw native calls without mutation retry wrappers.
// The injectable clients also let tests distinguish preflight from dispatch.
type Coordination struct {
	Scheme Ydb_Scheme_V1.SchemeServiceClient
	Nodes  Ydb_Coordination_V1.CoordinationServiceClient
}

// NewCoordination binds native calls to the session's authenticated driver.
func NewCoordination(driver *ydb.Driver) *Coordination {
	conn := ydb.GRPCConn(driver)
	return &Coordination{Scheme: Ydb_Scheme_V1.NewSchemeServiceClient(conn), Nodes: Ydb_Coordination_V1.NewCoordinationServiceClient(conn)}
}

// Apply verifies captured state, dispatches once, and reads back the outcome.
// Dispatched is true as soon as a write RPC is attempted, including transport
// failure or cancellation. Native YDB has no atomic schema compare-and-set.
func (c *Coordination) Apply(ctx context.Context, database string, value *protocol.Envelope, beforeWrite func(context.Context) error) (dispatched bool, err error) {
	normalized, err := schema.NormalizeCoordinationOperation(&protocol.ValueInput{Id: "execute", Representation: protocol.ValueInput_OPERATION, Value: value})
	if err != nil {
		return false, err
	}
	owner := proto.CloneOf(normalized.NormalizedOwner)
	if owner.Database != "" && owner.Database != database {
		return false, fmt.Errorf("coordination operation targets another database")
	}
	owner.Database = database
	if err := schema.ValidateCoordinationOwner(owner); err != nil {
		return false, err
	}
	before, after, err := schema.CoordinationOperands(schema.CoordinationOperationChange(normalized.Value))
	if err != nil {
		return false, err
	}
	current, err := c.read(ctx, owner)
	if err != nil {
		return false, err
	}
	if (before == nil) != (current == nil) || (before != nil && !bytes.Equal(before.Payload, current.Payload)) {
		return false, fmt.Errorf("coordination state changed since capture")
	}
	var config *Ydb_Coordination.Config
	if after != nil {
		intent, err := schema.CoordinationConfiguration(after, protocol.ValueInput_DESIRED)
		if err != nil {
			return false, err
		}
		config, err = schema.CoordinationNativeConfig(intent)
		if err != nil {
			return false, err
		}
	}
	if err := beforeWrite(ctx); err != nil {
		return false, err
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	absolute := path.Join(database, owner.Schema, owner.Name)
	switch {
	case before == nil:
		response, callErr := c.Nodes.CreateNode(ctx, &Ydb_Coordination.CreateNodeRequest{Path: absolute, Config: config})
		if callErr != nil {
			return true, fmt.Errorf("create YDB coordination node: transport failed")
		}
		err = Status(response.GetOperation())
	case after == nil:
		response, callErr := c.Nodes.DropNode(ctx, &Ydb_Coordination.DropNodeRequest{Path: absolute})
		if callErr != nil {
			return true, fmt.Errorf("drop YDB coordination node: transport failed")
		}
		err = Status(response.GetOperation())
	default:
		response, callErr := c.Nodes.AlterNode(ctx, &Ydb_Coordination.AlterNodeRequest{Path: absolute, Config: config})
		if callErr != nil {
			return true, fmt.Errorf("alter YDB coordination node: transport failed")
		}
		err = Status(response.GetOperation())
	}
	if err != nil {
		return true, err
	}
	observed, err := c.read(ctx, owner)
	if err != nil {
		return true, err
	}
	if err := confirmCoordination(before, after, observed); err != nil {
		return true, err
	}
	return true, nil
}

func (c *Coordination) read(ctx context.Context, owner *protocol.Reference) (*protocol.Envelope, error) {
	directory := path.Join(owner.Database, owner.Schema)
	response, err := c.Scheme.ListDirectory(ctx, &Ydb_Scheme.ListDirectoryRequest{Path: directory})
	if err != nil {
		return nil, fmt.Errorf("read YDB coordination namespace: transport failed")
	}
	var listing Ydb_Scheme.ListDirectoryResult
	if err := Operation(response.GetOperation(), &listing); err != nil {
		return nil, err
	}
	if listing.GetSelf().GetName() != path.Base(directory) || (listing.GetSelf().GetType() != Ydb_Scheme.Entry_DATABASE && listing.GetSelf().GetType() != Ydb_Scheme.Entry_DIRECTORY) || len(listing.Children) > 4096 {
		return nil, fmt.Errorf("invalid YDB coordination namespace")
	}
	found := false
	for _, entry := range listing.Children {
		if entry.GetName() != owner.Name {
			continue
		}
		if found || entry.GetType() != Ydb_Scheme.Entry_COORDINATION_NODE {
			return nil, fmt.Errorf("coordination path has a different or ambiguous occupant")
		}
		found = true
	}
	if !found {
		return nil, nil
	}
	absolute := path.Join(directory, owner.Name)
	described, err := c.Nodes.DescribeNode(ctx, &Ydb_Coordination.DescribeNodeRequest{Path: absolute})
	if err != nil {
		return nil, fmt.Errorf("describe YDB coordination node: transport failed")
	}
	var result Ydb_Coordination.DescribeNodeResult
	if err := Operation(described.GetOperation(), &result); err != nil {
		return nil, err
	}
	if result.GetSelf().GetName() != owner.Name || result.GetSelf().GetType() != Ydb_Scheme.Entry_COORDINATION_NODE || (result.GetConfig().GetPath() != "" && result.GetConfig().GetPath() != absolute) {
		return nil, fmt.Errorf("YDB coordination observation has a different identity")
	}
	return schema.CaptureObject(owner, &result)
}

func confirmCoordination(before, after, current *protocol.Envelope) error {
	if after == nil {
		if current != nil {
			return fmt.Errorf("coordination removal was not confirmed")
		}
		return nil
	}
	if current == nil {
		return fmt.Errorf("coordination result is absent")
	}
	if err := schema.ValidateCoordinationAccess(current, after); err != nil {
		return err
	}
	actual, err := schema.CoordinationConfiguration(current, protocol.ValueInput_OBSERVED)
	if err != nil {
		return err
	}
	wanted, err := schema.CoordinationConfiguration(after, protocol.ValueInput_DESIRED)
	if err != nil {
		return err
	}
	a, err := schema.EffectiveCoordination(actual)
	if err != nil {
		return err
	}
	b, err := schema.EffectiveCoordination(wanted)
	if err != nil {
		return err
	}
	if a != b {
		return fmt.Errorf("coordination result differs from the requested configuration")
	}
	if before != nil {
		var old, observed Ydb_Coordination.DescribeNodeResult
		if err := protojson.Unmarshal(before.Payload, &old); err != nil {
			return err
		}
		if err := protojson.Unmarshal(current.Payload, &observed); err != nil {
			return err
		}
		if !proto.Equal(old.Self, observed.Self) {
			return fmt.Errorf("coordination access metadata changed during alteration")
		}
	}
	return nil
}
