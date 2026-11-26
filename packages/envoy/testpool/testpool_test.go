package testpool

import (
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// A pool_max_conns the DSN names wins, and PoolConns is the size where the DSN names none: the DSN
// a daemon test gets from testdb.PoolBounded names the size LEGION_TEST_PG_DSN names, and a pool a
// test builds on it keeps that size rather than PoolConns over it.
func TestParseConfigKeepsTheSizeTheDSNNamesAndSizesTheRest(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, dsn string
		want      int32
	}{
		{"a DSN that names no size", "postgres://test:secret@127.0.0.1:1/test?sslmode=disable", PoolConns},
		{"a DSN that names 7", "postgres://test:secret@127.0.0.1:1/test?sslmode=disable&pool_max_conns=7", 7},
		{"a keyword DSN that names 7", "host=127.0.0.1 port=1 user=test dbname=test sslmode=disable pool_max_conns=7", 7},
		{"a DSN that names 1", "postgres://test:secret@127.0.0.1:1/test?pool_max_conns=1", 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			config, err := ParseConfig(tc.dsn)
			if err != nil {
				t.Fatal(err)
			}
			if got := config.MaxConns; got != tc.want {
				t.Errorf("ParseConfig(%q) MaxConns = %d, want %d", tc.dsn, got, tc.want)
			}
		})
	}
}

// Size applies the same rule to a configuration another factory parsed, as the daemon store's
// PoolConfig does, so the factory's hooks stay and only the size is decided here.
func TestSizeKeepsTheSizeTheDSNNamesOnAnotherFactorysConfiguration(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, dsn string
		want      int32
	}{
		{"a DSN that names no size", "postgres://test:secret@127.0.0.1:1/test", PoolConns},
		{"a DSN that names 7", "postgres://test:secret@127.0.0.1:1/test?pool_max_conns=7", 7},
	} {
		t.Run(tc.name, func(t *testing.T) {
			config, err := pgxpool.ParseConfig(tc.dsn)
			if err != nil {
				t.Fatal(err)
			}
			if err := Size(config); err != nil {
				t.Fatal(err)
			}
			if got := config.MaxConns; got != tc.want {
				t.Errorf("Size on %q: MaxConns = %d, want %d", tc.dsn, got, tc.want)
			}
		})
	}
}
