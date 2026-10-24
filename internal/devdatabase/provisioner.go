package devdatabase

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/stokaro/ptah-ydb-plugin/internal/connection"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
)

// readyTimeout bounds the wait for a started server to take DDL.
const readyTimeout = 2 * time.Minute

// removeTimeout bounds a removal that runs on a context detached from the
// caller's, so a canceled call still removes what it started.
const removeTimeout = time.Minute

// readyTable is the table the readiness probe creates and drops.
const readyTable = "ptah_ready"

// Probe waits until the server at address takes DDL.
type Probe func(ctx context.Context, address string) error

// Provisioner implements the provider's database.dev service. The zero value
// is not usable; use [New].
type Provisioner struct {
	runtime Runtime
	probe   Probe
	mu      sync.Mutex
	leases  map[string]string
}

// New returns a provisioner that starts containers through runtime and waits
// for them with probe. A nil probe waits with [Ready].
func New(runtime Runtime, probe Probe) *Provisioner {
	if probe == nil {
		probe = Ready
	}
	return &Provisioner{runtime: runtime, probe: probe, leases: map[string]string{}}
}

// NewDocker returns a provisioner that drives the docker command line.
func NewDocker() *Provisioner {
	return New(DockerCLI{RunLabel: "ydb-plugin-" + token(8)}, nil)
}

func token(size int) string {
	raw := make([]byte, size)
	_, _ = rand.Read(raw)
	return hex.EncodeToString(raw)
}

// ProvisionDevDatabase starts a local-ydb container for the spec and returns a
// lease and the address of its database. A spec the provider does not serve and
// a runtime it lacks are typed refusals, not errors. A canceled call removes
// the container it started before it returns.
func (p *Provisioner) ProvisionDevDatabase(ctx context.Context, request *protocol.ProvisionDevRequest) (*protocol.DevDatabase, error) {
	spec, err := ParseSpec(request.GetSpec())
	if err != nil {
		return refused(err)
	}
	if err := p.runtime.Available(ctx, spec.Image); err != nil {
		return refused(err)
	}
	name := "ptah-dev-" + token(8)
	hostPort, err := p.runtime.Start(ctx, name, spec.Image, []string{"YDB_USE_IN_MEMORY_PDISKS=true"}, ServerPort)
	if err != nil {
		p.discard(name)
		return nil, err
	}
	address := spec.URL(hostPort)
	waitCtx, cancel := context.WithTimeout(ctx, readyTimeout)
	defer cancel()
	if err := p.waitReady(waitCtx, name, address); err != nil {
		p.discard(name)
		return nil, err
	}
	lease := token(16)
	p.mu.Lock()
	p.leases[lease] = name
	p.mu.Unlock()
	return &protocol.DevDatabase{Lease: lease, Url: address}, nil
}

// refused turns a refusal into the reply that carries it. Any other error is
// returned as one.
func refused(err error) (*protocol.DevDatabase, error) {
	var refusal *Refusal
	if !errors.As(err, &refusal) {
		return nil, err
	}
	kind := protocol.DevDatabaseRefusal_UNSUPPORTED
	if refusal.Unavailable {
		kind = protocol.DevDatabaseRefusal_UNAVAILABLE
	}
	return &protocol.DevDatabase{Refusal: &protocol.DevDatabaseRefusal{Kind: kind, Reason: refusal.Reason}}, nil
}

// waitReady waits for the server to take DDL, and stops early when the
// container is gone.
func (p *Provisioner) waitReady(ctx context.Context, name, address string) error {
	var last error
	for {
		last = p.probe(ctx, address)
		if last == nil {
			return nil
		}
		if ctx.Err() != nil {
			return fmt.Errorf("the dev database did not become ready: %w", errors.Join(ctx.Err(), last))
		}
		if running, err := p.runtime.Running(ctx, name); err == nil && !running {
			return fmt.Errorf("the dev database container stopped before it became ready: %w", last)
		}
		select {
		case <-ctx.Done():
		case <-time.After(time.Second):
		}
	}
}

// discard removes a container whose provisioning failed.
func (p *Provisioner) discard(name string) {
	ctx, cancel := context.WithTimeout(context.Background(), removeTimeout)
	defer cancel()
	_ = p.runtime.Remove(ctx, name)
}

// ReleaseDevDatabase removes the container a lease names. A lease that is gone
// already succeeds. A removal that fails keeps the lease, so the release can be
// repeated and [Provisioner.Close] tries again.
func (p *Provisioner) ReleaseDevDatabase(ctx context.Context, request *protocol.ReleaseDevRequest) (*protocol.Empty, error) {
	p.mu.Lock()
	name, found := p.leases[request.GetLease()]
	p.mu.Unlock()
	if !found {
		return &protocol.Empty{}, nil
	}
	if err := p.runtime.Remove(context.WithoutCancel(ctx), name); err != nil {
		return nil, err
	}
	p.mu.Lock()
	delete(p.leases, request.GetLease())
	p.mu.Unlock()
	return &protocol.Empty{}, nil
}

// Close removes every container still leased. The provider calls it when its
// process ends, because a host that crashed never releases its leases.
func (p *Provisioner) Close() error {
	p.mu.Lock()
	names := make([]string, 0, len(p.leases))
	for lease, name := range p.leases {
		names = append(names, name)
		delete(p.leases, lease)
	}
	p.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), removeTimeout)
	defer cancel()
	var errs []error
	for _, name := range names {
		errs = append(errs, p.runtime.Remove(ctx, name))
	}
	return errors.Join(errs...)
}

// Ready checks once that the server at address takes DDL: it connects, then
// creates and drops a table. A YDB server answers a query before it takes DDL
// (measured on local-ydb 26.2.1.14, SELECT Version() answered 1.1 seconds after
// the start and CREATE TABLE answered "database doesn't have storage pools at
// all" for another 0.8 to 1.3 seconds), so a connection alone is not readiness.
func Ready(ctx context.Context, address string) (err error) {
	parsed, err := connection.Parse(address)
	if err != nil {
		return err
	}
	probeCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	opened, err := connection.Open(probeCtx, parsed)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, opened.Close()) }()
	if _, err := opened.DB.ExecContext(probeCtx, "CREATE TABLE IF NOT EXISTS `"+readyTable+"` (`id` Int64 NOT NULL, PRIMARY KEY (`id`))"); err != nil {
		return err
	}
	_, err = opened.DB.ExecContext(probeCtx, "DROP TABLE IF EXISTS `"+readyTable+"`")
	return err
}
