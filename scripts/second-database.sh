#!/usr/bin/env bash
# second-database.sh <container> serves the database /local/db2 on the
# local-ydb server in <container>, beside its /local, for the tests whose dev
# database is a database of another name (PTAH_YDB_OTHER_DATABASE_TEST_URL).
# local-ydb serves one database, so the second one is a tenant with a dynamic
# node of its own: gRPC on 2137 and monitoring on 8767, which the job
# publishes on the same ports, since the server advertises them.
set -euo pipefail
container=${1:?usage: second-database.sh <container>}
docker exec "$container" /ydbd -s grpc://localhost:2136 admin database /local/db2 create hdd:1
docker exec -d "$container" bash -c 'mkdir -p /ydb_data/cluster/node_2 && exec /ydbd server --yaml-config=/ydb_data/cluster/kikimr_configs/config.yaml --tenant=/local/db2 --node-broker=grpc://localhost:2136 --grpc-port=2137 --mon-port=8767 --ic-port=19002 --tiny-mode > /ydb_data/cluster/node_2/out.log 2>&1'
for _ in $(seq 1 60); do
	if docker exec "$container" /ydb -e grpc://localhost:2137 -d /local/db2 scheme ls >/dev/null 2>&1; then
		printf 'database /local/db2 is served\n'
		exit 0
	fi
	sleep 2
done
docker exec "$container" tail -n 50 /ydb_data/cluster/node_2/out.log >&2 || true
printf 'database /local/db2 did not start\n' >&2
exit 1
