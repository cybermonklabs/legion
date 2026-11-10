package ratelimit

import (
	"os"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

func TestMain(m *testing.M) { os.Exit(storetest.Main(m)) }

// scope is the one scope these tests strike. It is the webhook redelivery sweep's own name,
// since that is the only caller Strike has.
const scope = "redelivery"

// quiet moves a scope's last write back by age, which is how these tests reach an hour ago
// without waiting an hour. It writes updated_at alone, exactly as a strike of that age would
// have left it, and leaves until where it is.
func quiet(t *testing.T, store *Store, age time.Duration) {
	t.Helper()
	tag, err := store.pool.Exec(t.Context(), `
		update github_rate_limits set updated_at = now() - make_interval(secs => $2) where scope = $1
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
