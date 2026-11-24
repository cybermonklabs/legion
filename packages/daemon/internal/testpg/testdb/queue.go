// A test binary of this module creates a database per test and drops it at the test's end
// (Database, and internal/testpg's Clone). Both statements force a checkpoint: `create database …
// strategy file_copy` forces one before the copy and one after it, and `drop database … with
// (force)` forces one of its own. One server runs every package's tests at once, and it has one
// checkpointer, so those forced checkpoints queue: a create or a drop waits on IPC/CheckpointDone
// while the checkpointer works through the requests ahead of it. internal/store alone makes 731 of
// each in a run.
//
// This file is the one path every database of this module's tests is created and dropped through,
// so that queue is held in one place: one admin pool per test binary rather than a connection per
// database, and a drop that runs in the background under dropSlots rather than on the clock of the
// test that asked for it. Main waits for the drops a package's tests left, so no database outlives
// the binary that made it, and a drop that failed turns a green run red rather than surprising the
// next one.

package testdb

import (
	"context"
	"fmt"
	"os"
	"sync"

	"github.com/jackc/pgx/v5/pgxpool"
)

// shared is a test binary's one admin pool, the drops its tests left in flight, and whether Main
// runs. A binary whose TestMain does not call Main drops each database on the clock of the test
// that made it, since nothing else would wait for a background drop: correct, and the cost this
// file exists to move.
var shared struct {
	mu    sync.Mutex
	pool  *pgxpool.Pool
	main  bool
	drops sync.WaitGroup

	failMu sync.Mutex
	fails  []string
}

// adminConns is the admin pool's size: the pool is this file's bookkeeping, not a pool a test
// opens, and every package's test binary holds one at once against one server, so it stays small.
// It is the same size internal/testpg's own admin pool held before this file took that work.
const adminConns = 4

// dropSlots holds the drops in flight to half the admin pool's connections, so a create always has
// a connection of the pool to run on while drops are in flight. A drop forces a checkpoint and
// waits for it; with every connection on drops, the next create waits for a checkpoint on its own
// clock, which is the cost this file moves off it.
var dropSlots = make(chan struct{}, adminConns/2)

// Main runs a test binary's tests, waits for the databases its tests left to drop, and answers
// run's code, or 1 where a drop failed. A package whose tests take a database of their own calls
// it from TestMain, composing it with the other Mains it needs:
//
//	func TestMain(m *testing.M) { os.Exit(testdb.Main(m.Run)) }
//	func TestMain(m *testing.M) { os.Exit(testdb.Main(func() int { return testnats.Main(m) })) }
//
// Without it every drop is correct and runs on its test's own clock (dropDatabase).
func Main(run func() int) int {
	shared.mu.Lock()
	shared.main = true
	shared.mu.Unlock()

	code := run()

	shared.drops.Wait()
	shared.failMu.Lock()
	fails := shared.fails
	shared.failMu.Unlock()
	for _, failure := range fails {
		fmt.Fprintf(os.Stderr, "testdb: %s\n", failure)
		if code == 0 {
			code = 1
		}
	}
	shared.mu.Lock()
	pool := shared.pool
	shared.pool, shared.main = nil, false
	shared.mu.Unlock()
	if pool != nil {
		pool.Close()
	}
	return code
}

// adminPool is the test binary's one pool on the postgres database of the server dsn names, opened
// on the first database a test takes. Its URL is DatabaseURL's, as every database of this module's
// tests is, and it holds adminConns connections whatever pool size the DSN sets, since dropSlots'
// count is against this pool and not against a test's.
func adminPool(dsn string) (*pgxpool.Pool, error) {
	shared.mu.Lock()
	defer shared.mu.Unlock()
	if shared.pool != nil {
		return shared.pool, nil
	}
	admin, err := DatabaseURL(dsn, "postgres")
	if err != nil {
		return nil, err
	}
	config, err := pgxpool.ParseConfig(admin)
	if err != nil {
		return nil, fmt.Errorf("configure the admin pool: %w", err)
	}
	config.MaxConns = adminConns
	pool, err := pgxpool.NewWithConfig(context.Background(), config)
	if err != nil {
		return nil, fmt.Errorf("connect to the admin database: %w", err)
	}
	shared.pool = pool
	return pool, nil
}

// dropDatabase drops name: in the background under dropSlots where Main runs, which Main waits
// for, and on the caller's clock otherwise. It answers the error of a drop it ran, and nil for one
// it left in flight, whose failure reaches Main.
func dropDatabase(pool *pgxpool.Pool, name string) error {
	shared.mu.Lock()
	background := shared.main
	if background {
		shared.drops.Add(1)
	}
	shared.mu.Unlock()
	if !background {
		if err := Drop(context.Background(), pool, name); err != nil {
			return fmt.Errorf("drop %s: %w", name, err)
		}
		return nil
	}
	go func() {
		defer shared.drops.Done()
		dropSlots <- struct{}{}
		defer func() { <-dropSlots }()
		if err := Drop(context.Background(), pool, name); err != nil {
			shared.failMu.Lock()
			shared.fails = append(shared.fails, fmt.Sprintf("drop %s: %v", name, err))
			shared.failMu.Unlock()
		}
	}()
	return nil
}
