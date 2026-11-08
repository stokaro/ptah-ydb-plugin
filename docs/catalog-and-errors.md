---
title: YDB catalog and diagnostics
description: Error reasons, storage reservations, schema observations, and object counts.
type: reference
audience:
  - database-engineer
  - schema-author
readerQuestion: "What can the YDB provider observe, and how does it report uncertainty?"
goal: "Understand YDB catalog reads, coverage, counts, and diagnostics."
sourceOfTruth:
  - internal/backend
  - internal/inspect
generated: false
overlaps: []
disposition: keep
sidebar:
  order: 8
---

# YDB catalog and diagnostics

This reference records the provider's YDB-specific behavior. For a first connection, start with [Using the provider with Ptah](usage.md). The [documentation index](README.md) links the task-focused guides.

References to the former embedded YDB engine describe the historical [Ptah implementation at 7b4b19f](https://github.com/stokaro/ptah/tree/7b4b19f49fadf03ce81def2f6168ea60b3261a71); current Ptah uses this provider.

## Error reasons

The provider lists `errors.reasons`, so a host that lists it too prints why a
call failed after its gRPC code:

```text
provider compare models failed (InvalidArgument): streaming query capability was not established
```

A reason is a sentence the provider wrote. It names the subject, a capability
or a parameter, and never quotes statement text, a value, an address or a
credential. Package `internal/authored` keeps such sentences apart from the
errors the provider receives: `authored.New` marks one, and `authored.Status`
sends it as the reason, with the error's whole text as the status message,
which no host prints.

The reasons the provider gives are its refusals of a
connection address, of a monitoring page it could not read, of a capability
the target lacks, a declared external object among them, of a lock the server
refuses this user, of a streaming query body changed without
`allow_state_reset`, the refusals whose text is a fixed sentence, such as a
closed session or a reached handle limit, and the refusal of a table's
declaration by table preparation or the creation
projection. That last reason names the table and gives the sentence schema
validation reports for the same declaration; the compilation behind it reads no
server. A YDB driver's error is never a
reason, because YDB can quote a statement and its values, or a credential
source: the host prints its code alone.

A declaration a source decoder refuses gives a reason in the words of Ptah's
built-in YDB engine: a topic, a changefeed, an external data source or table, a
resource pool or classifier, a streaming query, an async replication or a
transfer, by its YAML key or its name, the attribute, and what the attribute
takes. `authored.Declared` writes it from the decoder's words around the
refusal and the refusal itself without the value it quotes. A refusal another
package wrote, such as the YQL splitter's, gives none. A Go directive that
leaves out a required attribute is refused as the host's annotation parser
refused it, and the host names the directive's place after the reason:

```text
provider decode source failed (InvalidArgument): topic "events": invalid retention_period: takes an ISO 8601 duration such as PT12H or P1D
decode Go directives of engine "ydb": provider decode source failed (InvalidArgument): missing required annotation attribute "rank" on //ptah:schema:resourcepool:classifier at Refused
```

A failed execution and a commit that did not apply give a reason too, in
`Execution.diagnostic` and `TransactionResult.diagnostic` (contract 25.18), and
a host of that contract prints it after the outcome:

```text
provider execution: NOT_APPLIED (0 units confirmed applied): YDB refused the statement while compiling it (GENERIC_ERROR, line 1, column 27)
provider execution: NOT_APPLIED (0 units confirmed applied): YDB refused the statement: it violates a constraint of the table, such as a key the table holds (PRECONDITION_FAILED, issue 2012)
```

The reason of a unit YDB answered says what YDB did with it (refused it,
refused it while compiling it, aborted its transaction, or answered without
showing whether it applied), followed by YDB's status, the most specific issue
code and the position YDB named, counted in the unit's own text: on a dev
realm, the realm's pragma YDB received first is not counted. For the issue
codes 2003, 2012, 2031 and 2032
it also says what the statement did, in the provider's words. A reason never
repeats YDB's issue text as a whole: measured on 25.1.4.7 and 26.2.1.14, that
text quotes the token YDB refused, a string literal of the statement and the
table's path, and a statement can carry a password or a bound secret. The issue
text stays in the result's message.

A reason carries the parts of it that quote
none of those, as the host's embedded engine printed them: the paths of the
objects a refused DROP leaves in place, YDB's fixed sentences, and the check a
rebuild's copy fails with, which the provider wrote into the statement and which
names the table and the column:

```text
provider execution: NOT_APPLIED (0 units confirmed applied): YDB refused the statement: rebuilding table app.items: column n holds a value that does not convert to its new type (PRECONDITION_FAILED)
```

YDB's fixed sentences follow `saying`. A name YDB does not take in a path, such
as a secret whose name holds a space, is refused on 26.2.1.14 with an issue that
quotes the path and the name; the reason carries YDB's sentence without them:

```text
provider execution: NOT_APPLIED (0 units confirmed applied): YDB refused the statement, saying "symbol ' ' is not allowed in the path part" (SCHEME_ERROR, issue 2019)
```

INTERNAL_ERROR shows nothing about a statement, with one measured exception:
26.2.1.14 fails an `UPSERT` on a table with a unique index that names no column
a synchronous index is keyed on while it plans the query
(`verification=!hasUniqIndex || !usedIndexes.empty()` in
`kqp_opt_phy_upsert_index.cpp`), and writes nothing; 25.1.4.7 runs the same
statement. Such a data statement is refused rather than unknown: a unit is
NOT_APPLIED with the reason that YDB cannot plan the `UPSERT` and that naming
every column of the table, or `UPDATE` or `INSERT`, avoids it, and a statement
the host runs for an operator, such as a check or a test assertion, FAILED
with YDB's issues. A unit the provider refused before YDB answered, a statement
timeout, a lost lock and a commit YDB did not confirm each give a fixed
sentence.

Where YDB's issues say a feature flag is off, the reason names the capability
and the flag, as the host's embedded engine words such a refusal, around the
provider's own reason where the host's engine quotes YDB's text, then what to
do. The provider's reason carries YDB's sentence for the flag, without what
YDB wrote around it. A session whose address names no monitoring endpoint took
the capability from its release line's default, and is told to name the
endpoint as well as to turn the flag on; one whose address names it read the
flags already, and is told only to turn the flag on. The texts and the flags they name are the
host's measured ones (`internal/ydbflags` in Ptah). Measured on 25.1.4.7 and
26.2.1.14, where `EnableResourcePools` is off by default, without the endpoint:

```text
provider execution: NOT_APPLIED (0 units confirmed applied): capability resource_pools is off on this YDB cluster, which runs with feature flag EnableResourcePools off: YDB refused the statement, saying "Resource pools are disabled" (UNSUPPORTED, issue 2030). Turn the flag on, or name the cluster's monitoring endpoint in the URL (monitoring=http://host:8765) so Ptah reads the flags before it plans
```

and with it, the same sentence up to `Turn the flag on`.

YDB refuses a DROP of an object other objects depend on, such as an external
data source an external table reads, with `SCHEME_ERROR` before it changes
anything, and names the dependents inside issue 2019. A reason repeats their
paths, as Ptah's built-in YDB engine printed them, because the operator has to
drop them first. Measured on 25.1.4.7 and 26.2.1.14:

```text
provider execution: NOT_APPLIED (0 units confirmed applied): YDB refused the statement: other scheme objects depend on what it drops, /local/readers/tbl; drop them first (SCHEME_ERROR, issue 2019)
```

25.1.4.7 has no unique secondary index, and refuses one with `Unknown index
type: syncGlobalUnique` inside its execution stage, before it proposes a
schema operation. The unit is NOT_APPLIED (PARTIAL after an applied prefix),
with the reason `YDB refused the statement, saying "Unknown index type:
syncGlobalUnique": the YDB release the cluster runs has no unique secondary
index (GENERIC_ERROR, issue 1060, line 1, column N)`.

## Operational storage reservations

Inspection recognizes history and metadata storage from exact ownership attributes and
complete native layout, before extracting indexes or changefeeds. A marked
table with an altered layout refuses the entire inspection. An unknown marker
version is also refused. An unmarked table remains managed even when its name
or layout matches a history table.

Confirmed history, journal, and tag stores are returned as SDK reservations
instead of managed tables. A reservation occupies the name for every supported native scheme-object
kind and for common tables. Exact UNREPRESENTABLE subject coverage overrides
directory completeness at these occupied names, so omission does not assert
absence. A comparison leaves a reserved name out. When a plan changes an object
a streaming query could read, it counts no query at a reserved name, even where
the desired source claims nothing there, as an HCL document without a coverage
header does. Filtered reads return reservations only from the exact directories
they inspect. Names or layout alone never establish ownership.

The coordination node `ptah_locks` at the root of the session's database is
reserved with purpose LOCK by its name alone. The lock service takes whatever
node it finds at that path, after checking its consistency settings, so the
name is the reservation; YDB returns SUCCESS for a repeated CreateNode, so no
creation receipt could prove more. A read reports the name as taken and
describes no node there, so `db read` and an export leave it out, as the host's
embedded engine does. A reservation never changes or drops the node. Every
other coordination node, one named `ptah_locks` in a directory included, is a
managed observation.

Operational storage reservations support host collision and reference guards. They do not
establish native directory-cascade safety; containment qualification remains
pending. Planning orders an object created below a path after the drop of the object there, and refuses
any other overlap of directory lifecycles. Markers
are storage-format identifiers, not an access-control mechanism against a database administrator.

## Schema observations

Inspection reads the native scheme, table, topic, view, and coordination APIs. Common columns preserve
YDB type spelling, Decimal precision/scale, nullability, and key membership,
and with `schema.observed-primary-keys` each table's key in key order, so a Go
export writes `primary_key="b,a"` for a table keyed in an order other than its
columns'.
Provider-owned table properties retain key order, TTL, column families,
partitioning, storage settings, attributes, and access metadata. Column
properties retain family assignment and typed literal or sequence defaults.
The observations carry their complete native field definitions and digests.
Normalization works offline after the database session closes.

Schema selectors name exact directories relative to the selected database.
Use `/` for its root. An unfiltered read walks subdirectories. A directory
whose name starts with a dot is not read, as the host's embedded engine read
none: `.sys`, `.metadata` and the other directories YDB keeps for itself, and
a user's directory such as `.hidden`. The read claims such a directory, and a
selected path below one, as empty, so a declared object there is created, and
nothing there is dropped. Measured on 26.2.1.14, that engine also plans the
creation again on the next run, which YDB refuses (`path exist`); through the
provider the next run is refused before the unit is sent.
A table whose own name begins with a dot is read like any other. Traversal and
snapshot size are bounded.

A database nested in the selected one, such as `/local/db2` in `/local`, is
another database, and the read leaves it out as the host's embedded engine
did: YDB lists it as an entry of type DATABASE, and an unfiltered read neither
descends into it nor reads what it holds. A selector that names a nested
database, or a path below one, reads as an empty directory, as that engine
read it. The read claims the nested database's path as holding nothing of the
selected database and says why, and a plan that creates a table, a view or a
native object there is refused with a reason that names the nested database.

Here the provider is stricter than that engine on purpose: that engine planned
the `CREATE TABLE`, YDB ran it through the outer session into the nested
database, and every later run planned it again and failed (`path exist`). A
native write whose directory is at or below a nested database is also refused
before anything is sent.
Coverage names inspected subjects and records native-object completeness for
each directory whose listing and child reads finished. These regions bind the
exact database, empty catalog, and exact schema directory; an unfiltered read
sends subtree regions instead where the host negotiates them (see below). An
empty directory therefore establishes native-object absence there.

Unread
descendants, excluded system directories, and other namespaces remain unknown.
The table and column facet kinds (a row table's settings, indexes and
changefeeds, a column table's storage, a column's properties) get the same
complete region for each such directory, since the read describes every table
there with each facet its kind of table has. Without it those kinds stay
narrowed in an export the read otherwise knows completely, and a Go or HCL
source has no not-described entry for a facet kind, so `ptah introspect` of
such a read, of the whole database or of a directory beside the database's
pools, is refused before it writes (`provider encode source failed
(Internal)`).

A selected directory YDB does not show (it answers SCHEME_ERROR) reads as
empty when its parent's listing, proved the same way up to the database root,
does not name it. That is the evidence a walk from the root takes for any
entry a listing leaves out, and it lets `schema apply --schemas` create the
first object of a new directory. A directory its parent names but the session
cannot list is still refused, and so is a path below an object that is not a
directory. Execution reads the directory of each object it creates through
the same rule; see [Execution and transactions](connections-and-execution.md#execution-and-transactions).

A selected path that an object holds reads as an empty directory, with
nothing below it: YDB needs every path above an object to be a directory. The
object itself is read with the directory that lists it. So `schema apply
--schemas app,app/holder` reads the table `app/holder` in `app`, and a secret
declared in `app/holder` is planned after the table's drop.

A read with no selector claims more than the directories it lists. With a
host that lists `schema.subtree-coverage` (contract 25.22, ADR 0064 in Ptah),
the provider declares `/` as its schema separator, and the read sends one
complete region over the root and every directory below it. The directories
the walk does not list, a dot-prefixed one and at the database root the dev
realm directory, are covered by it as holding nothing, as the host's embedded
engine read them, so an export of the whole database claims every native kind
and writes no `ptah:schema:notdescribed` entry for them. A native object in a
directory that does not exist yet is then absent, and `schema apply` or
`migrations generate` with no `--schemas` creates it, as the host's embedded
engine does.

A filtered read keeps one exact region per directory, since a
selector names one directory. With an older host, the read claims each
directory it lists alone, and a native object in a new directory is left
unplanned with a warning, as for a topic in `fresh/nested`:

```text
Warning: github.com/stokaro/ptah-ydb-plugin/topic fresh/nested.events could not be compared between the database and the desired schema: The current topic or its absence was not established.
No changes planned, but 1 comparison limit remains unresolved.
```

Name the directory with `--schemas` to create the object.

Unknown native fields and enum values are refused before JSON conversion,
except where the host's embedded engine recorded the object and read the rest:
a coordination node whose description holds a mode or a field the provider
does not model is reported unrepresentable, and so is a resource pool or a
classifier holding a setting outside what YDB keeps. A column store and a
sequence, which the provider does not model, are left out and named in
`unread_tables` where the host lists `schema.unread-tables`. A queue of the
older persistent queue kind (`PERS_QUEUE_GROUP`) is reported uninspected, so a
plan neither keeps nor drops it, and a system view outside a dot-directory is
the server's and is left out.

Any other kind of scheme entry refuses
inspection, naming the object and its kind. Shared topic consumers refuse
inspection.
A table an async replication writes is left out of the tables, and the
replication's items name it (see [Async replications and
transfers](streaming-and-workload-objects.md#async-replications-and-transfers)). Right after `CREATE ASYNC
REPLICATION`, YDB can list the replica while DescribeTable still answers
SCHEME_ERROR. So a row table a listing names and the table service answers
SCHEME_ERROR for is described again for up to 10 seconds, and the directory is
listed again before each wait.

The topic of a changefeed is waited for the
same way, with its table described again before each wait: the changefeed the
replication adds to its source table can be named before DescribeTopic
describes its topic. The read fails if the new listing or description stops
naming the object, if it fails, or if the object is still not described when
the time runs out; it never reads such an object as absent. A failed traversal
returns no snapshot.
These checks keep an incomplete read from being mistaken for an absent object.

Observed property models use YDB's typed protocol definitions inside this
provider. The neutral SDK carries their envelopes. Common fields and runtime
statistics are excluded from property payloads; they cannot become a second
writable representation. Index observations preserve sync, async, unique, vector, prefixed vector,
and fulltext variants. The provider reads every kind-specific implementation
table, including its columns, keys, and partition settings; parent table
metadata alone does not expose those settings. Missing implementation tables
or non-ready indexes refuse inspection. Internal sequence configuration is retained;
its mutable position is excluded after unknown-field validation. An empty index facet records the
absence of indexes on that inspected table.

Changefeed observations combine the table API description with a topic API
read for each child stream. They retain modes, formats, state, retention,
consumers, access metadata, and attributes, including replication bindings.
Initial scan presence is retained after completion; progress counters are
excluded. A changefeed in `STATE_INITIAL_SCAN` asked for a scan even before YDB
reports its first progress, as for the changefeed `CREATE ASYNC REPLICATION`
adds to a source table. An empty changefeed facet records absence on its
inspected table.
Standalone topics use the same reader and normalization rules, with their own
object identities.

Topic observations preserve partition settings and quotas, retention, codecs,
consumer options, and attributes. Live partition topology and topic/consumer
statistics are excluded. A missing child description refuses the entire read.

Indexes, implementation tables, changefeeds, and the consumers of a
changefeed's topic are read in name order; a topic's own consumers keep their
order (see the topic section).
Column and key arrays retain their observed order. Desired coordination-node declarations are available as described below.
Every observed model has a separate desired contract and an offline conversion.
The provider also advertises table and access comparison and planning, plus native-object lifecycles. The [table reference](table-creation-and-rebuild.md) and [object lifecycle reference](native-objects.md) state the operations supported by each model; a source format can represent fewer details than the native catalog.

Views retain the query returned by the view service and the attributes read
through the table service on the same path. This includes comments and custom
attributes. Coordination nodes retain native consistency modes, timing, and
counter configuration; the envelope owns the node path. External data sources
retain source type, location, authentication references, and native properties.
External tables retain source references, location, ordered typed columns, and
content options without rewriting their native keys or JSON values.

## Object counts

For a host that lists `models.report-counts`, the provider reports count
metrics for its topic, changefeed, coordination node, streaming query, external
data source, external table, async replication, transfer, secret, resource pool
and resource pool classifier models, and for its row-table properties, column
storage, index set and index settings models, desired and observed, under the
metric names and help texts of the host's built-in YDB engine. It counts each
value offline: a topic is one topic and its consumers, a table's changefeed set
its feeds and the consumers across them, an external table one table and its
columns, and every other object one. `ptah schema stats` then writes these
lines through the provider as through the built-in engine.

The table setting families count as the built-in engine counted them:

| Metric | Counts |
| --- | --- |
| `ydb_ttl_tables` | Row tables with a TTL, and column tables whose TTL is one tier that deletes rows. |
| `ydb_column_families` | The column families of each row table, without a `default` family that has no storage pool of its own, no compression, the regular cache and `keep_in_memory` off. |
| `ydb_table_partitioning` | Row tables whose partitioning, read replicas or key bloom filter differ from what YDB gives a new table, and declarations that state a starting layout. |
| `ydb_index_partitioning` | Global sync, async and unique indexes whose partitioning or read replicas differ from what YDB gives a new index. |
| `ydb_column_tables` | Column tables. |
| `ydb_tiered_ttl_tables` | Column tables whose TTL has any other tiers, such as one that moves rows to an external data source. |
| `ydb_vector_indexes` | Vector indexes. |

The host counts the global indexes it reads as common indexes in `indexes`. The
index set and the column storage models add the indexes they hold under the
same metric and help, `Indexes`: vector and full-text indexes, and a column
table's local indexes, which the built-in engine counted among the indexes too.
A host adds them to its count with stokaro/ptah#5378; a host without it refuses
`schema stats` when a provider reports the metric.

What YDB gives a new table or global index is automatic partitioning by size
at 2048 MB, none by load, at least one partition, no maximum, no read replicas
and no key bloom filter; an unspecified setting is that value. A read holds
every setting the server describes and is counted against these values, as is
the declaration the host converts it to before it counts.

A host that reads global indexes as common indexes (`schema.observed-indexes`)
leaves them out of a table's index set, so `ydb_index_partitioning` counts each
such index from its own settings, the index settings model. A host reads those
when it negotiates `schema.observed-index-facets`; through a host that does
not, the count leaves the common indexes out.
