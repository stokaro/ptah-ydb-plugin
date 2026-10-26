package streaming_test

import (
	"testing"

	qt "github.com/frankban/quicktest"
	"github.com/stokaro/ptah-ydb-plugin/internal/streaming"
)

func TestEqualUsesTokensAndDefaults(t *testing.T) {
	for _, tc := range []struct {
		name        string
		left, right streaming.Spec
		want        bool
	}{
		{"comments and whitespace", streaming.Spec{Text: "SELECT 1;"}, streaming.Spec{Text: "-- note\nSELECT /* note */ 1 ;", Run: new(true), ResourcePool: "default"}, true},
		{"changed literal", streaming.Spec{Text: "SELECT 'a';"}, streaming.Spec{Text: "SELECT 'b';"}, false},
		{"changed operator", streaming.Spec{Text: "SELECT 1+2;"}, streaming.Spec{Text: "SELECT 1-2;"}, false},
		{"comment markers inside literal", streaming.Spec{Text: "SELECT 'a--b';"}, streaming.Spec{Text: "SELECT 'a';"}, false},
		{"stopped", streaming.Spec{Text: "SELECT 1;"}, streaming.Spec{Text: "SELECT 1;", Run: new(false)}, false},
		{"pool", streaming.Spec{Text: "SELECT 1;"}, streaming.Spec{Text: "SELECT 1;", ResourcePool: "batch"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			qt.Assert(t, streaming.Equal(tc.left, tc.right), qt.Equals, tc.want)
		})
	}
}

func TestStepsRetainPreconditions(t *testing.T) {
	c := qt.New(t)
	before := &streaming.Spec{Text: "SELECT 1;"}
	after := &streaming.Spec{Text: "SELECT 2;", ResourcePool: "batch"}
	change := streaming.Change{Schema: "app", Name: "copy", Before: before, After: after, AllowStateReset: true}
	steps, err := streaming.Steps(change)
	c.Assert(err, qt.IsNil)
	c.Assert(len(steps), qt.Equals, 2)
	c.Assert(streaming.Running(*before), qt.IsTrue)
	c.Assert(steps[0].Before, qt.DeepEquals, before)
	c.Assert(steps[0].After, qt.DeepEquals, &streaming.Spec{Text: "SELECT 1;", Run: new(false)})
	c.Assert(steps[1].Before, qt.DeepEquals, steps[0].After)
	c.Assert(steps[1].After, qt.DeepEquals, after)
	c.Assert(steps[0].AllowStateReset, qt.IsFalse)
	c.Assert(steps[1].AllowStateReset, qt.IsTrue)
	c.Assert(streaming.Early(steps[0]), qt.IsTrue)
	c.Assert(streaming.Early(steps[1]), qt.IsFalse)
	var statements []string
	for _, step := range steps {
		rendered, err := streaming.Statements(step)
		c.Assert(err, qt.IsNil)
		c.Assert(len(rendered), qt.Equals, 1)
		statements = append(statements, rendered...)
	}
	c.Assert(statements, qt.DeepEquals, []string{
		"ALTER STREAMING QUERY `app/copy` SET (RUN = FALSE, RESOURCE_POOL = `default`);",
		"ALTER STREAMING QUERY `app/copy` SET (RUN = TRUE, RESOURCE_POOL = `batch`, FORCE = TRUE) AS DO BEGIN\nSELECT 2;\nEND DO;",
	})
}

func TestStepsAndCheckpointImpact(t *testing.T) {
	query := &streaming.Spec{Text: "SELECT 1;"}
	stopped := &streaming.Spec{Text: "SELECT 1;", Run: new(false)}
	for _, tc := range []struct {
		name              string
		change            streaming.Change
		early, checkpoint bool
	}{
		{"create", streaming.Change{After: query}, false, false},
		{"replace creation", streaming.Change{After: query, AllowStateReset: true}, false, true},
		{"drop", streaming.Change{Before: query}, true, true},
		{"stop", streaming.Change{Before: query, After: stopped}, true, false},
		{"start", streaming.Change{Before: stopped, After: query}, false, false},
		{"change stopped body", streaming.Change{Before: stopped, After: &streaming.Spec{Text: "SELECT 2;", Run: new(false)}, AllowStateReset: true}, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := qt.New(t)
			tc.change.Name = "copy"
			steps, err := streaming.Steps(tc.change)
			c.Assert(err, qt.IsNil)
			c.Assert(len(steps), qt.Equals, 1)
			c.Assert(streaming.Early(steps[0]), qt.Equals, tc.early)
			c.Assert(streaming.LosesCheckpoint(tc.change), qt.Equals, tc.checkpoint)
		})
	}
}

func TestStatementsRefuseInvalidChanges(t *testing.T) {
	query := &streaming.Spec{Text: "SELECT 1;"}
	for _, tc := range []struct {
		name   string
		change streaming.Change
		want   string
	}{
		{"no operands", streaming.Change{}, ".*has no operand"},
		{"equal operands", streaming.Change{Before: query, After: query}, ".*holds no difference"},
		{"unapproved reset", streaming.Change{Before: query, After: &streaming.Spec{Text: "SELECT 2;"}}, ".*declare allow_state_reset=true.*"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := qt.New(t)
			statements, err := streaming.Statements(tc.change)
			c.Assert(err, qt.ErrorMatches, tc.want)
			c.Assert(statements, qt.IsNil)
			steps, err := streaming.Steps(tc.change)
			c.Assert(err, qt.IsNotNil)
			c.Assert(steps, qt.IsNil)
		})
	}
}

func TestValidateRefusesSchemaAndEscapedBlocks(t *testing.T) {
	for _, body := range []string{"", "-- only a comment", "DROP TABLE t;", "SELECT 1; CREATE TABLE t(id Int32);", "END DO; DROP TABLE t;", "SELECT '\xff';"} {
		t.Run(body, func(t *testing.T) {
			qt.Assert(t, streaming.Validate(streaming.Spec{Text: body}), qt.IsNotNil)
		})
	}
	qt.Assert(t, streaming.Validate(streaming.Spec{Text: "INSERT INTO stream_output SELECT * FROM stream_input;", Run: new(false)}), qt.IsNil)
}

func TestValidateIdentity(t *testing.T) {
	for _, tc := range []struct{ directory, name string }{{"../app", "q"}, {"/app", "q"}, {"app/../jobs", "q"}, {"app", "dir/q"}, {"app", ".."}, {"", " "}, {"", "\x00"}, {"", "\xff"}} {
		qt.Assert(t, streaming.ValidateIdentity(tc.directory, tc.name), qt.IsNotNil)
	}
	qt.Assert(t, streaming.ValidateIdentity("app/jobs", "copy"), qt.IsNil)
	qt.Assert(t, streaming.ValidateIdentity("", "copy.v2"), qt.IsNil)
}

func TestNamedPaths(t *testing.T) {
	for _, tc := range []struct {
		name, text string
		want       []string
	}{
		{"paths", "INSERT INTO `app/out` SELECT * FROM input JOIN `app/extra` ON true JOIN input ON true;", []string{"app/out", "input", "app/extra"}},
		{"expressions", "SELECT * FROM $rows JOIN pq.topic ON true JOIN AS_TABLE($rows) ON true;", nil},
		{"absolute prefix", "PRAGMA TablePathPrefix = '/local/app'; SELECT * FROM input;", []string{"/local/app/input"}},
		{"relative prefix", "PRAGMA TablePathPrefix = 'app'; INSERT INTO output SELECT * FROM `/local/absolute`;", []string{"app/output", "/local/absolute"}},
		{"prefix reset", "PRAGMA TablePathPrefix = 'app'; SELECT * FROM first; PRAGMA TablePathPrefix = ''; SELECT * FROM second;", []string{"app/first", "second"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := streaming.NamedPaths(tc.text)
			qt.Assert(t, err, qt.IsNil)
			qt.Assert(t, got, qt.DeepEquals, tc.want)
		})
	}
}

func TestNamedPathsRefusesUnresolvedPaths(t *testing.T) {
	for _, text := range []string{
		"SELECT * FROM good; PRAGMA TablePathPrefix = $prefix; SELECT * FROM input;",
		"PRAGMA TablePathPrefix = '../app'; SELECT * FROM input;",
		"PRAGMA TablePathPrefix('app'); SELECT * FROM input;",
		"SELECT * FROM `app/../input`;",
		"SELECT * FROM `/local//input`;",
	} {
		got, err := streaming.NamedPaths(text)
		qt.Assert(t, err, qt.IsNotNil)
		qt.Assert(t, got, qt.IsNil)
	}
}
