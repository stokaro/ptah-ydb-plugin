# Ptah YDB provider

This repository owns the executable YDB provider for [Ptah #4204](https://github.com/stokaro/ptah/issues/4204).
It is under development and is not ready for schema or migration workloads.

The current executable opens native YDB connections through the public Ptah
SDK. It supports anonymous, URL user/password, and URL token authentication.
Session receipts contain the selected database and measured server version.
The implementation does not import the Ptah application module.

## Build and test

```sh
GOWORK=off go test -race ./...
GOWORK=off CGO_ENABLED=0 go build -o bin/ptah-plugin-ydb ./cmd/ptah-plugin-ydb
```

The executable requires the SDK's authenticated local subprocess bootstrap.
Run it through a Ptah SDK client rather than invoking it as a command-line tool.
The SDK dependency is pinned to a published experimental revision. There is no
`replace` directive or workspace dependency.

Live checks require an existing test database. They open sessions and read
server metadata; they do not create or change database objects.

```sh
PTAH_YDB_TEST_URL='ydb://localhost:2136/local?go_balancer=disable' \
  GOWORK=off go test -race -tags integration ./integration -count=1 -v
```

CI runs this process test against YDB 25.1.4.7 and 26.2.1.14. It also rejects
host-module imports and local module replacements. These checks qualify the
session service only; they do not establish schema or migration support.

## Connection addresses

Use `ydb://host:2136/database` or `ydbs://host:2135/database` for TLS.
`database`, `token`, and `go_balancer` are the supported query parameters.
A token and URL user/password cannot be combined. Unknown or repeated
parameters, conflicting database paths, and malformed values are refused.
Credentials travel in the session-open request and are omitted from receipts,
SDK addresses, and error diagnostics. Native errors currently report only the
failed operation, which limits diagnosis until structured redaction is added.

Session close releases both the SQL pool and its parent native driver, even
when the RPC context is canceled. Cleanup results are retained for repeated
closes. A process accepts at most 1,024 lifetime session handles; start a new
provider process when that bound is reached.

## Extraction still required

The source reference is Ptah commit `7b4b19f49fadf03ce81def2f6168ea60b3261a71`.
The complete scope includes typed schema objects and facets, offline source
formats, normalization, comparison, planning, rendering, execution, migration
transactions/history/locks/logs/tags, capability detection, environment
credential sources, monitoring, and dev-database provisioning. The current
session service advertises none of these unfinished capabilities.

The host's embedded implementation still needs removal when these operations
are wired through this executable. Provider release workflows, default Ptah bundling,
and independent installation acceptance remain pending. No v1 stability or
full YDB support is claimed by this development state.
