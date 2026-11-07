package tablecreate_test

import (
	"testing"

	qt "github.com/frankban/quicktest"

	"github.com/stokaro/ptah-ydb-plugin/internal/tablecreate"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
)

// A new column table takes an n-gram index at each end of the bounds YDB
// holds it to: an n-gram size of 3 or 8, and a false-positive probability
// of 0.003, which takes eight hashes, the most YDB builds. The index is
// written with the setting as declared.
//
// Ported from the host's TestResolveLocal_NgramAcceptedBounds.
func TestCompileColumnTableLocalIndexes_AcceptsTheBounds(t *testing.T) {
	for _, test := range []struct{ name, index, clause string }{
		{"the smallest n-gram size", `{"name":"ng","columns":["body"],"ngram":{"ngramSize":"3"}}`,
			"INDEX `ng` LOCAL USING bloom_ngram_filter ON (`body`) WITH (case_sensitive = true, false_positive_probability = 0.1, ngram_size = 3)"},
		{"the largest n-gram size", `{"name":"ng","columns":["body"],"ngram":{"ngramSize":"8"}}`,
			"INDEX `ng` LOCAL USING bloom_ngram_filter ON (`body`) WITH (case_sensitive = true, false_positive_probability = 0.1, ngram_size = 8)"},
		{"the probability that takes the most hashes", `{"name":"ng","columns":["body"],"ngram":{"falsePositiveProbability":0.003}}`,
			"INDEX `ng` LOCAL USING bloom_ngram_filter ON (`body`) WITH (case_sensitive = true, false_positive_probability = 0.003, ngram_size = 3)"},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			created, err := tablecreate.CompileColumnTable(owner(), columnDeclaration(c, `{"localIndexes":[`+test.index+`]}`), localIndexCapabilities)
			c.Assert(err, qt.IsNil)
			c.Assert(tablecreate.ColumnTableStatement("/local/app/records", created), qt.Contains, test.clause)
		})
	}
}
