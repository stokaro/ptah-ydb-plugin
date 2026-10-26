package nativeapi

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path"

	"github.com/stokaro/ptah-ydb-plugin/internal/workload"
	"github.com/stokaro/ptah-ydb-plugin/internal/ydbtype"
	"github.com/stokaro/ptah-ydb-plugin/schema"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
)

// HeldWorkload is the resource pools and the classifiers a database holds,
// by name.
type HeldWorkload struct {
	Pools       map[string]workload.PoolSpec
	Classifiers map[string]workload.ClassifierSpec
}

// ReadWorkload reads the resource pools and the classifiers of database from
// its system views. A setting YDB reports as -1, its spelling of no limit, is
// unset, and a classifier with no member reports an empty one. Both views list
// nothing while the EnableResourcePools flag is off, so an empty answer
// establishes absence only on a target that keeps resource pools.
func ReadWorkload(ctx context.Context, db *sql.DB, database string) (held HeldWorkload, err error) {
	held = HeldWorkload{Pools: map[string]workload.PoolSpec{}, Classifiers: map[string]workload.ClassifierSpec{}}
	pools, err := db.QueryContext(ctx, "SELECT Name, ConcurrentQueryLimit, QueueSize, DatabaseLoadCpuThreshold, QueryMemoryLimitPercentPerNode, "+
		"QueryCpuLimitPercentPerNode, TotalCpuLimitPercentPerNode, ResourceWeight FROM "+ydbtype.QuoteIdentifier(path.Join(database, ".sys/resource_pools")))
	if err != nil {
		return HeldWorkload{}, fmt.Errorf("read YDB resource pools: %w", err)
	}
	defer func() { err = errors.Join(err, pools.Close()) }()
	for pools.Next() {
		var name sql.NullString
		var concurrent, queue sql.NullInt32
		var threshold, memory, cpu, total, weight sql.NullFloat64
		if err := pools.Scan(&name, &concurrent, &queue, &threshold, &memory, &cpu, &total, &weight); err != nil {
			return HeldWorkload{}, fmt.Errorf("read YDB resource pools: %w", err)
		}
		if !name.Valid || !workload.ValidName(name.String) {
			return HeldWorkload{}, fmt.Errorf("read YDB resource pools: a pool has a name YDB does not take")
		}
		if _, found := held.Pools[name.String]; found {
			return HeldWorkload{}, fmt.Errorf("read YDB resource pools: pool %q is listed twice", name.String)
		}
		held.Pools[name.String] = workload.PoolSpec{ConcurrentQueryLimit: limit(concurrent), QueueSize: limit(queue), DatabaseLoadCPUThreshold: share(threshold),
			QueryMemoryLimitPercentPerNode: share(memory), QueryCPULimitPercentPerNode: share(cpu), TotalCPULimitPercentPerNode: share(total), ResourceWeight: share(weight)}
	}
	if err := pools.Err(); err != nil {
		return HeldWorkload{}, fmt.Errorf("read YDB resource pools: %w", err)
	}
	classifiers, err := db.QueryContext(ctx, "SELECT Name, Rank, MemberName, ResourcePool FROM "+ydbtype.QuoteIdentifier(path.Join(database, ".sys/resource_pool_classifiers")))
	if err != nil {
		return HeldWorkload{}, fmt.Errorf("read YDB resource pool classifiers: %w", err)
	}
	defer func() { err = errors.Join(err, classifiers.Close()) }()
	for classifiers.Next() {
		var name, member, pool sql.NullString
		var rank sql.NullInt64
		if err := classifiers.Scan(&name, &rank, &member, &pool); err != nil {
			return HeldWorkload{}, fmt.Errorf("read YDB resource pool classifiers: %w", err)
		}
		if !name.Valid || !workload.ValidName(name.String) || !rank.Valid || !pool.Valid {
			return HeldWorkload{}, fmt.Errorf("read YDB resource pool classifiers: a classifier is listed incomplete")
		}
		if _, found := held.Classifiers[name.String]; found {
			return HeldWorkload{}, fmt.Errorf("read YDB resource pool classifiers: classifier %q is listed twice", name.String)
		}
		held.Classifiers[name.String] = workload.ClassifierSpec{ResourcePool: pool.String, MemberName: member.String, Rank: rank.Int64}
	}
	if err := classifiers.Err(); err != nil {
		return HeldWorkload{}, fmt.Errorf("read YDB resource pool classifiers: %w", err)
	}
	return held, nil
}

func limit(value sql.NullInt32) *int32 {
	if !value.Valid || value.Int32 == -1 {
		return nil
	}
	return new(value.Int32)
}

func share(value sql.NullFloat64) *float64 {
	if !value.Valid || value.Float64 == -1 {
		return nil
	}
	return new(value.Float64)
}

// Workload owns the precondition, the dispatch and the confirmation of a
// workload operation. Pools and classifiers change through YQL, as the host's
// embedded engine changes them, and are read from the system views.
type Workload struct {
	// Read reads what the database holds.
	Read func(context.Context) (HeldWorkload, error)
	// Execute runs one schema statement through the query service, once.
	Execute func(context.Context, string) error
}

// NewWorkload binds the session's database handle and statement runner.
func NewWorkload(db *sql.DB, database string, execute func(context.Context, string) error) *Workload {
	return &Workload{Read: func(ctx context.Context) (HeldWorkload, error) { return ReadWorkload(ctx, db, database) }, Execute: execute}
}

// Apply verifies that every pool and classifier is as the plan read it, or
// absent for a creation, runs the operation's statements one query each, and
// reads the result back. Dispatched is true as soon as a statement was sent.
// A failed statement or an unconfirmed read leaves the outcome uncertain;
// nothing is retried. YDB has no atomic schema compare-and-set, so another
// writer can change an object between the check and the statement.
func (w *Workload) Apply(ctx context.Context, database string, value *protocol.Envelope, beforeWrite func(context.Context) error) (dispatched bool, err error) {
	normalized, err := schema.NormalizeWorkloadOperation(&protocol.ValueInput{Id: "execute", Representation: protocol.ValueInput_OPERATION, Value: value})
	if err != nil {
		return false, err
	}
	if owner := normalized.NormalizedOwner.GetDatabase(); owner != "" && owner != database {
		return false, fmt.Errorf("workload operation targets another database")
	}
	changes, err := schema.DecodeWorkloadOperation(normalized.Value)
	if err != nil {
		return false, err
	}
	statements, err := workload.Statements(changes.Pools, changes.Classifiers)
	if err != nil {
		return false, err
	}
	held, err := w.Read(ctx)
	if err != nil {
		return false, err
	}
	if subject := differs(held, changes, true); subject != "" {
		return false, fmt.Errorf("%s changed since the plan read it", subject)
	}
	if err := beforeWrite(ctx); err != nil {
		return false, err
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	for _, statement := range statements {
		if err := w.Execute(ctx, statement); err != nil {
			return true, err
		}
	}
	held, err = w.Read(ctx)
	if err != nil {
		return true, err
	}
	if subject := differs(held, changes, false); subject != "" {
		return true, fmt.Errorf("%s was not confirmed: the database holds it otherwise", subject)
	}
	return true, nil
}

// differs names the first object held does not carry as a change's before
// side, or its after side once the operation ran, and is empty when held
// carries them all.
func differs(held HeldWorkload, changes schema.WorkloadChanges, before bool) string {
	for _, change := range changes.Pools {
		want := change.After
		if before {
			want = change.Before
		}
		have, found := held.Pools[change.Name]
		if (want == nil) != !found || (want != nil && !workload.PoolsEqual(*want, have)) {
			return fmt.Sprintf("resource pool %q", change.Name)
		}
	}
	for _, change := range changes.Classifiers {
		want := change.After
		if before {
			want = change.Before
		}
		have, found := held.Classifiers[change.Name]
		if (want == nil) != !found || (want != nil && *want != have) {
			return fmt.Sprintf("resource pool classifier %q", change.Name)
		}
	}
	return ""
}
