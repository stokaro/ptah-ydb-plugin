package devdatabase_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	qt "github.com/frankban/quicktest"
	"github.com/stokaro/ptah-ydb-plugin/internal/devdatabase"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
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
	provisioner := devdatabase.New(runtime, ready)

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
	provisioner := devdatabase.New(runtime, ready)
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
	provisioner := devdatabase.New(runtime, ready)

	released, err := provisioner.ReleaseDevDatabase(c.Context(), &protocol.ReleaseDevRequest{Engine: "ydb", Lease: "gone"})

	c.Assert(err, qt.IsNil)
	c.Assert(released, qt.IsNotNil)
	c.Assert(runtime.removed, qt.HasLen, 0)
}

func TestProvisionDevDatabase_FailedReleaseKeepsTheLease(t *testing.T) {
	c := qt.New(t)
	runtime := &fakeRuntime{removeErr: errors.New("docker rm failed")}
	provisioner := devdatabase.New(runtime, ready)
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
	provisioner := devdatabase.New(runtime, ready)
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
			provisioner := devdatabase.New(runtime, ready)

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
		provisioner := devdatabase.New(runtime, ready)

		reply, err := provisioner.ProvisionDevDatabase(c.Context(), request("docker://ydb"))

		c.Assert(err, qt.ErrorMatches, "docker run failed")
		c.Assert(reply, qt.IsNil)
		c.Assert(runtime.removed, qt.DeepEquals, runtime.started)
	})
	t.Run("the container stops before it is ready", func(t *testing.T) {
		c := qt.New(t)
		runtime := &fakeRuntime{running: false}
		provisioner := devdatabase.New(runtime, func(context.Context, string) error { return errors.New("connection refused") })

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
		provisioner := devdatabase.New(runtime, ready)

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
		provisioner := devdatabase.New(runtime, func(ctx context.Context, _ string) error { <-ctx.Done(); return ctx.Err() })

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
