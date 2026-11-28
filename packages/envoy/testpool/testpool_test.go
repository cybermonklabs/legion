package testpool

import (
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// largeDefault is pgx's own size for an unsized pool on a machine of PoolConns+3 CPUs,
// max(4, runtime.NumCPU()). The rows give it to an unsized configuration themselves, so a row that
// expects PoolConns fails without the rule on any machine: on CI's 4-vCPU runners pgx's default is
// PoolConns already, and a row that left it to pgx could not fail there.
const largeDefault = PoolConns + 3

// sizeRows are DSNs that name a pool size and DSNs that do not, in the URL and keyword forms.
// named says whether the row's own literal DSN names pool_max_conns.
var sizeRows = []struct {
	name, dsn string
	named     bool
	want      int32
}{
	{"a URL that names no size", "postgres://test:secret@127.0.0.1:1/test?sslmode=disable", false, PoolConns},
	{"a keyword DSN that names no size", "host=127.0.0.1 port=1 user=test dbname=test sslmode=disable", false, PoolConns},
	{"a URL that names 7", "postgres://test:secret@127.0.0.1:1/test?sslmode=disable&pool_max_conns=7", true, 7},
	{"a keyword DSN that names 7", "host=127.0.0.1 port=1 user=test dbname=test sslmode=disable pool_max_conns=7", true, 7},
	{"a URL that names 1", "postgres://test:secret@127.0.0.1:1/test?pool_max_conns=1", true, 1},
}

// A pool_max_conns the DSN names wins, and PoolConns is the size where the DSN names none: the DSN
// a daemon test gets from testdb.PoolBounded names the size LEGION_TEST_PG_DSN names, and a pool a
// test builds on it keeps that size rather than PoolConns over it. ParseConfig runs over a parse
// that answers as pgx does on a machine of largeDefault CPUs, so an unsized row reads PoolConns
// only because the rule set it.
func TestParseConfigKeepsTheSizeTheDSNNamesAndSizesTheRest(t *testing.T) {
	t.Parallel()
	for _, tc := range sizeRows {
		t.Run(tc.name, func(t *testing.T) {
			manyCPUs := func(dsn string) (*pgxpool.Config, error) {
				config, err := pgxpool.ParseConfig(dsn)
				if err == nil && !tc.named {
					config.MaxConns = largeDefault
				}
				return config, err
			}
			config, err := parseConfig(manyCPUs, tc.dsn)
			if err != nil {
				t.Fatal(err)
			}
			if got := config.MaxConns; got != tc.want {
				t.Errorf("ParseConfig(%q) on a machine whose pgx default is %d: MaxConns = %d, want %d", tc.dsn, largeDefault, got, tc.want)
			}
		})
	}
}

// ParseConfig itself parses with pgx: a DSN that names a size, which no CPU count changes, keeps it.
func TestParseConfigParsesWithPgx(t *testing.T) {
	t.Parallel()
	const dsn = "postgres://test:secret@127.0.0.1:1/test?pool_max_conns=7"
	config, err := ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	if got := config.MaxConns; got != 7 {
		t.Errorf("ParseConfig(%q) MaxConns = %d, want 7", dsn, got)
	}
}

// Size applies the same rule to a configuration another factory parsed, as the daemon store's
// PoolConfig does, so the factory's hooks stay and only the size is decided here. An unsized
// configuration starts at largeDefault, as pgx's own would on a larger machine.
func TestSizeKeepsTheSizeTheDSNNamesOnAnotherFactorysConfiguration(t *testing.T) {
	t.Parallel()
	for _, tc := range sizeRows {
		t.Run(tc.name, func(t *testing.T) {
			config, err := pgxpool.ParseConfig(tc.dsn)
			if err != nil {
				t.Fatal(err)
			}
			if !tc.named {
				config.MaxConns = largeDefault
			}
			start := config.MaxConns
			if err := Size(config); err != nil {
				t.Fatal(err)
			}
			if got := config.MaxConns; got != tc.want {
				t.Errorf("Size on %q from MaxConns %d: MaxConns = %d, want %d", tc.dsn, start, got, tc.want)
			}
		})
	}
}
