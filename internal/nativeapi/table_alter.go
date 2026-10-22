package nativeapi

import (
	"context"
	"errors"
	"fmt"
	"path"
	"slices"

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
// of a captured column family alteration.
type TableAlteration struct {
	Open func(context.Context) (AlterationSession, error)
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
	if err := session.Alter(ctx, request); err != nil {
		return true, err
	}
	observed, err := session.Describe(ctx, target)
	if err != nil {
		return true, err
	}
	return true, confirmTableAlteration(owner, alteration, before, observed)
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
	if !slices.Equal(before.PrimaryKey, observed.PrimaryKey) || len(before.Columns) != len(observed.Columns) {
		return fmt.Errorf("YDB altered table key or columns differ from the request")
	}
	request := alteration.Request
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
	for i, column := range before.Columns {
		actual := observed.Columns[i]
		want := familyName(column.Family)
		if index := slices.IndexFunc(request.AlterColumns, func(move *Ydb_Table.ColumnMeta) bool { return move.Name == column.Name }); index >= 0 {
			want = familyName(request.AlterColumns[index].Family)
		}
		if actual.Name != column.Name || !proto.Equal(actual.Type, column.Type) || actual.GetNotNull() != column.GetNotNull() || familyName(actual.Family) != want {
			return fmt.Errorf("YDB altered column differs from the request")
		}
	}
	return nil
}
