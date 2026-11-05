//go:build integration

package integration_test

import (
	"context"
	"io"
	"net"
	"net/url"
	"os"
	"path"
	"sync"
	"testing"
	"time"

	qt "github.com/frankban/quicktest"
	"github.com/ydb-platform/ydb-go-genproto/Ydb_Operation_V1"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb_Table"
	"github.com/ydb-platform/ydb-go-sdk/v3"
	"github.com/ydb-platform/ydb-go-sdk/v3/table"

	"github.com/stokaro/ptah-ydb-plugin/internal/connection"
	"github.com/stokaro/ptah-ydb-plugin/internal/nativeapi"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
)

// cuttingProxy forwards TCP connections to a server, and cut closes every
// connection it carries at that moment, as a network failure does. A
// connection opened after a cut is forwarded again.
type cuttingProxy struct {
	listener net.Listener
	target   string

	mu    sync.Mutex
	conns []net.Conn
}

// startCuttingProxy listens on a local port and forwards to target until the
// test ends.
func startCuttingProxy(c *qt.C, target string) *cuttingProxy {
	c.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	c.Assert(err, qt.IsNil)
	proxy := &cuttingProxy{listener: listener, target: target}
	go proxy.serve()
	c.Cleanup(func() {
		_ = listener.Close()
		proxy.cut()
	})
	return proxy
}

func (p *cuttingProxy) serve() {
	for {
		client, err := p.listener.Accept()
		if err != nil {
			return
		}
		server, err := net.Dial("tcp", p.target)
		if err != nil {
			_ = client.Close()
			continue
		}
		p.mu.Lock()
		p.conns = append(p.conns, client, server)
		p.mu.Unlock()
		go p.pipe(client, server)
		go p.pipe(server, client)
	}
}

func (p *cuttingProxy) pipe(to, from net.Conn) {
	_, _ = io.Copy(to, from)
	_ = to.Close()
	_ = from.Close()
}

// cut closes every connection the proxy carries.
func (p *cuttingProxy) cut() {
	p.mu.Lock()
	conns := p.conns
	p.conns = nil
	p.mu.Unlock()
	for _, conn := range conns {
		_ = conn.Close()
	}
}

// throughProxy is the test URL with the proxy as its endpoint. The balancer
// is off in the test URL, so the driver keeps to that endpoint rather than to
// the node discovery names.
func throughProxy(c *qt.C, proxy *cuttingProxy) string {
	c.Helper()
	parsed, err := url.Parse(os.Getenv("PTAH_YDB_TEST_URL"))
	c.Assert(err, qt.IsNil)
	parsed.Host = proxy.listener.Addr().String()
	return parsed.String()
}

// indexStatus reads the state of index on the table at absolute through the
// fixture's own connection, and reports whether the table has the index.
func indexStatus(opened *connection.Connection, absolute, index string) (Ydb_Table.TableIndexDescription_Status, bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var status Ydb_Table.TableIndexDescription_Status
	found := false
	_ = opened.Driver.Table().Do(ctx, func(ctx context.Context, session table.Session) error {
		described, err := session.DescribeTable(ctx, absolute)
		if err != nil {
			return err
		}
		for _, held := range described.Indexes {
			if held.Name == index {
				status, found = held.Status, true
			}
		}
		return nil
	})
	return status, found
}

// backgroundRun is what a host command a test runs in the background wrote
// and the error it exited with.
type backgroundRun struct {
	output string
	err    error
}

// waitForBuild waits until the server lists index on the table at absolute,
// which it does from the moment the build starts, and fails the test when the
// run ends before that or the build does not start within two minutes.
func waitForBuild(c *qt.C, opened *connection.Connection, absolute, index string, run <-chan backgroundRun) {
	c.Helper()
	deadline := time.Now().Add(2 * time.Minute)
	for time.Now().Before(deadline) {
		select {
		case ended := <-run:
			c.Fatalf("the migration ended before the server started building %s: %v\n%s", index, ended.err, ended.output)
		default:
		}
		if _, found := indexStatus(opened, absolute, index); found {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	c.Fatalf("the server did not start building %s within two minutes", index)
}

// waitForReady reports whether index on the table at absolute becomes ready
// to use within five minutes: the build ran to its end.
func waitForReady(opened *connection.Connection, absolute, index string) bool {
	deadline := time.Now().Add(5 * time.Minute)
	for time.Now().Before(deadline) {
		if status, found := indexStatus(opened, absolute, index); found && status == Ydb_Table.TableIndexDescription_STATUS_READY {
			return true
		}
		time.Sleep(200 * time.Millisecond)
	}
	return false
}

// Ported from the host's TestYDBMigrator_TransportFailureLeavesTheOutcomeUnknown:
// the connection breaks while ALTER TABLE ... ADD INDEX runs, the client gets
// a transport error, and the server builds the index all the same. The
// statement ran outside a transaction after the host marked it in flight, so
// the mark stays: a resume refuses to run the statement again rather than
// guess, which here would answer that the index exists. A failure the server
// answered is recorded as one; TestHostResumesWhereAFailedMigrationStopped_HappyPath
// is that control.
//
// The cut waits for the server's own sign that it took the statement: the
// index the build creates, which DescribeTable lists while it builds. The
// cleanup cancels a build still running, since until it ends the table is
// locked.
func TestHostTransportFailureLeavesTheOutcomeUnknown_FailurePath(t *testing.T) {
	c := testcheck.New(t)
	f := timeoutFixture(c)
	absolute := path.Join(f.database, f.dir, "big")
	c.Cleanup(func() {
		builds := nativeapi.NewBuilds(Ydb_Operation_V1.NewOperationServiceClient(ydb.GRPCConn(f.opened.Driver)), f.database)
		_, err := builds.CancelRunning(context.Background(), f.table("big"), nativeapi.BuildWait{Settle: 2 * time.Minute, Poll: 100 * time.Millisecond})
		c.Check(err, qt.IsNil)
	})
	fillRows(c, f.opened, absolute, 1000000)
	parsed, err := url.Parse(os.Getenv("PTAH_YDB_TEST_URL"))
	c.Assert(err, qt.IsNil)
	proxy := startCuttingProxy(c, parsed.Host)
	migrations := writeFiles(c, c.TempDir(), map[string]string{
		"0000000001_index.up.sql":   "ALTER TABLE `" + f.table("big") + "` ADD INDEX big_v GLOBAL SYNC ON (v);\n",
		"0000000001_index.down.sql": "ALTER TABLE `" + f.table("big") + "` DROP INDEX big_v;\n",
	})
	// The host and its manifest are resolved here, since the goroutine may not
	// skip or fail the test.
	host, manifest := hostManifest(c)
	interruptible := []string{"migrations", "up", "--db-url", throughProxy(c, proxy), "--migrations-dir", migrations, "--migrations-schema", f.dir,
		"--provider-config", manifest, "--provider-engine", "ydb"}
	run := make(chan backgroundRun, 1)
	go func() {
		output, err := runHost(c, host, interruptible...)
		run <- backgroundRun{output: output, err: err}
	}()

	waitForBuild(c, f.opened, absolute, "big_v", run)
	proxy.cut()
	interrupted := <-run
	ready := waitForReady(f.opened, absolute, "big_v")
	resumed, resumeErr := hostCommand(c, append([]string{"migrations", "up", "--allow-dirty"}, f.target(migrations)...)...)

	c.Assert(interrupted.err, qt.IsNotNil, qt.Commentf("%s", interrupted.output))
	c.Assert(ready, qt.IsTrue)
	c.Assert(resumeErr, qt.IsNotNil, qt.Commentf("%s", resumed))
	c.Assert(resumed, qt.Contains, "migration 1 cannot resume automatically: the outcome of statement 1 is unknown",
		qt.Commentf("the interrupted run:\n%s", interrupted.output))
}
