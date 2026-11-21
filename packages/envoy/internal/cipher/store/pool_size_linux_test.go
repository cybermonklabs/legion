//go:build linux

package store

import (
	"os"
	"testing"

	"github.com/sjawhar/envoy/poolsetting/poolsettingtest"
)

// Open's pool holds poolConns connections whatever machine it runs on
// (poolsettingtest.SameOnEveryMachine, which starts this test binary again on one CPU and on all).
func TestOpensPoolSizeIsTheSameOnEveryMachine(t *testing.T) {
	database := testDatabaseURL(t)
	poolsettingtest.SameOnEveryMachine(t, os.Args[0], poolConns, func(t *testing.T) int32 { return poolSize(t, database) })
}

// Settings that name pool_max_conns keep their own size, and settings that name none get
// poolConns (poolsettingtest.OutranksTheDefault). The deployed brokers name 10 in
// BROKER_DATABASE_URL; a deployment that needs another number writes it there.
func TestASettingsPoolMaxConnsOutranksTheStoresSize(t *testing.T) {
	poolsettingtest.OutranksTheDefault(t, testDatabaseURL(t), poolConns, poolSize)
}

// poolSize is the MaxConns of the pool Open makes on databaseURL.
func poolSize(t *testing.T, databaseURL string) int32 {
	t.Helper()
	s, err := Open(t.Context(), databaseURL)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer s.Close()
	return s.Pool.Config().MaxConns
}
