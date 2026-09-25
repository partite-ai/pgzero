#!/bin/bash
# Link the postgres binary with its loadable modules built in.
#
# usage: link-postgres.sh <meson build dir> <output> <module.so.p dir>...
#
# <output> is relative to the build dir.
#
# meson links postgres without modules (they are separate shared modules,
# which on wasm are only placeholders; see wasm/toolchain/clang). This relinks it
# with meson's own link command, adding every module's objects, a private
# copy of libpq (for libpqwalreceiver, dblink and postgres_fdw) and the
# generated table that libpgzero's dlopen() uses.
set -euo pipefail
root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
build="$1"; out="$2"; shift 2

# Module directories may be anywhere (extensions from outside the tree
# are built elsewhere): make them absolute before changing directory.
dirs=()
for d in "$@"; do dirs+=("$(cd "$d" && pwd)"); done

cd "$build"
# meson's link command for postgres. Where command lines are short (macOS)
# it keeps the arguments in a response file, which ninja deletes unless
# told to keep it; elsewhere (Linux) they are on the command line.
if [ ! -f src/backend/postgres.rsp ]; then
	rm -f src/backend/postgres
fi
ninja -d keeprsp src/backend/postgres >/dev/null
link_cmd="$(ninja -t commands src/backend/postgres | tail -1)"
if [[ "$link_cmd" == *"@src/backend/postgres.rsp"* ]]; then
	link_args="$(cat src/backend/postgres.rsp)"
else
	link_args="${link_cmd#* }" # drop the compiler
fi

mods=()
objs=()
for d in "${dirs[@]}"; do
	mods+=("$d")
	for o in "$d"/*.o; do objs+=("$o"); done
done

go run "$root/wasm/cmd/genmodules" -o pgzero-modules.c "${mods[@]}"
"$root/wasm/toolchain/clang" -O2 -isystem "$root/wasm/libpgzero/include" -c pgzero-modules.c -o pgzero-modules.o

# libpq (for libpqwalreceiver, dblink and postgres_fdw) must use its own
# frontend builds of libpgcommon/libpgport, not the backend's: they differ
# (memory allocation, the link canary libpq checks at connect time, ...).
# Partial-link libpq with them into one object, then make everything in it
# local except libpq's exported API, so it neither collides with nor binds
# to the backend's versions of those functions.
"$root/.tools/wasi-sdk/bin/wasm-ld" -r -o libpq-combined.o \
	--whole-archive src/interfaces/libpq/libpq.a --no-whole-archive \
	src/common/libpgcommon_shlib.a src/port/libpgport_shlib.a
go run "$root/wasm/cmd/wasmlocalize" -keep "$root/postgres/src/interfaces/libpq/exports.txt" \
	libpq-combined.o libpq-private.o

printf '%s\n' "$link_args" |
	sed -E 's#(^| )-o src/backend/postgres( |$)#\1-o '"$out"'\2#' > postgres-full.rsp
"$root/wasm/toolchain/clang" @postgres-full.rsp "${objs[@]}" pgzero-modules.o \
	libpq-private.o
