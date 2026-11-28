//go:build e2e

package daemon

import (
	"slices"
	"strings"
	"time"

	"github.com/sjawhar/legion/daemon/internal/dispatch"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// one-record: the tester records a pass of k1 and a fail of k2, and its `legion` tool completes
// testing with the summary "Every check passes." and no verdict; the issue goes back to
// implementing, the tester's phase-finished notice carries the head and k2 alone as the check that
// does not pass, and the implementer has a task. In a second run, on an issue of its own, the
// tester records a pass of both checks and completes the same way; the issue goes to reviewing,
// its reviewer starts, and the implementer is given no task: the results in Dispatch decide,
// whatever the completion's summary says. Each run reads back the results Dispatch holds of its
// issue.
func (r *statesProof) oneRecord() {
	issues := r.issues["one-record"]
	r.oneRecordRuns(issues[0], issues[1])
}

// oneRecordRuns makes the two runs of the case one-record, on failing and passing.
func (r *statesProof) oneRecordRuns(failing, passing *statesIssue) {
	r.sentBack(failing, []statesResult{{"k1", "pass"}, {"k2", "fail"}}, "back-to-implementing", record.FailingCheck{Check: "k2", State: "no", Reason: "fail"})
	done := r.testerRun(passing, []statesResult{{"k1", "pass"}, {"k2", "pass"}}, "Every check passes.")
	r.movesWithin(passing, phase.Reviewing, "to-reviewing", done, statesCompletion)
	r.reviewerLaunched(passing, "to-reviewing", done, statesCompletion)
	r.noImplementerTask(passing)
	r.resultsHeld(passing)
}

// daemon-records-nothing: the two runs of the case one-record, on issues of its own; then no
// request Dispatch's access log holds of a result's record or retraction was made by anyone but a
// tester or an implementer of the run, the daemon's own session above all, and every result and
// retraction Dispatch stores names such a session.
func (r *statesProof) daemonRecordsNothing() {
	issues := r.issues["daemon-records-nothing"]
	r.oneRecordRuns(issues[0], issues[1])
	allowed := r.roleSessions()
	daemonSession := dispatch.DaemonSession(issues[0].key)

	writes := r.resultWrites()
	r.note("read back: Dispatch's access log holds %d requests that record or retract a result", len(writes))
	if len(writes) == 0 {
		r.fail("access-log", "UNKNOWN: Dispatch's access log holds no request that records or retracts a result, so it cannot say who wrote the results the runs recorded")
	}
	for _, write := range writes {
		r.note("  %s %s status=%s caller_kind=%s caller=%s user_agent=%q", write["method"], write["path"], write["status"], write["caller_kind"], write["caller"], write["user_agent"])
		if write["caller_kind"] != "session" || !slices.Contains(allowed, write["caller"]) {
			r.fail("access-log", "Dispatch served %s %s for %s %q, which is no tester's or implementer's session of the run (they are %v; the daemon's is %s)",
				write["method"], write["path"], write["caller_kind"], write["caller"], allowed, daemonSession)
		}
	}

	results := r.storedResults("check_results", "")
	r.note("read back: check_results holds %d results", len(results))
	for _, issue := range issues {
		if mine := slices.IndexFunc(results, func(s statesStored) bool { return s.issue == issue.key }); mine < 0 {
			r.fail("results", "UNKNOWN: check_results holds no result of %s, whose runs recorded two, so it cannot say who wrote them", issue.key)
		}
	}
	for _, table := range []string{"ci_check_results", "check_result_retractions", "ci_check_result_retractions"} {
		stored := r.storedResults(table, "")
		r.note("read back: %s holds %d rows", table, len(stored))
		results = append(results, stored...)
	}
	for _, stored := range results {
		r.note("  %s %s", stored.table, stored)
		if stored.actorKind != "session" || stored.actorID != stored.session || !slices.Contains(allowed, stored.actorID) {
			r.fail("results", "%s holds %s, written by %s %q under session %q: not a tester's or implementer's session of the run (they are %v; the daemon's is %s)",
				stored.table, stored, stored.actorKind, stored.actorID, stored.session, allowed, daemonSession)
		}
	}
	r.note("every result and retraction Dispatch served and stores names a tester's or an implementer's session; none names the daemon's %s", daemonSession)
}

// dispatch-tells: the tester records a fail of k1 with the real `dispatch` command, and within 10
// seconds of the start of that write, by the record's own time, the daemon's log holds `workflow:
// a check state is held` for the issue, k1, the head, the state no and the reason fail: Dispatch's
// event, which nothing but its outbox and the daemon's consumer carry. The tester has not
// completed, so the issue stays in testing and the implementer is given no task; the case reads
// both, and the results Dispatch holds of the issue.
func (r *statesProof) dispatchTells() {
	issue := r.issues["dispatch-tells"][0]
	r.readyTester(issue)
	r.stageHead(issue)
	r.statesBefore(issue)
	from := r.daemon.count()
	sent := time.Now()
	r.record(issue, issue.tester, "tester", statesResult{"k1", "fail"})
	var line *loggedRecord
	for line == nil && time.Since(sent) < statesLate {
		line = r.daemon.firstAfter(from, func(l loggedRecord) bool {
			return l.Message == statesHeld && l.Attrs["issue"] == issue.key && l.Attrs["check"] == "k1" && l.Attrs["head"] == issue.head &&
				l.Attrs["state"] == "no" && l.Attrs["reason"] == "fail"
		})
		time.Sleep(50 * time.Millisecond)
	}
	if line == nil {
		r.fail("line", "UNKNOWN: the daemon's log holds no %q line for %s, k1, %s, no, fail within %s of %s: Dispatch's event of that state did not reach it",
			statesHeld, issue.key, issue.head, statesLate, statesWrite)
	}
	took := line.at.Sub(sent)
	r.note("daemon, %s after %s (bound %s): %s issue=%v check=%v repo=%v head=%v state=%v reason=%v",
		took.Round(time.Millisecond), statesWrite, statesWithin, line.Message, line.Attrs["issue"], line.Attrs["check"], line.Attrs["repo"],
		line.Attrs["head"], line.Attrs["state"], line.Attrs["reason"])
	if took > statesWithin {
		r.fail("line-within", "the daemon logged %q for %s, k1, no, fail at %s, %s after %s, past the bound of %s",
			statesHeld, issue.key, line.at.UTC().Format(time.RFC3339Nano), took.Round(time.Millisecond), statesWrite, statesWithin)
	}
	if got := r.phaseOf(issue, "phase"); got != phase.Testing {
		r.fail("phase", "GET /legion/v1/state shows %s in %s after the daemon held k1's fail, want %s: its tester has not completed", issue.key, got, phase.Testing)
	}
	r.note("read back: GET /legion/v1/state shows %s in %s after the daemon held k1's fail: its tester has not completed", issue.key, phase.Testing)
	r.noImplementerTask(issue)
	r.resultsHeld(issue)
}

// pass-goes-to-review: the implementer and the tester record a pass of k1 and of k2, the tester's
// `legion` tool completes testing with no verdict, and within 10 seconds of the completion the
// issue is in reviewing and the daemon has started its reviewer. The implementer is given no task,
// and Dispatch holds the four results.
func (r *statesProof) passGoesToReview() {
	issue := r.issues["pass-goes-to-review"][0]
	r.readyTester(issue)
	r.stageHead(issue)
	implementer := "implementer-" + strings.ToLower(issue.key)
	r.record(issue, implementer, "implementer", statesResult{"k1", "pass"}, statesResult{"k2", "pass"})
	done := r.testerRun(issue, []statesResult{{"k1", "pass"}, {"k2", "pass"}}, "Every check passes.")
	r.movesWithin(issue, phase.Reviewing, "reviewing", done, statesCompletion)
	r.reviewerStarted(issue, done, statesCompletion)
	r.noImplementerTask(issue)
	r.resultsHeld(issue)
}

// no-check: an issue whose Verification document exists and holds no before_merge check (its one
// check, k3, is after_merge), as Dispatch answers it. Its tester completes with no verdict and no
// result. Within 10 seconds of the completion the issue is in reviewing and its reviewer started;
// the implementer is given no task; Dispatch holds no result of the issue; and Dispatch published
// the head's set with no check, computed from that document.
func (r *statesProof) noCheck() {
	issue := r.issues["no-check"][0]
	document := r.noBeforeMergeCheck(issue)
	done := r.testerRun(issue, nil, "No check to run: the Verification document holds no before_merge check.")
	r.movesWithin(issue, phase.Reviewing, "reviewing", done, statesCompletion)
	r.reviewerStarted(issue, done, statesCompletion)
	r.noImplementerTask(issue)
	r.resultsHeld(issue)
	r.emptySet(issue, document)
}

// fail-sends-back: the tester records a pass of k1 and a fail of k2 and completes with no verdict;
// the issue goes back to implementing, the tester's phase-finished notice the architect's topic
// received carries the tested head and k2, the state no and the reason fail, as the one check that
// does not pass, and the implementer has a task. On a second issue the tester records a pass and a
// fail of k1 and a pass of k2, and the notice carries k1, unknown for pass_and_fail, alone. Each
// run reads back the results Dispatch holds of its issue.
func (r *statesProof) failSendsBack() {
	issues := r.issues["fail-sends-back"]
	r.sentBack(issues[0], []statesResult{{"k1", "pass"}, {"k2", "fail"}}, "implementing", record.FailingCheck{Check: "k2", State: "no", Reason: "fail"})
	r.sentBack(issues[1], []statesResult{{"k1", "pass"}, {"k1", "fail"}, {"k2", "pass"}}, "implementing", record.FailingCheck{Check: "k1", State: "unknown", Reason: "pass_and_fail"})
}

// sentBack makes one run that sends issue back: the tester's results, its completion, the move
// back to implementing within statesWithin of the completion (assertion names it), the tester's one
// phase-finished notice the architect's topic received, whose head is the issue's head and whose
// failing is failing, compared whole, the implementer's pending task, which carries the reason
// that notice gives (the move writes both from one value; neither is compared with a literal), the
// daemon's JSON line of the move, with move implementing, the issue's head and failing the ids of
// failing, the states the daemon holds at the head, each check of failing in its state for its
// reason, and the results Dispatch holds of issue. The implementers' stand-in holds every turn
// while the task is read, so the task is still the claim's pending delivery.
func (r *statesProof) sentBack(issue *statesIssue, results []statesResult, assertion string, failing ...record.FailingCheck) {
	r.implementer.holdTurn()
	defer r.implementer.releaseTurn()
	done := r.testerRun(issue, results, "Every check passes.")
	r.movesWithin(issue, phase.Implementing, assertion, done, statesCompletion)
	task := r.implementerTask(issue)
	r.note("read back: the implementer has a pending task in the daemon's store (%d bytes)", len(task))
	var told []record.Notice
	r.until("notice-fields", "the tester's phase-finished notice of "+issue.key+" on its architect's topic", statesWithin, func() bool {
		told = r.testerNotices(issue.key)
		return len(told) > 0
	})
	if len(told) != 1 || told[0].Head != issue.head || !slices.Equal(told[0].Failing, failing) || told[0].WaitsFor != "" || len(told[0].Waiting) != 0 {
		r.fail("notice-fields", "the tester's phase-finished notices of %s on its architect's topic = %+v, want one with the head %s and failing %+v", issue.key, told, issue.head, failing)
	}
	if told[0].Reason == "" || !strings.Contains(task, told[0].Reason) {
		r.fail("task-carries-notice", "the implementer's task of %s does not carry the reason %q its tester's notice gives: %q", issue.key, told[0].Reason, task)
	}
	r.note("the tester's notice carries the head %s and failing %+v, and the implementer's task carries its reason", told[0].Head, told[0].Failing)
	var ids []string
	for _, check := range failing {
		ids = append(ids, check.Check)
	}
	line := r.moveLine(issue, string(phase.Implementing), "move-line")
	if line.Attrs["head"] != issue.head || !slices.Equal(attrStrings(line.Attrs["failing"]), ids) || len(attrStrings(line.Attrs["waiting"])) != 0 {
		r.fail("move-line", "the daemon's line %q of %s = %v, want the head %s and failing %v", statesMoveDecided, issue.key, line.Attrs, issue.head, ids)
	}
	held := r.heldStates(issue, "held-states")
	for _, check := range failing {
		if state := held.States[check.Check]; state.State != check.State || state.Reason != check.Reason {
			r.fail("held-states", "the daemon holds %s of %s at %s as %+v, want %s for %s", check.Check, issue.key, issue.head, state, check.State, check.Reason)
		}
	}
	r.note("read back: the daemon's line of the move names the head %s and failing %v, and it holds each of them at that head as the notice says", issue.head, ids)
	r.resultsHeld(issue)
}

// completion-before-event: with the states from before the results held, the run's own
// nats-server is stopped (SIGSTOP, by the pid the run started it with). The tester records a pass
// of k1 and of k2, writes its test record and completes testing. For 10 seconds the issue stays in
// testing and the daemon starts no worker, and the architect is told once, in the tester's
// phase-finished notice, whose head is the tested head and whose waits_for is fence, and the
// daemon's line of the move says it waits, at that head, for the fence: the move waits for
// Dispatch's check states. Then the broker is continued (SIGCONT), Dispatch's outbox and
// the daemon's consumer deliver what waited, and the issue goes to reviewing within 10 seconds of
// the SIGCONT. Once its reviewer starts, the implementer has been given no task, and Dispatch holds
// the tester's two results.
func (r *statesProof) completionBeforeEvent() {
	issue := r.issues["completion-before-event"][0]
	r.readyTester(issue)
	r.stageHead(issue)
	r.statesBefore(issue)
	r.stopBroker()
	defer r.continueBroker()
	r.record(issue, issue.tester, "tester", statesResult{"k1", "pass"}, statesResult{"k2", "pass"})
	r.writeTestRecord(issue)
	launches := len(r.rt.Launches())
	claims := r.issueClaims(issue)
	r.complete(issue, "Every check passes.")
	deadline := time.Now().Add(statesWithin)
	for time.Now().Before(deadline) {
		if got := r.phaseOf(issue, "stays-testing"); got != phase.Testing {
			r.fail("stays-testing", "%s moved to %s while Dispatch's events could not reach the daemon, on the states from before the tester's results", issue.key, got)
		}
		if now := len(r.rt.Launches()); now != launches {
			r.fail("starts-no-worker", "the daemon started %d workers while the move waited: %v", now-launches, r.rt.Launches()[launches:])
		}
		if now := r.issueClaims(issue); !slices.Equal(now, claims) {
			r.fail("starts-no-worker", "the daemon holds the claims %v of %s while the move waited, and held %v before the completion", now, issue.key, claims)
		}
		time.Sleep(100 * time.Millisecond)
	}
	r.note("read back: for %s %s stayed in %s, the daemon started no worker (launches %d, claims %v)", statesWithin, issue.key, phase.Testing, launches, claims)
	notices := r.architectNotices(issue.key, "phase-finished")
	if len(notices) != 1 {
		r.fail("notice", "%s's architect was told %d phase-finished notices while the move waited, want one: %v", issue.key, len(notices), notices)
	}
	told := r.testerNotices(issue.key)
	if len(told) != 1 || told[0].Head != issue.head || told[0].WaitsFor != "fence" || len(told[0].Failing) != 0 {
		r.fail("notice", "the tester's phase-finished notices of %s while the move waited = %+v, want one with the head %s and waits_for fence", issue.key, told, issue.head)
	}
	r.note("read back: the tester's one phase-finished notice carries the head %s and waits_for %s", told[0].Head, told[0].WaitsFor)
	waits := r.moveLine(issue, "waits", "move-line")
	if waits.Attrs["head"] != issue.head || waits.Attrs["waits_for"] != "fence" || len(attrStrings(waits.Attrs["failing"])) != 0 {
		r.fail("move-line", "the daemon's line %q of %s = %v, want move waits, the head %s and waits_for fence", statesMoveDecided, issue.key, waits.Attrs, issue.head)
	}
	r.note("read back: the daemon's line of the move says it waits, at %s, for the fence", issue.head)
	continued := r.continueBroker()
	if continued.IsZero() {
		r.fail("broker", "the run's nats-server was not continued, so nothing can be measured from the SIGCONT")
	}
	r.movesWithin(issue, phase.Reviewing, "moves-after", continued, statesContinue)
	r.reviewerLaunched(issue, "moves-after", continued, statesContinue)
	r.noImplementerTask(issue)
	r.resultsHeld(issue)
}
