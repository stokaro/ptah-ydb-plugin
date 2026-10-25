package backend

import protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"

// queryDialect is the spelling the host's query builder reads for YDB, the
// values of the host's YDB dialect descriptor (internal/builtindialects):
//
//   - parameters are named $p1, $p2 and so on, the names YQL declares;
//   - every name is quoted as YQL quotes it, in backticks with a backtick or a
//     backslash inside escaped by a backslash, and a qualified name is one
//     path inside one pair of backticks, because YDB has no schemas;
//   - the builder decides a WITH clause, a correlated subquery, a join
//     condition other than column equalities and an OFFSET without a LIMIT by
//     the target's capabilities, and binds LIMIT and OFFSET as unsigned
//     integers, which YQL types them as;
//   - an upsert is YQL's UPSERT INTO, and an ON CONFLICT clause is refused.
//
// The default capabilities are what the builder assumes for YDB when its
// caller names no target: the host's YDB 26.2 preset for the keys the builder
// reads. internal/rowdata/testdata/query-dialect.golden.json holds the host's
// answer, and a test compares the two.
func queryDialect() *protocol.QueryDialect {
	return &protocol.QueryDialect{
		Placeholder: "dollar-named",
		Quoting:     &protocol.QueryQuoting{Style: "yql", Bare: "never", Paths: true},
		Query: &protocol.QuerySpelling{
			ChecksCapabilities: true,
			UnsignedBounds:     true,
			Upsert:             "upsert-into",
			UpsertRefusal: "YDB has no ON CONFLICT: UpsertInto writes each row over the row with the same " +
				"primary key, and the server refuses INSERT OR IGNORE",
		},
		DefaultCapabilities: map[string]bool{
			"common_table_expressions": false,
			"correlated_subqueries":    false,
			"non_equi_joins":           false,
			"offset_without_limit":     false,
			"returning_clause":         true,
		},
	}
}
