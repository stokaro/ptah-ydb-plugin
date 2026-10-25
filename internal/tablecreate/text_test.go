package tablecreate_test

import (
	"regexp"
	"testing"

	qt "github.com/frankban/quicktest"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb_Table"
	"google.golang.org/protobuf/encoding/protojson"

	native "github.com/stokaro/ptah-ydb-plugin/internal/nativeprotocol"
	"github.com/stokaro/ptah-ydb-plugin/internal/tablecreate"
)

func creationRequest(c *qt.C, text string) *Ydb_Table.CreateTableRequest {
	c.Helper()
	request := &Ydb_Table.CreateTableRequest{}
	c.Assert(protojson.Unmarshal([]byte(text), request), qt.IsNil)
	return request
}

// heldPartitioning reads the partitioning a table holds, nil for a row that
// states none.
func heldPartitioning(c *qt.C, text string) *Ydb_Table.PartitioningSettings {
	c.Helper()
	if text == "" {
		return nil
	}
	held := &Ydb_Table.PartitioningSettings{}
	c.Assert(protojson.Unmarshal([]byte(text), held), qt.IsNil)
	return held
}

func alterationRequest(c *qt.C, text string) *Ydb_Table.AlterTableRequest {
	c.Helper()
	request := &Ydb_Table.AlterTableRequest{}
	c.Assert(protojson.Unmarshal([]byte(text), request), qt.IsNil)
	return request
}

// The columns every creation case shares: a Uint64 key, a nullable Utf8, a
// NOT NULL Timestamp and a nullable Uint64.
const creationColumns = `"columns":[
	{"name":"id","type":{"typeId":"UINT64"},"notNull":true},
	{"name":"note","type":{"optionalType":{"item":{"typeId":"UTF8"}}},"notNull":false},
	{"name":"expires","type":{"typeId":"TIMESTAMP"},"notNull":true},
	{"name":"hits","type":{"optionalType":{"item":{"typeId":"UINT64"}}},"notNull":false}],
	"primaryKey":["id"]`

// Each row is a native creation request and the statement the host's
// embedded engine writes for the same table.
func TestRowTableStatement_HappyPath(t *testing.T) {
	for _, tc := range []struct {
		name    string
		path    string
		request string
		want    string
	}{
		{
			name:    "columns and key",
			path:    "app/jobs",
			request: `{` + creationColumns + `}`,
			want: "CREATE TABLE `app/jobs` (\n    `id` Uint64 NOT NULL,\n    `note` Utf8,\n    `expires` Timestamp NOT NULL,\n" +
				"    `hits` Uint64,\n    PRIMARY KEY (`id`)\n);",
		},
		{
			name: "a composite key in its declared order",
			path: "pairs",
			request: `{"columns":[{"name":"a","type":{"typeId":"INT32"},"notNull":true},{"name":"b","type":{"typeId":"UTF8"},"notNull":true}],
				"primaryKey":["b","a"]}`,
			want: "CREATE TABLE `pairs` (\n    `a` Int32 NOT NULL,\n    `b` Utf8 NOT NULL,\n    PRIMARY KEY (`b`, `a`)\n);",
		},
		{
			name: "families sorted by name, the default one only where it states a setting",
			path: "app/jobs",
			request: `{` + creationColumns + `,"columnFamilies":[{"name":"warm","data":{"media":"ssd"}},
				{"name":"archive","compression":"COMPRESSION_LZ4"},{"name":"default","compression":"COMPRESSION_NONE"}]}`,
			want: "CREATE TABLE `app/jobs` (\n    `id` Uint64 NOT NULL,\n    `note` Utf8,\n    `expires` Timestamp NOT NULL,\n" +
				"    `hits` Uint64,\n    PRIMARY KEY (`id`),\n    FAMILY `archive` (COMPRESSION = 'lz4'),\n" +
				"    FAMILY `default` (COMPRESSION = 'off'),\n    FAMILY `warm` (DATA = 'ssd')\n);",
		},
		{
			name: "a column placed in a family names it after its type",
			path: "app/jobs",
			request: `{"columns":[{"name":"id","type":{"typeId":"UINT64"},"notNull":true},
				{"name":"note","type":{"optionalType":{"item":{"typeId":"UTF8"}}},"family":"archive"},
				{"name":"other","type":{"optionalType":{"item":{"typeId":"UTF8"}}},"family":"default"}],
				"primaryKey":["id"],"columnFamilies":[{"name":"archive","compression":"COMPRESSION_LZ4"},{"name":"default"}]}`,
			want: "CREATE TABLE `app/jobs` (\n    `id` Uint64 NOT NULL,\n    `note` Utf8 FAMILY `archive`,\n    `other` Utf8,\n" +
				"    PRIMARY KEY (`id`),\n    FAMILY `archive` (COMPRESSION = 'lz4')\n);",
		},
		{
			name:    "a date TTL",
			path:    "app/jobs",
			request: `{` + creationColumns + `,"ttlSettings":{"dateTypeColumn":{"columnName":"expires","expireAfterSeconds":93600}}}`,
			want: "CREATE TABLE `app/jobs` (\n    `id` Uint64 NOT NULL,\n    `note` Utf8,\n    `expires` Timestamp NOT NULL,\n" +
				"    `hits` Uint64,\n    PRIMARY KEY (`id`)\n) WITH (TTL = Interval(\"P1DT2H\") ON `expires`);",
		},
		{
			name: "an epoch TTL keeping YDB's own run interval",
			path: "app/jobs",
			request: `{` + creationColumns + `,"ttlSettings":{"valueSinceUnixEpoch":{"columnName":"hits","columnUnit":"UNIT_MILLISECONDS",
				"expireAfterSeconds":3600},"runIntervalSeconds":3600}}`,
			want: "CREATE TABLE `app/jobs` (\n    `id` Uint64 NOT NULL,\n    `note` Utf8,\n    `expires` Timestamp NOT NULL,\n" +
				"    `hits` Uint64,\n    PRIMARY KEY (`id`)\n) WITH (TTL = Interval(\"PT1H\") ON `hits` AS MILLISECONDS);",
		},
		{
			name: "every setting in the order the embedded engine writes them",
			path: "app/jobs",
			request: `{` + creationColumns + `,"ttlSettings":{"dateTypeColumn":{"columnName":"expires","expireAfterSeconds":60}},
				"partitioningSettings":{"partitioningBySize":"ENABLED","partitionSizeMb":"2048","partitioningByLoad":"DISABLED",
				"minPartitionsCount":"2","maxPartitionsCount":"8"},"readReplicasSettings":{"perAzReadReplicasCount":"1"},
				"keyBloomFilter":"ENABLED","uniformPartitions":"4"}`,
			want: "CREATE TABLE `app/jobs` (\n    `id` Uint64 NOT NULL,\n    `note` Utf8,\n    `expires` Timestamp NOT NULL,\n" +
				"    `hits` Uint64,\n    PRIMARY KEY (`id`)\n) WITH (TTL = Interval(\"PT1M\") ON `expires`, AUTO_PARTITIONING_BY_SIZE = ENABLED, " +
				"AUTO_PARTITIONING_PARTITION_SIZE_MB = 2048, AUTO_PARTITIONING_BY_LOAD = DISABLED, AUTO_PARTITIONING_MIN_PARTITIONS_COUNT = 2, " +
				"AUTO_PARTITIONING_MAX_PARTITIONS_COUNT = 8, READ_REPLICAS_SETTINGS = \"PER_AZ:1\", KEY_BLOOM_FILTER = ENABLED, UNIFORM_PARTITIONS = 4);",
		},
		{
			name:    "replicas in any zone",
			path:    "t",
			request: `{"columns":[{"name":"id","type":{"typeId":"UINT64"},"notNull":true}],"primaryKey":["id"],"readReplicasSettings":{"anyAzReadReplicasCount":"2"}}`,
			want:    "CREATE TABLE `t` (\n    `id` Uint64 NOT NULL,\n    PRIMARY KEY (`id`)\n) WITH (READ_REPLICAS_SETTINGS = \"ANY_AZ:2\");",
		},
		{
			name: "split points over integer and text key columns",
			path: "t",
			request: `{"columns":[{"name":"id","type":{"typeId":"UINT64"},"notNull":true},{"name":"name","type":{"typeId":"UTF8"},"notNull":true}],
				"primaryKey":["id","name"],"partitionAtKeys":{"splitPoints":[
				{"type":{"tupleType":{"elements":[{"optionalType":{"item":{"typeId":"UINT64"}}}]}},"value":{"items":[{"nestedValue":{"uint64Value":"10"}}]}},
				{"type":{"tupleType":{"elements":[{"optionalType":{"item":{"typeId":"UINT64"}}},{"optionalType":{"item":{"typeId":"UTF8"}}}]}},
				"value":{"items":[{"nestedValue":{"uint64Value":"20"}},{"nestedValue":{"textValue":"it's"}}]}}]}}`,
			want: "CREATE TABLE `t` (\n    `id` Uint64 NOT NULL,\n    `name` Utf8 NOT NULL,\n    PRIMARY KEY (`id`, `name`)\n) " +
				"WITH (PARTITION_AT_KEYS = ((10), (20, 'it\\'s')));",
		},
		{
			name: "defaults in their own type",
			path: "t",
			request: `{"columns":[{"name":"id","type":{"typeId":"UINT64"},"notNull":true},
				{"name":"hits","type":{"typeId":"UINT64"},"notNull":true,"fromLiteral":{"type":{"typeId":"UINT64"},"value":{"uint64Value":"7"}}},
				{"name":"small","type":{"typeId":"INT8"},"notNull":true,"fromLiteral":{"type":{"typeId":"INT8"},"value":{"int32Value":-5}}},
				{"name":"label","type":{"typeId":"UTF8"},"notNull":true,"fromLiteral":{"type":{"typeId":"UTF8"},"value":{"textValue":"new"}}},
				{"name":"flag","type":{"typeId":"BOOL"},"notNull":true,"fromLiteral":{"type":{"typeId":"BOOL"},"value":{"boolValue":true}}},
				{"name":"day","type":{"typeId":"DATE"},"notNull":true,"fromLiteral":{"type":{"typeId":"DATE"},"value":{"uint32Value":20455}}},
				{"name":"price","type":{"decimalType":{"precision":10,"scale":2}},"notNull":true,
				"fromLiteral":{"type":{"decimalType":{"precision":10,"scale":2}},"value":{"low128":"150"}}}],"primaryKey":["id"]}`,
			want: "CREATE TABLE `t` (\n    `id` Uint64 NOT NULL,\n    `hits` Uint64 NOT NULL DEFAULT 7ul,\n    `small` Int8 NOT NULL DEFAULT -5t,\n" +
				"    `label` Utf8 NOT NULL DEFAULT 'new'u,\n    `flag` Bool NOT NULL DEFAULT true,\n    `day` Date NOT NULL DEFAULT Date('2026-01-02'),\n" +
				"    `price` Decimal(10,2) NOT NULL DEFAULT Decimal('1.5', 10, 2),\n    PRIMARY KEY (`id`)\n);",
		},
		{
			name:    "a name that needs escaping",
			path:    "dir/we`ird",
			request: `{"columns":[{"name":"a\\b","type":{"typeId":"UINT64"},"notNull":true}],"primaryKey":["a\\b"]}`,
			want:    "CREATE TABLE `dir/we\\`ird` (\n    `a\\\\b` Uint64 NOT NULL,\n    PRIMARY KEY (`a\\\\b`)\n);",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := qt.New(t)
			got, err := tablecreate.RowTableStatement(tc.path, creationRequest(c, tc.request))
			c.Assert(err, qt.IsNil)
			c.Assert(got, qt.Equals, tc.want)
		})
	}
}

// Each row is a creation request that sets something YQL has no spelling
// for, and the words of the refusal.
func TestRowTableStatement_FailurePath(t *testing.T) {
	for _, tc := range []struct {
		name    string
		request string
		want    string
	}{
		{name: "storage settings", request: `{` + creationColumns + `,"storageSettings":{"storeExternalBlobs":"ENABLED"}}`,
			want: "storage settings"},
		{name: "user attributes", request: `{` + creationColumns + `,"attributes":{"purpose":"x"}}`, want: "sets no user attribute"},
		{name: "tiering", request: `{` + creationColumns + `,"tiering":"tier"}`, want: "tiering"},
		{name: "partition columns", request: `{` + creationColumns + `,"partitioningSettings":{"partitionBy":["id"]}}`, want: "partition columns"},
		{name: "a run interval of its own",
			request: `{` + creationColumns + `,"ttlSettings":{"dateTypeColumn":{"columnName":"expires","expireAfterSeconds":60},"runIntervalSeconds":60}}`,
			want:    "runs on an interval of its own"},
		{name: "a family kept in memory", request: `{` + creationColumns + `,"columnFamilies":[{"name":"hot","keepInMemory":"ENABLED"}]}`,
			want: `column family "hot" keeps its columns in memory`},
		{name: "a default from a sequence",
			request: `{"columns":[{"name":"id","type":{"typeId":"INT64"},"notNull":true,"fromSequence":{"name":"_serial_column_id"}}],"primaryKey":["id"]}`,
			want:    `column "id" takes its default from a sequence`},
		{name: "a default of a type without a literal",
			request: `{"columns":[{"name":"id","type":{"typeId":"UINT64"},"notNull":true},{"name":"u","type":{"typeId":"UUID"},"notNull":true,
				"fromLiteral":{"type":{"typeId":"UUID"},"value":{"low128":"1","high128":"2"}}}],"primaryKey":["id"]}`,
			want: `the default of column "u" has no YQL literal`},
		{name: "a split point over a type the setting does not take",
			request: `{"columns":[{"name":"d","type":{"typeId":"DATE"},"notNull":true}],"primaryKey":["d"],"partitionAtKeys":{"splitPoints":[
				{"type":{"tupleType":{"elements":[{"optionalType":{"item":{"typeId":"DATE"}}}]}},"value":{"items":[{"nestedValue":{"uint32Value":10}}]}}]}}`,
			want: "PARTITION_AT_KEYS does not take"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := qt.New(t)
			got, err := tablecreate.RowTableStatement("app/jobs", creationRequest(c, tc.request))
			c.Assert(err, qt.ErrorIs, tablecreate.ErrNoTextForm)
			c.Assert(err, qt.ErrorMatches, ".*"+regexp.QuoteMeta(tc.want)+".*")
			c.Assert(got, qt.Equals, "")
		})
	}
}

func TestAlterationStatement_HappyPath(t *testing.T) {
	for _, tc := range []struct {
		name    string
		request string
		held    string
		want    string
	}{
		{
			name: "families: additions, settings and moves, each group by name",
			request: `{"addColumnFamilies":[{"name":"warm","data":{"media":"ssd"}},{"name":"cold","compression":"COMPRESSION_LZ4"}],
				"alterColumnFamilies":[{"name":"default","data":{"media":"hdd"},"compression":"COMPRESSION_NONE"}],
				"alterColumns":[{"name":"zeta","family":"cold"},{"name":"alpha","family":"default"}]}`,
			want: "ALTER TABLE `app/jobs` ADD FAMILY `cold` (COMPRESSION = 'lz4'), ADD FAMILY `warm` (DATA = 'ssd'), " +
				"ALTER FAMILY `default` SET DATA 'hdd', ALTER FAMILY `default` SET COMPRESSION 'off', " +
				"ALTER COLUMN `alpha` SET FAMILY `default`, ALTER COLUMN `zeta` SET FAMILY `cold`;",
		},
		{
			name:    "a family added without settings",
			request: `{"addColumnFamilies":[{"name":"plain"}]}`,
			want:    "ALTER TABLE `app/jobs` ADD FAMILY `plain` ();",
		},
		{
			name: "nullable columns added",
			request: `{"addColumns":[{"name":"note","type":{"optionalType":{"item":{"typeId":"UTF8"}}}},
				{"name":"seen","type":{"optionalType":{"item":{"typeId":"TIMESTAMP"}}}}]}`,
			want: "ALTER TABLE `app/jobs` ADD COLUMN `note` Utf8, ADD COLUMN `seen` Timestamp;",
		},
		{
			name:    "columns dropped",
			request: `{"dropColumns":["note","seen"]}`,
			want:    "ALTER TABLE `app/jobs` DROP COLUMN `note`, DROP COLUMN `seen`;",
		},
		{
			name:    "a TTL set",
			request: `{"setTtlSettings":{"dateTypeColumn":{"columnName":"ts","expireAfterSeconds":86400}}}`,
			want:    "ALTER TABLE `app/jobs` SET (TTL = Interval(\"P1D\") ON `ts`);",
		},
		{
			name:    "a TTL dropped",
			request: `{"dropTtlSettings":{}}`,
			want:    "ALTER TABLE `app/jobs` RESET (TTL);",
		},
		{
			name:    "the partitioning names every setting the table ends with",
			request: `{"alterPartitioningSettings":{"partitioningByLoad":"ENABLED"}}`,
			held:    `{"partitioningBySize":"ENABLED","partitionSizeMb":"2048","partitioningByLoad":"DISABLED","minPartitionsCount":"3","maxPartitionsCount":"9"}`,
			want: "ALTER TABLE `app/jobs` SET (AUTO_PARTITIONING_BY_SIZE = ENABLED, AUTO_PARTITIONING_PARTITION_SIZE_MB = 2048, " +
				"AUTO_PARTITIONING_BY_LOAD = ENABLED, AUTO_PARTITIONING_MIN_PARTITIONS_COUNT = 3, AUTO_PARTITIONING_MAX_PARTITIONS_COUNT = 9);",
		},
		{
			name:    "the partitioning without splitting by size names no size",
			request: `{"alterPartitioningSettings":{"partitioningBySize":"DISABLED","minPartitionsCount":"2"}}`,
			held:    `{"partitioningBySize":"ENABLED","partitionSizeMb":"2048","partitioningByLoad":"DISABLED","minPartitionsCount":"1"}`,
			want: "ALTER TABLE `app/jobs` SET (AUTO_PARTITIONING_BY_SIZE = DISABLED, AUTO_PARTITIONING_BY_LOAD = DISABLED, " +
				"AUTO_PARTITIONING_MIN_PARTITIONS_COUNT = 2);",
		},
		{
			name:    "a sync index with covered columns",
			request: `{"addIndexes":[{"name":"by_ts","indexColumns":["ts","id"],"dataColumns":["payload"],"globalIndex":{}}]}`,
			want:    "ALTER TABLE `app/jobs` ADD INDEX `by_ts` GLOBAL SYNC ON (`ts`, `id`) COVER (`payload`);",
		},
		{
			name:    "an async index",
			request: `{"addIndexes":[{"name":"by_ts","indexColumns":["ts"],"globalAsyncIndex":{}}]}`,
			want:    "ALTER TABLE `app/jobs` ADD INDEX `by_ts` GLOBAL ASYNC ON (`ts`);",
		},
		{
			name:    "a unique index",
			request: `{"addIndexes":[{"name":"by_mail","indexColumns":["mail"],"globalUniqueIndex":{}}]}`,
			want:    "ALTER TABLE `app/jobs` ADD INDEX `by_mail` GLOBAL UNIQUE SYNC ON (`mail`);",
		},
		{
			name:    "an index dropped",
			request: `{"dropIndexes":["by_ts"]}`,
			want:    "ALTER TABLE `app/jobs` DROP INDEX `by_ts`;",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := qt.New(t)
			alteration := &tablecreate.Alteration{Request: alterationRequest(c, tc.request), HeldPartitioning: heldPartitioning(c, tc.held)}
			got, err := tablecreate.AlterationStatement("app/jobs", alteration)
			c.Assert(err, qt.IsNil)
			c.Assert(got, qt.Equals, tc.want)
		})
	}
}

func TestAlterationStatement_FailurePath(t *testing.T) {
	for _, tc := range []struct {
		name    string
		request string
		want    string
	}{
		{name: "nothing", request: `{}`, want: "changes nothing YQL names"},
		{name: "a run interval of its own",
			request: `{"setTtlSettings":{"dateTypeColumn":{"columnName":"ts","expireAfterSeconds":60},"runIntervalSeconds":120}}`,
			want:    "runs on an interval of its own"},
		{name: "keep_in_memory changed", request: `{"alterColumnFamilies":[{"name":"hot","keepInMemory":"DISABLED"}]}`,
			want: `column family "hot" changes keep_in_memory`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := qt.New(t)
			got, err := tablecreate.AlterationStatement("app/jobs", &tablecreate.Alteration{Request: alterationRequest(c, tc.request)})
			c.Assert(err, qt.ErrorIs, tablecreate.ErrNoTextForm)
			c.Assert(err, qt.ErrorMatches, ".*"+regexp.QuoteMeta(tc.want)+".*")
			c.Assert(got, qt.Equals, "")
		})
	}
}

func TestTextPath(t *testing.T) {
	c := qt.New(t)
	c.Assert(tablecreate.TextPath("", "t"), qt.Equals, "t")
	c.Assert(tablecreate.TextPath("app/", "t"), qt.Equals, "app/t")
	c.Assert(tablecreate.TextPath("app/sub", "t"), qt.Equals, "app/sub/t")
}

func TestLiteral_FailurePath(t *testing.T) {
	c := qt.New(t)
	null := &Ydb.TypedValue{Type: &Ydb.Type{Type: &Ydb.Type_OptionalType{OptionalType: &Ydb.OptionalType{Item: &Ydb.Type{Type: &Ydb.Type_TypeId{TypeId: Ydb.Type_UTF8}}}}},
		Value: &Ydb.Value{Value: &Ydb.Value_NullFlagValue{}}}
	got, err := tablecreate.Literal(null)
	c.Assert(err, qt.ErrorMatches, "a NULL default has no literal")
	c.Assert(got, qt.Equals, "")
}

// The held index of a removal travels for the confirmation only; the text
// names the index alone.
func TestAlterationStatementDropsAnIndexByName(t *testing.T) {
	c := qt.New(t)
	alteration := &tablecreate.Alteration{Request: alterationRequest(c, `{"dropIndexes":["by_ts"]}`),
		HeldIndexes: []*native.TableIndexDescription{{Name: "by_ts", IndexColumns: []string{"ts"}}}}
	got, err := tablecreate.AlterationStatement("t", alteration)
	c.Assert(err, qt.IsNil)
	c.Assert(got, qt.Equals, "ALTER TABLE `t` DROP INDEX `by_ts`;")
}
