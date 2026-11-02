package nativeapi_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	qt "github.com/frankban/quicktest"
	"github.com/stokaro/ptah-ydb-plugin/internal/nativeapi"
	native "github.com/stokaro/ptah-ydb-plugin/internal/nativeprotocol"
	"github.com/stokaro/ptah-ydb-plugin/internal/tablecreate"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb_Table"
)

func TestColumnInventoryChecksNativeStateAndDispatch(t *testing.T) {
	removed := tableHead + `"columns":[` + idColumn + `],"columnFamilies":[{"name":"default"}]}`
	added := strings.Replace(beforeTable, `],"columnFamilies"`, `,{"name":"new","type":{"optionalType":{"item":{"typeId":"UTF8"}}},"notNull":false}],"columnFamilies"`, 1)
	for _, test := range []struct {
		name          string
		add           bool
		before, after string
		guardErr      bool
		dispatch      bool
		wantErr       bool
		alterFailure  bool
	}{
		{name: "add", add: true, before: beforeTable, after: added, dispatch: true},
		{name: "drop", before: beforeTable, after: removed, dispatch: true},
		{name: "stale native setting", before: strings.Replace(beforeTable, `"name":"payload"`, `"name":"payload","family":"cold"`, 1), after: removed, wantErr: true},
		{name: "stale type", before: strings.Replace(beforeTable, `"typeId":"UTF8"`, `"typeId":"STRING"`, 1), after: removed, wantErr: true},
		{name: "key dependency", before: strings.Replace(beforeTable, `"primaryKey":["id"]`, `"primaryKey":["id","payload"]`, 1), after: removed, wantErr: true},
		{name: "guard refusal", before: beforeTable, after: removed, guardErr: true, wantErr: true},
		{name: "dispatch uncertain", before: beforeTable, after: removed, alterFailure: true, dispatch: true, wantErr: true},
		{name: "not confirmed", before: beforeTable, after: beforeTable, dispatch: true, wantErr: true},
		{name: "neighbor default changed", before: beforeTable, after: strings.Replace(removed, `"name":"id"`, `"name":"id","fromLiteral":{"type":{"typeId":"UINT64"},"value":{"uint64Value":"9"}}`, 1), dispatch: true, wantErr: true},
		{name: "table attributes changed", before: beforeTable, after: strings.Replace(removed, `"columns":`, `"attributes":{"custom":"changed"},"columns":`, 1), dispatch: true, wantErr: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := qt.New(t)
			change := &tablecreate.ColumnInventory{Drop: &native.ColumnMeta{Name: "payload", Type: &Ydb.Type{Type: &Ydb.Type_TypeId{TypeId: Ydb.Type_UTF8}}}}
			if test.add {
				change.Drop = nil
				change.Add = &Ydb_Table.ColumnMeta{Name: "new", Type: &Ydb.Type{Type: &Ydb.Type_OptionalType{OptionalType: &Ydb.OptionalType{Item: &Ydb.Type{Type: &Ydb.Type_TypeId{TypeId: Ydb.Type_UTF8}}}}}, NotNull: new(false)}
			}
			operation, err := tablecreate.CaptureInventory("inventory", &protocol.Reference{Engine: "ydb", Kind: "table", Database: "/local", Schema: "app", Name: "records"}, change)
			c.Assert(err, qt.IsNil)
			fixture := &alterFixture{before: test.before, after: test.after, alterFailure: test.alterFailure}
			api := &nativeapi.ColumnInventory{Open: func(context.Context) (nativeapi.AlterationSession, error) { return fixture, nil }}
			dispatched, err := api.Apply(c.Context(), "/local", operation.Value, nil, func(context.Context) error {
				if test.guardErr {
					return errors.New("guard refusal")
				}
				return nil
			})
			c.Assert(dispatched, qt.Equals, test.dispatch)
			if test.wantErr {
				c.Assert(err, qt.IsNotNil)
			} else {
				c.Assert(err, qt.IsNil)
			}
			c.Assert(fixture.sent != nil, qt.Equals, test.dispatch)
			c.Assert(fixture.events[len(fixture.events)-1], qt.Equals, "close")
		})
	}
}

func TestDefaultAdditionUsesOneQueryAndConfirmsEveryColumn(t *testing.T) {
	const addition = `{"name":"new","type":{"typeId":"UTF8"},"fromLiteral":{"type":{"typeId":"UTF8"},"value":{"textValue":""}}}`
	added := strings.Replace(beforeTable, `],"columnFamilies"`, `,`+addition+`],"columnFamilies"`, 1)
	for _, test := range []struct {
		name, before, after                                          string
		caps, missingExecutor, guardErr, queryErr, dispatch, wantErr bool
	}{
		{name: "confirmed empty default", before: beforeTable, after: added, caps: true, dispatch: true},
		{name: "capability absent", before: beforeTable, after: added, wantErr: true},
		{name: "executor absent", before: beforeTable, after: added, caps: true, missingExecutor: true, wantErr: true},
		{name: "column exists", before: added, after: added, caps: true, wantErr: true},
		{name: "lock lost", before: beforeTable, after: added, caps: true, guardErr: true, wantErr: true},
		{name: "query outcome unknown", before: beforeTable, after: added, caps: true, queryErr: true, dispatch: true, wantErr: true},
		{name: "column absent after query", before: beforeTable, after: beforeTable, caps: true, dispatch: true, wantErr: true},
		{name: "default changed", before: beforeTable, after: strings.Replace(added, `"textValue":""`, `"textValue":"wrong"`, 1), caps: true, dispatch: true, wantErr: true},
		{name: "neighbor changed", before: beforeTable, after: strings.Replace(added, `"name":"payload"`, `"name":"payload","family":"cold"`, 1), caps: true, dispatch: true, wantErr: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := qt.New(t)
			kind := &Ydb.Type{Type: &Ydb.Type_TypeId{TypeId: Ydb.Type_UTF8}}
			change := &tablecreate.ColumnInventory{Add: &Ydb_Table.ColumnMeta{Name: "new", Type: kind, NotNull: new(true), DefaultValue: &Ydb_Table.ColumnMeta_FromLiteral{FromLiteral: &Ydb.TypedValue{Type: kind, Value: &Ydb.Value{Value: &Ydb.Value_TextValue{TextValue: ""}}}}}}
			operation, err := tablecreate.CaptureInventory("add", &protocol.Reference{Engine: "ydb", Kind: "table", Database: "/local", Schema: "app", Name: "records"}, change)
			c.Assert(err, qt.IsNil)
			fixture := &alterFixture{before: test.before, after: test.after}
			var statements []string
			api := &nativeapi.ColumnInventory{Open: func(context.Context) (nativeapi.AlterationSession, error) {
				fixture.events = append(fixture.events, "open")
				return fixture, nil
			}, Execute: func(_ context.Context, text string) error {
				statements = append(statements, text)
				if test.queryErr {
					return errors.New("lost query response")
				}
				return nil
			}}
			if test.missingExecutor {
				api.Execute = nil
			}
			dispatched, err := api.Apply(c.Context(), "/local", operation.Value, map[string]bool{"add_column_with_default": test.caps}, func(context.Context) error {
				if test.guardErr {
					return errors.New("lease lost")
				}
				return nil
			})
			c.Assert(dispatched, qt.Equals, test.dispatch)
			if test.wantErr {
				c.Assert(err, qt.IsNotNil)
			} else {
				c.Assert(err, qt.IsNil)
			}
			c.Assert(fixture.sent, qt.IsNil)
			if test.dispatch {
				c.Assert(statements, qt.DeepEquals, []string{"ALTER TABLE `/local/app/records` ADD COLUMN `new` Utf8 NOT NULL DEFAULT ''u;"})
			} else {
				c.Assert(statements, qt.HasLen, 0)
			}
			if len(fixture.events) > 0 {
				c.Assert(fixture.events[len(fixture.events)-1], qt.Equals, "close")
			}
		})
	}
}
