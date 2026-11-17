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
// whole-daemon tests walk it (stepRecordsRig, step_records_test.go): each role over the worker
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
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/fixtureenv"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/testbuild"
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
	// failed is the id of the failed review's record, which the read's control deletes.
	failed string
}

// TestStepRecordsLive drives the proof's steps against one booted daemon and one Dispatch, over
// one issue that walks planning to done with one failed review. The handoff of planning,
// implementing and testing is read from Dispatch at each completion, so Dispatch is stopped only
// once the issue is in reviewing for the second time: the second review, the acceptance, the retro
// and the merge then walk it to done with Dispatch stopped.
func TestStepRecordsLive(t *testing.T) {
	fixtureenv.Live(t)
	root := liveCheckout(t)
	proof := newLiveProof(t, "scripts/e2e/step-records.sh")
	requireBun(t)
	l := &stepRecordsLive{stepRecordsRig: newStepRecordsRigOn(t, proof, startHandoffDispatch(t, root).scratchDispatch), root: root, cliHome: t.TempDir()}
	l.mintReader("steps-reader")
	l.note("Dispatch %s (database of its own, NATS off), project %s mapped to %s, issue %s on pull request %d; daemon at dispatch_url %s; the reader is the person's personal token",
		l.dispatch.base, l.cfg.Project, liveRepo, l.key, l.number, l.cfg.DispatchURL)
	l.step("walk-to-the-second-review-with-dispatch-up", l.walkToTheSecondReview)
	l.step("a-phase-moves-while-dispatch-is-down", l.outage)
	l.step("walk-to-done-with-one-failed-review", l.readByIssue)
	l.step("a-write-that-names-another-session-is-refused", l.writerRefused)
}

// liveCheckout is the checkout the proof's script runs from, whose packages/envoy the driver builds
// Dispatch from and whose packages/envoy-client holds the CLI: testbuild.RootEnv, which the script
// exports, since the driver binary runs from the caller's directory, not the package's. Unset (a
// plain `go test -tags e2e`, which runs in the package directory), it is four directories up.
func liveCheckout(t *testing.T) string {
	t.Helper()
	root := os.Getenv(testbuild.RootEnv)
	if root == "" {
		root = filepath.Join("..", "..", "..", "..")
	}
	root, err := filepath.Abs(root)
	if err != nil {
		t.Fatal(err)
	}
	return root
}

// requireBun fails the run when bun, which the dispatch CLI runs under, is not on PATH.
func requireBun(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("bun"); err != nil {
		t.Fatalf("bun is not on PATH, and the dispatch CLI runs under it: %v", err)
	}
}

// mintReader has Dispatch mint a personal token, named name, for the person who set the run's
// project up (POST /api/v1/me/agent-tokens), which every read of the CLI then carries.
func (l *stepRecordsLive) mintReader(name string) {
	l.t.Helper()
	var minted struct {
		Token string `json:"token"`
	}
	l.person(http.MethodPost, "/api/v1/me/agent-tokens", map[string]string{"name": name}, http.StatusCreated, &minted)
	if minted.Token == "" {
		l.t.Fatal("Dispatch minted the person a personal token with no plaintext")
	}
	l.reader = minted.Token
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
		var output string
		if output, code = l.steps(); code != 0 {
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

// readByIssue is v-records, the read by issue: `dispatch steps` run as a person of the project
// prints the header `<KEY>: 10 steps in 1 tasks` and one line for each of the walk's ten runs,
// each ended with its result, each with an agent, and each of kind step with the model that
// answered it. The control deletes the failed review's record from Dispatch's own store, its node
// and every tuple that names it, and the same parse of the same command must then report that
// record missing.
func (l *stepRecordsLive) readByIssue() {
	t := l.t
	out, code := l.steps()
	if code != 0 {
		t.Fatalf("`dispatch steps --issue %s` exited %d: %s", l.key, code, out)
	}
	printed, err := parseSteps(l.key, out)
	if err == nil {
		err = holdsTen(printed)
	}
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
	l.failed = read.find("step", 4, 1).ID
	if l.failed == "" {
		t.Fatalf("GET /api/v1/issues/%s/steps holds no record of step 4 attempt 1", l.key)
	}
	l.deleteStepRecord(l.failed)
	l.note("control: deleted the failed review's record %s from Dispatch's database (its workflow_nodes row and its workflow_tuples rows)", l.failed)
	out, code = l.steps()
	if code != 0 {
		t.Fatalf("control: `dispatch steps --issue %s` after the delete exited %d: %s", l.key, code, out)
	}
	printed, err = parseSteps(l.key, out)
	if err == nil {
		err = holdsTen(printed)
	}
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

// steps runs `dispatch steps --issue <KEY>` as the person and returns both streams and its exit
// code.
func (l *stepRecordsLive) steps() (string, int) {
	l.t.Helper()
	return l.cli("steps", "--issue", l.key)
}

// cli runs the dispatch CLI with args as the person and returns both streams and its exit code.
func (l *stepRecordsLive) cli(args ...string) (string, int) {
	l.t.Helper()
	command := exec.Command("bun", append([]string{filepath.Join(l.root, "packages", "envoy-client", "bin", "dispatch.ts")}, args...)...)
	command.Env = l.personsShell(os.Getenv("PATH"))
	return l.run(command, "dispatch "+strings.Join(args, " "))
}

// personsShell is the environment of a person's own shell running dispatch by hand, on path: the
// host and a session of their own, the server and their personal token. Nothing else of the
// caller's environment reaches it, and its home, configuration file (absent) and session state
// are the run's own.
func (l *stepRecordsLive) personsShell(path string) []string {
	l.t.Helper()
	if err := os.MkdirAll(filepath.Join(l.cliHome, "home"), 0o700); err != nil {
		l.t.Fatal(err)
	}
	return []string{
		"PATH=" + path,
		"HOME=" + filepath.Join(l.cliHome, "home"),
		"TMPDIR=" + l.cliHome,
		"XDG_CONFIG_HOME=" + filepath.Join(l.cliHome, "config"),
		"XDG_STATE_HOME=" + filepath.Join(l.cliHome, "state"),
		"XDG_CACHE_HOME=" + filepath.Join(l.cliHome, "cache"),
		"ENVOY_CONFIG_FILE=" + filepath.Join(l.cliHome, "envoy.json"),
		"DISPATCH_STATE_DIR=" + filepath.Join(l.cliHome, "dispatch-state"),
		"DISPATCH_HOST=omp",
		"DISPATCH_SESSION_ID=" + stepRecordsReaderSession,
		"DISPATCH_URL=" + l.dispatch.base,
		"DISPATCH_TOKEN=" + l.reader,
	}
}

// run runs command, named what, in the person's directory and returns both streams and its exit
// code.
func (l *stepRecordsLive) run(command *exec.Cmd, what string) (string, int) {
	l.t.Helper()
	command.Dir = l.cliHome
	var output bytes.Buffer
	command.Stdout, command.Stderr = &output, &output
	code := 0
	if err := command.Run(); err != nil {
		exit, ok := err.(*exec.ExitError)
		if !ok {
			l.t.Fatalf("run `%s`: %v", what, err)
		}
		code = exit.ExitCode()
	}
	return output.String(), code
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

// holdsLines says whether lines hold each run of want, of the task default, ended with its
// result, with an agent, and each of kind step with the model that answered it; with exact, they
// hold no other run. The error names the first run that is missing or wrong as lineName spells it.
func holdsLines(lines []stepLine, want []stepWant, exact bool) error {
	for _, w := range want {
		got, name := findLine(lines, w), lineName(w)
		typ := w.typ
		if typ == "" {
			typ = "-"
		}
		switch {
		case got.text == "":
			return fmt.Errorf("no line of %s", name)
		case got.task != "default" || got.role != w.role || got.typ != typ || got.agent == "-":
			return fmt.Errorf("%s prints %q: want task default, role %s, type %s and an agent", name, got.text, w.role, typ)
		case got.started == "-" || got.ended == "running" || got.ended == "-" || got.result != w.result:
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

// findLine is the line of w's kind, step and attempt, the zero line when lines hold none.
func findLine(lines []stepLine, w stepWant) stepLine {
	for _, line := range lines {
		if line.kind == w.kind && line.step == w.step && line.attempt == w.attempt {
			return line
		}
	}
	return stepLine{}
}

// lineName spells w as the command prints its leading fields, then its result: `4 step reviewer
// review attempt 1 … failed`.
func lineName(w stepWant) string {
	typ := w.typ
	if typ == "" {
		typ = "-"
	}
	return fmt.Sprintf("%d %s %s %s attempt %d … %s", w.step, w.kind, w.role, typ, w.attempt, w.result)
}
