package nativeapi

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"path"
	"slices"

	native "github.com/stokaro/ptah-ydb-plugin/internal/nativeprotocol"
	"github.com/stokaro/ptah-ydb-plugin/internal/tablecreate"
	"github.com/stokaro/ptah-ydb-plugin/schema"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/ydb-platform/ydb-go-genproto/Ydb_Scheme_V1"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb_Scheme"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb_Table"
	ydb "github.com/ydb-platform/ydb-go-sdk/v3"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
)

// CreationSession exposes one native table session without a retry wrapper.
type CreationSession interface {
	Create(context.Context, *Ydb_Table.CreateTableRequest) error
	Describe(context.Context, string) (*native.DescribeTableResult, error)
	Close() error
}

// TableCreation owns the absence check, single dispatch, and confirmation.
type TableCreation struct {
	Scheme Ydb_Scheme_V1.SchemeServiceClient
	Open   func(context.Context) (CreationSession, error)
}

// NewTableCreation binds native clients to the authenticated session driver.
func NewTableCreation(driver *ydb.Driver) *TableCreation {
	return &TableCreation{Scheme: Ydb_Scheme_V1.NewSchemeServiceClient(ydb.GRPCConn(driver)),
		Open: func(ctx context.Context) (CreationSession, error) { return OpenTable(ctx, driver) }}
}

// Apply refuses any existing path, then creates and confirms the captured table.
// A failure after dispatch remains uncertain and is never retried.
func (c *TableCreation) Apply(ctx context.Context, database string, value *protocol.Envelope, beforeWrite func(context.Context) error) (dispatched bool, err error) {
	request, err := tablecreate.Request(value)
	if err != nil {
		return false, err
	}
	owner := proto.CloneOf(value.Owner)
	if owner.Database != "" && owner.Database != database {
		return false, fmt.Errorf("YDB table creation targets another database")
	}
	owner.Database = database
	// Re-normalize the bound identity before path.Join can erase path components.
	bound := proto.CloneOf(value)
	bound.Owner = owner
	if _, err := tablecreate.Normalize(&protocol.ValueInput{Id: "execute", Representation: protocol.ValueInput_OPERATION, Value: bound}); err != nil {
		return false, err
	}
	directory := path.Join(database, owner.Schema)
	if err := absent(ctx, c.Scheme, directory, owner); err != nil {
		return false, err
	}
	session, err := c.Open(ctx)
	if err != nil {
		return false, err
	}
	defer func() { err = errors.Join(err, session.Close()) }()
	if err := beforeWrite(ctx); err != nil {
		return false, err
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	request.Path = path.Join(directory, owner.Name)
	if err := session.Create(ctx, request); err != nil {
		return true, err
	}
	observed, err := session.Describe(ctx, request.Path)
	if err != nil {
		return true, err
	}
	return true, confirmTableCreation(owner, request, observed)
}

func confirmTableCreation(owner *protocol.Reference, wanted *Ydb_Table.CreateTableRequest, observed *native.DescribeTableResult) error {
	if err := schema.Known(observed); err != nil {
		return err
	}
	if observed.GetSelf().GetName() != owner.Name || observed.GetSelf().GetType() != Ydb_Scheme.Entry_TABLE ||
		(observed.StoreType != Ydb_Table.StoreType_STORE_TYPE_ROW && observed.StoreType != Ydb_Table.StoreType_STORE_TYPE_UNSPECIFIED) ||
		!slices.Equal(wanted.PrimaryKey, observed.PrimaryKey) || len(wanted.Columns) != len(observed.Columns) ||
		len(observed.Changefeeds) != 0 {
		return fmt.Errorf("YDB created table identity or structure differs from the request")
	}
	if err := confirmCreatedIndexes(wanted.Indexes, observed.Indexes); err != nil {
		return err
	}
	for i, column := range wanted.Columns {
		actual := observed.Columns[i]
		expectedType, expectedNullable, err := schema.ColumnType(column.Type)
		if err != nil {
			return err
		}
		actualType, actualNullable, err := schema.ColumnType(actual.Type)
		if err != nil {
			return err
		}
		if actual.GetNotNull() {
			actualNullable = false
		}
		family := func(value string) string {
			if value == "" {
				return "default"
			}
			return value
		}
		if column.Name != actual.Name || expectedType != actualType || expectedNullable != actualNullable ||
			family(column.Family) != family(actual.Family) || !proto.Equal(column.GetFromLiteral(), actual.GetFromLiteral()) ||
			actual.GetFromSequence() != nil || actual.Compression != nil || len(actual.Encoding) > 0 {
			return fmt.Errorf("YDB created column differs from the request")
		}
	}
	if wanted.TtlSettings == nil {
		if observed.TtlSettings != nil {
			return fmt.Errorf("YDB created table has unexpected TTL")
		}
	} else if observed.TtlSettings == nil ||
		!proto.Equal(wanted.TtlSettings.GetDateTypeColumn(), observed.TtlSettings.GetDateTypeColumn()) ||
		!proto.Equal(wanted.TtlSettings.GetValueSinceUnixEpoch(), observed.TtlSettings.GetValueSinceUnixEpoch()) ||
		(wanted.TtlSettings.RunIntervalSeconds != 0 && wanted.TtlSettings.RunIntervalSeconds != observed.TtlSettings.RunIntervalSeconds) {
		return fmt.Errorf("YDB created table TTL differs from the request")
	}
	if !messageContains(wanted.StorageSettings, observed.StorageSettings) ||
		!messageContains(wanted.PartitioningSettings, observed.PartitioningSettings) ||
		!messageContains(wanted.ReadReplicasSettings, observed.ReadReplicasSettings) ||
		(wanted.KeyBloomFilter != 0 && wanted.KeyBloomFilter != observed.KeyBloomFilter) ||
		wanted.Tiering != observed.Tiering || observed.Temporary ||
		!maps.Equal(wanted.Attributes, observed.Attributes) {
		return fmt.Errorf("YDB created table settings differ from the request")
	}
	// YDB keeps no record of a starting layout beyond the minimum partition
	// count it sets from it, unless the request names a minimum of its own.
	if started := tablecreate.StartingPartitions(wanted); started != 0 && wanted.GetPartitioningSettings().GetMinPartitionsCount() == 0 &&
		observed.GetPartitioningSettings().GetMinPartitionsCount() != started {
		return fmt.Errorf("YDB created table starting layout differs from the request")
	}
	for _, family := range wanted.ColumnFamilies {
		var actual *Ydb_Table.ColumnFamily
		for _, candidate := range observed.ColumnFamilies {
			if candidate.Name == family.Name {
				if actual != nil {
					return fmt.Errorf("YDB returned duplicate column families")
				}
				actual = candidate
			}
		}
		if actual == nil || !messageContains(family, actual) {
			return fmt.Errorf("YDB created column family differs from the request")
		}
	}
	return nil
}

// Native unset enum values and zero tuning values ask the server for defaults.
// Match explicit tuning fields without inventing a version-specific default.
func messageContains(wanted, actual proto.Message) bool {
	if wanted == nil || !wanted.ProtoReflect().IsValid() {
		return true
	}
	if actual == nil || !actual.ProtoReflect().IsValid() {
		return false
	}
	a, b := wanted.ProtoReflect(), actual.ProtoReflect()
	if a.Descriptor().FullName() != b.Descriptor().FullName() {
		return false
	}
	matches := true
	a.Range(func(field protoreflect.FieldDescriptor, value protoreflect.Value) bool {
		other := b.Get(field)
		switch {
		case field.IsMap():
			value.Map().Range(func(key protoreflect.MapKey, entry protoreflect.Value) bool {
				matches = other.Map().Has(key) && entry.Equal(other.Map().Get(key))
				return matches
			})
		case field.IsList():
			if value.List().Len() != other.List().Len() {
				matches = false
				break
			}
			for i := 0; i < value.List().Len(); i++ {
				if !value.List().Get(i).Equal(other.List().Get(i)) {
					matches = false
					break
				}
			}
		case field.Message() != nil:
			matches = messageContains(value.Message().Interface(), other.Message().Interface())
		default:
			matches = value.Equal(other)
		}
		return matches
	})
	return matches
}

// confirmCreatedIndexes requires the table read back to hold exactly the
// indexes the creation asked for, each ready, of the requested kind, on the
// requested key columns and covering the requested columns.
func confirmCreatedIndexes(wanted []*Ydb_Table.TableIndex, observed []*native.TableIndexDescription) error {
	if len(wanted) != len(observed) {
		return fmt.Errorf("YDB created table holds other indexes than the request")
	}
	kind := func(message protoreflect.Message) string {
		field := message.WhichOneof(message.Descriptor().Oneofs().ByName("type"))
		if field == nil {
			return ""
		}
		return string(field.Name())
	}
	for _, index := range wanted {
		at := slices.IndexFunc(observed, func(held *native.TableIndexDescription) bool { return held.GetName() == index.GetName() })
		if at < 0 {
			return fmt.Errorf("YDB created table lacks index %q", index.GetName())
		}
		held := observed[at]
		if held.GetStatus() != native.TableIndexDescription_STATUS_READY || !slices.Equal(held.GetIndexColumns(), index.GetIndexColumns()) ||
			!slices.Equal(held.GetDataColumns(), index.GetDataColumns()) || kind(held.ProtoReflect()) != kind(index.ProtoReflect()) {
			return fmt.Errorf("YDB created index %q differs from the request", index.GetName())
		}
	}
	return nil
}
