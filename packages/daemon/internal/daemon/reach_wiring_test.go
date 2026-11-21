package daemon

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/stream"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/testwait"
)

// The close of a connection carries the generation its hello resolved to into the machine, so
// the close of a generation the claim has left is not read as the claim's close (R3).
func TestTheStreamsCloseCarriesItsGenerationToTheMachine(t *testing.T) {
	ev, err := superviseEvent(stream.Closed{Claim: "legion-legion-legion-209-implementer", Generation: 7})
	if err != nil {
		t.Fatal(err)
	}
	if closed, ok := ev.(supervise.StreamClosed); !ok || closed.Generation != 7 {
		t.Fatalf("superviseEvent = %#v, want a StreamClosed of generation 7", ev)
	}
}

// relaunchRig is the controller's `relaunch` over a bare supervisor on the fake runtime and the
// reach rig's keeper, as the API route reaches it (supervision.relaunch).
func relaunchRig(t *testing.T) (*supervision, *reachRig, *supervisor, *runtime.Locator) {
	t.Helper()
	sup, _ := newPluginSupervisor(t, api.DaemonAPIVersion, true)
	// The store's report of the launch, which the machine's own guard reads: the activity the
	// claim was restored with, read off its published view, since the read is made inside the
	// machine's decision.
	sup.deps.ClaimActivity = func(_ context.Context, token claim.Token, _ uint64) (supervise.ActivityReport, bool, error) {
		m, ok := sup.Machine(token)
		if !ok {
			return supervise.ActivityReport{}, false, nil
		}
		c := m.View().Claim
		return supervise.ActivityReport{At: c.ActivityAt, Background: c.ActivityBackground, Kinds: c.ActivityKinds}, !c.ActivityAt.IsZero(), nil
	}
	r := newReachRig(t, false)
	s := &supervision{log: quietLogger(), supervisor: sup, reach: r.k}
	return s, r, sup, nil
}

// waitingQuietly is c in a long wait with nothing running and a report of its launch, which the
// guard lets through.
func waitingQuietly(c supervise.Claim) supervise.Claim {
	c.State = supervise.StateWorking
	c.Session, c.SessionFile = "ses_"+string(c.Role), "/sessions/"+string(c.Role)+".jsonl"
	c.PluginContract = api.DaemonAPIVersion
	c.Step = supervise.StepRecord{At: reachBoot.Add(-time.Hour), Tool: "wait", CallID: "toolu_wait", StartedAt: reachBoot.Add(-time.Hour)}
	c.ActivityAt = reachBoot.Add(-50 * time.Minute)
	c.Pending = &supervise.Delivery{ID: "delivery", Task: "work", QueuedAt: reachBoot.Add(-2 * time.Hour),
		DeliveredAt: reachBoot.Add(-2 * time.Hour), ConfirmedAt: reachBoot.Add(-2 * time.Hour)}
	return c
}

func TestTheControllersRelaunchChecksWhatOnlyTheDaemonKnowsFirst(t *testing.T) {
	s, r, sup, _ := relaunchRig(t)
	worker := waitingQuietly(reachClaim(t, "LEGION-208", "LEGION-209", claim.RoleImplementer, "ses_implementer"))
	if _, err := sup.restore(context.Background(), []supervise.Claim{worker}); err != nil {
		t.Fatal(err)
	}
	m, _ := sup.Machine(worker.Token)

	if err := s.relaunch(context.Background(), m); !errors.Is(err, supervise.ErrClaimReachable) {
		t.Fatalf("relaunch of a claim not marked = %v, want ErrClaimReachable", err)
	}

	r.at(21 * time.Minute)
	r.listing(map[string]time.Duration{"ses_other": 21 * time.Minute})
	r.sweep(worker)
	if !r.reach(worker.Token).Unreachable() {
		t.Fatal("the worker was not marked")
	}
	generation := m.Claim().Generation
	if err := s.relaunch(context.Background(), m); err != nil {
		t.Fatalf("relaunch of a marked claim the guard lets through = %v", err)
	}
	testwait.Eventually(t, "the relaunch to move the claim's generation", func() bool { return m.Claim().Generation > generation })
	if got := r.reach(worker.Token); !got.Relaunched || got.RelaunchedAt.IsZero() || got.Generation != m.Claim().Generation {
		t.Fatalf("the keeper's record after the relaunch = %+v, want the relaunch recorded", got)
	}
	if err := s.relaunch(context.Background(), m); !errors.Is(err, supervise.ErrRelaunchedRecently) {
		t.Fatalf("a second relaunch within 6 hours = %v, want ErrRelaunchedRecently", err)
	}
}

func TestTheControllersRelaunchRefusedByTheGuardIsRecordedForItsNotices(t *testing.T) {
	s, r, sup, _ := relaunchRig(t)
	worker := waitingQuietly(reachClaim(t, "LEGION-208", "LEGION-209", claim.RoleImplementer, "ses_implementer"))
	worker.ActivityBackground, worker.ActivityKinds = true, []string{"subagent"}
	if _, err := sup.restore(context.Background(), []supervise.Claim{worker}); err != nil {
		t.Fatal(err)
	}
	m, _ := sup.Machine(worker.Token)
	r.at(21 * time.Minute)
	r.listing(map[string]time.Duration{"ses_other": 21 * time.Minute})
	r.sweep(worker)

	if err := s.relaunch(context.Background(), m); !errors.Is(err, supervise.ErrRelaunchGuard) {
		t.Fatalf("relaunch of a claim whose report shows a subagent = %v, want the guard's refusal", err)
	}
	if got := r.reach(worker.Token); got.GuardRefusedAt.IsZero() || got.Relaunched {
		t.Fatalf("the keeper's record after the refusal = %+v, want the refusal recorded and no relaunch", got)
	}
}

// A pod that holds two or more live claims, every one out of reach, is a case for a person: a
// relaunch into it reaches Envoy for none of them.
func TestTheControllersRelaunchRefusesAPodWhoseEveryClaimIsOutOfReach(t *testing.T) {
	s, r, sup, _ := relaunchRig(t)
	pod := func(c supervise.Claim) supervise.Claim {
		c.Locator = &runtime.Locator{Runtime: runtime.RuntimeSandbox, Claim: c.Token, Incarnation: "pod-a:3",
			Sandbox: &runtime.SandboxLocator{Namespace: "legion", Name: "legion-legion-208", PodUID: "pod-a", Container: "roles", Generation: 3}}
		return c
	}
	worker := pod(waitingQuietly(reachClaim(t, "LEGION-208", "LEGION-209", claim.RoleImplementer, "ses_implementer")))
	tester := pod(waitingQuietly(reachClaim(t, "LEGION-208", "LEGION-209", claim.RoleTester, "ses_tester")))
	if _, err := sup.restore(context.Background(), []supervise.Claim{worker, tester}); err != nil {
		t.Fatal(err)
	}
	m, _ := sup.Machine(worker.Token)
	r.at(21 * time.Minute)
	r.listing(map[string]time.Duration{"ses_other": 21 * time.Minute})
	r.sweep(worker, tester)

	if err := s.relaunch(context.Background(), m); !errors.Is(err, supervise.ErrPodUnreachable) {
		t.Fatalf("relaunch into a pod whose two claims are both out of reach = %v, want ErrPodUnreachable", err)
	}

	// The tester registers again: the pod reaches Envoy for one claim, and the worker's relaunch
	// goes on.
	r.at(22 * time.Minute)
	r.listing(map[string]time.Duration{"ses_tester": 22 * time.Minute})
	r.sweep(r.with(worker), r.with(tester))
	if err := s.relaunch(context.Background(), m); errors.Is(err, supervise.ErrPodUnreachable) {
		t.Fatalf("relaunch with one claim of the pod reachable = %v, want no pod refusal", err)
	}
}

// lockOutRig is a supervisor holding worker, and the reach rig's lock-out notices the lock-out is
// told through, whose worker stream reads the worker's lock-out of generation 4 by 3 as open while
// locked holds.
func lockOutRig(t *testing.T, locked *atomic.Bool) (*supervision, *reachRig, supervise.Claim) {
	t.Helper()
	sup, _ := newPluginSupervisor(t, api.DaemonAPIVersion, true)
	worker := reachClaim(t, "LEGION-208", "LEGION-209", claim.RoleImplementer, "ses_implementer")
	if _, err := sup.restore(context.Background(), []supervise.Claim{worker}); err != nil {
		t.Fatal(err)
	}
	r := newReachRig(t, false)
	r.lockOuts.lockedOut = func(token claim.Token) (stream.LockOut, bool) {
		return stream.LockOut{Claim: token, Newer: 4, Older: 3, Since: reachBoot}, token == worker.Token && locked.Load()
	}
	r.lockOuts.claimOf = supervisedClaimOf(sup)
	return &supervision{log: quietLogger(), supervisor: sup, reach: r.k, lockOuts: r.lockOuts}, r, worker
}

// The worker stream reports a lock-out once, so a notice of it the controller's lane did not queue
// — here, no controller registered — is told again until it is queued, and queued once (R3).
func TestARefusedLockOutNoticeIsToldAgainUntilItIsQueued(t *testing.T) {
	locked := &atomic.Bool{}
	locked.Store(true)
	s, r, worker := lockOutRig(t, locked)
	r.mu.Lock()
	r.refuseTell = true
	r.mu.Unlock()
	s.lockOuts.report(stream.LockOut{Claim: worker.Token, Newer: 4, Older: 3, Since: reachBoot})
	if told := r.notices(record.WorkerLockedOutNotice); len(told) != 0 {
		t.Fatalf("lock-out notices = %+v while the controller's lane refuses them, want none", told)
	}

	r.mu.Lock()
	r.refuseTell = false
	r.mu.Unlock()
	for minute := 1; minute <= 3; minute++ {
		r.at(time.Duration(minute) * time.Minute)
		r.sweep()
	}
	told := r.notices(record.WorkerLockedOutNotice)
	if len(told) != 1 || told[0].notice.Claim != string(worker.Token) || told[0].issue != worker.Issue {
		t.Fatalf("lock-out notices = %+v, want one naming the claim on its issue", told)
	}
}

// A lock-out that ended before its notice was queued is told no more: only a person can end the
// process it names, and it has ended.
func TestALockOutNoticeIsToldNoMoreOnceItsLockOutEnds(t *testing.T) {
	locked := &atomic.Bool{}
	locked.Store(true)
	s, r, worker := lockOutRig(t, locked)
	r.mu.Lock()
	r.refuseTell = true
	r.mu.Unlock()
	s.lockOuts.report(stream.LockOut{Claim: worker.Token, Newer: 4, Older: 3, Since: reachBoot})

	locked.Store(false)
	r.mu.Lock()
	r.refuseTell = false
	r.mu.Unlock()
	for minute := 1; minute <= 3; minute++ {
		r.at(time.Duration(minute) * time.Minute)
		r.sweep()
	}
	if told := r.notices(record.WorkerLockedOutNotice); len(told) != 0 {
		t.Fatalf("lock-out notices = %+v after the lock-out ended, want none", told)
	}
}
