package changefeeds_test

import (
	"testing"

	qt "github.com/frankban/quicktest"

	"github.com/stokaro/ptah-ydb-plugin/internal/changefeeds"
	native "github.com/stokaro/ptah-ydb-plugin/internal/nativeprotocol"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
	"github.com/stokaro/ptah-ydb-plugin/internal/ydbdecl"
)

// heldDisabled is a read set holding the disabled changefeed held.
func heldDisabled() *native.ChangefeedObservations {
	set := held("held")
	set.Changefeeds[0].Description.State = native.ChangefeedDescription_STATE_DISABLED
	return set
}

// declaredDisabled declares specs, the one named held disabled, as a
// declaration converted from a read of it states it.
func declaredDisabled(c *qt.C, specs ...ydbdecl.Changefeed) *native.ChangefeedSetIntent {
	c.Helper()
	set := declared(c, specs...)
	for _, intent := range set.GetChangefeeds() {
		intent.Disabled = intent.GetName() == "held"
	}
	return set
}

// A disabled changefeed the table holds and the declaration states disabled
// stays where it is: a changefeed added beside it is the only change, and a
// changed retention of its topic is changed in place by one ALTER TOPIC,
// never a drop and an addition. Ported from the host's
// TestGenerateMigrationAST_Changefeeds_PreservesDisabledStateInPlace.
func TestDiff_KeepsADisabledChangefeedInPlace(t *testing.T) {
	updates := func(name string) ydbdecl.Changefeed {
		return ydbdecl.Changefeed{Name: name, Mode: "UPDATES", Format: "JSON"}
	}
	longer := updates("held")
	longer.RetentionPeriod = "PT6H"
	for _, test := range []struct {
		name       string
		desired    []ydbdecl.Changefeed
		want       []step
		statements []string
	}{
		{name: "another changefeed added", desired: []ydbdecl.Changefeed{updates("held"), updates("fresh")},
			want:       []step{{Action: changefeeds.Add, Name: "fresh"}},
			statements: []string{"ALTER TABLE `items` ADD CHANGEFEED `fresh` WITH (MODE = 'UPDATES', FORMAT = 'JSON');"}},
		{name: "its topic's retention changed", desired: []ydbdecl.Changefeed{longer},
			want:       []step{{Action: changefeeds.Topic, Name: "held"}},
			statements: []string{"ALTER TOPIC `items/held` SET (retention_period = Interval('PT6H'));"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)

			planned, undecided := changefeeds.Diff(declaredDisabled(c, test.desired...), heldDisabled())

			c.Assert(undecided, qt.Equals, "")
			c.Assert(steps(planned), qt.DeepEquals, test.want)
			statements, err := changefeeds.Statements("items", planned)
			c.Assert(err, qt.IsNil)
			c.Assert(statements, qt.DeepEquals, test.statements)
		})
	}
}
