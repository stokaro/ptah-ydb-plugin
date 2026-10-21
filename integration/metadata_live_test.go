//go:build integration

package integration_test

import (
	"context"
	"crypto/rand"
	"database/sql"
	"fmt"
	"path"
	"strings"
	"testing"

	qt "github.com/frankban/quicktest"
	"github.com/stokaro/ptah-ydb-plugin/internal/connection"
	"github.com/stokaro/ptah-ydb-plugin/internal/metadata"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/stokaro/ptah/provider-sdk/provider"
	providerruntime "github.com/stokaro/ptah/provider-sdk/runtime"
	"google.golang.org/protobuf/proto"
)

func prepareMetadataStore(c *qt.C, client *providerruntime.Client, opened *connection.Connection, database string, scope *protocol.MetadataScope) {
	c.Helper()
	result, err := client.PrepareMetadata(c.Context(), &protocol.PrepareMetadataRequest{Scope: scope, RequestId: "prepare"})
	c.Assert(err, qt.IsNil)
	c.Assert(result.Outcome, qt.Equals, protocol.MetadataResult_APPLIED)
	c.Cleanup(func() {
		_, err := opened.DB.ExecContext(context.Background(), "DROP TABLE `"+path.Join(database, scope.Schema, scope.Table)+"`")
		c.Check(err, qt.IsNil)
	})
}
func logRecord(sequence int64) *protocol.MigrationLogRecord {
	return &protocol.MigrationLogRecord{RunId: "run π", Sequence: sequence, Operation: "down", Version: -9223372036854775808, State: "failed", Actor: "actor\x00π", ActorSource: "provided", Checksum: "checksum\x00", Error: "exact\nerror\x00", AtSeconds: -62135596800, AtNanos: 999999999}
}
func tagRecord(name string, version int64) *protocol.MigrationTagRecord {
	return &protocol.MigrationTagRecord{Name: name, Version: version, RecordedAtSeconds: 253402300799, RecordedAtNanos: 999999999}
}
func metadataMutation(c *qt.C, client *providerruntime.Client, request *protocol.MutateMetadataRequest, want protocol.MetadataResult_Outcome) *protocol.MetadataResult {
	c.Helper()
	request.RequestId = "mutation"
	result, err := client.MutateMetadata(c.Context(), request)
	switch want {
	case protocol.MetadataResult_APPLIED:
		c.Assert(err, qt.IsNil)
	case protocol.MetadataResult_UNKNOWN:
		c.Assert(err, qt.ErrorIs, provider.ErrOutcomeUnknown)
	default:
		c.Assert(err, qt.ErrorIs, provider.ErrExecution)
	}
	c.Assert(result, qt.IsNotNil)
	c.Assert(result.Outcome, qt.Equals, want)
	return result
}
func putTag(c *qt.C, client *providerruntime.Client, scope *protocol.MetadataScope, row *protocol.MigrationTagRecord) {
	c.Helper()
	metadataMutation(c, client, &protocol.MutateMetadataRequest{Scope: scope, Mutation: &protocol.MutateMetadataRequest_PutTag{PutTag: row}}, protocol.MetadataResult_APPLIED)
}
func lookupTag(c *qt.C, client *providerruntime.Client, scope *protocol.MetadataScope, name string) *protocol.MetadataTagResult {
	c.Helper()
	result, err := client.LookupMetadataTag(c.Context(), &protocol.LookupMetadataTagRequest{Scope: scope, RequestId: "lookup", Name: name})
	c.Assert(err, qt.IsNil)
	return result
}
func openMetadata(c *qt.C, client *providerruntime.Client, scope *protocol.MetadataScope) *protocol.MetadataReadRequest {
	c.Helper()
	request := &protocol.MetadataReadRequest{Scope: proto.CloneOf(scope), RequestId: "open", ReaderId: rand.Text()}
	_, err := client.OpenMetadataRead(c.Context(), request)
	c.Assert(err, qt.IsNil)
	c.Cleanup(func() { _, err := client.CloseMetadataRead(context.Background(), request); c.Check(err, qt.IsNil) })
	return request
}
func drainMetadata(c *qt.C, client *providerruntime.Client, reader *protocol.MetadataReadRequest, limit uint32) (records []proto.Message, exists bool, pages int) {
	c.Helper()
	for index := uint64(0); ; index++ {
		page, err := client.ReadMetadataPage(c.Context(), &protocol.ReadMetadataPageRequest{Reader: reader, Page: index, MaxRecords: limit})
		c.Assert(err, qt.IsNil)
		if index > 0 {
			c.Assert(page.Exists, qt.Equals, exists)
		}
		exists = page.Exists
		pages++
		for _, row := range page.Logs {
			records = append(records, row)
		}
		for _, row := range page.Tags {
			records = append(records, row)
		}
		if page.Complete {
			break
		}
	}
	_, err := client.CloseMetadataRead(c.Context(), reader)
	c.Assert(err, qt.IsNil)
	return records, exists, pages
}

func TestNativeMetadataDurabilityTransactionsAndSnapshots(t *testing.T) {
	c := qt.New(t)
	opened, directory, database := inspectionFixture(c)
	binary, digest := providerBinary(c)
	client, id, _ := historySession(c, binary, digest)
	logs := &protocol.MetadataScope{SessionId: id, Schema: directory, Table: "events", Kind: protocol.MetadataScope_LOG}
	tags := &protocol.MetadataScope{SessionId: id, Schema: directory, Table: "tags", Kind: protocol.MetadataScope_TAGS}
	missing := openMetadata(c, client, logs)
	records, exists, _ := drainMetadata(c, client, missing, 1)
	c.Assert(exists, qt.IsFalse)
	c.Assert(records, qt.HasLen, 0)
	missingTag := lookupTag(c, client, tags, "release")
	c.Assert(missingTag.Found, qt.IsFalse)
	prepareMetadataStore(c, client, opened, database, logs)
	prepareMetadataStore(c, client, opened, database, tags)
	empty := openMetadata(c, client, logs)
	records, exists, _ = drainMetadata(c, client, empty, 1)
	c.Assert(exists, qt.IsTrue)
	c.Assert(records, qt.HasLen, 0)
	putTag(c, client, tags, tagRecord("release", 0))
	zero := lookupTag(c, client, tags, "release")
	c.Assert(zero.Found, qt.IsTrue)
	c.Assert(proto.Equal(zero.Record, tagRecord("release", 0)), qt.IsTrue)
	appendLog := func(sequence int64) {
		c.Helper()
		metadataMutation(c, client, &protocol.MutateMetadataRequest{Scope: logs, Mutation: &protocol.MutateMetadataRequest_AppendLog{AppendLog: logRecord(sequence)}}, protocol.MetadataResult_APPLIED)
	}
	appendLog(1)
	metadataMutation(c, client, &protocol.MutateMetadataRequest{Scope: logs, Mutation: &protocol.MutateMetadataRequest_AppendLog{AppendLog: logRecord(1)}}, protocol.MetadataResult_REJECTED)
	other, otherID, _ := historySession(c, binary, digest)
	otherTags := proto.CloneOf(tags)
	otherTags.SessionId = otherID
	tx, err := client.BeginTransaction(c.Context(), &protocol.BeginTransactionRequest{SessionId: id, TransactionId: "rollback-tags", Isolation: protocol.TransactionIsolation_SERIALIZABLE})
	c.Assert(err, qt.IsNil)
	bound := proto.CloneOf(tags)
	bound.TransactionId = tx.Id
	putTag(c, client, bound, tagRecord("release", -9223372036854775808))
	c.Assert(lookupTag(c, other, otherTags, "release").Record.Version, qt.Equals, int64(0))
	appendLog(2)
	prepared, err := client.PrepareMetadata(c.Context(), &protocol.PrepareMetadataRequest{Scope: bound, RequestId: "inside"})
	c.Assert(err, qt.IsNil)
	c.Assert(prepared.Outcome, qt.Equals, protocol.MetadataResult_APPLIED)
	absent := proto.CloneOf(bound)
	absent.Table = "must_not_create"
	prepared, err = client.PrepareMetadata(c.Context(), &protocol.PrepareMetadataRequest{Scope: absent, RequestId: "inside-missing"})
	c.Assert(err, qt.ErrorIs, provider.ErrExecution)
	c.Assert(prepared.Outcome, qt.Equals, protocol.MetadataResult_REJECTED)
	captured := openMetadata(c, client, bound)
	putTag(c, client, bound, tagRecord("release", 42))
	records, exists, _ = drainMetadata(c, client, captured, 1)
	c.Assert(exists, qt.IsTrue)
	c.Assert(records, qt.HasLen, 1)
	c.Assert(proto.Equal(records[0], tagRecord("release", -9223372036854775808)), qt.IsTrue)
	unfinished := openMetadata(c, client, bound)
	rolled, err := client.FinishTransaction(c.Context(), &protocol.FinishTransactionRequest{SessionId: id, TransactionId: tx.Id, Action: protocol.FinishTransactionRequest_ROLLBACK})
	c.Assert(err, qt.IsNil)
	c.Assert(rolled.Outcome, qt.Equals, protocol.TransactionResult_ROLLED_BACK)
	_, err = client.ReadMetadataPage(c.Context(), &protocol.ReadMetadataPageRequest{Reader: unfinished, MaxRecords: 1})
	c.Assert(err, qt.IsNotNil)
	c.Assert(lookupTag(c, client, tags, "release").Record.Version, qt.Equals, int64(0))
	read := openMetadata(c, client, logs)
	records, exists, _ = drainMetadata(c, client, read, 1)
	c.Assert(exists, qt.IsTrue)
	c.Assert(records, qt.HasLen, 2)
	c.Assert(proto.Equal(records[0], logRecord(1)), qt.IsTrue)
	c.Assert(proto.Equal(records[1], logRecord(2)), qt.IsTrue)
	tx, err = client.BeginTransaction(c.Context(), &protocol.BeginTransactionRequest{SessionId: id, TransactionId: "commit-tags", Isolation: protocol.TransactionIsolation_SERIALIZABLE})
	c.Assert(err, qt.IsNil)
	bound.TransactionId = tx.Id
	putTag(c, client, bound, tagRecord("release", -7))
	appendLog(3)
	openMetadata(c, client, bound)
	committed, err := client.FinishTransaction(c.Context(), &protocol.FinishTransactionRequest{SessionId: id, TransactionId: tx.Id, Action: protocol.FinishTransactionRequest_COMMIT})
	c.Assert(err, qt.IsNil)
	c.Assert(committed.Outcome, qt.Equals, protocol.TransactionResult_COMMITTED)
	c.Assert(proto.Equal(lookupTag(c, other, otherTags, "release").Record, tagRecord("release", -7)), qt.IsTrue)
	for _, want := range []bool{true, false} {
		deleted := metadataMutation(c, client, &protocol.MutateMetadataRequest{Scope: tags, Mutation: &protocol.MutateMetadataRequest_DeleteTag{DeleteTag: "release"}}, protocol.MetadataResult_APPLIED)
		c.Assert(deleted.Deleted, qt.Equals, want)
	}
	c.Assert(lookupTag(c, client, tags, "release").Found, qt.IsFalse)
}

func TestNativeMetadataPagesExceedOneRPCWithoutChangingSnapshot(t *testing.T) {
	c := qt.New(t)
	opened, directory, database := inspectionFixture(c)
	binary, digest := providerBinary(c)
	client, id, _ := historySession(c, binary, digest)
	scope := &protocol.MetadataScope{SessionId: id, Schema: directory, Table: "events", Kind: protocol.MetadataScope_LOG}
	prepareMetadataStore(c, client, opened, database, scope)
	store, err := metadata.Resolve(database, scope)
	c.Assert(err, qt.IsNil)
	const count = 300
	var declarations, values []string
	var args []any
	for index := 0; index < count; index++ {
		row := logRecord(int64(index + 1))
		row.RunId = "bulk"
		row.Error = strings.Repeat("x", 12<<10)
		payload, err := store.Encode(row)
		c.Assert(err, qt.IsNil)
		name := fmt.Sprintf("record%d", index)
		declarations = append(declarations, "DECLARE $"+name+" AS String;")
		values = append(values, fmt.Sprintf("(\"bulk\",%d,$%s)", index+1, name))
		args = append(args, sql.Named(name, payload))
	}
	_, err = opened.DB.ExecContext(c.Context(), strings.Join(declarations, " ")+" INSERT INTO "+store.QuotedPath()+" (run_id, sequence, record) VALUES "+strings.Join(values, ","), args...)
	c.Assert(err, qt.IsNil)
	reader := openMetadata(c, client, scope)
	metadataMutation(c, client, &protocol.MutateMetadataRequest{Scope: scope, Mutation: &protocol.MutateMetadataRequest_AppendLog{AppendLog: logRecord(1)}}, protocol.MetadataResult_APPLIED)
	records, exists, pages := drainMetadata(c, client, reader, provider.MaxMetadataRecords)
	c.Assert(exists, qt.IsTrue)
	c.Assert(records, qt.HasLen, count)
	c.Assert(pages > 1, qt.IsTrue)
	for index, record := range records {
		row, ok := record.(*protocol.MigrationLogRecord)
		c.Assert(ok, qt.IsTrue)
		c.Assert(row.Sequence, qt.Equals, int64(index+1))
		c.Assert(row.RunId, qt.Equals, "bulk")
		c.Assert(row.Error, qt.Equals, strings.Repeat("x", 12<<10))
	}
	records, _, _ = drainMetadata(c, client, openMetadata(c, client, scope), provider.MaxMetadataRecords)
	c.Assert(records, qt.HasLen, count+1)
}

func TestNativeMetadataOwnershipInspectionAndCorruption(t *testing.T) {
	c := qt.New(t)
	opened, directory, database := inspectionFixture(c)
	binary, digest := providerBinary(c)
	client, id, _ := historySession(c, binary, digest)
	tags := &protocol.MetadataScope{SessionId: id, Schema: directory, Table: "tags", Kind: protocol.MetadataScope_TAGS}
	logs := proto.CloneOf(tags)
	logs.Table = "events"
	logs.Kind = protocol.MetadataScope_LOG
	prepareMetadataStore(c, client, opened, database, tags)
	prepareMetadataStore(c, client, opened, database, logs)
	foreign := proto.CloneOf(tags)
	foreign.Table = "foreign"
	foreignPath := path.Join(database, directory, foreign.Table)
	_, err := opened.DB.ExecContext(c.Context(), "CREATE TABLE `"+foreignPath+"` (name Utf8 NOT NULL, record String NOT NULL, PRIMARY KEY(name))")
	c.Assert(err, qt.IsNil)
	c.Cleanup(func() {
		_, err := opened.DB.ExecContext(context.Background(), "DROP TABLE `"+foreignPath+"`")
		c.Check(err, qt.IsNil)
	})
	refused, err := client.PrepareMetadata(c.Context(), &protocol.PrepareMetadataRequest{Scope: foreign, RequestId: "foreign"})
	c.Assert(err, qt.ErrorIs, provider.ErrExecution)
	c.Assert(refused.Outcome, qt.Equals, protocol.MetadataResult_REJECTED)
	snapshot, err := client.Inspect(c.Context(), &protocol.InspectRequest{SessionId: id, Schemas: []string{directory}})
	c.Assert(err, qt.IsNil)
	c.Assert(snapshot.Tables, qt.HasLen, 1)
	c.Assert(snapshot.Tables[0].Identity.Name, qt.Equals, "foreign")
	c.Assert(snapshot.ReservedObjects, qt.HasLen, 2)
	purposes := map[string]protocol.ReservedObject_Purpose{}
	for _, reserved := range snapshot.ReservedObjects {
		purposes[reserved.Object.Name] = reserved.Purpose
	}
	c.Assert(purposes, qt.DeepEquals, map[string]protocol.ReservedObject_Purpose{"events": protocol.ReservedObject_LOG, "tags": protocol.ReservedObject_TAG})
	putTag(c, client, tags, tagRecord("release", 0))
	store, err := metadata.Resolve(database, tags)
	c.Assert(err, qt.IsNil)
	_, err = opened.DB.ExecContext(c.Context(), "DECLARE $payload AS String; UPSERT INTO "+store.QuotedPath()+" (name, record) VALUES (\"release\", $payload)", sql.Named("payload", []byte("corrupt")))
	c.Assert(err, qt.IsNil)
	_, err = client.LookupMetadataTag(c.Context(), &protocol.LookupMetadataTagRequest{Scope: tags, RequestId: "corrupt", Name: "release"})
	c.Assert(err, qt.IsNotNil)
	_, err = client.OpenMetadataRead(c.Context(), &protocol.MetadataReadRequest{Scope: tags, RequestId: "corrupt", ReaderId: "corrupt"})
	c.Assert(err, qt.IsNotNil)
	metadataMutation(c, client, &protocol.MutateMetadataRequest{Scope: tags, Mutation: &protocol.MutateMetadataRequest_PutTag{PutTag: tagRecord("release", 1)}}, protocol.MetadataResult_REJECTED)
	tx, err := client.BeginTransaction(c.Context(), &protocol.BeginTransactionRequest{SessionId: id, TransactionId: "corrupt-read", Isolation: protocol.TransactionIsolation_SERIALIZABLE})
	c.Assert(err, qt.IsNil)
	bound := proto.CloneOf(tags)
	bound.TransactionId = tx.Id
	putTag(c, client, bound, tagRecord("must-rollback", 7))
	_, err = client.LookupMetadataTag(c.Context(), &protocol.LookupMetadataTagRequest{Scope: bound, RequestId: "corrupt-in-transaction", Name: "release"})
	c.Assert(err, qt.IsNotNil)
	ended, err := client.FinishTransaction(c.Context(), &protocol.FinishTransactionRequest{SessionId: id, TransactionId: tx.Id, Action: protocol.FinishTransactionRequest_COMMIT})
	c.Assert(err, qt.ErrorIs, provider.ErrExecution)
	c.Assert(ended.Outcome, qt.Equals, protocol.TransactionResult_ROLLED_BACK)
	c.Assert(lookupTag(c, client, tags, "must-rollback").Found, qt.IsFalse)

	_, err = opened.DB.ExecContext(c.Context(), "ALTER TABLE `"+path.Join(database, directory, logs.Table)+"` ADD COLUMN unexpected Utf8")
	c.Assert(err, qt.IsNil)
	snapshot, err = client.Inspect(c.Context(), &protocol.InspectRequest{SessionId: id, Schemas: []string{directory}})
	c.Assert(err, qt.IsNotNil)
	c.Assert(snapshot, qt.IsNil)
}

func TestNativeMetadataReaderFencingAndSessionCleanup(t *testing.T) {
	c := qt.New(t)
	opened, directory, database := inspectionFixture(c)
	binary, digest := providerBinary(c)
	client, id, closeSession := historySession(c, binary, digest)
	scope := &protocol.MetadataScope{SessionId: id, Schema: directory, Table: "tags", Kind: protocol.MetadataScope_TAGS}
	prepareMetadataStore(c, client, opened, database, scope)
	putTag(c, client, scope, tagRecord("first", 1))
	putTag(c, client, scope, tagRecord("second", 2))
	fenced := &protocol.MetadataReadRequest{Scope: scope, RequestId: "close-first", ReaderId: "fenced"}
	_, err := client.CloseMetadataRead(c.Context(), fenced)
	c.Assert(err, qt.IsNil)
	_, err = client.OpenMetadataRead(c.Context(), fenced)
	c.Assert(err, qt.IsNotNil)
	reader := openMetadata(c, client, scope)
	wrong := proto.CloneOf(reader)
	wrong.Scope.Table = "foreign"
	_, err = client.CloseMetadataRead(c.Context(), wrong)
	c.Assert(err, qt.IsNotNil)
	first, err := client.ReadMetadataPage(c.Context(), &protocol.ReadMetadataPageRequest{Reader: reader, MaxRecords: 1})
	c.Assert(err, qt.IsNil)
	c.Assert(first.Complete, qt.IsFalse)
	_, err = client.ReadMetadataPage(c.Context(), &protocol.ReadMetadataPageRequest{Reader: reader, MaxRecords: 1})
	c.Assert(err, qt.IsNotNil)
	remaining := openMetadata(c, client, scope)
	closeSession()
	_, err = client.ReadMetadataPage(c.Context(), &protocol.ReadMetadataPageRequest{Reader: remaining, MaxRecords: 1})
	c.Assert(err, qt.IsNotNil)
	_, err = client.CloseMetadataRead(c.Context(), remaining)
	c.Assert(err, qt.IsNil)
}
