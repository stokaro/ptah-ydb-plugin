package featureflags_test

import (
	"testing"

	qt "github.com/frankban/quicktest"
	"github.com/stokaro/ptah-ydb-plugin/internal/featureflags"
)

func TestStreamingNeedsBothClusterFlags(t *testing.T) {
	for _, test := range []struct {
		name  string
		flags featureflags.Flags
		want  bool
	}{
		{"both on", featureflags.Flags{"EnableStreamingQueries": true, "EnableExternalDataSources": true}, true},
		{"external sources off", featureflags.Flags{"EnableStreamingQueries": true}, false},
		{"streaming off", featureflags.Flags{"EnableExternalDataSources": true}, false},
		{"unknown flags", featureflags.Flags{}, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := qt.New(t)
			preset := map[string]bool{"streaming_queries": false}
			c.Assert(test.flags.Refine(preset)["streaming_queries"], qt.Equals, test.want)
			c.Assert(preset["streaming_queries"], qt.IsFalse)
		})
	}
}
