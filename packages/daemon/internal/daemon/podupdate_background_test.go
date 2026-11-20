package daemon

// The early end's wait for background work (dispatch://INFRA-4203 R1, R4, R5): before its bound,
// a round whose asked claims have all answered, and in which no turn runs, still keeps its pod
// while a claim that records a process in that pod reports background work that runs, has sent no
// activity report, or has sent none since its last turn started (supervise.Claim.LastTurnStart).
// A failed, retired or suspended claim holds nothing, nor does a claim whose process is in another
// pod. At the bound the round ends as it did before, whatever runs, and the bound's line names
// each claim that R1's hold still named.
//
// Each report here is written through the store's own call (store.PutClaimActivity, by
// silence_test.go's reportsBackgroundWork and reportsNothingRunning), as the activity route writes
// it, and no activity field is set on a claim's machine. Every role answers through
// answersWithoutReport, which sends no report, so each test writes the report of every role that it
// does not mean to hold the round with, and each test holds the round for the reason its case
// names and no other. These cases drive the pod-update rig of podupdate_test.go, with the bound of
// podupdate_bound_test.go.

import (
	"fmt"
	"maps"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

const (
	// backgroundWaitLine is the line the walk logs for a round that R1's hold keeps open before its
	// bound (completePodUpdate). It names the issue and the round, a background object from each
	// holding role to the kinds its report names, and an unreported list of the roles with no
	// report since their last turn. Each of those two fields is written only when it names a role,
	// and the line carries no working and no unanswered list: it is written only once every asked
	// claim has answered and no turn runs, so either list would name nobody.
	backgroundWaitLine = "supervise: every worker of this update round has answered and no turn runs; the replacement waits for the background work its roles report, or for the round's bound"
	// backgroundTwoPodsLine is the walk's line for a live group whose claims record two pods, a
	// replacement in flight (completePodUpdate's waitsForPodInFlight).
	backgroundTwoPodsLine = "supervise: the claims of this issue run in two pods now; this round's replacement waits for the one in flight"
	// backgroundNotDueLine is the line of a replacement that the daemon's last word refused under
	// the pod's launch turn (replacementDue). Its reason field is that refusal, which names what
	// held the replacement.
	backgroundNotDueLine = "supervise: this update round's replacement was no longer due when its pod's launch turn came; nothing was replaced and the round stays open"
)

// answersWithoutReport is the worker answering ready_for_update, as api's
// claims/ready-for-update posts it, with no activity report sent. The rig's own answer
// (podUpdateRig.signal) is this answer plus the report that an answering turn's end sends. Each
// test here writes every report itself, so a claim with no report at all is a case it can hold.
// The answer waits for the machine to have handled each acknowledgement it is owed
// (supervise.PromptAcked) before it is sent.
func (r *podUpdateRig) answersWithoutReport(role claim.Role, head string) {
	r.t.Helper()
	m, ok := r.sup.Machine(r.tokens[role])
	if !ok {
		r.t.Fatalf("%s is not supervised", role)
	}
	m.Wait()
	c := r.claimOf(role)
	if err := m.Handle(r.ctx, supervise.PodUpdateSignalled{
		Claim: c.Token, Generation: c.Generation, Session: c.Session, Round: c.UpdateRound, Head: head,
	}); err != nil {
		r.t.Fatalf("%s answered the update round: %v", role, err)
	}
}

// openRoundAnsweredBy scripts the drift of the issue's pod, sweeps the round open over it, and
// has each of roles answer the round through answersWithoutReport, so no report is sent.
func (r *podUpdateRig) openRoundAnsweredBy(roles ...claim.Role) {
	r.t.Helper()
	r.drifts(podUpdateDrift)
	r.tick()
	for _, role := range roles {
		r.awaitRound(role, podUpdateRound)
	}
	for _, role := range roles {
		r.answersWithoutReport(role, "a1b2c3d")
	}
}

// sweepSettled is one sweep over machines that decide nothing: a claim whose machine is deciding
// reads as one that has not answered (roundGroup's mask), so a sweep over a settled rig decides
// by the rule under test and by nothing else, and its outcome can be read once.
func (r *podUpdateRig) sweepSettled() {
	r.t.Helper()
	r.settleAll()
	r.tick()
}

// heldLines is every line the walk logged for a round that R1's hold keeps open
// (backgroundWaitLine), each checked to be of this rig's issue and round and to carry neither a
// working nor an unanswered list.
func heldLines(t *testing.T, rig *podUpdateRig) []map[string]any {
	t.Helper()
	lines := loggedMessages(t, rig.logs.String(), backgroundWaitLine)
	for _, line := range lines {
		if line["issue"] != podUpdateIssue || line["round"] != podUpdateRound {
			t.Errorf("the hold's line is %+v, want it of issue %s and round %s", line, podUpdateIssue, podUpdateRound)
		}
		for _, field := range []string{"working", "unanswered"} {
			if _, present := line[field]; present {
				t.Errorf("the hold's line carries %s, want no list but the hold's own: %+v", field, line)
			}
		}
	}
	return lines
}

// backgroundOf is the background field of a decoded log line: each role it names, with the kinds
// the role's report names, in the line's order. A line with no such field names no role.
func backgroundOf(line map[string]any) map[string][]string {
	field, _ := line["background"].(map[string]any)
	named := make(map[string][]string, len(field))
	for role, kinds := range field {
		listed, _ := kinds.([]any)
		named[role] = make([]string, len(listed))
		for i, kind := range listed {
			named[role][i] = fmt.Sprint(kind)
		}
	}
	return named
}

// unreportedOf is the unreported field of a decoded log line: the roles it names, in the line's
// order. A line with no such field names no role.
func unreportedOf(line map[string]any) []string {
	listed, _ := line["unreported"].([]any)
	names := make([]string, len(listed))
	for i, role := range listed {
		names[i] = fmt.Sprint(role)
	}
	return names
}

// wantHold holds one line that names R1's hold to exactly the roles of background, each with its
// kinds in that order, and exactly the roles of unreported, in that order. Each of the two fields
// is written only when it names a role, so a field that names none must be absent from the line.
func wantHold(t *testing.T, line map[string]any, background map[claim.Role][]string, unreported ...claim.Role) {
	t.Helper()
	if _, present := line["background"]; present != (len(background) > 0) {
		t.Errorf("the line carries background=%t, want it only where it names a role (%v): %+v", present, background, line)
	}
	if _, present := line["unreported"]; present != (len(unreported) > 0) {
		t.Errorf("the line carries unreported=%t, want it only where it names a role (%v): %+v", present, unreported, line)
	}
	want := make(map[string][]string, len(background))
	for role, kinds := range background {
		want[string(role)] = kinds
	}
	if got := backgroundOf(line); !maps.EqualFunc(got, want, slices.Equal[[]string]) {
		t.Errorf("the line's background = %v, want %v: %+v", got, want, line)
	}
	wantField(t, line, "unreported", unreported...)
}

// holdLinesNaming is every line of the sweep's log whose background or unreported field names
// role, whatever words the line is in: the walk's line, the replacement's line and the bound's
// line all name a role that R1's hold names in those two fields.
func holdLinesNaming(t *testing.T, rig *podUpdateRig, role claim.Role) []map[string]any {
	t.Helper()
	var naming []map[string]any
	for _, line := range loggedLines(t, rig.logs.String(), string(role)) {
		_, background := backgroundOf(line)[string(role)]
		if background || slices.Contains(unreportedOf(line), string(role)) {
			naming = append(naming, line)
		}
	}
	return naming
}

// A round that every asked claim has answered, and in which no turn runs, is not replaced before
// its bound while one role's report names a subagent that runs (R1, R5 case 1). The sweep runs
// when the round opens, half-way to the bound and a minute before the bound, and replaces nothing
// each time, and its line names the role that holds the round, with the kind its report names,
// and no other role.
//
// This is the loss of dispatch://BET-1064: the round ended on the answers while the implementer's
// report named running subagents. Before R1, the first sweep after the answers replaces the pod,
// and the first assertion fails.
func TestPodUpdateBackgroundSubagentHoldsTheRoundBeforeItsBound(t *testing.T) {
	rig := newPodUpdateRig(t, readyWorker(claim.RoleImplementer), readyWorker(claim.RoleTester))
	rig.openRoundAnsweredBy(claim.RoleImplementer, claim.RoleTester)
	rig.reportsBackgroundWork(claim.RoleImplementer, "subagent")
	rig.reportsNothingRunning(claim.RoleTester)

	for _, at := range []time.Duration{0, podUpdateGrace / 2, podUpdateGrace - time.Minute} {
		rig.clockAt(podUpdateStart.Add(at))
		rig.sweepSettled()
		if calls := rig.replacements(); len(calls) != 0 {
			t.Fatalf("at %s, before the bound %s, the pod was replaced %d times while the implementer's report named a subagent that runs",
				rig.clock.Now(), podUpdateBound, len(calls))
		}
	}

	lines := heldLines(t, rig)
	if len(lines) == 0 {
		t.Fatal("the sweep logged no line of the hold, want each sweep to name the implementer and its subagent")
	}
	for _, line := range lines {
		wantHold(t, line, map[claim.Role][]string{claim.RoleImplementer: {"subagent"}})
	}
}

// The same round ends at the first sweep after the report that nothing runs (R5 case 2): that
// report is all the hold waited for, so the pod is replaced there, before the bound, as on a round
// that its answers end, and no line says the bound made the replacement.
func TestPodUpdateBackgroundReportOfNothingRunningEndsTheRoundAtTheNextSweep(t *testing.T) {
	rig := newPodUpdateRig(t, readyWorker(claim.RoleImplementer), readyWorker(claim.RoleTester))
	clocks := rig.replacementClocks()
	rig.openRoundAnsweredBy(claim.RoleImplementer, claim.RoleTester)
	rig.reportsBackgroundWork(claim.RoleImplementer, "subagent")
	rig.reportsNothingRunning(claim.RoleTester)

	rig.sweepSettled()
	if calls := rig.replacements(); len(calls) != 0 {
		t.Fatalf("the pod was replaced %d times while the implementer's report named a subagent that runs", len(calls))
	}

	rig.clockAt(podUpdateStart.Add(10 * time.Minute))
	rig.reportsNothingRunning(claim.RoleImplementer)
	reported := rig.clock.Now()
	rig.sweepSettled()

	if calls := rig.replacements(); len(calls) != 1 {
		t.Fatalf("the pod was replaced %d times at the first sweep after the implementer reported that nothing runs, want once", len(calls))
	}
	if at := clocks(); len(at) != 1 || !at[0].Equal(reported) || !at[0].Before(podUpdateBound) {
		t.Fatalf("the replacements were made at %v, want one at %s, the sweep of the report, before the bound %s", at, reported, podUpdateBound)
	}
	if replaced := loggedLines(t, rig.logs.String(), podUpdateReplacedLine); len(replaced) != 1 || replaced[0]["atTheBound"] != false {
		t.Fatalf("the replacement was logged %+v, want one line that no bound made", replaced)
	}
	if lines := loggedLines(t, rig.logs.String(), podUpdateBoundLine); len(lines) != 0 {
		t.Fatalf("the sweep logged a bounded end %+v for a round that ended before its bound", lines)
	}
}

// A round with no background work ends on its answers, as before R1 (R5 case 3): each role
// reports that nothing runs, and the next sweep replaces the pod. No line of the hold is written,
// and no line says the bound made the replacement.
func TestPodUpdateBackgroundNoneLetsTheAnswersEndTheRound(t *testing.T) {
	rig := newPodUpdateRig(t, readyWorker(claim.RoleImplementer), readyWorker(claim.RoleTester))
	rig.openRoundAnsweredBy(claim.RoleImplementer, claim.RoleTester)
	rig.reportsNothingRunning(claim.RoleImplementer)
	rig.reportsNothingRunning(claim.RoleTester)

	rig.sweepSettled()

	if calls := rig.replacements(); len(calls) != 1 {
		t.Fatalf("the pod was replaced %d times at the sweep after every role answered and reported that nothing runs, want once", len(calls))
	}
	if lines := heldLines(t, rig); len(lines) != 0 {
		t.Fatalf("the sweep logged the hold %+v for a round in which no role reported background work", lines)
	}
	if replaced := loggedLines(t, rig.logs.String(), podUpdateReplacedLine); len(replaced) != 1 || replaced[0]["atTheBound"] != false {
		t.Fatalf("the replacement was logged %+v, want one line that no bound made", replaced)
	}
	if lines := loggedLines(t, rig.logs.String(), podUpdateBoundLine); len(lines) != 0 {
		t.Fatalf("the sweep logged a bounded end %+v for a round that its answers ended", lines)
	}
}

// Background work that outlives the bound holds the round to the bound and no further (R1, R4,
// R5 case 4). Before the bound nothing is replaced; at the bound the pod is replaced whatever
// runs. The replacement's line says the bound made it, and the bound's own line names the role
// that R1's hold named, with the kind its report names, apart from the claims that had not
// answered and the turns that ran, of which there were none.
func TestPodUpdateBackgroundThatOutlivesTheBoundEndsAtTheBound(t *testing.T) {
	rig := newPodUpdateRig(t, readyWorker(claim.RoleImplementer), readyWorker(claim.RoleTester))
	clocks := rig.replacementClocks()
	rig.openRoundAnsweredBy(claim.RoleImplementer, claim.RoleTester)
	rig.reportsBackgroundWork(claim.RoleImplementer, "subagent")
	rig.reportsNothingRunning(claim.RoleTester)

	for _, at := range []time.Duration{0, podUpdateGrace - time.Minute} {
		rig.clockAt(podUpdateStart.Add(at))
		rig.sweepSettled()
		if calls := rig.replacements(); len(calls) != 0 {
			t.Fatalf("at %s, before the bound %s, the pod was replaced %d times while the implementer's report named a subagent that runs",
				rig.clock.Now(), podUpdateBound, len(calls))
		}
	}

	rig.clockAt(podUpdateBound)
	rig.awaitReplacement(1, "at the bound, with the implementer's subagent still running")
	for _, at := range clocks() {
		if at.Before(podUpdateBound) {
			t.Fatalf("a replacement was made at %s, before the bound %s", at, podUpdateBound)
		}
	}
	if replaced := loggedLines(t, rig.logs.String(), podUpdateReplacedLine); len(replaced) != 1 || replaced[0]["atTheBound"] != true {
		t.Fatalf("the replacement was logged %+v, want one line that says the bound made it", replaced)
	}
	bounded := loggedLines(t, rig.logs.String(), podUpdateBoundLine)
	if len(bounded) != 1 {
		t.Fatalf("the bounded end was logged %d times, want once: %+v", len(bounded), bounded)
	}
	line := bounded[0]
	if line["round"] != podUpdateRound || line["pod"] != podUpdatePod || line["issue"] != podUpdateIssue {
		t.Errorf("the bounded end's line is %+v, want it of round %s, pod %s and issue %s", line, podUpdateRound, podUpdatePod, podUpdateIssue)
	}
	wantHold(t, line, map[claim.Role][]string{claim.RoleImplementer: {"subagent"}})
	wantField(t, line, "unanswered")
	wantField(t, line, "working")
	wantField(t, line, "answered", claim.RoleImplementer, claim.RoleTester)
}

// A report read before the claim's last turn started says nothing of that turn, so the claim
// holds the round as a claim with no report does (R1, R5 case 5). The tester reports that nothing
// runs; the clock moves on, and then a turn of the tester's own starts and ends, a turn that can
// have started work the earlier report does not name. Once the tester reports again after that
// turn, the pod is replaced.
func TestPodUpdateBackgroundReportOlderThanTheLastTurnHoldsTheRound(t *testing.T) {
	rig := newPodUpdateRig(t, readyWorker(claim.RoleImplementer), readyWorker(claim.RoleTester))
	rig.openRoundAnsweredBy(claim.RoleImplementer, claim.RoleTester)
	rig.reportsNothingRunning(claim.RoleImplementer)
	rig.reportsNothingRunning(claim.RoleTester)

	rig.clock.advance(time.Minute)
	rig.envoyTurn(claim.RoleTester)
	rig.clock.advance(time.Minute)
	rig.handle(claim.RoleTester, supervise.StreamTurnEnd{Claim: rig.tokens[claim.RoleTester]})
	if c := rig.claimOf(claim.RoleTester); c.State == supervise.StateWorking || c.UpdateSignalledAt == nil {
		t.Fatalf("the tester after its turn is %s answered=%t, want its turn over and its answer standing", c.State, c.UpdateSignalledAt != nil)
	}

	rig.sweepSettled()
	if calls := rig.replacements(); len(calls) != 0 {
		t.Fatalf("the pod was replaced %d times after the tester's turn ended, with no report from the tester since that turn started", len(calls))
	}
	lines := heldLines(t, rig)
	if len(lines) == 0 {
		t.Fatal("the sweep logged no line of the hold, want it to name the tester as unreported")
	}
	for _, line := range lines {
		wantHold(t, line, nil, claim.RoleTester)
	}

	rig.reportsNothingRunning(claim.RoleTester)
	rig.sweepSettled()

	rig.awaitReplacement(1, "once the tester reported after its turn that nothing runs")
}

// R1 weighs only a claim that can still run work in the round's pod, and a failed or a suspended
// claim runs none there, whatever its report says (R1, R5 case 6). The planner failed with a
// process still recorded in the pod, and the daemon suspended the tester after the tester
// answered; the report of each names work that runs. The round ends on the answers at the next
// sweep, and no line of the hold names either claim.
func TestPodUpdateBackgroundOfAFailedOrSuspendedClaimHoldsNothing(t *testing.T) {
	rig := newPodUpdateRig(t,
		readyWorker(claim.RoleImplementer),
		readyWorker(claim.RoleTester),
		podUpdateRole{role: claim.RolePlanner, state: supervise.StateFailed, session: true, inPod: true, contract: api.DaemonAPIVersion},
	)
	if c := rig.claimOf(claim.RolePlanner); c.State != supervise.StateFailed || podOf(c.Locator) != podUpdatePod {
		t.Fatalf("the planner is %s at %+v, want it failed with a process recorded in pod %s", c.State, c.Locator, podUpdatePod)
	}
	rig.openRoundAnsweredBy(claim.RoleImplementer, claim.RoleTester)
	rig.reportsNothingRunning(claim.RoleImplementer)
	rig.reportsBackgroundWork(claim.RoleTester, "async-job")
	rig.reportsBackgroundWork(claim.RolePlanner, "subagent")
	rig.suspendBy(claim.RoleTester, supervise.SuspendedByDaemon)
	if c := rig.claimOf(claim.RoleTester); c.UpdateRound != podUpdateRound || c.UpdateSignalledAt == nil {
		t.Fatalf("the suspended tester = round %q answered=%t, want the round open on it and its answer standing",
			c.UpdateRound, c.UpdateSignalledAt != nil)
	}

	rig.sweepSettled()

	if calls := rig.replacements(); len(calls) != 1 {
		t.Fatalf("the pod was replaced %d times at the sweep after the live roles answered, want once: a failed or suspended claim holds nothing", len(calls))
	}
	if refused := rig.notDue(); len(refused) != 0 {
		t.Fatalf("the last word refused the replacement %d times, want none: %+v", len(refused),
			loggedMessages(t, rig.logs.String(), backgroundNotDueLine))
	}
	for _, role := range []claim.Role{claim.RoleTester, claim.RolePlanner} {
		if lines := holdLinesNaming(t, rig, role); len(lines) != 0 {
			t.Errorf("the sweep named the %s in a hold: %+v", role, lines)
		}
	}
	if replaced := loggedLines(t, rig.logs.String(), podUpdateReplacedLine); len(replaced) != 1 || replaced[0]["atTheBound"] != false {
		t.Fatalf("the replacement was logged %+v, want one line that no bound made", replaced)
	}
}

// The daemon's last word under the pod's launch turn reads the reports again (R1, R5 case 7).
// Every role answered and reported that nothing runs, so the sweep found the round complete; while
// the runtime waited for the pod's launch turn, the implementer reported an async job. The last
// word refuses the replacement once, naming the implementer and its kind, nothing is replaced,
// and the round stays open. Once the implementer reports that nothing runs, the pod is replaced.
func TestPodUpdateBackgroundReportedInTheLaunchTurnRefusesTheReplacement(t *testing.T) {
	rig := newPodUpdateRig(t, readyWorker(claim.RoleImplementer), readyWorker(claim.RoleTester))
	rig.openRoundAnsweredBy(claim.RoleImplementer, claim.RoleTester)
	rig.reportsNothingRunning(claim.RoleImplementer)
	rig.reportsNothingRunning(claim.RoleTester)
	rig.clock.advance(time.Minute)

	// The runtime holds where it waits for the pod's launch turn, and the implementer's session
	// reports an async job inside that wait. One hold only: the next call finds the pod as the
	// sweep left it.
	rig.rt.HoldReplacePod(func() {
		rig.rt.HoldReplacePod(nil)
		rig.reportsBackgroundWork(claim.RoleImplementer, "async-job")
	})
	rig.sweepSettled()

	if calls := rig.replacements(); len(calls) != 0 {
		t.Fatalf("the pod was replaced %d times although the implementer reported an async job inside the pod's launch turn", len(calls))
	}
	if refused := rig.notDue(); len(refused) != 1 {
		t.Fatalf("the runtime was told the replacement was no longer due %d times, want once", len(refused))
	}
	if c := rig.claimOf(claim.RoleImplementer); c.UpdateRound != podUpdateRound || c.UpdateSignalledAt == nil {
		t.Fatalf("the implementer after the refusal = round %q answered=%t, want the round open and its answer standing",
			c.UpdateRound, c.UpdateSignalledAt != nil)
	}
	refusals := loggedMessages(t, rig.logs.String(), backgroundNotDueLine)
	if len(refusals) != 1 {
		t.Fatalf("the refusal was logged %d times, want once: %+v", len(refusals), refusals)
	}
	if reason := fmt.Sprint(refusals[0]["reason"]); !strings.Contains(reason, string(claim.RoleImplementer)) || !strings.Contains(reason, "async-job") {
		t.Errorf("the refusal's reason is %q, want it to name the implementer and the async job its report names", reason)
	}

	rig.clock.advance(time.Minute)
	rig.reportsNothingRunning(claim.RoleImplementer)
	rig.sweepSettled()

	rig.awaitReplacement(1, "once the implementer reported that nothing runs")
}

// The line of a held reading names each claim that holds the round and nothing else (R4, R5 case
// 8): the implementer, with the kinds its report names in the order the session sent them, and
// the tester, whose report predates its last turn, as a role with no report since that turn. The
// reviewer reported that nothing runs and is named nowhere, and the line carries no list of the
// answers or the turns.
func TestPodUpdateBackgroundHeldLineNamesEachRoleAndItsKinds(t *testing.T) {
	roles := []claim.Role{claim.RoleImplementer, claim.RoleTester, claim.RoleReviewer}
	rig := newPodUpdateRig(t, readyWorker(claim.RoleImplementer), readyWorker(claim.RoleTester), readyWorker(claim.RoleReviewer))
	rig.openRoundAnsweredBy(roles...)
	rig.reportsNothingRunning(claim.RoleTester)
	rig.reportsNothingRunning(claim.RoleReviewer)

	rig.clock.advance(time.Minute)
	rig.envoyTurn(claim.RoleTester)
	rig.clock.advance(time.Minute)
	rig.handle(claim.RoleTester, supervise.StreamTurnEnd{Claim: rig.tokens[claim.RoleTester]})
	rig.reportsBackgroundWork(claim.RoleImplementer, "async-job", "subagent")

	rig.sweepSettled()
	if calls := rig.replacements(); len(calls) != 0 {
		t.Fatalf("the pod was replaced %d times while the implementer reported background work and the tester had no report since its last turn", len(calls))
	}
	lines := heldLines(t, rig)
	if len(lines) == 0 {
		t.Fatal("the sweep logged no line of the hold, want it to name the implementer with its kinds and the tester as unreported")
	}
	for _, line := range lines {
		wantHold(t, line, map[claim.Role][]string{claim.RoleImplementer: {"async-job", "subagent"}}, claim.RoleTester)
	}
	if named := holdLinesNaming(t, rig, claim.RoleReviewer); len(named) != 0 {
		t.Errorf("the sweep named the reviewer, which reported that nothing runs, in a hold: %+v", named)
	}
}

// A claim with a process in the round's pod and no activity report at all (activity_at null)
// holds the round (R1, R5 case 9): a session that cannot report degrades to the bound, not to the
// loss of its work. The line names it as unreported, and its first report that nothing runs ends
// the round.
func TestPodUpdateBackgroundNoReportAtAllHoldsTheRound(t *testing.T) {
	rig := newPodUpdateRig(t, readyWorker(claim.RoleImplementer), readyWorker(claim.RoleTester))
	rig.openRoundAnsweredBy(claim.RoleImplementer, claim.RoleTester)
	rig.reportsNothingRunning(claim.RoleTester)

	rig.sweepSettled()
	if calls := rig.replacements(); len(calls) != 0 {
		t.Fatalf("the pod was replaced %d times while the implementer had sent no report at all", len(calls))
	}
	lines := heldLines(t, rig)
	if len(lines) == 0 {
		t.Fatal("the sweep logged no line of the hold, want it to name the implementer as unreported")
	}
	for _, line := range lines {
		wantHold(t, line, nil, claim.RoleImplementer)
	}

	rig.clock.advance(time.Minute)
	rig.reportsNothingRunning(claim.RoleImplementer)
	rig.sweepSettled()

	rig.awaitReplacement(1, "once the implementer sent its first report, that nothing runs")
}

// A claim whose machine's view is of a later launch than the store's row the sweep read is read
// as a claim with no report (R1, R5 case 10). The sweep's read is minutes old: the implementer's
// row in it says that nothing runs, and since that read the daemon suspended the implementer and
// resumed it in the round's pod, a launch that cleared the report. The walk over the older read
// takes the report from the row only at the view's own generation, so it holds the round with the
// implementer unreported, rather than end the round on the report of the process that went. The
// relaunched process's first report that nothing runs ends the round.
func TestPodUpdateBackgroundViewOfALaterLaunchReadsAsNoReport(t *testing.T) {
	rig := newPodUpdateRig(t, readyWorker(claim.RoleImplementer), readyWorker(claim.RoleTester))
	rig.openRoundAnsweredBy(claim.RoleImplementer, claim.RoleTester)
	rig.reportsNothingRunning(claim.RoleImplementer)
	rig.reportsNothingRunning(claim.RoleTester)
	rig.settleAll()

	// The group this tick carries, read while the implementer's row holds its report. Minutes
	// later in that same tick the daemon suspends the implementer, and it is resumed in the
	// round's own pod, so the live read finds a later launch than that row.
	group := rig.snapshot()
	read := rig.claimOf(claim.RoleImplementer).Generation
	rig.suspendBy(claim.RoleImplementer, supervise.SuspendedByDaemon)
	rig.resumeInPod(claim.RoleImplementer)
	rig.settleAll()
	if c := rig.claimOf(claim.RoleImplementer); c.Generation <= read || c.UpdateSignalledAt == nil {
		t.Fatalf("the resumed implementer is at generation %d answered=%t, want a launch after generation %d with its answer standing",
			c.Generation, c.UpdateSignalledAt != nil, read)
	}

	rig.s.reconcilePodUpdates(rig.ctx, group)

	if calls := rig.replacements(); len(calls) != 0 {
		t.Fatalf("the pod was replaced %d times over a relaunch of the implementer that has sent no report", len(calls))
	}
	lines := heldLines(t, rig)
	if len(lines) != 1 {
		t.Fatalf("the walk over the older read logged the hold %d times, want once naming the implementer as unreported: %+v", len(lines), lines)
	}
	wantHold(t, lines[0], nil, claim.RoleImplementer)

	rig.reportsNothingRunning(claim.RoleImplementer)
	rig.awaitReplacement(1, "once the relaunched implementer reported that nothing runs")
}

// A claim whose process is in another pod than the round's holds nothing (R1, R5 case 11). After
// the sweep's read, the tester is displaced into the pod that replaced the round's own, so its
// relaunch has sent no report. A claim with no report in the round's pod would hold the round,
// but one in another pod holds nothing. The walk over that read finds the live claims in two
// pods, which holds the round as a replacement in flight (waitsForPodInFlight), and no line of
// the hold names the tester. The case of a running report in another pod is read in
// podupdate_background_reading_test.go.
func TestPodUpdateBackgroundOfAClaimInAnotherPodHoldsNothing(t *testing.T) {
	rig := newPodUpdateRig(t, readyWorker(claim.RoleImplementer), readyWorker(claim.RoleTester))
	rig.openRoundAnsweredBy(claim.RoleImplementer, claim.RoleTester)
	rig.reportsNothingRunning(claim.RoleImplementer)
	rig.reportsNothingRunning(claim.RoleTester)
	rig.settleAll()

	group := rig.snapshot()
	rig.displacedInto(claim.RoleTester, podUpdateReplacementPod)
	rig.settleAll()

	rig.s.reconcilePodUpdates(rig.ctx, group)

	if calls := rig.replacements(); len(calls) != 0 {
		t.Fatalf("the pod was replaced %d times with the issue's claims in two pods", len(calls))
	}
	twoPods := loggedMessages(t, rig.logs.String(), backgroundTwoPodsLine)
	if len(twoPods) != 1 || twoPods[0]["round"] != podUpdateRound {
		t.Fatalf("the walk logged the two pods %+v, want one line of round %s", twoPods, podUpdateRound)
	}
	if lines := holdLinesNaming(t, rig, claim.RoleTester); len(lines) != 0 {
		t.Fatalf("the sweep named the tester, whose process is in another pod, in a hold: %+v", lines)
	}
}

// A claim in the round's pod that the round has not asked holds the round too (R1, R5 case 12).
// The planner is still launching there with no session, so the round neither asks it nor waits
// for its answer (askedClaims): only its report can hold the round, and that report names a
// subagent. The line names the planner with that kind, and the planner's report that nothing
// runs ends the round.
func TestPodUpdateBackgroundOfAClaimTheRoundHasNotAskedHoldsTheRound(t *testing.T) {
	rig := newPodUpdateRig(t,
		readyWorker(claim.RoleImplementer),
		readyWorker(claim.RoleTester),
		podUpdateRole{role: claim.RolePlanner, state: supervise.StateLaunching, inPod: true, contract: api.DaemonAPIVersion},
	)
	if c := rig.claimOf(claim.RolePlanner); c.State != supervise.StateLaunching || c.Session != "" || podOf(c.Locator) != podUpdatePod {
		t.Fatalf("the planner is %s with session %q at %+v, want it launching with no session in pod %s", c.State, c.Session, c.Locator, podUpdatePod)
	}
	rig.openRoundAnsweredBy(claim.RoleImplementer, claim.RoleTester)
	rig.reportsNothingRunning(claim.RoleImplementer)
	rig.reportsNothingRunning(claim.RoleTester)
	rig.reportsBackgroundWork(claim.RolePlanner, "subagent")

	rig.sweepSettled()
	if calls := rig.replacements(); len(calls) != 0 {
		t.Fatalf("the pod was replaced %d times while the planner, which the round has not asked, reported a subagent that runs in the pod", len(calls))
	}
	if c := rig.claimOf(claim.RolePlanner); c.UpdateRound != "" {
		t.Fatalf("the planner records the round %q, want it in no round: the round asks no claim with no session", c.UpdateRound)
	}
	lines := heldLines(t, rig)
	if len(lines) == 0 {
		t.Fatal("the sweep logged no line of the hold, want it to name the planner and its subagent")
	}
	for _, line := range lines {
		wantHold(t, line, map[claim.Role][]string{claim.RolePlanner: {"subagent"}})
	}

	rig.clock.advance(time.Minute)
	rig.reportsNothingRunning(claim.RolePlanner)
	rig.sweepSettled()

	rig.awaitReplacement(1, "once the planner reported that nothing runs")
}

// The round reads the report from the store's row, not from the claim's machine
// (dispatch://INFRA-4203/spec, Design, "The round reads the report from the store"). The
// implementer reports a subagent, and the daemon restarts: each machine boots holding the report
// it read then. The activity route writes the store and posts nothing to a machine, so the later
// report that nothing runs reaches the row alone, and the restarted daemon replaces the pod on
// it, before the bound. A round that read the machine's view would wait for the bound. The
// restarted supervision logs to a quiet logger, so the test reads the replacements.
func TestPodUpdateBackgroundIsReadFromTheStoreAcrossARestart(t *testing.T) {
	rig := newPodUpdateRig(t, readyWorker(claim.RoleImplementer), readyWorker(claim.RoleTester))
	rig.openRoundAnsweredBy(claim.RoleImplementer, claim.RoleTester)
	rig.reportsBackgroundWork(claim.RoleImplementer, "subagent")
	rig.reportsNothingRunning(claim.RoleTester)
	rig.settleAll()

	rig = rig.restart()
	rig.drifts(podUpdateDrift)
	clocks := rig.replacementClocks()
	if c := rig.claimOf(claim.RoleImplementer); !c.ActivityBackground {
		t.Fatalf("the implementer's machine booted with the report at %s background=%t kinds=%v, want the report of the subagent it read at its boot",
			c.ActivityAt, c.ActivityBackground, c.ActivityKinds)
	}

	rig.clock.advance(time.Minute)
	rig.reportsNothingRunning(claim.RoleImplementer)
	if c := rig.claimOf(claim.RoleImplementer); !c.ActivityBackground {
		t.Fatalf("the implementer's machine holds the report at %s background=%t kinds=%v after the report that nothing runs, want the report of its boot still",
			c.ActivityAt, c.ActivityBackground, c.ActivityKinds)
	}

	rig.awaitReplacement(1, "once the store's row of the implementer said that nothing runs")
	for _, at := range clocks() {
		if !at.Before(podUpdateBound) {
			t.Fatalf("the replacement was made at %s, want it before the bound %s: the store said that nothing runs", at, podUpdateBound)
		}
	}
}
