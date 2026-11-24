package pgstore

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"slices"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/envoy/consumernames"
	"github.com/sjawhar/envoy/internal/testpg/testdb"
)

// pgTestDatabase is an empty database of this test's own, holding the intake_consumers table as
// the families' migrations create it, on the Postgres server DISPATCH_TEST_DATABASE_URL names: the
// variable the envoy module's Postgres-backed tests read (internal/dispatch/store,
// internal/pgmigrate), which CI's envoy-go job sets for the step that tests this package. A test
// that calls it skips when the variable is unset.
func pgTestDatabase(t *testing.T) *pgxpool.Pool {
	t.Helper()
	server := os.Getenv("DISPATCH_TEST_DATABASE_URL")
	if server == "" {
		t.Skip("DISPATCH_TEST_DATABASE_URL must be set to run the Postgres store rows")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, testdb.DatabaseOn(t, server, "consumernames"))
	if err != nil {
		t.Fatalf("open the test database: %v", err)
	}
	t.Cleanup(pool.Close)
	if _, err := pool.Exec(ctx, `create table `+table+` (
		logical  text primary key,
		record   jsonb not null,
		revision bigint not null check (revision > 0))`); err != nil {
		t.Fatalf("create %s: %v", table, err)
	}
	return pool
}

// tableKeys is every key of the intake_consumers table, in order.
func tableKeys(t *testing.T, pool *pgxpool.Pool) []string {
	t.Helper()
	rows, err := pool.Query(context.Background(), `select logical from `+table)
	if err != nil {
		t.Fatal(err)
	}
	keys, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatal(err)
	}
	// Go's string order: the server's collation may order '_' otherwise.
	slices.Sort(keys)
	return keys
}

// On a real Postgres, several families' stores share one database: two daemons whose projects
// differ only where one holds a '_' (daemon/a_b and daemon/aXb, which a like prefix would confuse)
// and Dispatch. Each reads and writes only its own sentinel and records, under its own keys, and
// lists only its own records; p's convert writing done to p's sentinel leaves q's fixed reader at
// "not done". The control is the single sentinel row every store shared before, written and read
// with the statements the store ran then: q's sentinel cannot be written beside p's, q's fixed
// reader reads p's sentinel as another family's, which it can never get past, and p's list holds
// every family's records.
func TestPGStoresOfSeveralFamiliesShareARealPostgresDatabase(t *testing.T) {
	ctx := context.Background()
	p, q := daemonFamily(t, "a_b"), daemonFamily(t, "aXb")
	families := []string{p, q, consumernames.DispatchFamily}
	logicals := map[string]string{p: "legion-go-a_b-github", q: "legion-go-aXb-github", consumernames.DispatchFamily: "delivery-events"}

	t.Run("each family's own rows", func(t *testing.T) {
		pool := pgTestDatabase(t)
		stores := map[string]*Store{}
		for _, family := range families {
			stores[family] = newStore(t, family, pool)
			if _, err := stores[family].WriteSentinel(ctx, consumernames.StoreState{Family: family}, 0); err != nil {
				t.Fatalf("the %s store's first sentinel beside the others' = %v", family, err)
			}
			if _, err := stores[family].Write(ctx, logicals[family], consumernames.Record{Start: 3}, 0); err != nil {
				t.Fatalf("write %s in the %s store = %v", logicals[family], family, err)
			}
		}
		// p's convert marks p's store converted.
		if _, err := stores[p].WriteSentinel(ctx, consumernames.StoreState{Family: p, Cutover: consumernames.CutoverDone}, 1); err != nil {
			t.Fatalf("p's convert writing done to p's sentinel = %v", err)
		}
		for _, family := range families {
			want := ""
			if family == p {
				want = consumernames.CutoverDone
			}
			if state, err := stores[family].State(ctx); err != nil || state.Family != family || state.Cutover != want {
				t.Fatalf("the %s store's sentinel after p's convert = %+v, %v; want its own, cutover %q", family, state, err, want)
			}
			records, err := stores[family].List(ctx)
			if err != nil || len(records) != 1 || records[logicals[family]].Start != 3 {
				t.Fatalf("List of the %s store = %v, %v; want %s alone, and no sentinel", family, records, err, logicals[family])
			}
			for _, other := range families {
				if other != family {
					if _, _, err := stores[family].Read(ctx, logicals[other]); !errors.Is(err, consumernames.ErrNoRecord) {
						t.Fatalf("the %s store reading %s, the %s store's = %v; want ErrNoRecord", family, logicals[other], other, err)
					}
				}
			}
		}
		want := []string{
			"daemon/aXb/legion-go-aXb-github", "daemon/a_b/legion-go-a_b-github", "dispatch/delivery-events",
			"envoy.sentinel/daemon/aXb", "envoy.sentinel/daemon/a_b", "envoy.sentinel/dispatch",
		}
		slices.Sort(want)
		if keys := tableKeys(t, pool); !slices.Equal(keys, want) {
			t.Fatalf("the table's keys = %q, want %q", keys, want)
		}
	})

	t.Run("control: the single sentinel row before", func(t *testing.T) {
		pool := pgTestDatabase(t)
		const sentinel = "envoy.sentinel"
		// The statements the store ran before each family had keys of its own: its write at
		// revision 0, its sentinel read, and its list.
		insert := func(key string, value any) error {
			data, err := json.Marshal(value)
			if err != nil {
				t.Fatal(err)
			}
			var revision int64
			return pool.QueryRow(ctx, `insert into `+table+` (logical, record, revision) values ($1, $2, 1)
				on conflict (logical) do nothing returning revision`, key, data).Scan(&revision)
		}
		readSentinel := func(family string) (consumernames.StoreState, error) {
			var data []byte
			var revision int64
			if err := pool.QueryRow(ctx, `select record, revision from `+table+` where logical = $1`, sentinel).Scan(&data, &revision); err != nil {
				return consumernames.StoreState{}, err
			}
			return consumernames.DecodeSentinel(sentinel, family, data)
		}
		if err := insert(sentinel, consumernames.StoreState{Key: sentinel, Family: p, Cutover: consumernames.CutoverDone}); err != nil {
			t.Fatalf("p's sentinel, the first = %v", err)
		}
		if err := insert(sentinel, consumernames.StoreState{Key: sentinel, Family: q}); !errors.Is(err, pgx.ErrNoRows) {
			t.Fatalf("q's sentinel beside p's under the one key = %v, want it refused", err)
		}
		if state, err := readSentinel(q); !errors.Is(err, consumernames.ErrSentinelFamily) {
			t.Fatalf("q's fixed reader of the one sentinel row = %+v, %v; want p's sentinel read as another family's, so q never boots", state, err)
		}
		for _, family := range families {
			if err := insert(logicals[family], consumernames.Record{Key: logicals[family], Start: 3}); err != nil {
				t.Fatal(err)
			}
		}
		rows, err := pool.Query(ctx, `select logical from `+table+` where logical <> $1 order by logical`, sentinel)
		if err != nil {
			t.Fatal(err)
		}
		listed, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil || len(listed) != len(families) {
			t.Fatalf("p's list under the one sentinel row = %q, %v; want every family's %d records", listed, err, len(families))
		}
	})
}
