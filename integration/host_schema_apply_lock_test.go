//go:build integration

package integration_test

import (
	"context"
	"io"
	"os/exec"
	"path"
	"testing"
	"time"

	qt "github.com/frankban/quicktest"

	"github.com/stokaro/ptah-ydb-plugin/internal/locking"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
)

// Ported from the host's TestYDBSchemaApplyLock_ReportsALoss, through native
// ptah: `schema apply` holds the schema apply lock while it plans, asks and
// applies. The lock node is dropped while the command waits for its
// confirmation, which ends the session that held the semaphore. Once
// confirmed, the command applies nothing, since another run may have taken
// the lock and changed the schema it planned against: it reports the lost
// lock and creates no table.
// TestHostCompatSchemaApplyStopsWhenTheLockIsLost_FailurePath is the same
// run through ptah-compat.
func TestHostSchemaApplyStopsWhenTheLockIsLost_FailurePath(t *testing.T) {
	c := testcheck.New(t)
	host, manifest := hostManifest(c)
	realm := newCLIRealm(c)
	entities := writeFiles(c, c.TempDir(), map[string]string{"items.go": cliEntities})
	ctx, cancel := context.WithTimeout(c.Context(), 5*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, host, "schema", "apply", "--db-url", realm.address, "--root-dir", entities,
		"--provider-config", manifest, "--provider-engine", "ydb")
	stdin, err := cmd.StdinPipe()
	c.Assert(err, qt.IsNil)
	var output lockedOutput
	cmd.Stdout, cmd.Stderr = &output, &output
	c.Assert(cmd.Start(), qt.IsNil)

	asked := output.waitFor("Type 'YES' to confirm", 2*time.Minute)
	dropped := realm.opened.Driver.Coordination().DropNode(ctx, path.Join(realm.root, locking.NodeName))
	// The holder trusts the server's last answer that it holds the semaphore
	// for half the session timeout; give it longer than that before the
	// command goes on.
	time.Sleep(8 * time.Second)
	_, answerErr := io.WriteString(stdin, "YES\n")
	closeErr := stdin.Close()
	waitErr := cmd.Wait()

	c.Assert(asked, qt.IsTrue, qt.Commentf("%s", output.String()))
	c.Assert(dropped, qt.IsNil)
	c.Assert(answerErr, qt.IsNil)
	c.Assert(closeErr, qt.IsNil)
	c.Assert(waitErr, qt.ErrorMatches, "exit status [1-9][0-9]*", qt.Commentf("%s", output.String()))
	c.Assert(output.String(), qt.Contains, `schema apply lock: advisory lock "`+compatLockName+`" on ydb was lost while it was held`)
	c.Assert(output.String(), qt.Not(qt.Contains), "Schema apply completed successfully.")
	c.Assert(realm.entries(c), qt.HasLen, 0)
}
