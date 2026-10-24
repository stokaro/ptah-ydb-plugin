package backend

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/stokaro/ptah-ydb-plugin/internal/nativeapi"
	"github.com/stokaro/ptah-ydb-plugin/internal/ownedtable"
	"github.com/stokaro/ptah-ydb-plugin/internal/rowdata"
	"github.com/stokaro/ptah-ydb-plugin/internal/seeds"
	"github.com/stokaro/ptah-ydb-plugin/internal/yql"
	"github.com/stokaro/ptah-ydb-plugin/internal/yqlquery"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/ydb-platform/ydb-go-genproto/Ydb_Scheme_V1"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb_Scheme"
	ydb "github.com/ydb-platform/ydb-go-sdk/v3"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// seedSession locks the session a seed request names for the duration of the
// request. A seed runs in a transaction of its own, so a session with an
// active transaction is refused.
func (p *Provider) seedSession(ctx context.Context, id string) (*session, error) {
	s, err := p.lookup(id)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	switch {
	case s.closed:
		s.mu.Unlock()
		return nil, status.Error(codes.FailedPrecondition, "YDB session is closed")
	case s.active != "":
		s.mu.Unlock()
		return nil, status.Error(codes.FailedPrecondition, "YDB session has an active transaction")
	}
	if err := ctx.Err(); err != nil {
		s.mu.Unlock()
		return nil, status.FromContextError(err).Err()
	}
	return s, nil
}

// ListSeedTables lists the row and column tables of one directory, in name
// order, as the host's embedded engine does for its protected-table check. A
// directory that does not exist, and one under a directory whose name starts
// with a dot, which belongs to the server, holds no table.
func (p *Provider) ListSeedTables(ctx context.Context, request *protocol.ListSeedTablesRequest) (*protocol.SeedTables, error) {
	if err := rowdata.ValidateDirectory(request.GetSchema()); err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	s, err := p.seedSession(ctx, request.GetSessionId())
	if err != nil {
		return nil, err
	}
	defer s.mu.Unlock()
	result := &protocol.SeedTables{}
	if slices.ContainsFunc(strings.Split(request.GetSchema(), "/"), func(part string) bool { return strings.HasPrefix(part, ".") }) {
		return result, nil
	}
	client := Ydb_Scheme_V1.NewSchemeServiceClient(ydb.GRPCConn(s.connection.Driver))
	response, err := client.ListDirectory(ctx, &Ydb_Scheme.ListDirectoryRequest{Path: path.Join(s.database, request.GetSchema())})
	if err != nil {
		return nil, connectionError(ctx, "list the YDB directory")
	}
	if response.GetOperation().GetStatus() == Ydb.StatusIds_SCHEME_ERROR {
		return result, nil
	}
	var listing Ydb_Scheme.ListDirectoryResult
	if err := nativeapi.Operation(response.GetOperation(), &listing); err != nil {
		return nil, status.Error(codes.Unavailable, err.Error())
	}
	var names []string
	for _, entry := range listing.GetChildren() {
		switch entry.GetType() {
		case Ydb_Scheme.Entry_TABLE, Ydb_Scheme.Entry_COLUMN_TABLE:
			names = append(names, entry.GetName())
		}
	}
	slices.Sort(names)
	for _, name := range names {
		result.Tables = append(result.Tables, &protocol.Reference{Kind: "table", Schema: request.GetSchema(), Name: name})
	}
	return result, nil
}

// ApplySeed runs a seed file and its ledger row in one serializable
// transaction; see internal/seeds. A file that holds a statement YDB runs only
// outside a transaction, a translation setting or a delimiter directive the
// file cannot be split under, and in a dev realm a statement whose effect
// leaves the realm, is refused before anything runs.
func (p *Provider) ApplySeed(ctx context.Context, request *protocol.ApplySeedRequest) (*protocol.SeedResult, error) {
	s, err := p.seedSession(ctx, request.GetSessionId())
	if err != nil {
		return nil, err
	}
	defer s.mu.Unlock()
	seed := request.GetSeed()
	result := &protocol.SeedResult{RequestId: request.GetRequestId(), Path: seed.GetPath()}
	if offset, reason, refused := seeds.Refusal(seed.GetText()); refused {
		return refuseSeed(result, seed.GetText(), offset, reason), nil
	}
	if s.realm {
		if err := yql.ConfineToRealm(seed.GetText()); err != nil {
			return refuseSeed(result, seed.GetText(), 0, err.Error()), nil
		}
	}
	queries, err := yqlquery.Split(seed.GetText())
	if err != nil {
		return refuseSeed(result, seed.GetText(), 0, err.Error()), nil
	}
	return s.runSeed(ctx, result, func(ctx context.Context, begin seeds.Begin) seeds.Outcome {
		return seeds.Apply(ctx, begin, seeds.Classify, queries, seed, time.Now)
	})
}

// RecordSeed writes the ledger row of a seed without running it.
func (p *Provider) RecordSeed(ctx context.Context, request *protocol.RecordSeedRequest) (*protocol.SeedResult, error) {
	s, err := p.seedSession(ctx, request.GetSessionId())
	if err != nil {
		return nil, err
	}
	defer s.mu.Unlock()
	result := &protocol.SeedResult{RequestId: request.GetRequestId(), Path: request.GetSeed().GetPath()}
	return s.runSeed(ctx, result, func(ctx context.Context, begin seeds.Begin) seeds.Outcome {
		return seeds.Record(ctx, begin, seeds.Classify, request.GetSeed(), time.Now)
	})
}

// runSeed prepares the ledger and runs a seed transaction under the session's
// guarding locks.
func (s *session) runSeed(ctx context.Context, result *protocol.SeedResult, run func(context.Context, seeds.Begin) seeds.Outcome) (*protocol.SeedResult, error) {
	guarded, stop, err := s.guard(ctx)
	if err != nil {
		result.Outcome, result.Reason = protocol.SeedResult_NOT_APPLIED, "the session's locks are no longer held; nothing ran"
		return result, nil
	}
	defer stop()
	ctx = guarded
	if reason, err := s.prepareLedger(ctx); err != nil {
		result.Outcome, result.Reason = protocol.SeedResult_NOT_APPLIED, reason
		return result, nil
	}
	conn, err := s.connection.DB.Conn(ctx)
	if err != nil {
		result.Outcome, result.Reason = protocol.SeedResult_NOT_APPLIED, "no YDB connection was available; nothing ran"
		return result, nil
	}
	defer func() { s.cleanupErr = errors.Join(s.cleanupErr, closeSQLConnection(conn)) }()
	begin := func(ctx context.Context) (seeds.Tx, error) {
		if err := s.verifyLocks(ctx); err != nil {
			return nil, err
		}
		return conn.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable})
	}
	outcome := run(ctx, begin)
	if outcome.Outcome == protocol.SeedResult_APPLIED && s.verifyLocks(ctx) != nil {
		outcome = seeds.Outcome{Outcome: protocol.SeedResult_UNKNOWN, Reason: "the session's locks were lost while the seed ran"}
	}
	result.Outcome, result.Reason = outcome.Outcome, rowdata.OneLine(outcome.Reason)
	return result, nil
}

// prepareLedger creates a missing ledger and refuses one this provider does
// not recognize. The reason names what stopped it.
func (s *session) prepareLedger(ctx context.Context) (string, error) {
	identity, err := ownedtable.Resolve(s.database, "", seeds.Table)
	if err != nil {
		return "the seed ledger's path is not valid", err
	}
	exists, err := identity.Check(ctx, s.connection.Driver, seeds.CheckLayout)
	if err != nil {
		return rowdata.OneLine("the seed ledger cannot be used: " + err.Error()), err
	}
	if exists {
		return "", nil
	}
	if err := s.verifyLocks(ctx); err != nil {
		return "the session's locks are no longer held; nothing ran", err
	}
	if err := s.executeSchema(ctx, seeds.CreateStatement); err != nil {
		return "the seed ledger could not be created; nothing ran", err
	}
	if _, err := identity.Check(ctx, s.connection.Driver, seeds.CheckLayout); err != nil {
		return rowdata.OneLine("the seed ledger cannot be used: " + err.Error()), err
	}
	return "", nil
}

// ReadSeedLedger reads the whole ledger in path order. Reading never creates
// it: a missing ledger is a complete answer that says so.
func (p *Provider) ReadSeedLedger(ctx context.Context, request *protocol.ReadSeedLedgerRequest) (*protocol.SeedLedger, error) {
	s, err := p.seedSession(ctx, request.GetSessionId())
	if err != nil {
		return nil, err
	}
	defer s.mu.Unlock()
	identity, err := ownedtable.Resolve(s.database, "", seeds.Table)
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	exists, err := identity.Check(ctx, s.connection.Driver, seeds.CheckLayout)
	if err != nil {
		return nil, status.Error(codes.FailedPrecondition, err.Error())
	}
	ledger := &protocol.SeedLedger{Exists: exists, Complete: true}
	if !exists {
		return ledger, nil
	}
	rows, err := s.connection.DB.QueryContext(ctx, seeds.ReadStatement(request.GetMaxEntries()))
	if err != nil {
		return nil, connectionError(ctx, "read the YDB seed ledger")
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var entry protocol.SeedLedgerEntry
		var applied time.Time
		if err := rows.Scan(&entry.Path, &entry.Checksum, &entry.Env, &applied); err != nil {
			return nil, connectionError(ctx, "read the YDB seed ledger")
		}
		if len(ledger.Entries) == int(request.GetMaxEntries()) {
			return nil, status.Error(codes.ResourceExhausted, fmt.Sprintf("the seed ledger holds more than %d entries", request.GetMaxEntries()))
		}
		entry.AppliedAtSeconds, entry.AppliedAtNanos = applied.Unix(), int32(applied.Nanosecond())
		ledger.Entries = append(ledger.Entries, &entry)
	}
	if err := rows.Err(); err != nil {
		return nil, connectionError(ctx, "read the YDB seed ledger")
	}
	return ledger, nil
}

// refuseSeed is a REFUSED result at a byte offset of the seed text.
func refuseSeed(result *protocol.SeedResult, text string, offset int, reason string) *protocol.SeedResult {
	result.Outcome, result.Reason = protocol.SeedResult_REFUSED, rowdata.OneLine(reason)
	offset = min(max(offset, 0), len(text))
	line := strings.Count(text[:offset], "\n") + 1
	column := utf8.RuneCountInString(text[strings.LastIndexByte(text[:offset], '\n')+1:offset]) + 1
	result.Position = &protocol.SourcePosition{ByteOffset: uint64(offset), Line: uint32(line), Column: uint32(column)}
	return result
}
