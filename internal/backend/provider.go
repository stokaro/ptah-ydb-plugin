// Package backend implements the YDB provider's experimental services.
package backend

import (
	"context"
	"crypto/rand"
	"errors"
	"sync"

	"github.com/stokaro/ptah-ydb-plugin/internal/connection"
	"github.com/stokaro/ptah-ydb-plugin/internal/coordinationcompare"
	"github.com/stokaro/ptah-ydb-plugin/internal/coordinationplan"
	"github.com/stokaro/ptah-ydb-plugin/schema"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/stokaro/ptah/provider-sdk/provider"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// Provider owns connection handles for one subprocess lifetime.
// Closed entries retain their cleanup result so repeated closes cannot turn
// an uncertain cleanup into success. The process bounds retained handles.
type Provider struct {
	mu       sync.Mutex
	sessions map[string]*session
	opening  int
}

type session struct {
	mu              sync.Mutex
	connection      *connection.Connection
	database        string
	closed          bool
	closeErr        error
	cleanupErr      error
	transactions    map[string]*transaction
	active          string
	locks           map[string]*lockRecord
	fenced          bool
	metadataReaders map[string]*metadataReader
}

// New constructs an isolated session service.
func New() *Provider { return &Provider{sessions: make(map[string]*session)} }

// Description advertises native runtime services and offline coordination
// decoding, normalization, comparison, and native operation planning.
func Description() *protocol.Description {
	return &protocol.Description{
		Revision: provider.Revision, ProviderId: schema.ProviderID,
		Version: "0.0.0-dev", SemanticIdentity: "ydb-namespaced-models-1",
		Features: []string{provider.PlanFeaturesFeature, provider.RenderFeature, provider.CompareModelsFeature, provider.DecodeSourceFeature, provider.InspectFeature, provider.NormalizeValuesFeature, provider.ExecuteFeature, provider.TransactionsFeature, provider.HistoryFeature, provider.LocksFeature, provider.MetadataFeature, provider.AnalyzeSQLFeature},
		Engines:  []*protocol.Engine{{Id: "ydb", Aliases: []string{"ydbs"}, Identifiers: identifierRules(), ValueModels: schema.Models(), Planning: []*protocol.ModelPlanning{{Id: coordinationplan.ServiceID, ChangeKinds: []string{schema.CoordinationChange}, OperationKinds: []string{schema.CoordinationOperation}}}, Comparisons: []*protocol.ModelComparison{{Id: coordinationcompare.ServiceID, Placement: protocol.ModelComparison_OBJECTS, Kinds: []string{schema.CoordinationProperties}, ChangeKinds: []string{schema.CoordinationChange}}}, YamlSourceKeys: []*protocol.YAMLSourceKey{schema.CoordinationYAMLKey()}, Features: []string{provider.PlanFeaturesFeature, provider.RenderFeature, provider.CompareModelsFeature, provider.DecodeSourceFeature, provider.InspectFeature, provider.NormalizeValuesFeature, provider.ExecuteFeature, provider.TransactionsFeature, provider.HistoryFeature, provider.LocksFeature, provider.MetadataFeature, provider.AnalyzeSQLFeature}}},
	}
}

func identifierRules() *protocol.IdentifierRules {
	return &protocol.IdentifierRules{
		TableNames: protocol.IdentifierRules_EXACT, ColumnNames: protocol.IdentifierRules_EXACT,
		IndexNames: protocol.IdentifierRules_EXACT, IndexNamespace: protocol.IdentifierRules_TABLE,
	}
}

// Open verifies a native connection and server version before publishing its
// handle. The address and credentials never appear in receipts or errors.
func (p *Provider) Open(ctx context.Context, request *protocol.OpenRequest) (*protocol.Session, error) {
	if err := ctx.Err(); err != nil {
		return nil, status.FromContextError(err).Err()
	}
	if request.GetEngine() != "ydb" {
		return nil, status.Error(codes.InvalidArgument, "unsupported engine")
	}
	address, err := connection.Parse(request.GetAddress())
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	p.mu.Lock()
	if len(p.sessions)+p.opening >= 1024 {
		p.mu.Unlock()
		return nil, status.Error(codes.ResourceExhausted, "YDB session handle limit reached; restart the provider")
	}
	p.opening++
	p.mu.Unlock()
	defer func() { p.mu.Lock(); p.opening--; p.mu.Unlock() }()
	opened, err := connection.Open(ctx, address)
	if err != nil {
		return nil, connectionError(ctx, "open YDB connection")
	}
	var version string
	if err := opened.DB.QueryRowContext(ctx, "SELECT Version()").Scan(&version); err != nil {
		_ = opened.Close()
		return nil, connectionError(ctx, "read YDB server version")
	}
	if err := ctx.Err(); err != nil {
		_ = opened.Close()
		return nil, status.FromContextError(err).Err()
	}
	id := rand.Text()
	p.mu.Lock()
	p.sessions[id] = &session{connection: opened, database: address.Database()}
	p.mu.Unlock()
	return &protocol.Session{Id: id, Engine: "ydb", Database: address.Database(), ServerVersion: version, Identifiers: identifierRules(), Capabilities: map[string]bool{"transactional_ddl": false, "coordination_nodes": true}}, nil
}

// Close releases only the named session. Repeated closes return the original
// cleanup result; a canceled caller still triggers bounded cleanup.
func (p *Provider) Close(_ context.Context, request *protocol.CloseRequest) error {
	p.mu.Lock()
	opened := p.sessions[request.GetSessionId()]
	p.mu.Unlock()
	if opened == nil {
		return status.Error(codes.NotFound, "unknown YDB session")
	}
	opened.mu.Lock()
	defer opened.mu.Unlock()
	if !opened.closed {
		opened.closed = true
		opened.closeMetadataReaders("", true)
		opened.rollbackTransactions()
		opened.closeLocks()
		opened.closeErr = errors.Join(opened.cleanupErr, opened.connection.Close())
	}
	if opened.closeErr != nil {
		return status.Error(codes.Unavailable, "close YDB connection failed")
	}
	return nil
}

func connectionError(ctx context.Context, operation string) error {
	if err := ctx.Err(); err != nil {
		return status.FromContextError(err).Err()
	}
	// Native authentication errors may embed credential-source details. Until
	// structured redaction is available, return the operation without raw text.
	return status.Error(codes.Unavailable, operation+" failed")
}
