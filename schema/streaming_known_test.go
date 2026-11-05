package schema_test

import (
	"testing"

	qt "github.com/frankban/quicktest"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb_Scheme"

	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
	"github.com/stokaro/ptah-ydb-plugin/schema"
)

func TestKnownStreamingEntryType(t *testing.T) {
	c := testcheck.New(t)
	c.Assert(schema.Known(&Ydb_Scheme.ListDirectoryResult{Children: []*Ydb_Scheme.Entry{{Name: "copy", Type: 26}}}), qt.IsNil)
	c.Assert(schema.Known(&Ydb_Scheme.ListDirectoryResult{Children: []*Ydb_Scheme.Entry{{Name: "other", Type: 999}}}), qt.ErrorMatches, "unknown enum in Ydb.Scheme.Entry.type")
}
