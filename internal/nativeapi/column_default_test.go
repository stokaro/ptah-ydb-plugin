package nativeapi_test

import (
	"context"
	"errors"
	"testing"

	qt "github.com/frankban/quicktest"
	"github.com/stokaro/ptah-ydb-plugin/internal/nativeapi"
	native "github.com/stokaro/ptah-ydb-plugin/internal/nativeprotocol"
	"github.com/stokaro/ptah-ydb-plugin/internal/tablecreate"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb_Scheme"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb_Table"
	"google.golang.org/protobuf/proto"
)

type defaultSession struct {
	before, after *native.DescribeTableResult
	reads, closed int
}

func (s *defaultSession) Describe(context.Context, string) (*native.DescribeTableResult, error) {
	s.reads++
	if s.reads == 1 {
		return proto.CloneOf(s.before), nil
	}
	return proto.CloneOf(s.after), nil
}
func (s *defaultSession) Close() error { s.closed++; return nil }
func defaultTextValue(value string) *Ydb.TypedValue {
	return &Ydb.TypedValue{Type: &Ydb.Type{Type: &Ydb.Type_TypeId{TypeId: Ydb.Type_UTF8}}, Value: &Ydb.Value{Value: &Ydb.Value_TextValue{TextValue: value}}}
}

func TestColumnDefaultChecksBeforeAndAfterSingleDispatch(t *testing.T) {
	sentinel := errors.New("dispatch failed")
	for _, test := range []struct {
		name            string
		mutate          func(*defaultSession)
		executeErr      error
		wantDispatch    bool
		wantError       bool
		drop            bool
		beforeWriteErr  error
		database        string
		cancel          bool
		notNullPresence bool
	}{
		{name: "confirmed", wantDispatch: true},
		{name: "drop", drop: true, wantDispatch: true},
		{name: "equivalent nullability", notNullPresence: true, wantDispatch: true},
		{name: "native setting changed", mutate: func(s *defaultSession) { s.before.Columns[1].Family = "cold" }, wantError: true},
		{name: "nullability changed", mutate: func(s *defaultSession) {
			s.before.Columns[1].Type = &Ydb.Type{Type: &Ydb.Type_OptionalType{OptionalType: &Ydb.OptionalType{Item: s.before.Columns[1].Type}}}
		}, wantError: true},
		{name: "write guard refuses", beforeWriteErr: sentinel, wantError: true},
		{name: "canceled at guard", cancel: true, wantError: true},
		{name: "foreign database", database: "/other", wantError: true},
		{name: "changed literal value", mutate: func(s *defaultSession) {
			s.before.Columns[1].DefaultValue = &native.ColumnMeta_FromLiteral{FromLiteral: defaultTextValue("different")}
		}, wantError: true},
		{name: "changed literal type", mutate: func(s *defaultSession) {
			s.before.Columns[1].DefaultValue = &native.ColumnMeta_FromLiteral{FromLiteral: &Ydb.TypedValue{Type: &Ydb.Type{Type: &Ydb.Type_TypeId{TypeId: Ydb.Type_STRING}}, Value: &Ydb.Value{Value: &Ydb.Value_BytesValue{BytesValue: []byte("old")}}}}
		}, wantError: true},
		{name: "equivalent optional literal", mutate: func(s *defaultSession) {
			literal := s.before.Columns[1].GetFromLiteral()
			literal.Type = &Ydb.Type{Type: &Ydb.Type_OptionalType{OptionalType: &Ydb.OptionalType{Item: literal.Type}}}
		}, wantDispatch: true},
		{name: "stale", mutate: func(s *defaultSession) { s.before.Columns[1].DefaultValue = nil }, wantError: true},
		{name: "wrong default", mutate: func(s *defaultSession) { s.after.Columns[1].DefaultValue = nil }, wantDispatch: true, wantError: true},
		{name: "neighbor changed", mutate: func(s *defaultSession) { s.after.Columns[0].Name = "other" }, wantDispatch: true, wantError: true},
		{name: "key changed", mutate: func(s *defaultSession) { s.after.PrimaryKey = nil }, wantDispatch: true, wantError: true},
		{name: "dispatch failed", executeErr: sentinel, wantDispatch: true, wantError: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := qt.New(t)
			held := &native.ColumnMeta{Name: "value", Type: defaultTextValue("").Type, DefaultValue: &native.ColumnMeta_FromLiteral{FromLiteral: defaultTextValue("old")}}
			before := &native.DescribeTableResult{Self: &Ydb_Scheme.Entry{Name: "records", Type: Ydb_Scheme.Entry_TABLE}, StoreType: Ydb_Table.StoreType_STORE_TYPE_ROW, PrimaryKey: []string{"id"}, Columns: []*native.ColumnMeta{{Name: "id", Type: &Ydb.Type{Type: &Ydb.Type_TypeId{TypeId: Ydb.Type_UINT64}}}, held}}
			session := &defaultSession{before: proto.CloneOf(before), after: proto.CloneOf(before)}
			session.after.Columns[1].DefaultValue = &native.ColumnMeta_FromLiteral{FromLiteral: defaultTextValue("")}
			set := defaultTextValue("")
			if test.drop {
				set = nil
				session.after.Columns[1].DefaultValue = nil
			}
			if test.notNullPresence {
				held = proto.CloneOf(held)
				held.NotNull = new(true)
			}
			if test.mutate != nil {
				test.mutate(session)
			}
			operation, err := tablecreate.CaptureDefault("default", &protocol.Reference{Engine: "ydb", Kind: "table", Database: "/local", Name: "records"}, &tablecreate.DefaultAlteration{Held: held, Set: set})
			c.Assert(err, qt.IsNil)
			calls := 0
			runner := &nativeapi.ColumnDefault{Open: func(context.Context) (nativeapi.DefaultSession, error) { return session, nil }, Execute: func(_ context.Context, text string) error {
				calls++
				want := "ALTER TABLE `/local/records` ALTER COLUMN `value` SET DEFAULT ''u;"
				if test.drop {
					want = "ALTER TABLE `/local/records` ALTER COLUMN `value` DROP DEFAULT;"
				}
				c.Assert(text, qt.Equals, want)
				return test.executeErr
			}}
			ctx, cancel := context.WithCancel(c.Context())
			t.Cleanup(cancel)
			database := test.database
			if database == "" {
				database = "/local"
			}
			dispatched, err := runner.Apply(ctx, database, operation.Value, func(context.Context) error {
				if test.cancel {
					cancel()
				}
				return test.beforeWriteErr
			})
			c.Assert(dispatched, qt.Equals, test.wantDispatch)
			c.Assert(err != nil, qt.Equals, test.wantError)
			c.Assert(calls, qt.Equals, map[bool]int{false: 0, true: 1}[test.wantDispatch])
			wantClosed := 1
			if test.database != "" {
				wantClosed = 0
			}
			c.Assert(session.closed, qt.Equals, wantClosed)
			if test.executeErr != nil {
				c.Assert(err, qt.ErrorIs, sentinel)
				c.Assert(session.reads, qt.Equals, 1)
			}
		})
	}
}
