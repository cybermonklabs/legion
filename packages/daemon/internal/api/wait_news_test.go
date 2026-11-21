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
// wait id names: a person's message, which carries no delivery key.
func (h *waitHarness) news(c liveClaim, id string) *httptest.ResponseRecorder {
	h.t.Helper()
	return h.delivered(c, id, "")
}

// delivered posts the report of c's plugin that a delivery under key reached its session after
// the declaration of the wait id names, the key verbatim as the plugin read it off the delivery.
func (h *waitHarness) delivered(c liveClaim, id, key string) *httptest.ResponseRecorder {
	h.t.Helper()
	return h.request(http.MethodPost, "/legion/v1/claims/wait-news", WaitNewsRequest{ClaimToken: c.token,
		SessionID: c.session, Secret: c.secret, WaitID: id, DeliveryKey: key}, nil)
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

// updateAskKey is the key the daemon publishes the ask of the round c's claim is in under
// (record.UpdateAskKey, the daemon's updateAsk): of the claim, its round and the opening it
// records for that round.
func (h *waitHarness) updateAskKey(t *testing.T, c liveClaim) string {
	t.Helper()
	machine, _ := h.supervisor.Machine(c.token)
	held := machine.Claim()
	if held.UpdateRound == "" || held.UpdateOpenedAt == nil {
		t.Fatalf("the claim %s records round %q opened at %v, want a round with its opening", c.token, held.UpdateRound, held.UpdateOpenedAt)
	}
	return record.UpdateAskKey(c.token, held.UpdateRound, held.UpdateOpenedAt)
}

// noNewsLines is every `wait: the update ask is no news` line logs holds for wait id under key.
func noNewsLines(logs, id, key string) []string {
	var lines []string
	for line := range strings.SplitSeq(logs, "\n") {
		if strings.Contains(line, `msg="wait: the update ask is no news"`) && strings.Contains(line, " wait="+id+" ") &&
			strings.HasSuffix(line, " key="+key) {
			lines = append(lines, line)
		}
	}
	return lines
}

// wantNoNews asserts that the report recorder answered is the claim's own update ask under key:
// the answer is not ended and names the update ask, wait id has no end and still stands, the
// report was applied as no fact, and it was logged once more, with the claim, the issue, the role,
// the wait and the key, than the logged lines given.
func (h *waitHarness) wantNoNews(t *testing.T, c liveClaim, id, key string, recorder *httptest.ResponseRecorder, logged int) {
	t.Helper()
	if answer := newsAnswer(t, recorder); answer != (WaitNewsResponse{UpdateAsk: true}) {
		t.Fatalf("the report of the claim's own update ask under %s = %+v, want not ended, the update ask", key, answer)
	}
	if rows := waitRows(t, h.harness, c.token); len(rows) != 1 || rows[0].ID != id || !rows[0].EndedAt.IsZero() {
		t.Fatalf("claim_waits holds %+v after the report of the update ask, want wait %s with no end", rows, id)
	}
	if !waitStands(t, h.harness, c, claim.RoleImplementer) {
		t.Fatalf("the wait does not stand after the report of the update ask under %s", key)
	}
	if newsApplied(t, h.harness, id) {
		t.Fatalf("the report of the update ask under %s was applied as a fact; it must record nothing", key)
	}
	lines := noNewsLines(h.logs.String(), id, key)
	if len(lines) != logged+1 {
		t.Fatalf("the report of the update ask under %s was logged %d times, want %d; the log is:\n%s", key, len(lines), logged+1, h.logs)
	}
	wantLogged(t, lines[logged], "level=INFO", "claim="+string(c.token), "issue=LEGION-208", "role=implementer")
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
		t.Fatalf("the report's answer = %+v, want ended, and not the update ask", answer)
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

// A report whose delivery key is the claim's own update ask ends nothing (dispatch://INFRA-2040
// D9): the round's ask reaches the session as a message to its role, which the plugin reports with
// the delivery's key, and an update round and its ask end no wait (R3). The key is the one the
// daemon published the ask under, bare as the listener keeps it and behind the role-forward mark
// a host may hand it on under. Each report answers not ended and names the update ask, records
// nothing — no end, no applied fact — and is logged with the claim, the issue, the role, the wait
// and the key. The same report sent again once the round has ended, as the plugin sends a report
// that had no answer in time, is still the ask and ends nothing, and the wait then still ends for
// news.
func TestWaitNewsOfTheClaimsOwnUpdateAskEndsNothingDuringItsRoundOrAfter(t *testing.T) {
	h := newWaitHarness(t)
	h.waitIssue(phase.Implementing, nil)
	c := h.waitingWorker(t, claim.RoleImplementer)
	wait := declared(t, h.declare(c, "the review of LEGION-207's pull request", waitNow.Add(time.Hour)))
	h.openRound(t, c)
	key := h.updateAskKey(t, c)
	forwarded := "envoy.role.forward." + key

	h.wantNoNews(t, c, wait.ID, key, h.delivered(c, wait.ID, key), 0)
	h.wantNoNews(t, c, wait.ID, forwarded, h.delivered(c, wait.ID, forwarded), 0)

	h.endRound(t, c)

	h.wantNoNews(t, c, wait.ID, key, h.delivered(c, wait.ID, key), 1)
	h.wantNoNews(t, c, wait.ID, forwarded, h.delivered(c, wait.ID, forwarded), 1)

	if answer := newsAnswer(t, h.news(c, wait.ID)); answer != (WaitNewsResponse{Ended: true}) {
		t.Fatalf("a person's message after the update ask's reports = %+v, want ended", answer)
	}
	if rows := waitRows(t, h.harness, c.token); len(rows) != 1 || rows[0].EndCause != workflow.WaitEndNews {
		t.Fatalf("claim_waits holds %+v, want wait %s ended with cause %q", rows, wait.ID, workflow.WaitEndNews)
	}
}

// While the claim's update round is open, every report but one of the claim's own update ask is
// news and ends the wait (R3 whole, dispatch://INFRA-2040 D9): a delivery under a key the listener
// minted, the same behind the role-forward mark, a role message under an outbox row's key, a
// person's message with no key, and the ask of the same round to another claim of the issue,
// whose key carries that claim's tag and not this one's. Each answer says ended and names no
// update ask, the wait is ended by the report's own fact, and no line calls it the update ask.
func TestWaitNewsDuringAnUpdateRoundEndsTheWaitForEveryKeyButTheClaimsOwnAsk(t *testing.T) {
	for _, tc := range []struct {
		name string
		key  func(h *waitHarness, c liveClaim) string
	}{
		{name: "a key the listener minted", key: func(*waitHarness, liveClaim) string {
			return "publish.0123456789abcdef0123456789abcdef"
		}},
		{name: "a minted key behind the role-forward mark", key: func(*waitHarness, liveClaim) string {
			return "envoy.role.forward.publish.0123456789abcdef0123456789abcdef"
		}},
		{name: "an outbox row's key", key: func(*waitHarness, liveClaim) string { return record.OutboxKey(41) }},
		{name: "a person's message, with no key", key: func(*waitHarness, liveClaim) string { return "" }},
		{name: "another claim's update ask", key: func(h *waitHarness, c liveClaim) string {
			other := newLiveClaim(h.t, h.harness, "LEGION-208", claim.RoleTester)
			machine, _ := h.supervisor.Machine(c.token)
			mine := machine.Claim()
			key := record.UpdateAskKey(other.token, mine.UpdateRound, mine.UpdateOpenedAt)
			if !record.IsUpdateAskKey(other.token, key) || key == h.updateAskKey(h.t, c) {
				h.t.Fatalf("the tester's ask key %s is not the tester's own or is the implementer's", key)
			}
			return key
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newWaitHarness(t)
			h.waitIssue(phase.Implementing, nil)
			c := h.waitingWorker(t, claim.RoleImplementer)
			wait := declared(t, h.declare(c, "the review of LEGION-207's pull request", waitNow.Add(time.Hour)))
			h.openRound(t, c)
			key := tc.key(h, c)

			if answer := newsAnswer(t, h.delivered(c, wait.ID, key)); answer != (WaitNewsResponse{Ended: true}) {
				t.Fatalf("the report under %q during the round = %+v, want ended, and not the update ask", key, answer)
			}
			endedBy := "api/wait-news:" + string(c.token) + ":" + wait.ID
			if rows := waitRows(t, h.harness, c.token); len(rows) != 1 || rows[0].EndCause != workflow.WaitEndNews || rows[0].EndedBy != endedBy {
				t.Fatalf("claim_waits holds %+v, want wait %s ended with cause %q by %s", rows, wait.ID, workflow.WaitEndNews, endedBy)
			}
			if waitStands(t, h.harness, c, claim.RoleImplementer) {
				t.Fatal("the wait still stands after news during the update round")
			}
			if strings.Contains(h.logs.String(), "wait: the update ask is no news") {
				t.Fatalf("a report under %q was logged as the update ask; the log is:\n%s", key, h.logs)
			}
		})
	}
}

// The update round's relaunch of the same session leaves the wait standing: the round's pod goes,
// the claim's process is found gone and launched again at the next launch generation on the
// session it had, which registers again, and the round then ends (R3: "an update round and its
// task" and "a relaunch of the same session" end no wait). The relaunched plugin's report of the
// round's ask, which it may be handed again after the relaunch, ends nothing either, during the
// round and after it; its report of news once the round has ended ends the wait.
func TestWaitStandsThroughTheUpdateRoundsRelaunchOfTheSameSession(t *testing.T) {
	h := newWaitHarness(t)
	h.waitIssue(phase.Implementing, nil)
	c := h.waitingWorker(t, claim.RoleImplementer)
	wait := declared(t, h.declare(c, "the review of LEGION-207's pull request", waitNow.Add(time.Hour)))
	h.openRound(t, c)
	key := h.updateAskKey(t, c)
	before := h.stored(c.token)

	relaunched := c
	relaunched.secret = h.registered(h.relaunch(c.token), c.session).Secret

	after := h.stored(c.token)
	if after.Generation <= before.Generation || after.Session != c.session {
		t.Fatalf("the claim after the round's relaunch is at generation %d in session %q, want past %d in %q",
			after.Generation, after.Session, before.Generation, c.session)
	}
	h.wantNoNews(t, relaunched, wait.ID, key, h.delivered(relaunched, wait.ID, key), 0)
	h.endRound(t, c)

	if rows := waitRows(t, h.harness, c.token); len(rows) != 1 || rows[0].ID != wait.ID || !rows[0].EndedAt.IsZero() ||
		rows[0].Session != c.session {
		t.Fatalf("claim_waits holds %+v after the round, want wait %s as it was declared, with no end", rows, wait.ID)
	}
	if !waitStands(t, h.harness, c, claim.RoleImplementer) {
		t.Fatal("the wait does not stand after the update round's relaunch of the session that declared it")
	}
	h.wantNoNews(t, relaunched, wait.ID, key, h.delivered(relaunched, wait.ID, key), 1)

	if answer := newsAnswer(t, h.news(relaunched, wait.ID)); answer != (WaitNewsResponse{Ended: true}) {
		t.Fatalf("the relaunched plugin's report of news once the round ended = %+v, want ended", answer)
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
