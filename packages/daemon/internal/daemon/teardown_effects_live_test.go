//go:build e2e

package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"maps"
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

// teardownEffectsRun is one booted daemon and pane on a case's starting state. fail ends the run
// on a failure of its setup, which is a harness failure for a replay.
type teardownEffectsRun struct {
	liveAgentProof
	harness                                                         teardownEffectsHarness
	clock                                                           *teardownEffectsClock
	dispatch                                                        *handoffDispatch
	github                                                          *teardownEffectsGitHub
	proxy                                                           *teardownEffectsProxy
	model                                                           *teardownEffectsModel
	key, branch, repo, base, workspace, remote, remoteName, session string
	role                                                            claim.Role
	token                                                           claim.Token
	git                                                             fixturegit.Runner
	commits                                                         map[string]string
	handoffs                                                        map[string]teardownEffectsHandoff
	before                                                          phase.Phase
	fail                                                            func(error)
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

// start boots the whole daemon on the case's starting state: world's Git, its pull requests and its
// Dispatch records. A replay (plan, from runDir) seeds Dispatch from the run's own record of that
// state and builds its Git with lane R's builder; harness, given, ends the run on a setup failure.
func (h teardownEffectsHarness) start(t *testing.T, world teardownEffectsWorld, role, task string, plan *teardownEffectsPlan, runDir string, harness func(error)) *teardownEffectsRun {
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
	case "headless-controller":
		// The project's controller, which this daemon launches itself (`controller: daemon`): its
		// claim holds no issue, and its token is the project's (claim.ControllerToken).
		roleOfClaim = claim.RoleController
		cfg.ControllerLaunch = config.ControllerLaunchDaemon
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
	token := claim.ControllerToken(projectToken)
	if roleOfClaim != claim.RoleController {
		if token, err = claim.NewToken(projectToken, key, roleOfClaim); err != nil {
			t.Fatal(err)
		}
	}
	st, err := store.Open(t.Context(), cfg.PostgresDSN)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(st.Close)
	proof := liveProof{t: t, cfg: cfg, st: st, records: record.NewStore(), project: projectToken}
	r := &teardownEffectsRun{liveAgentProof: liveAgentProof{liveProof: proof, legion: h.legion}, harness: h, dispatch: dispatch, key: key, branch: "legion/" + key, role: roleOfClaim, token: token,
		clock: &teardownEffectsClock{now: world.Clock}, git: fixturegit.Isolated(t, h.home), commits: map[string]string{}, handoffs: map[string]teardownEffectsHandoff{}, repo: liveRepo, base: "main", remoteName: "origin", fail: harness}
	if harness == nil {
		r.fail = func(err error) { t.Helper(); t.Fatal(err) }
	}
	if world.Git != nil && world.Git.Remote.Name != "" {
		r.remoteName = world.Git.Remote.Name
	}
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
	githubURL := githubtest.Server(t, githubtest.Spec{Handle: r.github.serve})
	r.setupIssue(world)
	if plan != nil {
		// The seeded records go in before the issue takes its status: Dispatch refuses a handoff on a
		// closed issue (ISSUE_CLOSED).
		for _, c := range plan.Seed {
			if err := r.applyDispatch(*c.event, "", &plan.Readback); err != nil {
				r.fail(fmt.Errorf("seed %s (%s) of the case's starting state: %w", c.Source, c.What, err))
			}
		}
	} else {
		for _, doc := range world.Dispatch.Documents {
			r.document(doc.Name, doc.Body)
		}
		for _, handoff := range world.Dispatch.Handoffs {
			r.handoff(handoff.Phase, handoff.Author, r.mapBody(handoff.Body))
		}
	}
	if world.Dispatch.Issue.Parent == "" {
		r.dispatch.user(http.MethodPatch, "/api/v1/issues/"+r.key, map[string]any{"status": world.Dispatch.Issue.Status, "labels": []string{"legion"}}, http.StatusOK, nil)
	} else {
		// As lane R's seed sets a child's status (seed.ts seedCaseDispatch): each ancestor not in
		// work moves to in_progress with it, as Dispatch keeps a parent in line with its children.
		if world.Dispatch.Issue.Status != "triage" {
			r.dispatch.user(http.MethodPost, "/api/v1/issues/"+r.key+"/status-change", map[string]any{"status": world.Dispatch.Issue.Status, "include_parent": true}, http.StatusOK, nil)
		}
		// The parent carries Legion's label too, as the root handed over that seed records it as.
		for _, labelled := range []string{world.Dispatch.Issue.Parent, r.key} {
			r.dispatch.user(http.MethodPatch, "/api/v1/issues/"+labelled, map[string]any{"labels": []string{"legion"}}, http.StatusOK, nil)
		}
	}
	r.seed(world, task)
	o := r.rt.daemonOverrides(r.rt)
	o.clock, o.githubAPI, o.workflowTokens = r.clock, githubURL, &workflowTokenRecorder{}
	r.d = startDaemonWithin(t, cfg, o, r.log(), 2*time.Minute)
	if got := r.issue(key).Phase; got != world.Daemon.Phase {
		r.fail(fmt.Errorf("boot changed %s from %s to %s", key, world.Daemon.Phase, got))
	}
	r.before = world.Daemon.Phase
	r.model.await(t, r.model.ready, "the real pane's first turn")
	r.session = r.claim(token).Session
	if r.session == "" {
		t.Fatal("the pane did not register a session")
	}
	t.Cleanup(r.model.finish)
	// A child stands in its parent's tree, as it does in Dispatch, and that tree runs.
	if parent := world.Dispatch.Issue.Parent; parent != "" {
		if s := r.state(); s.Tree != parent || s.Parent != parent || !s.TreeRuns {
			r.fail(fmt.Errorf("the replay holds %s in tree %q (running: %t) under the Dispatch parent %q, and the case's world names the parent %s", key, s.Tree, s.TreeRuns, s.Parent, parent))
		}
	}
	return r
}

// setupIssue files the case's issue in the scratch Dispatch under its own key and, where the world
// names one, under its parent, as lane R's seed files them (seed.ts seedCaseDispatch): the parent
// first, titled as lane R titles it, then the issue naming it as its parent, so a child is no root
// here either. A parent in another project, or one that follows its child, is not set up.
func (r *teardownEffectsRun) setupIssue(world teardownEffectsWorld) {
	project := r.cfg.Project
	r.dispatch.user(http.MethodPost, "/api/v1/projects", map[string]string{"key": project, "name": "Teardown effects"}, 201, nil)
	r.dispatch.user(http.MethodPut, "/api/v1/projects/"+project+"/default-repository", map[string]string{"repo": r.repo}, 200, nil)
	fields := map[string]any{"title": world.Dispatch.Issue.Title, "spec": world.Dispatch.Issue.Spec}
	if parent := world.Dispatch.Issue.Parent; parent != "" {
		if !strings.HasPrefix(parent, project+"-") || r.issueNumber(parent) >= r.issueNumber(r.key) {
			r.fail(fmt.Errorf("the case's issue %s has the parent %s, which is in another project or follows it, and which the replay does not set up", r.key, parent))
		}
		r.fileIssue(parent, map[string]any{"title": "Parent of " + r.key})
		fields["parent"] = parent
	}
	r.fileIssue(r.key, fields)
}

// issueNumber is the number of key, an issue of the run's project.
func (r *teardownEffectsRun) issueNumber(key string) int {
	r.t.Helper()
	number, err := strconv.Atoi(strings.TrimPrefix(key, r.cfg.Project+"-"))
	if err != nil || number < 1 {
		r.t.Fatalf("invalid issue number: %s", key)
	}
	return number
}

// fileIssue files key in the scratch Dispatch with fields. Dispatch allocates the key: its counter
// is set in this disposable database so the replay does not change the key embedded in branch
// names, prompts and the original requests.
func (r *teardownEffectsRun) fileIssue(key string, fields map[string]any) {
	t := r.t
	t.Helper()
	teardownEffectsIssueCounter(t, r.dispatch, r.cfg.Project, r.issueNumber(key))
	fields["project"], fields["force"] = r.cfg.Project, true
	var issue struct{ Key string }
	r.dispatch.user(http.MethodPost, "/api/v1/issues", fields, http.StatusCreated, &issue)
	if issue.Key != key {
		t.Fatalf("created issue %s, want %s", issue.Key, key)
	}
}

// seed records the case's daemon state. A child (the world names its parent) stands in its parent's
// tree, as lane R's daemon stand-in holds it (`--tree`, the parent): its record, its claim and its
// production check name that tree, it was not handed over itself, and the tree's lifecycle stands
// open under the workflow. The world holds nothing of the parent's own daemon state, so the parent
// is recorded with the least that makes its tree run, without which admission takes the child's
// tree for one that finished and releases its slot (admit's releaseSlots): a root handed over,
// in Dispatch's status, at admitted, with no slot, no claim and no phases row, so no worker of it is
// owed a start (admit's owedOf) and it lingers nowhere. A controller case's issue that is a child
// is not seeded: the daemon knows that issue and has not admitted it, and under a running tree a
// child the daemon has not admitted would be admitted into it.
func (r *teardownEffectsRun) seed(world teardownEffectsWorld, task string) {
	t := r.t
	t.Helper()
	if task == "" {
		task = "Continue " + world.Dispatch.Issue.Title + ". Issue: " + r.key + ". Phase: " + string(world.Daemon.Phase) + "."
	}
	listed := r.dispatch.issue(r.key)
	now := r.clock.Now()
	delivery := supervise.Delivery{ID: "teardown-" + randomSuffix(t), Task: teardownEffectsTaskMarker + "\n" + task, Generation: 1, Phase: world.Daemon.Phase, QueuedAt: now}
	// The controller's claim holds no issue, no step and no tree: its case's issue is one the
	// daemon knows and has not admitted, and its wake is a delivery to the controller's claim,
	// which the daemon's keeper launches at boot and hands its start message after this one.
	controller := r.role == claim.RoleController
	if controller {
		delivery.Phase = ""
	}
	tree, parent := r.key, world.Dispatch.Issue.Parent
	if parent != "" {
		if controller {
			r.fail(fmt.Errorf("the controller case's issue %s is a child of %s, which the replay does not seed", r.key, parent))
		}
		tree = parent
	}
	var root struct {
		Title, Rank, Status string
		LastSeq             int64 `json:"last_seq"`
	}
	if parent != "" {
		r.dispatch.agentGet("/api/v1/issues/"+parent, &root)
	}
	ctx := t.Context()
	if err := store.Transact(ctx, r.st.Pool(), func(ctx context.Context, tx pgx.Tx) error {
		if parent != "" {
			if err := r.records.PutIssue(ctx, tx, record.Issue{Key: parent, Title: root.Title, Project: r.cfg.Project, Tree: parent, Repo: r.repo, GitHubRepo: r.repo, DefaultBranch: r.base, Phase: phase.Admitted,
				Generation: 1, Status: root.Status, DispatchStatus: root.Status, Rank: root.Rank, LastDispatchSeq: root.LastSeq, HandedOver: true}); err != nil {
				return err
			}
		}
		issue := record.Issue{Key: r.key, Title: listed.Title, Project: r.cfg.Project, Tree: tree, Repo: r.repo, GitHubRepo: r.repo, DefaultBranch: r.base, Phase: world.Daemon.Phase, Generation: 1,
			Status: world.Dispatch.Issue.Status, DispatchStatus: world.Dispatch.Issue.Status, Rank: listed.Rank, LastDispatchSeq: listed.LastSeq, HandedOver: parent == ""}
		if parent != "" {
			issue.Parent = &parent
		}
		// A tree lingers after its root closes: the linger is the root's, never a child's.
		if issue.Phase == phase.Done && parent == "" {
			linger := now.Add(48 * time.Hour)
			if wall := time.Now().Add(time.Hour); linger.Before(wall) {
				linger = wall
			}
			issue.LingerUntil = &linger
		}
		if err := r.records.PutIssue(ctx, tx, issue); err != nil {
			return err
		}
		if step := world.Daemon.Step; step != nil && !controller {
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
			if err := r.records.PutProductionCheck(ctx, tx, record.ProductionCheck{Issue: r.key, Project: r.cfg.Project, Tree: tree, Repo: r.repo, Number: number, MergeSHA: r.sha(check.MergeCommit),
				Role: r.role, OpenedAt: check.OpenedAt, BoundAt: check.BoundAt, Looks: check.Look, LookOpen: true, LookWakeAt: &now, Last: check.Last}); err != nil {
				return err
			}
		}
		if issue.Phase != phase.Done && !controller {
			return r.admit(ctx, tx, r.key)
		}
		return nil
	}); err != nil {
		t.Fatalf("seed daemon state: %v", err)
	}
	c := supervise.Claim{Token: r.token, Project: r.project, Tree: tree, Issue: r.key, Role: r.role, Generation: 1, State: supervise.StateLaunching, BootTokenHash: supervise.HashBootToken("seed-" + randomSuffix(t))}
	if delivery.CheckLook.Look == 0 {
		c.ServingGeneration = 1
	}
	if controller {
		c.Tree, c.Issue = "", ""
		if err := r.st.PutClaim(ctx, c); err != nil {
			t.Fatal(err)
		}
	} else {
		if _, err := r.st.OpenTreeLifecycle(ctx, r.project, tree, treelifecycle.AuthorityWorkflow); err != nil {
			t.Fatal(err)
		}
		if _, err := r.st.AdmitClaim(ctx, c); err != nil {
			t.Fatal(err)
		}
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
		r.buildCaseGit(runDir)
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

// buildCaseGit builds the case's starting Git as lane R built it, with lane R's own builder
// (git-world.ts build), so each commit keeps its id, parents and tree; the commit map it writes must
// be the run's commit-map.json. The run's final workspace and remote are never copied in: they hold
// what the run left. The objects of every commit the run's workspace stood on (workspace.pack, which
// git-world.ts record writes) go into the workspace's object store and nowhere else, as sources of
// the commits the run made: a commit there is on no ref, so neither jj nor the remote sees it until
// a recorded move (applyWorkspace) or push (applyPush) makes it seen.
func (r *teardownEffectsRun) buildCaseGit(runDir string) {
	t := r.t
	t.Helper()
	built, home := t.TempDir(), t.TempDir()
	command := exec.CommandContext(t.Context(), "bun", filepath.Join(r.harness.root, "packages", "pi-legion", "scripts", "skill-scenarios", "git-world.ts"), "build", built, filepath.Join(runDir, "world.json"))
	command.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + home, "TMPDIR=" + home}
	if out, err := command.CombinedOutput(); err != nil {
		r.fail(fmt.Errorf("build the case's starting Git with git-world.ts: %v\n%s", err, out))
	}
	var made, recorded map[string]string
	teardownEffectsReadJSON(t, filepath.Join(built, "commit-map.json"), &made)
	teardownEffectsReadJSON(t, filepath.Join(runDir, "commit-map.json"), &recorded)
	if !maps.Equal(made, recorded) {
		r.fail(fmt.Errorf("the case's starting Git has the commits %v, and the run's commit-map.json names %v: the run did not start from this world", made, recorded))
	}
	r.commits = made
	for _, pair := range [][2]string{{"workspace", r.workspace}, {"remote.git", r.remote}} {
		if out, err := exec.CommandContext(t.Context(), "cp", "-a", filepath.Join(built, pair[0])+"/.", pair[1]).CombinedOutput(); err != nil {
			t.Fatalf("copy the built %s: %v\n%s", pair[0], err, out)
		}
	}
	// The builder's push recorder writes outside the copied remote; replay.json names each push the
	// replay applies, and the replay pushes nothing of its own.
	if err := os.Remove(filepath.Join(r.remote, "hooks", "post-receive")); err != nil {
		t.Fatal(err)
	}
	r.git.Run(r.workspace, "jj", "git", "remote", "set-url", r.remoteName, r.remote)
	pack, err := os.Open(filepath.Join(runDir, "workspace.pack"))
	if os.IsNotExist(err) {
		return
	}
	if err != nil {
		t.Fatal(err)
	}
	defer pack.Close()
	unpack := exec.CommandContext(t.Context(), "git", "unpack-objects", "-q")
	unpack.Dir, unpack.Env, unpack.Stdin = r.workspace, r.git.Env, pack
	if out, err := unpack.CombinedOutput(); err != nil {
		r.fail(fmt.Errorf("load workspace.pack into the replay's workspace: %v\n%s", err, out))
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
			r := h.start(t, c.World, c.Role, c.Task, nil, "", nil)
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

// replay drives each run of root (a run directory, or a directory of them) on the real daemon from
// the case's starting state. Before each of the run's legion calls it applies the Git, GitHub and
// Dispatch changes the run recorded before that call (teardownEffectsOrder), then lets the real
// pane and daemon answer the call and make its effects. It writes replay.json for every run, and
// effects.json for a run whose every call it judged: `ok`, or `DIVERGED` where the real daemon
// answered a request otherwise than the stand-in, after which no later call is replayed. A run
// whose records cannot place a change, pair a request or join a difference is `HARNESS`, and has
// no effects.json. A replay never overwrites another's record, nor any answers file.
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
			out := filepath.Join(h.out, outcome.ID)
			for _, name := range []string{"effects.json", "replay.json"} {
				if _, err := os.Stat(filepath.Join(out, name)); err == nil {
					t.Fatalf("%s already holds %s: a replay never overwrites another's record", out, name)
				}
			}
			var world teardownEffectsWorld
			teardownEffectsReadJSON(t, filepath.Join(dir, "world.json"), &world)
			var launch struct {
				Role  string
				Plain bool
			}
			teardownEffectsReadJSON(t, filepath.Join(dir, "launch.json"), &launch)
			if launch.Plain {
				t.Fatal("a plain session has no Legion claim to replay")
			}
			world.Clock = teardownEffectsReadTime(t, filepath.Join(dir, "clock.txt"))
			launched := teardownEffectsReadTime(t, filepath.Join(dir, "clock-real.txt"))
			record := teardownEffectsReplayRecord{ID: outcome.ID, Seed: []teardownEffectsChange{}, Calls: []teardownEffectsReplayCall{}, After: []teardownEffectsChange{}, NotReplayed: []string{}}
			harness := func(err error) {
				t.Helper()
				record.Verdict, record.Reason = "HARNESS", err.Error()
				teardownEffectsWriteNew(t, filepath.Join(out, "replay.json"), record)
				t.Fatalf("HARNESS %s: %v", outcome.ID, err)
			}
			calls, err := teardownEffectsReadCalls(filepath.Join(dir, "sessions"))
			if err != nil {
				harness(err)
			}
			own := ""
			for _, call := range calls {
				if own == "" {
					own = call.Session
				}
				if call.Session != own {
					harness(fmt.Errorf("legion calls of the sessions %s and %s: the replay drives one claim's", own, call.Session))
				}
			}
			plan, err := teardownEffectsOrder(dir, calls, world.Clock, launched)
			if err != nil {
				harness(err)
			}
			record.Seed, record.After = append(record.Seed, plan.Seed...), append(record.After, plan.After...)
			r := h.start(t, world, launch.Role, "", &plan, dir, harness)
			ran := func(id string) (teardownEffectsHandoff, bool) {
				_, version, word, ok := plan.Readback.handoffBody(id)
				return teardownEffectsHandoff{Phase: word, Version: version, ID: id}, ok
			}
			wrote := func(id string) (teardownEffectsHandoff, bool) {
				found, ok := r.handoffs[id]
				return found, ok
			}
			same := func(run, replay teardownEffectsHTTP) error {
				return teardownEffectsSameRequest(run, replay, ran, wrote)
			}
			var answers []teardownEffectsAnswer
			var differences []teardownEffectsDifference
			var names []teardownEffectsHandoff
			for i, step := range plan.Steps {
				call := step.Call
				if len(differences) > 0 {
					record.NotReplayed = append(record.NotReplayed, call.ID)
					continue
				}
				for _, c := range step.Before {
					if err := r.applyChange(c, own, dir, &plan.Readback); err != nil {
						harness(fmt.Errorf("%s (%s), before legion call %d (%s): %w", c.Source, c.What, i+1, call.ID, err))
					}
				}
				replayed := teardownEffectsReplayCall{Call: call.ID, Op: call.Op, At: world.Clock.Add(call.At.Sub(launched)).UTC(), Before: append([]teardownEffectsChange{}, step.Before...), State: r.state(),
					Standin: []teardownEffectsAnswerRef{}, Daemon: []teardownEffectsAnswerRef{}}
				seq := len(answers)
				more, replies, handoffs, result := r.call(seq, replayed.At, call.Args)
				answers = append(answers, more...)
				names = append(names, handoffs...)
				for _, s := range step.Standin {
					replayed.Standin = append(replayed.Standin, teardownEffectsAnswerRef{OutcomeSeq: s.OutcomeSeq, Route: s.route(), teardownEffectsStatus: s.status()})
				}
				for k, x := range replies {
					at := seq + k
					replayed.Daemon = append(replayed.Daemon, teardownEffectsAnswerRef{Seq: &at, Route: x.route(), teardownEffectsStatus: x.status()})
				}
				replayed.Client = teardownEffectsClientRef{Run: teardownEffectsClientAnswer{IsError: call.IsError, Text: call.Result}, Replay: teardownEffectsClientAnswer{IsError: result.isError, Text: result.text}}
				replayed.PhaseAfter = string(r.issue(r.key).Phase)
				record.Calls = append(record.Calls, replayed)
				found, err := teardownEffectsJudge(i, call, step.Standin, replies, teardownEffectsAnswered{isError: result.isError, text: result.text}, seq, same)
				if err != nil {
					harness(err)
				}
				differences = append(differences, found...)
			}
			got := r.evidence(outcome.ID, answers, differences, names)
			record.Verdict = "ok"
			if len(differences) > 0 {
				record.Verdict = "DIVERGED"
			}
			teardownEffectsWriteNew(t, filepath.Join(out, "effects.json"), got)
			teardownEffectsWriteNew(t, filepath.Join(out, "replay.json"), record)
			if len(differences) > 0 {
				first := differences[0]
				t.Errorf("DIVERGED %s: %d answers differ; the first, legion call %s (outcome action %d) %s: the stand-in answered %s and the real daemon %s",
					outcome.ID, len(differences), first.Call, first.OutcomeSeq, first.Route, teardownEffectsStatusText(first.Standin), teardownEffectsStatusText(first.Daemon))
				return
			}
			fmt.Println("ok replay " + outcome.ID)
		})
	}
}

var teardownEffectsUUID = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// teardownEffectsStatusText is an answer's status and refusal code, as a verdict line names it.
func teardownEffectsStatusText(s teardownEffectsStatus) string {
	if s.Code == nil {
		return strconv.Itoa(s.Status)
	}
	return strconv.Itoa(s.Status) + " " + *s.Code
}

// teardownEffectsWriteNew writes value to path, which must not exist: a replay's record is never
// written over another's.
func teardownEffectsWriteNew(t *testing.T, path string, value any) {
	t.Helper()
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	defer file.Close()
	if _, err := file.Write(append(data, '\n')); err != nil {
		t.Fatal(err)
	}
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

// result records a check result as the run did, each value through its `--<field>-file` flag: a
// value given on the command line that starts with `-` (an observed `--- PASS: …` line) is read as
// a flag, and `dispatch result` refuses the call.
func (r *teardownEffectsRun) result(fields map[string]any, session string) {
	r.t.Helper()
	args := []string{"result", "--issue", r.key}
	dir := r.t.TempDir()
	for _, field := range []string{"check", "role", "verdict", "commit", "surface", "run", "observed", "negative-control"} {
		value := teardownEffectsString(fields, field)
		if field == "commit" {
			value = r.sha(value)
		}
		if value != "" {
			file := filepath.Join(dir, field)
			writeProofFile(r.t, file, value)
			args = append(args, "--"+field+"-file", file)
		}
	}
	r.cli(session, args...)
}

// applyChange makes one recorded change again, before the call it preceded in the run.
func (r *teardownEffectsRun) applyChange(c teardownEffectsChange, own, runDir string, readback *teardownEffectsReadback) error {
	switch {
	case c.push != nil:
		return r.applyPush(*c.push, runDir)
	case c.github != nil:
		return r.applyGitHub(*c.github)
	case c.event != nil:
		return r.applyDispatch(*c.event, own, readback)
	case c.workspace != "":
		return r.applyWorkspace(c.workspace)
	}
	return fmt.Errorf("%s records no change", c.Source)
}

// teardownEffectsStandingRef is the local branch through which the replay lets jj see a commit
// the role's workspace moved onto. It stays, as setupGit's fixture-standing does: jj's import of a
// deleted Git ref can abandon the commit it named and rebase the working copy onto its parent.
const teardownEffectsStandingRef = "refs/heads/teardown-replay-standing"

// applyWorkspace moves the replay's workspace onto commit as the run's recorded jj operation moved
// the role's (workspace-ops.jsonl): a new working-copy commit whose parent, `@-`, is commit, which
// is what the legion tool reads as the commit the workspace stands on. The commit comes from
// workspace.pack, so it keeps the run's id, parents and tree. What the role's working copy held
// above it is not rebuilt: no recorded call reads it.
func (r *teardownEffectsRun) applyWorkspace(commit string) error {
	if r.standing() == commit {
		return nil
	}
	if out, err := r.git.Try(r.workspace, "git", "update-ref", teardownEffectsStandingRef, commit); err != nil {
		return fmt.Errorf("the run's workspace stood on %s, which neither workspace.pack nor the case's world holds: %v\n%s", commit, err, out)
	}
	for _, argv := range [][]string{{"jj", "git", "import"}, {"jj", "new", commit}} {
		if out, err := r.git.Try(r.workspace, argv...); err != nil {
			return fmt.Errorf("%s: %v\n%s", strings.Join(argv, " "), err, out)
		}
	}
	if got := r.standing(); got != commit {
		return fmt.Errorf("the replay's workspace stands on %q after its move onto %s", got, commit)
	}
	return nil
}

// standing is the commit the replay's workspace stands on, `@-`, as the legion tool reads it (each
// of several parents on a line of its own).
func (r *teardownEffectsRun) standing() string {
	return strings.TrimSpace(r.git.Out(r.workspace, "jj", "log", "-r", "@-", "--no-graph", "-T", `commit_id ++ "\n"`))
}

// remoteHead is the commit ref names on the replay's remote, "" where it names none.
func (r *teardownEffectsRun) remoteHead(ref string) string {
	out, err := r.git.Try(r.workspace, "git", "--git-dir", r.remote, "rev-parse", "--verify", "-q", ref+"^{commit}")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(out)
}

// applyPush moves the remote's ref as the run's push did, from the head it moved it from, and
// lets the workspace's jj read the move. A commit the run made comes from the run's own remote or
// workspace, which are sources only: the commit is on the remote from this push on, not before.
func (r *teardownEffectsRun) applyPush(p teardownEffectsPush, runDir string) error {
	from := p.Old
	if from == teardownEffectsNoCommit {
		from = ""
	}
	if got := r.remoteHead(p.Ref); got != from {
		return fmt.Errorf("the run's push moved %s from %q, and the replay's remote holds %q there: the replay's state is not the run's", p.Ref, from, got)
	}
	gitDir := []string{"git", "--git-dir", r.remote}
	switch {
	case p.New == teardownEffectsNoCommit:
		if out, err := r.git.Try(r.workspace, append(gitDir, "update-ref", "-d", p.Ref)...); err != nil {
			return fmt.Errorf("delete %s: %v\n%s", p.Ref, err, out)
		}
	default:
		moved := false
		if _, err := r.git.Try(r.workspace, append(gitDir, "cat-file", "-e", p.New+"^{commit}")...); err == nil {
			out, err := r.git.Try(r.workspace, append(gitDir, "update-ref", p.Ref, p.New)...)
			if err != nil {
				return fmt.Errorf("move %s to %s: %v\n%s", p.Ref, p.New, err, out)
			}
			moved = true
		}
		for _, source := range []string{r.workspace, filepath.Join(runDir, "remote.git"), filepath.Join(runDir, "workspace")} {
			if moved {
				break
			}
			if _, err := os.Stat(source); err != nil {
				continue
			}
			_, err := r.git.Try(r.workspace, append(gitDir, "fetch", "-q", "--no-tags", source, "+"+p.New+":"+p.Ref)...)
			moved = err == nil
		}
		if !moved {
			return fmt.Errorf("the run pushed %s to %s, and neither the replay's workspace nor the run's remote.git or workspace holds that commit", p.New, p.Ref)
		}
	}
	if out, err := r.git.Try(r.workspace, "jj", "git", "fetch", "--remote", r.remoteName); err != nil {
		return fmt.Errorf("jj git fetch after the push to %s: %v\n%s", p.Ref, err, out)
	}
	return nil
}

// applyGitHub makes a GitHub change again: the change gh-standin kept (`pull`), with the pull
// request it opened at the head it opened it on, which the replay's remote must hold there; or, in
// a capture from before gh-standin recorded it, the call read from its arguments (githubAction).
// A merge or a close is always read from its arguments, as the daemon stand-in reads it.
func (r *teardownEffectsRun) applyGitHub(row teardownEffectsGitHubRow) error {
	g := r.github
	if p := row.Pull; p != nil {
		if p.Action == "open" {
			if got := r.remoteHead("refs/heads/" + p.Head); got != p.SHA {
				return fmt.Errorf("gh-standin opened pull request #%d on %s at %s, and the replay's remote holds %q there: the replay's state is not the run's", p.Number, p.Head, p.SHA, got)
			}
		}
		g.mu.Lock()
		defer g.mu.Unlock()
		switch p.Action {
		case "open":
			if _, taken := g.pulls[p.Number]; taken {
				return fmt.Errorf("gh-standin opened pull request #%d, which the replay already holds", p.Number)
			}
			pull := teardownEffectsPull{Number: p.Number, State: "open", Base: p.Base, Body: p.Body, Draft: p.Draft}
			pull.Head.Ref, pull.Head.SHA = p.Head, p.SHA
			g.pulls[p.Number] = pull
		case "edit":
			for _, n := range p.Numbers {
				pull, ok := g.pulls[n]
				if !ok {
					return fmt.Errorf("gh-standin edited the body of pull request #%d, which the replay does not hold", n)
				}
				pull.Body = p.Body
				g.pulls[n] = pull
			}
		case "review":
			pull, ok := g.pulls[p.Number]
			if !ok {
				return fmt.Errorf("gh-standin reviewed pull request #%d, which the replay does not hold", p.Number)
			}
			at, err := time.Parse(time.RFC3339Nano, p.SubmittedAt)
			if err != nil {
				return fmt.Errorf("review of #%d: %w", p.Number, err)
			}
			pull.Reviews = append(pull.Reviews, struct {
				Author, State, Commit, Body string
				At                          time.Time
			}{Author: "legion-review[bot]", State: p.State, Commit: p.Commit, Body: p.Body, At: at})
			g.pulls[p.Number] = pull
		default:
			return fmt.Errorf("gh-standin kept a pull request change %q the replay does not make", p.Action)
		}
		return nil
	}
	command, method, path, fields, err := teardownEffectsGitHubRequest(row)
	if err != nil {
		return err
	}
	if command == "gh pr create" || method == "POST" && teardownEffectsPullsPath.MatchString(path) {
		head := r.branch
		if named, ok := fields["head"].(string); ok && named != "" {
			head = named
			if _, branch, owned := strings.Cut(named, ":"); owned {
				head = branch
			}
		}
		fields["head"] = head
		if r.remoteHead("refs/heads/"+head) == "" {
			return fmt.Errorf("gh %s opened a pull request on %s, which the replay's remote does not hold", strings.Join(row.Argv, " "), head)
		}
	} else if _, named := fields["number"]; !named && !strings.Contains(path, "/pulls/") {
		// A gh pr command that names no pull request acts on the one of the issue's branch.
		if pull, ok := g.forBranch(r.branch); ok {
			fields["number"] = float64(pull.Number)
		}
	}
	r.githubAction(command, method, path, fields)
	return nil
}

// applyDispatch writes one Dispatch event's record again as its author wrote it: the run's own
// session's as the replay's pane (own names the run's session), any other session's as that
// session, and a person's as the scratch Dispatch's person. Dispatch gives each record a new id;
// a handoff keeps its phase's version.
func (r *teardownEffectsRun) applyDispatch(e teardownEffectsEvent, own string, b *teardownEffectsReadback) error {
	actor := e.Actor
	if actor.Kind == "session" && own != "" && actor.ID == own {
		actor.ID = r.session
	}
	prefix := "/api/v1/issues/" + r.key
	text := func(key string) string { s, _ := e.Payload[key].(string); return s }
	if fault := teardownEffectsDispatchFault(b, e); fault != "" {
		return fmt.Errorf("%s: %s", e.Type, fault)
	}
	switch e.Type {
	case "handoff.recorded":
		body, version, word, _ := b.handoffBody(text("handoff_id"))
		if err := r.dispatchPost(actor, prefix+"/handoffs/"+word, map[string]any{"body": body}); err != nil {
			return err
		}
		got := r.dispatch.latest(r.key, word)
		if got.Version != version {
			return fmt.Errorf("the replay wrote the %s handoff version %d, and the run's was version %d", word, got.Version, version)
		}
		r.handoffs[got.ID] = teardownEffectsHandoff{Phase: word, ID: got.ID, Version: got.Version}
		return nil
	case "comment.created":
		return r.dispatchPost(actor, prefix+"/comments", map[string]any{"body": text("body")})
	case "result.recorded":
		result, check, _ := b.result(text("result_id"))
		return r.recordResult(actor.ID, check, result)
	case "ask.opened":
		for _, ask := range b.Asks {
			if ask.ID == text("id") {
				return r.dispatchPost(actor, prefix+"/asks", map[string]any{"question": ask.Question, "options": ask.Options})
			}
		}
	case "artifact.created":
		for _, doc := range b.Documents {
			if doc.ID == e.ArtifactID {
				return r.dispatchPost(actor, prefix+"/artifacts", map[string]any{"name": doc.Name, "content": doc.Text})
			}
		}
	}
	return fmt.Errorf("the replay writes no %s", e.Type)
}

// dispatchPost posts body as actor: a session through the agent's bearer, naming it, and a person
// as the scratch Dispatch's person (handoffRecordsUser), whose login stands for the run's.
func (r *teardownEffectsRun) dispatchPost(actor teardownEffectsActor, path string, body map[string]any) error {
	var status int
	var answer []byte
	switch actor.Kind {
	case "session":
		body["actor"] = actor
		status, answer = r.dispatch.agentPost(path, body)
	case "user":
		encoded, err := json.Marshal(body)
		if err != nil {
			return err
		}
		request, err := http.NewRequest(http.MethodPost, r.dispatch.base+path, bytes.NewReader(encoded))
		if err != nil {
			return err
		}
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("X-Dispatch-User", handoffRecordsUser)
		status, answer = r.dispatch.do(request)
	default:
		return fmt.Errorf("the replay writes nothing as a %s", actor.Kind)
	}
	if status != http.StatusCreated {
		return fmt.Errorf("Dispatch POST %s = %d %s", path, status, answer)
	}
	return nil
}

// recordResult records a check result as the session that recorded it in the run, each value of the
// stored result through its `--<field>-file` flag, as `result` does.
func (r *teardownEffectsRun) recordResult(session, check string, result map[string]any) error {
	dir := r.t.TempDir()
	args := []string{"result", "--issue", r.key}
	for _, field := range []struct{ flag, key string }{{"check", ""}, {"role", "role"}, {"verdict", "verdict"}, {"commit", "commit_sha"}, {"surface", "surface"}, {"run", "run"}, {"observed", "observed"}, {"negative-control", "negative_control"}} {
		value := check
		if field.key != "" {
			value, _ = result[field.key].(string)
		}
		if value == "" {
			continue
		}
		file := filepath.Join(dir, field.flag)
		writeProofFile(r.t, file, value)
		args = append(args, "--"+field.flag+"-file", file)
	}
	shell := cliShell{t: r.t, dir: dir, session: session, base: r.dispatch.base, token: r.dispatch.token}
	out, status := shell.bun(nil, dispatchCLI(r.harness.root), args...)
	if status != 0 {
		return fmt.Errorf("dispatch result as %s = %d: %s%s", session, status, out.stdout, out.stderr)
	}
	return nil
}

// state is what the next call runs against: the issue's phase, the tree the daemon holds it in and
// whether that tree runs, its parent in Dispatch, the commit its workspace stands on, its branch on
// the remote and the pull requests the GitHub fixture holds.
func (r *teardownEffectsRun) state() teardownEffectsState {
	issues := r.d.state().Issues
	issue, ok := issues[r.key]
	if !ok {
		r.t.Fatalf("the state records no issue %s", r.key)
	}
	var listed struct{ Parent *string }
	r.dispatch.agentGet("/api/v1/issues/"+r.key, &listed)
	s := teardownEffectsState{Phase: string(issue.Phase), Tree: issue.Tree, Standing: r.standing(), Branch: r.remoteHead("refs/heads/" + r.branch), Pulls: []teardownEffectsPullState{}}
	if root, ok := issues[issue.Tree]; ok && root.TreeRuns != nil {
		s.TreeRuns = *root.TreeRuns
	}
	if listed.Parent != nil {
		s.Parent = *listed.Parent
	}
	r.github.mu.Lock()
	for _, pull := range r.github.pulls {
		s.Pulls = append(s.Pulls, teardownEffectsPullState{Number: pull.Number, State: pull.State, Head: pull.Head.SHA})
	}
	r.github.mu.Unlock()
	slices.SortFunc(s.Pulls, func(a, b teardownEffectsPullState) int { return a.Number - b.Number })
	return s
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
