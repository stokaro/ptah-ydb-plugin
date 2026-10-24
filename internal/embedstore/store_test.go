package embedstore_test

import (
	"testing"

	qt "github.com/frankban/quicktest"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"

	"github.com/stokaro/ptah-ydb-plugin/internal/embedstore"
)

// What a line does with vectors follows the host's YDB presets: 25.1 has no
// 64-bit timestamps and builds a vector index only with EnableVectorIndex on,
// 25.2 builds one whose search stands at the build, and 25.3 and newer take in
// later writes. A version the store cannot read is the newest line's.
func TestLineCapabilities(t *testing.T) {
	for _, test := range []struct {
		version string
		flags   map[string]bool
		want    embedstore.Capabilities
	}{
		{version: "25.1.4.7", want: embedstore.Capabilities{}},
		{version: "25.1.4.7", flags: map[string]bool{"EnableVectorIndex": true}, want: embedstore.Capabilities{VectorIndexes: true}},
		{version: "25.2.1.24", want: embedstore.Capabilities{WideTimestamps: true, VectorIndexes: true}},
		{version: "stable-25-4-1", want: embedstore.Capabilities{WideTimestamps: true, VectorIndexes: true, MaintainedOnWrite: true}},
		{version: "26.2.1.14", want: embedstore.Capabilities{WideTimestamps: true, VectorIndexes: true, MaintainedOnWrite: true}},
		{version: "26.2.1.14", flags: map[string]bool{"EnableVectorIndex": false}, want: embedstore.Capabilities{WideTimestamps: true, MaintainedOnWrite: true}},
		{version: "trunk", want: embedstore.Capabilities{WideTimestamps: true, VectorIndexes: true, MaintainedOnWrite: true}},
	} {
		t.Run(test.version, func(t *testing.T) {
			c := qt.New(t)
			c.Assert(embedstore.LineCapabilities(test.version, test.flags), qt.Equals, test.want)
		})
	}
}

// A refusal's text is one line of at most 512 bytes, as the contract
// requires of a refusal.
func TestDescribeTargetRefusesInOneLine(t *testing.T) {
	c := qt.New(t)
	spec := &protocol.EmbeddingSpec{Generation: "g", Target: &protocol.EmbeddingTarget{Representation: "uint8\nbits", Table: &protocol.Reference{Name: "t"}},
		Source: &protocol.EmbeddingSource{Table: &protocol.Reference{Name: "s"}}}

	refusal := embedstore.DescribeTarget(spec).GetRefusal()

	c.Assert(refusal.GetReason(), qt.Equals, protocol.EmbeddingRefusal_UNSUPPORTED)
	c.Assert(refusal.GetMessage(), qt.Not(qt.Contains), "\n")
	c.Assert(len(refusal.GetMessage()) <= 512, qt.IsTrue)
}
