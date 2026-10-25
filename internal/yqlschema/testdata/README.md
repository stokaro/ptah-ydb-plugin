# YQL schema reading corpus

The corpus compares this provider's reading of YQL schema documents with
the reading of Ptah's bundled YDB owner. Each case is one load of documents
and the answer Ptah recorded for it. The unit suite reads every case and
holds the provider to the answer: a refused load must be refused with the
same words, in the same document and at the same position, and an accepted
load must read to the same tables, columns, indexes, views, principals,
grants, objects and per-document declarations.

The answers come from Ptah commit
`a4e286dbf5ac42a609bd6ccd2b60dcc3335c210d`. Each golden file names the commit
it was recorded at in its `host` field.

## Layout

- `corpus/<case>/` holds the documents of one load, `01.sql`, `02.sql` and
  so on, read in name order. The bytes are kept exactly as recorded
  (`.gitattributes` turns off line-ending conversion), because a refusal's
  position is a byte offset.
- `corpus/<case>.load.json`, when present, names the database root the
  load's absolute paths resolve against.
- `corpus/<case>.golden.json` is Ptah's answer. `grammar` is what the
  owner's grammar read, which is the call a provider's `ReadSQLSchema`
  replaces. `load` is what `ptah schema plan --schema-file` reads from the
  same documents, with the coverage they claim. `limits` are the
  `ptah:not-described` header entries Ptah sends a provider with the
  documents.
- `unsupported.txt` lists the accepted loads this provider refuses by name,
  with the construct it names. Ptah reads these statements into objects
  this provider has no model for: async replications, transfers, streaming
  queries, secrets, resource pools and resource pool classifiers. The suite
  fails when a listed load is accepted, and when an unlisted load is refused
  this way.
- `oracle/main.go` is the program that records the answers.

## Where the cases come from

- `<package>-<test>-NN` and `parse-<package>-<test>-NN` are the loads and the
  single documents Ptah's own tests read: `internal/ydbsql`,
  `internal/yqlparse`, and every other test that reaches the YDB owner.
  `live-ydb-NN` and `parse-live-ydb-NN` come from the live suite in
  `integration/dbschema/ydb`. They were collected once, by running those
  tests with a hook that wrote each load the owner read to a file.
- `render-<fixture>` is `ptah schema render --dialect ydb` of a fixture in
  `integration/internal/fixtures/entities` that renders for YDB.
- `fixture-053-*` to `fixture-055-*` are written by hand from the annotations
  of those fixtures: Ptah renders them only for a cluster with the matching
  feature flags on.
- `edge-*` are written by hand for what the other cases leave out: duplicate
  objects across documents, statements that continue in a later document,
  quoting, positions, limits and refusals.

## Recording the answers again

Record again after a change to Ptah's YQL schema reader, and record a new
case before its first test run. The oracle imports Ptah packages behind an
`internal/` boundary, so it runs from a checkout of Ptah:

```sh
host=$(mktemp -d)
git clone https://github.com/stokaro/ptah "$host"
git -C "$host" checkout a4e286dbf5ac42a609bd6ccd2b60dcc3335c210d
mkdir -p "$host/cmd/ydbsqlschemaoracle"
cp internal/yqlschema/testdata/oracle/main.go "$host/cmd/ydbsqlschemaoracle/main.go"
(cd "$host" && HOST_COMMIT=$(git rev-parse HEAD) go run ./cmd/ydbsqlschemaoracle "$OLDPWD/internal/yqlschema/testdata/corpus")
rm -rf "$host"
```

Run it from the root of this repository. It rewrites every
`<case>.golden.json` and leaves the documents alone. To move to another
Ptah commit, check that commit out instead, record, and update the commit
named above and in the provider README. Then run
`go test ./internal/yqlschema` and read the differences: a changed answer is
a change in Ptah's reader that this provider has to follow.

A new case is a new directory under `corpus/` with its documents, and a
`<case>.load.json` when its paths are absolute.
