//go:build integration

package integration_test

import (
	"testing"

	qt "github.com/frankban/quicktest"

	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
)

// A coordination node and another authored object cannot occupy one YDB
// scheme path. This ports the render half of the former host's
// TestValidateSchema_YDBRefusesACoordinationNodeOnAnotherObjectsPath and
// TestValidateSchema_YDBRefusesACoordinationNodeOnATopicsPath.
func TestHostRefusesCoordinationNodePathCollisions_FailurePath(t *testing.T) {
	const node = "coordination_nodes:\n  locks:\n    schema: app\n"
	for _, test := range []struct {
		name, other, want string
	}{
		{"table", "tables:\n  locks:\n    schema: app\n    columns:\n      id: {type: Uint64, primary: true, nullable: false}\n", "provider decode source failed (InvalidArgument)"},
		{"view", "views:\n  locks:\n    name: app.locks\n    body: SELECT 1 AS id\n", "authored native object conflicts with a declared view path"},
		{"topic", "topics:\n  locks:\n    schema: app\n", "authored native objects share or contain one scheme path"},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			file := writeYAML(c, node+test.other)
			for _, source := range []string{node, test.other} {
				out, err := hostCommand(c, "schema", "render", "--schema-file", writeYAML(c, source), "--dialect", "ydb", "--server-version", "26.2.1.14")
				c.Assert(err, qt.IsNil, qt.Commentf("%s", out))
			}
			for _, version := range []string{"25.1.4.7", "26.2.1.14"} {
				t.Run(version, func(t *testing.T) {
					c := testcheck.New(t)
					out, err := hostCommand(c, "schema", "render", "--schema-file", file, "--dialect", "ydb", "--server-version", version)
					c.Assert(err, qt.IsNotNil, qt.Commentf("%s", out))
					c.Assert(out, qt.Contains, test.want)
					c.Assert(out, qt.Not(qt.Contains), "-- Statement ")
				})
			}
		})
	}
}

// A different final path component leaves both objects creatable.
func TestHostRendersCoordinationNodeBesideAnotherView_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	file := writeYAML(c, "coordination_nodes:\n  locks:\n    schema: app\n"+
		"views:\n  locks2:\n    name: app.locks2\n    body: SELECT 1 AS id\n")
	out, err := hostCommand(c, "schema", "render", "--schema-file", file, "--dialect", "ydb", "--server-version", "26.2.1.14")

	c.Assert(err, qt.IsNil, qt.Commentf("%s", out))
	c.Assert(out, qt.Contains, "CREATE COORDINATION NODE `app/locks`")
	c.Assert(out, qt.Contains, "CREATE VIEW `app/locks2`")
}
