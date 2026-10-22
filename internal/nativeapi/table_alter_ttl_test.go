package nativeapi_test

import (
	"encoding/json"
	"strings"
	"testing"

	qt "github.com/frankban/quicktest"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb_Table"
	"google.golang.org/protobuf/encoding/protojson"
)

const (
	tsCol    = `{"name":"ts","type":{"optionalType":{"item":{"typeId":"TIMESTAMP"}}}}`
	dayCol   = `{"name":"day","type":{"optionalType":{"item":{"typeId":"DATE"}}}}`
	stampCol = `{"name":"stamp","type":{"optionalType":{"item":{"typeId":"UINT64"}}}}`
	// ttlOn reads a table setting that holds the given TTL.
	hourlyTS    = `{"dateTypeColumn":{"columnName":"ts","expireAfterSeconds":3600}}`
	hourlyDay   = `{"dateTypeColumn":{"columnName":"day","expireAfterSeconds":3600}}`
	daily       = `{"dateTypeColumn":{"columnName":"ts","expireAfterSeconds":86400}}`
	hourlyStamp = `{"valueSinceUnixEpoch":{"columnName":"stamp","columnUnit":"UNIT_MILLISECONDS","expireAfterSeconds":3600}}`
	scheduled   = `{"dateTypeColumn":{"columnName":"ts","expireAfterSeconds":3600},"runIntervalSeconds":900}`
)

func ttlTableOf(ttl string, columns ...string) string {
	if ttl == "" {
		return withColumns("", columns...)
	}
	return withColumns(`,"ttlSettings":`+ttl, columns...)
}

func setTTL(ttl, held string) string {
	if held == "" {
		return `{"request":{"setTtlSettings":` + ttl + `}}`
	}
	return `{"request":{"setTtlSettings":` + ttl + `},"held_ttl":` + held + `}`
}

func dropTTL(held string) string {
	return `{"request":{"dropTtlSettings":{}},"held_ttl":` + held + `}`
}

func TestNativeTableAlteration_TTLHappyPath(t *testing.T) {
	for _, test := range []struct {
		name          string
		before, after string
		payload       string
		wantSet       string
		wantDrop      bool
	}{
		{"sets a TTL on a table without one", ttlTableOf("", tsCol), ttlTableOf(hourlyTS, tsCol), setTTL(hourlyTS, ""), hourlyTS, false},
		{"changes the interval", ttlTableOf(hourlyTS, tsCol), ttlTableOf(daily, tsCol), setTTL(daily, hourlyTS), daily, false},
		{"moves the TTL to another column", ttlTableOf(hourlyTS, tsCol, dayCol), ttlTableOf(hourlyDay, tsCol, dayCol), setTTL(hourlyDay, hourlyTS), hourlyDay, false},
		{"reads an integer column", ttlTableOf(hourlyTS, tsCol, stampCol), ttlTableOf(hourlyStamp, tsCol, stampCol), setTTL(hourlyStamp, hourlyTS), hourlyStamp, false},
		{"keeps the run interval", ttlTableOf(scheduled, tsCol),
			ttlTableOf(`{"dateTypeColumn":{"columnName":"ts","expireAfterSeconds":86400},"runIntervalSeconds":900}`, tsCol),
			setTTL(`{"dateTypeColumn":{"columnName":"ts","expireAfterSeconds":86400},"runIntervalSeconds":900}`, scheduled),
			`{"dateTypeColumn":{"columnName":"ts","expireAfterSeconds":86400},"runIntervalSeconds":900}`, false},
		{"removes the TTL", ttlTableOf(hourlyTS, tsCol), ttlTableOf("", tsCol), dropTTL(hourlyTS), "", true},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := qt.New(t)
			fixture := &alterFixture{before: test.before, after: test.after}
			dispatched, err := applyAlteration(c, fixture, "", test.payload, false)
			c.Assert(err, qt.IsNil)
			c.Assert(dispatched, qt.IsTrue)
			c.Assert(fixture.events, qt.DeepEquals, []string{"open", "describe", "guard", "alter", "describe", "close"})
			c.Assert(fixture.sent.GetDropTtlSettings() != nil, qt.Equals, test.wantDrop)
			sent, err := protojson.Marshal(fixture.sent.GetSetTtlSettings())
			c.Assert(err, qt.IsNil)
			want := new(Ydb_Table.TtlSettings)
			c.Assert(protojson.Unmarshal([]byte(orEmpty(test.wantSet)), want), qt.IsNil)
			wantJSON, err := protojson.Marshal(want)
			c.Assert(err, qt.IsNil)
			c.Assert(string(sent), qt.JSONEquals, jsonOf(c, string(wantJSON)))
		})
	}
}

func orEmpty(text string) string {
	if text == "" {
		return "{}"
	}
	return text
}

func jsonOf(c *qt.C, text string) any {
	c.Helper()
	var value any
	c.Assert(json.Unmarshal([]byte(text), &value), qt.IsNil)
	return value
}

func TestNativeTableAlteration_TTLFailurePath(t *testing.T) {
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
		{"the table holds another TTL than planned", alterFixture{before: ttlTableOf(daily, tsCol)}, setTTL(hourlyDay, hourlyTS), false, refused, "YDB table TTL differs from the plan"},
		{"the table gained a TTL since planning", alterFixture{before: ttlTableOf(hourlyTS, tsCol)}, setTTL(hourlyTS, ""), false, refused, "YDB table TTL differs from the plan"},
		{"the table lost its TTL since planning", alterFixture{before: ttlTableOf("", tsCol)}, dropTTL(hourlyTS), false, refused, "YDB table TTL differs from the plan"},
		{"the run interval changed since planning", alterFixture{before: ttlTableOf(scheduled, tsCol)}, setTTL(daily, hourlyTS), false, refused, "YDB table TTL differs from the plan"},
		{"the TTL column is absent", alterFixture{before: ttlTableOf("", payloadCol)}, setTTL(hourlyTS, ""), false, refused, "YDB TTL column is absent"},
		{"a date mode on an integer column", alterFixture{before: ttlTableOf("", stampCol)},
			setTTL(`{"dateTypeColumn":{"columnName":"stamp","expireAfterSeconds":60}}`, ""), false, refused, `YDB TTL column "stamp" is Uint64, an integer type.*`},
		{"an integer mode on a date column", alterFixture{before: ttlTableOf("", tsCol)},
			setTTL(`{"valueSinceUnixEpoch":{"columnName":"ts","columnUnit":"UNIT_SECONDS","expireAfterSeconds":60}}`, ""), false, refused, `YDB TTL column "ts" is Timestamp, a date type.*`},
		{"a text column", alterFixture{before: ttlTableOf("", payloadCol)},
			setTTL(`{"dateTypeColumn":{"columnName":"payload","expireAfterSeconds":60}}`, ""), false, refused, `YDB TTL column "payload" is Utf8, and YDB reads a TTL from.*`},
		{"the TTL is not applied", alterFixture{before: ttlTableOf("", tsCol), after: ttlTableOf("", tsCol)}, setTTL(hourlyTS, ""), true, unconfirmed, "YDB altered table TTL differs from the request"},
		{"another column is read", alterFixture{before: ttlTableOf("", tsCol, dayCol), after: ttlTableOf(hourlyDay, tsCol, dayCol)}, setTTL(hourlyTS, ""), true, unconfirmed, "YDB altered table TTL differs from the request"},
		{"another interval is read", alterFixture{before: ttlTableOf("", tsCol), after: ttlTableOf(daily, tsCol)}, setTTL(hourlyTS, ""), true, unconfirmed, "YDB altered table TTL differs from the request"},
		{"the run interval is reset", alterFixture{before: ttlTableOf(scheduled, tsCol), after: ttlTableOf(daily, tsCol)},
			setTTL(`{"dateTypeColumn":{"columnName":"ts","expireAfterSeconds":86400},"runIntervalSeconds":900}`, scheduled), true, unconfirmed, "YDB altered table TTL differs from the request"},
		{"the removal is not applied", alterFixture{before: ttlTableOf(hourlyTS, tsCol), after: ttlTableOf(hourlyTS, tsCol)}, dropTTL(hourlyTS), true, unconfirmed, "YDB altered table TTL differs from the request"},
		{"the alteration changed a column", alterFixture{before: ttlTableOf("", tsCol), after: ttlTableOf(hourlyTS, `{"name":"ts","type":{"typeId":"STRING"}}`)}, setTTL(hourlyTS, ""), true, unconfirmed, "YDB altered column differs from the request"},
		{"a column alteration changed the TTL", alterFixture{before: ttlTableOf(hourlyTS, tsCol, payloadCol, oldCol), after: ttlTableOf(daily, tsCol, payloadCol)}, dropOld, true, unconfirmed, "YDB altered table TTL differs from the request"},
		{"a family alteration dropped the TTL", alterFixture{before: ttlTableOf(hourlyTS, tsCol), after: strings.Replace(ttlTableOf("", tsCol), `"columnFamilies":[{"name":"default"}]`, `"columnFamilies":[{"name":"default"},{"name":"archive"}]`, 1)}, `{"request":{"addColumnFamilies":[{"name":"archive"}]}}`, true, unconfirmed, "YDB altered table TTL differs from the request"},
		{"lost reply", alterFixture{before: ttlTableOf("", tsCol), alterFailure: true}, setTTL(hourlyTS, ""), true, []string{"open", "describe", "guard", "alter", "close"}, "lost native reply"},
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
