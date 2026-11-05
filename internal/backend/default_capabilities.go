package backend

import (
	"maps"

	"github.com/stokaro/ptah-ydb-plugin/internal/commonobject"
)

// offlineLine is the release line an offline command plans YDB with: the
// newest certified line, with the cluster's default feature flags.
const offlineLine = "26.2"

// defaultCapabilities are the facts the host plans the ydb engine with when no
// session states a server's (contract 25.27, engine.default-capabilities): an
// offline render, generation or export. They are the facts a session states
// for a server of offlineLine whose address names no monitoring endpoint,
// with the facts of the common objects the host carries on the common wire.
// A host that negotiates neither the common wire nor the feature never reads
// them. With hostCapabilities they are the facts of the built-in engine's
// YDB262 preset.
func defaultCapabilities() map[string]bool {
	return lineCapabilities(offlineLine)
}

// lineCapabilities are the facts a session states for a server of line whose
// address names no monitoring endpoint, with the facts of the common objects
// and the facts only the host reads: what the host plans with for a
// --server-version that names the line (ReleaseLine.capabilities, contract
// 25.27).
func lineCapabilities(line string) map[string]bool {
	result := sessionCapabilities(line)
	maps.Copy(result, commonobject.Capabilities())
	maps.Copy(result, hostCapabilities())
	return result
}

// hostCapabilities are facts of a YDB server the host reads through
// capability.ForDialect whenever it plans ydb, connected or offline, and that
// no session states, because this provider decides nothing with them. The
// host registers the engine's offline facts as that answer, so a key left out
// here reads as absent in every plan. index_covering_columns is the one a
// plan shows first: without it every index with covered columns compares as
// changed, and a second apply of an unchanged schema drops the index and
// creates it again. The values are the built-in engine's, which stated each of
// them the same way on every line from 25.1 to 26.2.
func hostCapabilities() map[string]bool {
	return map[string]bool{
		"alter_column_drop_not_null": true, "async_indexes": true, "column_families": true,
		"column_store_tables": true, "index_covering_columns": true, "index_partitioning": true,
		"key_bloom_filter": true, "object_existence_guards": true, "partitioning_options": true,
		"primary_key_required": true, "read_replicas": true, "rename_table": true, "row_deletion_policy": true,
		"row_deletion_policy_epoch_column": true,

		"add_constraint_not_valid": false, "advisory_locks": false, "alter_column_set_not_null": false,
		"alter_column_type": false, "alter_generated_column_expression": false,
		"alter_table_algorithm_lock": false, "backup_collections": false,
		"catalog_check_constraint_table_name": false, "catalog_default_privileges": false,
		"catalog_dependencies": false, "catalog_partitions": false, "catalog_recursive_cte": false,
		"catalog_row_statistics": false, "catalog_trigger_definitions": false, "catalog_vector_info": false,
		"catalog_view_dependencies": false, "check_constraints": false, "check_constraints_enforced": false,
		"check_grant_statement": false, "composite_types": false, "continuous_aggregates": false,
		"create_index_concurrently": false, "create_index_statement": false, "create_or_replace_trigger": false,
		"deferrable_constraints": false, "deferrable_keys": false, "domain_comments": false, "domain_types": false,
		"drop_check_clause": false, "drop_constraint_generic": false, "drop_constraint_if_exists": false,
		"drop_index_concurrently": false, "drop_index_if_exists": false, "enum_custom_type": false,
		"enum_inline_column": false, "expression_defaults": false, "extension_comments": false,
		"foreign_key_delete_column_list": false, "foreign_key_match_full": false,
		"foreign_key_match_partial": false, "foreign_keys": false, "foreign_keys_create_backing_index": false,
		"foreign_keys_require_indexed_reference": false, "foreign_keys_require_unique_reference": false,
		"function_comments": false, "functions": false, "generated_columns": false, "hypertables": false,
		"index_include_spgist": false, "invisible_indexes": false, "migration_lock_timeout": false,
		"named_not_null_constraints": false, "not_enforced_checks": false, "not_enforced_foreign_keys": false,
		"policy_comments": false, "postgres_catalog_functions": false, "primary_key_alterable": false,
		"procedure_comments": false, "procedures": false, "range_types": false, "rename_column_clause": false,
		"routine_replacement_resets_definer": false, "row_level_security": false, "row_level_ttl": false,
		"sequence_start_counter_only": false, "sequences": false, "show_routine_privilege": false,
		"trigger_comments": false, "triggers": false, "type_comments": false, "unique_constraints": false,
		"unique_nulls_distinct_clause": false, "xml_type": false,
	}
}
