package backend_test

import (
	"context"
	"maps"
	"testing"

	qt "github.com/frankban/quicktest"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb_Coordination"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb_Scheme"
	"google.golang.org/protobuf/proto"
	protocol "ptah.run/provider-sdk/protocol/experimental"
	"ptah.run/provider-sdk/provider"

	"github.com/stokaro/ptah-ydb-plugin/internal/backend"
	"github.com/stokaro/ptah-ydb-plugin/internal/coordinationplan"
	"github.com/stokaro/ptah-ydb-plugin/internal/replication"
	"github.com/stokaro/ptah-ydb-plugin/internal/replicationcompare"
	"github.com/stokaro/ptah-ydb-plugin/internal/replicationplan"
	"github.com/stokaro/ptah-ydb-plugin/internal/reversal"
	"github.com/stokaro/ptah-ydb-plugin/internal/secretplan"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcomparison"
	"github.com/stokaro/ptah-ydb-plugin/internal/testplanning"
	"github.com/stokaro/ptah-ydb-plugin/internal/topicplan"
	"github.com/stokaro/ptah-ydb-plugin/internal/workload"
	"github.com/stokaro/ptah-ydb-plugin/internal/workloadplan"
	"github.com/stokaro/ptah-ydb-plugin/internal/ydbdecl"
	"github.com/stokaro/ptah-ydb-plugin/schema"
)

// reverseRequest asks the provider's reversal to undo changes on a target
// holding capabilities.
func reverseRequest(capabilities map[string]bool, changes ...*protocol.PlanningChange) *protocol.ReverseChangesRequest {
	return &protocol.ReverseChangesRequest{Engine: "ydb", ReversalId: reversal.ServiceID, TargetFeatures: backend.Description().Engines[0].Features,
		Identifiers: &protocol.ComparisonIdentifiers{Rules: proto.CloneOf(backend.Description().Engines[0].Identifiers)}, Capabilities: capabilities, Changes: changes}
}

// comparedChanges compares request and returns every change it plans as a
// change to reverse, each under an ID of its own across comparisons.
func comparedChanges(c *qt.C, request *protocol.CompareModelsRequest) []*protocol.PlanningChange {
	c.Helper()
	compared, err := backend.New().CompareModels(c.Context(), request)
	c.Assert(err, qt.IsNil)
	var changes []*protocol.PlanningChange
	for _, change := range compared.Changes {
		change.Value.Id = request.ComparisonId + "/" + change.Value.Id
		changes = append(changes, &protocol.PlanningChange{Subject: change.Subject, Value: change.Value})
	}
	return changes
}

// reverseText plans the reverse changes with the planner planning on a target
// holding capabilities and returns the statements a down file holds for them.
func reverseText(c *qt.C, planning string, capabilities map[string]bool, reversed ...*protocol.ReversedChange) []string {
	c.Helper()
	capabilities = maps.Clone(capabilities)
	capabilities["transactional_ddl"] = false
	request := &protocol.PlanFeaturesRequest{Engine: "ydb", PlanningId: planning, DatabasePath: "/local", TargetFeatures: []string{provider.PlanningLifecycleFeature},
		Identifiers: &protocol.ComparisonIdentifiers{Rules: proto.CloneOf(backend.Description().Engines[0].Identifiers)}, Capabilities: capabilities}
	for _, change := range reversed {
		request.Changes = append(request.Changes, &protocol.PlanningChange{Subject: change.Subject, Value: change.Value})
	}
	plan, err := backend.New().PlanFeatures(c.Context(), request)
	c.Assert(err, qt.IsNil)
	c.Assert(plan.Diagnostics, qt.HasLen, 0)
	rendering := testplanning.RenderRequest(backend.Description(), plan, capabilities)
	rendering.Form = protocol.RenderRequest_TEXT
	rendered, err := backend.New().Render(c.Context(), rendering)
	c.Assert(err, qt.IsNil)
	c.Assert(rendered.Refusal, qt.IsNil)
	var statements []string
	for _, unit := range rendered.Units {
		statements = append(statements, unit.GetSql())
	}
	return statements
}

// Ported from the host's TestGenerateDownMigration_CoordinationNodes: the
// rollback of a change gives the node back the settings the change moved, at
// the values it ran with, a setting it ran with at YDB's default included.
func TestReverseChanges_RestoresACoordinationSettingAtItsDefault(t *testing.T) {
	c := testcheck.New(t)
	held := &Ydb_Coordination.DescribeNodeResult{Config: &Ydb_Coordination.Config{SelfCheckPeriodMillis: 2000}, Self: &Ydb_Scheme.Entry{Owner: "original-owner"}}
	request, err := testcomparison.Request(&schema.CoordinationIntent{SelfCheckPeriodMillis: new(uint32(3000)), ReadConsistencyMode: new("strict")}, held)
	c.Assert(err, qt.IsNil)

	_, reverse, reversed := reverseOf(c, request)

	c.Assert(reverseText(c, coordinationplan.ServiceID, reverse.Capabilities, reversed), qt.DeepEquals,
		[]string{"ALTER COORDINATION NODE `app/node` SET (self_check_period = Interval('PT2S'), read_consistency_mode = 'relaxed');"})
}

// Ported from the host's
// TestCoordinationReversalProjectsStoredSettingsAndReportsRuntimeLoss: a
// declaration that resets the one setting it changed leaves the node at YDB's
// default for it and the setting it kept as it was, and restoring stored
// settings loses nothing at run time.
func TestReverseChanges_PredictsTheSettingsAnAlterationLeaves(t *testing.T) {
	c := testcheck.New(t)
	held := &Ydb_Coordination.DescribeNodeResult{Config: &Ydb_Coordination.Config{ReadConsistencyMode: Ydb_Coordination.ConsistencyMode_CONSISTENCY_MODE_STRICT, SelfCheckPeriodMillis: 2500},
		Self: &Ydb_Scheme.Entry{Owner: "original-owner"}}
	request, err := testcomparison.Request(&schema.CoordinationIntent{SelfCheckPeriodMillis: new(uint32(2500))}, held)
	c.Assert(err, qt.IsNil)

	_, _, reversed := reverseOf(c, request)

	c.Assert(reversed.Strategy, qt.Equals, "restore coordination settings in place")
	c.Assert(reversed.Limitations, qt.HasLen, 0)
	left, err := schema.EffectiveCoordination(must(schema.CoordinationConfiguration(reversed.ForwardState[0].Value.Value, protocol.ValueInput_OBSERVED)))
	c.Assert(err, qt.IsNil)
	c.Assert(left.ReadMode, qt.Equals, "relaxed")
	c.Assert(left.SelfCheckMillis, qt.Equals, uint32(2500))
}

// forged is change under the name name, its value holding payload: a change
// the provider never returned, which reaches the reversal only because the
// host's checks read none of its operands.
func forged(change *protocol.PlanningChange, name, payload string) *protocol.PlanningChange {
	result := proto.CloneOf(change)
	result.Subject.Name = testcomparison.Part(name)
	result.Value.Id = name
	result.Value.Value.Owner.Name = name
	result.Value.NormalizedOwner.Name = name
	result.Value.Value.Payload = []byte(payload)
	return result
}

// Ported from the host's TestCoordinationReversalRejectsAnInvalidSuffixAtomically
// and TestWorkloadReversalRejectsInvalidBatchesWithoutAPrefix: a batch whose
// later change cannot be reversed is refused whole, with no reverse of the
// valid change before it.
func TestReverseChanges_RefusesAnInvalidSuffixWithoutAPrefix(t *testing.T) {
	c := testcheck.New(t)
	node, err := testcomparison.Request(&schema.CoordinationIntent{}, nil)
	c.Assert(err, qt.IsNil)
	pools := workloadRequest(c, schema.ResourcePoolProperties, map[string]proto.Message{"valid": pool(workload.PoolSpec{})}, nil)
	for _, test := range []struct {
		name    string
		request *protocol.CompareModelsRequest
	}{
		{name: "a coordination node", request: node},
		{name: "a resource pool", request: pools},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			changes := comparedChanges(c, proto.CloneOf(test.request))
			c.Assert(changes, qt.HasLen, 1)

			reversed, err := backend.New().ReverseChanges(c.Context(), reverseRequest(test.request.Capabilities, changes[0], forged(changes[0], "invalid", `{}`)))

			c.Assert(err, qt.ErrorMatches, `.*the change of (app/)?invalid cannot be reversed.*`)
			c.Assert(reversed, qt.IsNil)
		})
	}
}

// Ported from the host's TestReversalValidatesContextTargetAndCapabilities and
// TestReverseChanges_FailurePath: a caller that has given up gets no answer.
// The change is a drop, whose reverse predicts nothing, so no normalization
// of a prediction notices the canceled context for the reversal.
func TestReverseChanges_RefusesACanceledContext(t *testing.T) {
	c := testcheck.New(t)
	request, err := testcomparison.Request(nil, coordinationObserved())
	c.Assert(err, qt.IsNil)
	changes := comparedChanges(c, request)
	ctx, cancel := context.WithCancel(c.Context())
	cancel()

	reversed, err := backend.New().ReverseChanges(ctx, reverseRequest(request.Capabilities, changes...))

	c.Assert(err, qt.ErrorMatches, `.*context canceled`)
	c.Assert(reversed, qt.IsNil)
}

// Ported from the host's TestSecretReversal_HappyPath and
// TestSecrets_RollbackRestoresWhatItCan: a dropped secret is created again
// with the value of the default variable for its path, since the database
// never returned the old value and a read names no variable, and the reverse
// says so.
func TestReverseChanges_CreatesADroppedSecretAgainWithItsDefaultVariable(t *testing.T) {
	c := testcheck.New(t)
	request := secretRequest(c, secretPresent)
	request.Desired.Records = nil

	_, reverse, reversed := reverseOf(c, request)

	c.Assert(reversed.Strategy, qt.Equals, "create the dropped secret again")
	c.Assert(reversed.Limitations, qt.DeepEquals, []string{"the dropped value of secret app/pg.pw was never read; the rollback takes the value PTAH_SECRET_APP_PG_PW holds when it runs"})
	c.Assert(reversed.ForwardState[0].Value, qt.IsNil)
	c.Assert(reverseText(c, secretplan.ServiceID, reverse.Capabilities, reversed), qt.DeepEquals, []string{"CREATE SECRET `app/pg.pw` WITH (value = $PTAH_SECRET_APP_PG_PW);"})
}

// Ported from the host's TestTopicReversal_HappyPath and
// TestTopics_RollbackRestoresWhatItCan: the rollback of a change moves the
// topic back to the nearest state YDB reaches in place. It keeps the
// partitions the change added, since YDB never removes one, and the
// auto-partitioning the change turned on, paused rather than disabled, adds
// back the consumer the change dropped, and says what it cannot restore.
func TestReverseChanges_KeepsThePartitionsATopicChangeAdded(t *testing.T) {
	worker := []ydbdecl.Consumer{{Name: "worker"}}
	for _, test := range []struct {
		name        string
		declared    ydbdecl.Topic
		limitations []string
		want        []string
	}{
		{name: "raised partitions and a dropped consumer", declared: ydbdecl.Topic{RetentionPeriod: "PT24H", MinActivePartitions: 3},
			limitations: []string{"topic app/events keeps the 3 partitions the change gave it, since YDB never removes a partition",
				`consumer "worker" of topic app/events was dropped; the rollback adds it again without its position`},
			want: []string{"ALTER TOPIC `app/events` ADD CONSUMER `worker`;"}},
		{name: "auto-partitioning turned on", declared: ydbdecl.Topic{RetentionPeriod: "PT24H", MinActivePartitions: 3, MaxActivePartitions: 6, AutoPartitioningStrategy: "scale_up",
			Consumers: worker},
			limitations: []string{"topic app/events keeps the 3 partitions the change gave it, since YDB never removes a partition",
				"topic app/events keeps auto-partitioning, paused, since YDB does not disable it once it is on"},
			want: []string{"ALTER TOPIC `app/events` SET (min_active_partitions = 3, auto_partitioning_strategy = 'paused', max_active_partitions = 6, " +
				"auto_partitioning_up_utilization_percent = 90, auto_partitioning_down_utilization_percent = 30, auto_partitioning_stabilization_window = Interval('PT5M'), " +
				"retention_period = Interval('P1D'), partition_write_speed_bytes_per_second = 1048576, partition_write_burst_bytes = 1048576, supported_codecs = '');"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			request := topicRequest(c, &test.declared, readTopic())

			_, reverse, reversed := reverseOf(c, request)

			c.Assert(reversed.Strategy, qt.Equals, "restore the topic's settings and consumers in place")
			c.Assert(reversed.Limitations, qt.DeepEquals, test.limitations)
			capabilities := maps.Clone(reverse.Capabilities)
			capabilities["topic_consumer_availability_period"] = true
			c.Assert(reverseText(c, topicplan.ServiceID, capabilities, reversed), qt.DeepEquals, test.want)
		})
	}
}

// Ported from the host's TestTopicReversal_HappyPath: a change that only
// raised a topic's partitions has no reverse YDB can run, so the reverse runs
// nothing and says the partitions stay.
func TestReverseChanges_RunsNothingForRaisedPartitionsAlone(t *testing.T) {
	c := testcheck.New(t)
	request := topicRequest(c, &ydbdecl.Topic{RetentionPeriod: "PT24H", MinActivePartitions: 3, Consumers: []ydbdecl.Consumer{{Name: "worker"}}}, readTopic())

	_, _, reversed := reverseOf(c, request)

	c.Assert(reversed.Value, qt.IsNil)
	c.Assert(reversed.Limitations, qt.DeepEquals, []string{"topic app/events keeps the 3 partitions the change gave it, since YDB never removes a partition"})
	c.Assert(reversed.ForwardState[0].Value, qt.IsNotNil)
}

// Ported from the host's TestResourcePoolsBidirectionalPlanUsesOwnedChanges:
// the rollback of a limit reset and a rank swap drops both classifiers, gives
// the pool the limits it held and creates the classifiers again at their old
// ranks; the rollback of a creation drops the classifier before its pool; and
// the default pool gets back an explicit zero. Every reverse says it cannot
// undo the queries the forward settings ran, queued or rejected.
func TestReverseChanges_RestoresWorkloadSettings(t *testing.T) {
	held := workload.PoolSpec{ConcurrentQueryLimit: new(int32(5)), QueueSize: new(int32(9))}
	for _, test := range []struct {
		name                                           string
		pools, readPools, classifiers, readClassifiers map[string]proto.Message
		want                                           []string
	}{
		{name: "a creation", pools: map[string]proto.Message{"batch": pool(held)}, classifiers: map[string]proto.Message{"alpha": routing("batch", 10)},
			want: []string{"DROP RESOURCE POOL CLASSIFIER `alpha`;", "DROP RESOURCE POOL `batch`;"}},
		{name: "a limit reset and a rank swap",
			pools: map[string]proto.Message{"batch": pool(workload.PoolSpec{ConcurrentQueryLimit: new(int32(0))})}, readPools: map[string]proto.Message{"batch": pool(held)},
			classifiers:     map[string]proto.Message{"alpha": routing("batch", 20), "beta": routing("batch", 10)},
			readClassifiers: map[string]proto.Message{"alpha": routing("batch", 10), "beta": routing("batch", 20)},
			want: []string{"DROP RESOURCE POOL CLASSIFIER `alpha`;", "DROP RESOURCE POOL CLASSIFIER `beta`;", "ALTER RESOURCE POOL `batch` SET (CONCURRENT_QUERY_LIMIT = 5, QUEUE_SIZE = 9);",
				"CREATE RESOURCE POOL CLASSIFIER `alpha` WITH (RESOURCE_POOL = 'batch', RANK = 10);", "CREATE RESOURCE POOL CLASSIFIER `beta` WITH (RESOURCE_POOL = 'batch', RANK = 20);"}},
		// Ported from the host's TestPoolReversalPreservesResetAndZeroWithoutSharingPointers.
		{name: "explicit zeros and a reset setting", pools: map[string]proto.Message{"batch": pool(workload.PoolSpec{ResourceWeight: new(12.5)})},
			readPools: map[string]proto.Message{"batch": pool(workload.PoolSpec{ConcurrentQueryLimit: new(int32(0)), QueueSize: new(int32(0))})},
			want:      []string{"ALTER RESOURCE POOL `batch` SET (CONCURRENT_QUERY_LIMIT = 0, QUEUE_SIZE = 0), RESET (RESOURCE_WEIGHT);"}},
		{name: "the default pool's explicit zero", pools: map[string]proto.Message{"default": pool(workload.PoolSpec{})},
			readPools: map[string]proto.Message{"default": pool(workload.PoolSpec{ResourceWeight: new(0.0)})},
			want:      []string{"ALTER RESOURCE POOL `default` SET (RESOURCE_WEIGHT = 0);"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			changes := append(comparedChanges(c, workloadRequest(c, schema.ResourcePoolProperties, test.pools, test.readPools)),
				comparedChanges(c, workloadRequest(c, schema.ResourcePoolClassifierProperties, test.classifiers, test.readClassifiers))...)

			reversed, err := backend.New().ReverseChanges(c.Context(), reverseRequest(workloadCapabilities(), changes...))

			c.Assert(err, qt.IsNil)
			for _, change := range reversed.Changes {
				c.Assert(change.Limitations, qt.DeepEquals, []string{"Restoring workload configuration cannot undo queries run, queued, or rejected under the forward settings."})
			}
			c.Assert(reverseText(c, workloadplan.ServiceID, workloadCapabilities(), reversed.Changes...), qt.DeepEquals, test.want)
		})
	}
}

// replicationReversal compares a replication declared and read as given and
// reverses its one change.
func replicationReversal(c *qt.C, declared, read *replication.ReplicationSpec, state string) (*protocol.ReverseChangesRequest, *protocol.ReversedChange) {
	c.Helper()
	request := objectRequest(c, schema.ReplicationProperties, replicationcompare.ReplicationServiceID, "mirror", declaredReplication(c, declared), readReplication(c, read, state))
	_, reverse, reversed := reverseOf(c, request)
	return reverse, reversed
}

// Ported from the host's TestReplicationReversal_HappyPath: a replication
// dropped with CASCADE is created again, which copies its source from the
// start, and one paused when it was dropped runs once it is; a failed-over
// replication the forward plan dropped is not created again, since its tables
// stayed as ordinary tables.
func TestReverseChanges_UndoesAReplicationsDrop(t *testing.T) {
	for _, test := range []struct {
		name        string
		state       string
		strategy    string
		limitations []string
		value       bool
	}{
		{name: "a running replication", state: replication.StateRunning, strategy: "create the dropped replication again with its connection and items", value: true,
			limitations: []string{"the replica tables async replication app/mirror dropped are created again empty, and the replication copies its source again from the start"}},
		{name: "a paused replication", state: replication.StatePaused, strategy: "create the dropped replication again with its connection and items", value: true,
			limitations: []string{"the replica tables async replication app/mirror dropped are created again empty, and the replication copies its source again from the start",
				"async replication app/mirror was paused when it was dropped, and runs once it is created again"}},
		{name: "a failed-over replication", state: replication.StateDone, strategy: "leave the failed-over replication dropped",
			limitations: []string{"async replication app/mirror was failed over before it was dropped, so it is not created again: it replicated nothing more, and its tables stayed as ordinary tables"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)

			_, reversed := replicationReversal(c, nil, new(mirror()), test.state)

			c.Assert(reversed.Strategy, qt.Equals, test.strategy)
			c.Assert(reversed.Limitations, qt.DeepEquals, test.limitations)
			c.Assert(reversed.Value != nil, qt.Equals, test.value)
		})
	}
}

// Ported from the host's TestReplicationReversal_HappyPath: a paused
// replication whose connection the forward change moved is moved back while
// paused, the state the forward change ran in, so the reverse is an ALTER YDB
// takes.
func TestReverseChanges_MovesAPausedReplicationBack(t *testing.T) {
	c := testcheck.New(t)
	moved := mirror()
	moved.Connection.ConnectionString = "grpc://standby:2136/?database=/local"

	reverse, reversed := replicationReversal(c, &moved, new(mirror()), replication.StatePaused)

	c.Assert(reversed.Strategy, qt.Equals, "restore the replication's connection and credential in place")
	c.Assert(reversed.Limitations, qt.HasLen, 0)
	_, state, err := schema.ObservedReplication(reversed.ForwardState[0].Value.Value)
	c.Assert(err, qt.IsNil)
	c.Assert(state, qt.Equals, replication.StatePaused)
	c.Assert(reverseText(c, replicationplan.ReplicationServiceID, reverse.Capabilities, reversed), qt.DeepEquals,
		[]string{"ALTER ASYNC REPLICATION `app/mirror` SET (CONNECTION_STRING = 'grpc://localhost:2136/?database=/local');"})
}

// Ported from the host's TestReplicationReversal_HappyPath: a credential the
// forward change gave a replication stays, since YDB has no statement that
// takes a credential away, so the reverse runs nothing and says so. What the
// change leaves is the replication with the credential, in the state the
// change ran in.
func TestReverseChanges_KeepsACredentialTheChangeAdded(t *testing.T) {
	c := testcheck.New(t)
	bare := mirror()
	bare.Connection = replication.Connection{ConnectionString: bare.Connection.ConnectionString}

	_, reversed := replicationReversal(c, new(mirror()), &bare, replication.StatePaused)

	c.Assert(reversed.Strategy, qt.Equals, "restore the replication's connection and credential in place")
	c.Assert(reversed.Limitations, qt.DeepEquals, []string{"the credential the change gave async replication app/mirror stays, since YDB has no statement that takes a credential away"})
	c.Assert(reversed.Value, qt.IsNil)
	left, state, err := schema.ObservedReplication(reversed.ForwardState[0].Value.Value)
	c.Assert(err, qt.IsNil)
	c.Assert(left.Connection.TokenSecretName, qt.Equals, "token")
	c.Assert(state, qt.Equals, replication.StatePaused)
}

// Ported from the host's TestReplicationReversal_HappyPath: the rows a
// transfer wrote stay whatever its reverse does, the consumer YDB created for
// a transfer that names none goes with it, and a transfer created again does
// not resume from the consumer's position. A lambda changed back keeps the
// rows the changed one wrote, and what the change leaves runs as the
// transfer did.
func TestReverseChanges_UndoesATransferChange(t *testing.T) {
	own := ingest()
	own.Consumer = "reader"
	relambda := ingest()
	relambda.Lambda = "($m) -> { return []; }"
	for _, test := range []struct {
		name           string
		declared, read *replication.TransferSpec
		limitations    []string
	}{
		{name: "a created transfer", declared: new(ingest()), limitations: []string{"the rows transfer app/ingest wrote into table order_log stay",
			"dropping transfer app/ingest drops the consumer YDB created for it, with its position in topic orders/feed"}},
		{name: "a created transfer with its own consumer", declared: &own, limitations: []string{"the rows transfer app/ingest wrote into table order_log stay"}},
		{name: "a dropped transfer", read: new(ingest()), limitations: []string{"dropping transfer app/ingest removed the consumer YDB created for it, if it had one, " +
			"with its position in topic orders/feed; the transfer created again does not resume from it"}},
		{name: "a new lambda", declared: &relambda, read: new(ingest()), limitations: []string{"the rows transfer app/ingest wrote with the changed lambda stay as written"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			request := objectRequest(c, schema.TransferProperties, replicationcompare.TransferServiceID, "ingest", declaredTransfer(c, test.declared),
				readTransfer(c, test.read, replication.StateRunning))

			_, _, reversed := reverseOf(c, request)

			c.Assert(reversed.Limitations, qt.DeepEquals, test.limitations)
		})
	}
}

// Ported from the host's TestReplicationReversal_HappyPath: a transfer whose
// lambda the forward change replaced runs on, so what the change leaves is a
// running transfer and the reverse is the ALTER that puts the old lambda back.
func TestReverseChanges_PutsATransfersLambdaBack(t *testing.T) {
	c := testcheck.New(t)
	relambda := ingest()
	relambda.Lambda = "($m) -> { return []; }"
	request := objectRequest(c, schema.TransferProperties, replicationcompare.TransferServiceID, "ingest", declaredTransfer(c, &relambda),
		readTransfer(c, new(ingest()), replication.StateRunning))

	_, reverse, reversed := reverseOf(c, request)

	_, state, err := schema.ObservedTransfer(reversed.ForwardState[0].Value.Value)
	c.Assert(err, qt.IsNil)
	c.Assert(state, qt.Equals, replication.StateRunning)
	c.Assert(reverseText(c, replicationplan.TransferServiceID, reverse.Capabilities, reversed), qt.HasLen, 1)
}

// Ported from the host's TestExternalReversal_HappyPath: the reverse of an
// external object's change swaps its operands. A creation is dropped, a drop
// is created again as it was, a replacement is replaced back, and a table
// recreated over a recreated data source is recreated back with it; nothing
// is lost that the reverse cannot restore.
func TestReverseChanges_UndoesAnExternalObjectChange(t *testing.T) {
	bucket, moved := external{name: "bucket", source: bucketAt("https://a/")}, external{name: "bucket", source: bucketAt("https://b/")}
	events := external{name: "events", table: eventsOver("app/bucket")}
	for _, test := range []struct {
		name           string
		declared, read []external
		capabilities   map[string]bool
		strategies     []string
	}{
		{name: "a created data source", declared: []external{bucket}, capabilities: withSources(), strategies: []string{"drop the created data source"}},
		{name: "a dropped data source", read: []external{bucket}, capabilities: withSources(), strategies: []string{"create the dropped data source again with the definition it had"}},
		{name: "a replaced data source", declared: []external{moved}, read: []external{bucket}, capabilities: withReplace(),
			strategies: []string{"replace the data source with the definition it had"}},
		{name: "a table recreated over a recreated data source", declared: []external{moved, events}, read: []external{bucket, events}, capabilities: withSources(),
			strategies: []string{"replace the data source with the definition it had", "replace the external table with the definition it had"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			request := reverseRequest(test.capabilities, comparedChanges(c, externalRequest(c, test.declared, test.read, test.capabilities))...)

			reversed, err := backend.New().ReverseChanges(c.Context(), request)

			c.Assert(err, qt.IsNil)
			c.Assert(provider.ValidateReversedChanges(backend.Description(), request, reversed), qt.IsNil)
			var strategies []string
			for _, change := range reversed.Changes {
				strategies = append(strategies, change.Strategy)
				c.Assert(change.Limitations, qt.HasLen, 0)
				c.Assert(change.Value, qt.IsNotNil)
			}
			c.Assert(strategies, qt.DeepEquals, test.strategies)
		})
	}
}

// renamedKind is change with its subject named as an object of kind, or
// change itself for no kind.
func renamedKind(change *protocol.PlanningChange, kind string) *protocol.PlanningChange {
	if kind == "" {
		return change
	}
	result := proto.CloneOf(change)
	result.Subject.Kind = kind
	return result
}

// withPayload is change with its value holding payload, or change itself for
// no payload.
func withPayload(change *protocol.PlanningChange, payload string) *protocol.PlanningChange {
	if payload == "" {
		return change
	}
	result := proto.CloneOf(change)
	result.Value.Value.Payload = []byte(payload)
	return result
}

// Ported from the host's TestSecretReversal_FailurePath,
// TestExternalReversal_FailurePath, TestTopicReversal_FailurePath and
// TestReplicationReversal_FailurePath: a reverse is refused, with no partial
// result, on a target without the capability its forward change needed, for
// a change whose subject is an object of another kind, and for a change that
// holds no operand.
func TestReverseChanges_RefusesWhatItCannotReverse(t *testing.T) {
	c := testcheck.New(t)
	secret := secretRequest(c, secretAbsent)
	source := externalRequest(c, []external{{name: "bucket", source: bucketAt("https://a/")}}, nil, withSources())
	topic := topicRequest(c, &ydbdecl.Topic{}, nil)
	replica := objectRequest(c, schema.ReplicationProperties, replicationcompare.ReplicationServiceID, "mirror", nil, readReplication(c, new(mirror()), replication.StateRunning))
	for _, test := range []struct {
		name         string
		request      *protocol.CompareModelsRequest
		capabilities map[string]bool
		kind         string
		payload      string
		want         string
	}{
		{name: "a secret on a line without secrets", request: secret, capabilities: map[string]bool{"secrets": false},
			want: `.*the change of app/pg.pw cannot be reversed: reversing it requires target capability secrets, unavailable on this ydb target`},
		{name: "an external object on a target without external data sources", request: source, capabilities: map[string]bool{"external_data_sources": false},
			want: `.*the change of app/bucket cannot be reversed: reversing it requires target capability external_data_sources, unavailable on this ydb target`},
		{name: "a data source change on a table", request: source, kind: schema.ExternalTableProperties, want: `.*an object state names another kind than its subject`},
		{name: "a replication change on a transfer", request: replica, kind: schema.TransferProperties, want: `.*an object state names another kind than its subject`},
		{name: "an empty secret change", request: secret, payload: `{}`, want: `.*the change of app/pg.pw cannot be reversed.*`},
		{name: "an empty external change", request: source, payload: `{}`, want: `.*the change of app/bucket cannot be reversed.*`},
		{name: "an empty topic change", request: topic, payload: `{}`, want: `.*the change of app/events cannot be reversed.*`},
		{name: "an empty replication change", request: replica, payload: `{}`, want: `.*the change of app/mirror cannot be reversed.*`},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			changes := comparedChanges(c, proto.CloneOf(test.request))
			c.Assert(changes, qt.HasLen, 1)
			change := withPayload(renamedKind(changes[0], test.kind), test.payload)
			capabilities := maps.Clone(test.request.Capabilities)
			maps.Copy(capabilities, test.capabilities)

			reversed, err := backend.New().ReverseChanges(c.Context(), reverseRequest(capabilities, change))

			c.Assert(err, qt.ErrorMatches, test.want)
			c.Assert(reversed, qt.IsNil)
		})
	}
}
