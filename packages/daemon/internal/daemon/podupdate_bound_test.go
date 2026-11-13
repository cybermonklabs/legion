package daemon

// The round's bound (dispatch://INFRA-2981 R2, R3, R5, R6, R8): a round ends as soon as every
// claim it asks has answered and no turn runs, and at its bound — pod_update_grace_minutes after
// it opened — whatever its roles said. An operator's suspension is the one wait the bound does not
// step over, the push check runs before every replacement the bound makes, and the controller is
// told only of a round something holds past its bound, once, at the bound. Each test moves the
// rig's held clock to the moment it decides at, and names that moment in what it asserts.

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/rescue/rescuetest"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/testwait"
)

const (
	// podUpdateReplacedLine is the line every replacement a round makes is logged on, whichever
	// way the round ended.
	podUpdateReplacedLine = "supervise: the pod of this update round is replaced and the round is over"
	// podUpdateBoundLine is the line a replacement the bound made is logged on as well, in the
	// words the production check reads (R8).
	podUpdateBoundLine = "supervise pod update replaced at the bound"
	// podUpdateRefusedNotice is what the sweep logs when the store will not take a notice's row.
	podUpdateRefusedNotice = "supervise: tell the operator's side that an update round is held past its bound; telling again at the next sweep"
)

// podUpdateBound is the bound of a round the rig opens at its start: the moment the round ends
// whatever its roles say.
var podUpdateBound = podUpdateStart.Add(podUpdateGrace)

// clockAt moves the rig's held clock forward to at.
func (r *podUpdateRig) clockAt(at time.Time) {
	r.t.Helper()
	d := at.Sub(r.clock.Now())
	if d < 0 {
		r.t.Fatalf("the rig's clock is at %s, already past %s", r.clock.Now(), at)
	}
	r.clock.advance(d)
}

// replacementClocks records the rig's clock at every ReplacePod the sweep makes, on the hold
// every call takes before its last word (fake's HoldReplacePod), and returns what it recorded.
func (r *podUpdateRig) replacementClocks() func() []time.Time {
	var at []time.Time
	r.rt.HoldReplacePod(func() { at = append(at, r.clock.Now()) })
	return func() []time.Time { return at }
}

// wantWaiting holds a notice's list to exactly the roles of want, each marked answered as want
// says: every late-round notice lists every claim the round asks (waitingOf).
func wantWaiting(t *testing.T, notice record.ControllerNotice, want map[claim.Role]bool) {
	t.Helper()
	if notice.Update == nil || len(notice.Update.Waiting) != len(want) {
		t.Fatalf("the notice lists %+v, want %v", notice.Update, want)
	}
	for _, w := range notice.Update.Waiting {
		if answered, ok := want[w.Role]; !ok || w.Answered != answered {
			t.Errorf("the notice lists %+v, want the %s marked answered=%t", w, w.Role, answered)
		}
	}
}

// wantNoRefusedNotice holds the sweep to having written each notice it built: the store took
// every row, so none was refused and none is written again at the next tick.
func wantNoRefusedNotice(t *testing.T, rig *podUpdateRig) {
	t.Helper()
	if refused := loggedLines(t, rig.logs.String(), podUpdateRefusedNotice); len(refused) != 0 {
		t.Fatalf("the store refused %d notices: %+v", len(refused), refused)
	}
}

// wantField holds one field of a decoded log line to the roles of want, in order.
func wantField(t *testing.T, line map[string]any, field string, want ...claim.Role) {
	t.Helper()
	got, _ := line[field].([]any)
	names := make([]string, len(got))
	for i, v := range got {
		names[i] = fmt.Sprint(v)
	}
	wantNames := make([]string, len(want))
	for i, role := range want {
		wantNames[i] = string(role)
	}
	if strings.Join(names, ",") != strings.Join(wantNames, ",") {
		t.Errorf("the line's %s = %v, want %v: %+v", field, line[field], wantNames, line)
	}
}

// Every claim answers at once, so the round ends at once, before its bound, and the line of the
// replacement says no bound made it. No line of a bounded end is written (R8's other half).
func TestPodUpdateBoundEveryAnswerEndsTheRoundBeforeTheBound(t *testing.T) {
	roles := []claim.Role{claim.RoleImplementer, claim.RoleTester}
	rig := newPodUpdateRig(t, readyWorker(claim.RoleImplementer), readyWorker(claim.RoleTester))
	clocks := rig.replacementClocks()
	rig.drifts(podUpdateDrift)

	rig.tick()
	for _, role := range roles {
		rig.awaitRound(role, podUpdateRound)
		rig.signal(role, "a1b2c3d")
	}

	rig.awaitReplacement(1, "once every claim answered")
	for _, at := range clocks() {
		if !at.Before(podUpdateBound) {
			t.Fatalf("the replacement was made at %s, want it before the bound %s: every claim had answered", at, podUpdateBound)
		}
	}
	replaced := loggedLines(t, rig.logs.String(), podUpdateReplacedLine)
	if len(replaced) != 1 || replaced[0]["atTheBound"] != false || replaced[0]["round"] != podUpdateRound || replaced[0]["pod"] != podUpdatePod {
		t.Fatalf("the replacement was logged %+v, want one line of round %s of pod %s that no bound made", replaced, podUpdateRound, podUpdatePod)
	}
	if lines := loggedLines(t, rig.logs.String(), podUpdateBoundLine); len(lines) != 0 {
		t.Fatalf("the sweep logged a bounded end %+v for a round its answers ended", lines)
	}
	if notices := rig.notices(); len(notices) != 0 {
		t.Fatalf("the controller was told %+v of a round that ended by its answers", notices)
	}
}

// A claim stays in a turn and never answers. Before the bound the pod is not replaced, whatever
// the clock reads, and no turn is cut; at the bound the pod is replaced, and the bounded end is
// logged with the claims that answered and those that did not. Nobody is told: a round that
// waits on its roles is no stall.
func TestPodUpdateBoundReplacesThePodOfAClaimThatNeverAnswers(t *testing.T) {
	rig := newPodUpdateRig(t,
		podUpdateRole{role: claim.RoleImplementer, state: supervise.StateWorking, session: true, inPod: true, contract: readyWorker(claim.RoleImplementer).contract},
		readyWorker(claim.RoleTester),
	)
	clocks := rig.replacementClocks()
	rig.drifts(podUpdateDrift)

	rig.tick()
	rig.awaitRound(claim.RoleImplementer, podUpdateRound)
	rig.awaitRound(claim.RoleTester, podUpdateRound)
	rig.signal(claim.RoleTester, "a1b2c3d")

	for _, at := range []time.Duration{0, 10 * time.Minute, 20 * time.Minute, podUpdateGrace - time.Minute} {
		rig.clockAt(podUpdateStart.Add(at))
		rig.tick()
		rig.tick()
		if calls := rig.replacements(); len(calls) != 0 {
			t.Fatalf("at %s, before the bound %s, the pod was replaced %d times with the implementer in a turn", rig.clock.Now(), podUpdateBound, len(calls))
		}
		if c := rig.claimOf(claim.RoleImplementer); c.State != supervise.StateWorking {
			t.Fatalf("at %s the implementer is %s, want its turn still running", rig.clock.Now(), c.State)
		}
	}

	rig.clockAt(podUpdateBound)
	rig.awaitReplacement(1, "at the bound")
	for _, at := range clocks() {
		if at.Before(podUpdateBound) {
			t.Fatalf("a replacement was made at %s, before the bound %s", at, podUpdateBound)
		}
	}
	bounded := loggedLines(t, rig.logs.String(), podUpdateBoundLine)
	if len(bounded) != 1 {
		t.Fatalf("the bounded end was logged %d times, want once: %+v", len(bounded), bounded)
	}
	line := bounded[0]
	if line["msg"] != podUpdateBoundLine || line["round"] != podUpdateRound || line["pod"] != podUpdatePod || line["issue"] != podUpdateIssue {
		t.Errorf("the bounded end's line is %+v, want the exact words with round %s, pod %s and issue %s", line, podUpdateRound, podUpdatePod, podUpdateIssue)
	}
	wantField(t, line, "answered", claim.RoleTester)
	wantField(t, line, "unanswered", claim.RoleImplementer)
	wantField(t, line, "working", claim.RoleImplementer)
	if replaced := loggedLines(t, rig.logs.String(), podUpdateReplacedLine); len(replaced) != 1 || replaced[0]["atTheBound"] != true {
		t.Errorf("the replacement was logged %+v, want one line that says the bound made it", replaced)
	}
	if notices := rig.notices(); len(notices) != 0 {
		t.Fatalf("the controller was told %+v of a round its bound ended", notices)
	}
	for _, role := range []claim.Role{claim.RoleImplementer, claim.RoleTester} {
		rig.awaitClaim(role, "its round to end with the replacement", func(c supervise.Claim) bool { return c.UpdateRound == "" })
	}
}

// One claim answers while another, which answered too, is still in a turn. Before the bound the
// replacement waits for that turn; at the bound it cuts it.
func TestPodUpdateBoundCutsTheTurnOfAnAnsweredClaim(t *testing.T) {
	rig := newPodUpdateRig(t,
		podUpdateRole{role: claim.RoleImplementer, state: supervise.StateWorking, session: true, inPod: true, contract: readyWorker(claim.RoleImplementer).contract},
		readyWorker(claim.RoleTester),
	)
	rig.drifts(podUpdateDrift)

	rig.tick()
	rig.awaitRound(claim.RoleImplementer, podUpdateRound)
	rig.awaitRound(claim.RoleTester, podUpdateRound)
	rig.signal(claim.RoleTester, "a1b2c3d")
	rig.signal(claim.RoleImplementer, "e4f5a6b")

	rig.clockAt(podUpdateBound.Add(-time.Minute))
	rig.tick()
	rig.tick()
	if calls := rig.replacements(); len(calls) != 0 {
		t.Fatalf("at %s, a minute before the bound, the pod was replaced %d times with the implementer's turn running", rig.clock.Now(), len(calls))
	}

	rig.clockAt(podUpdateBound)
	rig.awaitReplacement(1, "at the bound, over the turn still running")
	bounded := loggedLines(t, rig.logs.String(), podUpdateBoundLine)
	if len(bounded) != 1 {
		t.Fatalf("the bounded end was logged %d times, want once: %+v", len(bounded), bounded)
	}
	wantField(t, bounded[0], "answered", claim.RoleImplementer, claim.RoleTester)
	wantField(t, bounded[0], "unanswered")
	wantField(t, bounded[0], "working", claim.RoleImplementer)
}

// A replacement the bound let through reaches the daemon's last word under the pod's launch turn
// (replacementDue), and a turn starts there, of a claim that never answered. The last word takes
// the bound as the sweep's reading does, so it does not refuse the replacement: read without the
// bound, it would refuse it as errReplacementNotDue and no pod would ever be replaced.
func TestPodUpdateBoundIsReadByTheLastWordBeforeTheReplacement(t *testing.T) {
	rig := newPodUpdateRig(t, readyWorker(claim.RoleImplementer), readyWorker(claim.RoleTester))
	rig.drifts(podUpdateDrift)

	rig.tick()
	rig.awaitRound(claim.RoleImplementer, podUpdateRound)
	rig.awaitRound(claim.RoleTester, podUpdateRound)
	rig.signal(claim.RoleImplementer, "a1b2c3d")
	rig.clockAt(podUpdateBound)
	rig.settleAll()

	held := false
	rig.rt.HoldReplacePod(func() {
		rig.rt.HoldReplacePod(nil)
		held = true
		rig.handle(claim.RoleTester, supervise.StreamTurnStart{Claim: rig.tokens[claim.RoleTester]})
	})

	// The sweep that takes the hold is the one decision under test: the hold is taken once, and
	// the last word that follows it either lets the replacement through or refuses it.
	testwait.Eventually(t, "the replacement to wait for the pod's launch turn", func() bool {
		if !held {
			rig.tick()
		}
		return held
	})
	if refused := rig.notDue(); len(refused) != 0 {
		t.Fatalf("at %s, the bound, the last word refused the replacement %d times, want none: %+v", rig.clock.Now(), len(refused),
			loggedLines(t, rig.logs.String(), "was no longer due when its pod's launch turn came"))
	}
	if calls := rig.replacements(); len(calls) != 1 {
		t.Fatalf("at %s, the bound, the pod was replaced %d times through the last word, want once", rig.clock.Now(), len(calls))
	}
	bounded := loggedLines(t, rig.logs.String(), podUpdateBoundLine)
	if len(bounded) != 1 {
		t.Fatalf("the bounded end was logged %d times, want once: %+v", len(bounded), bounded)
	}
	wantField(t, bounded[0], "unanswered", claim.RoleTester)
	wantField(t, bounded[0], "working", claim.RoleTester)
}

// Two claims; the operator suspends one, and the other answered and still runs a process in the
// pod. Past the bound nothing but the operator's suspension holds the pod (readReplacement's
// waitsForOperator), and it does: no ReplacePod is called however long the clock runs. Once the
// operator resumes the suspended claim, after the bound, the bound ends the round.
func TestPodUpdateSuspensionHoldsThePodPastTheBoundUntilTheOperatorResumes(t *testing.T) {
	rig := newPodUpdateRig(t, readyWorker(claim.RoleImplementer), readyWorker(claim.RoleTester))
	rig.drifts(podUpdateDrift)

	rig.tick()
	rig.awaitRound(claim.RoleImplementer, podUpdateRound)
	rig.awaitRound(claim.RoleTester, podUpdateRound)
	rig.signal(claim.RoleImplementer, "a1b2c3d")
	rig.suspendByOperator(claim.RoleTester)

	for _, at := range []time.Duration{0, time.Minute, time.Hour} {
		rig.clockAt(podUpdateBound.Add(at))
		rig.tick()
		rig.tick()
		if calls := rig.replacements(); len(calls) != 0 {
			t.Fatalf("at %s, past the bound %s, the pod was replaced %d times under the operator's suspension of the tester",
				rig.clock.Now(), podUpdateBound, len(calls))
		}
		if c := rig.claimOf(claim.RoleImplementer); c.UpdateRound != podUpdateRound || podOf(c.Locator) != podUpdatePod {
			t.Fatalf("at %s the implementer = round %q in pod %q, want the round open over the pod it runs in", rig.clock.Now(), c.UpdateRound, podOf(c.Locator))
		}
	}
	if notices := rig.notices(); len(notices) != 1 || !strings.Contains(notices[0].Reason, "An operator suspended") {
		t.Fatalf("the controller was told %+v, want one notice naming the operator's suspension", notices)
	}

	rig.resumeInPod(claim.RoleTester)

	rig.awaitReplacement(1, "once the operator resumed the tester, past the bound")
	bounded := loggedLines(t, rig.logs.String(), podUpdateBoundLine)
	if len(bounded) != 1 {
		t.Fatalf("the bounded end was logged %d times, want once: %+v", len(bounded), bounded)
	}
	wantField(t, bounded[0], "answered", claim.RoleImplementer)
	wantField(t, bounded[0], "unanswered", claim.RoleTester)
}

// The only claim of the round is the one an operator suspends, after the sweep read the claims:
// the snapshot records the pod, the claim as its machine holds it records no process. Past the
// bound the pod is not replaced, at that sweep or at the podless sweeps after it, and the
// controller is told once, naming the suspension.
func TestPodUpdateSuspensionOfTheOnlyClaimHoldsThePodPastTheBound(t *testing.T) {
	rig := newPodUpdateRig(t, readyWorker(claim.RoleImplementer))
	rig.drifts(podUpdateDrift)

	rig.tick()
	rig.awaitRound(claim.RoleImplementer, podUpdateRound)
	rig.settleAll()
	rig.clockAt(podUpdateBound.Add(time.Hour))

	group := rig.snapshot()
	rig.suspendByOperator(claim.RoleImplementer)
	rig.s.reconcilePodUpdates(rig.ctx, group)
	rig.tick()
	rig.tick()

	if calls := rig.replacements(); len(calls) != 0 {
		t.Fatalf("at %s, an hour past the bound, the pod was replaced %d times under the operator's suspension of its only claim", rig.clock.Now(), len(calls))
	}
	if c := rig.claimOf(claim.RoleImplementer); c.UpdateRound != podUpdateRound {
		t.Fatalf("the suspended claim's round = %q, want it still open: only the operator's resume ends this wait", c.UpdateRound)
	}
	notices := rig.notices()
	if len(notices) != 1 || !strings.Contains(notices[0].Reason, "An operator suspended") {
		t.Fatalf("the controller was told %+v, want one notice naming the operator's suspension", notices)
	}
	wantWaiting(t, notices[0], map[claim.Role]bool{claim.RoleImplementer: false})
	wantNoRefusedNotice(t, rig)
}

// An operator suspends the only claim of a round, which had not answered, and while the sweep
// runs the pod is replaced where it stood for a failure of its own: the sweep's snapshot records
// the pod and reads it drifted, and the runtime answers it gone when the round asks about it.
// The operator's suspension is the one wait the bound does not step over, and the clock is an
// hour past the bound, but that hold is of a pod that is there: this one is gone, so the round
// ends there, the issue is told that its implementer lost what it had not pushed, and no
// ReplacePod is called — not at that sweep and not at the sweeps after it. Read the hold
// before the pod, the round would wait for the resume of a claim on a pod that no longer exists.
func TestPodUpdateEndsTheRoundOfAnOperatorsSuspensionWhenItsPodIsGone(t *testing.T) {
	rig := newPodUpdateRig(t, readyWorker(claim.RoleImplementer))
	rig.drifts(podUpdateDrift)

	rig.tick()
	rig.awaitRound(claim.RoleImplementer, podUpdateRound)
	rig.settleAll()
	rig.clockAt(podUpdateBound.Add(time.Hour))

	group := rig.snapshot()
	rig.suspendByOperator(claim.RoleImplementer)
	if c := rig.claimOf(claim.RoleImplementer); !c.RuleHolds() {
		t.Fatalf("the suspended implementer = %s held by %q, want an operator's hold", c.State, c.SuspensionHeldBy)
	}
	rig.s.runtime = &podGoneAfterTheDriftRead{Runtime: rig.rt}
	rig.s.reconcilePodUpdates(rig.ctx, group)

	rig.awaitClaim(claim.RoleImplementer, "its round to end with the pod that is not there",
		func(c supervise.Claim) bool { return c.UpdateRound == "" })
	messages := rig.messages()
	if len(messages) != 1 || !strings.Contains(messages[0], "The pod that ran this issue's roles is gone") ||
		!strings.Contains(messages[0], string(claim.RoleImplementer)) {
		t.Fatalf("the issue was written %q, want one message that its pod went and its implementer lost what it had not pushed", messages)
	}

	rig.tick()
	rig.tick()
	if calls := rig.replacements(); len(calls) != 0 {
		t.Fatalf("ReplacePod called %d times for a pod that is not there", len(calls))
	}
	if notices := rig.notices(); len(notices) != 0 {
		t.Fatalf("the controller was told %+v of a round that ended with its pod", notices)
	}
	if c := rig.claimOf(claim.RoleImplementer); c.UpdateRound != "" || c.State != supervise.StateSuspended {
		t.Fatalf("the implementer after the sweeps = %s in round %q, want it suspended in no round", c.State, c.UpdateRound)
	}
}

// The other reading that says a pod went, under the same hold: every worker answered, an
// operator suspends the implementer while it is still in the round's pod, and then that pod goes
// and the displaced path brings the tester back in the pod that replaced it. The claims record
// that other pod in the round's own place, which says the round's pod went whatever the
// operator's suspension says, so the round ends there over the pod that went
// (endRoundOverAGonePod), with nothing lost and nothing told, and nothing is replaced. Read the
// hold first, the round would wait for the operator's resume over a pod that is gone, and tell
// the controller of a suspension that holds nothing.
func TestPodUpdateEndsTheRoundOfAnOperatorsSuspensionWhenAnotherPodIsInItsPlace(t *testing.T) {
	const overAGonePod = "supervise: the pod of this issue's update round is not there, and every claim of the round had answered; nothing was lost and the round is over"
	rig := newPodUpdateRig(t, readyWorker(claim.RoleImplementer), readyWorker(claim.RoleTester))
	rig.drifts(podUpdateDrift)

	rig.tick()
	roles := []claim.Role{claim.RoleImplementer, claim.RoleTester}
	for _, role := range roles {
		rig.awaitRound(role, podUpdateRound)
		rig.signal(role, "a1b2c3d")
	}
	rig.settleAll()
	rig.suspendByOperator(claim.RoleImplementer)
	rig.displacedInto(claim.RoleTester, podUpdateReplacementPod)
	rig.settleAll()
	if c := rig.claimOf(claim.RoleImplementer); !c.RuleHolds() || c.LeftPodRound(podUpdateRound) {
		t.Fatalf("the suspended implementer = %s held by %q, left the round %t; want an operator's hold on a member of the round",
			c.State, c.SuspensionHeldBy, c.LeftPodRound(podUpdateRound))
	}
	rig.clockAt(podUpdateBound.Add(time.Hour))

	rig.tick()

	if lines := loggedLines(t, rig.logs.String(), overAGonePod); len(lines) != 1 {
		t.Fatalf("the sweep ended the round over a gone pod %d times, want once: %+v", len(lines), lines)
	}
	if notices := rig.notices(); len(notices) != 0 {
		t.Fatalf("the controller was told %+v of a round whose pod another one has replaced", notices)
	}
	if messages := rig.messages(); len(messages) != 0 {
		t.Fatalf("the issue was written %q; every worker of the round had answered before its pod went", messages)
	}
	if calls := rig.replacements(); len(calls) != 0 {
		t.Fatalf("ReplacePod called %d times for a pod that is already gone", len(calls))
	}
	for _, role := range roles {
		rig.awaitClaim(role, "its round to end over the pod that is gone",
			func(c supervise.Claim) bool { return c.UpdateRound == "" })
	}
}

// The only claim of a round is the one an operator suspends, and no claim of the issue records a
// pod from then on (letGo clears the locator), so every sweep reads the round podless and nothing
// of it can move until the operator resumes the claim (tellPodlessRound). That is no stall
// before the bound, so the controller is told nothing then. At the bound it is told once, naming
// what holds the round — the operator's suspension, and no claim recording a process in a pod —
// and the sweeps after it tell nothing more, however long the round stays open.
func TestPodUpdateTellsAPodlessRoundAnOperatorHoldsOncePastItsBound(t *testing.T) {
	rig := newPodUpdateRig(t, readyWorker(claim.RoleImplementer))
	rig.drifts(podUpdateDrift)

	rig.tick()
	rig.awaitRound(claim.RoleImplementer, podUpdateRound)
	rig.settleAll()
	rig.suspendByOperator(claim.RoleImplementer)

	rig.clockAt(podUpdateBound.Add(-time.Minute))
	rig.tick()
	if notices := rig.notices(); len(notices) != 0 {
		t.Fatalf("at %s, before the bound %s, the controller was told %+v of a podless round", rig.clock.Now(), podUpdateBound, notices)
	}

	rig.clockAt(podUpdateBound)
	rig.tick()
	notices := rig.notices()
	if len(notices) != 1 {
		t.Fatalf("at the bound %s the controller was told %d times of a podless round an operator holds, want once: %+v", podUpdateBound, len(notices), notices)
	}
	for _, want := range []string{"No claim of this issue records a process in a pod", "An operator suspended", string(claim.RoleImplementer)} {
		if !strings.Contains(notices[0].Reason, want) {
			t.Errorf("the notice says %q, want it to say %q", notices[0].Reason, want)
		}
	}
	wantWaiting(t, notices[0], map[claim.Role]bool{claim.RoleImplementer: false})

	rig.clockAt(podUpdateBound.Add(time.Hour))
	rig.tick()
	if after := rig.notices(); len(after) != 1 {
		t.Fatalf("an hour past the bound the controller has been told %d times of the round, want the one notice at the bound: %+v", len(after), after)
	}
	if c := rig.claimOf(claim.RoleImplementer); c.UpdateRound != podUpdateRound {
		t.Fatalf("the suspended claim's round = %q, want it still open: only the operator's resume ends this wait", c.UpdateRound)
	}
	if calls := rig.replacements(); len(calls) != 0 {
		t.Fatalf("the pod was replaced %d times with no claim recording it", len(calls))
	}
	wantNoRefusedNotice(t, rig)
}

// An operator suspends a claim while the replacement the bound let through waits for the pod's
// launch turn. The sweep's own reading was taken before the suspension, so only the daemon's
// last word (replacementDue) can see it, and it refuses the replacement: no pod is replaced
// under an operator's hold, whatever the clock says.
func TestPodUpdateSuspensionInTheLaunchTurnRefusesTheReplacement(t *testing.T) {
	rig := newPodUpdateRig(t, readyWorker(claim.RoleImplementer), readyWorker(claim.RoleTester))
	rig.drifts(podUpdateDrift)

	rig.tick()
	rig.awaitRound(claim.RoleImplementer, podUpdateRound)
	rig.awaitRound(claim.RoleTester, podUpdateRound)
	rig.signal(claim.RoleImplementer, "a1b2c3d")
	rig.clockAt(podUpdateBound)
	rig.settleAll()

	held := false
	rig.rt.HoldReplacePod(func() {
		rig.rt.HoldReplacePod(nil)
		held = true
		rig.handle(claim.RoleTester, supervise.RequestSuspend{Claim: rig.tokens[claim.RoleTester], By: supervise.SuspendedByOperator})
		rig.awaitClaim(claim.RoleTester, "the operator's suspension to land", func(c supervise.Claim) bool {
			return c.State == supervise.StateSuspended && c.RuleHolds()
		})
	})
	testwait.Eventually(t, "the replacement to wait for the pod's launch turn", func() bool {
		if !held {
			rig.tick()
		}
		return held
	})

	if calls := rig.replacements(); len(calls) != 0 {
		t.Fatalf("at %s, the bound, the pod was replaced %d times under an operator's suspension", rig.clock.Now(), len(calls))
	}
	if refused := rig.notDue(); len(refused) != 1 {
		t.Fatalf("the last word refused the replacement %d times, want once: it reads the operator's hold", len(refused))
	}
	if lines := loggedLines(t, rig.logs.String(), "an operator's suspension holds tester"); len(lines) != 1 {
		t.Errorf("the refusal was logged %+v, want one line naming the operator's suspension of the tester", lines)
	}
	if c := rig.claimOf(claim.RoleImplementer); c.UpdateRound != podUpdateRound {
		t.Fatalf("the implementer's round = %q, want it left open", c.UpdateRound)
	}
}

// At the bound, over a pod whose workspace holds a commit the remote lacks and whose claims never
// answered: the push check runs before the replacement the bound makes, pushes the commit to a
// rescue branch, and the issue is told the branch and its commit (R3).
func TestPodUpdateBoundRunsThePushCheckBeforeItReplacesThePod(t *testing.T) {
	d := rescuetest.New(t, podUpdateIssue)
	d.Write(d.Workspace, "feature.txt", "work nobody pushed before the bound\n")
	d.JJ("commit", "-m", "work nobody pushed before the bound")
	commit := d.JJ("log", "--no-graph", "-r", "@-", "-T", "commit_id")
	rig := newPodUpdateRig(t, readyWorker(claim.RoleImplementer), readyWorker(claim.RoleTester))
	rig.rescueOn(d)
	rig.drifts(podUpdateDrift)

	rig.tick()
	rig.awaitRound(claim.RoleImplementer, podUpdateRound)
	rig.awaitRound(claim.RoleTester, podUpdateRound)
	rig.clockAt(podUpdateBound)

	rig.awaitReplacement(1, "at the bound, with no claim answered")
	check, replace := -1, -1
	for i, call := range rig.rt.Calls() {
		switch {
		case call.Method == "RescueUnpushed" && check < 0:
			check = i
		case call.Method == "ReplacePod" && replace < 0:
			replace = i
		}
	}
	if check < 0 || check > replace {
		t.Fatalf("the runtime's calls ran the push check at %d and the replacement at %d, want the check first: %+v", check, replace, rig.rt.Calls())
	}
	branch := podUpdateRescueBranch + "-1"
	if held := d.RemoteBranch(branch); held != commit {
		t.Fatalf("the remote holds %s at %q, want the commit the disk held %s", branch, held, commit)
	}
	if messages := rig.messages(); len(messages) != 1 || !strings.Contains(messages[0], "`"+branch+"` (commit "+commit+")") {
		t.Fatalf("the issue was told %q, want one message naming %s at %s", messages, branch, commit)
	}
	if bounded := loggedLines(t, rig.logs.String(), podUpdateBoundLine); len(bounded) != 1 {
		t.Fatalf("the bounded end was logged %d times, want once: %+v", len(bounded), bounded)
	}
}

// At the bound, over a pod whose push check fails: the pod is held, and the controller is told
// once, at the bound — the check does not run before it, since the answers held the round there
// — naming the failed check. The store takes the row, and later sweeps tell nothing more.
func TestPodUpdateBoundHoldsAPodWhosePushCheckFails(t *testing.T) {
	d := rescuetest.New(t, podUpdateIssue)
	d.Write(d.Workspace, "feature.txt", "work the remote will not take\n")
	d.JJ("commit", "-m", "work the remote will not take")
	d.RefuseWrites()
	rig := newPodUpdateRig(t, readyWorker(claim.RoleImplementer), readyWorker(claim.RoleTester))
	rig.rescueOn(d)
	rig.drifts(podUpdateDrift)

	rig.tick()
	rig.awaitRound(claim.RoleImplementer, podUpdateRound)
	rig.awaitRound(claim.RoleTester, podUpdateRound)
	rig.clockAt(podUpdateBound.Add(-time.Minute))
	rig.tick()
	rig.tick()
	if checks := rig.rt.CallsOf("RescueUnpushed"); len(checks) != 0 || len(rig.notices()) != 0 {
		t.Fatalf("at %s, a minute before the bound, the sweep ran %d push checks and told %d notices, want neither", rig.clock.Now(), len(checks), len(rig.notices()))
	}

	rig.clockAt(podUpdateBound)
	rig.awaitRescues(1, "at the bound")
	notices := rig.notices()
	if len(notices) != 1 {
		t.Fatalf("at the bound %s the controller was told %d times of a failed push check, want once", rig.clock.Now(), len(notices))
	}
	for _, want := range []string{"check for unpushed work", "the remote refuses this push", d.Workspace} {
		if !strings.Contains(notices[0].Reason, want) {
			t.Errorf("the notice says %q, want it to name %q", notices[0].Reason, want)
		}
	}
	wantWaiting(t, notices[0], map[claim.Role]bool{claim.RoleImplementer: false, claim.RoleTester: false})
	wantNoRefusedNotice(t, rig)

	rig.clockAt(podUpdateBound.Add(time.Hour))
	rig.awaitRescues(len(rig.rt.CallsOf("RescueUnpushed"))+1, "an hour past the bound")
	if calls := rig.replacements(); len(calls) != 0 {
		t.Fatalf("the pod was replaced %d times while its push check fails, want none", len(calls))
	}
	if notices := rig.notices(); len(notices) != 1 {
		t.Fatalf("the controller had been told %d times, want once per round", len(notices))
	}
}

// At the bound, over a pod whose shim predates the push check (runtime.ErrRescueUnsupported) and
// whose claims never answered: the check is asked, it cannot run, and that holds nothing. The pod
// is replaced at the bound, the issue is told that nobody checked the pod's workspaces, and the
// controller is told nothing, since nothing holds the round.
//
// The negative control is a hold on that answer: the round then waits for ever past its bound and
// the replacement below never comes.
func TestPodUpdateBoundReplacesAPodWhoseShimPredatesTheCheckAndSaysSo(t *testing.T) {
	rig := newPodUpdateRig(t, readyWorker(claim.RoleImplementer), readyWorker(claim.RoleTester))
	rig.rt.RescueWith(func(context.Context, runtime.Known, runtime.RescueRequest) ([]runtime.WorkspaceRescue, error) {
		return nil, fmt.Errorf("worker-stream: %w (its agent answered: Unknown command: rescue-unpushed)", runtime.ErrRescueUnsupported)
	})
	rig.drifts(podUpdateDrift)

	rig.tick()
	rig.awaitRound(claim.RoleImplementer, podUpdateRound)
	rig.awaitRound(claim.RoleTester, podUpdateRound)
	rig.clockAt(podUpdateBound.Add(-time.Minute))
	rig.tick()
	rig.tick()
	if checks, calls := rig.rt.CallsOf("RescueUnpushed"), rig.replacements(); len(checks) != 0 || len(calls) != 0 {
		t.Fatalf("at %s, a minute before the bound, the sweep ran %d push checks and %d replacements, want neither", rig.clock.Now(), len(checks), len(calls))
	}

	rig.clockAt(podUpdateBound)
	rig.awaitReplacement(1, "at the bound, over a shim that cannot run the push check")
	if checks := rig.rt.CallsOf("RescueUnpushed"); len(checks) == 0 {
		t.Fatal("the pod was replaced at the bound with no push check asked of its shim")
	}
	messages := rig.messages()
	if len(messages) != 1 || !strings.Contains(messages[0], "did not check the pod's workspaces") || !strings.Contains(messages[0], "predates the push check") {
		t.Fatalf("the issue was told %q, want one message that nobody checked the pod's workspaces", messages)
	}
	if bounded := loggedLines(t, rig.logs.String(), podUpdateBoundLine); len(bounded) != 1 {
		t.Fatalf("the bounded end was logged %d times, want once: %+v", len(bounded), bounded)
	}
	if notices := rig.notices(); len(notices) != 0 {
		t.Fatalf("the controller was told %+v, want nothing: a shim that cannot run the check holds nothing", notices)
	}
}

// A round whose claims never answer is told to nobody at any clock: before its bound it waits on
// its roles, which is no stall, at its bound the bound ends it, and after it there is no round.
func TestPodUpdateBoundTellsNobodyOfARoundWhoseClaimsNeverAnswer(t *testing.T) {
	rig := newPodUpdateRig(t, readyWorker(claim.RoleImplementer), readyWorker(claim.RoleTester))
	rig.drifts(podUpdateDrift)

	rig.tick()
	rig.awaitRound(claim.RoleImplementer, podUpdateRound)
	rig.awaitRound(claim.RoleTester, podUpdateRound)
	for _, at := range []time.Duration{0, 10 * time.Minute, 20 * time.Minute, podUpdateGrace - time.Minute} {
		rig.clockAt(podUpdateStart.Add(at))
		rig.tick()
		if notices := rig.notices(); len(notices) != 0 {
			t.Fatalf("at %s the controller was told %+v of a round waiting on its roles", rig.clock.Now(), notices)
		}
	}

	rig.clockAt(podUpdateBound)
	rig.awaitReplacement(1, "at the bound")
	rig.drifts()
	for _, role := range []claim.Role{claim.RoleImplementer, claim.RoleTester} {
		rig.awaitClaim(role, "its round to end with the replacement", func(c supervise.Claim) bool { return c.UpdateRound == "" })
	}
	rig.clockAt(podUpdateBound.Add(time.Hour))
	rig.tick()

	if notices := rig.notices(); len(notices) != 0 {
		t.Fatalf("at %s the controller had been told %+v of a round its bound ended", rig.clock.Now(), notices)
	}
	wantNoRefusedNotice(t, rig)
}

// A round held past its bound by a claim recording no process in the round's pod — the daemon
// suspended the one claim after the sweep read it, and the pod is still there — is told once, at
// the bound, naming that reading. A minute before the bound the same reading tells nobody.
func TestPodUpdateBoundToldOnceOfARoundWithNoProcessInItsPod(t *testing.T) {
	rig := newPodUpdateRig(t, readyWorker(claim.RoleImplementer))
	rig.drifts(podUpdateDrift)

	rig.tick()
	rig.awaitRound(claim.RoleImplementer, podUpdateRound)
	rig.settleAll()
	group := rig.snapshot()
	rig.suspendBy(claim.RoleImplementer, supervise.SuspendedByDaemon)

	rig.clockAt(podUpdateBound.Add(-time.Minute))
	rig.s.reconcilePodUpdates(rig.ctx, group)
	if notices := rig.notices(); len(notices) != 0 {
		t.Fatalf("at %s, a minute before the bound, the controller was told %+v", rig.clock.Now(), notices)
	}

	rig.clockAt(podUpdateBound)
	rig.s.reconcilePodUpdates(rig.ctx, group)

	// The bound steps over no fact about the pod: with no process recorded in it, ReplacePod is not
	// called at all, not even to be refused inside it.
	if calls, refused := rig.replacements(), rig.notDue(); len(calls)+len(refused) != 0 {
		t.Fatalf("at the bound %s ReplacePod was called %d times (%d of them refused) with no process recorded in the pod", rig.clock.Now(),
			len(calls)+len(refused), len(refused))
	}
	notices := rig.notices()
	if len(notices) != 1 {
		t.Fatalf("at the bound %s the controller was told %d times, want once", rig.clock.Now(), len(notices))
	}
	if !strings.Contains(notices[0].Reason, "No claim of the issue records a process in the pod of the round") {
		t.Errorf("the notice says %q, want it to name the claim recording no process in the round's pod", notices[0].Reason)
	}
	if notices[0].Update == nil || notices[0].Update.Pod != podUpdatePod {
		t.Errorf("the notice carries %+v, want the round's pod %s", notices[0].Update, podUpdatePod)
	}
	wantWaiting(t, notices[0], map[claim.Role]bool{claim.RoleImplementer: false})
	wantNoRefusedNotice(t, rig)

	rig.tick()
	rig.tick()
	if notices := rig.notices(); len(notices) != 1 {
		t.Fatalf("the controller had been told %d times, want once per round", len(notices))
	}
}
