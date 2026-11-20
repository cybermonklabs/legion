package daemon

// The bound a round's ask states (askRound), as the walk writes it. A project's pause moves a
// round's bound by the time it held the round's pod: a hold that stands counts to the moment it is
// read, and one that ended counts to its end (roundClock.roundBound, heldUntil). The walk can wait
// before it reaches an issue, on a write it makes for an issue before it (sweepHeldInAWrite), and a
// hold can stand or end meanwhile, so the ask reads the clock and then the holds of its pod as it
// is written, never the sweep's top. The limit rig is podupdate_limits_test.go's, with no limit
// set.

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/testwait"
)

// Both rounds open at one sweep. A minute later a pause holds the second issue's pod, and the hold
// stands. A minute after that, the next sweep's walk first writes the ask of the first issue's
// round, which takes 20 seconds, and then writes the ask of the second: the bound it states counts
// the hold to that moment, 1m20s, not to the top of the sweep. The first issue's ask, written at
// the same sweep, counts no hold: the pause holds the second issue's pod alone, and an ask that
// counted the holds of another issue's pod would state an end later than the one at which the
// daemon replaces its own.
func TestPodUpdateAnAskStatesTheBoundAtTheMomentTheWalkWritesIt(t *testing.T) {
	asked, held := "ACME-1", "ACME-2"
	rig := newLimitRig(t, asked, held)
	rig.tick()
	rig.awaitRounds("the sweep that opened both rounds", asked, held)
	c := rig.claimOf(held, claim.RoleArchitect)
	if c.UpdateOpenedAt == nil {
		t.Fatalf("%s's claim records round %q with no opening", held, c.UpdateRound)
	}
	round, opened := c.UpdateRound, *c.UpdateOpenedAt

	rig.clock.advance(time.Minute)
	heldAt := rig.clock.Now()
	rig.pauseHolds(held, heldAt)

	rig.clock.advance(time.Minute)
	top := rig.clock.Now()
	rig.sweepHeldInAWrite(asked, 20*time.Second)
	written := top.Add(20 * time.Second)

	texts := rig.askTexts(held)
	if len(texts) != 1 {
		t.Fatalf("the sweep wrote %d asks of %s's round, want 1", len(texts), held)
	}
	bound := opened.Add(podUpdateGrace + written.Sub(heldAt))
	if want := supervise.UpdateTask(round, limitDrift, bound); texts[0] != want {
		t.Errorf("the ask of %s's round states %q, want %q: the walk wrote it %s after the top of its sweep, with the pause's hold standing, so the bound is %s",
			held, texts[0], want, written.Sub(top), bound.Format(time.RFC3339))
	}

	a := rig.claimOf(asked, claim.RoleArchitect)
	if a.UpdateOpenedAt == nil {
		t.Fatalf("%s's claim records round %q with no opening", asked, a.UpdateRound)
	}
	texts = rig.askTexts(asked)
	if len(texts) != 1 {
		t.Fatalf("the sweep wrote %d asks of %s's round, want 1", len(texts), asked)
	}
	if want := supervise.UpdateTask(a.UpdateRound, limitDrift, a.UpdateOpenedAt.Add(podUpdateGrace)); texts[0] != want {
		t.Errorf("the ask of %s's round states %q, want %q: no pause holds %s's pod; the hold is %s's",
			asked, texts[0], want, asked, held)
	}
}

// The case above, but the pause's hold of the second issue's pod ends, as a role's ready ends it,
// 5 seconds into the walk's 20-second write for the first issue. The sweep read the hold at its
// top, when it stood. The round now ends at its opening, the grace and the 1m5s the hold kept the
// pod, and that is where the next sweep past it replaces the pod with its disk: the ask states
// that end. Counted over the hold the sweep read at its top, the hold would run to the moment the
// ask is written, and the ask would tell the roles an end 15 seconds after the pod and the work
// they had not pushed are gone.
func TestPodUpdateAnAskStatesNoEndLaterThanTheRoundsWhenAHoldEndsInTheWalk(t *testing.T) {
	asked, held := "ACME-1", "ACME-2"
	rig := newLimitRig(t, asked, held)
	rig.tick()
	rig.awaitRounds("the sweep that opened both rounds", asked, held)
	c := rig.claimOf(held, claim.RoleArchitect)
	if c.UpdateOpenedAt == nil {
		t.Fatalf("%s's claim records round %q with no opening", held, c.UpdateRound)
	}
	round, opened := c.UpdateRound, *c.UpdateOpenedAt
	pod := strings.TrimPrefix(round, supervise.PodRoundPrefix)

	rig.clock.advance(time.Minute)
	heldAt := rig.clock.Now()
	rig.pauseHolds(held, heldAt)

	rig.clock.advance(time.Minute)
	top := rig.clock.Now()
	ended := top.Add(5 * time.Second)
	if !rig.sweepWaitingInAWrite(asked, 20*time.Second, func() {
		rig.clock.advance(5 * time.Second)
		if done, err := rig.sup.store.EndPausePodHold(rig.ctx, rig.s.cfg.Project, held, store.PausePodHoldReady, ended); err != nil || !done {
			t.Errorf("end the pause's hold of %s's pod at %s = %t, %v; want it ended", held, ended.Format(time.RFC3339), done, err)
		}
		rig.clock.advance(15 * time.Second)
	}) {
		t.Fatalf("the sweep made no write for %s", asked)
	}

	texts := rig.askTexts(held)
	if len(texts) != 1 {
		t.Fatalf("the sweep wrote %d asks of %s's round, want 1", len(texts), held)
	}
	bound := opened.Add(podUpdateGrace + ended.Sub(heldAt))
	if want := supervise.UpdateTask(round, limitDrift, bound); texts[0] != want {
		t.Errorf("the ask of %s's round states %q, want %q: the pause's hold of its pod ended %s after the top of the sweep, before the ask, so the round ends at %s",
			held, texts[0], want, ended.Sub(top), bound.Format(time.RFC3339))
	}

	// The first sweep at or past that end replaces the pod: the end the ask stated is that sweep's.
	rig.clock.advance(bound.Sub(rig.clock.Now()))
	rig.tick()
	testwait.Eventually(t, "the round's pod to be replaced at the end its ask stated", func() bool {
		return len(rig.replacementsOf(pod)) > 0
	})
}

// The ask reads the end it states after its read of the round's asks still queued (store's
// QueuedUpdateAsks), the other read of the store the ask makes, which the walk waits on as it
// waits on a write. A pause holds the round's pod, and the hold stands. That read waits 20 seconds
// behind a lock another session holds on the outbox, and the end the ask states counts the hold to
// after it. Read before it, the end would be the round's as it stood 20 seconds before the ask.
func TestPodUpdateAnAskStatesTheBoundAfterItsReadOfTheQueuedAsks(t *testing.T) {
	held := "ACME-1"
	rig := newLimitRig(t, held)
	rig.tick()
	rig.awaitRounds("the sweep that opened the round", held)
	c := rig.claimOf(held, claim.RoleArchitect)
	if c.UpdateOpenedAt == nil {
		t.Fatalf("%s's claim records round %q with no opening", held, c.UpdateRound)
	}
	round, opened := c.UpdateRound, *c.UpdateOpenedAt

	rig.clock.advance(time.Minute)
	heldAt := rig.clock.Now()
	rig.pauseHolds(held, heldAt)

	rig.clock.advance(time.Minute)
	top := rig.clock.Now()
	rig.sweepHeldInTheQueuedAskRead(20 * time.Second)
	read := top.Add(20 * time.Second)

	texts := rig.askTexts(held)
	if len(texts) != 1 {
		t.Fatalf("the sweep wrote %d asks of %s's round, want 1", len(texts), held)
	}
	bound := opened.Add(podUpdateGrace + read.Sub(heldAt))
	if want := supervise.UpdateTask(round, limitDrift, bound); texts[0] != want {
		t.Errorf("the ask of %s's round states %q, want %q: its read of the queued asks ended %s after the top of its sweep, with the pause's hold standing, so the bound is %s",
			held, texts[0], want, read.Sub(top), bound.Format(time.RFC3339))
	}
}

// The fleet rig's daemon runs the project of its issue keys (fleetProject), as production's daemon
// runs the project whose keys its outbox writes, so it reads the holds of that project's pauses. A
// pause of ACME, the keys' project named here and not read from the rig, holds the second issue's
// pod, and the ask of its round counts the hold. A rig whose daemon ran another project than its
// outbox would read none of that pause's holds, and every ask test of this file would hold a pod
// its daemon cannot see.
func TestPodUpdateTheFleetRigReadsThePauseHoldsOfItsKeysProject(t *testing.T) {
	asked, held := "ACME-1", "ACME-2"
	rig := newLimitRig(t, asked, held)
	rig.tick()
	rig.awaitRounds("the sweep that opened both rounds", asked, held)
	c := rig.claimOf(held, claim.RoleArchitect)
	if c.UpdateOpenedAt == nil {
		t.Fatalf("%s's claim records round %q with no opening", held, c.UpdateRound)
	}
	round, opened := c.UpdateRound, *c.UpdateOpenedAt

	rig.clock.advance(time.Minute)
	heldAt := rig.clock.Now()
	holdProjectPod(t, rig.sup.store, openProjectPause(t, rig.pool, "ACME", heldAt), "ACME", held, heldAt)

	rig.clock.advance(time.Minute)
	top := rig.clock.Now()
	rig.sweepHeldInAWrite(asked, 20*time.Second)
	written := top.Add(20 * time.Second)

	texts := rig.askTexts(held)
	if len(texts) != 1 {
		t.Fatalf("the sweep wrote %d asks of %s's round, want 1", len(texts), held)
	}
	bound := opened.Add(podUpdateGrace + written.Sub(heldAt))
	if want := supervise.UpdateTask(round, limitDrift, bound); texts[0] != want {
		t.Errorf("the ask of %s's round under a pause of ACME states %q, want %q, the end %s that counts the hold: the rig's daemon runs project %q",
			held, texts[0], want, bound.Format(time.RFC3339), rig.s.cfg.Project)
	}
}

// The ask reads the clock and then its pod's holds (roundClockNow), and a hold that ends between
// the two reads counts to its end. A pause holds the round's pod; the hold ends, as a role's ready
// ends it, just after the ask's read of the clock, and the clock moves 5 seconds before the read
// of the holds (beforePodHoldsRead). The end the ask states counts the hold to its end. Read the
// other way round, the holds would show the hold standing and count it on to the later clock read,
// 5 seconds after the hold ended and after the round's own end.
func TestPodUpdateAnAskReadsTheClockBeforeItsPodsHolds(t *testing.T) {
	held := "ACME-1"
	rig := newLimitRig(t, held)
	rig.tick()
	rig.awaitRounds("the sweep that opened the round", held)
	c := rig.claimOf(held, claim.RoleArchitect)
	if c.UpdateOpenedAt == nil {
		t.Fatalf("%s's claim records round %q with no opening", held, c.UpdateRound)
	}
	round, opened := c.UpdateRound, *c.UpdateOpenedAt

	rig.clock.advance(time.Minute)
	heldAt := rig.clock.Now()
	rig.pauseHolds(held, heldAt)

	rig.clock.advance(time.Minute)
	var ended time.Time
	rig.s.beforePodHoldsRead = func(issue string) {
		if issue != held || !ended.IsZero() {
			return
		}
		ended = rig.clock.Now()
		if done, err := rig.sup.store.EndPausePodHold(rig.ctx, rig.s.cfg.Project, held, store.PausePodHoldReady, ended); err != nil || !done {
			t.Errorf("end the pause's hold of %s's pod at %s = %t, %v; want it ended", held, ended.Format(time.RFC3339), done, err)
		}
		rig.clock.advance(5 * time.Second)
	}
	rig.tick()
	rig.s.beforePodHoldsRead = nil
	if ended.IsZero() {
		t.Fatalf("the sweep read no holds of %s's pod for an act of its round", held)
	}

	texts := rig.askTexts(held)
	if len(texts) != 1 {
		t.Fatalf("the sweep wrote %d asks of %s's round, want 1", len(texts), held)
	}
	bound := opened.Add(podUpdateGrace + ended.Sub(heldAt))
	if want := supervise.UpdateTask(round, limitDrift, bound); texts[0] != want {
		t.Errorf("the ask of %s's round states %q, want %q: the pause's hold of its pod ended at %s, after the ask read the clock and before it read the holds, so the round ends at %s",
			held, texts[0], want, ended.Format(time.RFC3339), bound.Format(time.RFC3339))
	}
}

// An ask whose read of its pod's holds fails writes nothing: an ask written with no holds would
// state an end with no hold counted, earlier than the round's. The sweep reads the holds at its
// top; its walk then writes the ask of the first issue's round, and while it does the holds become
// unreadable (pauseHoldsRead), so the ask of the second issue, whose pod a pause holds, cannot read
// them. The next sweep, with the holds readable again, writes the ask, stating the end that counts
// the hold to that moment.
func TestPodUpdateAnAskWhoseReadOfItsPodsHoldsFailsWritesNoAsk(t *testing.T) {
	asked, held := "ACME-1", "ACME-2"
	rig := newLimitRig(t, asked, held)
	rig.tick()
	rig.awaitRounds("the sweep that opened both rounds", asked, held)
	c := rig.claimOf(held, claim.RoleArchitect)
	if c.UpdateOpenedAt == nil {
		t.Fatalf("%s's claim records round %q with no opening", held, c.UpdateRound)
	}
	round, opened := c.UpdateRound, *c.UpdateOpenedAt

	rig.clock.advance(time.Minute)
	heldAt := rig.clock.Now()
	rig.pauseHolds(held, heldAt)

	rig.clock.advance(time.Minute)
	if !rig.sweepWaitingInAWrite(asked, 20*time.Second, func() {
		rig.pauseHoldsRead(false)
		rig.clock.advance(20 * time.Second)
	}) {
		t.Fatalf("the sweep made no write for %s", asked)
	}
	if texts := rig.askTexts(held); len(texts) != 0 {
		t.Fatalf("the sweep whose read of %s's pod holds failed wrote %d asks of its round, want none: %q", held, len(texts), texts)
	}

	rig.pauseHoldsRead(true)
	rig.tick()
	written := rig.clock.Now()
	texts := rig.askTexts(held)
	if len(texts) != 1 {
		t.Fatalf("the sweep after the holds read again wrote %d asks of %s's round, want 1", len(texts), held)
	}
	bound := opened.Add(podUpdateGrace + written.Sub(heldAt))
	if want := supervise.UpdateTask(round, limitDrift, bound); texts[0] != want {
		t.Errorf("the ask of %s's round states %q, want %q: the hold stands, so it counts to the ask, %s",
			held, texts[0], want, bound.Format(time.RFC3339))
	}
}

// An ask whose read of the round's asks still queued fails writes nothing: it cannot say which
// roles an ask already waits for, and asking them again would put a second ask of the round in
// flight for a claim. The read fails here as a read the store ends does: it waits on a lock
// another session holds on the outbox, and is cancelled there (pg_cancel_backend). The next sweep
// reads the queued asks and writes the ask.
func TestPodUpdateAnAskWhoseReadOfTheQueuedAsksFailsWritesNoAsk(t *testing.T) {
	issue := "ACME-1"
	rig := newLimitRig(t, issue)
	rig.tick()
	rig.awaitRounds("the sweep that opened the round", issue)
	c := rig.claimOf(issue, claim.RoleArchitect)
	if c.UpdateOpenedAt == nil {
		t.Fatalf("%s's claim records round %q with no opening", issue, c.UpdateRound)
	}
	round, opened := c.UpdateRound, *c.UpdateOpenedAt

	rig.clock.advance(time.Minute)
	rig.sweepInTheQueuedAskRead("cancelled", func(pid int32) {
		var cancelled bool
		if err := rig.pool.QueryRow(rig.ctx, `select pg_cancel_backend($1)`, pid).Scan(&cancelled); err != nil || !cancelled {
			t.Fatalf("cancel the sweep's read of the queued asks (backend %d) = %t, %v; want it cancelled", pid, cancelled, err)
		}
	})
	if texts := rig.askTexts(issue); len(texts) != 0 {
		t.Fatalf("the sweep whose read of %s's queued asks failed wrote %d asks of its round, want none: %q", issue, len(texts), texts)
	}

	rig.tick()
	texts := rig.askTexts(issue)
	if len(texts) != 1 {
		t.Fatalf("the sweep after the failed read wrote %d asks of %s's round, want 1", len(texts), issue)
	}
	if want := supervise.UpdateTask(round, limitDrift, opened.Add(podUpdateGrace)); texts[0] != want {
		t.Errorf("the ask of %s's round states %q, want %q", issue, texts[0], want)
	}
}

// sweepHeldInTheQueuedAskRead is one sweep whose read of an ask's queued rows (store's
// QueuedUpdateAsks) waits behind a lock that another session holds on the outbox, while the clock
// moves by hold.
func (r *fleetRig) sweepHeldInTheQueuedAskRead(hold time.Duration) {
	r.t.Helper()
	r.sweepInTheQueuedAskRead("held", func(int32) { r.clock.advance(hold) })
}

// sweepInTheQueuedAskRead is one sweep whose read of an ask's queued rows (store's
// QueuedUpdateAsks) waits behind a lock that another session holds on the outbox: once that read
// is seen waiting on it, inRead runs with the backend pid of the read, and then the lock is let
// go. A sweep whose read is never seen waiting fails the test; what names how the read ends.
func (r *fleetRig) sweepInTheQueuedAskRead(what string, inRead func(pid int32)) {
	r.t.Helper()
	claims := r.sweepClaims()
	tx, err := r.pool.Begin(r.ctx)
	if err != nil {
		r.t.Fatalf("begin the session that locks the outbox: %v", err)
	}
	defer func() { _ = tx.Rollback(r.ctx) }()
	if _, err := tx.Exec(r.ctx, `lock table outbox in access exclusive mode`); err != nil {
		r.t.Fatalf("lock the outbox: %v", err)
	}
	swept := make(chan struct{})
	go func() {
		defer close(swept)
		r.s.reconcilePodUpdates(r.ctx, claims)
	}()
	var pid int32
	testwait.Eventually(r.t, "the sweep's read of the queued asks to wait on the lock of the outbox", func() bool {
		if err := r.pool.QueryRow(r.ctx, `select coalesce(max(pid), 0) from pg_stat_activity
			where datname = current_database() and pid <> pg_backend_pid() and wait_event_type = 'Lock'
			and query like 'select payload->>''role'' from outbox%' and query like '%kind = ''update_ask''%'`).Scan(&pid); err != nil {
			r.t.Fatalf("read the sessions that wait on a lock: %v", err)
		}
		return pid != 0
	})
	inRead(pid)
	if err := tx.Rollback(r.ctx); err != nil {
		r.t.Fatalf("let go of the outbox's lock: %v", err)
	}
	testwait.Eventually(r.t, "the sweep whose read of the queued asks was "+what+" to end", func() bool {
		select {
		case <-swept:
			return true
		default:
			return false
		}
	})
}

// pauseHolds opens a pause of the rig's own project at at, as an operator's pause does, and its
// hold of issue's pod (openProjectPause, holdProjectPod): the pair a daemon reads its holds by.
func (r *fleetRig) pauseHolds(issue string, at time.Time) {
	r.t.Helper()
	project := r.s.cfg.Project
	holdProjectPod(r.t, r.sup.store, openProjectPause(r.t, r.pool, project, at), project, issue, at)
}

// askTexts is the text of every update ask the outbox holds for issue, oldest first.
func (r *fleetRig) askTexts(issue string) []string {
	r.t.Helper()
	var texts []string
	for _, row := range r.rows(record.OutboxKindUpdateAsk) {
		if row.Issue != issue {
			continue
		}
		var ask record.UpdateAsk
		if err := json.Unmarshal(row.Payload, &ask); err != nil {
			r.t.Fatalf("decode the update ask %d: %v", row.ID, err)
		}
		texts = append(texts, ask.Text)
	}
	return texts
}
