package nativeapi_test

import (
	"context"
	"errors"
	"testing"

	qt "github.com/frankban/quicktest"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb_Operations"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb_Scheme"

	"github.com/stokaro/ptah-ydb-plugin/internal/nativeapi"
	native "github.com/stokaro/ptah-ydb-plugin/internal/nativeprotocol"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
	"github.com/stokaro/ptah-ydb-plugin/internal/ydbcomment"
)

// A comment statement runs on a table strictly below the directory the
// session is confined to, the database or a dev realm under it: the table
// at the absolute path the statement names is the one described and
// written. Ported from the host's TestWithin.
func TestRunStatement_RunsBelowTheRoot(t *testing.T) {
	for _, tc := range []struct {
		name, text, root, absolute string
	}{
		{name: "below the database", text: "COMMENT ON TABLE `/local/a/t` IS 'rows'", root: "/local", absolute: "/local/a/t"},
		{name: "below a realm", text: "COMMENT ON TABLE `/local/.ptah/r/t` IS 'rows'", root: "/local/.ptah/r", absolute: "/local/.ptah/r/t"},
		{name: "relative to a realm", text: "COMMENT ON TABLE `t` IS 'rows'", root: "/local/.ptah/r", absolute: "/local/.ptah/r/t"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := testcheck.New(t)
			query, recognized, err := ydbcomment.Recognize(tc.text)
			c.Assert(err, qt.IsNil)
			c.Assert(recognized, qt.IsTrue)
			session := &attributeSession{alter: &Ydb_Operations.Operation{Ready: true, Status: Ydb.StatusIds_SUCCESS}, described: &native.DescribeTableResult{}}
			comments := &nativeapi.Comments{
				Scheme: &describedPaths{entries: map[string]*Ydb_Scheme.Entry{tc.absolute: {Name: "t", Type: Ydb_Scheme.Entry_TABLE}}, missing: Ydb.StatusIds_SCHEME_ERROR},
				Open:   func(context.Context) (nativeapi.AttributeSession, error) { return session, nil },
			}

			dispatched, err := comments.RunStatement(c.Context(), "/local", tc.root, query, func(context.Context) error { return nil })

			c.Assert(err, qt.IsNil)
			c.Assert(dispatched, qt.IsTrue)
		})
	}
}

// A statement naming the root itself, a sibling that shares the root's
// prefix, a path that climbs out of the root, or a path outside the realm
// the session is confined to is refused before anything is described or
// written. Ported from the host's TestWithin.
func TestRunStatement_RefusesAPathOutsideTheRoot(t *testing.T) {
	for _, tc := range []struct {
		name, text, root, want string
	}{
		{name: "the root", text: "COMMENT ON TABLE `/local` IS 'rows'", root: "/local",
			want: `invalid comment statement: /local is not in /local`},
		{name: "a sibling with the same prefix", text: "COMMENT ON TABLE `/localx/t` IS 'rows'", root: "/local",
			want: `invalid comment statement: /localx/t is not in /local`},
		{name: "climbing out", text: "COMMENT ON TABLE `/local/r/../../etc/t` IS 'rows'", root: "/local/r",
			want: `invalid comment statement: /etc/t is not in /local/r`},
		{name: "outside a realm", text: "COMMENT ON TABLE `/local/t` IS 'rows'", root: "/local/.ptah/r",
			want: `invalid comment statement: /local/t is not in /local/.ptah/r`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := testcheck.New(t)
			query, recognized, err := ydbcomment.Recognize(tc.text)
			c.Assert(err, qt.IsNil)
			c.Assert(recognized, qt.IsTrue)
			comments := &nativeapi.Comments{Scheme: &describedPaths{missing: Ydb.StatusIds_SCHEME_ERROR}, Open: unopened(c)}

			dispatched, err := comments.RunStatement(c.Context(), "/local", tc.root, query, func(context.Context) error { return nil })

			c.Assert(dispatched, qt.IsFalse)
			c.Assert(err, qt.ErrorMatches, tc.want)
			_, isRefusal := errors.AsType[*nativeapi.Refusal](err)
			c.Assert(isRefusal, qt.IsTrue)
		})
	}
}
