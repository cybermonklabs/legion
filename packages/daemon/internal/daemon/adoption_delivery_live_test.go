//go:build e2e

// The live proof that a phase task reaches its role across the adoption of the working copy
// (dispatch://INFRA-3916 R1, R3, R6 and R7), driven against real Oh My Pi panes under the real
// daemon, each working a real jj workspace. scripts/e2e/adoption-delivery.sh builds this driver
// with `-tags e2e` and owns the nats-server and the PostgreSQL it runs against.
//
// The daemon is the whole daemon: run, the function `legion start` calls, through this package's
// own whole-daemon harness (startDaemonLogging) over the live-proof fixture (live_proof_test.go),
// so the proof exercises the real configuration, the real store and its migrations, the real HTTP
// API, the real claim supervision with its worker stream and its timers, and the real workflow.
// Each worker is a real `omp --mode rpc` under the real `legion worker-shim`, started by
// localruntime_live_test.go, with this checkout's `@sjawhar/pi-envoy` and `@sjawhar/pi-legion` in
// an Oh My Pi profile of the run's own. The runtime adopts each working copy through the pane's own
// worker-stream connection (localRuntime.adoptWithin), as the tmux runtime does, so the adoption
// is the real shim's (internal/shim's runAdoption) running the jj on PATH in the pane's workspace.
//
// Each pane's working directory is a jj workspace of a clone the driver makes, as a pod's is: a
// git clone of a bare origin of the run's own, made a colocated jj repository (`jj git init
// --colocate`), with the pane's workspace added to it by `jj workspace add`, which makes it a git
// worktree of the clone. The clone's own workspace is the second workspace the driver works in, as
// another role of the issue works one. The origin's one commit holds a README.md and a .gitignore
// that leaves out /tmp/, the TMPDIR the runtime gives each pane inside its working directory.
//
// The three checks, each its own issue of the run's project:
//
//   - stale-adopt: before the daemon boots, the clone's own workspace rebases the implementer's
//     working-copy commit onto a commit that changes a file, which leaves the implementer's
//     workspace stale, and then a tracked file is changed and a new file written in it with no jj
//     command between. The daemon sends the phase task its seed left pending once the pane is
//     ready; the adoption brings the working copy up to date with `jj workspace update-stale`,
//     which leaves the two edits in a divergent commit that is not checked out, and the stand-in
//     model receives a prompt that names that commit. The commit is read three ways — from the
//     prompt, from the daemon's own line and from `jj log` of the change's commits that are not
//     @ — and must be one; the shim's own log line names it too; the edits are no longer on disk;
//     `jj status` in the workspace exits 0 and names no stale working copy; and both edits are in
//     that commit (`jj file show -r <commit>`).
//   - refused-adoption-resent: before the daemon boots, a stale index.lock is left in the git
//     directory of the implementer's workspace, a worktree's (`git rev-parse --git-dir`), whose @
//     is undescribed and authored by the driver. With the adoption's flags (--ignore-working-copy)
//     the lock fails the adoption at its metaedit alone, the one command that writes an operation,
//     as jj then resets git's HEAD state (`Failed to reset Git HEAD state`); the metaedit's
//     operation is not committed, so the author is unchanged and every later adoption fails the
//     same way. So the shim refuses each send of the phase task, at every sweep of the runtime,
//     more times than prompt_failure_limit (adoptionPromptFailureLimit); the claim is not retired,
//     keeps its one launch and its generation and is charged nothing, and the issue's architect
//     gets one `worker-stalled` notice for the task. The lock is then removed, and the task
//     reaches the stand-in at the next sweep, with no message and no operator call.
//   - ended-turn-holds-nothing: the implementer and the tester of one issue in testing share one
//     workspace. The implementer answers a warm-up task at boot; then the step gives it a task
//     whose request its stand-in holds open, so its turn runs. The tester's launch is held at its
//     start until then (localRuntime.gate), so its pane reports ready while that turn runs, and
//     its phase task, which its seed left pending, waits rather than adopt the working copy the two
//     share. The stand-in then answers the held request 529 with a retry-after over Oh My Pi's own
//     cap, so the implementer's turn ends in a provider error and its own task waits out the
//     ladder's first 30 s rung; the tester's phase task is sent at the next sweep, before that rung
//     ends, and its prompt reaches its stand-in.
//
// What stands in is each pane's model endpoint, one HTTP stand-in per claim (adoptionStandin),
// over the live-proof base this proof shares with the other agent proofs (live_agent_proof_test.go),
// and the two Apps' tokens, whose leases carry the bot identity, an email with it, that the
// adoption sets as the working copy's author (idleRigAppTokens). Each pane's operator overlay sets
// `retry.enabled: false`, so the host walks no fallback chain and a failed turn reaches the daemon.
//
// What the daemon already holds when it boots is what provider_error_retry_live_test.go's seed
// says: each issue holding its tree's admission slot in the phase it stands in, each claim
// mid-launch, which this boot relaunches, and a task pending on each claim, which keeps admission's
// repair of a slotted worker off it (internal/admit's resumeSlottedWorkers). The two implementers
// of the first two checks and the tester of the third hold their phase's task, the one this proof
// watches go; the third check's implementer works no phase of its issue and holds a warm-up task
// of no phase, which adopts nothing.
package daemon

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/fixtureenv"
	"github.com/sjawhar/legion/daemon/internal/fixturegit"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/treelifecycle"
)

// The four model stand-ins, by the provider name each is given in the run's models.yml: one per
// claim, so the endpoint a request reached says which pane sent it.
const (
	// adoptStaleProvider is the stale-adopt implementer's.
	adoptStaleProvider = "stale"
	// adoptLockedProvider is the refused-adoption-resent implementer's.
	adoptLockedProvider = "locked"
	// adoptEndingProvider is the ended-turn-holds-nothing implementer's, whose turn ends in a
	// provider error.
	adoptEndingProvider = "ending"
	// adoptWaitingProvider is the ended-turn-holds-nothing tester's, whose phase task waits for
	// that turn.
	adoptWaitingProvider = "waiting"
)

// adoptionPromptFailureLimit is the proof's prompt_failure_limit: the count of answered failures
// of one task at which the issue's architect is told, which the refused adoptions pass. It is low
// so that the refusals pass it within a few of the runtime's sweeps.
const adoptionPromptFailureLimit = 2

// adoptionBootTimeout is the proof's worker_boot_timeout_seconds. The tester's launch is held at
// its start until the third check has the implementer in a turn, after the first two checks, and a
// launch whose shim has not said hello within the boot timeout is retired as one that failed; the
// whole run fits inside this bound with room.
const adoptionBootTimeout = 15 * time.Minute

// adoptionNextSweep is how soon after its cause is gone a task must reach its send: a few of the
// local runtime's two-second sweeps (localObserveInterval), an adoption already running when the
// cause went, and the adoption's own jj commands, on a loaded runner.
const adoptionNextSweep = 20 * time.Second

// adoptionEdits are the edits the stale-adopt check writes in the stale workspace with no jj
// command between: a tracked file changed and a new file.
var adoptionEdits = map[string]string{
	"README.md": "smoke, edited by a role\n",
	"new.txt":   "a file no jj command recorded\n",
}

// The daemon's own lines a step reads (internal/supervise).
const (
	// adoptionAdoptedMessage is an adoption the shim answered ok (supervise's adopt).
	adoptionAdoptedMessage = "supervise: the working copy was adopted for the phase task"
	// adoptionLeftEditsMessage is a stale working copy's update that left edits in a commit, which
	// the task then names (supervise's leftEdits).
	adoptionLeftEditsMessage = "supervise: the adoption brought a stale working copy up to date and left the edits no jj command had recorded in a commit; the task names it and is sent at once"
	// adoptionRefusedMessage is an adoption the shim refused (supervise's refused).
	adoptionRefusedMessage = "supervise: the shim refused to adopt the working copy; the prompt was never sent"
	// adoptionHeldMessage is a phase task held behind another claim of its issue in a turn
	// (supervise's sendPending).
	adoptionHeldMessage = "supervise: a claim of this issue is in a turn; the phase task waits rather than adopt the working copy they share"
	// adoptionResentMessage is the ladder's wait after a provider error over, the task sent again
	// (supervise's promptRetryDue).
	adoptionResentMessage = "supervise: the wait after a provider error is over; the task goes again"
)

// adoptionStaleSentence opens the sentence the daemon adds to a task whose adoption brought a
// stale working copy up to date (supervise's staleEditsSentence), and adoptionEditsCommit reads the
// one commit it names.
const adoptionStaleSentence = "Before this task, the working copy was stale."

var adoptionEditsCommit = regexp.MustCompile(`left the edits that no jj command had recorded in commit ([0-9a-f]{40})`)

// adoptionDeliveryLive is one run of the proof: the booted daemon, the local runtime that started
// each pane, the daemon's own log as this driver reads it back, the three issues it drives, the
// clones it made, and each claim's stand-in.
type adoptionDeliveryLive struct {
	liveAgentProof
	// stale, locked and shared are the three issues, one per check.
	stale, locked, shared string
	// home and profile are the Oh My Pi home and profile every pane runs under. The driver's own
	// jj and git run under that home too, so they read the repository configuration the panes'
	// jj reads.
	home, profile string
	claims        []adoptionClaim
	stands        map[claim.Token]*adoptionStandin
	// fixture runs the driver's own git and jj (fixturegit.Isolated of home); jj and git are the
	// compiled ones of PATH, which it runs in place of a wrapper of the same name.
	fixture fixturegit.Runner
	jj, git string
	// workspaces is each issue's clone and the pane's workspace in it.
	workspaces map[string]adoptionWorkspace
	// staleChange is the change of the stale workspace's working-copy commit, read before the
	// rebase that stales it.
	staleChange string
	// lock is the stale index.lock in the locked workspace's git directory.
	lock string
	// gate is the file the tester's launch waits for (localRuntime.gate).
	gate string
}

// adoptionClaim is one claim the proof seeds: its issue and role, the phase of the task its seed
// leaves pending on it ("" for a warm-up task of no phase), the stand-in its pane talks to, that
// task, and the word the task asks for, which tells the task's requests from every other.
type adoptionClaim struct {
	issue    string
	role     claim.Role
	phase    phase.Phase
	provider string
	task     string
	marker   string
}

// adoptionWorkspace is one issue's clone, a colocated jj repository whose own workspace the
// driver works in, and the pane's workspace in it, a git worktree of the clone at the runtime's
// working directory for the issue, with its jj name.
type adoptionWorkspace struct {
	clone, dir, name string
}

// TestAPhaseTaskReachesItsRoleAcrossTheAdoptionLive drives the three checks in turn: a stale
// working copy brought up to date with the commit of its edits named in the task, a refused
// adoption sent again once its cause is gone, and a phase task sent once another role's turn has
// ended in a provider error.
func TestAPhaseTaskReachesItsRoleAcrossTheAdoptionLive(t *testing.T) {
	// Not parallel: it gives the process back the operator's own environment, as t.Setenv does (fixtureenv.Live).
	fixtureenv.Live(t)
	r := newAdoptionDeliveryLive(t)
	r.step("stale-adopt", r.staleAdopt)
	r.step("refused-adoption-resent", r.refusedAdoptionResent)
	r.step("ended-turn-holds-nothing", r.endedTurnHoldsNothing)
}

// newAdoptionDeliveryLive readies everything a pane needs before the daemon boots: the `legion`
// binary and the two plugins built from this checkout, an Oh My Pi profile of the run's own routed
// to the four stand-ins, a clone and a workspace per issue, the stale workspace and the stale
// lock, the three issues seeded with their claims mid-launch, and the daemon booted over the local
// runtime with the proof's own prompt failure limit and boot timeout.
func newAdoptionDeliveryLive(t *testing.T) *adoptionDeliveryLive {
	t.Helper()
	omp := os.Getenv(liveOmpPathEnv)
	if omp == "" {
		t.Skipf("%s is unset, so there is no Oh My Pi for a pane to run; scripts/e2e/adoption-delivery.sh requires it", liveOmpPathEnv)
	}
	proof := newLiveProof(t, "scripts/e2e/adoption-delivery.sh")
	proof.cfg.PromptFailureLimit = adoptionPromptFailureLimit
	proof.cfg.WorkerBootTimeout = adoptionBootTimeout
	r := &adoptionDeliveryLive{
		liveAgentProof: liveAgentProof{liveProof: proof, omp: omp},
		stale:          proof.cfg.Project + "-1", locked: proof.cfg.Project + "-2", shared: proof.cfg.Project + "-3",
		stands: map[claim.Token]*adoptionStandin{}, workspaces: map[string]adoptionWorkspace{},
	}
	r.claims = []adoptionClaim{
		{issue: r.stale, role: claim.RoleImplementer, phase: phase.Implementing, provider: adoptStaleProvider, marker: "STALE-" + randomSuffix(t)},
		{issue: r.locked, role: claim.RoleImplementer, phase: phase.Implementing, provider: adoptLockedProvider, marker: "LOCKED-" + randomSuffix(t)},
		{issue: r.shared, role: claim.RoleImplementer, provider: adoptEndingProvider, marker: "WARM-IMPLEMENTER-" + randomSuffix(t)},
		{issue: r.shared, role: claim.RoleTester, phase: phase.Testing, provider: adoptWaitingProvider, marker: "TESTED-" + randomSuffix(t)},
	}
	for i, seeded := range r.claims {
		r.claims[i].task = "Say the word " + seeded.marker + " and nothing else."
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
	r.profile = "legion-adoption-delivery-" + strings.ToLower(r.cfg.Project)
	installProofPlugins(t, root, work, r.home, r.profile, omp)
	for _, seeded := range r.claims {
		r.stands[r.token(seeded.issue, seeded.role)] = newAdoptionStandin(t, seeded.provider)
	}
	r.writeOmpProfile()
	overlays := r.writeOverlays(work)

	r.rt = newLocalRuntime(t, r.cfg.StateDir, r.legion, omp, r.home, r.profile)
	r.rt.daemonURL, r.rt.envoyURL, r.rt.natsURLs = r.cfg.DaemonURL, newPaneListener(t), r.cfg.NatsURLs
	r.rt.overlays = func(token claim.Token) []string { return overlays[token] }
	// The adoption's bound is the one the tmux runtime takes (slow_command_timeout_seconds).
	r.rt.adoptWithin = r.cfg.SlowCommandTimeout
	tester := r.token(r.shared, claim.RoleTester)
	r.gate = filepath.Join(work, "tester-launch-gate")
	r.rt.gate = func(token claim.Token) string {
		if token == tester {
			return r.gate
		}
		return ""
	}

	r.makeWorkspaces(work)
	r.staleTheWorkspace()
	r.lockTheWorkspace()
	r.seed()
	r.daemon = newRecordingLog(r.log())
	r.d = startDaemonLogging(t, r.cfg, r.overrides(), slog.New(r.daemon))
	return r
}

// overrides is the daemon this proof boots: the local runtime's own wiring (daemonOverrides), and
// the workflow's App tokens stood in for by leases whose identity carries an email, which the
// adoption's frame needs (idleRigAppTokens). The clock is the real one.
func (r *adoptionDeliveryLive) overrides() overrides {
	o := r.rt.daemonOverrides(r.rt)
	o.workflowTokens = idleRigAppTokens{workflowTokenRecorder: &workflowTokenRecorder{}}
	return o
}

// token is the claim token of role on issue, as the daemon spells it.
func (r *adoptionDeliveryLive) token(issue string, role claim.Role) claim.Token {
	r.t.Helper()
	token, err := claim.NewToken(r.project, issue, role)
	if err != nil {
		r.t.Fatal(err)
	}
	return token
}

// seededFor is the seeded claim of role on issue, and fails the run when the proof seeds none.
func (r *adoptionDeliveryLive) seededFor(issue string, role claim.Role) adoptionClaim {
	r.t.Helper()
	for _, seeded := range r.claims {
		if seeded.issue == issue && seeded.role == role {
			return seeded
		}
	}
	r.t.Fatalf("this proof seeds no %s on %s", role, issue)
	return adoptionClaim{}
}

// ---- the clones and the workspaces -------------------------------------------------------------

// tool is argv with a leading git or jj replaced by the compiled one of PATH: under
// fixtureenv.Live a Legion pod's PATH leads with the image's git and jj wrappers.
func (r *adoptionDeliveryLive) tool(argv []string) []string {
	switch argv[0] {
	case "git":
		return append([]string{r.git}, argv[1:]...)
	case "jj":
		return append([]string{r.jj}, argv[1:]...)
	}
	return argv
}

// run runs one of the driver's git or jj commands in dir and answers its standard output, trimmed,
// failing the run when it fails.
func (r *adoptionDeliveryLive) run(dir string, argv ...string) string {
	r.t.Helper()
	return strings.TrimSpace(r.fixture.Out(dir, r.tool(argv)...))
}

// try runs one of them for a caller that reads its failure: its combined output and its error.
func (r *adoptionDeliveryLive) try(dir string, argv ...string) (string, error) {
	r.t.Helper()
	return r.fixture.Try(dir, r.tool(argv)...)
}

// makeWorkspaces makes the run's origin, a bare repository holding one commit on main, and for
// each issue a clone of it, made a colocated jj repository, with the pane's workspace added at
// the runtime's working directory for the issue, as `jj workspace add` adds a pod's: a git
// worktree of the clone, on main.
func (r *adoptionDeliveryLive) makeWorkspaces(work string) {
	t := r.t
	t.Helper()
	r.jj, r.git = fixturegit.CompiledJJ(t), fixturegit.CompiledGit(t)
	r.fixture = fixturegit.Isolated(t, r.home)
	origin, seed := filepath.Join(work, "origin.git"), filepath.Join(work, "seed")
	r.run(work, fixturegit.InitBare(origin, "main")...)
	r.run(work, fixturegit.InitWorkTree(seed, "main")...)
	writeProofFile(t, filepath.Join(seed, "README.md"), "smoke\n")
	// The runtime gives each pane a TMPDIR inside its working directory (paneEnvironment), which
	// no snapshot of the workspace should take in.
	writeProofFile(t, filepath.Join(seed, ".gitignore"), "/tmp/\n")
	r.run(seed, "git", "add", "README.md", ".gitignore")
	r.run(seed, "git", "commit", "--quiet", "-m", "the default branch")
	r.run(seed, "git", "push", "--quiet", origin, "main")
	for _, issue := range []string{r.stale, r.locked, r.shared} {
		w := adoptionWorkspace{clone: filepath.Join(work, "clones", issue), dir: r.rt.workDir(issue), name: strings.ToLower(issue)}
		r.run(work, "git", "clone", "--quiet", origin, w.clone)
		r.run(w.clone, fixturegit.JJInit("", true)...)
		if err := os.MkdirAll(filepath.Dir(w.dir), 0o700); err != nil {
			t.Fatalf("make the parent of %s: %v", w.dir, err)
		}
		r.run(w.clone, "jj", "workspace", "add", w.dir, "--name", w.name, "-r", "main")
		r.workspaces[issue] = w
	}
	r.note("made a clone of the run's origin per issue, each a colocated jj repository, with the pane's workspace added to it as a git worktree "+
		"(%s for %s, %s for %s, %s for %s); jj %s", r.workspaces[r.stale].dir, r.stale, r.workspaces[r.locked].dir, r.locked,
		r.workspaces[r.shared].dir, r.shared, r.run(work, "jj", "--version"))
}

// staleTheWorkspace makes the stale-adopt implementer's workspace stale as another role of the
// issue does: the clone's own workspace describes a change to a file and rebases the workspace's
// working-copy commit onto it. jj then refuses every ordinary command in that workspace, which the
// driver reads once before the edits. Then a tracked file is changed and a new file written there,
// with no jj command between them and the adoption.
func (r *adoptionDeliveryLive) staleTheWorkspace() {
	t := r.t
	t.Helper()
	w := r.workspaces[r.stale]
	r.staleChange = r.run(w.clone, "jj", "--ignore-working-copy", "log", "-r", w.name+"@", "--no-graph", "-T", "change_id")
	writeProofFile(t, filepath.Join(w.clone, "other.txt"), "another role's file\n")
	r.run(w.clone, "jj", "describe", "-m", "another role's change")
	onto := r.run(w.clone, "jj", "--ignore-working-copy", "log", "-r", "@", "--no-graph", "-T", "commit_id")
	r.run(w.clone, "jj", "rebase", "-r", w.name+"@", "-d", onto)
	if status, err := r.try(w.dir, "jj", "status"); err == nil || !strings.Contains(status, "The working copy is stale") {
		t.Fatalf("jj status in %s after the rebase ended %v and printed:\n%s\nwant the stale working copy every ordinary command meets", w.dir, err, status)
	}
	for name, body := range adoptionEdits {
		writeProofFile(t, filepath.Join(w.dir, name), body)
	}
	r.note("rebased %s's working-copy commit (change %s) onto %s from the clone's own workspace, so jj status there says the working copy is stale; "+
		"then changed README.md and wrote new.txt in it with no jj command between", w.dir, r.staleChange, onto)
}

// lockTheWorkspace leaves a stale index.lock in the git directory of the refused-adoption-resent
// implementer's workspace, which is a worktree's, as a git command cut off at its step limit
// leaves one. The workspace's @ is undescribed and authored by the driver, so the adoption's
// metaedit changes the author and writes an operation, and jj's reset of git's HEAD state after
// it is what the lock fails.
func (r *adoptionDeliveryLive) lockTheWorkspace() {
	t := r.t
	t.Helper()
	w := r.workspaces[r.locked]
	gitDir := r.run(w.dir, "git", "rev-parse", "--path-format=absolute", "--git-dir")
	r.lock = filepath.Join(gitDir, "index.lock")
	writeProofFile(t, r.lock, "")
	at := r.run(w.dir, "jj", "--ignore-working-copy", "log", "-r", "@", "--no-graph", "-T",
		`if(description, "described", "undescribed") ++ " " ++ author.name()`)
	if !strings.HasPrefix(at, "undescribed ") || strings.HasSuffix(at, "[bot]") {
		t.Fatalf("%s's @ is %q, want an undescribed commit the driver authored, whose author the adoption changes", w.dir, at)
	}
	r.note("left a stale %s in %s's git directory; its @ is %s", r.lock, w.dir, at)
}

// ---- the seed ----------------------------------------------------------------------------------

// seed records the three issues as the daemon would hold them at the moment their workers began:
// each a root handed to Legion, in the phase it stands in, holding its tree's admission slot with
// its lifecycle open, each of its claims mid-launch, which this boot relaunches, and the task each
// claim's start left pending on it. The phase task of a phase worker is of the issue's own
// generation and phase, which is what admission's repair of a slotted worker passes over
// (workflow.StartFor, the delivery's StillWorked); the warm-up task of the implementer of the
// issue in testing names no phase and no run, as an operator's task does.
func (r *adoptionDeliveryLive) seed() {
	t := r.t
	t.Helper()
	ctx := context.Background()
	issues := []record.Issue{
		{Key: r.stale, Title: "the issue whose implementer's workspace another workspace made stale", Phase: phase.Implementing},
		{Key: r.locked, Title: "the issue whose implementer's workspace holds a stale git index.lock", Phase: phase.Implementing},
		{Key: r.shared, Title: "the issue whose tester's phase task waits for the implementer's turn", Phase: phase.Testing},
	}
	keys := make([]string, 0, len(issues))
	for _, issue := range issues {
		r.board.show(issue.Key, "in_progress")
		keys = append(keys, issue.Key)
	}
	if err := store.Transact(ctx, r.st.Pool(), func(ctx context.Context, tx pgx.Tx) error {
		for _, issue := range issues {
			issue.Repo, issue.DefaultBranch, issue.GitHubRepo, issue.Tree, issue.Project = liveRepo, "main", liveRepo, issue.Key, r.cfg.Project
			issue.Generation, issue.Status, issue.DispatchStatus, issue.Rank, issue.LastDispatchSeq, issue.HandedOver = 1, "in_progress", "in_progress", "U", 1, true
			if err := r.records.PutIssue(ctx, tx, issue); err != nil {
				return err
			}
		}
		return r.admit(ctx, tx, keys...)
	}); err != nil {
		t.Fatalf("seed the issues: %v", err)
	}
	for _, key := range keys {
		if _, err := r.st.OpenTreeLifecycle(ctx, r.project, key, treelifecycle.AuthorityWorkflow); err != nil {
			t.Fatalf("open the lifecycle of %s: %v", key, err)
		}
	}
	for _, seeded := range r.claims {
		token := r.token(seeded.issue, seeded.role)
		if _, err := r.st.AdmitClaim(ctx, supervise.Claim{
			Token: token, Project: r.project, Tree: seeded.issue, Issue: seeded.issue, Role: seeded.role,
			Generation: 1, State: supervise.StateLaunching,
			BootTokenHash: supervise.HashBootToken("interrupted-" + randomSuffix(t)), ServingGeneration: 1,
		}); err != nil {
			t.Fatalf("record the unfinished launch of %s: %v", token, err)
		}
		// Unconfirmed and unread: the launch this seed interrupts never put the task in front of
		// an agent, so the relaunched pane is sent it once it reports ready.
		run := uint64(0)
		if seeded.phase != "" {
			run = 1
		}
		if err := r.st.PutDelivery(ctx, token, supervise.Delivery{
			ID: "seeded-start-" + randomSuffix(t), Task: seeded.task, Generation: run,
			Phase: seeded.phase, QueuedAt: time.Now(),
		}); err != nil {
			t.Fatalf("record the task %s's start left pending: %v", token, err)
		}
		writePrompt(t, r.cfg, token)
	}
	r.note("seeded %s and %s in %s, each with its implementer mid-launch holding that phase's task, and %s in %s with its tester mid-launch "+
		"holding that phase's task, its launch held at its start, and its implementer mid-launch holding a warm-up task of no phase",
		r.stale, r.locked, phase.Implementing, r.shared, phase.Testing)
}

// ---- the checks --------------------------------------------------------------------------------

// staleAdopt is R1 on a stale working copy: the phase task the stale-adopt implementer's seed left
// pending reaches its stand-in naming the commit that holds the edits the update took off the
// disk, the workspace is fresh for the role's next jj command, and both edits are in that commit.
func (r *adoptionDeliveryLive) staleAdopt() {
	t := r.t
	seeded := r.seededFor(r.stale, claim.RoleImplementer)
	token := r.token(seeded.issue, seeded.role)
	w := r.workspaces[r.stale]
	stand := r.stands[token]
	r.awaitReady(token)
	var prompt string
	r.await(string(token)+"'s stand-in to receive the phase task naming the commit of the edits", paneReady, func() bool {
		for _, body := range stand.bodiesHolding(seeded.marker) {
			if strings.Contains(body, adoptionStaleSentence) && adoptionEditsCommit.MatchString(body) {
				prompt = body
				return true
			}
		}
		return false
	})
	named := adoptionEditsCommit.FindStringSubmatch(prompt)[1]
	r.note("the stand-in received the phase task, which says %q and names commit %s", adoptionStaleSentence, named)

	left := r.lastLog(func(record loggedRecord) bool {
		return record.Message == adoptionLeftEditsMessage && record.Attrs["claim"] == string(token)
	})
	if left == nil {
		t.Fatalf("the daemon wrote no line that the adoption of %s left the edits in a commit", token)
	}
	if logged := strings.Fields(strings.Trim(fmt.Sprint(left.Attrs["editsCommits"]), "[]")); len(logged) != 1 || logged[0] != named {
		t.Fatalf("the daemon's line names the commits %v, want the one the task names, %s", logged, named)
	}
	held := strings.Fields(r.run(w.dir, "jj", "--ignore-working-copy", "log", "-r", "change_id("+r.staleChange+") ~ @", "--no-graph", "-T", `commit_id ++ "\n"`))
	if len(held) != 1 || held[0] != named {
		t.Fatalf("jj log of change %s's commits that are not @ reads %v, want the one commit the task names, %s", r.staleChange, held, named)
	}
	r.note("the daemon's own line and jj log of change %s's commits that are not @ each name that one commit", r.staleChange)
	if shim := r.paneLog(token); !strings.Contains(shim, "left the edits that no jj command had recorded in commit "+named) {
		t.Fatalf("the shim's log of %s names no commit %s:\n%s", token, named, shim)
	}
	r.note("the shim's own log line names it too")

	for name, body := range adoptionEdits {
		if got, err := os.ReadFile(filepath.Join(w.dir, name)); err == nil && string(got) == body {
			t.Fatalf("%s still holds the edit %q on disk after the update, want it only in commit %s", name, body, named)
		}
	}
	status, err := r.try(w.dir, "jj", "status")
	if err != nil || strings.Contains(strings.ToLower(status), "stale") {
		t.Fatalf("jj status in %s after the adoption ended %v and printed:\n%s\nwant exit 0 and no stale working copy", w.dir, err, status)
	}
	r.note("the edits are off the disk, and jj status in the workspace exits 0 and names no stale working copy:\n%s", indentOutput(strings.TrimSpace(status)))
	for name, body := range adoptionEdits {
		if got := r.fixture.Out(w.dir, r.tool([]string{"jj", "--ignore-working-copy", "file", "show", "-r", named, "--", name})...); got != body {
			t.Fatalf("commit %s holds %s as %q, want the edit %q", named, name, got, body)
		}
	}
	r.note("commit %s holds both edits: README.md changed and new.txt written", named)
}

// refusedAdoptionResent is R3 and R7 on a refused adoption: the stale lock refuses every send of
// the phase task, more times than prompt_failure_limit, the claim is kept and charged nothing, the
// issue's architect is told once, and once the lock is removed the daemon's own next sweep sends
// the task.
func (r *adoptionDeliveryLive) refusedAdoptionResent() {
	t := r.t
	seeded := r.seededFor(r.locked, claim.RoleImplementer)
	token := r.token(seeded.issue, seeded.role)
	stand := r.stands[token]
	r.awaitReady(token)
	limit := r.cfg.PromptFailureLimit
	var held api.OperatorClaim
	r.await(fmt.Sprintf("%s's phase task refused more than %d times", token, limit), 3*time.Minute, func() bool {
		held = r.claim(token)
		return held.Pending != nil && held.Pending.SendFailures > limit
	})
	if !strings.Contains(held.Pending.Task, seeded.marker) {
		t.Fatalf("%s holds pending %q, want the phase task its seed left", token, held.Pending.Task)
	}
	if !strings.Contains(held.Pending.LastSendError, "Failed to reset Git HEAD state") {
		t.Fatalf("%s's last refusal is %q, want the metaedit's reset of git's HEAD state the lock fails", token, held.Pending.LastSendError)
	}
	r.note("the shim refused %d sends of the phase task (prompt_failure_limit %d); the last refusal: %s",
		held.Pending.SendFailures, limit, firstLine(held.Pending.LastSendError))
	launches := r.rt.LaunchesOf(token)
	switch {
	case held.State != string(supervise.StateReady) && held.State != string(supervise.StateIdle):
		t.Fatalf("%s is %s after the refusals, want it ready or idle", token, held.State)
	case len(launches) != 1 || held.Generation != launches[0].Generation:
		t.Fatalf("%s is at generation %d after %d launches, want the one launch's generation", token, held.Generation, len(launches))
	case held.Budgets.PromptFailures != 0 || held.Budgets.PromptRetires != 0:
		t.Fatalf("%s was charged %+v for answered refusals, want nothing", token, held.Budgets)
	}
	if got := stand.requestsHolding(seeded.marker); got != 0 {
		t.Fatalf("the %s stand-in received %d requests carrying the task while the lock stood", seeded.provider, got)
	}
	r.note("%s is %s at generation %d, launched once, with its prompt failures and retirements at %d and %d",
		token, held.State, held.Generation, held.Budgets.PromptFailures, held.Budgets.PromptRetires)
	notices := r.waitArchitectNotices(r.locked, "worker-stalled", 1)
	if reason := notices[0].Payload; !strings.Contains(reason, string(token)) || !strings.Contains(reason, "did not get its task") ||
		!strings.Contains(reason, fmt.Sprintf("%d sends in a row failed with an answer", limit)) {
		t.Fatalf("%s's architect was told %q, want the task that kept failing named with its claim and the count %d", r.locked, reason, limit)
	}
	r.note("%s's architect was told once: %q", r.locked, notices[0].Payload)

	from := r.daemon.count()
	removed := time.Now()
	if err := os.Remove(r.lock); err != nil {
		t.Fatalf("remove the stale lock %s: %v", r.lock, err)
	}
	adopted := r.awaitLogFrom(string(token)+"'s adoption once the lock is gone", from, 2*time.Minute, func(record loggedRecord) bool {
		return record.Message == adoptionAdoptedMessage && record.Attrs["claim"] == string(token)
	})
	if gap := adopted.at.Sub(removed); gap > adoptionNextSweep {
		t.Fatalf("the adoption of %s came %s after the lock was removed, want it at the next sweep, within %s", token, gap.Round(time.Millisecond), adoptionNextSweep)
	}
	refusedSince := r.logs(func(record loggedRecord) bool {
		return record.Message == adoptionRefusedMessage && record.Attrs["claim"] == string(token) &&
			record.at.After(removed) && record.at.Before(adopted.at)
	})
	if len(refusedSince) > 1 {
		t.Fatalf("the shim refused %d sends of %s's task after the lock was removed, want at most the one already running", len(refusedSince), token)
	}
	r.await("the "+seeded.provider+" stand-in to receive the phase task", paneReady, func() bool {
		return stand.requestsHolding(seeded.marker) > 0
	})
	after := r.claim(token)
	if after.Generation != held.Generation || len(r.rt.LaunchesOf(token)) != 1 || after.Budgets.PromptRetires != 0 {
		t.Fatalf("%s is at generation %d after %d launches with %d retirements once the task went, want generation %d, one launch and none",
			token, after.Generation, len(r.rt.LaunchesOf(token)), after.Budgets.PromptRetires, held.Generation)
	}
	if got := len(r.architectNotices(r.locked, "worker-stalled")); got != 1 {
		t.Fatalf("%s's architect was told %d worker-stalled notices, want one", r.locked, got)
	}
	r.note("the lock removed, the daemon's own sweep adopted the working copy %s later and the task reached the stand-in, "+
		"with no message and no operator call: the driver sent the claim nothing; %s is still at generation %d, launched once",
		adopted.at.Sub(removed).Round(time.Millisecond), token, after.Generation)
}

// endedTurnHoldsNothing is R6 on a turn that ends in a provider error: while the implementer of the
// issue in testing is in a turn, the tester's phase task waits rather than adopt the working copy
// the two share; once that turn ends in a 529, the implementer's own task waits on the ladder's
// backoff and holds nothing, and the tester's prompt reaches its stand-in at the next sweep.
//
// The implementer's request is held from the deliver until the tester's pane has reported ready
// and its task is held, which paneReady bounds at three minutes: inside the 300 s Oh My Pi waits
// by default for a stream's first event (PI_STREAM_FIRST_EVENT_TIMEOUT_MS, read in the fleet's
// 18.x build), so the turn is still running when the step releases it.
func (r *adoptionDeliveryLive) endedTurnHoldsNothing() {
	t := r.t
	implementer, tester := r.token(r.shared, claim.RoleImplementer), r.token(r.shared, claim.RoleTester)
	warm, waiting := r.seededFor(r.shared, claim.RoleImplementer), r.seededFor(r.shared, claim.RoleTester)
	ending, waiter := r.stands[implementer], r.stands[tester]
	r.awaitReady(implementer)
	r.await(string(implementer)+" to answer its warm-up task", paneReady, func() bool {
		held := r.claim(implementer)
		return held.Pending == nil && held.State != string(supervise.StateWorking)
	})
	if ending.requestsHolding(warm.marker) == 0 {
		t.Fatalf("the %s stand-in answered no request carrying %s's warm-up task", adoptEndingProvider, implementer)
	}
	if state := r.claim(tester).State; state != string(supervise.StateLaunching) {
		t.Fatalf("%s is %s while its launch is held at its start, want it %s", tester, state, supervise.StateLaunching)
	}
	r.note("%s answered its warm-up task and holds nothing; %s's launch is held at its start", implementer, tester)

	marker := "ENDING-" + randomSuffix(t)
	ending.holdTurn(marker)
	status, body := r.d.request(http.MethodPost, "/legion/v1/operator/claims/"+string(implementer)+"/deliver",
		api.DeliverRequest{Task: "Say the word " + marker + " and nothing else."}, true)
	if status != http.StatusOK {
		t.Fatalf("deliver %s's task = %d %s, want 200", implementer, status, body)
	}
	r.await(string(implementer)+"'s turn to run with its request held at its stand-in", 2*time.Minute, func() bool {
		return r.claim(implementer).State == string(supervise.StateWorking) && ending.heldTurns() > 0
	})
	r.note("%s is in a turn whose request its stand-in holds", implementer)

	from := r.daemon.count()
	writeProofFile(t, r.gate, "")
	r.awaitReady(tester)
	r.awaitLogFrom(string(tester)+"'s phase task held behind "+string(implementer)+"'s turn", from, paneReady, func(record loggedRecord) bool {
		return record.Message == adoptionHeldMessage && record.Attrs["claim"] == string(tester) && record.Attrs["inTurn"] == string(implementer)
	})
	if got := waiter.requestsHolding(waiting.marker); got != 0 {
		t.Fatalf("the %s stand-in received %d requests carrying the tester's phase task while the implementer's turn ran", adoptWaitingProvider, got)
	}
	if state := r.claim(implementer).State; state != string(supervise.StateWorking) {
		t.Fatalf("%s is %s while its stand-in holds its request, want it %s", implementer, state, supervise.StateWorking)
	}
	r.note("%s reported ready and its phase task waits behind %s's turn rather than adopt the working copy they share", tester, implementer)

	ended := r.daemon.count()
	released := time.Now()
	ending.releaseTurn()
	errored := r.awaitLogFrom(string(implementer)+"'s turn ending in a provider error", ended, 2*time.Minute, func(record loggedRecord) bool {
		return record.Message == providerErroredMessage && record.Attrs["claim"] == string(implementer)
	})
	if errored.Attrs["httpStatus"] != float64(529) {
		t.Fatalf("%s's turn ended with status %v, want the stand-in's 529", implementer, errored.Attrs["httpStatus"])
	}
	adopted := r.awaitLogFrom(string(tester)+"'s phase task sent", ended, ladderDelays[0]+time.Minute, func(record loggedRecord) bool {
		return record.Message == adoptionAdoptedMessage && record.Attrs["claim"] == string(tester)
	})
	// From the release, since the turn's end and the prompt's result that names the provider
	// error reach the daemon as two frames, and the turn's end alone is what lets the task go.
	gap := adopted.at.Sub(released)
	if gap > adoptionNextSweep {
		t.Fatalf("%s's phase task went %s after %s's turn was released into its provider error, want it at the next sweep, within %s",
			tester, gap.Round(time.Millisecond), implementer, adoptionNextSweep)
	}
	if resent := r.daemon.firstAfter(ended, func(record loggedRecord) bool {
		return record.Message == adoptionResentMessage && record.Attrs["claim"] == string(implementer)
	}); resent != nil && !resent.at.After(adopted.at) {
		t.Fatalf("%s's phase task went only after %s's task was sent again, want it while that task waited on the ladder", tester, implementer)
	}
	r.await("the "+adoptWaitingProvider+" stand-in to receive the tester's phase task", paneReady, func() bool {
		return waiter.requestsHolding(waiting.marker) > 0
	})
	r.note("%s's turn ended in a %v and its task waits out the ladder's %s; %s's phase task was adopted %s after the release, at the next sweep, and its prompt reached its stand-in",
		implementer, errored.Attrs["httpStatus"], ladderDelays[0], tester, gap.Round(time.Millisecond))
}

// paneLog is everything token's panes have written, every launch's log in turn (localRuntime's
// paneLogs).
func (r *adoptionDeliveryLive) paneLog(token claim.Token) string {
	r.t.Helper()
	entries, err := os.ReadDir(r.rt.paneLogs)
	if err != nil {
		r.t.Fatalf("read the pane logs under %s: %v", r.rt.paneLogs, err)
	}
	var logs strings.Builder
	for _, entry := range entries {
		if !strings.HasPrefix(entry.Name(), string(token)+".") {
			continue
		}
		body, err := os.ReadFile(filepath.Join(r.rt.paneLogs, entry.Name()))
		if err != nil {
			r.t.Fatalf("read the pane log %s: %v", entry.Name(), err)
		}
		logs.Write(body)
	}
	return logs.String()
}

// ---- the Oh My Pi profile and the overlays ----------------------------------------------------

// writeOmpProfile writes the profile every pane shares: the four stand-in providers, one model
// each, and the settings a pane needs to run unattended.
func (r *adoptionDeliveryLive) writeOmpProfile() {
	t := r.t
	t.Helper()
	agent := filepath.Join(r.home, ".omp", "profiles", r.profile, "agent")
	if err := os.MkdirAll(agent, 0o700); err != nil {
		t.Fatalf("make the profile's agent directory %s: %v", agent, err)
	}
	var models, enabled strings.Builder
	models.WriteString("# Written by packages/daemon/internal/daemon/adoption_delivery_live_test.go.\nproviders:\n")
	for _, seeded := range r.claims {
		stand := r.stands[r.token(seeded.issue, seeded.role)]
		fmt.Fprintf(&models, "  %s:\n    baseUrl: %s\n    api: anthropic-messages\n    apiKey: a-literal-key-no-endpoint-checks\n"+
			"    models:\n      - id: %s\n        name: %s\n        contextWindow: 200000\n        maxTokens: 8192\n",
			seeded.provider, stand.url, seeded.provider+standinModelID, seeded.provider)
		fmt.Fprintf(&enabled, "  - %s\n", standinModel(seeded.provider))
	}
	writeProofFile(t, filepath.Join(agent, "models.yml"), models.String())
	writeProofFile(t, filepath.Join(agent, "config.yml"),
		"# Written by packages/daemon/internal/daemon/adoption_delivery_live_test.go.\n"+
			"enabledModels:\n"+enabled.String()+
			// A fresh profile opens Oh My Pi's setup wizard, and a pane waiting on it never starts.
			"startup:\n  setupWizard: false\n")
}

// writeOverlays writes the Oh My Pi overlays each pane is told through PI_CONFIG_FILES: the
// operator's own, which every pane names first and which turns `retry.enabled` off so the host
// walks no fallback chain of its own and a failed turn reaches the daemon, and one per claim naming
// the single model that claim's stand-in serves (writeRoleOverlay).
func (r *adoptionDeliveryLive) writeOverlays(work string) map[claim.Token][]string {
	t := r.t
	t.Helper()
	dir := filepath.Join(work, "overlays")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	operator := filepath.Join(dir, "operator.yml")
	writeProofFile(t, operator, "# The operator's pane overlay (runtime.tmux.pi_config_files).\nretry:\n  enabled: false\n")
	overlays := map[claim.Token][]string{}
	for _, seeded := range r.claims {
		path := filepath.Join(dir, seeded.issue+"-"+string(seeded.role)+".yml")
		writeRoleOverlay(t, path, standinModel(seeded.provider))
		overlays[r.token(seeded.issue, seeded.role)] = []string{operator, path}
	}
	return overlays
}

// ---- the model stand-ins -----------------------------------------------------------------------

// adoptionStandin is one pane's provider endpoint, over the stand-in every agent proof shares
// (live_agent_proof_test.go), which records each request: it answers every turn with text, and the
// plugin's own side question as the plugins' pane harness answers it (isSideQuestion), until a step
// holds the turns that carry a word (holdTurn). Each request carrying that word then waits, unanswered,
// until the step releases them (releaseTurn), and is answered 529 with a retry-after over Oh My
// Pi's own cap (writeOverloaded), so the turn ends in a provider error and no retry of the host's
// hides it.
type adoptionStandin struct {
	*modelStandin

	mu      sync.Mutex
	hold    string
	release chan struct{}
	held    int
}

// newAdoptionStandin starts the stand-in named name, and releases whatever it still holds when the
// test ends, before its server closes: a server's Close waits for every request it is serving.
func newAdoptionStandin(t *testing.T, name string) *adoptionStandin {
	t.Helper()
	s := &adoptionStandin{modelStandin: &modelStandin{name: name}}
	s.serve(t, s)
	t.Cleanup(s.releaseTurn)
	return s
}

func (s *adoptionStandin) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	body, ok := s.take(w, req)
	if !ok {
		return
	}
	// Only a Messages request is a turn. Oh My Pi asks the same base for a token count of its
	// own, which is answered a count.
	if !strings.HasSuffix(req.URL.Path, "/v1/messages") {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"input_tokens":1000}`)
		return
	}
	if isSideQuestion(body) {
		streamAnthropicText(w, s.name, "PROCEEDING")
		return
	}
	if release := s.holding(body); release != nil {
		select {
		case <-release:
			writeOverloaded(w)
		case <-req.Context().Done():
		}
		return
	}
	streamAnthropicText(w, s.name, "ANSWERED")
}

// holdTurn holds every later request carrying marker until releaseTurn.
func (s *adoptionStandin) holdTurn(marker string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.hold, s.release = marker, make(chan struct{})
}

// holding is the channel a request's answer waits on when the request carries the held word, and
// nil when it does not.
func (s *adoptionStandin) holding(body string) chan struct{} {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.release == nil || !strings.Contains(body, s.hold) {
		return nil
	}
	s.held++
	return s.release
}

// releaseTurn answers every held request 529, and answers every request after it with text.
func (s *adoptionStandin) releaseTurn() {
	s.mu.Lock()
	release := s.release
	s.hold, s.release = "", nil
	s.mu.Unlock()
	if release != nil {
		close(release)
	}
}

// heldTurns is how many requests the stand-in has held so far.
func (s *adoptionStandin) heldTurns() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.held
}

// bodiesHolding is every recorded request body that carries marker, oldest first.
func (s *adoptionStandin) bodiesHolding(marker string) []string {
	s.modelStandin.mu.Lock()
	defer s.modelStandin.mu.Unlock()
	var bodies []string
	for _, body := range s.requests {
		if strings.Contains(body, marker) {
			bodies = append(bodies, body)
		}
	}
	return bodies
}
