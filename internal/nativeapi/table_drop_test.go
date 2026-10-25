package nativeapi_test

import (
	"context"
	"errors"
	"testing"

	qt "github.com/frankban/quicktest"
	"github.com/ydb-platform/ydb-go-genproto/Ydb_Scheme_V1"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb_Scheme"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/proto"

	"github.com/stokaro/ptah-ydb-plugin/internal/nativeapi"
	native "github.com/stokaro/ptah-ydb-plugin/internal/nativeprotocol"
	"github.com/stokaro/ptah-ydb-plugin/internal/tablecreate"
	"github.com/stokaro/ptah-ydb-plugin/schema"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
)

// dropFixture holds /local/app/records, a row table of (id, payload) keyed
// by id, until a DROP TABLE removes it.
type dropFixture struct {
	Ydb_Scheme_V1.SchemeServiceClient
	entry     *Ydb_Scheme.Entry
	held      *native.DescribeTableResult
	events    []string
	executed  []string
	keepTable bool
	failDrop  bool
}

func newDropFixture() *dropFixture {
	return &dropFixture{entry: &Ydb_Scheme.Entry{Name: "records", Type: Ydb_Scheme.Entry_TABLE},
		held: &native.DescribeTableResult{Columns: []*native.ColumnMeta{{Name: "id"}, {Name: "payload"}}, PrimaryKey: []string{"id"}}}
}

func (f *dropFixture) ListDirectory(context.Context, *Ydb_Scheme.ListDirectoryRequest, ...grpc.CallOption) (*Ydb_Scheme.ListDirectoryResponse, error) {
	f.events = append(f.events, "list")
	listing := &Ydb_Scheme.ListDirectoryResult{Self: &Ydb_Scheme.Entry{Name: "app", Type: Ydb_Scheme.Entry_DIRECTORY}}
	if f.entry != nil {
		listing.Children = []*Ydb_Scheme.Entry{proto.CloneOf(f.entry)}
	}
	return &Ydb_Scheme.ListDirectoryResponse{Operation: result(listing)}, nil
}

func (f *dropFixture) Describe(_ context.Context, path string) (*native.DescribeTableResult, error) {
	f.events = append(f.events, "describe "+path)
	return proto.CloneOf(f.held), nil
}

func (f *dropFixture) DescribeColumnTable(_ context.Context, path string) (*native.ColumnTableDescribeResult, error) {
	f.events = append(f.events, "describe column table "+path)
	return &native.ColumnTableDescribeResult{Columns: f.held.Columns, PrimaryKey: f.held.PrimaryKey}, nil
}

func (f *dropFixture) Close() error {
	f.events = append(f.events, "close")
	return nil
}

func (f *dropFixture) api() *nativeapi.TableDrop {
	return &nativeapi.TableDrop{Scheme: f, Open: func(context.Context) (nativeapi.TableDescriber, error) { return f, nil },
		Execute: func(_ context.Context, statement string) error {
			f.executed = append(f.executed, statement)
			if f.failDrop {
				return errors.New("lost reply")
			}
			if !f.keepTable {
				f.entry = nil
			}
			return nil
		}}
}

// removal is the captured removal of app/records in store.
func removal(c *qt.C, store string) *protocol.Envelope {
	c.Helper()
	observed := &protocol.ObservedTable{Name: "records", Schema: "app", Columns: []*protocol.ObservedColumn{{Name: "id", PrimaryKey: true}, {Name: "payload"}}}
	if store == tablecreate.ColumnStore {
		observed.Features = &protocol.CapturedFeatures{Facets: &protocol.ComparisonState{Records: []*protocol.ComparisonRecord{{Kind: schema.ColumnStorageProperties}}}}
	}
	value, err := tablecreate.CaptureDrop("drop", &protocol.Reference{Engine: "ydb", Kind: "table", Schema: "app", Name: "records"}, observed)
	c.Assert(err, qt.IsNil)
	return value.Value
}

func TestTableDropDispatchAndConfirmation(t *testing.T) {
	for _, tc := range []struct {
		store string
		kind  Ydb_Scheme.Entry_Type
		read  string
	}{
		{store: tablecreate.RowStore, kind: Ydb_Scheme.Entry_TABLE, read: "describe /local/app/records"},
		{store: tablecreate.ColumnStore, kind: Ydb_Scheme.Entry_COLUMN_TABLE, read: "describe column table /local/app/records"},
	} {
		t.Run(tc.store, func(t *testing.T) {
			c := qt.New(t)
			fixture := newDropFixture()
			fixture.entry.Type = tc.kind
			dispatched, err := fixture.api().Apply(c.Context(), "/local", removal(c, tc.store), allow)
			c.Assert(err, qt.IsNil)
			c.Assert(dispatched, qt.IsTrue)
			c.Assert(fixture.executed, qt.DeepEquals, []string{"DROP TABLE `/local/app/records`"})
			c.Assert(fixture.events, qt.DeepEquals, []string{"list", tc.read, "list", "close"})
		})
	}
}

// Each row changes the table after the plan read it, and the removal refuses
// before anything is sent.
func TestTableDropRefusesAnotherTable(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*dropFixture)
		want   string
	}{
		{name: "gone", change: func(f *dropFixture) { f.entry = nil },
			want: "the path of YDB table /local/app/records no longer holds the row table the plan captured"},
		{name: "another store", change: func(f *dropFixture) { f.entry.Type = Ydb_Scheme.Entry_COLUMN_TABLE },
			want: "the path of YDB table /local/app/records no longer holds the row table the plan captured"},
		{name: "a column added", change: func(f *dropFixture) { f.held.Columns = append(f.held.Columns, &native.ColumnMeta{Name: "extra"}) },
			want: "YDB table /local/app/records holds other columns or another key than the plan captured"},
		{name: "another key", change: func(f *dropFixture) { f.held.PrimaryKey = []string{"payload"} },
			want: "YDB table /local/app/records holds other columns or another key than the plan captured"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := qt.New(t)
			fixture := newDropFixture()
			tc.change(fixture)
			dispatched, err := fixture.api().Apply(c.Context(), "/local", removal(c, tablecreate.RowStore), allow)
			c.Assert(err, qt.ErrorMatches, tc.want)
			c.Assert(dispatched, qt.IsFalse)
			c.Assert(fixture.executed, qt.HasLen, 0)
		})
	}
}

func TestTableDropOutcomes(t *testing.T) {
	c := qt.New(t)
	fixture := newDropFixture()
	fixture.keepTable = true
	dispatched, err := fixture.api().Apply(c.Context(), "/local", removal(c, tablecreate.RowStore), allow)
	c.Assert(err, qt.ErrorMatches, "the removal of YDB table /local/app/records was not confirmed")
	c.Assert(dispatched, qt.IsTrue)

	fixture = newDropFixture()
	fixture.failDrop = true
	dispatched, err = fixture.api().Apply(c.Context(), "/local", removal(c, tablecreate.RowStore), allow)
	c.Assert(err, qt.ErrorMatches, "lost reply")
	c.Assert(dispatched, qt.IsTrue)

	fixture = newDropFixture()
	lost := errors.New("lock lost")
	dispatched, err = fixture.api().Apply(c.Context(), "/local", removal(c, tablecreate.RowStore), func(context.Context) error { return lost })
	c.Assert(err, qt.ErrorIs, lost)
	c.Assert(dispatched, qt.IsFalse)
	c.Assert(fixture.executed, qt.HasLen, 0)

	fixture = newDropFixture()
	dispatched, err = fixture.api().Apply(c.Context(), "/other", removal(c, tablecreate.RowStore), allow)
	c.Assert(err, qt.IsNil)
	c.Assert(fixture.executed, qt.DeepEquals, []string{"DROP TABLE `/other/app/records`"})
	c.Assert(dispatched, qt.IsTrue)
}
