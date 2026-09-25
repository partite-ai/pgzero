// Package pgzerox configures pgx and pgxpool to connect to a pgzero.Server.
//
//	conn, err := pgx.ConnectConfig(ctx, pgzerox.ConnConfig(srv, "postgres"))
//
//	cfg := pgzerox.PoolConfig(srv, "postgres")
//	pool, err := pgxpool.NewWithConfig(ctx, cfg)
//
// For database/sql, register the ConnConfig with pgx's stdlib package:
//
//	db := stdlib.OpenDB(*pgzerox.ConnConfig(srv, "postgres"))
package pgzerox

import (
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/partite-ai/pgzero"
)

// ConnConfig returns a pgx connection config for database on srv. It
// dials srv in memory, for queries and cancel requests alike.
func ConnConfig(srv *pgzero.Server, database string) *pgx.ConnConfig {
	cfg, err := pgx.ParseConfig(srv.ConnString(database))
	if err != nil {
		// ConnString always produces a valid connection string.
		panic("pgzerox: " + err.Error())
	}
	cfg.DialFunc = srv.Dial
	return cfg
}

// PoolConfig returns a pgxpool config for database on srv. Adjust pool
// settings (MaxConns, ...) before passing it to pgxpool.NewWithConfig.
func PoolConfig(srv *pgzero.Server, database string) *pgxpool.Config {
	cfg, err := pgxpool.ParseConfig(srv.ConnString(database))
	if err != nil {
		panic("pgzerox: " + err.Error())
	}
	cfg.ConnConfig.DialFunc = srv.Dial
	return cfg
}
