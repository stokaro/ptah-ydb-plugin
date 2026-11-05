package sourceencode_test

import (
	"testing"

	qt "github.com/frankban/quicktest"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"

	"github.com/stokaro/ptah-ydb-plugin/internal/sourceencode"
	"github.com/stokaro/ptah-ydb-plugin/internal/tablesource"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
	"github.com/stokaro/ptah-ydb-plugin/schema"
)

// overrides are the overrides an HCL table part writes, as key=value.
func overrides(parts []*protocol.EncodedSourcePart) []string {
	written := make([]string, 0)
	for _, part := range parts {
		for _, entry := range part.GetFragment().GetValue().GetMapping().GetEntries() {
			written = append(written, entry.GetKey()+"="+entry.GetValue().GetText())
		}
	}
	return written
}

// losses are the loss lines of a result.
func losses(result sourceencode.TableResult) []string {
	lines := make([]string, 0)
	for _, loss := range result.Losses {
		lines = append(lines, loss.GetValueId()+": "+loss.GetMessage())
	}
	return lines
}

// readDefaults are the settings a read reports for a table YDB created with
// its documented defaults, and the default family it reports for every row
// table.
const readDefaults = `"columnFamilies":[{"name":"default","compression":"COMPRESSION_NONE"}],` +
	`"partitioningSettings":{"partitioningBySize":"ENABLED","partitionSizeMb":"2048","partitioningByLoad":"DISABLED","minPartitionsCount":"1"},"keyBloomFilter":"DISABLED"`

// TestEncodeTableHCL_HappyPath writes a TTL as the overrides of the table's
// platform "ydb" block, in the built-in engine's spelling, and reports what the
// block cannot say beside it in the built-in engine's words: the settings a
// read reports for every table are no loss. A table that holds neither is
// written by a part with no override. The part reads back as the same TTL.
func TestEncodeTableHCL_HappyPath(t *testing.T) {
	for _, test := range []struct {
		name      string
		kind      string
		payload   string
		overrides []string
		losses    []string
		row       string
	}{
		{"a row table's TTL beside the defaults a read reports", schema.TableProperties,
			`{"primaryKey":["id"],` + readDefaults + `,"ttlSettings":{"dateTypeColumn":{"columnName":"ts","expireAfterSeconds":2592000}}}`,
			[]string{"row_deletion_column=ts", "row_deletion_interval=P30D"}, make([]string, 0),
			`{"ttlSettings":{"dateTypeColumn":{"columnName":"ts","expireAfterSeconds":2592000}}}`},
		{"an epoch TTL", schema.TableProperties,
			`{"ttlSettings":{"valueSinceUnixEpoch":{"columnName":"at","columnUnit":"UNIT_SECONDS","expireAfterSeconds":3600}}}`,
			[]string{"row_deletion_column=at", "row_deletion_interval=PT1H", "row_deletion_unit=SECONDS"}, make([]string, 0),
			`{"ttlSettings":{"valueSinceUnixEpoch":{"columnName":"at","columnUnit":"UNIT_SECONDS","expireAfterSeconds":3600}}}`},
		{"a TTL beside a setting HCL cannot say", schema.TableProperties,
			`{"columnFamilies":[{"name":"default","compression":"COMPRESSION_NONE"},{"name":"cold","compression":"COMPRESSION_LZ4"}],` +
				`"partitioningSettings":{"partitioningBySize":"ENABLED","partitionSizeMb":"2048","partitioningByLoad":"DISABLED","minPartitionsCount":"3"},` +
				`"ttlSettings":{"dateTypeColumn":{"columnName":"ts","expireAfterSeconds":3600}}}`,
			[]string{"row_deletion_column=ts", "row_deletion_interval=PT1H"},
			[]string{"value: column family cold is not represented in HCL; YDB table settings (AUTO_PARTITIONING_MIN_PARTITIONS_COUNT = 3) are not represented in HCL"},
			`{"ttlSettings":{"dateTypeColumn":{"columnName":"ts","expireAfterSeconds":3600}}}`},
		{"a column table's TTL that only deletes", schema.ColumnStorageProperties,
			`{"hashColumns":["id"],"ttl":{"enabled":{"column":"ts","tiers":[{"applyAfterSeconds":"86400","delete":{}}]}}}`,
			[]string{"row_deletion_column=ts", "row_deletion_interval=P1D"}, []string{"value: YDB column storage, hash partitioning and tiered TTL are not represented in HCL"},
			`{"ttlSettings":{"dateTypeColumn":{"columnName":"ts","expireAfterSeconds":86400}}}`},
		{"the defaults a read reports alone", schema.TableProperties, `{"primaryKey":["id"],` + readDefaults + `}`, make([]string, 0), make([]string, 0), ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)

			result, err := sourceencode.EncodeTableHCL(table(c, facet{"value", test.kind, "", test.payload}))

			c.Assert(err, qt.IsNil)
			c.Assert(result.Written, qt.DeepEquals, []string{"value"})
			c.Assert(result.Parts, qt.HasLen, 1)
			c.Assert(result.Parts[0].GetFragment().GetHclBlock().GetSite(), qt.Equals, protocol.HCLSourceBlock_TABLE_PLATFORM)
			c.Assert(result.Parts[0].GetFragment().GetBindingId(), qt.Equals, binding.Id)
			c.Assert(overrides(result.Parts), qt.DeepEquals, test.overrides)
			c.Assert(losses(result), qt.DeepEquals, test.losses)
			c.Assert(decode(c, protocol.SourceDocument_HCL, result.Parts).row, qt.Equals, canonical(c, schema.TableProperties, test.row))
		})
	}
}

// TestEncodeTableHCL_LosesWhatHasNoTTL loses a facet that holds a setting HCL
// cannot say and no TTL it can, and one whose TTL it cannot say: nothing
// writes it.
func TestEncodeTableHCL_LosesWhatHasNoTTL(t *testing.T) {
	for _, test := range []struct {
		name    string
		kind    string
		payload string
		loss    string
	}{
		{"settings without a TTL", schema.TableProperties, `{"partitioningSettings":{"partitioningByLoad":"ENABLED"},"uniformPartitions":"4"}`,
			"value: YDB table settings (AUTO_PARTITIONING_BY_LOAD = ENABLED, UNIFORM_PARTITIONS = 4) are not represented in HCL"},
		{"split points", schema.TableProperties, `{"partitionAtKeys":[{"values":["10"]},{"values":["it's","a,b"]}]}`,
			`value: YDB table settings (PARTITION_AT_KEYS = ('10', ('it\'s', 'a,b'))) are not represented in HCL`},
		{"a TTL with a run interval", schema.TableProperties,
			`{"ttlSettings":{"dateTypeColumn":{"columnName":"ts","expireAfterSeconds":3600},"runIntervalSeconds":60}}`,
			"value: table app/events: its TTL run interval has no source spelling"},
		{"a tiered column TTL", schema.ColumnStorageProperties,
			`{"ttl":{"enabled":{"column":"at","tiers":[{"applyAfterSeconds":"3600","externalStorage":"/local/app/cold"},{"applyAfterSeconds":"86400","delete":{}}]}}}`,
			"value: YDB column storage, hash partitioning and tiered TTL are not represented in HCL"},
		{"a facet HCL has no place for", schema.ChangefeedProperties, `{}`, "value: table changefeeds of table app/events has no HCL spelling"},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)

			result, err := sourceencode.EncodeTableHCL(table(c, facet{"value", test.kind, "", test.payload}))

			c.Assert(err, qt.IsNil)
			c.Assert(result.Written, qt.HasLen, 0)
			c.Assert(result.Parts, qt.HasLen, 0)
			c.Assert(losses(result), qt.DeepEquals, []string{test.loss})
		})
	}
}

// The part an HCL export writes carries the provider's platform claim, which
// the table decoders own.
func TestEncodeTableHCL_WritesTheClaim(t *testing.T) {
	c := testcheck.New(t)

	result, err := sourceencode.EncodeTableHCL(table(c, facet{"value", schema.TableProperties, "", `{"ttlSettings":{"dateTypeColumn":{"columnName":"ts","expireAfterSeconds":60}}}`}))

	c.Assert(err, qt.IsNil)
	c.Assert(tablesource.Owns(result.Parts[0].GetFragment()), qt.IsTrue)
	c.Assert(result.Parts[0].GetFragment().GetSelector(), qt.Equals, tablesource.HCLPlatformBlock().GetSelector())
}
