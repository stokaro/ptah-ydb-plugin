package nativeapi_test

import (
	"context"
	"errors"
	"os"
	"testing"

	qt "github.com/frankban/quicktest"
	"github.com/stokaro/ptah-ydb-plugin/internal/nativeapi"
	native "github.com/stokaro/ptah-ydb-plugin/internal/nativeprotocol"
	"github.com/stokaro/ptah-ydb-plugin/internal/tablecreate"
	"github.com/stokaro/ptah-ydb-plugin/schema"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/ydb-platform/ydb-go-genproto/Ydb_Scheme_V1"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb_Scheme"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb_Table"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

// columnFixture answers the creation of /local/probe/c1 with the descriptions
// local-ydb 26.2.1.14 gave for a column table hashed by id in three shards
// (internal/columnstore/testdata/26-2-hash).
type columnFixture struct {
	Ydb_Scheme_V1.SchemeServiceClient
	// directory is the entry the parent listing reports for itself.
	directory                             *Ydb_Scheme.Entry
	events                                []string
	statement                             string
	occupied, executeFailure, readFailure bool
	monitoringFailure, closeFailure       bool
	page                                  []byte
	described                             *native.ColumnTableDescribeResult
}

func newColumnFixture(c *qt.C) *columnFixture {
	c.Helper()
	page, err := os.ReadFile("../columnstore/testdata/26-2-hash.json")
	c.Assert(err, qt.IsNil)
	encoded, err := os.ReadFile("../columnstore/testdata/26-2-hash.table.json")
	c.Assert(err, qt.IsNil)
	var described native.ColumnTableDescribeResult
	c.Assert(protojson.Unmarshal(encoded, &described), qt.IsNil)
	return &columnFixture{page: page, described: &described, directory: &Ydb_Scheme.Entry{Name: "probe", Type: Ydb_Scheme.Entry_DIRECTORY}}
}

func (f *columnFixture) ListDirectory(_ context.Context, _ *Ydb_Scheme.ListDirectoryRequest, _ ...grpc.CallOption) (*Ydb_Scheme.ListDirectoryResponse, error) {
	f.events = append(f.events, "list")
	listing := &Ydb_Scheme.ListDirectoryResult{Self: proto.CloneOf(f.directory)}
	if f.occupied {
		listing.Children = []*Ydb_Scheme.Entry{{Name: "c1", Type: Ydb_Scheme.Entry_COLUMN_TABLE}}
	}
	return &Ydb_Scheme.ListDirectoryResponse{Operation: result(listing)}, nil
}

func (f *columnFixture) DescribeColumnTable(_ context.Context, path string) (*native.ColumnTableDescribeResult, error) {
	f.events = append(f.events, "describe "+path)
	if f.readFailure {
		return nil, errors.New("lost confirmation")
	}
	return proto.CloneOf(f.described), nil
}

func (f *columnFixture) Close() error {
	f.events = append(f.events, "close")
	if f.closeFailure {
		return errors.New("unconfirmed session release")
	}
	return nil
}

func (f *columnFixture) api() *nativeapi.ColumnTableCreation {
	return &nativeapi.ColumnTableCreation{Scheme: f,
		Open: func(context.Context) (nativeapi.ColumnDescriber, error) {
			f.events = append(f.events, "open")
			return f, nil
		},
		Execute: func(_ context.Context, statement string) error {
			f.events = append(f.events, "execute")
			f.statement = statement
			if f.executeFailure {
				return errors.New("lost reply")
			}
			return nil
		},
		Monitoring: func(_ context.Context, path string) ([]byte, error) {
			f.events = append(f.events, "monitoring "+path)
			if f.monitoringFailure {
				return nil, errors.New("monitoring unavailable")
			}
			return f.page, nil
		},
	}
}

// columnOperation captures the creation of probe/c1 with the given storage.
func columnOperation(c *qt.C, storage string) *protocol.NormalizedValue {
	c.Helper()
	return tableOperation(c, &protocol.DeclaredTable{Name: "c1", Schema: "probe", PrimaryKey: []string{"id", "ts"},
		Columns: []*protocol.DeclaredColumn{{Name: "id", Type: "Uint64", Primary: true}, {Name: "ts", Type: "Timestamp", Primary: true}, {Name: "body", Type: "Utf8", Nullable: true}}}, storage)
}

// tableOperation captures the creation of a declared column table in /local
// with the given storage.
func tableOperation(c *qt.C, declared *protocol.DeclaredTable, storage string) *protocol.NormalizedValue {
	c.Helper()
	owner := &protocol.Reference{Engine: "ydb", Kind: "table", Database: "/local", Schema: declared.Schema, Name: declared.Name}
	model := schema.NativeDesiredModel(schema.ColumnStorageProperties)
	facet, err := schema.NormalizeDesired(&protocol.Envelope{ProviderId: schema.ProviderID, Kind: model.Kind, SchemaVersion: 1, Encoding: model.Encoding,
		DefinitionSha256: model.DefinitionSha256, Owner: owner, Payload: []byte(storage)})
	c.Assert(err, qt.IsNil)
	subject := &protocol.ComparisonIdentity{Kind: "table", Database: &protocol.ComparisonPart{Source: "/local", Normalized: "/local"},
		Schema: &protocol.ComparisonPart{Source: declared.Schema, Normalized: declared.Schema}, Name: &protocol.ComparisonPart{Source: declared.Name, Normalized: declared.Name}}
	record := &protocol.ComparisonRecord{Kind: model.Kind, Subject: subject, Value: &protocol.NormalizedValue{Id: "storage", Value: facet,
		NormalizedOwner: proto.CloneOf(owner), CanonicalFormat: model.CanonicalFormat, CanonicalProjection: facet.Payload}}
	declared = proto.CloneOf(declared)
	declared.Features = &protocol.CapturedFeatures{Facets: &protocol.ComparisonState{Representation: protocol.ValueInput_DESIRED, Records: []*protocol.ComparisonRecord{record}}}
	operation, err := tablecreate.CaptureColumnTable("create", owner, declared, nil)
	c.Assert(err, qt.IsNil)
	return operation
}

func TestColumnTableCreationDispatchAndConfirmation(t *testing.T) {
	confirmed := []string{"list", "open", "guard", "execute", "describe /local/probe/c1", "monitoring /local/probe/c1", "close"}
	for _, test := range []struct {
		name                             string
		storage                          string
		modify                           func(*columnFixture)
		guardFailure, dispatched, failed bool
		events                           []string
	}{
		{"confirmed", `{"hashColumns":["id"],"partitions":"3"}`, func(*columnFixture) {}, false, true, false, confirmed},
		{"confirmed with the server's shard count", `{"hashColumns":["id"]}`, func(*columnFixture) {}, false, true, false, confirmed},
		{"occupied", `{"hashColumns":["id"],"partitions":"3"}`, func(f *columnFixture) { f.occupied = true }, false, false, true, []string{"list"}},
		{"lease lost", `{"hashColumns":["id"],"partitions":"3"}`, func(*columnFixture) {}, true, false, true, []string{"list", "open", "guard", "close"}},
		{"lost reply", `{"hashColumns":["id"],"partitions":"3"}`, func(f *columnFixture) { f.executeFailure = true }, false, true, true, []string{"list", "open", "guard", "execute", "close"}},
		{"lost read", `{"hashColumns":["id"],"partitions":"3"}`, func(f *columnFixture) { f.readFailure = true }, false, true, true,
			[]string{"list", "open", "guard", "execute", "describe /local/probe/c1", "close"}},
		{"monitoring unreachable", `{"hashColumns":["id"],"partitions":"3"}`, func(f *columnFixture) { f.monitoringFailure = true }, false, true, true, confirmed},
		{"another shard count", `{"hashColumns":["id"],"partitions":"4"}`, func(*columnFixture) {}, false, true, true, confirmed},
		{"another hash key", `{"hashColumns":["id","ts"],"partitions":"3"}`, func(*columnFixture) {}, false, true, true, confirmed},
		{"the whole key without a hash key", `{"partitions":"3"}`, func(*columnFixture) {}, false, true, true, confirmed},
		{"a TTL the table does not hold", `{"hashColumns":["id"],"ttl":{"enabled":{"column":"id","columnUnit":"1","tiers":[{"applyAfterSeconds":"60","delete":{}}]}}}`,
			func(*columnFixture) {}, false, true, true, confirmed},
		{"unconfirmed cleanup", `{"hashColumns":["id"],"partitions":"3"}`, func(f *columnFixture) { f.closeFailure = true }, false, true, true, confirmed},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := qt.New(t)
			fixture := newColumnFixture(c)
			test.modify(fixture)
			operation := columnOperation(c, test.storage)
			original := proto.CloneOf(operation.Value)
			dispatched, err := fixture.api().Apply(t.Context(), "/local", operation.Value, func(context.Context) error {
				fixture.events = append(fixture.events, "guard")
				return map[bool]error{true: errors.New("lease lost")}[test.guardFailure]
			})
			c.Assert(dispatched, qt.Equals, test.dispatched)
			c.Assert(err != nil, qt.Equals, test.failed)
			c.Assert(fixture.events, qt.DeepEquals, test.events)
			c.Assert(proto.Equal(original, operation.Value), qt.IsTrue)
		})
	}
}

// TestColumnTableCreationWritesTheHostsStatement dispatches YQL's CREATE TABLE,
// which writes the hash function the host reads, at the bound path.
func TestColumnTableCreationWritesTheHostsStatement(t *testing.T) {
	c := qt.New(t)
	fixture := newColumnFixture(c)
	_, err := fixture.api().Apply(t.Context(), "/local", columnOperation(c, `{"hashColumns":["id"],"partitions":"3"}`).Value, func(context.Context) error { return nil })
	c.Assert(err, qt.IsNil)
	c.Assert(fixture.statement, qt.Equals, "CREATE TABLE `/local/probe/c1` (\n    `id` Uint64 NOT NULL,\n    `ts` Timestamp NOT NULL,\n    `body` Utf8,\n"+
		"    PRIMARY KEY (`id`, `ts`)\n) PARTITION BY HASH (`id`) WITH (STORE = COLUMN, AUTO_PARTITIONING_MIN_PARTITIONS_COUNT = 3);")
}

// TestColumnTableCreationNeedsTheMonitoringEndpoint refuses before anything is
// read or written when the session names no monitoring endpoint.
func TestColumnTableCreationNeedsTheMonitoringEndpoint(t *testing.T) {
	c := qt.New(t)
	fixture := newColumnFixture(c)
	api := fixture.api()
	api.Monitoring = nil
	dispatched, err := api.Apply(t.Context(), "/local", columnOperation(c, `{"hashColumns":["id"],"partitions":"3"}`).Value, func(context.Context) error { return nil })
	c.Assert(dispatched, qt.IsFalse)
	c.Assert(err, qt.ErrorIs, nativeapi.ErrNoMonitoring)
	c.Assert(fixture.events, qt.HasLen, 0)
}

// TestColumnTableCreationRefusesAnotherDatabase binds the owner to the session.
func TestColumnTableCreationRefusesAnotherDatabase(t *testing.T) {
	c := qt.New(t)
	fixture := newColumnFixture(c)
	dispatched, err := fixture.api().Apply(t.Context(), "/other", columnOperation(c, `{"hashColumns":["id"],"partitions":"3"}`).Value, func(context.Context) error { return nil })
	c.Assert(dispatched, qt.IsFalse)
	c.Assert(err, qt.ErrorMatches, `YDB table creation targets another database`)
	c.Assert(fixture.events, qt.HasLen, 0)
}

// TestColumnTableCreationConfirmsTheWholeKeyAsTheHashKey confirms a creation
// that states no hash key against a table YDB hashes by its whole key, read
// from local-ydb 25.1.4.7 (internal/columnstore/testdata/25-1-delete-ttl-inline).
func TestColumnTableCreationConfirmsTheWholeKeyAsTheHashKey(t *testing.T) {
	c := qt.New(t)
	fixture := newColumnFixture(c)
	page, err := os.ReadFile("../columnstore/testdata/25-1-delete-ttl-inline.json")
	c.Assert(err, qt.IsNil)
	encoded, err := os.ReadFile("../columnstore/testdata/25-1-delete-ttl-inline.table.json")
	c.Assert(err, qt.IsNil)
	fixture.page, fixture.described = page, &native.ColumnTableDescribeResult{}
	c.Assert(protojson.Unmarshal(encoded, fixture.described), qt.IsNil)
	operation := tableOperation(c, &protocol.DeclaredTable{Name: "c3", Schema: "probe", PrimaryKey: []string{"ts", "id"},
		Columns: []*protocol.DeclaredColumn{{Name: "ts", Type: "Timestamp", Primary: true}, {Name: "id", Type: "Uint64", Primary: true}}},
		`{"ttl":{"enabled":{"column":"ts","tiers":[{"applyAfterSeconds":"86400","delete":{}}]}}}`)
	dispatched, err := fixture.api().Apply(t.Context(), "/local", operation.Value, func(context.Context) error { return nil })
	c.Assert(err, qt.IsNil)
	c.Assert(dispatched, qt.IsTrue)
}

// TestColumnTableCreationRefusesUnexpectedLocalIndexes leaves the creation
// unconfirmed when the table reads back with local indexes nobody asked for,
// as a page from local-ydb 26.2.1.14 holds them
// (internal/columnstore/testdata/26-2-indexes).
func TestColumnTableCreationRefusesUnexpectedLocalIndexes(t *testing.T) {
	c := qt.New(t)
	fixture := newColumnFixture(c)
	page, err := os.ReadFile("../columnstore/testdata/26-2-indexes.json")
	c.Assert(err, qt.IsNil)
	optional := &Ydb.Type{Type: &Ydb.Type_OptionalType{OptionalType: &Ydb.OptionalType{Item: &Ydb.Type{Type: &Ydb.Type_TypeId{TypeId: Ydb.Type_UTF8}}}}}
	fixture.page, fixture.directory = page, &Ydb_Scheme.Entry{Name: "local", Type: Ydb_Scheme.Entry_DATABASE}
	fixture.described = &native.ColumnTableDescribeResult{Self: &Ydb_Scheme.Entry{Name: "col_index_probe", Type: Ydb_Scheme.Entry_COLUMN_TABLE},
		StoreType: Ydb_Table.StoreType_STORE_TYPE_COLUMN, PrimaryKey: []string{"id"}, PartitioningSettings: &Ydb_Table.PartitioningSettings{PartitionBy: []string{"id"}},
		Columns: []*native.ColumnMeta{{Name: "id", Type: &Ydb.Type{Type: &Ydb.Type_TypeId{TypeId: Ydb.Type_UINT64}}}, {Name: "body", Type: optional}}}
	operation := tableOperation(c, &protocol.DeclaredTable{Name: "col_index_probe", PrimaryKey: []string{"id"},
		Columns: []*protocol.DeclaredColumn{{Name: "id", Type: "Uint64", Primary: true}, {Name: "body", Type: "Utf8", Nullable: true}}}, `{"hashColumns":["id"]}`)
	dispatched, err := fixture.api().Apply(t.Context(), "/local", operation.Value, func(context.Context) error { return nil })
	c.Assert(err, qt.ErrorMatches, `YDB created column table has unexpected local indexes`)
	c.Assert(dispatched, qt.IsTrue)
}

// TestColumnTableCreationRefusesOtherColumns leaves the creation unconfirmed
// when a column reads back with another type or nullability than the request
// states.
func TestColumnTableCreationRefusesOtherColumns(t *testing.T) {
	for _, test := range []struct {
		name string
		body *protocol.DeclaredColumn
	}{
		{"another type", &protocol.DeclaredColumn{Name: "body", Type: "String", Nullable: true}},
		{"another nullability", &protocol.DeclaredColumn{Name: "body", Type: "Utf8"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := qt.New(t)
			fixture := newColumnFixture(c)
			operation := tableOperation(c, &protocol.DeclaredTable{Name: "c1", Schema: "probe", PrimaryKey: []string{"id", "ts"},
				Columns: []*protocol.DeclaredColumn{{Name: "id", Type: "Uint64", Primary: true}, {Name: "ts", Type: "Timestamp", Primary: true}, test.body}}, `{"hashColumns":["id"],"partitions":"3"}`)
			dispatched, err := fixture.api().Apply(t.Context(), "/local", operation.Value, func(context.Context) error { return nil })
			c.Assert(err, qt.ErrorMatches, `YDB created column differs from the request`)
			c.Assert(dispatched, qt.IsTrue)
		})
	}
}
