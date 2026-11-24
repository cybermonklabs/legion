package daemon

// The round decision reads the kind of each reason a pod's drift gives (runtime.DriftKind), never
// its phrase: a pod whose drift is only of kinds that drain keeps working on its old image and
// settings however long, and only a kind that breaks opens an update round (dispatch://INFRA-4754
// R1, R3, R4). A round still open at a boot over drift that only drains ends with no replacement.

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/runtime/fake"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// kindsThat is every declared kind that drains, or every one that breaks, in declared order.
func kindsThat(drain bool) []runtime.DriftKind {
	var kinds []runtime.DriftKind
	for _, d := range runtime.DriftDeclarations() {
		if d.Drains == drain {
			kinds = append(kinds, d.Kind)
		}
	}
	return kinds
}

// driftOf is one reason of kind, with a phrase that names it.
func driftOf(kind runtime.DriftKind) runtime.DriftReason {
	return runtime.DriftReason{Kind: kind, Text: string(kind) + " held, now handed"}
}

// sweepsSettled is n sweeps a minute apart, each followed by every machine deciding what the sweep
// posted to it, so a round a sweep opened is recorded on the claims before they are read.
func (r *podUpdateRig) sweepsSettled(n int) {
	r.t.Helper()
	for i := range n {
		if i > 0 {
			r.clock.advance(time.Minute)
		}
		r.tick()
		for role := range r.tokens {
			r.awaitPosted(role)
		}
	}
}

// wantNoRound holds every claim of the rig to no round, no ask queued and no pod replaced.
func (r *podUpdateRig) wantNoRound(what string) {
	r.t.Helper()
	for role := range r.tokens {
		if c := r.claimOf(role); c.UpdateRound != "" {
			r.t.Errorf("%s: the %s records round %q for %q, want no round", what, role, c.UpdateRound, c.UpdateReasons)
		}
	}
	if asks := r.asks(); len(asks) > 0 {
		r.t.Errorf("%s: the sweep queued %d update asks, want none: %+v", what, len(asks), asks)
	}
	if replaced := r.replacements(); len(replaced) > 0 {
		r.t.Errorf("%s: the pod was replaced %d times, want never", what, len(replaced))
	}
}

// A pod whose drift is of one kind that drains opens no round, whichever kind it is: the kinds
// that drain are iterated as data, so a decision that opens a round for any one of them fails here
// for that kind.
func TestADrainingDriftOpensNoRound(t *testing.T) {
	t.Parallel()
	for _, kind := range kindsThat(true) {
		t.Run(string(kind), func(t *testing.T) {
			t.Parallel()
			rig := newPodUpdateRig(t, readyWorker(claim.RoleImplementer), readyWorker(claim.RoleTester))
			rig.driftsBy(driftOf(kind))
			rig.sweepsSettled(3)
			rig.wantNoRound("three sweeps over a pod drifted on " + string(kind))
		})
	}
}

// A pod whose drift is of one kind that breaks opens a round at the first sweep, whichever kind it
// is, the floor included, and the round records that kind's phrase as its reasons, starting with
// the marker runtime.BreakingDrift: the kinds that break are iterated as data, so a decision that
// skips any one of them, or records its reason with no marker, fails here for that kind.
func TestABreakingDriftOpensARoundAtOnce(t *testing.T) {
	t.Parallel()
	for _, kind := range kindsThat(false) {
		t.Run(string(kind), func(t *testing.T) {
			t.Parallel()
			rig := newPodUpdateRig(t, readyWorker(claim.RoleImplementer), readyWorker(claim.RoleTester))
			reason := driftOf(kind)
			rig.driftsBy(reason)
			rig.tick()
			want := runtime.BreakingDrift + reason.Text
			for _, role := range []claim.Role{claim.RoleImplementer, claim.RoleTester} {
				if c := rig.awaitRound(role, podUpdateRound); c.UpdateReasons != want {
					t.Errorf("the %s's round records reasons %q, want %q", role, c.UpdateReasons, want)
				}
			}
		})
	}
}

// A pod drifted on its image, which drains, and on its account, which breaks, opens a round whose
// reasons hold both phrases, the account's starting with the marker and the image's carrying none,
// and the line that opens it names both kinds.
func TestAMixedDriftOpensARoundWithEveryReason(t *testing.T) {
	t.Parallel()
	rig := newPodUpdateRig(t, readyWorker(claim.RoleImplementer))
	image, account := driftOf(runtime.DriftImage), driftOf(runtime.DriftAccount)
	rig.driftsBy(image, account)
	rig.tick()
	want := image.Text + ", " + runtime.BreakingDrift + account.Text
	c := rig.awaitRound(claim.RoleImplementer, podUpdateRound)
	if c.UpdateReasons != want {
		t.Errorf("the round records reasons %q, want both, the breaking one marked, %q", c.UpdateReasons, want)
	}
	if !strings.Contains(c.UpdateReasons, runtime.BreakingDrift+account.Text) {
		t.Errorf("the round's reasons %q do not start the account's reason with the marker %q", c.UpdateReasons, runtime.BreakingDrift)
	}
	if strings.Contains(c.UpdateReasons, runtime.BreakingDrift+image.Text) || strings.Count(c.UpdateReasons, runtime.BreakingDrift) != 1 {
		t.Errorf("the round's reasons %q carry the marker other than once, on the account's reason: the image drains", c.UpdateReasons)
	}
	opened := loggedMessages(t, rig.logs.String(), roundOpensLine)
	if len(opened) != 1 {
		t.Fatalf("the sweep wrote %d lines that open a round, want one:\n%s", len(opened), rig.logs.String())
	}
	kinds := fmt.Sprint(opened[0]["kinds"])
	for _, kind := range []runtime.DriftKind{runtime.DriftImage, runtime.DriftAccount} {
		if !strings.Contains(kinds, string(kind)) {
			t.Errorf("the line that opens the round names kinds %s, want %s among them", kinds, kind)
		}
	}
}

// A round an older daemon opened with its reasons as text, still open at this daemon's boot, ends
// with no replacement at the first sweep that reads its pod's drift when that drift only drains:
// each claim it asked gets the task the round held back, and the pod keeps working. A round whose
// claims record no pod ends the same way, its drift read by the issue. A round over a pod whose
// drift holds a kind that breaks goes on.
func TestARoundOpenAtBootOverDrainingDriftEnds(t *testing.T) {
	t.Parallel()
	const olderReasons = "image ghcr.io/cybermonklabs/legion-worker@sha256:old, now ghcr.io/cybermonklabs/legion-worker@sha256:new"
	inRound := func(role podUpdateRole) podUpdateRole {
		role.round, role.reasons = podUpdateRound, olderReasons
		return role
	}
	held := func(rig *podUpdateRig, role claim.Role) string {
		task := "work the phase, " + string(role)
		rig.handle(role, supervise.RequestDeliver{Claim: rig.tokens[role], Task: task, ID: "outbox:held-" + string(role)})
		if prompts := rig.workers[role].Prompts(); len(prompts) > 0 {
			t.Fatalf("the %s was prompted %+v while its round was open, want its task held", role, prompts)
		}
		return task
	}
	wantTaskSent := func(rig *podUpdateRig, role claim.Role, task string) {
		t.Helper()
		rig.awaitClaim(role, "the round to end with its held task sent", func(c supervise.Claim) bool {
			return c.UpdateRound == "" && slices.ContainsFunc(rig.workers[role].Prompts(), func(p fake.Prompt) bool { return p.Message == task })
		})
	}

	t.Run("by claim", func(t *testing.T) {
		t.Parallel()
		rig := newPodUpdateRig(t, inRound(readyWorker(claim.RoleImplementer)), inRound(readyWorker(claim.RoleTester)))
		tasks := map[claim.Role]string{}
		for _, role := range []claim.Role{claim.RoleImplementer, claim.RoleTester} {
			tasks[role] = held(rig, role)
		}
		rig.driftsBy(driftOf(runtime.DriftImage))
		rig.tick()
		for role, task := range tasks {
			wantTaskSent(rig, role, task)
		}
		rig.sweepsSettled(2)
		rig.wantNoRound("the sweeps after the round open at the boot ended")
		if ended := loggedMessages(t, rig.logs.String(), roundEndsOverDrainingDriftLine); len(ended) != 1 {
			t.Errorf("the sweep wrote %d lines that end the round over drift that drains, want one", len(ended))
		}
	})

	t.Run("by issue", func(t *testing.T) {
		t.Parallel()
		rig := newPodUpdateRig(t, inRound(suspendedRole(claim.RoleImplementer, supervise.SuspendedByDaemon)))
		rig.rt.ScriptIssuePod(podUpdateIssue, podUpdatePod)
		rig.rt.ScriptIssuePodDrift(podUpdateIssue, driftOf(runtime.DriftImage))
		rig.tick()
		rig.awaitClaim(claim.RoleImplementer, "the podless round to end", func(c supervise.Claim) bool { return c.UpdateRound == "" })
		rig.sweepsSettled(2)
		if replaced := rig.issueReplacements(); len(replaced) > 0 {
			t.Errorf("the pod was replaced by issue %d times, want never: its drift only drains", len(replaced))
		}
		rig.wantNoRound("the sweeps after the podless round open at the boot ended")
	})

	t.Run("a kind that breaks", func(t *testing.T) {
		t.Parallel()
		rig := newPodUpdateRig(t, inRound(readyWorker(claim.RoleImplementer)))
		image, account := driftOf(runtime.DriftImage), driftOf(runtime.DriftAccount)
		rig.driftsBy(image, account)
		rig.sweepsSettled(2)
		// The round goes on, and its reasons are written again from the first reading since the
		// boot, the marker on the reason of the kind that breaks.
		want := image.Text + ", " + runtime.BreakingDrift + account.Text
		if c := rig.claimOf(claim.RoleImplementer); c.UpdateRound != podUpdateRound || c.UpdateReasons != want {
			t.Errorf("the implementer records round %q for %q, want the open round %q kept, its reasons written again as %q",
				c.UpdateRound, c.UpdateReasons, podUpdateRound, want)
		}
		if ended := loggedMessages(t, rig.logs.String(), roundEndsOverDrainingDriftLine); len(ended) > 0 {
			t.Errorf("the sweep ended the round over a kind that breaks: %v", ended)
		}
	})
}

// An Oh My Pi claim on another plugin contract, in a pod whose only drift is its image and with no
// round over it, is relaunched onto the daemon's plugins by the stale-plugin lane at the end of its
// turn (it is ready), and no round opens over the pod through the sweeps of the case.
func TestTheStalePluginLaneRelaunchesAClaimInAPodOnAnOlderImage(t *testing.T) {
	t.Parallel()
	rig := newPodUpdateRig(t,
		podUpdateRole{role: claim.RoleImplementer, state: supervise.StateReady, session: true, inPod: true, contract: api.DaemonAPIVersion - 1},
		readyWorker(claim.RoleTester),
	)
	rig.driftsBy(driftOf(runtime.DriftImage))
	rig.sweepsSettled(2)
	rig.wantNoRound("the sweeps before the lane")
	claims, err := rig.sup.Claims(rig.ctx)
	if err != nil {
		t.Fatalf("read the claims for the lane: %v", err)
	}
	rig.s.refreshStalePlugins(claims, "sweep")
	rig.awaitClaim(claim.RoleImplementer, "the stale claim to be relaunched onto the daemon's plugins", func(c supervise.Claim) bool {
		return c.State == supervise.StateLaunching || c.PluginContract == api.DaemonAPIVersion
	})
	if resumes := rig.launchesOf(claim.RoleImplementer); len(resumes) == 0 {
		t.Errorf("the lane started no process for the stale implementer")
	}
	if launches := rig.launchesOf(claim.RoleTester); len(launches) > 0 {
		t.Errorf("the lane relaunched the tester, which speaks this daemon's contract: %+v", launches)
	}
	rig.sweepsSettled(2)
	for role := range rig.tokens {
		if c := rig.claimOf(role); c.UpdateRound != "" {
			t.Errorf("the %s records round %q after the lane, want no round over a pod whose drift only drains", role, c.UpdateRound)
		}
	}
	if replaced := rig.replacements(); len(replaced) > 0 {
		t.Errorf("the pod was replaced %d times, want never", len(replaced))
	}
}

// A Claude Code claim on another contract than the daemon keeps its pod and its plugin: no install
// can move it, the lane posts it nothing, and no round opens over its pod through the sweeps of the
// case, though the pod is also on an older image. The idle release takes such a pod in the end.
func TestAClaudeCodeClaimOnAnotherContractKeepsItsPod(t *testing.T) {
	t.Parallel()
	rig := newPodUpdateRig(t,
		podUpdateRole{role: claim.RoleReviewer, state: supervise.StateReady, session: true, inPod: true, contract: api.DaemonAPIVersion - 1},
	)
	rig.s.cfg.WorkerBackends = map[claim.Role]runtime.Backend{claim.RoleReviewer: runtime.BackendClaude}
	rig.driftsBy(driftOf(runtime.DriftImage))
	rig.sweepsSettled(3)
	claims, err := rig.sup.Claims(rig.ctx)
	if err != nil {
		t.Fatalf("read the claims for the lane: %v", err)
	}
	rig.s.refreshStalePlugins(claims, "sweep")
	rig.sweepsSettled(2)
	rig.wantNoRound("the sweeps over a pod with a Claude Code claim on another contract")
	if c := rig.claimOf(claim.RoleReviewer); c.State != supervise.StateReady {
		t.Errorf("the Claude Code reviewer is %s, want it left running in its pod", c.State)
	}
	if launches := rig.launchesOf(claim.RoleReviewer); len(launches) > 0 {
		t.Errorf("the lane relaunched the Claude Code reviewer, which no install can move: %+v", launches)
	}
}

// Pods drifted on their image alone open no round however old they grow: at a sweep an hour, 25
// hours and 30 days after the pods were launched.
func TestAnOldPodWithDrainingDriftOpensNoRoundAtAnyAge(t *testing.T) {
	t.Parallel()
	rig := newPodUpdateRig(t, readyWorker(claim.RoleImplementer), readyWorker(claim.RoleTester))
	rig.driftsBy(driftOf(runtime.DriftImage))
	for _, age := range []time.Duration{time.Hour, 25 * time.Hour, 30 * 24 * time.Hour} {
		rig.clockAt(podUpdateStart.Add(age))
		rig.sweepsSettled(1)
		rig.wantNoRound(fmt.Sprintf("a sweep %s after the pods were launched", age))
	}
}

// A daemon that restarts at 30 days opens no round for those pods either, at its first sweep or
// at the sweeps after it.
func TestAnOldPodWithDrainingDriftOpensNoRoundAfterARestart(t *testing.T) {
	t.Parallel()
	rig := newPodUpdateRig(t, readyWorker(claim.RoleImplementer), readyWorker(claim.RoleTester))
	rig.driftsBy(driftOf(runtime.DriftImage))
	rig.sweepsSettled(1)
	rig.clockAt(podUpdateStart.Add(30 * 24 * time.Hour))
	next := rig.restart()
	next.driftsBy(driftOf(runtime.DriftImage))
	next.sweepsSettled(5)
	next.wantNoRound("the sweeps of a daemon that restarted 30 days after the pods were launched")
}

// The floor kind breaks: three pods below the floor open rounds at once, paced by the round keys,
// so under pod_update_rounds_per_minute 1 a sweep within 50 seconds of the first opens no second
// round, and each round's reasons name the floor.
func TestAFloorKindOpensAPacedRoundAtOnce(t *testing.T) {
	t.Parallel()
	keys := []string{"LEGION-1", "LEGION-2", "LEGION-3"}
	rig := newLimitRig(t, keys...)
	floor := runtime.DriftReason{Kind: runtime.DriftFloor, Text: "contract 200, below the floor 219"}
	for _, key := range keys {
		rig.driftsBy(key, floor)
	}
	rig.s.cfg.PodUpdateRoundsPerMinute = new(1)

	rig.tick()
	rig.awaitRounds("the first sweep over three pods below the floor", keys[0])
	rig.clock.advance(50 * time.Second)
	rig.tick()
	for _, key := range keys {
		rig.machineOf(key, claim.RoleArchitect).Wait()
	}
	if open := rig.openRounds(); !slices.Equal(open, keys[:1]) {
		t.Errorf("the rounds open after a sweep 50 seconds after the first are %v, want only %v", open, keys[:1])
	}
	if reasons := rig.claimOf(keys[0], claim.RoleArchitect).UpdateReasons; !strings.Contains(reasons, "floor") {
		t.Errorf("the round's reasons are %q, want them to name the floor", reasons)
	}
	rig.clock.advance(time.Minute)
	rig.tick()
	rig.awaitRounds("a sweep a minute after the first", keys[:2]...)
}

// A role's start at a daemon's boot, before the boot's first sweep has read the pod's drift, keeps
// the pod of a round an older daemon left open with no role process in it, though nothing else
// holds the pod: that reading may end the round with no replacement, and no start of a role
// replaces a pod whose drift only drains (R1, R4). The first sweep then ends the round with no
// replacement, and the role goes on working in the pod.
func TestALaunchBeforeTheBootsFirstReadingKeepsTheRoundsPod(t *testing.T) {
	t.Parallel()
	rig := newPodUpdateRig(t, answeredThenSuspended(claim.RoleImplementer))
	image := driftOf(runtime.DriftImage)
	rig.driftsBy(image)
	rig.rt.ScriptIssuePod(podUpdateIssue, podUpdatePod)
	rig.rt.ScriptIssuePodDrift(podUpdateIssue, image)

	token := rig.tokens[claim.RoleImplementer]
	rig.intoPod(claim.RoleImplementer, podUpdatePod)
	rig.sup.post(token, supervise.RequestResume{Claim: token})
	c := rig.awaitClaim(claim.RoleImplementer, "its launch to record its process", func(c supervise.Claim) bool { return c.Locator != nil })
	if pod := podOf(c.Locator); pod != podUpdatePod {
		t.Fatalf("the implementer was launched into pod %s, want the round's pod %s kept: no sweep has read its drift since the boot", pod, podUpdatePod)
	}
	if replaced := rig.rt.CallsOf("ReplaceRoundPodAtLaunch"); len(replaced) != 0 {
		t.Fatalf("a launch replaced the round's pod %+v before the boot's first reading of its drift", replaced)
	}
	rig.registerInto(claim.RoleImplementer, podUpdatePod)
	rig.awaitSwept(claim.RoleImplementer, "the round to end with no replacement", func(c supervise.Claim) bool { return c.UpdateRound == "" })
	rig.sweepsSettled(2)
	rig.wantNoRound("the sweeps after the round open at the boot ended")
	if replaced := rig.issueReplacements(); len(replaced) != 0 {
		t.Errorf("the pod was replaced by issue %+v, want never: its drift only drains", replaced)
	}
}

// A round whose claims record no pod, open at the boot over drift that breaks the pod, goes on,
// and the replacement by issue that the same sweep decides names the reasons written again at that
// first reading, each breaking one with the marker, not the older daemon's unmarked text, so a
// reader of the replacement's reasons sees the marker. The sweep's own decision is read here, since
// a later sweep reads the restated reasons from the claims whatever this one passed on.
func TestAPodlessRoundGoingOnAtTheBootIsReplacedWithTheMarkedReasons(t *testing.T) {
	t.Parallel()
	rig := newPodUpdateRig(t, answeredThenSuspended(claim.RoleTester),
		podlessMember(claim.RoleReviewer, supervise.StateSuspended, supervise.SuspendedByReleased, false))
	rig.rt.ScriptIssuePod(podUpdateIssue, podUpdatePod)
	claims := rig.snapshot()
	rig.s.noteBootRounds(claims)
	group := claimsOf(claims, podUpdateIssue)
	now := rig.clock.Now()
	sweep := rig.s.beginPodUpdateSweep(now, map[string][]supervise.Claim{podUpdateIssue: group})
	replace := rig.s.reconcileIssuePodUpdate(rig.ctx, podUpdateIssue, group, &sweep, rig.s.roundClockAt(now, nil))
	if replace == nil {
		t.Fatal("the first sweep since the boot decided no replacement by issue of a round over drift that breaks its pod, which nothing holds")
	}
	if want := recorded(podUpdateDrift); replace.reasons != want {
		t.Errorf("the replacement by issue names reasons %q, want the reasons written again with the marker, %q", replace.reasons, want)
	}
}

// A failed read by the issue of a round whose claims record no pod, open at the boot, decides
// nothing: the round is neither ended nor replaced at that sweep, since nobody read whether its
// drift drains. The next sweep that reads it decides, and over drift that drains ends the round
// with no replacement.
func TestAFailedBootReadOfAPodlessRoundDecidesNothing(t *testing.T) {
	t.Parallel()
	rig := newPodUpdateRig(t, answeredThenSuspended(claim.RoleTester),
		podlessMember(claim.RoleReviewer, supervise.StateSuspended, supervise.SuspendedByReleased, false))
	rig.rt.ScriptIssuePod(podUpdateIssue, podUpdatePod)
	rig.rt.ScriptIssuePodDrift(podUpdateIssue, driftOf(runtime.DriftImage))
	rig.rt.FailPodDrift(errors.New("the api server did not answer"))
	for range 3 {
		rig.tick()
	}
	rig.settleAll()
	if asked := rig.byIssueAsks(); len(asked) != 0 {
		t.Fatalf("the runtime was asked to replace the pod by issue %+v while nobody could read its drift, want nothing", asked)
	}
	if c := rig.claimOf(claim.RoleTester); c.UpdateRound != podUpdateRound {
		t.Fatalf("the tester is in round %q after failed reads, want the round kept open", c.UpdateRound)
	}

	rig.rt.FailPodDrift(nil)
	rig.tick()
	rig.awaitClaim(claim.RoleTester, "the round to end with no replacement", func(c supervise.Claim) bool { return c.UpdateRound == "" })
	if asked := rig.byIssueAsks(); len(asked) != 0 {
		t.Errorf("the runtime was asked to replace the pod by issue %+v, want never: its drift only drains", asked)
	}
}

// A round this daemon opened is never one found open at its boot, however long its post takes to
// reach the claims. The round's post waits in a machine held inside a relaunch; meanwhile the pod's
// breaking drift is mended and only drift that drains is left. Once the machine takes the post, the
// round is the one this daemon opened, its terms fixed at its open, and it goes on: a sweep never
// reads it as a boot round and ends it with no replacement.
func TestARoundThisDaemonOpenedIsNeverABootRound(t *testing.T) {
	t.Parallel()
	const issue = "LEGION-1"
	relaunching := limitIssue(1, issue)
	relaunching.roles = []claim.Role{claim.RoleArchitect, claim.RoleImplementer}
	relaunching.unregistered = []claim.Role{claim.RoleImplementer}
	rig := newFleetRig(t, relaunching)
	rig.drifts(issue, limitDrift)

	architect := rig.token(issue, claim.RoleArchitect)
	died := *rig.claimOf(issue, claim.RoleArchitect).Locator
	rig.rt.ScriptResume(fake.SpawnResult{Locator: *podUpdateLocator(architect, issue, rig.pods[issue])})
	entered, release := make(chan struct{}), make(chan struct{})
	releaseMachine := sync.OnceFunc(func() { close(release) })
	rig.rt.HoldResume(func() {
		rig.rt.HoldResume(nil)
		close(entered)
		<-release
	})
	defer releaseMachine()
	go func() {
		m, _ := rig.sup.Machine(architect)
		_ = m.Handle(rig.ctx, supervise.RuntimeObservation{
			Observation: runtime.Observation{Locator: died, Kind: runtime.Gone, Detail: "the process ended"},
		})
	}()
	select {
	case <-entered:
	case <-time.After(10 * time.Second):
		t.Fatal("the architect's machine never reached the Resume this test holds it in")
	}

	// The first sweep opens the round over drift that breaks; its post waits in the held machine.
	rig.tick()
	// The breaking drift is mended while the post is in flight.
	rig.driftsBy(issue, driftOf(runtime.DriftImage))
	for range 2 {
		rig.clock.advance(time.Minute)
		rig.tick()
	}

	releaseMachine()
	rig.awaitRounds("the held machine took the round it was posted", issue)
	for range 2 {
		rig.clock.advance(time.Minute)
		rig.tick()
	}
	if c := rig.claimOf(issue, claim.RoleArchitect); !supervise.IsPodRound(c.UpdateRound) {
		t.Errorf("the architect records round %q after the sweeps, want the round this daemon opened kept open: it is no boot round", c.UpdateRound)
	}
}

// A sweep can end a round found open at the boot, over drift its pod's roles keep working under,
// while a member's launch into that pod is held inside its decision: the member's machine takes the
// end only once that launch is over. The launch then still finds the round on its own claim, and
// keeps the pod, since the sweep has ended the round with no replacement (R4).
func TestALaunchHeldWhileTheSweepEndsTheBootRoundKeepsThePod(t *testing.T) {
	t.Parallel()
	rig := newPodUpdateRig(t, answeredThenSuspended(claim.RoleImplementer))
	image := driftOf(runtime.DriftImage)
	rig.driftsBy(image)
	rig.rt.ScriptIssuePod(podUpdateIssue, podUpdatePod)
	rig.rt.ScriptIssuePodDrift(podUpdateIssue, image)

	token := rig.tokens[claim.RoleImplementer]
	holding, let := rig.holdNextResume(claim.RoleImplementer, podUpdatePod)
	rig.sup.post(token, supervise.RequestResume{Claim: token})
	select {
	case <-holding:
	case <-time.After(10 * time.Second):
		t.Fatal("the implementer's launch did not reach Resume")
	}
	rig.tick()
	if ended := loggedMessages(t, rig.logs.String(), roundEndsOverDrainingDriftLine); len(ended) != 1 {
		t.Fatalf("the sweep wrote %d lines that end the boot round, want one while the launch is held", len(ended))
	}

	let()
	c := rig.awaitClaim(claim.RoleImplementer, "its launch to record its process", func(c supervise.Claim) bool { return c.Locator != nil })
	if pod := podOf(c.Locator); pod != podUpdatePod {
		t.Fatalf("the implementer was launched into pod %s, want the round's pod %s kept: the sweep ended its round with no replacement", pod, podUpdatePod)
	}
	if replaced := rig.rt.CallsOf("ReplaceRoundPodAtLaunch"); len(replaced) != 0 {
		t.Fatalf("the launch replaced the round's pod %+v after the sweep ended the round", replaced)
	}
	rig.awaitClaim(claim.RoleImplementer, "its machine to take the end", func(c supervise.Claim) bool { return c.UpdateRound == "" })
}

// A round found open at the boot whose claims record no pod, and whose read by the issue keeps
// failing, decides nothing; past its bound the operator's side is told of it once, as a round whose
// pod a sweep cannot read is told.
func TestAPodlessBootRoundWhoseReadKeepsFailingIsToldPastItsBound(t *testing.T) {
	t.Parallel()
	rig := newPodUpdateRig(t, answeredThenSuspended(claim.RoleTester),
		podlessMember(claim.RoleReviewer, supervise.StateSuspended, supervise.SuspendedByReleased, false))
	rig.rt.ScriptIssuePod(podUpdateIssue, podUpdatePod)
	rig.rt.FailPodDrift(errors.New("the api server did not answer"))
	rig.clockAt(podUpdateBound.Add(time.Minute))
	for range 3 {
		rig.tick()
	}
	rig.settleAll()
	if asked := rig.byIssueAsks(); len(asked) != 0 {
		t.Fatalf("the runtime was asked to replace the pod by issue %+v while nobody could read its drift, want nothing", asked)
	}
	notices := rig.notices()
	if len(notices) != 1 || notices[0].Update == nil || notices[0].Update.Round != podUpdateRound {
		t.Fatalf("past the bound the controller was told %+v, want one update-stalled notice of round %s", notices, podUpdateRound)
	}
}

// A launch before the boot's first sweep into the pod of a round found open at the boot, over drift
// that breaks the pod, writes the round's reasons again with the marker before it goes on to the
// rule of a launch into a round's pod: a member of the round that is not launching records the
// marked reasons, so the round's end names them.
func TestALaunchBeforeTheFirstSweepWritesABreakingRoundsReasonsAgain(t *testing.T) {
	t.Parallel()
	rig := newPodUpdateRig(t, answeredThenSuspended(claim.RoleImplementer), answeredThenSuspended(claim.RoleTester))
	rig.rt.ScriptIssuePod(podUpdateIssue, podUpdatePod)
	token := rig.tokens[claim.RoleImplementer]
	rig.intoPod(claim.RoleImplementer, podUpdatePod)
	rig.sup.post(token, supervise.RequestResume{Claim: token})
	rig.awaitClaim(claim.RoleImplementer, "its launch to record its process", func(c supervise.Claim) bool { return c.Locator != nil })
	want := recorded(podUpdateDrift)
	rig.awaitClaim(claim.RoleTester, "the round's reasons written again with the marker", func(c supervise.Claim) bool {
		return c.UpdateRound == podUpdateRound && c.UpdateReasons == want
	})
}

// pausingHandler is a log handler that holds the first record whose message is msg until release is
// closed, after closing reached: a hook point inside the code that writes that line, for a test of
// what runs at that moment. It holds no lock while it waits, so other lines are written meanwhile.
type pausingHandler struct {
	slog.Handler
	msg              string
	reached, release chan struct{}
	once             *sync.Once
}

func (h *pausingHandler) Handle(ctx context.Context, r slog.Record) error {
	if r.Message == h.msg {
		h.once.Do(func() {
			close(h.reached)
			<-h.release
		})
	}
	return h.Handler.Handle(ctx, r)
}

func (h *pausingHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &pausingHandler{Handler: h.Handler.WithAttrs(attrs), msg: h.msg, reached: h.reached, release: h.release, once: h.once}
}

func (h *pausingHandler) WithGroup(name string) slog.Handler {
	return &pausingHandler{Handler: h.Handler.WithGroup(name), msg: h.msg, reached: h.reached, release: h.release, once: h.once}
}

// A launch that runs while a sweep is ending a round found open at the boot, over drift that
// drains, keeps the pod at every moment of that end: the sweep records the end before it drops the
// round's boot mark, and the launch reads the boot mark before the end, so it sees one or the
// other. The sweep is held inside the end, at the line it writes before it posts the end, while the
// launch runs.
func TestALaunchDuringTheBootRoundsEndKeepsThePod(t *testing.T) {
	t.Parallel()
	rig := newPodUpdateRig(t, answeredThenSuspended(claim.RoleImplementer))
	image := driftOf(runtime.DriftImage)
	rig.driftsBy(image)
	rig.rt.ScriptIssuePod(podUpdateIssue, podUpdatePod)
	rig.rt.ScriptIssuePodDrift(podUpdateIssue, image)
	reached, release := make(chan struct{}), make(chan struct{})
	releaseSweep := sync.OnceFunc(func() { close(release) })
	defer releaseSweep()
	rig.s.log = slog.New(&pausingHandler{Handler: rig.s.log.Handler(), msg: roundEndsOverDrainingDriftLine,
		reached: reached, release: release, once: &sync.Once{}})

	token := rig.tokens[claim.RoleImplementer]
	holding, letLaunch := rig.holdNextResume(claim.RoleImplementer, podUpdatePod)
	rig.sup.post(token, supervise.RequestResume{Claim: token})
	select {
	case <-holding:
	case <-time.After(10 * time.Second):
		t.Fatal("the implementer's launch did not reach Resume")
	}
	swept := make(chan struct{})
	go func() {
		defer close(swept)
		rig.tick()
	}()
	select {
	case <-reached:
	case <-time.After(10 * time.Second):
		t.Fatal("the sweep did not reach the end of the boot round")
	}

	letLaunch()
	c := rig.awaitClaim(claim.RoleImplementer, "its launch to record its process", func(c supervise.Claim) bool { return c.Locator != nil })
	releaseSweep()
	<-swept
	if pod := podOf(c.Locator); pod != podUpdatePod {
		t.Fatalf("the implementer was launched into pod %s while the sweep ended the round, want the round's pod %s kept", pod, podUpdatePod)
	}
	if replaced := rig.rt.CallsOf("ReplaceRoundPodAtLaunch"); len(replaced) != 0 {
		t.Fatalf("a launch replaced the round's pod %+v while the sweep ended the round over drift that drains", replaced)
	}
}
