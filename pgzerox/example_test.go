package pgzerox_test

import (
	"context"
	"fmt"
	"log"

	"github.com/jackc/pgx/v5"

	"github.com/partite-ai/pgzero"
	"github.com/partite-ai/pgzero/pgzerox"
)

// Start an in-memory Postgres and query it with pgx.
func Example() {
	ctx := context.Background()
	srv, err := pgzero.Start(ctx, pgzero.Config{})
	if err != nil {
		log.Fatal(err)
	}
	defer srv.Close()

	conn, err := pgx.ConnectConfig(ctx, pgzerox.ConnConfig(srv, "postgres"))
	if err != nil {
		log.Fatal(err)
	}
	defer conn.Close(ctx)

	var sum int
	if err := conn.QueryRow(ctx, "select sum(g) from generate_series(1, 10) g").Scan(&sum); err != nil {
		log.Fatal(err)
	}
	fmt.Println(sum)
	// Output: 55
}
