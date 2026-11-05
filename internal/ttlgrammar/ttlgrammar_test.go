package ttlgrammar_test

import (
	"errors"
	"testing"

	qt "github.com/frankban/quicktest"

	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
	"github.com/stokaro/ptah-ydb-plugin/internal/ttlgrammar"
)

// The intervals YDB takes, and the seconds it keeps of each.
func TestIntervalSeconds_HappyPath(t *testing.T) {
	tests := []struct {
		interval string
		want     uint64
	}{
		{"P1D", 86400},
		{"PT1H", 3600},
		{"PT90M", 5400},
		{"PT3600S", 3600},
		{"P1W", 604800},
		{"P2W3D", 1468800},
		{"P1DT2H30M15S", 95415},
		{"PT0S", 0},
		{"P0D", 0},
		{"PT1.0S", 1},
		{"PT1000000000S", 1000000000},
		{"P49672DT23H59M59S", ttlgrammar.MaxSeconds},
	}
	for _, test := range tests {
		t.Run(test.interval, func(t *testing.T) {
			c := testcheck.New(t)
			got, err := ttlgrammar.IntervalSeconds(test.interval)
			c.Assert(err, qt.IsNil)
			c.Assert(got, qt.Equals, test.want)
		})
	}
}

// The spellings YDB refuses, and a fraction of a second, which it would keep as
// fewer seconds than were declared.
func TestIntervalSeconds_FailurePath(t *testing.T) {
	tests := []struct {
		interval string
		wantErr  string
	}{
		{"30 days", `.*it does not begin with P`},
		{"P1M", `.*designator "M" is out of place.*`},
		{"P1Y", `.*designator "Y" is out of place.*`},
		{"p1d", `.*it does not begin with P`},
		{"P1D ", `.*" " is not a number followed by a designator`},
		{"-P1D", `.*it does not begin with P`},
		{"P", `.*it names no weeks, days, hours, minutes or seconds`},
		{"PT", `.*T is not followed by hours, minutes or seconds`},
		{"P1DT", `.*T is not followed by hours, minutes or seconds`},
		{"PT1H1H", `.*designator "H" is out of place.*`},
		{"P1D1W", `.*designator "W" is out of place.*`},
		{"P1.5D", `.*"1.5" is not a number YDB takes there`},
		{"PT1.5S", `interval "PT1.5S" has a fraction of a second.*`},
		{"PT0.000001S", `interval "PT0.000001S" has a fraction of a second.*`},
		{"P49673D", `interval "P49673D" is longer than YDB's Interval type holds.*`},
	}
	for _, test := range tests {
		t.Run(test.interval, func(t *testing.T) {
			c := testcheck.New(t)
			got, err := ttlgrammar.IntervalSeconds(test.interval)
			c.Assert(err, qt.ErrorMatches, test.wantErr)
			c.Assert(got, qt.Equals, uint64(0))
		})
	}
}

func TestUnit_HappyPath(t *testing.T) {
	tests := []struct{ declared, want string }{
		{"", ""},
		{"seconds", ttlgrammar.Seconds},
		{" Milliseconds ", ttlgrammar.Milliseconds},
		{"MICROSECONDS", ttlgrammar.Microseconds},
		{"nanoseconds", ttlgrammar.Nanoseconds},
	}
	for _, test := range tests {
		t.Run(test.declared, func(t *testing.T) {
			c := testcheck.New(t)
			got, err := ttlgrammar.Unit(test.declared)
			c.Assert(err, qt.IsNil)
			c.Assert(got, qt.Equals, test.want)
		})
	}
}

func TestUnit_FailurePath(t *testing.T) {
	c := testcheck.New(t)
	got, err := ttlgrammar.Unit("ms")
	c.Assert(err, qt.ErrorMatches, `unit "ms" is not one YDB takes: use SECONDS, MILLISECONDS, MICROSECONDS, NANOSECONDS`)
	c.Assert(got, qt.Equals, "")
}

// A refusal says the same as its text in its reason, without the value the
// declaration wrote.
func TestIntervalSeconds_FailurePathReason(t *testing.T) {
	tests := []struct{ interval, reason string }{
		{"30 days", "the interval is not an ISO 8601 duration YDB takes (such as P30D or PT1H30M): it does not begin with P"},
		{"P", "the interval is not an ISO 8601 duration YDB takes (such as P30D or PT1H30M): it names no weeks, days, hours, minutes or seconds"},
		{"PT", "the interval is not an ISO 8601 duration YDB takes (such as P30D or PT1H30M): the T is not followed by hours, minutes or seconds"},
		{"P1D ", "the interval is not an ISO 8601 duration YDB takes (such as P30D or PT1H30M): a part is not a number followed by a designator"},
		{"P1Y", "the interval is not an ISO 8601 duration YDB takes (such as P30D or PT1H30M): " +
			"a designator is out of place or not one YDB takes (weeks, days, hours, minutes, seconds)"},
		{"P1.5D", "the interval is not an ISO 8601 duration YDB takes (such as P30D or PT1H30M): a number has a fraction where YDB takes a whole number"},
		{"PT1..5S", "the interval is not an ISO 8601 duration YDB takes (such as P30D or PT1H30M): a number is not one YDB takes there"},
		{"PT.5S", "the interval is not an ISO 8601 duration YDB takes (such as P30D or PT1H30M): a number has no whole part"},
		{"PT99999999999999999999S", "the interval is not an ISO 8601 duration YDB takes (such as P30D or PT1H30M): a number is too large"},
		{"PT0.5S", "the interval has a fraction of a second, and YDB keeps whole seconds only (PT1.5S reads back as PT1S)"},
		{"P49673D", "the interval is longer than YDB's Interval type holds (P49672DT23H59M59S)"},
	}
	for _, test := range tests {
		t.Run(test.interval, func(t *testing.T) {
			c := testcheck.New(t)
			got, err := ttlgrammar.IntervalSeconds(test.interval)
			c.Assert(got, qt.Equals, uint64(0))
			refused, grammar := errors.AsType[*ttlgrammar.Error](err)
			c.Assert(grammar, qt.IsTrue, qt.Commentf("%v", err))
			c.Assert(refused.Reason(), qt.Equals, test.reason)
		})
	}
}

// A unit's refusal names the units YDB takes, without the one declared.
func TestUnit_FailurePathReason(t *testing.T) {
	c := testcheck.New(t)
	_, err := ttlgrammar.Unit("ms")
	refused, grammar := errors.AsType[*ttlgrammar.Error](err)
	c.Assert(grammar, qt.IsTrue)
	c.Assert(refused.Reason(), qt.Equals, "the unit is not one YDB takes: use SECONDS, MILLISECONDS, MICROSECONDS, NANOSECONDS")
}

// Within puts a prefix in front of the text and the reason alike.
func TestError_Within(t *testing.T) {
	c := testcheck.New(t)
	_, err := ttlgrammar.IntervalSeconds("PT0.5S")
	refused, grammar := errors.AsType[*ttlgrammar.Error](err)
	c.Assert(grammar, qt.IsTrue)
	within := refused.Within("TTL tier 2: ")
	c.Assert(within.Error(), qt.Equals, `TTL tier 2: interval "PT0.5S" has a fraction of a second, and YDB keeps whole seconds only (PT1.5S reads back as PT1S)`)
	c.Assert(within.Reason(), qt.Equals, "TTL tier 2: the interval has a fraction of a second, and YDB keeps whole seconds only (PT1.5S reads back as PT1S)")
}
