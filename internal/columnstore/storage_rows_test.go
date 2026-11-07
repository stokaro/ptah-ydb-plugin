package columnstore_test

import (
	"strings"
	"testing"

	qt "github.com/frankban/quicktest"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb"

	"github.com/stokaro/ptah-ydb-plugin/internal/columnstore"
	native "github.com/stokaro/ptah-ydb-plugin/internal/nativeprotocol"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
)

// inlineTable is the table service's description of the inline index
// fixture.
func inlineTable() *native.ColumnTableDescribeResult {
	return table("ptah_inline_probe", column("id", Ydb.Type_UINT64), nullableColumn("body", Ydb.Type_UTF8))
}

// A field of the storage configuration the decoder does not know is
// refused. Ported from the host's TestDecode_UnknownStorageField.
func TestDecodeRefusesAnUnknownStorageField(t *testing.T) {
	c := testcheck.New(t)
	c.Assert(string(indexDescription), qt.Contains, `"StorageConfig": {`)
	body := strings.Replace(string(indexDescription), `"StorageConfig": {`, `"StorageConfig": {"FutureStorage": true,`, 1)

	result, err := columnstore.Decode([]byte(body), "/local/col_index_probe", indexTable())

	c.Assert(err, qt.ErrorMatches, "invalid or unsupported YDB column description")
	c.Assert(result, qt.IsNil)
}

// The local indexes a CREATE TABLE declared inline read with their names,
// columns and settings: a Bloom filter at 0.01, and a case-sensitive n-gram
// filter whose size and probability the description leaves to YDB's
// defaults, which the provider leaves unset where the host filled them in.
// Ported from the host's TestDecode_InlineLocalIndexes.
func TestDecodeReadsInlineLocalIndexes(t *testing.T) {
	c := testcheck.New(t)

	result, err := columnstore.Decode(inlineDescription, "/local/ptah_inline_probe", inlineTable())

	c.Assert(err, qt.IsNil)
	c.Assert(result.LocalIndexes, qt.HasLen, 2)
	bloom, ngram := result.LocalIndexes[0], result.LocalIndexes[1]
	c.Assert([]any{bloom.Name, bloom.Columns, bloom.GetBloom().GetFalsePositiveProbability()}, qt.DeepEquals, []any{"bf", []string{"body"}, 0.01})
	c.Assert([]any{ngram.Name, ngram.Columns, ngram.GetNgram().GetCaseSensitive()}, qt.DeepEquals, []any{"ng", []string{"body"}, true})
	c.Assert(ngram.GetNgram().NgramSize, qt.IsNil)
	c.Assert(ngram.GetNgram().FalsePositiveProbability, qt.IsNil)
}

// A Bloom filter on a storage other than the default one, a named one, none,
// or the local metadata, is refused. Ported from the host's
// TestDecode_LocalIndexStorageRefusals.
func TestDecodeRefusesALocalIndexOnAnotherStorage(t *testing.T) {
	for _, storage := range []string{"custom", "", "__LOCAL_METADATA"} {
		t.Run(storage, func(t *testing.T) {
			c := testcheck.New(t)
			body := strings.Replace(string(inlineDescription), `"StorageId": "__DEFAULT"`, `"StorageId": "`+storage+`"`, 1)

			result, err := columnstore.Decode([]byte(body), "/local/ptah_inline_probe", inlineTable())

			c.Assert(err, qt.ErrorMatches, "unsupported YDB bloom storage")
			c.Assert(result, qt.IsNil)
		})
	}
}
