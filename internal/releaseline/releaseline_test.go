package releaseline_test

import (
	"testing"

	qt "github.com/frankban/quicktest"
	"github.com/stokaro/ptah-ydb-plugin/internal/releaseline"
)

func TestAtLeast_HappyPath(t *testing.T) {
	for _, test := range []struct {
		version string
		want    bool
	}{
		{"25.1.4.7", false}, {"25.3.1.25", false}, {"25.4.1.15", true}, {"26.2.1.14", true}, {"stable-25-4-1", true}, {"stable-25-3", false}, {"main", true},
	} {
		t.Run(test.version, func(t *testing.T) {
			c := qt.New(t)
			c.Assert(releaseline.AtLeast(test.version, 25, 4), qt.Equals, test.want)
		})
	}
}

func TestParse_FailurePath(t *testing.T) {
	c := qt.New(t)
	major, minor, ok := releaseline.Parse("26")
	c.Assert(ok, qt.IsFalse)
	c.Assert(major, qt.Equals, 0)
	c.Assert(minor, qt.Equals, 0)
}
