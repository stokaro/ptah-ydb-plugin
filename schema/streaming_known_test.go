package schema_test

import (
	"testing"

	qt "github.com/frankban/quicktest"
	"github.com/stokaro/ptah-ydb-plugin/schema"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb_Scheme"
)

func TestKnownStreamingEntryType(t *testing.T) {
	c := qt.New(t)
	c.Assert(schema.Known(&Ydb_Scheme.ListDirectoryResult{Children: []*Ydb_Scheme.Entry{{Name: "copy", Type: 26}}}), qt.IsNil)
	c.Assert(schema.Known(&Ydb_Scheme.ListDirectoryResult{Children: []*Ydb_Scheme.Entry{{Name: "other", Type: 999}}}), qt.ErrorMatches, "unknown enum in Ydb.Scheme.Entry.type")
}
