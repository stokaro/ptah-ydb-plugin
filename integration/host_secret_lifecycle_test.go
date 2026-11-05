//go:build integration

package integration_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	qt "github.com/frankban/quicktest"

	"github.com/stokaro/ptah-ydb-plugin/internal/connection"
	"github.com/stokaro/ptah-ydb-plugin/internal/locking"
	"github.com/stokaro/ptah-ydb-plugin/internal/secrets"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
)

// secretValue is a made-up secret value every output is searched for.
func secretValue() string { return "SENTINEL-" + rand.Text() }

// secretVariable sets a variable of the run's own, under the prefix a
// secret's value is read from, to value, and returns its name.
func secretVariable(t *testing.T, value string) string {
	t.Helper()
	name := secrets.ValuePrefix + "LIVE_" + rand.Text()
	t.Setenv(name, value)
	return name
}

// secretsFixture is a locked fixture directory on a line with secrets. The
// cleanup drops every secret the directory holds, named or not.
func secretsFixture(c *qt.C) (opened *connection.Connection, directory, database string) {
	c.Helper()
	opened, directory, database = lockFixture(c)
	_, session := accessSession(c)
	skipWithout(c, session, secrets.Capability)
	c.Cleanup(func() { dropSecretsBelow(c, opened, path.Join(database, directory)) })
	return opened, directory, database
}

// dropSecretsBelow drops every secret, table and directory below absolute,
// deepest first, and keeps absolute itself.
func dropSecretsBelow(c *qt.C, opened *connection.Connection, absolute string) {
	c.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	listing, err := opened.Driver.Scheme().ListDirectory(ctx, absolute)
	if err != nil {
		// A directory the run never made has nothing to drop.
		return
	}
	for _, entry := range listing.Children {
		child := path.Join(absolute, entry.Name)
		switch {
		case entry.IsDirectory():
			dropSecretsBelow(c, opened, child)
			c.Check(opened.Driver.Scheme().RemoveDirectory(ctx, child), qt.IsNil)
		case entry.IsTable() || entry.IsColumnTable():
			c.Check(execFixtureSchema(ctx, opened.DB, "DROP TABLE `"+child+"`"), qt.IsNil)
		default:
			_ = execFixtureSchema(ctx, opened.DB, "DROP SECRET `"+child+"`;")
		}
	}
}

// hostApplyAt runs the host's `schema apply` with args, with this provider
// selected for ydb, against address, and returns the planned units as
// appliedUnits lists them and how it exited.
func hostApplyAt(c *qt.C, address string, args ...string) (string, error) {
	c.Helper()
	host, manifest := hostManifest(c)
	ctx, cancel := context.WithTimeout(c.Context(), 4*time.Minute)
	defer cancel()
	arguments := slices.Concat([]string{"schema", "apply", "--json"}, args, []string{"--provider-config", manifest, "--provider-engine", "ydb", "--db-url", address})
	cmd := exec.CommandContext(ctx, host, arguments...)
	var report, printed bytes.Buffer
	cmd.Stdout, cmd.Stderr = &report, &printed
	err := cmd.Run()
	return appliedUnits(report.Bytes(), printed.String()), err
}

// hostRunAt runs one host command line with this provider selected for ydb
// against address and returns what it printed and how it exited.
func hostRunAt(c *qt.C, address string, args ...string) (string, error) {
	c.Helper()
	host, manifest := hostManifest(c)
	ctx, cancel := context.WithTimeout(c.Context(), 4*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, host, append(args, "--provider-config", manifest, "--provider-engine", "ydb", "--db-url", address)...)
	var output bytes.Buffer
	cmd.Stdout, cmd.Stderr = &output, &output
	err := cmd.Run()
	return output.String(), err
}

// Ported from the host's TestYDBSecrets_FailurePath_RefusesWithoutSendingTheValue:
// a secret declared over the path of a table the declaration keeps is
// refused, the table stays, no secret is made, and no output holds the value
// the variable defines. The host's test sends the statement through its
// writer and reads the server's refusal; through a plan the provider refuses
// the occupied path before it sends anything. A declaration that leaves the
// table out drops it first and creates the secret at its path, as the host's
// engine plans the handoff.
func TestHostRefusesASecretOverATableWithoutItsValue_FailurePath(t *testing.T) {
	value := secretValue()
	variable := secretVariable(t, value)
	c := testcheck.New(t)
	opened, directory, database := secretsFixture(c)
	c.Assert(execFixtureSchema(c.Context(), opened.DB, "CREATE TABLE `"+path.Join(database, directory, "taken")+"` (id Int64 NOT NULL, PRIMARY KEY (id))"), qt.IsNil)

	output, err := hostApplySource(c, directory, "yaml", "tables:\n  taken:\n    schema: "+directory+"\n    columns:\n      id: {type: Int64, primary: true, not_null: true}\n"+
		"secrets:\n  taken:\n    schema: "+directory+"\n    value_env: "+variable+"\n")

	c.Assert(err, qt.IsNotNil, qt.Commentf("%s", output))
	c.Assert(output, qt.Not(qt.Contains), value)
	c.Assert(listedNames(c, opened, path.Join(database, directory)), qt.DeepEquals, []string{"taken"})
	described, err := opened.Driver.Scheme().DescribePath(c.Context(), path.Join(database, directory, "taken"))
	c.Assert(err, qt.IsNil)
	c.Assert(described.IsTable(), qt.IsTrue)
}

// secretsEntities is a Go source with a table and the secret beside it.
func secretsEntities(directory, variable string) string {
	return "package entities\n\n//ptah:schema:table name=\"accounts\" schema=\"" + directory + "\"\ntype Account struct {\n" +
		"\t//ptah:schema:field name=\"id\" type=\"BIGINT\" primary\n\tID int64\n}\n\n" +
		"//ptah:schema:secret name=\"pg_password\" schema=\"" + directory + "\" value_env=\"" + variable + "\"\ntype Credentials struct{}\n"
}

// refusedSecretsEntities is secretsEntities with a second secret whose name
// holds a space, which the server refuses at execution (`symbol ' ' is not
// allowed in the path part`).
func refusedSecretsEntities(directory, variable string) string {
	return secretsEntities(directory, variable) +
		"\n//ptah:schema:secret name=\"bad name\" schema=\"" + directory + "\" value_env=\"" + variable + "\"\ntype Refused struct{}\n"
}

// Ported from the host's TestYDBBinary_KeepsASecretsValueOutOfEveryOutput:
// the host drives a secret through its life with the value in the
// environment: schema apply creates it beside a table, schema compare
// --exit-code finds nothing to change, a dry run rotates it, and a run that
// declares a second secret the server refuses fails. No output holds the
// value. The host's test also rotates the secret through a generated migration
// that migrations up applies; through this provider migrations generate
// refuses a secret change it cannot reverse, so that part is not run here.
func TestHostKeepsASecretsValueOutOfEveryOutput_HappyPath(t *testing.T) {
	first, second := secretValue(), secretValue()
	variable := secretVariable(t, first)
	c := testcheck.New(t)
	opened, directory, database := secretsFixture(c)
	entities, refusing := c.TempDir(), c.TempDir()
	c.Assert(os.WriteFile(filepath.Join(entities, "schema.go"), []byte(secretsEntities(directory, variable)), 0o600), qt.IsNil)
	c.Assert(os.WriteFile(filepath.Join(refusing, "schema.go"), []byte(refusedSecretsEntities(directory, variable)), 0o600), qt.IsNil)
	address := os.Getenv("PTAH_YDB_TEST_URL")
	scope := []string{"--schemas", directory, "--tx-mode", "none"}

	applied, applyErr := hostApplyAt(c, address, append([]string{"--root-dir", entities, "--auto-approve"}, scope...)...)
	compared, compareErr := hostRunAt(c, address, "schema", "compare", "--root-dir", entities, "--exit-code", "--schemas", directory)
	t.Setenv(variable, second)
	previewed, previewErr := hostApplyAt(c, address, append([]string{"--root-dir", entities, "--dry-run",
		"--rotate-secret", directory + "/pg_password"}, scope...)...)
	refused, refusedErr := hostRunAt(c, address, append([]string{"schema", "apply", "--root-dir", refusing, "--auto-approve"}, scope...)...)

	c.Assert(applyErr, qt.IsNil, qt.Commentf("schema apply:\n%s", applied))
	c.Assert(applied, qt.Contains, "secret-operation")
	c.Assert(compareErr, qt.IsNil, qt.Commentf("schema compare:\n%s", compared))
	c.Assert(previewErr, qt.IsNil, qt.Commentf("schema apply --dry-run:\n%s", previewed))
	c.Assert(previewed, qt.Contains, "secret-operation")
	c.Assert(refusedErr, qt.IsNotNil, qt.Commentf("%s", refused))
	c.Assert(listedNames(c, opened, path.Join(database, directory)), qt.DeepEquals, []string{"accounts", "pg_password"})
	for _, text := range []string{applied, compared, previewed, refused} {
		c.Assert(text, qt.Not(qt.Contains), "SENTINEL")
	}
}

// Ported from the deprecated-object half of the host's
// TestYDBBinary_ReplaysASecretInADevRealm: the deprecated secret object
// belongs to a user rather than to a path, so migrations validate refuses to
// replay it on a dev realm. The other half, a confined replay of CREATE SECRET
// and ALTER SECRET, fails through this provider before it reaches the server
// ("dispatch inputs differ from the approved references") and is not run here.
func TestHostRefusesTheDeprecatedSecretObjectInAReplay_FailurePath(t *testing.T) {
	c := testcheck.New(t)
	_, session := accessSession(c)
	skipWithout(c, session, secrets.Capability)
	migrations := c.TempDir()
	for name, text := range map[string]string{ // #nosec G101 -- a made-up value the replay refuses
		"0000000001_secret.up.sql":   "CREATE OBJECT ptah_ydb_devrealm_secret (TYPE SECRET) WITH value = 'x';\n",
		"0000000001_secret.down.sql": "DROP OBJECT ptah_ydb_devrealm_secret (TYPE SECRET);\n",
	} {
		c.Assert(os.WriteFile(filepath.Join(migrations, name), []byte(text), 0o600), qt.IsNil)
	}
	host, manifest := hostManifest(c)
	ctx, cancel := context.WithTimeout(c.Context(), 4*time.Minute)
	defer cancel()
	hashed, err := exec.CommandContext(ctx, host, "migrations", "hash", "--dir", migrations).CombinedOutput()
	c.Assert(err, qt.IsNil, qt.Commentf("%s", hashed))
	cmd := exec.CommandContext(ctx, host, "migrations", "validate", "--dir", migrations, "--dev-url", os.Getenv("PTAH_YDB_TEST_URL"),
		"--provider-config", manifest, "--provider-engine", "ydb")

	refused, err := cmd.CombinedOutput()

	var exited *exec.ExitError
	c.Assert(err, qt.ErrorAs, &exited, qt.Commentf("%s", refused))
	c.Assert(string(refused), qt.Contains, "ydb migration replay rejects an object of the whole database because its "+
		"effects cannot be confined to the disposable database realm")
}

// Ported from the host's TestYDBDesiredYQL_Secrets: YQL declares secrets in
// a dev realm, two, then one, then none; each source plans a change, and the
// plan converges.
func TestHostConvergesYQLSecretsInADevRealm_HappyPath(t *testing.T) {
	variable := secretVariable(t, secretValue())
	c := testcheck.New(t)
	_, session := accessSession(c)
	skipWithout(c, session, secrets.Capability)
	base, err := connection.Parse(os.Getenv("PTAH_YDB_TEST_URL"))
	c.Assert(err, qt.IsNil)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	opened, err := connection.Open(ctx, base)
	c.Assert(err, qt.IsNil)
	c.Cleanup(func() { c.Check(opened.Close(), qt.IsNil) })
	address, root := realmFixture(c, opened, base)
	c.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		for _, name := range []string{"first", "second"} {
			_ = execFixtureSchema(cleanup, opened.DB, "DROP SECRET `"+path.Join(root, name)+"`;")
		}
		// The host's schema lock leaves its node at the realm's root.
		_ = opened.Driver.Coordination().DropNode(cleanup, path.Join(root, locking.NodeName))
	})
	schemaFile := filepath.Join(c.TempDir(), "schema.sql")
	for _, source := range []string{
		"CREATE SECRET first WITH (value = $" + variable + "); CREATE SECRET second WITH (value = $" + variable + ");",
		"CREATE SECRET first WITH (value = $" + variable + ");",
		"",
	} {
		c.Assert(os.WriteFile(schemaFile, []byte(source), 0o600), qt.IsNil)
		planned, planErr := hostApplyAt(c, address, "--dry-run", "--tx-mode", "none", "--schema-file", schemaFile)
		applied, applyErr := hostRunAt(c, address, "schema", "apply", "--auto-approve", "--tx-mode", "none", "--schema-file", schemaFile)
		settled, settleErr := hostRunAt(c, address, "schema", "apply", "--dry-run", "--tx-mode", "none", "--schema-file", schemaFile)

		c.Assert(planErr, qt.IsNil, qt.Commentf("%s", planned))
		c.Assert(planned, qt.Contains, "secret-operation", qt.Commentf("%q", source))
		c.Assert(applyErr, qt.IsNil, qt.Commentf("%s", applied))
		c.Assert(settleErr, qt.IsNil, qt.Commentf("%s", settled))
		c.Assert(settled, qt.Contains, "Schema is synced, no changes to be made", qt.Commentf("%q", source))
	}
}

// pgSourceAndSecret declares a PostgreSQL data source at location whose
// password the secret at secretPath holds, and the secret abs_pw whose value
// variable holds, from a source that describes every secret.
func pgSourceAndSecret(directory, location, secretPath, variable string) string {
	return "secrets:\n  abs_pw:\n    schema: " + directory + "\n    value_env: " + variable + "\n" + warehouseSource(directory, location, secretPath)
}

// pgSourceAlone declares the data source of pgSourceAndSecret and no secret,
// from a source that describes every secret.
func pgSourceAlone(directory, location, secretPath string) string {
	return "secrets: {}\n" + warehouseSource(directory, location, secretPath)
}

// warehouseSource is the external_data_sources section of pgSourceAndSecret
// and pgSourceAlone: the data source warehouse at location whose password the
// secret at secretPath holds.
func warehouseSource(directory, location, secretPath string) string {
	return "external_data_sources:\n  warehouse:\n    schema: " + directory + "\n    source_type: PostgreSQL\n    location: " + location +
		"\n    auth_method: BASIC\n    options:\n      DATABASE_NAME: app\n      LOGIN: reader\n      PASSWORD_SECRET_PATH: " + secretPath + "\n"
}

// Ported from the host's TestYDBSecrets_ReadThroughAnAbsolutePath: a data
// source that names its secret by an absolute path, as YDB stores it, is
// read against the database the plan runs in: the secret is created before
// the source, and the two compare equal after.
func TestHostReadsASecretThroughAnAbsolutePath_HappyPath(t *testing.T) {
	variable := secretVariable(t, secretValue())
	c := testcheck.New(t)
	opened, directory, database := secretsFixture(c)
	c.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		_ = execFixtureSchema(ctx, opened.DB, "DROP EXTERNAL DATA SOURCE `"+path.Join(database, directory, "warehouse")+"`")
	})
	declared := pgSourceAndSecret(directory, "pg.invalid:5432", path.Join(database, directory, "abs_pw"), variable)

	first := hostPlanned(c, directory, "yaml", declared)
	hostRun(c, directory, "the secret and the source", declared, `(?s).*Schema apply completed successfully.*`)

	c.Assert(first, qt.DeepEquals, []string{"secret-operation " + directory + ".abs_pw", "external-data-source-operation " + directory + ".warehouse"})
	c.Assert(hostPlanned(c, directory, "yaml", declared), qt.HasLen, 0)
}

// Ported from the host's TestYDBSecrets_FailurePath_ReadThroughAnAbsolutePath:
// from a data source that reads its secret by an absolute path, a source
// whose path is outside the database is refused, since no statement of it
// could read the secret, and so is a source that leaves the secret out while
// the source still reads it, whether the source is created again or kept.
// Each refusal leaves the database as it was.
//
// The host's engine refuses the drop only when a statement of its plan reads
// the secret, and drops the secret under a source it keeps, which YDB allows
// and which leaves the source naming a secret that is gone. This provider
// refuses both, in the comparison.
func TestHostRefusesASecretPathNoStatementCanRead_FailurePath(t *testing.T) {
	variable := secretVariable(t, secretValue())
	c := testcheck.New(t)
	opened, directory, database := secretsFixture(c)
	c.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		_ = execFixtureSchema(ctx, opened.DB, "DROP EXTERNAL DATA SOURCE `"+path.Join(database, directory, "warehouse")+"`")
	})
	secretPath := path.Join(database, directory, "abs_pw")
	hostRun(c, directory, "the secret and the source", pgSourceAndSecret(directory, "pg.invalid:5432", secretPath, variable), `(?s).*Schema apply completed successfully.*`)

	outside, outsideErr := hostApplySource(c, directory, "yaml", pgSourceAndSecret(directory, "pg2.invalid:5432", "/elsewhere/abs_pw", variable))
	replaced, replacedErr := hostApplySource(c, directory, "yaml", pgSourceAlone(directory, "pg2.invalid:5432", secretPath))
	kept, keptErr := hostApplySource(c, directory, "yaml", pgSourceAlone(directory, "pg.invalid:5432", secretPath))

	c.Assert(outsideErr, qt.IsNotNil, qt.Commentf("%s", outside))
	c.Assert(replacedErr, qt.IsNotNil, qt.Commentf("%s", replaced))
	c.Assert(replaced, qt.Contains, "secret "+directory+"/abs_pw is dropped while a declared object reads it by its path")
	c.Assert(keptErr, qt.IsNotNil, qt.Commentf("%s", kept))
	c.Assert(kept, qt.Contains, "secret "+directory+"/abs_pw is dropped while a declared object reads it by its path")
	c.Assert(listedNames(c, opened, path.Join(database, directory)), qt.DeepEquals, []string{"abs_pw", "warehouse"})
	c.Assert(hostPlanned(c, directory, "yaml", pgSourceAndSecret(directory, "pg.invalid:5432", secretPath, variable)), qt.HasLen, 0)
}

// Ported from the 25.1 half of the host's
// TestYDBSecrets_NotRefusedWithoutTheKeyWhenNothingRuns: on a line without
// secrets, a source that describes every secret and declares none plans
// nothing, so the comparison is not refused. The other half compares against
// a capability set the host substitutes for a 25.3 server, which no server of
// this suite lists.
func TestHostDescribesNoSecretOnALineWithout_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	_, directory, _ := lockFixture(c)
	_, session := accessSession(c)
	skipWith(c, session, secrets.Capability)

	planned := hostPlanned(c, directory, "yaml", "secrets: {}\n")

	c.Assert(planned, qt.HasLen, 0)
}

// Ported from the host's TestYDBSecrets_DropAllTablesDropsThem: db drop-all
// of a dev realm drops the secrets it holds and the column table beside them,
// and removes the directories they leave empty.
func TestHostDropAllDropsSecretsInARealm_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	_, session := accessSession(c)
	skipWithout(c, session, secrets.Capability)
	base, err := connection.Parse(os.Getenv("PTAH_YDB_TEST_URL"))
	c.Assert(err, qt.IsNil)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	opened, err := connection.Open(ctx, base)
	c.Assert(err, qt.IsNil)
	c.Cleanup(func() { c.Check(opened.Close(), qt.IsNil) })
	address, root := realmFixture(c, opened, base)
	c.Cleanup(func() {
		dropSecretsBelow(c, opened, root)
		cleanup, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		_ = opened.Driver.Coordination().DropNode(cleanup, path.Join(root, locking.NodeName))
	})
	for _, statement := range []string{
		"CREATE SECRET `" + path.Join(root, "gone/pw") + "` WITH (value = 'dropped')",
		"CREATE SECRET `" + path.Join(root, "keep/pw") + "` WITH (value = 'dropped')",
		"CREATE TABLE `" + path.Join(root, "keep/olap") + "` (`id` Int64 NOT NULL, PRIMARY KEY (`id`)) PARTITION BY HASH(`id`) WITH (STORE = COLUMN)",
	} {
		c.Assert(execFixtureSchema(c.Context(), opened.DB, statement), qt.IsNil, qt.Commentf("%s", statement))
	}

	output, err := hostRunAt(c, address, "db", "drop-all", "--auto-approve")

	c.Assert(err, qt.IsNil, qt.Commentf("%s", output))
	c.Assert(strings.Join(listedNames(c, opened, root), ","), qt.Not(qt.Matches), `.*(gone|keep).*`)
}
