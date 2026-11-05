# Migration state the built-in YDB engine wrote

Ptah's built-in YDB engine, removed from Ptah after commit 74de0b65b, kept a
database's migration state in tables of its own layout: the native revision
table `schema_migrations`, its log `schema_migrations_log`, the tag table
`ptah_migration_tags`, and the Atlas-compatible revision table
`atlas_schema_revisions` that `ptah-compat migrate apply` writes. The tests in
`integration/builtin_history_live_test.go` create that state and run the host
over it through this provider.

- `tables.yql` is the engine's `CREATE TABLE` statements for those tables
  (`ydbRevisionTable` in `internal/builtindialects/ydb.go` at 74de0b65b), and
  the two tables the recorded migrations created.
- `migrations/` and `atlas/` are the migration directories the recording ran:
  the native one applies `1_create` and stops at the second statement of
  `2_more`; the Atlas one applies its one file.
- Each `<table>.jsonl` is the rows that engine wrote, one JSON object per
  row as `ydb yql --format json-unicode` prints it.

`record.sh` records the rows again. Run it from this directory with `ptah`
and `ptah-compat` built at 74de0b65b, against an empty local-ydb container:

```sh
PTAH=<ptah> PTAH_COMPAT=<ptah-compat> \
URL='ydb://127.0.0.1:2136/local?go_balancer=disable' CONTAINER=<container> \
./record.sh
```

The rows here were recorded on 25.1.4.7; the engine writes the same layout
on 26.2.1.14.
