#!/bin/bash
# Compare pgzero with native Postgres using pgbench.
#
# usage: bench.sh
#
# Both servers are the same Postgres version and configuration: the wasm
# build under pgzero-run, and the native build (build/native-install). Each
# gets a fresh cluster in a host directory, and the native pgbench talks to
# both over TCP. fsync is off for both, so that neither is measuring the
# disk.
#
# Environment:
#   SCALE     pgbench scale factor (default 10: 1M accounts, about 150MB)
#   CLIENTS   client counts to run (default "1 4 16")
#   DURATION  seconds per run (default 20)
#   MODES     "select" (pgbench -S) and/or "tpcb" (the default mix)
#   TARGETS   "native" and/or "pgzero"
#   PGZERO_RUN  the pgzero-run binary (default build/pgzero-run), to
#             compare builds of the host
set -uo pipefail
root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
cd "$root"

SCALE=${SCALE:-10}
CLIENTS=${CLIENTS:-"1 4 16"}
DURATION=${DURATION:-20}
MODES=${MODES:-"select tpcb"}
TARGETS=${TARGETS:-"native pgzero"}

port=5441
work="$root/build/bench"
bin="$root/build/native-install/usr/local/pgsql/bin"
results="$work/results.txt"
run=${PGZERO_RUN:-"$root/build/pgzero-run"}
# A DESTDIR install: the programs look for libpq under the real prefix.
export DYLD_LIBRARY_PATH="$bin/../lib" LD_LIBRARY_PATH="$bin/../lib"

rm -rf "$work"
mkdir -p "$work"
: > "$results"

configure() {
	cat >> "$1/postgresql.conf" <<EOF
listen_addresses = 'localhost'
port = $port
shared_buffers = 256MB
max_connections = 100
fsync = off
EOF
}

server=
stop_server() {
	if [ -n "$server" ]; then
		kill -INT "$server" 2>/dev/null
		wait "$server" 2>/dev/null
		server=
	fi
}
trap stop_server EXIT

wait_ready() {
	for _ in $(seq 1 60); do
		"$bin/psql" -X -h localhost -p $port -U postgres -d postgres \
			-c 'select 1' > /dev/null 2>&1 && return 0
		sleep 1
	done
	echo "server did not start; see $work/$1/server.log" >&2
	exit 1
}

start_native() {
	local d="$work/native"
	"$bin/initdb" -D "$d/data" -U postgres -E UTF8 --no-locale -N --no-instructions \
		> "$d.initdb.log" 2>&1 || { cat "$d.initdb.log"; exit 1; }
	configure "$d/data"
	"$bin/postgres" -D "$d/data" > "$d/server.log" 2>&1 &
	server=$!
}

start_pgzero() {
	local d="$work/pgzero"
	mkdir -p "$d/tmp"
	"$run" -data "$d/data" -tmp "$d/tmp" \
		-- initdb -D /data -U postgres -E UTF8 --no-locale -N --no-instructions \
		> "$d.initdb.log" 2>&1 || { cat "$d.initdb.log"; exit 1; }
	configure "$d/data"
	# Directly, so that $! is pgzero-run and the SIGINT reaches it.
	"$run" -data "$d/data" -tmp "$d/tmp" \
		-- postgres -D /data > "$d/server.log" 2>&1 &
	server=$!
}

pgbench() {
	"$bin/pgbench" -h localhost -p $port -U postgres "$@" postgres
}

for target in $TARGETS; do
	echo "== $target"
	mkdir -p "$work/$target"
	"start_$target"
	wait_ready "$target"

	start=$(date +%s)
	pgbench -i -q -s "$SCALE" > "$work/$target/init.log" 2>&1 ||
		{ cat "$work/$target/init.log"; exit 1; }
	echo "   pgbench -i -s $SCALE: $(( $(date +%s) - start ))s"

	for mode in $MODES; do
		case $mode in
		select) flags=(-b select-only) ;;
		tpcb) flags=(-b tpcb-like) ;;
		*) echo "unknown mode $mode" >&2; exit 2 ;;
		esac
		for c in $CLIENTS; do
			log="$work/$target/$mode-c$c.log"
			pgbench "${flags[@]}" -M prepared -c "$c" -j "$(( c < 8 ? c : 8 ))" \
				-T "$DURATION" > "$log" 2>&1 || { cat "$log"; exit 1; }
			tps=$(sed -nE 's/^tps = ([0-9.]+).*/\1/p' "$log")
			lat=$(sed -nE 's/^latency average = ([0-9.]+) ms/\1/p' "$log")
			printf '   %-6s c=%-3s %10.0f tps  %8.3f ms\n' "$mode" "$c" "$tps" "$lat"
			echo "$target $mode $c $tps $lat" >> "$results"
		done
	done
	stop_server
done

# Side by side, when both ran.
if [[ " $TARGETS " == *" native "* && " $TARGETS " == *" pgzero "* ]]; then
	echo
	printf '%-6s %4s %12s %12s %8s\n' mode c native pgzero ratio
	awk '
		$1 == "native" { n[$2 " " $3] = $4; order[++k] = $2 " " $3 }
		$1 == "pgzero" { z[$2 " " $3] = $4 }
		END {
			for (i = 1; i <= k; i++) {
				split(order[i], f, " ")
				printf "%-6s %4s %12.0f %12.0f %7.1f%%\n", f[1], f[2], n[order[i]], z[order[i]],
					100 * z[order[i]] / n[order[i]]
			}
		}' "$results"
fi
