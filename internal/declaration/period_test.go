package declaration_test

import (
	qt "github.com/frankban/quicktest"
	"github.com/stokaro/ptah-ydb-plugin/internal/declaration"
	"testing"
)

func TestCoordinationPeriodsRemainExact(t *testing.T) {
	for _, test := range []struct {
		source string
		want   uint32
	}{{"PT0.5S", 500}, {"PT1.001000S", 1001}, {"P0DT0H0M2S", 2000}, {" PT1S ", 1000}, {"P1W", 604800000}, {"PT4294967.295S", 4294967295}} {
		c := qt.New(t)
		got, err := declaration.CoordinationPeriod(test.source)
		c.Assert(err, qt.IsNil)
		c.Assert(got, qt.Equals, test.want)
	}
	for _, source := range []string{"P", "PT", "P1DT", "-PT1S", "PT0S", "PT0.000001S", "PT1.0000001S", "PT4294967.296S", "P999999999999999999999W", "1s", "PTNaNS"} {
		c := qt.New(t)
		_, err := declaration.CoordinationPeriod(source)
		c.Assert(err, qt.IsNotNil, qt.Commentf("period %s", source))
	}
}
