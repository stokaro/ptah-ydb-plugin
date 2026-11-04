//go:build integration

package integration_test

import (
	"context"
	"fmt"
	"net"
	"net/url"
	"os"
	"path"
	"slices"
	"testing"
	"time"

	qt "github.com/frankban/quicktest"
	"github.com/stokaro/ptah-ydb-plugin/internal/connection"
	"github.com/stokaro/ptah-ydb-plugin/internal/replication"
	"github.com/stokaro/ptah-ydb-plugin/internal/replicationcompare"
	"github.com/stokaro/ptah-ydb-plugin/internal/replicationplan"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcomparison"
	"github.com/stokaro/ptah-ydb-plugin/internal/testplanning"
	"github.com/stokaro/ptah-ydb-plugin/schema"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/stokaro/ptah/provider-sdk/provider"
	providerruntime "github.com/stokaro/ptah/provider-sdk/runtime"
	ydb "github.com/ydb-platform/ydb-go-sdk/v3"
	"github.com/ydb-platform/ydb-go-sdk/v3/scheme"
	"github.com/ydb-platform/ydb-go-sdk/v3/table"
	"google.golang.org/protobuf/proto"
)

// selfConnection is the connection string through which the server's own
// nodes reach its database: the address discovery advertises, which a
// replication of the database's own tables reads through. local-ydb
// advertises the port it listens on inside its container, so the string
// holds even where the test reaches the server through another port.
func selfConnection(c *qt.C, opened *connection.Connection) string {
	c.Helper()
	ctx, cancel := context.WithTimeout(c.Context(), 30*time.Second)
	defer cancel()
	endpoints, err := opened.Driver.Discovery().Discover(ctx)
	c.Assert(err, qt.IsNil)
	c.Assert(endpoints, qt.Not(qt.HasLen), 0)
	return replication.Endpoint{Address: endpoints[0].Address(), Database: opened.Database()}.String()
}

// alternateSelfConnection addresses the same single-node server from inside
// its container through its other loopback spelling: a connection string
// that differs and still reaches the database.
func alternateSelfConnection(c *qt.C, connection string) string {
	c.Helper()
	address, err := url.Parse(connection)
	c.Assert(err, qt.IsNil)
	host := "127.0.0.1"
	if address.Hostname() == host {
		host = "localhost"
	}
	address.Host = net.JoinHostPort(host, address.Port())
	return address.String()
}

// replicationFixture is a provider session over a fresh directory, and the
// helpers that drive one object of the family through the provider's
// services the way the host drives it.
type replicationFixture struct {
	c         *qt.C
	ctx       context.Context
	opened    *connection.Connection
	client    *providerruntime.Client
	session   *protocol.Session
	directory string
	database  string
}

func newReplicationFixture(c *qt.C) *replicationFixture {
	c.Helper()
	opened, directory, database := inspectionFixture(c)
	ctx, cancel := context.WithTimeout(c.Context(), 8*time.Minute)
	c.Cleanup(cancel)
	binary, digest := providerBinary(c)
	client := startProvider(c, binary, digest)
	session, err := client.Open(ctx, &protocol.OpenRequest{Engine: "ydb", Address: os.Getenv("PTAH_YDB_TEST_URL")})
	c.Assert(err, qt.IsNil)
	c.Cleanup(func() { c.Check(client.CloseSession(context.Background(), session.Id), qt.IsNil) })
	return &replicationFixture{c: c, ctx: ctx, opened: opened, client: client, session: session, directory: directory, database: database}
}

// absolute is the absolute path of a path under the fixture's directory.
func (f *replicationFixture) absolute(rest string) string {
	return path.Join(f.database, f.directory, rest)
}

// exec runs one statement, waiting out a schema operation on the path.
func (f *replicationFixture) exec(statement string) {
	f.c.Helper()
	f.c.Assert(execFixtureSchema(f.ctx, f.opened.DB, statement), qt.IsNil, qt.Commentf("%s", statement))
}

// drop runs a cleanup statement and ignores its answer: the object may be
// gone already.
func (f *replicationFixture) drop(statements ...string) {
	f.c.Cleanup(func() {
		cleanup, stop := context.WithTimeout(context.Background(), time.Minute)
		defer stop()
		for _, statement := range statements {
			_ = execFixtureSchema(cleanup, f.opened.DB, statement)
		}
	})
}

// inspect reads the fixture's directory and the directories below it named
// by rest through the provider.
func (f *replicationFixture) inspect(rest ...string) *protocol.Schema {
	f.c.Helper()
	schemas := []string{f.directory}
	for _, name := range rest {
		schemas = append(schemas, path.Join(f.directory, name))
	}
	snapshot, err := f.client.Inspect(f.ctx, &protocol.InspectRequest{SessionId: f.session.Id, Schemas: schemas})
	f.c.Assert(err, qt.IsNil)
	return snapshot
}

// object returns the snapshot's native object of kind named name, or nil.
func object(snapshot *protocol.Schema, kind, name string) *protocol.Envelope {
	for _, value := range snapshot.NativeObjects {
		if value.Kind == kind && value.Owner.Name == name {
			return value
		}
	}
	return nil
}

// await inspects until ready reports the snapshot it wants, for up to two
// minutes: YDB's replication controller creates replica tables and settles a
// state after the statement returns.
func (f *replicationFixture) await(what string, ready func(*protocol.Schema) bool, rest ...string) *protocol.Schema {
	f.c.Helper()
	deadline := time.Now().Add(2 * time.Minute)
	for {
		snapshot := f.inspect(rest...)
		if ready(snapshot) {
			return snapshot
		}
		f.c.Assert(time.Now().Before(deadline), qt.IsTrue, qt.Commentf("waiting for %s", what))
		time.Sleep(time.Second)
	}
}

// replicaWritten reports whether the fixture's directory lists name as a row
// table an async replication writes, read through the test's own connection
// rather than the provider's. The replication controller creates the replica
// after CREATE ASYNC REPLICATION returns, and the replication reports itself
// running with its item before that: measured on 26.2.1.14, a forced failover
// sent then left the replication running for the two minutes a test waits,
// and the replica, created a moment later, read-only. A SCHEME_ERROR for a listed name is a replica
// still being created, which the next call asks about again.
func (f *replicationFixture) replicaWritten(name string) bool {
	f.c.Helper()
	listing, err := f.opened.Driver.Scheme().ListDirectory(f.ctx, path.Join(f.database, f.directory))
	f.c.Assert(err, qt.IsNil)
	if !slices.ContainsFunc(listing.Children, func(entry scheme.Entry) bool { return entry.Name == name && entry.IsTable() }) {
		return false
	}
	var attributes map[string]string
	err = f.opened.Driver.Table().Do(f.ctx, func(ctx context.Context, session table.Session) error {
		described, err := session.DescribeTable(ctx, f.absolute(name))
		attributes = described.Attributes
		return err
	})
	if ydb.IsOperationErrorSchemeError(err) {
		return false
	}
	f.c.Assert(err, qt.IsNil)
	return replication.IsReplica(attributes)
}

// plan compares the declaration of kind at name, nil for none, with what the
// fixture's directory holds, and plans and renders the one change that
// follows. A refused plan returns its diagnostic's message and no unit.
func (f *replicationFixture) plan(kind, compareService, planService, name string, declared *protocol.Envelope) ([]*protocol.Unit, string) {
	f.c.Helper()
	snapshot := f.inspect()
	request := &protocol.CompareModelsRequest{Engine: "ydb", TargetFeatures: negotiated(f.client), DatabasePath: f.session.Database, ComparisonId: compareService, Kinds: []string{kind},
		Capabilities: f.session.Capabilities, Identifiers: &protocol.ComparisonIdentifiers{Rules: proto.CloneOf(f.client.Description().Engines[0].Identifiers)},
		Desired: testcomparison.State(schema.NativeDesiredModel(kind)),
		Current: &protocol.ComparisonState{Representation: protocol.ValueInput_OBSERVED, Coverage: capturedKindCoverage(snapshot.Coverage, kind)}}
	if declared != nil {
		normalized, err := f.client.NormalizeValues(f.ctx, &protocol.NormalizeValuesRequest{Engine: "ydb", Values: []*protocol.ValueInput{{Id: "declared", Representation: protocol.ValueInput_DESIRED, Value: declared}}})
		f.c.Assert(err, qt.IsNil)
		request.Desired.Records = []*protocol.ComparisonRecord{{Kind: kind, Subject: testplanning.Subject(declared.Owner), Value: normalized.Values[0]}}
	}
	for _, value := range snapshot.NativeObjects {
		if value.Kind != kind {
			continue
		}
		normalized, err := f.client.NormalizeValues(f.ctx, &protocol.NormalizeValuesRequest{Engine: "ydb", TargetFeatures: negotiated(f.client), Values: []*protocol.ValueInput{{Id: "read", Representation: protocol.ValueInput_OBSERVED, Value: value}}})
		f.c.Assert(err, qt.IsNil)
		request.Current.Records = append(request.Current.Records, &protocol.ComparisonRecord{Kind: kind, Subject: testplanning.Subject(value.Owner), Value: normalized.Values[0]})
	}
	compared, err := f.client.CompareModels(f.ctx, request)
	f.c.Assert(err, qt.IsNil)
	f.c.Assert(compared.Undecided, qt.HasLen, 0)
	if len(compared.Changes) == 0 {
		return nil, ""
	}
	f.c.Assert(compared.Changes, qt.HasLen, 1)
	planned, err := f.client.PlanFeatures(f.ctx, &protocol.PlanFeaturesRequest{Engine: "ydb", TargetFeatures: negotiated(f.client), PlanningId: planService, Capabilities: f.session.Capabilities,
		Identifiers: &protocol.ComparisonIdentifiers{Rules: proto.CloneOf(f.client.Description().Engines[0].Identifiers)}, Changes: []*protocol.PlanningChange{{Subject: compared.Changes[0].Subject, Value: compared.Changes[0].Value}}})
	f.c.Assert(err, qt.IsNil)
	if len(planned.Diagnostics) != 0 {
		return nil, planned.Diagnostics[0].Message
	}
	rendered, err := f.client.Render(f.ctx, testplanning.RenderRequest(f.client.Description(), planned, f.session.Capabilities))
	f.c.Assert(err, qt.IsNil)
	return rendered.Units, ""
}

// execute runs units and expects outcome.
func (f *replicationFixture) execute(id string, expected protocol.Execution_Outcome, units ...*protocol.Unit) {
	f.c.Helper()
	f.c.Assert(units, qt.Not(qt.HasLen), 0)
	result, err := f.client.Execute(f.ctx, &protocol.ExecuteRequest{SessionId: f.session.Id, RequestId: id, Units: units})
	if expected == protocol.Execution_APPLIED {
		f.c.Assert(err, qt.IsNil)
	} else {
		f.c.Assert(err, qt.ErrorIs, provider.ErrExecution)
	}
	f.c.Assert(result.Outcome, qt.Equals, expected, qt.Commentf("%s", result.GetDiagnosticCode()))
}

// TestReplicationLifecycleSubprocess drives an async replication of the
// server's own table through the provider's services and reads it back after
// each step: the read leaves the replica table out of the tables and keeps
// the replication, a connection change is refused while it runs and made in
// place while it is paused, and a removal drops it with CASCADE, which takes
// the replica along.
func TestReplicationLifecycleSubprocess(t *testing.T) {
	c := testcheck.New(t)
	f := newReplicationFixture(c)
	c.Assert(f.session.Capabilities[replication.CapabilityReplication], qt.IsTrue)
	f.exec(fmt.Sprintf("CREATE TABLE `%s` (id Int64 NOT NULL, note Utf8, PRIMARY KEY (id))", f.absolute("src")))
	f.drop("DROP ASYNC REPLICATION `"+f.absolute("mirror")+"` CASCADE", "DROP TABLE `"+f.absolute("rep")+"`", "DROP TABLE `"+f.absolute("src")+"`")
	connection := selfConnection(c, f.opened)
	declare := func(connectionString string) *protocol.Envelope {
		c.Helper()
		value, err := schema.CaptureReplicationIntent(&protocol.Reference{Engine: "ydb", Kind: schema.ReplicationProperties, Schema: f.directory, Name: "mirror"},
			replication.ReplicationSpec{Connection: replication.Connection{ConnectionString: connectionString},
				Items: []replication.Item{{Source: f.absolute("src"), Target: path.Join(f.directory, "rep")}}})
		c.Assert(err, qt.IsNil)
		return value
	}
	plan := func(declared *protocol.Envelope) ([]*protocol.Unit, string) {
		c.Helper()
		return f.plan(schema.ReplicationProperties, replicationcompare.ReplicationServiceID, replicationplan.ReplicationServiceID, "mirror", declared)
	}
	// The replica must exist before the read is asked to leave it out, and
	// before the replication is paused.
	running := func(state string) func(*protocol.Schema) bool {
		return func(snapshot *protocol.Schema) bool {
			value := object(snapshot, schema.ReplicationProperties, "mirror")
			if value == nil {
				return false
			}
			spec, held, err := schema.ObservedReplication(value)
			return err == nil && held == state && len(spec.Items) == 1 && f.replicaWritten("rep")
		}
	}

	units, refusal := plan(declare(connection))
	c.Assert(refusal, qt.Equals, "")
	f.execute("create", protocol.Execution_APPLIED, units...)
	snapshot := f.await("the replication running with its item", running(replication.StateRunning))
	for _, table := range snapshot.Tables {
		c.Assert(table.Identity.Name, qt.Not(qt.Equals), "rep", qt.Commentf("the replica table is the replication's"))
	}
	spec, _, err := schema.ObservedReplication(object(snapshot, schema.ReplicationProperties, "mirror"))
	c.Assert(err, qt.IsNil)
	c.Assert(spec, qt.DeepEquals, replication.ReplicationSpec{Connection: replication.Connection{ConnectionString: connection},
		Items: []replication.Item{{Source: path.Join(f.directory, "src"), Target: path.Join(f.directory, "rep")}}})
	units, refusal = plan(declare(connection))
	c.Assert(refusal, qt.Equals, "")
	c.Assert(units, qt.HasLen, 0)

	moved := alternateSelfConnection(c, connection)
	_, refusal = plan(declare(moved))
	c.Assert(refusal, qt.Matches, `async replication .*/mirror: its connection or credential differs, and YDB changes them only while the replication is paused .*it is running.*`)

	f.exec("ALTER ASYNC REPLICATION `" + f.absolute("mirror") + "` SET (STATE = 'PAUSED')")
	f.await("the replication paused", running(replication.StatePaused))
	units, refusal = plan(declare(moved))
	c.Assert(refusal, qt.Equals, "")
	f.execute("reconnect", protocol.Execution_APPLIED, units...)
	snapshot = f.inspect()
	spec, state, err := schema.ObservedReplication(object(snapshot, schema.ReplicationProperties, "mirror"))
	c.Assert(err, qt.IsNil)
	c.Assert(state, qt.Equals, replication.StatePaused)
	c.Assert(spec.Connection.ConnectionString, qt.Equals, moved)
	units, refusal = plan(declare(moved))
	c.Assert(refusal, qt.Equals, "")
	c.Assert(units, qt.HasLen, 0)

	units, refusal = plan(nil)
	c.Assert(refusal, qt.Equals, "")
	f.execute("drop", protocol.Execution_APPLIED, units...)
	listing, err := f.opened.Driver.Scheme().ListDirectory(f.ctx, path.Join(f.database, f.directory))
	c.Assert(err, qt.IsNil)
	var names []string
	for _, child := range listing.Children {
		names = append(names, child.Name)
	}
	c.Assert(names, qt.DeepEquals, []string{"src"})
}

// TestReplicationFailedOverSubprocess fails a replication over by hand and
// removes it: the drop keeps the tables, which are ordinary now and take a
// write.
func TestReplicationFailedOverSubprocess(t *testing.T) {
	c := testcheck.New(t)
	f := newReplicationFixture(c)
	f.exec(fmt.Sprintf("CREATE TABLE `%s` (id Int64 NOT NULL, note Utf8, PRIMARY KEY (id))", f.absolute("src")))
	f.drop("DROP ASYNC REPLICATION `"+f.absolute("mirror")+"` CASCADE", "DROP TABLE `"+f.absolute("rep")+"`", "DROP TABLE `"+f.absolute("src")+"`")
	connection := selfConnection(c, f.opened)
	f.exec(fmt.Sprintf("CREATE ASYNC REPLICATION `%s` FOR `%s` AS `%s` WITH (CONNECTION_STRING = '%s')", f.absolute("mirror"), f.absolute("src"), f.absolute("rep"), connection))
	f.await("the replica created", func(snapshot *protocol.Schema) bool {
		value := object(snapshot, schema.ReplicationProperties, "mirror")
		if value == nil {
			return false
		}
		spec, state, err := schema.ObservedReplication(value)
		return err == nil && state == replication.StateRunning && len(spec.Items) == 1 && f.replicaWritten("rep")
	})
	f.exec("ALTER ASYNC REPLICATION `" + f.absolute("mirror") + "` SET (STATE = 'DONE', FAILOVER_MODE = 'FORCE')")
	snapshot := f.await("the replication failed over", func(snapshot *protocol.Schema) bool {
		value := object(snapshot, schema.ReplicationProperties, "mirror")
		if value == nil {
			return false
		}
		_, state, err := schema.ObservedReplication(value)
		return err == nil && state == replication.StateDone && len(snapshot.Tables) == 2
	})
	c.Assert(snapshot.Tables[0].Identity.Name, qt.Equals, "rep")

	units, refusal := f.plan(schema.ReplicationProperties, replicationcompare.ReplicationServiceID, replicationplan.ReplicationServiceID, "mirror", nil)
	c.Assert(refusal, qt.Equals, "")
	f.execute("drop", protocol.Execution_APPLIED, units...)
	f.exec("UPSERT INTO `" + f.absolute("rep") + "` (id, note) VALUES (1, 'written'u)")
	snapshot = f.inspect()
	c.Assert(object(snapshot, schema.ReplicationProperties, "mirror"), qt.IsNil)
	c.Assert(snapshot.Tables, qt.HasLen, 2)
}

// lambdaWriting is a transfer lambda that writes each message's position and
// the text it carries, with prefix written in front of the text.
func lambdaWriting(prefix string) string {
	return fmt.Sprintf("($msg) -> { return [<| partition: $msg._partition, offset: $msg._offset, "+
		"message: '%s'u || CAST($msg._data AS Utf8) |>]; }", prefix)
}

// TestTransferLifecycleSubprocess drives a transfer from a changefeed's topic
// into a table through the provider's services: a creation that moves a row,
// no change while it runs through the consumer YDB created for it, a lambda
// changed in place, and a removal.
func TestTransferLifecycleSubprocess(t *testing.T) {
	c := testcheck.New(t)
	f := newReplicationFixture(c)
	if !f.session.Capabilities[replication.CapabilityTransfers] {
		c.Skip("the target does not support transfers")
	}
	f.exec(fmt.Sprintf("CREATE TABLE `%s` (id Int64 NOT NULL, PRIMARY KEY (id))", f.absolute("orders")))
	f.exec(fmt.Sprintf("ALTER TABLE `%s` ADD CHANGEFEED feed WITH (MODE = 'NEW_IMAGE', FORMAT = 'JSON')", f.absolute("orders")))
	f.exec(fmt.Sprintf("CREATE TABLE `%s` (partition Uint32 NOT NULL, offset Uint64 NOT NULL, message Utf8, PRIMARY KEY (partition, offset))", f.absolute("order_log")))
	// YDB 25.1.4.7 stops on a schemeshard VERIFY (`AliveChildrenCount >= 1`)
	// when the changefeed goes while DROP TRANSFER still removes the consumer
	// it created there, so the tables go once the consumer has.
	c.Cleanup(func() {
		cleanup, stop := context.WithTimeout(context.Background(), 2*time.Minute)
		defer stop()
		_ = execFixtureSchema(cleanup, f.opened.DB, "DROP TRANSFER `"+f.absolute("ingest")+"`")
		for cleanup.Err() == nil {
			description, err := f.opened.Driver.Topic().Describe(cleanup, f.absolute("orders/feed"))
			if err != nil || len(description.Consumers) == 0 {
				break
			}
			time.Sleep(time.Second)
		}
		_ = execFixtureSchema(cleanup, f.opened.DB, "DROP TABLE `"+f.absolute("order_log")+"`")
		_ = execFixtureSchema(cleanup, f.opened.DB, "DROP TABLE `"+f.absolute("orders")+"`")
	})
	declare := func(lambda string) *protocol.Envelope {
		c.Helper()
		value, err := schema.CaptureTransferIntent(&protocol.Reference{Engine: "ydb", Kind: schema.TransferProperties, Schema: f.directory, Name: "ingest"},
			replication.TransferSpec{Source: path.Join(f.directory, "orders/feed"), Target: path.Join(f.directory, "order_log"), Lambda: lambda, FlushInterval: "PT1S"})
		c.Assert(err, qt.IsNil)
		return value
	}
	plan := func(declared *protocol.Envelope) []*protocol.Unit {
		c.Helper()
		units, refusal := f.plan(schema.TransferProperties, replicationcompare.TransferServiceID, replicationplan.TransferServiceID, "ingest", declared)
		c.Assert(refusal, qt.Equals, "")
		return units
	}
	rows := func(prefix string) {
		c.Helper()
		deadline := time.Now().Add(2 * time.Minute)
		for {
			var count uint64
			c.Assert(f.opened.DB.QueryRowContext(f.ctx, fmt.Sprintf("SELECT COUNT(*) FROM `%s` WHERE StartsWith(message, '%s')", f.absolute("order_log"), prefix)).Scan(&count), qt.IsNil)
			if count > 0 {
				return
			}
			c.Assert(time.Now().Before(deadline), qt.IsTrue, qt.Commentf("waiting for a row written through %s", prefix))
			time.Sleep(time.Second)
		}
	}

	f.execute("create", protocol.Execution_APPLIED, plan(declare(lambdaWriting("a:")))...)
	// YDB names the consumer it creates once the transfer runs: 25.1.4.7
	// describes a new transfer with none at first.
	snapshot := f.await("the transfer running through its consumer", func(snapshot *protocol.Schema) bool {
		value := object(snapshot, schema.TransferProperties, "ingest")
		if value == nil {
			return false
		}
		spec, state, err := schema.ObservedTransfer(value)
		return err == nil && state == replication.StateRunning && spec.Consumer != ""
	})
	spec, _, err := schema.ObservedTransfer(object(snapshot, schema.TransferProperties, "ingest"))
	c.Assert(err, qt.IsNil)
	c.Assert(spec.Source, qt.Equals, path.Join(f.directory, "orders/feed"))
	c.Assert(spec.FlushInterval, qt.Equals, "PT1S")
	c.Assert(plan(declare(lambdaWriting("a:"))), qt.HasLen, 0)
	f.exec("UPSERT INTO `" + f.absolute("orders") + "` (id) VALUES (1)")
	rows("a:")

	f.execute("relambda", protocol.Execution_APPLIED, plan(declare(lambdaWriting("b:")))...)
	c.Assert(plan(declare(lambdaWriting("b:"))), qt.HasLen, 0)
	f.exec("UPSERT INTO `" + f.absolute("orders") + "` (id) VALUES (2)")
	rows("b:")

	f.execute("drop", protocol.Execution_APPLIED, plan(nil)...)
	c.Assert(object(f.inspect(), schema.TransferProperties, "ingest"), qt.IsNil)
	// DROP TRANSFER removes the consumer YDB created for it.
	deadline := time.Now().Add(time.Minute)
	for {
		description, err := f.opened.Driver.Topic().Describe(f.ctx, f.absolute("orders/feed"))
		c.Assert(err, qt.IsNil)
		if len(description.Consumers) == 0 {
			break
		}
		c.Assert(time.Now().Before(deadline), qt.IsTrue, qt.Commentf("waiting for the transfer's consumer to go"))
		time.Sleep(time.Second)
	}
}

// TestTransferRefusedWithoutTheCapabilitySubprocess refuses a transfer on a
// line without transfers, such as 25.1 with its default flags (`Topic
// transfer creation is disabled`), before anything is planned.
func TestTransferRefusedWithoutTheCapabilitySubprocess(t *testing.T) {
	c := testcheck.New(t)
	f := newReplicationFixture(c)
	if f.session.Capabilities[replication.CapabilityTransfers] {
		c.Skip("the target supports transfers")
	}
	declared, err := schema.CaptureTransferIntent(&protocol.Reference{Engine: "ydb", Kind: schema.TransferProperties, Schema: f.directory, Name: "ingest"},
		replication.TransferSpec{Source: path.Join(f.directory, "orders/feed"), Target: path.Join(f.directory, "order_log"), Lambda: lambdaWriting("a:")})
	c.Assert(err, qt.IsNil)
	normalized, err := f.client.NormalizeValues(f.ctx, &protocol.NormalizeValuesRequest{Engine: "ydb", Values: []*protocol.ValueInput{{Id: "declared", Representation: protocol.ValueInput_DESIRED, Value: declared}}})
	c.Assert(err, qt.IsNil)
	snapshot := f.inspect()
	request := &protocol.CompareModelsRequest{Engine: "ydb", TargetFeatures: negotiated(f.client), DatabasePath: f.session.Database, ComparisonId: replicationcompare.TransferServiceID, Kinds: []string{schema.TransferProperties},
		Capabilities: f.session.Capabilities, Identifiers: &protocol.ComparisonIdentifiers{Rules: proto.CloneOf(f.client.Description().Engines[0].Identifiers)},
		Desired: testcomparison.State(schema.NativeDesiredModel(schema.TransferProperties)),
		Current: &protocol.ComparisonState{Representation: protocol.ValueInput_OBSERVED, Coverage: capturedKindCoverage(snapshot.Coverage, schema.TransferProperties)}}
	request.Desired.Records = []*protocol.ComparisonRecord{{Kind: schema.TransferProperties, Subject: testplanning.Subject(declared.Owner), Value: normalized.Values[0]}}
	// The subprocess client reports the refusal by its status code alone;
	// TestTransferPlanning_FailurePath reads the provider's words.
	compared, err := f.client.CompareModels(f.ctx, request)
	c.Assert(err, qt.ErrorMatches, `provider compare models failed \(InvalidArgument\)`)
	c.Assert(compared, qt.IsNil)
}
