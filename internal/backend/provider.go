// Package backend implements the YDB provider's experimental services.
package backend

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"maps"
	"net/url"
	"slices"
	"sync"

	"github.com/stokaro/ptah-ydb-plugin/internal/changefeeds"
	"github.com/stokaro/ptah-ydb-plugin/internal/changefeedsource"
	"github.com/stokaro/ptah-ydb-plugin/internal/commonobject"
	"github.com/stokaro/ptah-ydb-plugin/internal/connection"
	"github.com/stokaro/ptah-ydb-plugin/internal/coordinationcompare"
	"github.com/stokaro/ptah-ydb-plugin/internal/coordinationplan"
	"github.com/stokaro/ptah-ydb-plugin/internal/declarationplan"
	"github.com/stokaro/ptah-ydb-plugin/internal/embedstore"
	"github.com/stokaro/ptah-ydb-plugin/internal/externalcompare"
	"github.com/stokaro/ptah-ydb-plugin/internal/externalplan"
	"github.com/stokaro/ptah-ydb-plugin/internal/externalsource"
	"github.com/stokaro/ptah-ydb-plugin/internal/featureflags"
	"github.com/stokaro/ptah-ydb-plugin/internal/migrationlint"
	"github.com/stokaro/ptah-ydb-plugin/internal/releaseline"
	"github.com/stokaro/ptah-ydb-plugin/internal/secretcompare"
	"github.com/stokaro/ptah-ydb-plugin/internal/secretplan"
	"github.com/stokaro/ptah-ydb-plugin/internal/secretsource"
	"github.com/stokaro/ptah-ydb-plugin/internal/streaming"
	"github.com/stokaro/ptah-ydb-plugin/internal/streamingcompare"
	"github.com/stokaro/ptah-ydb-plugin/internal/streamingplan"
	"github.com/stokaro/ptah-ydb-plugin/internal/streamingsource"
	"github.com/stokaro/ptah-ydb-plugin/internal/tablecompare"
	"github.com/stokaro/ptah-ydb-plugin/internal/tablecreate"
	"github.com/stokaro/ptah-ydb-plugin/internal/tablesource"
	"github.com/stokaro/ptah-ydb-plugin/internal/topiccompare"
	"github.com/stokaro/ptah-ydb-plugin/internal/topicplan"
	"github.com/stokaro/ptah-ydb-plugin/internal/topicsource"
	"github.com/stokaro/ptah-ydb-plugin/internal/workload"
	"github.com/stokaro/ptah-ydb-plugin/internal/workloadcompare"
	"github.com/stokaro/ptah-ydb-plugin/internal/workloadplan"
	"github.com/stokaro/ptah-ydb-plugin/internal/workloadsource"
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
	mu         sync.Mutex
	connection *connection.Connection
	// database is the root the session treats as its database: the dev realm's
	// directory when the address names one.
	database string
	// realm reports a session on a dev realm, whose statements are confined to it.
	realm            bool
	closed           bool
	closeErr         error
	cleanupErr       error
	transactions     map[string]*transaction
	active           string
	locks            map[string]*lockRecord
	fenced           bool
	metadataReaders  map[string]*metadataReader
	statementResults map[string]*statementResult
	// version is what the server answered SELECT Version() with.
	version string
	// embeddings is the session's embedding store, made on its first
	// embedding call.
	embeddings *embedstore.Store
	// capabilities are the capabilities the session reported.
	capabilities map[string]bool
}

// providerFeatures are the services and behaviors the provider offers.
func providerFeatures() []string {
	return []string{provider.SchemaDeclarationsFeature, provider.SensitiveInputsFeature, provider.ScopeSelectionsFeature, provider.StatementsFeature, provider.ValidateSchemaFeature, provider.ProjectTableCreationsFeature, provider.PrepareTablesFeature, provider.PlanFeaturesFeature, provider.RenderFeature, provider.CompareModelsFeature, provider.DecodeSourceFeature, provider.InspectFeature, provider.NormalizeValuesFeature, provider.ConvertValuesFeature, provider.ExecuteFeature, provider.TransactionsFeature, provider.HistoryFeature, provider.LocksFeature, provider.MetadataFeature, provider.AnalyzeSQLFeature, provider.DevDatabaseFeature, provider.LintFeature, provider.TypeRowsFeature, provider.ReadRowsFeature, provider.RenderRowWritesFeature, provider.SeedsFeature, provider.DevCleanupFeature, provider.ReplayGuardFeature, provider.StatementTimeoutFeature, provider.EmbeddingStoreFeature, provider.ReadSQLSchemaFeature, provider.TextUnitsFeature, provider.PlanningPolicyFeature, provider.CommonObjectsFeature, provider.CommonAccessFeature, provider.PlanningLifecycleFeature, provider.EntryKeysFeature, provider.HCLSourceFeature, provider.EncodeSourceFeature}
}

// engineFeatures is the feature list of the ydb engine: every feature of the
// provider, dev realms, a property of the engine rather than a service, and the
// statement facts of its migration lint, which the contract lists beside the
// engine's lint. The description, the session it opens and the inspection
// snapshot all report it, so the host holds one target profile for observed,
// desired and planned values.
func engineFeatures() []string {
	return append(providerFeatures(), provider.DevRealmsFeature, provider.LintStatementFactsFeature)
}

// negotiatedFeatures is the engine's feature list as the host holds it: the
// two-way features the host did not list are left out, as the SDK leaves them
// out of the description the host reads. A session and an inspection snapshot
// report it, so the host can check them against that description.
func negotiatedFeatures(ctx context.Context) []string {
	return provider.NegotiatedDescription(Description(), provider.HostFeatures(ctx)).GetEngines()[0].GetFeatures()
}

// sessionCapabilities are the host capabilities a session reports before the
// cluster's feature flags are read: what every supported release line does
// with its default flags.
//
// migration_statement_timeout is true because the host bounds each statement of
// a migration with a deadline on the Execute call, and a unit the deadline
// stops is reported with outcome UNKNOWN and no retry (see executeUnits): YDB
// keeps running a schema statement after its client stops waiting, so the
// provider never reports a stopped statement as not applied.
//
// tiered_ttl is false, because EnableTieringInColumnShard is off by default
// on every line from 25.1 to 26.2. An address that names the monitoring
// endpoint replaces it with the flag's value (see featureflags).
//
// resource_pools is false, because EnableResourcePools is off by default on
// every line, and the monitoring endpoint's flags decide it the same way.
//
// topics is true on every line: the provider declares, reads and plans a
// standalone topic. topic_consumer_availability_period is a consumer's
// availability_period, which 25.4 and later take (`AVAILABILITY_PERIOD:
// unknown option for consumer` on 25.1 to 25.3, as the host measured).
//
// external_data_sources and external_object_replace are false, because
// their flags are off by default on every line; the cluster's flags replace
// them where the address names the monitoring endpoint.
// external_data_source_secret_paths is an option ending in _SECRET_PATH,
// which 25.4 and later take and 25.1 to 25.3 read as a missing name
// (`PASSWORD_SECRET_NAME requires key`).
func sessionCapabilities(version string) map[string]bool {
	return map[string]bool{"transactional_ddl": false, "secrets": releaseline.AtLeast(version, 25, 4), "coordination_nodes": true, "migration_statement_timeout": true, "tiered_ttl": false,
		"role_management": true, "role_membership": true, "group_principals": true, "database_grants": true,
		"relative_grant_paths": releaseline.AtLeast(version, 26, 1), "topics": true, "topic_consumer_availability_period": releaseline.AtLeast(version, 25, 4), workload.CapabilityResourcePools: false, streaming.Capability: false,
		changefeeds.CapabilityChangefeeds: true, changefeeds.CapabilityUserSIDs: releaseline.AtLeast(version, 26, 1),
		changefeeds.CapabilitySchemaChanges: releaseline.AtLeast(version, 25, 3), changefeeds.CapabilityTopicAutoPartitioning: releaseline.AtLeast(version, 25, 2),
		"external_data_sources": false, "external_object_replace": false, "external_data_source_secret_paths": releaseline.AtLeast(version, 25, 4)}
}

// clusterCapabilities refines the preset with the feature flags of the
// database, read from the monitoring endpoint when the address names one.
// Without the endpoint the preset stands. A failed read fails the session
// rather than falling back to the preset: the operator asked for the cluster's
// answer, and planning without it would be planning with an answer nobody
// asked for.
func clusterCapabilities(ctx context.Context, opened *connection.Connection, version string) (map[string]bool, error) {
	if !opened.HasMonitoring() {
		return sessionCapabilities(version), nil
	}
	body, err := opened.ReadMonitoring(ctx, "feature flags", featureflags.Path, url.Values{"database": {opened.Database()}})
	if err != nil {
		return nil, err
	}
	flags, err := featureflags.Decode(body, opened.Database())
	if err != nil {
		return nil, fmt.Errorf("read the YDB feature flags: %w", err)
	}
	return flags.Refine(sessionCapabilities(version)), nil
}

// New constructs an isolated session service.
func New() *Provider { return &Provider{sessions: make(map[string]*session)} }

// minContractMinor requires authored declarations and lifecycle planning.
// Native replacements share physical paths with common objects, so their
// ordering requires the complete sequence under planning.lifecycle.
const minContractMinor = 5

// Description advertises native runtime services, offline coordination models,
// captured row-table creation, column family alteration of existing tables,
// the table-scope YAML keys and Go directives that declare row-table
// partitioning and column families, and offline migration lint with the YD
// rule catalog.
func Description() *protocol.Description {
	return &protocol.Description{
		Revision: provider.Revision, MinContract: &protocol.ContractVersion{Major: provider.Revision, Minor: minContractMinor}, ProviderId: schema.ProviderID,
		Version: "0.0.0-dev", SemanticIdentity: "ydb-table-lifecycle-2",
		Features: providerFeatures(),
		Engines:  []*protocol.Engine{{Id: "ydb", Aliases: []string{"ydbs"}, Identifiers: identifierRules(), ReleaseLines: releaseLines(), ValueModels: append(schema.Models(), tablecreate.Model(), tablecreate.AlterModel(), tablecreate.ColumnModel(), tablecreate.ColumnTTLModel(), tablecreate.DropModel(), tablecreate.CopyModel(), tablecreate.RenameModel(), declarationplan.Model()), Declarations: []*protocol.ModelDeclarations{{Id: declarationplan.ServiceID, Kinds: declarationplan.Kinds(), OperationKinds: []string{declarationplan.OperationKind}}}, Planning: []*protocol.ModelPlanning{{Id: secretplan.ServiceID, ChangeKinds: []string{schema.SecretChange}, OperationKinds: []string{schema.SecretOperation}}, {Id: streamingplan.ServiceID, ChangeKinds: []string{schema.StreamingQueryChange}, OperationKinds: []string{schema.StreamingQueryOperation}}, {Id: coordinationplan.ServiceID, ChangeKinds: []string{schema.CoordinationChange}, OperationKinds: []string{schema.CoordinationOperation}}, {Id: topicplan.ServiceID, ChangeKinds: []string{schema.TopicChange}, OperationKinds: []string{schema.TopicOperation}}, {Id: externalplan.ServiceID, ChangeKinds: externalplan.ChangeKinds(), OperationKinds: externalplan.OperationKinds()}, {Id: workloadplan.ServiceID, ChangeKinds: workloadplan.ChangeKinds(), OperationKinds: []string{schema.WorkloadOperation}}, {Id: tablecreate.ServiceID, ChangeKinds: tablecreate.ChangeKinds(), ParentKinds: tablecreate.ParentKinds(), OperationKinds: []string{tablecreate.OperationKind, tablecreate.AlterOperationKind, tablecreate.ColumnOperationKind, tablecreate.ColumnTTLOperationKind, tablecreate.DropOperationKind, tablecreate.CopyOperationKind, tablecreate.RenameOperationKind, schema.ChangefeedOperation}}}, Comparisons: []*protocol.ModelComparison{{Id: secretcompare.ServiceID, Actions: []string{"rotate"}, Placement: protocol.ModelComparison_OBJECTS, Kinds: []string{schema.SecretProperties}, ChangeKinds: []string{schema.SecretChange}}, {Id: streamingcompare.ServiceID, Placement: protocol.ModelComparison_OBJECTS, Kinds: []string{schema.StreamingQueryProperties}, ChangeKinds: []string{schema.StreamingQueryChange}}, {Id: coordinationcompare.ServiceID, Placement: protocol.ModelComparison_OBJECTS, Kinds: []string{schema.CoordinationProperties}, ChangeKinds: []string{schema.CoordinationChange}}, {Id: topiccompare.ServiceID, Placement: protocol.ModelComparison_OBJECTS, Kinds: []string{schema.TopicProperties}, ChangeKinds: []string{schema.TopicChange}}, {Id: externalcompare.ServiceID, Placement: protocol.ModelComparison_OBJECTS, Kinds: externalcompare.Kinds(), ChangeKinds: externalcompare.ChangeKinds()}, {Id: workloadcompare.PoolServiceID, Placement: protocol.ModelComparison_OBJECTS, Kinds: []string{schema.ResourcePoolProperties}, ChangeKinds: []string{schema.ResourcePoolChange}}, {Id: workloadcompare.ClassifierServiceID, Placement: protocol.ModelComparison_OBJECTS, Kinds: []string{schema.ResourcePoolClassifierProperties}, ChangeKinds: []string{schema.ResourcePoolClassifierChange}}, {Id: tablecompare.ServiceID, Placement: protocol.ModelComparison_FACETS, Kinds: tablecompare.Kinds(), ChangeKinds: tablecompare.ChangeKinds(), OwnerKinds: tablecompare.OwnerKinds()}}, YamlSourceKeys: slices.Concat([]*protocol.YAMLSourceKey{secretsource.YAMLKey(), schema.CoordinationYAMLKey(), streamingsource.YAMLKey(), topicsource.YAMLKey(), changefeedsource.YAMLKey()}, externalsource.YAMLKeys(), tablesource.YAMLKeys(), tablesource.IndexYAMLKeys(), workloadsource.YAMLKeys()), GoSourceDirectives: slices.Concat(streamingsource.GoDirectives(), tablesource.GoDirectives(), changefeedsource.GoDirectives(), []*protocol.GoSourceDirective{secretsource.GoDirective(), schema.CoordinationGoDirective()}, topicsource.GoDirectives(), externalsource.GoDirectives(), workloadsource.GoDirectives()), LintRules: migrationlint.Rules(), QueryDialect: queryDialect(), SqlSchema: sqlSchemaDeclaration(), Features: engineFeatures(), Embeddings: &protocol.EmbeddingEngine{ConsistencyModes: consistencyModes}, CommonObjects: commonobject.Support(), ScopeSelections: scopeSelections(), HclSource: &protocol.HCLSourceDeclaration{Blocks: []*protocol.HCLSourceBlock{schema.CoordinationHCLBlock()}, HeaderKinds: []string{"coordination_node"}}}},
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
	capabilities, err := clusterCapabilities(ctx, opened, version)
	if err != nil {
		_ = opened.Close()
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, status.FromContextError(ctxErr).Err()
		}
		return nil, status.Error(codes.Unavailable, err.Error())
	}
	if err := ctx.Err(); err != nil {
		_ = opened.Close()
		return nil, status.FromContextError(err).Err()
	}
	id := rand.Text()
	p.mu.Lock()
	p.sessions[id] = &session{connection: opened, database: address.Root(), realm: address.Realm() != "", version: version, capabilities: maps.Clone(capabilities)}
	p.mu.Unlock()
	return &protocol.Session{Id: id, Engine: "ydb", Database: address.Root(), ServerVersion: version, Identifiers: identifierRules(), Capabilities: commonCapabilities(ctx, capabilities), Features: negotiatedFeatures(ctx)}, nil
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
		opened.closeStatements("", true)
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

// leftOut names the directories at the session's root that belong to other
// runs. A session on the database leaves the dev realms' directory out of what
// it reads; a session on a realm owns everything below it.
func (s *session) leftOut() []string {
	if s.realm {
		return nil
	}
	return []string{connection.RealmDirectory}
}
