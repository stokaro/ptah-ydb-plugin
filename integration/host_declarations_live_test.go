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

// Rendering needs no session. Applying that authored text and comparing the
// resulting catalog establishes that CREATE retains each declared property.
func TestHostAuthoredSchemaAppliesAndReplansSubprocess(t *testing.T) {
	c := qt.New(t)
	host, manifest := hostManifest(c)
	opened, directory, database := inspectionFixture(c)
	source := filepath.Join(c.TempDir(), "desired.yaml")
	contents := fmt.Sprintf("tables:\n  items:\n    schema: %s\n    columns:\n      id: {type: Uint64, primary: true, nullable: false}\ncoordination_nodes:\n  workers:\n    schema: %s\n    self_check_period: PT2S\ntopics:\n  events:\n    schema: %s\n    consumers:\n      worker: {}\n", directory, directory, directory)
	c.Assert(os.WriteFile(source, []byte(contents), 0o600), qt.IsNil)
	run := func(arguments ...string) []byte {
		c.Helper()
		ctx, cancel := context.WithTimeout(c.Context(), 2*time.Minute)
		defer cancel()
		command := exec.CommandContext(ctx, host, append(arguments, "--provider-config", manifest, "--provider-engine", "ydb")...)
		var stderr bytes.Buffer
		command.Stderr = &stderr
		output, err := command.Output()
		c.Assert(err, qt.IsNil, qt.Commentf("%s", stderr.String()))
		return output
	}
	rendered := run("schema", "render", "--schema-file", source, "--dialect", "ydb", "--server-version", "26.2")
	c.Assert(string(rendered), qt.Contains, "CREATE TABLE")
	c.Assert(string(rendered), qt.Contains, "CREATE COORDINATION NODE")
	c.Assert(string(rendered), qt.Contains, "CREATE TOPIC")
	binary, digest := providerBinary(c)
	client := startProvider(c, binary, digest)
	session, err := client.Open(c.Context(), &protocol.OpenRequest{Engine: "ydb", Address: os.Getenv("PTAH_YDB_TEST_URL")})
	c.Assert(err, qt.IsNil)
	c.Cleanup(func() { c.Check(client.CloseSession(context.Background(), session.Id), qt.IsNil) })
	c.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		c.Check(opened.Driver.Coordination().DropNode(ctx, path.Join(database, directory, "workers")), qt.IsNil)
		_, err := opened.DB.ExecContext(ctx, "DROP TOPIC `"+path.Join(directory, "events")+"`")
		c.Check(err, qt.IsNil)
		_, err = opened.DB.ExecContext(ctx, "DROP TABLE `"+path.Join(directory, "items")+"`")
		c.Check(err, qt.IsNil)
	})
	result := executeSQL(c, client, session.Id, "", string(rendered))
	c.Assert(result.Outcome, qt.Equals, protocol.Execution_APPLIED)
	output := run("schema", "diff", "--from", os.Getenv("PTAH_YDB_TEST_URL"), "--to", source, "--schemas", directory, "--format", "json")
	var plan struct {
		Statements []string `json:"statements"`
	}
	c.Assert(json.Unmarshal(output, &plan), qt.IsNil, qt.Commentf("%s", output))
	c.Assert(plan.Statements, qt.HasLen, 0)
}
