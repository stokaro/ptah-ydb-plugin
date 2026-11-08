# Contributing to the Ptah YDB provider

This page collects build, test, conformance, and release details. The [repository README](README.md) is the user entry point; [YDB provider documentation](docs/README.md) holds behavior and operational reference material.

## Releases

A tag that starts with `v` runs `.github/workflows/release.yml`, which
publishes a GitHub release through GoReleaser (`.goreleaser.yaml`). Each
release carries one archive per platform, two JSON documents and a
`checksums.txt` file:

```text
ptah-plugin-ydb_<version>_<os>_<arch>.tar.gz   (.zip on Windows)
ptah-provider.json
release-lines.json
checksums.txt
```

`<version>` is the tag without the leading `v`. The platforms are linux,
darwin and windows on amd64 and arm64. Every archive holds the
`ptah-plugin-ydb` binary (`ptah-plugin-ydb.exe` on Windows), `README.md` and
`LICENSE`. The binaries are built with `CGO_ENABLED=0` and `-trimpath`.

`ptah-provider.json` names the provider, the release version, the oldest
provider contract a host must implement to start it (`min_contract`, a major and
a minor version), its engines and the archive and executable for each platform.
A host starts the release when its contract has the same major version and a
minor version at least `min_contract.minor`. The document states the minimum the
provider declares in its `Describe` reply, so the two cannot disagree. This
provider requires contract 25.7. It negotiates `database.context` and
`values.context` to resolve dependency paths, and `planning.lifecycle` to order
native replacements around common table operations. It offers
`planning.statement-style` (contract 25.8), which ptah-compat's `migrate diff`
needs, because it plans in the exact statement style: no comment that only
describes a statement, no guard the plan does not need and no CASCADE. The
operations the provider plans carry none of these in either style, so an exact
plan is the plan the annotated style gets. `ptah providers install`
reads the document to choose the newest release the host can start, without
downloading an archive. `release-lines.json` lists the release lines the provider declares.

`checksums.txt` lists the SHA-256 digest of every archive and of both documents
in `sha256sum` format (`<hex digest>  <file name>`). A host checks the archive
it downloads against the digest listed there.

Pull requests that touch the release inputs run `goreleaser check` and a
snapshot build, then verify the snapshot's `checksums.txt` and that
`ptah-provider.json` names exactly the archives GoReleaser wrote. To try it
locally:

```sh
GOWORK=off goreleaser release --snapshot --clean
(cd dist && shasum -a 256 -c checksums.txt)
```

## Build and test

At this revision, [`go.mod`](go.mod) pins `provider-sdk` v0.38.0, whose
[contract declarations](https://github.com/stokaro/ptah/blob/provider-sdk/v0.38.0/provider-sdk/provider/contract.go)
speak 25.40. The provider's [description](internal/backend/provider.go)
requires only 25.7 to start; later features are negotiated with the host.
Check those source files when changing a dependency or capability. Builds
require neither the Ptah application source nor a local SDK checkout.

Run the unit suite and build the executable:

```sh
GOWORK=off go test -race ./...
GOWORK=off CGO_ENABLED=0 go build -o bin/ptah-plugin-ydb ./cmd/ptah-plugin-ydb
```

The provider is held to Ptah's lint contract: `.golangci.yml` carries the
host's linters and settings, and the Makefile runs the host's other lint tools.
Lint as CI does, with golangci-lint v2.15.0 on the `PATH`; qtlint, nolintguard
and teststyle are pinned as tools in `go.mod`:

```sh
make lint       # qtlint, nolintguard, golangci-lint and the test style baseline
make lint-fix   # the automatic fixes of qtlint and golangci-lint, then make lint
```

Every linter runs over both build contours, the default one and the
`integration` tag. `scripts/check-test-style.sh` holds the tests to the
declarative rule: no `if`, `switch` or `goto` in a test function. The functions
that broke it when the rule was adopted are listed in
`.teststyle-baseline.json`; the list may shrink and must not grow.

CI compares `.golangci.yml` with the host's: every linter and formatter the
host enables is enabled, every setting of one is the host's, and the
exclusions that name no path are the host's. The gci prefix, the rowserrcheck
packages, the depguard messages and the path exclusions name this module and
are not compared. To run the comparison against a checkout of Ptah:

```sh
GOWORK=off go run ./internal/cmd/lintcontract -host ../ptah/.golangci.yml
```

The executable requires the SDK's authenticated local subprocess bootstrap.
Run it through a Ptah SDK client rather than invoking it as a command-line tool.
The committed module has no `replace` directive or workspace file.
`scripts/check-module-boundaries.sh` rejects local replacements and host-module
dependencies.

Live checks require an isolated test database. Schema tests create uniquely
named directories, tables, and topics and remove them afterward. Session tests only
read server metadata.

```sh
PTAH_YDB_TEST_URL='ydb://localhost:2136/local?go_balancer=disable' \
  GOWORK=off go test -race -tags integration ./integration -count=1 -v
```

A test whose dev database must be on a server other than the target's reads a
second isolated server from `PTAH_YDB_OTHER_TEST_URL` and skips without it. CI
starts the second server beside the first.

The test servers enable `enable_vector_index` and
`enable_external_data_sources`; the 26.2 fixture also enables
`enable_add_unique_index`. A full-text index is refused on 25.1, and the
full-text tests that need one skip there; the 25.1 run checks the refusal
instead. One full-text test turns `enable_fulltext_index` off for the whole
26.2 cluster through its dynamic configuration and drops the configuration
when it ends.

Ptah's provider conformance suite, from the SDK's package `conformance`, runs
in `integration/conformance_e2e_test.go`. Its offline part runs in the unit job
and needs no database: the handshake, the description and its negotiation
under every host profile, every RPC, offline calls, and the model identities
of the last release in `integration/testdata`. Its live part runs with the
other live tests: sessions, reads, locks across two processes, transactions, a
table created and dropped, a refused statement, and a login whose password is
the suite's canary. A release records its model identities there with the
suite's command:

```sh
GOWORK=off go run github.com/stokaro/ptah/provider-sdk/cmd/ptah-provider-conformance \
  models -executable bin/ptah-plugin-ydb -provider-id github.com/stokaro/ptah-ydb-plugin
```

CI runs this process test against YDB 25.1.4.7 and 26.2.1.14. It also rejects
host-module imports and local module replacements. These checks cover sessions,
SQL execution, commit/rollback, range conflicts, durable migration history,
atomic revision comparisons, shared data/history/tag transactions, durable event
journals, immutable paged metadata snapshots, process lock
exclusion, confirmed release, and write refusal after lease loss,
native row-table observations, offline
normalization, index implementation tables, changefeed consumers and retention,
standalone native objects, exact-directory completeness, and refusals for
incomplete reads. Coordination tests cover source decoding through planning,
rendering, native execution, and inspection. The subprocess conformance group
checks those services; the separate [host-driven integration suite](integration)
checks Ptah CLI planning, application, inspection, and migrations.

## Host-driven integration checks

The host-driven tests exercise schema inspection, planning, application,
migration execution and rollback, status, journals, tags, and locking through
the executable on YDB 25.1.4.7 and 26.2.1.14. Schema changes use
`--tx-mode none` or a `-- +ptah no_transaction` directive because YDB does not
provide transactional schema changes. For example,
[`TestHostPlansAMigrationAgainstTheLiveDatabase_HappyPath`](integration/host_migration_plan_test.go)
checks a plan against a live database, its application, row retention, and
subsequent convergence.

The host and provider are built against the published SDK pin with
`GOWORK=off`. The live tests run twice per change and nightly. The blocking
run builds the newest published Ptah release whose provider contract this
provider can run on (`scripts/resolve-host.sh`). A second, non-blocking run
builds Ptah's master branch to catch host changes early. A named `host_ref`
branch, tag, or commit can be tested in a blocking run when a pull request
needs an unmerged host change (`scripts/live-lines.sh`). The same checkout
builds `ptah-compat` for its host-driven tests. This repository owns the YDB
end-to-end and parity tests; Ptah's CI tests its provider contract on an
in-tree fixture provider. Release workflows also verify the packaged
executable and installable artifacts.

`TestHostAppliesAndAltersARowTable_HappyPath` drives `ptah schema apply` through
this provider: it creates a row table from a Go source, finds nothing to change
on the next run, sets a TTL in place, and finds nothing to change again.
