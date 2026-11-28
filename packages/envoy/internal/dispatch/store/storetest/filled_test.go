package storetest

import (
	"context"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/testcleanup"
)

// filledRuns numbers the templates this file's rows make. A Filled's template is made once per
// key in the test binary, so a row that named the same key in each run under -count would make it
// in its first run only, and every later run would read what the first one made.
var filledRuns atomic.Int64

// filledKey is a key that no other test names, nor another run of t's row: t's name and the
// run's number.
func filledKey(t *testing.T) string {
	return fmt.Sprintf("%s #%d", t.Name(), filledRuns.Add(1))
}

// leftOpenApplication and leftOpenQuery are what the session a Fill leaves open names itself and
// last ran, which the template's error must quote.
const (
	leftOpenApplication = "storetest left open"
	leftOpenQuery       = "select 'left open by its Fill'"
)

// A Fill that returns with a session of its own still connected to its template fails the template
// at once, with an error that names the Filled's key and the session: its application_name, its
// state and its query. Without the check (noSessionLeftOpen) each clone of the template waits
// Postgres's five seconds and fails with an error that names only the template's random database
// ("is being accessed by other users"), which this row's assertions do not find. The session is
// either a connection the Fill opens on database's configuration or a pool it opens from it: the
// second copies the record of closed connections the store's pools keep (fillThroughStore), and a
// pool left open closes nothing, so the record must not excuse it. The store's own connections
// close as the Fill returns, and the error names the one session the Fill left, not theirs. A
// second test that asks for the template gets the same error, with no Fill run again. The row
// closes what the Fill left once it ends, so Run can drop the template.
func TestAFillThatLeavesASessionOpenFailsItsTemplateNamingTheKeyAndTheSession(t *testing.T) {
	t.Parallel()
	for _, leave := range []struct {
		name string
		// open leaves a session connected to the template, and answers what closes it.
		open func(ctx context.Context, database *store.Store) (func(), error)
	}{
		{"a connection of its own", func(ctx context.Context, database *store.Store) (func(), error) {
			config := database.Pool.Config().ConnConfig
			config.RuntimeParams["application_name"] = leftOpenApplication
			connection, err := pgx.ConnectConfig(ctx, config)
			if err != nil {
				return nil, err
			}
			closeConnection := func() { _ = connection.Close(context.Background()) }
			if _, err := connection.Exec(ctx, leftOpenQuery); err != nil {
				return closeConnection, err
			}
			return closeConnection, nil
		}},
		{"a pool opened from database's configuration", func(ctx context.Context, database *store.Store) (func(), error) {
			config := database.Pool.Config()
			config.ConnConfig.RuntimeParams["application_name"] = leftOpenApplication
			pool, err := pgxpool.NewWithConfig(ctx, config)
			if err != nil {
				return nil, err
			}
			if _, err := pool.Exec(ctx, leftOpenQuery); err != nil {
				return pool.Close, err
			}
			return pool.Close, nil
		}},
	} {
		t.Run(leave.name, func(t *testing.T) {
			t.Parallel()
			// Where no database is named this skips t on its own goroutine: fatalOf runs
			// OpenFilled on another, where a skip would not end the row.
			ready(t)
			var closeWhatWasLeft func()
			testcleanup.Add(t, func() {
				if closeWhatWasLeft != nil {
					closeWhatWasLeft()
				}
			})
			filled := Filled{Key: filledKey(t), Fill: func(ctx context.Context, database *store.Store) error {
				var err error
				closeWhatWasLeft, err = leave.open(ctx, database)
				return err
			}}
			failure := fatalOf(t, func(t testing.TB) { OpenFilled(t, filled) })
			if failure == "" {
				t.Fatal("OpenFilled cloned a template whose Fill left a session connected to it; want it to fail naming the key and the session")
			}
			for _, want := range []string{
				fmt.Sprintf("the Fill of Filled %q returned with 1 session(s) still connected", filled.Key),
				fmt.Sprintf("application_name %q, state %q, query %q", leftOpenApplication, "idle", leftOpenQuery),
			} {
				if !strings.Contains(failure, want) {
					t.Errorf("OpenFilled failed with %q; want it to say %q", failure, want)
				}
			}
			if again := fatalOf(t, func(t testing.TB) { OpenFilled(t, filled) }); again != failure {
				t.Errorf("a second OpenFilled of the template failed with %q; want the first one's error, %q", again, failure)
			}
			t.Logf("OpenFilled failed: %s", failure)
		})
	}
}

// A Fill that writes through database and returns with nothing of its own left open makes its
// template, though Postgres ends the backends of the connections database's pools close as it
// returns only a moment after: a check that read the template's sessions at once found them, and
// failed the template, in 12 of 20 runs of this row (store.Open's ping connection, the shared
// pool's, the rooms pool's), so noSessionLeftOpen waits for them. The Fill holds eight connections
// of the shared pool at once, so the pool closes eight as it returns; writes one row through the
// rooms pool (store.Pool.Rooms), whose connections name themselves dispatch-rooms; and writes one
// through a pool of its own opened from database.Pool.Config(), which it closes before it returns.
// Each run makes a template of its own (filledKey), so -count=20 runs the check 20 times.
func TestAFillWhoseConnectionsAreStillClosingMakesItsTemplate(t *testing.T) {
	t.Parallel()
	const held = 8
	filled := Filled{Key: filledKey(t), Fill: func(ctx context.Context, database *store.Store) error {
		if _, err := database.Pool.Exec(ctx, "create table filled_closing (n integer not null)"); err != nil {
			return err
		}
		transactions := make([]pgx.Tx, 0, held)
		defer func() {
			for _, tx := range transactions {
				_ = tx.Rollback(ctx) // a no-op on a committed transaction
			}
		}()
		for n := range held {
			tx, err := database.Pool.Begin(ctx)
			if err != nil {
				return err
			}
			transactions = append(transactions, tx)
			if _, err := tx.Exec(ctx, "insert into filled_closing values ($1)", n); err != nil {
				return err
			}
		}
		for _, tx := range transactions {
			if err := tx.Commit(ctx); err != nil {
				return err
			}
		}
		rooms, err := database.Pool.Rooms()
		if err != nil {
			return err
		}
		if _, err := rooms.Exec(ctx, "insert into filled_closing values ($1)", held); err != nil {
			return err
		}
		own, err := pgxpool.NewWithConfig(ctx, database.Pool.Config())
		if err != nil {
			return err
		}
		defer own.Close()
		_, err = own.Exec(ctx, "insert into filled_closing values ($1)", held+1)
		return err
	}}
	clone := OpenFilled(t, filled)
	var rows int
	if err := clone.Pool.QueryRow(context.Background(), "select count(*) from filled_closing").Scan(&rows); err != nil {
		t.Fatalf("read the rows the Fill wrote on the clone: %v", err)
	}
	if rows != held+2 {
		t.Fatalf("the clone holds %d rows of its Fill; want the %d it wrote", rows, held+2)
	}
}
