//go:build e2e

package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/fixturegit"
	"github.com/sjawhar/legion/daemon/internal/githubtest"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/testnats"
	"github.com/sjawhar/legion/daemon/internal/testpg"
	"github.com/sjawhar/legion/daemon/internal/treelifecycle"
)

const teardownEffectsTaskMarker = "Run the teardown-effects trace."
const teardownEffectsComplete = "/legion/v1/handoff/complete"

// The shell validates case files with heldout-case.ts. These projections only read the
// fields needed to set up or exercise the live daemon; they do not define the case schema.
type teardownEffectsCase struct {
	ID, Role, Task string
	World          teardownEffectsWorld
	Actions        []struct {
		Name   string
		Routes []struct {
			Kind  string
			Match map[string]any
		}
		Effect struct {
			Kind, Answer, PhaseAfter, Outbox string
			WakeAt                           struct{ After, Before time.Time }
		}
	}
	Traces struct{ Good, Bad teardownEffectsTrace }
}
type teardownEffectsTrace struct {
	ID      string
	Actions []struct {
		Kind, Op, Command, Method, Path, Ref, Commit string
		Seq                                          int
		At                                           time.Time
		Args, Fields                                 map[string]any
		Answer                                       struct{ Code *string }
	}
}
type teardownEffectsWorld struct {
	Clock    time.Time
	Dispatch struct {
		Issue     struct{ Key, Title, Status, Spec, Parent string }
		Documents []struct{ Name, Body string }
		Handoffs  []struct {
			Phase, Author string
			Version       int
			Body          json.RawMessage
		}
	}
	Git *struct {
		Commits []struct {
			ID, Description string
			Parents         []string
		}
		Remote struct {
			Name     string
			Branches []struct{ Name, Head string }
		}
		Workspace struct{ StandingOn string }
	}
	GitHub *struct {
		Repository   string
		PullRequests []teardownEffectsPull
	}
	Daemon *struct {
		Phase phase.Phase
		Step  *struct {
			Place int
			Name  string
		}
		Task            map[string]string
		ProductionCheck *struct {
			Look              int
			Last              bool
			OpenedAt, BoundAt time.Time
			MergeCommit       string
		}
	}
}
type teardownEffectsPull struct {
	Number                         int
	State, Base, Body, MergeCommit string
	Draft                          bool
	Head                           struct{ Ref, SHA string }
	Reviews                        []struct {
		Author, State, Commit, Body string
		At                          time.Time
	}
}
type teardownEffectsCapture struct {
	Version  int `json:"version"`
	Captured struct {
		Command string    `json:"command"`
		At      time.Time `json:"at"`
		Commit  string    `json:"commit"`
	} `json:"captured"`
	Answers []teardownEffectsFixtureAnswer `json:"answers"`
}
type teardownEffectsFixtureAnswer struct {
	Name   string          `json:"name"`
	Method string          `json:"method"`
	Path   string          `json:"path"`
	Match  map[string]any  `json:"match"`
	Status int             `json:"status"`
	Body   json.RawMessage `json:"body"`
}

// The case clock advances only at actions in the recorded trace. Runtime timeouts still
// wait in real time; At translates the daemon's absolute deadlines to durations.
type teardownEffectsClock struct {
	supervise.RealClock
	mu  sync.RWMutex
	now time.Time
}

func (c *teardownEffectsClock) Now() time.Time { c.mu.RLock(); defer c.mu.RUnlock(); return c.now }
func (c *teardownEffectsClock) At(at time.Time, f func()) supervise.Cancel {
	return c.AfterFunc(at.Sub(c.Now()), f)
}
func (c *teardownEffectsClock) advance(at time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if at.After(c.now) {
		c.now = at
	}
}

type teardownEffectsHarness struct{ root, out, legion, omp, home, profile string }
type teardownEffectsRun struct {
	liveAgentProof
	harness                                             teardownEffectsHarness
	clock                                               *teardownEffectsClock
	dispatch                                            *handoffDispatch
	github                                              *teardownEffectsGitHub
	proxy                                               *teardownEffectsProxy
	model                                               *teardownEffectsModel
	key, branch, repo, base, workspace, remote, session string
	role                                                claim.Role
	token                                               claim.Token
	git                                                 fixturegit.Runner
	commits                                             map[string]string
	handoffs                                            map[string]teardownEffectsHandoff
	before                                              phase.Phase
}

func TestTeardownEffectsLive(t *testing.T) {
	mode := os.Getenv("LEGION_E2E_TEARDOWN_MODE")
	if mode == "" {
		t.Skip("use scripts/e2e/teardown-effects.sh")
	}
	natsURL := os.Getenv(liveNATSURLEnv)
	if natsURL == "" {
		t.Fatal("missing live proof broker")
	}
	testnats.CreateStream(t, workflowJetStream(t, natsURL), jetstream.StreamConfig{Name: "ENVOY_NOTIFICATIONS", Subjects: []string{"notifications.>"}})
	work := t.TempDir()
	h := teardownEffectsHarness{root: proofCheckoutRoot(t), out: filepath.Dir(os.Getenv(liveDaemonLogEnv)), omp: os.Getenv("LEGION_E2E_TEARDOWN_OMP"), home: filepath.Join(work, "home"), profile: "teardown-effects"}
	h.legion = buildProofLegion(t, h.root, work)
	installProofPlugins(t, h.root, work, h.home, h.profile, h.omp)
	switch mode {
	case "controls":
		h.controls(t, os.Getenv("LEGION_E2E_TEARDOWN_CASES"))
	case "runs":
		h.replay(t, os.Getenv("LEGION_E2E_TEARDOWN_RUNS"))
	default:
		t.Fatalf("unknown teardown mode %q", mode)
	}
}

func (h teardownEffectsHarness) start(t *testing.T, world teardownEffectsWorld, role, task, runDir string) *teardownEffectsRun {
	t.Helper()
	if world.Daemon == nil {
		t.Fatal("world has no daemon record")
	}
	key := world.Dispatch.Issue.Key
	project, _, ok := strings.Cut(key, "-")
	if !ok {
		t.Fatalf("invalid fixture issue %q", key)
	}
	dispatch := startHandoffDispatch(t, h.root)
	cfg := testConfig(t)
	cfg.Project, cfg.DispatchURL, cfg.NatsURLs = project, dispatch.base, []string{os.Getenv(liveNATSURLEnv)}
	cfg.PostgresDSN = testpg.Clone(t, "teardown_effects")
	cfg.DispatchTokenFile = filepath.Join(t.TempDir(), "dispatch-token")
	writeProofFile(t, cfg.DispatchTokenFile, dispatch.token+"\n")
	notices := httptest.NewServer(newNoticeListener())
	t.Cleanup(notices.Close)
	cfg.EnvoyURL = notices.URL
	roleOfClaim := claim.Role(role)
	switch role {
	case "", "retro":
		roleOfClaim = phase.RoleFor(world.Daemon.Phase)
	case "production-check":
		roleOfClaim = claim.RoleTester
	case "root-architect", "child-architect":
		roleOfClaim = claim.RoleArchitect
	}
	if world.Daemon.ProductionCheck != nil && role == "" {
		roleOfClaim = claim.RoleTester
	}
	if roleOfClaim == "" {
		t.Fatalf("no claim role for %q in %s", role, world.Daemon.Phase)
	}
	projectConfig := config.Project{}
	if world.Daemon.ProductionCheck != nil {
		projectConfig.ProductionCheck = roleOfClaim
	}
	cfg.Projects = map[string]config.Project{project: projectConfig}
	projectToken, err := claim.ProjectToken(project)
	if err != nil {
		t.Fatal(err)
	}
	token, err := claim.NewToken(projectToken, key, roleOfClaim)
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(t.Context(), cfg.PostgresDSN)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(st.Close)
	proof := liveProof{t: t, cfg: cfg, st: st, records: record.NewStore(), project: projectToken}
	r := &teardownEffectsRun{liveAgentProof: liveAgentProof{liveProof: proof, legion: h.legion}, harness: h, dispatch: dispatch, key: key, branch: "legion/" + key, role: roleOfClaim, token: token,
		clock: &teardownEffectsClock{now: world.Clock}, git: fixturegit.Isolated(t, h.home), commits: map[string]string{}, handoffs: map[string]teardownEffectsHandoff{}, repo: liveRepo, base: "main"}
	if world.GitHub != nil {
		r.repo = world.GitHub.Repository
	}
	r.proxy = teardownEffectsStartProxy(t, cfg.DaemonURL)
	r.model = teardownEffectsStartModel(t, h.home, h.profile)
	r.rt = newLocalRuntime(t, cfg.StateDir, h.legion, h.omp, h.home, h.profile)
	r.rt.paneLogs = filepath.Join(h.out, "panes", filepath.Base(cfg.StateDir))
	if err := os.MkdirAll(r.rt.paneLogs, 0700); err != nil {
		t.Fatal(err)
	}
	r.rt.daemonURL, r.rt.dispatchURL, r.rt.dispatchTokenFile, r.rt.natsURLs, r.rt.envoyURL = r.proxy.url, dispatch.base, cfg.DispatchTokenFile, cfg.NatsURLs, newPaneListener(t)
	r.rt.environment = []string{"GIT_ALLOW_PROTOCOL=file"}
	overlay := filepath.Join(t.TempDir(), "model.yml")
	writeRoleOverlay(t, overlay, standinModel(r.model.name))
	r.rt.overlays = func(claim.Token) []string { return []string{overlay} }
	r.workspace, r.remote = r.rt.workDir(key), filepath.Join(t.TempDir(), "origin.git")
	r.setupGit(world, runDir)
	r.github = &teardownEffectsGitHub{t: t, repo: r.repo, base: r.base, pulls: map[int]teardownEffectsPull{}}
	if world.GitHub != nil {
		for _, pull := range world.GitHub.PullRequests {
			pull.Head.SHA, pull.MergeCommit = r.sha(pull.Head.SHA), r.sha(pull.MergeCommit)
			for i := range pull.Reviews {
				pull.Reviews[i].Commit = r.sha(pull.Reviews[i].Commit)
			}
			r.github.pulls[pull.Number] = pull
		}
	}
	if runDir != "" {
		r.rebuildGitHub(runDir)
	}
	githubURL := githubtest.Server(t, githubtest.Spec{Handle: r.github.serve})
	r.setupIssue(world)
	if runDir != "" {
		// The run's own claim session, whose handoffs the replay writes under its new pane's.
		calls, err := teardownEffectsReadCalls(filepath.Join(runDir, "sessions"))
		if err != nil {
			t.Fatal(err)
		}
		original := ""
		if len(calls) > 0 {
			original = calls[0].Session
		}
		r.restoreDispatch(runDir, original, false)
	} else {
		for _, doc := range world.Dispatch.Documents {
			r.document(doc.Name, doc.Body)
		}
		for _, handoff := range world.Dispatch.Handoffs {
			r.handoff(handoff.Phase, handoff.Author, r.mapBody(handoff.Body))
		}
	}
	r.dispatch.user(http.MethodPatch, "/api/v1/issues/"+r.key, map[string]any{"status": world.Dispatch.Issue.Status, "labels": []string{"legion"}}, http.StatusOK, nil)
	r.seed(world, task)
	o := r.rt.daemonOverrides(r.rt)
	o.clock, o.githubAPI, o.workflowTokens = r.clock, githubURL, &workflowTokenRecorder{}
	r.d = startDaemonWithin(t, cfg, o, r.log(), 2*time.Minute)
	if got := r.issue(key).Phase; got != world.Daemon.Phase {
		t.Fatalf("boot changed %s from %s to %s", key, world.Daemon.Phase, got)
	}
	r.before = world.Daemon.Phase
	r.model.await(t, r.model.ready, "the real pane's first turn")
	r.session = r.claim(token).Session
	if r.session == "" {
		t.Fatal("the pane did not register a session")
	}
	t.Cleanup(r.model.finish)
	return r
}

func (r *teardownEffectsRun) setupIssue(world teardownEffectsWorld) {
	t := r.t
	t.Helper()
	project := r.cfg.Project
	r.dispatch.user(http.MethodPost, "/api/v1/projects", map[string]string{"key": project, "name": "Teardown effects"}, 201, nil)
	r.dispatch.user(http.MethodPut, "/api/v1/projects/"+project+"/default-repository", map[string]string{"repo": r.repo}, 200, nil)
	wanted, err := strconv.Atoi(strings.TrimPrefix(r.key, project+"-"))
	if err != nil || wanted < 1 {
		t.Fatalf("invalid issue number: %s", r.key)
	}
	// Dispatch allocates the key. Set its counter in this disposable database so replay does
	// not change the key embedded in branch names, prompts and the original requests.
	teardownEffectsIssueCounter(t, r.dispatch, project, wanted)
	var issue struct{ Key string }
	r.dispatch.user(http.MethodPost, "/api/v1/issues", map[string]any{"project": project, "title": world.Dispatch.Issue.Title, "spec": world.Dispatch.Issue.Spec, "force": true}, http.StatusCreated, &issue)
	if issue.Key != r.key {
		t.Fatalf("created issue %s, want %s", issue.Key, r.key)
	}
}

func (r *teardownEffectsRun) seed(world teardownEffectsWorld, task string) {
	t := r.t
	t.Helper()
	if task == "" {
		task = "Continue " + world.Dispatch.Issue.Title + ". Issue: " + r.key + ". Phase: " + string(world.Daemon.Phase) + "."
	}
	listed := r.dispatch.issue(r.key)
	now := r.clock.Now()
	delivery := supervise.Delivery{ID: "teardown-" + randomSuffix(t), Task: teardownEffectsTaskMarker + "\n" + task, Generation: 1, Phase: world.Daemon.Phase, QueuedAt: now}
	ctx := t.Context()
	if err := store.Transact(ctx, r.st.Pool(), func(ctx context.Context, tx pgx.Tx) error {
		issue := record.Issue{Key: r.key, Title: listed.Title, Project: r.cfg.Project, Tree: r.key, Repo: r.repo, GitHubRepo: r.repo, DefaultBranch: r.base, Phase: world.Daemon.Phase, Generation: 1,
			Status: world.Dispatch.Issue.Status, DispatchStatus: world.Dispatch.Issue.Status, Rank: listed.Rank, LastDispatchSeq: listed.LastSeq, HandedOver: true}
		if issue.Phase == phase.Done {
			linger := now.Add(48 * time.Hour)
			if wall := time.Now().Add(time.Hour); linger.Before(wall) {
				linger = wall
			}
			issue.LingerUntil = &linger
		}
		if err := r.records.PutIssue(ctx, tx, issue); err != nil {
			return err
		}
		if step := world.Daemon.Step; step != nil {
			if err := r.records.PutPhase(ctx, tx, record.PhaseRow{Issue: r.key, Role: r.role, Claim: r.token, StepPlace: step.Place, StepTask: step.Name}); err != nil {
				return err
			}
		}
		number := 0
		if pull, ok := r.github.forBranch(r.branch); ok {
			number = pull.Number
			if err := r.records.PutPullRequest(ctx, tx, record.PullRequest{Issue: r.key, Repo: r.repo, Number: number, Branch: r.branch, Base: pull.Base, HeadSHA: pull.Head.SHA, State: record.PullRequestState(pull.State), MergeSHA: pull.MergeCommit, Failing: []string{}, Mergeability: record.MergeabilityMergeable, MergeState: record.MergeStateClean}); err != nil {
				return err
			}
		}
		if check := world.Daemon.ProductionCheck; check != nil {
			if number == 0 {
				return fmt.Errorf("production check has no pull request")
			}
			delivery.Phase = ""
			delivery.CheckLook = supervise.CheckLook{Look: check.Look, OpenedAt: check.OpenedAt}
			if err := r.records.PutProductionCheck(ctx, tx, record.ProductionCheck{Issue: r.key, Project: r.cfg.Project, Tree: r.key, Repo: r.repo, Number: number, MergeSHA: r.sha(check.MergeCommit),
				Role: r.role, OpenedAt: check.OpenedAt, BoundAt: check.BoundAt, Looks: check.Look, LookOpen: true, LookWakeAt: &now, Last: check.Last}); err != nil {
				return err
			}
		}
		if issue.Phase != phase.Done {
			return r.admit(ctx, tx, r.key)
		}
		return nil
	}); err != nil {
		t.Fatalf("seed daemon state: %v", err)
	}
	if _, err := r.st.OpenTreeLifecycle(ctx, r.project, r.key, treelifecycle.AuthorityWorkflow); err != nil {
		t.Fatal(err)
	}
	c := supervise.Claim{Token: r.token, Project: r.project, Tree: r.key, Issue: r.key, Role: r.role, Generation: 1, State: supervise.StateLaunching, BootTokenHash: supervise.HashBootToken("seed-" + randomSuffix(t))}
	if delivery.CheckLook.Look == 0 {
		c.ServingGeneration = 1
	}
	if _, err := r.st.AdmitClaim(ctx, c); err != nil {
		t.Fatal(err)
	}
	if err := r.st.PutDelivery(ctx, r.token, delivery); err != nil {
		t.Fatal(err)
	}
	writePrompt(t, r.cfg, r.token)
}

func (r *teardownEffectsRun) sha(id string) string {
	if mapped := r.commits[id]; mapped != "" {
		return mapped
	}
	return id
}
func (r *teardownEffectsRun) setupGit(world teardownEffectsWorld, runDir string) {
	t := r.t
	t.Helper()
	if err := os.MkdirAll(r.workspace, 0700); err != nil {
		t.Fatal(err)
	}
	if runDir != "" {
		teardownEffectsReadJSON(t, filepath.Join(runDir, "commit-map.json"), &r.commits)
		// Copy the artifacts rather than rebuilding them from a SHA or running the role's shell
		// commands. Repoint origin without fetching: its local tracking refs are part of the evidence.
		for _, pair := range [][2]string{{"workspace", r.workspace}, {"remote.git", r.remote}} {
			src := filepath.Join(runDir, pair[0])
			if out, err := exec.CommandContext(t.Context(), "cp", "-a", src+"/.", pair[1]).CombinedOutput(); err != nil {
				t.Fatalf("copy %s: %v\n%s", src, err, out)
			}
		}
		r.git.Run(r.workspace, "jj", "git", "remote", "set-url", "origin", r.remote)
		return
	}
	r.git.Run(r.workspace, fixturegit.JJInit("", true)...)
	r.git.Run(filepath.Dir(r.remote), fixturegit.InitBare(r.remote, r.base)...)
	r.git.Run(r.workspace, "jj", "git", "remote", "add", "origin", r.remote)
	if world.Git == nil {
		return
	}
	tree := strings.TrimSpace(r.git.Out(r.workspace, "git", "write-tree"))
	pending := slices.Clone(world.Git.Commits)
	for len(pending) > 0 {
		rest := pending[:0]
		for _, commit := range pending {
			args := []string{"git", "commit-tree", tree, "-m", commit.Description}
			for _, parent := range commit.Parents {
				if r.commits[parent] == "" {
					break
				}
				args = append(args, "-p", r.commits[parent])
			}
			if len(args) != 5+2*len(commit.Parents) {
				rest = append(rest, commit)
				continue
			}
			r.commits[commit.ID] = strings.TrimSpace(r.git.Out(r.workspace, args...))
		}
		if len(rest) == len(pending) {
			t.Fatal("fixture commits have missing or cyclic parents")
		}
		pending = rest
	}
	for _, b := range world.Git.Remote.Branches {
		r.push(b.Name, r.sha(b.Head))
	}
	// Git's object store can hold a commit that jj has not imported. Give jj a local
	// ref for the standing commit without giving origin either the ref or its object.
	standing := r.sha(world.Git.Workspace.StandingOn)
	const localRef = "refs/heads/fixture-standing"
	r.git.Run(r.workspace, "git", "update-ref", localRef, standing)
	r.git.Run(r.workspace, "jj", "git", "import")
	r.git.Run(r.workspace, "jj", "new", standing)
	// Keep fixture-standing local: importing a deleted Git ref can abandon that
	// commit in jj, which would silently rebase the working copy onto its parent.
	if got := strings.TrimSpace(r.git.Out(r.workspace, "jj", "log", "-r", "@-", "--no-graph", "-T", "commit_id")); got != standing {
		t.Fatalf("workspace stands on %s, want %s", got, standing)
	}
}
func (r *teardownEffectsRun) push(branch, head string) {
	ref := "refs/heads/" + branch
	r.git.Run(r.workspace, "git", "update-ref", ref, head)
	r.git.Run(r.workspace, "git", "--git-dir", r.remote, "fetch", "--quiet", r.workspace, "+"+ref+":"+ref)
	r.git.Run(r.workspace, "jj", "git", "fetch", "--remote", "origin")
}

func (r *teardownEffectsRun) cli(session string, args ...string) string {
	r.t.Helper()
	shell := cliShell{t: r.t, dir: r.workspace, session: session, base: r.dispatch.base, token: r.dispatch.token}
	out, status := shell.bun(nil, dispatchCLI(r.harness.root), args...)
	if status != 0 {
		r.t.Fatalf("dispatch %s = %d: %s%s", args[0], status, out.stdout, out.stderr)
	}
	return string(out.stdout)
}
func (r *teardownEffectsRun) document(name, body string) {
	r.t.Helper()
	r.dispatch.user(http.MethodPost, "/api/v1/issues/"+r.key+"/artifacts", map[string]string{"name": name, "content": body}, 201, nil)
}
func (r *teardownEffectsRun) handoff(word, session, body string) handoffRecord {
	r.t.Helper()
	status, answer := r.dispatch.agentPost("/api/v1/issues/"+r.key+"/handoffs/"+word, map[string]any{"actor": map[string]string{"kind": "session", "id": session}, "body": body})
	if status != http.StatusCreated {
		r.t.Fatalf("write %s handoff: %d %s", word, status, answer)
	}
	got := r.dispatch.latest(r.key, word)
	r.handoffs[got.ID] = teardownEffectsHandoff{Phase: word, ID: got.ID, Version: got.Version}
	return got
}
func (r *teardownEffectsRun) evidence(id string, answers []teardownEffectsAnswer, differences []teardownEffectsDifference, names []teardownEffectsHandoff) teardownEffectsRecord {
	t := r.t
	t.Helper()
	got := teardownEffectsRecord{ID: id, Answers: append([]teardownEffectsAnswer{}, answers...), Diverged: append([]teardownEffectsDifference{}, differences...), HandoffRecords: append([]teardownEffectsHandoff{}, names...), PhaseBefore: string(r.before), PhaseAfter: string(r.issue(r.key).Phase)}
	if err := store.Transact(t.Context(), r.st.Pool(), func(ctx context.Context, tx pgx.Tx) error {
		check, err := r.records.ProductionCheck(ctx, tx, r.key)
		if err != nil || check == nil {
			return err
		}
		got.ProductionCheck = &teardownEffectsCheck{WakeAt: check.WakeAt}
		rows, err := tx.Query(ctx, `select next_at from outbox where kind='check_wake' and issue=$1 order by id`, r.key)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			if got.ProductionCheck.CheckWakeRow != nil {
				return fmt.Errorf("more than one check_wake row for %s", r.key)
			}
			var due time.Time
			if err := rows.Scan(&due); err != nil {
				return err
			}
			got.ProductionCheck.CheckWakeRow = &teardownEffectsWakeRow{DueAt: due.UTC()}
		}
		return rows.Err()
	}); err != nil {
		t.Fatal(err)
	}
	return got
}

func teardownEffectsReadJSON(t *testing.T, path string, into any) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, into); err != nil {
		t.Fatalf("%s: %v", path, err)
	}
}
func teardownEffectsWriteJSON(t *testing.T, path string, value any) {
	t.Helper()
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	temporary := path + ".partial"
	if err := os.WriteFile(temporary, append(data, '\n'), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(temporary, path); err != nil {
		t.Fatal(err)
	}
}

// Observe actual bytes on the pane's HTTP hop to the real daemon. Boot, registration,
// and asynchronous lifecycle calls are not tool answers.
type teardownEffectsHTTP struct {
	At           time.Time `json:"at"`
	Method, Path string
	Status       int
	Body, Answer json.RawMessage
}
type teardownEffectsProxy struct {
	mu      sync.Mutex
	url     string
	records []teardownEffectsHTTP
}

func teardownEffectsStartProxy(t *testing.T, target string) *teardownEffectsProxy {
	t.Helper()
	dest, err := url.Parse(target)
	if err != nil {
		t.Fatal(err)
	}
	p := &teardownEffectsProxy{}
	forward := httputil.NewSingleHostReverseProxy(dest)
	type requestKey struct{}
	forward.ModifyResponse = func(res *http.Response) error {
		sent, ok := res.Request.Context().Value(requestKey{}).(teardownEffectsHTTP)
		if !ok {
			return nil
		}
		answer, err := io.ReadAll(res.Body)
		res.Body.Close()
		if err != nil {
			return err
		}
		res.Body = io.NopCloser(bytes.NewReader(answer))
		if !json.Valid(answer) {
			answer, _ = json.Marshal(string(answer))
		}
		sent.Answer, sent.Status = answer, res.StatusCode
		p.mu.Lock()
		p.records = append(p.records, sent)
		p.mu.Unlock()
		return nil
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if teardownEffectsToolRoute(req.URL.Path) {
			body, err := io.ReadAll(req.Body)
			if err != nil {
				http.Error(w, "cannot observe request", 400)
				return
			}
			req.Body = io.NopCloser(bytes.NewReader(body))
			req = req.WithContext(context.WithValue(req.Context(), requestKey{}, teardownEffectsHTTP{At: time.Now(), Method: req.Method, Path: req.URL.Path, Body: body}))
		}
		forward.ServeHTTP(w, req)
	}))
	p.url = server.URL
	t.Cleanup(server.Close)
	return p
}
func teardownEffectsToolRoute(path string) bool {
	if !strings.HasPrefix(path, "/legion/v1/") || path == "/legion/v1/grants" {
		return false
	}
	switch path {
	case "/legion/v1/claims/register", "/legion/v1/claims/ready", "/legion/v1/claims/exit", "/legion/v1/claims/turn-outcome", "/legion/v1/claims/activity":
		return false
	}
	return true
}
func (p *teardownEffectsProxy) since(from int) []teardownEffectsHTTP {
	p.mu.Lock()
	defer p.mu.Unlock()
	return slices.Clone(p.records[from:])
}
func (p *teardownEffectsProxy) mark() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.records)
}

// One scripted turn executes one tool at a time. The next request carries the tool result;
// holding it lets the driver inspect durable state before the next recorded action.
type teardownEffectsModel struct {
	*modelStandin
	mu          sync.Mutex
	started     bool
	current     string
	ready, done chan struct{}
	stop        sync.Once
	next        chan map[string]any
	result      chan toolResult
}

func teardownEffectsStartModel(t *testing.T, home, profile string) *teardownEffectsModel {
	t.Helper()
	s := &teardownEffectsModel{modelStandin: &modelStandin{name: "teardowneffects"}, ready: make(chan struct{}), done: make(chan struct{}), next: make(chan map[string]any), result: make(chan toolResult, 1)}
	s.serve(t, s)
	t.Cleanup(s.finish)
	agent := filepath.Join(home, ".omp", "profiles", profile, "agent")
	if err := os.MkdirAll(agent, 0700); err != nil {
		t.Fatal(err)
	}
	writeProofFile(t, filepath.Join(agent, "models.yml"), fmt.Sprintf("providers:\n  %s:\n    baseUrl: %s\n    api: anthropic-messages\n    apiKey: a-literal-key-no-endpoint-checks\n    models:\n      - id: %s%s\n        name: replay\n        contextWindow: 200000\n        maxTokens: 8192\n", s.name, s.url, s.name, standinModelID))
	writeProofFile(t, filepath.Join(agent, "config.yml"), "enabledModels:\n  - "+standinModel(s.name)+"\nstartup:\n  setupWizard: false\nretry:\n  enabled: false\n")
	return s
}
func (s *teardownEffectsModel) finish() { s.stop.Do(func() { close(s.done) }) }
func (s *teardownEffectsModel) await(t *testing.T, ready <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ready:
	case <-time.After(paneReady):
		t.Fatalf("no %s within %s (provider requests: %d)", what, paneReady, s.count())
	}
}
func (s *teardownEffectsModel) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	body, ok := s.take(w, req)
	if !ok {
		return
	}
	if !strings.HasSuffix(req.URL.Path, "/v1/messages") {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"input_tokens":1000}`)
		return
	}
	if isSideQuestion(body) {
		streamAnthropicText(w, s.name, "PROCEEDING")
		return
	}
	var message messagesRequest
	if json.Unmarshal([]byte(body), &message) != nil {
		streamAnthropicText(w, s.name, lateWaitingReply)
		return
	}
	offered := slices.IndexFunc(message.Tools, func(t offeredTool) bool { return strings.TrimLeft(t.Name, "_") == "legion" })
	turn := turnOf(message)
	s.mu.Lock()
	active := false
	if !s.started && offered >= 0 && strings.Contains(blocksText(turn), teardownEffectsTaskMarker) {
		s.started, active = true, true
		close(s.ready)
	}
	for _, block := range turn {
		if block.Type == "tool_result" && s.current != "" && block.ToolUseID == s.current {
			s.current, active = "", true
			s.result <- toolResult{text: blocksText(contentBlocks(block.Content)), isError: block.IsError}
		}
	}
	s.mu.Unlock()
	if !active {
		streamAnthropicText(w, s.name, lateWaitingReply)
		return
	}
	select {
	case <-req.Context().Done():
	case <-s.done:
		streamAnthropicText(w, s.name, lateWaitingReply)
	case input := <-s.next:
		if offered < 0 {
			streamAnthropicText(w, s.name, lateWaitingReply)
			return
		}
		id := "toolu_teardown_" + strconv.FormatInt(time.Now().UnixNano(), 10)
		s.mu.Lock()
		s.current = id
		s.mu.Unlock()
		streamAnthropicToolUse(w, s.name, id, message.Tools[offered].Name, input)
	}
}

func (r *teardownEffectsRun) call(seq int, at time.Time, args map[string]any) ([]teardownEffectsAnswer, []teardownEffectsHTTP, []teardownEffectsHandoff, toolResult) {
	t := r.t
	t.Helper()
	r.clock.advance(at)
	from := r.proxy.mark()
	select {
	case r.model.next <- args:
	case <-time.After(paneReady):
		t.Fatalf("pane did not ask for call %d", seq)
	}
	var result toolResult
	select {
	case result = <-r.model.result:
	case <-time.After(paneReady):
		t.Fatalf("no result of call %d", seq)
	}
	records := r.proxy.since(from)
	op, _ := args["op"].(string)
	var answers []teardownEffectsAnswer
	var handoffs []teardownEffectsHandoff
	for i, res := range records {
		answers = append(answers, teardownEffectsAnswer{Seq: seq + i, At: at.UTC(), By: "daemon", Op: op, teardownEffectsStatus: res.status(), Body: res.Answer})
		var body struct{ HandoffID string }
		_ = json.Unmarshal(res.Body, &body)
		if found, ok := r.handoffs[body.HandoffID]; ok && !slices.Contains(handoffs, found) {
			handoffs = append(handoffs, found)
		}
	}
	body, _ := json.Marshal(map[string]any{"text": result.text, "isError": result.isError})
	answers = append(answers, teardownEffectsAnswer{Seq: seq + len(answers), At: at.UTC(), By: "client", Op: op, Body: body})
	return answers, records, handoffs, result
}
func (x teardownEffectsHTTP) status() teardownEffectsStatus {
	var a struct{ Code *string }
	_ = json.Unmarshal(x.Answer, &a)
	return teardownEffectsStatus{Status: x.Status, Code: a.Code}
}
func teardownEffectsCode(x teardownEffectsHTTP) string {
	if code := x.status().Code; code != nil {
		return *code
	}
	return ""
}

func (h teardownEffectsHarness) controls(t *testing.T, path string) {
	var file struct{ Cases []teardownEffectsCase }
	teardownEffectsReadJSON(t, path, &file)
	if len(file.Cases) == 0 {
		t.Fatal("no cases")
	}
	capture := teardownEffectsCapture{Version: 1}
	capture.Captured.Command = "bash scripts/e2e/teardown-effects.sh --controls --cases " + os.Getenv("LEGION_E2E_TEARDOWN_CASES_LABEL")
	capture.Captured.At = time.Now().UTC()
	capture.Captured.Commit = os.Getenv("LEGION_E2E_TEARDOWN_COMMIT")
	var implementer, check bool
	for _, c := range file.Cases {
		if !t.Run(c.ID, func(t *testing.T) {
			r := h.start(t, c.World, c.Role, c.Task, "")
			if c.World.Daemon.Phase == phase.Implementing && c.Role == "implementer" && !implementer {
				implementer = true
				r.implementerControls(c, &capture)
			}
			if c.World.Daemon.ProductionCheck != nil && !c.World.Daemon.ProductionCheck.Last && !check {
				check = true
				r.commentControl(c)
			}
			r.goodTrace(c, &capture)
		}) {
			t.FailNow()
		}
	}
	if !implementer || !check {
		t.Fatal("controls require an implementer and a non-final production-check case")
	}
	teardownEffectsWriteJSON(t, filepath.Join(h.out, "daemon-answers.json"), capture)
}

func (r *teardownEffectsRun) saveControl(name, id string, answers []teardownEffectsAnswer, handoffs []teardownEffectsHandoff) teardownEffectsRecord {
	got := r.evidence(id, answers, nil, handoffs)
	teardownEffectsWriteJSON(r.t, filepath.Join(r.harness.out, name, "effects.json"), got)
	return got
}
func (r *teardownEffectsRun) implementerControls(c teardownEffectsCase, capture *teardownEffectsCapture) {
	t := r.t
	var input map[string]any
	for _, a := range c.Traces.Good.Actions {
		if a.Op == "handoff_complete" {
			input = teardownEffectsParams(a.Op, a.Args)
			break
		}
	}
	if input == nil || c.World.Git == nil {
		t.Fatal("implementer control needs a handoff_complete and git world")
	}
	head := r.sha(c.World.Git.Workspace.StandingOn)
	r.handoff("implement", r.session, fmt.Sprintf(`{"head":%q}`, head))
	r.step("local-only-implementer-refused", func() {
		answers, requests, handoffs, result := r.call(0, r.clock.Now(), input)
		got := r.saveControl("local-only-implementer-refused", c.Traces.Good.ID, answers, handoffs)
		if !result.isError || !strings.Contains(result.text, "is not on "+r.branch+"@origin") || teardownEffectsCount(requests, teardownEffectsComplete) != 0 || got.PhaseAfter != got.PhaseBefore {
			t.Fatalf("local-only completion: %+v, requests %+v, %s -> %s", result, requests, got.PhaseBefore, got.PhaseAfter)
		}
	})
	r.push(r.branch, head)
	r.step("pushed-without-pull-request-refused", func() {
		answers, requests, handoffs, result := r.call(0, r.clock.Now(), input)
		got := r.saveControl("pushed-without-pull-request-refused", c.Traces.Good.ID, answers, handoffs)
		response := teardownEffectsRequireAnswer(t, requests, 409, "HANDOFF_NO_PULL_REQUEST")
		if !result.isError || got.PhaseAfter != got.PhaseBefore {
			t.Fatalf("no-PR completion: %+v, %s -> %s", result, got.PhaseBefore, got.PhaseAfter)
		}
		// No request field carries whether GitHub has an open PR. Retain this exact control
		// request as evidence, never a wildcard overriding the stand-in's GitHub-dependent rule.
		capture.add("implementer-no-pull-request", response, map[string]any{"commit": head, "handoffId": handoffs[0].ID})
	})
}
func (r *teardownEffectsRun) commentControl(c teardownEffectsCase) {
	r.step("dispatch-comment-names-no-wake", func() {
		for _, a := range c.Traces.Bad.Actions {
			if a.Kind == "dispatch" && a.Command == "comment" {
				r.cli(r.session, "comment", "--issue", r.key, "--body", teardownEffectsString(a.Fields, "body"))
				got := r.saveControl("dispatch-comment-names-no-wake", c.Traces.Bad.ID, nil, nil)
				teardownEffectsNoWake(r.t, got)
				return
			}
		}
		r.t.Fatal("non-final check case has no comment control in its bad trace")
	})
}
func (r *teardownEffectsRun) goodTrace(c teardownEffectsCase, capture *teardownEffectsCapture) {
	t := r.t
	r.step("known-good-"+c.ID, func() {
		var answers []teardownEffectsAnswer
		var handoffs []teardownEffectsHandoff
		completions := 0
		for _, a := range c.Traces.Good.Actions {
			r.clock.advance(a.At)
			switch a.Kind {
			case "push":
				r.push(a.Ref, r.sha(a.Commit))
			case "github":
				r.githubAction(a.Command, a.Method, a.Path, a.Fields)
			case "dispatch":
				switch a.Command {
				case "handoff-write":
					body, _ := json.Marshal(a.Fields["body"])
					r.handoff(teardownEffectsString(a.Fields, "phase"), r.session, r.mapBody(body))
				case "result":
					r.result(a.Fields, r.session)
				case "comment":
					r.cli(r.session, "comment", "--issue", r.key, "--body", teardownEffectsString(a.Fields, "body"))
				default:
					t.Fatalf("unknown trace Dispatch action %s", a.Command)
				}
			case "daemon":
				more, reqs, names, client := r.call(len(answers), a.At, teardownEffectsParams(a.Op, a.Args))
				answers = append(answers, more...)
				handoffs = append(handoffs, names...)
				if a.Op != "handoff_complete" {
					t.Fatalf("no control assertion for op %s", a.Op)
				}
				completions++
				if a.Answer.Code != nil && *a.Answer.Code == "CHECK_WAKE_REQUIRED" {
					res := teardownEffectsRequireAnswer(t, reqs, 400, *a.Answer.Code)
					got := r.saveControl("pending-without-wake-refused", c.Traces.Good.ID, more, names)
					teardownEffectsNoWake(t, got)
					if !client.isError || !strings.Contains(client.text, *a.Answer.Code) {
						t.Fatalf("pending refusal tool result: %+v", client)
					}
					capture.add("check-wake-required", res, map[string]any{"verdict": "pending", "wakeAt": nil, "checkOpenedAt": a.Args["check_opened_at"]})
					fmt.Println("ok pending-without-wake-refused (400 CHECK_WAKE_REQUIRED; no wakeAt or check_wake row)")
					continue
				}
				res := teardownEffectsRequireAnswer(t, reqs, 200, "")
				if client.isError || a.Answer.Code != nil {
					t.Fatalf("unexpected tool result %+v for known-good action %+v", client, a)
				}
				switch {
				case a.Args["verdict"] == "pending" && a.Args["wake_at"] != nil:
					got := r.saveControl("valid-pending-names-wake", c.Traces.Good.ID, more, names)
					wake, err := time.Parse(time.RFC3339Nano, teardownEffectsString(a.Args, "wake_at"))
					if err != nil || got.ProductionCheck == nil || got.ProductionCheck.WakeAt == nil || got.ProductionCheck.CheckWakeRow == nil || !got.ProductionCheck.WakeAt.Equal(wake) || !got.ProductionCheck.CheckWakeRow.DueAt.Equal(wake) || got.PhaseAfter != got.PhaseBefore {
						t.Fatalf("pending: wake and outbox must both equal %s, with phase unchanged: %+v", wake, got)
					}
					capture.add("pending-accepted", res, map[string]any{"verdict": "pending", "wakeAt": a.Args["wake_at"], "checkOpenedAt": a.Args["check_opened_at"]})
					fmt.Println("ok valid-pending-names-wake (200; wakeAt and check_wake due at " + wake.Format(time.RFC3339) + ")")
				case r.role == claim.RoleImplementer && r.before == phase.Implementing:
					got := r.saveControl("pushed-with-open-pull-request-accepted", c.Traces.Good.ID, more, names)
					if got.PhaseAfter == got.PhaseBefore || len(names) != 1 {
						t.Fatalf("accepted implementer did not move phase with its handoff: %+v", got)
					}
					capture.add("implementer-accepted", res, map[string]any{"commit": r.sha(c.World.Git.Workspace.StandingOn), "handoffId": names[0].ID})
					fmt.Printf("ok pushed-with-open-pull-request-accepted (200; %s -> %s)\n", got.PhaseBefore, got.PhaseAfter)
				}
			default:
				t.Fatalf("unsupported trace action %q", a.Kind)
			}
		}
		got := r.saveControl("known-good-"+c.ID, c.Traces.Good.ID, answers, handoffs)
		if completions == 0 {
			t.Fatal("known-good trace made no completion")
		}
		for _, action := range c.Actions {
			effect := action.Effect
			switch effect.Kind {
			case "answered":
				if got.PhaseAfter != effect.PhaseAfter {
					t.Fatalf("%s phaseAfter = %s, want %s", action.Name, got.PhaseAfter, effect.PhaseAfter)
				}
			case "check-wake":
				if got.ProductionCheck == nil || got.ProductionCheck.WakeAt == nil || got.ProductionCheck.CheckWakeRow == nil || !got.ProductionCheck.WakeAt.After(effect.WakeAt.After) || !got.ProductionCheck.WakeAt.Before(effect.WakeAt.Before) || !got.ProductionCheck.CheckWakeRow.DueAt.Equal(*got.ProductionCheck.WakeAt) {
					t.Fatalf("%s has no check_wake in the expected window: %+v", action.Name, got.ProductionCheck)
				}
			}
		}
	})
}
func (c *teardownEffectsCapture) add(name string, res teardownEffectsHTTP, match map[string]any) {
	c.Answers = append(c.Answers, teardownEffectsFixtureAnswer{Name: name, Path: res.Path, Method: res.Method, Match: match, Status: res.Status, Body: res.Answer})
}
func teardownEffectsParams(op string, args map[string]any) map[string]any {
	params := make(map[string]any, len(args)+1)
	for k, v := range args {
		params[k] = v
	}
	params["op"] = op
	return params
}
func teardownEffectsString(fields map[string]any, key string) string {
	text, _ := fields[key].(string)
	return text
}
func teardownEffectsCount(records []teardownEffectsHTTP, path string) int {
	n := 0
	for _, res := range records {
		if res.Path == path {
			n++
		}
	}
	return n
}
func teardownEffectsRequireAnswer(t *testing.T, records []teardownEffectsHTTP, status int, code string) teardownEffectsHTTP {
	t.Helper()
	var found []teardownEffectsHTTP
	for _, res := range records {
		if res.Path == teardownEffectsComplete {
			found = append(found, res)
		}
	}
	if len(found) != 1 || found[0].Status != status || teardownEffectsCode(found[0]) != code {
		for _, res := range found {
			t.Logf("completion: %d %s", res.Status, res.Answer)
		}
		t.Fatalf("got %d completion responses, want one %d %s", len(found), status, code)
	}
	return found[0]
}
func teardownEffectsNoWake(t *testing.T, got teardownEffectsRecord) {
	t.Helper()
	if got.PhaseBefore != got.PhaseAfter || got.ProductionCheck == nil || got.ProductionCheck.WakeAt != nil || got.ProductionCheck.CheckWakeRow != nil {
		t.Fatalf("expected same phase and no wakeAt or check_wake: %+v", got)
	}
}

func (h teardownEffectsHarness) replay(t *testing.T, root string) {
	dirs, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	var runs []string
	if _, err := os.Stat(filepath.Join(root, "outcome.json")); err == nil {
		runs = append(runs, root)
	} else {
		for _, dir := range dirs {
			if dir.IsDir() {
				runs = append(runs, filepath.Join(root, dir.Name()))
			}
		}
	}
	if len(runs) == 0 {
		t.Fatal("no run directories")
	}
	for _, dir := range runs {
		t.Run(filepath.Base(dir), func(t *testing.T) {
			var outcome struct{ ID string }
			teardownEffectsReadJSON(t, filepath.Join(dir, "outcome.json"), &outcome)
			if !teardownEffectsUUID.MatchString(outcome.ID) {
				t.Fatalf("invalid outcome id %q", outcome.ID)
			}
			var world teardownEffectsWorld
			teardownEffectsReadJSON(t, filepath.Join(dir, "world.json"), &world)
			var launch struct {
				Role  string
				Plain bool
			}
			teardownEffectsReadJSON(t, filepath.Join(dir, "launch.json"), &launch)
			world.Clock = teardownEffectsReadTime(t, filepath.Join(dir, "clock.txt"))
			realStart := teardownEffectsReadTime(t, filepath.Join(dir, "clock-real.txt"))
			calls, err := teardownEffectsReadCalls(filepath.Join(dir, "sessions"))
			if err != nil {
				t.Fatal(err)
			}
			old := teardownEffectsReadLines[teardownEffectsHTTP](t, filepath.Join(dir, "daemon.log"))
			if launch.Plain {
				t.Fatal("a plain session has no Legion claim to replay")
			}
			r := h.start(t, world, launch.Role, "", dir)
			session := ""
			if len(calls) > 0 {
				session = calls[0].Session
			}
			r.restoreDispatch(dir, session, true)
			var answers []teardownEffectsAnswer
			var differences []teardownEffectsDifference
			var names []teardownEffectsHandoff
			for _, call := range calls {
				if call.Session != session {
					t.Fatal("cannot attribute legion calls from more than one claim session")
				}
				seq := len(answers)
				more, replies, handoffs, _ := r.call(seq, world.Clock.Add(call.At.Sub(realStart)), call.Args)
				answers = append(answers, more...)
				names = append(names, handoffs...)
				var standin []teardownEffectsHTTP
				for _, res := range old {
					if teardownEffectsToolRoute(res.Path) && !res.At.Before(call.At) && (call.ResultAt.IsZero() || !res.At.After(call.ResultAt)) {
						standin = append(standin, res)
					}
				}
				for i := range max(len(standin), len(replies)) {
					var a, b teardownEffectsHTTP
					if i < len(standin) {
						a = standin[i]
					}
					if i < len(replies) {
						b = replies[i]
					}
					if !teardownEffectsSameAnswer(a, b) {
						differences = append(differences, teardownEffectsDifference{Seq: min(seq+i, len(answers)-1), Standin: a.status(), Daemon: b.status()})
					}
				}
			}
			got := r.evidence(outcome.ID, answers, differences, names)
			teardownEffectsWriteJSON(t, filepath.Join(h.out, outcome.ID, "effects.json"), got)
			if len(differences) > 0 {
				t.Errorf("DIVERGED %s: %d stand-in and real daemon answers differ", outcome.ID, len(differences))
			} else {
				fmt.Println("ok replay " + outcome.ID)
			}
		})
	}
}

var teardownEffectsUUID = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// teardownEffectsSameAnswer reports whether the real daemon answered a request as the stand-in
// did. A run diverges on an acceptance against a refusal, or one refusal against another (plan
// T4, lane D step 3): the route and the status must match, and a refusal's code too. An accepted
// answer's body is not compared, since the stand-in's simpler records say less than the daemon's.
func teardownEffectsSameAnswer(a, b teardownEffectsHTTP) bool {
	if a.Status != b.Status || a.Method != b.Method || a.Path != b.Path {
		return false
	}
	if a.Status < 400 {
		return true
	}
	return teardownEffectsRefusalCode(a.Answer) == teardownEffectsRefusalCode(b.Answer)
}

// teardownEffectsRefusalCode is the `code` of a refusal's JSON body, or the whole body where it is
// no JSON object naming one.
func teardownEffectsRefusalCode(body []byte) string {
	var refusal struct {
		Code string `json:"code"`
	}
	if json.Unmarshal(body, &refusal) != nil || refusal.Code == "" {
		return string(body)
	}
	return refusal.Code
}
func teardownEffectsReadTime(t *testing.T, path string) time.Time {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	at, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(string(b)))
	if err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	return at
}
func teardownEffectsReadLines[T any](t *testing.T, path string) []T {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var out []T
	if err := teardownEffectsLines(f, func(line []byte) error {
		var item T
		if err := json.Unmarshal(line, &item); err != nil {
			return err
		}
		out = append(out, item)
		return nil
	}); err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	return out
}

func teardownEffectsIssueCounter(t *testing.T, d *handoffDispatch, project string, number int) {
	t.Helper()
	db, err := pgx.Connect(t.Context(), d.database)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close(context.Background())
	if tag, err := db.Exec(t.Context(), `update projects set next_number=$1 where key=$2`, number, project); err != nil || tag.RowsAffected() != 1 {
		t.Fatalf("set scratch Dispatch issue counter: %v (%s)", err, tag)
	}
}

func (r *teardownEffectsRun) mapBody(body json.RawMessage) string {
	var object map[string]any
	if err := json.Unmarshal(body, &object); err != nil || object == nil {
		r.t.Fatalf("invalid handoff body: %s", body)
	}
	if head := teardownEffectsString(object, "head"); head != "" {
		object["head"] = r.sha(head)
	}
	data, err := json.Marshal(object)
	if err != nil {
		r.t.Fatal(err)
	}
	return string(data)
}

func (r *teardownEffectsRun) result(fields map[string]any, session string) {
	r.t.Helper()
	args := []string{"result", "--issue", r.key}
	for _, field := range []string{"check", "role", "verdict", "commit", "surface", "run", "observed", "negative-control"} {
		value := teardownEffectsString(fields, field)
		if field == "commit" {
			value = r.sha(value)
		}
		if value != "" {
			args = append(args, "--"+field, value)
		}
	}
	r.cli(session, args...)
}

// Dispatch creates new artifact and handoff IDs. Preserve the order and authors of
// handoffs; only the original claim's author is rebound to the new pane's session.
func (r *teardownEffectsRun) restoreDispatch(dir, session string, withHandoffs bool) {
	t := r.t
	t.Helper()
	var snapshot struct {
		Documents []struct{ Name, Kind, Text string }
		Handoffs  []handoffRecord
		Comments  []struct {
			Body   string
			Author struct{ Kind, ID string }
		}
		Asks []struct {
			Question, State string
			Options         json.RawMessage
			Author          struct{ Kind, ID string }
		}
		Results struct {
			Checks []struct {
				ID      string `json:"block_id"`
				Results []map[string]any
			}
		}
	}
	teardownEffectsReadJSON(t, filepath.Join(dir, "dispatch-readback.json"), &snapshot)
	// The handoffs other sessions wrote go in with the rest of the world, before the issue takes
	// its status: Dispatch refuses a handoff on a closed issue (ISSUE_CLOSED). The run's own go in
	// once the replay's pane has registered, rebound to its session.
	slices.SortStableFunc(snapshot.Handoffs, func(a, b handoffRecord) int { return a.Version - b.Version })
	for _, old := range snapshot.Handoffs {
		own := session != "" && old.Author.ID == session
		if own != withHandoffs {
			continue
		}
		author := old.Author.ID
		if own {
			author = r.session
		}
		got := r.handoff(old.Phase, author, old.Body)
		if got.Version != old.Version {
			t.Fatalf("restored %s version %d, want %d", old.Phase, got.Version, old.Version)
		}
	}
	if withHandoffs {
		return
	}
	prefix := "/api/v1/issues/" + r.key
	for _, doc := range snapshot.Documents {
		if doc.Kind == "doc" && doc.Name != "spec.md" {
			r.document(doc.Name, doc.Text)
		}
	}
	for _, comment := range snapshot.Comments {
		switch comment.Author.Kind {
		case "system":
			// Dispatch's own comment, such as the parent-status rule's on a seeded status change:
			// Dispatch writes it again where the restored writes call for it, and no caller can.
			continue
		case "user":
			r.dispatch.user(http.MethodPost, prefix+"/comments", map[string]any{"body": comment.Body}, http.StatusCreated, nil)
			continue
		}
		status, body := r.dispatch.agentPost(prefix+"/comments", map[string]any{"actor": comment.Author, "body": comment.Body})
		if status != 201 {
			t.Fatalf("restore comment: %d %s", status, body)
		}
	}
	for _, ask := range snapshot.Asks {
		if ask.State != "" && ask.State != "open" {
			t.Fatalf("cannot restore %s ask without its transition record", ask.State)
		}
		status, body := r.dispatch.agentPost(prefix+"/asks", map[string]any{"actor": ask.Author, "question": ask.Question, "options": ask.Options})
		if status != 201 {
			t.Fatalf("restore ask: %d %s", status, body)
		}
	}
	for _, check := range snapshot.Results.Checks {
		for _, result := range check.Results {
			result["check"] = check.ID
			result["commit"] = result["commit_sha"]
			result["negative-control"] = result["negative_control"]
			r.result(result, "teardown-results")
		}
	}
}

type teardownEffectsGitHub struct {
	t          *testing.T
	mu         sync.Mutex
	repo, base string
	pulls      map[int]teardownEffectsPull
}

func (g *teardownEffectsGitHub) forBranch(branch string) (teardownEffectsPull, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	var best teardownEffectsPull
	for _, pull := range g.pulls {
		if pull.Head.Ref == branch && pull.Number > best.Number {
			best = pull
		}
	}
	return best, best.Number != 0
}
func (g *teardownEffectsGitHub) serve(w http.ResponseWriter, req *http.Request) {
	g.mu.Lock()
	defer g.mu.Unlock()
	send := func(x any) {
		if err := json.NewEncoder(w).Encode(x); err != nil {
			g.t.Errorf("GitHub fixture response: %v", err)
		}
	}
	if req.Method == http.MethodPost && req.URL.Path == "/graphql" {
		numbers, _ := liveBatchQuery(req)
		nodes := map[int]map[string]any{}
		for n, pull := range g.pulls {
			node := liveBatchNode(n, pull.Head.SHA, pull.Head.Ref, pull.Base, true, "clean")
			node["state"] = strings.ToUpper(pull.State)
			if pull.State == "merged" {
				node["mergedAt"] = "2026-01-01T00:00:00Z"
				node["mergeCommit"] = map[string]string{"oid": pull.MergeCommit}
			}
			nodes[n] = node
		}
		_, _ = io.WriteString(w, liveBatchAnswer(g.t, numbers, nodes))
		return
	}
	if req.Method == http.MethodGet {
		prefix := "/repos/" + g.repo
		if req.URL.Path == prefix {
			send(map[string]any{"full_name": g.repo, "default_branch": g.base})
			return
		}
		if req.URL.Path == prefix+"/pulls" {
			pulls := []any{}
			_, branch, _ := strings.Cut(req.URL.Query().Get("head"), ":")
			for _, pull := range g.pulls {
				if (branch == "" || pull.Head.Ref == branch) && (req.URL.Query().Get("state") != "open" || pull.State == "open") {
					pulls = append(pulls, pull.rest())
				}
			}
			send(pulls)
			return
		}
		if rest, ok := strings.CutPrefix(req.URL.Path, prefix+"/pulls/"); ok {
			number, sub, _ := strings.Cut(rest, "/")
			n, _ := strconv.Atoi(number)
			if pull, ok := g.pulls[n]; ok {
				switch sub {
				case "":
					send(pull.rest())
					return
				case "reviews":
					reviews := []any{}
					for i, review := range pull.Reviews {
						reviews = append(reviews, map[string]any{"id": i + 1, "state": review.State, "user": map[string]string{"login": review.Author}, "body": review.Body, "commit_id": review.Commit, "submitted_at": review.At})
					}
					send(reviews)
					return
				}
			}
		}
	}
	w.WriteHeader(http.StatusNotFound)
	send(map[string]string{"message": "Not Found"})
}
func (p teardownEffectsPull) rest() map[string]any {
	state := p.State
	if state == "merged" {
		state = "closed"
	}
	return map[string]any{"number": p.Number, "state": state, "draft": p.Draft, "body": p.Body, "head": map[string]string{"ref": p.Head.Ref, "sha": p.Head.SHA},
		"base": map[string]string{"ref": p.Base}, "merged": p.State == "merged", "merge_commit_sha": p.MergeCommit, "mergeable": true, "mergeable_state": "clean"}
}

func (r *teardownEffectsRun) githubAction(command, method, path string, fields map[string]any) {
	g := r.github
	g.mu.Lock()
	defer g.mu.Unlock()
	number := 0
	if n, ok := fields["number"].(float64); ok {
		number = int(n)
	}
	if number == 0 {
		if _, suffix, ok := strings.Cut(path, "/pulls/"); ok {
			number, _ = strconv.Atoi(strings.Split(suffix, "/")[0])
		}
	}
	switch {
	case command == "gh pr create" || method == "POST" && strings.HasSuffix(path, "/pulls"):
		if number == 0 {
			for n := range g.pulls {
				number = max(number, n)
			}
			number++
		}
		pull := teardownEffectsPull{Number: number, State: "open", Base: teardownEffectsString(fields, "base"), Body: teardownEffectsString(fields, "body")}
		if pull.Base == "" {
			pull.Base = r.base
		}
		pull.Head.Ref = teardownEffectsString(fields, "head")
		if pull.Head.Ref == "" {
			pull.Head.Ref = r.branch
		}
		pull.Head.SHA = strings.TrimSpace(r.git.Out(r.workspace, "git", "--git-dir", r.remote, "rev-parse", "refs/heads/"+pull.Head.Ref))
		g.pulls[number] = pull
	case command == "gh pr edit" || method == "PATCH" && strings.Contains(path, "/pulls/"):
		if pull, ok := g.pulls[number]; ok {
			pull.Body = teardownEffectsString(fields, "body")
			if state := teardownEffectsString(fields, "state"); state != "" {
				pull.State = state
			}
			g.pulls[number] = pull
		}
	case command == "gh pr merge" || method == "PUT" && strings.HasSuffix(path, "/merge"):
		if pull, ok := g.pulls[number]; ok {
			pull.State = "merged"
			pull.MergeCommit = strings.TrimSpace(r.git.Out(r.workspace, "git", "--git-dir", r.remote, "rev-parse", "refs/heads/"+pull.Base))
			g.pulls[number] = pull
		}
	case command == "gh pr close":
		if pull, ok := g.pulls[number]; ok {
			pull.State = "closed"
			g.pulls[number] = pull
		}
	case command == "gh pr review" || method == "POST" && strings.HasSuffix(path, "/reviews"):
		if pull, ok := g.pulls[number]; ok {
			state := teardownEffectsString(fields, "event")
			switch state {
			case "APPROVE":
				state = "APPROVED"
			case "REQUEST_CHANGES":
				state = "CHANGES_REQUESTED"
			default:
				state = "COMMENTED"
			}
			pull.Reviews = append(pull.Reviews, struct {
				Author, State, Commit, Body string
				At                          time.Time
			}{Author: "legion-reviewer[bot]", State: state, Commit: pull.Head.SHA, Body: teardownEffectsString(fields, "body"), At: r.clock.Now()})
			g.pulls[number] = pull
		}
	}
}

func (r *teardownEffectsRun) rebuildGitHub(runDir string) {
	for _, call := range teardownEffectsReadLines[struct {
		Argv  []string
		Exit  int
		Body  *string
		Files map[string]string
	}](r.t, filepath.Join(runDir, "calls.jsonl")) {
		if call.Exit != 0 || len(call.Argv) < 2 {
			continue
		}
		args := call.Argv
		command, method, path := "", "", ""
		fields := map[string]any{}
		if args[0] == "pr" {
			command = "gh pr " + args[1]
			args = args[2:]
			if len(args) > 0 {
				if n, err := strconv.Atoi(args[0]); err == nil {
					fields["number"] = float64(n)
					args = args[1:]
				}
			}
		} else if args[0] == "api" {
			method = "GET"
			args = args[1:]
		} else {
			continue
		}
		for len(args) > 0 {
			flag := args[0]
			args = args[1:]
			if key, value, ok := strings.Cut(flag, "="); ok && strings.HasPrefix(key, "--") {
				flag = key
				args = append([]string{value}, args...)
			}
			if flag == "--approve" {
				fields["event"] = "APPROVE"
				continue
			}
			if flag == "--request-changes" {
				fields["event"] = "REQUEST_CHANGES"
				continue
			}
			if !strings.HasPrefix(flag, "-") && path == "" {
				path = strings.TrimPrefix(flag, "/")
				continue
			}
			if len(args) == 0 {
				continue
			}
			value := args[0]
			switch flag {
			case "-X", "--method":
				method = value
			case "--head", "--base", "--body":
				fields[strings.TrimPrefix(flag, "--")] = value
			case "--body-file":
				fields["body"] = call.Files[value]
			case "--input":
				if err := json.Unmarshal([]byte(call.Files[value]), &fields); err != nil {
					r.t.Fatalf("read GitHub request input: %v", err)
				}
			case "-f", "-F", "--raw-field", "--field":
				if key, text, ok := strings.Cut(value, "="); ok {
					if name, ok := strings.CutPrefix(text, "@"); ok && flag != "-f" && flag != "--raw-field" {
						text = call.Files[name]
					}
					fields[key] = text
				}
			default:
				continue
			}
			args = args[1:]
		}
		if call.Body != nil {
			fields["body"] = *call.Body
		}
		r.githubAction(command, method, path, fields)
	}
}
