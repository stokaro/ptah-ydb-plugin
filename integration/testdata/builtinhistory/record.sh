#!/bin/sh
# record.sh records what Ptah's built-in YDB engine writes for migration
# state, for the tests in integration/builtin_history_live_test.go.
#
#   PTAH=<ptah at 74de0b65b> PTAH_COMPAT=<ptah-compat at 74de0b65b> \
#   URL='ydb://127.0.0.1:2136/local?go_balancer=disable' CONTAINER=<local-ydb container> \
#   ./record.sh
#
# It runs in this directory against an empty database and overwrites the
# *.jsonl files.
set -eu
ydb() { docker exec "$CONTAINER" /ydb -e grpc://localhost:2136 -d /local "$@"; }
for directory in ptah_builtin_history ptah_builtin_history_atlas; do
	ydb scheme rmdir --recursive --force "$directory" >/dev/null 2>&1 || true
done
here=$(pwd)
# The log records the user a run is made as; the recording names a neutral one.
export USER=ptah LOGNAME=ptah
"$PTAH" migrations up --db-url "$URL" --migrations-dir "$here/migrations" --migrations-schema ptah_builtin_history || true
"$PTAH" migrations tag --db-url "$URL" --migrations-schema ptah_builtin_history v1 --version 1
(cd atlas && "$PTAH_COMPAT" migrate hash --dir file://.)
"$PTAH_COMPAT" migrate apply --url "$URL" --dir "file://$here/atlas" --revisions-schema ptah_builtin_history_atlas
dump() { ydb yql -s "SELECT * FROM \`$1\` ORDER BY $2" --format json-unicode > "$3"; }
dump ptah_builtin_history/schema_migrations version schema_migrations.jsonl
dump ptah_builtin_history/schema_migrations_log 'run_id, seq' schema_migrations_log.jsonl
dump ptah_builtin_history/ptah_migration_tags tag ptah_migration_tags.jsonl
dump ptah_builtin_history_atlas/atlas_schema_revisions version atlas_schema_revisions.jsonl
