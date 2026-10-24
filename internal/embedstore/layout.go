// Package embedstore is the YDB provider's embedding store (ADR 0032): the
// run state of `ptah inference` in row tables of the provider's layout, and a
// generation's storage, rows, vectors, index and searches.
//
// The host keeps the lifecycle; this package stores what the host decided and
// refuses nothing for a lifecycle reason. It keeps the layout of the Ptah
// host's embedded YDB engine (internal/embedydb): the same four
// ptah_embedding_* tables with the same columns, keys and nullability, the
// five columns a generation adds beside its vector or the table of its own a
// chunked generation keys by the source key and the chunk ordinal, the vector
// as the bytes Knn::ToBinaryStringFloat produces, and the vector_kmeans_tree
// index. So a database the embedded engine wrote reads and runs on through
// this provider, and the other way round.
//
// Every compare-and-write is one serializable YDB transaction that reads what
// it compares, compares, writes and commits. YDB aborts a transaction whose
// reads another transaction changed; the operation then runs again, a bounded
// number of times, on what is there now. A commit that fails any other way is
// UNKNOWN: nothing proves it did not commit.
package embedstore

import (
	"fmt"
	"slices"
	"strings"
)

// The run-state tables, the Ptah host's names (embedstore.TablePrefix).
const (
	GenerationTable = "ptah_embedding_generation"
	RunTable        = "ptah_embedding_run"
	EventTable      = "ptah_embedding_event"
	PointerTable    = "ptah_embedding_pointer"
)

// kind is the kind of a run-state column: the host's descriptor types TEXT,
// INTEGER, BIGINT, BOOLEAN and TIMESTAMPTZ, which YDB stores as Utf8, Int32,
// Int64, Bool and a timestamp.
type kind int

const (
	text kind = iota
	integer
	bigint
	boolean
	timestamp
)

// column is one run-state column.
type column struct {
	name     string
	kind     kind
	nullable bool
}

// table is one run-state table: its columns in the order the host declares
// them, and its key.
type table struct {
	name    string
	columns []column
	key     []string
}

func required(name string, kind kind) column { return column{name: name, kind: kind} }
func optional(name string, kind kind) column { return column{name: name, kind: kind, nullable: true} }

// generationLayout, runLayout, eventLayout and pointerLayout are the host's
// field descriptors (embedstore.GenerationFields and the others), column for
// column.
var (
	generationLayout = table{name: GenerationTable, key: []string{"identity"}, columns: []column{
		required("identity", text), required("spec_digest", text), required("spec_document", text), optional("name", text),
		required("reproducibility", text), optional("reproducibility_reason", text), optional("resolved_model", text),
		required("dimension", integer), required("target_schema", text), required("target_table", text),
		required("target_column", text), required("source_schema", text), required("source_table", text),
		required("consistency_mode", text), required("created_at", timestamp), optional("retired_at", timestamp),
		optional("verified_at", timestamp), optional("maintained_until", timestamp),
	}}
	runLayout = table{name: RunTable, key: []string{"id"}, columns: []column{
		required("id", text), required("spec_digest", text), required("generation_identity", text), required("environment", text),
		required("source", text), required("target", text), required("provider_profile", text), optional("resolved_model", text),
		required("ptah_version", text), required("policy_digest", text), required("phase", text), required("status", text),
		optional("lease_owner", text), optional("lease_expires", timestamp), required("fencing_token", bigint),
		required("snapshot_done", boolean), optional("snapshot_watermark", text), optional("catch_up_watermark", text),
		optional("cursor", text), required("rows_scanned", bigint), required("rows_embedded", bigint),
		required("rows_skipped", bigint), required("rows_deleted", bigint), required("batches_committed", bigint),
		required("provider_prompt_tokens", bigint), required("provider_total_tokens", bigint),
		required("provider_usage_batches", bigint), required("retry_count", integer), optional("verification_ref", text),
		optional("cutover_plan_ref", text), optional("approval_ref", text), optional("active_pointer", text),
		required("rollback_eligible", boolean), optional("failure_class", text), optional("failure_detail", text),
		required("created_at", timestamp), required("updated_at", timestamp),
	}}
	eventLayout = table{name: EventTable, key: []string{"run_id", "sequence"}, columns: []column{
		required("sequence", bigint), required("run_id", text), required("kind", text), required("at", timestamp),
		optional("actor", text), required("fencing_token", bigint), optional("from_phase", text), optional("to_phase", text),
		optional("detail", text), required("rows_scanned", bigint), required("rows_embedded", bigint),
		required("rows_skipped", bigint), required("rows_deleted", bigint), required("batches_committed", bigint),
	}}
	pointerLayout = table{name: PointerTable, key: []string{"target_schema", "target_table"}, columns: []column{
		required("target_schema", text), required("target_table", text), required("active_generation", text),
		optional("previous_generation", text), required("cut_over_at", timestamp), optional("cut_over_by", text),
		optional("plan_digest", text),
	}}
)

// layout lists the run-state tables in the order they are created.
func layout() []table {
	return []table{generationLayout, runLayout, eventLayout, pointerLayout}
}

// typeName is the YQL type a column of kind is created with. A timestamp is
// Timestamp64 on a line with the 64-bit date and time types and Timestamp on
// 25.1, which has none, as the host renders it.
func typeName(k kind, wide bool) string {
	switch k {
	case integer:
		return "Int32"
	case bigint:
		return "Int64"
	case boolean:
		return "Bool"
	case timestamp:
		if wide {
			return "Timestamp64"
		}
		return "Timestamp"
	default:
		return "Utf8"
	}
}

// accepts reports whether a column of kind may be stored as the type a table
// declares: its own type, or for a timestamp either width, because a store
// created on 25.1 stays the store after the server moves to a newer line.
func accepts(k kind, declared string) bool {
	if k == timestamp {
		return declared == "Timestamp" || declared == "Timestamp64"
	}
	return declared == typeName(k, false)
}

// createStatement is the CREATE TABLE of one run-state table, the text the
// host's renderer writes. It creates the table only where none is.
func (t table) createStatement(wide bool) string {
	definitions := make([]string, 0, len(t.columns)+1)
	for _, c := range t.columns {
		definition := Quote(c.name) + " " + typeName(c.kind, wide)
		if !c.nullable {
			definition += " NOT NULL"
		}
		definitions = append(definitions, definition)
	}
	keys := make([]string, len(t.key))
	for i, name := range t.key {
		keys[i] = Quote(name)
	}
	definitions = append(definitions, "PRIMARY KEY ("+strings.Join(keys, ", ")+")")
	return fmt.Sprintf("CREATE TABLE IF NOT EXISTS %s (\n    %s\n);\n", Quote(t.name), strings.Join(definitions, ",\n    "))
}

// selectList is the table's columns for a SELECT, in declared order.
func (t table) selectList() string {
	quoted := make([]string, len(t.columns))
	for i, c := range t.columns {
		quoted[i] = Quote(c.name)
	}
	return strings.Join(quoted, ", ")
}

// mismatch says how a described table differs from the layout, or returns ""
// when it is the layout: the same columns in the same order, each of a type
// and nullability the layout accepts, and the same key.
func (t table) mismatch(described RowTable) string {
	if !slices.Equal(described.PrimaryKey, t.key) {
		return fmt.Sprintf("table %s is keyed by %s, and the embedding store keys it by %s", t.name,
			strings.Join(described.PrimaryKey, ", "), strings.Join(t.key, ", "))
	}
	if len(described.Columns) != len(t.columns) {
		return fmt.Sprintf("table %s has %d columns, and the embedding store's has %d", t.name, len(described.Columns), len(t.columns))
	}
	for i, c := range t.columns {
		found := described.Columns[i]
		if found.Name != c.name || !accepts(c.kind, found.Type) || found.Nullable != c.nullable {
			return fmt.Sprintf("column %d of table %s is %s %s, and the embedding store's is %s %s", i+1, t.name,
				found.Name, found.spelling(), c.name, column{kind: c.kind, nullable: c.nullable}.spelling())
		}
	}
	return ""
}

// spelling is a layout column's type for a diagnostic.
func (c column) spelling() string {
	name := typeName(c.kind, false)
	if c.kind == timestamp {
		name = "Timestamp or Timestamp64"
	}
	if c.nullable {
		return "Optional<" + name + ">"
	}
	return name
}

// Quote renders a name or a path as one YQL identifier.
func Quote(name string) string {
	return "`" + strings.NewReplacer("\\", "\\\\", "`", "\\`").Replace(name) + "`"
}

// SchemaStatements are the statements that create the run-state tables, with
// 64-bit timestamps where wide holds.
func SchemaStatements(wide bool) []string {
	statements := make([]string, 0, len(layout()))
	for _, t := range layout() {
		statements = append(statements, t.createStatement(wide))
	}
	return statements
}
