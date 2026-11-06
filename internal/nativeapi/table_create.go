package nativeapi

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"path"
	"slices"

	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/ydb-platform/ydb-go-genproto/Ydb_Scheme_V1"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb_Scheme"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb_Table"
	ydb "github.com/ydb-platform/ydb-go-sdk/v3"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"

	native "github.com/stokaro/ptah-ydb-plugin/internal/nativeprotocol"
	"github.com/stokaro/ptah-ydb-plugin/internal/tablecreate"
	"github.com/stokaro/ptah-ydb-plugin/schema"
)

// CreationSession exposes one native table session without a retry wrapper.
type CreationSession interface {
	Create(context.Context, *Ydb_Table.CreateTableRequest) error
	Describe(context.Context, string) (*native.DescribeTableResult, error)
	Close() error
}

// TableCreation owns the absence check, single dispatch, and confirmation.
// A creation opens a session with Open and closes it, unless Sessions lends it
// one.
type TableCreation struct {
	Scheme   Ydb_Scheme_V1.SchemeServiceClient
	Open     func(context.Context) (CreationSession, error)
	Sessions *CreationSessions
}

// CreationSessions lends the creations of one execution one table session in
// turn, so a batch of creations opens and closes one session rather than one
// for each table. A session is
// lent again only after a creation that succeeded on it; one a creation failed
// on, or gave up waiting for, is closed at once. The zero value is not usable:
// Open opens a session.
type CreationSessions struct {
	Open func(context.Context) (CreationSession, error)
	held CreationSession
}

// borrow returns the session the lender holds, or opens one.
func (s *CreationSessions) borrow(ctx context.Context) (CreationSession, error) {
	if s.held != nil {
		session := s.held
		s.held = nil
		return session, nil
	}
	return s.Open(ctx)
}

// giveBack keeps session for the next creation when the creation that
// borrowed it succeeded, and closes it when the creation failed.
func (s *CreationSessions) giveBack(session CreationSession, failure error) error {
	if failure != nil || s.held != nil {
		return session.Close()
	}
	s.held = session
	return nil
}

// Close closes the session the lender holds. Closing twice, or with no session
// held, does nothing.
func (s *CreationSessions) Close() error {
	if s.held == nil {
		return nil
	}
	session := s.held
	s.held = nil
	return session.Close()
}

// session returns the session a creation runs in, and the function that
// releases it once the creation has an answer, given the creation's failure.
func (c *TableCreation) session(ctx context.Context) (CreationSession, func(failure error) error, error) {
	if c.Sessions != nil {
		session, err := c.Sessions.borrow(ctx)
		if err != nil {
			return nil, nil, err
		}
		return session, func(failure error) error { return c.Sessions.giveBack(session, failure) }, nil
	}
	session, err := c.Open(ctx)
	if err != nil {
		return nil, nil, err
	}
	return session, func(error) error { return session.Close() }, nil
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
	if err := absent(ctx, c.Scheme, database, owner); err != nil {
		return false, err
	}
	session, release, err := c.session(ctx)
	if err != nil {
		return false, err
	}
	defer func() { err = errors.Join(err, release(err)) }()
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
	if err := confirmCreatedColumns(wanted.Columns, observed.Columns); err != nil {
		return err
	}
	if err := confirmCreatedTTL(wanted.TtlSettings, observed.TtlSettings); err != nil {
		return err
	}
	if err := confirmCreatedSettings(wanted, observed); err != nil {
		return err
	}
	return confirmCreatedFamilies(wanted.ColumnFamilies, observed.ColumnFamilies)
}

// confirmCreatedSettings requires the created table to hold at least the
// storage, partitioning, read replica and bloom filter settings the request
// names, its tiering and attributes exactly, no temporary flag, and the
// minimum partition count a requested starting layout sets.
func confirmCreatedSettings(wanted *Ydb_Table.CreateTableRequest, observed *native.DescribeTableResult) error {
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
	return nil
}

// confirmCreatedColumns requires each created column, in the request's order,
// to hold the requested name, type, nullability, family, default and sequence,
// and no compression or encoding. The two lists have the same length.
func confirmCreatedColumns(wanted []*Ydb_Table.ColumnMeta, observed []*native.ColumnMeta) error {
	for i, column := range wanted {
		actual := observed[i]
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
			!sameSequence(column.GetFromSequence(), actual.GetFromSequence()) || actual.Compression != nil || len(actual.Encoding) > 0 {
			return fmt.Errorf("YDB created column differs from the request")
		}
	}
	return nil
}

// confirmCreatedTTL requires the created table to hold the requested TTL, or
// none when the request names none. A run interval the request leaves unset
// takes the server's default.
func confirmCreatedTTL(wanted, observed *Ydb_Table.TtlSettings) error {
	if wanted == nil {
		if observed != nil {
			return fmt.Errorf("YDB created table has unexpected TTL")
		}
	} else if observed == nil ||
		!proto.Equal(wanted.GetDateTypeColumn(), observed.GetDateTypeColumn()) ||
		!proto.Equal(wanted.GetValueSinceUnixEpoch(), observed.GetValueSinceUnixEpoch()) ||
		(wanted.RunIntervalSeconds != 0 && wanted.RunIntervalSeconds != observed.RunIntervalSeconds) {
		return fmt.Errorf("YDB created table TTL differs from the request")
	}
	return nil
}

// confirmCreatedFamilies requires each requested column family to be created
// once, holding at least the settings the request names.
func confirmCreatedFamilies(wanted []*Ydb_Table.ColumnFamily, observed []*native.ColumnFamily) error {
	families, err := requestFamilies(wanted)
	if err != nil {
		return err
	}
	for _, family := range families {
		var actual *native.ColumnFamily
		for _, candidate := range observed {
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
