package outbox

import (
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/sjawhar/envoy/internal/dispatch/duty"
)

// TestTheOutboxDoesNotScanWhileIdle's record of the holder begins a rest at either of the two
// moments one can begin, and at neither while the holder has a scan still to run (heldScans.rest).
// The row reaches a rest at listening only when the holder took the catch-up wake, scanned and
// parked before the connection counted as up, which a run cannot arrange, so this calls the
// record's own hooks in the orders the outbox calls them, with no database. At the first
// subtest's first wait, and in each subtest after the second, exactly one condition of the rule
// refuses the rest, so a rule without any one of them fails a subtest: the connection up (the
// first), no wake queued now (the third), no take or LISTEN without its scan (the fourth), no wake
// queued at the wait (the fifth) and no retry armed (the sixth). The last condition, no claim
// since the wait, needs no subtest: a rest recorded without it names a claim count the outbox has
// already passed, which atRest never matches.
func TestTheIdleRowRestsAtEitherCallbackOnlyWithNothingLeftToDo(t *testing.T) {
	// The hold taken and the first connection's LISTEN succeeded, its catch-up wake not sent yet.
	started := func() (*Outbox, *heldScans) {
		outbox := New(Deps{})
		h := &heldScans{outbox: outbox}
		h.restingAt.Store(-1)
		outbox.hold.Store(new(duty.Hold))
		h.logged(slog.NewRecord(time.Now(), slog.LevelInfo, tookHoldLine, 0))
		h.listened()
		return outbox, h
	}
	t.Run("the connection up after the wait", func(t *testing.T) {
		outbox, h := started()
		outbox.signal()
		<-outbox.wake
		outbox.claims.Add(1)
		h.waited()
		if got := h.restingAt.Load(); got != -1 {
			t.Fatalf("a wait before the connection counted as up began a rest at %d claims, want none", got)
		}
		h.listening()
		if got, claims := h.restingAt.Load(), outbox.claims.Load(); got != claims {
			t.Fatalf("the connection up after the wait began a rest at %d claims, want one at the wait's %d", got, claims)
		}
	})
	t.Run("the connection up before the wait", func(t *testing.T) {
		outbox, h := started()
		outbox.signal()
		h.listening()
		<-outbox.wake
		outbox.claims.Add(1)
		h.waited()
		if got, claims := h.restingAt.Load(), outbox.claims.Load(); got != claims {
			t.Fatalf("the wait after the connection came up began a rest at %d claims, want one at %d", got, claims)
		}
	})
	t.Run("a wake queued when the connection comes up", func(t *testing.T) {
		outbox, h := started()
		outbox.signal()
		<-outbox.wake
		outbox.claims.Add(1)
		h.waited()
		outbox.signal()
		h.listening()
		if got := h.restingAt.Load(); got != -1 {
			t.Fatalf("the connection up with a wake queued began a rest at %d claims, want none", got)
		}
	})
	t.Run("the connection up while the holder runs the catch-up scan", func(t *testing.T) {
		// The listener sends the catch-up wake before it calls afterListening, so the holder can
		// take that wake before the connection counts as up and still be scanning when it does:
		// the take and the LISTEN have no scan yet.
		outbox, h := started()
		outbox.signal()
		<-outbox.wake
		h.listening()
		if got := h.restingAt.Load(); got != -1 {
			t.Fatalf("the connection up while the holder runs its LISTEN's catch-up scan began a rest at %d claims, want none", got)
		}
	})
	t.Run("a wake queued at the wait and taken when the connection comes up", func(t *testing.T) {
		// The wake queued at the wait has been taken by the time the connection counts as up, so
		// none is queued now, and the holder is scanning for it.
		outbox, h := started()
		outbox.signal()
		<-outbox.wake
		outbox.claims.Add(1)
		outbox.signal()
		h.waited()
		<-outbox.wake
		h.listening()
		if got := h.restingAt.Load(); got != -1 {
			t.Fatalf("the connection up while the holder scans for the wake queued at its wait began a rest at %d claims, want none", got)
		}
	})
	t.Run("a wait after a scan that armed its retry", func(t *testing.T) {
		// A scan that met a failed statement arms its retry before the wait, and the retry's scan
		// is still to come.
		outbox, h := started()
		outbox.signal()
		h.listening()
		<-outbox.wake
		outbox.claims.Add(1)
		h.armed(time.Second)
		h.waited()
		if got := h.restingAt.Load(); got != -1 {
			t.Fatalf("a wait whose scan armed a retry began a rest at %d claims, want none", got)
		}
	})
}

// answeringHolds answers every statement of a hold as one that affected the hold's row, so
// duty.TryHold gives a hold with no database behind it, whose deadline Release pulls back to the
// moment it is released.
type answeringHolds struct{}

func (answeringHolds) Exec(context.Context, string, ...any) (pgconn.CommandTag, error) {
	return pgconn.NewCommandTag("UPDATE 1"), nil
}

func (answeringHolds) QueryRow(context.Context, string, ...any) pgx.Row { return nil }

// TestTheOutboxDoesNotScanWhileIdle's record of the holder reads which event gave a take's scan
// each of its batches. A LISTEN connection lost with the database comes back with no claim
// between it and the next take: before the take, while the outbox holds no hold, its catch-up
// wake waits for the next holder (Outbox.listen); after the take's line and before the take's
// first claim, its wake reaches the take's scan as it starts or runs. Either way the take's scan
// reads the take's batch and the connection's, or the scan after it reads the connection's, and
// each cause stays the take's or the connection's whatever order their records come in. A wake
// queued with no LISTEN beside the take is no catch-up, and the row counts its batch. This calls
// the record's hooks in the orders the outbox calls them, with no database, and judges the scans
// against a witness that saw the database fail over the loss.
func TestTheIdleRowReadsTheCatchUpOfALISTENBackAtATakeByItsEvent(t *testing.T) {
	// lostHold runs the record to a hold taken, its LISTEN connection lost with the database and
	// the hold lost after renewals the database left unanswered, and returns the record and the
	// witness's evidence over that loss.
	lostHold := func(t *testing.T) (*Outbox, *heldScans, evidence) {
		outbox := New(Deps{})
		h := &heldScans{outbox: outbox}
		h.restingAt.Store(-1)
		hold, err := duty.TryHold(context.Background(), answeringHolds{}, publisherHold, uuid.New(), publisherHoldOptions)
		if err != nil || hold == nil {
			t.Fatalf("take a hold with no database: %v", err)
		}
		outbox.hold.Store(hold)
		h.logged(slog.NewRecord(time.Now(), slog.LevelInfo, tookHoldLine, 0))
		h.listened()
		outbox.signal()
		h.listening()
		<-outbox.wake
		outbox.claims.Add(1)
		h.waited()
		if err := hold.Release(context.Background()); err != nil {
			t.Fatalf("release the hold: %v", err)
		}
		lapsed := hold.Deadline()
		redial := slog.NewRecord(lapsed.Add(-2*time.Second), slog.LevelWarn, redialLine, 0)
		redial.AddAttrs(slog.String("channels", "["+eventsChannel+"]"), slog.String("error", "connection refused"))
		h.logged(redial)
		failed := slog.NewRecord(lapsed.Add(-time.Second), slog.LevelWarn, renewFailedLine+"; the loss deadline stays where it was", 0)
		failed.AddAttrs(slog.String("hold", publisherHold), slog.Time("deadline", lapsed), slog.String("error", "timeout"))
		h.logged(failed)
		outbox.hold.Store(nil)
		window := failureWindow{from: lapsed.Add(-3 * time.Second), to: lapsed.Add(200 * time.Millisecond), err: "timeout"}
		return outbox, h, evidence{witnessed: witnessed{windows: []failureWindow{window}}}
	}
	// takeBack takes the next hold, as the standby does.
	takeBack := func(t *testing.T, outbox *Outbox, h *heldScans) {
		hold, err := duty.TryHold(context.Background(), answeringHolds{}, publisherHold, uuid.New(), publisherHoldOptions)
		if err != nil || hold == nil {
			t.Fatalf("take the next hold with no database: %v", err)
		}
		t.Cleanup(func() { _ = hold.Release(context.Background()) })
		outbox.hold.Store(hold)
		h.logged(slog.NewRecord(time.Now(), slog.LevelInfo, tookHoldLine, 0))
	}
	// last is the last scan the record ended, judged.
	last := func(h *heldScans, saw evidence) judgedScan {
		return h.scans[len(h.scans)-1].judge(saw)
	}

	t.Run("the connection back before the take, its wake in the take's scan", func(t *testing.T) {
		outbox, h, saw := lostHold(t)
		h.listened()
		outbox.signal()
		takeBack(t, outbox, h)
		<-outbox.wake
		outbox.claims.Add(2)
		h.waited()
		if judged := last(h, saw); judged.plain() {
			t.Fatalf("the take's scan that read the connection's catch-up was counted: %d by %s", judged.claims, judged.woken)
		}
	})
	t.Run("the connection back before the take, its wake in the scan after", func(t *testing.T) {
		outbox, h, saw := lostHold(t)
		h.listened()
		outbox.signal()
		takeBack(t, outbox, h)
		outbox.claims.Add(1)
		h.waited()
		<-outbox.wake
		outbox.claims.Add(1)
		h.waited()
		for _, scan := range h.scans[len(h.scans)-2:] {
			if judged := scan.judge(saw); judged.plain() {
				t.Fatalf("a scan of the take or of the connection's queued catch-up was counted: %d by %s", judged.claims, judged.woken)
			}
		}
	})
	t.Run("the connection back after the take's line, its wake in the take's scan", func(t *testing.T) {
		outbox, h, saw := lostHold(t)
		takeBack(t, outbox, h)
		h.listened()
		outbox.signal()
		h.listening()
		<-outbox.wake
		outbox.claims.Add(2)
		h.waited()
		judged := last(h, saw)
		if judged.plain() {
			t.Fatalf("the take's scan that read the catch-up of a connection back after the take's line was counted: %d by %s", judged.claims, judged.woken)
		}
		if !strings.Contains(judged.fault, "the publisher hold taken back") || !strings.Contains(judged.fault, "LISTEN connection that came back at the take") {
			t.Fatalf("the take's scan was left out as %q, want the take's cause and the connection's catch-up both named", judged.fault)
		}
	})
	t.Run("the connection back after the take's line, its wake in the scan after", func(t *testing.T) {
		outbox, h, saw := lostHold(t)
		takeBack(t, outbox, h)
		h.listened()
		outbox.claims.Add(1)
		outbox.signal()
		h.listening()
		h.waited()
		<-outbox.wake
		outbox.claims.Add(1)
		h.waited()
		for _, scan := range h.scans[len(h.scans)-2:] {
			if judged := scan.judge(saw); judged.plain() {
				t.Fatalf("a scan of the take or of the connection's queued catch-up was counted: %d by %s", judged.claims, judged.woken)
			}
		}
	})
	t.Run("the standby's retry takes the hold as the LISTEN statement ends, the wake reaching the take's first batch", func(t *testing.T) {
		outbox, h, saw := lostHold(t)
		takeBack(t, outbox, h)
		h.listened()
		outbox.claims.Add(1)
		outbox.signal()
		h.listening()
		<-outbox.wake
		outbox.claims.Add(1)
		h.waited()
		if judged := last(h, saw); judged.plain() {
			t.Fatalf("the take's scan whose first batch took the catch-up wake of a connection back as the hold was taken was counted: %d by %s", judged.claims, judged.woken)
		}
	})
	t.Run("the connection back after the take's line, and one batch more", func(t *testing.T) {
		outbox, h, saw := lostHold(t)
		takeBack(t, outbox, h)
		h.listened()
		outbox.signal()
		h.listening()
		<-outbox.wake
		outbox.claims.Add(3)
		h.waited()
		if judged := last(h, saw); !judged.plain() || judged.claims != 1 || !strings.Contains(judged.woken, "beyond the two batches the take and the LISTEN connection's catch-up explain") {
			t.Fatalf("the take's scan that read its batch, the connection's and one more was judged %d plain=%v %q, want 1 counted beyond the two",
				judged.claims, judged.plain(), judged.woken)
		}
	})
	t.Run("a wake queued with no connection back, in the take's scan", func(t *testing.T) {
		outbox, h, saw := lostHold(t)
		outbox.signal()
		takeBack(t, outbox, h)
		<-outbox.wake
		outbox.claims.Add(2)
		h.waited()
		if judged := last(h, saw); !judged.plain() || judged.claims != 1 || !strings.Contains(judged.woken, "a wake the take's catch-up took") {
			t.Fatalf("the take's scan that read a queued wake no LISTEN sent was judged %d plain=%v %q, want 1 counted as a wake the take's catch-up took",
				judged.claims, judged.plain(), judged.woken)
		}
	})
	t.Run("a wake queued with no connection back, in the scan after", func(t *testing.T) {
		outbox, h, saw := lostHold(t)
		outbox.signal()
		takeBack(t, outbox, h)
		outbox.claims.Add(1)
		h.waited()
		<-outbox.wake
		outbox.claims.Add(1)
		h.waited()
		if judged := last(h, saw); !judged.plain() || judged.claims != 1 {
			t.Fatalf("the scan of a queued wake no LISTEN sent was judged %d plain=%v %q, want 1 counted", judged.claims, judged.plain(), judged.woken)
		}
	})
}

// A take's line starts its first scan afresh: what the record held open for the scan in progress
// before it - a wait armed, an error the outbox logged after the last wait - is no part of the
// take's scan, so that scan carries no wait and no failed statement of its own, and the holder
// rests after it.
func TestATakesFirstScanCarriesNothingLeftOpenBeforeIt(t *testing.T) {
	outbox := New(Deps{})
	h := &heldScans{outbox: outbox}
	h.restingAt.Store(-1)
	hold, err := duty.TryHold(context.Background(), answeringHolds{}, publisherHold, uuid.New(), publisherHoldOptions)
	if err != nil || hold == nil {
		t.Fatalf("take a hold with no database: %v", err)
	}
	outbox.hold.Store(hold)
	h.logged(slog.NewRecord(time.Now(), slog.LevelInfo, tookHoldLine, 0))
	h.listened()
	outbox.signal()
	h.listening()
	<-outbox.wake
	outbox.claims.Add(1)
	h.waited()
	h.armed(time.Second)
	failed := slog.NewRecord(time.Now(), slog.LevelError, outboxLine+"claim events", 0)
	failed.AddAttrs(slog.String("error", "connection refused"))
	h.logged(failed)
	if err := hold.Release(context.Background()); err != nil {
		t.Fatalf("release the hold: %v", err)
	}
	outbox.hold.Store(nil)
	next, err := duty.TryHold(context.Background(), answeringHolds{}, publisherHold, uuid.New(), publisherHoldOptions)
	if err != nil || next == nil {
		t.Fatalf("take the next hold with no database: %v", err)
	}
	t.Cleanup(func() { _ = next.Release(context.Background()) })
	outbox.hold.Store(next)
	h.logged(slog.NewRecord(time.Now(), slog.LevelInfo, tookHoldLine, 0))
	outbox.claims.Add(1)
	h.waited()
	if scan := h.scans[len(h.scans)-1]; len(scan.armed) != 0 || scan.failed != "" {
		t.Fatalf("the take's first scan carried %v armed and %q failed from before the take's line, want neither", scan.armed, scan.failed)
	}
	if got, want := h.restingAt.Load(), outbox.claims.Load(); got != want {
		t.Fatalf("the holder rested at %d claims after the take's scan, want %d", got, want)
	}
}

// The witness's record leaves out a question that got no answer and ended more than witnessLate
// past its bound: the test process held it there - stopped while the bound ran out - and the
// database may have answered it in time, so it opens no window. A question that failed within
// witnessLate of its bound opens one, and a question held past it leaves an open window open, for
// the next answer to close.
func TestAQuestionTheTestProcessHeldPastItsBoundOpensNoWindow(t *testing.T) {
	w := &databaseWitness{}
	sent := fixtureStart
	answered := func() {
		w.record(sent, sent.Add(5*time.Millisecond), answer{at: sent.Add(5 * time.Millisecond)})
		sent = sent.Add(witnessPeriod)
	}
	failedLate := func(late time.Duration) {
		at := sent.Add(witnessBound + late)
		w.record(sent, at, answer{err: "timeout: context deadline exceeded", at: at})
		sent = at
	}
	answered()
	failedLate(witnessLate + 600*time.Millisecond)
	answered()
	if len(w.windows) != 0 || w.open != nil {
		t.Fatalf("a question the process held %s past its bound opened a window: %v, open %v", witnessLate+600*time.Millisecond, w.windows, w.open)
	}
	if len(w.heldBack) != 1 {
		t.Fatalf("the witness kept %d questions held past their bound, want 1: %v", len(w.heldBack), w.heldBack)
	}
	opened := sent.Add(-witnessPeriod)
	failedLate(witnessLate / 2)
	failedLate(witnessLate + time.Second)
	closed := sent.Add(5 * time.Millisecond)
	answered()
	if want := (failureWindow{from: opened, to: closed}); len(w.windows) != 1 || !w.windows[0].from.Equal(want.from) || !w.windows[0].to.Equal(want.to) {
		t.Fatalf("the witness recorded %v, want one window %s: a question that failed within witnessLate of its bound opens it, and one held past it leaves it open", w.windows, want)
	}
}

// A scan that reads a back-off only once it is due is the outbox's right answer only when a stall
// of the test process, or a failure of the database, held the outbox from about the insert's
// commit past the last reading that put the back-off still ahead (evidence.holding): one that
// began more than witnessBound after the commit, or ended before that reading, left the outbox time
// to scan.
func TestOnlyAStallFromTheInsertPastTheLastReadingHoldsABackOffsScan(t *testing.T) {
	inserted := fixtureStart
	by := inserted.Add(witnessBound)
	stopped := processStall{from: inserted.Add(-10 * time.Millisecond), to: inserted.Add(11 * time.Second)}
	if got := (evidence{stalls: []processStall{stopped}}).holding(by, inserted.Add(20*time.Millisecond)); got == "" {
		t.Fatal("a stop from the insert to past the due moment did not hold the outbox")
	}
	frozen := failureWindow{from: inserted.Add(-time.Second), to: inserted.Add(10500 * time.Millisecond), err: "timeout"}
	if got := (evidence{witnessed: witnessed{windows: []failureWindow{frozen}}}).holding(by, inserted.Add(20*time.Millisecond)); got == "" {
		t.Fatal("a failure of the database from the insert to past the due moment did not hold the outbox")
	}
	late := processStall{from: inserted.Add(witnessBound + 100*time.Millisecond), to: inserted.Add(11 * time.Second)}
	if got := (evidence{stalls: []processStall{late}}).holding(by, late.from.Add(-5*time.Millisecond)); got != "" {
		t.Fatalf("a stop that began %s after the insert held the outbox: %s", late.from.Sub(inserted), got)
	}
	short := processStall{from: inserted.Add(-10 * time.Millisecond), to: inserted.Add(3 * time.Second)}
	if got := (evidence{stalls: []processStall{short}}).holding(by, inserted.Add(9995*time.Millisecond)); got != "" {
		t.Fatalf("a stop that ended 7 s before the last reading that put the back-off ahead held the outbox: %s", got)
	}
}
