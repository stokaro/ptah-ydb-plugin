package ydbdecl_test

import (
	"testing"

	qt "github.com/frankban/quicktest"

	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
	"github.com/stokaro/ptah-ydb-plugin/internal/ydbdecl"
)

// A global index has the common spelling inspection and table preparation
// both use: none for a synchronous one and async for an asynchronous one.
// Every other kind is no common index.
func TestIndexKindCommonMethod(t *testing.T) {
	for _, row := range []struct {
		name   string
		kind   ydbdecl.IndexKind
		method string
		common bool
	}{
		{"synchronous", ydbdecl.IndexSync, "", true},
		{"asynchronous", ydbdecl.IndexAsync, "async", true},
		{"vector", ydbdecl.IndexVector, "", false},
		{"full-text", ydbdecl.IndexFullTextPlain, "", false},
		{"local", ydbdecl.IndexLocalBloom, "", false},
		{"unknown", ydbdecl.IndexKind(0), "", false},
	} {
		t.Run(row.name, func(t *testing.T) {
			c := testcheck.New(t)
			method, common := row.kind.CommonMethod()
			c.Assert(method, qt.Equals, row.method)
			c.Assert(common, qt.Equals, row.common)
		})
	}
}

// The built-in engine's spellings of a global index read as the kinds whose
// common spelling inspection reports.
func TestIndexKindOfReadsTheCommonKinds(t *testing.T) {
	for _, row := range []struct {
		declared string
		method   string
	}{
		{"", ""}, {"BTREE", ""}, {"GLOBAL", ""}, {"SYNC", ""}, {"GLOBAL SYNC", ""}, {"global_sync", ""},
		{"ASYNC", "async"}, {"GLOBAL ASYNC", "async"}, {"async", "async"},
	} {
		t.Run(row.declared, func(t *testing.T) {
			c := testcheck.New(t)
			kind, err := ydbdecl.IndexKindOf(row.declared)
			c.Assert(err, qt.IsNil)
			method, common := kind.CommonMethod()
			c.Assert(common, qt.IsTrue)
			c.Assert(method, qt.Equals, row.method)
		})
	}
}
