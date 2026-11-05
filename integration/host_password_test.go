//go:build integration

package integration_test

import (
	"context"
	"crypto/rand"
	"os"
	"path"
	"path/filepath"
	"strings"
	"testing"
	"time"

	qt "github.com/frankban/quicktest"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
	"github.com/stokaro/ptah/provider-sdk/provider"
)

// passwordVariable is the variable the host names a user's password by: the
// PTAH_SECRET_PASSWORD_ prefix and the name in upper case, which for a YDB
// user, lower-case letters and digits, is the name with its letters raised.
func passwordVariable(user string) string {
	return "PTAH_SECRET_PASSWORD_" + strings.ToUpper(user)
}

// passwordSource writes a YAML schema that declares user, who logs in with
// password, and returns its path.
func passwordSource(c *qt.C, user, password string) string {
	c.Helper()
	path := filepath.Join(c.TempDir(), "schema.yaml")
	c.Assert(os.WriteFile(path, []byte("roles:\n  "+user+":\n    login: true\n    password: \""+password+"\"\n"), 0o600), qt.IsNil)
	return path
}

// Ported from the host's TestYDBBinary_KeepsAUsersPasswordOutOfEveryOutput:
// the host drives this provider through every way a declared user password
// reaches YDB -- a saved plan that schema apply --plan runs, a plan schema
// apply computes and runs in one go, a dry run and its JSON document, a
// comparison, a generated migration that migrations up runs, and a password
// the server refuses. Each user logs in with the password the schema declares
// and not with another. Everything the runs wrote, the saved plan and the
// migration files hold no password: the statements name each user's variable
// (ADR 0071). A saved plan and a migration file read the value from that
// variable, which the test exports as an operator would; the computed apply
// reads the declaration and has no variable set.
func TestHostKeepsAUsersPasswordOutOfEveryOutput_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	requireHostNegotiates(c, provider.PasswordInputsFeature)
	opened, directory, database := lockFixture(c)
	// The revision tables go below the fixture's directory, in a directory of
	// their own the test removes, so the directory is empty when it goes.
	revisions := path.Join(directory, "m")
	c.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		removeTree(ctx, opened, path.Join(database, revisions))
	})
	suffix := strings.ToLower(rand.Text())[:8]
	planned, applied, migrated, refused := "ptahpwp"+suffix, "ptahpwa"+suffix, "ptahpwm"+suffix, "ptahpwr"+suffix
	passwords := map[string]string{planned: "Plan1SENTINEL" + suffix, applied: "Apply1SENTINEL" + suffix,
		migrated: "Migrate1SENTINEL" + suffix, refused: "Refuse1SENTINEL,comma"}
	c.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		for user := range passwords {
			c.Check(execFixtureSchema(ctx, opened.DB, "DROP USER IF EXISTS `"+user+"`"), qt.IsNil)
		}
	})
	address := os.Getenv("PTAH_YDB_TEST_URL")
	planFile := filepath.Join(c.TempDir(), "plan.json")
	migrations := c.TempDir()
	scope := []string{"--db-url", address, "--schemas", directory}
	appliedSource, migratedSource := passwordSource(c, applied, passwords[applied]), passwordSource(c, migrated, passwords[migrated])

	plannedOut, planErr := hostCommand(c, append([]string{"schema", "plan", "--schema-file", passwordSource(c, planned, passwords[planned]),
		"--output", planFile}, scope...)...)
	c.Setenv(passwordVariable(planned), passwords[planned])
	fromPlanOut, fromPlanErr := hostCommand(c, "schema", "apply", "--plan", planFile, "--db-url", address, "--auto-approve")
	previewOut, previewErr := hostCommand(c, append([]string{"schema", "apply", "--schema-file", appliedSource, "--dry-run"}, scope...)...)
	jsonOut, jsonErr := hostCommand(c, append([]string{"schema", "apply", "--schema-file", appliedSource, "--dry-run", "--json"}, scope...)...)
	appliedOut, applyErr := hostCommand(c, append([]string{"schema", "apply", "--schema-file", appliedSource, "--auto-approve"}, scope...)...)
	comparedOut, _ := hostCommand(c, append([]string{"schema", "compare", "--schema-file", migratedSource}, scope...)...)
	generatedOut, generateErr := hostCommand(c, append([]string{"migrations", "generate", "--schema-file", migratedSource,
		"--migrations-dir", migrations, "--name", "user"}, scope...)...)
	c.Setenv(passwordVariable(migrated), passwords[migrated])
	upOut, upErr := hostCommand(c, "migrations", "up", "--db-url", address, "--migrations-dir", migrations, "--migrations-schema", revisions)
	refusedOut, refusedErr := hostCommand(c, append([]string{"schema", "apply", "--schema-file", passwordSource(c, refused, passwords[refused]),
		"--auto-approve"}, scope...)...)

	c.Assert(planErr, qt.IsNil, qt.Commentf("schema plan:\n%s", plannedOut))
	c.Assert(plannedOut, qt.Contains, "CREATE USER `"+planned+"` PASSWORD $"+passwordVariable(planned)+";")
	c.Assert(fromPlanErr, qt.IsNil, qt.Commentf("schema apply --plan:\n%s", fromPlanOut))
	c.Assert(previewErr, qt.IsNil, qt.Commentf("schema apply --dry-run:\n%s", previewOut))
	c.Assert(previewOut, qt.Contains, "CREATE USER `"+applied+"` PASSWORD $"+passwordVariable(applied)+";")
	c.Assert(jsonErr, qt.IsNil, qt.Commentf("schema apply --dry-run --json:\n%s", jsonOut))
	c.Assert(jsonOut, qt.Contains, passwordVariable(applied))
	c.Assert(applyErr, qt.IsNil, qt.Commentf("schema apply:\n%s", appliedOut))
	c.Assert(comparedOut, qt.Contains, "roles_added (1): "+migrated+"\n")
	c.Assert(generateErr, qt.IsNil, qt.Commentf("migrations generate:\n%s", generatedOut))
	c.Assert(upErr, qt.IsNil, qt.Commentf("migrations up:\n%s", upOut))
	c.Assert(refusedErr, qt.IsNotNil)
	c.Assert(refusedOut, qt.Contains, "YDB refused the statement")
	for _, user := range []string{planned, applied, migrated} {
		c.Assert(loginError(c, user, passwords[user]), qt.IsNil, qt.Commentf("user %s", user))
		c.Assert(loginError(c, user, "Other1password"), qt.IsNotNil, qt.Commentf("user %s", user))
	}

	plan, err := os.ReadFile(planFile)
	c.Assert(err, qt.IsNil)
	c.Assert(string(plan), qt.Contains, "PASSWORD $"+passwordVariable(planned))
	upFiles, err := filepath.Glob(filepath.Join(migrations, "*user*.up.sql"))
	c.Assert(err, qt.IsNil)
	c.Assert(upFiles, qt.HasLen, 1)
	upSQL, err := os.ReadFile(upFiles[0])
	c.Assert(err, qt.IsNil)
	c.Assert(string(upSQL), qt.Contains, "CREATE USER `"+migrated+"` PASSWORD $"+passwordVariable(migrated)+";")
	written := []string{plannedOut, fromPlanOut, previewOut, jsonOut, appliedOut, comparedOut, generatedOut, upOut, refusedOut, string(plan)}
	files, err := os.ReadDir(migrations)
	c.Assert(err, qt.IsNil)
	for _, file := range files {
		body, err := os.ReadFile(filepath.Join(migrations, file.Name()))
		c.Assert(err, qt.IsNil)
		written = append(written, string(body))
	}
	for _, text := range written {
		c.Assert(text, qt.Not(qt.Contains), "SENTINEL")
		c.Assert(text, qt.Not(qt.Contains), "plain text")
	}
	c.Assert(written, qt.HasLen, 10+len(files))
	c.Assert(len(files) >= 2, qt.IsTrue, qt.Commentf("migrations generate wrote %d files", len(files)))
}
