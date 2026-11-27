package ratelimit

import (
	"errors"
	"os"
	"testing"
	"time"

	dispatchstore "github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

func TestMain(m *testing.M) { os.Exit(storetest.Main(m)) }

// scope is the one scope these tests strike. It is the webhook redelivery sweep's own name,
// since that is the only caller Strike has.
const scope = "redelivery"

// quiet moves a scope's last strike back by age, which is how these tests reach an hour ago
// without waiting an hour. It writes struck_at and updated_at, exactly as a strike of that age
// and nothing since would have left them, and leaves until where it is.
func quiet(t *testing.T, store *Store, age time.Duration) {
	t.Helper()
	tag, err := store.pool.Exec(t.Context(), `
		update github_rate_limits
		set struck_at = now() - make_interval(secs => $2), updated_at = now() - make_interval(secs => $2)
		where scope = $1
	`, scope, age.Seconds())
	if err != nil {
		t.Fatalf("age the row of %s: %v", scope, err)
	}
	if tag.RowsAffected() != 1 {
		t.Fatalf("aged %d rows of %s, want 1", tag.RowsAffected(), scope)
	}
}

// strikeNow is one strike with no wait of GitHub's own, so the wait the row carries is the
// doubling alone.
func strikeNow(t *testing.T, store *Store) Limit {
	t.Helper()
	limit, err := store.Strike(t.Context(), scope, 0)
	if err != nil {
		t.Fatalf("strike %s: %v", scope, err)
	}
	return limit
}

// The backoff doubles while the strikes keep coming: a minute, two, four. This is the behaviour
// the reset below must leave alone, and it is written out here because nothing else held it.
func TestStrikesDoubleTheWaitWhileTheyKeepComing(t *testing.T) {
	store := New(storetest.Open(t).Pool)
	// The wait each strike leaves: a minute, doubled by every strike before it.
	waits := []time.Duration{time.Minute, 2 * time.Minute, 4 * time.Minute}
	for i, want := range waits {
		limit := strikeNow(t, store)
		if limit.Strikes != i+1 {
			t.Fatalf("strike %d recorded %d strikes, want %d", i+1, limit.Strikes, i+1)
		}
		if limit.Wait < want-5*time.Second || limit.Wait > want+5*time.Second {
			t.Fatalf("strike %d waits %s, want about %s", i+1, limit.Wait, want)
		}
	}
}

// A strike after an hour of quiet starts the run again: one strike, one minute. Without the
// reset the count only ever rose -- Clear is reached only by a sweep that found GitHub willing
// again within the pass that was already backing off -- so a scope struck seven times over a
// month met its next limit, unrelated to any of them, with a 64-minute wait: an hour of webhook
// redeliveries not made because of a limit weeks old.
func TestAStrikeAfterAnHourOfQuietStartsTheCountAgain(t *testing.T) {
	store := New(storetest.Open(t).Pool)
	for range 7 {
		strikeNow(t, store)
	}
	at64 := strikeNow(t, store)
	if at64.Strikes != 8 || at64.Wait < 63*time.Minute {
		t.Fatalf("eight strikes recorded %d strikes waiting %s, want 8 at the 64-minute ceiling", at64.Strikes, at64.Wait)
	}

	// An hour and a minute with no strike, which is also an hour past the limit this run left.
	quiet(t, store, StrikeReset+time.Minute)
	if _, err := store.pool.Exec(t.Context(), `update github_rate_limits set until = now() - interval '1 minute' where scope = $1`, scope); err != nil {
		t.Fatalf("let the last limit pass: %v", err)
	}

	again := strikeNow(t, store)
	if again.Strikes != 1 {
		t.Fatalf("the strike after %s of quiet recorded %d strikes, want 1", StrikeReset+time.Minute, again.Strikes)
	}
	if again.Wait < 55*time.Second || again.Wait > 65*time.Second {
		t.Fatalf("the strike after %s of quiet waits %s, want about a minute", StrikeReset+time.Minute, again.Wait)
	}
}

// A strike just inside the hour keeps the run going: the reset is a quiet hour, not a timer that
// forgives a scope being struck every few minutes.
func TestAStrikeInsideTheHourKeepsTheCountGoing(t *testing.T) {
	store := New(storetest.Open(t).Pool)
	strikeNow(t, store)
	strikeNow(t, store)
	quiet(t, store, StrikeReset-time.Minute)

	third := strikeNow(t, store)
	if third.Strikes != 3 {
		t.Fatalf("the strike %s after the last recorded %d strikes, want 3", StrikeReset-time.Minute, third.Strikes)
	}
	if third.Wait < 3*time.Minute+55*time.Second || third.Wait > 4*time.Minute+5*time.Second {
		t.Fatalf("the third strike waits %s, want about four minutes", third.Wait)
	}
}

// The reset is an hour with no strike, not an hour with no write of any kind: a limit Record
// wrote on the scope meanwhile does not keep the run of strikes going. One scope takes both
// writes -- an installation's, where a secondary limit its queries meet is struck because
// GitHub shares that limit with its REST calls, and where every ordinary REST limit of that
// installation is recorded -- so a busy installation's REST limits would have held its run open
// for ever, and a secondary limit days later would have waited the full 64 minutes for limits
// unrelated to it.
func TestARecordedLimitDoesNotKeepTheStrikeCountGoing(t *testing.T) {
	store := New(storetest.Open(t).Pool)
	strikeNow(t, store)
	quiet(t, store, StrikeReset+time.Minute)
	// An ordinary REST limit of that scope, recorded an hour after the strike and a moment
	// before the next one.
	if err := store.Record(t.Context(), scope, time.Minute); err != nil {
		t.Fatalf("record a limit of %s: %v", scope, err)
	}

	again := strikeNow(t, store)
	if again.Strikes != 1 {
		t.Fatalf("the strike after %s of strike quiet recorded %d strikes, want 1: the limit recorded in between is no strike",
			StrikeReset+time.Minute, again.Strikes)
	}
	if again.Wait < 55*time.Second || again.Wait > 65*time.Second {
		t.Fatalf("that strike waits %s, want about the minute a run's first strike takes", again.Wait)
	}
}

// A limit still in force outlives the reset: a strike after a quiet hour never shortens a limit
// GitHub has already given, because the wait the row carries is the later of the two.
func TestTheResetNeverShortensALimitStillInForce(t *testing.T) {
	store := New(storetest.Open(t).Pool)
	// A limit of two hours, which is longer than the quiet hour that follows it.
	if _, err := store.Strike(t.Context(), scope, 2*time.Hour); err != nil {
		t.Fatalf("strike %s: %v", scope, err)
	}
	quiet(t, store, StrikeReset+time.Minute)

	again := strikeNow(t, store)
	if again.Strikes != 1 {
		t.Fatalf("the strike after the quiet hour recorded %d strikes, want 1", again.Strikes)
	}
	if again.Wait < 50*time.Minute {
		t.Fatalf("the strike shortened a limit still in force to %s, want the hour or so it had left", again.Wait)
	}
}

// PreviewStrike answers what Strike would write and writes nothing, the reset included: the
// sweep reads it to log the wait it would take without taking it.
func TestPreviewStrikeReadsTheResetAndWritesNothing(t *testing.T) {
	store := New(storetest.Open(t).Pool)
	strikeNow(t, store)
	strikeNow(t, store)
	quiet(t, store, StrikeReset+time.Minute)

	preview, err := store.PreviewStrike(t.Context(), scope, 0)
	if err != nil {
		t.Fatalf("preview a strike of %s: %v", scope, err)
	}
	if preview.Strikes != 1 {
		t.Fatalf("the preview says %d strikes, want the 1 the reset gives", preview.Strikes)
	}
	stored, found, err := store.Get(t.Context(), scope)
	if err != nil || !found {
		t.Fatalf("read %s back: found=%v err=%v", scope, found, err)
	}
	if stored.Strikes != 2 {
		t.Fatalf("the preview wrote the row: it holds %d strikes, want the 2 it held before", stored.Strikes)
	}
}

// A store OnBackground made - the ledger of the GitHub client a route's architecture import and
// the background loops call through (api's Deps.BackgroundGitHub) - is handed the request pool.
// Each read and write opens the background pool as it runs and runs there, taking no connection
// of the request pool. Once the background pool cannot open, each read and write fails with the
// open's error, and none of them runs on the request pool instead.
func TestAStoreOnTheBackgroundPoolReadsAndWritesThereAndNowhereElse(t *testing.T) {
	database := storetest.Open(t)
	limits := OnBackground(database.Pool)
	background, err := database.Pool.Background()
	if err != nil {
		t.Fatalf("open the background pool: %v", err)
	}
	requests, backgrounds := database.Pool.Stat().AcquireCount(), background.Stat().AcquireCount()
	if err := limits.Record(t.Context(), scope, time.Minute); err != nil {
		t.Fatalf("record a limit of %s: %v", scope, err)
	}
	if wait, _, err := limits.Wait(t.Context(), scope); err != nil || wait <= 0 {
		t.Fatalf("read the limit of %s back: wait=%s err=%v, want the minute recorded", scope, wait, err)
	}
	if got := database.Pool.Stat().AcquireCount() - requests; got != 0 {
		t.Errorf("the read and the write took %d request-pool connections, want none", got)
	}
	if background.Stat().AcquireCount() == backgrounds {
		t.Errorf("the read and the write took no background-pool connection, want their statements there")
	}

	refused := errors.New("test: the background pool cannot open")
	database.Pool.SetOpenFailureForTest(dispatchstore.BackgroundPool, refused)
	requests = database.Pool.Stat().AcquireCount()
	if _, _, err := limits.Wait(t.Context(), scope); !errors.Is(err, refused) {
		t.Errorf("a read with the background pool refused: %v, want the open's error", err)
	}
	if err := limits.Record(t.Context(), scope, time.Minute); !errors.Is(err, refused) {
		t.Errorf("a write with the background pool refused: %v, want the open's error", err)
	}
	if _, err := limits.Strike(t.Context(), scope, 0); !errors.Is(err, refused) {
		t.Errorf("a strike with the background pool refused: %v, want the open's error", err)
	}
	if got := database.Pool.Stat().AcquireCount() - requests; got != 0 {
		t.Errorf("with the background pool refused the reads and writes took %d request-pool connections, want none", got)
	}
}
