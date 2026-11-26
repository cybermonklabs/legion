//go:build e2e

// The live proof that a role process is launched with the role prompt the daemon composed for its
// role. Three roles of one tree each run a real pane: the tree's root architect, the planner of the
// same issue, and the architect of a child of the root, a sub-architect. The prompt each pane's own
// Oh My Pi was started with is the bytes of every part the composer gives that role, in the
// composer's order, followed by a blank line and the addressing the launch carried, and nothing
// after it (runtime.ComposePrompt, given no deployment or variant instructions by this rig). The
// sub-architect's addressing names the root's architect as the architect above it
// (addressingFragment, in specs.go). scripts/e2e/role-prompt-rule.sh builds this driver with
// `-tags e2e` and owns the nats-server and the PostgreSQL it runs against.
//
// Where this proof reads the prompt, and why it reads it there. It reads the prompt from what the
// process itself was started with: the Oh My Pi process's own command line, /proc/<pid>/cmdline,
// and the file its one `--append-system-prompt` word names. The pid the claim's locator carries is
// the `legion worker-shim`'s (parseLocalIncarnation, over the incarnation
// localruntime_live_test.go's launch mints), and the shim starts Oh My Pi as a child rather than
// exec'ing into it (internal/shim/shim.go's exec.Command), so the proof walks /proc for the child
// of that shim whose argv[0] is LEGION_OMP_PATH and reads that child. Reading the shim's own argv
// would pass a shim that dropped or rewrote the prompt, which is not what this proof claims. The
// argv is NUL-separated, and the word after `--append-system-prompt` is the path of the file that
// holds the whole prompt the agent runs under: Oh My Pi reads a value with no newline as a path
// and appends the file's contents, and the launch writes that file before the process starts and
// never while it runs (runtime.WriteSystemPrompt).
//
// What it holds that prompt against, and why. It holds no word of any prompt, so a sentence or a
// heading of a part may change and the proof still passes. It holds each role's composition: the
// parts of the composer's snapshot that prompts.Composer.Compose gives that role, in that order,
// named by their paths below the state directory's prompts/ (composedParts, in
// live_proof_test.go; at boot prompts.New writes shared/ from the role prompts the daemon embeds,
// and go/ from the daemon's own parts). It reads the bytes of those parts from the snapshot once
// the panes run. A part left out of a role's composition, or one added to it, fails the proof for
// that role. A marker planted in the snapshot would need a write between the boot's New and the
// launch's read of the parts, and this rig has no such moment: New rewrites every snapshot file
// that does not hold its embedded bytes, and the boot relaunches the three seeded claims at once.
// The unit tests of internal/prompts compose each role's parts (prompts_test.go's composition
// table and common_part_test.go's planted markers), but none of them starts a process, so none
// says a role process was ever launched with them, and that is what this proof is for.
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

// argvWordShown is how much of one argv word, or of a prompt, a failure prints before it cuts it. A
// role prompt is tens of kilobytes, and a failure that printed it whole would bury the words around
// it.
const argvWordShown = 80

// The three panes, by the provider name each one's stand-in is given in the run's models.yml.
const (
	ruleArchitectProvider    = "architectpane"
	rulePlannerProvider      = "plannerpane"
	ruleSubArchitectProvider = "subarchitectpane"
)

// architectAboveYou is the clause a sub-architect's addressing names the architect above it with,
// which the proof reads followed by that architect's role topic. It is the daemon's own Go text
// (addressingFragment, in specs.go) and no prompt part's, so holding it tests the code that
// writes it.
const architectAboveYou = "the architect above you is `"

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
// is on, the role, the provider name of the stand-in that role's pane talks to, and the name a
// step gives it; and its composition.
type ruleRole struct {
	issue    string
	role     claim.Role
	provider string
	name     string
	// parts is the role's composition (composedParts): each part of the composer's snapshot that
	// prompts.Composer.Compose gives the role, in its order, as a path below the state
	// directory's prompts/ (snapshotDir).
	parts []string
}

// launchedPrompt is one agent process's own system prompt, with the pid it was read from and the
// pid of the shim that started it.
type launchedPrompt struct {
	pid    int
	shim   int
	prompt string
}

// TestEveryRoleIsLaunchedWithTheRulesItsOwnPromptCarriesLive boots the daemon over three real
// panes, reads the prompt each of those three processes was launched with, and holds that it is
// the prompt the daemon composed for that role, so that every rule the role's prompt carries
// reached the process. The planner's, the root architect's and the child's architect's prompts
// are each the bytes of their own composition's parts, in order, followed by the addressing their
// launch carried and nothing after it. The child's architect's addressing names the root's
// architect as the architect above it.
func TestEveryRoleIsLaunchedWithTheRulesItsOwnPromptCarriesLive(t *testing.T) {
	fixtureenv.Live(t)
	r := newRolePromptRuleLive(t)
	r.step("step1-every-role-of-the-tree-runs-a-pane-whose-own-argv-carries-a-system-prompt", r.everyPaneIsReady)
	r.step("step2-the-planner-was-launched-with-the-prompt-composed-for-the-planner", r.plannerCarriesItsComposition)
	r.step("step3-the-root-architect-was-launched-with-the-prompt-composed-for-a-root-architect", r.rootArchitectCarriesItsComposition)
	r.step("step4-the-childs-architect-was-launched-with-the-prompt-composed-for-a-sub-architect-under-the-root-architect", r.subArchitectCarriesItsComposition)
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
	r.note("Oh My Pi %s at %s, the build %s names", strings.TrimSpace(string(version)), omp, liveOmpPathEnv)

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

// seededRoles is the three claims of the one tree that run a real pane, each with its
// composition (composedParts). The issue is its own tree's root, so its architect claim is the
// root architect's. The issue stands in planning, so the phase worker of that phase is the
// planner. The child's issue is not its tree's root, so its architect claim is a sub-architect's.
func (r *rolePromptRuleLive) seededRoles() []ruleRole {
	return []ruleRole{
		{issue: r.issue, role: claim.RoleArchitect, provider: ruleArchitectProvider, name: "root architect",
			parts: composedParts[composedRole{claim.RoleArchitect, true}]},
		{issue: r.issue, role: claim.RolePlanner, provider: rulePlannerProvider, name: "planner",
			parts: composedParts[composedRole{claim.RolePlanner, false}]},
		{issue: r.child, role: claim.RoleArchitect, provider: ruleSubArchitectProvider, name: "sub-architect",
			parts: composedParts[composedRole{claim.RoleArchitect, false}]},
	}
}

// seeded is the seeded role of role on issue.
func (r *rolePromptRuleLive) seeded(issue string, role claim.Role) ruleRole {
	r.t.Helper()
	for _, seeded := range r.seededRoles() {
		if seeded.issue == issue && seeded.role == role {
			return seeded
		}
	}
	r.t.Fatalf("the proof seeds no %s on %s", role, issue)
	return ruleRole{}
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
// steps hold each role's composition against.
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

// plannerCarriesItsComposition holds the planner's composition in the prompt the planner's own Oh
// My Pi was launched with.
func (r *rolePromptRuleLive) plannerCarriesItsComposition() {
	r.carriesItsComposition(r.seeded(r.issue, claim.RolePlanner))
}

// rootArchitectCarriesItsComposition holds the root architect's composition in the prompt the root
// architect's own Oh My Pi was launched with.
func (r *rolePromptRuleLive) rootArchitectCarriesItsComposition() {
	r.carriesItsComposition(r.seeded(r.issue, claim.RoleArchitect))
}

// subArchitectCarriesItsComposition holds the sub-architect's composition in the prompt the
// child's architect's own Oh My Pi was launched with, and in that prompt's addressing the clause
// that names the root's architect, by its role topic, as the architect above it.
func (r *rolePromptRuleLive) subArchitectCarriesItsComposition() {
	t := r.t
	t.Helper()
	seeded := r.seeded(r.child, claim.RoleArchitect)
	at := r.carriesItsComposition(seeded)
	sub, root := r.token(r.child, claim.RoleArchitect), r.token(r.issue, claim.RoleArchitect)
	read := r.launched[sub]
	above := architectAboveYou + notify.RoleTopicPrefix + string(root) + "`"
	found := strings.Index(read.prompt[at:], above)
	if found < 0 {
		t.Fatalf("the %s %s ran Oh My Pi as pid %d, launched with a prompt whose addressing, from byte %d, does not name the root's architect as the architect above it: %q",
			seeded.name, sub, read.pid, at, above)
	}
	r.note("the %s's addressing names the root's architect as the architect above it at byte %d: %q", seeded.name, at+found, above)
}

// carriesItsComposition holds that the prompt the role's own Oh My Pi was launched with is the
// role prompt the daemon composed for that role (runtime.ComposePrompt): from byte 0, the bytes
// of each part of the role's composition as the composer's snapshot holds them, one after the
// other in that order, the last with its trailing newlines dropped; then a blank line and the
// addressing the role's launch carried, and nothing after it. It prints where each part stands,
// and answers the byte at which the addressing begins.
//
// ComposePrompt puts only two things after the addressing: the launch's deployment instructions
// and the instructions of each experiment arm it runs in. This rig gives a launch neither, so the
// prompt's end is held exactly, and a launch that names either ends the proof naming the role and
// the files it named.
//
// A part that is not where the composition puts it ends the proof naming the role, its pid, the
// part, the byte it was looked for at, where its bytes stand instead, and the parts the launch
// named, since that line is the whole evidence of a negative control's run.
func (r *rolePromptRuleLive) carriesItsComposition(seeded ruleRole) int {
	t := r.t
	t.Helper()
	token := r.token(seeded.issue, seeded.role)
	read, ok := r.launched[token]
	if !ok {
		t.Fatalf("step 1 read no command line for the %s (%s)", seeded.name, token)
	}
	launch := r.launchOf(token)
	if launch.Prompt.DeploymentInstructionsPath != "" || len(launch.Prompt.VariantInstructionsPaths) > 0 {
		t.Fatalf("the %s %s was launched with deployment instructions %q and variant instructions %v, which this rig gives no launch, so the end of its prompt is not the addressing this proof holds it to",
			seeded.name, token, launch.Prompt.DeploymentInstructionsPath, launch.Prompt.VariantInstructionsPaths)
	}
	at := 0
	for i, part := range seeded.parts {
		body := r.snapshotPart(part)
		if i == len(seeded.parts)-1 {
			body = strings.TrimRight(body, "\n")
		}
		if !strings.HasPrefix(read.prompt[at:], body) {
			elsewhere := "its bytes stand nowhere in that prompt"
			if found := strings.Index(read.prompt, body); found >= 0 {
				elsewhere = fmt.Sprintf("its bytes stand at byte %d instead", found)
			}
			t.Fatalf("the %s %s ran Oh My Pi as pid %d, launched with a prompt of %d bytes that does not hold part %d of %d of its composition, %s (%d bytes), at byte %d, where the parts before it end; %s; its launch named %v",
				seeded.name, token, read.pid, len(read.prompt), i+1, len(seeded.parts), part, len(body), at, elsewhere,
				r.snapshotNames(launch.Prompt.RolePromptPaths))
		}
		r.note("part %d of %d of the %s's composition, %s, is bytes %d to %d of the prompt %s's own Oh My Pi (pid %d) was launched with",
			i+1, len(seeded.parts), seeded.name, part, at, at+len(body), token, read.pid)
		at += len(body)
	}
	end := "\n\n" + launch.Prompt.Addressing
	if tail := read.prompt[at:]; tail != end {
		if strings.HasPrefix(tail, end) {
			t.Fatalf("the %s %s ran Oh My Pi as pid %d, launched with a prompt whose addressing, the one its launch carried, ends at byte %d and is followed by %d bytes its launch named no file for: %s",
				seeded.name, token, read.pid, at+len(end), len(tail)-len(end), promptShown(tail[len(end):]))
		}
		t.Fatalf("the %s %s ran Oh My Pi as pid %d, launched with a prompt whose composition ends at byte %d and is not followed by a blank line and the addressing its launch carried; there it holds %s",
			seeded.name, token, read.pid, at, promptShown(tail))
	}
	r.note("the %s's composition, %d parts and %d bytes, is followed by a blank line and the %d bytes of addressing its launch carried, which end the prompt",
		seeded.name, len(seeded.parts), at, len(launch.Prompt.Addressing))
	return at + len("\n\n")
}

// ---- what a step reads --------------------------------------------------------------------------

// snapshotDir is the composer's snapshot below the daemon's state directory: prompts.New writes
// shared/ from the role prompts the daemon embeds and go/ from the daemon's own parts at boot, and
// a launch's composition names files of it (prompts.Composer.Compose).
func (r *rolePromptRuleLive) snapshotDir() string {
	return filepath.Join(r.cfg.StateDir, "prompts")
}

// snapshotPart is the bytes of one part of the composer's snapshot, named by its path below
// snapshotDir. A part that cannot be read, and an empty one, end the proof: an empty part stands
// in every prompt, so finding it would show nothing.
func (r *rolePromptRuleLive) snapshotPart(part string) string {
	t := r.t
	t.Helper()
	path := filepath.Join(r.snapshotDir(), part)
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read the composer's %s, the part the proof holds a composition to: %v", path, err)
	}
	if len(body) == 0 {
		t.Fatalf("the composer's %s is empty, so finding it in a prompt would show nothing", path)
	}
	return string(body)
}

// snapshotNames is each path as a failure names it: below snapshotDir when it is there, else
// whole.
func (r *rolePromptRuleLive) snapshotNames(paths []string) []string {
	names := make([]string, 0, len(paths))
	for _, path := range paths {
		if rel, err := filepath.Rel(r.snapshotDir(), path); err == nil && !strings.HasPrefix(rel, "..") {
			path = rel
		}
		names = append(names, path)
	}
	return names
}

// launchOf is the spec of the claim's latest launch, the one its live pane was started from: the
// addressing it carried, and the role prompt files it named, which a failure prints.
func (r *rolePromptRuleLive) launchOf(token claim.Token) runtime.SpawnSpec {
	r.t.Helper()
	launches := r.rt.LaunchesOf(token)
	if len(launches) == 0 {
		r.t.Fatalf("the proof's runtime records no launch of %s", token)
	}
	return launches[len(launches)-1]
}

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

// ompChildOf is the pid of the Oh My Pi the shim at pid shim started: the live process whose
// PPid is that shim and whose argv[0] is the binary LEGION_OMP_PATH names. /proc is walked
// rather than /proc/<pid>/task/<tid>/children read, because that file lists only the children
// one thread of the process forked, and the Go shim forks from whichever of its threads runs
// exec.Command: measured in a gVisor pod, the shim's main thread listed no child while another
// of its threads listed its Oh My Pi. A process that ends between the two reads is passed over,
// as a walk of /proc must do.
func (r *rolePromptRuleLive) ompChildOf(token claim.Token, shim int) int {
	t := r.t
	t.Helper()
	entries, err := os.ReadDir("/proc")
	if err != nil {
		t.Fatalf("read /proc to find the Oh My Pi %s's shim (pid %d) started: %v", token, shim, err)
	}
	var ofTheShim []string
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil || pid == shim {
			continue
		}
		status, err := os.ReadFile("/proc/" + entry.Name() + "/status")
		if err != nil || parentOf(string(status)) != shim {
			continue
		}
		raw, err := os.ReadFile("/proc/" + entry.Name() + "/cmdline")
		if err != nil {
			continue
		}
		argv := strings.Split(strings.TrimSuffix(string(raw), "\x00"), "\x00")
		if len(argv) == 0 {
			continue
		}
		if argv[0] == r.omp {
			return pid
		}
		ofTheShim = append(ofTheShim, fmt.Sprintf("%d %s", pid, argv[0]))
	}
	t.Fatalf("the shim of %s (pid %d) has no live child whose argv[0] is %s, the Oh My Pi this run launches; its children are %v, and that pane's own output is under %s",
		token, shim, r.omp, ofTheShim, r.rt.paneLogs)
	return 0
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

// argvSummary is an argv as a failure names it: every word, each quoted and cut (promptShown).
func argvSummary(argv []string) string {
	words := make([]string, 0, len(argv))
	for _, word := range argv {
		words = append(words, promptShown(word))
	}
	return "[" + strings.Join(words, " ") + "]"
}

// promptShown is a word or a prompt as a failure prints it: quoted, and when it is longer than
// argvWordShown, cut to its first runes with its length given. The cut is of runes rather than
// bytes, since a role prompt carries `—` and `…` and half a rune reads as an escape.
func promptShown(text string) string {
	if runes := []rune(text); len(runes) > argvWordShown {
		text = fmt.Sprintf("%s… (%d bytes)", string(runes[:argvWordShown]), len(text))
	}
	return strconv.Quote(text)
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
