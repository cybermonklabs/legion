package daemon

import (
	"context"
	"errors"
	"fmt"
	goruntime "runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/wait"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// stoppedPodForTakeover leaves the rig's pod as the release of a pod no claim owns takes it: an
// architect an earlier release left `released` and an implementer whose launch budget ran out,
// so every claim is stopped, none records a process and the pod has been quiet past the idle
// period. It answers the architect's machine, which is the claim an operator takes over.
func stoppedPodForTakeover(t *testing.T) (*idleRig, *supervise.Machine) {
	t.Helper()
	rig := newIdleRig(t)
	architect := rig.releasedArchitect(t)
	rig.stoppedImplementer(t, false)
	return rig, architect
}

// suspendAs is a suspension of a claim that is already suspended, posted to its machine as the
// operator's route (`legion claims suspend`) or a daemon rule posts it. A source that outranks
// the one holding the claim takes the suspension over and keeps the claim's session
// (supervise's suspendedAgain). It reports failures with Errorf, since the runtime's hook calls
// it from the teardown's own row.
func suspendAs(t *testing.T, m *supervise.Machine, by supervise.SuspendSource) {
	t.Helper()
	c := m.Claim()
	if err := m.Handle(context.Background(), supervise.RequestSuspend{Claim: c.Token,
		Reason: "suspended after the release was queued", By: by}); err != nil {
		t.Errorf("the %s suspend of %s: %v", by, c.Token, err)
		return
	}
	if got := m.Claim(); got.State != supervise.StateSuspended || got.SuspendedBy != by {
		t.Errorf("after the %s suspend the architect is %s by %s, want suspended by %s", by, got.State, got.SuspendedBy, by)
	}
}

// queueUnownedRelease runs the pass of the second release rule and requires it to have queued
// that release's one row, the teardown, under the release's mark.
func queueUnownedRelease(t *testing.T, rig *idleRig) {
	t.Helper()
	queued := rig.queuedBy(t, func() { rig.w.releaseUnownedPods(context.Background()) })
	if len(queued) != 1 || queued[0] != "issue_release" || !rig.marked(t) {
		t.Fatalf("the pass queued %v (marked %t), want the release's teardown alone under its mark", queued, rig.marked(t))
	}
}

// keptForTheOperator requires what a teardown that found the operator's suspension leaves: the
// pod standing, the release cancelled with its mark gone, no release recorded and one hold under
// the second rule's own name for the operator's suspension, the architect still the operator's
// with its session, and the cancel's own line, which says no role is coming back.
func keptForTheOperator(t *testing.T, rig *idleRig, architect *supervise.Machine, logs *syncBuffer) {
	t.Helper()
	if _, teardowns := rig.rt.reads(); teardowns != 0 {
		t.Fatalf("Sandbox teardowns = %d, want none: the operator's suspension holds the pod", teardowns)
	}
	if rig.marked(t) {
		t.Fatal("the cancelled release left its mark behind")
	}
	records := rig.records(t)
	if len(records.Releases) != 0 {
		t.Fatalf("releases recorded = %+v, want none: the release was cancelled", records.Releases)
	}
	want := workflow.IdleHoldUnownedOperatorSuspended.String()
	if len(records.Holds) != 1 || records.Holds[0].Reason != want || records.Holds[0].Issue != rig.issue.Key {
		t.Fatalf("holds recorded = %+v, want the one %s of %s", records.Holds, want, rig.issue.Key)
	}
	if got := architect.Claim(); got.State != supervise.StateSuspended || got.SuspendedBy != supervise.SuspendedByOperator ||
		got.Session != "ses-architect" {
		t.Fatalf("the architect is %s by %s with session %q, want the operator's suspension keeping ses-architect",
			got.State, got.SuspendedBy, got.Session)
	}
	line := releaseLogLine(t, logs, idleRigUnownedCancel)
	if !strings.Contains(line, "issue="+rig.issue.Key) || !strings.Contains(line, "hold="+want) {
		t.Errorf("the cancelled release's line does not name the issue and the hold that stopped it:\n%s", line)
	}
	saysNoRoleComesBack(t, logs, line, "the cancelled release's line")
}

// The release of a pod no claim owns holds a pod for as long as an operator's suspension holds a
// claim of it (workflow.UnownedPodRelease). The pass reads that suspension when it queues the
// release, and the release is one row, the teardown, which runs later on the issue's lane. In
// between, an operator can suspend a claim of the pod that is already suspended, which takes
// the suspension over. The teardown reads it again, at its own read and under the runtime's pod
// lock, and keeps the pod: the operator's resume then starts the role in the pod that is still
// there, with the workspace as the roles left it.
//
// The controls are the same suspension made before the queue, which the pass itself holds the
// pod for; a daemon's suspension after the queue, which is no operator's and holds nothing; and
// a pause after the queue, which a teardown of this release does not read (the pause holds the
// rows its own decisions name, and the teardown is none of them).
func TestAnOperatorsSuspensionAfterTheUnownedReleaseIsQueuedKeepsThePod(t *testing.T) {
	t.Run("the operator suspends a claim after the queue", func(t *testing.T) {
		rig, architect := stoppedPodForTakeover(t)
		logs, log := watchedLog()
		rig.runner.log = log
		queueUnownedRelease(t, rig)

		suspendAs(t, architect, supervise.SuspendedByOperator)
		rig.drain(t)

		keptForTheOperator(t, rig, architect, logs)
	})

	t.Run("the operator suspends a claim under the runtime's pod lock", func(t *testing.T) {
		rig, architect := stoppedPodForTakeover(t)
		logs, log := watchedLog()
		rig.runner.log = log
		queueUnownedRelease(t, rig)
		// The teardown's own read has answered that the pod may go; the runtime's read under its
		// pod lock is the one the pod is taken on, and the suspension lands between the two.
		rig.rt.beforeAuthorize = func() { suspendAs(t, architect, supervise.SuspendedByOperator) }

		rig.drain(t)

		if refused := rig.rt.refusals(); refused != 1 {
			t.Fatalf("authorizations refused under the pod lock = %d, want the one the teardown asked for", refused)
		}
		keptForTheOperator(t, rig, architect, logs)
	})

	t.Run("control: the operator's suspension before the queue keeps the pod", func(t *testing.T) {
		rig, architect := stoppedPodForTakeover(t)
		suspendAs(t, architect, supervise.SuspendedByOperator)
		// The suspend's write stamps the claim row now, which is a write inside the quiet
		// period; backdated with the rest of the pod, the suspension is what holds it.
		rig.wrote(t, architect.Claim().Token, 30)

		if queued := rig.queuedBy(t, func() { rig.w.releaseUnownedPods(context.Background()) }); len(queued) != 0 {
			t.Fatalf("the pass queued %v, want nothing: the operator's suspension holds the pod", queued)
		}
		rig.drain(t)

		if _, teardowns := rig.rt.reads(); teardowns != 0 {
			t.Fatalf("Sandbox teardowns = %d, want none", teardowns)
		}
		want := workflow.IdleHoldUnownedOperatorSuspended.String()
		if holds := rig.records(t).Holds; len(holds) != 1 || holds[0].Reason != want {
			t.Fatalf("holds recorded = %+v, want the pass's one %s", holds, want)
		}
	})

	t.Run("control: a daemon's suspension after the queue holds nothing", func(t *testing.T) {
		rig, architect := stoppedPodForTakeover(t)
		queueUnownedRelease(t, rig)

		suspendAs(t, architect, supervise.SuspendedByDaemon)
		rig.drain(t)

		if _, teardowns := rig.rt.reads(); teardowns != 1 {
			t.Fatalf("Sandbox teardowns = %d, want the one: a daemon's suspension is one a daemon rule ends, and holds no pod", teardowns)
		}
		if holds := rig.records(t).Holds; len(holds) != 0 {
			t.Fatalf("holds recorded = %+v, want none: the pod was released", holds)
		}
	})

	t.Run("control: a pause after the queue does not hold the teardown", func(t *testing.T) {
		rig, _ := stoppedPodForTakeover(t)
		queueUnownedRelease(t, rig)

		openLegionPause(t, rig.pool, time.Now().UTC())
		rig.drain(t)

		if _, teardowns := rig.rt.reads(); teardowns != 1 {
			t.Fatalf("Sandbox teardowns = %d, want the one: a release of a pod no claim owns has no save, so a pause never cancels it", teardowns)
		}
		if records := rig.records(t); len(records.Releases) != 1 || len(records.Holds) != 0 {
			t.Fatalf("records = %d releases and holds %+v, want the one release and no hold", len(records.Releases), records.Holds)
		}
	})
}

// releaseID is the id of the release the rig's issue has in flight (issue_releases.id).
func (r *idleRig) releaseID(t *testing.T) int64 {
	t.Helper()
	var id int64
	if err := r.pool.QueryRow(context.Background(), `select id from issue_releases where issue = $1`, r.issue.Key).Scan(&id); err != nil {
		t.Fatalf("read the release in flight: %v", err)
	}
	return id
}

// storedSuspension is the state and the suspension's source the store holds for m's claim.
func (r *idleRig) storedSuspension(t *testing.T, m *supervise.Machine) (state, by string) {
	t.Helper()
	if err := r.pool.QueryRow(context.Background(), `select state, suspended_by from claims where token = $1`,
		string(m.Claim().Token)).Scan(&state, &by); err != nil {
		t.Fatalf("read the stored claim: %v", err)
	}
	return state, by
}

// suspendAsOperator posts the operator's suspend (`legion claims suspend`) to m's claim and answers
// what the machine answered, which the route reads: a *supervise.RefusedError is its 409.
func suspendAsOperator(m *supervise.Machine) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	return m.Handle(ctx, supervise.RequestSuspend{Claim: m.Claim().Token, Reason: "the operator suspended it",
		By: supervise.SuspendedByOperator})
}

// refusedWhileTheReleaseTakesThePod requires err to be the operator's answer while release takes
// the rig's pod: a refusal (409 on the operator's route) whose sentence names the release and
// the pod's Sandbox, so the operator learns the pod is going.
func refusedWhileTheReleaseTakesThePod(t *testing.T, err error, rig *idleRig, release int64) {
	t.Helper()
	var refused *supervise.RefusedError
	if !errors.As(err, &refused) {
		t.Fatalf("the operator's suspend after the teardown's last read answered %v, want a refusal: the pod it would hold is being deleted", err)
	}
	if named := fmt.Sprintf("release %d of %s", release, rig.issue.Key); !strings.Contains(err.Error(), named) ||
		!strings.Contains(err.Error(), rig.rt.sandboxName) {
		t.Fatalf("the refusal %q does not name %q and the Sandbox %s", err, named, rig.rt.sandboxName)
	}
}

// The runtime takes the pod on the teardown's last read, the one it asks under its pod lock,
// and reads the store no more: it sets the Sandbox suspended, waits the pod out and deletes it.
// An operator's suspension that comes in that window cannot keep the pod, so the daemon refuses
// it, naming the release and the pod, and leaves the claim as it was: no operator is told a pod
// is held that is going. Once the release has ended, the same suspension is taken. Both release
// rules take the pod on that same read, so both refuse it.
//
// The control is a daemon's suspension in the same window, which holds no pod and is taken.
func TestAnOperatorsSuspensionAfterTheTeardownsLastReadIsRefused(t *testing.T) {
	t.Run("the release of a pod no claim owns refuses it and takes the pod", func(t *testing.T) {
		rig, architect := stoppedPodForTakeover(t)
		queueUnownedRelease(t, rig)
		release := rig.releaseID(t)
		hooked := false
		var suspended error
		rig.rt.afterAuthorize = func() { hooked, suspended = true, suspendAsOperator(architect) }

		rig.drain(t)

		if !hooked {
			t.Fatal("the runtime's last read never answered that the pod goes, so the window was not reached")
		}
		refusedWhileTheReleaseTakesThePod(t, suspended, rig, release)
		if got := architect.Claim(); !got.Released() || got.Session != "ses-architect" {
			t.Fatalf("the architect is %s by %s with session %q, want it left released with ses-architect", got.State, got.SuspendedBy, got.Session)
		}
		if state, by := rig.storedSuspension(t, architect); state != "suspended" || by != "released" {
			t.Fatalf("the stored architect is %s by %s, want suspended by released: a refused suspension writes nothing", state, by)
		}
		if _, teardowns := rig.rt.reads(); teardowns != 1 {
			t.Fatalf("Sandbox teardowns = %d, want the one the last read answered", teardowns)
		}
		if records := rig.records(t); len(records.Releases) != 1 || len(records.Holds) != 0 {
			t.Fatalf("records = %d releases and holds %+v, want the one release and no hold", len(records.Releases), records.Holds)
		}
		if rig.marked(t) {
			t.Fatal("the finished release left its mark behind")
		}
		// The release has ended, and the same suspension is taken now: it holds the claim, whose
		// next resume builds the issue's pod again.
		if err := suspendAsOperator(architect); err != nil {
			t.Fatalf("the operator's suspend once the release ended = %v, want it taken", err)
		}
		if state, by := rig.storedSuspension(t, architect); state != "suspended" || by != "operator" {
			t.Fatalf("the stored architect is %s by %s, want suspended by operator", state, by)
		}
	})

	t.Run("an idle release refuses it for a claim it suspended and takes the pod", func(t *testing.T) {
		rig := newIdleRig(t)
		queued := rig.queuedBy(t, func() { rig.w.releaseIdlePods(context.Background()) })
		if want := []string{"issue_bundle", "supervise:release", "issue_release"}; !slices.Equal(queued, want) {
			t.Fatalf("queued rows = %v, want the save, then the role stop, then the teardown", queued)
		}
		release := rig.releaseID(t)
		hooked := false
		var suspended error
		rig.rt.afterAuthorize = func() { hooked, suspended = true, suspendAsOperator(rig.machine) }

		rig.drain(t)

		if !hooked {
			t.Fatal("the runtime's last read never answered that the pod goes, so the window was not reached")
		}
		refusedWhileTheReleaseTakesThePod(t, suspended, rig, release)
		if got := rig.machine.Claim(); !got.Released() {
			t.Fatalf("the implementer is %s by %s, want it left released", got.State, got.SuspendedBy)
		}
		if state, by := rig.storedSuspension(t, rig.machine); state != "suspended" || by != "released" {
			t.Fatalf("the stored implementer is %s by %s, want suspended by released", state, by)
		}
		if _, teardowns := rig.rt.reads(); teardowns != 1 {
			t.Fatalf("Sandbox teardowns = %d, want the one the last read answered", teardowns)
		}
		if records := rig.records(t); len(records.Releases) != 1 || len(records.Holds) != 0 {
			t.Fatalf("records = %d releases and holds %+v, want the one release and no hold", len(records.Releases), records.Holds)
		}
		if err := suspendAsOperator(rig.machine); err != nil {
			t.Fatalf("the operator's suspend once the release ended = %v, want it taken", err)
		}
		if got := rig.machine.Claim(); got.SuspendedBy != supervise.SuspendedByOperator {
			t.Fatalf("the implementer is suspended by %s, want the operator", got.SuspendedBy)
		}
	})

	t.Run("control: a daemon's suspension in the same window is taken, and the pod goes", func(t *testing.T) {
		rig, architect := stoppedPodForTakeover(t)
		queueUnownedRelease(t, rig)
		hooked := false
		var suspended error
		rig.rt.afterAuthorize = func() {
			hooked = true
			suspended = architect.Handle(context.Background(), supervise.RequestSuspend{Claim: architect.Claim().Token,
				Reason: "a daemon rule suspended it", By: supervise.SuspendedByDaemon})
		}

		rig.drain(t)

		if !hooked || suspended != nil {
			t.Fatalf("hook ran %t, the daemon's suspend = %v; want it run and taken: a daemon's suspension holds no pod", hooked, suspended)
		}
		if state, by := rig.storedSuspension(t, architect); state != "suspended" || by != "daemon" {
			t.Fatalf("the stored architect is %s by %s, want suspended by daemon", state, by)
		}
		if _, teardowns := rig.rt.reads(); teardowns != 1 {
			t.Fatalf("Sandbox teardowns = %d, want the one", teardowns)
		}
	})
}

// A start of a claim of the issue after the teardown's last read would stand live in the pod that
// is going, and an operator's suspension of it would then answer success while the pod is deleted.
// So the launch waits for the release instead, by name (supervise.PodTakenLaunchError, the store's
// launch check), with nothing of the claim changed and nothing started, and the operator's
// suspension that follows meets the release's refusal, as it does for a claim no start touched.
// Both a resume an operator asks for and the resume a wake's rule makes wait. Once the release has
// ended, the same resume launches the claim, in the issue's next pod.
func TestALaunchAfterTheTeardownsLastReadWaitsForTheRelease(t *testing.T) {
	for _, tc := range []struct {
		name   string
		resume func(claim.Token) supervise.Event
	}{
		{"the operator's resume", func(token claim.Token) supervise.Event { return supervise.RequestResume{Claim: token} }},
		{"a wake's rule resume", func(token claim.Token) supervise.Event {
			return supervise.RequestRuleResume{Claim: token, Reason: "a notice for the role"}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rig, architect := stoppedPodForTakeover(t)
			queueUnownedRelease(t, rig)
			release := rig.releaseID(t)
			token := architect.Claim().Token
			hooked, startsInWindow, startsBefore := false, 0, len(rig.rt.CallsOf("Resume"))+len(rig.rt.CallsOf("Spawn"))
			var resumed, suspended error
			var storedState, storedBy string
			rig.rt.afterAuthorize = func() {
				hooked = true
				resumed = architect.Handle(context.Background(), tc.resume(token))
				startsInWindow = len(rig.rt.CallsOf("Resume")) + len(rig.rt.CallsOf("Spawn")) - startsBefore
				storedState, storedBy = rig.storedSuspension(t, architect)
				suspended = suspendAsOperator(architect)
			}

			rig.drain(t)

			if !hooked {
				t.Fatal("the runtime's last read never answered that the pod goes, so the window was not reached")
			}
			var waits *supervise.PodTakenLaunchError
			if !errors.As(resumed, &waits) || !errors.Is(resumed, wait.ErrWaiting) || waits.Release != release ||
				waits.Sandbox != rig.rt.sandboxName {
				t.Fatalf("the resume after the last read = %v, want the launch's wait naming release %d and the Sandbox %s",
					resumed, release, rig.rt.sandboxName)
			}
			if startsInWindow != 0 || storedState != "suspended" || storedBy != "released" {
				t.Fatalf("in the window the runtime was asked %d starts and the stored architect is %s by %s; want none, and it released",
					startsInWindow, storedState, storedBy)
			}
			refusedWhileTheReleaseTakesThePod(t, suspended, rig, release)
			if state, by := rig.storedSuspension(t, architect); state != "suspended" || by != "released" || !architect.Claim().Released() {
				t.Fatalf("the stored architect is %s by %s (in memory %s by %s), want it left released",
					state, by, architect.Claim().State, architect.Claim().SuspendedBy)
			}
			if _, teardowns := rig.rt.reads(); teardowns != 1 || rig.marked(t) {
				t.Fatalf("Sandbox teardowns = %d, marked %t; want the one the last read answered, and the release over", teardowns, rig.marked(t))
			}

			if err := architect.Handle(context.Background(), tc.resume(token)); err != nil {
				t.Fatalf("the resume once the release ended = %v, want it to launch", err)
			}
			if starts := len(rig.rt.CallsOf("Resume")) + len(rig.rt.CallsOf("Spawn")) - startsBefore; starts != 1 || architect.Claim().Released() {
				t.Fatalf("once the release ended the runtime was asked %d starts and the architect is %s; want one launch",
					starts, architect.Claim().State)
			}
		})
	}
}

// takenPod is the Sandbox the rig's release mark records its teardown took, or "" for none.
func (r *idleRig) takenPod(t *testing.T) string {
	t.Helper()
	var taken *string
	err := r.pool.QueryRow(context.Background(), `select taken_pod from issue_releases where issue = $1`, r.issue.Key).Scan(&taken)
	if err != nil || taken == nil {
		return ""
	}
	return *taken
}

// A teardown that fails after its last read and before the runtime touched the pod (a Sandbox
// read the cluster did not answer, runtime.PodUntouchedError) leaves the pod standing with no
// teardown holding it, so the take that read recorded ends in the transaction that records the
// row's failure. An operator's suspension made then is taken, and the row's next run reads it and
// keeps the pod, for both release rules. The control is a teardown that failed after it touched
// the pod: the take stands, the suspension meets the refusal, and the row's next run deletes the
// pod.
func TestATeardownThatFailsBeforeTouchingThePodEndsItsTake(t *testing.T) {
	untouched := &runtime.PodUntouchedError{Err: errors.New("release issue: read Sandbox: the API did not answer")}
	touched := errors.New("release issue: wait the pod out: the API did not answer")
	for _, tc := range []struct {
		name    string
		idle    bool
		failure error
	}{
		{name: "the release of a pod no claim owns", failure: untouched},
		{name: "an idle release", idle: true, failure: untouched},
		{name: "control: a teardown that touched the pod", failure: touched},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var rig *idleRig
			var m *supervise.Machine
			if tc.idle {
				rig = newIdleRig(t)
				queued := rig.queuedBy(t, func() { rig.w.releaseIdlePods(context.Background()) })
				if want := []string{"issue_bundle", "supervise:release", "issue_release"}; !slices.Equal(queued, want) {
					t.Fatalf("queued rows = %v, want the save, then the role stop, then the teardown", queued)
				}
				m = rig.machine
			} else {
				rig, m = stoppedPodForTakeover(t)
				queueUnownedRelease(t, rig)
			}
			release := rig.releaseID(t)
			rig.rt.failAfterAuthorize = tc.failure

			rig.drain(t)

			taken := rig.takenPod(t)
			if _, teardowns := rig.rt.reads(); teardowns != 0 || !rig.marked(t) {
				t.Fatalf("after the failed teardown: teardowns %d, marked %t; want the pod standing and the release in flight", teardowns, rig.marked(t))
			}
			suspended := suspendAsOperator(m)
			rig.rt.failAfterAuthorize = nil
			rig.runner.now = func() time.Time { return time.Now().Add(time.Hour) }
			rig.drain(t)

			_, teardowns := rig.rt.reads()
			state, by := rig.storedSuspension(t, m)
			if tc.failure == touched {
				if taken != rig.rt.sandboxName {
					t.Fatalf("after a teardown that touched the pod the mark records the take %q, want %q", taken, rig.rt.sandboxName)
				}
				refusedWhileTheReleaseTakesThePod(t, suspended, rig, release)
				if teardowns != 1 || by != "released" {
					t.Fatalf("the row's next run: teardowns %d, the claim %s by %s; want the pod deleted and the claim released", teardowns, state, by)
				}
				return
			}
			if taken != "" || suspended != nil {
				t.Fatalf("after a teardown that did not touch the pod the mark records the take %q and the operator's suspension = %v; want no take and the suspension taken",
					taken, suspended)
			}
			if records := rig.records(t); teardowns != 0 || state != "suspended" || by != "operator" || len(records.Releases) != 0 || len(records.Holds) != 1 {
				t.Fatalf("the row's next run: teardowns %d, the claim %s by %s, %d releases and holds %+v; want the pod kept for the operator's suspension",
					teardowns, state, by, len(records.Releases), records.Holds)
			}
		})
	}
}

// A daemon that stops between the teardown's last read and the runtime's first call to the
// cluster leaves no take behind it. A stop the outbox settles — its context ended, so the
// runtime's read of the Sandbox fails before it touches the pod — ends the take with the row's
// failure, which the outbox records on a context of its own. A daemon that dies there records
// nothing: the take stands, and the row's next run, once its lease has lapsed, ends it whichever
// way its own teardown goes: it deletes the pod and the release ends, or it fails before it touches
// the pod and the take is cleared with its failure. While that run's teardown holds the pod, past
// its own last read, the take stands and an operator's suspension is refused.
func TestATeardownStoppedAfterItsLastReadLeavesNoTakeBehind(t *testing.T) {
	t.Run("a stop the outbox settles ends the take with the run", func(t *testing.T) {
		rig, architect := stoppedPodForTakeover(t)
		queueUnownedRelease(t, rig)
		ctx, stop := context.WithCancel(context.Background())
		rig.rt.afterAuthorize = stop
		if err := rig.runner.RunOnce(ctx); err != nil {
			t.Fatalf("the run the daemon's stop cut short: %v", err)
		}
		if taken := rig.takenPod(t); taken != "" || !rig.marked(t) {
			t.Fatalf("after the stop the mark records the take %q (marked %t), want no take and the release in flight", taken, rig.marked(t))
		}
		if err := suspendAsOperator(architect); err != nil {
			t.Fatalf("the operator's suspension after the stop = %v, want it taken: the pod stands and no teardown holds it", err)
		}
	})

	for _, tc := range []struct {
		name    string
		failure error
	}{
		{name: "a daemon that dies there, whose next run deletes the pod"},
		{name: "a daemon that dies there, whose next run fails before it touches the pod",
			failure: &runtime.PodUntouchedError{Err: errors.New("release issue: read Sandbox: the API did not answer")}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rig, architect := stoppedPodForTakeover(t)
			queueUnownedRelease(t, rig)
			release := rig.releaseID(t)
			// The run's goroutine ends inside the runtime, past the last read: nothing after that
			// point of the run is made, its settle included, as a process killed there makes none.
			var died sync.WaitGroup
			died.Add(1)
			rig.rt.afterAuthorize = goruntime.Goexit
			go func() {
				defer died.Done()
				_ = rig.runner.RunOnce(context.Background())
			}()
			died.Wait()
			if taken := rig.takenPod(t); taken != rig.rt.sandboxName || !rig.marked(t) {
				t.Fatalf("after the daemon died the mark records the take %q (marked %t), want %q: nothing ended it", taken, rig.marked(t), rig.rt.sandboxName)
			}

			var suspended error
			rig.rt.afterAuthorize = func() { suspended = suspendAsOperator(architect) }
			rig.rt.failAfterAuthorize = tc.failure
			rig.runner.now = func() time.Time { return time.Now().Add(time.Hour) }
			if err := rig.runner.RunOnce(context.Background()); err != nil {
				t.Fatalf("the row's next run: %v", err)
			}
			refusedWhileTheReleaseTakesThePod(t, suspended, rig, release)
			_, teardowns := rig.rt.reads()
			if tc.failure == nil {
				if teardowns != 1 || rig.marked(t) {
					t.Fatalf("the next run: teardowns %d, marked %t; want the pod deleted and the release, with its take, over", teardowns, rig.marked(t))
				}
				return
			}
			if taken := rig.takenPod(t); teardowns != 0 || taken != "" || !rig.marked(t) {
				t.Fatalf("the next run that failed before touching the pod: teardowns %d, take %q, marked %t; want the pod standing and no take",
					teardowns, taken, rig.marked(t))
			}
			if err := suspendAsOperator(architect); err != nil {
				t.Fatalf("the operator's suspension once the take ended = %v, want it taken", err)
			}
		})
	}
}
