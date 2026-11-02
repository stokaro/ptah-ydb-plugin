package tablecreate_test

import (
	"testing"

	qt "github.com/frankban/quicktest"
	"github.com/stokaro/ptah-ydb-plugin/internal/backend"
	"github.com/stokaro/ptah-ydb-plugin/internal/tablecreate"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

// settingsTable is a table that states the given settings beside its key.
func settingsTable(fields string) string {
	return `{"primaryKey":["id"],"columnFamilies":[{"name":"default"}],` + fields + `}`
}

func TestTableSettingsPlan_HappyPath(t *testing.T) {
	for _, test := range []struct {
		name   string
		in     alterCase
		want   string
		reason string
	}{
		{"turns the key bloom filter on",
			partitioningCase(settingsTable(`"keyBloomFilter":"ENABLED"`), plainTable),
			`{"setKeyBloomFilter":"ENABLED"}`, "Changes the table's read replicas or key bloom filter.*"},
		{"turns the key bloom filter off",
			partitioningCase(settingsTable(`"keyBloomFilter":"DISABLED"`), settingsTable(`"keyBloomFilter":"ENABLED"`)),
			`{"setKeyBloomFilter":"DISABLED"}`, "Changes the table's read replicas or key bloom filter.*"},
		{"sets read replicas on a table that holds none",
			partitioningCase(settingsTable(`"readReplicasSettings":{"anyAzReadReplicasCount":"2"}`), plainTable),
			`{"setReadReplicasSettings":{"anyAzReadReplicasCount":"2"}}`, "Changes the table's read replicas or key bloom filter.*"},
		{"changes the replica count",
			partitioningCase(settingsTable(`"readReplicasSettings":{"perAzReadReplicasCount":"2"}`), settingsTable(`"readReplicasSettings":{"perAzReadReplicasCount":"1"}`)),
			`{"setReadReplicasSettings":{"perAzReadReplicasCount":"2"}}`, "Changes the table's read replicas or key bloom filter.*"},
		{"changes the replica mode",
			partitioningCase(settingsTable(`"readReplicasSettings":{"anyAzReadReplicasCount":"1"}`), settingsTable(`"readReplicasSettings":{"perAzReadReplicasCount":"1"}`)),
			`{"setReadReplicasSettings":{"anyAzReadReplicasCount":"1"}}`, "Changes the table's read replicas or key bloom filter.*"},
		{"removes read replicas declared as PER_AZ:0",
			partitioningCase(settingsTable(`"readReplicasSettings":{"perAzReadReplicasCount":"0"}`), settingsTable(`"readReplicasSettings":{"anyAzReadReplicasCount":"2"}`)),
			`{"setReadReplicasSettings":{"perAzReadReplicasCount":"0"}}`, "Changes the table's read replicas or key bloom filter.*"},
		{"removes read replicas declared as ANY_AZ:0",
			partitioningCase(settingsTable(`"readReplicasSettings":{"anyAzReadReplicasCount":"0"}`), settingsTable(`"readReplicasSettings":{"perAzReadReplicasCount":"1"}`)),
			`{"setReadReplicasSettings":{"perAzReadReplicasCount":"0"}}`, "Changes the table's read replicas or key bloom filter.*"},
		{"changes the replicas and the filter together",
			partitioningCase(settingsTable(`"keyBloomFilter":"ENABLED","readReplicasSettings":{"perAzReadReplicasCount":"1"}`), plainTable),
			`{"setKeyBloomFilter":"ENABLED","setReadReplicasSettings":{"perAzReadReplicasCount":"1"}}`, "Changes the table's read replicas or key bloom filter.*"},
		{"changes the partitioning, the replicas and the filter in one call",
			partitioningCase(settingsTable(`"partitioningSettings":{"maxPartitionsCount":"8"},"keyBloomFilter":"ENABLED","readReplicasSettings":{"anyAzReadReplicasCount":"1"}`),
				partitionedTable(defaultPartitioning)),
			`{"alterPartitioningSettings":{"maxPartitionsCount":"8"},"setKeyBloomFilter":"ENABLED","setReadReplicasSettings":{"anyAzReadReplicasCount":"1"}}`,
			"Changes when YDB splits and merges the table's partitions, and its read replicas or key bloom filter"},
		{"changes the filter beside partitioning the table already holds",
			partitioningCase(settingsTable(`"partitioningSettings":{"maxPartitionsCount":"50"},"keyBloomFilter":"ENABLED"`), partitionedTable(defaultPartitioning)),
			`{"setKeyBloomFilter":"ENABLED"}`, "Changes the table's read replicas or key bloom filter.*"},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			request := alterRequest(c, test.in)
			original := proto.CloneOf(request)
			plan, err := backend.New().PlanFeatures(c.Context(), request)
			c.Assert(err, qt.IsNil)
			c.Assert(plan.Diagnostics, qt.HasLen, 0)
			c.Assert(plan.Contributions, qt.HasLen, 1)
			c.Assert(plan.Contributions[0].Steps, qt.HasLen, 1)
			step := plan.Contributions[0].Steps[0]
			c.Assert(step.Id.Name, qt.Equals, "row-table-partitioning/000000")
			c.Assert(step.Metadata.Transaction, qt.Equals, protocol.PlanningMetadata_FORBIDDEN)
			c.Assert(step.Metadata.Impact, qt.Equals, protocol.ChangeSemantics_BEHAVIORAL)
			c.Assert(step.Metadata.Reason, qt.Matches, test.reason)
			c.Assert(step.Metadata.Effects, qt.HasLen, 2)
			c.Assert(step.Metadata.Effects[1].Subject.Kind, qt.Equals, "ydb.partitioning")
			c.Assert(plan.Parents[0].Receipt.Steps, qt.HasLen, 1)
			alteration, err := tablecreate.DecodeAlteration(step.Operation.Value.Value)
			c.Assert(err, qt.IsNil)
			got, err := protojson.Marshal(alteration.Request)
			c.Assert(err, qt.IsNil)
			c.Assert(string(got), qt.JSONEquals, jsonValue(c, test.want))
			c.Assert(proto.Equal(request, original), qt.IsTrue)
		})
	}
}

func TestTableSettingsPlan_CarriesTheHeldSettings(t *testing.T) {
	for _, test := range []struct {
		name         string
		in           alterCase
		bloom        string
		replicas     string
		partitioning bool
	}{
		{"the filter a table reports as unspecified",
			partitioningCase(settingsTable(`"keyBloomFilter":"ENABLED"`), plainTable), "STATUS_UNSPECIFIED", "", false},
		{"no replicas as an empty value",
			partitioningCase(settingsTable(`"readReplicasSettings":{"perAzReadReplicasCount":"1"}`), plainTable), "", `{}`, false},
		{"the replicas a table holds",
			partitioningCase(settingsTable(`"readReplicasSettings":{"perAzReadReplicasCount":"0"}`), settingsTable(`"readReplicasSettings":{"anyAzReadReplicasCount":"2"}`)),
			"", `{"anyAzReadReplicasCount":"2"}`, false},
		{"the partitioning beside a filter",
			partitioningCase(settingsTable(`"partitioningSettings":{"maxPartitionsCount":"8"},"keyBloomFilter":"ENABLED"`), partitionedTable(defaultPartitioning)),
			"STATUS_UNSPECIFIED", "", true},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			plan, err := backend.New().PlanFeatures(c.Context(), alterRequest(c, test.in))
			c.Assert(err, qt.IsNil)
			alteration, err := tablecreate.DecodeAlteration(plan.Contributions[0].Steps[0].Operation.Value.Value)
			c.Assert(err, qt.IsNil)
			c.Assert(heldBloom(alteration), qt.Equals, test.bloom)
			held, err := protojson.Marshal(alteration.HeldReadReplicas)
			c.Assert(err, qt.IsNil)
			c.Assert(alteration.HeldReadReplicas != nil, qt.Equals, test.replicas != "")
			c.Assert(string(held), qt.JSONEquals, jsonValue(c, orEmptyObject(test.replicas)))
			c.Assert(alteration.HeldPartitioning != nil, qt.Equals, test.partitioning)
		})
	}
}

// heldBloom names the key bloom filter an alteration carries as held, or ""
// for none.
func heldBloom(alteration *tablecreate.Alteration) string {
	if alteration.HeldKeyBloomFilter == nil {
		return ""
	}
	return alteration.HeldKeyBloomFilter.String()
}

func orEmptyObject(text string) string {
	if text == "" {
		return `{}`
	}
	return text
}

func TestTableSettingsPlan_NoChange(t *testing.T) {
	for _, test := range []struct {
		name string
		in   alterCase
	}{
		{"a filter declared off on a table that reports none", partitioningCase(settingsTable(`"keyBloomFilter":"DISABLED"`), plainTable)},
		{"a filter declared on that the table holds", partitioningCase(settingsTable(`"keyBloomFilter":"ENABLED"`), settingsTable(`"keyBloomFilter":"ENABLED"`))},
		{"a filter the declaration leaves out", partitioningCase(plainTable, settingsTable(`"keyBloomFilter":"ENABLED"`))},
		{"no replicas declared as PER_AZ:0 on a table that holds none",
			partitioningCase(settingsTable(`"readReplicasSettings":{"perAzReadReplicasCount":"0"}`), plainTable)},
		{"no replicas declared as ANY_AZ:0 on a table read as PER_AZ:0",
			partitioningCase(settingsTable(`"readReplicasSettings":{"anyAzReadReplicasCount":"0"}`), settingsTable(`"readReplicasSettings":{"perAzReadReplicasCount":"0"}`))},
		{"the replicas the table holds",
			partitioningCase(settingsTable(`"readReplicasSettings":{"anyAzReadReplicasCount":"2"}`), settingsTable(`"readReplicasSettings":{"anyAzReadReplicasCount":"2"}`))},
		{"replicas the declaration leaves out", partitioningCase(plainTable, settingsTable(`"readReplicasSettings":{"anyAzReadReplicasCount":"2"}`))},
		{"a declaration without table properties leaves both alone",
			partitioningCase("", settingsTable(`"keyBloomFilter":"ENABLED","readReplicasSettings":{"anyAzReadReplicasCount":"2"}`))},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			plan, err := backend.New().PlanFeatures(c.Context(), alterRequest(c, test.in))
			c.Assert(err, qt.IsNil)
			c.Assert(plan.Diagnostics, qt.HasLen, 0)
			c.Assert(plan.Contributions, qt.HasLen, 0)
			c.Assert(plan.Parents[0].Receipt.Steps, qt.HasLen, 0)
		})
	}
}

func TestTableSettingsPlan_RefusesThePartitioningBesideASetting(t *testing.T) {
	c := testcheck.New(t)
	plan, err := backend.New().PlanFeatures(c.Context(), alterRequest(c,
		partitioningCase(settingsTable(`"partitioningSettings":{"minPartitionsCount":"60"},"keyBloomFilter":"ENABLED"`), partitionedTable(defaultPartitioning))))
	c.Assert(err, qt.IsNil)
	c.Assert(plan.Diagnostics, qt.HasLen, 1)
	c.Assert(plan.Diagnostics[0].Message, qt.Matches, `AUTO_PARTITIONING_MIN_PARTITIONS_COUNT 60 exceeds AUTO_PARTITIONING_MAX_PARTITIONS_COUNT 50`)
	c.Assert(plan.Contributions, qt.HasLen, 0)
}

func TestTableSettingsPlan_RoundTrip(t *testing.T) {
	c := testcheck.New(t)
	service := backend.New()
	request := alterRequest(c, partitioningCase(settingsTable(`"keyBloomFilter":"ENABLED","readReplicasSettings":{"perAzReadReplicasCount":"0"}`),
		settingsTable(`"readReplicasSettings":{"anyAzReadReplicasCount":"2"}`)))
	plan, err := service.PlanFeatures(c.Context(), request)
	c.Assert(err, qt.IsNil)
	value := plan.Contributions[0].Steps[0].Operation.Value
	normalized, err := service.NormalizeValues(c.Context(), &protocol.NormalizeValuesRequest{Engine: "ydb", Values: []*protocol.ValueInput{
		{Id: value.Id, Representation: protocol.ValueInput_OPERATION, Value: value.Value}}})
	c.Assert(err, qt.IsNil)
	c.Assert(proto.Equal(normalized.Values[0], value), qt.IsTrue)
}

func TestTableSettingsOperation_FailurePath(t *testing.T) {
	for _, test := range []struct {
		name    string
		payload string
		message string
	}{
		{"a filter without the held one", `{"request":{"setKeyBloomFilter":"ENABLED"}}`,
			"YDB key bloom filter alteration requires the held filter"},
		{"a held filter without a filter", `{"request":{"setReadReplicasSettings":{"perAzReadReplicasCount":"1"}},"held_read_replicas":{},"held_key_bloom_filter":"DISABLED"}`,
			"a held YDB key bloom filter disagrees with the alteration"},
		{"a filter the table already keeps off", `{"request":{"setKeyBloomFilter":"DISABLED"},"held_key_bloom_filter":"STATUS_UNSPECIFIED"}`,
			"YDB key bloom filter alteration changes nothing"},
		{"a filter the table already keeps on", `{"request":{"setKeyBloomFilter":"ENABLED"},"held_key_bloom_filter":"ENABLED"}`,
			"YDB key bloom filter alteration changes nothing"},
		{"an unnamed filter value", `{"request":{"setKeyBloomFilter":7},"held_key_bloom_filter":"DISABLED"}`,
			"unknown enum in Ydb.Table.AlterTableRequest.set_key_bloom_filter"},
		{"an unknown held filter", `{"request":{"setKeyBloomFilter":"ENABLED"},"held_key_bloom_filter":"SOMETIMES"}`,
			"invalid held YDB key bloom filter"},
		{"replicas without the held ones", `{"request":{"setReadReplicasSettings":{"perAzReadReplicasCount":"1"}}}`,
			"YDB read replica alteration requires the held replicas"},
		{"held replicas without replicas", `{"request":{"setKeyBloomFilter":"ENABLED"},"held_key_bloom_filter":"DISABLED","held_read_replicas":{}}`,
			"held YDB read replicas disagree with the alteration"},
		{"replicas with no mode", `{"request":{"setReadReplicasSettings":{}},"held_read_replicas":{}}`,
			"YDB read replica alteration names no mode"},
		{"no replicas on a table that holds none", `{"request":{"setReadReplicasSettings":{"perAzReadReplicasCount":"0"}},"held_read_replicas":{}}`,
			"YDB read replica alteration changes nothing"},
		{"the replicas the table holds", `{"request":{"setReadReplicasSettings":{"anyAzReadReplicasCount":"2"}},"held_read_replicas":{"anyAzReadReplicasCount":"2"}}`,
			"YDB read replica alteration changes nothing"},
		{"held replicas with an unknown field", `{"request":{"setReadReplicasSettings":{"anyAzReadReplicasCount":"2"}},"held_read_replicas":{"everyAz":"2"}}`,
			"invalid held YDB read replicas"},
		{"a filter and a TTL removal", `{"request":{"setKeyBloomFilter":"ENABLED","dropTtlSettings":{}},"held_key_bloom_filter":"DISABLED","held_ttl":{"dateTypeColumn":{"columnName":"x","expireAfterSeconds":60}}}`,
			"YDB table alteration mixes family, column, TTL, table setting and index changes"},
		{"replicas and a column removal", `{"request":{"setReadReplicasSettings":{"anyAzReadReplicasCount":"2"},"dropColumns":["a"]},"held_read_replicas":{}}`,
			"YDB table alteration mixes family, column, TTL, table setting and index changes"},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			definition := tablecreate.AlterModel()
			normalized, err := tablecreate.NormalizeAlteration(&protocol.ValueInput{Id: "operation", Representation: protocol.ValueInput_OPERATION, Value: &protocol.Envelope{
				ProviderId: "github.com/stokaro/ptah-ydb-plugin", Kind: tablecreate.AlterOperationKind, Owner: owner(), SchemaVersion: 1, Encoding: definition.Encoding,
				DefinitionSha256: definition.DefinitionSha256, Payload: []byte(test.payload)}})
			c.Assert(err, qt.ErrorMatches, test.message)
			c.Assert(normalized, qt.IsNil)
		})
	}
}
