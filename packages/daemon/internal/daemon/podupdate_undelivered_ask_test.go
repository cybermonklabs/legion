package daemon

// An update round's ask counts once it reached a session (dispatch://INFRA-4109 R4,
// dispatch://INFRA-4131 R4, verification's c-undelivered-ask-asked-again). The sweep enqueues the
// ask and marks nothing; the outbox's row publishes it under the claim's own lock and marks the
// claim asked only when the publish reached a session (updateAsk, supervise's
// Machine.PublishUpdateAsk). So an ask that finds no live holder on the role's topic
// (notify.ErrNoHolder) leaves the claim unasked, a later sweep asks it again, and the round ends
// once it answers, long before its bound; an ask that reached a session is the only one its role
// gets, whatever other row of the round runs before or after it.
//
// These cases drive the pod-update rig of podupdate_test.go, whose role-message lane runs each ask
// row the sweep wrote through the outbox's own executor, against a listener that refuses a topic
// no live session holds as the real one does (holderPublisher, notice_routing_test.go).

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/notify"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/store/migrations"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/testwait"
	"github.com/sjawhar/legion/daemon/internal/wait"
)

// updateAskCutover is the migration that clears the ask mark an earlier release wrote when it
// queued an ask still queued at the deploy (store's 20261117200337).
const updateAskCutover = 20261117200337

// migrate runs the embedded migration of version over the rig's database again, as the deploy's
// Migrate runs it once over a database the earlier release left. The rig's database was built
// from every embedded migration before any claim was seeded, so this is the one run that finds
// the state the migration is for.
func (r *podUpdateRig) migrate(version int) {
	r.t.Helper()
	all, err := migrations.All()
	if err != nil {
		r.t.Fatalf("read the embedded migrations: %v", err)
	}
	for _, migration := range all {
		if migration.Version == version {
			if _, err := r.pool.Exec(r.ctx, migration.SQL); err != nil {
				r.t.Fatalf("run %s: %v", migration.Name, err)
			}
			return
		}
	}
	r.t.Fatalf("no embedded migration has version %d", version)
}

// A deploy stops the earlier release with an ask it queued, and marked asked at the enqueue, still
// in the outbox. This release reads the mark as an ask that reached a session, so its row would
// publish nothing. The cutover migration clears that mark, so after the deploy the row publishes
// the ask, which reaches the session and marks the claim, and no sweep asks again: the role reads
// the ask once.
//
// The negative control is the same deploy without the migration: the row finds the earlier
// release's mark and publishes nothing, so the role is never asked and the round waits for its
// bound. It is run by leaving the migrate call out.
func TestPodUpdateAskTheEarlierReleaseQueuedAndMarkedIsDeliveredOnceAfterTheCutover(t *testing.T) {
	t.Parallel()
	earlier := readyWorker(claim.RoleImplementer)
	earlier.round, earlier.reasons = podUpdateRound, podUpdateDrift
	rig := newPodUpdateRig(t, earlier)
	if c := rig.claimOf(claim.RoleImplementer); c.UpdateTaskedAt == nil {
		t.Fatalf("the implementer holds no ask mark, want the earlier release's mark written at the enqueue")
	}
	if written, err := rig.s.enqueue(rig.ctx, podUpdateIssue, record.UpdateAsk{
		Role: claim.RoleImplementer, Round: podUpdateRound, Pod: podUpdatePod,
		Text: supervise.UpdateTask(claim.RoleImplementer, podUpdateRound, podUpdateDrift, podUpdateBound),
	}); err != nil || !written {
		t.Fatalf("queue the earlier release's ask = %t, %v; want it written", written, err)
	}

	rig.migrate(updateAskCutover)
	next := rig.restart()
	next.drifts(podUpdateDrift)
	next.runLane()
	if n := next.deliveredTo(claim.RoleImplementer); n != 1 {
		t.Fatalf("the implementer's topic was delivered %d asks after the deploy, want the queued one", n)
	}
	if c := next.claimOf(claim.RoleImplementer); c.UpdateTaskedAt == nil {
		t.Fatalf("the implementer records no ask after its queued ask was delivered, want it marked")
	}
	for range 3 {
		next.clock.advance(time.Minute)
		next.tick()
		next.runLane()
	}
	if n := next.deliveredTo(claim.RoleImplementer); n != 1 {
		t.Errorf("the implementer's topic was delivered %d asks, want one in all", n)
	}
	if n := next.askedOf(claim.RoleImplementer); n != 1 {
		t.Errorf("the implementer was asked %d times, want the one the earlier release queued", n)
	}
}

// deliveredTo is how many asks the rig's listener delivered to role's topic.
func (r *podUpdateRig) deliveredTo(role claim.Role) int {
	r.listener.mu.Lock()
	defer r.listener.mu.Unlock()
	n := 0
	for _, publish := range r.listener.delivered {
		if publish.topic == r.roleTopic(role) {
			n++
		}
	}
	return n
}

// askedOf is how many update-ask rows the sweep has written for role, finished or not.
func (r *podUpdateRig) askedOf(role claim.Role) int {
	r.t.Helper()
	n := 0
	for _, ask := range r.asks() {
		if ask.Role == role {
			n++
		}
	}
	return n
}

// A round opens over two ready roles. The implementer's fresh session has not claimed its topic
// yet, so its first ask finds no holder and ends undelivered; the tester's first ask is delivered.
// The ask that reached no session leaves the implementer unasked, so a later sweep asks it again,
// once its session holds the topic, and that ask is delivered. Each role then answers, and the
// pod is replaced minutes after the round opened, long before its thirty-minute bound and with no
// bound named on the replacement's line.
//
// The control is the tester: its first ask reached a session, so it is marked and asked once over
// every sweep of the round. The implementer is asked twice, and no more once its second ask was
// delivered.
//
// The negative control is the rule of main before this change, BET-1291's planner and BET-690's
// reviewer on 2026-11-17: the sweep marked the claim asked when it enqueued the ask, the row that
// found no holder finished, and no sweep asked again, so the round waited for its bound.
func TestPodUpdateAskThatFoundNoHolderIsAskedAgainAndTheRoundEndsLongBeforeItsBound(t *testing.T) {
	t.Parallel()
	rig := newPodUpdateRig(t, readyWorker(claim.RoleImplementer), readyWorker(claim.RoleTester))
	rig.drifts(podUpdateDrift)
	clocks := rig.replacementClocks()
	rig.listener.setAbsent(rig.roleTopic(claim.RoleImplementer))

	rig.tick()
	rig.awaitAskQueued(claim.RoleImplementer, podUpdateRound)
	rig.awaitAskQueued(claim.RoleTester, podUpdateRound)
	rig.runLane()

	lost := loggedLines(t, rig.logs.String(), `"msg":"`+askNoHolderLine+`"`)
	if len(lost) == 0 {
		t.Fatalf("no line %q in\n%s\nwant the implementer's ask finished undelivered", askNoHolderLine, rig.logs.String())
	}
	for _, line := range lost {
		if line["role"] != string(claim.RoleImplementer) {
			t.Fatalf("line %q = %v, want it for the implementer's ask alone", askNoHolderLine, line)
		}
	}
	if c := rig.claimOf(claim.RoleImplementer); c.UpdateTaskedAt != nil {
		t.Fatalf("the implementer's ask reached no session, and its claim records it asked at %v; want it unasked, so a later sweep asks it again", c.UpdateTaskedAt)
	}
	if c := rig.claimOf(claim.RoleTester); c.UpdateTaskedAt == nil {
		t.Fatalf("the tester's ask was delivered, and its claim records no ask; want it marked")
	}

	// The implementer's session now holds its topic, as a ready session does inside its first
	// turn or at the first-turn bound (supervise.SaidReady).
	rig.listener.setAbsent()
	rig.clock.advance(time.Minute)
	rig.awaitAsked(claim.RoleImplementer, podUpdateRound)
	for range 3 {
		rig.clock.advance(time.Minute)
		rig.tick()
		rig.runLane()
	}

	if n := rig.askedOf(claim.RoleImplementer); n != 2 {
		t.Errorf("the implementer was asked %d times, want twice: once lost, once delivered", n)
	}
	if n := rig.askedOf(claim.RoleTester); n != 1 {
		t.Errorf("the tester was asked %d times, want once: its first ask was delivered", n)
	}
	for _, role := range []claim.Role{claim.RoleImplementer, claim.RoleTester} {
		if n := rig.deliveredTo(role); n != 1 {
			t.Errorf("the %s's topic was delivered %d asks, want one", role, n)
		}
	}

	rig.signal(claim.RoleImplementer, "a1b2c3d")
	rig.signal(claim.RoleTester, "")
	rig.awaitReplacement(1, "once both roles answered")
	at := clocks()
	if len(at) != 1 || !at[0].Before(podUpdateStart.Add(10*time.Minute)) {
		t.Fatalf("the pod was replaced at %v, want once, minutes after the round opened at %s and long before its bound at %s",
			at, podUpdateStart, podUpdateBound)
	}
	if ended := loggedLines(t, rig.logs.String(), podUpdateReplacedLine); len(ended) != 1 || ended[0]["atTheBound"] != false {
		t.Fatalf("the round's end was logged %+v, want one line that no bound made", ended)
	}
}

// The row can run as soon as the enqueue commits, before the sweep takes its next step. Here the
// lane runs it inside the enqueue itself, and the role's topic has no holder: the ask is lost,
// and nothing the sweep does after the enqueue may count it as given. The next sweep asks the
// claim again, and that ask, once the session holds the topic, is delivered and marks the claim.
//
// The negative control is a mark made after the enqueue, which is where the sweep made it: on
// main the lost row finished and the sweep's mark stood, and with the take-back of 74ab1cd3 the
// row found no mark to take back, the sweep's mark landed after it, and no sweep asked again.
func TestPodUpdateAskThatFoundNoHolderBeforeTheSweepMovedOnIsAskedAgain(t *testing.T) {
	t.Parallel()
	rig := newPodUpdateRig(t, readyWorker(claim.RoleImplementer))
	rig.drifts(podUpdateDrift)
	rig.listener.setAbsent(rig.roleTopic(claim.RoleImplementer))
	enqueue := rig.s.enqueue
	rig.s.enqueue = func(ctx context.Context, issue string, payloads ...record.OutboxPayload) (bool, error) {
		written, err := enqueue(ctx, issue, payloads...)
		if err != nil || !written {
			return written, err
		}
		for _, payload := range payloads {
			if _, ask := payload.(record.UpdateAsk); ask {
				rig.runLane()
				break
			}
		}
		return true, nil
	}

	rig.tick()
	rig.awaitRound(claim.RoleImplementer, podUpdateRound)
	rig.tick()
	if n := rig.askedOf(claim.RoleImplementer); n != 1 {
		t.Fatalf("the sweep after the round recorded itself asked the implementer %d times, want once", n)
	}
	if n := rig.deliveredTo(claim.RoleImplementer); n != 0 {
		t.Fatalf("the implementer's topic was delivered %d asks while no session held it, want none", n)
	}
	if c := rig.claimOf(claim.RoleImplementer); c.UpdateTaskedAt != nil {
		t.Fatalf("the implementer's ask was lost before the sweep moved on, and its claim records it asked at %v; want it unasked", c.UpdateTaskedAt)
	}

	rig.listener.setAbsent()
	rig.clock.advance(time.Minute)
	rig.awaitAsked(claim.RoleImplementer, podUpdateRound)
	for range 3 {
		rig.clock.advance(time.Minute)
		rig.tick()
	}
	if n := rig.askedOf(claim.RoleImplementer); n != 2 {
		t.Errorf("the implementer was asked %d times, want twice: once lost, once delivered", n)
	}
	if n := rig.deliveredTo(claim.RoleImplementer); n != 1 {
		t.Errorf("the implementer's topic was delivered %d asks, want one", n)
	}
}

// Two rows of one round for one claim: an older row and a newer one, both queued before the
// sweep asks — what a daemon before this change could leave, a row whose mark the store refused
// beside the row the next sweep wrote for the same claim. Whichever order they meet a holder in,
// the role is delivered one ask: the row that reaches the session marks the claim under its lock,
// the other finds the mark and publishes nothing, and an older row that found no holder takes
// nothing from the newer one. The sweep asks no claim while an ask of it waits in the outbox, and
// none once an ask reached it.
//
// The negative control is 74ab1cd3's take-back: the older row that found no holder cleared the
// mark the sweep had made for the newer row, the newer row's delivery left the claim unmarked,
// and the next sweep asked the session again. On main, with no take-back, every queued row
// publishes, and a session the first row reached is asked by the second as well.
func TestPodUpdateAskNoHolderOfAnOlderRowLeavesOneDeliveredAskOfTheRound(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		// unheld is whether the older row runs while the role's topic has no holder.
		unheld bool
	}{
		{name: "the older row finds no holder, and the newer row the session", unheld: true},
		{name: "both rows find the session", unheld: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rig := newPodUpdateRig(t, readyWorker(claim.RoleImplementer))
			rig.drifts(podUpdateDrift)
			rig.tick()
			rig.awaitRound(claim.RoleImplementer, podUpdateRound)
			text := supervise.UpdateTask(claim.RoleImplementer, podUpdateRound, podUpdateDrift, podUpdateBound)
			for range 2 {
				if written, err := rig.s.enqueue(rig.ctx, podUpdateIssue, record.UpdateAsk{
					Role: claim.RoleImplementer, Round: podUpdateRound, Pod: podUpdatePod, Text: text,
				}); err != nil || !written {
					t.Fatalf("queue a row of the round = %t, %v; want it written", written, err)
				}
			}
			rig.tick()
			rows := rig.rows(record.OutboxKindUpdateAsk)
			if len(rows) < 2 {
				t.Fatalf("the outbox holds %d asks, want the two queued rows", len(rows))
			}

			if tc.unheld {
				rig.listener.setAbsent(rig.roleTopic(claim.RoleImplementer))
			}
			rig.runAsk(rows[0])
			rig.listener.setAbsent()
			rig.runLane()
			rig.awaitAsked(claim.RoleImplementer, podUpdateRound)
			for range 3 {
				rig.clock.advance(time.Minute)
				rig.tick()
				rig.runLane()
			}

			if n := rig.deliveredTo(claim.RoleImplementer); n != 1 {
				t.Fatalf("the implementer's topic was delivered %d asks of the round, want one", n)
			}
			if c := rig.claimOf(claim.RoleImplementer); c.UpdateTaskedAt == nil {
				t.Fatalf("the implementer records no ask, want the delivered ask marked")
			}
			if n := rig.askedOf(claim.RoleImplementer); n != 2 {
				t.Errorf("the implementer was asked %d times, want the two queued rows alone: the sweep asks no claim an ask of the round reached or waits for", n)
			}
		})
	}
}

// Two claims of one issue can hold one role: the agent implementer's claim on the tester's step,
// beside the tester's own claim of an earlier version of the plan. An ask of the round still
// queued for the one is no ask of the other, so the sweep asks the tester's own claim while the
// keyed claim's ask waits in the outbox (store's QueuedUpdateAsks, read by the claim each row
// names). A row the release before wrote names no claim and stands for each claim of its role, so
// the sweep asks no claim of the tester while it waits.
func TestPodUpdateAsksAClaimWhileAnotherClaimOfItsRoleHasAnAskQueued(t *testing.T) {
	t.Parallel()
	keyed := mustClaimToken(t, podUpdateIssue, claim.RoleImplementer)
	for _, tc := range []struct {
		name string
		// queued is the claim the row already queued names, "" for a row the release before wrote.
		queued claim.Token
		// asked is how many asks the sweep writes the tester's own claim.
		asked int
	}{
		{name: "a row naming the keyed claim", queued: keyed, asked: 1},
		{name: "a row naming no claim", queued: "", asked: 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rig := newPodUpdateRig(t, readyWorker(claim.RoleTester))
			rig.drifts(podUpdateDrift)
			rig.tick()
			rig.awaitRound(claim.RoleTester, podUpdateRound)
			text := supervise.UpdateTask(claim.RoleTester, podUpdateRound, podUpdateDrift, podUpdateBound)
			if written, err := rig.s.enqueue(rig.ctx, podUpdateIssue, record.UpdateAsk{
				Role: claim.RoleTester, Claim: tc.queued, Round: podUpdateRound, Pod: podUpdatePod, Text: text,
			}); err != nil || !written {
				t.Fatalf("queue the other claim's ask of the round = %t, %v; want it written", written, err)
			}
			rig.tick()

			own := rig.tokens[claim.RoleTester]
			n := 0
			for _, ask := range rig.asks() {
				if ask.Claim == own {
					n++
				}
			}
			if n != tc.asked {
				t.Fatalf("the sweep wrote %d asks of the round to the tester's own claim %s while an ask naming %q waited, want %d",
					n, own, tc.queued, tc.asked)
			}
		})
	}
}

// The ask is published and marked under the claim's own lock, and that lock is taken without
// waiting (supervise's Machine.PublishUpdateAsk). An issue's role-message lane runs one row at a
// time (record.LaneKey), and a Sandbox relaunch holds a claim's lock for minutes while the runtime
// waits out its pod, so a row that waited for that lock would hold every hook prompt, merge
// forward's message and ask of that issue behind one relaunch. Here the claim's machine is held
// inside a Resume: the row returns a wait at once, having published nothing and marked nothing,
// and the row's next attempt once the relaunch is over publishes the ask, which is delivered once
// in all.
//
// The negative control is a row that takes the lock with waiting: the run below does not return
// while the relaunch is held inside its Resume, and the wait for its answer gives up by name at the
// test binary's deadline (testwait.Receive). On 74ab1cd3, which published with no lock held, the
// row publishes while the relaunch holds the claim.
func TestPodUpdateAskWaitsWhileItsClaimsLockIsHeldAndIsDeliveredOnce(t *testing.T) {
	t.Parallel()
	rig := newPodUpdateRig(t, readyWorker(claim.RoleImplementer))
	rig.drifts(podUpdateDrift)
	rig.tick()
	row := rig.awaitAskQueued(claim.RoleImplementer, podUpdateRound)
	rig.settleAll()

	// The implementer's machine is held inside a Resume: it holds its own lock for the whole
	// call, as a relaunch into a pod being built does.
	inside, held := make(chan struct{}), make(chan struct{})
	release := sync.OnceFunc(func() { close(held) })
	defer release()
	rig.rt.HoldResume(func() {
		rig.rt.HoldResume(nil)
		close(inside)
		<-held
	})
	m, _ := rig.sup.Machine(rig.tokens[claim.RoleImplementer])
	gone := supervise.RuntimeObservation{Observation: runtime.Observation{
		Locator: *rig.claimOf(claim.RoleImplementer).Locator, Kind: runtime.Gone, Detail: "the process ended",
	}}
	go func() { _ = m.Handle(rig.worker(), gone) }()
	testwait.Receive(t, "the relaunch to reach its Resume", inside)

	ran := make(chan error, 1)
	go func() { ran <- rig.lane.execute(rig.worker(), row) }()
	err := testwait.Receive(t, "the ask's answer while its claim's machine is inside a Resume; a row that waits on the lock gives none", ran)
	if !errors.Is(err, wait.ErrWaiting) {
		t.Fatalf("the ask over a held claim = %v, want a wait that keeps the row", err)
	}
	t.Logf("the row waits: %v", err)
	if n := rig.deliveredTo(claim.RoleImplementer); n != 0 {
		t.Fatalf("the ask was delivered %d times while the claim's lock was held, want none: the publish and its mark are one step under that lock", n)
	}
	if v := m.View(); v.UpdateTaskedAt != nil {
		t.Fatalf("the claim records an ask at %v while its lock was held, want none", v.UpdateTaskedAt)
	}

	release()
	rig.runAsk(row)
	if c := rig.claimOf(claim.RoleImplementer); c.UpdateTaskedAt == nil {
		t.Fatalf("the claim records no ask after the row's next attempt, want the delivered ask marked")
	}
	for range 3 {
		rig.clock.advance(time.Minute)
		rig.tick()
		rig.runLane()
	}
	if n := rig.deliveredTo(claim.RoleImplementer); n != 1 {
		t.Errorf("the implementer's topic was delivered %d asks, want one in all", n)
	}
	if n := rig.askedOf(claim.RoleImplementer); n != 1 {
		t.Errorf("the implementer was asked %d times, want once", n)
	}
}

// stoppingPublisher delivers through the listener and then cancels the context its row runs
// under, as a daemon stop does while a row is in its publish.
type stoppingPublisher struct {
	inner  notify.Publisher
	cancel context.CancelFunc
}

func (p stoppingPublisher) Publish(ctx context.Context, topic, message string, payload any, key string) error {
	err := p.inner.Publish(ctx, topic, message, payload, key)
	p.cancel()
	return err
}

func (p stoppingPublisher) PublishKept(ctx context.Context, topic, message string, payload any, key string) (bool, error) {
	kept, err := p.inner.PublishKept(ctx, topic, message, payload, key)
	p.cancel()
	return kept, err
}

// A daemon stop that cancels the lane while its row is in a delivered publish still leaves the
// mark written: the mark's write runs under context.WithoutCancel, bounded by the claim-write
// deadline (supervise's Machine.PublishUpdateAsk), as the outbox's own settle takes a fresh
// context. Otherwise every ordinary stop after a delivered publish would leave the stored claim
// unmarked, and the next boot would ask the role again. The negative control is dc009ea4, which
// wrote the mark under the lane's context: the store refused the write as cancelled, the row
// answered an error and the stored claim held no mark.
func TestPodUpdateAskDeliveredAsTheDaemonStopsIsStillMarked(t *testing.T) {
	t.Parallel()
	rig := newPodUpdateRig(t, readyWorker(claim.RoleImplementer))
	rig.drifts(podUpdateDrift)
	rig.tick()
	rig.awaitRound(claim.RoleImplementer, podUpdateRound)
	row := rig.awaitAskQueued(claim.RoleImplementer, podUpdateRound)
	rig.settleAll()

	stopping, stop := context.WithCancel(rig.ctx)
	defer stop()
	rig.lane.notices = stoppingPublisher{inner: rig.listener, cancel: stop}
	if err := rig.lane.execute(stopping, row); err != nil {
		t.Fatalf("the ask delivered as the daemon stopped = %v, want the row finished", err)
	}
	if n := rig.deliveredTo(claim.RoleImplementer); n != 1 {
		t.Fatalf("the implementer's topic was delivered %d asks, want one", n)
	}
	var marked *time.Time
	if err := rig.pool.QueryRow(rig.ctx, "select update_tasked_at from claims where token = $1",
		string(rig.tokens[claim.RoleImplementer])).Scan(&marked); err != nil {
		t.Fatalf("read the implementer's stored claim: %v", err)
	}
	if marked == nil {
		t.Fatal("the ask reached the implementer as the daemon stopped, and the stored claim records no mark; want it marked, so the next boot does not ask again")
	}
}

// An ask of a round reaches a role's agent only while its claim holds no answer of that round,
// read under the claim's own lock where the ask is published (supervise's
// Machine.PublishUpdateAsk). The sweep reads the claims before the ask is published, so a role
// that answers in between is told nothing, and the row ends on its own line (askAnsweredLine). A
// role asked first and answering after is told once, and that line is not written.
func TestPodUpdateAskReachesNoRoleThatAnsweredBeforeItsPublish(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name        string
		answerFirst bool
		want        int
	}{
		{name: "the answer, then the ask", answerFirst: true, want: 0},
		{name: "the ask, then the answer", answerFirst: false, want: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rig := newPodUpdateRig(t, readyWorker(claim.RoleImplementer))
			rig.drifts(podUpdateDrift)
			rig.tick()
			row := rig.awaitAskQueued(claim.RoleImplementer, podUpdateRound)
			if tc.answerFirst {
				rig.signal(claim.RoleImplementer, "a1b2c3d")
			}
			rig.runAsk(row)
			if !tc.answerFirst {
				rig.signal(claim.RoleImplementer, "a1b2c3d")
			}
			if n := rig.deliveredTo(claim.RoleImplementer); n != tc.want {
				t.Fatalf("the implementer's topic was delivered %d asks of the round, want %d", n, tc.want)
			}
			lines := loggedLines(t, rig.logs.String(), `"msg":"`+askAnsweredLine+`"`)
			if tc.want == 1 && len(lines) != 0 {
				t.Fatalf("lines %q = %v, want none: the ask reached the role before it answered", askAnsweredLine, lines)
			}
			if tc.want == 0 && (len(lines) != 1 || lines[0]["level"] != "INFO" || lines[0]["row"] != float64(row.ID) || lines[0]["round"] != podUpdateRound) {
				t.Fatalf("lines %q = %v, want one Info line naming row %d and round %s", askAnsweredLine, lines, row.ID, podUpdateRound)
			}
		})
	}
}
