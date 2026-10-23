package ttlgrammar_test

import (
	"testing"

	qt "github.com/frankban/quicktest"
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
			c := qt.New(t)
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
			c := qt.New(t)
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
			c := qt.New(t)
			got, err := ttlgrammar.Unit(test.declared)
			c.Assert(err, qt.IsNil)
			c.Assert(got, qt.Equals, test.want)
		})
	}
}

func TestUnit_FailurePath(t *testing.T) {
	c := qt.New(t)
	got, err := ttlgrammar.Unit("ms")
	c.Assert(err, qt.ErrorMatches, `unit "ms" is not one YDB takes: use SECONDS, MILLISECONDS, MICROSECONDS, NANOSECONDS`)
	c.Assert(got, qt.Equals, "")
}
