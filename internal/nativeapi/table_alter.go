package nativeapi

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"path"
	"slices"
	"time"

	native "github.com/stokaro/ptah-ydb-plugin/internal/nativeprotocol"
	"github.com/stokaro/ptah-ydb-plugin/internal/tablecreate"
	"github.com/stokaro/ptah-ydb-plugin/schema"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb_Scheme"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb_Table"
	ydb "github.com/ydb-platform/ydb-go-sdk/v3"
	"google.golang.org/protobuf/proto"
)

// AlterationSession exposes one native table session without a retry wrapper.
type AlterationSession interface {
	Alter(context.Context, *Ydb_Table.AlterTableRequest) error
	Describe(context.Context, string) (*native.DescribeTableResult, error)
	Close() error
}

// TableAlteration owns the precondition read, single dispatch, and confirmation
// of a captured table alteration: column families and their placements, the
// addition of nullable columns, the removal of columns, the TTL, the automatic
// partitioning settings, or one global secondary index.
type TableAlteration struct {
	Open func(context.Context) (AlterationSession, error)
	// IndexPoll is how long Apply waits between reads while a new index is
	// being built. Zero means one second.
	IndexPoll time.Duration
	// IndexTimeout bounds the build of a new index: the call that adds it, and
	// the wait afterward for it to read back as ready. Zero means ten minutes. An
	// index not ready by then leaves the outcome uncertain.
	IndexTimeout time.Duration
}

// NewTableAlteration binds the native table client to the authenticated session driver.
func NewTableAlteration(driver *ydb.Driver) *TableAlteration {
	return &TableAlteration{Open: func(ctx context.Context) (AlterationSession, error) { return OpenTable(ctx, driver) }}
}

// Apply reads the table, requires it to hold exactly what the plan captured,
// alters it once and reads it back. A failure before dispatch leaves the table
// untouched. A failure after dispatch is uncertain and is never retried.
func (a *TableAlteration) Apply(ctx context.Context, database string, value *protocol.Envelope, beforeWrite func(context.Context) error) (dispatched bool, err error) {
	alteration, err := tablecreate.DecodeAlteration(value)
	if err != nil {
		return false, err
	}
	owner := proto.CloneOf(value.Owner)
	if owner.Database != "" && owner.Database != database {
		return false, fmt.Errorf("YDB table alteration targets another database")
	}
	owner.Database = database
	// Re-normalize the bound identity before path.Join can erase path components.
	bound := proto.CloneOf(value)
	bound.Owner = owner
	if _, err := tablecreate.NormalizeAlteration(&protocol.ValueInput{Id: "execute", Representation: protocol.ValueInput_OPERATION, Value: bound}); err != nil {
		return false, err
	}
	session, err := a.Open(ctx)
	if err != nil {
		return false, err
	}
	defer func() { err = errors.Join(err, session.Close()) }()
	target := path.Join(database, owner.Schema, owner.Name)
	before, err := session.Describe(ctx, target)
	if err != nil {
		return false, err
	}
	if err := checkAlterationPrecondition(owner, alteration, before); err != nil {
		return false, err
	}
	if err := beforeWrite(ctx); err != nil {
		return false, err
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	request := proto.CloneOf(alteration.Request)
	request.Path = target
	if err := a.alter(ctx, session, request); err != nil {
		return true, err
	}
	observed, err := session.Describe(ctx, target)
	if err != nil {
		return true, err
	}
	if observed, err = a.awaitIndex(ctx, session, target, alteration.Request, observed); err != nil {
		return true, err
	}
	return true, confirmTableAlteration(owner, alteration, before, observed)
}

// alter dispatches the request. An index addition is bounded by the index
// timeout, since the call returns only when the server has built the index.
func (a *TableAlteration) alter(ctx context.Context, session AlterationSession, request *Ydb_Table.AlterTableRequest) error {
	if len(request.AddIndexes) == 0 {
		return session.Alter(ctx, request)
	}
	ctx, cancel := context.WithTimeout(ctx, cmp.Or(a.IndexTimeout, 10*time.Minute))
	defer cancel()
	return session.Alter(ctx, request)
}

// awaitIndex reads the table again while an index the request added is still
// being built. YDB builds an index in the background, and an index that is not
// ready cannot be confirmed. The wait ends when the index is ready, when it is
// gone, when the timeout passes or when the context ends; the last reading is
// then confirmed as it stands.
func (a *TableAlteration) awaitIndex(ctx context.Context, session AlterationSession, target string, request *Ydb_Table.AlterTableRequest, observed *native.DescribeTableResult) (*native.DescribeTableResult, error) {
	if len(request.AddIndexes) == 0 {
		return observed, nil
	}
	poll, timeout := cmp.Or(a.IndexPoll, time.Second), cmp.Or(a.IndexTimeout, 10*time.Minute)
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	for {
		index := indexByName(observed.Indexes, request.AddIndexes[0].Name)
		if index == nil || index.Status == native.TableIndexDescription_STATUS_READY {
			return observed, nil
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-deadline.C:
			return observed, nil
		case <-time.After(poll):
		}
		var err error
		if observed, err = session.Describe(ctx, target); err != nil {
			return nil, err
		}
	}
}

// familyName spells a family the way the server and the plan agree on: the
// default family has an empty name on a column and "default" in a family list.
func familyName(name string) string {
	if name == "" {
		return tablecreate.DefaultFamily
	}
	return name
}

func holdsRowTable(owner *protocol.Reference, table *native.DescribeTableResult) error {
	if err := schema.Known(table); err != nil {
		return err
	}
	if table.GetSelf().GetName() != owner.Name || table.GetSelf().GetType() != Ydb_Scheme.Entry_TABLE ||
		(table.StoreType != Ydb_Table.StoreType_STORE_TYPE_ROW && table.StoreType != Ydb_Table.StoreType_STORE_TYPE_UNSPECIFIED) {
		return fmt.Errorf("YDB table identity differs from the alteration")
	}
	return nil
}

func familyByName(families []*Ydb_Table.ColumnFamily, name string) *Ydb_Table.ColumnFamily {
	for _, family := range families {
		if familyName(family.Name) == familyName(name) {
			return family
		}
	}
	return nil
}

func columnByName(columns []*native.ColumnMeta, name string) *native.ColumnMeta {
	for _, column := range columns {
		if column.Name == name {
			return column
		}
	}
	return nil
}

// checkAlterationPrecondition requires the table to hold every touched family
// and every moved column as the plan captured them, so a change made after
// planning turns into a refusal instead of an unreviewed alteration.
func checkAlterationPrecondition(owner *protocol.Reference, alteration *tablecreate.Alteration, table *native.DescribeTableResult) error {
	if err := holdsRowTable(owner, table); err != nil {
		return err
	}
	request := alteration.Request
	for _, family := range request.AddColumnFamilies {
		if familyByName(table.ColumnFamilies, family.Name) != nil {
			return fmt.Errorf("YDB column family to add already exists")
		}
	}
	for _, held := range alteration.HeldFamilies {
		if actual := familyByName(table.ColumnFamilies, held.Name); actual == nil || !proto.Equal(held, actual) {
			return fmt.Errorf("YDB column family differs from the plan")
		}
	}
	for _, column := range request.AddColumns {
		if columnByName(table.Columns, column.Name) != nil {
			return fmt.Errorf("YDB column to add already exists")
		}
	}
	for _, name := range request.DropColumns {
		if columnByName(table.Columns, name) == nil {
			return fmt.Errorf("YDB column to drop is absent")
		}
		if err := dropBlocker(table, name); err != nil {
			return err
		}
	}
	for _, move := range request.AlterColumns {
		column := columnByName(table.Columns, move.Name)
		if column == nil || familyName(column.Family) != familyName(alteration.HeldColumns[move.Name]) {
			return fmt.Errorf("YDB column differs from the plan")
		}
		inRequest := slices.ContainsFunc(request.AddColumnFamilies, func(f *Ydb_Table.ColumnFamily) bool { return f.Name == move.Family })
		if !inRequest && familyByName(table.ColumnFamilies, move.Family) == nil && familyName(move.Family) != tablecreate.DefaultFamily {
			return fmt.Errorf("YDB column moves into a family the table does not hold")
		}
	}
	if err := checkTTLPrecondition(alteration, table); err != nil {
		return err
	}
	if err := checkIndexPrecondition(alteration, table); err != nil {
		return err
	}
	return checkPartitioningPrecondition(alteration, table)
}

func indexByName(indexes []*native.TableIndexDescription, name string) *native.TableIndexDescription {
	for _, index := range indexes {
		if index.Name == name {
			return index
		}
	}
	return nil
}

// sameIndex reports whether two descriptions state one definition: the name,
// the ordered key and covered columns, and the kind with its settings. Status
// and size are runtime state.
func sameIndex(a, b *native.TableIndexDescription) bool {
	return proto.Equal(schema.IndexSpecification(a), schema.IndexSpecification(b))
}

// checkIndexPrecondition requires an index to add to be absent and to read
// columns the table holds, and an index to drop to exist as the plan captured
// it, so an index changed after planning turns into a refusal. The server
// refuses a name that is taken or a column that is absent only after the call,
// which would leave the outcome uncertain.
func checkIndexPrecondition(alteration *tablecreate.Alteration, table *native.DescribeTableResult) error {
	request := alteration.Request
	for _, index := range request.AddIndexes {
		if indexByName(table.Indexes, index.Name) != nil {
			return fmt.Errorf("YDB index to add already exists")
		}
		for _, name := range slices.Concat(index.IndexColumns, index.DataColumns) {
			if columnByName(table.Columns, name) == nil {
				return fmt.Errorf("YDB index to add reads the absent column %q", name)
			}
		}
	}
	for _, name := range request.DropIndexes {
		actual := indexByName(table.Indexes, name)
		if actual == nil {
			return fmt.Errorf("YDB index to drop is absent")
		}
		if !sameIndex(alteration.HeldIndexes[0], actual) {
			return fmt.Errorf("YDB index differs from the plan")
		}
	}
	return nil
}

// checkPartitioningPrecondition requires the table to hold exactly the
// partitioning settings the plan captured, so settings changed after planning
// turn into a refusal.
func checkPartitioningPrecondition(alteration *tablecreate.Alteration, table *native.DescribeTableResult) error {
	if alteration.Request.AlterPartitioningSettings == nil {
		return nil
	}
	held := table.PartitioningSettings
	if held == nil {
		held = &Ydb_Table.PartitioningSettings{}
	}
	if !proto.Equal(held, alteration.HeldPartitioning) {
		return fmt.Errorf("YDB table partitioning settings differ from the plan")
	}
	return nil
}

// checkTTLPrecondition requires the table to hold exactly the TTL the plan
// captured, or none, so a TTL changed after planning turns into a refusal. A
// TTL to set must read a column the table holds, of a type its mode reads; the
// server refuses the rest only after the call, which would leave the outcome
// uncertain.
func checkTTLPrecondition(alteration *tablecreate.Alteration, table *native.DescribeTableResult) error {
	if alteration.Request.TtlAction == nil {
		return nil
	}
	if !proto.Equal(table.TtlSettings, alteration.HeldTTL) {
		return fmt.Errorf("YDB table TTL differs from the plan")
	}
	set := alteration.Request.GetSetTtlSettings()
	if set == nil {
		return nil
	}
	name := set.GetDateTypeColumn().GetColumnName()
	if name == "" {
		name = set.GetValueSinceUnixEpoch().GetColumnName()
	}
	column := columnByName(table.Columns, name)
	if column == nil {
		return fmt.Errorf("YDB TTL column is absent")
	}
	kind, _, err := schema.ColumnType(column.Type)
	if err != nil {
		return err
	}
	if reason := tablecreate.TTLColumnRefusal(name, kind, set.GetValueSinceUnixEpoch() != nil); reason != "" {
		return fmt.Errorf("YDB TTL %s", reason)
	}
	return nil
}

// dropBlocker names what keeps the table from dropping a column. The server
// refuses each case, but only after dispatch, which would leave the outcome
// uncertain; reading it first turns the refusal into one before the call.
func dropBlocker(table *native.DescribeTableResult, name string) error {
	if slices.Contains(table.PrimaryKey, name) {
		return fmt.Errorf("YDB column to drop is part of the primary key")
	}
	for _, index := range table.Indexes {
		if slices.Contains(index.IndexColumns, name) || slices.Contains(index.DataColumns, name) {
			return fmt.Errorf("YDB column to drop is used by index %q", index.Name)
		}
	}
	ttl := table.TtlSettings.GetDateTypeColumn().GetColumnName()
	if ttl == "" {
		ttl = table.TtlSettings.GetValueSinceUnixEpoch().GetColumnName()
	}
	if ttl == name {
		return fmt.Errorf("YDB column to drop is the TTL column")
	}
	if slices.Contains(table.PartitioningSettings.GetPartitionBy(), name) {
		return fmt.Errorf("YDB column to drop is a partitioning column")
	}
	return nil
}

// compression reads an unspecified value as the uncompressed family it stands for.
func compression(value Ydb_Table.ColumnFamily_Compression) Ydb_Table.ColumnFamily_Compression {
	if value == Ydb_Table.ColumnFamily_COMPRESSION_UNSPECIFIED {
		return Ydb_Table.ColumnFamily_COMPRESSION_NONE
	}
	return value
}

// confirmTableAlteration compares the table read back with the table read
// before the call and the request. Everything the request names holds its new
// value, and everything else is unchanged: other families, the family of every
// other column, column names and types, and the key.
func confirmTableAlteration(owner *protocol.Reference, alteration *tablecreate.Alteration, before, observed *native.DescribeTableResult) error {
	if err := holdsRowTable(owner, observed); err != nil {
		return err
	}
	request := alteration.Request
	if !slices.Equal(before.PrimaryKey, observed.PrimaryKey) || len(before.Columns)-len(request.DropColumns)+len(request.AddColumns) != len(observed.Columns) {
		return fmt.Errorf("YDB altered table key or columns differ from the request")
	}
	for _, family := range before.ColumnFamilies {
		changed := familyByName(request.AlterColumnFamilies, family.Name)
		actual := familyByName(observed.ColumnFamilies, family.Name)
		want := proto.CloneOf(family)
		if changed != nil {
			if changed.GetData() != nil {
				want.Data = changed.Data
			}
			if changed.Compression != Ydb_Table.ColumnFamily_COMPRESSION_UNSPECIFIED {
				want.Compression = changed.Compression
			}
		}
		if actual == nil || actual.GetData().GetMedia() != want.GetData().GetMedia() ||
			compression(actual.Compression) != compression(want.Compression) || actual.KeepInMemory != want.KeepInMemory {
			return fmt.Errorf("YDB altered column family differs from the request")
		}
	}
	for _, family := range request.AddColumnFamilies {
		actual := familyByName(observed.ColumnFamilies, family.Name)
		if actual == nil || (family.GetData().GetMedia() != "" && actual.GetData().GetMedia() != family.Data.Media) ||
			(family.Compression != Ydb_Table.ColumnFamily_COMPRESSION_UNSPECIFIED && compression(actual.Compression) != compression(family.Compression)) {
			return fmt.Errorf("YDB added column family differs from the request")
		}
	}
	if len(observed.ColumnFamilies) != len(before.ColumnFamilies)+len(request.AddColumnFamilies) {
		return fmt.Errorf("YDB altered table holds unexpected column families")
	}
	// The columns the request leaves in place keep their order, type and family
	// (or the one the request moves them to); the added ones follow.
	var kept []*native.ColumnMeta
	for _, column := range before.Columns {
		if !slices.Contains(request.DropColumns, column.Name) {
			kept = append(kept, column)
		}
	}
	for i, column := range kept {
		actual := observed.Columns[i]
		want := familyName(column.Family)
		if index := slices.IndexFunc(request.AlterColumns, func(move *Ydb_Table.ColumnMeta) bool { return move.Name == column.Name }); index >= 0 {
			want = familyName(request.AlterColumns[index].Family)
		}
		if actual.Name != column.Name || !proto.Equal(actual.Type, column.Type) || actual.GetNotNull() != column.GetNotNull() || familyName(actual.Family) != want {
			return fmt.Errorf("YDB altered column differs from the request")
		}
	}
	for _, added := range request.AddColumns {
		actual := columnByName(observed.Columns[len(kept):], added.Name)
		if actual == nil || !proto.Equal(actual.Type, added.Type) || actual.GetNotNull() || familyName(actual.Family) != tablecreate.DefaultFamily {
			return fmt.Errorf("YDB added column differs from the request")
		}
	}
	if err := confirmTTL(request, before, observed); err != nil {
		return err
	}
	if err := confirmIndexes(request, before, observed); err != nil {
		return err
	}
	return confirmPartitioning(request, before, observed)
}

// confirmIndexes requires an added index to exist, be ready and hold the
// definition the request states, a dropped index to be gone, and every other
// index to be as it was before the call.
func confirmIndexes(request *Ydb_Table.AlterTableRequest, before, observed *native.DescribeTableResult) error {
	named := map[string]bool{}
	for _, index := range request.AddIndexes {
		named[index.Name] = true
		actual := indexByName(observed.Indexes, index.Name)
		if actual == nil || actual.Status != native.TableIndexDescription_STATUS_READY || !slices.Equal(actual.IndexColumns, index.IndexColumns) ||
			!slices.Equal(actual.DataColumns, index.DataColumns) || !sameIndexKind(actual, index) {
			return fmt.Errorf("YDB added index differs from the request")
		}
	}
	for _, name := range request.DropIndexes {
		named[name] = true
		if indexByName(observed.Indexes, name) != nil {
			return fmt.Errorf("YDB dropped index is still present")
		}
	}
	for _, index := range before.Indexes {
		if named[index.Name] {
			continue
		}
		if actual := indexByName(observed.Indexes, index.Name); actual == nil || !sameIndex(index, actual) {
			return fmt.Errorf("YDB altered table changed the index %q", index.Name)
		}
	}
	if len(observed.Indexes) != len(before.Indexes)-len(request.DropIndexes)+len(request.AddIndexes) {
		return fmt.Errorf("YDB altered table holds unexpected indexes")
	}
	return nil
}

// sameIndexKind reports whether an observed index is the kind the request adds.
func sameIndexKind(actual *native.TableIndexDescription, requested *Ydb_Table.TableIndex) bool {
	switch requested.Type.(type) {
	case *Ydb_Table.TableIndex_GlobalIndex:
		return actual.GetGlobalIndex() != nil
	case *Ydb_Table.TableIndex_GlobalAsyncIndex:
		return actual.GetGlobalAsyncIndex() != nil
	case *Ydb_Table.TableIndex_GlobalUniqueIndex:
		return actual.GetGlobalUniqueIndex() != nil
	}
	return false
}

// confirmPartitioning requires a partitioning alteration to leave the table
// with every setting the request names and with the partition columns it held.
// The server derives other settings from the ones named, so those are not
// compared. Any other request leaves the partitioning unread.
func confirmPartitioning(request *Ydb_Table.AlterTableRequest, before, observed *native.DescribeTableResult) error {
	if request.AlterPartitioningSettings == nil {
		return nil
	}
	if !tablecreate.PartitioningHolds(observed.PartitioningSettings, request.AlterPartitioningSettings) ||
		!slices.Equal(observed.PartitioningSettings.GetPartitionBy(), before.PartitioningSettings.GetPartitionBy()) {
		return fmt.Errorf("YDB altered table partitioning settings differ from the request")
	}
	return nil
}

// confirmTTL requires the TTL read back to be the one the request sets, none
// after a removal, and otherwise the TTL the table held before the call.
func confirmTTL(request *Ydb_Table.AlterTableRequest, before, observed *native.DescribeTableResult) error {
	want := before.TtlSettings
	switch action := request.TtlAction.(type) {
	case *Ydb_Table.AlterTableRequest_SetTtlSettings:
		want = action.SetTtlSettings
	case *Ydb_Table.AlterTableRequest_DropTtlSettings:
		want = nil
	}
	if !proto.Equal(observed.TtlSettings, want) {
		return fmt.Errorf("YDB altered table TTL differs from the request")
	}
	return nil
}
