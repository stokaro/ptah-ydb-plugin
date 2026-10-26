package readproof_test

import (
	"context"
	"errors"
	"testing"

	qt "github.com/frankban/quicktest"
	"github.com/stokaro/ptah-ydb-plugin/internal/readproof"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb_Scheme"
)

type catalog struct {
	kinds map[string]Ydb_Scheme.Entry_Type
	views map[string]string
}

func (catalog) Database() string { return "/local" }
func (c catalog) EntryType(_ context.Context, name string) (Ydb_Scheme.Entry_Type, error) {
	kind, ok := c.kinds[name]
	if !ok {
		return 0, errors.New("not found")
	}
	return kind, nil
}
func (c catalog) ViewQuery(_ context.Context, name string) (string, error) {
	query, ok := c.views[name]
	if !ok {
		return "", errors.New("view cannot be read")
	}
	return query, nil
}

func TestProveLocalReads(t *testing.T) {
	for _, tc := range []struct {
		name, query, view string
		kind              Ydb_Scheme.Entry_Type
		fails             bool
	}{
		{name: "local table", query: "SELECT * FROM t", kind: Ydb_Scheme.Entry_TABLE},
		{name: "local column table", query: "SELECT * FROM t", kind: Ydb_Scheme.Entry_COLUMN_TABLE},
		{name: "local view", query: "SELECT * FROM v", view: "SELECT * FROM t", kind: Ydb_Scheme.Entry_TABLE},
		{name: "view prefix", query: "SELECT * FROM v", view: "PRAGMA TablePathPrefix('/local/app'); SELECT * FROM t", kind: Ydb_Scheme.Entry_TABLE},
		{name: "external table", query: "SELECT * FROM t", kind: Ydb_Scheme.Entry_EXTERNAL_TABLE, fails: true},
		{name: "external view", query: "SELECT * FROM v", view: "SELECT * FROM t", kind: Ydb_Scheme.Entry_EXTERNAL_TABLE, fails: true},
		{name: "unknown source", query: "SELECT * FROM missing", fails: true},
		{name: "another database", query: "SELECT * FROM `/other/t`", kind: Ydb_Scheme.Entry_TABLE, fails: true},
		{name: "view cycle", query: "SELECT * FROM v", view: "SELECT * FROM v", fails: true},
		{name: "file call in view", query: "SELECT * FROM v", view: "SELECT FileContent('x')", fails: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := qt.New(t)
			catalog := catalog{kinds: map[string]Ydb_Scheme.Entry_Type{"/local/app/t": tc.kind, "/local/app/v": Ydb_Scheme.Entry_VIEW, "/local/v": Ydb_Scheme.Entry_VIEW, "/local/t": tc.kind}, views: map[string]string{"/local/app/v": tc.view, "/local/v": tc.view}}
			err := readproof.ProveLocalReads(c.Context(), catalog, "/local/app", tc.query)
			c.Assert(err != nil, qt.Equals, tc.fails)
		})
	}
}
