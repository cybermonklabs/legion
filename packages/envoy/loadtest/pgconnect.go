package main

import (
	"context"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/envoy/pgsetting"
)

// connectPG connects to url, a -pg value, through pgsetting, as each opener of a Postgres
// connection string does: a value whose user part pgx may read apart is refused, and one pgx
// cannot parse, each named by its length, and a failed connect names no user and no database. With
// -allow-foreign the value can name another database, with a user and a password, and pgx's errors
// would print a piece of the user part or the user.
func connectPG(ctx context.Context, url string) (*pgx.Conn, error) {
	config, err := pgsetting.ParseConfig("-pg", url)
	if err != nil {
		return nil, err
	}
	conn, err := pgx.ConnectConfig(ctx, config)
	if err != nil {
		return nil, pgsetting.ConnectFailure(err)
	}
	return conn, nil
}

// newPGPool is connectPG for a pool: pgxpool.New of url, read through pgsetting the same way.
func newPGPool(ctx context.Context, url string) (*pgxpool.Pool, error) {
	config, err := pgsetting.ParsePoolConfig("-pg", url)
	if err != nil {
		return nil, err
	}
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		return nil, pgsetting.ConnectFailure(err)
	}
	return pool, nil
}
