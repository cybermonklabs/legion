//go:build e2e

// The live proof that a role process is launched with the rules its own system prompt carries:
// the pod memory rule, in the paragraph every phase worker is given
// (packages/daemon/internal/prompts/roles/mechanics/headless.md, "What you run in the pod") and
// the paragraph every architect is given
// (packages/daemon/internal/prompts/go/architect-common.md); the planner's own rule that its
// completion reports work already done, in the paragraph that names the `head` its handoff
// carries, what the daemon does with it, and the order that records the handoff before the
// completion is reported
// (packages/daemon/internal/prompts/roles/planner.md, "Plan handoff"); and the prompt of a
// child's own architect, a sub-architect, whose role part is the sub-architect's
// (packages/daemon/internal/prompts/roles/architect.md, `# Legion Sub-Architect`) and whose
// addressing line names the architect above it (addressingFragment, in specs.go).
// scripts/e2e/role-prompt-rule.sh builds this driver with `-tags e2e` and owns the nats-server
// and the PostgreSQL it runs against.
//
// What this proof reads, and why it reads it there. The prompts package's unit tests hold which
// parts each role's prompt is composed of (packages/daemon/internal/prompts/prompts_test.go's
// composition table and common_part_test.go's planted markers).
// None of them starts anything, so none says that a role process was ever launched with that
// text. This driver reads the text from what the process itself was started with: the Oh My Pi
// process's own command line, /proc/<pid>/cmdline, and the file its one `--append-system-prompt`
// word names. The pid the claim's locator carries is the
// `legion worker-shim`'s (parseLocalIncarnation, over the incarnation localruntime_live_test.go's
// launch mints), and the shim starts Oh My Pi as a child
// rather than exec'ing into it (internal/shim/shim.go's exec.Command), so the proof walks /proc
// for the child of that shim whose argv[0] is LEGION_OMP_PATH and reads that child. Reading the
// shim's own argv would pass a shim that dropped or rewrote the prompt, which is not what this
// proof claims. The argv is NUL-separated, and the word after `--append-system-prompt` is the
// path of the file that holds the whole prompt the agent runs under: Oh My Pi reads a value with
// no newline as a path and appends the file's contents, and the launch writes that file before
// the process starts and never while it runs (runtime.WriteSystemPrompt).
//
// It reads the prompt from nowhere else on purpose. The composer, the role prompt files below the
// state directory and the SpawnSpec the runtime recorded each hold the text before a process ever
// took it, so a proof that read any of the three would prove again what the unit tests prove.
// Only the kernel's copy of the argv says which file the agent was started with.
//
// The daemon is the whole daemon: run, the function `legion start` calls, through this package's
// own whole-daemon harness (startDaemonWithin) over the live-proof fixture (live_proof_test.go).
// Each pane is a real `omp --mode rpc` under the real `legion worker-shim`, started by
// localruntime_live_test.go, since a Legion pod on a worker image from before
// dispatch://INFRA-3257 carries no tmux, with this checkout's `@sjawhar/pi-envoy` and
// `@sjawhar/pi-legion` in an Oh My Pi profile of the run's own. What stands in is the model
// endpoint and nothing else: each pane talks to one HTTP stand-in of the driver's own
// (newPaneStandin, claim_compaction_live_test.go), which answers every turn with one short
// message.
//
// The seed is one tree with one child, so that the daemon starts three real panes of three
// different kinds: the tree's root architect, the planner of the same issue, which stands in
// planning, and the architect of the child, which stands in planning too. Each architect's claim
// is seeded with no task (a child's architect's start carries none, and this seed gives the root's
// none); the planner's carries the task its phase's start left pending, because a start writes the
// relaunch and the task together (workflow's StartOrOwe) and a slotted worker holding no task at
// all is the state admission repairs (internal/admit/admit.go's resumeSlottedWorkers), which would
// deliver a task of its own instead.
// The child has no planner's claim and no phases row, so that repair owes it nothing and starts no
// fourth pane: the planner this proof reads is the root's.
//
// This proof writes no role prompt file of its own, which is the one thing its seed does
// differently from its two siblings' (seed). An operator's prompt file below the state directory
// is a launch's whole role prompt (specs.rolePromptPaths), so a seed that wrote one would launch
// every pane on that literal and the composed prompts would reach no process at all.
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

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/fixtureenv"
	"github.com/sjawhar/legion/daemon/internal/notify"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/store"
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

// The three panes, by the provider name each one's stand-in is given in the run's models.yml.
const (
	ruleArchitectProvider    = "architectpane"
	rulePlannerProvider      = "plannerpane"
	ruleSubArchitectProvider = "subarchitectpane"
)

// The three sentences every reader is given word for word. Each is a literal of this file, so a
// reworded rule fails this proof rather than passing on a text the driver derived from the same
// files it is reading.
const (
	ruleSandboxEndsTheContainer = "where that pod's sandbox ends the whole container rather than the one process that filled it"
	ruleEveryRolesTurn          = "a command that fills the limit ends every role's turn and not only yours"
	ruleCIsWork                 = "A whole module's test run (`go test ./...`), a whole Postgres-backed package, a browser suite run whole, and `make -j` or `lake build` are CI's work on the pull request"
)

// podRuleForPhaseWorker is the pod memory rule as a phase worker's prompt carries it
// (packages/daemon/internal/prompts/roles/mechanics/headless.md), sentence by sentence.
var podRuleForPhaseWorker = []string{
	"In an issue pod every role of this issue runs in one container and shares its memory limit",
	ruleSandboxEndsTheContainer,
	ruleEveryRolesTurn,
	ruleCIsWork,
	"what a role runs in the pod is what `skill://legion-worker/references/tools.md` names",
	"Every other heavy run goes where your deployment's instructions say",
	"instructions that name no such route are a gap you report to the architect that owns your issue",
	"Each subagent you start runs its commands in this pod, so put these facts in its context",
}

// podRuleForArchitect is the same rule as every architect's prompt carries it, a root's and a
// child's alike (packages/daemon/internal/prompts/go/architect-common.md, which the composer adds
// to either architect's own part), written for the subagents an architect starts rather than for
// the architect's own commands.
var podRuleForArchitect = []string{
	"Your subagents run their commands in your own issue pod",
	ruleSandboxEndsTheContainer,
	ruleEveryRolesTurn,
	ruleCIsWork,
	"what may run in the pod is what `skill://legion-worker/references/tools.md` names",
	"every other heavy run goes where your deployment's instructions say and instructions that name no such route are a gap to report, and you put these facts in the context of each subagent you start",
}

// What a child's architect is launched with and a root architect is not. The heading opens the
// sub-architect's own part (packages/daemon/internal/prompts/roles/architect.md), which the
// composer gives an architect whose issue is not its tree's root (prompts.Composer.Compose), where
// a root architect's opens with the root's heading instead (roles/architect-root.md). The clause
// is the one its addressing line names the architect above it with (addressingFragment, in
// specs.go), which the proof reads followed by that architect's role topic.
const (
	subArchitectHeading  = "# Legion Sub-Architect"
	rootArchitectHeading = "# Legion Root Architect"
	architectAboveYou    = "the architect above you is `"
)

// completionAfterWorkForPlanner is the rule the planner's own part carries about its completion
// (packages/daemon/internal/prompts/roles/planner.md, "Plan handoff"): the completion reports
// work already done, at a named head. Three needles hold it. The first two are the `head` field as
// the handoff body names it, with the one thing its value is, and the clause that carries that
// head to the daemon, which names the planner's own phase word. The third is the paragraph's order
// sentence, copied from planner.md byte for byte: the record written with `dispatch handoff-write` under
// the planner's own phase word `plan`, then the completion, which is what makes the completion a
// report of work already recorded rather than of work still to come. The three are short and sit
// inside the paragraph, so a rewording of the prose around them stands, and the field's removal
// or an order sentence that no longer puts the record before the completion is what fails them.
// A planner's composed prompt carries no other role's part (prompts_test.go's composition table),
// so the first needle is this role's own field and no other's.
var completionAfterWorkForPlanner = []string{
	"`head` (the 40-character lower-case commit",
	"to the daemon, which takes a completion of an earlier `plan` entry only at the head the record holds",
	"Record it with `dispatch handoff-write --issue <issue> --phase plan --body-file - <<'EOF'`, the JSON on the lines after it and `EOF` alone on the last line, then report completion",
}

// rolePromptRuleLive is one run of the proof: the booted daemon, the one tree it seeds, and the
// three claims whose own command lines it reads.
type rolePromptRuleLive struct {
	liveAgentProof
	// issue is the tree's root: its own root architect works it, and it stands in planning, so
	// its planner works it too.
	issue string
	// child is a child of issue in its tree, which stands in planning too and whose own
	// architect works it.
	child string
	// panes is each live claim's own model stand-in, the one thing its pane talks to.
	panes map[claim.Token]*paneStandin
	// home and profile are the Oh My Pi home and profile every pane runs under.
	home, profile string
	// overlays is each claim's PI_CONFIG_FILES, in order.
	overlays map[claim.Token][]string
	// launched is what step 1 read out of each claim's own Oh My Pi: its pid, the pid of the
	// shim that started it, and the prompt in the file its one `--append-system-prompt` word names.
	launched map[claim.Token]launchedPrompt
}

// ruleRole is one of the proof's three claims as the seed and the profile name it: the issue it
// is on, the role, and the provider name of the stand-in that role's pane talks to.
type ruleRole struct {
	issue    string
	role     claim.Role
	provider string
}

// launchedPrompt is one agent process's own system prompt, with the pid it was read from and the
// pid of the shim that started it.
type launchedPrompt struct {
	pid    int
	shim   int
	prompt string
}

// TestEveryRoleIsLaunchedWithTheRulesItsOwnPromptCarriesLive boots the daemon over three real
// panes, reads the command line each of those three processes was launched with, and holds in
// each the rules that role's own prompt carries: the pod memory rule, the phase worker's eight
// sentences and every architect's six; in the planner's, the three needles of the rule that its
// completion reports work already done at a named head: the `head` field, the clause that carries
// it to the daemon, and the order sentence that records the `plan` handoff before the completion
// is reported; and in the child's architect's, the sub-architect's own heading in place of the
// root's, and the addressing clause that names the root's architect as the architect above it.
func TestEveryRoleIsLaunchedWithTheRulesItsOwnPromptCarriesLive(t *testing.T) {
	fixtureenv.Live(t)
	r := newRolePromptRuleLive(t)
	r.step("step1-every-role-of-the-tree-runs-a-pane-whose-own-argv-carries-a-system-prompt", r.everyPaneIsReady)
	r.step("step2-the-phase-worker-was-launched-with-the-pod-memory-rule", r.phaseWorkerCarriesTheRule)
	r.step("step3-the-root-architect-was-launched-with-the-pod-memory-rule", r.rootArchitectCarriesTheRule)
	r.step("step4-the-planner-was-launched-with-the-completion-after-work-rule", r.plannerCarriesTheCompletionRule)
	r.step("step5-the-childs-architect-was-launched-with-the-sub-architect-prompt-and-the-pod-memory-rule", r.subArchitectCarriesItsPrompt)
}

// newRolePromptRuleLive readies everything a pane needs before the daemon boots: the `legion`
// binary and the two plugins built from this checkout, an Oh My Pi profile of the run's own routed
// to the three stand-ins, the one tree seeded with its three claims mid-launch, and the daemon
// booted over the local runtime.
func newRolePromptRuleLive(t *testing.T) *rolePromptRuleLive {
	t.Helper()
	omp := os.Getenv(liveOmpPathEnv)
	if omp == "" {
		t.Skipf("%s is unset, so there is no Oh My Pi for a pane to run; scripts/e2e/role-prompt-rule.sh requires it", liveOmpPathEnv)
	}
	proof := newLiveProof(t, "scripts/e2e/role-prompt-rule.sh")
	r := &rolePromptRuleLive{
		liveAgentProof: liveAgentProof{liveProof: proof, omp: omp},
		issue:          proof.cfg.Project + "-1",
		child:          proof.cfg.Project + "-2",
		panes:          map[claim.Token]*paneStandin{},
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
		r.panes[r.token(seeded.issue, seeded.role)] = newPaneStandin(t, seeded.provider)
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

// seededRoles is the three claims of the one tree that run a real pane. The issue is its own
// tree's root, so its architect claim is the root architect's, which is the claim that carries
// architect-root.md and architect-common.md; the issue stands in planning, so the phase worker of
// that phase is the planner, which is the claim that carries mechanics/headless.md. The child's
// issue is not its tree's root, so its architect claim is a sub-architect's, which carries
// architect.md and architect-common.md.
func (r *rolePromptRuleLive) seededRoles() []ruleRole {
	return []ruleRole{
		{r.issue, claim.RoleArchitect, ruleArchitectProvider},
		{r.issue, claim.RolePlanner, rulePlannerProvider},
		{r.child, claim.RoleArchitect, ruleSubArchitectProvider},
	}
}

// token is the claim token of role on issue, as the daemon spells it.
func (r *rolePromptRuleLive) token(issue string, role claim.Role) claim.Token {
	r.t.Helper()
	token, err := claim.NewToken(r.project, issue, role)
	if err != nil {
		r.t.Fatal(err)
	}
	return token
}

// overrides is the daemon this proof boots: the local runtime's own wiring (daemonOverrides), and
// the workflow's App tokens stood in for. The clock is the real one, since nothing here is measured
// against a wait.
func (r *rolePromptRuleLive) overrides() overrides {
	o := r.rt.daemonOverrides(r.rt)
	o.workflowTokens = &workflowTokenRecorder{}
	return o
}

// seed records the tree as the daemon would hold it at the moment its roles began: a root handed
// to Legion, in planning, holding its admission slot with its tree's lifecycle open, and its
// child, in planning on a slot of its own as a started child holds one; and the three claims
// mid-launch, which this boot relaunches. The planner's claim also holds the task its phase's
// start left pending, which is the state admission's repair of a slotted worker passes over
// (workflow.StartFor, the delivery's StillWorked); neither architect's holds one: the repair
// weighs a root's architect claim (admit's owedEntryRoles) but never a child's. The child has no
// planner's claim and no phases row, so that repair owes it nothing.
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
	r.board.place(r.child, "in_progress", r.issue, false)
	if err := store.Transact(ctx, r.st.Pool(), func(ctx context.Context, tx pgx.Tx) error {
		if err := r.records.PutIssue(ctx, tx, record.Issue{
			Key: r.issue, Repo: liveRepo, DefaultBranch: "main", GitHubRepo: liveRepo, Tree: r.issue,
			Project: r.cfg.Project, Title: "the issue whose architect and planner this proof reads the command line of",
			Phase: phase.Planning, Generation: 1, Status: "in_progress",
			DispatchStatus: "in_progress", Rank: "U", LastDispatchSeq: 1, HandedOver: true,
		}); err != nil {
			return err
		}
		if err := r.records.PutIssue(ctx, tx, record.Issue{
			Key: r.child, Repo: liveRepo, DefaultBranch: "main", GitHubRepo: liveRepo, Tree: r.issue, Parent: &r.issue,
			Project: r.cfg.Project, Title: "the child whose own architect this proof reads the command line of",
			Phase: phase.Planning, Generation: 1, Status: "in_progress",
			DispatchStatus: "in_progress", Rank: "V", LastDispatchSeq: 1,
		}); err != nil {
			return err
		}
		return r.admit(ctx, tx, r.issue, r.child)
	}); err != nil {
		t.Fatalf("seed %s and its child %s: %v", r.issue, r.child, err)
	}
	if _, err := r.st.OpenTreeLifecycle(ctx, r.project, r.issue, treelifecycle.AuthorityWorkflow); err != nil {
		t.Fatalf("open the lifecycle of %s: %v", r.issue, err)
	}
	for _, seeded := range r.seededRoles() {
		token := r.token(seeded.issue, seeded.role)
		if _, err := r.st.AdmitClaim(ctx, supervise.Claim{
			Token: token, Project: r.project, Tree: r.issue, Issue: seeded.issue, Role: seeded.role,
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
	r.note("seeded %s in %s, with its %s and its %s mid-launch, and its child %s in %s with its own %s mid-launch, "+
		"and no role prompt file of this proof's own, so each pane is launched on the role prompts the daemon composed",
		r.issue, phase.Planning, claim.RoleArchitect, claim.RolePlanner, r.child, phase.Planning, claim.RoleArchitect)
}

// ---- the steps -----------------------------------------------------------------------------------

// everyPaneIsReady waits for each claim's real pane to start Oh My Pi, register and report ready,
// and then reads the prompt file that Oh My Pi's own command line names, which is what the later
// steps hold each rule in.
func (r *rolePromptRuleLive) everyPaneIsReady() {
	for _, seeded := range r.seededRoles() {
		token := r.token(seeded.issue, seeded.role)
		r.awaitReady(token)
		read := r.launchedPromptOf(token)
		r.launched[token] = read
		r.note("%s runs Oh My Pi as pid %d, the child of its shim %d, whose own /proc/%d/cmdline carries one %s word, the path of a prompt file of %d bytes",
			token, read.pid, read.shim, read.pid, promptWord, len(read.prompt))
	}
}

// phaseWorkerCarriesTheRule holds the eight sentences of the pod memory rule in the planner's own
// command line.
func (r *rolePromptRuleLive) phaseWorkerCarriesTheRule() {
	r.carriesTheRule("phase worker", "pod memory rule", r.token(r.issue, claim.RolePlanner), podRuleForPhaseWorker)
}

// rootArchitectCarriesTheRule holds the six sentences of the architect's own wording of the rule
// in the root architect's own command line.
func (r *rolePromptRuleLive) rootArchitectCarriesTheRule() {
	r.carriesTheRule("root architect", "pod memory rule", r.token(r.issue, claim.RoleArchitect), podRuleForArchitect)
}

// plannerCarriesTheCompletionRule holds the three parts of the rule the planner's own part carries
// about its completion — the `head` the handoff body names, the clause that carries that head to
// the daemon, and the sentence that records the `plan` handoff with `dispatch handoff-write` and
// then reports completion — in the planner's own command line. The root architect is held to no
// part of it: an architect writes no handoff record, and its prompt carries no such paragraph.
func (r *rolePromptRuleLive) plannerCarriesTheCompletionRule() {
	r.carriesTheRule("planner", "completion-after-work rule", r.token(r.issue, claim.RolePlanner), completionAfterWorkForPlanner)
}

// subArchitectCarriesItsPrompt holds, in the prompt the child's architect's own Oh My Pi was
// launched with, what a sub-architect is launched with: the heading of its own part, and not the
// root's; the addressing clause that names the root's architect, by its role topic, as the
// architect above it; and every architect's six sentences of the pod memory rule. The root
// architect's own prompt carries the root's heading and not the sub-architect's, so the two
// architects were composed apart.
func (r *rolePromptRuleLive) subArchitectCarriesItsPrompt() {
	t := r.t
	t.Helper()
	sub, root := r.token(r.child, claim.RoleArchitect), r.token(r.issue, claim.RoleArchitect)
	above := architectAboveYou + notify.RoleTopicPrefix + string(root) + "`"
	r.carriesTheRule("sub-architect", "sub-architect prompt", sub, []string{subArchitectHeading, above})
	for _, wrong := range []struct {
		token   claim.Token
		heading string
	}{{sub, rootArchitectHeading}, {root, subArchitectHeading}} {
		read := r.launched[wrong.token]
		if at := strings.Index(read.prompt, wrong.heading); at >= 0 {
			t.Fatalf("%s ran Oh My Pi as pid %d, launched with a prompt that carries %q at byte %d, the other architect's heading",
				wrong.token, read.pid, wrong.heading, at)
		}
	}
	r.note("the sub-architect's own Oh My Pi prompt carries no %q, and the root architect's carries no %q",
		rootArchitectHeading, subArchitectHeading)
	r.carriesTheRule("sub-architect", "pod memory rule", sub, podRuleForArchitect)
}

// carriesTheRule holds every part of the role's own wording of the named rule in the prompt that
// role's process was launched with, and prints where each one was found. A part it cannot find
// ends the proof naming the role, its pid and that part's own text, since that line is the whole
// evidence of a negative control's run.
//
// The match is of the prompt file's own bytes, where the composed-prompt unit test folds
// whitespace first (prompts_test.go's foldWhitespace). That is the stronger claim and the one this
// proof is for: the text stands in the launched prompt word for word. The cost is that a reflow of
// one of those paragraphs across a line break would fail here while the unit test stayed green,
// and the reader of that failure is told the text it could not find.
func (r *rolePromptRuleLive) carriesTheRule(what, rule string, token claim.Token, parts []string) {
	t := r.t
	t.Helper()
	read, ok := r.launched[token]
	if !ok {
		t.Fatalf("step 1 read no command line for the %s (%s)", what, token)
	}
	for i, part := range parts {
		at := strings.Index(read.prompt, part)
		if at < 0 {
			t.Fatalf("the %s %s ran Oh My Pi as pid %d, launched with a prompt of %d bytes that does not carry part %d of %d of the %s: %q",
				what, token, read.pid, len(read.prompt), i+1, len(parts), rule, part)
		}
		r.note("part %d of %d of the %s is at byte %d of the %s %s's own Oh My Pi prompt: %q",
			i+1, len(parts), rule, at, what, token, part)
	}
	r.note("all %d parts of the %s are in the command line Oh My Pi pid %d was launched with",
		len(parts), rule, read.pid)
}

// ---- what a step reads --------------------------------------------------------------------------

// launchedPromptOf is the prompt the claim's own Oh My Pi was launched with: the contents of the
// file the `--append-system-prompt` word of that process's /proc/<pid>/cmdline names, the
// kernel's own copy of its argv, one word per NUL-separated field.
//
// The pid the claim's locator carries is the shim's, not the agent's: localruntime_live_test.go
// launches `legion worker-shim … -- omp --mode rpc --append-system-prompt <prompt file>` and
// records that process's pid, and the shim starts Oh My Pi as a child (internal/shim/shim.go's
// exec.Command) rather than exec'ing into it. So this walks /proc for the shim's own child whose
// argv[0] is the Oh My Pi this run launches, and reads that child. A shim that dropped or
// rewrote the prompt fails here, where a read of the shim's own argv would pass it.
//
// A shim with no such child, a command line it cannot read, an argv with no prompt word, and a
// prompt file that cannot be read — which Oh My Pi would have taken as the prompt's text — each
// end the proof naming what it found.
func (r *rolePromptRuleLive) launchedPromptOf(token claim.Token) launchedPrompt {
	t := r.t
	t.Helper()
	held := r.claim(token)
	if held.Locator == nil {
		t.Fatalf("%s is %s and the daemon's claim listing gives it no locator, so it names no process to read", token, held.State)
	}
	shim, _, err := parseLocalIncarnation(held.Locator.Incarnation)
	if err != nil {
		t.Fatalf("the locator of %s names no pid: %v", token, err)
	}
	pid := r.ompChildOf(token, shim)
	path := "/proc/" + strconv.Itoa(pid) + "/cmdline"
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s, the command line %s's Oh My Pi was launched with: %v; that pane's own output is under %s",
			path, token, err, r.rt.paneLogs)
	}
	argv := strings.Split(strings.TrimSuffix(string(raw), "\x00"), "\x00")
	// From the end, because Oh My Pi applies the last such word. A launch builds exactly one, so
	// the two readings agree today; this one is the word the process runs under whatever the argv
	// carries.
	for i := len(argv) - 1; i >= 0; i-- {
		if argv[i] != promptWord {
			continue
		}
		if i+1 == len(argv) {
			t.Fatalf("%s's Oh My Pi (pid %d, under shim %d) was launched with %s as the last word of its argv, so it carries no prompt: %s",
				token, pid, shim, promptWord, argvSummary(argv))
		}
		file := argv[i+1]
		text, err := os.ReadFile(file)
		if err != nil {
			t.Fatalf("%s's Oh My Pi (pid %d, under shim %d) was launched with %s %s, which could not be read, so it ran under that path's own text: %v",
				token, pid, shim, promptWord, file, err)
		}
		return launchedPrompt{pid: pid, shim: shim, prompt: string(text)}
	}
	t.Fatalf("%s's Oh My Pi (pid %d, under shim %d) was launched with no %s word at all; its argv is %s",
		token, pid, shim, promptWord, argvSummary(argv))
	return launchedPrompt{}
}

// ompChildOf is the pid of the Oh My Pi the shim at pid shim started, the binary LEGION_OMP_PATH
// names, which the free ompChildOf (omp_child_live_test.go) finds by a walk of /proc and fails
// the proof without.
func (r *rolePromptRuleLive) ompChildOf(token claim.Token, shim int) int {
	r.t.Helper()
	return ompChildOf(r.t, r.omp, r.rt.paneLogs, token, shim)
}

// parentOf is the PPid a /proc/<pid>/status holds, or 0 when it holds none.
func parentOf(status string) int {
	for _, line := range strings.Split(status, "\n") {
		rest, found := strings.CutPrefix(line, "PPid:")
		if !found {
			continue
		}
		parent, err := strconv.Atoi(strings.TrimSpace(rest))
		if err != nil {
			return 0
		}
		return parent
	}
	return 0
}

// argvSummary is an argv as a failure names it: every word, each quoted, with a word longer than
// argvWordShown cut to its first runes and its length given. The cut is of runes rather than
// bytes, since a role prompt carries `—` and `…` and half a rune reads as an escape.
func argvSummary(argv []string) string {
	words := make([]string, 0, len(argv))
	for _, word := range argv {
		shown := word
		if runes := []rune(word); len(runes) > argvWordShown {
			shown = fmt.Sprintf("%s… (%d bytes)", string(runes[:argvWordShown]), len(word))
		}
		words = append(words, strconv.Quote(shown))
	}
	return "[" + strings.Join(words, " ") + "]"
}

// ---- the Oh My Pi profile and the overlays -----------------------------------------------------------

// writeOmpProfile writes the profile every pane shares: the three stand-in providers, one model
// each, and the settings a pane needs to run unattended. The context window is declared because a
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
		stand := r.panes[r.token(seeded.issue, seeded.role)]
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
		path := filepath.Join(dir, seeded.issue+"-"+string(seeded.role)+".yml")
		writeRoleOverlay(t, path, standinModel(seeded.provider))
		overlays[r.token(seeded.issue, seeded.role)] = []string{operator, path}
	}
	return overlays
}
