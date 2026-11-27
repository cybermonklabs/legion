package daemon

// Row H47 of the work hold's plan (decision 17): under `work`, a breaking-drift round passed over
// a stopped worker whose unpushed commit is on the pod's disk. A read that finds work or fails
// keeps the round open and the pod in place until the
// hold opens or the work is pushed. The round holds no task of a claim the hold does not stop
// meanwhile: an answer to the round is told once, on its role's topic, that the round waits for
// the hold, and holds none of its claim's tasks from then on, and the round does not ask it again
// at each sweep. A launch goes into the pod the hold keeps, so the issue's architect goes on there,
// and a released merger woken for the close message gets it there and is stopped 2 minutes after
// its start. Once a read finds the held work pushed, or at the opening, the round asks each claim
// whose answer it lifted again, under a bound of the round's own length from that ask, and replaces
// the pod only once each claim it asks has answered. Under dispatch_token_broker with no
// dispatch_token_transition_file the runtime refuses every launch into such a pod: no role starts,
// the pod is kept, and each launch that begins to wait is told to the operator's side.
//
// The cases drive the work-hold rig of workhold_outbox_test.go, whose close, opening and outbox
// are the daemon's own, with the update-round sweep of supervision bound over its supervisor and
// runtime (heldRoundRig). The texts they read are written out here rather than read from the code,
// so each case reads the same over the code before the change.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/pods"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/runtime/fake"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

const (
	// heldRoundGrace is the round's bound in these cases: longer than every drain of the rig's
	// outbox moves the clock (65 seconds a pass), so no bound decides what a case does not name.
	heldRoundGrace = 24 * time.Hour
	// Spell the marker literally so H47's controls compile before #5285.
	heldAccountDrift  = "[breaks the pod] account default, now legion-worker"
	heldExchangeDrift = "[breaks the pod] agent-secrets-exchange (unset), now audience=legion-broker-exchange expiry=3600s path=token"
	// heldRoundWaits is what a claim whose answer the hold lifted is told on its role's topic.
	heldRoundWaits = "This pod update round waits until the tree's work hold ends. Go on with your work: the daemon asks you again before it replaces the pod."
	// heldRoundAsk is the first sentence of the round's ask (supervise.UpdateTask).
	heldRoundAsk = "The daemon must replace the pod that runs this issue's roles."
	// heldRoundRemedy is the setting the broker state's notice names as its remedy.
	heldRoundRemedy = "dispatch_token_transition_file"
)

// heldRound is the round of the pod every role of the held tree runs in.
var heldRound = supervise.PodRound(podUpdatePod)

// heldRoundRig is a work-hold rig with the update-round sweep bound over its supervisor and
// runtime, as the daemon's boot binds both (openSupervision, plan.launchRounds).
type heldRoundRig struct {
	*workHoldRig
	s    *supervision
	logs *syncBuffer
}

func newHeldRoundRig(t *testing.T, at phase.Phase) *heldRoundRig {
	t.Helper()
	h := &heldRoundRig{workHoldRig: newWorkHoldRig(t, legionTree(at, config.HoldWork)), logs: &syncBuffer{}}
	h.attach()
	return h
}

// attach builds the sweep over the rig's supervisor, runtime and clock, and binds the runtime's
// launches to its reading of the round over a pod (roundPodEndsAtLaunch). A restart builds both
// afresh (restarted), so it attaches again, with nothing of the first sweep's memory.
func (h *heldRoundRig) attach() {
	h.sup.store = h.trees
	w := &workflowRuntime{pool: h.pool, records: h.records, dispatchProject: h.tree.project, log: quietLogger()}
	h.s = &supervision{
		cfg: config.Config{Project: h.tree.project, Runtime: config.Runtime{Name: "kubernetes"},
			PodUpdateGrace: heldRoundGrace, WorkerSilence: podUpdateSilence},
		log: slog.New(slog.NewJSONHandler(h.logs, nil)),
		plan: plan{clock: h.clock, project: h.tokenProject, pluginRelaunchable: true, orphanSweep: orphanSweepInterval,
			enqueue: w.enqueueOutbox},
		runtime: h.rt, supervisor: h.sup, refreshGate: supervise.NewRefreshGate(0),
	}
	h.rt.RoundPodEnds = h.s.roundPodEndsAtLaunch
}

// restarted is a daemon restart (workHoldRig.restart) with the sweep attached again, and the new
// runtime scripted as the pod still reads: drifted, through each of drifted, with held work on
// its disk where unpushed says so.
func (h *heldRoundRig) restarted(unpushed bool, drifted ...claim.Role) {
	h.t.Helper()
	h.restart()
	h.attach()
	for _, role := range drifted {
		h.rt.ScriptPodDrift(h.token(role), heldAccountDrift)
	}
	if unpushed {
		h.unpushed()
	} else {
		h.pushed()
	}
}

// locator is role's process in the held tree's pod, as the Sandbox runtime records it.
func (h *heldRoundRig) locator(role claim.Role) runtime.Locator {
	return *podUpdateLocator(h.token(role), h.tree.key, podUpdatePod)
}

// liveInPod launches role's claim into the tree's pod and makes its agent ready.
func (h *heldRoundRig) liveInPod(role claim.Role) {
	h.t.Helper()
	h.rt.ScriptSpawn(fake.SpawnResult{Locator: h.locator(role)})
	h.live(role)
	h.wantInPod(role)
}

func (h *heldRoundRig) wantInPod(role claim.Role) {
	h.t.Helper()
	if c := h.machine(role).Claim(); podOf(c.Locator) != podUpdatePod {
		h.t.Fatalf("the %s records %+v, want a process in the kept pod %s", role, c.Locator, podUpdatePod)
	}
	if replaced := h.rt.CallsOf("ReplaceRoundPodAtLaunch"); len(replaced) != 0 {
		h.t.Fatalf("a launch replaced the round's pod %+v, want the pod kept for the held work", replaced)
	}
}

func (h *heldRoundRig) unpushed() {
	h.rt.ScriptWorkspaceWork(h.tree.key, runtime.WorkspaceWork{Head: passedOverHead, Commits: []string{passedOverCommit}, Count: 1})
}

func (h *heldRoundRig) pushed() {
	h.rt.ScriptWorkspaceWork(h.tree.key, runtime.WorkspaceWork{Head: passedOverHead})
}

// closeOverAWorkingImplementer has the implementer at work in the pod when the close at version 4
// closes the gate: the close stops it once that turn ends, its session kept and its process gone
// from the pod, and its unpushed commit on the pod's disk.
func (h *heldRoundRig) closeOverAWorkingImplementer() {
	h.t.Helper()
	h.liveInPod(claim.RoleImplementer)
	h.handle(claim.RoleImplementer, supervise.StreamTurnStart{Claim: h.token(claim.RoleImplementer)})
	h.close(4)
	h.drain()
	endTurn(h.ctx, h.t, h.machine(claim.RoleImplementer), h.token(claim.RoleImplementer))
	h.drain()
	h.wantHeld(claim.RoleImplementer, 1, "after the close")
}

// settle waits for every machine of the rig to have handled what the last sweep posted.
func (h *heldRoundRig) settle() {
	h.t.Helper()
	for role := range h.conns {
		if _, ok := h.sup.Machine(h.token(role)); ok {
			awaitDrained(h.t, h.sup, h.token(role), "the sweep's posts")
		}
	}
}

// sweep is one pass of the update-round sweep over the claims the store holds, as the orphan
// sweep's tick makes it, with every post it makes handled.
func (h *heldRoundRig) sweep() {
	h.t.Helper()
	claims, err := h.sup.Claims(h.ctx)
	if err != nil {
		h.t.Fatalf("read the claims for the sweep: %v", err)
	}
	h.s.reconcilePodUpdates(h.ctx, claims)
	h.settle()
}

// runNow runs every outbox row due a second from now.
func (h *heldRoundRig) runNow() {
	h.t.Helper()
	h.clock.advance(time.Second)
	h.runOnce()
}

// opensOver scripts drift through each live role, then sweeps until the round opens.
func (h *heldRoundRig) opensOver(drift string, roles ...claim.Role) {
	h.t.Helper()
	for _, role := range roles {
		h.rt.ScriptPodDrift(h.token(role), drift)
	}
	h.until("the round to open over the pod", func() bool {
		h.sweep()
		return h.machine(roles[0]).Claim().UpdateRound == heldRound
	})
}

// until asks done, which may sweep or run the outbox, up to sixty times a few milliseconds apart,
// and fails the case with the sweep's last lines where it never holds: what held the round is on
// those lines.
func (h *heldRoundRig) until(what string, done func() bool) {
	h.t.Helper()
	for range 60 {
		if done() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	lines := strings.Split(strings.TrimSpace(h.logs.String()), "\n")
	if len(lines) > 8 {
		lines = lines[len(lines)-8:]
	}
	h.t.Fatalf("gave up waiting for %s; the sweep's last lines:\n%s", what, strings.Join(lines, "\n"))
}

// asked sweeps and runs the outbox until the round's ask reached role's session.
func (h *heldRoundRig) asked(role claim.Role) {
	h.t.Helper()
	h.until(fmt.Sprintf("the round's ask to reach the %s", role), func() bool {
		h.sweep()
		h.runNow()
		c := h.machine(role).Claim()
		return c.UpdateRound == heldRound && c.UpdateTaskedAt != nil
	})
}

// answers is role's ready_for_update, and the report its turn's end sends that nothing of its own
// runs (reports).
func (h *heldRoundRig) answers(role claim.Role) {
	h.t.Helper()
	m := h.machine(role)
	m.Wait()
	c := m.Claim()
	if err := m.Handle(h.ctx, supervise.PodUpdateSignalled{Claim: c.Token, Generation: c.Generation, Session: c.Session,
		Round: heldRound, Head: "a1b2c3d"}); err != nil {
		h.t.Fatalf("the %s answered the round: %v", role, err)
	}
	h.reports(role)
}

// reports is role's session reporting that nothing of its own runs, onto its current launch, at
// the rig's clock. The store's narrow call (store.PutClaimActivity) takes no report later than the
// wall clock, and the drains of this rig's outbox move its clock ahead of the wall clock, so the
// report is written as that call writes it, at the rig's moment.
func (h *heldRoundRig) reports(role claim.Role) {
	h.t.Helper()
	c := h.machine(role).Claim()
	tag, err := h.pool.Exec(h.ctx, `update claims set activity_at = $2, activity_background = false, activity_kinds = '',
		activity_next_reminder_at = null where token = $1 and generation = $3`, string(c.Token), h.clock.Now().UTC(), int64(c.Generation))
	if err != nil || tag.RowsAffected() != 1 {
		h.t.Fatalf("report that nothing of the %s runs: %v, %d rows", role, err, tag.RowsAffected())
	}
}

// published is every message role's topic was given that holds text.
func (h *heldRoundRig) publishedHolding(role claim.Role, text string) []string {
	var of []string
	for _, message := range h.published(role) {
		if strings.Contains(message, text) {
			of = append(of, message)
		}
	}
	return of
}

// prompted is whether role's agent was prompted with a message that holds task.
func (h *heldRoundRig) prompted(role claim.Role, task string) bool {
	for _, prompt := range h.conn(role).Prompts() {
		if strings.Contains(prompt.Message, task) {
			return true
		}
	}
	return false
}

// takesTask is role's agent given task, a task sendPending hands over, and that task's turn: it
// starts, ends, and its end reports that nothing of the role's own runs.
func (h *heldRoundRig) takesTask(role claim.Role, task, what string) {
	h.t.Helper()
	h.handle(role, supervise.RequestDeliver{Claim: h.token(role), Task: task})
	h.takesHeldTask(role, task, what)
}

// takesHeldTask is takesTask for a task the claim already holds.
func (h *heldRoundRig) takesHeldTask(role claim.Role, task, what string) {
	h.t.Helper()
	h.until(fmt.Sprintf("the %s to be handed %s", role, what), func() bool {
		h.machine(role).Wait()
		return h.prompted(role, task)
	})
	h.handle(role, supervise.StreamTurnStart{Claim: h.token(role)})
	endTurn(h.ctx, h.t, h.machine(role), h.token(role))
	h.reports(role)
}

func (h *heldRoundRig) wantNotReplaced(when string) {
	h.t.Helper()
	if calls := h.rt.CallsOf("ReplacePod"); len(calls) != 0 {
		h.t.Fatalf("%s the pod was replaced %d times, want none", when, len(calls))
	}
	if calls := h.rt.CallsOf("ReplaceIssuePod"); len(calls) != 0 {
		h.t.Fatalf("%s the pod was replaced by issue %d times, want none", when, len(calls))
	}
}

// awaitReplacedOnce sweeps until the pod is replaced, and holds it to one replacement.
func (h *heldRoundRig) awaitReplacedOnce(what string) {
	h.t.Helper()
	h.until("the pod's replacement "+what, func() bool {
		h.sweep()
		return len(h.rt.CallsOf("ReplacePod")) > 0
	})
	if calls := h.rt.CallsOf("ReplacePod"); len(calls) != 1 {
		h.t.Fatalf("the pod was replaced %d times %s, want once", len(calls), what)
	}
}

// keeps sweeps until the round's replacement keeps the pod for the held work (heldWorkKeepsLine).
func (h *heldRoundRig) keeps(what string) {
	h.t.Helper()
	before := len(loggedLines(h.t, h.logs.String(), heldWorkKeepsLine))
	h.until("a sweep to keep the pod for the held work "+what, func() bool {
		h.sweep()
		h.wantNotReplaced(what)
		return len(loggedLines(h.t, h.logs.String(), heldWorkKeepsLine)) > before
	})
}

// askedAgain sweeps until the round has taken role's answer back to ask it again.
func (h *heldRoundRig) askedAgain(role claim.Role, what string) {
	h.t.Helper()
	h.until(fmt.Sprintf("the round to take the %s's answer back %s", role, what), func() bool {
		h.sweep()
		return h.machine(role).Claim().UpdateSignalledAt == nil
	})
}

// wantAskEndsAt holds the last ask role's topic was given to name the moment at as the round's end.
func (h *heldRoundRig) wantAskEndsAt(role claim.Role, at time.Time) {
	h.t.Helper()
	asks := h.publishedHolding(role, heldRoundAsk)
	want := "The round ends at " + at.UTC().Format(time.RFC3339) + " (UTC)."
	if len(asks) == 0 || !strings.Contains(asks[len(asks)-1], want) {
		h.t.Fatalf("the %s's asks are %q, want the last to say %q: a bound of the round's own length from the ask again", role, asks, want)
	}
}

// wantLateAccountNotice advances beyond the round's original bound, not beyond a later re-ask.
func (h *heldRoundRig) wantLateAccountNotice(role claim.Role) {
	h.t.Helper()
	c := h.machine(role).Claim()
	if c.UpdateOpenedAt == nil {
		h.t.Fatal("the held round records no opening")
	}
	at := c.UpdateOpenedAt.Add(heldRoundGrace + time.Second)
	if now := h.clock.Now(); now.Before(at) {
		h.clock.advance(at.Sub(now))
	}
	h.keeps("past the service-account round's bound")
	late := h.updateNotices("The bound of the update round is past.")
	if len(late) != 1 {
		h.t.Fatalf("the controller got %d late notices, want one: %+v", len(late), late)
	}
	for _, want := range []string{`service account "default"`, `service account is "legion-worker"`,
		"implementer (suspended)", "The architect and a merger that starts in this pod use the old account",
		"until the work hold opens or an operator acts on a held claim"} {
		if !strings.Contains(late[0].Reason, want) {
			h.t.Fatalf("the late notice says %q, want it to name %q", late[0].Reason, want)
		}
	}
	h.wantNotReplaced("past the service-account round's bound")
}

// architectAnsweredUnderTheHold is the first case's common part: the root's architect live in the
// pod answers the round the hold keeps the pod of, and is then handed two tasks, a person's answer
// and a resume task, each its own turn, with sweeps of the kept round between them. The round
// asks it once and tells it once.
func architectAnsweredUnderTheHold(t *testing.T) *heldRoundRig {
	t.Helper()
	h := newHeldRoundRig(t, phase.Implementing)
	architect := claim.RoleArchitect
	h.liveInPod(architect)
	h.closeOverAWorkingImplementer()
	h.unpushed()
	h.opensOver(heldAccountDrift, architect)
	h.asked(architect)
	h.answers(architect)
	h.keeps("over the architect's answer")
	h.wantNotReplaced("while the workspace holds the held work")

	h.takesTask(architect, "A person answered your question: keep the field optional.", "the person's answer after its answer to the round")
	h.sweep()
	h.sweep()
	h.takesTask(architect, "Resume your work on the design.", "the resume task after its answer to the round")
	h.keeps("after the architect's two turns")
	h.runNow()
	if told := h.publishedHolding(architect, heldRoundWaits); len(told) != 1 || told[0] != heldRoundWaits {
		t.Fatalf("the architect was told %q of the hold, want once %q", told, heldRoundWaits)
	}
	if asks := h.publishedHolding(architect, heldRoundAsk); len(asks) != 1 {
		t.Fatalf("the architect was asked %d times, want once: its answer still counts for the round", len(asks))
	}
	h.wantNotReplaced("while the workspace holds the held work")
	h.wantLateAccountNotice(architect)
	if c := h.machine(claim.RoleImplementer).Claim(); c.State != supervise.StateSuspended || c.SuspendedBy != supervise.SuspendedByHold {
		t.Fatalf("the implementer is %s by %q, want it still held by the work hold", c.State, c.SuspendedBy)
	}
	return h
}

func TestWorkHoldKeepsTheRoundsPodAndHoldsNoTaskOfAClaimItDoesNotStop(t *testing.T) {
	architect, implementer := claim.RoleArchitect, claim.RoleImplementer

	t.Run("a live architect, then the held work is pushed", func(t *testing.T) {
		h := architectAnsweredUnderTheHold(t)
		h.pushed()
		reasked := h.clock.Now()
		h.askedAgain(architect, "once a read finds the held work pushed")
		h.wantNotReplaced("before the architect answered again")
		h.asked(architect)
		h.wantAskEndsAt(architect, reasked.Add(heldRoundGrace))
		h.sweep()
		h.wantNotReplaced("before the architect answered again")
		h.answers(architect)
		h.awaitReplacedOnce("once the architect answered again")
	})

	t.Run("a live architect, then the gate opens", func(t *testing.T) {
		h := architectAnsweredUnderTheHold(t)
		h.rt.ScriptResume(fake.SpawnResult{Locator: h.locator(implementer)})
		h.approve(4, 12)
		h.drain()
		if got := h.launches(implementer); got != 2 {
			t.Fatalf("the opening started the implementer %d times in all, want its first launch and the opening's", got)
		}
		h.wantInPod(implementer)
		readyClaimAs(t, h.sup, h.token(implementer), implementer, "ses-implementer")
		reasked := h.clock.Now()
		h.askedAgain(architect, "at the opening")
		h.wantNotReplaced("before the round's claims answered again")
		h.asked(architect)
		h.asked(implementer)
		h.wantAskEndsAt(architect, reasked.Add(heldRoundGrace))
		h.wantAskEndsAt(implementer, reasked.Add(heldRoundGrace))
		h.answers(architect)
		h.sweep()
		h.wantNotReplaced("before the implementer answered")
		h.answers(implementer)
		h.awaitReplacedOnce("once the architect and the implementer answered")
	})

	t.Run("a live architect, then the daemon restarts", func(t *testing.T) {
		h := architectAnsweredUnderTheHold(t)
		h.restarted(true, architect)
		h.keeps("after the restart")
		h.takesTask(architect, "A second person answered: ship it behind the flag.", "a task after the restart")
		h.runNow()
		if told := h.publishedHolding(architect, heldRoundWaits); len(told) != 1 {
			t.Fatalf("the architect was told of the hold %d times across the restart, want once: the claim records that it was told", len(told))
		}
		h.pushed()
		reasked := h.clock.Now()
		h.askedAgain(architect, "once a read finds the held work pushed")
		h.restarted(false, architect)
		h.asked(architect)
		h.wantAskEndsAt(architect, reasked.Add(heldRoundGrace))
		h.answers(architect)
		h.awaitReplacedOnce("once the architect answered again after the restart")
	})

	t.Run("a released architect woken by a role message", func(t *testing.T) {
		h := newHeldRoundRig(t, phase.Implementing)
		h.liveInPod(architect)
		h.liveInPod(implementer)
		h.handle(implementer, supervise.StreamTurnStart{Claim: h.token(implementer)})
		h.unpushed()
		h.opensOver(heldAccountDrift, architect, implementer)
		h.asked(architect)
		h.answers(architect)
		h.handle(architect, supervise.RequestSuspend{Claim: h.token(architect), By: supervise.SuspendedByReleased,
			Reason: "the issue's pod is released"})
		h.close(4)
		h.drain()
		endTurn(h.ctx, t, h.machine(implementer), h.token(implementer))
		h.drain()
		h.wantHeld(implementer, 1, "after the close")
		h.rt.ScriptIssuePod(h.tree.key, podUpdatePod)
		h.keeps("with no role process in the pod")
		h.wantNotReplaced("with no role process in the pod")

		// A person's message to the architect's role topic while it is released: the listener's
		// report wakes it, and the wake's start launches it into the pod the hold keeps.
		h.rt.ScriptResume(fake.SpawnResult{Locator: h.locator(architect)})
		h.apply("wake", intake.ListenerWake{Kind: intake.ListenerWakeRole, Role: string(h.token(architect)),
			At: h.clock.Now().UTC(), Report: "role-message-1"})
		h.drain()
		h.wantInPod(architect)
		readyClaimAs(t, h.sup, h.token(architect), architect, "ses-architect")
		wake := workflow.WakeTask(pods.WakeRoleMessage, string(h.token(architect)), workflow.StoppedByRelease)
		h.sweep()
		h.takesHeldTask(architect, wake, "the wake's task in the kept pod")
		h.takesTask(architect, "A person answered your question: keep the field optional.", "a second task in the kept pod")
		h.runNow()
		if told := h.publishedHolding(architect, heldRoundWaits); len(told) != 1 {
			t.Fatalf("the woken architect was told of the hold %d times, want once", len(told))
		}
		h.wantNotReplaced("while the workspace holds the held work")
		h.wantLateAccountNotice(architect)
		h.pushed()
		h.askedAgain(architect, "after the held work was pushed")
		h.asked(architect)
		h.answers(architect)
		h.awaitReplacedOnce("after the woken architect answered again")
	})

	t.Run("a released merger woken for the close message", func(t *testing.T) {
		merger := claim.RoleMerger
		h := newHeldRoundRig(t, phase.Merging)
		h.liveInPod(architect)
		h.liveInPod(implementer)
		h.handle(implementer, supervise.StreamTurnStart{Claim: h.token(implementer)})
		h.released(merger)
		h.unpushed()
		h.opensOver(heldAccountDrift, architect, implementer)
		h.close(4)
		h.rt.ScriptResume(fake.SpawnResult{Locator: h.locator(merger)})
		// The close's first run wakes the released merger for its message, into the pod the round
		// is open over, and posts the implementer's stop, which its turn's end makes.
		h.runOnce()
		if got := h.launches(merger); got != 1 {
			t.Fatalf("the close's message started the released merger %d times, want once", got)
		}
		h.wantInPod(merger)
		h.runOnce()
		endTurn(h.ctx, t, h.machine(implementer), h.token(implementer))
		h.until("the implementer's stop to be made at its turn's end", func() bool {
			h.runOnce()
			c := h.machine(implementer).Claim()
			return c.State == supervise.StateSuspended && c.SuspendedBy == supervise.SuspendedByHold
		})
		h.wantHeld(implementer, 1, "after its turn under the close")
		h.clock.advance(65 * time.Second)
		readyClaimAs(t, h.sup, h.token(merger), merger, "ses-merger")
		ready := h.clock.Now()
		h.runOnce()
		if got := h.published(merger); len(got) != 1 || got[0] != h.workerClose(4) {
			t.Fatalf("the merger woken into the kept pod was told %q, want the work close, %q", got, h.workerClose(4))
		}
		stops := h.holdStops(merger)
		if len(stops) != 1 || !stops[0].Deadline.Equal(ready.Add(holdGrace)) {
			t.Fatalf("the merger's stops are %+v, want one cut at %s, 2 minutes from its start at %s", stops, ready.Add(holdGrace), ready)
		}
		h.runOnce()
		if fired := h.clock.fire(holdGrace); fired == 0 {
			t.Fatal("no timer was armed for the merger's 2 minutes")
		}
		h.wantHeld(merger, 1, "2 minutes after its start")
		h.wantNotReplaced("while the work hold stands")
		h.asked(architect)
		h.answers(architect)
		h.wantLateAccountNotice(architect)
		h.takesTask(architect, "Continue your work on the old service account.", "the task during the service-account hold")
		h.pushed()
		h.askedAgain(architect, "after the held work was pushed")
		h.asked(architect)
		h.answers(architect)
		h.awaitReplacedOnce("after the merger received the close and the architect answered again")
	})

	t.Run("under dispatch_token_broker every launch into the pod is refused", func(t *testing.T) {
		h := newHeldRoundRig(t, phase.Implementing)
		h.liveInPod(architect)
		h.closeOverAWorkingImplementer()
		h.unpushed()
		h.opensOver(heldExchangeDrift, architect)
		h.refuseEveryLaunch()
		h.diesRefused(architect)

		h.keeps("through the claim whose launch waits on it")
		h.wantNotReplaced("while every launch is refused")
		notices := h.waitNotices()
		if len(notices) != 1 {
			t.Fatalf("the operator's side was told of %d waiting launches, want the architect's one: %+v", len(notices), notices)
		}
		for _, want := range []string{string(h.token(architect)), podUpdatePod, heldRoundRemedy, "The work hold stopped implementer (suspended)"} {
			if !strings.Contains(notices[0].Reason, want) {
				t.Fatalf("the notice of the waiting launch says %q, want it to name %q", notices[0].Reason, want)
			}
		}

		// The launch's retry a minute later is the same wait, and tells nothing more.
		if fired := h.clock.fire(podRoundRetry); fired == 0 {
			t.Fatal("no retry of the refused launch was armed")
		}
		h.keeps("after the launch's retry")
		if notices := h.waitNotices(); len(notices) != 1 {
			t.Fatalf("after the retry the operator's side was told of %d waiting launches, want still one", len(notices))
		}

		// Another launch that begins to wait on the pod is a wait of its own, and is told: here
		// the first launch of the tree's reviewer, which the runtime refuses as it refuses all.
		h.clock.advance(time.Minute)
		reviewer := claim.RoleReviewer
		h.create(reviewer, supervise.Claim{State: supervise.StateQueued})
		if err := h.machine(reviewer).Handle(h.ctx, supervise.RequestSpawn{Claim: h.token(reviewer)}); !errors.Is(err, runtime.ErrAwaitingPodRound) {
			t.Fatalf("the reviewer's launch answered %v, want the runtime's wait for its pod's round", err)
		}
		h.keeps("after the reviewer's wait began")
		notices = h.waitNotices()
		if len(notices) != 2 || !strings.Contains(notices[1].Reason, string(h.token(reviewer))) {
			t.Fatalf("after the reviewer's wait began the operator's side was told %+v, want a second notice naming the reviewer's claim", notices)
		}
		for _, role := range []claim.Role{architect, reviewer} {
			if c := h.machine(role).Claim(); c.Locator != nil {
				t.Fatalf("the %s records %+v, want no role started in the pod", role, c.Locator)
			}
		}
		h.wantNotReplaced("while every launch is refused")
	})
}

func TestWorkHoldNonBreakingRoundKeepsThePodAndEnds(t *testing.T) {
	const architect, implementer, merger = claim.RoleArchitect, claim.RoleImplementer, claim.RoleMerger

	t.Run("a live architect receives its held task", func(t *testing.T) {
		h := newHeldRoundRig(t, phase.Implementing)
		h.liveInPod(architect)
		h.closeOverAWorkingImplementer()
		h.unpushed()
		h.opensOver(podUpdateDrift, architect)
		h.asked(architect)
		h.answers(architect)
		const task = "A person answered your question: keep the field optional."
		h.handle(architect, supervise.RequestDeliver{Claim: h.token(architect), Task: task})
		if h.prompted(architect, task) {
			t.Fatal("the architect received its task before the round ended")
		}
		h.until("the non-breaking round to end with no replacement", func() bool {
			h.sweep()
			h.wantNotReplaced("over a non-breaking drift")
			return h.machine(architect).Claim().UpdateRound == ""
		})
		h.takesHeldTask(architect, task, "the task from the finished round")
		h.wantInPod(architect)
		h.runNow()
		if told, asked := h.publishedHolding(architect, heldRoundWaits), h.publishedHolding(architect, heldRoundAsk); len(told) != 0 || len(asked) != 1 {
			t.Fatalf("the non-breaking round told the architect %q and asked it %q, want just its first ask", told, asked)
		}
	})

	t.Run("a released architect and a merger woken by the close", func(t *testing.T) {
		h := newHeldRoundRig(t, phase.Merging)
		h.liveInPod(architect)
		h.liveInPod(implementer)
		h.released(merger)
		h.unpushed()
		h.opensOver(podUpdateDrift, architect, implementer)
		h.asked(architect)
		h.answers(architect)
		h.handle(architect, supervise.RequestSuspend{Claim: h.token(architect), By: supervise.SuspendedByReleased,
			Reason: "the issue's pod is released"})
		h.close(4)
		// Make the close's stop before draining the merger's close message. Neither the
		// merger's wake nor a sweep can get the replacement decision before the launch below.
		h.handle(implementer, supervise.RequestSuspend{Claim: h.token(implementer), By: supervise.SuspendedByHold,
			Reason: "the design gate closed", Deadline: h.clock.Now()})
		h.wantHeld(implementer, 1, "after the close")
		h.rt.ScriptIssuePod(h.tree.key, podUpdatePod)
		if c := h.machine(architect).Claim(); c.UpdateRound != heldRound {
			t.Fatalf("the released architect records round %q, want the open round %q", c.UpdateRound, heldRound)
		}

		h.rt.ScriptResume(fake.SpawnResult{Locator: h.locator(architect)})
		h.handle(architect, supervise.RequestRuleResume{Claim: h.token(architect), Reason: "wake the architect"})
		h.wantInPod(architect)
		if reads := h.rt.CallsOf("PodWorkspaceWork"); len(reads) != 1 || reads[0].Issue != h.tree.key {
			t.Fatalf("the launch asked for launcher reads %+v, want the read of the round's pod", reads)
		}
		readyClaimAs(t, h.sup, h.token(architect), architect, "ses-architect")
		h.settle()
		if c := h.machine(architect).Claim(); c.UpdateRound != "" {
			t.Fatalf("the launch kept the pod but left the round open: %q", c.UpdateRound)
		}
		h.takesTask(architect, "A person answered your question: keep the field optional.", "a person's answer in the kept pod")
		h.takesTask(architect, "Resume your work on the design.", "the resume task in the kept pod")

		h.rt.ScriptResume(fake.SpawnResult{Locator: h.locator(merger)})
		h.runOnce()
		h.wantInPod(merger)
		h.clock.advance(65 * time.Second)
		readyClaimAs(t, h.sup, h.token(merger), merger, "ses-merger")
		h.runOnce()
		if got := h.published(merger); len(got) != 1 || got[0] != h.workerClose(4) {
			t.Fatalf("the merger in the kept pod got %q, want the close message %q", got, h.workerClose(4))
		}
		if told := h.publishedHolding(architect, heldRoundWaits); len(told) != 0 {
			t.Fatalf("the architect got a work-hold notice in a non-breaking round: %q", told)
		}
		h.wantNotReplaced("after both roles started in the kept pod")
	})
}

// refuseEveryLaunch makes every launch into the pod, a Resume or a Spawn of any claim, the Sandbox
// runtime's refusal of a launch into a pod built before the exchange identity under
// dispatch_token_broker with no dispatch_token_transition_file (runtime.AwaitingPodRound), at the
// launch's own generation.
func (h *heldRoundRig) refuseEveryLaunch() {
	refuse := func(method string) func(context.Context) error {
		return func(context.Context) error {
			calls := h.rt.CallsOf(method)
			spec := calls[len(calls)-1].Spec
			loc := *podUpdateLocator(spec.Claim, h.tree.key, podUpdatePod)
			sandbox := *loc.Sandbox
			sandbox.Generation = spec.Generation
			loc.Sandbox, loc.Incarnation = &sandbox, runtime.SandboxIncarnation(podUpdatePod, spec.Generation)
			return fmt.Errorf("launch %s: %w", spec.Claim, &runtime.AwaitingPodRound{Pod: loc,
				Reason: "pod legion-legion-208 carries no exchange identity and dispatch_token_transition_file is not set; its update round replaces it"})
		}
	}
	h.rt.OnResume, h.rt.OnSpawn = refuse("Resume"), refuse("Spawn")
}

// diesRefused is role's process dying in the pod, and the relaunch that raises refused for want
// of the pod's round: the claim's launch begins to wait on the pod.
func (h *heldRoundRig) diesRefused(role claim.Role) {
	h.t.Helper()
	ended := h.machine(role).Claim().Locator
	if ended == nil {
		h.t.Fatalf("the %s records no process to die", role)
	}
	err := h.machine(role).Handle(h.ctx, supervise.RuntimeObservation{Observation: runtime.Observation{Locator: *ended, Kind: runtime.Gone, At: h.clock.Now()}})
	if !errors.Is(err, runtime.ErrAwaitingPodRound) {
		h.t.Fatalf("the %s's relaunch answered %v, want the runtime's wait for its pod's round", role, err)
	}
	if c := h.machine(role).Claim(); c.Locator != nil || podOf(c.AwaitsPod) != podUpdatePod {
		h.t.Fatalf("the refused %s is at %+v waiting on %+v, want no process and a wait on pod %s", role, c.Locator, c.AwaitsPod, podUpdatePod)
	}
}

// waitNotices is every update-stalled notice the outbox holds for the tree's issue whose reason
// names the broker state's remedy: one for each launch that began to wait on the kept pod.
func (h *heldRoundRig) waitNotices() []record.ControllerNotice {
	return h.updateNotices(heldRoundRemedy)
}

func (h *heldRoundRig) updateNotices(reason string) []record.ControllerNotice {
	h.t.Helper()
	rows, err := h.pool.Query(h.ctx, "select payload from outbox where kind = $1 and issue = $2 order by id",
		string(record.OutboxKindControllerNotice), h.tree.key)
	if err != nil {
		h.t.Fatalf("read the controller notices: %v", err)
	}
	defer rows.Close()
	var notices []record.ControllerNotice
	for rows.Next() {
		var payload []byte
		if err := rows.Scan(&payload); err != nil {
			h.t.Fatalf("read a controller notice: %v", err)
		}
		var notice record.ControllerNotice
		if err := json.Unmarshal(payload, &notice); err != nil {
			h.t.Fatalf("decode a controller notice: %v", err)
		}
		if notice.Kind == record.UpdateStalledNotice && strings.Contains(notice.Reason, reason) {
			if notice.Update == nil || len(notice.Update.Waiting) == 0 {
				h.t.Fatalf("the notice of a waiting launch lists %+v, want the claims of the round", notice.Update)
			}
			notices = append(notices, notice)
		}
	}
	if err := rows.Err(); err != nil {
		h.t.Fatalf("read the controller notices: %v", err)
	}
	return notices
}

// Decision 17 (dispatch://INFRA-4528's plan) over the project's controller's pod replacement
// (#4065, requestPodUpdate): the round the controller asks for records its own reasons
// (requestedPodUpdateReasons), a drift no pod breaks under, so it ends as main's keep ends a
// round, after a read of the pod's workspace before the replacement (readReplacement, roundKeeps).
// The implementer the work hold stopped left an unpushed commit that only the pod's disk holds:
// the read finds it, the round ends with no replacement, the pod is kept, the architect goes on
// in it, and the held claim stays the hold's.
func TestWorkHoldTheControllersPodReplacementKeepsThePodOfHeldWork(t *testing.T) {
	const architect, implementer = claim.RoleArchitect, claim.RoleImplementer
	h := newHeldRoundRig(t, phase.Implementing)
	h.liveInPod(architect)
	h.closeOverAWorkingImplementer()
	h.unpushed()

	answer, err := h.s.requestPodUpdate(h.ctx, h.tree.key)
	if err != nil || answer.Round != heldRound || !answer.Opened || answer.Reasons != requestedPodUpdateReasons {
		t.Fatalf("the controller's pod update answered %+v, %v; want the round %s opened with the controller's reasons", answer, err, heldRound)
	}
	h.settle()
	h.asked(architect)
	h.answers(architect)
	h.until("the controller's round to end with no replacement", func() bool {
		h.sweep()
		h.wantNotReplaced("over the controller's round while the held work is unpushed")
		return h.machine(architect).Claim().UpdateRound == ""
	})
	h.wantInPod(architect)
	h.wantHeld(implementer, 1, "after the controller's round ended")
}
