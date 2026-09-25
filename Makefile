# Builds the WebAssembly Postgres that the pgzero library embeds.
#
# Using the library needs none of this: internal/assets holds the build
# output, committed. These targets are for changing that build.
#
#   make tools      fetch the wasi-sdk and wasm-tools into .tools/
#   make assets     build Postgres and regenerate internal/assets
#   make gen        regenerate the Go host bindings and ABI constants
#   make test       run the library's tests
#   make regress    run Postgres's main regression suite against the wasm build
#   make regress-pgvector   the same for pgvector's suite
#   make pg-patch   re-export wasm/patches after editing the postgres/ checkout
#
# `make tools` fetches the wasi-sdk and wasm-tools. Other host tools needed:
# go, meson, ninja, python3, a C compiler (for the native regression test
# clients), and bison/flex/perl for Postgres itself.

ROOT     := $(abspath .)
B        := build
WASI_SDK := .tools/wasi-sdk
CC       := $(WASI_SDK)/bin/clang --target=wasm32-wasip2

PG_TAG   := REL_18_6
PG_SRC   := postgres
PG_BUILD := $(B)/pg
PG_PATCH := wasm/patches/postgres-$(PG_TAG).patch
WIT      := wasm/wit/pgzero.wit

.PHONY: tools assets gen test regress regress-pgvector pg-patch pg pg-setup pg-build pg-install pgzero-run native native-install bench bench-go

# ---- Toolchain ----

WASI_SDK_VERSION := 34
WASI_SDK_ARCH    := $(shell uname -m | sed 's/aarch64/arm64/')
WASI_SDK_OS      := $(shell uname -s | tr A-Z a-z | sed 's/darwin/macos/')
WASI_SDK_NAME    := wasi-sdk-$(WASI_SDK_VERSION).0-$(WASI_SDK_ARCH)-$(WASI_SDK_OS)

WASM_TOOLS_VERSION := 1.254.0
WASM_TOOLS_ARCH    := $(shell uname -m | sed 's/arm64/aarch64/')
WASM_TOOLS_NAME    := wasm-tools-$(WASM_TOOLS_VERSION)-$(WASM_TOOLS_ARCH)-$(WASI_SDK_OS)
WASM_TOOLS         := .tools/wasm-tools

tools: $(WASI_SDK)/bin/clang $(WASM_TOOLS)
$(WASI_SDK)/bin/clang:
	mkdir -p .tools
	curl -sSfL https://github.com/WebAssembly/wasi-sdk/releases/download/wasi-sdk-$(WASI_SDK_VERSION)/$(WASI_SDK_NAME).tar.gz \
		| tar -xz -C .tools
	ln -sfn $(WASI_SDK_NAME) $(WASI_SDK)
$(WASM_TOOLS):
	mkdir -p .tools
	curl -sSfL https://github.com/bytecodealliance/wasm-tools/releases/download/v$(WASM_TOOLS_VERSION)/$(WASM_TOOLS_NAME).tar.gz \
		| tar -xz -C .tools
	ln -sfn $(WASM_TOOLS_NAME)/wasm-tools $(WASM_TOOLS)

# ---- Code generation ----

gen:
	rm -rf internal/gen
	go tool wacogo-witgen generate -w pgzero:host/guest -o ./internal/gen \
		-p github.com/partite-ai/pgzero/internal/gen ./$(WIT)
	go generate ./internal/abi

# ---- libpgzero: the guest-side POSIX layer ----

LIBPGZERO_CFLAGS := -O2 -g -Wall -mllvm -wasm-enable-sjlj -mllvm -wasm-use-legacy-eh=false \
	-matomics -mbulk-memory -isystem wasm/libpgzero/include \
	-D_WASI_EMULATED_MMAN -D_WASI_EMULATED_PROCESS_CLOCKS -D_WASI_EMULATED_GETPID
LIBPGZERO_SRCS   := $(wildcard wasm/libpgzero/src/*.c)
LIBPGZERO_OBJS   := $(patsubst wasm/libpgzero/src/%.c,$(B)/libpgzero/%.o,$(LIBPGZERO_SRCS))
LIBPGZERO_DEPS   := $(wildcard wasm/libpgzero/src/*.h wasm/libpgzero/include/*.h wasm/libpgzero/include/*/*.h)
LIBPGZERO        := $(B)/libpgzero/libpgzero.a

$(B)/libpgzero/%.o: wasm/libpgzero/src/%.c $(LIBPGZERO_DEPS) | tools
	@mkdir -p $(@D)
	$(CC) $(LIBPGZERO_CFLAGS) -c $< -o $@

$(LIBPGZERO): $(LIBPGZERO_OBJS)
	rm -f $@
	$(WASI_SDK)/bin/llvm-ar rcs $@ $^

# ---- Postgres ----

CROSS      := $(B)/wasm32-wasip2.cross
# b_asneeded/b_lundef: wasm-ld has neither --as-needed nor --no-undefined,
# which meson adds by default on some platforms (e.g. Linux). They must be
# set here: older meson versions ignore them in a cross file.
PG_OPTIONS := --buildtype=debugoptimized -Db_asneeded=false -Db_lundef=false \
	-Dauto_features=disabled -Dreadline=disabled \
	-Dzlib=disabled -Dssl=none -Dnls=disabled -Dtap_tests=disabled -Dplperl=disabled \
	-Dplpython=disabled -Dpltcl=disabled -Dllvm=disabled -Dicu=disabled

# The checkout is patched from wasm/patches, the source of truth.
$(PG_SRC)/meson.build:
	git clone -q --depth 1 --branch $(PG_TAG) https://github.com/postgres/postgres $(PG_SRC)
	git -C $(PG_SRC) checkout -q -b pgzero
	git -C $(PG_SRC) apply $(ROOT)/$(PG_PATCH)

pg-patch:
	git -C $(PG_SRC) add -N .
	git -C $(PG_SRC) diff > $(PG_PATCH)
	git -C $(PG_SRC) reset -q

$(CROSS): wasm/toolchain/wasm32-wasip2.cross.in
	@mkdir -p $(B)
	sed 's|@ROOT@|$(ROOT)|g' $< > $@

pg-setup: $(CROSS) $(LIBPGZERO) $(PG_SRC)/meson.build
	rm -rf $(PG_BUILD)
	cd $(PG_SRC) && meson setup $(ROOT)/$(PG_BUILD) --cross-file $(ROOT)/$(CROSS) $(PG_OPTIONS)

$(PG_BUILD)/build.ninja:
	$(MAKE) pg-setup

# ninja doesn't know that programs link libpgzero.a (it comes in through the
# cross file's link arguments), so relink them when it changes.
$(PG_BUILD)/.libpgzero-stamp: $(LIBPGZERO) $(PG_BUILD)/build.ninja
	find $(PG_BUILD) -type f -perm -u+x \( -path '*/src/bin/*' -o -path '*/src/backend/postgres' \) \
		! -name '*.*' -delete
	touch $@

# Build everything that builds (tools that need fork() don't), then insist
# on what we need.
PG_REQUIRED := src/backend/postgres src/bin/initdb/initdb src/interfaces/libpq/libpq.a \
	src/common/libpgcommon_shlib.a src/port/libpgport_shlib.a
pg-build: $(PG_BUILD)/.libpgzero-stamp
	-ninja -C $(PG_BUILD) -k 0 > $(B)/pg-build.log 2>&1
	ninja -C $(PG_BUILD) $(PG_REQUIRED)

# ---- Extensions from outside Postgres's tree ----
#
# Compiled like Postgres's own loadable modules: objects go in
# $(EXT)/<module>.so.p/, where the clang wrapper gives the module's entry
# points module-specific names, and they are linked into postgres with the
# rest.

EXT          := $(B)/ext
EXT_CC       := wasm/toolchain/clang
EXT_CFLAGS   := -I$(PG_BUILD)/src/include -I$(PG_SRC)/src/include -D_FILE_OFFSET_BITS=64 \
	-O2 -g -fno-strict-aliasing -fwrapv -fexcess-precision=standard -D_GNU_SOURCE -fvisibility=hidden \
	-mllvm -wasm-enable-sjlj -mllvm -wasm-use-legacy-eh=false -matomics -mbulk-memory \
	-isystem wasm/libpgzero/include -D_WASI_EMULATED_MMAN -D_WASI_EMULATED_PROCESS_CLOCKS \
	-D_WASI_EMULATED_GETPID

PGVECTOR_TAG := v0.8.6
pgvector/Makefile:
	git clone -q --depth 1 --branch $(PGVECTOR_TAG) https://github.com/pgvector/pgvector pgvector

# pgvector's own flags (minus -march=native), plus wasm SIMD so that its
# distance loops vectorize.
.PHONY: ext
ext: pg-build pgvector/Makefile
	rm -rf $(EXT)
	mkdir -p $(EXT)/vector.so.p $(EXT)/pgcrypto_lite.so.p
	for f in pgvector/src/*.c; do \
		$(EXT_CC) $(EXT_CFLAGS) -msimd128 -ftree-vectorize -fassociative-math -fno-signed-zeros \
			-fno-trapping-math -Ipgvector/src -c $$f -o $(EXT)/vector.so.p/$$(basename $$f).o || exit 1; \
	done
	$(EXT_CC) $(EXT_CFLAGS) -c wasm/extensions/pgcrypto_lite/pgcrypto_lite.c -o $(EXT)/pgcrypto_lite.so.p/pgcrypto_lite.c.o

# postgres with its loadable modules linked in (see wasm/scripts/link-postgres.sh).
PG_MODULE_DIRS = $(shell find $(PG_BUILD)/src/backend $(PG_BUILD)/src/pl $(PG_BUILD)/contrib \
	$(PG_BUILD)/src/test/regress $(EXT) -name '*.so.p' -type d | sort)

$(PG_BUILD)/postgres-full: pg-build ext
	wasm/scripts/link-postgres.sh $(PG_BUILD) postgres-full $(PG_MODULE_DIRS)

# Development components, with debug info. The core modules import
# pgzero:host/* directly; embed the WIT next to wasi-libc's component type.
$(B)/bin/postgres.wasm: $(PG_BUILD)/postgres-full $(WIT)
	@mkdir -p $(@D)
	$(WASM_TOOLS) component embed $(WIT) --world pgzero:host/guest $< -o $(B)/postgres.embed.wasm
	$(WASM_TOOLS) component new $(B)/postgres.embed.wasm -o $@

$(B)/bin/initdb.wasm: pg-build $(WIT)
	@mkdir -p $(@D)
	$(WASM_TOOLS) component embed $(WIT) --world pgzero:host/guest $(PG_BUILD)/src/bin/initdb/initdb -o $(B)/initdb.embed.wasm
	$(WASM_TOOLS) component new $(B)/initdb.embed.wasm -o $@

PGLIB  := $(B)/install/usr/local/pgsql/lib
PGEXT  := $(B)/install/usr/local/pgsql/share/extension

pg-install: pg-build pgvector/Makefile
	rm -rf $(B)/install
	wasm/scripts/install.py $(PG_BUILD) $(B)/install
	@# Extensions from outside the tree: their SQL, and (as for every
	@# module) a placeholder where Postgres expects the shared library.
	cp pgvector/vector.control pgvector/sql/vector--*--*.sql $(PGEXT)/
	cp pgvector/sql/vector.sql $(PGEXT)/vector--$(PGVECTOR_TAG:v%=%).sql
	cp wasm/extensions/pgcrypto_lite/pgcrypto_lite.control wasm/extensions/pgcrypto_lite/*.sql $(PGEXT)/
	for m in vector pgcrypto_lite; do cp $(PGLIB)/plpgsql.so $(PGLIB)/$$m.so; done
	@# The regression tests load their test module as $$libdir/regress with
	@# the client platform's suffix; Postgres checks the file exists.
	for sfx in .so .dylib; do \
		cp $(PGLIB)/plpgsql.so $(PGLIB)/regress$$sfx; \
	done

# pgzero-run runs the wasm programs with host directories mounted; the build
# and the regression tests use it.
pgzero-run:
	go build -o $(B)/pgzero-run ./wasm/cmd/pgzero-run

pg: $(B)/bin/postgres.wasm $(B)/bin/initdb.wasm pg-install pgzero-run

# ---- Assets: what the library embeds ----

EMBED  := $(B)/embed
ASSETS := internal/assets
PREFIX := $(B)/install/usr/local/pgsql

assets: pg
	rm -rf $(EMBED)
	mkdir -p $(EMBED)/install/bin $(EMBED)/install/lib
	@# Components without DWARF (function names stay, for crash traces).
	for p in postgres:$(PG_BUILD)/postgres-full initdb:$(PG_BUILD)/src/bin/initdb/initdb; do \
		n=$${p%%:*}; f=$${p#*:}; \
		$(WASI_SDK)/bin/llvm-strip --strip-debug -o $(EMBED)/$$n.core.wasm $$f && \
		$(WASM_TOOLS) component embed $(WIT) --world pgzero:host/guest $(EMBED)/$$n.core.wasm -o $(EMBED)/$$n.embed.wasm && \
		$(WASM_TOOLS) component new $(EMBED)/$$n.embed.wasm -o $(EMBED)/$$n.wasm && \
		gzip -9 -n -c $(EMBED)/$$n.wasm > $(ASSETS)/$$n.wasm.gz || exit 1; \
	done
	@# What the server needs of the install tree: share/, the loadable
	@# module placeholders, and files where Postgres looks for its binaries.
	cp -R $(PREFIX)/share $(EMBED)/install/
	cp $(PREFIX)/lib/*.so $(EMBED)/install/lib/
	: > $(EMBED)/install/bin/postgres
	: > $(EMBED)/install/bin/initdb
	go run ./wasm/cmd/mktar -o $(ASSETS)/install.tar.gz $(EMBED)/install
	@# A freshly initialized cluster, so servers start without running initdb.
	$(B)/pgzero-run -programs $(B)/bin -data $(EMBED)/template -- \
		initdb -D /data -U postgres -E UTF8 --no-locale -A trust -N --no-instructions
	go run ./wasm/cmd/mktar -o $(ASSETS)/template.tar.gz $(EMBED)/template
	ls -la $(ASSETS)

# ---- Tests ----

test:
	go test ./...

# Postgres's main suite, with native pg_regress and psql as clients.
NATIVE := $(B)/native
native: $(PG_SRC)/meson.build
	test -d $(NATIVE) || (cd $(PG_SRC) && meson setup $(ROOT)/$(NATIVE) $(PG_OPTIONS))
	ninja -C $(NATIVE) src/bin/psql/psql src/test/regress/pg_regress

regress: pg native
	wasm/scripts/regress.sh

# ---- Benchmarks ----

# pgbench against pgzero and native Postgres (wasm/scripts/bench.sh; SCALE,
# CLIENTS, DURATION, MODES and TARGETS adjust it).
native-install: native
	ninja -C $(NATIVE)
	meson install -C $(NATIVE) --no-rebuild --quiet --destdir $(ROOT)/$(B)/native-install

bench: pg native-install
	wasm/scripts/bench.sh

# The library's benchmarks: in memory, through pgx.
bench-go:
	go test -run '^$$' -bench . -benchtime 2s .

# pgvector's suite, as its Makefile runs it (installcheck).
regress-pgvector: pg native
	wasm/scripts/regress.sh --inputdir=$(ROOT)/pgvector/test --load-extension=vector \
		$$(ls pgvector/test/sql | sed 's/\.sql$$//')
