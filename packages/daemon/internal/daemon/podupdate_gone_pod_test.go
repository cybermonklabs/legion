package daemon

// A round over a pod its claims no longer record, as dispatch://INFRA-1583's implementer claim held
// one from 2026-11-12 (dispatch://INFRA-3276, R5). Round `pod:0a389176…` was ended by its own
// replacement, which posted the end to every member's machine. Four machines took it; the
// implementer's was in a relaunch, the daemon stopped before it took the post, and the post went
// with the daemon, since it lives in memory alone. The boot restored the round from the store,
// every claim came to record the pod that replaced the round's own, and that pod still drifted, so
// the sweep read the round as one to carry on. No answer could end it, it held the implementer's
// next task, and it held the one open-round slot `pod_update_max_open_rounds` gave the project,
// for 13 hours, until a person cleared it by hand.
//
// The other fault of this file is the same end made again at every sweep (dispatch://INFRA-4109
// R6). On OSS Verify's daemon on 2026-11-18, OSSV-522's round replaced its pod at 07:13:36Z. The
// implementer's machine was in a relaunch, and it takes the events posted to it only once that
// relaunch is done, so its claim kept the round. Every sweep from 07:14:31Z to 07:24:31Z decided
// the round again and posted the end again, 11 times, and the implementer took the 11 ends at
// once at 07:25:09Z.
//
// The first test uses nothing INFRA-3276's change adds: laid over the commit before that change,
// it fails there, because the round stays open after the sweep and the issue waiting for the slot
// stays held. The tests of R6 fail over the commit before R6's change: there each sweep decides
// the ended round again, posts its end again, writes on the issue again, and counts a slot for it.

import (
	"context"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/runtime/fake"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/testwait"
)

// The round's pod went, the claims record the pod that replaced it, and that pod has drifted: one
// sweep ends the round on the one member the earlier end never reached, in its machine and in the
// store, and the task the round was holding back from that member goes to it. The round held no
// slot at that sweep, and the next sweep finds the issue that was waiting for the slot in a round
// of its own, with the issue whose round ended waiting behind it for its own next round, over the
// pod in its place.
func TestPodUpdateEndsARoundOverAPodItsClaimsNoLongerRecord(t *testing.T) {
	t.Parallel()
	const (
		waiting    = "LEGION-485"
		waitingPod = "2e7c9a41-6b0d-4f58-a3c2-91d4e8b07f63"
		held       = "outbox:7"
	)
	rig := newPodUpdateRig(t,
		// The member the end never reached: it answered the round before the round's own
		// replacement, and its relaunch came up in the pod that replaced the round's own, which
		// it records, leaving the round as that pod left it (supervise's leftPodRoundAt).
		podUpdateRole{role: claim.RoleImplementer, state: supervise.StateReady, session: true, contract: api.DaemonAPIVersion(),
			pod: podUpdateReplacementPod, round: podUpdateRound, reasons: podUpdateDrift, signalled: true, left: podUpdateRound},
		// The members the end did reach: in no round, and in that same pod.
		podUpdateRole{role: claim.RoleTester, state: supervise.StateReady, session: true, contract: api.DaemonAPIVersion(),
			pod: podUpdateReplacementPod},
		podUpdateRole{role: claim.RoleReviewer, state: supervise.StateReady, session: true, contract: api.DaemonAPIVersion(),
			pod: podUpdateReplacementPod},
	)
	// The pod in the place of the round's own was built before the change the round was opened
	// for, so it has drifted as that pod had.
	rig.drifts(podUpdateDrift)
	rig.s.cfg.PodUpdateMaxOpenRounds = new(1)
	waitingToken := rig.driftingIssue(waiting, waitingPod)

	// The implementer's next phase task, which the round it answered holds back from it.
	const task = "Carry the implementation on."
	implementer := rig.tokens[claim.RoleImplementer]
	rig.handle(claim.RoleImplementer, supervise.RequestDeliver{Claim: implementer, ID: held,
		Task: task, Phase: phase.Implementing, Generation: 1})
	rig.settle(claim.RoleImplementer)
	if answer := rig.take(claim.RoleImplementer); answer.Outcome != supervise.TakeOutcomeRefused || answer.Hold != supervise.HoldUpdateRound {
		t.Fatalf("the implementer's take while it stood in the round it answered = %+v, want it refused by %q", answer, supervise.HoldUpdateRound)
	}

	rig.tick()

	// The one sweep ends the round: the end reaches the machine through its own queue, and the
	// machine writes the claim before it decides anything else.
	rig.awaitClaim(claim.RoleImplementer, "its round over the pod that went to end at the sweep that read the claims recording another pod",
		func(c supervise.Claim) bool { return c.UpdateRound == "" })
	stored, err := rig.sup.Claims(rig.ctx)
	if err != nil {
		t.Fatalf("read the claims the store holds: %v", err)
	}
	for _, c := range stored {
		if c.Issue == podUpdateIssue && c.UpdateRound != "" {
			t.Errorf("the store holds the %s in round %q after the sweep, want no claim of %s in a round",
				c.Role, c.UpdateRound, podUpdateIssue)
		}
	}
	rig.settle(claim.RoleImplementer)
	if answer := rig.take(claim.RoleImplementer); answer.Outcome != supervise.TakeOutcomeAccepted || !strings.HasSuffix(answer.Text, task) {
		t.Fatalf("the implementer's take once its round ended = %+v, want the task the round held back accepted", answer)
	}
	if messages := rig.messages(); len(messages) != 0 {
		t.Errorf("the issue was written %q; the one member of the round had answered before its pod went", messages)
	}
	if calls := rig.replacements(); len(calls) != 0 {
		t.Errorf("ReplacePod called %d times for a pod that is already gone", len(calls))
	}

	rig.tick()

	testwait.Eventually(t, waiting+" to hold the round over its own pod", func() bool {
		return rig.machineClaim(waitingToken).UpdateRound == supervise.PodRound(waitingPod)
	})
	rounds := rig.s.podUpdateRoundsView()
	if !slices.Equal(rounds.InRound, []string{waiting}) || !slices.Equal(rounds.WaitingOnMaxOpenRounds, []string{podUpdateIssue}) {
		t.Errorf("the next sweep recorded in a round %v and waiting on the open-round limit %v, want %s in its round and %s waiting for its own over the pod in its place",
			rounds.InRound, rounds.WaitingOnMaxOpenRounds, waiting, podUpdateIssue)
	}
}

// OSSV-522's round, as the rig holds it (dispatch://INFRA-4109 R6). Every member of the round has
// answered, and the round replaces its pod. The pod's processes end with it, and the tester's
// machine is relaunching its process when the sweep posts the end, so that end waits behind the
// relaunch and the tester's claim keeps the round. The implementer's claim still records the pod
// that is gone, as every claim records it until its process runs again.
//
// Over the five sweeps after the replacement, the sweep decides the round no more: the
// replacement is the one end it logs, and each member's machine takes one end, the tester's once
// its relaunch is done. From the sweep after the replacement the round holds no open-round slot,
// so the issue that the project's one slot held back opens its round at that sweep. A sweep that
// read the round again would end it again at each of the five sweeps, post five more ends to the
// tester, and keep the slot for a round it has ended.
func TestARoundOverAGonePodIsDecidedOnce(t *testing.T) {
	t.Parallel()
	const (
		waiting    = "LEGION-485"
		waitingPod = "2e7c9a41-6b0d-4f58-a3c2-91d4e8b07f63"
	)
	roles := []claim.Role{claim.RoleImplementer, claim.RoleTester}
	rig, ends := newPodUpdateRigCountingEnds(t, readyWorker(claim.RoleImplementer), readyWorker(claim.RoleTester))
	rig.drifts(podUpdateDrift)
	rig.s.cfg.PodUpdateMaxOpenRounds = new(1)
	waitingToken := rig.driftingIssue(waiting, waitingPod)

	rig.tick()
	for _, role := range roles {
		rig.awaitRound(role, podUpdateRound)
		rig.signal(role, "a1b2c3d")
	}
	rig.settleAll()

	// The replacement ends every process of the pod: the pod is gone once ReplacePod returns, and
	// the tester's machine is relaunching its process when the sweep posts the end.
	var release func()
	rig.s.runtime = &podReplacedThen{Runtime: rig.rt, then: func() {
		rig.podGone()
		release = rig.holdRelaunch(claim.RoleTester, true, podUpdateReplacementPod)
	}}
	rig.awaitReplacement(1, "once every worker of the round answered")
	if release == nil {
		t.Fatal("the replacement was made without the relaunch it holds")
	}
	rig.awaitClaim(claim.RoleImplementer, "the end to reach the implementer",
		func(c supervise.Claim) bool { return c.UpdateRound == "" })

	for sweep := 1; sweep <= 5; sweep++ {
		rig.tick()
		if sweep == 1 {
			testwait.Eventually(t, waiting+" to open its round in the slot at the sweep after the replacement", func() bool {
				return rig.machineClaim(waitingToken).UpdateRound == supervise.PodRound(waitingPod)
			})
		}
		if rounds := rig.s.podUpdateRoundsView(); slices.Contains(rounds.InRound, podUpdateIssue) {
			t.Errorf("sweep %d after the replacement recorded %s in a round (%v), want it in none: the sweep ended its round",
				sweep, podUpdateIssue, rounds.InRound)
		}
	}
	if round := rig.storedRound(claim.RoleTester); round != podUpdateRound {
		t.Fatalf("the tester's claim records round %q after the five sweeps, want %s: its machine is relaunching and has not taken the end",
			round, podUpdateRound)
	}
	if n := rig.endsLogged(podUpdateRound); n != 1 {
		t.Errorf("the sweep logged the end of the round %d times, want once", n)
	}
	if lines := loggedLines(t, rig.logs.String(), podUpdateReplacedLine); len(lines) != 1 {
		t.Errorf("the sweep logged the replacement %d times, want once: %+v", len(lines), lines)
	}
	if calls := rig.replacements(); len(calls) != 1 {
		t.Errorf("ReplacePod called %d times, want once", len(calls))
	}

	release()
	for _, role := range roles {
		rig.awaitPosted(role)
		if c := rig.claimOf(role); c.UpdateRound != "" {
			t.Errorf("the %s is in round %q once its machine took what was posted to it, want none", role, c.UpdateRound)
		}
		if n := ends.of(rig.tokens[role]); n != 1 {
			t.Errorf("the %s's machine took %d ends of the round, want one", role, n)
		}
	}
}

// The same round with a member that had not answered (dispatch://INFRA-4109 R6). The pod goes
// for a failure of its own before the implementer answers, so what the implementer had not pushed
// went with the disk, and the implementer's machine is relaunching its process. The first sweep
// that reads the pod gone writes one message on the issue and ends the round
// (podWentWithoutItsRound). The implementer's claim keeps the round until its relaunch is done.
// Over five sweeps the issue is told once: a sweep that read the round again would write the
// message again at each sweep.
func TestARoundOverAGonePodThatLostWorkWritesOnce(t *testing.T) {
	t.Parallel()
	rig := newPodUpdateRig(t, readyWorker(claim.RoleImplementer), readyWorker(claim.RoleTester))
	rig.drifts(podUpdateDrift)
	rig.tick()
	rig.awaitRound(claim.RoleImplementer, podUpdateRound)
	rig.awaitRound(claim.RoleTester, podUpdateRound)
	rig.signal(claim.RoleTester, "a1b2c3d")
	rig.settleAll()

	release := rig.holdRelaunch(claim.RoleImplementer, false, podUpdateReplacementPod)
	rig.podGone()
	for range 5 {
		rig.tick()
	}

	if round := rig.storedRound(claim.RoleImplementer); round != podUpdateRound {
		t.Fatalf("the implementer's claim records round %q after the five sweeps, want %s: its machine is relaunching and has not taken the end",
			round, podUpdateRound)
	}
	messages := rig.messages()
	if len(messages) != 1 {
		t.Fatalf("the issue was written on %d times over five sweeps, want once for the pod that went: %q", len(messages), messages)
	}
	if !strings.Contains(messages[0], string(claim.RoleImplementer)) || strings.Contains(messages[0], string(claim.RoleTester)) {
		t.Errorf("the message does not name the implementer alone, the one member that had not answered: %s", messages[0])
	}

	release()
	rig.awaitPosted(claim.RoleImplementer)
	if c := rig.claimOf(claim.RoleImplementer); c.UpdateRound != "" {
		t.Errorf("the implementer is in round %q once its machine took what was posted to it, want none", c.UpdateRound)
	}
}

// A restart forgets the rounds the sweep has ended (podUpdateEnded). The post that carries an end
// lives in the daemon's memory, so a daemon that stops before a machine takes the post loses it,
// and the store keeps the round on that claim for the next boot to restore (dispatch://INFRA-3276
// R5). The daemon that boots ends the round once more, and then not again
// (dispatch://INFRA-4109 R6).
//
// Here the round's pod goes with every member answered, and the first daemon ends the round once.
// The daemon stops while the tester's machine is relaunching, before that machine takes the end.
// The tester comes back in the pod in the gone pod's place, still in the round, and the new
// daemon's first sweep ends the round on it.
func TestARoundOverAGonePodIsEndedAgainAfterARestart(t *testing.T) {
	t.Parallel()
	const overAPodInItsPlace = podWentOverLine
	roles := []claim.Role{claim.RoleImplementer, claim.RoleTester}
	rig, ends := newPodUpdateRigCountingEnds(t, readyWorker(claim.RoleImplementer), readyWorker(claim.RoleTester))
	rig.drifts(podUpdateDrift)
	rig.tick()
	for _, role := range roles {
		rig.awaitRound(role, podUpdateRound)
		rig.signal(role, "a1b2c3d")
	}
	rig.settleAll()

	release := rig.holdRelaunch(claim.RoleTester, false, podUpdateReplacementPod)
	rig.podGone()
	rig.tick()
	rig.awaitClaim(claim.RoleImplementer, "the end to reach the implementer",
		func(c supervise.Claim) bool { return c.UpdateRound == "" })
	rig.tick()
	if n := rig.endsLogged(podUpdateRound); n != 1 {
		t.Fatalf("the first daemon logged the end of the round %d times, want once", n)
	}

	// The daemon stops with the tester's end still queued behind its relaunch, and the end goes
	// with the daemon. Each role then runs in the pod in the gone pod's place.
	rig.sup.halt()
	release()
	rig.displacedInto(claim.RoleImplementer, podUpdateReplacementPod)
	if n := ends.of(rig.tokens[claim.RoleTester]); n != 0 {
		t.Fatalf("the tester's machine took %d ends before the restart, want none", n)
	}

	next := rig.restart()
	next.s.log = rig.log
	if round := next.storedRound(claim.RoleTester); round != podUpdateRound {
		t.Fatalf("the tester's claim records round %q at the boot, want %s: the store keeps the round the end never reached", round, podUpdateRound)
	}

	next.tick()
	next.awaitPosted(claim.RoleTester)
	if c := next.claimOf(claim.RoleTester); c.UpdateRound != "" {
		t.Fatalf("the tester is in round %q after the new daemon's first sweep, want none: that daemon ends the round once more", c.UpdateRound)
	}
	for range 3 {
		next.tick()
	}
	next.awaitPosted(claim.RoleTester)

	if n := ends.of(rig.tokens[claim.RoleTester]); n != 1 {
		t.Errorf("the tester's machine took %d ends of the round, want the one the new daemon made", n)
	}
	if lines := loggedLines(t, rig.logs.String(), overAPodInItsPlace); len(lines) != 1 {
		t.Errorf("the new daemon logged the end of the round over the pod in its place %d times, want once: %+v", len(lines), lines)
	}
	if n := next.endsLogged(podUpdateRound); n != 2 {
		t.Errorf("the two daemons logged the end of the round %d times, want once each", n)
	}
}

// driftingIssue seeds an issue beside the rig's own: a tree of its own, with one ready
// implementer in pod, a pod the runtime answers drifted, supervised by the rig's daemon as a boot
// restores a claim. It is the issue the project's open-round limit holds back while another holds
// the one slot, and it answers its implementer's claim. The rig's own drifts scripts the rig's
// issue alone, so this issue stays drifted whatever a test scripts of that one.
func (r *podUpdateRig) driftingIssue(key, pod string) claim.Token {
	r.t.Helper()
	token := mustClaimToken(r.t, key, claim.RoleImplementer)
	seedWorkflowTree(r.t, r.pool.Config().ConnString(), podUpdateProject, record.Issue{
		Key: key, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets",
		Project: "LEGION", Tree: key, Title: "The issue a limit holds back", Phase: phase.Implementing,
		Generation: 1, Status: "in_progress", HandedOver: true, Rank: "U",
	}, supervise.Claim{
		Token: token, Project: podUpdateProject, Tree: key, Issue: key, Role: claim.RoleImplementer,
		Generation: 1, State: supervise.StateReady, PluginContract: api.DaemonAPIVersion(),
		Session: "ses_" + key, SessionFile: "/sessions/" + key + ".jsonl",
		Locator: podUpdateLocator(token, key, pod),
	})
	stored, err := r.sup.Claims(r.ctx)
	if err != nil {
		r.t.Fatalf("read the claims of %s: %v", key, err)
	}
	seeded := slices.DeleteFunc(stored, func(c supervise.Claim) bool { return c.Token != token })
	if len(seeded) != 1 {
		r.t.Fatalf("the store holds %d claims of %s's implementer, want the one seeded", len(seeded), key)
	}
	if _, err := r.sup.restore(r.ctx, seeded); err != nil {
		r.t.Fatalf("restore the claim of %s: %v", key, err)
	}
	r.conns.Register(token, fake.NewConn())
	r.rt.ScriptPodDrift(token, podUpdateDrift)
	return token
}

// machineClaim is the claim of token as its machine holds it, for a claim of an issue the rig's
// roles do not name (driftingIssue).
func (r *podUpdateRig) machineClaim(token claim.Token) supervise.Claim {
	r.t.Helper()
	m, ok := r.sup.Machine(token)
	if !ok {
		r.t.Fatalf("%s is not supervised", token)
	}
	return m.Claim()
}

// podUpdateEnds counts, by claim, the round ends each machine's feed handed it to decide
// (supervise.PodUpdateEnded, podUpdateRigOptions.beforeDecision). It counts each end the machine
// took, the ones that found the claim in no round included, and no end that went with a stopped
// daemon.
type podUpdateEnds struct {
	mu    sync.Mutex
	taken map[claim.Token]int
}

func (e *podUpdateEnds) count(token claim.Token, ev supervise.Event) {
	if _, ended := ev.(supervise.PodUpdateEnded); !ended {
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	e.taken[token]++
}

// of is how many ends token's machines took, over every daemon of the rig (restart).
func (e *podUpdateEnds) of(token claim.Token) int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.taken[token]
}

// newPodUpdateRigCountingEnds is newPodUpdateRig with the ends each machine takes counted.
func newPodUpdateRigCountingEnds(t *testing.T, roles ...podUpdateRole) (*podUpdateRig, *podUpdateEnds) {
	t.Helper()
	ends := &podUpdateEnds{taken: map[claim.Token]int{}}
	return newPodUpdateRigWith(t, podUpdateRigOptions{beforeDecision: ends.count}, roles...), ends
}

// podUpdateEndLines is the line the sweep logs as it ends a round, for each way a round ends: its
// own replacement, its pod gone with another pod recorded in its place, its pod read gone with
// every member answered, its pod gone with a member that had not answered and ran a worker in it,
// and its pod gone where each member that had not answered ran no worker in it.
var podUpdateEndLines = []string{
	podUpdateReplacedLine,
	podWentOverLine,
	podNotThereOverLine,
	podWentLostLine,
	podWentNoWorkerLine,
}

// endsLogged is how many times the sweep logged the end of round on the rig's issue, by any of
// the ways a round ends (podUpdateEndLines).
func (r *podUpdateRig) endsLogged(round string) int {
	r.t.Helper()
	ended := 0
	for _, line := range podUpdateEndLines {
		for _, logged := range loggedLines(r.t, r.logs.String(), line) {
			if logged["issue"] == podUpdateIssue && logged["round"] == round {
				ended++
			}
		}
	}
	return ended
}

// storedRound is the round role's claim records in the store, which is what a sweep reads.
func (r *podUpdateRig) storedRound(role claim.Role) string {
	r.t.Helper()
	for _, c := range r.snapshot() {
		if c.Token == r.tokens[role] {
			return c.UpdateRound
		}
	}
	r.t.Fatalf("the store holds no claim of the %s", role)
	return ""
}

// holdRelaunch ends role's process and holds the relaunch that follows inside Resume, as a
// relaunch into a pod being built holds a machine for minutes (holdNextResume). The observation
// goes on the claim's own queue, as the runtime's observations do, so every event posted to the
// claim after it waits behind the relaunch, the round's end among them. displaced says the daemon
// replaced the pod the process ran in (runtime.Observation.Displaced), and pod is the pod the
// relaunch comes up in.
//
// It returns once the machine is inside Resume. release lets the Resume go on and registers the
// role in pod, as its new process does. A test that stops before it releases is released at its
// cleanup, ahead of the supervisor's stop, which waits for the held decision.
func (r *podUpdateRig) holdRelaunch(role claim.Role, displaced bool, pod string) (release func()) {
	r.t.Helper()
	token := r.tokens[role]
	ended := r.claimOf(role).Locator
	if ended == nil {
		r.t.Fatalf("the %s records no process to end", role)
	}
	holding, let := r.holdNextResume(role, pod)
	r.sup.post(token, supervise.RuntimeObservation{Observation: runtime.Observation{
		Locator: *ended, Kind: runtime.Gone, At: r.clock.Now(), Detail: "the process ended with its pod", Displaced: displaced,
	}})
	select {
	case <-holding:
	case <-time.After(10 * time.Second):
		r.t.Fatalf("the %s's relaunch did not reach Resume", role)
	}
	return func() {
		r.t.Helper()
		let()
		r.registerInto(role, pod)
	}
}

// awaitPosted waits until role's machine has decided every event posted to it so far, the round's
// ends among them: a mark goes on the claim's queue behind them (supervisor.drained).
func (r *podUpdateRig) awaitPosted(role claim.Role) {
	r.t.Helper()
	done := make(chan struct{})
	r.sup.drained(r.tokens[role], done)
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		r.t.Fatalf("the %s's machine did not decide what was posted to it", role)
	}
}

// podReplacedThen is the rig's runtime with what follows a pod's replacement run inside the
// replacement: ReplacePod returns once the pod is gone (the Sandbox runtime's awaitPodGone), and
// by then every role process of the pod has ended and each machine relaunches its own. then runs
// once, after the fake has made the replacement and before the sweep posts the round's end.
type podReplacedThen struct {
	*fake.Runtime
	then func()
	once sync.Once
}

func (r *podReplacedThen) ReplacePod(ctx context.Context, k runtime.Known, due func(context.Context) error) error {
	if err := r.Runtime.ReplacePod(ctx, k, due); err != nil {
		return err
	}
	r.once.Do(r.then)
	return nil
}
