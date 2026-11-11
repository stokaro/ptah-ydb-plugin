---
title: Using the provider with Ptah
description: How a ydb:// URL, a dev database and ptah-compat reach YDB through this provider.
type: how-to
audience:
  - database-engineer
readerQuestion: "How do ptah and ptah-compat connect to YDB through this provider?"
goal: "Connect Ptah to a YDB database and choose where a run's dev database lives."
sourceOfTruth:
  - internal/connection
  - internal/devdatabase
  - internal/devclean
  - cmd/ptah-plugin-ydb
  - internal/vulnerability
  - .github/workflows/security-snapshots.yml
  - stokaro/ptah#4204
generated: false
searchAliases:
  - "ydb:// URL"
  - "YDB dev database"
overlaps: []
disposition: keep
sidebar:
  order: 1
---

# Using the provider with Ptah

Ptah reaches YDB only through this provider, provider ID
`github.com/stokaro/ptah-ydb-plugin`. The `ptah`, `ptah-compat` and `ptah-ls`
binaries hold no YDB implementation: the provider renders YQL, compares and
plans, reads a live database, applies changes, keeps the migration history and
the migration lock, lints YQL migrations, writes data and runs embedding
generations. Ptah keeps the shared pipeline: the source formats, the comparison
and ordering of a plan, the migration policy and the command surfaces.

[Understanding the provider boundary](https://docs.ptah.run/edge/extend/write-a-provider/ydb-walkthrough/)
follows one table, its changefeed, a transfer and the transfer's target table
through Ptah Core and this provider, and
[Explore the YDB walkthrough](https://docs.ptah.run/edge/extend/write-a-provider/ydb-walkthrough-explore/)
shows the same example as an interactive figure.

The engine name is `ydb`, with the alias `ydbs`. Ptah recognizes it without the
provider: a `ydb://` or `ydbs://` URL, `--dialect ydb` and a `docker://ydb/...`
or `docker+ydb://...` dev URL all name it.

## Install and select the provider

```console
ptah providers install ydb
```

Ptah's [Configure the YDB provider](https://docs.ptah.run/edge/extend/providers/ydb/)
describes the install checks, how a command selects the provider, an explicit
manifest, and the refusals a command reports when no usable installation is
selected.

## Connecting

`ydb://` connects over plaintext gRPC and `ydbs://` over TLS:

```text
ydb://localhost:2136/local
ydbs://ydb.example.com:2135/?database=/ru-central1/b1g/etn
```

The provider owns the grammar of the address: the database path, the credential
parameters, the balancer settings and the `monitoring` endpoint it reads the
cluster's feature flags from. See
[Connection addresses](connections-and-execution.md#connection-addresses).

## Server vulnerability scans

Run `ptah db vulnerabilities --db-url "$DATABASE_URL"` against a self-managed
YDB server. The provider sends its [reviewed server advisory
snapshot](../internal/vulnerability/ydb.json) to Ptah when the two negotiate
`security.advisory-snapshot`. Ptah matches the server version read during
connection setup. A provider without a reviewed snapshot produces an
`unassessed` result unless you select a local advisory file.

The [daily source check](../.github/workflows/security-snapshots.yml) reads
YDB's security changelog and opens a draft pull request for a changed advisory
body. Review the CVE scope and version ranges, update the snapshot, and remove
the `review/ydb.json` report before merging. The source also names a Go SDK
CVE, which does not affect the YDB server. A changed documentation revision
outside the advisory body creates no review report.

For a managed service, pass `--deployment managed`. Ptah reports
`not_applicable` because the service operator controls server patching. See the
[vulnerability scan guide](https://docs.ptah.run/edge/operate/vulnerability-scan/)
for coverage and CI failure policy.

## Dev, shadow and scratch databases

SQL cannot create a YDB database, so a `--dev-url`, `--shadow-db` or scratch
database that names a YDB server the run does not own is a dev realm: a
directory the provider creates for the run in the database the URL names, and
removes when the run ends. `docker://ydb[/<tag>[/local]]` asks the provider to
start a local-ydb container for the run. See
[Dev realms](connections-and-execution.md#dev-realms) and
[Dev databases](connections-and-execution.md#dev-databases).

## ptah-compat

No Atlas edition has a YDB driver, so YDB on `ptah-compat` is a Ptah extension.
The default profile takes a `ydb://` or `ydbs://` URL on the `schema`, `migrate`
and `script` verbs and starts the provider when a connection, a dev database or
a planning runtime first needs it. [Use a provider from
ptah-compat](https://docs.ptah.run/edge/extend/executable-providers/#use-a-provider-from-ptah-compat)
describes the session.

`PTAH_ATLAS_STRICT_COMPAT=1` reproduces the community binary, which answers
`sql/sqlclient: unknown driver "ydb". See: https://atlasgo.io/url` for a
`ydb://` URL, with `ydbs` for a `ydbs://` URL. Strict mode refuses a YDB URL in
those words wherever it comes from, and refuses `PTAH_PROVIDER_CONFIG` by name.
See [Compatibility differences](https://docs.ptah.run/edge/atlas/retained-divergences/#a-ydb-database-url).
