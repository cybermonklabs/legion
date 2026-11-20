// The live proof of the Claude Code backend on tmux: an implementer that `worker_backends` puts on
// claude runs as `legion claude-bridge -- claude -p …` in a pane of the real tmux runtime, under
// the real `legion worker-shim`, with this checkout's claude-envoy plugin, against the whole
// daemon. It holds that the implementer registers its Claude Code transcript as its session; that
// a turn of its which ends with its phase still open is reminded by Claude Code's own `Stop` hook
// (the `legion` MCP server's legion_phase_hook, packages/claude-envoy/src/legion-phase-hook.ts),
// which Claude Code carries into one more turn of the same session; that this turn completes the
// phase through the `legion` MCP tool's handoff_complete, naming the implement record it wrote,
// at the head its workspace pushed, and the daemon records that record and moves the issue on;
// and that, its pane killed, the implementer comes back with `claude --resume` into the same
// session, its transcript's earlier bytes unchanged and its earlier turns in the context it sends.
// CI's agent-harness job runs it by name in its tmux-scripts part
// (.github/workflows/ci-job-agent-harness.yaml), which installs tmux and Claude Code
// (LEGION_TEST_CLAUDE), over the PostgreSQL that job gives it and the package's test NATS server.
// It replaces the devbox script that ran the same path against real models, real GitHub Apps and
// a smoke repository.
//
// Only the implementer is real. The tree's architect and the planner are claims of the package's
// fake runtime: the runtime the daemon is given (claudeBackendRuntime) launches the implementer
// through a real tmux runtime (internal/runtime/tmux, its Claude Code launcher) and every other
// role through runtime/fake, whose launches the driver plays over the worker stream and the
// daemon's routes, as handoff_records_live_test.go plays them. The issue is seeded in planning
// with its planner mid-launch, and the driver's planner completes planning naming a plan record of
// its own session, which is what starts the implementer.
//
// The model is claudeStandin, a Messages endpoint Claude Code reaches through ANTHROPIC_BASE_URL
// (runtime.tmux.pane_env), keyed by a placeholder its claim directory's apiKeyHelper prints. It
// answers the phase's first task with one line of text and no tool, so the turn ends with
// implementing open; the `Stop` hook's reminder is the next turn, and the stand-in answers that
// one with a `tool_use` of `mcp__legion__legion` calling handoff_complete, held until the driver
// has written the implement record; and it answers every other request with a WAITING line. The
// pane runs with `ENABLE_TOOL_SEARCH=false`, so each turn offers the `legion` server's tools by
// name and not behind Claude Code's ToolSearch.
//
// Dispatch is the real server this checkout builds, on a database of its own
// (startHandoffDispatch), which the daemon and the pane both talk to. GitHub is the harness's REST
// stand-in (liveGitHub, on githubtest) answering the repository and the pull request open on the
// issue's branch, which the implementer's completion reads, and the two Apps' tokens are
// workflowTokenRecorder's placeholders, each lease's identity given an email (idleRigAppTokens),
// since shimwire refuses a working-copy adoption whose identity has none. The implementer's
// workspace is a jj repository of the run's own at the place the tmux runtime looks for it
// (workspace.Location), whose `@-` is the implementer's commit, pushed to `legion/<issue>` of a
// local bare origin, which is the push the tool's handoff report holds a completion in
// implementing to. The runtime says it provisions every workspace itself, so the daemon clones
// nothing from GitHub.
package daemon

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/testprereq"

	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/appauth"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/fixturegit"
	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/githubtest"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/runtime/fake"
	"github.com/sjawhar/legion/daemon/internal/runtime/tmux"
	"github.com/sjawhar/legion/daemon/internal/shimwire"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/stream"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/treelifecycle"
	"github.com/sjawhar/legion/daemon/internal/workspace"
)

// claudeLegionTool is the name Claude Code offers the pane's `legion` MCP tool under: the MCP
// server's name (tmux.ClaudeMCPServer) between Claude Code's own prefix and the tool's name.
const claudeLegionTool = "mcp__" + tmux.ClaudeMCPServer + "__legion"

// claudePhaseStallNeedle is the opening of what a worker whose turn ended with its phase still
// open is told (REMINDER in packages/pi-shared/src/phase-stall.ts), which the pane's MCP server
// answers the `Stop` hook with, and which Claude Code writes into the session as `Stop hook
// feedback`. A fragment of that paragraph, so a rewording of the rest breaks nothing here.
const claudePhaseStallNeedle = "Your turn ended with your Legion phase still open"

// claudeStandinModel is the model the implementer's claim directory names; the stand-in answers
// any model alike.
const claudeStandinModel = "standin-model"

// claudeBackendLive is one run of the proof: the booted daemon over the real Dispatch, the one
// issue, the two runtimes, the implementer's workspace and model stand-in, and what one step reads
// for the next.
type claudeBackendLive struct {
	liveAgentProof
	dispatch *handoffDispatch
	tokens   *workflowTokenRecorder
	github   *liveGitHub
	// fake launches every claim but the implementer; panes is the tmux runtime the implementer runs
	// under, set once the daemon builds its runtime; address is the worker stream's.
	fake    *fake.Runtime
	mu      sync.Mutex
	panes   *tmux.Runtime
	address string
	// claude is the Claude Code launch the tmux runtime runs the implementer with, and
	// paneListener the Envoy listener its pane is told.
	claude       tmux.ClaudeLaunch
	paneListener string
	// tmuxDir is TMUX_TMPDIR, where the runtime's private server keeps its socket.
	tmuxDir string
	// key is the proof's issue, title its title, number its pull request's; head is the
	// implementer's commit, which its workspace's `@-` and `legion/<issue>@origin` are.
	key, title, head string
	number           int
	workspace        string
	run              fixturegit.Runner
	model            *claudeStandin
	// session and transcript are the implementer's Claude Code session and its transcript, as
	// its claim registered them; implementRecord is the record step 3 wrote under that session.
	session, transcript string
	implementRecord     handoffRecord
}

// TestTheClaudeCodeImplementerRegistersCompletesThroughItsStopHookAndResumesLive boots the daemon
// over the real Dispatch with the implementer on Claude Code under tmux, has the fake runtime's
// planner complete planning, and drives the implementer from its registration through the
// reminded completion to its resume after its pane is killed.
func TestTheClaudeCodeImplementerRegistersCompletesThroughItsStopHookAndResumesLive(t *testing.T) {
	r := newClaudeBackendLive(t)
	r.step("step1-the-planner-on-the-fake-runtime-completes-planning-and-the-issue-starts-implementing", r.plannerCompletes)
	r.step("step2-the-claude-implementer-registers-its-transcript-as-its-session-under-the-bridge", r.implementerRegisters)
	r.step("step3-a-turn-that-ends-with-the-phase-open-is-carried-on-by-the-stop-hooks-reminder", r.stopHookReminds)
	r.step("step4-the-reminded-turn-completes-implementing-through-the-legion-mcp-tool-and-the-handoff-is-recorded", r.implementerCompletes)
	r.step("step5-a-killed-pane-resumes-the-same-claude-session-with-its-earlier-turns-in-context", r.killedPaneResumes)
}

// newClaudeBackendLive readies everything the pane needs before the daemon boots: the real
// Dispatch with the project, its repository mapping and the issue; the `legion` binary and the
// claude-envoy plugin built from this checkout; Claude Code's model stand-in and its claim
// directory; the implementer's workspace at its pushed head; GitHub's stand-in at that head; the
// store seeded; and the daemon booted over the two runtimes.
func newClaudeBackendLive(t *testing.T) *claudeBackendLive {
	t.Helper()
	claudeBinary := os.Getenv("LEGION_TEST_CLAUDE")
	testprereq.Require(t, claudeBinary != "", agentHarnessJob,
		"LEGION_TEST_CLAUDE names no Claude Code binary, and this proof's implementer runs one")
	for _, tool := range []string{"tmux", "bun", "jj", "git"} {
		_, err := exec.LookPath(tool)
		testprereq.Require(t, err == nil, agentHarnessJob,
			"%s is not on PATH, and the proof runs its implementer in a tmux pane, builds claude-envoy with bun and makes the workspace with jj and git: %v", tool, err)
	}
	bun, _ := exec.LookPath("bun")
	takeOwnDatabase(t)
	proof := newLiveProof(t)
	// A Dispatch project key is at most ten characters (contracts ProjectKeyPattern), and the
	// daemon's project is its Dispatch project, so the run's project is a short one of its own.
	proof.cfg.Project = "CB" + randomSuffix(t)[:6]
	proof.cfg.Projects = map[string]config.Project{proof.cfg.Project: {}}
	// The loader's own default, which workflowConfig leaves at zero: the pane's tool reads GET
	// /legion/v1/state through the daemon API contract, which takes a positive cap alone.
	proof.cfg.ReviewRoundCap = 3
	proof.cfg.WorkerBackends = map[claim.Role]runtime.Backend{claim.RoleImplementer: runtime.BackendClaude}
	var err error
	if proof.project, err = claim.ProjectToken(proof.cfg.Project); err != nil {
		t.Fatal(err)
	}
	number, err := strconv.ParseInt(randomSuffix(t)[:6], 16, 64)
	if err != nil {
		t.Fatal(err)
	}
	r := &claudeBackendLive{
		liveAgentProof: liveAgentProof{liveProof: proof},
		tokens:         &workflowTokenRecorder{},
		fake:           fake.NewRuntime(),
		title:          "the Claude Code implementer completes its phase through its Stop hook",
		number:         int(number) + 1,
		paneListener:   newPaneListener(t),
		tmuxDir:        shortTempDir(t),
	}
	r.fake.InPod = true
	version, err := exec.Command(claudeBinary, "--version").Output()
	if err != nil {
		t.Fatalf("read the Claude Code version of %s: %v", claudeBinary, err)
	}
	r.note("Claude Code %s at %s, the binary LEGION_TEST_CLAUDE names", strings.TrimSpace(string(version)), claudeBinary)

	root := proofCheckoutRoot(t)
	r.dispatch = startHandoffDispatch(t, root)
	if err := os.WriteFile(r.cfg.DispatchTokenFile, []byte(r.dispatch.token+"\n"), 0o600); err != nil {
		t.Fatalf("write the Dispatch token file: %v", err)
	}
	r.cfg.DispatchURL = r.dispatch.base
	r.setUpDispatch()

	work := t.TempDir()
	r.legion = buildProofLegion(t, root, work)
	plugin := filepath.Join(work, "claude-envoy")
	build := exec.Command(bun, filepath.Join(root, "packages", "claude-envoy", "scripts", "build.ts"), "--out", plugin)
	build.Dir = root
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build claude-envoy from this checkout: %v\n%s", err, out)
	}
	r.claude = tmux.ClaudeLaunch{Binary: claudeBinary, PluginDir: plugin, Bun: bun}
	// Boot resolves the Claude Code launch from the daemon's own environment for a configuration
	// whose worker_backends names claude (launchers.go, resolveClaude), as `legion start` does;
	// the runtime this proof builds is handed the same three.
	t.Setenv("LEGION_CLAUDE_PATH", claudeBinary)
	t.Setenv("LEGION_BUN_PATH", bun)
	t.Setenv("LEGION_CLAUDE_PLUGIN_DIR", plugin)
	// The runtime's private tmux server keeps its socket here, a short path of the run's own, and
	// the server is killed when the run ends, which takes every pane with it.
	t.Setenv("TMUX_TMPDIR", r.tmuxDir)
	t.Cleanup(r.killServer)

	r.model = newClaudeStandin(t, "claudepane", "claude-backend-marker-"+randomSuffix(t)[:8])
	r.cfg.Runtime.Tmux = &config.Tmux{PaneEnv: map[string]string{
		"ANTHROPIC_BASE_URL": r.model.url,
		// Behind ANTHROPIC_BASE_URL, Claude Code 2.1.304 and later defer every MCP tool behind
		// ToolSearch: a turn offers ToolSearch and a DeferredToolPlaceholder, and not
		// mcp__legion__legion. The stand-in finds the phase's start and the Stop hook's reminder by
		// the tool the turn offers, so it would answer both with its WAITING line. With this
		// value, every MCP tool is offered with the turn, as the real-hosts rig of
		// packages/envoy-client/test/session-stall.ts offers them.
		"ENABLE_TOOL_SEARCH": "false",
		// Nothing but the turns this proof drives reaches the stand-in, and nothing reaches
		// Anthropic's own hosts.
		"DISABLE_NON_ESSENTIAL_MODEL_CALLS": "1",
		"DISABLE_AUTOUPDATER":               "1",
		"DISABLE_TELEMETRY":                 "1",
		"DISABLE_ERROR_REPORTING":           "1",
	}}
	r.writeClaudeRoute(work)
	r.initWorkspace(work)
	r.serveGitHub()
	r.seed()
	r.daemon = newRecordingLog(r.log())
	r.d = startDaemonWithin(t, r.cfg, r.overrides(slog.New(r.daemon)), slog.New(r.daemon), 2*time.Minute)
	r.note("Dispatch %s (database of its own), project %s mapped to %s, issue %s in planning; the implementer on Claude Code under tmux (socket %s under %s), every other role on the fake runtime",
		r.dispatch.base, r.cfg.Project, liveRepo, r.key, r.socket(), r.tmuxDir)
	return r
}

// setUpDispatch creates the run's project on Dispatch, maps liveRepo to it as its default, and
// creates the issue, in progress and carrying the legion label, as a running daemon leaves it.
func (r *claudeBackendLive) setUpDispatch() {
	t := r.t
	t.Helper()
	r.dispatch.user(http.MethodPost, "/api/v1/projects", map[string]string{"key": r.cfg.Project, "name": "Claude Code backend live proof"}, http.StatusCreated, nil)
	r.dispatch.user(http.MethodPut, "/api/v1/settings/repo-projects/"+liveRepo, map[string]any{"project": r.cfg.Project, "default": true}, http.StatusOK, nil)
	var created struct {
		Key string `json:"key"`
	}
	r.dispatch.user(http.MethodPost, "/api/v1/issues", map[string]string{"project": r.cfg.Project, "title": r.title}, http.StatusCreated, &created)
	if created.Key == "" {
		t.Fatalf("Dispatch created the issue %q with no key", r.title)
	}
	r.dispatch.user(http.MethodPatch, "/api/v1/issues/"+created.Key, map[string]any{"status": "in_progress", "labels": []string{"legion"}}, http.StatusOK, nil)
	r.key = created.Key
}

// token is the claim token of role on the proof's issue, as the daemon spells it.
func (r *claudeBackendLive) token(role claim.Role) claim.Token {
	r.t.Helper()
	token, err := claim.NewToken(r.project, r.key, role)
	if err != nil {
		r.t.Fatal(err)
	}
	return token
}

// writeClaudeRoute gives the implementer's claim its model before the claim exists: the daemon
// makes the claim's Claude Code directory, <state_dir>/claude/<claim token>, only if it is missing,
// and Claude Code reads settings.json there. Its apiKeyHelper prints a placeholder of the run's
// own, so no key reaches the pane's environment or argv.
func (r *claudeBackendLive) writeClaudeRoute(work string) {
	t := r.t
	t.Helper()
	dir := filepath.Join(r.cfg.StateDir, "claude", string(r.token(claim.RoleImplementer)))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("make the implementer's Claude Code directory %s: %v", dir, err)
	}
	helper := filepath.Join(work, "claude-key-command")
	if err := os.WriteFile(helper, []byte("#!/bin/sh\necho claude-backend-standin-key\n"), 0o700); err != nil {
		t.Fatalf("write the implementer's key command: %v", err)
	}
	settings, err := json.Marshal(map[string]string{"apiKeyHelper": helper, "model": claudeStandinModel})
	if err != nil {
		t.Fatal(err)
	}
	writeProofFile(t, filepath.Join(dir, "settings.json"), string(settings))
	r.note("the implementer's Claude Code directory %s names %s and the run's key command", dir, claudeStandinModel)
}

// initWorkspace makes the implementer's workspace where the tmux runtime looks for it
// (workspace.Location): a repository made with jj's own `git init`, colocated, whose `@-` is the
// implementer's commit on `legion/<issue>`, which a bare origin of the run's own holds, so the jj
// the pane's tool runs there finds `@-` on the branch at origin. The origin takes the branch with
// a fetch of its own from the workspace, and the workspace then fetches the origin, which leaves
// both where a push leaves them (late_completion_live_test.go's initWorkspace says why a fetch and
// not a push).
func (r *claudeBackendLive) initWorkspace(work string) {
	t := r.t
	t.Helper()
	repo, err := ghrepo.Parse("the proof's repository", liveRepo)
	if err != nil {
		t.Fatal(err)
	}
	located, err := workspace.Location(r.cfg.StateDir, repo, r.key)
	if err != nil {
		t.Fatal(err)
	}
	r.workspace = located.Dir
	if err := os.MkdirAll(r.workspace, 0o700); err != nil {
		t.Fatalf("make the implementer's workspace %s: %v", r.workspace, err)
	}
	r.run = fixturegit.Isolated(t, t.TempDir())
	origin := filepath.Join(work, "origin.git")
	branch := claim.IssueBranch(r.key)
	r.run.Run(r.workspace, fixturegit.JJInit("", true)...)
	writeProofFile(t, filepath.Join(r.workspace, "implementation.txt"), "The change "+r.key+" asks for.\n")
	r.workspaceJJ("commit", "-m", "implement "+r.key, "implementation.txt")
	r.workspaceJJ("bookmark", "set", branch, "-r", "@-")
	r.head = r.workspaceJJ("log", "-r", "@-", "--no-graph", "-T", "commit_id")
	ref := "refs/heads/" + branch
	r.run.Run(work, fixturegit.InitBare(origin, "")...)
	r.run.Run(work, "git", "--git-dir", origin, "fetch", "--quiet", r.workspaceJJ("git", "root"), ref+":"+ref)
	r.workspaceJJ("git", "remote", "add", "origin", origin)
	r.workspaceJJ("git", "fetch", "--remote", "origin")
	pushed := r.workspaceJJ("log", "-r", fmt.Sprintf("remote_bookmarks(exact:%q, exact:\"origin\")", branch), "--no-graph", "-T", "commit_id")
	if pushed != r.head {
		t.Fatalf("%s@origin is %q, want the implementer's commit %s", branch, pushed, r.head)
	}
	r.note("the implementer's workspace %s stands on its commit %s, which %s@origin is (%s)", r.workspace, r.head, branch, origin)
}

// workspaceJJ runs jj in the implementer's workspace and answers its trimmed stdout.
func (r *claudeBackendLive) workspaceJJ(args ...string) string {
	r.t.Helper()
	return strings.TrimSpace(r.run.Out(r.workspace, append([]string{"jj"}, args...)...))
}

// serveGitHub starts the harness's GitHub stand-in for the issue's pull request: the repository
// with its default branch, the pull request at the implementer's head, and that pull request
// listed open on the issue's branch, which is the read the implementer's completion in
// implementing makes (api's implementationPullRequest).
func (r *claudeBackendLive) serveGitHub() {
	r.github = &liveGitHub{
		t: r.t, key: r.key, number: r.number, base: "main", sha: r.head, mergeState: "clean",
		also: map[string]any{"/pulls": []any{map[string]any{
			"number": r.number, "state": "open",
			"base": map[string]string{"ref": "main"},
			"head": map[string]string{"ref": claim.IssueBranch(r.key), "sha": r.head},
		}}},
	}
	r.github.url = githubtest.Server(r.t, githubtest.Spec{Handle: r.github.ServeHTTP})
}

// seed records the issue as the daemon would hold it at the moment its planner began: a root
// handed to Legion, in planning, holding its tree's admission slot with its lifecycle open, and its
// planner's claim mid-launch, which this boot relaunches on the fake runtime.
func (r *claudeBackendLive) seed() {
	t := r.t
	t.Helper()
	ctx := context.Background()
	listed := r.dispatch.issue(r.key)
	if err := r.st.Tx(ctx, func(tx pgx.Tx) error {
		if err := r.records.PutIssue(ctx, tx, record.Issue{
			Key: r.key, Repo: liveRepo, DefaultBranch: "main", GitHubRepo: liveRepo, Tree: r.key, Project: r.cfg.Project,
			Title: listed.Title, Phase: phase.Planning, Generation: 1, Status: "in_progress", DispatchStatus: "in_progress",
			Rank: listed.Rank, LastDispatchSeq: listed.LastSeq,
		}); err != nil {
			return err
		}
		return r.admit(ctx, tx, r.key)
	}); err != nil {
		t.Fatalf("seed %s: %v", r.key, err)
	}
	if _, err := r.st.OpenTreeLifecycle(ctx, r.project, r.key, treelifecycle.AuthorityWorkflow); err != nil {
		t.Fatalf("open the lifecycle of %s: %v", r.key, err)
	}
	planner := r.token(claim.RolePlanner)
	if _, err := r.st.AdmitClaim(ctx, supervise.Claim{
		Token: planner, Project: r.project, Tree: r.key, Issue: r.key, Role: claim.RolePlanner,
		Generation: 1, State: supervise.StateLaunching,
		BootTokenHash: supervise.HashBootToken("interrupted-" + randomSuffix(t)), ServingGeneration: 1,
	}); err != nil {
		t.Fatalf("record the planner's unfinished launch: %v", err)
	}
	writePrompt(t, r.cfg, planner)
	r.note("seeded %s in %s with its planner mid-launch", r.key, phase.Planning)
}

// overrides is the daemon this proof boots: the two runtimes behind one (claudeBackendRuntime)
// over the real worker stream, the API listener the fixture holds, the workflow's App tokens stood
// in for, and GitHub's stand-in.
func (r *claudeBackendLive) overrides(log *slog.Logger) overrides {
	return overrides{
		runtime: func(ctx context.Context, listener *stream.Listener, address string, _ appauth.Source,
			_ func(context.Context, string) (ghrepo.Repository, string, error), _ *store.Store, _ func() []ghrepo.Repository) (runtime.Runtime, error) {
			panes, err := r.tmuxRuntime(ctx, listener, address, log)
			if err != nil {
				return nil, err
			}
			r.mu.Lock()
			r.panes, r.address = panes, address
			r.mu.Unlock()
			return &claudeBackendRuntime{fake: r.fake, tmux: panes}, nil
		},
		listen:         heldListen,
		workflowTokens: idleRigAppTokens{workflowTokenRecorder: r.tokens},
		githubAPI:      r.github.url,
	}
}

// tmuxRuntime is the runtime `legion start` builds for this configuration (daemon.go's
// tmuxOptions), with the Claude Code launcher boot resolved from the same three variables, over the
// worker stream the daemon hands its runtime factory. The differences are the proof's: each pane
// runs the `legion` this checkout built rather than the test binary (Executable), the pane's
// Envoy listener is the pane listener stand-in, and the daemon holds no gh bundle for it, since
// the workflow's tokens are placeholders. Its private server's environment is scrubbed, as boot's
// is.
func (r *claudeBackendLive) tmuxRuntime(ctx context.Context, listener *stream.Listener, address string, log *slog.Logger) (*tmux.Runtime, error) {
	dispatchTokenFile, err := runtime.WriteDispatchTokenFile(r.cfg.StateDir, r.dispatch.token)
	if err != nil {
		return nil, err
	}
	launchers := map[runtime.Backend]tmux.Launcher{runtime.BackendClaude: tmux.NewClaudeLauncher(r.claude)}
	opts := tmuxOptions(r.cfg, r.project, launchers, "", dispatchTokenFile, log)
	opts.StreamAddress, opts.Conns = address, listener
	opts.EnvoyURL = r.paneListener
	legion := r.legion
	opts.Executable = func() (string, error) { return legion, nil }
	panes, err := tmux.New(opts)
	if err != nil {
		return nil, err
	}
	if _, err := panes.ScrubServerEnvironment(ctx); err != nil {
		return nil, fmt.Errorf("scrub the private tmux server's environment: %w", err)
	}
	return panes, nil
}

// socket is the private tmux server's socket name, "" before the daemon built its runtime.
func (r *claudeBackendLive) socket() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.panes == nil {
		return ""
	}
	return r.panes.Socket()
}

// tmuxCommand runs tmux against the runtime's private server and answers its output.
func (r *claudeBackendLive) tmuxCommand(args ...string) (string, error) {
	cmd := exec.Command("tmux", append([]string{"-L", r.socket()}, args...)...)
	cmd.Env = append(os.Environ(), "TMUX_TMPDIR="+r.tmuxDir)
	out, err := cmd.CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

// killServer ends the runtime's private tmux server, and every pane on it, at the end of the run.
func (r *claudeBackendLive) killServer() {
	if r.socket() == "" {
		return
	}
	if out, err := r.tmuxCommand("kill-server"); err != nil && !strings.Contains(out, "no server running") {
		r.t.Logf("kill the private tmux server %s: %v: %s", r.socket(), err, out)
	}
}

// ---- the steps -----------------------------------------------------------------------------------

// Step 1. The planner the boot relaunches on the fake runtime is brought to ready as its pane would
// be, and in the turn its phase task starts writes its plan record under its own session and
// completes planning naming it, and that turn ends; the issue moves to implementing, whose start
// launches the implementer on the tmux runtime.
func (r *claudeBackendLive) plannerCompletes() {
	t := r.t
	planner := r.fakeClaimant(claim.RolePlanner, "ses_claude_backend_planner")
	// A worker completes inside the turn its phase task started: a completion sent before that
	// task's prompt would leave the planner no turn for endTurn to end.
	planner.awaitTurn()
	written := r.writeRecord(planner.session, "plan")
	status, failure := planner.complete(api.HandoffCompleteRequest{Summary: "planned", HandoffID: written.ID, HandoffVersion: written.Version})
	if status != http.StatusOK {
		t.Fatalf("the planner's completion naming its own plan record = %d %+v, want 200", status, failure)
	}
	// The planner's turn ends with its completion, as a worker's does: supervise holds the
	// implementer's phase task while another role of the issue is in a turn, since the two share
	// the issue's working copy (Machine.sendPending, issueInTurn), so the implementer is prompted
	// only once it has.
	planner.endTurn()
	got := r.waitPhase(phase.Implementing)
	r.note("the planner on the fake runtime completed planning naming its record %s (version %d); %s is in %s",
		written.ID, written.Version, r.key, got.Phase)
}

// Step 2. The implementer the phase's start launched is on the tmux runtime, and its Claude Code
// registers its transcript as the claim's session: the session file is the transcript Claude Code
// writes under the claim's own Claude Code directory, named for the session; the pane's claude is
// started with `--session-id <session>`; and its parent is `legion claude-bridge -- <claude>`, the
// `legion` this checkout built.
func (r *claudeBackendLive) implementerRegisters() {
	t := r.t
	token := r.token(claim.RoleImplementer)
	var held api.OperatorClaim
	r.await(string(token)+"'s Claude Code to register its session", paneReady, func() bool {
		if !slices.ContainsFunc(r.d.claims(), func(c api.OperatorClaim) bool { return c.Token == token }) {
			return false
		}
		held = r.claim(token)
		if held.State == string(supervise.StateFailed) {
			t.Fatalf("%s failed before its Claude Code registered: %+v", token, held)
		}
		return held.Session != "" && held.SessionFile != ""
	})
	r.session, r.transcript = held.Session, held.SessionFile
	directory := filepath.Join(r.cfg.StateDir, "claude", string(token), "projects") + string(filepath.Separator)
	if !strings.HasPrefix(r.transcript, directory) || filepath.Base(r.transcript) != r.session+".jsonl" {
		t.Fatalf("the implementer registered session file %s, want %s<cwd>/%s.jsonl", r.transcript, directory, r.session)
	}
	if held.Locator == nil || held.Locator.Tmux == nil {
		t.Fatalf("the implementer's claim records locator %+v, want a tmux pane", held.Locator)
	}
	bridge, claude := r.claudeOf(*held.Locator)
	if args := procArgs(bridge); len(args) < 4 || args[1] != "claude-bridge" || args[2] != "--" || args[3] != r.claude.Binary {
		t.Fatalf("the implementer's claude (pid %d) runs under %q, want `%s claude-bridge -- %s`", claude, args, r.legion, r.claude.Binary)
	}
	legion, err := filepath.EvalSymlinks(r.legion)
	if err != nil {
		t.Fatalf("resolve this checkout's legion %s: %v", r.legion, err)
	}
	if exe, _ := os.Readlink(fmt.Sprintf("/proc/%d/exe", bridge)); exe != legion {
		t.Fatalf("the implementer's bridge (pid %d) executes %s, want this checkout's legion %s", bridge, exe, legion)
	}
	if !argvHasPair(procArgs(claude), "--session-id", r.session) {
		t.Fatalf("the implementer's claude (pid %d) was not started with --session-id %s: %q", claude, r.session, procArgs(claude))
	}
	r.await("the implementer's transcript "+r.transcript+" to be written", time.Minute, func() bool {
		info, err := os.Stat(r.transcript)
		return err == nil && info.Size() > 0
	})
	r.note("%s is %s in pane %s, Claude Code session %s, transcript %s; its claude (pid %d) is a child of `legion claude-bridge` (pid %d) and runs with --session-id %s",
		token, held.State, held.Locator.Tmux.Pane, r.session, r.transcript, claude, bridge, r.session)
}

// Step 3. The implementer's first turn is its phase's start, which the stand-in answers with one
// line of text and no tool, so the turn ends with implementing open. The `Stop` hook answers that
// settle with the reminder, and Claude Code carries it into one more turn of the same session: the
// stand-in is asked again, with the reminder as the turn's message, and the transcript holds it as
// `Stop hook feedback`. The stand-in holds that turn's answer until step 4.
func (r *claudeBackendLive) stopHookReminds() {
	t := r.t
	r.model.awaitFirst(t, paneReady)
	r.note("the stand-in answered the implementer's start with text alone, ending the turn with implementing open: %q", r.model.firstReply())
	r.model.awaitReminded(t, 3*time.Minute)
	r.note("the stand-in was asked again with the Stop hook's reminder as the turn's message, offering %s", claudeLegionTool)
	r.await("the transcript to hold the Stop hook's feedback", time.Minute, func() bool {
		for _, text := range transcriptUserTexts(r.transcript) {
			if strings.Contains(text, "Stop hook feedback") && strings.Contains(text, claudePhaseStallNeedle) {
				return true
			}
		}
		return false
	})
	if issue := r.issue(r.key); issue.Phase != phase.Implementing {
		t.Fatalf("%s is in %s while the reminded turn is held, want implementing: the phase was not open when the turn ended", r.key, issue.Phase)
	}
	r.note("the transcript holds Claude Code's own Stop hook feedback naming %q, in session %s", claudePhaseStallNeedle, r.session)
}

// Step 4. The driver writes the implementer's implement record under its Claude Code session,
// naming the head its workspace pushed; then the reminded turn calls handoff_complete through the
// `legion` MCP tool. The tool answers without an error, the daemon answers the completion 200
// naming that record at that head, the issue moves to testing, and its implementer's phases row
// holds the record.
func (r *claudeBackendLive) implementerCompletes() {
	t := r.t
	token := r.token(claim.RoleImplementer)
	r.implementRecord = r.writeRecord(r.session, "implement")
	// The daemon answers the completion before its answer reaches the stand-in as the tool's
	// result, so the read of its line starts from a count taken before the turn is released.
	from := r.daemon.count()
	r.model.release()
	result := r.model.awaitResult(t, 3*time.Minute)
	r.note("the reminded turn called %s %s; the tool answered (error %t): %s", claudeLegionTool, r.model.callInput(), result.isError, result.text)
	if result.isError {
		t.Fatalf("the legion MCP tool refused the implementer's handoff_complete: %s", result.text)
	}
	line := r.awaitLogFrom("the daemon's answer to the implementer's completion", from, time.Minute, func(l loggedRecord) bool {
		return l.Message == lateCompletionAnswered && l.Attrs["claim"] == string(token)
	})
	r.note("daemon: %s", answeredLine(*line))
	if line.Attrs["status"] != float64(http.StatusOK) || line.Attrs["handoffId"] != r.implementRecord.ID || line.Attrs["commit"] != r.head {
		t.Fatalf("the implementer's completion was answered %v %v naming record %v at commit %v; want 200 naming its implement record %s at its pushed head %s",
			line.Attrs["status"], line.Attrs["code"], line.Attrs["handoffId"], line.Attrs["commit"], r.implementRecord.ID, r.head)
	}
	got := r.waitPhase(phase.Testing)
	held := got.Workers[claim.RoleImplementer].HandoffRecord
	if held == nil || held.ID != r.implementRecord.ID || held.Version != r.implementRecord.Version {
		t.Fatalf("GET /legion/v1/state shows the implementer's handoffRecord %+v, want %s version %d", held, r.implementRecord.ID, r.implementRecord.Version)
	}
	r.note("read back: %s is in %s, and the implementer's phases row holds record %s (version %d)", r.key, got.Phase, held.ID, held.Version)
}

// Step 5. Once the implementer's last turn has ended, its pane is killed. The daemon launches its
// next generation, which registers the same session and transcript; its claude is started with
// `--resume <session>`; the transcript's bytes from before the kill are unchanged; and the turn an
// operator's task then starts is sent to the stand-in with the first turn's reply in its context.
func (r *claudeBackendLive) killedPaneResumes() {
	t := r.t
	token := r.token(claim.RoleImplementer)
	r.await("the implementer's last turn to end", 2*time.Minute, func() bool {
		state := r.claim(token).State
		return state == string(supervise.StateIdle) || state == string(supervise.StateReady)
	})
	before := r.claim(token)
	prefix, size := fileDigest(t, r.transcript)
	if out, err := r.tmuxCommand("kill-pane", "-t", before.Locator.Tmux.Pane); err != nil {
		t.Fatalf("kill the implementer's pane %s: %v: %s", before.Locator.Tmux.Pane, err, out)
	}
	r.note("killed pane %s (generation %d), its transcript at %d bytes", before.Locator.Tmux.Pane, before.Generation, size)
	var after api.OperatorClaim
	r.await("the implementer's next generation to register", paneReady, func() bool {
		after = r.claim(token)
		return after.Generation > before.Generation && after.Session != "" && after.Locator != nil && after.Locator.Tmux != nil &&
			(after.State == string(supervise.StateReady) || after.State == string(supervise.StateIdle))
	})
	if after.Session != r.session || after.SessionFile != r.transcript {
		t.Fatalf("the relaunch registered session %s at %s, want %s at %s", after.Session, after.SessionFile, r.session, r.transcript)
	}
	_, claude := r.claudeOf(*after.Locator)
	if !argvHasPair(procArgs(claude), "--resume", r.session) {
		t.Fatalf("the relaunched claude (pid %d) was not started with --resume %s: %q", claude, r.session, procArgs(claude))
	}
	if digest, _ := fileDigestOf(t, r.transcript, size); digest != prefix {
		t.Fatalf("the transcript's first %d bytes changed across the relaunch", size)
	}
	marker := "claude-backend-after-relaunch-" + randomSuffix(t)[:8]
	status, body := r.d.request(http.MethodPost, "/legion/v1/operator/claims/"+string(token)+"/deliver",
		api.DeliverRequest{Task: "Claude backend proof after a relaunch (" + marker + "): reply with one line."}, true)
	if status != http.StatusOK && status != http.StatusCreated {
		t.Fatalf("deliver a task to the relaunched implementer = %d %s", status, body)
	}
	r.await("the resumed session's turn to reach the stand-in with its first turn in context", 3*time.Minute, func() bool {
		return r.model.requestsHoldingBoth(marker, r.model.marker) > 0
	})
	r.note("generation %d runs claude --resume %s in pane %s; the transcript kept its first %d bytes, and the turn the operator's task started was sent with the first turn's reply (%s) in its context",
		after.Generation, r.session, after.Locator.Tmux.Pane, size, r.model.marker)
}

// ---- what a step drives and reads ----------------------------------------------------------------

// writeRecord writes the issue's handoff of word to Dispatch's own route as session, with a body
// naming the implementer's head, and reads it back as the newest record of word.
func (r *claudeBackendLive) writeRecord(session, word string) handoffRecord {
	t := r.t
	t.Helper()
	status, answer := r.dispatch.agentPost("/api/v1/issues/"+r.key+"/handoffs/"+word, map[string]any{
		"actor": map[string]string{"kind": "session", "id": session}, "body": string(headBody(r.key, word, r.head)),
	})
	if status != http.StatusCreated {
		t.Fatalf("the write of %s's %s handoff as session %s = %d %s, want 201", r.key, word, session, status, answer)
	}
	written := r.dispatch.latest(r.key, word)
	if written.Author.Kind != "session" || written.Author.ID != session || recordHeadOf(written.Body) != r.head {
		t.Fatalf("the newest %s record of %s = %+v, want one by session %s naming head %s", word, r.key, written, session, r.head)
	}
	r.note("wrote %s's %s record as session %s: version %d (id %s), head %s", r.key, word, session, written.Version, written.ID, r.head)
	return written
}

// waitPhase waits for the issue to reach want.
func (r *claudeBackendLive) waitPhase(want phase.Phase) api.Issue {
	r.t.Helper()
	var got api.Issue
	r.await(fmt.Sprintf("%s in %s", r.key, want), 2*time.Minute, func() bool {
		got = r.issue(r.key)
		return got.Phase == want
	})
	return got
}

// claudeOf is the implementer's `legion claude-bridge` process and its Claude Code under the pane
// the locator records: the first process under the pane's whose argv[1] is claude-bridge, and the
// child of it that names the session it starts or resumes.
func (r *claudeBackendLive) claudeOf(loc runtime.Locator) (bridge, claude int) {
	t := r.t
	t.Helper()
	pane, err := strconv.Atoi(strings.SplitN(loc.Incarnation, ":", 2)[0])
	if err != nil {
		t.Fatalf("the implementer's incarnation %q names no process: %v", loc.Incarnation, err)
	}
	r.await("the implementer's pane "+loc.Tmux.Pane+" to run claude under its bridge", time.Minute, func() bool {
		bridge, claude = 0, 0
		queue := []int{pane}
		for len(queue) > 0 && bridge == 0 {
			pid := queue[0]
			queue = queue[1:]
			if args := procArgs(pid); len(args) > 1 && args[1] == "claude-bridge" {
				bridge = pid
			}
			queue = append(queue, procChildren(pid)...)
		}
		for _, child := range procChildren(bridge) {
			args := procArgs(child)
			if slices.Contains(args, "--session-id") || slices.Contains(args, "--resume") {
				claude = child
			}
		}
		return bridge != 0 && claude != 0
	})
	return bridge, claude
}

// procArgs is a process's argv, nil when it cannot be read.
func procArgs(pid int) []string {
	raw, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid))
	if err != nil || len(raw) == 0 {
		return nil
	}
	return strings.Split(strings.TrimSuffix(string(raw), "\x00"), "\x00")
}

// procChildren is every child of a process, from every thread's children list: a Go process (the
// shim, the bridge) starts a child from whichever thread runs the goroutine.
func procChildren(pid int) []int {
	threads, _ := filepath.Glob(fmt.Sprintf("/proc/%d/task/*/children", pid))
	var children []int
	for _, thread := range threads {
		raw, err := os.ReadFile(thread)
		if err != nil {
			continue
		}
		for _, field := range strings.Fields(string(raw)) {
			if child, err := strconv.Atoi(field); err == nil {
				children = append(children, child)
			}
		}
	}
	return children
}

// argvHasPair is whether argv holds flag immediately followed by value.
func argvHasPair(argv []string, flag, value string) bool {
	for i := 0; i+1 < len(argv); i++ {
		if argv[i] == flag && argv[i+1] == value {
			return true
		}
	}
	return false
}

// transcriptUserTexts is the text of every user entry of a Claude Code transcript, a string or the
// text of its blocks, in order. A line that does not parse is skipped: Claude Code may be mid-write.
func transcriptUserTexts(path string) []string {
	file, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer file.Close()
	var texts []string
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 1<<20), 1<<26)
	for scanner.Scan() {
		var entry struct {
			Type    string `json:"type"`
			Message struct {
				Content json.RawMessage `json:"content"`
			} `json:"message"`
		}
		if json.Unmarshal(scanner.Bytes(), &entry) != nil || entry.Type != "user" {
			continue
		}
		texts = append(texts, blocksText(contentBlocks(entry.Message.Content)))
	}
	return texts
}

// fileDigest is the sha256 of a file's bytes and its size now.
func fileDigest(t *testing.T, path string) (string, int64) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return fileDigestOf(t, path, info.Size())
}

// fileDigestOf is the sha256 of a file's first size bytes, and size.
func fileDigestOf(t *testing.T, path string, size int64) (string, int64) {
	t.Helper()
	file, err := os.Open(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	defer file.Close()
	sum := sha256.New()
	if _, err := io.CopyN(sum, file, size); err != nil {
		t.Fatalf("read the first %d bytes of %s: %v", size, path, err)
	}
	return hex.EncodeToString(sum.Sum(nil)), size
}

// ---- the fake runtime's claims -------------------------------------------------------------------

// fakeClaimant is a claim of the fake runtime brought to ready as its pane would be: the shim
// connected with the launch's boot token, the session registered and ready, and a reader that
// answers what the daemon asks of the shim and starts a turn on each prompt, which endTurn ends, as
// handoff_records_live_test.go's handoffClaimant does.
type fakeClaimant struct {
	r       *claudeBackendLive
	token   claim.Token
	session string
	secret  string
	sh      *shim
	mu      sync.Mutex
}

// fakeClaimant waits for the daemon to launch role's claim on the fake runtime and brings it to
// ready as session.
func (r *claudeBackendLive) fakeClaimant(role claim.Role, session string) *fakeClaimant {
	t := r.t
	t.Helper()
	token := r.token(role)
	r.await("the launch of "+string(token)+" on the fake runtime", 2*time.Minute, func() bool {
		return slices.ContainsFunc(r.fake.Calls(), func(call fake.Call) bool {
			return (call.Method == "Spawn" || call.Method == "Resume") && call.Spec.Claim == token
		})
	})
	launch := lastLaunch(t, r.fake, token)
	r.mu.Lock()
	address := r.address
	r.mu.Unlock()
	c := &fakeClaimant{r: r, token: token, session: session, sh: dialShim(t, address, launch.BootToken)}
	r.await("the shim of "+string(token)+" to connect", time.Minute, func() bool {
		return r.d.claim(token).State == string(supervise.StateShimConnected)
	})
	status, body := r.d.request(http.MethodPost, "/legion/v1/claims/register", claim.RegisterRequest{
		BootToken: launch.BootToken, SessionID: session, OmpSessionFile: "/sessions/" + session + ".jsonl",
		AgentID: session, PluginContract: api.DaemonAPIVersion,
	}, false)
	if status != http.StatusOK {
		t.Fatalf("register %s = %d %s, want 200", token, status, body)
	}
	var registered claim.RegisterResponse
	if err := json.Unmarshal(body, &registered); err != nil {
		t.Fatalf("decode the registration of %s: %v", token, err)
	}
	c.secret = registered.Secret
	go c.serve()
	if status, body := r.d.request(http.MethodPost, "/legion/v1/claims/ready", claim.ReadyRequest{
		ClaimToken: token, SessionID: session, Secret: registered.Secret, Generation: launch.Generation,
	}, false); status != http.StatusNoContent {
		t.Fatalf("ready %s = %d %s, want 204", token, status, body)
	}
	r.note("%s's %s is registered and ready on the fake runtime as session %s", r.key, role, session)
	return c
}

// serve reads every frame the daemon sends the shim until the connection closes: it answers the
// protocol negotiation and the state probe, and acknowledges each prompt and starts its turn, as
// Oh My Pi does.
func (c *fakeClaimant) serve() {
	reader := c.sh.lines
	_ = c.sh.conn.SetReadDeadline(time.Time{})
	for {
		line, err := reader.ReadBytes('\n')
		if err != nil {
			return
		}
		frame, err := shimwire.Decode(line)
		if err != nil {
			continue
		}
		switch f := frame.(type) {
		case shimwire.NegotiateProtocol:
			c.send(shimwire.Response{ID: f.ID, Command: shimwire.TypeNegotiateProtocol, Success: true})
		case shimwire.GetState:
			streaming := false
			data, _ := json.Marshal(shimwire.StateData{IsStreaming: &streaming})
			c.send(shimwire.Response{ID: f.ID, Command: shimwire.TypeGetState, Success: true, Data: data})
		case shimwire.Prompt:
			c.send(shimwire.Response{ID: f.ID, Command: shimwire.TypePrompt, Success: true})
			c.send(shimwire.AgentStart{DeliveryID: f.DeliveryID})
		}
	}
}

// send writes one frame to the daemon.
func (c *fakeClaimant) send(frame shimwire.Frame) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.sh.writer.WriteFrame(frame); err != nil && !errors.Is(err, io.ErrClosedPipe) {
		c.r.t.Logf("send %s for %s: %v", frame.FrameType(), c.token, err)
	}
}

// awaitTurn waits for the daemon to hold this claim in a turn: the one its reader starts on the
// task the daemon prompts it with (serve).
func (c *fakeClaimant) awaitTurn() {
	c.r.t.Helper()
	c.r.await("the turn of "+string(c.token)+" to start", time.Minute, func() bool {
		return c.r.claim(c.token).State == string(supervise.StateWorking)
	})
}

// endTurn ends the turn this claim is in, as the agent's turn ends: it waits for the daemon to
// hold the claim in that turn, sends agent_end, and returns once the daemon has the claim out of
// it.
func (c *fakeClaimant) endTurn() {
	c.r.t.Helper()
	c.awaitTurn()
	c.send(shimwire.AgentEnd{})
	c.r.await("the turn of "+string(c.token)+" to end", time.Minute, func() bool {
		return c.r.claim(c.token).State != string(supervise.StateWorking)
	})
}

// complete posts a completion for this claim with a grant minted for it now, as the `legion`
// tool's handoff_complete does, and returns the status and the refusal, if any.
func (c *fakeClaimant) complete(req api.HandoffCompleteRequest) (int, api.Failure) {
	t := c.r.t
	t.Helper()
	status, body := c.r.d.request(http.MethodPost, "/legion/v1/grants",
		api.GrantRequest{SessionID: c.session, Secret: c.secret, Tree: c.r.key, Issue: c.r.key}, false)
	if status != http.StatusOK {
		t.Fatalf("the grant of %s = %d %s, want 200", c.token, status, body)
	}
	var granted api.GrantResponse
	if err := json.Unmarshal(body, &granted); err != nil {
		t.Fatalf("decode the grant of %s: %v", c.token, err)
	}
	req.GrantID = granted.GrantID
	status, body = c.r.d.request(http.MethodPost, "/legion/v1/handoff/complete", req, false)
	var failure api.Failure
	if status != http.StatusOK {
		if err := json.Unmarshal(body, &failure); err != nil {
			t.Fatalf("decode the refusal %s: %v", body, err)
		}
	}
	return status, failure
}

// ---- the two runtimes behind one -----------------------------------------------------------------

// claudeBackendRuntime is the runtime the proof's daemon runs: the implementer's claim on the real
// tmux runtime, and every other claim on the fake. A claim's runtime is its role's, read off its
// token, since both runtimes' locators name the tmux runtime. It says it provisions every
// workspace, so the daemon provisions none on its own host: the implementer's is the driver's
// (initWorkspace), and the fake's claims run nowhere.
type claudeBackendRuntime struct {
	fake *fake.Runtime
	tmux *tmux.Runtime
}

// onTmux is whether token's claim runs on the tmux runtime: the implementer's alone.
func (c *claudeBackendRuntime) onTmux(token claim.Token) bool {
	role, ok := token.Role()
	return ok && role == claim.RoleImplementer
}

// of is the runtime token's claim runs on.
func (c *claudeBackendRuntime) of(token claim.Token) runtime.Runtime {
	if c.onTmux(token) {
		return c.tmux
	}
	return c.fake
}

func (c *claudeBackendRuntime) Spawn(ctx context.Context, spec runtime.SpawnSpec) (runtime.Locator, error) {
	return c.of(spec.Claim).Spawn(ctx, spec)
}

func (c *claudeBackendRuntime) Resume(ctx context.Context, prev *runtime.Locator, spec runtime.SpawnSpec) (runtime.Locator, error) {
	return c.of(spec.Claim).Resume(ctx, prev, spec)
}

func (c *claudeBackendRuntime) Suspend(ctx context.Context, loc runtime.Locator) error {
	return c.of(loc.Claim).Suspend(ctx, loc)
}

func (c *claudeBackendRuntime) Release(ctx context.Context, k runtime.Known) error {
	return c.of(k.Claim).Release(ctx, k)
}

func (c *claudeBackendRuntime) Probe(ctx context.Context, loc runtime.Locator) (runtime.Observation, error) {
	return c.of(loc.Claim).Probe(ctx, loc)
}

// Observe is both runtimes' sweeps on one channel, closed once both have closed theirs.
func (c *claudeBackendRuntime) Observe(ctx context.Context) (<-chan runtime.Observation, error) {
	fromFake, err := c.fake.Observe(ctx)
	if err != nil {
		return nil, err
	}
	fromTmux, err := c.tmux.Observe(ctx)
	if err != nil {
		return nil, err
	}
	merged := make(chan runtime.Observation)
	var both sync.WaitGroup
	for _, observations := range []<-chan runtime.Observation{fromFake, fromTmux} {
		both.Add(1)
		go func() {
			defer both.Done()
			for observation := range observations {
				select {
				case merged <- observation:
				case <-ctx.Done():
					return
				}
			}
		}()
	}
	go func() {
		both.Wait()
		close(merged)
	}()
	return merged, nil
}

// ReconcileOrphans hands each runtime the known claims that run on it: the tmux runtime refuses a
// locator it did not open, and the fake's claims have none of its panes.
func (c *claudeBackendRuntime) ReconcileOrphans(ctx context.Context, known []runtime.Known, grace time.Duration) error {
	var onTmux, onFake []runtime.Known
	for _, k := range known {
		if c.onTmux(k.Claim) {
			onTmux = append(onTmux, k)
		} else {
			onFake = append(onFake, k)
		}
	}
	return errors.Join(c.fake.ReconcileOrphans(ctx, onFake, grace), c.tmux.ReconcileOrphans(ctx, onTmux, grace))
}

func (c *claudeBackendRuntime) AdoptWorkingCopy(ctx context.Context, loc runtime.Locator, id runtime.GitIdentity) error {
	return c.of(loc.Claim).AdoptWorkingCopy(ctx, loc, id)
}

func (c *claudeBackendRuntime) ProvisionsWorkspaces() bool { return true }

func (c *claudeBackendRuntime) ProvisionBound() time.Duration { return 0 }

func (c *claudeBackendRuntime) LaunchBound() time.Duration { return 0 }

func (c *claudeBackendRuntime) PodDrift(ctx context.Context, k runtime.Known) ([]string, error) {
	return c.of(k.Claim).PodDrift(ctx, k)
}

func (c *claudeBackendRuntime) ReplacePod(ctx context.Context, k runtime.Known, due func(context.Context) error) error {
	return c.of(k.Claim).ReplacePod(ctx, k, due)
}

func (c *claudeBackendRuntime) CleanupTree(ctx context.Context, tree string) error {
	return errors.Join(c.fake.CleanupTree(ctx, tree), c.tmux.CleanupTree(ctx, tree))
}

// ---- the stand-in Claude Code talks to -----------------------------------------------------------

// claudeStandin is the model endpoint the implementer's Claude Code reaches through
// ANTHROPIC_BASE_URL. It answers the turn whose message carries the phase's start with one line of
// text carrying marker and no tool, which ends the turn with the phase open; the turn whose
// message carries the Stop hook's reminder, when the request offers the `legion` MCP tool, with a
// `tool_use` of that tool calling handoff_complete, held until release; the request carrying that
// call's result with lateWaitingReply, handing the result to the step; and every other request
// with lateWaitingReply too. A turn is matched on what starts it (turnOf), since Claude Code may
// put messages of its own beside a task.
type claudeStandin struct {
	*modelStandin
	marker string

	mu      sync.Mutex
	first   string
	called  bool
	callID  string
	input   map[string]any
	once    sync.Once
	held    chan struct{}
	started chan struct{}
	asked   chan struct{}
	result  chan toolResult
}

func newClaudeStandin(t *testing.T, name, marker string) *claudeStandin {
	t.Helper()
	s := &claudeStandin{
		modelStandin: &modelStandin{name: name}, marker: marker, callID: "toolu_" + name + "_handoff",
		input: map[string]any{"op": "handoff_complete", "summary": "Implemented the change at the head the implement record names, pushed to the branch."},
		held:  make(chan struct{}), started: make(chan struct{}), asked: make(chan struct{}),
		result: make(chan toolResult, 1),
	}
	s.serve(t, s)
	return s
}

// release lets the held handoff_complete call go.
func (s *claudeStandin) release() { s.once.Do(func() { close(s.held) }) }

// firstReply is the text the stand-in ended the phase's first turn with.
func (s *claudeStandin) firstReply() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.first
}

// callInput is the input the held call carries, as a note prints it.
func (s *claudeStandin) callInput() string {
	encoded, _ := json.Marshal(s.input)
	return string(encoded)
}

// requestsHoldingBoth is how many requests carried both a and b.
func (s *claudeStandin) requestsHoldingBoth(a, b string) int {
	s.modelStandin.mu.Lock()
	defer s.modelStandin.mu.Unlock()
	held := 0
	for _, body := range s.requests {
		if strings.Contains(body, a) && strings.Contains(body, b) {
			held++
		}
	}
	return held
}

// awaitFirst waits for the stand-in to answer the phase's first turn.
func (s *claudeStandin) awaitFirst(t *testing.T, within time.Duration) {
	t.Helper()
	select {
	case <-s.started:
	case <-time.After(within):
		t.Fatalf("no turn carrying the implementing phase's start reached the stand-in %s within %s; it answered %d requests", s.name, within, s.count())
	}
}

// awaitReminded waits for the turn carrying the Stop hook's reminder.
func (s *claudeStandin) awaitReminded(t *testing.T, within time.Duration) {
	t.Helper()
	select {
	case <-s.asked:
	case <-time.After(within):
		t.Fatalf("no turn carrying the Stop hook's reminder %q and offering %s reached the stand-in %s within %s; it answered %d requests",
			claudePhaseStallNeedle, claudeLegionTool, s.name, within, s.count())
	}
}

// awaitResult waits for the tool's answer to the call.
func (s *claudeStandin) awaitResult(t *testing.T, within time.Duration) toolResult {
	t.Helper()
	select {
	case result := <-s.result:
		return result
	case <-time.After(within):
		t.Fatalf("Claude Code sent no result of its %s call %s within %s", claudeLegionTool, s.callID, within)
		return toolResult{}
	}
}

func (s *claudeStandin) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	body, ok := s.take(w, req)
	if !ok {
		return
	}
	// Only a Messages request is a turn; a token count is answered a count.
	if !strings.HasSuffix(req.URL.Path, "/v1/messages") {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"input_tokens":1000}`)
		return
	}
	var request struct {
		messagesRequest
		Stream bool `json:"stream"`
	}
	if err := json.Unmarshal([]byte(body), &request); err != nil || len(request.Messages) == 0 {
		s.text(w, request.Stream, lateWaitingReply)
		return
	}
	turn := turnOf(request.messagesRequest)
	for _, block := range turn {
		if block.Type == "tool_result" && block.ToolUseID == s.callID {
			select {
			case s.result <- toolResult{text: blocksText(contentBlocks(block.Content)), isError: block.IsError}:
			default:
			}
			s.text(w, request.Stream, lateWaitingReply)
			return
		}
	}
	text := blocksText(turn)
	offers := slices.ContainsFunc(request.Tools, func(tool offeredTool) bool { return tool.Name == claudeLegionTool })
	s.mu.Lock()
	call := offers && request.Stream && !s.called && strings.Contains(text, claudePhaseStallNeedle)
	opening := !call && offers && s.first == "" && strings.Contains(text, "Phase: implementing")
	if call {
		s.called = true
	}
	if opening {
		s.first = "Standing by for the implementation instruction (" + s.marker + ")."
	}
	first := s.first
	s.mu.Unlock()
	switch {
	case call:
		close(s.asked)
		select {
		case <-s.held:
		case <-req.Context().Done():
			return
		}
		streamAnthropicToolUse(w, s.name, s.callID, claudeLegionTool, s.input)
	case opening:
		s.text(w, request.Stream, first)
		close(s.started)
	default:
		s.text(w, request.Stream, lateWaitingReply)
	}
}

// text answers a request with one text block (answerAnthropicText), under this stand-in's
// provider and model.
func (s *claudeStandin) text(w http.ResponseWriter, streamed bool, text string) {
	answerAnthropicText(w, streamed, s.name, s.name+standinModelID, text)
}
