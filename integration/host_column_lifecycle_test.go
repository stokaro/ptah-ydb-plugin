//go:build integration

package integration_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"testing"
	"time"

	qt "github.com/frankban/quicktest"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
)

// The public CLI must reach a structural fixed point, not only render no units.
// The primary declaration retains its authored nullable value throughout.
func TestHostColumnDefaultsAndInventory(t *testing.T) {
	c := qt.New(t)
	host, manifest := hostManifest(c)
	client := startProviderFor(c)
	session, err := client.Open(c.Context(), &protocol.OpenRequest{Engine: "ydb", Address: os.Getenv("PTAH_YDB_TEST_URL")})
	c.Assert(err, qt.IsNil)
	c.Cleanup(func() { c.Check(client.CloseSession(context.Background(), session.Id), qt.IsNil) })
	if !session.Capabilities["alter_column_default"] {
		c.Skip("the target does not support setting and dropping column defaults")
	}
	opened, directory, database := lockFixture(c)
	table := path.Join(database, directory, "records")
	c.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		c.Check(execFixtureSchema(ctx, opened.DB, "DROP TABLE IF EXISTS `"+table+"`"), qt.IsNil)
	})
	source := func(defaultClause, primaryClause, extra string) string {
		return fmt.Sprintf("tables:\n  records:\n    schema: %s\n    columns:\n      id: {type: BIGINT, primary: true%s}\n      amount: {type: INTEGER, nullable: true%s}\n      label: {type: TEXT, nullable: true, default: \"\"}\n%s", directory, primaryClause, defaultClause, extra)
	}
	for _, step := range []struct{ name, defaultClause, primaryClause, extra string }{
		{name: "create", defaultClause: ", default: '+05'", primaryClause: ", nullable: true"},
		{name: "canonical literal and omitted nullability", defaultClause: ", default: '5'"},
		{name: "explicit not null", defaultClause: ", default: '5'", primaryClause: ", nullable: false"},
		{name: "set default", defaultClause: ", default: '6'", primaryClause: ", nullable: true"},
		{name: "drop default", primaryClause: ", nullable: true"},
		{name: "add column", primaryClause: ", nullable: true", extra: "      note: {type: INTEGER, nullable: true}\n"},
		{name: "drop column", primaryClause: ", nullable: true"},
	} {
		text := source(step.defaultClause, step.primaryClause, step.extra)
		output, err := hostApplySource(c, directory, "yaml", text)
		c.Assert(err, qt.IsNil, qt.Commentf("%s: %s", step.name, output))
		assertColumnFixedPoint(c, host, manifest, directory, text)
		if step.name == "set default" {
			_, err = opened.DB.ExecContext(c.Context(), "INSERT INTO `"+table+"` (id) VALUES (1)") // #nosec G202 -- the fixture generates the directory and owns the fixed table name
			c.Assert(err, qt.IsNil)
			var amount int64
			var label string
			c.Assert(opened.DB.QueryRowContext(c.Context(), "SELECT amount,label FROM `"+table+"` WHERE id=1").Scan(&amount, &label), qt.IsNil)
			c.Assert(amount, qt.Equals, int64(6))
			c.Assert(label, qt.Equals, "")
		}
	}
}

func assertColumnFixedPoint(c *qt.C, host, manifest, directory, text string) {
	c.Helper()
	source := filepath.Join(c.TempDir(), "desired.yaml")
	c.Assert(os.WriteFile(source, []byte(text), 0o600), qt.IsNil)
	ctx, cancel := context.WithTimeout(c.Context(), 2*time.Minute)
	defer cancel()
	// #nosec G204 G702 -- the external host binary is the explicit CI fixture input
	command := exec.CommandContext(ctx, host, "schema", "diff", "--from", os.Getenv("PTAH_YDB_TEST_URL"), "--to", source, "--schemas", directory, "--format", "json", "--provider-config", manifest, "--provider-engine", "ydb")
	var diagnostics bytes.Buffer
	command.Stderr = &diagnostics
	output, err := command.Output()
	c.Assert(err, qt.IsNil, qt.Commentf("%s", diagnostics.String()))
	var plan struct {
		Statements []string                   `json:"statements"`
		Changes    map[string]json.RawMessage `json:"changes"`
	}
	c.Assert(json.Unmarshal(output, &plan), qt.IsNil, qt.Commentf("%s", output))
	c.Assert(plan.Statements, qt.HasLen, 0)
	for key, value := range plan.Changes {
		var changes []json.RawMessage
		if json.Unmarshal(value, &changes) == nil {
			c.Assert(changes, qt.HasLen, 0, qt.Commentf("%s: %s", key, value))
		}
	}
}
