package nativeapi_test

import (
	"context"
	"errors"
	"testing"

	qt "github.com/frankban/quicktest"
	"github.com/stokaro/ptah-ydb-plugin/internal/nativeapi"
	native "github.com/stokaro/ptah-ydb-plugin/internal/nativeprotocol"
	"github.com/stokaro/ptah-ydb-plugin/internal/tablecreate"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
	"github.com/stokaro/ptah-ydb-plugin/schema"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

// indexSettingsFixture serves /local/app/records before and after the
// statement, and records what ran.
type indexSettingsFixture struct {
	before, after string
	events        []string
	executed      []string
	failExecute   bool
	described     int
}

func (f *indexSettingsFixture) Describe(_ context.Context, path string) (*native.DescribeTableResult, error) {
	f.events = append(f.events, "describe")
	f.described++
	if path != "/local/app/records" {
		return nil, errors.New("wrong describe path")
	}
	text := f.before
	if f.described > 1 {
		text = f.after
	}
	var result native.DescribeTableResult
	if err := protojson.Unmarshal([]byte(text), &result); err != nil {
		return nil, err
	}
	return &result, nil
}

func (f *indexSettingsFixture) Close() error {
	f.events = append(f.events, "close")
	return nil
}

const (
	labelColumn = `{"name":"label","type":{"optionalType":{"item":{"typeId":"UTF8"}}}}`
	// newIndexSettings are the settings YDB gives a new global index.
	newIndexSettings = `"partitioningSettings":{"partitioningBySize":"ENABLED","partitionSizeMb":"2048","partitioningByLoad":"DISABLED","minPartitionsCount":"1"}`
	minThree         = `"partitioningSettings":{"partitioningBySize":"ENABLED","partitionSizeMb":"2048","partitioningByLoad":"DISABLED","minPartitionsCount":"3"}`
	onePerZone       = `"readReplicasSettings":{"perAzReadReplicasCount":"1"}`
	nonePerZone      = `"readReplicasSettings":{"perAzReadReplicasCount":"0"}`
)

// settingsIndexedTable is records holding by_label, a global index of the given kind
// with the given settings fields, and the other indexes given.
func settingsIndexedTable(kind, settings string, others ...string) string {
	indexes := `{"name":"by_label","indexColumns":["label"],"` + kind + `":{"settings":{` + settings + `}},"status":"STATUS_READY"}`
	for _, other := range others {
		indexes += "," + other
	}
	return tableHead + `"columns":[` + idColumn + `,` + labelColumn + `],"columnFamilies":[{"name":"default"}],"indexes":[` + indexes + `]}`
}

func indexSettingsPayload(set, heldSettings string) string {
	return `{"index":"by_label","set":` + set + `,"held_index":{"name":"by_label","indexColumns":["label"],"globalIndex":{"settings":{` + heldSettings +
		`}},"status":"STATUS_READY"}}`
}

func applyIndexSettings(c *qt.C, fixture *indexSettingsFixture, payload string) (bool, error) {
	c.Helper()
	api := &nativeapi.IndexSettings{
		Open: func(context.Context) (nativeapi.IndexDescriber, error) {
			fixture.events = append(fixture.events, "open")
			return fixture, nil
		},
		Execute: func(_ context.Context, statement string) error {
			fixture.events = append(fixture.events, "execute")
			fixture.executed = append(fixture.executed, statement)
			if fixture.failExecute {
				return errors.New("lost reply")
			}
			return nil
		},
	}
	definition := tablecreate.IndexSettingsModel()
	envelope := &protocol.Envelope{ProviderId: schema.ProviderID, Kind: tablecreate.IndexSettingsOperationKind,
		Owner: &protocol.Reference{Engine: "ydb", Kind: "table", Schema: "app", Name: "records"}, SchemaVersion: 1,
		Encoding: definition.Encoding, DefinitionSha256: definition.DefinitionSha256, Payload: []byte(payload)}
	original := proto.CloneOf(envelope)
	dispatched, err := api.Apply(c.Context(), "/local", envelope, func(context.Context) error {
		fixture.events = append(fixture.events, "guard")
		return nil
	})
	c.Assert(proto.Equal(original, envelope), qt.IsTrue)
	return dispatched, err
}

func TestNativeIndexSettings_HappyPath(t *testing.T) {
	for _, test := range []struct {
		name          string
		before, after string
		payload       string
		statement     string
	}{
		{"raises the minimum", settingsIndexedTable("globalIndex", newIndexSettings), settingsIndexedTable("globalIndex", minThree),
			indexSettingsPayload(`{"partitioningSettings":{"minPartitionsCount":"3"}}`, newIndexSettings),
			"ALTER TABLE `/local/app/records` ALTER INDEX `by_label` SET (AUTO_PARTITIONING_BY_SIZE = ENABLED, AUTO_PARTITIONING_PARTITION_SIZE_MB = 2048, " +
				"AUTO_PARTITIONING_BY_LOAD = DISABLED, AUTO_PARTITIONING_MIN_PARTITIONS_COUNT = 3);"},
		{"sets read replicas", settingsIndexedTable("globalIndex", newIndexSettings), settingsIndexedTable("globalIndex", newIndexSettings+","+onePerZone),
			indexSettingsPayload(`{"readReplicasSettings":{"perAzReadReplicasCount":"1"}}`, newIndexSettings),
			"ALTER TABLE `/local/app/records` ALTER INDEX `by_label` SET (READ_REPLICAS_SETTINGS = \"PER_AZ:1\");"},
		{"removes read replicas, read back as PER_AZ:0", settingsIndexedTable("globalIndex", newIndexSettings+","+onePerZone),
			settingsIndexedTable("globalIndex", newIndexSettings+","+nonePerZone),
			indexSettingsPayload(`{"readReplicasSettings":{"perAzReadReplicasCount":"0"}}`, newIndexSettings+","+onePerZone),
			"ALTER TABLE `/local/app/records` ALTER INDEX `by_label` SET (READ_REPLICAS_SETTINGS = \"PER_AZ:0\");"},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			fixture := &indexSettingsFixture{before: test.before, after: test.after}
			dispatched, err := applyIndexSettings(c, fixture, test.payload)
			c.Assert(err, qt.IsNil)
			c.Assert(dispatched, qt.IsTrue)
			c.Assert(fixture.events, qt.DeepEquals, []string{"open", "describe", "guard", "execute", "describe", "close"})
			c.Assert(fixture.executed, qt.DeepEquals, []string{test.statement})
		})
	}
}

func TestNativeIndexSettings_FailurePath(t *testing.T) {
	refused := []string{"open", "describe", "close"}
	unconfirmed := []string{"open", "describe", "guard", "execute", "describe", "close"}
	raise := indexSettingsPayload(`{"partitioningSettings":{"minPartitionsCount":"3"}}`, newIndexSettings)
	otherIndex := `{"name":"by_id","indexColumns":["label","id"],"globalIndex":{"settings":{` + newIndexSettings + `}},"status":"STATUS_READY"}`
	otherChanged := `{"name":"by_id","indexColumns":["label","id"],"globalIndex":{"settings":{` + minThree + `}},"status":"STATUS_READY"}`
	for _, test := range []struct {
		name       string
		fixture    indexSettingsFixture
		payload    string
		dispatched bool
		events     []string
		message    string
	}{
		{"the index changed its settings since planning", indexSettingsFixture{before: settingsIndexedTable("globalIndex", minThree)}, raise, false, refused,
			`YDB index "by_label" differs from the plan`},
		{"the index changed its kind since planning", indexSettingsFixture{before: settingsIndexedTable("globalAsyncIndex", newIndexSettings)}, raise, false, refused,
			`YDB index "by_label" differs from the plan`},
		{"the index is gone", indexSettingsFixture{before: tableHead + `"columns":[` + idColumn + `,` + labelColumn + `],"columnFamilies":[{"name":"default"}]}`},
			raise, false, refused, `YDB index "by_label" differs from the plan`},
		{"the setting is not applied", indexSettingsFixture{before: settingsIndexedTable("globalIndex", newIndexSettings), after: settingsIndexedTable("globalIndex", newIndexSettings)},
			raise, true, unconfirmed, `YDB index "by_label" partitioning settings differ from the request`},
		{"the replicas moved with the partitioning", indexSettingsFixture{before: settingsIndexedTable("globalIndex", newIndexSettings), after: settingsIndexedTable("globalIndex", minThree+","+onePerZone)},
			raise, true, unconfirmed, `YDB index "by_label" read replicas differ from the request`},
		{"the partitioning moved with the replicas", indexSettingsFixture{before: settingsIndexedTable("globalIndex", newIndexSettings), after: settingsIndexedTable("globalIndex", minThree+","+onePerZone)},
			indexSettingsPayload(`{"readReplicasSettings":{"perAzReadReplicasCount":"1"}}`, newIndexSettings), true, unconfirmed,
			`YDB index "by_label" partitioning settings differ from the request`},
		{"the index changed its kind", indexSettingsFixture{before: settingsIndexedTable("globalIndex", newIndexSettings), after: settingsIndexedTable("globalAsyncIndex", minThree)},
			raise, true, unconfirmed, `YDB index "by_label" changed its definition with its settings`},
		{"another index changed", indexSettingsFixture{before: settingsIndexedTable("globalIndex", newIndexSettings, otherIndex), after: settingsIndexedTable("globalIndex", minThree, otherChanged)},
			raise, true, unconfirmed, `YDB altered table changed the index "by_id"`},
		{"lost reply", indexSettingsFixture{before: settingsIndexedTable("globalIndex", newIndexSettings), failExecute: true},
			raise, true, []string{"open", "describe", "guard", "execute", "close"}, "lost reply"},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			fixture := test.fixture
			dispatched, err := applyIndexSettings(c, &fixture, test.payload)
			c.Assert(err, qt.ErrorMatches, test.message)
			c.Assert(dispatched, qt.Equals, test.dispatched)
			c.Assert(fixture.events, qt.DeepEquals, test.events)
		})
	}
}
