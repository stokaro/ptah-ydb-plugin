package nativeapi

import (
	"context"
	"errors"
	"fmt"
	"path"
	"slices"

	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	ydb "github.com/ydb-platform/ydb-go-sdk/v3"
	"google.golang.org/protobuf/proto"

	native "github.com/stokaro/ptah-ydb-plugin/internal/nativeprotocol"
	"github.com/stokaro/ptah-ydb-plugin/internal/tablecreate"
	"github.com/stokaro/ptah-ydb-plugin/schema"
)

// IndexDescriber reads a row table from the table service.
type IndexDescriber interface {
	Describe(context.Context, string) (*native.DescribeTableResult, error)
	Close() error
}

// IndexSettings owns the precondition read, the single dispatch of the YQL
// statement that changes one global index's partitioning and read replicas,
// and its confirmation. The native AlterTable call has no field for an
// index's settings, so the statement is YQL's ALTER TABLE ... ALTER INDEX, as
// the host's embedded engine writes it.
type IndexSettings struct {
	// Open starts the table session the precondition and the confirmation read with.
	Open func(context.Context) (IndexDescriber, error)
	// Execute runs one schema statement through the query service, once.
	Execute func(context.Context, string) error
}

// NewIndexSettings binds the native table client of the session's driver and
// the statement runner.
func NewIndexSettings(driver *ydb.Driver, execute func(context.Context, string) error) *IndexSettings {
	return &IndexSettings{Execute: execute, Open: func(ctx context.Context) (IndexDescriber, error) { return OpenTable(ctx, driver) }}
}

// Apply reads the table, requires it to hold the index exactly as the plan
// captured it, runs the statement once and reads the table back. A failure
// before dispatch leaves the table untouched. A failure after dispatch is
// uncertain and is never retried.
func (s *IndexSettings) Apply(ctx context.Context, database string, value *protocol.Envelope, beforeWrite func(context.Context) error) (dispatched bool, err error) {
	settings, err := tablecreate.DecodeIndexSettings(value)
	if err != nil {
		return false, err
	}
	owner := proto.CloneOf(value.Owner)
	if owner.Database != "" && owner.Database != database {
		return false, fmt.Errorf("YDB index settings change targets another database")
	}
	owner.Database = database
	bound := proto.CloneOf(value)
	bound.Owner = owner
	if _, err := tablecreate.NormalizeIndexSettings(&protocol.ValueInput{Id: "execute", Representation: protocol.ValueInput_OPERATION, Value: bound}); err != nil {
		return false, err
	}
	session, err := s.Open(ctx)
	if err != nil {
		return false, err
	}
	defer func() { err = errors.Join(err, session.Close()) }()
	target := path.Join(database, owner.Schema, owner.Name)
	before, err := session.Describe(ctx, target)
	if err != nil {
		return false, err
	}
	if err := holdsRowTable(owner, before); err != nil {
		return false, err
	}
	// The specification compared carries the index's settings, so settings
	// changed since planning refuse the statement too.
	if actual := indexByName(before.Indexes, settings.Index); actual == nil || !sameIndex(settings.Held, actual) {
		return false, fmt.Errorf("YDB index %q differs from the plan", settings.Index)
	}
	if err := beforeWrite(ctx); err != nil {
		return false, err
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if err := s.Execute(ctx, tablecreate.IndexSettingsStatement(target, settings)); err != nil {
		return true, err
	}
	observed, err := session.Describe(ctx, target)
	if err != nil {
		return true, err
	}
	return true, confirmIndexSettings(owner, settings, before, observed)
}

// confirmIndexSettings requires the index read back to keep its definition
// and be ready, to hold every partitioning setting the change names and the
// partition columns it held, and the read replicas it sets or else the ones it
// held. The server derives other partitioning settings from the ones named,
// so those are not compared; a change that names none leaves the partitioning
// as it was. Every other index, every column and the key stay as they were.
func confirmIndexSettings(owner *protocol.Reference, settings *tablecreate.IndexSettings, before, observed *native.DescribeTableResult) error {
	if err := holdsRowTable(owner, observed); err != nil {
		return err
	}
	if !slices.Equal(before.PrimaryKey, observed.PrimaryKey) || len(before.Columns) != len(observed.Columns) {
		return fmt.Errorf("YDB table key or columns changed with the index settings")
	}
	for i, column := range before.Columns {
		if !proto.Equal(column, observed.Columns[i]) {
			return fmt.Errorf("YDB table columns changed with the index settings")
		}
	}
	if len(observed.Indexes) != len(before.Indexes) {
		return fmt.Errorf("YDB altered table holds unexpected indexes")
	}
	for _, index := range before.Indexes {
		actual := indexByName(observed.Indexes, index.Name)
		if actual == nil {
			return fmt.Errorf("YDB altered table lost the index %q", index.Name)
		}
		if index.Name != settings.Index && !sameIndex(index, actual) {
			return fmt.Errorf("YDB altered table changed the index %q", index.Name)
		}
	}
	actual := indexByName(observed.Indexes, settings.Index)
	if actual.Status != native.TableIndexDescription_STATUS_READY || !sameDefinition(actual, settings.Held) {
		return fmt.Errorf("YDB index %q changed its definition with its settings", settings.Index)
	}
	held, got := tablecreate.HeldIndexSettings(settings.Held), tablecreate.HeldIndexSettings(actual)
	partitioning := proto.Equal(got.GetPartitioningSettings(), held.GetPartitioningSettings())
	if set := settings.Set.GetPartitioningSettings(); set != nil {
		partitioning = tablecreate.PartitioningHolds(got.GetPartitioningSettings(), set) &&
			slices.Equal(got.GetPartitioningSettings().GetPartitionBy(), held.GetPartitioningSettings().GetPartitionBy())
	}
	if !partitioning {
		return fmt.Errorf("YDB index %q partitioning settings differ from the request", settings.Index)
	}
	replicas := held.GetReadReplicasSettings()
	if set := settings.Set.GetReadReplicasSettings(); set != nil {
		replicas = set
	}
	if !tablecreate.SameReplicas(got.GetReadReplicasSettings(), replicas) {
		return fmt.Errorf("YDB index %q read replicas differ from the request", settings.Index)
	}
	return nil
}

// sameDefinition reports whether two descriptions state one index apart from
// its settings: the name, the ordered key and covered columns, and the kind.
func sameDefinition(a, b *native.TableIndexDescription) bool {
	return proto.Equal(withoutSettings(a), withoutSettings(b))
}

// withoutSettings returns the specification of a global sync, async or unique
// index with its settings left out.
func withoutSettings(index *native.TableIndexDescription) *native.IndexIntent {
	spec := schema.IndexSpecification(index)
	switch kind := spec.Type.(type) {
	case *native.IndexIntent_GlobalIndex:
		kind.GlobalIndex = &native.GlobalIndex{}
	case *native.IndexIntent_GlobalAsyncIndex:
		kind.GlobalAsyncIndex = &native.GlobalAsyncIndex{}
	case *native.IndexIntent_GlobalUniqueIndex:
		kind.GlobalUniqueIndex = &native.GlobalUniqueIndex{}
	}
	return spec
}
