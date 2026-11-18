package store

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// storedActivity is four of the activity columns of token's claim row, which no claim read
// carries: the report lives on the row alone, and only the idle view reads it back. at is the
// daemon's receipt of the report.
func storedActivity(t *testing.T, store *Store, token claim.Token) (at time.Time, background bool, kinds string, reminder time.Time) {
	t.Helper()
	var read, due *time.Time
	if err := store.Pool().QueryRow(context.Background(),
		`select activity_at, activity_background, activity_kinds, activity_next_reminder_at from claims where token = $1`,
		string(token)).Scan(&read, &background, &kinds, &due); err != nil {
		t.Fatalf("read the activity of %s: %v", token, err)
	}
	return orZero(read), background, kinds, orZero(due)
}

// storedSentAt is the session's own time of the report token's claim row holds, which orders its
// reports (activity_sent_at).
func storedSentAt(t *testing.T, store *Store, token claim.Token) time.Time {
	t.Helper()
	var sent *time.Time
	if err := store.Pool().QueryRow(context.Background(), `select activity_sent_at from claims where token = $1`,
		string(token)).Scan(&sent); err != nil {
		t.Fatalf("read the activity's sent time of %s: %v", token, err)
	}
	return orZero(sent)
}

// A session fires its reports without an in-flight guard — pi-legion's five-second timer and its
// session_stop report race — so an earlier reading can reach the daemon after a later one. The
// earlier one must not win: a reading taken mid-turn before a background job started would then
// stand as the newest, and the idle rule would release a pod whose job is still running.
func TestAnActivityReportNeverOverwritesANewerOne(t *testing.T) {
	ctx := context.Background()
	store := migratedStore(t)
	c := tmuxClaim("legion-LEGION-209-implementer")
	if err := store.PutClaim(ctx, c); err != nil {
		t.Fatalf("put claim: %v", err)
	}

	// The end-of-turn report: a background job of the session's own is running.
	due := at(20)
	newest := claim.SessionActivity{
		At:             at(10),
		BackgroundWork: claim.BackgroundWork{Running: true, Kinds: []string{"async-job"}},
		Phase:          claim.PhaseActivity{NextReminderAt: &due},
	}
	if known, err := store.PutClaimActivity(ctx, c.Token, c.Generation, newest, newest.At); err != nil || !known {
		t.Fatalf("PutClaimActivity of the newest report = %t, %v", known, err)
	}

	// The reading taken before the job started, which the five-second timer sent first and which
	// arrives second.
	stale := claim.SessionActivity{At: at(9)}
	if known, err := store.PutClaimActivity(ctx, c.Token, c.Generation, stale, newest.At.Add(time.Second)); err != nil || !known {
		t.Fatalf("PutClaimActivity of the stale report = %t, %v", known, err)
	}
	_, background, kinds, reminder := storedActivity(t, store, c.Token)
	if sent := storedSentAt(t, store, c.Token); !sent.Equal(newest.At) || !background || kinds != "async-job" || !reminder.Equal(due) {
		t.Fatalf("after the stale report the row reads sent %s, background %t, kinds %q, reminder %s; want the newest report kept",
			sent, background, kinds, reminder)
	}

	// A report of the same instant is the same reading sent twice, and is written: a retry must
	// not be refused.
	same := claim.SessionActivity{At: at(10)}
	if known, err := store.PutClaimActivity(ctx, c.Token, c.Generation, same, same.At); err != nil || !known {
		t.Fatalf("PutClaimActivity of the repeated report = %t, %v", known, err)
	}
	if _, background, kinds, reminder = storedActivity(t, store, c.Token); background || kinds != "" || !reminder.IsZero() {
		t.Fatalf("the repeated report left background %t, kinds %q, reminder %s; want the quiet reading written",
			background, kinds, reminder)
	}

	// A newer one is written whole, the armed reminder cleared with the rest.
	newer := claim.SessionActivity{At: at(11)}
	if known, err := store.PutClaimActivity(ctx, c.Token, c.Generation, newer, newer.At); err != nil || !known {
		t.Fatalf("PutClaimActivity of the newer report = %t, %v", known, err)
	}
	if sent := storedSentAt(t, store, c.Token); !sent.Equal(newer.At) {
		t.Fatalf("activity_sent_at = %s after the newer report, want %s", sent, newer.At)
	}
}

// A session chooses the time it reports. One dated ahead of the daemon's clock — a host whose
// clock stepped forward and was corrected back afterwards — would, under the ordering guard
// above, drop every honest report after it while the reading it left stood as current,
// background work and all. Its own time is bounded to the daemon's receipt, as the listener's
// report is (intake.decodeListenerWake), and the report is dated by that receipt.
func TestAnActivityReportDatedAheadOfTheDaemonIsBoundedToItsReceipt(t *testing.T) {
	ctx := context.Background()
	store := migratedStore(t)
	c := tmuxClaim("legion-LEGION-210-implementer")
	if err := store.PutClaim(ctx, c); err != nil {
		t.Fatalf("put claim: %v", err)
	}

	received := at(30)
	ahead := claim.SessionActivity{At: received.Add(2 * time.Hour),
		BackgroundWork: claim.BackgroundWork{Running: true, Kinds: []string{"async-job"}}}
	if known, err := store.PutClaimActivity(ctx, c.Token, c.Generation, ahead, received); err != nil || !known {
		t.Fatalf("PutClaimActivity of the report dated ahead = %t, %v", known, err)
	}
	stored, _, _, _ := storedActivity(t, store, c.Token)
	if sent := storedSentAt(t, store, c.Token); !stored.Equal(received) || !sent.Equal(received) {
		t.Fatalf("a report dated %s and received at %s was stored at %s, sent at %s; want both the receipt",
			ahead.At, received, stored, sent)
	}

	// The session's next honest report is written, which is what the unbounded time cost.
	honest := claim.SessionActivity{At: received.Add(time.Minute)}
	if known, err := store.PutClaimActivity(ctx, c.Token, c.Generation, honest, honest.At); err != nil || !known {
		t.Fatalf("PutClaimActivity of the honest report = %t, %v", known, err)
	}
	if _, background, kinds, _ := storedActivity(t, store, c.Token); background || kinds != "" {
		t.Fatalf("after the honest report the row reads background %t, kinds %q; want the quiet reading written",
			background, kinds)
	}
}

// A session's clock can run behind the daemon's. Its report is written at the moment the daemon
// received it, so a report of a turn the claim is still in, sent by a session whose clock runs 25
// minutes behind, still says the process runs that turn, and the relaunch guard refuses to cut it
// (R5's turn report rule, supervise.RelaunchGuard).
func TestAReportFromASessionClockBehindTheDaemonStillHoldsTheTurnReportGuard(t *testing.T) {
	ctx := context.Background()
	store := migratedStore(t)
	c := tmuxClaim("legion-LEGION-211-implementer")
	now := time.Now().UTC()
	c.State, c.LastTurnEnd, c.LastTurnStart = supervise.StateWorking, now.Add(-time.Hour), now.Add(-time.Minute)
	if err := store.PutClaim(ctx, c); err != nil {
		t.Fatalf("put claim: %v", err)
	}

	received := time.Now().UTC()
	behind := claim.SessionActivity{At: received.Add(-25 * time.Minute)}
	if known, err := store.PutClaimActivity(ctx, c.Token, c.Generation, behind, received); err != nil || !known {
		t.Fatalf("PutClaimActivity of the report from a clock 25 minutes behind = %t, %v", known, err)
	}
	claims, err := store.Claims(ctx)
	if err != nil {
		t.Fatalf("read the claims: %v", err)
	}
	i := slices.IndexFunc(claims, func(read supervise.Claim) bool { return read.Token == c.Token })
	if i < 0 {
		t.Fatalf("the claims read back hold no %s", c.Token)
	}
	why := supervise.RelaunchGuard(claims[i], time.Time{}, time.Now())
	if !strings.Contains(why, "still runs that turn") {
		t.Fatalf("the relaunch guard of a claim whose session reported in its turn a moment ago, by a clock 25 minutes behind, answers %q; want it refused by the turn report rule (activity at %s, turn begun at %s)",
			why, claims[i].ActivityAt, claims[i].LastTurnStart)
	}
}

// A rollback runs the release before activity_sent_at, whose launch clears the four activity
// columns it knows and leaves activity_sent_at as the process before it left it. The launched
// process's reports are of its own generation, so its first is written whatever its own clock
// reads beside the time the process before it last reported at.
func TestAReportAfterALaunchTheEarlierReleaseMadeIsWritten(t *testing.T) {
	ctx := context.Background()
	store := migratedStore(t)
	c := tmuxClaim("legion-LEGION-212-implementer")
	if err := store.PutClaim(ctx, c); err != nil {
		t.Fatalf("put claim: %v", err)
	}
	if known, err := store.PutClaimActivity(ctx, c.Token, c.Generation, claim.SessionActivity{At: at(20)}, at(20)); err != nil || !known {
		t.Fatalf("PutClaimActivity of the process before = %t, %v", known, err)
	}
	// The earlier release's launch, in its own statement: it names no activity_sent_at.
	if _, err := store.Pool().Exec(ctx, `update claims set generation = generation + 1, activity_at = null,
		activity_background = false, activity_kinds = '', activity_next_reminder_at = null, updated_at = now()
		where token = $1`, string(c.Token)); err != nil {
		t.Fatalf("launch the next process as the earlier release does: %v", err)
	}

	// The launched process's clock runs behind the one before it.
	first := claim.SessionActivity{At: at(5), BackgroundWork: claim.BackgroundWork{Running: true, Kinds: []string{"async-job"}}}
	if known, err := store.PutClaimActivity(ctx, c.Token, c.Generation+1, first, at(21)); err != nil || !known {
		t.Fatalf("PutClaimActivity of the launched process = %t, %v", known, err)
	}
	if got, background, kinds, _ := storedActivity(t, store, c.Token); !got.Equal(at(21)) || !background || kinds != "async-job" {
		t.Fatalf("after the launched process's first report the row reads %s, background %t, kinds %q; want the report received at %s written",
			got, background, kinds, at(21))
	}
}

// A report for a claim the daemon has forgotten is nothing to record, and says so: the route
// answers it as it answers a wrong secret. A report the ordering guard drops is not that.
func TestAnActivityReportOfAnUnknownClaimIsNotRecorded(t *testing.T) {
	ctx := context.Background()
	store := migratedStore(t)
	known, err := store.PutClaimActivity(ctx, "legion-LEGION-999-implementer", 1, claim.SessionActivity{At: at(1)}, at(1))
	if err != nil {
		t.Fatalf("PutClaimActivity: %v", err)
	}
	if known {
		t.Fatal("a report for a claim the store does not hold was recorded")
	}
}

// A session's clock can step backwards within one launch: a host corrected after it ran ahead,
// a VM resumed on a stale clock. Every reading the session takes after the step is dated before
// the one recorded, and the ordering guard, were it to compare the session's times alone, would
// drop each of them until the session's clock passed the recorded time again, while the reading
// it left behind stood as current. A reading dated 10 minutes before the recorded one that the
// daemon received later is no earlier reading overtaken in flight, so it is written, and the
// guard goes on ordering the session's readings by its clock as it now runs.
func TestAnActivityReportAfterTheSessionClockSteppedBackIsWritten(t *testing.T) {
	ctx := context.Background()
	store := migratedStore(t)
	c := tmuxClaim("legion-LEGION-213-implementer")
	if err := store.PutClaim(ctx, c); err != nil {
		t.Fatalf("put claim: %v", err)
	}

	before := claim.SessionActivity{At: at(30), BackgroundWork: claim.BackgroundWork{Running: true, Kinds: []string{"async-job"}}}
	if known, err := store.PutClaimActivity(ctx, c.Token, c.Generation, before, at(30)); err != nil || !known {
		t.Fatalf("PutClaimActivity of the report before the step = %t, %v", known, err)
	}

	// The session's clock steps back 10 minutes; its next reading, that the job has ended, reaches
	// the daemon 30 seconds after the one before.
	received := at(30).Add(30 * time.Second)
	stepped := claim.SessionActivity{At: at(20)}
	if known, err := store.PutClaimActivity(ctx, c.Token, c.Generation, stepped, received); err != nil || !known {
		t.Fatalf("PutClaimActivity of the report after the step = %t, %v", known, err)
	}
	got, background, kinds, _ := storedActivity(t, store, c.Token)
	if sent := storedSentAt(t, store, c.Token); !got.Equal(received) || !sent.Equal(stepped.At) || background || kinds != "" {
		t.Fatalf("after a report dated 10 minutes before the recorded one and received 30 seconds after it, the row reads "+
			"received %s, sent %s, background %t, kinds %q; want the report received at %s and sent at %s written, nothing running",
			got, sent, background, kinds, received, stepped.At)
	}

	// The session's next reading, a minute on by its clock as it now runs, is written.
	next := claim.SessionActivity{At: at(21), BackgroundWork: claim.BackgroundWork{Running: true, Kinds: []string{"async-job"}}}
	if known, err := store.PutClaimActivity(ctx, c.Token, c.Generation, next, at(31).Add(30*time.Second)); err != nil || !known {
		t.Fatalf("PutClaimActivity of the next report = %t, %v", known, err)
	}
	// A reading taken 30 seconds before that one, which the daemon receives a second after it, is
	// an earlier reading overtaken in flight on the same clock, and is dropped.
	overtaken := claim.SessionActivity{At: at(20).Add(30 * time.Second)}
	if known, err := store.PutClaimActivity(ctx, c.Token, c.Generation, overtaken, at(31).Add(31*time.Second)); err != nil || !known {
		t.Fatalf("PutClaimActivity of the overtaken report = %t, %v", known, err)
	}
	_, background, kinds, _ = storedActivity(t, store, c.Token)
	if sent := storedSentAt(t, store, c.Token); !sent.Equal(next.At) || !background || kinds != "async-job" {
		t.Fatalf("after an earlier reading of the stepped clock arrived late the row reads sent %s, background %t, kinds %q; "+
			"want the reading sent at %s kept", sent, background, kinds, next.At)
	}
}
