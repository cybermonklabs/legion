// Package testpool holds the size of every PostgreSQL pool a test opens on a DSN that does not
// itself name `pool_max_conns`. It supplies PoolConns and ParseConfig, which applies it; NewPool
// builds a pool from that configuration. pgxpool's own default follows the machine's CPU count, so
// a test that took it would run with a pool of a different size on every machine.
//
// It is a public package of the envoy module so that the daemon module imports it as it is: Go's
// internal rule keeps the daemon from importing a package under packages/envoy/internal, and a
// copy in the daemon would be a second size to keep equal. It imports pgxpool and the standard
// library and nothing else, so a test binary that takes it links no more than that:
// internal/testpg, beside which it would otherwise sit, imports testcontainers to start a Postgres
// container. Its users are tests and test helpers of both modules: the daemon's internal/testpg's
// ClonePool among them. A URL testdb.PoolBounded returns is already sized from
// LEGION_TEST_PG_DSN, so its callers use pgxpool.ParseConfig and keep that size.
package testpool

import (
	"context"

	"github.com/jackc/pgx/v5/pgxpool"
)

// PoolConns is the size, as MaxConns, of every Postgres pool a test builds with pgxpool itself.
// pgxpool's default is max(4, runtime.NumCPU()), so an unsized pool holds 4 connections on CI's
// 4-vCPU runners and hundreds on a large devbox, where a test's concurrent work on one pool can
// open more connections than the server's max_connections and fail with SQLSTATE 53300. Four is
// what every unsized pool already got on the CI runners that run
// these suites, so every test is already green at it, and it is pgx's own floor. It is never
// derived from runtime.NumCPU or GOMAXPROCS: a test's pool is the same size on every machine.
//
// A pool a store's own Open builds is not one of these: it takes the size its DSN names, or
// Dispatch's store its own sharedPoolSize, and a test that opens a store keeps that store's size.
// A test that needs another size sets MaxConns itself.
const PoolConns = 4

// SetMaxConns gives config the test-owned pool size. A caller that gets its configuration from a
// package-specific factory first (the daemon store's PoolConfig) keeps that factory's hooks and
// adds only this size.
func SetMaxConns(config *pgxpool.Config) { config.MaxConns = PoolConns }

// ParseConfig is pgxpool.ParseConfig for a test-owned DSN that names no pool size, with MaxConns
// set to PoolConns. A DSN testdb.PoolBounded returned already names the test server's size, which
// callers must keep with pgxpool.ParseConfig instead.
func ParseConfig(dsn string) (*pgxpool.Config, error) {
	config, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, err
	}
	SetMaxConns(config)
	return config, nil
}

// NewPool opens a test-owned, unsized DSN with ParseConfig: pgxpool.New at PoolConns.
func NewPool(ctx context.Context, dsn string) (*pgxpool.Pool, error) {
	config, err := ParseConfig(dsn)
	if err != nil {
		return nil, err
	}
	return pgxpool.NewWithConfig(ctx, config)
}
