package nativeapi

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"path"

	native "github.com/stokaro/ptah-ydb-plugin/internal/nativeprotocol"
	"github.com/stokaro/ptah-ydb-plugin/internal/tablecreate"
	"github.com/stokaro/ptah-ydb-plugin/schema"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	ydb "github.com/ydb-platform/ydb-go-sdk/v3"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

// ColumnInventory checks captured state around one native column addition or drop.
type ColumnInventory struct {
	Open func(context.Context) (AlterationSession, error)
}

// NewColumnInventory binds one-shot native table alteration and observation.
func NewColumnInventory(driver *ydb.Driver) *ColumnInventory {
	return &ColumnInventory{Open: func(ctx context.Context) (AlterationSession, error) { return OpenTable(ctx, driver) }}
}

// Apply refuses stale or dependent columns before dispatch and confirms the full table.
func (c *ColumnInventory) Apply(ctx context.Context, database string, value *protocol.Envelope, beforeWrite func(context.Context) error) (dispatched bool, err error) {
	change, err := tablecreate.DecodeInventory(value)
	if err != nil {
		return false, err
	}
	owner := proto.CloneOf(value.Owner)
	if owner.Database != "" && owner.Database != database {
		return false, fmt.Errorf("YDB column inventory targets another database")
	}
	owner.Database = database
	bound := proto.CloneOf(value)
	bound.Owner = owner
	if _, err := tablecreate.NormalizeInventory(&protocol.ValueInput{Id: "execute", Representation: protocol.ValueInput_OPERATION, Value: bound}); err != nil {
		return false, err
	}
	session, err := c.Open(ctx)
	if err != nil {
		return false, err
	}
	defer func() { err = errors.Join(err, session.Close()) }()
	target := path.Join(database, owner.Schema, owner.Name)
	before, err := session.Describe(ctx, target)
	if err != nil {
		return false, err
	}
	alteration := &tablecreate.Alteration{Request: change.Request()}
	if err := checkAlterationPrecondition(owner, alteration, before); err != nil {
		return false, err
	}
	if change.Drop != nil {
		matches, err := change.MatchesDrop(columnByName(before.Columns, change.Drop.Name))
		if err != nil {
			return false, err
		}
		if !matches {
			return false, fmt.Errorf("YDB removed column differs from the captured definition")
		}
	}
	if err := beforeWrite(ctx); err != nil {
		return false, err
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	request := change.Request()
	request.Path = target
	if err := session.Alter(ctx, request); err != nil {
		return true, err
	}
	after, err := session.Describe(ctx, target)
	if err != nil {
		return true, err
	}
	if err := confirmTableAlteration(owner, alteration, before, after); err != nil {
		return true, err
	}
	return true, confirmInventoryState(owner, change, before, after)
}

func confirmInventoryState(owner *protocol.Reference, change *tablecreate.ColumnInventory, before, after *native.DescribeTableResult) error {
	for _, column := range after.Columns {
		expected := columnByName(before.Columns, column.Name)
		if expected == nil && change.Add != nil && column.Name == change.Add.Name {
			data, err := protojson.Marshal(change.Add)
			if err != nil {
				return err
			}
			expected = &native.ColumnMeta{}
			if err := protojson.Unmarshal(data, expected); err != nil {
				return err
			}
		}
		equal, err := tablecreate.EqualCapturedColumns(expected, column)
		if err != nil {
			return err
		}
		if !equal {
			return fmt.Errorf("YDB column settings changed outside the inventory request")
		}
	}
	snapshots := []*native.DescribeTableResult{proto.CloneOf(before), proto.CloneOf(after)}
	var payloads [][]byte
	for _, snapshot := range snapshots {
		snapshot.Indexes, snapshot.Changefeeds = nil, nil
		captured, err := schema.CaptureTable(owner, snapshot)
		if err != nil {
			return err
		}
		payloads = append(payloads, captured.Payload)
	}
	unchangedFeeds := proto.Equal(&native.DescribeTableResult{Changefeeds: before.Changefeeds}, &native.DescribeTableResult{Changefeeds: after.Changefeeds})
	if !bytes.Equal(payloads[0], payloads[1]) || !unchangedFeeds {
		return fmt.Errorf("YDB table settings changed outside the inventory request")
	}
	return nil
}
