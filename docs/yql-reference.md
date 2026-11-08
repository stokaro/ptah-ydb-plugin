---
title: YDB YQL source and analysis
description: YQL schema reading, SQL analysis, migration lint, and schema rendering.
type: reference
audience:
  - database-engineer
  - schema-author
readerQuestion: "How does the provider read and analyze YQL sources?"
goal: "Find YDB YQL source, analysis, lint, and rendering rules."
sourceOfTruth:
  - internal/backend
  - internal/inspect
generated: false
overlaps: []
disposition: keep
sidebar:
  order: 16
---

# YDB YQL source and analysis

This reference records the provider's YDB-specific behavior. For a first connection, start with [Using the provider with Ptah](usage.md). The [documentation index](README.md) links the task-focused guides.

References to the former embedded YDB engine describe the historical [Ptah implementation at 7b4b19f](https://github.com/stokaro/ptah/tree/7b4b19f49fadf03ce81def2f6168ea60b3261a71); current Ptah uses this provider.

## SQL schema reading

The provider serves `schema.read-sql` for the `ydb` engine in document mode.
`ptah schema plan` and `ptah schema apply` hand it the documents of a
`--schema-file` or of a schema directory of `.sql` files, and it reads them as
one load without a database session. The reader is a port of Ptah's own YQL
schema reader at commit `a4e286dbf5ac42a609bd6ccd2b60dcc3335c210d`, so a load
reads to the model Ptah's bundled YDB owner reads, and a load Ptah refuses is
refused with Ptah's words and at the position Ptah names.

The reply carries the tables with their keys, columns, defaults, serial
columns, comments and indexes; views; users, groups and memberships; grants and
revocations; coordination nodes as this provider's coordination model; and
topics with the consumers `CREATE TOPIC` and `ALTER TOPIC ... ADD CONSUMER`
declare, as this provider's topic model; and the data sources and external
tables `CREATE [OR REPLACE] EXTERNAL DATA SOURCE` and `CREATE [OR REPLACE]
EXTERNAL TABLE` declare, as this provider's external object models; and
streaming query definitions as the streaming query model; and secrets whose
values are approved input references; and the async replications and transfers
`CREATE ASYNC REPLICATION` and `CREATE TRANSFER` declare, with what a later
`ALTER ASYNC REPLICATION ...

SET` or `ALTER TRANSFER ... SET` of the load
changes folded in, as this provider's replication and transfer models. For a
host that lists `schema.sql-facets` (contract 25.21), it carries the
changefeeds `ALTER TABLE ... ADD CHANGEFEED` declares, with the consumers
`ALTER TOPIC <table>/<changefeed> ADD CONSUMER` adds to their topics, as one
facet of each table in this provider's changefeed model, the value a YAML
`changefeeds:` key of the table declares.

Under the same feature it carries the settings of a `CREATE TABLE` as the
facets a YAML or Go source declares for the same table (see
[Offline row-table declarations](table-models.md#offline-row-table-declarations)), so a table
means the same in all three formats:

- a row table's partitioning (`AUTO_PARTITIONING_*`), read replicas, key bloom
  filter, starting layout (`UNIFORM_PARTITIONS` or `PARTITION_AT_KEYS`), column
  families and TTL, as one row-table facet of the table;
- for each column that `FAMILY <name>` places in a family other than the
  default one, a column facet that names the family;
- a column table's hash key (`PARTITION BY HASH`), shard count
  (`AUTO_PARTITIONING_MIN_PARTITIONS_COUNT`) and TTL, as one column-storage
  facet. A TTL that only deletes is the storage's one deleting tier, as the
  `row_deletion_*` properties beside a YAML or Go column table are;
- for a host that also lists `schema.observed-index-facets`, the partitioning
  (`AUTO_PARTITIONING_*`) and read replicas a global sync, async or unique
  index declares in its `WITH` clause, as one facet of the index, the value a
  Go index directive or a YAML index entry declares.

A setting the statement leaves out keeps what the table or the index holds,
and a row table declared without a TTL has its TTL removed, as for a YAML or Go
source.

The reply says what each document declares, traces every object to its
statement, and claims complete coverage of the
row-table, column, column-storage, coordination, topic, external data source,
external table, streaming query, secret, async replication and transfer models,
of the changefeed model under that feature, so a table a load declares
without a changefeed is one with none, of the index settings model where
the host also lists `schema.observed-index-facets`, and of the native index
model where it also lists `source.index-kinds`, so a table a load declares
without a vector or full-text index is one with none.

A vector or full-text
index of a CREATE TABLE is no common index then: it reaches the host in the
native index set of its table, and a local index in the storage of its column
table, the values a YAML or Go source of the same index declares, so a YQL
schema renders, plans and applies them as the host's embedded engine did. A
`ptah:not-described` header entry of kind `coordination_node`, `topic`,
`external_data_source`, `external_table`, `streaming_query`, `secret`,
`replication` or `transfer` limits that claim, for one object or, without a
name, for the kind. An object the claim leaves out keeps the entries of its
access list too: the host plans no REVOKE on an object its source does not
describe.

A replication or a transfer is read as the host's reader reads it: the items
of `FOR ... AS ...`, `ENDPOINT` and `DATABASE` as the connection string they
spell, the lambda of `USING` byte for byte, and in an `ALTER` only the
settings YDB changes in place. A malformed statement is refused without naming
a token, since its connection clause may hold a credential written by mistake.

A limit names an object by its path from the database root, a dot being part
of a name, so `app.events` names a topic at the root and `app/events` one in
the directory app. A dotted limit of a topic, a secret or an external object
that names nothing the database holds, while the plan would drop the object
the same path written with a slash names, refuses the comparison with the
spelling that keeps that object, as the host's embedded engine refuses it. A
dotted limit that names an object the database holds is read as written.

A refused token is quoted with at most its first 64 bytes, where Ptah quotes it
whole: a schema file can hold a secret in a string literal.

A statement that changes an object no document of the load declares is refused
in Ptah's words: an `ALTER TABLE ... ADD CHANGEFEED` of a table, a `COMMENT ON`
an object, an `ALTER TOPIC ... ADD CONSUMER` of a topic, an `ALTER USER` of a
user, or an `ALTER SEQUENCE` of a Serial the load does not declare. Under
`schema.read-sql-undeclared` (contract 25.38, ADR 0120 in Ptah) the refusal is
an `UNDECLARED` error, which the host reads as its own reader's refusal, so a
YQL file beside an HCL file that declares the table is told that the YQL files
change only what the YQL files declare, as the host's embedded engine said. A
host that does not list the feature gets an `INVALID` error with the same
words.

A load that declares something no reply field or model of this provider can
carry is refused by name, with no partial reading:

- the settings of a table, for a host that does not list `schema.sql-facets`,
  which has no home for a facet of a table;
- a column family on a column table, and a row table that declares both
  `UNIFORM_PARTITIONS` and `PARTITION_AT_KEYS`, with the words a YAML or Go
  source of the same table is refused with;
- the partitioning and read replicas of an index, for a host that does not list
  both `schema.sql-facets` and `schema.observed-index-facets`: without the
  second the comparison could not see what an index holds, as for a YAML or Go
  source;
- a vector, full-text or local index, for a host that does not list both
  `schema.sql-facets` and `source.index-kinds`;
- a local index of a row table, a vector or full-text index of a column table,
  and a comment on one of them, with the words a YAML or Go source of the same
  index is refused with, and a vector index's partitioning, which the native
  index set does not hold;
- changefeeds and the consumers of their topics, for a host that does not list
  `schema.sql-facets`, which has no home for a facet of a table. The load then
  claims nothing about changefeeds and leaves those of its tables as they are;
- a user's password, for a host that does not list
  `source.sql-role-passwords`: the reply's role carries none, so the host would
  create the user without it;
- a grant on a resource pool whose name holds a dot, a quote or a bracket: Ptah
  names a pool it reads from a provider as it names a table, so the grant
  would come back on a renamed pool.

Under `source.sql-role-passwords` (contract 25.33, ADR 0112 in Ptah) the reply
carries the password a document gives a user as the document writes it: a
literal after `PASSWORD`, a YDB password hash after `HASH`, or the
`$PTAH_SECRET_...` reference a migration file Ptah generated names it by. The
host binds it as it binds a password a YAML or Go source declares, so the plan
names the user's execution input and no value (see [Common
objects](migration-state.md#common-objects)), and execution writes `HASH` for a hash, as the
host's embedded engine did. Measured on 25.1.4.7 and 26.2.1.14: a user
created from `PASSWORD 'x'` logs in with `x`, one created from `HASH` logs in
with the password the hash stands for, and the next run plans nothing.

Some of what a reading carries is not planned yet. Table creation refuses
column defaults, and table comments on a host that does not list
`schema.common-objects`, which carries them. Ptah refuses views and roles from
a provider's reading. A coordination node is planned as the coordination
services describe.

`internal/yqlschema/testdata` holds the differential corpus: every load of YQL
schema documents Ptah's own tests read, the YDB fixtures Ptah renders, and edge
cases, each with the answer Ptah's reader recorded. The unit suite compares the
provider's reading with each answer, refusal for refusal. The corpus README says
how the answers are recorded again.

## Offline SQL analysis

The SQL analysis service reads default-mode YQL without a database session. It
preserves source byte ranges and line numbers, original tokens, and quoted case.
Semicolons inside quoted or multiline literals, comments, action definitions,
inline actions, and lambda bodies remain inside their enclosing statement.
Leading and trailing comments stay in the returned source ranges.

The scanner and value decoder were extracted from Ptah's YQL implementation,
with its lexical regression corpus. The provider keeps no host-package import.
Live tests compare reconstructed statements with results from both supported
YDB test lines, including escaped quotes, multiline strings, compound bodies,
and numeric suffixes.

Native safety judgments cover topic and consumer positions, transfer consumers,
replica tables, coordination state, secrets, external data access, and streaming
checkpoints. Common table creation and a single ADD COLUMN clause retain the
host's common DDL assessment. Unmodeled effects, including arbitrary compound
bodies and changes to automatic expiration, report UNKNOWN and require review.
The analysis does not replace the server's full SQL grammar validation.

Malformed literals, incomplete compound bodies, invalid UTF-8, NUL, and unsupported
translation settings refuse the whole batch. Only the default lexer and
`--!syntax_v1` are accepted; ANSI mode is refused even when its directive appears
later in a source, because splitting could move it to a new query's head.
Whitespace-only sources are accepted. A source containing only comments or
terminators is refused because the SDK requires tokens for every
non-whitespace source range. No prefix of a refused batch is returned.

A request contains at most 256 sources and 3 MiB. Each source contains at most
65,536 significant tokens, and the complete response is bounded by 3 MiB.
Cancellation returns no analysis. This analysis service has its own contract;
table comparison and planning are exercised separately by the
[host-driven tests](../integration/host_migration_plan_test.go).

### Offline capabilities

The provider lists `engine.default-capabilities` (contract 25.27) and states
the facts an offline command plans `ydb` with: those a session states for a
26.2 server whose address names no monitoring endpoint, with the facts of the
common objects. Each release line states its own the same way, and a
`--server-version` selects the line it equals or the longest line it starts
with followed by a dot, so `26.2.1.14` selects 26.2. `ptah schema render`, `sql
lint` and the other commands that plan without a database read them. A command
that connects plans with the session's facts.

The facts also carry the keys the host reads and no session states, with the
values of the built-in engine's presets, on every line. The host reads them
through its capability registry when it plans a connected command too:
`index_covering_columns`, for one, makes an unchanged list of covered columns
compare as unchanged, so a second apply of the same schema plans nothing for
the index.
A host of an earlier contract receives no facts, plans offline against unknown
facts, and the provider refuses what needs one, such as a table creation,
which needs nontransactional DDL established.

### Text scanning

The provider lists `sql.scan` (contract 25.26). `ScanSQL` returns the tokens of
one YQL text with the same scanner, so the host reads YQL wherever it reads SQL
text without parsing it: the lineage of view bodies, the indentation of the
`sql` template function, and the scans of migration bodies. Both quote styles
and `@@...@@` delimit strings, a type suffix belongs to its string, and a
backticked name carries the path it stands for. A `--!` comment is a setting
the server reads only at the head of a text; the host clears `head` for a range
inside one, where the comment is a comment. A string whose escape YDB refuses
carries no value.

A semicolon is a `SEMICOLON` token only where it ends a
statement of the text: one that ends a statement inside a DEFINE ACTION or
DEFINE SUBQUERY body, a DO BEGIN block or a lambda is an `OPERATOR`, so the
host's statement splitter keeps such a body whole, as Ptah's built-in YDB
engine split it. A host of an earlier contract never calls the RPC and reads
YQL by its shared rules.

## Migration lint

The provider serves the SDK's `lint.migrations` service for the `ydb` engine
without a database session. It publishes its rules as data, with the codes,
titles and severities of the host's built-in YDB owner, whether a rule also
reads the down half of a migration, and the shared rules a finding replaces on
the same statement.

A split reads each migration file as YQL, with the lexer the SQL analysis uses.
It returns every statement's byte range, line, words and the comments directly
above it, which the host reads suppression directives from. It also reports the
facts the host's shared rules read: a drop that loses state (a topic, a
transfer, a coordination node, a streaming query, a replication dropped with
CASCADE), YQL's unique index, the steps of a table rebuild, and a safety
judgment for the statements only YDB's owner recognizes. The splitter reads
what a rebuilt table holds from the directory itself: the up migrations of
lower versions in version order, then the statements of the file before the
rebuild. An annotation judges the statements a host split without a dialect,
and declines a statement YQL would split differently.

A check judges every file of a run under the rules the request enables. A rule
reads the statement text and the capability map the request carries, never the
server version. The rules that need to know what a table looks like before a
statement read it from the directory itself, as the splitter does: whether an
index or the TTL uses a column, a table's partitioning settings, the views that
read it, its changefeeds and column families, the Serial columns and sequence
restarts, failed-over replications, and the comments Ptah's COMMENT ON
statements left. A table created outside the directory is unknown and reports
nothing.

`internal/migrationlint/testdata` holds the host's answers for a corpus of
migration directories and requests, recorded at one host commit, and the tests
require the same statements, facts, annotations and findings, message for
message. Its README says how to record them again.

### Statement facts for `ptah sql lint`

For a host that lists `lint.statement-facts`, which the engine lists beside
`lint.migrations`, a split also says what each statement is, which table it
creates without a primary key, and which capabilities it needs. `ptah sql
lint` reads a YDB file through these facts when this provider serves the
engine, in place of the host's built-in YQL reading, and reports what that
reading reports:

- a CREATE TABLE or an ALTER TABLE is examined; any other CREATE, ALTER or
  DROP is a schema statement no rule examines, named by its first two words;
- any other statement changes no schema object and is named by its first word,
  or as a named expression;
- a CREATE TABLE without a PRIMARY KEY clause names its table, and a table
  written as a named expression keeps the name as written;
- the capabilities are those the migration lint rules read: a unique index or
  a column with a default added to an existing table, a vector index, and a
  vector index over bit vectors. Each names the index or the column and the
  clause that needs it. The host compares the key with the capabilities of the
  server version it lints for.

A statement that starts with no word, such as a string or a parenthesized
query, is an expression the linter does not lint, and the first statement of a
file that opens with a UTF-8 byte order mark is read like any other. The
built-in reading skips both and reports nothing, which lets such a file read as
clean; the provider is stricter on purpose.

`internal/migrationlint/testdata/statementfacts` holds YQL files with the
built-in reading's findings on three release lines, recorded at one host
commit. The unit tests and an end-to-end run of the host's `ptah sql lint`
through this provider require the same rules at the same positions about the
same tables and capabilities. Its README says how to record them again.

A host that does not list the feature is offered none of these facts, and an
annotation request that asks for them is refused.

The host owns output formats, severity overrides, suppression and the rules
every dialect shares. The host's built-in owner reports nothing from these rules
on a run that names no dialect; the provider is only asked about its own engine,
so a host does not send it the codes of such a run.

## Offline schema rendering

The provider uses `schema.declarations` to create authored coordination nodes,
topics, external sources and tables, resource pools, classifiers, secrets and
streaming queries. The host
validates the whole schema and combines these creations with table and common
object steps before rendering SQL. Declaration operations retain their desired
definitions and contain no observed-state claims.

A rendered schema opens no session. Capability limits, conflicting object paths,
duplicate classifier ranks, and explicit native access requirements refuse the
whole batch. Use live schema planning for access requirements that a CREATE
statement cannot express. External tables follow their declared data sources;
classifiers follow their declared resource pools. External sources follow their
declared secrets. A streaming query follows each topic and table its text reads
or writes, since YDB compiles a query when it creates it. A path the text names
by its database is read below the database the query's value names, the one a
read took it from, as `schema inspect --format sql` renders it, and the text
stays as written.

A value that names no database, such as a query a source
declares for an offline render, cannot place such a path and is refused.
Secret statements retain approved input references; rendering opens no session
and reads no input value.

Under `render.omissions` (contract 25.33, ADR 0110 in Ptah) a render reports
what its statements go on without, as the host's embedded engine reported it,
and the host carries the omissions into the result of a schema render and of a
node render, where a caller that must not lose anything refuses the rendering.
They are the part of a created column's declared type YDB does not keep, such
as `VARCHAR(255): length 255` for a Utf8 column or `TIMESTAMP(3): precision 3
(YDB keeps microseconds)`, in either form, and in the TEXT form of a creation
that names no database, such as an offline render's, the start and the
increment of a Serial's sequence: only an `ALTER SEQUENCE` naming the sequence
by its absolute path sets them, so the table is written without them, where a
plan against a database writes the statement (see [Serial columns](table-creation-and-rebuild.md#serial-columns)).

Each names its column `<directory>.<table>.<column>`, as the host did. The
creation carries the declared type a column keeps only in part, since the
operation holds the YDB type alone.
