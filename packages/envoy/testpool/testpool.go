// Package testpool holds the one rule for the size of a PostgreSQL pool a test builds itself: a
// `pool_max_conns` its DSN names wins, and PoolConns is the size where the DSN names none. Size
// applies the rule to a configuration, ParseConfig parses a DSN and applies it, and NewPool builds a
// pool from that. pgxpool's own default follows the machine's CPU count, so a test that took it
// would run with a pool of a different size on every machine.
//
// It is a public package of the envoy module so that the daemon module imports it as it is: Go's
// internal rule keeps the daemon from importing a package under packages/envoy/internal, and a
// copy in the daemon would be a second rule to keep equal. It imports pgx and the standard library
// and nothing else, so a test binary that takes it links no more than that: internal/testpg, beside
// which it would otherwise sit, imports testcontainers to start a Postgres container. Its users are
// tests and test helpers of both modules. A URL the daemon's testdb.PoolBounded returns names the
// size LEGION_TEST_PG_DSN names, or PoolConns where that names none, and this rule keeps it.
package testpool

import (
	"context"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// PoolConns is the size, as MaxConns, of a Postgres pool a test builds on a DSN that names no
// pool_max_conns. pgxpool's default is max(4, runtime.NumCPU()), so an unsized pool holds 4
// connections on CI's 4-vCPU runners and hundreds on a large devbox, where a test's concurrent
// work on one pool can open more connections than the server's max_connections and fail with
// SQLSTATE 53300. Four is what every unsized pool already got on the CI runners that run these
// suites, so every test is already green at it, and it is pgx's own floor. It is never derived
// from runtime.NumCPU or GOMAXPROCS: a test's pool is the same size on every machine.
//
// A pool a store's own Open builds is not one of these: it takes the size its DSN names, or
// Dispatch's store its own sharedPoolSize, and a test that opens a store keeps that store's size.
// A test that needs another size names it in the DSN or sets MaxConns itself.
const PoolConns = 4

// poolMaxConns is the DSN setting pgxpool reads as a pool's size.
const poolMaxConns = "pool_max_conns"

// Size applies the rule to config: it keeps the pool_max_conns config's DSN names, which pgxpool
// already read into MaxConns, and sets PoolConns where the DSN names none. Whether the DSN names
// one is read as pgx reads the DSN (pgconn.ParseConfig, a service file's settings included), not
// by matching text. A caller that gets its configuration from a package's own factory (the
// daemon store's PoolConfig) keeps that factory's hooks and adds only this size.
func Size(config *pgxpool.Config) error {
	settings, err := pgconn.ParseConfig(config.ConnString())
	if err != nil {
		return err
	}
	if _, named := settings.RuntimeParams[poolMaxConns]; !named {
		config.MaxConns = PoolConns
	}
	return nil
}

// ParseConfig is pgxpool.ParseConfig with Size applied: MaxConns is the pool_max_conns dsn names,
// or PoolConns where it names none.
func ParseConfig(dsn string) (*pgxpool.Config, error) {
	config, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, err
	}
	if err := Size(config); err != nil {
		return nil, err
	}
	return config, nil
}

// NewPool opens a pool on dsn configured by ParseConfig.
func NewPool(ctx context.Context, dsn string) (*pgxpool.Pool, error) {
	config, err := ParseConfig(dsn)
	if err != nil {
		return nil, err
	}
	return pgxpool.NewWithConfig(ctx, config)
}
