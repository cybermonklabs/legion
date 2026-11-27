//go:build e2e

// The live proof's driver for the group of each new issue: a project declares the experiment
// planning, whose arm a runs each issue as its owner gives it (plan owner) and whose arm b runs the
// project's default steps (plan default). The whole daemon records the group each new issue is
// drawn into, the operator reads it with the real `legion state`, the owner of an issue of a has
// its plan taken, and the owner of an issue of b is answered PLAN_NOT_TAKEN.
// scripts/e2e/experiment.sh builds this driver with `-tags e2e` and owns the nats-server and the
// PostgreSQL it runs against.
//
// Nothing of the draw's path is stood in for. Dispatch is the real server this checkout builds
// (startScratchDispatchOn), on a database of its own, its events on the script's broker, which the
// daemon consumes, as the person's handover proof runs it (personHandoverRig). The daemon is the
// whole daemon (run, through startDaemonLogging) with dispatch_url at that server. Each issue is
// created and handed to Legion as the person's todo through Dispatch's API, as at the dashboard;
// its owner writes its plan as the document plan.md with the real `dispatch artifact`
// (packages/envoy-client/bin/dispatch.ts under bun), as its prompt tells it to, and gives it with
// register_plan through the daemon's API, as its plugin does; and the operator reads the groups
// with the `legion` this checkout builds.
//
// What is stood in for lies outside the draw: GitHub's REST API (blockersGitHub), the Apps'
// tokens (workflowTokenRecorder), Envoy's listener (unreachable, as the rig leaves it), and the
// panes, which the package's fake runtime launches and nothing plays.
package daemon

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/fixtureenv"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/testwait"
)

// experimentLive is one run of the proof: the person's handover rig with the daemon it boots, the
// checkout the two commands run out of, the `legion` this run built, the dispatch CLI's own home,
// and the issues the steps drove. The embedded liveProof gives the steps their `== <name>` and
// `ok <name>` lines and their notes, and nothing else of it is set.
type experimentLive struct {
	liveProof
	r       *personHandoverRig
	root    string
	legion  string
	cliHome string
	// drawnA are the two issues drawn into the arm a, the first of which its owner gives a plan,
	// and drawnB the issue drawn into the arm b after the second boot.
	drawnA []experimentIssue
	drawnB experimentIssue
}

// experimentIssue is one issue the proof handed to Legion, and a minter of the grants its
// architect's registered session holds.
type experimentIssue struct {
	scratchIssue
	grant func() string
}

// TestExperimentLive drives the proof's steps against one Dispatch and the daemon booted twice on
// one database: first with the experiment at mode on and the arm a, for two issues, then with the
// arm b, for a third. Each issue keeps the arm of its draw whatever the settings say later, which
// the operator's line after the second boot shows, and each record of a step names the variant of
// its issue, which `dispatch steps` prints.
func TestExperimentLive(t *testing.T) {
	// Not parallel: it gives the process back the operator's own environment, as t.Setenv does (fixtureenv.Live).
	fixtureenv.Live(t)
	natsURL := os.Getenv(liveNATSURLEnv)
	if natsURL == "" {
		t.Skipf("%s is unset, so there is no broker for Dispatch's events; scripts/e2e/experiment.sh starts a nats-server and sets it", liveNATSURLEnv)
	}
	root := liveCLICheckout(t)
	options := scratchDispatchOptions{natsURL: natsURL, root: root}
	daemonLog := os.Getenv(liveDaemonLogEnv)
	if daemonLog != "" {
		options.log = evidenceFile(t, filepath.Join(filepath.Dir(daemonLog), "dispatch.log"))
	}
	r := newPersonHandoverRigOn(t, options, 3)
	if daemonLog != "" {
		r.daemonLog = evidenceFile(t, daemonLog)
	}
	l := &experimentLive{liveProof: liveProof{t: t}, r: r, root: root, legion: buildProofLegion(t, root, t.TempDir()), cliHome: t.TempDir()}
	l.declare("a")
	r.boot()
	l.note("Dispatch %s (database of its own, events on %s), project %s whose default repository is acme/widgets; daemon at %s, admission cap %d; the project declares the experiment planning at mode on, arm a",
		r.baseURL, natsURL, r.cfg.Project, r.d.base, r.cfg.AdmissionCap)
	l.step("group-recorded", l.groupRecorded)
	l.step("owner-plan-taken", l.ownerPlanTaken)
	l.step("default-not-taken", l.defaultNotTaken)
	l.step("variant-on-steps", l.variantOnSteps)
}

// declare sets the daemon's project to declare the experiment planning at mode on, giving every
// new issue the arm onVariant; the daemon reads it at its next boot.
func (l *experimentLive) declare(onVariant string) {
	l.r.cfg.Projects[l.r.cfg.Project] = config.Project{Experiments: []config.Experiment{planningExperiment(config.ExperimentOn, onVariant)}}
}

// groupRecorded hands two issues to Legion with the arm a in force. `legion state` prints the
// line `planning: 2 issues — 2 a (on)`, and `legion state --json` shows on each issue its
// assignment of planning, the arm a at mode on with the instant of its draw and the group the arm
// set, `owner`. Control: before either issue, the same command prints `planning: none`.
func (l *experimentLive) groupRecorded() {
	before := l.legionState()
	if !slices.Contains(strings.Split(before, "\n"), "planning: none") {
		l.t.Fatalf("`legion state` before any issue printed no line `planning: none`:\n%s", before)
	}
	l.note("before any issue, `legion state` prints `planning: none`")
	for _, title := range []string{"Ledger reconciles nightly", "Billing exports quarterly totals"} {
		created := l.r.create(asSession, title, map[string]any{"labels": []string{"legion"}})
		l.drawnA = append(l.drawnA, experimentIssue{scratchIssue: created, grant: l.r.admit(created.Key)})
	}
	const want = "planning: 2 issues — 2 a (on)"
	after := l.legionState()
	if !slices.Contains(strings.Split(after, "\n"), want) {
		l.t.Fatalf("`legion state` after two issues at the arm a printed no line %q:\n%s", want, after)
	}
	l.note("%s and %s handed to Legion; `legion state` prints %q", l.drawnA[0].Key, l.drawnA[1].Key, want)
	var state api.State
	if err := json.Unmarshal([]byte(l.legionState("--json")), &state); err != nil {
		l.t.Fatalf("decode `legion state --json`: %v", err)
	}
	for _, issue := range l.drawnA {
		assignments := state.Issues[issue.Key].Assignments
		at := slices.IndexFunc(assignments, func(a api.AssignmentView) bool { return a.Experiment == "planning" })
		if at < 0 || assignments[at].Variant != "a" || assignments[at].Mode != string(config.ExperimentOn) || assignments[at].DrawnAt == nil || assignments[at].Plan != string(config.PlanGroupOwner) {
			l.t.Fatalf("`legion state --json` shows %s's assignments as %+v; want planning at the arm a, mode on, with the instant of its draw and the group owner", issue.Key, assignments)
		}
		l.note("`legion state --json` shows %s in planning, arm %s, mode %s, group %s, drawn at %s", issue.Key, assignments[at].Variant, assignments[at].Mode, assignments[at].Plan, assignments[at].DrawnAt)
	}
}

// ownerPlanTaken: the owner of the first issue of the arm a writes plan.md with `dispatch
// artifact` and gives it with register_plan, which takes it; once its gate opens, the issue's run
// starts at implementing, the plan's first step, on the plan of the source owner.
func (l *experimentLive) ownerPlanTaken() {
	issue := l.drawnA[0]
	document := l.writePlan(issue.Key)
	if answer := l.r.givePlan(issue.Key, document, issue.grant); answer.status != http.StatusOK {
		l.t.Fatalf("register_plan on %s, of the arm a, answered %+v; want 200, the plan taken", issue.Key, answer)
	}
	l.note("register_plan on %s answered 200: the plan `implementer reviewer` is taken", issue.Key)
	ran := l.firstStep(issue)
	if ran.firstStep != phase.Implementing || ran.source != record.PlanSourceOwner || ran.variant != "a" || ran.plan != string(config.PlanGroupOwner) {
		l.t.Fatalf("%s came to %+v; want the arm a, whose plan is owner, and its run at implementing on the owner's plan", issue.Key, ran)
	}
	l.note("its gate opened: %s runs at %s on the plan of the source %s", issue.Key, ran.firstStep, ran.source)
}

// defaultNotTaken: the daemon is stopped and booted again with the arm b in force, and a third
// issue is handed to Legion. Its owner writes plan.md and gives it with register_plan twice, and
// each send is answered 409 PLAN_NOT_TAKEN with the group's sentence; once its gate opens, its run
// starts at planning, the project's default steps, on the plan of the source default. `legion
// state` then counts the two issues of a beside the one of b: each keeps the arm of its draw.
func (l *experimentLive) defaultNotTaken() {
	l.r.d.stop()
	rebindHeldPorts(l.t, &l.r.cfg)
	l.declare("b")
	l.r.boot()
	l.note("the daemon was stopped and booted again with the experiment planning at the arm b")
	created := l.r.create(asSession, "Search ranks fresher pages", map[string]any{"labels": []string{"legion"}})
	issue := experimentIssue{scratchIssue: created, grant: l.r.admit(created.Key)}
	l.drawnB = issue
	document := l.writePlan(issue.Key)
	refusal := intake.PlanNotTaken(issue.Key, record.Assignment{Experiment: "planning", Variant: "b", Plan: string(config.PlanGroupDefault)})
	want := planAnswer{status: refusal.Status, failure: api.Failure{Code: refusal.Code, Error: refusal.Message}}
	for send := 1; send <= 2; send++ {
		if answer := l.r.givePlan(issue.Key, document, issue.grant); answer != want {
			l.t.Fatalf("send %d of register_plan on %s, of the arm b, answered %+v; want %+v", send, issue.Key, answer, want)
		}
		l.note("send %d of register_plan on %s answered %d %s: %s", send, issue.Key, want.status, want.failure.Code, want.failure.Error)
	}
	ran := l.firstStep(issue)
	if ran.firstStep != phase.Planning || ran.source != record.PlanSourceDefault || ran.variant != "b" || ran.plan != string(config.PlanGroupDefault) {
		l.t.Fatalf("%s came to %+v; want the arm b, whose plan is default, and its run at planning on the default steps", issue.Key, ran)
	}
	l.note("its gate opened: %s runs at %s on the plan of the source %s", issue.Key, ran.firstStep, ran.source)
	const counted = "planning: 3 issues — 2 a (on), 1 b (on)"
	if state := l.legionState(); !slices.Contains(strings.Split(state, "\n"), counted) {
		l.t.Fatalf("`legion state` after the second boot printed no line %q:\n%s", counted, state)
	}
	l.note("`legion state` prints %q: the issues drawn before the second boot keep the arm a", counted)
}

// variantOnSteps reads the variant of each issue as a person reads it, with the real `dispatch
// steps --issue <KEY>` under bun: the third issue, drawn at the arm b, prints `Experiment
// planning: variant b.` right after its header and before its first run, and the first, drawn at
// the arm a, `Experiment planning: variant a.` there. The daemon writes each step record of an
// issue in a group with the experiment and the variant of its draw (the workflow's stepWrite), and
// its outbox sends the pair after the record, on the record's experiment route of this checkout's
// Dispatch, so the line stands once that row has run: the command runs again until it prints the
// line, within the wait's bound, and the line must then stand in that place (variantPlace).
// Control: a daemon whose step write leaves the pair out prints neither line, and this step fails;
// a `dispatch steps` that prints the line after the runs fails it too, naming where the line stands.
func (l *experimentLive) variantOnSteps() {
	shell := cliShell{t: l.t, dir: l.cliHome, session: scratchSession, base: l.r.baseURL, token: scratchAgentToken}
	for _, want := range []struct {
		issue experimentIssue
		line  string
	}{
		{issue: l.drawnB, line: "Experiment planning: variant b."},
		{issue: l.drawnA[0], line: "Experiment planning: variant a."},
	} {
		var printed, out string
		if !testwait.Holds(testwait.Bounded(l.t, time.Minute, 5*time.Second), 500*time.Millisecond, func() bool {
			ran, code := shell.bun(nil, dispatchCLI(l.root), "steps", "--issue", want.issue.Key)
			printed = string(ran.stdout)
			out = printed + string(ran.stderr)
			return code == 0 && slices.Contains(strings.Split(printed, "\n"), want.line)
		}) {
			l.t.Fatalf("`dispatch steps --issue %s` never printed the line %q; its last run printed:\n%s", want.issue.Key, want.line, out)
		}
		if err := variantPlace(want.issue.Key, want.line, printed); err != nil {
			l.t.Fatalf("`dispatch steps --issue %s` printed the line %q out of its place: %v; it printed:\n%s", want.issue.Key, want.line, err, out)
		}
		l.note("`dispatch steps --issue %s` prints %q right after its header and before its first run", want.issue.Key, want.line)
	}
}

// variantPlace answers nil where line stands in printed, the stdout of `dispatch steps --issue
// key`, where the command prints the variant of the issue: right after the header `<key>: <n>
// steps in <m> tasks` and before the first run, with the header's n runs below it and nothing
// else. It answers where the line stands otherwise.
func variantPlace(key, line, printed string) error {
	lines := strings.Split(strings.TrimRight(printed, "\n"), "\n")
	header, ok := strings.CutPrefix(lines[0], key+": ")
	var steps, tasks int
	if _, err := fmt.Sscanf(header, "%d steps in %d tasks", &steps, &tasks); !ok || err != nil {
		return fmt.Errorf("the first line %q is no header of %s", lines[0], key)
	}
	if at := slices.Index(lines, line); at != 1 {
		if at < 1 {
			return fmt.Errorf("no line below the header is %q", line)
		}
		return fmt.Errorf("it is line %d of %d, after %q; want it right after the header and before the first run", at+1, len(lines), lines[at-1])
	}
	runs := lines[2:]
	for _, run := range runs {
		if fields := strings.Fields(run); len(fields) != 12 || fields[5] != "attempt" {
			return fmt.Errorf("the line %q below it is no run", run)
		}
	}
	if steps == 0 || len(runs) != steps {
		return fmt.Errorf("the header says %d steps and %d runs follow the line", steps, len(runs))
	}
	return nil
}

// writePlan writes ownersPlan as the document plan.md of key with the real `dispatch artifact`,
// run as the owner's session runs it, and answers the document's id, which register_plan names,
// from Dispatch's listing of the issue's documents.
func (l *experimentLive) writePlan(key string) string {
	l.t.Helper()
	shell := cliShell{t: l.t, dir: l.cliHome, session: scratchSession, base: l.r.baseURL, token: scratchAgentToken}
	ran, code := shell.bun(nil, dispatchCLI(l.root), "artifact", "--issue", key, "--name", "plan.md", "--content", ownersPlan)
	out := string(ran.stdout) + string(ran.stderr)
	if code != 0 {
		l.t.Fatalf("`dispatch artifact --issue %s --name plan.md` exited %d:\n%s", key, code, out)
	}
	l.note("`dispatch artifact --issue %s --name plan.md`: %s", key, strings.SplitN(strings.TrimSpace(out), "\n", 2)[0])
	var documents []struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	}
	dispatchCall(l.t, l.r.baseURL, http.MethodGet, "/api/v1/issues/"+key+"/artifacts", nil, &documents)
	for _, document := range documents {
		if document.Name == "plan.md" {
			return document.ID
		}
	}
	l.t.Fatalf("Dispatch lists no plan.md among the documents of %s: %+v", key, documents)
	return ""
}

// firstStep opens issue's gate as the person does, waits for its run to start, and reads from the
// daemon's database the row of planning it holds, the phase its run started in and the source of
// the plan it walks.
func (l *experimentLive) firstStep(issue experimentIssue) drawnIssue {
	l.t.Helper()
	l.r.registerGate(issue.Key, issue.PrimaryArtifactID, issue.grant)
	l.r.approve(issue.PrimaryArtifactID)
	settled(l.t, l.r.d, "start the first step of "+issue.Key, func(s api.State) bool {
		started := s.Issues[issue.Key].Phase
		return started != "" && started != phase.Admitted
	})
	ran := drawnIssue{key: issue.Key}
	l.r.readDrawn(&ran)
	return ran
}

// legionState runs the `legion state` this run built, with args, as an operator's shell runs it
// against the daemon, and answers what it printed; a run that fails fails the proof.
func (l *experimentLive) legionState(args ...string) string {
	l.t.Helper()
	command := exec.Command(l.legion, append([]string{"state"}, args...)...)
	command.Dir = l.cliHome
	command.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + l.cliHome, "LEGION_DAEMON_URL=" + l.r.d.base}
	var stdout, stderr bytes.Buffer
	command.Stdout, command.Stderr = &stdout, &stderr
	if err := command.Run(); err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			l.t.Fatalf("`legion state %s` exited %d:\n%s%s", strings.Join(args, " "), exit.ExitCode(), stdout.String(), stderr.String())
		}
		l.t.Fatalf("run `legion state %s`: %v", strings.Join(args, " "), err)
	}
	return stdout.String()
}
