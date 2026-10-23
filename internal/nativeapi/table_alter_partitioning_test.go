package nativeapi_test

import (
	"testing"

	qt "github.com/frankban/quicktest"
	"google.golang.org/protobuf/encoding/protojson"
)

const (
	heldDefaults = `{"partitionBy":["id"],"partitioningBySize":"ENABLED","partitionSizeMb":"2048","minPartitionsCount":"1","maxPartitionsCount":"50"}`
	// maxEight is heldDefaults after the maximum drops to eight.
	maxEight = `{"partitionBy":["id"],"partitioningBySize":"ENABLED","partitionSizeMb":"2048","minPartitionsCount":"1","maxPartitionsCount":"8"}`
	// sizeOff is heldDefaults after partitioning by size is switched off: the
	// server then reports no partition size.
	sizeOff = `{"partitionBy":["id"],"partitioningBySize":"DISABLED","minPartitionsCount":"1","maxPartitionsCount":"50"}`
	// offWithMinimum is a table that does not partition by size and keeps a minimum of two.
	offWithMinimum = `{"partitionBy":["id"],"partitioningBySize":"DISABLED","minPartitionsCount":"2","maxPartitionsCount":"50"}`
)

func partitionedTableOf(settings string, columns ...string) string {
	if settings == "" {
		return withColumns("", columns...)
	}
	return withColumns(`,"partitioningSettings":`+settings, columns...)
}

func alterPartitioning(set, held string) string {
	return `{"request":{"alterPartitioningSettings":` + set + `},"held_partitioning":` + held + `}`
}

func TestNativeTableAlteration_PartitioningHappyPath(t *testing.T) {
	for _, test := range []struct {
		name          string
		before, after string
		payload       string
		wantSent      string
	}{
		{"lowers the maximum", partitionedTableOf(heldDefaults, tsCol), partitionedTableOf(maxEight, tsCol),
			alterPartitioning(`{"maxPartitionsCount":"8"}`, heldDefaults), `{"maxPartitionsCount":"8"}`},
		{"switches partitioning by size off and reads no size", partitionedTableOf(heldDefaults, tsCol), partitionedTableOf(sizeOff, tsCol),
			alterPartitioning(`{"partitioningBySize":"DISABLED"}`, heldDefaults), `{"partitioningBySize":"DISABLED"}`},
		{"does not compare the settings the server derives from the ones named",
			partitionedTableOf(offWithMinimum, tsCol), partitionedTableOf(heldDefaults, tsCol),
			alterPartitioning(`{"partitioningBySize":"ENABLED"}`, offWithMinimum), `{"partitioningBySize":"ENABLED"}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := qt.New(t)
			fixture := &alterFixture{before: test.before, after: test.after}
			dispatched, err := applyAlteration(c, fixture, "", test.payload, false)
			c.Assert(err, qt.IsNil)
			c.Assert(dispatched, qt.IsTrue)
			c.Assert(fixture.events, qt.DeepEquals, []string{"open", "describe", "guard", "alter", "describe", "close"})
			sent, err := protojson.Marshal(fixture.sent.GetAlterPartitioningSettings())
			c.Assert(err, qt.IsNil)
			c.Assert(string(sent), qt.JSONEquals, jsonOf(c, orEmpty(test.wantSent)))
		})
	}
}

func TestNativeTableAlteration_PartitioningFailurePath(t *testing.T) {
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
		{"the maximum changed since planning", alterFixture{before: partitionedTableOf(maxEight, tsCol)},
			alterPartitioning(`{"minPartitionsCount":"2"}`, heldDefaults), false, refused, "YDB table partitioning settings differ from the plan"},
		{"the table gained partitioning settings since planning", alterFixture{before: partitionedTableOf(heldDefaults, tsCol)},
			alterPartitioning(`{"maxPartitionsCount":"8"}`, `{}`), false, refused, "YDB table partitioning settings differ from the plan"},
		{"the table reports no partitioning but the plan held some", alterFixture{before: partitionedTableOf("", tsCol)},
			alterPartitioning(`{"maxPartitionsCount":"8"}`, heldDefaults), false, refused, "YDB table partitioning settings differ from the plan"},
		{"the setting is not applied", alterFixture{before: partitionedTableOf(heldDefaults, tsCol), after: partitionedTableOf(heldDefaults, tsCol)},
			alterPartitioning(`{"maxPartitionsCount":"8"}`, heldDefaults), true, unconfirmed, "YDB altered table partitioning settings differ from the request"},
		{"another value is read", alterFixture{before: partitionedTableOf(heldDefaults, tsCol),
			after: partitionedTableOf(`{"partitionBy":["id"],"partitioningBySize":"ENABLED","partitionSizeMb":"2048","minPartitionsCount":"1","maxPartitionsCount":"9"}`, tsCol)},
			alterPartitioning(`{"maxPartitionsCount":"8"}`, heldDefaults), true, unconfirmed, "YDB altered table partitioning settings differ from the request"},
		{"the partition columns changed", alterFixture{before: partitionedTableOf(heldDefaults, tsCol),
			after: partitionedTableOf(`{"partitionBy":["ts"],"partitioningBySize":"ENABLED","partitionSizeMb":"2048","minPartitionsCount":"1","maxPartitionsCount":"8"}`, tsCol)},
			alterPartitioning(`{"maxPartitionsCount":"8"}`, heldDefaults), true, unconfirmed, "YDB altered table partitioning settings differ from the request"},
		{"the table reports no partitioning afterward", alterFixture{before: partitionedTableOf(heldDefaults, tsCol), after: partitionedTableOf("", tsCol)},
			alterPartitioning(`{"maxPartitionsCount":"8"}`, heldDefaults), true, unconfirmed, "YDB altered table partitioning settings differ from the request"},
		{"lost reply", alterFixture{before: partitionedTableOf(heldDefaults, tsCol), alterFailure: true},
			alterPartitioning(`{"maxPartitionsCount":"8"}`, heldDefaults), true, []string{"open", "describe", "guard", "alter", "close"}, "lost native reply"},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := qt.New(t)
			fixture := test.fixture
			dispatched, err := applyAlteration(c, &fixture, "", test.payload, false)
			c.Assert(err, qt.ErrorMatches, test.message)
			c.Assert(dispatched, qt.Equals, test.dispatched)
			c.Assert(fixture.events, qt.DeepEquals, test.events)
		})
	}
}
