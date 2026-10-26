//go:build integration

package integration_test

import (
	"context"
	"os"
	"testing"
	"time"

	qt "github.com/frankban/quicktest"
	"github.com/stokaro/ptah-ydb-plugin/internal/connection"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/stokaro/ptah/provider-sdk/provider"
	providerruntime "github.com/stokaro/ptah/provider-sdk/runtime"
)

// devImageTag is the local-ydb tag the dev database tests start. They need a
// Docker daemon on the machine that runs them, which the service demands.
const devImageTag = "26.2.1.14"

// startProviderWithDocker starts the provider with the variables the docker
// command needs. The provider runs with an explicit environment, so it does not
// see PATH or the daemon's address unless the host passes them.
func startProviderWithDocker(c *qt.C, binary, digest string) *providerruntime.Client {
	c.Helper()
	var environment []string
	for _, name := range []string{"PATH", "HOME", "DOCKER_HOST", "DOCKER_CONTEXT", "DOCKER_CONFIG"} {
		if value, ok := os.LookupEnv(name); ok {
			environment = append(environment, name+"="+value)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	client, err := providerruntime.Start(ctx, providerruntime.Options{
		Executable: binary, SHA256: digest, ProviderID: "github.com/stokaro/ptah-ydb-plugin", Environment: environment, HostFeatures: []string{provider.ObservedColumnsFeature},
	})
	c.Assert(err, qt.IsNil)
	c.Cleanup(client.Close)
	return client
}

// TestDevDatabaseSubprocess provisions a dev database from each form of spec
// the operator types: the engine form, which names a tag of local-ydb, and the
// image form, which names the image.
func TestDevDatabaseSubprocess(t *testing.T) {
	for _, spec := range []string{
		"docker://ydb/" + devImageTag,
		"docker+ydb://_/ydbplatform/local-ydb:" + devImageTag + "/local",
	} {
		t.Run(spec, func(t *testing.T) { provisionDevDatabase(qt.New(t), spec) })
	}
}

func provisionDevDatabase(c *qt.C, spec string) {
	binary, digest := providerBinary(c)
	client := startProviderWithDocker(c, binary, digest)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	reply, err := client.ProvisionDevDatabase(ctx, &protocol.ProvisionDevRequest{Engine: "ydb", Spec: spec})
	c.Assert(err, qt.IsNil)
	c.Assert(reply.Refusal, qt.IsNil)
	released := false
	release := func() {
		if released {
			return
		}
		released = true
		_, err := client.ReleaseDevDatabase(context.Background(), &protocol.ReleaseDevRequest{Engine: "ydb", Lease: reply.Lease})
		c.Check(err, qt.IsNil)
	}
	c.Cleanup(release)

	session, err := client.Open(ctx, &protocol.OpenRequest{Engine: "ydb", Address: reply.Url})
	c.Assert(err, qt.IsNil)
	c.Cleanup(func() { c.Check(client.CloseSession(context.Background(), session.Id), qt.IsNil) })
	c.Assert(session.Database, qt.Equals, "/local")
	result := executeSQL(c, client, session.Id, "", "CREATE TABLE `records` (id Uint64 NOT NULL, PRIMARY KEY (id))")
	c.Assert(result.Outcome, qt.Equals, protocol.Execution_APPLIED)
	snapshot, err := client.Inspect(ctx, &protocol.InspectRequest{SessionId: session.Id})
	c.Assert(err, qt.IsNil)
	c.Assert(snapshot.Tables, qt.HasLen, 1)

	release()
	address, err := connection.Parse(reply.Url)
	c.Assert(err, qt.IsNil)
	probe, probeCancel := context.WithTimeout(ctx, 20*time.Second)
	defer probeCancel()
	gone, err := connection.Open(probe, address)
	if err == nil {
		_ = gone.Close()
	}
	c.Assert(err, qt.IsNotNil, qt.Commentf("the dev database still answers after its lease was released"))
}

func TestDevDatabaseRefusalsSubprocess(t *testing.T) {
	for _, test := range []struct{ name, spec string }{
		{"another engine", "docker://postgres/16/dev"},
		{"another database", "docker://ydb/" + devImageTag + "/dev"},
		{"a parameter that points elsewhere", "docker://ydb/" + devImageTag + "?database=/other"},
		{"a parameter YDB does not read", "docker://ydb/" + devImageTag + "?unknown=1"},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := qt.New(t)
			binary, digest := providerBinary(c)
			client := startProvider(c, binary, digest)

			reply, err := client.ProvisionDevDatabase(c.Context(), &protocol.ProvisionDevRequest{Engine: "ydb", Spec: test.spec})

			c.Assert(err, qt.IsNil)
			c.Assert(reply.Lease, qt.Equals, "")
			c.Assert(reply.Refusal.GetKind(), qt.Equals, protocol.DevDatabaseRefusal_UNSUPPORTED)
		})
	}
}
