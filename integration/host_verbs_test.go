//go:build integration

package integration_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"testing"
	"time"

	qt "github.com/frankban/quicktest"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb_Coordination"
	"github.com/ydb-platform/ydb-go-sdk/v3/coordination"
	"google.golang.org/protobuf/encoding/protojson"

	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
	"github.com/stokaro/ptah-ydb-plugin/schema"
)

// hostVerb runs one verb of the pinned host with this provider selected for
// ydb, against the test address, and returns its two streams apart and its
// exit status: a JSON document is the standard output alone, and a drift or
// a difference exits 1.
func hostVerb(c *qt.C, args ...string) (stdout, stderr string, exit int) {
	c.Helper()
	host, manifest := hostManifest(c)
	ctx, cancel := context.WithTimeout(c.Context(), 4*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, host, append(args, "--provider-config", manifest, "--provider-engine", "ydb", "--db-url", os.Getenv("PTAH_YDB_TEST_URL"))...)
	var out, diagnostics bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &diagnostics
	err := cmd.Run()
	var exited *exec.ExitError
	switch {
	case err == nil:
		return out.String(), diagnostics.String(), 0
	case errors.As(err, &exited):
		return out.String(), diagnostics.String(), exited.ExitCode()
	}
	c.Fatalf("run the host: %v\n%s", err, diagnostics.String())
	return "", "", -1
}

// writeSource writes text to a file of its own and returns its path, for a
// verb that reads a schema file.
func writeSource(c *qt.C, name, text string) string {
	c.Helper()
	file := filepath.Join(c.TempDir(), name)
	c.Assert(os.WriteFile(file, []byte(text), 0o600), qt.IsNil)
	return file
}

// driftChange is one feature change of a drift document, as the host writes
// a provider's value: the owner, kind and version of the value's model, and
// the provider's payload with the operands and the subject it changes.
type driftChange struct {
	Subject struct {
		Kind string `json:"kind"`
		Name struct {
			Source string `json:"source"`
		} `json:"name"`
	} `json:"subject"`
	Value struct {
		Owner          string `json:"owner"`
		Kind           string `json:"kind"`
		Representation string `json:"representation"`
		Version        uint32 `json:"version"`
		Payload        struct {
			Payload []byte       `json:"payload"`
			Subject driftSubject `json:"subject"`
		} `json:"payload"`
	} `json:"value"`
}

// driftSubject is the object a provider's value changes.
type driftSubject struct {
	Engine   string `json:"engine"`
	Kind     string `json:"kind"`
	Database string `json:"database"`
	Schema   string `json:"schema"`
	Name     string `json:"name"`
}

// envelope is the change as an envelope of the provider's change model,
// which the provider's own normalization reads back.
func (d driftChange) envelope() *protocol.Envelope {
	subject, model := d.Value.Payload.Subject, schema.CoordinationChangeModel()
	return &protocol.Envelope{ProviderId: d.Value.Owner, Kind: d.Value.Kind, SchemaVersion: d.Value.Version, Encoding: model.Encoding,
		DefinitionSha256: model.DefinitionSha256, Payload: d.Value.Payload.Payload,
		Owner: &protocol.Reference{Engine: subject.Engine, Kind: subject.Kind, Database: subject.Database, Schema: subject.Schema, Name: subject.Name}}
}

// The host's `schema drift --format json` reports a coordination node whose
// read consistency differs from the declared one: it exits 1, as drift does,
// and its document carries the one change the provider's comparison made,
// which the provider reads back as the node read relaxed and declared strict.
// The run changes nothing, so the node stays relaxed.
func TestHostDriftJSONCarriesACoordinationNodeChange_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	opened, directory, database := lockFixture(c)
	c.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		_ = opened.Driver.Coordination().DropNode(cleanup, path.Join(database, directory, "locks"))
	})
	document := func(mode string) string {
		return "coordination_nodes:\n  locks:\n    schema: " + directory + "\n    read_consistency_mode: " + mode + "\n"
	}
	applied, err := hostApplySource(c, directory, "yaml", document("relaxed"))
	c.Assert(err, qt.IsNil, qt.Commentf("%s", applied))

	stdout, stderr, exit := hostVerb(c, "schema", "drift", "--format", "json", "--schemas", directory, "--schema-file", writeSource(c, "desired.yaml", document("strict")))

	c.Assert(exit, qt.Equals, 1, qt.Commentf("stdout:\n%s\nstderr:\n%s", stdout, stderr))
	var report struct {
		Drift bool `json:"drift"`
		Diff  struct {
			FeatureChanges []driftChange `json:"feature_changes"`
		} `json:"diff"`
	}
	c.Assert(json.Unmarshal([]byte(stdout), &report), qt.IsNil, qt.Commentf("stdout:\n%s", stdout))
	c.Assert(report.Drift, qt.IsTrue)
	c.Assert(report.Diff.FeatureChanges, qt.HasLen, 1)
	change := report.Diff.FeatureChanges[0]
	c.Assert([]string{change.Subject.Kind, change.Subject.Name.Source, change.Value.Kind, change.Value.Representation},
		qt.DeepEquals, []string{schema.CoordinationProperties, "locks", schema.CoordinationChange, "change"})

	normalized, err := schema.NormalizeCoordinationChange(&protocol.ValueInput{Id: "drift", Representation: protocol.ValueInput_CHANGE, Value: change.envelope()})
	c.Assert(err, qt.IsNil)
	var operands struct {
		Before json.RawMessage           `json:"before"`
		After  schema.CoordinationIntent `json:"after"`
	}
	c.Assert(json.Unmarshal(normalized.Value.Payload, &operands), qt.IsNil)
	var before Ydb_Coordination.DescribeNodeResult
	c.Assert(protojson.Unmarshal(operands.Before, &before), qt.IsNil)
	c.Assert(before.GetConfig().GetReadConsistencyMode(), qt.Equals, Ydb_Coordination.ConsistencyMode_CONSISTENCY_MODE_RELAXED)
	c.Assert(operands.After, qt.DeepEquals, schema.CoordinationIntent{ReadConsistencyMode: new("strict")})
	settings, found := describedNode(c, opened, database, directory, "locks")
	c.Assert(found, qt.IsTrue)
	c.Assert(settings.ReadMode, qt.Equals, coordination.ConsistencyModeRelaxed)
}

// compareItems is a Go entity in a directory below the fixture directory, with
// a key that is not Serial, a column with a default and a nullable column;
// extra adds further fields.
func compareItems(directory, extra string) string {
	return "package entities\n\n//ptah:schema:table name=\"items\" schema=\"" + directory + "/nested\"\ntype Item struct {\n" +
		"\t//ptah:schema:field name=\"id\" type=\"Uint64\" primary=\"true\" not_null=\"true\"\n\tID uint64\n" +
		"\t//ptah:schema:field name=\"title\" type=\"Utf8\" not_null=\"true\" default=\"untitled\"\n\tTitle string\n" +
		"\t//ptah:schema:field name=\"created_at\" type=\"Timestamp\"\n\tCreatedAt int64\n" + extra + "}\n"
}

// The host's `schema compare --exit-code` finds nothing to change once its
// `schema apply` has built a table in a nested directory, and exits 0: the
// provider reads back the key, the default and the nullable column as
// declared. A source that adds a column exits 1 and names the table, so the
// clean run is a measurement. A directory is a schema of its own, so both
// runs select the nested one, which the fixture creates: the provider
// refuses a read of a directory that does not exist.
func TestHostComparesAnAppliedTable_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	opened, directory, database := lockFixture(c)
	nested := path.Join(directory, "nested")
	c.Assert(opened.Driver.Scheme().MakeDirectory(c.Context(), path.Join(database, nested)), qt.IsNil)
	c.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		c.Check(execFixtureSchema(cleanup, opened.DB, "DROP TABLE IF EXISTS `"+path.Join(database, nested, "items")+"`"), qt.IsNil)
		c.Check(opened.Driver.Scheme().RemoveDirectory(cleanup, path.Join(database, nested)), qt.IsNil)
	})
	applied, err := hostApply(c, nested, compareItems(directory, ""))
	c.Assert(err, qt.IsNil, qt.Commentf("%s", applied))
	compare := func(source string) (string, string, int) {
		c.Helper()
		root := c.TempDir()
		c.Assert(os.WriteFile(filepath.Join(root, "entities.go"), []byte(source), 0o600), qt.IsNil)
		return hostVerb(c, "schema", "compare", "--exit-code", "--schemas", nested, "--root-dir", root)
	}

	clean, cleanDiagnostics, cleanExit := compare(compareItems(directory, ""))
	changed, changedDiagnostics, changedExit := compare(compareItems(directory, "\t//ptah:schema:field name=\"note\" type=\"Utf8\"\n\tNote string\n"))

	c.Assert(cleanExit, qt.Equals, 0, qt.Commentf("%s\n%s", clean, cleanDiagnostics))
	c.Assert(clean, qt.Contains, "No schema differences detected.")
	c.Assert(changedExit, qt.Equals, 1, qt.Commentf("%s\n%s", changed, changedDiagnostics))
	c.Assert(changed, qt.Contains, directory+"/nested.items")
}
