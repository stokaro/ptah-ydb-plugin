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
			dispatched, err := api.Apply(c.Context(), "/local", operation.Value, func(context.Context) error {
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
