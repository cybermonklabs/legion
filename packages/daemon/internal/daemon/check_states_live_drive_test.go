//go:build e2e

package daemon

import (
	"context"
	"fmt"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// statesResult is one result a case records: the check and the verdict.
type statesResult struct {
	check, verdict string
}

// testerRun is one tester's run on issue: its pane ready, the issue's pull request staged in
// Dispatch, each of results recorded as the tester's session, its test record written, and its
// `legion` tool's completion of testing with summary and no verdict, which the tool must take. It
// answers the moment complete answers, which a bound after the completion is measured from.
func (r *statesProof) testerRun(issue *statesIssue, results []statesResult, summary string) time.Time {
	r.readyTester(issue)
	r.stageHead(issue)
	r.record(issue, issue.tester, "tester", results...)
	r.writeTestRecord(issue)
	return r.complete(issue, summary)
}

// readyTester waits for issue's tester's pane, which the boot relaunched, to register, take the
// testing task its claim held, and end that turn, which the testers' stand-in answers with a line
// starting WAITING; it holds the session the pane registered, which every write of the tester's is
// made as. The turn is found by the task seed left pending (statesIssue.seededTask), the test's own
// input and no text of the daemon's. The case's own task then starts a turn of a worker that holds
// no task of its phase but is serving its run, as a tester that said it waits is.
func (r *statesProof) readyTester(issue *statesIssue) {
	r.t.Helper()
	if issue.tester != "" {
		return
	}
	token := r.token(issue.key, claim.RoleTester)
	task := issue.seededTask
	r.until("tester", "the tester's pane to take its testing task "+strconv.Quote(task), paneReady, func() bool {
		if state := r.claim(token).State; state == string(supervise.StateFailed) {
			r.fail("tester", "%s failed before its pane took its task; its workers' output is under %s", token, r.rt.paneLogs)
		}
		return r.tester.requestsHolding(task) > 0
	})
	r.until("tester", "the tester's pane to end its first turn", time.Minute, func() bool {
		return r.claim(token).State == string(supervise.StateIdle)
	})
	held := r.claim(token)
	if issue.tester = held.Session; issue.tester == "" {
		r.fail("tester", "UNKNOWN: %s is idle and the daemon names no session of it, so nothing can be written as the tester", token)
	}
	r.note("%s took its testing task %q and ended the turn: session %s, plugin contract %d, process %s",
		token, task, issue.tester, held.PluginContract, held.Locator.Label())
}

// until polls want until it holds, and fails the case at assertion, naming what it waited for,
// once within has passed.
func (r *statesProof) until(assertion, what string, within time.Duration, want func() bool) {
	r.t.Helper()
	for deadline := time.Now().Add(within); !want(); time.Sleep(50 * time.Millisecond) {
		if time.Now().After(deadline) {
			r.fail(assertion, "waited %s for %s", within, what)
		}
	}
}

// stageHead stores issue's open pull request in Dispatch's database as the `pull_request` envelope
// Envoy relays would have stored it: one pull_request_heads row at the issue's head, attributed to
// the issue by its title and read just now, so Dispatch's reconcile has no reason to read it from
// GitHub again. The table's trigger marks the issue owed, as the envelope's own write does.
func (r *statesProof) stageHead(issue *statesIssue) {
	r.t.Helper()
	if issue.staged {
		return
	}
	issue.staged = true
	if _, err := r.dispatchDB.Exec(context.Background(), `
		insert into pull_request_heads (repo, number, head_sha, head_ref, base_ref, state, issue_key, attribution_source,
			title, url, event_at, checked_at)
		values ($1, $2, $3, $4, 'main', 'open', $5, 'title_body', $6, $7, now(), now())`,
		liveRepo, issue.number, issue.head, claim.IssueBranch(issue.key), issue.key, issue.key+" "+issue.title,
		fmt.Sprintf("https://github.com/%s/pull/%d", liveRepo, issue.number)); err != nil {
		r.fail("pull-request", "stage %s's pull request #%d at %s in Dispatch's database: %v", issue.key, issue.number, issue.head, err)
	}
	r.note("staged %s's pull request #%d, open at %s on %s, in Dispatch's database", issue.key, issue.number, issue.head, claim.IssueBranch(issue.key))
}

// statesBefore waits for the daemon to hold Dispatch's first state of each check at issue's head,
// no for no_result, which the staged pull request made Dispatch publish: the states from before any
// result. A read that finds them missing is UNKNOWN, which fails the case once its own judgement is
// made, so a run where no state reaches the daemon fails where the case says it does.
func (r *statesProof) statesBefore(issue *statesIssue) {
	r.t.Helper()
	deadline := time.Now().Add(time.Minute)
	for _, check := range []string{"k1", "k2"} {
		var line *loggedRecord
		for line == nil && time.Now().Before(deadline) {
			line = r.lastLog(func(l loggedRecord) bool {
				return l.Message == statesHeld && l.Attrs["issue"] == issue.key && l.Attrs["check"] == check && l.Attrs["head"] == issue.head
			})
			if line == nil {
				time.Sleep(100 * time.Millisecond)
			}
		}
		if line == nil {
			r.blind = append(r.blind, fmt.Sprintf("the daemon held no state of %s at %s of %s within a minute of its pull request's staging", check, issue.head, issue.key))
			r.note("UNKNOWN: the daemon holds no state of %s at %s yet", check, issue.head)
			continue
		}
		r.note("daemon: %s issue=%v check=%v head=%v state=%v reason=%v", line.Message, line.Attrs["issue"], line.Attrs["check"], line.Attrs["head"],
			line.Attrs["state"], line.Attrs["reason"])
	}
}

// record records each of results on issue at its head with the real `dispatch result`, as session
// in role, reads each back from Dispatch's database, and keeps it in issue.recorded.
func (r *statesProof) record(issue *statesIssue, session, role string, results ...statesResult) {
	r.t.Helper()
	for _, result := range results {
		args := []string{"result", "--issue", issue.key, "--check", result.check, "--role", role, "--verdict", result.verdict,
			"--commit", issue.head, "--surface", "cli", "--observed", "The " + role + "'s run of " + result.check + " at the head: " + result.verdict + "."}
		if result.verdict == "fail" {
			args = append(args, "--failed", "1")
		}
		out, code := r.cli(session, args...)
		id := statesResultID.FindStringSubmatch(out)
		if code != 0 || id == nil {
			r.fail("result", "dispatch result %s %s of %s as %s exited %d: %s", result.verdict, result.check, issue.key, role, code, strings.TrimSpace(out))
		}
		stored := r.storedResults("check_results", "id::text = $1", id[1])
		if len(stored) != 1 {
			r.fail("result", "UNKNOWN: Dispatch's check_results holds %d rows of the result %s the command reported", len(stored), id[1])
		}
		issue.recorded = append(issue.recorded, statesRecordedAs(role, result.check, result.verdict, issue.head))
		r.note("dispatch result as %s session %s: %s; Dispatch stores %s", role, session, firstLine(strings.TrimSpace(out)), stored[0])
	}
}

// statesRecordedAs is one result as resultsHeld compares what a case recorded with what Dispatch
// holds: its role, check, verdict and commit.
func statesRecordedAs(role, check, verdict, commit string) string {
	return fmt.Sprintf("%s %s %s at %s", role, check, verdict, commit)
}

// statesResultID is the result id `dispatch result` prints: `Result <id>.`.
var statesResultID = regexp.MustCompile(`Result ([0-9a-f-]{36})`)

// writeTestRecord writes issue's test record with the real `dispatch handoff-write` as the tester's
// session, its body naming the head the tester tested under `head`, and prints the check states
// serial Dispatch stored with it, "none" where it stores none. A write Dispatch does not take fails
// the case at test-record: a test record waits for the states the tester's results owe, so a
// Dispatch that never settles them shows here.
func (r *statesProof) writeTestRecord(issue *statesIssue) {
	r.t.Helper()
	path := filepath.Join(r.cliHome, issue.key+"-test.json")
	writeProofFile(r.t, path, string(headBody(issue.key, "test", issue.head)))
	out, code := r.cli(issue.tester, "handoff-write", "--issue", issue.key, "--phase", "test", "--body-file", path)
	if code != 0 {
		r.fail("test-record", "dispatch handoff-write of %s's test record as the tester exited %d: %s", issue.key, code, strings.TrimSpace(out))
	}
	written := parseWritten(r.t, out, code)
	var read struct {
		CheckStatesSerial *int64 `json:"check_states_serial"`
	}
	r.dispatch.agentGet("/api/v1/handoffs/"+written.id, &read)
	serial := "none"
	if read.CheckStatesSerial != nil {
		serial = strconv.FormatInt(*read.CheckStatesSerial, 10)
	}
	r.note("dispatch handoff-write as the tester: %s; head %s, check states serial %s", written.line, issue.head, serial)
}

// complete has the tester's pane complete testing through its real `legion` tool: an operator's
// task (`legion claims deliver`) starts a turn the tester's stand-in answers with one call of
// handoff_complete carrying summary and no verdict. The tool must take it.
//
// It answers the moment every bound after the completion is measured from, and that moment comes
// before the tool's request by construction, not by timing: the stand-in holds the answer of the
// turn that makes the call until the driver has taken the moment, after the turn reached the model,
// and only then releases it, so the tool's request, which the answer starts, can reach the daemon
// only after it. The delivery command runs beside the turn rather than before it, since the
// moment the command returns says nothing of when the turn ran: a delivery whose answer is slow
// cannot start the clock late, it only makes a case read later.
func (r *statesProof) complete(issue *statesIssue, summary string) time.Time {
	r.t.Helper()
	token := r.token(issue.key, claim.RoleTester)
	r.until("completion", "the tester's pane to be between turns", time.Minute, func() bool {
		state := r.claim(token).State
		return state == string(supervise.StateIdle) || state == string(supervise.StateReady)
	})
	marker := "CHECK-STATES " + issue.key + ": complete testing"
	call := r.tester.arm(marker, map[string]any{"op": "handoff_complete", "summary": summary}, true)
	defer call.release()
	task := marker + " with the legion tool."
	from := r.daemon.count()
	type delivered struct {
		out string
		err error
	}
	delivery := make(chan delivered, 1)
	go func() {
		out, err := r.legionClaimsTry("deliver", "--claim", string(token), "--task", task)
		delivery <- delivered{out: out, err: err}
	}()
	refused := func(answer delivered) {
		if answer.err != nil {
			r.fail("completion", "legion claims deliver --claim %s --task %q failed: %v\n%s", token, task, answer.err, answer.out)
		}
	}
	var answer *delivered
	var stamp time.Time
	for waited := time.After(2 * time.Minute); stamp.IsZero(); {
		select {
		case <-call.reached:
			stamp = time.Now()
		case got := <-delivery:
			refused(got)
			answer = &got
		case <-waited:
			r.fail("completion", "no turn of the tester's pane carried the task %q within two minutes; %s", task, r.tester.lastRequest())
		}
	}
	call.release()
	if answer == nil {
		select {
		case got := <-delivery:
			refused(got)
			answer = &got
		case <-time.After(2 * time.Minute):
			r.fail("completion", "legion claims deliver --claim %s did not return within two minutes", token)
		}
	}
	r.note("legion claims deliver --claim %s --task %q:\n%s", token, task, indentOutput(answer.out))
	var result toolResult
	select {
	case result = <-call.result:
	case <-time.After(2 * time.Minute):
		r.fail("completion", "the tester's pane sent no result of its legion call within two minutes")
	}
	r.note("the tester's turn called legion %s; the tool answered (error %t): %s", inputOf(call), result.isError, result.text)
	if line := r.daemon.firstAfter(from, func(l loggedRecord) bool {
		return l.Message == lateCompletionAnswered && l.Attrs["claim"] == string(token)
	}); line != nil {
		r.note("daemon, %s after the completion's moment, which the driver took before it released the call: %s",
			line.at.Sub(stamp).Round(time.Millisecond), answeredLine(*line))
	}
	if result.isError {
		r.fail("completion", "the tester's legion tool refused the completion of testing with no verdict: %s", result.text)
	}
	return stamp
}

// cli runs the real `dispatch` CLI as session (handoff_records_live_test.go's cliShell) and answers
// both streams together and its exit code.
func (r *statesProof) cli(session string, args ...string) (string, int) {
	r.t.Helper()
	out, code := cliShell{t: r.t, dir: r.cliHome, session: session, base: r.dispatch.base, token: r.dispatch.token}.bun(nil, dispatchCLI(r.root), args...)
	return string(out.stdout) + string(out.stderr), code
}
