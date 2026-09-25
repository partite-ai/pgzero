#!/bin/bash
# Run a regression suite against the wasm server.
#
# usage: regress.sh [pg_regress arguments...]
#
# With no arguments, runs Postgres's main suite (parallel_schedule).
# Otherwise the arguments select the tests, e.g. for an extension's suite:
#   regress.sh --inputdir=pgvector/test --load-extension=vector vector_type ...
#
# pg_regress and psql are native builds (build/native) acting as clients,
# like `make installcheck`. The server runs under pgzero with a fresh
# cluster. The repository is mounted into the guest at its host path, so
# that tests which have the server read or write files by absolute path
# (COPY ... FROM ':abs_srcdir/data/...') find them.
set -uo pipefail
root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
cd "$root"

port=5440
work="$root/build/regress"
native="$root/build/native"
regress_src="$root/postgres/src/test/regress"

rm -rf "$work"
mkdir -p "$work/tmp" "$work/out"

pgzero() {
	"$root/build/pgzero-run" -data "$work/data" -tmp "$work/tmp" \
		-mount "$root=$root" "$@"
}

echo "== initdb"
pgzero -- initdb -D /data -U postgres -E UTF8 --no-locale -N --no-instructions \
	> "$work/initdb.log" 2>&1 || { cat "$work/initdb.log"; exit 1; }

# The settings pg_regress uses for its own temporary installations.
cat >> "$work/data/postgresql.conf" <<EOF
listen_addresses = 'localhost'
port = $port
log_autovacuum_min_duration = 0
log_checkpoints = on
log_line_prefix = '%m %b[%p] %q%a '
log_lock_waits = on
log_temp_files = 128kB
max_prepared_transactions = 2
EOF

echo "== starting server"
# Not through the pgzero function: $! must be the pgzero process itself, so
# that the SIGINT below (fast shutdown) reaches it.
"$root/build/pgzero-run" -data "$work/data" -tmp "$work/tmp" -mount "$root=$root" \
	-- postgres -D /data > "$work/server.log" 2>&1 &
server=$!
stop_server() {
	kill -INT "$server" 2>/dev/null
	wait "$server"
}
trap stop_server EXIT

for _ in $(seq 1 60); do
	"$native/src/bin/psql/psql" -X -h localhost -p $port -U postgres -d postgres \
		-c 'select 1' > /dev/null 2>&1 && break
	sleep 1
done

if [ $# -eq 0 ]; then
	set -- --inputdir="$regress_src" --expecteddir="$regress_src" \
		--schedule="$regress_src/parallel_schedule"
fi

echo "== pg_regress $*"
start=$(date +%s)
# From the output directory, as pg_regress runs from a make build
# directory: some suites have psql write results/... relative to it.
cd "$work/out"
"$native/src/test/regress/pg_regress" \
	--host=localhost --port=$port --user=postgres \
	--bindir="$native/src/bin/psql" \
	--dlpath=/usr/local/pgsql/lib \
	--outputdir="$work/out" \
	--max-connections=20 \
	"$@"
status=$?
echo "== pg_regress exited $status after $(( $(date +%s) - start ))s; diffs: $work/out/regression.diffs"
exit $status
