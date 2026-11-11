//go:build e2e

// The live proof that a role process is launched with the pod memory rule in the system prompt it
// carries: the paragraph every phase worker is given
// (packages/daemon/internal/prompts/roles/mechanics/headless.md, "What you run in the pod") and
// the paragraph every architect is given
// (packages/daemon/internal/prompts/go/architect-common.md). scripts/e2e/role-prompt-rule.sh
// builds this driver with `-tags e2e` and owns the nats-server and the PostgreSQL it runs against.
//
// What this proof reads, and why it reads it there. Two unit tests already hold that the rule is
// in each composed prompt and in one part per tree
// (packages/daemon/internal/prompts/prompts_test.go and
// packages/daemon/internal/prompts/roles_test.go). Neither of them starts anything, so neither
// says that a role process was ever launched with that text. This driver reads the text from the
// one place that says it: the launched process's own command line, /proc/<pid>/cmdline, whose pid
// is the one the claim's locator carries (parseLocalIncarnation, over the incarnation
// localruntime_live_test.go's launch mints). The argv is NUL-separated, and the word after
// `--append-system-prompt` is the whole prompt that process runs under.
//
// It reads the prompt from nowhere else on purpose. The composer, the role prompt files below the
// state directory and the SpawnSpec the runtime recorded each hold the text before a process ever
// took it, so a proof that read any of the three would prove again what the unit tests prove.
// Only the kernel's copy of the argv says what the agent was started with.
//
// The daemon is the whole daemon: run, the function `legion start` calls, through this package's
// own whole-daemon harness (startDaemonWithin) over the live-proof fixture (live_proof_test.go).
// Each pane is a real `omp --mode rpc` under the real `legion worker-shim`, started by
// localruntime_live_test.go, since a Legion pod carries no tmux, with this checkout's
// `@sjawhar/pi-envoy` and `@sjawhar/pi-legion` in an Oh My Pi profile of the run's own. What
// stands in is the model endpoint and nothing else: each pane talks to one HTTP stand-in of the
// driver's own (newPaneStandin, claim_compaction_live_test.go), which answers every turn with one
// short message.
//
// The seed is one tree, so that the daemon starts two real panes of two different kinds: the
// tree's root architect, and the planner of the same issue, which stands in planning. The
// architect's claim carries no task, as an architect's start leaves none; the planner's carries
// the task its phase's start left pending, because a start writes the relaunch and the task
// together (workflow's StartOrOwe) and a slotted worker holding no task at all is the state
// admission repairs (internal/admit/admit.go's resumeSlottedWorkers), which would deliver a task
// of its own instead.
//
// This proof writes no role prompt file of its own, which is the one thing its seed does
// differently from its two siblings' (seed). An operator's prompt file below the state directory
// is a launch's whole role prompt (specs.rolePromptPaths), so a seed that wrote one would launch
// both panes on that literal and the composed prompts would reach no process at all.
package daemon

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/appauth"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/fixtureenv"
	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/stream"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/treelifecycle"
)

// promptWord is the Oh My Pi flag a role's whole system prompt rides in, in the argv a pane is
// started with (omplaunch.SystemPromptArgument, and the argv localruntime_live_test.go's launch
// builds). Oh My Pi applies the last occurrence alone, so a launch carries exactly one.
const promptWord = "--append-system-prompt"

// argvWordShown is how much of one argv word a failure prints before it cuts it. A role prompt is
// tens of kilobytes, and a failure that printed it whole would bury the words around it.
const argvWordShown = 80

// The two panes, by the provider name each one's stand-in is given in the run's models.yml.
const (
	ruleArchitectProvider = "architectpane"
	rulePlannerProvider   = "plannerpane"
)

// podRuleForPhaseWorker is the pod memory rule as a phase worker's prompt carries it
// (packages/daemon/internal/prompts/roles/mechanics/headless.md), sentence by sentence. Each is a
// literal of this file, so a reworded rule fails this proof rather than passing on a text the
// driver derived from the same files it is reading.
var podRuleForPhaseWorker = []string{
	"In an issue pod every role of this issue runs in one container and shares its memory limit",
	"where that pod's sandbox ends the whole container rather than the one process that filled it",
	"a command that fills the limit ends every role's turn and not only yours",
	"A whole module's test run (`go test ./...`), a whole Postgres-backed package, a browser suite run whole, and `make -j` or `lake build` are CI's work on the pull request",
	"what a role runs in the pod is what `skill://legion-worker/references/tools.md` names",
	"Every other heavy run goes where your deployment's instructions say",
	"instructions that name no such route are a gap you report to the architect that owns your issue",
	"Each subagent you start runs its commands in this pod, so put these facts in its context",
}

// podRuleForRootArchitect is the same rule as an architect's prompt carries it
// (packages/daemon/internal/prompts/go/architect-common.md), which is written for the subagents an
// architect starts rather than for the architect's own commands.
var podRuleForRootArchitect = []string{
	"Your subagents run their commands in your own issue pod",
	"where that pod's sandbox ends the whole container rather than the one process that filled it",
	"a command that fills the limit ends every role's turn and not only yours",
	"A whole module's test run (`go test ./...`), a whole Postgres-backed package, a browser suite run whole, and `make -j` or `lake build` are CI's work on the pull request",
	"what may run in the pod is what `skill://legion-worker/references/tools.md` names",
	"every other heavy run goes where your deployment's instructions say and instructions that name no such route are a gap to report, and you put these facts in the context of each subagent you start",
}

// rolePromptRuleLive is one run of the proof: the booted daemon, the one tree it seeds, and the
// two claims whose own command lines it reads.
type rolePromptRuleLive struct {
	liveAgentProof
	built *built
	// issue is the tree's root and this proof's only issue: its own root architect works it, and
	// it stands in planning, so its planner works it too.
	issue string
	// panes is each live claim's own pane: the role it holds and the stand-in it talks to.
	panes map[claim.Token]*rulePane
	// home and profile are the Oh My Pi home and profile both panes run under.
	home, profile string
	// overlays is each claim's PI_CONFIG_FILES, in order.
	overlays map[claim.Token][]string
	// launched is what step 1 read out of each claim's own process: its pid and the one
	// `--append-system-prompt` word it was started with.
	launched map[claim.Token]launchedPrompt
}

// rulePane is one live claim of the proof: the role it holds and the model stand-in its Oh My Pi
// talks to.
type rulePane struct {
	role  claim.Role
	stand *paneStandin
}

// ruleRole is one of the proof's two claims as the seed and the profile name it: the role, and the
// provider name of the stand-in that role's pane talks to.
type ruleRole struct {
	role     claim.Role
	provider string
}

// launchedPrompt is one process's own system prompt, and the pid it was read from.
type launchedPrompt struct {
	pid    int
	prompt string
}

// TestEveryRoleIsLaunchedWithThePodMemoryRuleLive boots the daemon over two real panes, reads the
// command line each of those two processes was launched with, and holds the pod memory rule in
// each: the phase worker's eight sentences and the root architect's six.
func TestEveryRoleIsLaunchedWithThePodMemoryRuleLive(t *testing.T) {
	fixtureenv.Live(t)
	r := newRolePromptRuleLive(t)
	r.step("step1-both-roles-of-the-tree-run-a-pane-whose-own-argv-carries-a-system-prompt", r.bothPanesAreReady)
	r.step("step2-the-phase-worker-was-launched-with-the-pod-memory-rule", r.phaseWorkerCarriesTheRule)
	r.step("step3-the-root-architect-was-launched-with-the-pod-memory-rule", r.rootArchitectCarriesTheRule)
}

// newRolePromptRuleLive readies everything a pane needs before the daemon boots: the `legion`
// binary and the two plugins built from this checkout, an Oh My Pi profile of the run's own routed
// to the two stand-ins, the one tree seeded with both its claims mid-launch, and the daemon booted
// over the local runtime.
func newRolePromptRuleLive(t *testing.T) *rolePromptRuleLive {
	t.Helper()
	omp := os.Getenv(liveOmpPathEnv)
	if omp == "" {
		t.Skipf("%s is unset, so there is no Oh My Pi for a pane to run; scripts/e2e/role-prompt-rule.sh requires it", liveOmpPathEnv)
	}
	takeOwnDatabase(t)
	proof := newLiveProof(t, "scripts/e2e/role-prompt-rule.sh")
	r := &rolePromptRuleLive{
		liveAgentProof: liveAgentProof{liveProof: proof, omp: omp},
		built:          &built{},
		issue:          proof.cfg.Project + "-1",
		panes:          map[claim.Token]*rulePane{},
		launched:       map[claim.Token]launchedPrompt{},
	}
	version, err := exec.Command(omp, "--version").Output()
	if err != nil {
		t.Fatalf("read the Oh My Pi version of %s: %v", omp, err)
	}
	r.note("Oh My Pi %s at %s (the pod's build, which is the fleet's pin and not this branch's fetch)",
		strings.TrimSpace(string(version)), omp)

	root := proofCheckoutRoot(t)
	work := t.TempDir()
	r.legion = buildProofLegion(t, root, work)
	r.home = filepath.Join(work, "omp-home")
	r.profile = "legion-role-prompt-rule-" + strings.ToLower(r.cfg.Project)
	installProofPlugins(t, root, work, r.home, r.profile, omp)
	for _, seeded := range r.seededRoles() {
		r.panes[r.token(seeded.role)] = &rulePane{role: seeded.role, stand: newPaneStandin(t, seeded.provider)}
	}
	r.writeOmpProfile()
	r.overlays = r.writeOverlays(work)

	r.rt = newLocalRuntime(t, r.cfg.StateDir, r.legion, omp, r.home, r.profile)
	r.rt.daemonURL, r.rt.envoyURL, r.rt.natsURLs = r.cfg.DaemonURL, newPaneListener(t), r.cfg.NatsURLs
	r.rt.overlays = func(token claim.Token) []string { return r.overlays[token] }

	r.seed()
	r.daemon = newRecordingLog(r.log())
	r.d = startDaemonWithin(t, r.cfg, r.overrides(), slog.New(r.daemon), 60*time.Second)
	return r
}

// seededRoles is the two claims of the one tree that run a real pane. The issue is its own tree's
// root, so its architect claim is the root architect's, which is the claim that carries
// architect-common.md; the issue stands in planning, so the phase worker of that phase is the
// planner, which is the claim that carries mechanics/headless.md.
func (r *rolePromptRuleLive) seededRoles() []ruleRole {
	return []ruleRole{
		{claim.RoleArchitect, ruleArchitectProvider},
		{claim.RolePlanner, rulePlannerProvider},
	}
}

// token is the claim token of role on this proof's one issue, as the daemon spells it.
func (r *rolePromptRuleLive) token(role claim.Role) claim.Token {
	r.t.Helper()
	token, err := claim.NewToken(r.project, r.issue, role)
	if err != nil {
		r.t.Fatal(err)
	}
	return token
}

// overrides is the daemon this proof boots: the local runtime over the real worker stream, the API
// listener the fixture holds, and the workflow's App tokens stood in for. The clock is the real
// one, since nothing here is measured against a wait.
func (r *rolePromptRuleLive) overrides() overrides {
	return overrides{
		runtime: func(_ context.Context, listener *stream.Listener, address string, _ appauth.Tokens,
			_ func(context.Context, string) (ghrepo.Repository, string, error), _ *store.Store, _ func() []ghrepo.Repository) (runtime.Runtime, error) {
			r.built.mu.Lock()
			defer r.built.mu.Unlock()
			r.built.conns, r.built.address = listener, address
			r.rt.conns, r.rt.stream = listener, address
			return r.rt, nil
		},
		listen:         heldListen,
		workflowTokens: &workflowTokenRecorder{},
	}
}

// seed records the one issue as the daemon would hold it at the moment its roles began: a root
// handed to Legion, in planning, holding its tree's admission slot with its lifecycle open, and
// both of its claims mid-launch, which this boot relaunches. The planner's claim also holds the
// task its phase's start left pending, which is the state admission's repair of a slotted worker
// passes over (workflow.StartFor, the delivery's StillWorked); the architect's holds none, since
// the repair reads no architect's claim at all.
//
// No role prompt file is written below the state directory. Such a file is an operator's own
// prompt, and a launch that finds one gives the pane that file and nothing else
// (specs.rolePromptPaths), so writing one — as both sibling proofs do, neither of which reads a
// prompt — would leave the composed role prompts out of every process this proof reads.
func (r *rolePromptRuleLive) seed() {
	t := r.t
	t.Helper()
	ctx := context.Background()
	r.board.show(r.issue, "in_progress")
	if err := r.st.Tx(ctx, func(tx pgx.Tx) error {
		if err := r.records.PutIssue(ctx, tx, record.Issue{
			Key: r.issue, Repo: liveRepo, DefaultBranch: "main", GitHubRepo: liveRepo, Tree: r.issue,
			Project: r.cfg.Project, Title: "the issue whose architect and planner this proof reads the command line of",
			Phase: phase.Planning, Generation: 1, Status: "in_progress",
			DispatchStatus: "in_progress", Rank: "U", LastDispatchSeq: 1, HandedOver: true,
		}); err != nil {
			return err
		}
		return r.admit(ctx, tx, r.issue)
	}); err != nil {
		t.Fatalf("seed %s: %v", r.issue, err)
	}
	if _, err := r.st.OpenTreeLifecycle(ctx, r.project, r.issue, treelifecycle.AuthorityWorkflow); err != nil {
		t.Fatalf("open the lifecycle of %s: %v", r.issue, err)
	}
	for _, seeded := range r.seededRoles() {
		token := r.token(seeded.role)
		if _, err := r.st.AdmitClaim(ctx, supervise.Claim{
			Token: token, Project: r.project, Tree: r.issue, Issue: r.issue, Role: seeded.role,
			Generation: 1, State: supervise.StateLaunching,
			BootTokenHash: supervise.HashBootToken("interrupted-" + randomSuffix(t)), ServingGeneration: 1,
		}); err != nil {
			t.Fatalf("record the unfinished launch of %s: %v", token, err)
		}
		if seeded.role == claim.RoleArchitect {
			continue
		}
		// Unconfirmed and unread: the launch this seed interrupts never put the task in front of
		// an agent, so the relaunched pane is sent it once it reports ready.
		if err := r.st.PutDelivery(ctx, token, supervise.Delivery{
			ID: "seeded-start-" + randomSuffix(t), Task: startTask(seeded.role), Generation: 1,
			Phase: phase.Planning, QueuedAt: time.Now(),
		}); err != nil {
			t.Fatalf("record the task %s's start left pending: %v", token, err)
		}
	}
	r.note("seeded %s in %s, with its %s and its %s mid-launch and no role prompt file of this proof's own, "+
		"so each pane is launched on the role prompts the daemon composed",
		r.issue, phase.Planning, claim.RoleArchitect, claim.RolePlanner)
}

// ---- the steps -----------------------------------------------------------------------------------

// bothPanesAreReady waits for each claim's real pane to start Oh My Pi, register and report ready,
// and then reads that process's own command line, which is what steps 2 and 3 hold the rule in.
func (r *rolePromptRuleLive) bothPanesAreReady() {
	for _, seeded := range r.seededRoles() {
		token := r.token(seeded.role)
		r.awaitReady(token)
		read := r.launchedPromptOf(token)
		r.launched[token] = read
		r.note("%s runs as pid %d, whose own /proc/%d/cmdline carries one %s word of %d bytes",
			token, read.pid, read.pid, promptWord, len(read.prompt))
	}
}

// phaseWorkerCarriesTheRule holds the eight sentences of the pod memory rule in the planner's own
// command line.
func (r *rolePromptRuleLive) phaseWorkerCarriesTheRule() {
	r.carriesTheRule("phase worker", r.token(claim.RolePlanner), podRuleForPhaseWorker)
}

// rootArchitectCarriesTheRule holds the six sentences of the architect's own wording of the rule
// in the root architect's own command line.
func (r *rolePromptRuleLive) rootArchitectCarriesTheRule() {
	r.carriesTheRule("root architect", r.token(claim.RoleArchitect), podRuleForRootArchitect)
}

// carriesTheRule holds every sentence of the role's own wording of the pod memory rule in the
// prompt that role's process was launched with, and prints where each one was found. A sentence it
// cannot find ends the proof naming the role, its pid and the sentence, since that line is the
// whole evidence of a negative control's run.
func (r *rolePromptRuleLive) carriesTheRule(what string, token claim.Token, sentences []string) {
	t := r.t
	t.Helper()
	read, ok := r.launched[token]
	if !ok {
		t.Fatalf("step 1 read no command line for the %s (%s)", what, token)
	}
	for i, sentence := range sentences {
		at := strings.Index(read.prompt, sentence)
		if at < 0 {
			t.Fatalf("the %s %s (pid %d) was launched with a prompt of %d bytes that does not carry sentence %d of %d of the pod memory rule: %q",
				what, token, read.pid, len(read.prompt), i+1, len(sentences), sentence)
		}
		r.note("sentence %d of %d is at byte %d of the %s %s's own argv: %q",
			i+1, len(sentences), at, what, token, sentence)
	}
	r.note("all %d sentences of the pod memory rule are in the command line pid %d was launched with",
		len(sentences), read.pid)
}

// ---- what a step reads --------------------------------------------------------------------------

// launchedPromptOf is the `--append-system-prompt` word the claim's own process was launched with.
// It is read from that process's /proc/<pid>/cmdline, the kernel's own copy of its argv, one word
// per NUL-separated field; the pid is the claim's own, off the incarnation its locator carries
// (parseLocalIncarnation). A command line it cannot read, and one with no such word, each end the
// proof naming what it found.
func (r *rolePromptRuleLive) launchedPromptOf(token claim.Token) launchedPrompt {
	t := r.t
	t.Helper()
	held := r.claim(token)
	if held.Locator == nil {
		t.Fatalf("%s is %s and the daemon's claim listing gives it no locator, so it names no process to read", token, held.State)
	}
	pid, _, err := parseLocalIncarnation(held.Locator.Incarnation)
	if err != nil {
		t.Fatalf("the locator of %s names no pid: %v", token, err)
	}
	path := "/proc/" + strconv.Itoa(pid) + "/cmdline"
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s, the command line %s was launched with: %v; that pane's own output is under %s",
			path, token, err, r.rt.paneLogs)
	}
	argv := strings.Split(strings.TrimSuffix(string(raw), "\x00"), "\x00")
	for i, word := range argv {
		if word != promptWord {
			continue
		}
		if i+1 == len(argv) {
			t.Fatalf("%s (pid %d) was launched with %s as the last word of its argv, so it carries no prompt: %s",
				token, pid, promptWord, argvSummary(argv))
		}
		return launchedPrompt{pid: pid, prompt: argv[i+1]}
	}
	t.Fatalf("%s (pid %d) was launched with no %s word at all; its argv is %s", token, pid, promptWord, argvSummary(argv))
	return launchedPrompt{}
}

// argvSummary is an argv as a failure names it: every word, each quoted, with a word longer than
// argvWordShown cut to its first characters and its length given.
func argvSummary(argv []string) string {
	words := make([]string, 0, len(argv))
	for _, word := range argv {
		shown := word
		if len(shown) > argvWordShown {
			shown = fmt.Sprintf("%s… (%d bytes)", shown[:argvWordShown], len(word))
		}
		words = append(words, strconv.Quote(shown))
	}
	return "[" + strings.Join(words, " ") + "]"
}

// ---- the Oh My Pi profile and the overlays -----------------------------------------------------------

// writeOmpProfile writes the profile both panes share: the two stand-in providers, one model each,
// and the settings a pane needs to run unattended. The context window is declared because a
// composed role prompt is tens of kilobytes, which is a session Oh My Pi would otherwise compact
// before its first turn.
func (r *rolePromptRuleLive) writeOmpProfile() {
	t := r.t
	t.Helper()
	agent := filepath.Join(r.home, ".omp", "profiles", r.profile, "agent")
	if err := os.MkdirAll(agent, 0o700); err != nil {
		t.Fatalf("make the profile's agent directory %s: %v", agent, err)
	}
	var models, enabled strings.Builder
	models.WriteString("# Written by packages/daemon/internal/daemon/role_prompt_rule_live_test.go.\nproviders:\n")
	for _, seeded := range r.seededRoles() {
		stand := r.panes[r.token(seeded.role)].stand
		fmt.Fprintf(&models, "  %s:\n    baseUrl: %s\n    api: anthropic-messages\n    apiKey: a-literal-key-no-endpoint-checks\n"+
			"    models:\n      - id: %s\n        name: %s\n        contextWindow: 200000\n        maxTokens: 8192\n",
			seeded.provider, stand.url, seeded.provider+standinModelID, seeded.provider)
		fmt.Fprintf(&enabled, "  - %s\n", standinModel(seeded.provider))
	}
	writeProofFile(t, filepath.Join(agent, "models.yml"), models.String())
	writeProofFile(t, filepath.Join(agent, "config.yml"),
		"# Written by packages/daemon/internal/daemon/role_prompt_rule_live_test.go.\n"+
			"enabledModels:\n"+enabled.String()+
			// A fresh profile opens Oh My Pi's setup wizard, and a pane waiting on it never starts.
			"startup:\n  setupWizard: false\n")
}

// writeOverlays writes the Oh My Pi overlays each pane is told through PI_CONFIG_FILES: the
// operator's own, which every pane names first and which turns `retry.enabled` off so the host
// walks no fallback chain of its own, and one per claim naming the single model that claim's
// stand-in serves (writeRoleOverlay), so each claim's chain has no fallback below it.
func (r *rolePromptRuleLive) writeOverlays(work string) map[claim.Token][]string {
	t := r.t
	t.Helper()
	dir := filepath.Join(work, "overlays")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	operator := filepath.Join(dir, "operator.yml")
	writeProofFile(t, operator, "# The operator's pane overlay (runtime.tmux.pi_config_files).\nretry:\n  enabled: false\n")
	overlays := map[claim.Token][]string{}
	for _, seeded := range r.seededRoles() {
		path := filepath.Join(dir, string(seeded.role)+".yml")
		writeRoleOverlay(t, path, standinModel(seeded.provider))
		overlays[r.token(seeded.role)] = []string{operator, path}
	}
	return overlays
}
