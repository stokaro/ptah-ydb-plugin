package nativeapi_test

import (
	"context"
	"slices"
	"testing"

	qt "github.com/frankban/quicktest"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb_Table"
	"google.golang.org/protobuf/proto"

	"github.com/stokaro/ptah-ydb-plugin/internal/nativeapi"
	native "github.com/stokaro/ptah-ydb-plugin/internal/nativeprotocol"
	"github.com/stokaro/ptah-ydb-plugin/internal/tablecreate"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
	"github.com/stokaro/ptah-ydb-plugin/schema"
)

// inMemory and regular are family hot in each cache mode.
var (
	inMemory = &native.ColumnFamily{Name: "hot", CacheMode: native.ColumnFamily_CACHE_MODE_IN_MEMORY}
	regular  = &native.ColumnFamily{Name: "hot", CacheMode: native.ColumnFamily_CACHE_MODE_REGULAR}
)

// cachedCreation creates app.records with family hot in memory, against a
// table that reads back with held, and returns what Create was sent.
func cachedCreation(c *qt.C, held []*native.ColumnFamily) (*Ydb_Table.CreateTableRequest, error) {
	c.Helper()
	fixture := creationFixture{families: held}
	api := &nativeapi.TableCreation{Scheme: &fixture, Open: func(context.Context) (nativeapi.CreationSession, error) { return &fixture, nil }}
	owner := &protocol.Reference{Engine: "ydb", Kind: "table", Database: "/local", Schema: "app", Name: "records"}
	operation, err := tablecreate.Capture("create", owner, &protocol.DeclaredTable{Name: "records", Schema: "app", PrimaryKey: []string{"id"},
		Columns: []*protocol.DeclaredColumn{{Name: "id", Type: "Uint64", Primary: true}}}, nil)
	c.Assert(err, qt.IsNil)
	request, err := tablecreate.Request(operation.Value)
	c.Assert(err, qt.IsNil)
	request.ColumnFamilies = []*Ydb_Table.ColumnFamily{schema.RequestFamily(inMemory)}
	operation, err = tablecreate.CaptureRequest("create", owner, request)
	c.Assert(err, qt.IsNil)
	dispatched, err := api.Apply(c.Context(), "/local", operation.Value, func(context.Context) error { return nil })
	c.Assert(dispatched, qt.IsTrue)
	return fixture.sent, err
}

// The table service is sent the cache mode as field 6 of the family, the
// bytes the upstream ColumnFamily writes, and the creation is confirmed by
// the cache mode the table reads back with.
func TestNativeTableCreation_CacheMode_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	sent, err := cachedCreation(c, []*native.ColumnFamily{{Name: "default"}, inMemory})
	c.Assert(err, qt.IsNil)
	c.Assert(sent.ColumnFamilies, qt.HasLen, 1)
	wire, err := proto.Marshal(sent.ColumnFamilies[0])
	c.Assert(err, qt.IsNil)
	upstream, err := proto.Marshal(inMemory)
	c.Assert(err, qt.IsNil)
	c.Assert(wire, qt.DeepEquals, upstream)
}

// A table that reads back without the cache mode it was created with, as
// the table service of a line that does not know the field creates it, or
// with another one, is not confirmed.
func TestNativeTableCreation_CacheMode_FailurePath(t *testing.T) {
	for _, test := range []struct {
		name string
		held []*native.ColumnFamily
	}{
		{name: "no cache mode", held: []*native.ColumnFamily{{Name: "default"}, {Name: "hot"}}},
		{name: "another cache mode", held: []*native.ColumnFamily{{Name: "default"}, regular}},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			_, err := cachedCreation(c, test.held)
			c.Assert(err, qt.ErrorMatches, `YDB created column family differs from the request`)
		})
	}
}

// heldHot is app.records holding family hot with the settings given.
func heldHot(settings string) string {
	return tableHead + `"columns":[` + idColumn + `],"columnFamilies":[{"name":"default"},{"name":"hot"` + settings + `}]}`
}

// A cache mode is sent as field 6 of the family the request changes or adds,
// and the alteration is confirmed by the cache mode the table then holds.
func TestNativeTableAlteration_CacheMode_HappyPath(t *testing.T) {
	for _, test := range []struct {
		name, before, after, payload string
	}{
		{name: "a cache mode set", before: heldHot(``), after: heldHot(`,"cacheMode":"CACHE_MODE_IN_MEMORY"`),
			payload: `{"request":{"alterColumnFamilies":[{"name":"hot","cacheMode":"CACHE_MODE_IN_MEMORY"}]},"held_families":[{"name":"hot"}]}`},
		{name: "a cache mode changed", before: heldHot(`,"cacheMode":"CACHE_MODE_REGULAR"`), after: heldHot(`,"cacheMode":"CACHE_MODE_IN_MEMORY"`),
			payload: `{"request":{"alterColumnFamilies":[{"name":"hot","cacheMode":"CACHE_MODE_IN_MEMORY"}]},"held_families":[{"name":"hot","cacheMode":"CACHE_MODE_REGULAR"}]}`},
		{name: "a family added in memory", before: tableHead + `"columns":[` + idColumn + `],"columnFamilies":[{"name":"default"}]}`, after: heldHot(`,"cacheMode":"CACHE_MODE_IN_MEMORY"`),
			payload: `{"request":{"addColumnFamilies":[{"name":"hot","cacheMode":"CACHE_MODE_IN_MEMORY"}]}}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			fixture := &alterFixture{before: test.before, after: test.after}
			dispatched, err := applyAlteration(c, fixture, "", test.payload, false)
			c.Assert(err, qt.IsNil)
			c.Assert(dispatched, qt.IsTrue)
			families := slices.Concat(fixture.sent.AddColumnFamilies, fixture.sent.AlterColumnFamilies)
			c.Assert(families, qt.HasLen, 1)
			wire, err := proto.Marshal(families[0])
			c.Assert(err, qt.IsNil)
			upstream, err := proto.Marshal(inMemory)
			c.Assert(err, qt.IsNil)
			c.Assert(wire, qt.DeepEquals, upstream)
		})
	}
}

// A cache mode the table holds other than the plan captured refuses the
// alteration before it is sent; one the table does not take, or one it takes
// for a family the request does not change, is not confirmed.
func TestNativeTableAlteration_CacheMode_FailurePath(t *testing.T) {
	setHot := `{"request":{"alterColumnFamilies":[{"name":"hot","cacheMode":"CACHE_MODE_IN_MEMORY"}]},"held_families":[{"name":"hot"}]}`
	for _, test := range []struct {
		name, before, after, payload string
		dispatched                   bool
		message                      string
	}{
		{name: "a cache mode the plan did not capture", before: heldHot(`,"cacheMode":"CACHE_MODE_REGULAR"`), payload: setHot,
			message: "YDB column family differs from the plan"},
		{name: "a cache mode not applied", before: heldHot(``), after: heldHot(``), payload: setHot, dispatched: true,
			message: "YDB altered column family differs from the request"},
		{name: "another family given a cache mode", before: heldHot(``),
			after:   tableHead + `"columns":[` + idColumn + `],"columnFamilies":[{"name":"default","cacheMode":"CACHE_MODE_REGULAR"},{"name":"hot","cacheMode":"CACHE_MODE_IN_MEMORY"}]}`,
			payload: setHot, dispatched: true, message: "YDB altered column family differs from the request"},
		{name: "an added family's cache mode not applied", before: tableHead + `"columns":[` + idColumn + `],"columnFamilies":[{"name":"default"}]}`, after: heldHot(``),
			payload: `{"request":{"addColumnFamilies":[{"name":"hot","cacheMode":"CACHE_MODE_IN_MEMORY"}]}}`, dispatched: true,
			message: "YDB added column family differs from the request"},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			dispatched, err := applyAlteration(c, &alterFixture{before: test.before, after: test.after}, "", test.payload, false)
			c.Assert(err, qt.ErrorMatches, test.message)
			c.Assert(dispatched, qt.Equals, test.dispatched)
		})
	}
}
