#!/usr/bin/env bash
# Reproduces the measurements in ../filtered-consumer-queries.md.
# Needs Docker. Writes out-*.txt next to this script and removes the containers.
set -euo pipefail
cd "$(dirname "$0")"
versions=(15 17 18)
for v in "${versions[@]}"; do
  docker run -d --rm --name "dwx227-pg$v" -e POSTGRES_PASSWORD=pw "postgres:$v" \
    -c shared_buffers=512MB -c work_mem=16MB -c max_wal_size=4GB >/dev/null
done
trap 'for v in "${versions[@]}"; do docker rm -f "dwx227-pg$v" >/dev/null 2>&1 || true; done' EXIT
sleep 10
psql_in() { docker exec -i "dwx227-pg$1" psql -U postgres -q -v ON_ERROR_STOP=1; }
for v in "${versions[@]}"; do
  psql_in "$v" < setup.sql >/dev/null
  # Each bench runs twice; the second (warm) run is kept.
  psql_in "$v" < bench.sql >/dev/null;       psql_in "$v" < bench.sql       > "out-pg$v.txt"
  psql_in "$v" < skew.sql >/dev/null
  psql_in "$v" < bench-skew.sql >/dev/null;  psql_in "$v" < bench-skew.sql  > "out-skew-pg$v.txt"
  psql_in "$v" < bench-extra.sql >/dev/null; psql_in "$v" < bench-extra.sql > "out-extra-pg$v.txt"
  psql_in "$v" < pending-function.sql
  docker exec -i "dwx227-pg$v" psql -U postgres -q < pending-function-test.sql > "out-function-pg$v.txt" 2>&1
done
