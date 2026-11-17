package daemon

// An update round's ask that reached no session is lost, not given (dispatch://INFRA-4109 R4,
// dispatch://INFRA-4131 R4, verification's c-undelivered-ask-asked-again). The ask row that finds
// no live holder on the role's topic (notify.ErrNoHolder) takes the claim's ask mark back
// (updateAsk, supervise's Machine.PodUpdateAskLost), so a later sweep asks the claim again, and
// the round ends once it answers, long before its bound. An ask that reached a session keeps its
// mark, and that claim is asked once.
//
// These cases drive the pod-update rig of podupdate_test.go and run each ask row the sweep wrote
// through the outbox's own executor, against a listener that refuses a topic no live session
// holds as the real one does (holderPublisher, notice_routing_test.go).

import (
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/notify"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/testwait"
	"github.com/sjawhar/legion/daemon/internal/wait"
)

// roleTopic is the Envoy topic of role's claim, which the round's ask to that role goes to.
func (r *podUpdateRig) roleTopic(role claim.Role) string {
	return notify.RoleTopicPrefix + string(r.tokens[role])
}

// askRunner is the outbox's executor of the rig's ask rows: the rig's project and supervisor, the
// listener given, and the sweep's own logger, so the rig's log holds the executor's lines too.
func (r *podUpdateRig) askRunner(listener notify.Publisher) *outbox {
	return &outbox{project: podUpdateProject, notices: listener, supervisor: r.sup, log: r.log}
}

// runAsks runs every update-ask row the sweep has written and ran does not hold yet, oldest
// first, through runner, as the role-message lane runs them, and records each in ran. Each run
// starts with every machine settled, as the lane's rows meet them between sweeps; a row that
// still meets a machine deciding (a post of the sweep's landing) answers a wait and is run again,
// as the lane runs a waiting row again. Every row here is delivered or finds no holder, which both
// finish the row, so any other error fails the test.
func (r *podUpdateRig) runAsks(runner *outbox, ran map[int64]bool) {
	r.t.Helper()
	for _, row := range r.rows(record.OutboxKindUpdateAsk) {
		if ran[row.ID] {
			continue
		}
		ran[row.ID] = true
		testwait.Eventually(r.t, fmt.Sprintf("update ask %d to finish", row.ID), func() bool {
			r.settleAll()
			err := runner.execute(r.ctx, row)
			if errors.Is(err, wait.ErrWaiting) {
				return false
			}
			if err != nil {
				r.t.Fatalf("run update ask %d: %v", row.ID, err)
			}
			return true
		})
	}
}

// A round opens over two ready roles. The implementer's fresh session has not claimed its topic
// yet, so its first ask finds no holder and ends undelivered; the tester's first ask is delivered.
// The ask that reached no session leaves the implementer unasked, so a later sweep asks it again,
// once its session holds the topic, and that ask is delivered. Each role then answers, and the
// pod is replaced minutes after the round opened, long before its thirty-minute bound and with no
// bound named on the replacement's line.
//
// The control is the tester: its first ask reached a session, so it keeps its mark and is asked
// once over every sweep of the round. The implementer is asked twice, and no more once its second
// ask was delivered.
//
// The negative control is the rule before this one, BET-1291's planner and BET-690's reviewer on
// 2026-11-17: the row that found no holder finished and left the claim marked asked, so no sweep
// asked it again and the round waited for its bound.
func TestPodUpdateAskThatFoundNoHolderIsAskedAgainAndTheRoundEndsLongBeforeItsBound(t *testing.T) {
	rig := newPodUpdateRig(t, readyWorker(claim.RoleImplementer), readyWorker(claim.RoleTester))
	rig.drifts(podUpdateDrift)
	clocks := rig.replacementClocks()
	listener := &holderPublisher{}
	listener.setAbsent(rig.roleTopic(claim.RoleImplementer))
	runner := rig.askRunner(listener)
	ran := map[int64]bool{}

	rig.tick()
	rig.awaitAsked(claim.RoleImplementer, podUpdateRound)
	rig.awaitAsked(claim.RoleTester, podUpdateRound)
	rig.runAsks(runner, ran)

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
		t.Fatalf("the tester's ask was delivered, and its claim records no ask; want the mark kept")
	}

	// The implementer's session now holds its topic, as a ready session does inside its first
	// turn or at the first-turn bound (supervise.SaidReady).
	listener.setAbsent()
	rig.clock.advance(time.Minute)
	rig.awaitAsked(claim.RoleImplementer, podUpdateRound)
	rig.runAsks(runner, ran)
	for range 3 {
		rig.clock.advance(time.Minute)
		rig.tick()
		rig.runAsks(runner, ran)
	}

	asked := map[claim.Role]int{}
	for _, ask := range rig.asks() {
		asked[ask.Role]++
	}
	if asked[claim.RoleImplementer] != 2 {
		t.Errorf("the implementer was asked %d times, want twice: once lost, once delivered", asked[claim.RoleImplementer])
	}
	if asked[claim.RoleTester] != 1 {
		t.Errorf("the tester was asked %d times, want once: its first ask was delivered", asked[claim.RoleTester])
	}
	delivered := map[string]int{}
	for _, publish := range listener.delivered {
		delivered[publish.topic]++
	}
	for _, role := range []claim.Role{claim.RoleImplementer, claim.RoleTester} {
		if n := delivered[rig.roleTopic(role)]; n != 1 {
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

// The executor reads the claim it takes the mark back from through the machine's view, and it
// takes the machine's lock only where no decision holds it (supervise's Machine.Deciding). The
// role-message lane runs one row at a time, and a Sandbox relaunch holds a claim's machine for
// minutes while the runtime waits out its pod, so a take-back that waited for that lock would
// hold every phase-ended message, hook prompt and ask of every issue behind one relaunch. So an
// ask that finds no holder on the topic of a claim whose machine is deciding is a wait: the row
// is kept, the mark stands, and the row's next attempt publishes the ask again, which takes the
// mark back once the decision is over.
//
// The negative control is a take-back that waits for the lock: the run below does not return
// while the relaunch is held inside its Resume.
func TestPodUpdateAskThatFoundNoHolderWaitsWhileItsClaimsMachineDecides(t *testing.T) {
	rig := newPodUpdateRig(t, readyWorker(claim.RoleImplementer))
	rig.drifts(podUpdateDrift)
	listener := &holderPublisher{}
	listener.setAbsent(rig.roleTopic(claim.RoleImplementer))
	runner := rig.askRunner(listener)

	rig.tick()
	rig.awaitAsked(claim.RoleImplementer, podUpdateRound)
	rows := rig.rows(record.OutboxKindUpdateAsk)
	if len(rows) != 1 {
		t.Fatalf("the sweep wrote %d asks, want one", len(rows))
	}
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
	go func() { _ = m.Handle(rig.ctx, gone) }()
	select {
	case <-inside:
	case <-time.After(10 * time.Second):
		t.Fatal("the relaunch never reached its Resume")
	}

	ran := make(chan error, 1)
	go func() { ran <- runner.execute(rig.ctx, rows[0]) }()
	select {
	case err := <-ran:
		if !errors.Is(err, wait.ErrWaiting) {
			t.Fatalf("the ask that found no holder over a deciding machine = %v, want a wait that keeps the row", err)
		}
		t.Logf("the row waits: %v", err)
	case <-time.After(10 * time.Second):
		release()
		<-ran
		t.Fatal("the ask that found no holder did not return while its claim's machine was inside a Resume; it waited on the decision")
	}
	if v := m.View(); v.UpdateTaskedAt == nil {
		t.Fatalf("the claim's mark was taken back while its machine decided; want it standing until the row's next attempt")
	}

	release()
	rig.settleAll()
	if err := runner.execute(rig.ctx, rows[0]); err != nil {
		t.Fatalf("the row's next attempt = %v, want the row finished", err)
	}
	if c := rig.claimOf(claim.RoleImplementer); c.UpdateTaskedAt != nil {
		t.Fatalf("the claim records its lost ask at %v after the row's next attempt; want it unasked", c.UpdateTaskedAt)
	}
	listener.mu.Lock()
	attempts := listener.attempts
	listener.mu.Unlock()
	if attempts != 2 {
		t.Errorf("the listener was asked %d times, want twice: the first attempt and the one after the decision", attempts)
	}
}
