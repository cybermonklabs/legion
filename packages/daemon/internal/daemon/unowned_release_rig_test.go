package daemon

import (
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// The issues of this rig, one pod each and one per subtest: each subtest has its own issue's pod
// released and made again, so no subtest reads a pod another one drove. They are launched and
// released one rig boot at a time, because this rig is the whole daemon over nine processes and
// a boot of it per subtest is three of them.
const (
	idleRigOperatorRetry = "operator-retry"
	idleRigFreshSession  = "fresh-session-retry"
	idleRigQueuedStart   = "queued-phase-start"
)

// idleRigGateTooOld is what the daemon logs when the idle release finds the Envoy listener's
// newest report older than its window and releases nothing (workflowRuntime.listenerLive). It is
// the line that says the idle rule returned at its gate in a tick, which is the state every pass
// this test asserts on is made in.
const idleRigGateTooOld = "idle release: the Envoy listener's newest report is too old to release a pod on; a released pod would not be woken by a role or agent message"

// idleRigUnownedTeardown is what the daemon logs once the teardown of a pod no role can use has
// taken that pod (outbox.releaseIssue).
const idleRigUnownedTeardown = "outbox: released the pod of an issue no role can use; the release suspended no role, so no session is waiting for a wake"

// idleRigUnownedFoundNothing is what the daemon logs in its place when the runtime found no
// Sandbox of the issue to release, and the teardown took nothing (outbox.releaseIssue).
const idleRigUnownedFoundNothing = "outbox: the release found no Sandbox of the issue to release and released nothing"

// idleRigSessionForgotten is what the daemon logs when a fresh session drops the session its
// claim recorded, before the start that brings that claim back in a new one (api's
// forgetRecordedSession). The rig's role process derives its session id from its pod and its
// role, so the id a relaunched process registers is the same string either way: this line and
// the claim's own record are what tell a retry that resumed the recorded session from one that
// left it behind.
const idleRigSessionForgotten = "api: a fresh session left the claim's recorded session behind"

// A start made after the release of a pod no claim owns makes that pod again, with everything
// the release took: the Sandbox, the pod and its disk. This is INFRA-1341's second release rule
// over the whole daemon — the rig of TestIdleReleaseAndWake, the Sandbox runtime on a fake
// Kubernetes API that starts a real `legion launcher` per pod, scripted role processes and a
// real Envoy listener (idle_rig_test.go says where each part comes from).
//
// Each subtest takes one issue of the rig and runs the same three acts on it: an operator stops
// its one role, which retires the claim and leaves a pod of 3 CPU and 19 GiB with nobody in it;
// the daemon's own pass releases that pod, changing no claim; and one of the starts that build
// such a pod again is made. The three subtests are the three of them the daemon has: the
// operator's retry, which resumes the claim's recorded session; the same retry with
// `--fresh-session`, which leaves that session behind; and the start the daemon queues for a
// phase worker itself.
//
// Every pass here is made with the listener gate shut (idleWakeRig.keepListenerGateShut), which is
// this rule's own case and not a convenience. The idle release reads that gate before it takes a
// pod, because a pod it releases comes back only for a role or an agent message the listener
// reports; this release suspends no claim and leaves nothing waiting for a message, so it runs
// outside the gate, and the two starts that make its pod again read no listener. So each pass
// asserted on is read twice over: the unowned pod goes, and the rig's other pods — made quiet,
// each holding a live claim ready or idle, each one the idle rule would otherwise take — stand
// with their claims untouched.
func TestAStartAfterTheReleaseOfAnIssueWithNoLiveClaimMakesThePodAgain(t *testing.T) {
	t.Parallel()
	rig := newIdleWakeRig(t, nil, idleRigOperatorRetry, idleRigFreshSession, idleRigQueuedStart)
	// From here no pod of the rig is released for being idle. The gate is shut now and kept
	// shut by a goroutine of the rig's for the rest of the test, because the listener
	// republishes a ready report every five minutes and the claim rows this test back-dates
	// stay back-dated: a report left standing for a tick of the daemon would open the gate on
	// pods that are quiet.
	rig.keepListenerGateShut()

	t.Run("TheOperatorsRetryMakesThePodAgainInTheClaimsOwnSession", func(t *testing.T) {
		issue := rig.issue(idleRigOperatorRetry)
		token := rig.token(issue)
		started, registered := rig.pods.starts(issue.pod), rig.agents.role(issue.key, string(issue.role)).Registered
		session := rig.api.claim(token).Session
		if session == "" {
			t.Fatalf("%s's claim records no session before its role is stopped; there is nothing for the retry to resume", issue.key)
		}
		rig.stopRole(issue)

		releasedWithNoLiveClaim(t, rig, issue)

		rig.retry(issue, false)
		idleRigPodMadeAgain(t, rig, issue, registered+1)

		if made := rig.pods.starts(issue.pod); made != started+1 {
			t.Errorf("the pod of %s has been started %d times, want the %d it had and the one the retry made", issue.key, made, started+1)
		}
		// The claim's own recorded session, the one the stop left on it: a plain retry resumes
		// it, and nothing dropped it. The process that registered reports the same, which is the
		// session the pod is running.
		if got := rig.api.claim(token).Session; got != session {
			t.Errorf("%s's claim records session %q after the retry, want the %q it recorded before its pod was released", issue.key, got, session)
		}
		if got := rig.agents.role(issue.key, string(issue.role)).Session; got != session {
			t.Errorf("%s's role registered session %q, want the claim's own %q", issue.key, got, session)
		}
		if forgotten := rig.logged(idleRigSessionForgotten, map[string]string{"claim": string(token)}); forgotten != 0 {
			t.Errorf("the daemon dropped %s's recorded session %d times, want none: a retry without --fresh-session resumes it", token, forgotten)
		}
	})

	t.Run("TheOperatorsFreshSessionRetryMakesThePodAgainInANewSession", func(t *testing.T) {
		issue := rig.issue(idleRigFreshSession)
		token := rig.token(issue)
		started, registered := rig.pods.starts(issue.pod), rig.agents.role(issue.key, string(issue.role)).Registered
		session := rig.api.claim(token).Session
		if session == "" {
			t.Fatalf("%s's claim records no session before its role is stopped; there is none for --fresh-session to leave behind", issue.key)
		}
		rig.stopRole(issue)

		releasedWithNoLiveClaim(t, rig, issue)

		rig.retry(issue, true)
		idleRigPodMadeAgain(t, rig, issue, registered+1)

		if made := rig.pods.starts(issue.pod); made != started+1 {
			t.Errorf("the pod of %s has been started %d times, want the %d it had and the one the retry made", issue.key, made, started+1)
		}
		// What makes it a new session is the record: the retry dropped the session the claim
		// recorded before it started anything, and the process the start made recorded its own.
		// The id itself cannot say so here — this rig's role process derives it from its pod and
		// its role (idleRigRoleProcess), so a pod of the same claim registers the same string
		// whether it resumed or spawned — and the line the daemon wrote when it dropped the one
		// the claim held is what can.
		if forgotten := rig.logged(idleRigSessionForgotten,
			map[string]string{"claim": string(token), "session": session}); forgotten != 1 {
			t.Errorf("the daemon dropped %s's recorded session %s %d times, want the once --fresh-session drops it",
				token, session, forgotten)
		}
		registration := rig.agents.role(issue.key, string(issue.role)).Session
		if got := rig.api.claim(token).Session; got == "" || got != registration {
			t.Errorf("%s's claim records session %q, want the %q the process the start made registered", issue.key, got, registration)
		}
	})

	t.Run("TheDaemonsOwnPhaseStartMakesThePodAgain", func(t *testing.T) {
		issue := rig.issue(idleRigQueuedStart)
		token := rig.token(issue)
		started, registered := rig.pods.starts(issue.pod), rig.agents.role(issue.key, string(issue.role)).Registered
		rig.stopRole(issue)

		releasedWithNoLiveClaim(t, rig, issue)

		// The other start that makes such a pod again: the row the daemon queues for a phase
		// worker, written through the record store the daemon writes it through. It is queued
		// after the release and not before, because a row waiting for delivery to a role of the
		// issue is a conjunct of the rule itself (workflow.IdleHoldUnownedDelivery), save a start
		// that waits for its pod, which the next pod serves — a release taken under any other
		// start would leave it to find its pod gone — so the start of a pod this rule released is
		// a start made after it, or one that waited for that pod.
		enqueueOutbox(t, rig.pool, rig.records, mustOutboxRow(t, issue.key, record.SuperviseRequest{
			Op: "start", Tree: issue.key, Role: issue.role, Generation: 1, Phase: phase.Implementing,
			Task: "Continue " + issue.key + ". Phase: implementing.",
		}, time.Now().UTC()))

		idleRigPodMadeAgain(t, rig, issue, registered+1)

		if made := rig.pods.starts(issue.pod); made != started+1 {
			t.Errorf("the pod of %s has been started %d times, want the %d it had and the one the start made", issue.key, made, started+1)
		}
		if got := rig.api.claim(token).Session; got == "" {
			t.Errorf("%s's claim records no session after the start, want the one its role registered", issue.key)
		}
	})

	rig.kube.requireServed(t)
}

// releasedWithNoLiveClaim is the pass each subtest asserts on: the listener gate is shut for the
// whole test (idleWakeRig.keepListenerGateShut), the daemon's own tick releases the pod of the
// issue whose claim is stopped, and the rig's other pods — each one the idle rule would take
// but for that gate — stand as they were.
//
// The order is the whole of what it reads. The other pods are waited for as idle-release
// candidates first, because a pod the idle rule would keep for a reason of its own says nothing
// about the gate; the count of the line the daemon logs at the gate is taken after that wait,
// so the lines read after the release belong to the ticks that made it. The teardown's own line
// is written only after the runtime has deleted the pod, its Sandbox and its disk (the outbox's
// ReleaseIssue, then its line), which is what the release wait reads, so the line is waited for
// rather than read once: read at once, a release that has deleted everything and not yet written
// its line reads as no release at all, which a loaded run of the whole package met.
func releasedWithNoLiveClaim(t *testing.T, rig *idleWakeRig, issue idleRigIssue) {
	t.Helper()
	others := idleRigOtherIssues(rig, issue)
	idleRigAwaitIdleCandidates(t, rig, others)
	gated := rig.logged(idleRigGateTooOld, nil)
	teardowns := rig.logged(idleRigUnownedTeardown, map[string]string{"issue": issue.key})

	rig.awaitUnownedReleased(issue)
	idleRigEventually(t, idleRigRecordWait, "the daemon's line of the teardown of "+issue.key+"'s pod", func() bool {
		return rig.logged(idleRigUnownedTeardown, map[string]string{"issue": issue.key}) > teardowns
	})

	if now := rig.logged(idleRigUnownedTeardown, map[string]string{"issue": issue.key}); now != teardowns+1 {
		t.Errorf("the daemon logged the teardown of %s's pod %d times, want the %d it had and this release's one",
			issue.key, now, teardowns+1)
	}
	if now := rig.logged(idleRigGateTooOld, nil); now <= gated {
		t.Errorf("the daemon read the listener gate %d times, want more than the %d it had read when the gate was shut: the idle release must have returned at it in the ticks that took the unowned pod",
			now, gated)
	}
	idleRigIdleCandidatesStand(t, rig, others)
}

// idleRigOtherIssues is every issue of the rig but one.
func idleRigOtherIssues(rig *idleWakeRig, except idleRigIssue) []idleRigIssue {
	var others []idleRigIssue
	for _, issue := range rig.issues {
		if issue.key != except.key {
			others = append(others, issue)
		}
	}
	return others
}

// idleRigAwaitIdleCandidates waits until each of these pods is one the idle release would take:
// a live claim ready or idle (workflow's idleStateReleasable), standing in its pod and its
// Sandbox. It is a wait and not a reading because a role the start of an earlier subtest
// brought back is in the turn of the task that start gave it for a moment, and a pod in a turn
// is one the idle rule keeps for a reason of its own — which would say nothing about the gate.
// The pods are quiet by then, since every wait of this test back-dates the project's claim rows
// (idleWakeRig.quiet), so the one thing left between them and a release is the listener gate.
func idleRigAwaitIdleCandidates(t *testing.T, rig *idleWakeRig, issues []idleRigIssue) {
	t.Helper()
	stood := "never read"
	defer func() {
		if t.Failed() {
			t.Logf("the rig's other pods, as the wait last read them: %s", stood)
		}
	}()
	idleRigEventually(t, idleRigRecordWait, "the rig's other pods standing as pods the idle release would take", func() bool {
		claims := rig.api.claims()
		for _, issue := range issues {
			state := listedClaim(t, claims, rig.token(issue)).State
			pod, sandbox := rig.kube.pod(issue.pod), rig.kube.sandbox(issue.pod) != nil
			if !idleRigReleasable(state) || !pod || !sandbox {
				stood = fmt.Sprintf("%s is %s with pod %t and sandbox %t", issue.key, state, pod, sandbox)
				return false
			}
		}
		return true
	})
}

// idleRigIdleCandidatesStand reads that each of those pods stands as the wait above left it: the
// pass released the pod no role can use and left every pod a live claim holds alone, which is
// the shut gate holding the idle release off.
func idleRigIdleCandidatesStand(t *testing.T, rig *idleWakeRig, issues []idleRigIssue) {
	t.Helper()
	claims := rig.api.claims()
	for _, issue := range issues {
		state := listedClaim(t, claims, rig.token(issue)).State
		pod, sandbox := rig.kube.pod(issue.pod), rig.kube.sandbox(issue.pod) != nil
		if !idleRigReleasable(state) || !pod || !sandbox {
			t.Errorf("after the release %s is %s with pod %t and sandbox %t, want the claim ready or idle in the pod it stood in: the gate the idle release reads is shut",
				issue.key, state, pod, sandbox)
		}
	}
}

// idleRigReleasable is whether a claim's state is one the idle release may suspend: an agent
// waiting for work or one between turns (workflow's idleStateReleasable). Every other live
// state is a process in a turn or a launch, which that rule keeps for a reason of its own.
func idleRigReleasable(state string) bool {
	return state == string(supervise.StateReady) || state == string(supervise.StateIdle)
}

// idleRigPodMadeAgain waits for the start to have made the issue's pod again: the same Sandbox,
// the same pod and a disk, with the role registered for the registrations-th time and its claim
// live. The pod's name is the claim's (sandbox.SandboxName), so a pod made again is the pod
// whose name the release deleted.
//
// The listener gate stays shut while this waits, as it does for the whole test: the pods this
// test made quiet stay quiet while a pod comes up, and a listener report left standing for a
// tick would open the gate on them (idleWakeRig.keepListenerGateShut).
func idleRigPodMadeAgain(t *testing.T, rig *idleWakeRig, issue idleRigIssue, registrations int) {
	t.Helper()
	token := rig.token(issue)
	defer func() {
		if t.Failed() {
			t.Log(rig.diagnose())
		}
	}()
	idleRigEventually(t, idleRigPodWait, "the pod of "+issue.key+" made again with its role registered", func() bool {
		return rig.agents.role(issue.key, string(issue.role)).Registered >= registrations &&
			idleRigWorking(rig.api.claim(token).State)
	})
	if !rig.kube.pod(issue.pod) || rig.kube.sandbox(issue.pod) == nil || !rig.kube.disk(issue.pod) {
		t.Fatalf("%s's role is registered again with pod %t, sandbox %t, disk %t; want the Sandbox, the pod and the disk the release took all made again",
			issue.key, rig.kube.pod(issue.pod), rig.kube.sandbox(issue.pod) != nil, rig.kube.disk(issue.pod))
	}
}

// stopRole stops the issue's role as an operator does — POST
// /legion/v1/operator/claims/{token}/stop, which `legion claims stop` posts — and waits until
// the claim reads retired holding no process. That leaves the pod this rule is for: its one
// claim is in one of the three states no role can take a turn from, and nothing of the daemon's
// starts such a worker again (TestAnOperatorsStopOfASlottedWorkerHolds). The pod, its disk and
// its Sandbox stand, with 3 CPU and 19 GiB reserved and nobody in them, which is what the
// release takes.
func (r *idleWakeRig) stopRole(issue idleRigIssue) {
	r.t.Helper()
	token := r.token(issue)
	status, body := r.api.request(http.MethodPost, "/legion/v1/operator/claims/"+string(token)+"/stop", nil, true)
	if status != http.StatusOK && status != http.StatusAccepted {
		r.t.Fatalf("the operator's stop of %s = %d %s", token, status, body)
	}
	idleRigEventually(r.t, idleRigRecordWait, "the stopped role of "+issue.key+" recording no process", func() bool {
		stopped := r.api.claim(token)
		return stopped.State == string(supervise.StateRetired) && stopped.Locator == nil
	})
	if !r.kube.pod(issue.pod) || r.kube.sandbox(issue.pod) == nil {
		r.t.Fatalf("the pod of %s reads pod %t and sandbox %t after its role was stopped, want the pod a stop leaves standing with nobody in it",
			issue.key, r.kube.pod(issue.pod), r.kube.sandbox(issue.pod) != nil)
	}
}
