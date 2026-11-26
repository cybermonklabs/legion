package supervise

// The stops of a role whose start waits at its issue pod's gate, the cuts of a start the gate
// granted before its machine ran it, and the stop at the park of the process a launch let go
// (dispatch://INFRA-5297). Each function below is the body of a subtest of
// TestAPodStartsItsRolesOneLoadAtATime (podstart_test.go), which runs it under the subtest's name:
// no stop of a claim waiting at the gate waits for the load it waits for, nor does a read of that
// claim; every cut of a start names its end, in the line of the launch cut short and in the start
// line of the start the gate grants next; and a launch that parks stops the process it let go
// first, so a cut of the park leaves nothing running, and a stop the runtime refuses there leaves
// the launch parked, not failed.

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/testwait"
	"github.com/sjawhar/legion/daemon/internal/wakepace"
)

// promptBound is how much real time a stop of a claim waiting at the gate, and a read of that
// claim through its machine's lock, may take: well under a second. Neither may wait for another
// role's session load, which on the harness's clock, which nothing moves meanwhile, would never
// end.
const promptBound = 500 * time.Millisecond

// promptly is the one receive from ch, which must come within promptBound of real time.
func promptly[T any](t *testing.T, what string, ch <-chan T) T {
	t.Helper()
	return testwait.ReceiveWithin(t, promptBound, what, "it waits on another role's session load", ch)
}

// claimPromptly is r's claim as Machine.Claim reads it, through r's machine's lock, within
// promptBound: no decision of a machine waits for another role's load (askPod).
func (h *harness) claimPromptly(r *podRole) Claim {
	h.t.Helper()
	read := make(chan Claim, 1)
	go func() { read <- r.m.Claim() }()
	return promptly(h.t, string(r.token)+"'s Claim, read through its machine's lock", read)
}

// postPromptly is ev handed to r's machine and answered within promptBound (postAsync).
func (h *harness) postPromptly(r *podRole, ev Event) error {
	h.t.Helper()
	return promptly(h.t, fmt.Sprintf("the answer to %T of %s", ev, r.token), h.postAsync(r.m, ev))
}

// wantNoStartsAfter is the pod with nothing left to start: no start waits at the gate, and the
// clock passing a load's settle and its bound asks the runtime for no process of roles beyond the
// starts it had been asked for by now.
func (h *harness) wantNoStartsAfter(roles ...*podRole) {
	h.t.Helper()
	before := make([]int, len(roles))
	for i, r := range roles {
		before[i] = h.starts(r.token)
	}
	h.clock.Advance(podLoadBound + podLoadSettle)
	for i, r := range roles {
		r.m.Wait()
		if got := h.starts(r.token); got != before[i] {
			h.t.Fatalf("the runtime was asked to start %s %d times, want %d: nothing is left to start", r.token, got, before[i])
		}
	}
	if waiting := h.podWaiting(); len(waiting) > 0 {
		h.t.Fatalf("starts waiting for the pod = %v, want none", waiting)
	}
}

// waitingTrio is the pod a stop of a waiting claim is asked in: first's process loads, and the
// starts of second and third wait at the gate behind it, second's asked first.
func (h *harness) waitingTrio() (first, second, third *podRole) {
	h.t.Helper()
	first, second, third = h.podRole(claim.RoleArchitect, StateSuspended), h.podRole(claim.RoleImplementer, StateSuspended),
		h.podRole(claim.RoleTester, StateSuspended)
	h.mustPostTo(first.m, RequestResume{Claim: first.token})
	h.wantStarts(first.token, 1)
	for _, r := range []*podRole{second, third} {
		h.parks(r, RequestResume{Claim: r.token}, 0)
	}
	return first, second, third
}

// stopsWithoutWaiting is ev, a stop of second while its start waits at the gate behind another
// role's load, waiting for nothing: ev is answered within promptBound of real time, Claim of
// second is read within it before ev and after, and ev leaves second in state, its start out of
// the gate's queue with one line saying its launch was cut short, and its process never asked of
// the runtime.
func (h *harness) stopsWithoutWaiting(second *podRole, ev Event, state ClaimState) {
	h.t.Helper()
	if c := h.claimPromptly(second); c.State != StateLaunching || c.Locator != nil {
		h.t.Fatalf("%s is %s recording %+v while its start waits at the gate, want launching with no process", second.token, c.State, c.Locator)
	}
	if err := h.postPromptly(second, ev); err != nil {
		h.t.Fatalf("%T of %s, whose start waits at the gate: %v", ev, second.token, err)
	}
	if c := h.claimPromptly(second); c.State != state {
		h.t.Fatalf("%s is %s after %T, want %s", second.token, c.State, ev, state)
	}
	h.wantStarts(second.token, 0)
	if waiting := h.podWaiting(); slices.Contains(waiting, second.token) {
		h.t.Fatalf("starts waiting for the pod = %v after %T of %s, want its start out of the queue", waiting, ev, second.token)
	}
	if lines := h.podRecords(podLaunchCutShortLine, second.token); len(lines) != 1 {
		h.t.Fatalf("cut-short lines of %s = %d, want 1", second.token, len(lines))
	}
}

// aSuspensionOfTheWaitingClaim: a suspension of the waiting claim returns at once and cuts its
// start short.
func aSuspensionOfTheWaitingClaim(t *testing.T) {
	h := podHarness(t, "")
	first, second, third := h.waitingTrio()

	h.stopsWithoutWaiting(second, RequestSuspend{Claim: second.token, By: SuspendedByOperator, Reason: "a person stops the role"}, StateSuspended)

	// The start behind it is the next one the gate grants.
	h.loadEnds(first)
	h.wantGrantedStart(third, 1)
	h.wantStartLine(third.token, podIssue, &podWait{claim: first.token, waited: podLoadSettle, endedBy: "load end"})
	h.wantStarts(second.token, 0)
}

// aRetirementOfTheWaitingClaim: a retirement of the waiting claim does not wait either and
// retires it.
func aRetirementOfTheWaitingClaim(t *testing.T) {
	h := podHarness(t, "")
	first, second, third := h.waitingTrio()

	h.stopsWithoutWaiting(second, RequestStop{Claim: second.token}, StateRetired)

	// Nothing waits on the retired claim to launch it again.
	h.clock.Advance(testProbe)
	second.m.Wait()
	h.wantClaimState(second, StateRetired)
	h.wantStarts(second.token, 0)
	h.loadEnds(first)
	h.wantGrantedStart(third, 1)
	h.wantStartLine(third.token, podIssue, &podWait{claim: first.token, waited: testProbe + podLoadSettle, endedBy: "load end"})
	h.wantStarts(second.token, 0)
}

// aReleaseOfThePodDuringAWait: a release of the pod during a wait does not wait.
func aReleaseOfThePodDuringAWait(t *testing.T) {
	h := podHarness(t, "")
	first, second, third := h.waitingTrio()

	// The idle release of the pod suspends every claim of the pod, one role stop at a time:
	// the waiting one, then the loading one.
	release := func(r *podRole) Event {
		return RequestSuspend{Claim: r.token, By: SuspendedByReleased, Reason: "the issue's idle pod is released"}
	}
	h.stopsWithoutWaiting(second, release(second), StateSuspended)

	// The loading claim's agent has not registered, so its release is held until the agent is
	// ready, as a suspension of a launch is: answered at once, without waiting for the load.
	if err := promptly(t, "the loading claim's release", h.postAsync(first.m, release(first))); !errors.Is(err, ErrSuspendHeld) {
		t.Fatalf("the loading claim's release answered %v, want %v", err, ErrSuspendHeld)
	}
	h.wantClaimState(first, StateLaunching)
	if waiting := h.podWaiting(); !slices.Equal(waiting, []claim.Token{third.token}) {
		t.Fatalf("starts waiting for the pod = %v, want the third role's alone", waiting)
	}

	// Its agent comes up and the held release runs: the load ends with the suspension, and
	// the start behind the released one is the next the gate grants.
	h.registered(first)
	h.mustPostTo(first.m, RequestReady{Claim: first.token, Generation: first.m.View().Claim.Generation, Session: first.session})
	h.wantClaimState(first, StateSuspended)
	h.wantLoadEndLine(first.token, podIssue, podLoadWant{endedBy: "suspension", registered: true})
	h.wantGrantedStart(third, 1)
	h.wantStartLine(third.token, podIssue, &podWait{claim: first.token, endedBy: "suspension"})
	for _, r := range []*podRole{first, second} {
		if by := r.m.View().Claim.SuspendedBy; by != SuspendedByReleased {
			t.Fatalf("%s is suspended by %q, want %q", r.token, by, SuspendedByReleased)
		}
	}
	h.wantNoStartsAfter(first, second)
}

// aDeletionOfThePodDuringAWait: a deletion of the pod during a wait does not wait.
func aDeletionOfThePodDuringAWait(t *testing.T) {
	h := podHarness(t, "")
	first, second, third := h.waitingTrio()

	// An issue that closes has its pod deleted, and its close retires every claim of the pod,
	// one row at a time: the waiting one, then the loading one.
	closed := func(r *podRole) Event { return RequestIssueClose{Claim: r.token} }
	h.stopsWithoutWaiting(second, closed(second), StateRetired)

	if err := promptly(t, "the loading claim's close", h.postAsync(first.m, closed(first))); err != nil {
		t.Fatalf("the loading claim's close: %v", err)
	}
	h.wantClaimState(first, StateRetired)
	h.wantLoadEndLine(first.token, podIssue, podLoadWant{endedBy: "retirement"})
	h.wantCalls("Release", 2)

	// The start behind the closed one, whose own close has not come yet, is the next the gate
	// grants.
	h.wantGrantedStart(third, 1)
	h.wantStartLine(third.token, podIssue, &podWait{claim: first.token, endedBy: "retirement"})

	// A close keeps each claim's session whole, for a reopen to resume.
	for _, r := range []*podRole{first, second} {
		if c := r.m.View().Claim; c.Session != r.session || c.SessionFile != r.sessionFile || c.Locator != nil {
			t.Fatalf("%s is %+v after the close, want it retired with its session and resume path kept and no locator", r.token, c)
		}
	}
	h.wantNoStartsAfter(first, second)
}

// aCloseOfTheIssueDuringAWait: the close of the waiting claim's issue during a wait does not
// wait, and the pod's next start goes to the claim behind it once the loading one has loaded.
func aCloseOfTheIssueDuringAWait(t *testing.T) {
	h := podHarness(t, "")
	first, second, third := h.waitingTrio()

	h.stopsWithoutWaiting(second, RequestIssueClose{Claim: second.token}, StateRetired)

	h.loadEnds(first)
	h.wantGrantedStart(third, 1)
	h.wantStartLine(third.token, podIssue, &podWait{claim: first.token, waited: podLoadSettle, endedBy: "load end"})
	h.wantStarts(second.token, 0)
}

// aGrantAfterTheDaemonsStop: a grant that reaches a machine the daemon's stop ended starts
// nothing, and the pod goes to the next start as a failed launch.
func aGrantAfterTheDaemonsStop(t *testing.T) {
	h := podHarness(t, "")
	stopping, stop := context.WithCancel(h.ctx)
	t.Cleanup(stop)
	first := h.podRole(claim.RoleArchitect, StateSuspended)
	second := h.podRoleUnder(stopping, claim.RoleImplementer, StateSuspended)
	third := h.podRole(claim.RoleTester, StateSuspended)
	h.mustPostTo(first.m, RequestResume{Claim: first.token})
	for _, r := range []*podRole{second, third} {
		h.parks(r, RequestResume{Claim: r.token}, 0)
	}

	// The daemon stops the second's machine while its start waits: the start stays in the
	// gate, which a real daemon's stop ends with every machine.
	stop()
	if waiting := h.podWaiting(); !slices.Equal(waiting, []claim.Token{second.token, third.token}) {
		t.Fatalf("starts waiting for the pod = %v after the second's machine stopped, want the second's and the third's", waiting)
	}

	// The first role's load ends and the gate grants the second's start. Its decision runs on
	// the machine's ended context, so it starts nothing and gives the pod on as a failed
	// launch, and the third's start is granted.
	h.loadEnds(first)
	h.wantGrantedStart(third, 1)
	h.wantStartLine(third.token, podIssue, &podWait{claim: second.token, waited: podLoadSettle, endedBy: "failed launch"})
	h.wantStarts(second.token, 0)
	// The second's decision writes its cut-short line after it gives the pod on, so the third can
	// start before the line is written.
	h.postsReturned()
	if lines := h.podRecords(podLaunchCutShortLine, second.token); len(lines) != 1 {
		t.Fatalf("cut-short lines of the stopped role = %d, want 1", len(lines))
	}
	h.wantClaimState(second, StateLaunching)
}

// aGrantedStartCutBeforeItRan: a start granted and cut before it ran names what cut it to the
// next start, for every end a cut passes but the daemon's stop's (aGrantAfterTheDaemonsStop) and
// the pause's (aGrantWhileTheProjectIsPaused), as the end of the decision that dropped its wake
// reads it from the claim (endPodPark): a suspension's and a retirement's own, the end of the
// process the launch was to start where its claim failed, and a failed launch where another park
// replaces this one or where a decision drops the wake and names nothing.
func aGrantedStartCutBeforeItRan(t *testing.T) {
	for _, tc := range []struct {
		name  string
		cut   func(h *harness, r *podRole) error
		state ClaimState
		ended string
	}{
		{name: "a suspension", state: StateSuspended, ended: "suspension", cut: func(h *harness, r *podRole) error {
			return h.postPromptly(r, RequestSuspend{Claim: r.token, By: SuspendedByOperator, Reason: "a person stops the role"})
		}},
		{name: "a retirement", state: StateRetired, ended: "retirement", cut: func(h *harness, r *podRole) error {
			return h.postPromptly(r, RequestStop{Claim: r.token})
		}},
		{name: "a close of the issue", state: StateRetired, ended: "retirement", cut: func(h *harness, r *podRole) error {
			return h.postPromptly(r, RequestIssueClose{Claim: r.token})
		}},
		// A turn-outcome report, which every live state takes, of a turn that ran no prompt of
		// the daemon's and that the provider refused with a status that cannot clear, at the
		// generation the claim stands at: the claim fails at once (unpromptedTurnEnded, fail),
		// which lets its process go (letGo), and the start it was to make is that process's end.
		// A run of refused reports fails no claim (dispatch://INFRA-4508), so one such report is
		// the turn-outcome path to a failure.
		{name: "a failure of its claim", state: StateFailed, ended: "process end", cut: func(h *harness, r *podRole) error {
			return h.postPromptly(r, RequestTurnOutcome{Claim: r.token, Generation: r.m.View().Claim.Generation, Sequence: 1,
				Refused: true, Message: "400 the provider refused the turn", HTTPStatus: 400, Unprompted: true})
		}},
		// The wake pacing's park replaces the gate's (waitOnMachine). No event of the
		// table raises a second launch over a claim whose launch waits at the gate, so the case
		// parks that wake where a decision would (insideDecision), on a place of a pacer of its own
		// whose turn is an hour away, behind the one place that pacer's bucket lets out at once;
		// the test never reaches that turn.
		{name: "another park", state: StateLaunching, ended: "failed launch", cut: func(h *harness, r *podRole) error {
			return h.insideDecision(r.m, func() error {
				pacer := NewPacer(wakepace.New(1, 1, func(time.Duration) time.Duration { return time.Hour }), h.clock)
				pacer.Join(wakepace.Second)
				place := pacer.Join(wakepace.Second)
				if _, out := r.m.bindWake(place); out {
					return errors.New("the place behind the bucket's one token was let out at its join")
				}
				return r.m.waitOnMachine(h.ctx, &wake{place: place, joined: h.clock.Now(), kind: WakeLaunch, state: StateLaunching,
					run: func(context.Context) error { return nil }})
			})
		}},
		// A path added later that drops the parked wake and takes nothing out of the gate: the
		// decision's end takes the start out all the same (Machine.unlock, endPodPark), so the
		// pod's start goes on rather than waiting for a grant nothing will run.
		{name: "a decision that drops the wake alone", state: StateLaunching, ended: "failed launch", cut: func(h *harness, r *podRole) error {
			return h.insideDecision(r.m, func() error {
				r.m.wake = nil
				return nil
			})
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := podHarness(t, "")
			first := h.podRole(claim.RoleArchitect, StateSuspended)
			// The second's machine runs the decision its grant posts only once the test lets
			// it, as a machine busy with another decision would: the grant stands, granted and
			// not yet run, when the cut comes (Deps.Worker, fire).
			held := make(chan struct{})
			release := sync.OnceFunc(func() { close(held) })
			t.Cleanup(release)
			h.deps.Worker = func(ctx context.Context) context.Context { <-held; return ctx }
			second := h.podRole(claim.RoleImplementer, StateSuspended)
			h.deps.Worker = nil
			third := h.podRole(claim.RoleTester, StateSuspended)
			h.mustPostTo(first.m, RequestResume{Claim: first.token})
			for _, r := range []*podRole{second, third} {
				h.parks(r, RequestResume{Claim: r.token}, 0)
			}
			// The generation the second's try wrote before it parked, which its cut-short line names.
			parked := second.m.View().Claim.Generation
			h.loadEnds(first)
			if waiting := h.podWaiting(); !slices.Equal(waiting, []claim.Token{third.token}) {
				t.Fatalf("starts waiting for the pod = %v once the second's start was granted, want the third's alone", waiting)
			}

			if err := tc.cut(h, second); err != nil {
				t.Fatalf("%s of the second, whose granted start has not run: %v", tc.name, err)
			}
			h.wantClaimState(second, tc.state)
			h.wantGrantedStart(third, 1)
			h.wantStartLine(third.token, podIssue, &podWait{claim: second.token, waited: podLoadSettle, endedBy: tc.ended})
			lines := h.podRecords(podLaunchCutShortLine, second.token)
			if len(lines) != 1 {
				t.Fatalf("cut-short lines of the second = %d, want 1", len(lines))
			}
			h.wantAttr(lines[0], "by", tc.ended)
			h.wantAttr(lines[0], "generation", fmt.Sprint(parked))

			// The grant's own decision, once it runs, finds its arming gone and starts nothing.
			release()
			testwait.Eventually(t, "the second's grant to reach its machine", func() bool {
				return len(h.podRecords("supervise: dropped a stale event", second.token)) == 1
			})
			second.m.Wait()
			h.wantStarts(second.token, 0)
			h.wantClaimState(second, tc.state)
		})
	}
}

// aRelaunchThatWaitsStopsAtThePark: a relaunch that waits stops the process it let go at the
// park, so a cut of the park leaves nothing running.
func aRelaunchThatWaitsStopsAtThePark(t *testing.T) {
	h := podHarness(t, "")
	first, second := h.podRole(claim.RoleArchitect, StateSuspended), h.podRole(claim.RoleImplementer, StateSuspended)
	h.runs(second)
	replaced := *second.m.View().Claim.Locator
	h.mustPostTo(first.m, RequestResume{Claim: first.token})
	h.wantStarts(first.token, 1)

	// The runtime finds the second role's live process holding an address a process launched
	// now is not handed: its relaunch lets it go and waits at the gate behind the first role's
	// load, and the process is stopped there, before any grant.
	h.parks(second, RuntimeObservation{Observation: runtime.Observation{Locator: replaced, Kind: runtime.StaleAddress, At: h.clock.Now()}}, 1)
	if stops := h.calls("Suspend"); len(stops) != 1 || stops[0].Locator != replaced {
		t.Fatalf("stops = %+v while the relaunch waits at the gate, want the one stop of the process it let go, %+v", stops, replaced)
	}

	// A suspension cuts the park. The one process the runtime started for the second role was
	// stopped at the park, and the relaunch started none, so nothing of the role runs.
	if err := h.postPromptly(second, RequestSuspend{Claim: second.token, By: SuspendedByOperator, Reason: "a person stops the role"}); err != nil {
		t.Fatalf("suspend the second, whose relaunch waits at the gate: %v", err)
	}
	if c := h.claimPromptly(second); c.State != StateSuspended || c.Locator != nil {
		t.Fatalf("the second is %s recording %+v after the suspension, want it suspended with no process", c.State, c.Locator)
	}
	if stops := h.calls("Suspend"); len(stops) != 1 || stops[0].Locator != replaced {
		t.Fatalf("stops = %+v after the suspension, want the stop the park made of the process the relaunch let go, %+v, and no other", stops, replaced)
	}
	h.wantStarts(second.token, 1)
	h.wantNoStartsAfter(second)
}

// aResumeParkedOverARefusedStop: an operator's resume of a role whose launch parks at the gate
// while the runtime refuses the stop of the process that launch let go (askPod, stopPrevious)
// answers no failure, since the launch has not failed: it waits at the gate to start as asked.
// The refusal is recorded for whoever must hear that the process may run still
// (stopUnconfirmed) and logged at Warn, and the grant starts the launch all the same.
func aResumeParkedOverARefusedStop(t *testing.T) {
	h := podHarness(t, "")
	first, second := h.podRole(claim.RoleArchitect, StateSuspended), h.podRole(claim.RoleImplementer, StateSuspended)
	// The second role ran a process and was suspended over it: the claim keeps that process as
	// the one its next launch lets go (letGo, previous).
	h.runs(second)
	ran := *second.m.View().Claim.Locator
	h.mustPostTo(second.m, RequestSuspend{Claim: second.token, By: SuspendedByOperator, Reason: "a person stops the role"})
	h.mustPostTo(first.m, RequestResume{Claim: first.token})
	h.wantStarts(first.token, 1)
	h.rt.FailSuspend(errors.New("the runtime refused the stop"))

	if err := h.postPromptly(second, RequestResume{Claim: second.token}); err != nil {
		t.Fatalf("the operator's resume of the second, whose launch parks over a stop the runtime refused, answered %v, want no failure", err)
	}
	h.wantWaitsForPod(second, 1)
	if stops := h.calls("Suspend"); len(stops) != 2 || stops[1].Locator != ran {
		t.Fatalf("stops = %+v, want the suspension's and then the park's of the process the launch let go, %+v", stops, ran)
	}
	second.m.mu.Lock()
	unconfirmed := second.m.stopUnconfirmed
	second.m.mu.Unlock()
	if !unconfirmed {
		t.Fatal("the stop the runtime refused at the park is not recorded as unconfirmed")
	}
	const refusedLine = "supervise: the runtime did not confirm the stop of the process this launch let go; the launch waits for its issue pod's start all the same"
	if lines := h.podRecords(refusedLine, second.token); len(lines) != 1 || lines[0].level != slog.LevelWarn {
		t.Fatalf("the lines saying the runtime refused the park's stop = %+v, want one at Warn", lines)
	}

	h.rt.FailSuspend(nil)
	h.loadEnds(first)
	h.wantGrantedStart(second, 2)
	h.wantClaimState(second, StateLaunching)
}

// pausedPodWithAWaitingStart is a pod whose first role has started and loads, and whose second
// role's start waits at the gate for that load, in a project that has just paused (pause 7): the
// first role's load ending grants the second's start during the pause. secondWorker, when not nil,
// is the second machine's Deps.Worker, through which each of its timers and its grant run (fire).
func pausedPodWithAWaitingStart(t *testing.T, secondWorker func(context.Context) context.Context) (h *harness, first, second *podRole, flag *pauseFlag, stops *stopTransaction) {
	t.Helper()
	h = podHarness(t, "")
	flag, stops = &pauseFlag{}, &stopTransaction{store: h.store}
	h.deps.Paused, h.deps.PauseStopped = flag.Paused, stops.PauseStopped
	first = h.podRole(claim.RoleArchitect, StateSuspended)
	h.deps.Worker = secondWorker
	second = h.podRole(claim.RoleImplementer, StateSuspended)
	h.deps.Worker = nil
	h.mustPostTo(first.m, RequestResume{Claim: first.token})
	h.wantStarts(first.token, 1)
	h.parks(second, RequestResume{Claim: second.token}, 0)
	flag.set(7)
	return h, first, second, flag, stops
}

// pauseStopPosted is whether r's machine holds the pause gate's stop of its claim, posted and not
// yet run (TimerPause, pauseGate).
func pauseStopPosted(r *podRole) bool {
	r.m.mu.Lock()
	defer r.m.mu.Unlock()
	_, posted := r.m.timers[TimerPause]
	return posted
}

// grantedDuringAPause is a pod whose second role's start waited at the gate from before a project
// pause and was granted while the pause stands, its pause's own stop row not having reached the
// claim yet, as a stop row behind other rows has not: the grant starts no process, the pod's start
// is free, and the pause's gate has posted the claim's stop for its next decision (podStartDue,
// pauseGate; decision 7 of dispatch://INFRA-3759), which the harness's clock fires when it moves.
func grantedDuringAPause(t *testing.T) (h *harness, second *podRole, flag *pauseFlag, stops *stopTransaction) {
	t.Helper()
	h, first, second, flag, stops := pausedPodWithAWaitingStart(t, nil)
	h.loadEnds(first)
	testwait.Eventually(t, "the second's grant, during the pause, to start its process or to post its stop for the pause", func() bool {
		return h.starts(second.token) > 0 || pauseStopPosted(second)
	})
	second.m.Wait()
	if n := h.starts(second.token); n != 0 {
		t.Fatalf("the runtime was asked to start the second %d times, want none: its start was granted while the project was paused", n)
	}
	if holder := h.podHolder(); holder != "" {
		t.Fatalf("the pod's start is held by %s once the pause's gate took the second's launch, want it free", holder)
	}
	if lines := h.podRecords(podLaunchCutShortLine, second.token); len(lines) != 1 {
		t.Fatalf("cut-short lines of the second = %d, want 1", len(lines))
	} else {
		h.wantAttr(lines[0], "by", "failed launch")
	}
	return h, second, flag, stops
}

// aGrantWhileTheProjectIsPaused: a start granted while the project is paused starts nothing, and
// the pause's gate stops its claim for the pause, a launch owed (pauseStopDue).
func aGrantWhileTheProjectIsPaused(t *testing.T) {
	h, second, _, stops := grantedDuringAPause(t)

	h.clock.Advance(0)
	second.m.Wait()
	if c := second.m.View().Claim; c.State != StateSuspended || c.SuspendedBy != SuspendedByReleased || c.Locator != nil {
		t.Fatalf("the second is %s by %q recording %+v, want it stopped for the pause with no process", c.State, c.SuspendedBy, c.Locator)
	}
	h.wantStarts(second.token, 0)
	stops.wantStops(t, stopCall{stop: PauseStop{Pause: 7, LaunchOwed: true}, landed: true})
	if waiting := h.podWaiting(); len(waiting) != 0 {
		t.Fatalf("starts waiting for the pod = %v, want none", waiting)
	}
}

// aGrantWhoseLaunchThePauseLetsOut: a start granted during a pause whose pause ends before the
// gate's stop runs goes back through the pod's gate as a fresh try, not on the grant the pause took
// (letOutHeldLaunch, the parked wake's run with no hold): its process starts as the pod's holder,
// so the next role's start waits for its load.
func aGrantWhoseLaunchThePauseLetsOut(t *testing.T) {
	h, second, flag, stops := grantedDuringAPause(t)
	parked := second.m.View().Claim.Generation

	flag.set(0)
	h.clock.Advance(0)
	h.wantGrantedStart(second, 1)
	if c := second.m.View().Claim; c.State != StateLaunching || c.Locator == nil || c.Generation != parked+1 {
		t.Fatalf("the second is %s at generation %d recording %+v, want it launching a fresh try (generation %d) with its process", c.State,
			c.Generation, c.Locator, parked+1)
	}
	if holder := h.podHolder(); holder != second.token {
		t.Fatalf("the pod's start is held by %q, want the second's load to hold it", holder)
	}
	h.wantStartLine(second.token, podIssue, nil)
	stops.wantStops(t)
	// The launch passed the wake gate before it parked, so its let-out writes no second wake line
	// (wake.ran).
	if lines := h.podRecords(WakeRanMessage, second.token); len(lines) != 1 {
		t.Fatalf("wake lines of the second = %d, want the one its resume wrote at the wake gate", len(lines))
	}
}
