package daemon

// The update round over a pod a claim's launch waits on (dispatch://INFRA-4425 R4). Under
// dispatch_token_broker with no dispatch_token_transition_file, a launch that would keep a live
// pod built before the exchange identity is refused (runtime.AwaitingPodRound), and the claim
// records no process while it waits: its machine keeps the pod the refusal named instead
// (supervise.Claim.AwaitsPod). For each claim it groups that records no process, the sweep reads
// that pod off the claim's machine (reconcilePodUpdates), so the round opens over it, waits on no
// answer of the claim, and replaces the pod through it where no claim records a process there;
// the claim's next try, within a minute, comes up in the pod that replaced it. The end-to-end
// cases drive the pod-update rig of podupdate_test.go; the rows hold each reader alone.
//
// That the launch after the round carries no Dispatch token is the Sandbox runtime's to show,
// since no runtime fake builds a pod: internal/runtime/sandbox's
// TestTheRoundReplacesThePodItsRefusalNamesAndTheNextLaunchTradesTheIdentity replaces the pod
// through the refusal's own locator and reads the start of the launch after it.

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/runtime/fake"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// podUpdateExchangeDrift is what the Sandbox runtime's PodDrift reports of a pod built before the
// exchange identity once the deployment names an exchange audience (movedExchangeIdentity): the
// drift a round over such a pod is opened for.
const podUpdateExchangeDrift = "agent-secrets-exchange (unset), now audience=legion-broker-exchange expiry=3600s path=token"

// podRoundRetry is how long a start refused for want of its pod's round waits before it is tried
// again (supervise's podRoundRetry): the wait a test fires to run that retry.
const podRoundRetry = time.Minute

// awaitedLocator is the pod the Sandbox runtime's refusal of token's launch names: the locator a
// process of token's claim in pod would have had, at the refused launch's generation.
func awaitedLocator(token claim.Token, pod string, generation uint64) runtime.Locator {
	loc := *podUpdateLocator(token, podUpdateIssue, pod)
	sandbox := *loc.Sandbox
	sandbox.Generation = generation
	loc.Sandbox, loc.Incarnation = &sandbox, runtime.SandboxIncarnation(pod, generation)
	return loc
}

// awaitingRound is the Sandbox runtime's refusal of token's launch into pod for want of its update
// round (runtime.AwaitingPodRound), at the launch's own generation: the one refusal every helper of
// these tests makes a launch answer (refuseUntilReplaced, refuseLaunches).
func awaitingRound(token claim.Token, pod string, generation uint64) error {
	return fmt.Errorf("launch %s: %w", token, &runtime.AwaitingPodRound{
		Pod:    awaitedLocator(token, pod, generation),
		Reason: "pod legion-legion-484 carries no exchange identity and dispatch_token_transition_file is not set; its update round replaces it",
	})
}

// refuseUntilReplaced makes every launch of role's claim, a Resume or a Spawn, the Sandbox
// runtime's refusal of a launch into podUpdatePod (awaitingRound), at that launch's own
// generation, until a pod has been replaced: a runtime refuses such a launch at every try while
// the kept pod stands. Once a ReplacePod has run, a launch goes through, and a Resume answers what
// ScriptResume queued.
func (r *podUpdateRig) refuseUntilReplaced(role claim.Role) {
	r.t.Helper()
	token := r.tokens[role]
	refuse := func(method string) func(context.Context) error {
		return func(context.Context) error {
			if len(r.rt.CallsOf("ReplacePod")) > 0 {
				return nil
			}
			launches := r.rt.CallsOf(method)
			spec := launches[len(launches)-1].Spec
			if spec.Claim != token {
				return nil
			}
			return awaitingRound(token, podUpdatePod, spec.Generation)
		}
	}
	r.rt.OnResume, r.rt.OnSpawn = refuse("Resume"), refuse("Spawn")
}

// refusedFirstLaunch is role's first launch, as a claim the boot restored queued with no process
// makes it, answered with the runtime's wait for its pod's round: the release of the boot's
// launch_uncertain settles the claim before its start, as the daemon's unfinished-launch path
// does (launchUnfinishedClaim), and the spawn that follows is refused. A test makes the runtime
// refuse it first (refuseUntilReplaced, refuseLaunches). It holds the claim to no session and no
// process after it, waiting on the pod the refusal named.
func (r *podUpdateRig) refusedFirstLaunch(role claim.Role) {
	r.t.Helper()
	token := r.tokens[role]
	m, ok := r.sup.Machine(token)
	if !ok {
		r.t.Fatalf("the %s is not supervised", role)
	}
	if _, err := m.ReleaseUncertainLaunch(r.ctx); err != nil {
		r.t.Fatalf("release the %s's unfinished launch: %v", role, err)
	}
	if err := m.Handle(r.ctx, supervise.RequestSpawn{Claim: token}); !errors.Is(err, runtime.ErrAwaitingPodRound) {
		r.t.Fatalf("the %s's first launch answered %v, want the runtime's wait for its pod's round", role, err)
	}
	if c := r.claimOf(role); c.Session != "" || c.Locator != nil || c.AwaitsPod == nil {
		r.t.Fatalf("the refused %s has session %q and process %+v, waiting on %+v; want no session and no process, waiting on a pod",
			role, c.Session, c.Locator, c.AwaitsPod)
	}
}

// diesRefused is role's process dying, and the relaunch that death raises refused by the runtime
// for want of the pod's round: the claim is left launching with no process, its machine keeping
// the pod the refusal named.
func (r *podUpdateRig) diesRefused(role claim.Role) {
	r.t.Helper()
	m, ok := r.sup.Machine(r.tokens[role])
	if !ok {
		r.t.Fatalf("%s is not supervised", role)
	}
	ended := r.claimOf(role).Locator
	if ended == nil {
		r.t.Fatalf("the %s records no process to die", role)
	}
	err := m.Handle(r.ctx, supervise.RuntimeObservation{Observation: runtime.Observation{Locator: *ended, Kind: runtime.Gone, At: r.clock.Now()}})
	if !errors.Is(err, runtime.ErrAwaitingPodRound) {
		r.t.Fatalf("the %s's relaunch answered %v, want the runtime's wait for its pod's round", role, err)
	}
	c := r.claimOf(role)
	if c.State != supervise.StateLaunching || c.Locator != nil || podOf(c.AwaitsPod) != podUpdatePod {
		r.t.Fatalf("the refused %s is %s at %+v waiting on %+v, want launching with no process and waiting on pod %s",
			role, c.State, c.Locator, c.AwaitsPod, podUpdatePod)
	}
}

// retryLaunch runs the minute's retry of every start the runtime refused for want of its pod's
// round, as the machines' timers do, and waits for each decision it starts.
func (r *podUpdateRig) retryLaunch() {
	r.t.Helper()
	if fired := r.clock.fire(podRoundRetry); fired == 0 {
		r.t.Fatal("no retry of a refused launch was armed")
	}
}

// The defect R4 names: an issue whose only claim's launch is refused for want of its pod's round,
// with no process recorded anywhere in that pod. The claim's retries are refused every minute,
// and it keeps waiting on the same pod through them. The sweep opens the round over that pod,
// asks the claim nothing and waits on no answer of it, and replaces the pod through the locator
// the refusal named. The claim's next try comes up in the pod that replaced it, which has drifted
// from nothing, and the controller is told nothing: the round ended before its bound.
//
// The round passed over no claim, but no role process ran in the pod, so the replacement is the
// replacement of a pod no role process ran in (processlessRead), as the claim's own retry would
// have made it had it reached the pod first (endRoundALaunchReplaced). Its line names the read as
// not made for that reason, and one message on the issue names the waiting implementer as a
// member that did not answer the round: its worker died in the pod, so what it did not push went
// with the pod (lostClaims).
func TestPodUpdateReplacesThePodItsOnlyClaimsLaunchWaitsOn(t *testing.T) {
	t.Parallel()
	rig := newPodUpdateRig(t, readyWorker(claim.RoleImplementer))
	token := rig.tokens[claim.RoleImplementer]
	rig.drifts(podUpdateExchangeDrift)
	rig.refuseUntilReplaced(claim.RoleImplementer)
	rig.diesRefused(claim.RoleImplementer)
	rig.retryLaunch()
	if c := rig.claimOf(claim.RoleImplementer); c.Locator != nil || podOf(c.AwaitsPod) != podUpdatePod {
		t.Fatalf("after a refused retry the claim is at %+v waiting on %+v, want no process and the same pod", c.Locator, c.AwaitsPod)
	}
	// The pod the round replaces is the one the last refusal named, at that refused try's own
	// generation (refuseUntilReplaced).
	resumes := rig.rt.CallsOf("Resume")
	refused := resumes[len(resumes)-1].Spec.Generation

	rig.tick()
	opened := rig.awaitRound(claim.RoleImplementer, podUpdateRound)
	if opened.UpdateReasons != podUpdateExchangeDrift {
		t.Errorf("the round was opened for %q, want the drift the runtime read through the awaited pod, %q", opened.UpdateReasons, podUpdateExchangeDrift)
	}

	replaced := rig.awaitReplacement(1, "through the claim whose launch waits on it")
	if got := replaced[0].Pod; got.Claim != token || got.Locator == nil ||
		!reflect.DeepEqual(*got.Locator, awaitedLocator(token, podUpdatePod, refused)) {
		t.Fatalf("ReplacePod was asked %+v, want the refused claim's own locator in pod %s", got, podUpdatePod)
	}
	for _, ask := range rig.asks() {
		t.Errorf("the round asked %s (%+v), want nothing asked of a claim that records no process", ask.Role, ask)
	}
	rig.awaitClaim(claim.RoleImplementer, "the round's end", func(c supervise.Claim) bool { return c.UpdateRound == "" })

	rig.rt.ScriptResume(fake.SpawnResult{Locator: *podUpdateLocator(token, podUpdateIssue, podUpdateReplacementPod)})
	rig.workers[claim.RoleImplementer] = fake.NewConn()
	rig.conns.Register(token, rig.workers[claim.RoleImplementer])
	rig.retryLaunch()
	if c := rig.claimOf(claim.RoleImplementer); podOf(c.Locator) != podUpdateReplacementPod || c.AwaitsPod != nil {
		t.Fatalf("the try after the round is at %+v waiting on %+v, want it in the pod %s and waiting on none",
			c.Locator, c.AwaitsPod, podUpdateReplacementPod)
	}
	rig.registerInto(claim.RoleImplementer, podUpdateReplacementPod)
	rig.tick()
	if c := rig.claimOf(claim.RoleImplementer); c.UpdateRound != "" {
		t.Errorf("a sweep over the pod that replaced the round's own opened round %q, want none: that pod has drifted from nothing", c.UpdateRound)
	}
	lines := loggedLines(t, rig.logs.String(), podUpdateReplacedLine)
	if len(lines) != 1 {
		t.Fatalf("the replacement was logged %+v, want one line", lines)
	}
	if line := lines[0]; line["workspaceRead"] != "not made" || line["workspaceNotReadBecause"] != noProcessRead {
		t.Errorf("the replacement's line names the read %+v, want one not made because %q", line, noProcessRead)
	}
	wantToldUnanswered(t, rig.messages(), "implementer (launching)")
	if notices := rig.notices(); len(notices) > 0 {
		t.Errorf("the controller was told %+v, want nothing: the round ended before its bound", notices)
	}
}

// A second claim of the issue records a process in the pod while the first claim's launch waits
// on it. The round goes through that process as every round does: the second claim is asked and
// the pod waits on its answer, and the pod is replaced through its process, not through the
// waiting claim's locator. The waiting claim is recorded in the round and asked nothing.
func TestPodUpdateGoesThroughAProcessASecondClaimStartsWhileTheFirstWaits(t *testing.T) {
	t.Parallel()
	rig := newPodUpdateRig(t,
		readyWorker(claim.RoleImplementer),
		podUpdateRole{role: claim.RoleTester, state: supervise.StateQueued},
	)
	tester := rig.tokens[claim.RoleTester]
	rig.drifts(podUpdateExchangeDrift)
	rig.refuseUntilReplaced(claim.RoleImplementer)
	rig.diesRefused(claim.RoleImplementer)
	rig.startInPod(claim.RoleTester, podUpdatePod)

	rig.awaitAsked(claim.RoleTester, podUpdateRound)
	waiting := rig.awaitClaim(claim.RoleImplementer, "the round recorded on the claim that waits", func(c supervise.Claim) bool {
		return c.UpdateRound == podUpdateRound
	})
	if waiting.UpdateTaskedAt != nil {
		t.Errorf("the waiting claim records an ask of the round at %s, want none", waiting.UpdateTaskedAt)
	}
	if n := len(rig.replacements()); n != 0 {
		t.Fatalf("the pod was replaced %d times before the tester answered, want none", n)
	}

	rig.signal(claim.RoleTester, "c0ffee")
	replaced := rig.awaitReplacement(1, "once the tester answered")
	if got := replaced[0].Pod; got.Claim != tester || got.Locator == nil || !reflect.DeepEqual(*got.Locator, *podUpdateLocator(tester, podUpdateIssue, podUpdatePod)) {
		t.Fatalf("ReplacePod was asked %+v, want the tester's process in pod %s", got, podUpdatePod)
	}
	roles := []claim.Role{}
	for _, ask := range rig.asks() {
		roles = append(roles, ask.Role)
	}
	if !slices.Equal(roles, []claim.Role{claim.RoleTester}) {
		t.Errorf("the round asked %v, want the tester alone", roles)
	}
}

// replacedThroughAProcessBesideAWaitingClaim carries a round to its replacement through the
// tester's process, beside waiting, whose process died in the pod and whose relaunch the runtime
// refused for want of the pod's round (diesRefused): a member of the round with a session whose
// launch waits on the pod (awaitsRoundPod), which has not answered and has no process to answer
// with. The round waits on no answer of it (waitingClaims) and passes over no claim, so it ends on
// the tester's answer. work is what a read of the issue's workspace finds. notWaited is each other
// claim of the issue, a claim with a session that the round neither asks nor waits for and that
// does not answer it: a failed claim. It holds that the pod is replaced once, through the tester's
// process, and that the replacement's one line names the roles of notWaited under notWaitedFor and
// waiting under waitingOnThePodNotWaitedFor, and it returns the rig and that line.
func replacedThroughAProcessBesideAWaitingClaim(t *testing.T, work runtime.WorkspaceWork, waiting claim.Role, notWaited ...podUpdateRole) (*podUpdateRig, map[string]any) {
	t.Helper()
	rig := newPodUpdateRig(t, append([]podUpdateRole{readyWorker(waiting), readyWorker(claim.RoleTester)}, notWaited...)...)
	tester := rig.tokens[claim.RoleTester]
	rig.rt.ScriptWorkspaceWork(podUpdateIssue, work)
	rig.drifts(podUpdateExchangeDrift)
	rig.refuseUntilReplaced(waiting)
	rig.diesRefused(waiting)

	rig.tick()
	rig.awaitAsked(claim.RoleTester, podUpdateRound)
	rig.awaitRound(waiting, podUpdateRound)
	rig.signal(claim.RoleTester, "c0ffee")
	replaced := rig.awaitReplacement(1, "once the tester answered")
	if got := replaced[0].Pod; got.Claim != tester || podOf(got.Locator) != podUpdatePod {
		t.Fatalf("ReplacePod was asked %+v, want the tester's process in pod %s", got, podUpdatePod)
	}
	rig.awaitClaim(claim.RoleTester, "the round's end", func(c supervise.Claim) bool { return c.UpdateRound == "" })
	lines := loggedLines(t, rig.logs.String(), podUpdateReplacedLine)
	if len(lines) != 1 || lines[0]["atTheBound"] != false {
		t.Fatalf("the replacement was logged %+v, want one line that no bound made", lines)
	}
	line := lines[0]
	roles := make([]claim.Role, len(notWaited))
	for i, role := range notWaited {
		roles[i] = role.role
	}
	wantField(t, line, "notWaitedFor", roles...)
	wantField(t, line, "waitingOnThePodNotWaitedFor", waiting)
	return rig, line
}

// wantReadThroughTheTester holds the replacement beside a waiting claim to one read of the
// workspace, made through the tester, whose process the pod is replaced through, and named on the
// line as found, which is "found work" or "found no work".
func wantReadThroughTheTester(t *testing.T, rig *podUpdateRig, line map[string]any, found string) {
	t.Helper()
	if reads := rig.rt.CallsOf("WorkspaceWork"); len(reads) != 1 || reads[0].Issue != podUpdateIssue || reads[0].Claim != rig.tokens[claim.RoleTester] {
		t.Errorf("the workspace was read %+v, want one read of %s through the tester", reads, podUpdateIssue)
	}
	if line["workspaceRead"] != found || line["workspaceReadBy"] != string(rig.tokens[claim.RoleTester]) {
		t.Errorf("the replacement's line names the read %+v, want one the tester made that %s", line, found)
	}
}

// The round's pod holds the tester's process, and the implementer's process died there before
// its relaunch was refused, so its launch waits on the pod. It is a member of the round with a
// session that has not answered. No role was asked to push what its worker left on the pod's
// disk, as no role is asked for a claim the round passed over, so the replacement through the
// tester's process reads the workspace through the tester first, though the round passed over no
// claim. The read finds an unpushed commit, and one message on the issue names the implementer
// with its state, under the sentence of a read made through a process, and the commit. The
// implementer's own retry cannot replace the pod here: a launch keeps a pod that a claim records a
// process in, and the tester records one (roundPodEndsAtLaunch, keepAtLaunch). So this replacement
// is the one that names the implementer on the issue, and it names it only where its read found
// work or failed (workspaceRead.tells), as it names a claim the round passed over.
func TestPodUpdateReadsTheWorkspaceForAWaitingClaimBesideAProcess(t *testing.T) {
	t.Parallel()
	rig, line := replacedThroughAProcessBesideAWaitingClaim(t,
		runtime.WorkspaceWork{Head: passedOverHead, Commits: []string{passedOverCommit}, Count: 1}, claim.RoleImplementer)
	wantReadThroughTheTester(t, rig, line, "found work")
	messages := rig.messages()
	if len(messages) != 1 {
		t.Fatalf("the issue was written %d messages, want one: %q", len(messages), messages)
	}
	for _, want := range []string{toldNotWaitedForRoles + "implementer (launching).", passedOverCommit, "Work that is not on a branch at origin is lost."} {
		if !strings.Contains(messages[0], want) {
			t.Errorf("the message does not say %q:\n%s", want, messages[0])
		}
	}
	if strings.Contains(messages[0], toldUnansweredRoles) {
		t.Errorf("the message says %q, want the sentence of a read made through a process:\n%s", toldUnansweredRoles, messages[0])
	}
}

// The same round over a workspace whose read finds no work: the replacement reads the workspace
// through the tester, and the issue is written nothing, since the read found nothing that went
// with the pod.
func TestPodUpdateWritesNothingWhereTheReadForAWaitingClaimFindsNoWork(t *testing.T) {
	t.Parallel()
	rig, line := replacedThroughAProcessBesideAWaitingClaim(t, runtime.WorkspaceWork{Head: passedOverHead}, claim.RoleImplementer)
	wantReadThroughTheTester(t, rig, line, "found no work")
	if messages := rig.messages(); len(messages) != 0 {
		t.Errorf("the issue was written %q, want nothing: the read found no work", messages)
	}
}

// The waiting member is the planner, and beside it two claims failed, the implementer and the
// reviewer: claims with a session that the round neither asks nor waits for (notWaitedFor). The
// group is read in the order of the claims' tokens, which end in the role, so the implementer comes
// before the planner and the reviewer after it. The replacement through the tester's process reads
// the workspace, the read finds work, and the one message on the issue names each of the three
// once, in the group's order: the implementer, the planner, the reviewer. A list that named the
// claims the round did not wait for and then the waiting members, or the waiting members first,
// would name the planner last or first.
//
// The controls are the message's list built as either of those two lists joined: this test then
// fails, and its message names the planner last, or first.
func TestPodUpdateNamesEachClaimItDidNotWaitForInTheGroupsOrder(t *testing.T) {
	t.Parallel()
	failed := func(role claim.Role) podUpdateRole {
		return podUpdateRole{role: role, state: supervise.StateFailed, session: true, contract: readyWorker(role).contract}
	}
	rig, line := replacedThroughAProcessBesideAWaitingClaim(t,
		runtime.WorkspaceWork{Head: passedOverHead, Commits: []string{passedOverCommit}, Count: 1},
		claim.RolePlanner, failed(claim.RoleImplementer), failed(claim.RoleReviewer))
	wantReadThroughTheTester(t, rig, line, "found work")
	messages := rig.messages()
	if len(messages) != 1 {
		t.Fatalf("the issue was written %d messages, want one: %q", len(messages), messages)
	}
	if want := toldNotWaitedForRoles + "implementer (failed), planner (launching), reviewer (failed)."; !strings.Contains(messages[0], want) {
		t.Errorf("the message does not say %q, which names each claim once, in the group's order:\n%s", want, messages[0])
	}
}

// The implementer answered the round in the pod, and then its process died there and the runtime
// refused its relaunch for want of the pod's round (diesRefused): a member of the round with a
// session whose launch waits on the pod, and that answered. The round passed over no claim. The
// implementer's retry of its launch is in its decision while the sweep decides the replacement:
// the runtime holds that retry inside Resume (holdNextResume), and the machine has published the
// claim with its answer. The replacement's decision reads such a claim as one that has not
// answered (masked), but the round waits for no answer of a claim whose launch waits on the pod,
// so the replacement is made at this sweep. The read of the workspace, the line and the message
// read the claim as it holds its answer (noticeGroup): the implementer answered, so it pushed its
// work, and no claim is left that the round did not wait for and that had not answered. So the
// replacement through the tester makes no read of the workspace, the line names no claim under
// notWaitedFor or waitingOnThePodNotWaitedFor, and the issue is written nothing.
func TestPodUpdateSkipsTheWorkspaceReadForAWaitingMemberThatAnsweredWhileItsMachineDecides(t *testing.T) {
	t.Parallel()
	rig := newPodUpdateRig(t, readyWorker(claim.RoleImplementer), readyWorker(claim.RoleTester))
	token, tester := rig.tokens[claim.RoleImplementer], rig.tokens[claim.RoleTester]
	rig.rt.ScriptWorkspaceWork(podUpdateIssue, runtime.WorkspaceWork{Head: passedOverHead, Commits: []string{passedOverCommit}, Count: 1})
	rig.drifts(podUpdateExchangeDrift)

	rig.tick()
	rig.awaitAsked(claim.RoleImplementer, podUpdateRound)
	rig.awaitAsked(claim.RoleTester, podUpdateRound)
	rig.signal(claim.RoleImplementer, "c0ffee")
	rig.refuseUntilReplaced(claim.RoleImplementer)
	rig.diesRefused(claim.RoleImplementer)
	if c := rig.claimOf(claim.RoleImplementer); c.UpdateSignalledAt == nil {
		t.Fatalf("the refused implementer is %+v, want its answer to the round standing", c)
	}
	rig.signal(claim.RoleTester, "c0ffee")
	rig.settleAll()
	if n := len(rig.replacements()); n != 0 {
		t.Fatalf("the pod was replaced %d times before the sweep the test holds the implementer's retry at, want none", n)
	}

	holding, release := rig.holdNextResume(claim.RoleImplementer, podUpdateReplacementPod)
	retried := make(chan int, 1)
	go func() { retried <- rig.clock.fire(podRoundRetry) }()
	select {
	case <-holding:
	case <-time.After(10 * time.Second):
		t.Fatal("the implementer's retry of its launch did not reach Resume")
	}
	m, _ := rig.sup.Machine(token)
	if view := m.View().Claim; m.Deciding() == nil || view.UpdateSignalledAt == nil || view.Locator != nil || podOf(view.AwaitsPod) != podUpdatePod {
		t.Fatalf("the implementer's machine decides %v and publishes %+v, want it deciding with its answer published and its launch waiting on pod %s",
			m.Deciding(), view, podUpdatePod)
	}
	rig.tick()
	release()
	select {
	case fired := <-retried:
		if fired != 1 {
			t.Fatalf("%d retries of a refused launch were armed, want the implementer's one", fired)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the implementer's retry did not end once its Resume was let go")
	}

	replaced := rig.replacements()
	if len(replaced) != 1 || replaced[0].Pod.Claim != tester || podOf(replaced[0].Pod.Locator) != podUpdatePod {
		t.Fatalf("ReplacePod was asked %+v at the sweep, want once, through the tester's process in pod %s", replaced, podUpdatePod)
	}
	rig.awaitClaim(claim.RoleTester, "the round's end", func(c supervise.Claim) bool { return c.UpdateRound == "" })
	lines := loggedLines(t, rig.logs.String(), podUpdateReplacedLine)
	if len(lines) != 1 {
		t.Fatalf("the replacement was logged %+v, want one line", lines)
	}
	if reads := rig.rt.CallsOf("WorkspaceWork"); len(reads) != 0 {
		t.Errorf("the workspace was read %+v, want no read: the implementer answered the round, so no claim had work that nobody was asked to push", reads)
	}
	wantField(t, lines[0], "notWaitedFor")
	wantField(t, lines[0], "waitingOnThePodNotWaitedFor")
	if messages := rig.messages(); len(messages) != 0 {
		t.Errorf("the issue was written %q, want nothing: the implementer answered the round, and no other claim lost work", messages)
	}
}

// The replacement's log line names each claim with a session that the round did not wait for
// (replacePodForRound). The issue has two such claims. The implementer's process died and the
// runtime refused its relaunch for want of the pod's round, so its launch waits on the pod and it
// has no process to answer. The planner failed, so the round asks it nothing. The round waits for
// neither of them: it replaces the pod before its bound, through the locator the refusal named,
// and its one line names the planner in notWaitedFor and the implementer in
// waitingOnThePodNotWaitedFor.
//
// The control is a line with one of the two fields empty: this test then fails and names that
// field.
func TestPodUpdateReplacementLineNamesEachClaimTheRoundDidNotWaitFor(t *testing.T) {
	t.Parallel()
	rig := newPodUpdateRig(t,
		readyWorker(claim.RoleImplementer),
		podUpdateRole{role: claim.RolePlanner, state: supervise.StateFailed, session: true,
			contract: readyWorker(claim.RolePlanner).contract},
	)
	token := rig.tokens[claim.RoleImplementer]
	rig.drifts(podUpdateExchangeDrift)
	rig.refuseUntilReplaced(claim.RoleImplementer)
	rig.diesRefused(claim.RoleImplementer)
	resumes := rig.rt.CallsOf("Resume")
	refused := resumes[len(resumes)-1].Spec.Generation

	rig.tick()
	rig.awaitRound(claim.RoleImplementer, podUpdateRound)
	replaced := rig.awaitReplacement(1, "with no claim to wait for")
	if got := replaced[0].Pod; got.Claim != token || got.Locator == nil ||
		!reflect.DeepEqual(*got.Locator, awaitedLocator(token, podUpdatePod, refused)) {
		t.Fatalf("ReplacePod was asked %+v, want the locator the refusal named, in pod %s",
			got, podUpdatePod)
	}
	lines := loggedLines(t, rig.logs.String(), podUpdateReplacedLine)
	if len(lines) != 1 || lines[0]["atTheBound"] != false {
		t.Fatalf("the replacement was logged %+v, want one line that no bound made", lines)
	}
	wantField(t, lines[0], "notWaitedFor", claim.RolePlanner)
	wantField(t, lines[0], "waitingOnThePodNotWaitedFor", claim.RoleImplementer)
}

// The round passed over a claim the daemon suspended, and no claim records a process in the pod,
// so the pod is replaced through the claim whose launch waits on it (podProcess). The passed-over
// claim would make a read of the issue's workspace due (readRoundWorkspace), but no role process
// runs in the pod to make it: the waiting claim runs none there, and the Sandbox runtime refuses a
// read through a claim that records no process (its runningRoleConn), as the fake is scripted to
// here. So the replacement makes no read, as the replacement of a pod no role process ran in makes
// none (processlessRead). Its line names the read as not made because no role process ran in the
// pod, and names the claims as every replacement through ReplacePod does. The one message on the
// issue names what the replacement by issue and at a launch name (lostClaims): each member that
// did not answer the round, the suspended planner and the waiting implementer, whose worker died
// in the pod with what it did not push. It says that no role process ran in the pod.
//
// The negative controls are #4392 before its fix, where the round read the workspace through the
// waiting implementer, the read failed, and the message said that the read failed.
func TestPodUpdateMakesNoReadThroughAClaimWhoseLaunchWaitsOnThePod(t *testing.T) {
	t.Parallel()
	rig := newPodUpdateRig(t,
		readyWorker(claim.RoleImplementer),
		suspendedRole(claim.RolePlanner, supervise.SuspendedByDaemon),
	)
	token := rig.tokens[claim.RoleImplementer]
	rig.rt.FailWorkspaceWork(podUpdateIssue, fmt.Errorf("read %s's workspace: %s holds no role process to read it", podUpdateIssue, token))
	rig.drifts(podUpdateExchangeDrift)
	rig.refuseUntilReplaced(claim.RoleImplementer)
	rig.diesRefused(claim.RoleImplementer)

	rig.tick()
	rig.awaitRound(claim.RoleImplementer, podUpdateRound)
	rig.awaitRound(claim.RolePlanner, podUpdateRound)
	replaced := rig.awaitReplacement(1, "through the claim whose launch waits on it")
	if got := replaced[0].Pod; got.Claim != token || podOf(got.Locator) != podUpdatePod {
		t.Fatalf("ReplacePod was asked %+v, want the waiting implementer's locator in pod %s", got, podUpdatePod)
	}
	if reads := rig.rt.CallsOf("WorkspaceWork"); len(reads) != 0 {
		t.Errorf("the workspace was read %+v, want no read: no role process ran in the pod", reads)
	}
	lines := loggedLines(t, rig.logs.String(), podUpdateReplacedLine)
	if len(lines) != 1 || lines[0]["atTheBound"] != false {
		t.Fatalf("the replacement was logged %+v, want one line that no bound made", lines)
	}
	line := lines[0]
	if line["workspaceRead"] != "not made" || line["workspaceNotReadBecause"] != noProcessRead {
		t.Errorf("the replacement's line names the read %+v, want one not made because %q", line, noProcessRead)
	}
	wantField(t, line, "notWaitedFor", claim.RolePlanner)
	wantField(t, line, "waitingOnThePodNotWaitedFor", claim.RoleImplementer)

	wantToldUnanswered(t, rig.messages(), "planner (suspended)", "implementer (launching)")
}

// toldUnansweredRoles is the lead sentence of the message on the issue that names the members of
// the round that had not answered it, of a pod no role process ran in (workspaceLostMessage).
const toldUnansweredRoles = "These roles did not answer the update round before the replacement. No process of these roles ran in the pod: "

// toldNotWaitedForRoles is the lead sentence of the message on the issue that names the claims the
// round did not wait for, over a read made through a process (workspaceLostMessage).
const toldNotWaitedForRoles = "The round did not wait for these roles, because no process of these roles ran in the pod: "

// wantToldUnanswered holds messages, what the issue was written, to the one message a replacement
// of a pod no role process ran in writes: it names each of named, a role with its state, as a
// member that did not answer the round, says that no role process ran in the pod to read the
// workspace, and says that work not on a branch at origin is lost.
func wantToldUnanswered(t *testing.T, messages []string, named ...string) {
	t.Helper()
	if len(messages) != 1 {
		t.Fatalf("the issue was written %d messages, want one: %q", len(messages), messages)
	}
	message := messages[0]
	for _, want := range append([]string{toldUnansweredRoles, "No role process ran in the pod", "Work that is not on a branch at origin is lost."}, named...) {
		if !strings.Contains(message, want) {
			t.Errorf("the message does not say %q:\n%s", want, message)
		}
	}
	for _, not := range []string{toldNotWaitedForRoles, "The read failed"} {
		if strings.Contains(message, not) {
			t.Errorf("the message says %q, want the message of a pod no role process ran in:\n%s", not, message)
		}
	}
}

// The lead sentence of the message on the issue says why it names its roles
// (workspaceLostMessage). A replacement of a pod no role process ran in names each member that had
// not answered the round (lostClaims), whoever replaced the pod: the sweep by issue, a launch, or
// the sweep through a claim whose launch waits on the pod. So its message says these roles did not
// answer. A read the sweep made through a process names the claims the round did not wait for
// (notWaitedFor), so its message says the round did not wait for them, whether the read failed or
// found work. The sweep makes both the replacement through a waiting claim and the read through a
// process (viaProcess), so whether a role process ran in the pod chooses the sentence
// (workspaceRead.noProcess), not who replaced it.
func TestTheWorkspaceLostMessageSaysWhyItNamesItsRoles(t *testing.T) {
	t.Parallel()
	round := podUpdateRound
	planner := supervise.Claim{Token: mustClaimToken(t, podUpdateIssue, claim.RolePlanner), Issue: podUpdateIssue, Role: claim.RolePlanner,
		State: supervise.StateSuspended, Session: "ses_planner", UpdateRound: round}
	unanswered, notWaitedFor := toldUnansweredRoles+"planner (suspended).", toldNotWaitedForRoles+"planner (suspended)."
	for name, tc := range map[string]struct {
		read      workspaceRead
		want, not string
	}{
		"a read through a process that failed": {
			read: workspaceRead{round: round, by: viaProcess, claim: mustClaimToken(t, podUpdateIssue, claim.RoleImplementer), err: errors.New("jj answered 1")},
			want: notWaitedFor, not: unanswered},
		"a read through a process that found work": {
			read: workspaceRead{round: round, by: viaProcess, claim: mustClaimToken(t, podUpdateIssue, claim.RoleImplementer), work: runtime.WorkspaceWork{Head: "abc123", Commits: []string{"def456"}, Count: 1}},
			want: notWaitedFor, not: unanswered},
		"the sweep's replacement through a claim whose launch waits on the pod": {
			read: processlessRead(round, viaProcess), want: unanswered, not: notWaitedFor},
		"a replacement by issue":    {read: processlessRead(round, viaIssue), want: unanswered, not: notWaitedFor},
		"a replacement at a launch": {read: processlessRead(round, viaLaunch), want: unanswered, not: notWaitedFor},
	} {
		t.Run(name, func(t *testing.T) {
			message := workspaceLostMessage(round, podUpdateExchangeDrift, []supervise.Claim{planner}, tc.read, false)
			if !strings.Contains(message, tc.want) || strings.Contains(message, tc.not) {
				t.Errorf("the message reads:\n%s\nwant it to say %q, and not %q", message, tc.want, tc.not)
			}
		})
	}
}

// waitingGeneration is the generation of the refused launch each claim waitingOn builds waits at:
// the generation of its awaited pod's locator (awaitedLocator).
const waitingGeneration = 2

// waitingOn is role's claim of the rig's issue with no process, its launch waiting on pod
// (supervise.Claim.AwaitsPod) at waitingGeneration, in round where round is not "", and with a
// session where session says so.
func waitingOn(t *testing.T, role claim.Role, pod, round string, session bool) supervise.Claim {
	t.Helper()
	token := mustClaimToken(t, podUpdateIssue, role)
	awaited := awaitedLocator(token, pod, waitingGeneration)
	c := supervise.Claim{Token: token, Issue: podUpdateIssue, Role: role, State: supervise.StateLaunching, AwaitsPod: &awaited, UpdateRound: round}
	if session {
		c.Session = "ses_" + string(role)
	}
	return c
}

// runningIn is role's claim of the rig's issue, ready with a session and a process in pod, in
// round where round is not "".
func runningIn(t *testing.T, role claim.Role, pod, round string) supervise.Claim {
	t.Helper()
	token := mustClaimToken(t, podUpdateIssue, role)
	return supervise.Claim{Token: token, Issue: podUpdateIssue, Role: role, State: supervise.StateReady,
		Session: "ses_" + string(role), Locator: podUpdateLocator(token, podUpdateIssue, pod), UpdateRound: round}
}

// registeredIn is c with set as the pods an agent of it registered a session in, as its machine
// knows them (supervise.Claim.RegisteredPods), which the sweep reads off the machine
// (reconcilePodUpdates). A claim waitingOn and runningIn build carries an empty, known set.
func registeredIn(c supervise.Claim, set supervise.RegisteredPods) supervise.Claim {
	c.RegisteredPods = set
	return c
}

// The group's pod, and the drift read of it, take a claim's awaited pod where it records no
// process: it is the pod the round opens over, two claims that record it and a process in it are
// one pod, and an awaited pod other than a process's is two pods, a replacement in flight. Its
// drift is read through the awaited locator, which carries the claim's own token.
func TestTheRoundReadsThePodAClaimsLaunchWaitsOn(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		group []supervise.Claim
		pod   string
		split bool
	}{
		"the only claim waits on the pod": {
			group: []supervise.Claim{waitingOn(t, claim.RoleImplementer, podUpdatePod, "", true)}, pod: podUpdatePod},
		"a claim waits on the pod another runs in": {
			group: []supervise.Claim{waitingOn(t, claim.RoleImplementer, podUpdatePod, "", true), runningIn(t, claim.RoleTester, podUpdatePod, "")}, pod: podUpdatePod},
		"a claim waits on another pod than a process runs in": {
			group: []supervise.Claim{waitingOn(t, claim.RoleImplementer, podUpdateReplacementPod, "", true), runningIn(t, claim.RoleTester, podUpdatePod, "")}, split: true},
		"no claim records a process or a pod": {
			group: []supervise.Claim{{Token: mustClaimToken(t, podUpdateIssue, claim.RoleTester), Issue: podUpdateIssue, Role: claim.RoleTester,
				State: supervise.StateSuspended, Session: "ses_tester"}}},
	} {
		t.Run(name, func(t *testing.T) {
			if pod, split := groupPod(tc.group); pod != tc.pod || split != tc.split {
				t.Fatalf("groupPod = %q, split %t; want %q, split %t", pod, split, tc.pod, tc.split)
			}
		})
	}

	t.Run("the drift is read through the awaited pod", func(t *testing.T) {
		waiting := waitingOn(t, claim.RoleImplementer, podUpdatePod, "", true)
		rt := fake.NewRuntime()
		rt.ScriptPodDrift(waiting.Token, podUpdateExchangeDrift)
		s := &supervision{runtime: rt, log: quietLogger()}
		reasons, reading := s.groupDrift(context.Background(), podUpdateIssue, []supervise.Claim{waiting})
		if reading != podRead || !slices.Equal(reasons, []string{podUpdateExchangeDrift}) {
			t.Fatalf("groupDrift = %q, reading %d; want %q, podRead", reasons, reading, podUpdateExchangeDrift)
		}
		asked := rt.CallsOf("PodDrift")
		if len(asked) != 1 || asked[0].Pod.Claim != waiting.Token || !reflect.DeepEqual(asked[0].Pod.Locator, waiting.AwaitsPod) {
			t.Fatalf("PodDrift was asked %+v, want the waiting claim's awaited pod %+v once", asked, waiting.AwaitsPod)
		}
		rt.ScriptPodGone(waiting.Token, true)
		if _, reading := s.groupDrift(context.Background(), podUpdateIssue, []supervise.Claim{waiting}); reading != podGone {
			t.Fatalf("groupDrift of an awaited pod that is gone reads %d, want podGone", reading)
		}
	})
}

// The round's answer set, and whom it names, by the one fact that names a claim
// (workOnRoundPod): its work may be on the round's pod's disk. A claim whose launch waits on the
// round's own pod is one the round is posted to — with a session or without, since it may be the
// issue's only claim — and one the replacement waits on no answer of, since it has no process in
// that pod. Where an agent of it registered in that pod and it has not answered, the gone-pod
// report names it as having lost what it left unpushed, as any member (lostClaims), as do the
// part of that report whose launch waits on the pod (lostWaitingClaims) and the message of a
// replacement through a process in the pod (notAskedToPush): its process can have died in that
// pod before its relaunch was refused, or it lost the session its agent registered there. The
// replacement's line names it too (unwaitedOnThePod), and also where it does not record the round
// yet, which no report names, since that claim is no member. With an answer to the round, it
// pushed its work before the pod went, and none names it. With a session registered in another
// pod only, or no registration at all, its work is on no disk of this pod, and none names it.
//
// An end names every member that had not answered whose set is unknown, as a restart leaves it
// (supervise.RegisteredPods.Unknown), with a session or with none, and whether or not its launch
// waits on the round's pod. A claim with a session that waits on another pod, and every claim
// with no process that waits on none, is weighed as any claim is. A claim the daemon suspended is
// passed over and waited for by nobody (roundAsks, dispatch://INFRA-4109 R1), and named where an
// agent of it registered in the round's pod. Where an earlier pod mounted the same kept volume,
// the store marks the round's pod as holding earlier work. Every claim with a session or a pod
// in its set is then named, wherever its agents registered, since the kept volume holds the
// shared workspace. A claim with neither is not named. A claim with no session that waits on
// another pod is not a way to find this round's pod, so the round is not posted to it.
func TestTheRoundWaitsOnNoAnswerOfAClaimWhoseLaunchWaitsOnItsPod(t *testing.T) {
	t.Parallel()
	round := podUpdateRound
	clock := roundClock{now: podUpdateStart, grace: podUpdateGrace}
	inPod := supervise.RegisteredPods{Pods: []string{podUpdatePod}}
	elsewhere := supervise.RegisteredPods{Pods: []string{podUpdateReplacementPod}}
	unknown := supervise.RegisteredPods{Unknown: true}
	answeredIt := registeredIn(waitingOn(t, claim.RoleImplementer, podUpdatePod, round, true), inPod)
	signalled := podUpdateStart
	answeredIt.UpdateSignalledAt = &signalled
	// released is a claim an idle release suspended, with its session, as the release leaves it.
	released := func(set supervise.RegisteredPods) supervise.Claim {
		return registeredIn(supervise.Claim{Token: mustClaimToken(t, podUpdateIssue, claim.RoleImplementer), Issue: podUpdateIssue,
			Role: claim.RoleImplementer, State: supervise.StateSuspended, SuspendedBy: supervise.SuspendedByReleased,
			Session: "ses_implementer", UpdateRound: round}, set)
	}
	queued := func(set supervise.RegisteredPods) supervise.Claim {
		return registeredIn(supervise.Claim{Token: mustClaimToken(t, podUpdateIssue, claim.RoleImplementer), Issue: podUpdateIssue,
			Role: claim.RoleImplementer, State: supervise.StateQueued, UpdateRound: round}, set)
	}
	for name, tc := range map[string]struct {
		claim supervise.Claim
		// earlierWork is whether the disk of the round's pod holds work from before the pod, here
		// an idle release's (roundPodHoldsEarlierWork).
		earlierWork                                            bool
		posted, waiting, lost, unwaited, waitingLost, notAsked bool
	}{
		"it waits on the round's pod, with a session registered there": {
			claim:  registeredIn(waitingOn(t, claim.RoleImplementer, podUpdatePod, round, true), inPod),
			posted: true, lost: true, unwaited: true, waitingLost: true, notAsked: true},
		"it waits on the round's pod, with a session registered there, and answered the round": {
			claim: answeredIt, posted: true},
		"it waits on the round's pod, with a session registered there, and records no round": {
			claim:  registeredIn(waitingOn(t, claim.RoleImplementer, podUpdatePod, "", true), inPod),
			posted: true, unwaited: true},
		"it waits on the round's pod, with a session registered in another pod only": {
			claim: registeredIn(waitingOn(t, claim.RoleImplementer, podUpdatePod, round, true), elsewhere), posted: true},
		"it waits on the round's pod, with a session registered in another pod only, and the pod is on a previously mounted kept volume": {
			claim: registeredIn(waitingOn(t, claim.RoleImplementer, podUpdatePod, round, true), elsewhere), earlierWork: true,
			posted: true, lost: true, unwaited: true, waitingLost: true, notAsked: true},
		"it waits on the round's pod, with a session, and its set unknown": {
			claim:  registeredIn(waitingOn(t, claim.RoleImplementer, podUpdatePod, round, true), unknown),
			posted: true, lost: true, unwaited: true, waitingLost: true, notAsked: true},
		"it waits on the round's pod, with no session, registered nowhere": {
			claim: waitingOn(t, claim.RoleImplementer, podUpdatePod, round, false), posted: true},
		"it waits on the round's pod, with no session, registered nowhere, and the pod is on a previously mounted kept volume": {
			claim: waitingOn(t, claim.RoleImplementer, podUpdatePod, round, false), earlierWork: true, posted: true},
		"it waits on the round's pod, with no session, registered there before": {
			claim:  registeredIn(waitingOn(t, claim.RoleImplementer, podUpdatePod, round, false), inPod),
			posted: true, lost: true, unwaited: true, waitingLost: true, notAsked: true},
		"it waits on the round's pod, with no session, registered in another pod only, and the pod is on a previously mounted kept volume": {
			claim: registeredIn(waitingOn(t, claim.RoleImplementer, podUpdatePod, round, false), elsewhere), earlierWork: true,
			posted: true, lost: true, unwaited: true, waitingLost: true, notAsked: true},
		"it waits on the round's pod, with no session, and its set unknown": {
			claim:  registeredIn(waitingOn(t, claim.RoleImplementer, podUpdatePod, round, false), unknown),
			posted: true, lost: true, unwaited: true, waitingLost: true, notAsked: true},
		"it waits on another pod, with a session registered in the round's pod": {
			claim:  registeredIn(waitingOn(t, claim.RoleImplementer, podUpdateReplacementPod, round, true), inPod),
			posted: true, waiting: true, lost: true},
		"it waits on another pod, with no session, registered nowhere": {
			claim: waitingOn(t, claim.RoleImplementer, podUpdateReplacementPod, round, false)},
		"it waits on another pod, with no session, and its set unknown": {
			claim: registeredIn(waitingOn(t, claim.RoleImplementer, podUpdateReplacementPod, round, false), unknown), lost: true},
		"it is suspended and waits on no pod, registered in the round's pod": {
			claim: released(inPod), posted: true, lost: true, notAsked: true},
		"it is suspended and waits on no pod, registered in another pod only": {
			claim: released(elsewhere), posted: true},
		"it is suspended and waits on no pod, registered in another pod only, and the pod took the release's work": {
			claim: released(elsewhere), earlierWork: true, posted: true, lost: true, notAsked: true},
		"it runs in the round's pod, registered there": {
			claim:  registeredIn(runningIn(t, claim.RoleImplementer, podUpdatePod, round), inPod),
			posted: true, waiting: true, lost: true},
		"it has no session, no process and waits on no pod, registered nowhere": {
			claim: queued(supervise.RegisteredPods{})},
		"it has no session, no process and waits on no pod, registered nowhere, and the pod is on a previously mounted kept volume": {
			claim: queued(supervise.RegisteredPods{}), earlierWork: true},
		"it has no session, no process and waits on no pod, and its set unknown": {
			claim: queued(unknown), lost: true},
	} {
		t.Run(name, func(t *testing.T) {
			group := []supervise.Claim{tc.claim}
			for _, check := range []struct {
				reader string
				got    []supervise.Claim
				want   bool
			}{
				{"postedClaims", postedClaims(group, round), tc.posted},
				{"waitingClaims", waitingClaims(group, round, clock), tc.waiting},
				{"lostClaims", lostClaims(group, round, tc.earlierWork), tc.lost},
				{"unwaitedOnThePod", unwaitedOnThePod(group, round, tc.earlierWork), tc.unwaited},
				{"lostWaitingClaims", lostWaitingClaims(group, round, tc.earlierWork), tc.waitingLost},
				{"notAskedToPush", notAskedToPush(group, round, clock, tc.earlierWork), tc.notAsked},
			} {
				if named := len(check.got) == 1; named != check.want {
					t.Errorf("%s names %v, want the claim named: %t", check.reader, namesOf(check.got), check.want)
				}
			}
		})
	}
}

// The pod is replaced through a claim whose launch waits on it where no claim records a process
// there, through the locator the runtime's refusal named; a process in the pod is still the one
// the pod is ended through, whichever claim comes first, and a claim waiting on another pod is no
// way to this one. podProcess answers the claim itself, and the runtime is asked through it at
// the process it records or the pod its launch waits on (podKnown). Each row writes that locator
// from the pod and the generation it built its claim with (runningIn, waitingOn).
func TestTheRoundReplacesThePodThroughAClaimWhoseLaunchWaitsOnIt(t *testing.T) {
	t.Parallel()
	round := podUpdateRound
	waiting := waitingOn(t, claim.RoleImplementer, podUpdatePod, round, true)
	elsewhere := waitingOn(t, claim.RoleImplementer, podUpdateReplacementPod, round, true)
	running := runningIn(t, claim.RoleTester, podUpdatePod, round)
	movedOn := runningIn(t, claim.RoleTester, podUpdateReplacementPod, round)
	for name, tc := range map[string]struct {
		group []supervise.Claim
		want  *supervise.Claim
		// at is the locator the runtime is asked through for want.
		at runtime.Locator
	}{
		"only a claim that waits on the pod": {group: []supervise.Claim{waiting}, want: &waiting,
			at: awaitedLocator(waiting.Token, podUpdatePod, waitingGeneration)},
		"a process in the pod, after a claim that waits": {group: []supervise.Claim{waiting, running}, want: &running,
			at: *podUpdateLocator(running.Token, podUpdateIssue, podUpdatePod)},
		"a claim that waits on another pod": {group: []supervise.Claim{elsewhere}},
		"a process in another pod, and a claim that waits": {group: []supervise.Claim{movedOn, waiting}, want: &waiting,
			at: awaitedLocator(waiting.Token, podUpdatePod, waitingGeneration)},
	} {
		t.Run(name, func(t *testing.T) {
			through, found := podProcess(tc.group, round)
			if tc.want == nil {
				if found {
					t.Fatalf("podProcess = %+v, want no way to the round's pod", through)
				}
				return
			}
			if !found || !reflect.DeepEqual(through, *tc.want) {
				t.Fatalf("podProcess = %+v (found %t), want %+v", through, found, *tc.want)
			}
			got := podKnown(through)
			if got.Claim != tc.want.Token || got.Locator == nil || !reflect.DeepEqual(*got.Locator, tc.at) {
				t.Fatalf("podKnown = %+v, want %s through %+v", got, tc.want.Token, tc.at)
			}
			if err := got.Validate(); err != nil {
				t.Fatalf("podKnown answered a process no runtime takes: %v", err)
			}
		})
	}
}

// The implementer is the issue's only claim, with no session, queued for its first launch, and the
// runtime refuses that launch for want of the pod's round, so its launch waits on the pod and no
// role process runs there. The sweep opens the round over that pod and posts it to the
// implementer as a way to the pod (postedClaims). Before the sweep replaces the pod through it,
// the implementer's own retry runs. The Sandbox runtime asks the daemon whether the round over a
// live pod is over before it refuses a launch for the identity (relaunch.go asks RoundPodEnds
// before dispatchCredential), the daemon answers yes (roundPodEndsAtLaunch), and the launch
// replaces the pod as the round's own replacement and starts the role in a new pod. The round then
// ends as a launch's replacement (endRoundALaunchReplaced).
//
// The implementer never had a worker in the round's pod: no agent of it registered a session
// there, so its machine's set of registered pods (supervise.Claim.RegisteredPods) does not hold
// that pod and workOnRoundPod answers no. Nothing of it was on that pod's disk, and the issue
// is written nothing about it, as where the sweep replaces the pod through the same claim
// (TestPodUpdateNamesWhatTheRoundsPodTookAtEachEndOfEachMember, its row where the sweep replaces
// the pod through a waiting claim that never registered there). What it records at the
// end, a process in the new pod and no wait on the round's pod, decides nothing.
func TestPodUpdateNamesNothingForAClaimWithNoSessionWhoseOwnRetryReplacesThePod(t *testing.T) {
	t.Parallel()
	rig := newPodUpdateRig(t, podUpdateRole{role: claim.RoleImplementer, state: supervise.StateQueued})
	token := rig.tokens[claim.RoleImplementer]
	rig.drifts(podUpdateExchangeDrift)
	// The runtime refuses each start of the implementer for the identity until the retry is let
	// through to the fake's own launch, which asks RoundPodEnds of the live pod first, as the
	// Sandbox runtime does.
	var letThrough atomic.Bool
	rig.rt.OnSpawn = func(context.Context) error {
		if letThrough.Load() || len(rig.rt.CallsOf("ReplacePod")) > 0 {
			return nil
		}
		spawns := rig.rt.CallsOf("Spawn")
		spec := spawns[len(spawns)-1].Spec
		if spec.Claim != token {
			return nil
		}
		return fmt.Errorf("launch %s: %w", token, &runtime.AwaitingPodRound{
			Pod:    awaitedLocator(token, podUpdatePod, spec.Generation),
			Reason: "pod legion-legion-484 carries no exchange identity and dispatch_token_transition_file is not set; its update round replaces it",
		})
	}
	m, ok := rig.sup.Machine(token)
	if !ok {
		t.Fatal("the implementer is not supervised")
	}
	if _, err := m.ReleaseUncertainLaunch(rig.ctx); err != nil {
		t.Fatalf("release the implementer's unfinished launch: %v", err)
	}
	if err := m.Handle(rig.ctx, supervise.RequestSpawn{Claim: token}); !errors.Is(err, runtime.ErrAwaitingPodRound) {
		t.Fatalf("the implementer's first launch answered %v, want the runtime's wait for its pod's round", err)
	}
	if c := rig.claimOf(claim.RoleImplementer); c.Session != "" || c.Locator != nil || podOf(c.AwaitsPod) != podUpdatePod {
		t.Fatalf("the refused implementer has session %q and process %+v, waiting on %+v; want no session and no process, waiting on pod %s",
			c.Session, c.Locator, c.AwaitsPod, podUpdatePod)
	}

	rig.tick()
	rig.awaitRound(claim.RoleImplementer, podUpdateRound)
	if calls := rig.replacements(); len(calls) != 0 {
		t.Fatalf("the sweep that opened the round replaced the pod through the implementer %+v, want no replacement before the next sweep: the retry is to come first", calls)
	}

	// The retry reaches the fake's own launch into the round's pod, which asks the daemon's hook
	// (roundPodEndsAtLaunch) before it keeps the pod.
	conn := fake.NewConn()
	rig.conns.Register(token, conn)
	rig.workers[claim.RoleImplementer] = conn
	rig.rt.ScriptSpawn(fake.SpawnResult{Locator: *podUpdateLocator(token, podUpdateIssue, podUpdatePod)})
	letThrough.Store(true)
	rig.retryLaunch()
	if atLaunch := rig.rt.CallsOf("ReplaceRoundPodAtLaunch"); len(atLaunch) != 1 {
		t.Fatalf("the retry replaced the round's pod %d times %+v, want once", len(atLaunch), atLaunch)
	}
	c := rig.awaitClaim(claim.RoleImplementer, "its retry to record a process", func(c supervise.Claim) bool { return c.Locator != nil })
	if pod := podOf(c.Locator); pod == "" || pod == podUpdatePod {
		t.Fatalf("the implementer came up in pod %q, want a pod other than the round's %s", pod, podUpdatePod)
	}

	rig.awaitSwept(claim.RoleImplementer, "its round to end", func(c supervise.Claim) bool { return c.UpdateRound == "" })
	rig.tick()
	rig.tick()
	rig.settleAll()
	if calls := rig.replacements(); len(calls) != 0 {
		t.Errorf("ReplacePod called %+v, want the retry's replacement alone", calls)
	}
	lines := loggedLines(t, rig.logs.String(), podUpdateReplacedLine)
	if len(lines) != 1 || lines[0]["atLaunch"] != true {
		t.Fatalf("the replacement was logged %+v, want one line of a replacement at a launch", lines)
	}
	wantField(t, lines[0], "unansweredMembers")
	if messages := rig.messages(); len(messages) != 0 {
		t.Errorf("the issue was written %q, want nothing: the implementer never had a worker in the round's pod", messages)
	}
}

// Two claims of the issue, the implementer and the tester, both with no session and queued for
// their first launch, are refused for want of the pod's round, so both launches wait on the pod
// and no role process runs there. The round is posted to both as a way to the pod (postedClaims).
// The implementer's retry replaces the round's pod (roundPodEndsAtLaunch). Then the tester's retry
// runs, before the sweep that ends the round: the round's pod is gone, so its launch comes up in
// the pod that replaced it. The round ends as a launch's replacement (endRoundALaunchReplaced).
//
// Neither claim ever had a worker in the round's pod: no agent of either registered a session
// there, so workOnRoundPod answers no for both, and the issue is written nothing about
// either. By the end both record a process in the new pod, which decides nothing.
func TestPodUpdateNamesNothingForAnotherClaimWithNoSessionWhoseRetryFollowsTheReplacement(t *testing.T) {
	t.Parallel()
	rig := newPodUpdateRig(t,
		podUpdateRole{role: claim.RoleImplementer, state: supervise.StateQueued},
		podUpdateRole{role: claim.RoleTester, state: supervise.StateQueued},
	)
	implementer, tester := rig.tokens[claim.RoleImplementer], rig.tokens[claim.RoleTester]
	rig.drifts(podUpdateExchangeDrift)
	// The runtime refuses each start of either claim for the identity until the test lets that
	// claim's retry through to the fake's own launch, which asks RoundPodEnds of a live pod first
	// and builds a new pod for a launch into one that was replaced, as the Sandbox runtime does.
	var letImplementer, letTester atomic.Bool
	rig.rt.OnSpawn = func(context.Context) error {
		spawns := rig.rt.CallsOf("Spawn")
		spec := spawns[len(spawns)-1].Spec
		switch {
		case spec.Claim == implementer && letImplementer.Load(), spec.Claim == tester && letTester.Load():
			return nil
		case spec.Claim != implementer && spec.Claim != tester:
			return nil
		}
		return fmt.Errorf("launch %s: %w", spec.Claim, &runtime.AwaitingPodRound{
			Pod:    awaitedLocator(spec.Claim, podUpdatePod, spec.Generation),
			Reason: "pod legion-legion-484 carries no exchange identity and dispatch_token_transition_file is not set; its update round replaces it",
		})
	}
	for _, role := range []claim.Role{claim.RoleImplementer, claim.RoleTester} {
		token := rig.tokens[role]
		m, ok := rig.sup.Machine(token)
		if !ok {
			t.Fatalf("the %s is not supervised", role)
		}
		if _, err := m.ReleaseUncertainLaunch(rig.ctx); err != nil {
			t.Fatalf("release the %s's unfinished launch: %v", role, err)
		}
		if err := m.Handle(rig.ctx, supervise.RequestSpawn{Claim: token}); !errors.Is(err, runtime.ErrAwaitingPodRound) {
			t.Fatalf("the %s's first launch answered %v, want the runtime's wait for its pod's round", role, err)
		}
		if c := rig.claimOf(role); c.Session != "" || c.Locator != nil || podOf(c.AwaitsPod) != podUpdatePod {
			t.Fatalf("the refused %s has session %q and process %+v, waiting on %+v; want no session and no process, waiting on pod %s",
				role, c.Session, c.Locator, c.AwaitsPod, podUpdatePod)
		}
	}

	rig.tick()
	rig.awaitRound(claim.RoleImplementer, podUpdateRound)
	rig.awaitRound(claim.RoleTester, podUpdateRound)
	if calls := rig.replacements(); len(calls) != 0 {
		t.Fatalf("the sweep that opened the round replaced the pod %+v, want no replacement before the next sweep: the retries are to come first", calls)
	}
	for _, role := range []claim.Role{claim.RoleImplementer, claim.RoleTester} {
		conn := fake.NewConn()
		rig.conns.Register(rig.tokens[role], conn)
		rig.workers[role] = conn
	}

	// The implementer's retry replaces the round's pod; the tester's, refused again, waits.
	rig.rt.ScriptSpawn(fake.SpawnResult{Locator: *podUpdateLocator(implementer, podUpdateIssue, podUpdatePod)})
	letImplementer.Store(true)
	rig.retryLaunch()
	if atLaunch := rig.rt.CallsOf("ReplaceRoundPodAtLaunch"); len(atLaunch) != 1 || atLaunch[0].Spec.Claim != implementer {
		t.Fatalf("the round's pod was replaced at a launch %+v, want once, by the implementer's retry", atLaunch)
	}
	rig.awaitClaim(claim.RoleImplementer, "its retry to record a process", func(c supervise.Claim) bool { return c.Locator != nil })

	// The tester's next retry, before any sweep, comes up in the pod that replaced the round's.
	rig.rt.ScriptSpawn(fake.SpawnResult{Locator: *podUpdateLocator(tester, podUpdateIssue, podUpdatePod)})
	letTester.Store(true)
	rig.retryLaunch()
	c := rig.awaitClaim(claim.RoleTester, "its retry to record a process", func(c supervise.Claim) bool { return c.Locator != nil })
	if pod := podOf(c.Locator); pod == "" || pod == podUpdatePod {
		t.Fatalf("the tester came up in pod %q, want the pod that replaced the round's %s", pod, podUpdatePod)
	}

	rig.awaitSwept(claim.RoleImplementer, "its round to end", func(c supervise.Claim) bool { return c.UpdateRound == "" })
	rig.awaitSwept(claim.RoleTester, "its round to end", func(c supervise.Claim) bool { return c.UpdateRound == "" })
	rig.tick()
	rig.tick()
	rig.settleAll()
	if calls := rig.replacements(); len(calls) != 0 {
		t.Errorf("ReplacePod called %+v, want the implementer's replacement at its launch alone", calls)
	}
	lines := loggedLines(t, rig.logs.String(), podUpdateReplacedLine)
	if len(lines) != 1 || lines[0]["atLaunch"] != true {
		t.Fatalf("the replacement was logged %+v, want one line of a replacement at a launch", lines)
	}
	wantField(t, lines[0], "unansweredMembers")
	if messages := rig.messages(); len(messages) != 0 {
		t.Errorf("the issue was written %q, want nothing: neither claim ever had a worker in the round's pod", messages)
	}
}

// The tester runs in the round's pod with a session. The implementer, with no session and queued
// for its first launch, is refused for want of the pod's round, so its launch waits on that pod.
// The round opens, asks the tester and is posted to the implementer as a way to the pod. Before
// the tester answers, the pod dies for a failure of its own, with no daemon restart: the tester is
// relaunched by its session into the pod that replaced it, and the implementer's next retry comes
// up there too. The sweep ends the round as one whose pod went (endRoundWhosePodWent,
// podWentWithoutItsRound).
//
// An agent of the tester registered a session in the round's pod and the tester had not
// answered, so the message names it (workOnRoundPod). No agent of the implementer
// registered there: it was refused for the round's pod while it had no session, so the message
// names it nothing, although by the end it records a process in the pod that replaced the round's
// (docs/kubernetes.md: every end of a round names a role that had not answered only where that
// role's unpushed work may be on the round's pod's disk).
func TestPodUpdateNamesNoClaimRefusedForThePodWithNoSessionWhenThePodDies(t *testing.T) {
	t.Parallel()
	rig := newPodUpdateRig(t,
		podUpdateRole{role: claim.RoleImplementer, state: supervise.StateQueued},
		readyWorker(claim.RoleTester),
	)
	implementer, tester := rig.tokens[claim.RoleImplementer], rig.tokens[claim.RoleTester]
	rig.drifts(podUpdateExchangeDrift)
	// The runtime refuses each start of the implementer for the identity until the test lets its
	// retry through, once the round's pod has gone.
	var letImplementer atomic.Bool
	rig.rt.OnSpawn = func(context.Context) error {
		spawns := rig.rt.CallsOf("Spawn")
		spec := spawns[len(spawns)-1].Spec
		if spec.Claim != implementer || letImplementer.Load() {
			return nil
		}
		return fmt.Errorf("launch %s: %w", spec.Claim, &runtime.AwaitingPodRound{
			Pod:    awaitedLocator(spec.Claim, podUpdatePod, spec.Generation),
			Reason: "pod legion-legion-484 carries no exchange identity and dispatch_token_transition_file is not set; its update round replaces it",
		})
	}
	m, ok := rig.sup.Machine(implementer)
	if !ok {
		t.Fatal("the implementer is not supervised")
	}
	if _, err := m.ReleaseUncertainLaunch(rig.ctx); err != nil {
		t.Fatalf("release the implementer's unfinished launch: %v", err)
	}
	if err := m.Handle(rig.ctx, supervise.RequestSpawn{Claim: implementer}); !errors.Is(err, runtime.ErrAwaitingPodRound) {
		t.Fatalf("the implementer's first launch answered %v, want the runtime's wait for its pod's round", err)
	}
	if c := rig.claimOf(claim.RoleImplementer); c.Session != "" || c.Locator != nil || podOf(c.AwaitsPod) != podUpdatePod {
		t.Fatalf("the refused implementer has session %q and process %+v, waiting on %+v; want no session and no process, waiting on pod %s",
			c.Session, c.Locator, c.AwaitsPod, podUpdatePod)
	}
	rig.tick()
	rig.awaitAsked(claim.RoleTester, podUpdateRound)
	rig.awaitRound(claim.RoleImplementer, podUpdateRound)

	// The pod dies: the tester is relaunched by its session into the pod in its place.
	ended := rig.claimOf(claim.RoleTester).Locator
	if ended == nil {
		t.Fatal("the tester records no process to end with its pod")
	}
	testerConn := fake.NewConn()
	rig.conns.Register(tester, testerConn)
	rig.workers[claim.RoleTester] = testerConn
	rig.rt.ScriptResume(fake.SpawnResult{Locator: *podUpdateLocator(tester, podUpdateIssue, podUpdateReplacementPod)})
	rig.handle(claim.RoleTester, supervise.RuntimeObservation{Observation: runtime.Observation{
		Locator: *ended, Kind: runtime.Gone, At: rig.clock.Now(), Displaced: true,
	}})
	rig.awaitClaim(claim.RoleTester, "its relaunch into the new pod", func(c supervise.Claim) bool { return podOf(c.Locator) == podUpdateReplacementPod })

	// The implementer's next retry comes up in the new pod too.
	implementerConn := fake.NewConn()
	rig.conns.Register(implementer, implementerConn)
	rig.workers[claim.RoleImplementer] = implementerConn
	rig.rt.ScriptSpawn(fake.SpawnResult{Locator: *podUpdateLocator(implementer, podUpdateIssue, podUpdateReplacementPod)})
	letImplementer.Store(true)
	rig.retryLaunch()
	if c := rig.awaitClaim(claim.RoleImplementer, "its retry to record a process", func(c supervise.Claim) bool { return c.Locator != nil }); podOf(c.Locator) != podUpdateReplacementPod {
		t.Fatalf("the implementer came up in pod %q, want the pod that replaced the round's, %s", podOf(c.Locator), podUpdateReplacementPod)
	}
	// The pod both relaunches came up in was built from the configuration the runtime has now.
	rig.rt.ScriptPodDrift(implementer)
	rig.rt.ScriptPodDrift(tester)

	rig.tick()
	rig.tick()
	rig.settleAll()
	if calls := rig.replacements(); len(calls) != 0 {
		t.Errorf("ReplacePod called %+v, want no replacement: the round's pod went on its own", calls)
	}
	messages := rig.messages()
	if len(messages) != 1 || !strings.Contains(messages[0], "The pod that ran this issue's roles is gone") ||
		!strings.Contains(messages[0], "tester (") {
		t.Fatalf("the issue was written %q, want one message that its pod went, naming the tester, whose worker ran in the round's pod", messages)
	}
	if strings.Contains(messages[0], string(claim.RoleImplementer)) {
		t.Errorf("the message names the implementer, which was refused for the round's pod with no session and never had a worker there:\n%s", messages[0])
	}
}
