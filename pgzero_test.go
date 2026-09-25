package pgzero_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"

	"github.com/partite-ai/pgzero"
	"github.com/partite-ai/pgzero/pgzerox"
)

func start(t testing.TB, cfg pgzero.Config) *pgzero.Server {
	t.Helper()
	if testing.Verbose() && cfg.Logs == nil {
		cfg.Logs = os.Stderr
	}
	srv, err := pgzero.Start(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := srv.Close(); err != nil {
			t.Error(err)
		}
	})
	return srv
}

func connect(t testing.TB, srv *pgzero.Server) *pgx.Conn {
	t.Helper()
	ctx := context.Background()
	conn, err := pgx.ConnectConfig(ctx, pgzerox.ConnConfig(srv, "postgres"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close(ctx) })
	return conn
}

func TestInMemory(t *testing.T) {
	ctx := context.Background()
	srv := start(t, pgzero.Config{})
	conn := connect(t, srv)

	var version string
	if err := conn.QueryRow(ctx, "select version()").Scan(&version); err != nil {
		t.Fatal(err)
	}
	t.Log(version)

	if _, err := conn.Exec(ctx, `
		create table t (id int primary key, v text);
		insert into t select g, md5(g::text) from generate_series(1, 1000) g`); err != nil {
		t.Fatal(err)
	}
	var n, sum int
	if err := conn.QueryRow(ctx, "select count(*), sum(id) from t").Scan(&n, &sum); err != nil {
		t.Fatal(err)
	}
	if n != 1000 || sum != 500500 {
		t.Fatalf("count=%d sum=%d", n, sum)
	}

	// Errors come back as Postgres errors.
	_, err := conn.Exec(ctx, "insert into t values (1, 'duplicate')")
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "23505" {
		t.Fatalf("want unique_violation, got %v", err)
	}
}

func TestPool(t *testing.T) {
	ctx := context.Background()
	srv := start(t, pgzero.Config{})
	cfg := pgzerox.PoolConfig(srv, "postgres")
	cfg.MaxConns = 8
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()

	if _, err := pool.Exec(ctx, "create table counters (id int primary key, n int); insert into counters values (1, 0)"); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 25 {
				if _, err := pool.Exec(ctx, "update counters set n = n + 1 where id = 1"); err != nil {
					t.Error(err)
					return
				}
			}
		}()
	}
	wg.Wait()
	var n int
	if err := pool.QueryRow(ctx, "select n from counters").Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 400 {
		t.Fatalf("n = %d, want 400", n)
	}
}

func TestCancel(t *testing.T) {
	ctx := context.Background()
	srv := start(t, pgzero.Config{})
	conn := connect(t, srv)

	done := make(chan error, 1)
	go func() {
		_, err := conn.Exec(ctx, "select pg_sleep(60)")
		done <- err
	}()
	time.Sleep(200 * time.Millisecond)
	// The cancel request is a second connection, through the same dialer.
	if err := conn.PgConn().CancelRequest(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != "57014" {
			t.Fatalf("want query_canceled, got %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("query was not cancelled")
	}
}

func TestDatabaseSQL(t *testing.T) {
	srv := start(t, pgzero.Config{})
	db := stdlib.OpenDB(*pgzerox.ConnConfig(srv, "postgres"))
	defer db.Close()
	var s string
	if err := db.QueryRow("select 'via database/sql'").Scan(&s); err != nil {
		t.Fatal(err)
	}
	if s != "via database/sql" {
		t.Fatalf("got %q", s)
	}
}

func TestDataDirPersists(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()

	srv, err := pgzero.Start(ctx, pgzero.Config{DataDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	conn, err := pgx.ConnectConfig(ctx, pgzerox.ConnConfig(srv, "postgres"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(ctx, "create table kept (v text); insert into kept values ('still here')"); err != nil {
		t.Fatal(err)
	}
	conn.Close(ctx)
	if err := srv.Close(); err != nil {
		t.Fatal(err)
	}

	srv = start(t, pgzero.Config{DataDir: dir})
	var v string
	if err := connect(t, srv).QueryRow(ctx, "select v from kept").Scan(&v); err != nil {
		t.Fatal(err)
	}
	if v != "still here" {
		t.Fatalf("got %q", v)
	}
}

func TestTCP(t *testing.T) {
	ctx := context.Background()
	srv := start(t, pgzero.Config{TCP: true})
	if srv.TCPAddr() == "" {
		t.Fatal("no TCP address")
	}
	// An ordinary connection string, as any client would use.
	conn, err := pgx.Connect(ctx, fmt.Sprintf("postgres://postgres@%s/postgres?sslmode=disable", srv.TCPAddr()))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	var one int
	if err := conn.QueryRow(ctx, "select 1").Scan(&one); err != nil {
		t.Fatal(err)
	}
}

func TestServersAreIsolated(t *testing.T) {
	ctx := context.Background()
	a := connect(t, start(t, pgzero.Config{}))
	b := connect(t, start(t, pgzero.Config{}))
	if _, err := a.Exec(ctx, "create table only_in_a (x int)"); err != nil {
		t.Fatal(err)
	}
	var exists bool
	if err := b.QueryRow(ctx, "select to_regclass('only_in_a') is not null").Scan(&exists); err != nil {
		t.Fatal(err)
	}
	if exists {
		t.Fatal("table created in one server is visible in another")
	}
}

func TestPgvector(t *testing.T) {
	ctx := context.Background()
	conn := connect(t, start(t, pgzero.Config{}))
	if _, err := conn.Exec(ctx, `
		create extension vector;
		create table items (id int primary key, embedding vector(3));
		insert into items select g, array[g, g * 2, g % 7]::vector from generate_series(1, 500) g;
		create index on items using hnsw (embedding vector_l2_ops)`); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(ctx, "set enable_seqscan = off"); err != nil {
		t.Fatal(err)
	}
	rows, err := conn.Query(ctx, "explain select id from items order by embedding <-> '[10,20,3]' limit 3")
	if err != nil {
		t.Fatal(err)
	}
	lines, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatal(err)
	}
	plan := strings.Join(lines, "\n")
	if !strings.Contains(plan, "Index Scan") {
		t.Errorf("nearest-neighbour query does not use the HNSW index: %s", plan)
	}
	var id int
	if err := conn.QueryRow(ctx, "select id from items order by embedding <-> '[10,20,3]' limit 1").Scan(&id); err != nil {
		t.Fatal(err)
	}
	if id != 10 {
		t.Fatalf("nearest neighbour = %d, want 10", id)
	}
}

func TestPgcryptoLite(t *testing.T) {
	ctx := context.Background()
	conn := connect(t, start(t, pgzero.Config{}))
	if _, err := conn.Exec(ctx, "create extension pgcrypto_lite"); err != nil {
		t.Fatal(err)
	}
	var a, b []byte
	if err := conn.QueryRow(ctx, "select gen_random_bytes(32), gen_random_bytes(32)").Scan(&a, &b); err != nil {
		t.Fatal(err)
	}
	if len(a) != 32 || bytes.Equal(a, b) {
		t.Fatalf("gen_random_bytes: %x, %x", a, b)
	}
	var pgErr *pgconn.PgError
	if _, err := conn.Exec(ctx, "select gen_random_bytes(0)"); !errors.As(err, &pgErr) || pgErr.Message != "Length not in range" {
		t.Fatalf("gen_random_bytes(0): %v", err)
	}
	var uuid string
	if err := conn.QueryRow(ctx, "select public.gen_random_uuid()::text").Scan(&uuid); err != nil || len(uuid) != 36 {
		t.Fatalf("gen_random_uuid: %q %v", uuid, err)
	}
	// Only the functions that need no OpenSSL are provided.
	if _, err := conn.Exec(ctx, "select digest('x', 'sha256')"); !errors.As(err, &pgErr) || pgErr.Code != "42883" {
		t.Fatalf("digest: want undefined_function, got %v", err)
	}
}
