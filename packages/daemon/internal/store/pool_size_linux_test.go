//go:build linux

package store

import (
	"os"
	"testing"

	"github.com/sjawhar/envoy/poolsetting/poolsettingtest"
)

// Open's pool holds PoolConns connections whatever machine it runs on
// (poolsettingtest.SameOnEveryMachine, which starts this test binary again on one CPU and on all).
func TestOpensPoolSizeIsTheSameOnEveryMachine(t *testing.T) {
	dsn := testDSN(t)
	poolsettingtest.SameOnEveryMachine(t, os.Args[0], PoolConns, func(t *testing.T) int32 { return poolSize(t, dsn) })
}

// A DSN whose settings name pool_max_conns keeps its own size, and one that names none gets
// PoolConns (poolsettingtest.OutranksTheDefault). The deployed daemons name 32 in their DSN; a
// deployment that needs another number writes it there.
func TestADSNsPoolMaxConnsOutranksTheStoresSize(t *testing.T) {
	poolsettingtest.OutranksTheDefault(t, testDSN(t), PoolConns, poolSize)
}

// poolSize is the MaxConns of the pool Open makes on dsn.
func poolSize(t *testing.T, dsn string) int32 {
	t.Helper()
	s, err := Open(t.Context(), dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer s.Close()
	return s.pool.Config().MaxConns
}
