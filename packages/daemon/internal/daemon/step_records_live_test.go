//go:build e2e

// The live proof's driver for the record of each step of an issue: the whole daemon writes each
// start and each end of a step onto the issue in Dispatch, and a person reads them back with the
// real `dispatch steps` command. scripts/e2e/step-records.sh builds this driver with `-tags e2e`
// and owns the nats-server and the PostgreSQL it runs against.
//
// Nothing of the record path is stood in for. Dispatch is the real server this checkout builds
// (startHandoffDispatch, as handoff-records.sh's driver starts it), on a database of its own on
// LEGION_TEST_PG_DSN's server, with NATS off; it is stopped and started again on the same database
// and port for the outage. The daemon is the whole daemon (run, the function `legion start` calls,
// through startDaemonWithin) with dispatch_url at that server, so every step record it writes goes
// through its outbox and its Dispatch client to the real route. The issue walks as the package's
// whole-daemon tests walk it (stepRecordsRig, step_records_rig_test.go): each role over the worker
// stream and the daemon's own routes, its handoff record written to Dispatch under its own
// session, a person's review and the merge as GitHub's envelopes on the broker, and the
// architect's sign-off through its own route. The reader is the real `dispatch` CLI
// (packages/envoy-client/bin/dispatch.ts under bun), run as a person of the project with the
// personal token Dispatch minted for that person.
//
// What is stood in for lies outside the records: GitHub's REST API (stepRecordsGitHub), the Apps'
// tokens (workflowTokenRecorder), Envoy's listener (noticeListener), and the panes, which the
// package's fake runtime launches and the driver plays, each answering with the model
// stepRecordsModel names in Oh My Pi's message_end frame.
package daemon

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/fixtureenv"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/testwait"
)

// stepRecordsReaderSession is the session the person's `dispatch steps` runs under, as a person's
// own shell names one to run dispatch by hand.
const stepRecordsReaderSession = "ses_step_records_reader"

// stepRecordsLive is one run of the proof: the walk's rig, the checkout the CLI runs out of, the
// CLI's own home, and the person's personal token.
type stepRecordsLive struct {
	*stepRecordsRig
	root    string
	cliHome string
	reader  string
}

// TestStepRecordsLive drives the proof's steps against one booted daemon and one Dispatch, over
// one issue that walks planning to done with one failed review. The handoff of planning,
// implementing and testing is read from Dispatch at each completion, so Dispatch is stopped only
// once the issue is in reviewing for the second time: the second review, the acceptance, the retro
// and the merge then walk it to done with Dispatch stopped.
func TestStepRecordsLive(t *testing.T) {
	fixtureenv.Live(t)
	root := liveCLICheckout(t)
	proof := newLiveProof(t, "scripts/e2e/step-records.sh")
	l := &stepRecordsLive{stepRecordsRig: newStepRecordsRigOn(t, proof, startHandoffDispatch(t, root).scratchDispatch), root: root, cliHome: t.TempDir()}
	var minted struct {
		Token string `json:"token"`
	}
	l.person(http.MethodPost, "/api/v1/me/agent-tokens", map[string]string{"name": "steps-reader"}, http.StatusCreated, &minted)
	if minted.Token == "" {
		t.Fatal("Dispatch minted the person a personal token with no plaintext")
	}
	l.reader = minted.Token
	l.note("Dispatch %s (database of its own, NATS off), project %s mapped to %s, issue %s on pull request %d; daemon at dispatch_url %s; the reader is the person's personal token",
		l.dispatch.base, l.cfg.Project, liveRepo, l.key, l.number, l.cfg.DispatchURL)
	l.step("walk-to-the-second-review-with-dispatch-up", l.walkToTheSecondReview)
	l.step("a-phase-moves-while-dispatch-is-down", l.outage)
	l.step("walk-to-done-with-one-failed-review", l.readByIssue)
	l.step("a-write-that-names-another-session-is-refused", l.writerRefused)
}

// walkToTheSecondReview walks the issue with Dispatch up through the rounds whose completions read
// their handoff record from Dispatch: planning, implementing and testing, the first review
// requesting changes, then implementing and testing again, to the second review.
func (l *stepRecordsLive) walkToTheSecondReview() {
	l.plan()
	l.implement()
	l.test()
	l.note("planning, implementing and testing completed, each naming its handoff record on Dispatch; %s in reviewing", l.key)
	l.review("CHANGES_REQUESTED", phase.Implementing)
	l.note("%s requested changes on the pull request's head: %s back in implementing", stepRecordsPerson, l.key)
	l.implement()
	l.test()
	l.note("implementing and testing completed again: %s in reviewing for the second time", l.key)
}

// outage stops Dispatch and walks the issue to done: the second review approves, the architect
// signs off, the retro completes and GitHub merges the pull request, with no Dispatch to write the
// records of those steps to. Dispatch then starts again on the same database, and the records of
// the steps that ran while it was stopped reach it, each ended. The control holds the outage real:
// once the walk is done and Dispatch still stopped, `dispatch steps` fails naming the server it
// could not reach, and the daemon's outbox holds the issue's step records unfinished.
func (l *stepRecordsLive) outage() {
	t := l.t
	l.dispatch.stop()
	l.note("Dispatch stopped (SIGINT) with %s in reviewing", l.key)
	l.review("APPROVED", phase.Accepting)
	l.accept()
	l.retro()
	l.merge()
	l.note("the review, the acceptance, the retro and the merge walked %s to done with Dispatch stopped", l.key)

	out, code := l.steps()
	if code == 0 || !strings.Contains(out, "is unreachable") {
		t.Fatalf("control: with Dispatch stopped `dispatch steps --issue %s` exited %d and printed %q, want a non-zero exit naming the unreachable server; the walk rode through no outage", l.key, code, out)
	}
	l.note("control: `dispatch steps --issue %s` with Dispatch stopped exits %d: %s", l.key, code, firstLine(strings.TrimSpace(out)))
	unfinished := l.unfinishedStepRows()
	if unfinished == 0 {
		t.Fatalf("control: the outbox holds no unfinished step record of %s while Dispatch is stopped: either Dispatch took each record (no outage) or the daemon wrote none", l.key)
	}
	l.note("control: the daemon's outbox holds %d unfinished step records of %s", unfinished, l.key)

	l.dispatch.start()
	l.note("Dispatch started again on the same database and port")
	want := []stepWant{stepRecordsTen[6], stepRecordsTen[7], stepRecordsTen[8], stepRecordsTen[9]}
	// A row Dispatch refused while it was stopped waits out its backoff, at most retryBackoff's
	// cap, before it runs again, and the sweep finds a lease that lapsed within one more lease.
	within := retryBackoff(6) + outboxLease
	started := time.Now()
	var last error
	var lines []stepLine
	if !testwait.Holds(within, 2*time.Second, func() bool {
		output, code := l.steps()
		if code != 0 {
			last = fmt.Errorf("exit %d: %s", code, output)
			return false
		}
		var parsed stepsPrinted
		if parsed, last = parseSteps(l.key, output); last != nil {
			return false
		}
		lines = parsed.lines
		last = holdsLines(parsed.lines, want, false)
		return last == nil
	}) {
		t.Fatalf("within %s of Dispatch's start `dispatch steps` did not hold the four steps that ran while it was stopped: %v", within, last)
	}
	l.note("the records reached Dispatch %s after its start:", time.Since(started).Round(time.Second))
	for _, w := range want {
		l.note("  %s", findLine(lines, w).text)
	}
}

// readByIssue is the read of an issue's steps: `dispatch steps` run as a person of the project
// prints the header `<KEY>: 10 steps in 1 tasks` and one line for each of the walk's ten runs,
// each ended with its result, each with an agent, and each of kind step with the model that
// answered it. The control deletes the failed review's record from Dispatch's own store, its node
// and every tuple that names it, and the same parse of the same command must then report that
// record missing.
func (l *stepRecordsLive) readByIssue() {
	t := l.t
	out, err := l.stepsHoldTen("as the person")
	if err != nil {
		t.Fatalf("`dispatch steps --issue %s` printed\n%s\nand the read does not hold the walk: %v", l.key, out, err)
	}
	l.note("`dispatch steps --issue %s` as the person, exit 0:", l.key)
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		l.note("  %s", line)
	}

	read, err := l.readSteps()
	if err != nil {
		t.Fatalf("read the steps of %s: %v", l.key, err)
	}
	failed := read.find("step", 4, 1).ID
	if failed == "" {
		t.Fatalf("GET /api/v1/issues/%s/steps holds no record of step 4 attempt 1", l.key)
	}
	l.deleteStepRecord(failed)
	l.note("control: deleted the failed review's record %s from Dispatch's database (its workflow_nodes row and its workflow_tuples rows)", failed)
	out, err = l.stepsHoldTen("in the control, after the delete,")
	if err == nil {
		t.Fatalf("control: the parse of `dispatch steps --issue %s` passed with the failed review's record deleted:\n%s", l.key, out)
	}
	if missing := lineName(stepRecordsTen[5]); !strings.Contains(err.Error(), missing) {
		t.Fatalf("control: the parse with the failed review's record deleted reports %q, want it to name %q", err, missing)
	}
	l.note("control: the same parse now fails: %v", err)
}

// writerRefused is the writer check of the step route. A write that names a session other than
// the daemon's, sent with the agent's bearer, is refused 403 STEP_WRITER and leaves no record;
// the control sends the same body with the daemon's own credential (the token its
// dispatch_token_file holds) as legion-daemon:<PROJECT>, and it is taken. What it holds, and no
// more: a write that names another session is refused. A credential that proves no session can
// still name the daemon's session, as it can for the daemon's status writes today.
func (l *stepRecordsLive) writerRefused() {
	t := l.t
	id := uuid.NewString()
	write := func(actor string) map[string]any {
		return map[string]any{
			"actor": map[string]string{"kind": "session", "id": actor}, "task": "default", "step": 1, "kind": "step",
			"role": "planner", "type": "planning", "agent": string(l.token(claim.RolePlanner)), "generation": 1,
			"started_at": time.Now().UTC().Format(time.RFC3339),
		}
	}
	status, answer := l.putStep(l.dispatch.token, id, write("ses_step_agent"))
	if status != http.StatusForbidden || !strings.Contains(string(answer), "STEP_WRITER") {
		t.Fatalf("PUT /api/v1/issues/%s/steps/%s as ses_step_agent = %d %s, want 403 STEP_WRITER", l.key, id, status, answer)
	}
	if read, err := l.readSteps(); err != nil || l.holdsID(read, id) {
		t.Fatalf("after the refused write the steps of %s read %v (%v), want no record %s", l.key, read, err, id)
	}
	l.note("PUT /api/v1/issues/%s/steps/%s with the agent's bearer as ses_step_agent: %d %s; the read holds no record %s", l.key, id, status, strings.TrimSpace(string(answer)), id)

	daemonToken, err := os.ReadFile(l.cfg.DispatchTokenFile)
	if err != nil {
		t.Fatalf("read the daemon's Dispatch token file: %v", err)
	}
	daemon := "legion-daemon:" + l.cfg.Project
	status, answer = l.putStep(strings.TrimSpace(string(daemonToken)), id, write(daemon))
	if status != http.StatusOK {
		t.Fatalf("control: the same PUT with the daemon's own credential as %s = %d %s, want 200", daemon, status, answer)
	}
	if read, err := l.readSteps(); err != nil || !l.holdsID(read, id) {
		t.Fatalf("control: after the daemon's write the steps of %s read %v (%v), want record %s", l.key, read, err, id)
	}
	l.note("control: the same body with the daemon's own credential as %s: %d; the read holds record %s", daemon, status, id)
}

// steps runs `dispatch steps --issue <KEY>` as the person, with the personal token Dispatch minted
// for that person, and returns what it printed on stdout, then on stderr, and its exit code.
func (l *stepRecordsLive) steps() (string, int) {
	l.t.Helper()
	shell := cliShell{t: l.t, dir: l.cliHome, session: stepRecordsReaderSession, base: l.dispatch.base, token: l.reader}
	out, code := shell.bun(nil, dispatchCLI(l.root), "steps", "--issue", l.key)
	return string(out.stdout) + string(out.stderr), code
}

// stepsHoldTen runs `dispatch steps --issue <KEY>` as the person, failing the test when the
// command fails (when says at which point of the proof it ran), and answers what it printed and
// whether that is the walk's ten runs (holdsTen).
func (l *stepRecordsLive) stepsHoldTen(when string) (string, error) {
	l.t.Helper()
	out, code := l.steps()
	if code != 0 {
		l.t.Fatalf("`dispatch steps --issue %s` %s exited %d: %s", l.key, when, code, out)
	}
	printed, err := parseSteps(l.key, out)
	if err != nil {
		return out, err
	}
	return out, holdsTen(printed)
}

// putStep sends body to PUT /api/v1/issues/<KEY>/steps/<id> with bearer and returns Dispatch's
// answer as it is.
func (l *stepRecordsLive) putStep(bearer, id string, body any) (int, []byte) {
	l.t.Helper()
	encoded, err := json.Marshal(body)
	if err != nil {
		l.t.Fatal(err)
	}
	request, err := http.NewRequest(http.MethodPut, l.dispatch.base+"/api/v1/issues/"+l.key+"/steps/"+id, bytes.NewReader(encoded))
	if err != nil {
		l.t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer "+bearer)
	return l.send(request)
}

// holdsID is whether read holds record id.
func (l *stepRecordsLive) holdsID(read stepRecordsRead, id string) bool {
	for _, record := range read.records() {
		if record.ID == id {
			return true
		}
	}
	return false
}

// stepsPrinted is what `dispatch steps` printed: its header's counts and one line per run.
type stepsPrinted struct {
	steps, tasks int
	lines        []stepLine
}

// stepLine is one run as `dispatch steps` prints it: `<task> <step> <kind> <role> <type|->
// attempt <a> <agent|-> <model|-> <started_at|-> <ended_at|running> <result|running>`.
type stepLine struct {
	text                                 string
	task, kind, role, typ                string
	step, attempt                        int
	agent, model, started, ended, result string
}

// parseSteps reads the output of `dispatch steps --issue key`: the header `<key>: <n> steps in <m>
// tasks`, then exactly n lines of runs.
func parseSteps(key, out string) (stepsPrinted, error) {
	lines := strings.Split(strings.TrimSpace(out), "\n")
	var printed stepsPrinted
	header := strings.TrimPrefix(lines[0], key+": ")
	if header == lines[0] {
		return printed, fmt.Errorf("the first line %q is no header of %s", lines[0], key)
	}
	if _, err := fmt.Sscanf(header, "%d steps in %d tasks", &printed.steps, &printed.tasks); err != nil {
		return printed, fmt.Errorf("the header %q: %v", lines[0], err)
	}
	for _, text := range lines[1:] {
		fields := strings.Fields(text)
		if len(fields) != 12 || fields[5] != "attempt" {
			return printed, fmt.Errorf("the line %q is no run", text)
		}
		step, err := strconv.Atoi(fields[1])
		if err != nil {
			return printed, fmt.Errorf("the line %q: step %v", text, err)
		}
		attempt, err := strconv.Atoi(fields[6])
		if err != nil {
			return printed, fmt.Errorf("the line %q: attempt %v", text, err)
		}
		printed.lines = append(printed.lines, stepLine{
			text: text, task: fields[0], step: step, kind: fields[2], role: fields[3], typ: fields[4], attempt: attempt,
			agent: fields[7], model: fields[8], started: fields[9], ended: fields[10], result: fields[11],
		})
	}
	if len(printed.lines) != printed.steps {
		return printed, fmt.Errorf("the header says %d steps and %d lines follow it", printed.steps, len(printed.lines))
	}
	return printed, nil
}

// holdsTen says whether printed is the walk's read: exactly the ten runs of stepRecordsTen, under
// the header `10 steps in 1 tasks`.
func holdsTen(printed stepsPrinted) error {
	if err := holdsLines(printed.lines, stepRecordsTen, true); err != nil {
		return err
	}
	if printed.steps != len(stepRecordsTen) || printed.tasks != 1 {
		return fmt.Errorf("the header says %d steps in %d tasks, want %d steps in 1 tasks", printed.steps, printed.tasks, len(stepRecordsTen))
	}
	return nil
}

// holdsLines says whether lines hold each run of want, of the task default, ended (its end a
// moment, printedMoment) with its result, with an agent, and each of kind step with the model that
// answered it; with exact, they hold no other run. The error names the first run that is missing
// or wrong as lineName spells it.
func holdsLines(lines []stepLine, want []stepWant, exact bool) error {
	for _, w := range want {
		got, name, typ := findLine(lines, w), lineName(w), printedType(w)
		switch {
		case got.text == "":
			return fmt.Errorf("no line of %s", name)
		case got.task != "default" || got.role != w.role || got.typ != typ || got.agent == "-":
			return fmt.Errorf("%s prints %q: want task default, role %s, type %s and an agent", name, got.text, w.role, typ)
		case got.started == "-" || !printedMoment(got.ended) || got.result != w.result:
			return fmt.Errorf("%s prints %q: want a start, an end and result %s", name, got.text, w.result)
		case w.kind == "step" && got.model != stepRecordsModel:
			return fmt.Errorf("%s prints %q: want the model %s", name, got.text, stepRecordsModel)
		}
	}
	if exact && len(lines) != len(want) {
		return fmt.Errorf("%d lines of runs, want %d", len(lines), len(want))
	}
	return nil
}

// printedMoment is whether s, a column of `dispatch steps`, is a moment as Dispatch writes one
// (RFC 3339): an end is one, and `running`, which steps.ts prints for a run that has not ended, or
// any other text, is none.
func printedMoment(s string) bool {
	_, err := time.Parse(time.RFC3339Nano, s)
	return err == nil
}

// findLine is the line of w's kind, step and attempt, the zero line when lines hold none.
func findLine(lines []stepLine, w stepWant) stepLine {
	for _, line := range lines {
		if line.kind == w.kind && line.step == w.step && line.attempt == w.attempt {
			return line
		}
	}
	return stepLine{}
}

// printedType is w's type as the command prints it: `-` for the retro and the merge, which have
// none.
func printedType(w stepWant) string {
	if w.typ == "" {
		return "-"
	}
	return w.typ
}

// lineName spells w as the command prints its leading fields, then its result: `4 step reviewer
// review attempt 1 … failed`.
func lineName(w stepWant) string {
	return fmt.Sprintf("%d %s %s %s attempt %d … %s", w.step, w.kind, w.role, printedType(w), w.attempt, w.result)
}

// The driver's end check, which the proof runs over what `dispatch steps` prints: a line ends its
// run only when it prints a moment as its end. steps.ts prints `running` there, and as the result,
// for a run that has not ended; `-`, which a printer that wrote the end through orDash would print,
// and any other word are no end either, so each fails the walk's read, naming the line. The control
// is the same ten lines with every end a moment, which the check passes.
func TestTheDriverReadsAnEndOnlyFromAMoment(t *testing.T) {
	const key = "SR-1"
	printed := func(end, result string) string {
		lines := []string{key + ": 10 steps in 1 tasks"}
		for _, w := range stepRecordsTen {
			model, lineEnd, lineResult := "-", "2026-11-20T09:40:00.123456Z", w.result
			if w.kind == "step" {
				model = stepRecordsModel
			}
			if w.kind == "step" && w.step == 2 && w.attempt == 2 {
				lineEnd, lineResult = end, result
			}
			lines = append(lines, fmt.Sprintf("default %d %s %s %s attempt %d agent-%s %s 2026-11-20T09:00:00Z %s %s",
				w.step, w.kind, w.role, printedType(w), w.attempt, w.role, model, lineEnd, lineResult))
		}
		return strings.Join(lines, "\n") + "\n"
	}
	read := func(out string) error {
		parsed, err := parseSteps(key, out)
		if err != nil {
			t.Fatalf("parse %q: %v", out, err)
		}
		return holdsTen(parsed)
	}
	if err := read(printed("2026-11-20T09:40:00Z", "passed")); err != nil {
		t.Fatalf("control: the ten lines, each ended at a moment, read as %v, want the walk's ten", err)
	}
	for _, no := range []struct{ end, result string }{{"running", "running"}, {"-", "passed"}, {"unknown", "passed"}} {
		err := read(printed(no.end, no.result))
		if err == nil || !strings.Contains(err.Error(), "2 step implementer implementation attempt 2") || !strings.Contains(err.Error(), "an end") {
			t.Errorf("the ten lines with implementing's second run ending %q %q read as %v, want an error naming that run and its end", no.end, no.result, err)
		}
	}
}
