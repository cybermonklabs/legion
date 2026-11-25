//go:build e2e

// The live proof of the owner's plan through the plugin's own daemon client: the calls the `legion`
// tool makes for register_plan, stop_task and handoff_complete, made by createLegionDaemonClient
// (packages/pi-shared/src/daemon-client.ts) under bun, with grants minted by mintClaimGrant and the
// completion sent by the phase op itself (executePhaseOp, legion-phase-ops.ts), against the whole
// daemon booted as the owner's steps driver boots it (newOwnerStepsRig over a Dispatch this
// checkout builds). scripts/e2e/owner-steps-client.sh builds this driver with `-tags e2e`; each
// call is one run of scripts/e2e/owner-steps-client.ts, which prints what the client answered or
// threw.
//
// A second client is the plugin of the release before, which the driver builds itself
// (olderClient): the newest commit of main's first-parent line whose daemon API contract is below
// this one's, taken with `git archive` and given its own `bun install`. Its tester registers at that
// contract and reads the answers of this daemon through that tree's client and schemas. That
// release is whatever main deployed last, so its contract moves as main's does: what its client
// must have and what the daemon must serve it are read from the contract its own tree declares,
// never from a contract this file names for it.
//
// Each check prints `== <name>`, what the client answered, and `ok <name>`; each control runs the
// same check's predicate on an answer it must refuse, and fails the run unless it does.
package daemon

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/fixtureenv"
	"github.com/sjawhar/legion/daemon/internal/phase"
	planwords "github.com/sjawhar/legion/daemon/internal/plan"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// clientPlan is the plan every issue of the run is given: one task a, the implementer then the
// tester, so a failed tester's returnTo has an earlier step to name.
var clientPlan = []string{"implementer", "tester"}

// ownerStepsClientLive is one run: the rig over four issues, the checkout the client runs from,
// and the tree and contract of the release before, which older-client-tree builds.
type ownerStepsClientLive struct {
	*ownerStepsRig
	root        string
	oldTree     string
	oldContract int
}

// clientOutcome is what one run of owner-steps-client.ts printed: the client's answer, or the
// class, status, code and message of what it threw, and the client's own calls.
type clientOutcome struct {
	OK      bool            `json:"ok"`
	Answer  json.RawMessage `json:"answer"`
	Error   string          `json:"error"`
	Status  int             `json:"status"`
	Code    string          `json:"code"`
	Message string          `json:"message"`
	Methods []string        `json:"methods"`
}

func (o clientOutcome) String() string {
	if o.OK {
		return "answered " + string(o.Answer)
	}
	return fmt.Sprintf("threw %s %d %s: %s", o.Error, o.Status, o.Code, o.Message)
}

// heldOlder is nil when outcome, the state read of the release before's client of an issue held
// task-stopped, is what that release can read: its own schema parsed the body (the read answered)
// and the issue is in it held from testing. A release below api.OwnerPlanContract must also be
// served neither a holdReason nor a row of admission.held naming a reason that arrived at that
// contract, which it does not know: the daemon serves such a caller the issue with no holdReason
// and the row as unclassified (api's withoutUnadmittedHoldReason and withoutUnadmittedHeldWords).
func heldOlder(outcome clientOutcome, contract int) error {
	if !outcome.OK {
		return fmt.Errorf("it %s; the release before's own schema must parse what this daemon serves it", outcome)
	}
	var read struct {
		Issue *struct {
			HoldReason string `json:"holdReason"`
			HeldFrom   string `json:"heldFrom"`
		} `json:"issue"`
		Held []struct {
			Reason string `json:"reason"`
		} `json:"held"`
	}
	if err := json.Unmarshal(outcome.Answer, &read); err != nil {
		return fmt.Errorf("decode its answer %s: %w", outcome.Answer, err)
	}
	if read.Issue == nil || read.Issue.HeldFrom != string(phase.Testing) {
		return fmt.Errorf("it answered %s, want the issue held from testing", outcome.Answer)
	}
	if contract >= api.OwnerPlanContract {
		return nil
	}
	if reason := read.Issue.HoldReason; reason == string(record.HoldTaskStopped) || reason == string(record.HoldForChildren) {
		return fmt.Errorf("it answered %s: holdReason %q arrived at contract %d, which contract %d does not know", outcome.Answer, reason, api.OwnerPlanContract, contract)
	}
	for _, row := range read.Held {
		if row.Reason == string(record.HeldTaskStopped) || row.Reason == string(record.HeldChildrenBeforeRetro) {
			return fmt.Errorf("it answered %s: the held word %q arrived at contract %d, which contract %d does not know", outcome.Answer, row.Reason, api.OwnerPlanContract, contract)
		}
	}
	return nil
}

func TestOwnerStepsClientLive(t *testing.T) {
	l := newOwnerStepsClientLive(t)
	planned, returnRefused, stopped, older := l.issues[0].key, l.issues[1].key, l.issues[2].key, l.issues[3].key
	l.note("Dispatch %s, project %s, issues %s, %s, %s and %s; daemon at %s (contract %d); client of %s",
		l.dispatch.base, l.cfg.Project, planned, returnRefused, stopped, older, l.d.base, api.DaemonAPIVersion, l.root)
	l.check("older-client-tree", l.olderClient)

	// register_plan with the architect's grant, and its control: the same call naming a version
	// Dispatch does not hold must fail the same check.
	var plan string
	var architectRegistered clientOutcome
	l.check("control-client-plan-version-absent", func() {
		l.on(planned)
		plan = l.document("plan.md", planOf(clientPlan...))
		outcome := l.client(l.root, l.worker(claim.RoleArchitect), "register_plan", map[string]any{"issue": planned, "artifactId": plan, "version": 2}, nil)
		l.note("register_plan of version 2, which Dispatch does not hold, with the architect's grant: %s", outcome)
		err := planTaken(outcome, clientPlan)
		if err == nil {
			t.Fatalf("control: the check of register_plan passed on %s", outcome)
		}
		l.note("control: the check fails: %v", err)
	})
	l.check("client-register-plan", func() {
		architectRegistered = l.client(l.root, l.worker(claim.RoleArchitect), "register_plan", map[string]any{"issue": planned, "artifactId": plan, "version": 1}, nil)
		if err := planTaken(architectRegistered, clientPlan); err != nil {
			t.Fatal(err)
		}
		l.note("planRegister with the architect's grant (mintClaimGrant), version 1: %s", architectRegistered)
	})

	// A phase worker's grant is refused ARCHITECT_REQUIRED on both routes; the control is the
	// architect's grant on the same call, which the same check must not pass.
	l.check("client-phase-worker-refused", func() {
		l.openGate(phase.Implementing)
		implementer := l.worker(claim.RoleImplementer)
		register := l.client(l.root, implementer, "register_plan", map[string]any{"issue": planned, "artifactId": plan, "version": 1}, nil)
		stop := l.client(l.root, implementer, "stop_task", map[string]any{"issue": planned, "task": "a"}, nil)
		for what, outcome := range map[string]clientOutcome{"register_plan": register, "stop_task": stop} {
			if err := refused(outcome, http.StatusForbidden, "ARCHITECT_REQUIRED"); err != nil {
				t.Fatalf("%s with the implementer's grant: %v", what, err)
			}
			l.note("%s with the implementer's grant: %s", what, outcome)
		}
		if err := refused(architectRegistered, http.StatusForbidden, "ARCHITECT_REQUIRED"); err == nil {
			t.Fatalf("control: the check of ARCHITECT_REQUIRED passed on the architect's own register_plan, %s", architectRegistered)
		} else {
			l.note("control: the same check on the architect's register_plan fails: %v", err)
		}
	})

	// handoff_complete with returnTo from a failed tester returns the task to that step.
	l.check("client-return-to", func() {
		l.runStep("implementer", answerPass, phase.Testing)
		tester := l.worker(claim.RoleTester)
		tester.take()
		outcome := l.client(l.root, tester, "handoff_complete", map[string]any{"summary": "a criterion fails", "verdict": "fail", "return_to": 1}, l.report(tester))
		if !outcome.OK {
			t.Fatalf("the tester's handoff_complete with verdict fail and return_to 1 %s, want it taken", outcome)
		}
		tester.endTurn()
		l.waitPhase(phase.Implementing)
		if starts := l.stepStarts(); !slices.ContainsFunc(starts, func(s string) bool { return strings.HasPrefix(s, "a step 1 starts 2 ") }) {
			t.Fatalf("%s's step starts after the return are %v, want task a's step 1 started a second time", planned, starts)
		}
		l.note("executePhaseOp handoff_complete {verdict fail, return_to 1} by the tester: %s; %s stands in implementing, task a's step 1 started again (%v)",
			outcome, planned, l.stepStarts())
	})

	// The control of returnTo: a place that is not earlier in the task is refused, so the field
	// reaches the daemon; and the tester's pass after it is taken, the answer TASK_STOPPED's check
	// must refuse.
	var taken clientOutcome
	l.check("control-client-return-to-refused", func() {
		l.on(returnRefused)
		l.givePlan(clientPlan...)
		l.openGate(phase.Implementing)
		l.runStep("implementer", answerPass, phase.Testing)
		tester := l.worker(claim.RoleTester)
		tester.take()
		outcome := l.client(l.root, tester, "handoff_complete", map[string]any{"summary": "a criterion fails", "verdict": "fail", "return_to": 2}, l.report(tester))
		if err := refused(outcome, http.StatusConflict, "RETURN_REFUSED"); err != nil {
			t.Fatalf("return_to 2, the tester's own place: %v", err)
		}
		if got := phase.Phase(l.d.state().Issues[returnRefused].Phase); got != phase.Testing {
			t.Fatalf("%s stands in %s after the refused return, want testing", returnRefused, got)
		}
		l.note("control: handoff_complete {verdict fail, return_to 2} %s; %s still stands in testing, so the check of client-return-to fails on it", outcome, returnRefused)
		taken = l.client(l.root, tester, "handoff_complete", map[string]any{"summary": "every criterion holds", "verdict": "pass"}, l.report(tester))
		if !taken.OK {
			t.Fatalf("the tester's pass after the refused return %s, want it taken", taken)
		}
		tester.endTurn()
		l.waitPhase(phase.Retro)
		l.note("the tester's pass after it: %s; %s reached its retro", taken, returnRefused)
	})

	// The plugin of the release before. This daemon refuses a registration at any contract but its
	// own (claim.StalePluginContract), and relaunches onto the current plugin a pane that speaks
	// another contract once its turn ends after a boot (supervise's plugin-refresh wake). So a pane
	// of the release before meets this daemon only as a pane registered under that release that
	// outlives the upgrade, within the turn it was in: the tester registers and takes its task, its
	// claim's recorded contract is set to the release before's while no daemon runs, as that release
	// left it, and within that one turn its client, of that release's tree, reads this daemon's
	// answers. The daemon serves each client the shape of the contract its request states
	// (api's shapeAnswer and valueStrips), so its read of the issue held task-stopped must parse in
	// its own schema (heldOlder). What else the case holds that release to follows from its contract
	// alone: planRegister and taskStop arrived at api.OwnerPlanContract, so its client has them
	// exactly when it speaks that contract or a later one, and a release below it reads an issue's
	// holdReason with the one value `escalated` and must carry no hold reason that contract added.
	l.check("older-client-reads", func() {
		l.on(older)
		l.givePlan(clientPlan...)
		l.openGate(phase.Implementing)
		l.runStep("implementer", answerPass, phase.Testing)
		tester := l.worker(claim.RoleTester)
		tester.take()
		l.restartWith(func() {
			l.exec("update claims set plugin_contract = $1 where token = $2", l.oldContract, string(tester.token))
		})
		if got := l.d.claim(tester.token).PluginContract; got != l.oldContract {
			t.Fatalf("the older tester's claim records plugin contract %d after the restart, want %d", got, l.oldContract)
		}
		l.note("%s's tester registered, and its claim's plugin_contract set to %d before a restart: the daemon holds contract %d for it", older, l.oldContract, l.oldContract)
		state := l.client(l.oldTree, tester, "state", map[string]any{"issue": older, "project": l.cfg.Project}, nil)
		if !state.OK {
			t.Fatalf("the older client's state read of %s, which stands on plan rows in testing, %s", older, state)
		}
		l.note("contract %d client's methods: %v", l.oldContract, state.Methods)
		planRoutes := l.oldContract >= api.OwnerPlanContract
		for _, route := range []string{"planRegister", "taskStop"} {
			if has := slices.Contains(state.Methods, route); has != planRoutes {
				t.Fatalf("the contract %d client has %s: %t, want %t, since the route arrived at contract %d", l.oldContract, route, has, planRoutes, api.OwnerPlanContract)
			}
		}
		l.note("contract %d client state(): %s", l.oldContract, state)
		stop := l.client(l.root, l.worker(claim.RoleArchitect), "stop_task", map[string]any{"issue": older, "task": "a"}, nil)
		if err := stopTaken(stop); err != nil {
			t.Fatal(err)
		}
		l.waitHeld(older, "task-stopped")
		late := l.client(l.oldTree, tester, "complete", map[string]any{"summary": "every criterion holds", "verdict": "pass"}, l.report(tester))
		if err := refused(late, http.StatusConflict, "TASK_STOPPED"); err != nil {
			t.Fatalf("the older tester's report of its stopped task: %v", err)
		}
		l.note("contract %d client handoffComplete after the owner's stop: %s", l.oldContract, late)
		head := l.client(l.root, tester, "state", map[string]any{"issue": older, "project": l.cfg.Project}, nil)
		l.note("this contract's client state() of the held issue: %s", head)
		heldState := l.client(l.oldTree, tester, "state", map[string]any{"issue": older, "project": l.cfg.Project}, nil)
		if err := heldOlder(heldState, l.oldContract); err != nil {
			t.Fatalf("the contract %d client's state read of %s, held task-stopped: %v", l.oldContract, older, err)
		}
		l.note("contract %d client state() of the held issue, shaped by the daemon to the contract the client states: %s", l.oldContract, heldState)
		// The pane's own boot token and session, as it registered before the restart (a new session
		// on that boot token is refused for that before its contract is read); the daemon that runs
		// now launched nothing for it (each boot gets a fresh fake runtime).
		registered := l.client(l.oldTree, tester, "register", map[string]any{"bootToken": tester.bootToken, "sessionId": tester.session,
			"ompSessionFile": "/sessions/" + tester.session + ".jsonl", "agentId": tester.session, "pluginContract": l.oldContract}, nil)
		if want := fmt.Sprintf("speaks daemon API contract %d; this daemon requires %d", l.oldContract, api.DaemonAPIVersion); registered.OK ||
			registered.Error != "LegionDaemonApiError" || registered.Status != http.StatusConflict || !strings.Contains(registered.Message, want) {
			t.Fatalf("the contract %d client's own registration %s, want LegionDaemonApiError 409 saying it %s", l.oldContract, registered, want)
		}
		l.note("contract %d client register() at its own contract: %s", l.oldContract, registered)
	})

	// stop_task with the architect's grant, its control (a task the plan does not hold), and the
	// stopped task's report refused TASK_STOPPED, whose control is the pass just taken.
	l.check("client-stop-task", func() {
		l.on(stopped)
		l.givePlan(clientPlan...)
		l.openGate(phase.Implementing)
		l.runStep("implementer", answerPass, phase.Testing)
		l.worker(claim.RoleTester).take()
		absent := l.client(l.root, l.worker(claim.RoleArchitect), "stop_task", map[string]any{"issue": stopped, "task": "zz"}, nil)
		if err := stopTaken(absent); err == nil {
			t.Fatalf("control: the check of stop_task passed on the task zz, %s", absent)
		} else {
			l.note("control: stop_task of task zz %s; the check fails: %v", absent, err)
		}
		outcome := l.client(l.root, l.worker(claim.RoleArchitect), "stop_task", map[string]any{"issue": stopped, "task": "a"}, nil)
		if err := stopTaken(outcome); err != nil {
			t.Fatal(err)
		}
		l.waitHeld(stopped, "task-stopped")
		l.note("taskStop of task a with the architect's grant: %s; %s stands held, holdReason task-stopped", outcome, stopped)
	})
	l.check("client-stopped-report-refused", func() {
		tester := l.worker(claim.RoleTester)
		outcome := l.client(l.root, tester, "handoff_complete", map[string]any{"summary": "every criterion holds", "verdict": "pass"}, l.report(tester))
		if err := refused(outcome, http.StatusConflict, "TASK_STOPPED"); err != nil {
			t.Fatalf("the stopped task's report: %v", err)
		}
		l.note("executePhaseOp handoff_complete of the stopped task's tester: %s", outcome)
		if err := refused(taken, http.StatusConflict, "TASK_STOPPED"); err == nil {
			t.Fatalf("control: the check of TASK_STOPPED passed on a report of a task nobody stopped, %s", taken)
		} else {
			l.note("control: the same check on %s's taken pass fails: %v", returnRefused, err)
		}
	})
}

// newOwnerStepsClientLive boots the rig over four issues, as newOwnerStepsLive does, with the
// project's default_steps named so the served settings carry the key.
func newOwnerStepsClientLive(t *testing.T) *ownerStepsClientLive {
	t.Helper()
	fixtureenv.Live(t)
	steps, err := planwords.ParseSteps(strings.Join(ownerStepsDefault, " "))
	if err != nil {
		t.Fatal(err)
	}
	proof := newLiveProof(t, "scripts/e2e/owner-steps-client.sh")
	// The plugin's client reads the state's settings with review_round_cap above 0, which the
	// loader's default (3, config's keytable) always gives a deployment; the package's test config
	// leaves the field 0, so the run names the default the loader would.
	proof.cfg.ReviewRoundCap = 3
	root := liveCLICheckout(t)
	rig := newOwnerStepsRig(t, proof, startHandoffDispatch(t, root).scratchDispatch, 4, config.Project{DefaultSteps: steps})
	return &ownerStepsClientLive{ownerStepsRig: rig, root: root}
}

// versionFile is the file whose DaemonAPIVersion line names a tree's daemon API contract.
const versionFile = "packages/daemon/internal/api/version.go"

// olderClient builds the plugin of the release before: the commit releaseBefore names, taken with
// `git archive` into a directory of the run's own and given node_modules of its own with
// `bun install --frozen-lockfile`, as busy-relaunch-delivery.sh builds its base tree.
func (l *ownerStepsClientLive) olderClient() {
	t := l.t
	t.Helper()
	commit, contract, ref := l.releaseBefore()
	archive := filepath.Join(t.TempDir(), "release-before.tar")
	l.run(l.root, "git", "archive", "-o", archive, commit)
	tree := t.TempDir()
	l.run(tree, "tar", "-xf", archive)
	l.run(tree, "bun", "install", "--frozen-lockfile")
	l.oldTree, l.oldContract = tree, contract
	l.note("the plugin of the release before: %s (%s), at contract %d, the newest commit below contract %d on the first-parent line of %s; its tree in %s with its own bun install",
		commit, l.run(l.root, "git", "log", "-1", "--format=%cs %s", commit), contract, api.DaemonAPIVersion, ref, tree)
}

// releaseBefore answers the newest commit of main's first-parent line whose DaemonAPIVersion is
// below this daemon's, its contract, and the ref the walk read main from. That is what is deployed
// before this checkout: in a pull request's run, the main it merges into; on main, the commit
// before the last change of the contract. main is origin/main in a clone that holds its history;
// a shallow clone (CI's checkout) fetches main from origin, deeper each time the walk reaches the
// end of what it fetched, and the whole of main's history once 4096 commits hold no such commit:
// how far back the last change of the contract is depends on how long main has kept it, which no
// depth written here bounds.
func (l *ownerStepsClientLive) releaseBefore() (string, int, string) {
	t := l.t
	t.Helper()
	if l.run(l.root, "git", "rev-parse", "--is-shallow-repository") != "true" {
		ref := "refs/remotes/origin/main"
		if commit, contract, ok := l.firstBelow(ref); ok {
			return commit, contract, ref
		}
		t.Fatalf("no commit of %s's first-parent line declares a daemon API contract below %d", ref, api.DaemonAPIVersion)
	}
	const deepest = 4096
	for depth := 64; depth <= deepest; depth *= 4 {
		l.run(l.root, "git", "fetch", "--quiet", "--no-tags", "--depth="+strconv.Itoa(depth), "origin", "main")
		if commit, contract, ok := l.firstBelow("FETCH_HEAD"); ok {
			return commit, contract, "main fetched from origin " + strconv.Itoa(depth) + " commits deep"
		}
	}
	if l.run(l.root, "git", "rev-parse", "--is-shallow-repository") == "true" {
		l.run(l.root, "git", "fetch", "--quiet", "--no-tags", "--unshallow", "origin", "main")
		if commit, contract, ok := l.firstBelow("FETCH_HEAD"); ok {
			return commit, contract, "main's whole history fetched from origin"
		}
	}
	t.Fatalf("no commit of main's first-parent line, fetched whole from origin, declares a daemon API contract below %d", api.DaemonAPIVersion)
	return "", 0, ""
}

// firstBelow answers the newest commit of ref's first-parent line whose DaemonAPIVersion is below
// this daemon's, and its contract.
func (l *ownerStepsClientLive) firstBelow(ref string) (string, int, bool) {
	for commit := range strings.FieldsSeq(l.run(l.root, "git", "rev-list", "--first-parent", ref)) {
		if contract := declaredContract(l.t, commit, l.run(l.root, "git", "show", commit+":"+versionFile)); contract < api.DaemonAPIVersion {
			return commit, contract, true
		}
	}
	return "", 0, false
}

// declaredContract is the DaemonAPIVersion that source, the version file of what, declares.
func declaredContract(t *testing.T, what, source string) int {
	t.Helper()
	match := regexp.MustCompile(`(?m)^const DaemonAPIVersion = (\d+)$`).FindStringSubmatch(source)
	if match == nil {
		t.Fatalf("%s's %s declares no DaemonAPIVersion", what, versionFile)
	}
	contract, err := strconv.Atoi(match[1])
	if err != nil {
		t.Fatal(err)
	}
	return contract
}

// run runs name with args in dir and answers its output with surrounding space trimmed; a failure
// fails the test with what the command wrote to stderr.
func (l *ownerStepsClientLive) run(dir, name string, args ...string) string {
	l.t.Helper()
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		l.t.Fatalf("%s %s in %s: %v; stderr %q", name, strings.Join(args, " "), dir, err, stderr.String())
	}
	return strings.TrimSpace(string(out))
}

// check is ownerStepsLive's check: `== <name>`, the body, and `ok <name>` or `FAIL <name>`.
func (l *ownerStepsClientLive) check(name string, body func()) {
	(&ownerStepsLive{ownerStepsRig: l.ownerStepsRig}).check(name, body)
}

// client runs owner-steps-client.ts once: call with args as c's session, the client imported from
// tree, report as the pane's handoff record answers it. It answers what the script printed.
func (l *ownerStepsClientLive) client(tree string, c *stepClaimant, call string, args, report map[string]any) clientOutcome {
	t := l.t
	t.Helper()
	request := map[string]any{
		"daemon": l.d.base,
		"claim": map[string]any{"sessionID": c.session, "tree": c.issue, "issue": c.issue, "role": string(c.role),
			"secret": c.secret, "claimToken": string(c.token), "generation": 1},
		"call": call,
		"args": args,
	}
	if report != nil {
		request["report"] = report
	}
	encoded, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("bun", filepath.Join(l.root, "scripts/e2e/owner-steps-client.ts"))
	cmd.Dir = l.root
	cmd.Env = append(os.Environ(), "OWNER_STEPS_CLIENT_REQUEST="+string(encoded), "OWNER_STEPS_CLIENT_ROOT="+tree)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("bun owner-steps-client.ts %s (client of %s): %v; stdout %q; stderr %q", call, tree, err, out, stderr.String())
	}
	var last string
	for scanner := bufio.NewScanner(bytes.NewReader(out)); scanner.Scan(); {
		if line := strings.TrimSpace(scanner.Text()); strings.HasPrefix(line, "{") {
			last = line
		}
	}
	var outcome clientOutcome
	if err := json.Unmarshal([]byte(last), &outcome); err != nil {
		t.Fatalf("decode what owner-steps-client.ts %s printed, %q: %v (stderr %q)", call, out, err, stderr.String())
	}
	return outcome
}

// report is what c's pane's handoff record answers for its completion: a record of its phase
// written to Dispatch under its own session now.
func (l *ownerStepsClientLive) report(c *stepClaimant) map[string]any {
	l.t.Helper()
	word := map[claim.Role]string{claim.RoleTester: "test", claim.RoleImplementer: "implement"}[c.role]
	id, version := l.writeHandoff(c.session, word)
	return map[string]any{"handoffId": id, "handoffVersion": version}
}

// waitHeld waits for key to stand held with holdReason reason in the state this contract serves.
func (l *ownerStepsClientLive) waitHeld(key, reason string) {
	l.t.Helper()
	l.waitPhaseOf(key, phase.Held)
	if got := l.d.state().Issues[key].HoldReason; got != reason {
		l.t.Fatalf("%s stands held for %q, want %q", key, got, reason)
	}
}

// planTaken says whether outcome is register_plan's 200 with version 1 and one task a naming steps.
func planTaken(outcome clientOutcome, steps []string) error {
	if !outcome.OK {
		return fmt.Errorf("register_plan %s, want 200 with the plan", outcome)
	}
	var answer api.PlanRegisterResponse
	if err := json.Unmarshal(outcome.Answer, &answer); err != nil {
		return fmt.Errorf("decode register_plan's answer %s: %v", outcome.Answer, err)
	}
	if answer.Plan.Version != 1 || len(answer.Plan.Tasks) != 1 || answer.Plan.Tasks[0].ID != "a" || !slices.Equal(answer.Plan.Tasks[0].Steps, steps) {
		return fmt.Errorf("register_plan answered %s, want version 1 with one task a naming %v", outcome.Answer, steps)
	}
	return nil
}

// stopTaken says whether outcome is stop_task's 200 `{}`.
func stopTaken(outcome clientOutcome) error {
	if !outcome.OK || strings.TrimSpace(string(outcome.Answer)) != "{}" {
		return fmt.Errorf("stop_task %s, want 200 {}", outcome)
	}
	return nil
}

// refused says whether outcome is the client's LegionDaemonApiError with status and code.
func refused(outcome clientOutcome, status int, code string) error {
	if outcome.OK || outcome.Error != "LegionDaemonApiError" || outcome.Status != status || outcome.Code != code {
		return fmt.Errorf("the client %s, want LegionDaemonApiError %d %s", outcome, status, code)
	}
	return nil
}
