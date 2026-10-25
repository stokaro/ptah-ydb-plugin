package devdatabase_test

import (
	"context"
	"errors"
	"net/url"
	"sync"
	"testing"
	"time"

	qt "github.com/frankban/quicktest"
	"github.com/stokaro/ptah-ydb-plugin/internal/backend"
	"github.com/stokaro/ptah-ydb-plugin/internal/connection"
	"github.com/stokaro/ptah-ydb-plugin/internal/devdatabase"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/stokaro/ptah/provider-sdk/provider"
)

// fakeRuntime records what the provisioner asks of a container runtime.
type fakeRuntime struct {
	mu          sync.Mutex
	available   error
	startErr    error
	running     bool
	removeErr   error
	started     []string
	images      []string
	env         [][]string
	removed     []string
	startCancel context.CancelFunc
}

func (r *fakeRuntime) Available(context.Context, string) error { return r.available }

func (r *fakeRuntime) Start(ctx context.Context, name, image string, env []string, _ string) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.started = append(r.started, name)
	r.images = append(r.images, image)
	r.env = append(r.env, env)
	if r.startCancel != nil {
		r.startCancel()
		<-ctx.Done()
		return "", ctx.Err()
	}
	return "127.0.0.1:32768", r.startErr
}

func (r *fakeRuntime) Running(context.Context, string) (bool, error) { return r.running, nil }

func (r *fakeRuntime) Remove(_ context.Context, name string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.removed = append(r.removed, name)
	return r.removeErr
}

func ready(context.Context, string) error { return nil }

func request(spec string) *protocol.ProvisionDevRequest {
	return &protocol.ProvisionDevRequest{Engine: "ydb", Spec: spec}
}

func TestProvisionDevDatabase_HappyPath(t *testing.T) {
	c := qt.New(t)
	runtime := &fakeRuntime{}
	provisioner := devdatabase.New(runtime, ready, nil)

	reply, err := provisioner.ProvisionDevDatabase(c.Context(), request("docker://ydb/26.2.1.14/local"))

	c.Assert(err, qt.IsNil)
	c.Assert(reply.Refusal, qt.IsNil)
	c.Assert(reply.Lease, qt.Not(qt.Equals), "")
	c.Assert(reply.Url, qt.Equals, "ydb://127.0.0.1:32768/local?go_balancer=disable")
	c.Assert(runtime.images, qt.DeepEquals, []string{"ydbplatform/local-ydb:26.2.1.14"})
	c.Assert(runtime.env, qt.DeepEquals, [][]string{{"YDB_USE_IN_MEMORY_PDISKS=true"}})
	c.Assert(runtime.removed, qt.HasLen, 0)
}

func TestProvisionDevDatabase_ReleaseRemovesTheContainerOnce(t *testing.T) {
	c := qt.New(t)
	runtime := &fakeRuntime{}
	provisioner := devdatabase.New(runtime, ready, nil)
	reply, err := provisioner.ProvisionDevDatabase(c.Context(), request("docker://ydb"))
	c.Assert(err, qt.IsNil)

	for range 2 {
		released, err := provisioner.ReleaseDevDatabase(c.Context(), &protocol.ReleaseDevRequest{Engine: "ydb", Lease: reply.Lease})
		c.Assert(err, qt.IsNil)
		c.Assert(released, qt.IsNotNil)
	}

	c.Assert(runtime.removed, qt.DeepEquals, runtime.started)
}

func TestProvisionDevDatabase_ReleaseOfAnUnknownLeaseSucceeds(t *testing.T) {
	c := qt.New(t)
	runtime := &fakeRuntime{}
	provisioner := devdatabase.New(runtime, ready, nil)

	released, err := provisioner.ReleaseDevDatabase(c.Context(), &protocol.ReleaseDevRequest{Engine: "ydb", Lease: "gone"})

	c.Assert(err, qt.IsNil)
	c.Assert(released, qt.IsNotNil)
	c.Assert(runtime.removed, qt.HasLen, 0)
}

func TestProvisionDevDatabase_FailedReleaseKeepsTheLease(t *testing.T) {
	c := qt.New(t)
	runtime := &fakeRuntime{removeErr: errors.New("docker rm failed")}
	provisioner := devdatabase.New(runtime, ready, nil)
	reply, err := provisioner.ProvisionDevDatabase(c.Context(), request("docker://ydb"))
	c.Assert(err, qt.IsNil)

	_, err = provisioner.ReleaseDevDatabase(c.Context(), &protocol.ReleaseDevRequest{Engine: "ydb", Lease: reply.Lease})
	c.Assert(err, qt.ErrorMatches, "docker rm failed")
	runtime.removeErr = nil
	c.Assert(provisioner.Close(), qt.IsNil)

	c.Assert(runtime.removed, qt.DeepEquals, []string{runtime.started[0], runtime.started[0]})
}

func TestProvisionDevDatabase_CloseRemovesEveryLease(t *testing.T) {
	c := qt.New(t)
	runtime := &fakeRuntime{}
	provisioner := devdatabase.New(runtime, ready, nil)
	for range 3 {
		_, err := provisioner.ProvisionDevDatabase(c.Context(), request("docker://ydb"))
		c.Assert(err, qt.IsNil)
	}

	c.Assert(provisioner.Close(), qt.IsNil)
	c.Assert(provisioner.Close(), qt.IsNil)

	c.Assert(runtime.started, qt.HasLen, 3)
	c.Assert(runtime.removed, qt.ContentEquals, runtime.started)
}

func TestProvisionDevDatabase_RefusalIsTyped(t *testing.T) {
	for _, test := range []struct {
		name      string
		spec      string
		available error
		kind      protocol.DevDatabaseRefusal_Kind
		reason    string
	}{
		{"spec not served", "docker://postgres/16/dev", nil, protocol.DevDatabaseRefusal_UNSUPPORTED, `unsupported docker image "postgres"`},
		{"runtime missing", "docker://ydb", &devdatabase.Refusal{Unavailable: true, Reason: "the docker command is not installed"}, protocol.DevDatabaseRefusal_UNAVAILABLE, "the docker command is not installed"},
		{"runtime elsewhere", "docker://ydb", &devdatabase.Refusal{Reason: "runs on remote-dev"}, protocol.DevDatabaseRefusal_UNSUPPORTED, "runs on remote-dev"},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := qt.New(t)
			runtime := &fakeRuntime{available: test.available}
			provisioner := devdatabase.New(runtime, ready, nil)

			reply, err := provisioner.ProvisionDevDatabase(c.Context(), request(test.spec))

			c.Assert(err, qt.IsNil)
			c.Assert(reply.Lease, qt.Equals, "")
			c.Assert(reply.Url, qt.Equals, "")
			c.Assert(reply.Refusal.Kind, qt.Equals, test.kind)
			c.Assert(reply.Refusal.Reason, qt.Equals, test.reason)
			c.Assert(runtime.started, qt.HasLen, 0)
		})
	}
}

func TestProvisionDevDatabase_FailurePath(t *testing.T) {
	t.Run("start fails", func(t *testing.T) {
		c := qt.New(t)
		runtime := &fakeRuntime{startErr: errors.New("docker run failed")}
		provisioner := devdatabase.New(runtime, ready, nil)

		reply, err := provisioner.ProvisionDevDatabase(c.Context(), request("docker://ydb"))

		c.Assert(err, qt.ErrorMatches, "docker run failed")
		c.Assert(reply, qt.IsNil)
		c.Assert(runtime.removed, qt.DeepEquals, runtime.started)
	})
	t.Run("the container stops before it is ready", func(t *testing.T) {
		c := qt.New(t)
		runtime := &fakeRuntime{running: false}
		provisioner := devdatabase.New(runtime, func(context.Context, string) error { return errors.New("connection refused") }, nil)

		reply, err := provisioner.ProvisionDevDatabase(c.Context(), request("docker://ydb"))

		c.Assert(err, qt.ErrorMatches, "the dev database container stopped before it became ready: connection refused")
		c.Assert(reply, qt.IsNil)
		c.Assert(runtime.removed, qt.DeepEquals, runtime.started)
	})
	t.Run("the call is canceled while the container starts", func(t *testing.T) {
		c := qt.New(t)
		ctx, cancel := context.WithCancel(c.Context())
		defer cancel()
		runtime := &fakeRuntime{startCancel: cancel}
		provisioner := devdatabase.New(runtime, ready, nil)

		reply, err := provisioner.ProvisionDevDatabase(ctx, request("docker://ydb"))

		c.Assert(err, qt.ErrorIs, context.Canceled)
		c.Assert(reply, qt.IsNil)
		c.Assert(runtime.removed, qt.DeepEquals, runtime.started)
	})
	t.Run("the call is canceled while the server starts", func(t *testing.T) {
		c := qt.New(t)
		ctx, cancel := context.WithTimeout(c.Context(), 50*time.Millisecond)
		defer cancel()
		runtime := &fakeRuntime{running: true}
		provisioner := devdatabase.New(runtime, func(ctx context.Context, _ string) error { <-ctx.Done(); return ctx.Err() }, nil)

		reply, err := provisioner.ProvisionDevDatabase(ctx, request("docker://ydb"))

		c.Assert(err, qt.ErrorIs, context.DeadlineExceeded)
		c.Assert(reply, qt.IsNil)
		c.Assert(runtime.removed, qt.DeepEquals, runtime.started)
	})
}

func TestDockerCLIAvailable_FailurePath(t *testing.T) {
	c := qt.New(t)
	c.Setenv("PATH", c.TempDir())

	err := devdatabase.DockerCLI{RunLabel: "test"}.Available(c.Context(), "ydbplatform/local-ydb:26.2.1.14")

	var refusal *devdatabase.Refusal
	c.Assert(errors.As(err, &refusal), qt.IsTrue)
	c.Assert(refusal.Unavailable, qt.IsTrue)
	c.Assert(refusal.Reason, qt.Matches, `the docker command is not on PATH; .* pass PATH, and DOCKER_HOST or DOCKER_CONTEXT .*`)
}

// fakeRealms records the realms the provisioner creates and removes.
type fakeRealms struct {
	createErr error
	created   []string
	removed   []string
}

func (r *fakeRealms) Create(_ context.Context, address connection.Address, name string) error {
	r.created = append(r.created, address.Database()+"/"+name)
	return r.createErr
}

func (r *fakeRealms) Remove(_ context.Context, address connection.Address, name string) error {
	r.removed = append(r.removed, address.Database()+"/"+name)
	return nil
}

func realmRequest(address string) *protocol.ProvisionDevRequest {
	return &protocol.ProvisionDevRequest{Engine: "ydb", Address: address}
}

func TestProvisionDevRealm_HappyPath(t *testing.T) {
	c := qt.New(t)
	realms := &fakeRealms{}
	provisioner := devdatabase.New(&fakeRuntime{}, ready, realms)
	request := realmRequest("ydbs://reader:secret@db.example:2135/Root/app?dev_realm=earlier&go_balancer=disable")

	reply, err := provisioner.ProvisionDevDatabase(t.Context(), request)

	c.Assert(err, qt.IsNil)
	c.Assert(provider.ValidateDevDatabase(backend.Description(), request, reply), qt.IsNil)
	parsed, err := url.Parse(reply.GetUrl())
	c.Assert(err, qt.IsNil)
	name := parsed.Query().Get("dev_realm")
	c.Assert(name, qt.Matches, `[0-9a-f]{16}`)
	c.Assert(parsed.Query().Get("go_balancer"), qt.Equals, "disable")
	c.Assert(parsed.User.String(), qt.Equals, "reader:secret")
	c.Assert(parsed.Host+parsed.Path, qt.Equals, "db.example:2135/Root/app")
	c.Assert(realms.created, qt.DeepEquals, []string{"/Root/app/" + name})

	_, err = provisioner.ReleaseDevDatabase(t.Context(), &protocol.ReleaseDevRequest{Engine: "ydb", Lease: reply.GetLease()})
	c.Assert(err, qt.IsNil)
	_, err = provisioner.ReleaseDevDatabase(t.Context(), &protocol.ReleaseDevRequest{Engine: "ydb", Lease: reply.GetLease()})
	c.Assert(err, qt.IsNil)
	c.Assert(realms.removed, qt.DeepEquals, []string{"/Root/app/" + name})
}

// TestCloseRemovesTheRealmsItHolds pins that a provider that ends removes
// the realms a host did not release.
func TestCloseRemovesTheRealmsItHolds(t *testing.T) {
	c := qt.New(t)
	realms := &fakeRealms{}
	provisioner := devdatabase.New(&fakeRuntime{}, ready, realms)
	reply, err := provisioner.ProvisionDevDatabase(t.Context(), realmRequest("ydb://db.example:2136/local"))
	c.Assert(err, qt.IsNil)

	c.Assert(provisioner.Close(), qt.IsNil)

	c.Assert(realms.removed, qt.HasLen, 1)
	c.Assert(realms.removed[0], qt.Equals, realms.created[0])
	c.Assert(reply.GetUrl(), qt.Contains, "dev_realm=")
}

func TestProvisionDevRealm_FailurePath(t *testing.T) {
	for _, test := range []struct {
		name    string
		realms  devdatabase.Realms
		address string
		kind    protocol.DevDatabaseRefusal_Kind
		reason  string
	}{
		{"no realms", nil, "ydb://db.example:2136/local", protocol.DevDatabaseRefusal_UNSUPPORTED, "this provider creates no dev realm"},
		{"not a YDB address", &fakeRealms{}, "postgres://db.example:5432/app", protocol.DevDatabaseRefusal_UNSUPPORTED, "the dev realm address is not a YDB address: .*"},
		{"an unknown parameter", &fakeRealms{}, "ydb://db.example:2136/local?secret-marker=1", protocol.DevDatabaseRefusal_UNSUPPORTED, "the dev realm address is not a YDB address: .*"},
		{"no database", &fakeRealms{}, "ydb://db.example:2136", protocol.DevDatabaseRefusal_UNSUPPORTED, "the dev realm address is not a YDB address: YDB URL must name a database"},
		{"unreachable", &fakeRealms{createErr: errors.New("the database could not be reached")}, "ydb://db.example:2136/local",
			protocol.DevDatabaseRefusal_UNAVAILABLE, "the database the dev realm address names could not take a realm: the database could not be reached"},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := qt.New(t)
			provisioner := devdatabase.New(&fakeRuntime{}, ready, test.realms)

			reply, err := provisioner.ProvisionDevDatabase(t.Context(), realmRequest(test.address))

			c.Assert(err, qt.IsNil)
			c.Assert(reply.GetLease(), qt.Equals, "")
			c.Assert(reply.GetRefusal().GetKind(), qt.Equals, test.kind)
			c.Assert(reply.GetRefusal().GetReason(), qt.Matches, test.reason)
			c.Assert(reply.GetRefusal().GetReason(), qt.Not(qt.Contains), "secret-marker=")
		})
	}
}
