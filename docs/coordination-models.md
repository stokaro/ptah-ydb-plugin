---
title: YDB coordination models
description: Offline coordination declarations, comparison, counts, and native protocol generation.
type: reference
audience:
  - database-engineer
  - schema-author
readerQuestion: "How are YDB coordination nodes represented and compared offline?"
goal: "Understand coordination node models and comparison."
sourceOfTruth:
  - internal/backend
  - internal/inspect
generated: false
overlaps: []
disposition: keep
sidebar:
  order: 9
---

# YDB coordination models

This reference records the provider's YDB-specific behavior. For a first connection, start with [Using the provider with Ptah](usage.md). The [documentation index](README.md) links the task-focused guides.

References to the former embedded YDB engine describe the historical [Ptah implementation at 7b4b19f](https://github.com/stokaro/ptah/tree/7b4b19f49fadf03ce81def2f6168ea60b3261a71); current Ptah uses this provider.

## Offline coordination declarations

The source service accepts the captured YAML document key `coordination_nodes`.
The host parses YAML and supplies typed values and original byte spans; the
provider interprets this grammar without reading files or opening a session.

```yaml
coordination_nodes:
  workers:
    schema: app
    self_check_period: PT2S
    session_grace_period: PT5S
    read_consistency_mode: strict
    attach_consistency_mode: relaxed
    rate_limiter_counters_mode: detailed
```

Each entry is a mapping. Its key supplies the node name unless `name` overrides
it. `schema` is an exact directory relative to the database; `struct_name`
retains source provenance. These fields and every setting require text.
Unknown keys, explicit nulls, numeric coercion, repeated fields, duplicate
object identities, and collisions with declared tables in the same captured
namespace refuse the whole source batch. Root `ptah_locks` is reserved for
runtime use; the same leaf name in a user directory is allowed. Inspection
still returns root coordination nodes as ordinary observations.

Periods use positive ISO8601 durations and must preserve whole milliseconds.
The self-check interval must be 500–10,000 milliseconds. The grace interval
must be at least the effective self-check interval plus 1,000 milliseconds and
at most 30,000 milliseconds. The provider refuses configurations the server
would clamp. Mode strings are case-insensitive in source and stored lowercase.

Omitted settings remain absent in the desired model. They request server
defaults when the node is created or compared: a 1,000-millisecond self-check,
10,000-millisecond grace interval, relaxed reads, strict attachment, and
aggregated counters. Explicit settings equal to defaults remain explicit.
Offline normalization does not assert desired/observed equivalence.

The decoder returns native objects with source traces and coverage limited to
the models enrolled by each document. An empty mapping declares no nodes within
that scope; an empty model scope stays empty. No successful prefix survives an
invalid entry or cancellation. Desired and observed models have distinct
encodings and definition digests; observation coverage contains only observed
models.

The same service decodes the host's Go directive `ptah:schema:coordinationnode`,
written on a struct of its own, one node per directive:

```go
//ptah:schema:coordinationnode name="limits" schema="app" self_check_period="PT2S" read_consistency_mode="strict"
type Limits struct{}
```

`name` is required, and the other attributes are the YAML settings above, with
the same rules; `struct_name` has no attribute, since the host names the struct.
A YAML document that writes `coordination_nodes`, and every Go source, describes
the nodes completely, as they do for the host's embedded engine: a node the
source does not declare is one the plan removes from the directories it reads.

A host that lists `source.hcl` (contract 25.3, ADR 0036 in Ptah) reads the
top-level `coordination_node` block the engine claims, as the host's bundled
YDB source reads it: labeled with the node's name, or its directory and its
name, its settings string attributes spelled as above, and named
`coordination_node` by a permission's `for`.

```hcl
coordination_node "app" "limits" {
  self_check_period     = "PT2S"
  read_consistency_mode = "strict"
}
```

It also claims, at the index-attributes site, a global index's partitioning
and read replicas inside the frontend's `index` block, spelled as the index
directive spells them (see [Index settings from a source](indexes-and-table-execution.md#index-settings-from-a-source)):
a switch and the read replicas as strings, a count as a number.

```hcl
index "by_body" {
  auto_partitioning_by_load              = "ENABLED"
  auto_partitioning_min_partitions_count = 2
  columns                                = [column.body]
}
```

A `ptah:not-described coordination_node` header entry, with a node's path or
none, is the one header kind the engine declares, and the provider refuses a
name no node can take with the host's words. With `source.limits` negotiated,
the document's claim leaves the node it names, or every node, unmanaged. A host
that does not list `source.hcl` reads no such block.

An HCL document is an Atlas file, which names none of the nodes it leaves out,
so it claims the nodes only through its header, as the host's embedded engine
reads it: a `coordination_node` entry, or the captured `ptah:feature-coverage`
header an export writes, which the host reads itself. A document with neither
claims nothing of them, and a plan from it creates and alters the nodes it
declares and drops none. The provider cannot keep this rule alone: the SDK lets
a decoding claim less than complete only through header limits, so its reply
for a document without them claims every node.

A host that takes an HCL
document's claim only from its header asks the provider about no other
document; one that asks about every document plans to drop each node the
document does not declare, and writes its HCL exports without the captured
header. `TestHostKeepsTheNodesAnHCLDocumentDoesNotName` measures the first
kind of host and skips on the second.

An HCL file the host's embedded engine exported names that engine's
coordination-node model in its captured header: owner `ptah.run/ydb`, kind
`ptah.run/ydb/coordination-node`, schema version 1. The provider names that
identity as a predecessor of its own coordination-node model
(`models.predecessors`, contract 25.25), so a host that lists the feature reads
such a file with the claim it was written with. Applied back, it plans
nothing, and a node whose block is removed is dropped, as the embedded engine
dropped it. A file the host cuts the document into carries the provider's
identity. A host without the feature refuses the file with `unknown feature
codec: "ptah.run/ydb/coordination-node"/desired`.

`TestHostReadsTheBuiltinEnginesHCLExports_HappyPath` applies exports recorded
from the embedded engine (`internal/backend/testdata/builtin-hcl`).

A captured header the host writes for this provider's models names each
model by the identity the provider describes: the provider as the owner, the
model's kind and representation, its schema version and its definition
digest. The release version, the features a host negotiated and the engine's
other declarations do not enter it, so an HCL export one release of this
provider wrote reads with the next one while no definition text changed.

When a text changes without changing what a claim of the model means, the
identity it replaces is named a predecessor of the model itself
(`models.own-predecessors`), and an earlier export still reads; a header
that names a definition the provider neither describes nor names is refused:
`incompatible feature codec: coverage model
"github.com/stokaro/ptah-ydb-plugin/coordination-node"/desired`.
`TestModelIdentitiesAreDescribedOrDeclaredPredecessors` pins the identity of
every desired model, so a definition text cannot change unseen, and
`TestHostReadsAnEarlierProviderHCLExport_HappyPath` applies an export an
earlier build recorded (`internal/backend/testdata/provider-hcl`). A Go export,
and an HCL export from a host that writes no captured header for a provider,
name no model identity.

A Go source carries the same entries as `ptah:schema:notdescribed`
declarations, under `source.limits`, of the kinds the engine lists in
`go_header_kinds`: `coordination_node`, `streaming_query`, `resource_pool`,
`resource_pool_classifier`, `secret`, `topic`, `external_data_source`,
`external_table`, `replication` and `transfer`. An entry without a name leaves
the kind unmanaged; one with a name, written as a YQL header names it, leaves
that object unmanaged. A source that declares an object it also leaves
unmanaged is refused.

```go
//ptah:schema:notdescribed kind="topic" reason="not-inspected" provenance="observed"
type PtahSchemaObjects struct{}
```

`EncodeSource` (`source.encode`, ADR 0037, and `source.encode-groups`, ADR
0040) writes desired values back as the keys, directives and blocks this
provider's decoders read, which decode to the same declarations:

- **Coordination node:** YAML: entry of `coordination_nodes`; Go: `ptah:schema:coordinationnode`; HCL: `coordination_node` block after the tables.
- **Topic and its consumers:** YAML: entry of `topics`; Go: `ptah:schema:topic`, `ptah:schema:topic:consumer`; HCL: loss.
- **Secret, by the variable its value comes from:** YAML: entry of `secrets`; Go: `ptah:schema:secret`; HCL: loss.
- **External data source and external table:** YAML: entries of their keys; Go: their directives; HCL: loss.
- **Resource pool and classifier:** YAML: entries of their keys; Go: their directives; HCL: loss.
- **Streaming query:** YAML: entry of `streaming_queries`; Go: `ptah:schema:streamingquery`; HCL: loss.
- **Row-table TTL:** YAML: `platform.ydb`; Go: attributes of `ptah:schema:table`; HCL: the table's `platform "ydb"` block.
- **Row-table partitioning, read replicas, key Bloom filter, starting layout:** YAML: table keys; Go: attributes of `ptah:schema:table`; HCL: loss.
- **Column families and the columns they hold:** YAML: `column_families`; Go: `ptah:schema:columnfamily`; HCL: loss.
- **Column-table storage, hash key, shards, TTL:** YAML: `column_store`; Go: attributes of `ptah:schema:table`; HCL: a TTL that only deletes in the table's `platform "ydb"` block; the rest is a loss.
- **Changefeeds and their consumers:** YAML: `changefeeds`; Go: `ptah:schema:changefeed`, `ptah:schema:changefeed:consumer`; HCL: loss.
- **A global index's partitioning and read replicas:** YAML: keys of its index entry; Go: attributes of `ptah:schema:index`; HCL: attributes of its `index` block.

A value is written as the declaration the provider compares it as: a topic or a
changefeed as its declared settings, without the attributes YDB keeps on it; a
table's settings without the primary key the common columns carry; a column
default and a Serial's sequence not at all, since the common column carries
them: the read reports a sequence's start and increment as the column's
`identity_start` and `identity_increment` beside `auto_increment`, and refuses a
sequence with any other setting. A facet a source states
by writing nothing, such as an empty changefeed set, an empty native index set
or a column of the default family, is written by the table's own part.

The
default family every row table holds is written only when it has a setting of
its own, such as a compression or a storage pool, and a declaration that leaves
it out keeps what the table holds. A Go export writes the table directive's
attributes in the host's embedded engine's order: a column table's storage,
with `store="column"`, then the TTL, then the partitioning and the starting
layout. It also gives each column the Go type of its YDB type, as that engine
does, and refuses a nullable key column, which no declaration can ask for.

A Go export writes the directives of the standalone objects in the order the
host's embedded engine wrote them, each with its attributes in the order its
claim lists them: topics and their consumers, secrets, external data sources,
external tables and streaming queries after the views, then resource pools,
their classifiers and coordination nodes. Under `source.go-placements`
(contract 25.26, ADR 0077) those last three stand after the default
privileges, below the grants, where the embedded engine wrote them; a host
that does not list the feature writes them after the views.

What no declaration states is a loss for its value, in one line that names it:
an access requirement, table attributes, storage settings, tiering, a temporary
table, a family kept in memory, a TTL run interval, a disabled changefeed or the
stream of an async replication, a native index, a column's compression or
encoding, a native view (the host writes views from the common
wire), and an object of another database than the one it was read in. A
secret's value is never written.

HCL declares coordination nodes, a global
index's settings, and a table's TTL in its `platform "ydb"` block, as the host's
embedded engine does; its other YDB settings are losses, which the host reports
at the table in the embedded engine's words, such as `YDB table settings
(AUTO_PARTITIONING_MIN_PARTITIONS_COUNT = 3) are not represented in HCL`. A
facet whose TTL the block writes and that holds such a setting too is written in
part, and the loss names the setting.

The settings a read reports for every
table, YDB's documented defaults, are no loss, and neither are the empty
changefeed and native index sets a read reports for a table without either,
which the table's `platform "ydb"` part writes by writing nothing; a
changefeed or a native index the table holds is a loss by name (`changefeed
updates is not represented in HCL`). The parts are
decoded again before they are returned, and a value they do not give back is a
loss. A value that needs several declarations is a loss for a host that does not
negotiate `source.encode-groups`. `struct_name` is written in YAML alone.

A source claims every model its format lists, completely. An export of a read
is complete for the selection it is applied to, as the host's embedded YDB
engine writes it: a host that writes a read narrowed by `--schemas` sends each
kind the read knew in every directory it listed as complete. Applied back over
that directory, the export plans nothing, and an object whose declaration is
removed from it is dropped; applied over more directories, it drops their
objects too.

What the request does not know is written as not-described
entries, so that the source read back claims no more than that: a kind no
directory of the read knew, or one the read knew less in some directory, is
left unmanaged as a whole, since a header cannot name a directory, with
`reason="not-inspected" provenance="observed"`. An object whose own knowledge
is not complete gets an entry that names it. A facet is known where its table
is, so a table the export declares carries its facets as the read holds them,
and a declared table whose facet knowledge is not complete refuses the export.
Knowledge left unmanaged by a source's own entry is passed on with
`provenance="declared"`.

YAML writes no header, and neither does a format for a
host that does not negotiate `source.limits`, so there such knowledge refuses
the export, as does a kind no entry names. A host that writes an HCL export's
account itself, as its captured `ptah:feature-coverage` header, sends the
provider no coverage for HCL, and the provider writes no entry there.

The subprocess tests exercise decoding, encoding, normalization, and
representation conversion without a database address; a YQL schema declares
coordination nodes through [SQL schema reading](yql-reference.md#sql-schema-reading).

## Coordination comparison

The offline `models.compare` service compares desired coordination declarations
with captured native observations. Its context must establish the
`coordination_nodes` capability, which session receipts advertise for the native
implementation. Omitted settings compare using server defaults;
source holder names do not change native configuration. Removing an explicit
nondefault setting produces a change back to the default. Raw stored values that
the server clamps refuse comparison.

The `github.com/stokaro/ptah-ydb-plugin/coordination-node-change` model carries complete `before` and `after`
operands. The native `before` retains access metadata. A null operand means
established absence. Normalization reconstructs impact, removal, and access
facts from these operands. Creation has unknown inherited access policy;
alteration preserves access; dropping a node removes its runtime state and access.
Planning lowers these changes to the `github.com/stokaro/ptah-ydb-plugin/coordination-node-operation` model,
which rendering preserves as a native execution envelope.

An unread object does not establish absence. Independent coverage sources must
all establish completeness; overlapping unknown regions override complete
regions, and explicit subject claims override namespace claims. Namespace
components, target scopes, and exact empty paths retain their boundaries.
An incomplete desired source preserves known current configuration. Every
decision is made for one node, so a namespace the read left out produces no
diagnostic of its own.

The host names a declared node without a database, because it lives in the
session's, while inspection names that database in every owner and region it
reads. The comparison reads both in the host's terms, so the declared node and
the node read at its path are one node. A change names the node as the host
sent it: by the declared identity, or by the read one for a removal. A
comparison whose read names two databases is refused.

Coordination exports retain owner, direct grants, and effective-permission
requirements in an optional `access` field. Its JSON field names use snake case.
Omission leaves access unmanaged. The YAML source grammar above does not accept
access declarations. Comparison reports unsatisfied access requirements as
undecided; change/operation normalization refuses them. Existing nodes can
change configuration while satisfying unchanged access requirements. Creation
with explicit access requirements is refused because the coordination service
cannot promise inherited permissions or apply grants.

The comparison refuses contradictory identities, forged normalization receipts,
and collisions with declared relations. Required nodes are not removed.
Exclusions for this model match exact relative paths such as `app/workers`,
without case folding or trimming. Inspection reserves the database-root
`ptah_locks` node (see the reservations above), and the comparison leaves that
path out too: it plans no removal and reports nothing about it. A declaration
at that path is the one case it reports, as undecided; YAML and Go declarations
and change normalization reject it.
This changes the desired model definition; pre-v1, no compatibility with the
earlier experimental definition is owed.

Offline subprocess tests cover decode, compare, and change normalization without
opening a session. Live tests compare native observations after closing their
session on both qualified YDB lines. `TestHostAppliesACoordinationNode_HappyPath`
drives `ptah schema apply` of the current host through this provider from a
YAML, a Go and a YQL source: it creates a node, finds nothing to change on the
next run, changes its settings in place, and removes it once the source no
longer declares it, reading the node back after each run.

## Comparison counts

The provider offers `models.compare-counts`, so `ptah schema drift --format json`
lists its native kinds in the report's inventory. Each object comparison counts,
under its change kind, the objects it decides: an object both sides hold counts
on both, whether or not it differs and also when the plan drops and creates it
again, an object only the source declares counts as desired, and an object only
the server holds counts as observed. An object the comparison excludes, reserves,
keeps because the source does not describe it, or leaves undecided counts on
neither side.

The counted kinds are secrets, streaming queries, coordination nodes, topics,
external data sources, external tables, resource pools, resource pool classifiers,
async replications and transfers. A service counts every kind it owns, with zeros
for a kind a batch holds nothing of, because the host reports a kind only when
every service that ran counted it.

The row-table facet comparison counts nothing, so its two change kinds have no
inventory entry. A table's TTL and partitioning are settings that stand for no
object. A table's changefeeds change as one set, with one change for the table,
and the contract refuses more additions and removals than a kind has changes, so
a set that gains two changefeeds cannot count them.

A host that does not list the feature receives no counts and reports none of
these kinds, never a zero.

## Native protocol generation

The upstream Go bindings lack some native index, sequence, changefeed, consumer
and column family fields, such as a family's cache mode. The provider's typed
definitions preserve their native wire numbers; attribution and the pinned
upstream source are in `third_party/ydb/NOTICE.md`. Unsupported fields remain
errors, a family's compression level among them. A table service request is
built with the pinned bindings and carries the fields they lack as unknown
fields, the only ones the provider accepts: a family's cache mode as field 6
of the family, and a new table's vector or full-text index as field 7, 8 or 9
of its index, the upstream arm. Runtime statistics are omitted from schema
observations.

With protoc 35.1 on `PATH`, regenerate with:

```sh
sh scripts/generate-native-schema.sh
```

The Go generator is pinned in `go.mod`. CI regenerates these definitions and
requires unchanged generated files.
