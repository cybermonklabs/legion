package testpool

import "testing"

// ParseConfig gives an unsized test-owned URL the pool size this package owns. A URL testdb made
// already names pool_max_conns and callers keep it with pgxpool.ParseConfig instead.
func TestParseConfigSetsTheTestPoolSize(t *testing.T) {
	t.Parallel()
	config, err := ParseConfig("postgres://test:secret@127.0.0.1:1/test?sslmode=disable")
	if err != nil {
		t.Fatal(err)
	}
	if got := config.MaxConns; got != PoolConns {
		t.Errorf("ParseConfig MaxConns = %d, want %d", got, PoolConns)
	}
}
