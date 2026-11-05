package nativeapi_test

import (
	"testing"

	qt "github.com/frankban/quicktest"
	"google.golang.org/protobuf/encoding/protojson"

	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
)

// settingsTableOf is a table with ts that holds the given settings fields
// beside the default partitioning.
func settingsTableOf(fields string) string {
	extra := `,"partitioningSettings":` + heldDefaults
	if fields != "" {
		extra += "," + fields
	}
	return withColumns(extra, tsCol)
}

const (
	bloomOn        = `"keyBloomFilter":"ENABLED"`
	bloomOff       = `"keyBloomFilter":"DISABLED"`
	twoAnyAZ       = `"readReplicasSettings":{"anyAzReadReplicasCount":"2"}`
	onePerAZ       = `"readReplicasSettings":{"perAzReadReplicasCount":"1"}`
	noneAsPerAZ    = `"readReplicasSettings":{"perAzReadReplicasCount":"0"}`
	setBloomOn     = `{"request":{"setKeyBloomFilter":"ENABLED"},"held_key_bloom_filter":"STATUS_UNSPECIFIED"}`
	setTwoAnyAZ    = `{"request":{"setReadReplicasSettings":{"anyAzReadReplicasCount":"2"}},"held_read_replicas":{}}`
	removeOnePerAZ = `{"request":{"setReadReplicasSettings":{"perAzReadReplicasCount":"0"}},"held_read_replicas":{"perAzReadReplicasCount":"1"}}`
)

func TestNativeTableAlteration_SettingsHappyPath(t *testing.T) {
	for _, test := range []struct {
		name          string
		before, after string
		payload       string
		wantSent      string
	}{
		{"turns the key bloom filter on", settingsTableOf(""), settingsTableOf(bloomOn), setBloomOn, `{"setKeyBloomFilter":"ENABLED"}`},
		{"turns the key bloom filter off, read back as unspecified", settingsTableOf(bloomOn), settingsTableOf(""),
			`{"request":{"setKeyBloomFilter":"DISABLED"},"held_key_bloom_filter":"ENABLED"}`, `{"setKeyBloomFilter":"DISABLED"}`},
		{"sets read replicas", settingsTableOf(""), settingsTableOf(twoAnyAZ), setTwoAnyAZ, `{"setReadReplicasSettings":{"anyAzReadReplicasCount":"2"}}`},
		{"removes read replicas, read back as none", settingsTableOf(onePerAZ), settingsTableOf(""), removeOnePerAZ,
			`{"setReadReplicasSettings":{"perAzReadReplicasCount":"0"}}`},
		{"removes read replicas, read back as PER_AZ:0", settingsTableOf(onePerAZ), settingsTableOf(noneAsPerAZ), removeOnePerAZ,
			`{"setReadReplicasSettings":{"perAzReadReplicasCount":"0"}}`},
		{"changes the partitioning and the filter together", settingsTableOf(""), withColumns(`,"partitioningSettings":`+maxEight+","+bloomOn, tsCol),
			`{"request":{"alterPartitioningSettings":{"maxPartitionsCount":"8"},"setKeyBloomFilter":"ENABLED"},"held_partitioning":` + heldDefaults + `,"held_key_bloom_filter":"STATUS_UNSPECIFIED"}`,
			`{"alterPartitioningSettings":{"maxPartitionsCount":"8"},"setKeyBloomFilter":"ENABLED"}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			fixture := &alterFixture{before: test.before, after: test.after}
			dispatched, err := applyAlteration(c, fixture, "", test.payload, nil)
			c.Assert(err, qt.IsNil)
			c.Assert(dispatched, qt.IsTrue)
			c.Assert(fixture.events, qt.DeepEquals, []string{"open", "describe", "guard", "alter", "describe", "close"})
			sent, err := protojson.Marshal(fixture.sent)
			c.Assert(err, qt.IsNil)
			c.Assert(string(sent), qt.JSONEquals, jsonOf(c, `{"path":"/local/app/records",`+test.wantSent[1:]))
		})
	}
}

func TestNativeTableAlteration_SettingsFailurePath(t *testing.T) {
	refused := []string{"open", "describe", "close"}
	unconfirmed := []string{"open", "describe", "guard", "alter", "describe", "close"}
	for _, test := range []struct {
		name       string
		fixture    alterFixture
		payload    string
		dispatched bool
		events     []string
		message    string
	}{
		{"the filter changed since planning", alterFixture{before: settingsTableOf(bloomOff)}, setBloomOn, false, refused,
			"YDB table key bloom filter differs from the plan"},
		{"the table gained replicas since planning", alterFixture{before: settingsTableOf(onePerAZ)}, setTwoAnyAZ, false, refused,
			"YDB table read replicas differ from the plan"},
		{"the replicas changed since planning", alterFixture{before: settingsTableOf(twoAnyAZ)}, removeOnePerAZ, false, refused,
			"YDB table read replicas differ from the plan"},
		{"the filter is not applied", alterFixture{before: settingsTableOf(""), after: settingsTableOf("")}, setBloomOn, true, unconfirmed,
			"YDB altered table key bloom filter differs from the request"},
		{"the replicas are not applied", alterFixture{before: settingsTableOf(""), after: settingsTableOf(onePerAZ)}, setTwoAnyAZ, true, unconfirmed,
			"YDB altered table read replicas differ from the request"},
		{"the replicas are not removed", alterFixture{before: settingsTableOf(onePerAZ), after: settingsTableOf(onePerAZ)}, removeOnePerAZ, true, unconfirmed,
			"YDB altered table read replicas differ from the request"},
		{"the filter call moved the replicas", alterFixture{before: settingsTableOf(twoAnyAZ), after: settingsTableOf(bloomOn)},
			setBloomOn, true, unconfirmed, "YDB altered table read replicas differ from the request"},
		{"the replica call moved the filter", alterFixture{before: settingsTableOf(bloomOn), after: settingsTableOf(twoAnyAZ)},
			setTwoAnyAZ, true, unconfirmed, "YDB altered table key bloom filter differs from the request"},
		{"the filter call moved the partitioning", alterFixture{before: settingsTableOf(""), after: withColumns(`,"partitioningSettings":`+maxEight+","+bloomOn, tsCol)},
			setBloomOn, true, unconfirmed, "YDB altered table partitioning settings differ from the request"},
		{"the partitioning call moved the filter", alterFixture{before: settingsTableOf(bloomOn), after: withColumns(`,"partitioningSettings":`+maxEight, tsCol)},
			alterPartitioning(`{"maxPartitionsCount":"8"}`, heldDefaults), true, unconfirmed, "YDB altered table key bloom filter differs from the request"},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			fixture := test.fixture
			dispatched, err := applyAlteration(c, &fixture, "", test.payload, nil)
			c.Assert(err, qt.ErrorMatches, test.message)
			c.Assert(dispatched, qt.Equals, test.dispatched)
			c.Assert(fixture.events, qt.DeepEquals, test.events)
		})
	}
}
