package sourceencode_test

import (
	"testing"

	qt "github.com/frankban/quicktest"

	"github.com/stokaro/ptah-ydb-plugin/internal/sourceencode"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
	"github.com/stokaro/ptah-ydb-plugin/schema"
)

// An HCL export names the column families it leaves out: two in one plural
// line, and a default family a profile compressed, which is not the default
// family as YDB gives it. Ported from the host's
// TestRender_ReportsTheColumnFamiliesItLeavesOut.
func TestEncodeTableHCL_NamesTheFamiliesItLeavesOut(t *testing.T) {
	const ttl = `"ttlSettings":{"dateTypeColumn":{"columnName":"ts","expireAfterSeconds":3600}}`
	for _, test := range []struct {
		name, payload string
		losses        []string
	}{
		{"two families", `{"columnFamilies":[{"name":"cold","compression":"COMPRESSION_LZ4"},{"name":"default","data":{"media":"ssd"}}],` + ttl + `}`,
			[]string{"value: column families cold, default are not represented in HCL"}},
		{"the default family a profile compressed", `{"columnFamilies":[{"name":"default","compression":"COMPRESSION_LZ4"}],` + ttl + `}`,
			[]string{"value: column family default is not represented in HCL"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)

			result, err := sourceencode.EncodeTableHCL(table(c, facet{"value", schema.TableProperties, "", test.payload}))

			c.Assert(err, qt.IsNil)
			c.Assert(losses(result), qt.DeepEquals, test.losses)
		})
	}
}
