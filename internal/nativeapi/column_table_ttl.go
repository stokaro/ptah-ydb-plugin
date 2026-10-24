package nativeapi

import (
	"context"
	"errors"
	"fmt"
	"path"
	"slices"

	"github.com/stokaro/ptah-ydb-plugin/internal/columnstore"
	native "github.com/stokaro/ptah-ydb-plugin/internal/nativeprotocol"
	"github.com/stokaro/ptah-ydb-plugin/internal/tablecreate"
	"github.com/stokaro/ptah-ydb-plugin/schema"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	ydb "github.com/ydb-platform/ydb-go-sdk/v3"
	"google.golang.org/protobuf/proto"
)

// ColumnTableTTL owns the precondition read, the single dispatch and the
// confirmation of one step of an in-place change of a column table's TTL.
type ColumnTableTTL struct {
	// Open starts the table session the reads use.
	Open func(context.Context) (ColumnDescriber, error)
	// Execute runs one schema statement through the query service, once.
	Execute func(context.Context, string) error
	// Monitoring reads the monitoring description of the column table at an
	// absolute path; nil for a session whose address names no endpoint.
	Monitoring func(context.Context, string) ([]byte, error)
}

// NewColumnTableTTL binds the native table client of the session's driver, the
// statement runner and the monitoring reader.
func NewColumnTableTTL(driver *ydb.Driver, execute func(context.Context, string) error, monitoring func(context.Context, string) ([]byte, error)) *ColumnTableTTL {
	return &ColumnTableTTL{Execute: execute, Monitoring: monitoring,
		Open: func(ctx context.Context) (ColumnDescriber, error) { return OpenTable(ctx, driver) }}
}

// Apply reads the column table, requires it to hold the TTL the step was
// planned against, runs the step's ALTER TABLE once and reads the table back.
// A failure before dispatch leaves the table untouched. A failure after
// dispatch is uncertain and is never retried.
func (c *ColumnTableTTL) Apply(ctx context.Context, database string, value *protocol.Envelope, beforeWrite func(context.Context) error) (dispatched bool, err error) {
	alteration, err := tablecreate.ColumnTTLAlteration(value)
	if err != nil {
		return false, err
	}
	if c.Monitoring == nil {
		return false, ErrNoMonitoring
	}
	owner := proto.CloneOf(value.Owner)
	if owner.Database != "" && owner.Database != database {
		return false, fmt.Errorf("YDB column table TTL change targets another database")
	}
	owner.Database = database
	bound := proto.CloneOf(value)
	bound.Owner = owner
	if _, err := tablecreate.NormalizeColumnTTLAlteration(&protocol.ValueInput{Id: "execute", Representation: protocol.ValueInput_OPERATION, Value: bound}); err != nil {
		return false, err
	}
	session, err := c.Open(ctx)
	if err != nil {
		return false, err
	}
	defer func() { err = errors.Join(err, session.Close()) }()
	absolute := path.Join(database, owner.Schema, owner.Name)
	described, observed, err := readColumnTable(ctx, session, c.Monitoring, absolute)
	if err != nil {
		return false, err
	}
	if err := checkColumnTTLPrecondition(alteration, described, observed); err != nil {
		return false, err
	}
	if err := beforeWrite(ctx); err != nil {
		return false, err
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if err := c.Execute(ctx, tablecreate.ColumnTTLStatement(absolute, alteration)); err != nil {
		return true, err
	}
	_, observed, err = readColumnTable(ctx, session, c.Monitoring, absolute)
	if err != nil {
		return true, err
	}
	if !tablecreate.ColumnTTLEqual(alteration.Set, observed.Ttl) {
		return true, fmt.Errorf("YDB column table TTL differs from the request after the change")
	}
	return true, nil
}

// readColumnTable reads a column table from the table service and the
// monitoring endpoint, as inspection reads it.
func readColumnTable(ctx context.Context, session ColumnDescriber, monitoring func(context.Context, string) ([]byte, error), absolute string) (*native.ColumnTableDescribeResult, *native.ColumnStorageObservation, error) {
	described, err := session.DescribeColumnTable(ctx, absolute)
	if err != nil {
		return nil, nil, err
	}
	body, err := monitoring(ctx, absolute)
	if err != nil {
		return nil, nil, err
	}
	observed, err := columnstore.Decode(body, absolute, described)
	if err != nil {
		return nil, nil, err
	}
	return described, observed, nil
}

// checkColumnTTLPrecondition requires the table to hold exactly the TTL the
// step was planned against, so a TTL changed after planning turns into a
// refusal. A TTL to set must read a column the table holds, first in its key,
// of a type its mode reads; the server refuses the rest only after the call,
// which would leave the outcome uncertain.
func checkColumnTTLPrecondition(alteration *native.ColumnTableTTLAlteration, described *native.ColumnTableDescribeResult, observed *native.ColumnStorageObservation) error {
	if !tablecreate.ColumnTTLEqual(alteration.Held, observed.Ttl) {
		return fmt.Errorf("YDB column table TTL differs from the plan")
	}
	enabled := alteration.Set.GetEnabled()
	if enabled == nil {
		return nil
	}
	at := slices.IndexFunc(described.Columns, func(column *native.ColumnMeta) bool { return column.GetName() == enabled.Column })
	if at < 0 {
		return fmt.Errorf("YDB TTL column is absent")
	}
	if len(described.PrimaryKey) == 0 || described.PrimaryKey[0] != enabled.Column {
		return fmt.Errorf("YDB TTL column %q must be first in the primary key of the column table", enabled.Column)
	}
	kind, _, err := schema.ColumnType(described.Columns[at].Type)
	if err != nil {
		return err
	}
	if reason := tablecreate.TTLColumnRefusal(enabled.Column, kind, enabled.ColumnUnit != 0); reason != "" {
		return fmt.Errorf("YDB TTL %s", reason)
	}
	return nil
}
