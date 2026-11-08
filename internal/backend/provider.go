// Package backend implements the YDB provider's experimental services.
package backend

import (
	"context"
	"crypto/rand"
	"errors"
	"maps"
	"net/url"
	"slices"
	"sync"

	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/stokaro/ptah/provider-sdk/provider"
	"github.com/stokaro/ptah/provider-sdk/runtime"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	"github.com/stokaro/ptah-ydb-plugin/internal/authored"
	"github.com/stokaro/ptah-ydb-plugin/internal/buildversion"
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
	"github.com/stokaro/ptah-ydb-plugin/internal/nativeapi"
	"github.com/stokaro/ptah-ydb-plugin/internal/releaseline"
	"github.com/stokaro/ptah-ydb-plugin/internal/replication"
	"github.com/stokaro/ptah-ydb-plugin/internal/replicationcompare"
	"github.com/stokaro/ptah-ydb-plugin/internal/replicationplan"
	"github.com/stokaro/ptah-ydb-plugin/internal/replicationsource"
	"github.com/stokaro/ptah-ydb-plugin/internal/reversal"
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
	"github.com/stokaro/ptah-ydb-plugin/internal/ydbsequence"
	"github.com/stokaro/ptah-ydb-plugin/internal/ydbtype"
	"github.com/stokaro/ptah-ydb-plugin/internal/yqlschema"
	"github.com/stokaro/ptah-ydb-plugin/schema"
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
	realm        bool
	closed       bool
	closeErr     error
	cleanupErr   error
	transactions map[string]*transaction
	active       string
	// lockMu guards locks, the fields of each lock record, fenced and
	// locksClosed. A lock check takes it and not mu, so the host's watch of
	// a lock is answered while an execution holds mu: the watch checks
	// every second and counts a check unanswered for ten seconds as a lost
	// lock. A caller that holds both took mu first.
	lockMu           sync.Mutex
	locks            map[string]*lockRecord
	fenced           bool
	locksClosed      bool
	metadataReaders  map[string]*metadataReader
	statementResults map[string]*statementResult
	// version is what the server answered SELECT Version() with.
	version string
	// embeddings is the session's embedding store, made on its first
	// embedding call.
	embeddings *embedstore.Store
	// capabilities are the capabilities the session reported.
	capabilities map[string]bool
	// creations lends one table session to the table creations of the
	// execution that runs, and is nil between executions.
	creations *nativeapi.CreationSessions
}

// providerFeatures are the services and behaviors the provider offers.
func providerFeatures() []string {
	return []string{provider.DatabaseContextFeature, provider.ValueContextFeature, provider.ColumnsFeature, provider.ObservedColumnsFeature, provider.ObservedIndexesFeature, provider.SensitiveInputsFeature, provider.SchemaDeclarationsFeature,
		// A read reports each table's primary key in key order, and the
		// planner of a row table plans the change of it the host hands over
		// as an alteration of the table; see inspect.Common.PrimaryKeys and
		// tablecreate.rebuildNeeds.
		provider.ObservedPrimaryKeysFeature, provider.PrimaryKeyChangesFeature,
		provider.IndexMethodsFeature,
		// A read with no selector claims every directory below the root
		// complete in one region, and each directory it leaves out unknown,
		// so an object declared in a new directory is planned; see
		// inspect.subtreeRegions.
		provider.SchemaSubtreeCoverageFeature,
		// A refusal reaches the host with the reason the provider wrote, and a
		// driver's error with its code alone; see package authored.
		provider.ErrorReasonsFeature,
		// A model of the description travels by reference in every other
		// message, so the coverage the host sends for each table names the
		// models it covers rather than repeating their definitions, and a
		// message may be 64 MiB: a database of a few thousand tables is read,
		// compared and planned in one request. The SDK sends and resolves the
		// references.
		provider.ModelReferencesFeature,
		provider.LargeMessagesFeature,
		// A planning service that plans no table's settings reads each table
		// of a request by its subject, its action and the names of its
		// states, and the host sends it no more; see the services that read
		// outlines in Description. The coverage kinds every table of a
		// request repeats travel once per message; the SDK shares and
		// resolves them.
		provider.TableOutlinesFeature,
		provider.SharedCoverageFeature,
		provider.ScopeSelectionsFeature,
		provider.StatementsFeature,
		provider.TypedArgumentsFeature,
		provider.ValidateSchemaFeature,
		provider.ProjectTableCreationsFeature,
		provider.PrepareTablesFeature,
		provider.PlanFeaturesFeature,
		provider.RenderFeature,
		provider.CompareModelsFeature,
		// An object comparison counts what it compares, so a drift inventory
		// reports the provider's native kinds; see objectcompare.countDecided.
		// The row-table facet comparison counts nothing: a table's settings
		// stand for no object, and a table's changefeeds change as one set.
		provider.ComparisonCountsFeature,
		// Export warnings count the changefeeds held by one table's set.
		provider.OmissionCountsFeature,
		provider.DecodeSourceFeature,
		// A table's facets, a topic with its consumers and a changefeed set
		// are each written by several declarations; see EncodeSource.
		provider.EncodeGroupsFeature,
		provider.InspectFeature,
		provider.NormalizeValuesFeature,
		provider.ConvertValuesFeature,
		provider.ExecuteFeature,
		provider.TransactionsFeature,
		provider.HistoryFeature,
		provider.LocksFeature,
		provider.MetadataFeature,
		provider.AnalyzeSQLFeature,
		// The host reads YQL text by the provider's scanner; see ScanSQL.
		provider.ScanSQLFeature,
		// An offline command plans with the facts of the newest line; see
		// defaultCapabilities.
		provider.DefaultCapabilitiesFeature,
		// ptah db capabilities names the monitoring endpoint as the source of
		// a capability the cluster's feature flags refine; see
		// connectionCapabilitySource.
		provider.ConnectionCapabilitySourceFeature,
		provider.DevDatabaseFeature,
		provider.LintFeature,
		provider.TypeRowsFeature,
		provider.ReadRowsFeature,
		provider.RenderRowWritesFeature,
		provider.SeedsFeature,
		provider.DevCleanupFeature,
		provider.ReplayGuardFeature,
		provider.StatementTimeoutFeature,
		provider.EmbeddingStoreFeature,
		provider.ReadSQLSchemaFeature,
		provider.TextUnitsFeature,
		provider.PlanningPolicyFeature,
		// The exact statement style asks for no comment that only describes
		// a statement, no IF EXISTS or IF NOT EXISTS guard the plan does not
		// need and no CASCADE. The operations this provider plans carry none
		// of them in either style, so it honors the exact style by planning
		// as it always does. ptah-compat's migrate diff plans in that style.
		provider.PlanningStatementStyleFeature,
		provider.CommonObjectsFeature,
		provider.CommonAccessFeature,
		// A user's password arrives as an execution input the statement names
		// and the execution binds; see commonobject.passwordClause and
		// secretvalue.Expand.
		provider.PasswordInputsFeature,
		provider.PlanningLifecycleFeature,
		provider.PlanningObjectContextFeature,
		provider.PlanningContextInventoryFeature,
		provider.EntryKeysFeature,
		provider.HCLSourceFeature,
		// A table's TTL is read from and written to its platform "ydb"
		// block in HCL; see tablesource.HCLPlatformBlock.
		provider.HCLPlatformFeature,
		provider.EncodeSourceFeature,
		// A Go or HCL source leaves a native kind unmanaged with a
		// not-described entry; see EncodeSource and DecodeSource.
		provider.SourceLimitsFeature,
		provider.MigrationSplitFeature,
		// The host sends an index it pairs with one declared under another
		// name as one rename step, which the table service claims with
		// RENAME INDEX; see indexRenames.
		provider.IndexRenamesFeature,
		// A read reports a global index's partitioning and read replicas as
		// the index's own facet, which a source's index directive or entry
		// declares; see inspect.Common.IndexFacets.
		provider.ObservedIndexFacetsFeature,
		// A YQL reading carries a table's changefeeds as a facet of the
		// table; see yqlschema.Reply.
		provider.SQLFacetsFeature,
		// The reverse migration of a standalone object's change is the
		// change that undoes it; see ReverseChanges.
		provider.ReverseChangesFeature,
		// Resource pools and their classifiers belong to the database, so a
		// read of directories reports them beside its scope; see
		// inspect.Workload.
		provider.DatabaseKindsFeature,
		// A read names each replica table no replication it describes
		// writes, which YDB keeps read-only, so the host refuses a table
		// declared there at planning; see inspect.unreadReplicas.
		provider.UnreadTablesFeature,
		// `ptah schema stats` counts topics, changefeeds, the other native
		// objects and the table settings with the provider's counts; see
		// ReportValues.
		provider.ReportCountsFeature,
		// A vector, full-text or local index declaration reaches the provider
		// whole, which reads it into the table's native index set or its
		// column storage and writes it back; see tablesource.IndexKinds.
		provider.IndexKindsFeature,
		// The note that counts the roles a read leaves out of the
		// description gives the reason this read does; see
		// inspect.RolesOutOfScopeReason.
		provider.RoleScopeReasonsFeature,
		// A Go export writes resource pools, their classifiers and
		// coordination nodes after the default privileges, where the host's
		// embedded engine wrote them; see goGroups.
		provider.GoPlacementsFeature,
		// A file Ptah's built-in YDB engine exported names that engine's
		// coordination-node model in its captured header, and the host reads
		// it as this provider's; see modelPredecessors.
		provider.ModelPredecessorsFeature,
		// A file an earlier release of this provider exported names a model
		// whose definition text has changed since by the identity it had, and
		// the host reads it as the same claim once modelPredecessors names
		// that identity; see TestModelIdentitiesAreDescribedOrDeclaredPredecessors.
		provider.OwnPredecessorsFeature,
		// A comparison reads every table the desired schema declares among
		// its declared relations, so a transfer into a table the schema does
		// not declare is refused; see replicationcompare.CompareTransfers.
		provider.DeclaredRelationsFeature,
		// YDB has no enum type, and an enum column is refused before any
		// call, as the host's embedded engine refused it (EnumColumns).
		provider.EnumColumnsFeature,
		// The transfer comparison reads the topics the schema declares, so a
		// transfer of a topic it does not declare is refused; see
		// replicationcompare.CompareTransfers.
		provider.DeclaredObjectsFeature,
		// The transfer comparison reads the changefeeds the schema's tables
		// declare, so a transfer of a stream of a declared table that is none
		// of its declared changefeeds is refused; see
		// replicationcompare.CompareTransfers.
		provider.DeclaredFacetsFeature,
		// A render reports what its units go on without: the part of a
		// created column's declared type YDB does not keep, and a Serial's
		// start and increment an offline creation cannot set; see
		// renderOmissions.
		provider.RenderOmissionsFeature,
		// A rebuild written into a migration file gives the new table the
		// grants the read found on the old one; see tablecreate.regrants.
		provider.TableGrantsFeature,
		// A YQL reading returns the password a document gives a user; see
		// yqlschema.Reply.
		provider.SQLRolePasswordsFeature,
		// A YQL reading reports a statement that changes an object no
		// document of the load declares as UNDECLARED, which the host reads
		// as its own reader's refusal; see yqlschema.diagnosticCode.
		provider.SQLUndeclaredFeature,
		// A view query is compared in the form YDB stores it, keeping case,
		// in place of the host's reading, which folds it; see
		// CompareViewQueries.
		provider.ViewQueryEquivalenceFeature,
	}
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

// connectionCapabilitySource is where the connection reads the capabilities
// that differ from a release line's: the cluster's feature flags, which the
// provider reads at the monitoring endpoint a ydb:// URL names, and only then
// (contract 25.35). ptah db capabilities prints it beneath them.
const connectionCapabilitySource = "read from this cluster's feature flags at its monitoring endpoint, not from its release line"

// negotiatedFeatures is the engine's feature list as the host holds it: the
// two-way features the host did not list are left out, as the SDK leaves them
// out of the description the host reads. A session and an inspection snapshot
// report it, so the host can check them against that description.
func negotiatedFeatures(ctx context.Context) []string {
	return provider.NegotiatedDescription(described(), provider.HostFeatures(ctx)).GetEngines()[0].GetFeatures()
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
// ddl_inside_transaction is false on every line: YDB refuses a schema
// statement inside an explicit transaction, so the host runs a schema apply's
// statements without one rather than asking for a transaction YDB cannot give.
//
// external_data_sources and external_object_replace are false, because
// their flags are off by default on every line; the cluster's flags replace
// them where the address names the monitoring endpoint.
// vector_indexes, vector_index_maintained_on_write, vector_bit_type and
// full_text_indexes are the host's embedded engine's line presets: a vector
// index from 25.2 (25.1 keeps it behind EnableVectorIndex, which the
// monitoring endpoint's flags decide), one that includes the rows written
// after its build from 25.3, one over bit vectors that builds over rows from
// 26.1, and a full-text index from 26.2 (EnableFulltextIndex, off by default
// before 26.2, which the flags decide too).
//
// local_bloom_indexes, local_ngram_indexes and local_min_max_indexes are the
// local indexes of a column table, as the host's line presets name them: 26.2
// builds bloom_filter and bloom_ngram_filter indexes with its default flags
// (EnableLocalBloomFilterIndex and EnableLocalBloomNgramFilterIndex), keeps
// min_max behind EnableLocalMinMaxIndex, off by default, and the lines before
// it have no LOCAL index grammar (`local: alternative is not implemented yet`
// on 25.1.4.7). The flags decide the three where the address names the
// monitoring endpoint.
//
// external_data_source_secret_paths is an option ending in _SECRET_PATH,
// which 25.4 and later take and 25.1 to 25.3 read as a missing name
// (`PASSWORD_SECRET_NAME requires key`).
//
// unique_constraints is false on every line: YDB refuses a UNIQUE constraint
// as a parse error and holds the same rows with a global unique index. The
// host then reads a declared UNIQUE as that index, which this service creates
// with the table and the inspection reports.
// unique_index_on_existing_table is false on every line: 25.1 and 25.2 add
// no unique index to a table that exists, and later lines keep it behind
// EnableAddUniqueIndex, off by default, which the flags decide where the
// address names the monitoring endpoint.
// index_rename is true on every line: YDB renames an index in place with
// EnableMoveIndex on, its default from 25.1.4.7 to 26.2.1.14, and the flag
// decides it where the address names the monitoring endpoint. A host that
// does not list planning.index-renames sees it false (see indexRenames).
//
// async_replication is true on every line, as the host measured: CREATE
// ASYNC REPLICATION runs on 25.1 to 26.2 with default flags, and 26.2 lists
// EnableReplication, on by default. transfers is EnableTopicTransfer, off by
// default on 25.1 (`Topic transfer creation is disabled`) and on from 25.2.
// replication_secret_paths is TOKEN_SECRET_PATH and PASSWORD_SECRET_PATH,
// which 25.4 and later take (`Unknown replication setting:
// TOKEN_SECRET_PATH` on 25.1 to 25.3). Reading either object also needs the
// replication API among the cluster's gRPC services, which no flag shows;
// inspection reports an object it cannot describe as uninspected.
//
// returning_clause, common_table_expressions, correlated_subqueries,
// non_equi_joins and offset_without_limit are the keys the host's query
// builder reads for a session, at the host's line presets: RETURNING on
// INSERT, UPDATE and DELETE from 25.3, since 25.1 and 25.2 fail `UPDATE ...
// RETURNING` on a table with a unique index with `INTERNAL_ERROR ... wrong
// returning expr type`, and none of the others on any line. The builder reads
// a key the session leaves out as unknown and refuses the clause it gates,
// on every line alike.
//
// column_family_cache_mode is a column family's CACHE_MODE, which 25.4 and
// later take with default flags; 25.3 keeps it behind EnableTableCacheModes,
// off by default (`Setting cache_mode is not allowed`), and the lines before
// it do not know it (`Unknown table setting: CACHE_MODE`), as the host
// measured. The flag decides it where the address names the monitoring
// endpoint.
func sessionCapabilities(version string) map[string]bool {
	return map[string]bool{
		"wide_date_time_types":                           releaseline.AtLeast(version, 25, 2),
		"parameterized_decimal":                          releaseline.AtLeast(version, 25, 2),
		"alter_column_default":                           releaseline.AtLeast(version, 26, 2),
		"add_column_with_default":                        releaseline.AtLeast(version, 26, 1),
		"small_integer_defaults":                         releaseline.AtLeast(version, 25, 2),
		"document_type_defaults":                         releaseline.AtLeast(version, 25, 3),
		"transactional_ddl":                              false,
		"ddl_inside_transaction":                         false,
		"secrets":                                        releaseline.AtLeast(version, 25, 4),
		"coordination_nodes":                             true,
		"migration_statement_timeout":                    true,
		"tiered_ttl":                                     false,
		"role_management":                                true,
		"role_membership":                                true,
		"group_principals":                               true,
		"database_grants":                                true,
		"relative_grant_paths":                           releaseline.AtLeast(version, 26, 1),
		"topics":                                         true,
		"topic_consumer_availability_period":             releaseline.AtLeast(version, 25, 4),
		workload.CapabilityResourcePools:                 false,
		streaming.Capability:                             false,
		changefeeds.CapabilityChangefeeds:                true,
		changefeeds.CapabilityUserSIDs:                   releaseline.AtLeast(version, 26, 1),
		changefeeds.CapabilitySchemaChanges:              releaseline.AtLeast(version, 25, 3),
		changefeeds.CapabilityTopicAutoPartitioning:      releaseline.AtLeast(version, 25, 2),
		"external_data_sources":                          false,
		"external_object_replace":                        false,
		"external_data_source_secret_paths":              releaseline.AtLeast(version, 25, 4),
		tablecreate.VectorIndexesCapability:              releaseline.AtLeast(version, 25, 2),
		tablecreate.VectorMaintainedOnWriteCapability:    releaseline.AtLeast(version, 25, 3),
		tablecreate.VectorBitTypeCapability:              releaseline.AtLeast(version, 26, 1),
		tablecreate.FullTextIndexesCapability:            releaseline.AtLeast(version, 26, 2),
		tablecreate.LocalBloomIndexesCapability:          releaseline.AtLeast(version, 26, 2),
		tablecreate.LocalNgramIndexesCapability:          releaseline.AtLeast(version, 26, 2),
		tablecreate.LocalMinMaxIndexesCapability:         false,
		replication.CapabilityReplication:                true,
		replication.CapabilityTransfers:                  releaseline.AtLeast(version, 25, 2),
		replication.CapabilitySecretPaths:                releaseline.AtLeast(version, 25, 4),
		ydbtype.SerialColumns:                            true,
		ydbsequence.Options:                              true,
		ydbsequence.KeepsRange:                           false,
		"unique_constraints":                             false,
		tablecreate.UniqueIndexOnExistingTableCapability: false,
		tablecreate.IndexRenameCapability:                true,
		"returning_clause":                               releaseline.AtLeast(version, 25, 3),
		"common_table_expressions":                       false,
		"correlated_subqueries":                          false,
		"non_equi_joins":                                 false,
		"offset_without_limit":                           false,
		tablecreate.CacheModeCapability:                  releaseline.AtLeast(version, 25, 4),
	}
}

// indexRenames returns capabilities with index_rename true only for a host
// that lists planning.index-renames. The host pairs renamed indexes under the
// capability, and a host that does not list the feature has no step to send a
// pair with: it would refuse every plan that renames an index. With the
// capability false the host plans a rename as a drop and an add.
func indexRenames(ctx context.Context, capabilities map[string]bool) map[string]bool {
	result := maps.Clone(capabilities)
	result[tablecreate.IndexRenameCapability] = result[tablecreate.IndexRenameCapability] && slices.Contains(provider.HostFeatures(ctx), provider.IndexRenamesFeature)
	return result
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
		// Decode writes its refusals itself and names a flag or the
		// database at most, so the refusal is a reason.
		return nil, authored.Wrap(err, "read the YDB feature flags: "+err.Error())
	}
	return flags.Refine(sessionCapabilities(version)), nil
}

// New constructs an isolated session service.
func New() *Provider { return &Provider{sessions: make(map[string]*session)} }

// minContractMinor requires durable database context, dependency readiness, and
// assessment of unchanged native objects.
// Native replacements share physical paths with common objects, so their
// ordering requires the complete sequence under planning.lifecycle.
const minContractMinor = 7

// Description advertises native runtime services, offline coordination models,
// captured row-table creation, column family alteration of existing tables,
// the table-scope YAML keys and Go directives that declare row-table
// partitioning and column families, and offline migration lint with the YD
// rule catalog. Each call returns a copy the caller may change.
func Description() *protocol.Description {
	return proto.CloneOf(described())
}

// described is the description the provider checks its own requests and
// replies against. It is built once and must not be changed. A request is
// checked against it several times, and an execution checks each of its units
// against it. Built for each check, at about a millisecond and a half of CPU
// each, it cost a second and a half in a migration of 500 new tables.
var described = sync.OnceValue(buildDescription)

func buildDescription() *protocol.Description {
	description := &protocol.Description{
		Revision: provider.Revision, MinContract: &protocol.ContractVersion{Major: provider.Revision, Minor: minContractMinor}, ProviderId: schema.ProviderID,
		Version: buildversion.Version(), SemanticIdentity: "ydb-table-lifecycle-2",
		Features: providerFeatures(),
		Engines: []*protocol.Engine{
			{
				Id:           "ydb",
				Aliases:      []string{"ydbs"},
				Identifiers:  identifierRules(),
				ReleaseLines: releaseLines(),
				ValueModels: append(
					schema.Models(),
					tablecreate.Model(),
					tablecreate.AlterModel(),
					tablecreate.IndexSettingsModel(),
					tablecreate.DefaultModel(),
					tablecreate.NullabilityModel(),
					tablecreate.InventoryModel(),
					tablecreate.ColumnModel(),
					tablecreate.ColumnTTLModel(),
					tablecreate.ColumnLocalIndexModel(),
					tablecreate.DropModel(),
					tablecreate.CopyModel(),
					tablecreate.RenameModel(),
					tablecreate.NativeIndexModel(),
					declarationplan.Model(),
				),
				ModelPredecessors: modelPredecessors(),
				Declarations: []*protocol.ModelDeclarations{
					{
						Id:             declarationplan.ServiceID,
						Kinds:          declarationplan.Kinds(),
						OperationKinds: []string{declarationplan.OperationKind},
					},
				},
				// A service reads the outline of each table, by its subject,
				// its action and the names of its states, unless it reads
				// more: the planner of external data sources reads the TTL a
				// column table keeps, and the planner of row tables plans
				// their settings.
				Planning: []*protocol.ModelPlanning{
					{
						Id:             secretplan.ServiceID,
						Tables:         protocol.PlanningTables_PLANNING_TABLES_OUTLINED,
						ChangeKinds:    []string{schema.SecretChange},
						OperationKinds: []string{schema.SecretOperation},
					},
					{
						Id:             streamingplan.ServiceID,
						Tables:         protocol.PlanningTables_PLANNING_TABLES_OUTLINED,
						ContextKinds:   []string{schema.StreamingQueryProperties},
						ChangeKinds:    []string{schema.StreamingQueryChange},
						OperationKinds: []string{schema.StreamingQueryOperation},
					},
					{
						Id:             coordinationplan.ServiceID,
						Tables:         protocol.PlanningTables_PLANNING_TABLES_OUTLINED,
						ChangeKinds:    []string{schema.CoordinationChange},
						OperationKinds: []string{schema.CoordinationOperation},
					},
					{
						Id:             topicplan.ServiceID,
						Tables:         protocol.PlanningTables_PLANNING_TABLES_OUTLINED,
						ChangeKinds:    []string{schema.TopicChange},
						OperationKinds: []string{schema.TopicOperation},
					},
					{
						Id:             externalplan.ServiceID,
						ChangeKinds:    externalplan.ChangeKinds(),
						OperationKinds: externalplan.OperationKinds(),
					},
					{
						Id:             workloadplan.ServiceID,
						Tables:         protocol.PlanningTables_PLANNING_TABLES_OUTLINED,
						ChangeKinds:    workloadplan.ChangeKinds(),
						OperationKinds: []string{schema.WorkloadOperation},
					},
					{
						Id:             replicationplan.ReplicationServiceID,
						Tables:         protocol.PlanningTables_PLANNING_TABLES_OUTLINED,
						ContextKinds:   []string{schema.ReplicationProperties},
						ChangeKinds:    []string{schema.ReplicationChange},
						OperationKinds: []string{schema.ReplicationOperation},
					},
					{
						Id:             replicationplan.TransferServiceID,
						Tables:         protocol.PlanningTables_PLANNING_TABLES_OUTLINED,
						ChangeKinds:    []string{schema.TransferChange},
						OperationKinds: []string{schema.TransferOperation},
					},
					{
						Id:          tablecreate.ServiceID,
						ChangeKinds: tablecreate.ChangeKinds(),
						ParentKinds: tablecreate.ParentKinds(),
						OperationKinds: []string{
							tablecreate.OperationKind,
							tablecreate.AlterOperationKind,
							tablecreate.IndexSettingsOperationKind,
							tablecreate.DefaultOperationKind,
							tablecreate.NullabilityOperationKind,
							tablecreate.InventoryOperationKind,
							tablecreate.ColumnOperationKind,
							tablecreate.ColumnTTLOperationKind,
							tablecreate.ColumnLocalIndexOperationKind,
							tablecreate.DropOperationKind,
							tablecreate.CopyOperationKind,
							tablecreate.RenameOperationKind,
							tablecreate.NativeIndexOperationKind,
							schema.ChangefeedOperation,
						},
					},
				},
				Reversals: []*protocol.ModelReversal{{Id: reversal.ServiceID, ChangeKinds: reversal.ChangeKinds()}},
				Comparisons: []*protocol.ModelComparison{
					{
						Id:          secretcompare.ServiceID,
						Actions:     []string{"rotate"},
						Placement:   protocol.ModelComparison_OBJECTS,
						Kinds:       []string{schema.SecretProperties},
						ChangeKinds: []string{schema.SecretChange},
					},
					{
						Id:          streamingcompare.ServiceID,
						Placement:   protocol.ModelComparison_OBJECTS,
						Kinds:       []string{schema.StreamingQueryProperties},
						ChangeKinds: []string{schema.StreamingQueryChange},
					},
					{
						Id:          coordinationcompare.ServiceID,
						Placement:   protocol.ModelComparison_OBJECTS,
						Kinds:       []string{schema.CoordinationProperties},
						ChangeKinds: []string{schema.CoordinationChange},
					},
					{
						Id:          topiccompare.ServiceID,
						Placement:   protocol.ModelComparison_OBJECTS,
						Kinds:       []string{schema.TopicProperties},
						ChangeKinds: []string{schema.TopicChange},
					},
					{
						Id:          externalcompare.ServiceID,
						Placement:   protocol.ModelComparison_OBJECTS,
						Kinds:       externalcompare.Kinds(),
						ChangeKinds: externalcompare.ChangeKinds(),
					},
					{
						Id:          workloadcompare.PoolServiceID,
						Placement:   protocol.ModelComparison_OBJECTS,
						Kinds:       []string{schema.ResourcePoolProperties},
						ChangeKinds: []string{schema.ResourcePoolChange},
					},
					{
						Id:          workloadcompare.ClassifierServiceID,
						Placement:   protocol.ModelComparison_OBJECTS,
						Kinds:       []string{schema.ResourcePoolClassifierProperties},
						ChangeKinds: []string{schema.ResourcePoolClassifierChange},
					},
					{
						Id:          replicationcompare.ReplicationServiceID,
						Placement:   protocol.ModelComparison_OBJECTS,
						Kinds:       []string{schema.ReplicationProperties},
						ChangeKinds: []string{schema.ReplicationChange},
					},
					{
						Id:          replicationcompare.TransferServiceID,
						Placement:   protocol.ModelComparison_OBJECTS,
						Kinds:       []string{schema.TransferProperties},
						ChangeKinds: []string{schema.TransferChange},
						// The topics the schema declares, and the changefeeds of
						// its tables, so a transfer of a topic or a changefeed
						// it does not declare is refused; see
						// replicationcompare.CompareTransfers.
						DeclaredKinds:      replicationcompare.TransferDeclaredKinds(),
						DeclaredFacetKinds: replicationcompare.TransferDeclaredFacetKinds(),
					},
					{
						Id:          tablecompare.ServiceID,
						Placement:   protocol.ModelComparison_FACETS,
						Kinds:       tablecompare.Kinds(),
						ChangeKinds: tablecompare.ChangeKinds(),
						OwnerKinds:  tablecompare.OwnerKinds(),
					},
				},
				YamlSourceKeys: slices.Concat(
					[]*protocol.YAMLSourceKey{
						secretsource.YAMLKey(),
						schema.CoordinationYAMLKey(),
						streamingsource.YAMLKey(),
						topicsource.YAMLKey(),
						changefeedsource.YAMLKey(),
					},
					externalsource.YAMLKeys(),
					replicationsource.YAMLKeys(),
					tablesource.YAMLKeys(),
					tablesource.IndexYAMLKeys(),
					workloadsource.YAMLKeys(),
				),
				GoSourceDirectives: slices.Concat(
					streamingsource.GoDirectives(),
					tablesource.GoDirectives(),
					changefeedsource.GoDirectives(),
					[]*protocol.GoSourceDirective{secretsource.GoDirective(), schema.CoordinationGoDirective()},
					topicsource.GoDirectives(),
					replicationsource.GoDirectives(),
					externalsource.GoDirectives(),
					workloadsource.GoDirectives(),
				),
				LintRules:                  migrationlint.Rules(),
				QueryDialect:               queryDialect(),
				DefaultCapabilities:        defaultCapabilities(),
				ConnectionCapabilitySource: connectionCapabilitySource,
				SqlSchema:                  sqlSchemaDeclaration(),
				Features:                   engineFeatures(),
				Embeddings:                 &protocol.EmbeddingEngine{ConsistencyModes: consistencyModes},
				CommonObjects:              commonobject.Support(),
				ScopeSelections:            scopeSelections(),
				DatabaseKinds:              databaseKinds(),
				HclSource: &protocol.HCLSourceDeclaration{
					Blocks:      []*protocol.HCLSourceBlock{schema.CoordinationHCLBlock(), tablesource.HCLIndexBlock(), tablesource.HCLPlatformBlock()},
					HeaderKinds: []string{"coordination_node"},
				},
				GoHeaderKinds: yqlschema.HeaderKinds(),
				Reports:       schema.Reports(),
				IndexKinds:    tablesource.IndexKinds(),
				EnumColumns:   &protocol.EnumColumns{Form: protocol.EnumColumns_REFUSED},
			},
		},
	}
	withSerialSequences(description.Engines[0])
	return description
}

// databaseKinds are the kinds whose objects belong to the database rather
// than to a directory of it: resource pools and their classifiers, which
// YDB names by name alone.
func databaseKinds() []string {
	return []string{schema.ResourcePoolProperties, schema.ResourcePoolClassifierProperties}
}

// withSerialSequences adds the change of a Serial column's sequence to the
// engine: its operation model, and its operation kind among those the table
// planning service writes.
func withSerialSequences(engine *protocol.Engine) {
	engine.ValueModels = append(engine.ValueModels, tablecreate.SequenceModel())
	for _, planning := range engine.Planning {
		if planning.Id == tablecreate.ServiceID {
			planning.OperationKinds = append(planning.OperationKinds, tablecreate.SequenceOperationKind)
		}
	}
}

// identifierRules are the engine's identifier rules. A directory path is one
// schema, so its components nest under /, and a coverage region can cover a
// directory and everything below it (schema.subtree-coverage).
func identifierRules() *protocol.IdentifierRules {
	return &protocol.IdentifierRules{
		TableNames: protocol.IdentifierRules_EXACT, ColumnNames: protocol.IdentifierRules_EXACT,
		IndexNames: protocol.IdentifierRules_EXACT, IndexNamespace: protocol.IdentifierRules_TABLE,
		SchemaSeparator: schema.SchemaSeparator,
	}
}

// negotiatedRules are the identifier rules as the host holds them: without
// the schema separator for a host that does not list
// schema.subtree-coverage, as the SDK leaves it out of the description that
// host reads. A request a host sends carries these rules back.
func negotiatedRules(ctx context.Context) *protocol.IdentifierRules {
	rules := identifierRules()
	if !slices.Contains(provider.HostFeatures(ctx), provider.SchemaSubtreeCoverageFeature) {
		rules.SchemaSeparator = ""
	}
	return rules
}

// rulesMatch reports whether rules a host sent are the engine's. The
// separator may be absent: a host that does not list schema.subtree-coverage
// sends none, and a host that does may still build a request's rules from an
// identity that does not carry it. It decides only subtree regions, which the
// SDK refuses in a comparison whose rules carry no separator, so an absent
// separator is accepted and a different one is not.
func rulesMatch(rules *protocol.IdentifierRules) bool {
	sent, want := proto.CloneOf(rules), identifierRules()
	if sent == nil {
		return false
	}
	if sent.SchemaSeparator == "" {
		want.SchemaSeparator = ""
	}
	return proto.Equal(sent, want)
}

// Open verifies a native connection and server version before publishing its
// handle. The address and credentials never appear in receipts or errors.
func (p *Provider) Open(ctx context.Context, request *protocol.OpenRequest) (*protocol.Session, error) {
	if err := ctx.Err(); err != nil {
		return nil, status.FromContextError(err).Err()
	}
	if request.GetEngine() != "ydb" {
		return nil, runtime.ErrorWithReason(codes.InvalidArgument, "unsupported engine")
	}
	address, err := connection.Parse(request.GetAddress())
	if err != nil {
		return nil, authored.Status(codes.InvalidArgument, err)
	}
	p.mu.Lock()
	if len(p.sessions)+p.opening >= 1024 {
		p.mu.Unlock()
		return nil, runtime.ErrorWithReason(codes.ResourceExhausted, "YDB session handle limit reached; restart the provider")
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
		return nil, authored.Status(codes.Unavailable, err)
	}
	capabilities = indexRenames(ctx, capabilities)
	if err := ctx.Err(); err != nil {
		_ = opened.Close()
		return nil, status.FromContextError(err).Err()
	}
	id := rand.Text()
	p.mu.Lock()
	p.sessions[id] = &session{connection: opened, database: address.Root(), realm: address.Realm() != "", version: version, capabilities: maps.Clone(capabilities)}
	p.mu.Unlock()
	result := &protocol.Session{Id: id, Engine: "ydb", Database: address.Root(), ServerVersion: version, Identifiers: negotiatedRules(ctx), Capabilities: commonCapabilities(ctx, capabilities), Features: negotiatedFeatures(ctx)}
	if slices.Contains(result.Features, provider.DatabaseContextFeature) {
		result.Context = databaseContext(address)
	}
	return result, nil
}

// Close releases only the named session. Repeated closes return the original
// cleanup result; a canceled caller still triggers bounded cleanup.
func (p *Provider) Close(_ context.Context, request *protocol.CloseRequest) error {
	p.mu.Lock()
	opened := p.sessions[request.GetSessionId()]
	p.mu.Unlock()
	if opened == nil {
		return runtime.ErrorWithReason(codes.NotFound, "unknown YDB session")
	}
	opened.mu.Lock()
	defer opened.mu.Unlock()
	if !opened.closed {
		opened.closed = true
		opened.closeStatements()
		opened.closeMetadataReaders()
		opened.rollbackTransactions()
		opened.closeLocks()
		opened.closeErr = errors.Join(opened.cleanupErr, opened.connection.Close())
	}
	if opened.closeErr != nil {
		return runtime.ErrorWithReason(codes.Unavailable, "close YDB connection failed")
	}
	return nil
}

func connectionError(ctx context.Context, operation string) error {
	if err := ctx.Err(); err != nil {
		return status.FromContextError(err).Err()
	}
	// Native authentication errors may embed credential-source details. Until
	// structured redaction is available, return the operation without raw text.
	return runtime.ErrorWithReason(codes.Unavailable, operation+" failed")
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
