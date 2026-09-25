package pgzero_test

import (
	"context"
	"math/rand/v2"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/partite-ai/pgzero"
	"github.com/partite-ai/pgzero/pgzerox"
)

// benchRows is the size of the table the point-query benchmarks use:
// pgbench's pgbench_accounts at scale 1.
const benchRows = 100_000

// benchServer starts an in-memory server with a pgbench_accounts-like table.
func benchServer(b *testing.B) *pgzero.Server {
	b.Helper()
	srv := start(b, pgzero.Config{})
	conn := connect(b, srv)
	for _, sql := range []string{
		`create table accounts (aid int primary key, bid int, abalance int, filler char(84))`,
		`insert into accounts select g, 1, 0, '' from generate_series(1, 100000) g`,
		`vacuum analyze accounts`, // not in a transaction, so a statement of its own
	} {
		if _, err := conn.Exec(context.Background(), sql); err != nil {
			b.Fatal(err)
		}
	}
	return srv
}

// BenchmarkSelect1 is the cost of one round trip: the in-memory
// connection, the protocol, and the executor's fixed overhead.
func BenchmarkSelect1(b *testing.B) {
	ctx := context.Background()
	conn := connect(b, start(b, pgzero.Config{}))
	var n int
	for b.Loop() {
		if err := conn.QueryRow(ctx, "select 1").Scan(&n); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkPointSelect is pgbench's select-only transaction.
func BenchmarkPointSelect(b *testing.B) {
	ctx := context.Background()
	conn := connect(b, benchServer(b))
	var bal int
	for b.Loop() {
		aid := rand.IntN(benchRows) + 1
		if err := conn.QueryRow(ctx, "select abalance from accounts where aid = $1", aid).Scan(&bal); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkUpdate is a single-row update in its own transaction, so it
// includes the commit (WAL insert and flush).
func BenchmarkUpdate(b *testing.B) {
	ctx := context.Background()
	conn := connect(b, benchServer(b))
	for b.Loop() {
		aid := rand.IntN(benchRows) + 1
		if _, err := conn.Exec(ctx, "update accounts set abalance = abalance + 1 where aid = $1", aid); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkPointSelectParallel runs point selects from GOMAXPROCS
// connections (each its own backend process) at once.
func BenchmarkPointSelectParallel(b *testing.B) {
	ctx := context.Background()
	srv := benchServer(b)
	cfg := pgzerox.PoolConfig(srv, "postgres")
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		b.Fatal(err)
	}
	defer pool.Close()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		var bal int
		for pb.Next() {
			aid := rand.IntN(benchRows) + 1
			if err := pool.QueryRow(ctx, "select abalance from accounts where aid = $1", aid).Scan(&bal); err != nil {
				b.Error(err)
				return
			}
		}
	})
}

// BenchmarkAggregate is CPU-bound work in one backend: a full scan with
// grouping and sorting.
func BenchmarkAggregate(b *testing.B) {
	ctx := context.Background()
	conn := connect(b, benchServer(b))
	// Keep it in one process, so this measures compiled code rather than
	// parallel query.
	if _, err := conn.Exec(ctx, "set max_parallel_workers_per_gather = 0"); err != nil {
		b.Fatal(err)
	}
	var n int
	for b.Loop() {
		if err := conn.QueryRow(ctx, `
			select count(*) from (
				select aid % 1000 k, sum(abalance), max(md5(aid::text))
				from accounts group by 1 order by 3) s`).Scan(&n); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkStart is the cost of starting a server (and shutting it down),
// once the engine is loaded.
func BenchmarkStart(b *testing.B) {
	ctx := context.Background()
	start(b, pgzero.Config{}) // load the engine
	for b.Loop() {
		srv, err := pgzero.Start(ctx, pgzero.Config{})
		if err != nil {
			b.Fatal(err)
		}
		if err := srv.Close(); err != nil {
			b.Fatal(err)
		}
	}
}
