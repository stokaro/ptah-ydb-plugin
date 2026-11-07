package coordinationsql_test

import (
	"testing"

	qt "github.com/frankban/quicktest"

	"github.com/stokaro/ptah-ydb-plugin/internal/coordinationsql"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
	"github.com/stokaro/ptah-ydb-plugin/internal/ydbdecl"
)

// every states each setting a node has.
var every = ydbdecl.CoordinationNode{SelfCheckPeriodMillis: 2_000, SessionGracePeriodMillis: 15_000, ReadConsistencyMode: "strict",
	AttachConsistencyMode: "relaxed", RateLimiterCountersMode: "detailed"}

// The statements the host's embedded engine writes, as `ptah schema render
// --dialect ydb` of fixture 049 writes them.
func TestStatementText_HappyPath(t *testing.T) {
	for _, tc := range []struct {
		name      string
		statement coordinationsql.Statement
		want      string
	}{
		{name: "a node with its defaults", statement: coordinationsql.Statement{Verb: coordinationsql.Create, Path: "app_locks"},
			want: "CREATE COORDINATION NODE `app_locks`;"},
		{name: "a node with every setting", statement: coordinationsql.Statement{Verb: coordinationsql.Create, Path: "app/limits", Settings: every},
			want: "CREATE COORDINATION NODE `app/limits` WITH (self_check_period = Interval('PT2S'), session_grace_period = Interval('PT15S'), " +
				"read_consistency_mode = 'strict', attach_consistency_mode = 'relaxed', rate_limiter_counters_mode = 'detailed');"},
		{name: "a change of the settings that differ",
			statement: coordinationsql.Statement{Verb: coordinationsql.Alter, Path: "app/limits", Settings: ydbdecl.CoordinationNode{SelfCheckPeriodMillis: 500}},
			want:      "ALTER COORDINATION NODE `app/limits` SET (self_check_period = Interval('PT0.5S'));"},
		{name: "a removal", statement: coordinationsql.Statement{Verb: coordinationsql.Drop, Path: "app/limits"},
			want: "DROP COORDINATION NODE `app/limits`;"},
		// Ported from the host's TestStatementText_HappyPath: a backtick in
		// the path is escaped inside the quoted name.
		{name: "a removal of a name that needs escaping", statement: coordinationsql.Statement{Verb: coordinationsql.Drop, Path: "dir/tick`name"},
			want: "DROP COORDINATION NODE `dir/tick\\`name`;"},
		// Ported from the host's TestRenderSQL_YDBWritesCoordinationNodeStatements:
		// a dotted name at the root stays one quoted name, and a node named
		// like Ptah's lock node inside a directory is an ordinary node.
		{name: "a root node under a dotted name", statement: coordinationsql.Statement{Verb: coordinationsql.Create, Path: "app.locks"},
			want: "CREATE COORDINATION NODE `app.locks`;"},
		{name: "a removal of a node named like the lock node in a directory",
			statement: coordinationsql.Statement{Verb: coordinationsql.Drop, Path: "app/ptah_locks"},
			want:      "DROP COORDINATION NODE `app/ptah_locks`;"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := testcheck.New(t)
			got, err := tc.statement.Text()
			c.Assert(err, qt.IsNil)
			c.Assert(got, qt.Equals, tc.want)
		})
	}
}

// A period is written as the ISO 8601 text the setting reads back to the same
// milliseconds, a fraction, minutes and the longest period a node keeps
// included. Ported from the host's TestPeriodText_IsWhatParsePeriodReads.
func TestStatementTextWritesPeriodsTheSettingReadsBack(t *testing.T) {
	for _, tc := range []struct {
		millis uint32
		want   string
	}{
		{millis: 500, want: "PT0.5S"},
		{millis: 1_000, want: "PT1S"},
		{millis: 1_001, want: "PT1.001S"},
		{millis: 90_000, want: "PT1M30S"},
		{millis: 4_294_967_295, want: "P49DT17H2M47.295S"},
	} {
		t.Run(tc.want, func(t *testing.T) {
			c := testcheck.New(t)
			statement := coordinationsql.Statement{Verb: coordinationsql.Alter, Path: "n",
				Settings: ydbdecl.CoordinationNode{SessionGracePeriodMillis: tc.millis}}
			text, err := statement.Text()
			c.Assert(err, qt.IsNil)
			c.Assert(text, qt.Equals, "ALTER COORDINATION NODE `n` SET (session_grace_period = Interval('"+tc.want+"'));")
			var read ydbdecl.CoordinationNode
			err = ydbdecl.SetCoordinationSetting(&read, ydbdecl.SettingSessionGracePeriod, tc.want)
			c.Assert(err, qt.IsNil)
			c.Assert(read.SessionGracePeriodMillis, qt.Equals, tc.millis)
		})
	}
}

func TestStatementText_FailurePath(t *testing.T) {
	for _, tc := range []struct {
		name      string
		statement coordinationsql.Statement
		want      string
	}{
		{name: "an unknown verb", statement: coordinationsql.Statement{Path: "n"}, want: "invalid coordination node statement: unknown verb 0"},
		{name: "a change of nothing", statement: coordinationsql.Statement{Verb: coordinationsql.Alter, Path: "n"},
			want: "invalid coordination node statement: ALTER COORDINATION NODE n names no setting to change"},
		{name: "a removal with settings", statement: coordinationsql.Statement{Verb: coordinationsql.Drop, Path: "n", Settings: every},
			want: "invalid coordination node statement: DROP COORDINATION NODE n takes no setting"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := testcheck.New(t)
			got, err := tc.statement.Text()
			c.Assert(err, qt.ErrorIs, coordinationsql.ErrStatement)
			c.Assert(err, qt.ErrorMatches, tc.want)
			c.Assert(got, qt.Equals, "")
		})
	}
}

// Every statement Text writes reads back to itself.
func TestRecognizeReadsWhatTextWrites(t *testing.T) {
	for _, statement := range []coordinationsql.Statement{
		{Verb: coordinationsql.Create, Path: "app_locks"},
		{Verb: coordinationsql.Create, Path: "app/limits", Settings: every},
		{Verb: coordinationsql.Alter, Path: "app/limits", Settings: ydbdecl.CoordinationNode{SessionGracePeriodMillis: 2_500, RateLimiterCountersMode: "aggregated"}},
		{Verb: coordinationsql.Drop, Path: "/local/app/limits"},
		// Ported from the host's TestRecognize_ReadsWhatTextWrites: a dot, a
		// backtick and a backslash in a name survive the round trip.
		{Verb: coordinationsql.Create, Path: "app/sub/locks", Settings: ydbdecl.CoordinationNode{SelfCheckPeriodMillis: 750,
			SessionGracePeriodMillis: 30_000, ReadConsistencyMode: "strict", AttachConsistencyMode: "relaxed", RateLimiterCountersMode: "detailed"}},
		{Verb: coordinationsql.Alter, Path: "a.b", Settings: ydbdecl.CoordinationNode{SessionGracePeriodMillis: 12_000}},
		{Verb: coordinationsql.Drop, Path: "tick`back\\slash"},
	} {
		t.Run(statement.Path, func(t *testing.T) {
			c := testcheck.New(t)
			text, err := statement.Text()
			c.Assert(err, qt.IsNil)
			query, recognized, err := coordinationsql.Recognize(text)
			c.Assert(err, qt.IsNil)
			c.Assert(recognized, qt.IsTrue)
			c.Assert(query, qt.DeepEquals, coordinationsql.Query{Statement: statement})
		})
	}
}

func TestRecognize_HappyPath(t *testing.T) {
	for _, tc := range []struct {
		name string
		text string
		want coordinationsql.Query
	}{
		{name: "in any case, with a trailing comment", text: "create Coordination node `n` -- note\n",
			want: coordinationsql.Query{Statement: coordinationsql.Statement{Verb: coordinationsql.Create, Path: "n"}}},
		{name: "after a path prefix and definitions",
			text: "--!syntax_v1\nPRAGMA TablePathPrefix(\"/local/app\");\nDECLARE $x AS Int32;\n$y = 1;\nDROP COORDINATION NODE `limits`;",
			want: coordinationsql.Query{Statement: coordinationsql.Statement{Verb: coordinationsql.Drop, Path: "limits"}, PathPrefix: "/local/app"}},
		{name: "the prefix spelled with an equals sign", text: "PRAGMA TablePathPrefix = '/local/app'; DROP COORDINATION NODE n;",
			want: coordinationsql.Query{Statement: coordinationsql.Statement{Verb: coordinationsql.Drop, Path: "n"}, PathPrefix: "/local/app"}},
		{name: "settings with double quotes and the Utf8 suffix",
			text: `ALTER COORDINATION NODE n SET (read_consistency_mode = "strict"u, self_check_period = Interval("PT3S"))`,
			want: coordinationsql.Query{Statement: coordinationsql.Statement{Verb: coordinationsql.Alter, Path: "n",
				Settings: ydbdecl.CoordinationNode{ReadConsistencyMode: "strict", SelfCheckPeriodMillis: 3_000}}}},
		{name: "a change of the grace period alone, checked against the node later",
			text: "ALTER COORDINATION NODE n SET (session_grace_period = Interval('PT1.5S'))",
			want: coordinationsql.Query{Statement: coordinationsql.Statement{Verb: coordinationsql.Alter, Path: "n",
				Settings: ydbdecl.CoordinationNode{SessionGracePeriodMillis: 1_500}}}},
		{name: "a self-check period alone at its upper bound", text: "ALTER COORDINATION NODE n SET (self_check_period = Interval('PT10S'))",
			want: coordinationsql.Query{Statement: coordinationsql.Statement{Verb: coordinationsql.Alter, Path: "n",
				Settings: ydbdecl.CoordinationNode{SelfCheckPeriodMillis: 10_000}}}},
		// Ported from the host's TestRecognize_HappyPath: setting names, the
		// Interval function and a mode are read in any case, and of two
		// prefix pragmas the last one wins.
		{name: "setting names and values in any case",
			text: "ALTER COORDINATION NODE `l` SET (Read_Consistency_Mode = \"Strict\"u, SELF_CHECK_PERIOD = interval(\"PT2S\"))",
			want: coordinationsql.Query{Statement: coordinationsql.Statement{Verb: coordinationsql.Alter, Path: "l",
				Settings: ydbdecl.CoordinationNode{ReadConsistencyMode: "strict", SelfCheckPeriodMillis: 2_000}}}},
		{name: "the last of two prefix pragmas",
			text: "PRAGMA TablePathPrefix('/local/a'); PRAGMA tablepathprefix = '/local/b'; CREATE COORDINATION NODE n",
			want: coordinationsql.Query{Statement: coordinationsql.Statement{Verb: coordinationsql.Create, Path: "n"}, PathPrefix: "/local/b"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := testcheck.New(t)
			query, recognized, err := coordinationsql.Recognize(tc.text)
			c.Assert(err, qt.IsNil)
			c.Assert(recognized, qt.IsTrue)
			c.Assert(query, qt.DeepEquals, tc.want)
		})
	}
}

// Text that holds none of the statements is not recognized, even where it
// mentions one.
func TestRecognizeLeavesOtherText(t *testing.T) {
	for _, text := range []string{
		"SELECT 1;",
		"SELECT 'CREATE COORDINATION NODE n';",
		"-- DROP COORDINATION NODE n\nSELECT 1;",
		"CREATE TABLE coordination (id Uint64, PRIMARY KEY (id));",
		"",
		// Ported from the host's TestRecognize_LeavesOtherTextAlone. The
		// multiline string holds a semicolon ahead of the statement, so the
		// row fails if the literal is split there.
		"CREATE TABLE coordination_node (id Int64 NOT NULL, PRIMARY KEY (id))",
		"UPSERT INTO t (note) VALUES (@@a note;\nDROP COORDINATION NODE n@@)",
		"CREATE TOPIC coordination",
	} {
		t.Run(text, func(t *testing.T) {
			c := testcheck.New(t)
			query, recognized, err := coordinationsql.Recognize(text)
			c.Assert(err, qt.IsNil)
			c.Assert(recognized, qt.IsFalse)
			c.Assert(query, qt.DeepEquals, coordinationsql.Query{})
		})
	}
}

func TestRecognize_FailurePath(t *testing.T) {
	for _, tc := range []struct {
		name string
		text string
		want string
	}{
		{name: "two statements", text: "CREATE COORDINATION NODE a; DROP COORDINATION NODE b;",
			want: "a query runs one DROP COORDINATION NODE statement, and this one holds two; put each in a query of its own"},
		{name: "followed by another statement", text: "CREATE COORDINATION NODE a; SELECT 1;",
			want: "CREATE COORDINATION NODE is followed by another statement in its query, and Ptah runs it through YDB's coordination service, which takes it alone"},
		{name: "after a statement that is not a definition", text: "SELECT 1; CREATE COORDINATION NODE a;",
			want: "CREATE COORDINATION NODE shares its query with SELECT 1, and Ptah runs it through YDB's coordination service, which takes it alone"},
		{name: "a prefix it cannot read", text: "PRAGMA TablePathPrefix(\"/a\", \"b\"); CREATE COORDINATION NODE a;",
			want: `CREATE COORDINATION NODE follows a TablePathPrefix pragma Ptah cannot read; write it as PRAGMA TablePathPrefix\("/database/path"\)`},
		// Ported from the host's TestRecognize_FailurePath, with its words.
		{name: "a prefix that is not a plain string", text: "PRAGMA TablePathPrefix($p); CREATE COORDINATION NODE a",
			want: "CREATE COORDINATION NODE follows a TablePathPrefix pragma whose path is not a plain string"},
		{name: "no node", text: "DROP COORDINATION NODE", want: "DROP COORDINATION NODE names no node"},
		{name: "a string for the node", text: "DROP COORDINATION NODE 'a'",
			want: "DROP COORDINATION NODE 'a' does not name a node; write its path in backticks"},
		{name: "a named expression for the node", text: "DROP COORDINATION NODE $n", want: `DROP COORDINATION NODE \$n does not name a node; write its path in backticks`},
		{name: "a change without SET", text: "ALTER COORDINATION NODE n", want: `ALTER COORDINATION NODE n names no setting; write SET \(...\)`},
		{name: "a removal with settings", text: "DROP COORDINATION NODE n WITH (self_check_period = Interval('PT1S'))",
			want: "DROP COORDINATION NODE n is followed by WITH, which Ptah does not read"},
		{name: "a creation with SET", text: "CREATE COORDINATION NODE a SET (read_consistency_mode = 'strict')",
			want: "CREATE COORDINATION NODE a is followed by SET, which Ptah does not read"},
		{name: "settings without parentheses", text: "CREATE COORDINATION NODE n WITH self_check_period = Interval('PT1S')",
			want: "CREATE COORDINATION NODE n: the settings are written in parentheses"},
		{name: "an unknown setting", text: "CREATE COORDINATION NODE n WITH (period = Interval('PT1S'))",
			want: "CREATE COORDINATION NODE n: period is not a setting; the settings are self_check_period, session_grace_period, read_consistency_mode, attach_consistency_mode, rate_limiter_counters_mode"},
		{name: "a setting twice", text: "CREATE COORDINATION NODE n WITH (read_consistency_mode = 'strict', read_consistency_mode = 'relaxed')",
			want: "CREATE COORDINATION NODE n: read_consistency_mode is set twice"},
		{name: "no equals sign", text: "CREATE COORDINATION NODE n WITH (read_consistency_mode 'strict')",
			want: "CREATE COORDINATION NODE n: read_consistency_mode is followed by 'strict', not ="},
		{name: "a mode that is not a string", text: "CREATE COORDINATION NODE n WITH (read_consistency_mode = strict)",
			want: "CREATE COORDINATION NODE n: read_consistency_mode takes a string such as 'strict', not strict"},
		{name: "an escaped string as a mode", text: `ALTER COORDINATION NODE a SET (read_consistency_mode = 'str\x69ct')`,
			want: `ALTER COORDINATION NODE a: read_consistency_mode takes a string such as 'strict', not 'str\\x69ct'`},
		{name: "a period that is not an interval", text: "CREATE COORDINATION NODE n WITH (self_check_period = 'PT1S')",
			want: `CREATE COORDINATION NODE n: self_check_period takes an interval such as Interval\('PT1S'\)`},
		{name: "a mode YDB does not take", text: "CREATE COORDINATION NODE n WITH (read_consistency_mode = 'eventual')",
			want: `CREATE COORDINATION NODE n: read_consistency_mode "eventual": the mode is "strict" or "relaxed"`},
		{name: "a self-check period YDB moves", text: "ALTER COORDINATION NODE n SET (self_check_period = Interval('PT11S'))",
			want: "ALTER COORDINATION NODE n: self_check_period PT11S: YDB runs a node's self-check every PT0.5S to PT10S and moves a period outside that range into it"},
		{name: "a grace period above its range", text: "ALTER COORDINATION NODE a SET (session_grace_period = Interval('PT1M'))",
			want: `ALTER COORDINATION NODE a: session_grace_period PT1M: YDB runs a node with a grace period from the self-check period plus PT1S \(PT2S here\) to PT30S and moves a period outside that range into it`},
		{name: "a creation whose grace period the default self-check does not leave room for",
			text: "CREATE COORDINATION NODE n WITH (session_grace_period = Interval('PT1.5S'))",
			want: `CREATE COORDINATION NODE n: session_grace_period PT1.5S: YDB runs a node with a grace period from the self-check period plus PT1S \(PT2S here\) to PT30S and moves a period outside that range into it`},
		{name: "text after the settings", text: "CREATE COORDINATION NODE n WITH (read_consistency_mode = 'strict') x",
			want: "CREATE COORDINATION NODE n: the settings end with \\) and nothing follows it"},
		{name: "a trailing comma", text: "ALTER COORDINATION NODE a SET (read_consistency_mode = 'strict',)",
			want: "ALTER COORDINATION NODE a: the settings end early; write name = value, separated by commas, in parentheses"},
		{name: "settings that end early", text: "CREATE COORDINATION NODE n WITH (read_consistency_mode =",
			want: "CREATE COORDINATION NODE n: the settings end early; write name = value, separated by commas, in parentheses"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := testcheck.New(t)
			query, recognized, err := coordinationsql.Recognize(tc.text)
			c.Assert(err, qt.ErrorIs, coordinationsql.ErrStatement)
			c.Assert(err, qt.ErrorMatches, "invalid coordination node statement: "+tc.want)
			c.Assert(recognized, qt.IsTrue)
			c.Assert(query, qt.DeepEquals, coordinationsql.Query{})
		})
	}
}

func TestQueryAbsolute_HappyPath(t *testing.T) {
	for _, tc := range []struct {
		name   string
		query  coordinationsql.Query
		prefix string
		want   string
	}{
		{name: "relative to the database", query: coordinationsql.Query{Statement: coordinationsql.Statement{Path: "app/n"}}, want: "/local/app/n"},
		{name: "absolute", query: coordinationsql.Query{Statement: coordinationsql.Statement{Path: "/local/x/../n"}}, want: "/local/n"},
		{name: "under the session's prefix", query: coordinationsql.Query{Statement: coordinationsql.Statement{Path: "n"}}, prefix: "/local/ptah_dev/r1",
			want: "/local/ptah_dev/r1/n"},
		{name: "under the query's own prefix", query: coordinationsql.Query{Statement: coordinationsql.Statement{Path: "n"}, PathPrefix: "/local/app"},
			prefix: "/local/ptah_dev/r1", want: "/local/app/n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := testcheck.New(t)
			got, err := tc.query.Absolute("local", tc.prefix)
			c.Assert(err, qt.IsNil)
			c.Assert(got, qt.Equals, tc.want)
		})
	}
}

func TestQueryAbsolute_FailurePath(t *testing.T) {
	c := testcheck.New(t)
	query := coordinationsql.Query{Statement: coordinationsql.Statement{Path: "n"}, PathPrefix: "relp"}
	got, err := query.Absolute("/local", "")
	c.Assert(err, qt.ErrorIs, coordinationsql.ErrStatement)
	c.Assert(err, qt.ErrorMatches, `invalid coordination node statement: path relp/n is not in database /local: the TablePathPrefix "relp" is relative, and YDB reads a prefix only as an absolute path`)
	c.Assert(got, qt.Equals, "")
}

func TestChanges(t *testing.T) {
	for _, tc := range []struct {
		name             string
		desired, current ydbdecl.CoordinationNode
		want             ydbdecl.CoordinationNode
	}{
		{name: "nothing differs", desired: every, current: every},
		{name: "a default left out on one side and stated on the other", desired: ydbdecl.CoordinationNode{SelfCheckPeriodMillis: 1_000, ReadConsistencyMode: "relaxed"}},
		{name: "a setting going back to its default is named", current: every,
			want: ydbdecl.CoordinationNode{SelfCheckPeriodMillis: 1_000, SessionGracePeriodMillis: 10_000, ReadConsistencyMode: "relaxed",
				AttachConsistencyMode: "strict", RateLimiterCountersMode: "aggregated"}},
		{name: "only what differs", desired: ydbdecl.CoordinationNode{SelfCheckPeriodMillis: 2_000, SessionGracePeriodMillis: 15_000},
			current: ydbdecl.CoordinationNode{SelfCheckPeriodMillis: 2_000}, want: ydbdecl.CoordinationNode{SessionGracePeriodMillis: 15_000}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := testcheck.New(t)
			c.Assert(coordinationsql.Changes(tc.desired, tc.current), qt.DeepEquals, tc.want)
		})
	}
}

func TestMerge(t *testing.T) {
	c := testcheck.New(t)
	merged := coordinationsql.Merge(every, ydbdecl.CoordinationNode{ReadConsistencyMode: "relaxed", SelfCheckPeriodMillis: 500})
	c.Assert(merged, qt.DeepEquals, ydbdecl.CoordinationNode{SelfCheckPeriodMillis: 500, SessionGracePeriodMillis: 15_000, ReadConsistencyMode: "relaxed",
		AttachConsistencyMode: "relaxed", RateLimiterCountersMode: "detailed"})
	c.Assert(coordinationsql.Effective(ydbdecl.CoordinationNode{}), qt.DeepEquals, ydbdecl.CoordinationNode{SelfCheckPeriodMillis: 1_000,
		SessionGracePeriodMillis: 10_000, ReadConsistencyMode: "relaxed", AttachConsistencyMode: "strict", RateLimiterCountersMode: "aggregated"})
}
