# Using the provider with Ptah

Ptah reaches YDB only through this provider, provider ID
`github.com/stokaro/ptah-ydb-plugin`. The `ptah`, `ptah-compat` and `ptah-ls`
binaries hold no YDB implementation: the provider renders YQL, compares and
plans, reads a live database, applies changes, keeps the migration history and
the migration lock, lints YQL migrations, writes data and runs embedding
generations. Ptah keeps the shared pipeline: the source formats, the comparison
and ordering of a plan, the migration policy and the command surfaces.

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
[Connection addresses](../README.md#connection-addresses).

## Dev, shadow and scratch databases

SQL cannot create a YDB database, so a `--dev-url`, `--shadow-db` or scratch
database that names a YDB server the run does not own is a dev realm: a
directory the provider creates for the run in the database the URL names, and
removes when the run ends. `docker://ydb[/<tag>[/local]]` asks the provider to
start a local-ydb container for the run. See
[Dev realms](../README.md#dev-realms) and
[Dev databases](../README.md#dev-databases).

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
