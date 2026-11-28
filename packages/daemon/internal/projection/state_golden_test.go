package projection

//ci:reads packages/contracts/fixtures/daemon-api/* golden

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/capabilities"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

var updateGolden = flag.Bool("update", false, "rewrite the golden fixtures this package pins")

// The daemon's own controller (`controller: daemon`) holds a claim on no issue and no tree. It is
// the project's controller, which the state shows as controllerLocator, and no issue's worker: an
// issue keyed "" with a `controller` worker refuses the whole document at every client, which
// reads an issue's `key` as a non-empty string — among them the daemon-launched controller's own
// claim, which reads the state before it registers, and every agent's read_record. The document
// is what Project emits for that claim beside a tree's architect, completed as the state route
// completes it (internal/daemon's source.State), and it is packages/contracts'
// state-controller-claim fixture: the contract's own
// schema parses it (packages/contracts/src/legion-api.test.ts), and the plugin's daemon-launched
// controller reads it as its daemon's state (packages/pi-legion/extensions/legion.test.ts).
//
// It carries what Legion is waiting for too (INFRA-225), judged from the project's mirror: the
// child LEGION-209, held at `admitted` behind an issue still in progress, and the waiting root
// LEGION-210, behind one the daemon has never observed and so cannot call finished. LEGION-211
// waits in the same line on an issue of its own, and is in neither `blocked` nor any judgement:
// a root carrying a run is work already under way.
//
// It carries the two holds before a retro that FixBesideReviewContract added, each on a root of
// its own tree whose design gate stands open: LEGION-212, held while a fix of its task reads its
// failed job (`holdReason: fix`), whose held row reads `fix_running` with the fix's check and head
// as `heldBy`, and LEGION-213, held until each check a CI job runs passes at the head of its open
// pull request (`holdReason: ci`), whose held row reads `ci_at_head` with each check that has not
// passed there, a check whose job runs and one whose job failed, and not the one that passed.
// Both stand from the retro, which keeps no slot, and no lifecycle of theirs is open, so `active`
// and `trees` stay as above. The contract's own schema parses both reasons and both words, and
// the frozen schema of the contract before it refuses the reasons
// (packages/contracts/src/legion-api.test.ts).
//
// It carries the trees too (dispatch://INFRA-980): LEGION-208's lifecycle stands open under the
// workflow, and the two roots waiting in the line are live trees of their own, so `trees` names
// three roots while one issue alone holds a slot — which is the reading `active` can no longer
// give. Each issue's `tree` names its root, so the child LEGION-209 is told from the three roots.
func TestTheStateWithTheControllersClaimGolden(t *testing.T) {
	const issue = "LEGION-208"
	architect, err := claim.NewToken("legion", issue, claim.RoleArchitect)
	if err != nil {
		t.Fatal(err)
	}
	controller := claim.ControllerToken("legion")
	admitted := time.Date(2026, 10, 6, 18, 2, 11, 0, time.UTC)
	store := projectionStore{
		issues: []record.Issue{
			{Key: issue, Project: "LEGION", Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Tree: issue, Status: "in_progress", Phase: phase.Admitted, Generation: 1, HandedOver: true},
			{Key: "LEGION-209", Project: "LEGION", Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Tree: issue, Status: "todo", Phase: phase.Admitted, Generation: 1},
			{Key: "LEGION-210", Project: "LEGION", Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Tree: "LEGION-210", Status: "todo", Phase: phase.Admitted, Generation: 1, Rank: "00010", HandedOver: true},
			{Key: "LEGION-211", Project: "LEGION", Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Tree: "LEGION-211", Status: "todo", Phase: phase.Implementing, Generation: 2, Rank: "00011", HandedOver: true, HandedOverFrom: "LEGION-200"},
			{Key: "LEGION-212", Project: "LEGION", Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Tree: "LEGION-212", Status: "in_progress", Phase: phase.Held, Generation: 1, Rank: "00012", HandedOver: true,
				Hold: &record.Hold{From: phase.Retro, Reason: record.HoldForFix}},
			{Key: "LEGION-213", Project: "LEGION", Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Tree: "LEGION-213", Status: "in_progress", Phase: phase.Held, Generation: 1, Rank: "00013", HandedOver: true,
				Hold: &record.Hold{From: phase.Retro, Reason: record.HoldForCI}},
		},
		phases: map[string][]record.PhaseRow{issue: {{Issue: issue, Role: claim.RoleArchitect, Claim: architect}}},
		slots:  []record.Slot{{Issue: issue, Index: 0, AdmittedAt: admitted}},
		// The tree the architect runs stands open under the workflow, which is what makes it a
		// live tree now that its slot no longer says so.
		lifecycles: []string{issue},
		// The two held roots' design gates stand open, so the held list reads neither of them
		// under design_gate: each takes the row the ladder gives its hold.
		gates: map[string]map[record.DesignStep]record.DesignGate{
			"LEGION-212": openGate("LEGION-212", admitted.Add(-2*time.Hour)),
			"LEGION-213": openGate("LEGION-213", admitted.Add(-2*time.Hour)),
		},
		mirror: []record.DispatchIssueRow{
			{Key: issue, Project: "LEGION", Status: "in_progress", Blockers: []string{}, Seq: 4},
			{Key: "LEGION-209", Project: "LEGION", Status: "todo", Blockers: []string{"LEGION-300"}, Seq: 5},
			{Key: "LEGION-210", Project: "LEGION", Status: "todo", Blockers: []string{"LEGION-301"}, Seq: 6},
			{Key: "LEGION-211", Project: "LEGION", Status: "todo", Blockers: []string{"LEGION-302"}, Seq: 7},
			{Key: "LEGION-300", Project: "LEGION", Status: "in_progress", Blockers: []string{}, Seq: 8},
			{Key: "LEGION-302", Project: "LEGION", Status: "todo", Blockers: []string{}, Seq: 9},
		},
		// LEGION-212's task and the fix of it that reads its failed job, and LEGION-213's open pull
		// request with what the daemon holds of its checks at its head.
		planTasks: []record.PlanTask{{Issue: "LEGION-212", Generation: 1, Task: "a"}},
		fixes: map[string]record.TaskFix{"LEGION-212": {Issue: "LEGION-212", Generation: 1, Task: "a", Fix: 1,
			State: record.FixReading, Check: "ci / test (ubuntu-24.04)", Head: fixHead, Place: 1}},
		pullRequests: map[string]*record.PullRequest{"LEGION-213": {Issue: "LEGION-213", Repo: "acme/widgets", Number: 213,
			Branch: "legion/LEGION-213", HeadSHA: fixHead, Generation: 1, State: record.PullRequestOpen}},
		headStates: map[string]map[string]record.HeadStates{"LEGION-213": {fixHead: {
			Checks: []string{"ci / build", "ci / lint", "ci / test (ubuntu-24.04)"},
			States: map[string]record.CheckState{
				"ci / build":               {Check: "ci / build", State: "no", Reason: "no_result", CICase: record.CINotCompleted},
				"ci / lint":                {Check: "ci / lint", State: "yes", CICase: "success"},
				"ci / test (ubuntu-24.04)": {Check: "ci / test (ubuntu-24.04)", State: "no", Reason: "fail", CICase: "failure"},
			},
		}}},
	}
	architectLocator := sandboxLocator(architect, "legion-legion-legion-208", "4b7e1c2a-0d93-4f6e-8a51-3c2f9e7d1b04", 1)
	// The architect's session sent its live rows on a `claims/ready` of its own, measured at an
	// idle tick of its own interval rather than at its boot (LEGION-663, INFRA-927), from the
	// process the locator names, so the fixture carries a claim's capability report; the
	// controller measures nothing and sends none, and its claim is on no issue besides.
	reports := map[claim.Token]capabilities.Report{
		architect: {
			Claim: architect, Generation: 1, Locator: *architectLocator,
			MeasuredAt: admitted.Add(45 * time.Second), ReportedAt: admitted.Add(46 * time.Second), ElapsedMs: 1200,
			Rows: []capabilities.Row{
				{Name: capabilities.Subagents, OK: true, Detail: "6 task agents discovered: deep-worker, oracle, plan-gap-analyst, plan-reviewer, thermonuclear-code-quality, thermonuclear-deep-review"},
				{Name: capabilities.WebSearch, OK: true, Detail: "web_search is active and a query answered"},
				{Name: capabilities.MCP, OK: true, Detail: "no MCP server is configured"},
				{Name: capabilities.RepositoryExtensions, OK: true, Detail: "2 extension paths discovered"},
				{Name: capabilities.DispatchEnvoyTools, OK: true, Detail: "dispatch read LEGION-208 answered; envoy_whoami is active"},
				{Name: capabilities.GitHub, Detail: "gh api user: HTTP 401: Bad credentials"},
			},
		},
	}
	// What the daemon last saw each agent do (supervise.StepRecord): the architect is in a
	// `dispatch` call whose start the daemon received and whose end it has not, and the
	// controller in a `bash` call of its own. The controller's reaches no claim view — its claim
	// is on no issue — which is why `legion claims list` carries the record too.
	architectStep := supervise.StepRecord{
		At:            admitted.Add(50 * time.Second),
		TurnStartedAt: admitted.Add(40 * time.Second),
		Tool:          "dispatch",
		CallID:        "toolu_arch_7",
		ArgsBytes:     96,
		ArgsHash:      "5a1c7e03b94d26f8",
		StartedAt:     admitted.Add(50 * time.Second),
	}
	state, err := Project(context.Background(), Inputs{Store: store, Project: "LEGION", Claims: []supervise.Claim{
		{
			Token: architect, Project: "legion", Tree: issue, Issue: issue, Role: claim.RoleArchitect,
			State: supervise.StateReady, Session: "ses_architect_208", Locator: architectLocator,
			Step: architectStep,
		},
		{
			Token: controller, Project: "legion", Role: claim.RoleController,
			State: supervise.StateReady, Session: "ses_controller", Locator: sandboxLocator(controller, "legion-legion-controller", "9d2a6f13-7c48-4e0b-b5a9-1f8e3d6c2a57", 1),
			Step: supervise.StepRecord{
				At: admitted.Add(55 * time.Second), TurnStartedAt: admitted.Add(52 * time.Second),
				Tool: "bash", CallID: "toolu_ctl_2", ArgsBytes: 40, ArgsHash: "8d30be61f7c2904a",
				StartedAt: admitted.Add(55 * time.Second),
			},
		},
	}, Reports: reports, Now: admitted.Add(time.Minute)})
	if err != nil {
		t.Fatalf("Project: %v", err)
	}
	for key, view := range state.Issues {
		if worker, listed := view.Workers[claim.RoleController]; listed {
			t.Errorf("issue %q lists a controller worker %+v: the controller's claim is on no issue", key, worker)
		}
	}
	if view := state.Issues[issue]; view.Architect == nil || view.Architect.Session != "ses_architect_208" {
		t.Errorf("%s's architect = %+v, want its ready claim", issue, view.Architect)
	} else if view.Architect.Capabilities == nil || view.Architect.Capabilities.Incarnation != architectLocator.Incarnation {
		t.Errorf("%s's architect capabilities = %+v, want its session's report from the process the locator names", issue, view.Architect.Capabilities)
	}
	if view := state.Issues[issue]; !reflect.DeepEqual(view.Architect.Step, api.StepViewOf(architectStep)) {
		t.Errorf("%s's architect step = %+v, want the call the daemon last saw it make, %+v",
			issue, view.Architect.Step, api.StepViewOf(architectStep))
	}
	// The controller's own record reaches no claim view, because its claim is on no issue: an
	// operator reads it on the operator claim routes (api.OperatorClaim.Step) and nowhere here.
	if encoded, err := json.Marshal(state); err != nil {
		t.Fatalf("marshal the state: %v", err)
	} else if bytes.Contains(encoded, []byte("toolu_ctl_2")) {
		t.Errorf("the state carries the controller's call: %s", encoded)
	}
	want := []api.BlockedIssue{
		{Issue: "LEGION-209", BlockedBy: []string{"LEGION-300"}},
		{Issue: "LEGION-210", BlockedBy: []string{"LEGION-301"}},
	}
	if !reflect.DeepEqual(state.Admission.Blocked, want) {
		t.Errorf("the state's blocked = %#v, want %#v: LEGION-211 carries a run and is never judged", state.Admission.Blocked, want)
	}
	// The two holds before a retro read their own words, each with what holds it.
	for _, want := range []api.HeldIssue{
		{Issue: "LEGION-212", Reason: record.HeldFixRunning, HeldBy: []string{"ci / test (ubuntu-24.04)", fixHead}},
		{Issue: "LEGION-213", Reason: record.HeldCIAtHead, HeldBy: []string{"ci / build", "ci / test (ubuntu-24.04)"}},
	} {
		if got := heldRow(t, state, want.Issue); !reflect.DeepEqual(got, want) {
			t.Errorf("the held row of %s = %#v, want %#v", want.Issue, got, want)
		}
	}

	state.Daemon = api.DaemonInfo{Project: "LEGION", SchemaVersion: 35, Boots: 2, FirstBootAt: admitted.Add(-time.Hour), StartedAt: admitted.Add(-time.Minute)}
	state.Admission.Cap = 2
	// The resolved file of a tmux daemon at every default, as the loader resolves a minimal
	// legion.yaml (the DSN is required and never emitted); the api package's goldens pin the
	// maximal kubernetes shape.
	minimal := filepath.Join(t.TempDir(), "legion.yaml")
	if err := os.WriteFile(minimal, []byte("project: LEGION\nstate_dir: /var/lib/legion\nadmission_cap: 2\npostgres_dsn: postgres://legion@127.0.0.1:5432/legion\n"), 0o600); err != nil {
		t.Fatalf("write legion.yaml: %v", err)
	}
	cfg, err := config.Load(minimal, func(string) string { return "" })
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	state.Settings = api.SettingsOf(cfg)
	state.ControllerLocator = &api.ControllerLocator{Runtime: "kubernetes", External: true, SessionID: "ses_controller", RegisteredAt: admitted.Add(-30 * time.Second)}
	golden(t, "state-controller-claim.json", state)
}

// sandboxLocator is one role process in a Sandbox: the Sandbox's name, the pod's uid, the pod's one
// container (`roles` in an issue pod, `controller` in the controller's) and the process generation,
// its incarnation `<pod uid>/<generation>`.
func sandboxLocator(token claim.Token, sandbox, podUID string, generation uint64) *runtime.Locator {
	container := runtime.IssuePodContainer
	if role, _ := token.Role(); role == claim.RoleController {
		container = runtime.ControllerPodContainer
	}
	return &runtime.Locator{
		Runtime: runtime.RuntimeSandbox, Claim: token, Incarnation: runtime.SandboxIncarnation(podUID, generation),
		Sandbox: &runtime.SandboxLocator{Namespace: "legion", Name: sandbox, PodUID: podUID, Container: container, Generation: generation},
	}
}

// golden pins the state document Project's caller serves: Go writes it (`-update`), and
// `packages/contracts/src/legion-api.test.ts` parses it through the schema the plugin reads the
// state with and holds the parse equal to the file. A fixture that differs from what Go now
// writes is stale. It is internal/api's golden helper with this package's command in its message:
// the daemon-api fixtures are written from two packages, so a state-shape change takes both
// `-update` runs whether or not the helper is shared. Once a third package writes one, the
// helper moves into an internal/testgolden package.
func golden(t *testing.T, name string, value any) {
	t.Helper()
	encoded, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		t.Fatalf("marshal %s: %v", name, err)
	}
	encoded = append(encoded, '\n')
	path := filepath.Join("..", "..", "..", "contracts", "fixtures", "daemon-api", name)
	if *updateGolden {
		if err := os.WriteFile(path, encoded, 0o644); err != nil {
			t.Fatalf("write golden: %v", err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read golden (run `go test ./internal/projection/ -update`): %v", err)
	}
	if !bytes.Equal(encoded, want) {
		t.Fatalf("%s is stale\n got: %s\nwant: %s", name, encoded, want)
	}
}
