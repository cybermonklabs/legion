package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// news posts the report of c's plugin that news reached its session after the declaration of the
// wait id names.
func (h *waitHarness) news(c liveClaim, id string) *httptest.ResponseRecorder {
	h.t.Helper()
	return h.request(http.MethodPost, "/legion/v1/claims/wait-news", WaitNewsRequest{ClaimToken: c.token,
		SessionID: c.session, Secret: c.secret, WaitID: id}, nil)
}

// newsAnswer asserts recorder is a news report's 200 and answers its body.
func newsAnswer(t *testing.T, recorder *httptest.ResponseRecorder) WaitNewsResponse {
	t.Helper()
	if recorder.Code != http.StatusOK {
		t.Fatalf("news = %d, want 200; body %s", recorder.Code, recorder.Body)
	}
	var answer WaitNewsResponse
	decodeInto(t, recorder, &answer)
	return answer
}

// newsApplied is whether a news report of wait id was applied as a fact: ApplyFact marks the
// report's event id, `wait-news:<claim>:<id>` from the source `api`, as processed in the fact's own
// transaction.
func newsApplied(t *testing.T, h *harness, id string) bool {
	t.Helper()
	var applied bool
	if err := h.store.Pool().QueryRow(context.Background(), `select exists (select 1 from processed_events
		where source = 'api' and event_id like 'wait-news:%:' || $1)`, id).Scan(&applied); err != nil {
		t.Fatalf("read whether the news of %s was applied: %v", id, err)
	}
	return applied
}

// endedLines is how many `wait: ended` lines logs holds for wait id.
func endedLines(logs, id string) int {
	n := 0
	for line := range strings.SplitSeq(logs, "\n") {
		if strings.Contains(line, `msg="wait: ended"`) && strings.Contains(line, " wait="+id+" ") {
			n++
		}
	}
	return n
}

// sameEnd says whether a and b are one wait with one end, or none.
func sameEnd(a, b record.ClaimWait) bool {
	return a.ID == b.ID && a.EndedAt.Equal(b.EndedAt) && a.EndCause == b.EndCause && a.EndedBy == b.EndedBy
}

// openRound opens the pod update round testUpdateRound over c's claim, as the daemon's sweep posts
// it (supervise.PodUpdateOpen).
func (h *waitHarness) openRound(t *testing.T, c liveClaim) {
	t.Helper()
	machine, _ := h.supervisor.Machine(c.token)
	if err := machine.Handle(h.ctx, supervise.PodUpdateOpen{Claim: c.token, Round: testUpdateRound, Reasons: "image"}); err != nil {
		t.Fatalf("open the update round over %s: %v", c.token, err)
	}
	if round := machine.Claim().UpdateRound; round != testUpdateRound {
		t.Fatalf("the claim is in round %q after the round opened, want %q", round, testUpdateRound)
	}
}

// endRound ends testUpdateRound over c's claim, as the sweep posts the end once the round's pod is
// gone (supervise.PodUpdateEnded).
func (h *waitHarness) endRound(t *testing.T, c liveClaim) {
	t.Helper()
	machine, _ := h.supervisor.Machine(c.token)
	if err := machine.Handle(h.ctx, supervise.PodUpdateEnded{Claim: c.token, Round: testUpdateRound}); err != nil {
		t.Fatalf("end the update round over %s: %v", c.token, err)
	}
	if round := machine.Claim().UpdateRound; round != "" {
		t.Fatalf("the claim is in round %q after the round ended, want none", round)
	}
}

// The plugin's report that news reached the session after the declaration ends the wait it names
// (dispatch://INFRA-2040 R3's second end): the answer says ended, the wait is ended with the cause
// that news reached the session, by the report's own fact, and no longer stands, and the end is
// logged with the claim, the issue, the role, the wait, the cause and what ended it.
//
// A second report of the same wait — the plugin sends a report again when it has no answer in
// time (D8 of dispatch://INFRA-2040's plan) — answers not ended and changes nothing: ApplyFact
// answers its event id as a duplicate before any handler runs, since the first report ended the
// wait.
func TestWaitNewsEndsTheWaitItNamesAndASecondReportEndsNothing(t *testing.T) {
	h := newWaitHarness(t)
	h.waitIssue(phase.Implementing, nil)
	c := h.waitingWorker(t, claim.RoleImplementer)
	wait := declared(t, h.declare(c, "the review of LEGION-207's pull request", waitNow.Add(time.Hour)))

	if answer := newsAnswer(t, h.news(c, wait.ID)); answer != (WaitNewsResponse{Ended: true}) {
		t.Fatalf("the report's answer = %+v, want ended and no round", answer)
	}
	endedBy := "api/wait-news:" + string(c.token) + ":" + wait.ID
	rows := waitRows(t, h.harness, c.token)
	if len(rows) != 1 || rows[0].ID != wait.ID || rows[0].EndCause != workflow.WaitEndNews || rows[0].EndedBy != endedBy ||
		rows[0].EndedAt.IsZero() {
		t.Fatalf("claim_waits holds %+v, want wait %s ended with cause %q by %s", rows, wait.ID, workflow.WaitEndNews, endedBy)
	}
	if waitStands(t, h.harness, c, claim.RoleImplementer) {
		t.Fatal("the wait still stands after the news ended it")
	}
	wantLogged(t, logLine(t, h.logs, "wait: ended"), "claim="+string(c.token), "issue=LEGION-208", "role=implementer",
		"wait="+wait.ID, "cause="+strconv.Quote(workflow.WaitEndNews), "ended_by="+endedBy)

	if answer := newsAnswer(t, h.news(c, wait.ID)); answer != (WaitNewsResponse{}) {
		t.Fatalf("the second report's answer = %+v, want not ended: the first report ended the wait", answer)
	}
	if again := waitRows(t, h.harness, c.token); len(again) != 1 || !sameEnd(again[0], rows[0]) {
		t.Fatalf("claim_waits holds %+v after the second report, want %+v as the first report ended it", again, rows[0])
	}
	if n := endedLines(h.logs.String(), wait.ID); n != 1 {
		t.Fatalf("the end of wait %s was logged %d times, want once; the log is:\n%s", wait.ID, n, h.logs)
	}
}

// A wait's id is in the record every role of the tree reads (read_record), so another claim can
// name it. Such a report ends no wait — the end matches the claim as well as the id — and it marks
// no event the wait's own claim reports under: the event id names the reporting claim beside the
// wait, so the owner's own report afterwards is applied and ends the wait, rather than being
// answered as a duplicate while the wait stands until its time.
func TestWaitNewsFromAnotherClaimNeitherEndsTheWaitNorBlocksItsOwnersReport(t *testing.T) {
	h := newWaitHarness(t)
	h.waitIssue(phase.Implementing, nil)
	c := h.waitingWorker(t, claim.RoleImplementer)
	other := newLiveClaim(t, h.harness, "LEGION-208", claim.RoleTester)
	wait := declared(t, h.declare(c, "the review of LEGION-207's pull request", waitNow.Add(time.Hour)))

	if answer := newsAnswer(t, h.news(other, wait.ID)); answer != (WaitNewsResponse{}) {
		t.Fatalf("the tester's report of the implementer's wait = %+v, want not ended", answer)
	}
	if !waitStands(t, h.harness, c, claim.RoleImplementer) {
		t.Fatal("the implementer's wait does not stand after another claim reported its id")
	}
	if answer := newsAnswer(t, h.news(c, wait.ID)); answer != (WaitNewsResponse{Ended: true}) {
		t.Fatalf("the implementer's own report after the tester's = %+v, want ended", answer)
	}
	if rows := waitRows(t, h.harness, c.token); len(rows) != 1 || rows[0].EndCause != workflow.WaitEndNews {
		t.Fatalf("claim_waits holds %+v, want wait %s ended with cause %q by its own claim's report", rows, wait.ID, workflow.WaitEndNews)
	}
}

// A report naming a wait that a newer declaration replaced changes nothing: the end is a
// compare-and-set on the claim and the wait's id, so the replaced wait keeps the end the
// replacement gave it, the newer wait still stands, the answer says not ended, and no end is
// logged for the report.
func TestWaitNewsNamingAReplacedWaitChangesNothing(t *testing.T) {
	h := newWaitHarness(t)
	h.waitIssue(phase.Implementing, nil)
	c := h.waitingWorker(t, claim.RoleImplementer)
	first := declared(t, h.declare(c, "the review of the root's pull request", waitNow.Add(time.Hour)))
	second := declared(t, h.declare(c, "CI on the rebased head", waitNow.Add(3*time.Hour)))
	before := waitRows(t, h.harness, c.token)

	if answer := newsAnswer(t, h.news(c, first.ID)); answer != (WaitNewsResponse{}) {
		t.Fatalf("the report's answer = %+v, want not ended: wait %s was replaced", answer, first.ID)
	}
	after := waitRows(t, h.harness, c.token)
	if len(after) != 2 || !sameEnd(after[0], before[0]) || !sameEnd(after[1], before[1]) {
		t.Fatalf("claim_waits holds %+v after the report, want %+v unchanged", after, before)
	}
	if after[0].EndCause != record.WaitReplaced || after[1].ID != second.ID || !after[1].EndedAt.IsZero() {
		t.Fatalf("claim_waits holds %+v, want the first wait replaced and the second standing", after)
	}
	if !waitStands(t, h.harness, c, claim.RoleImplementer) {
		t.Fatal("the newer wait does not stand after a report naming the one it replaced")
	}
	if strings.Contains(h.logs.String(), "cause="+strconv.Quote(workflow.WaitEndNews)) {
		t.Fatalf("an end for news was logged for a replaced wait; the log is:\n%s", h.logs)
	}
}

// While the claim's update round is open, a news report ends nothing (D9 of dispatch://INFRA-2040's
// plan): the round's own ask reaches the session as a message to its role, which the plugin
// reports as news. The route reads the round from its one read of the claim before it applies
// anything, answers not ended with the round, records nothing — no end, no applied fact — and
// logs it with the claim, the issue, the role, the wait and the round. The same report sent once
// the round has ended is applied, and ends the wait.
func TestWaitNewsDuringAnUpdateRoundEndsNothingUntilTheRoundEnds(t *testing.T) {
	h := newWaitHarness(t)
	h.waitIssue(phase.Implementing, nil)
	c := h.waitingWorker(t, claim.RoleImplementer)
	wait := declared(t, h.declare(c, "the review of LEGION-207's pull request", waitNow.Add(time.Hour)))
	h.openRound(t, c)

	if answer := newsAnswer(t, h.news(c, wait.ID)); answer != (WaitNewsResponse{Round: testUpdateRound}) {
		t.Fatalf("the report's answer during the round = %+v, want not ended, naming round %s", answer, testUpdateRound)
	}
	if rows := waitRows(t, h.harness, c.token); len(rows) != 1 || !rows[0].EndedAt.IsZero() {
		t.Fatalf("claim_waits holds %+v after a report during the round, want wait %s with no end", rows, wait.ID)
	}
	if !waitStands(t, h.harness, c, claim.RoleImplementer) {
		t.Fatal("the wait does not stand after a report during the update round")
	}
	if newsApplied(t, h.harness, wait.ID) {
		t.Fatal("the report during the round was applied as a fact; it must record nothing")
	}
	wantLogged(t, logLine(t, h.logs, "wait: news during an update round ends nothing"), "claim="+string(c.token),
		"issue=LEGION-208", "role=implementer", "wait="+wait.ID, "round="+testUpdateRound)

	h.endRound(t, c)

	if answer := newsAnswer(t, h.news(c, wait.ID)); answer != (WaitNewsResponse{Ended: true}) {
		t.Fatalf("the report's answer once the round ended = %+v, want ended and no round", answer)
	}
	if rows := waitRows(t, h.harness, c.token); len(rows) != 1 || rows[0].EndCause != workflow.WaitEndNews {
		t.Fatalf("claim_waits holds %+v once the round ended, want wait %s ended with cause %q", rows, wait.ID, workflow.WaitEndNews)
	}
}

// The update round's relaunch of the same session leaves the wait standing: the round's pod goes,
// the claim's process is found gone and launched again at the next launch generation on the
// session it had, which registers again, and the round then ends (R3: "an update round and its
// task" and "a relaunch of the same session" end no wait). A report the relaunched plugin sends
// during the round ends nothing; one it sends once the round has ended ends the wait.
func TestWaitStandsThroughTheUpdateRoundsRelaunchOfTheSameSession(t *testing.T) {
	h := newWaitHarness(t)
	h.waitIssue(phase.Implementing, nil)
	c := h.waitingWorker(t, claim.RoleImplementer)
	wait := declared(t, h.declare(c, "the review of LEGION-207's pull request", waitNow.Add(time.Hour)))
	h.openRound(t, c)
	before := h.stored(c.token)

	relaunched := c
	relaunched.secret = h.registered(h.relaunch(c.token), c.session).Secret

	after := h.stored(c.token)
	if after.Generation <= before.Generation || after.Session != c.session {
		t.Fatalf("the claim after the round's relaunch is at generation %d in session %q, want past %d in %q",
			after.Generation, after.Session, before.Generation, c.session)
	}
	if answer := newsAnswer(t, h.news(relaunched, wait.ID)); answer != (WaitNewsResponse{Round: testUpdateRound}) {
		t.Fatalf("the relaunched plugin's report during the round = %+v, want not ended, naming round %s", answer, testUpdateRound)
	}
	h.endRound(t, c)

	if rows := waitRows(t, h.harness, c.token); len(rows) != 1 || rows[0].ID != wait.ID || !rows[0].EndedAt.IsZero() ||
		rows[0].Session != c.session {
		t.Fatalf("claim_waits holds %+v after the round, want wait %s as it was declared, with no end", rows, wait.ID)
	}
	if !waitStands(t, h.harness, c, claim.RoleImplementer) {
		t.Fatal("the wait does not stand after the update round's relaunch of the session that declared it")
	}

	if answer := newsAnswer(t, h.news(relaunched, wait.ID)); answer != (WaitNewsResponse{Ended: true}) {
		t.Fatalf("the relaunched plugin's report once the round ended = %+v, want ended", answer)
	}
}

// A news report is the declaring session's own, authenticated as the wait route authenticates a
// declaration: a report from another session than the one the claim records is refused as
// ready-for-update's fence refuses one, a wrong secret as every claim request refuses one, and a
// report naming no wait by its name. None records anything, and the wait stands.
func TestWaitNewsIsTheDeclaringSessionsOwn(t *testing.T) {
	h := newWaitHarness(t)
	h.waitIssue(phase.Implementing, nil)
	c := h.waitingWorker(t, claim.RoleImplementer)
	wait := declared(t, h.declare(c, "the review of LEGION-207's pull request", waitNow.Add(time.Hour)))

	another := c
	another.session = "ses_someone_else"
	wantRefusal(t, h.news(another, wait.ID), claim.SameAgentRefusal.Status, claim.SameAgentRefusal.Message)
	wrong := c
	wrong.secret = "not-the-secret"
	wantRefusal(t, h.news(wrong, wait.ID), claim.InvalidSecret.Status, claim.InvalidSecret.Message)
	wantRefusal(t, h.news(c, " "), http.StatusBadRequest, "waitId is required")

	if rows := waitRows(t, h.harness, c.token); len(rows) != 1 || !rows[0].EndedAt.IsZero() {
		t.Fatalf("claim_waits holds %+v after the refused reports, want wait %s with no end", rows, wait.ID)
	}
	if newsApplied(t, h.harness, wait.ID) {
		t.Fatal("a refused report was applied as a fact")
	}
}
