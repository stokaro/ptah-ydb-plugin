package nativeapi_test

import (
	"bytes"
	"context"
	"errors"
	"os"
	"testing"

	qt "github.com/frankban/quicktest"
	"github.com/stokaro/ptah-ydb-plugin/internal/nativeapi"
	native "github.com/stokaro/ptah-ydb-plugin/internal/nativeprotocol"
	"github.com/stokaro/ptah-ydb-plugin/internal/tablecreate"
	"github.com/stokaro/ptah-ydb-plugin/schema"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb_Table"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

// ttlState is one reading of the column table probe/c6: its monitoring page and
// its table-service description.
type ttlState struct {
	page      []byte
	described *native.ColumnTableDescribeResult
}

// The TTL of probe/c6 as local-ydb 26.2.1.14 reported it, and the states the
// fixture derives from it (internal/columnstore/testdata/26-2-delete-ttl-inline).
const (
	hourlyTTLFragment = `,"TtlSettings":{"Version":"1","Enabled":{"Tiers":[{"Delete":{},"ApplyAfterSeconds":3600}],"ColumnName":"ts"}}`
	hourlyTTL         = `{"enabled":{"column":"ts","tiers":[{"applyAfterSeconds":"3600","delete":{}}]}}`
	twoHourTTL        = `{"enabled":{"column":"ts","tiers":[{"applyAfterSeconds":"7200","delete":{}}]}}`
)

// ttlStates reads probe/c6 with the hourly TTL it was created with, and derives
// the same table without a TTL and with a TTL of two hours.
func ttlStates(c *qt.C) (hourly, none, twoHours ttlState) {
	c.Helper()
	page, err := os.ReadFile("../columnstore/testdata/26-2-delete-ttl-inline.json")
	c.Assert(err, qt.IsNil)
	encoded, err := os.ReadFile("../columnstore/testdata/26-2-delete-ttl-inline.table.json")
	c.Assert(err, qt.IsNil)
	var described native.ColumnTableDescribeResult
	c.Assert(protojson.Unmarshal(encoded, &described), qt.IsNil)
	c.Assert(bytes.Count(page, []byte(hourlyTTLFragment)), qt.Equals, 1)
	hourly = ttlState{page: page, described: &described}
	none = ttlState{page: bytes.Replace(page, []byte(hourlyTTLFragment), nil, 1), described: proto.CloneOf(&described)}
	none.described.TtlSettings = nil
	twoHours = ttlState{page: bytes.Replace(page, []byte(`"ApplyAfterSeconds":3600`), []byte(`"ApplyAfterSeconds":7200`), 1), described: proto.CloneOf(&described)}
	twoHours.described.TtlSettings = &native.TtlSettings{Mode: &native.TtlSettings_DateTypeColumn{DateTypeColumn: &Ydb_Table.DateTypeColumnModeSettings{ColumnName: "ts", ExpireAfterSeconds: 7200}}}
	return hourly, none, twoHours
}

// ttlFixture answers the reads of probe/c6 with before until the statement
// runs and with after from then on.
type ttlFixture struct {
	before, after                                   ttlState
	executed                                        bool
	events                                          []string
	statement                                       string
	executeFailure, readFailure, monitoringDisabled bool
}

func (f *ttlFixture) state() ttlState {
	return map[bool]ttlState{false: f.before, true: f.after}[f.executed]
}

func (f *ttlFixture) DescribeColumnTable(_ context.Context, path string) (*native.ColumnTableDescribeResult, error) {
	f.events = append(f.events, "describe "+path)
	if f.readFailure {
		return nil, errors.New("lost read")
	}
	return proto.CloneOf(f.state().described), nil
}

func (f *ttlFixture) Close() error {
	f.events = append(f.events, "close")
	return nil
}

func (f *ttlFixture) api() *nativeapi.ColumnTableTTL {
	api := &nativeapi.ColumnTableTTL{
		Open: func(context.Context) (nativeapi.ColumnDescriber, error) {
			f.events = append(f.events, "open")
			return f, nil
		},
		Execute: func(_ context.Context, statement string) error {
			f.events = append(f.events, "execute")
			f.statement, f.executed = statement, true
			return map[bool]error{true: errors.New("lost reply")}[f.executeFailure]
		},
		Monitoring: func(_ context.Context, path string) ([]byte, error) {
			f.events = append(f.events, "monitoring "+path)
			return f.state().page, nil
		},
	}
	if f.monitoringDisabled {
		api.Monitoring = nil
	}
	return api
}

// ttlStep captures a TTL step of /local/probe/c6 with the given payload.
func ttlStep(c *qt.C, payload string) *protocol.Envelope {
	c.Helper()
	model := tablecreate.ColumnTTLModel()
	normalized, err := tablecreate.NormalizeColumnTTLAlteration(&protocol.ValueInput{Id: "step", Representation: protocol.ValueInput_OPERATION, Value: &protocol.Envelope{
		ProviderId: schema.ProviderID, Kind: tablecreate.ColumnTTLOperationKind, Owner: &protocol.Reference{Engine: "ydb", Kind: "table", Database: "/local", Schema: "probe", Name: "c6"},
		SchemaVersion: 1, Encoding: model.Encoding, DefinitionSha256: model.DefinitionSha256, Payload: []byte(payload)}})
	c.Assert(err, qt.IsNil)
	return normalized.Value
}

// TestColumnTableTTLStep_HappyPath reads the table, runs the step's statement
// once, and confirms the TTL the table reads back with.
func TestColumnTableTTLStep_HappyPath(t *testing.T) {
	read := []string{"describe /local/probe/c6", "monitoring /local/probe/c6"}
	for _, test := range []struct {
		name, payload, statement string
		before, after            func(hourly, none, twoHours ttlState) ttlState
	}{
		{"a reset", `{"held":` + hourlyTTL + `}`, "ALTER TABLE `/local/probe/c6` RESET (TTL);",
			func(hourly, _, _ ttlState) ttlState { return hourly }, func(_, none, _ ttlState) ttlState { return none }},
		{"a set on a table without a TTL", `{"set":` + twoHourTTL + `}`,
			"ALTER TABLE `/local/probe/c6` SET (TTL = Interval(\"PT7200S\") DELETE ON `ts`);",
			func(_, none, _ ttlState) ttlState { return none }, func(_, _, twoHours ttlState) ttlState { return twoHours }},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := qt.New(t)
			hourly, none, twoHours := ttlStates(c)
			fixture := &ttlFixture{before: test.before(hourly, none, twoHours), after: test.after(hourly, none, twoHours)}
			step := ttlStep(c, test.payload)
			original := proto.CloneOf(step)
			dispatched, err := fixture.api().Apply(t.Context(), "/local", step, func(context.Context) error {
				fixture.events = append(fixture.events, "guard")
				return nil
			})
			c.Assert(err, qt.IsNil)
			c.Assert(dispatched, qt.IsTrue)
			c.Assert(fixture.statement, qt.Equals, test.statement)
			c.Assert(fixture.events, qt.DeepEquals, append(append(append([]string{"open"}, read...), "guard", "execute"), append(read, "close")...))
			c.Assert(proto.Equal(step, original), qt.IsTrue)
		})
	}
}

// TestColumnTableTTLStep_FailurePath refuses a step before the statement runs
// when the table holds another TTL than the plan, or cannot take the TTL, and
// reports a failure after dispatch as such.
func TestColumnTableTTLStep_FailurePath(t *testing.T) {
	for _, test := range []struct {
		name, payload, database, want string
		modify                        func(*ttlFixture)
		guard                         error
		dispatched                    bool
	}{
		{"another TTL than the plan", `{"held":` + twoHourTTL + `}`, "/local", `YDB column table TTL differs from the plan`, func(*ttlFixture) {}, nil, false},
		{"a TTL the plan did not see", `{"set":` + twoHourTTL + `}`, "/local", `YDB column table TTL differs from the plan`, func(*ttlFixture) {}, nil, false},
		{"a TTL column after the first key column", `{"held":` + hourlyTTL + `,"set":{"enabled":{"column":"id","columnUnit":"1","tiers":[{"applyAfterSeconds":"60","delete":{}}]}}}`,
			"/local", `YDB TTL column "id" must be first in the primary key of the column table`, func(*ttlFixture) {}, nil, false},
		{"an absent TTL column", `{"held":` + hourlyTTL + `,"set":{"enabled":{"column":"gone","tiers":[{"applyAfterSeconds":"60","delete":{}}]}}}`,
			"/local", `YDB TTL column is absent`, func(*ttlFixture) {}, nil, false},
		{"a unit on a date column", `{"held":` + hourlyTTL + `,"set":{"enabled":{"column":"ts","columnUnit":"1","tiers":[{"applyAfterSeconds":"60","delete":{}}]}}}`,
			"/local", `YDB TTL column "ts" is Timestamp, a date type, and YDB takes no unit for one`, func(*ttlFixture) {}, nil, false},
		{"another database", `{"held":` + hourlyTTL + `}`, "/other", `YDB column table TTL change targets another database`, func(*ttlFixture) {}, nil, false},
		{"no monitoring endpoint", `{"held":` + hourlyTTL + `}`, "/local", `creating a column table or changing its TTL requires monitoring=.*`,
			func(f *ttlFixture) { f.monitoringDisabled = true }, nil, false},
		{"a lost read", `{"held":` + hourlyTTL + `}`, "/local", `lost read`, func(f *ttlFixture) { f.readFailure = true }, nil, false},
		{"a lost lease", `{"held":` + hourlyTTL + `}`, "/local", `lease lost`, func(*ttlFixture) {}, errors.New("lease lost"), false},
		{"a lost reply", `{"held":` + hourlyTTL + `}`, "/local", `lost reply`, func(f *ttlFixture) { f.executeFailure = true }, nil, true},
		{"a TTL that does not read back", `{"held":` + hourlyTTL + `}`, "/local", `YDB column table TTL differs from the request after the change`,
			func(f *ttlFixture) { f.after = f.before }, nil, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := qt.New(t)
			hourly, none, _ := ttlStates(c)
			fixture := &ttlFixture{before: hourly, after: none}
			test.modify(fixture)
			dispatched, err := fixture.api().Apply(t.Context(), test.database, ttlStep(c, test.payload), func(context.Context) error { return test.guard })
			c.Assert(err, qt.ErrorMatches, test.want)
			c.Assert(dispatched, qt.Equals, test.dispatched)
			c.Assert(fixture.executed, qt.Equals, test.dispatched)
		})
	}
}
