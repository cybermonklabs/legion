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
	r.proxy = teardownEffectsStartProxy(t, cfg.DaemonURL, teardownEffectsToolRoute)
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
			r.document(doc.Name, teardownEffectsBarePointers(key, doc.Name, doc.Body))
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
	r.d = startDaemonLogging(t, cfg, o, r.log())
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
	r.git.Run(r.workspace, "jj", "git", "remote", "add", r.remoteName, r.remote)
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
	// ref for the standing commit without giving the remote either the ref or its object.
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
	r.git.Run(r.workspace, "jj", "git", "fetch", "--remote", r.remoteName)
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
	return teardownEffectsRecord{ID: id, Answers: append([]teardownEffectsAnswer{}, answers...), Diverged: append([]teardownEffectsDifference{}, differences...), HandoffRecords: append([]teardownEffectsHandoff{}, names...), PhaseBefore: string(r.before), PhaseAfter: string(r.issue(r.key).Phase),
		ProductionCheck: r.productionCheck()}
}

// productionCheck is the issue's production check as the daemon's store holds it now: the wake of
// its next look and the check_wake outbox row due then; nil where the issue has no check.
func (r *teardownEffectsRun) productionCheck() *teardownEffectsCheck {
	t := r.t
	t.Helper()
	var got *teardownEffectsCheck
	if err := store.Transact(t.Context(), r.st.Pool(), func(ctx context.Context, tx pgx.Tx) error {
		check, err := r.records.ProductionCheck(ctx, tx, r.key)
		if err != nil || check == nil {
			return err
		}
		got = &teardownEffectsCheck{WakeAt: check.WakeAt}
		rows, err := tx.Query(ctx, `select next_at from outbox where kind='check_wake' and issue=$1 order by id`, r.key)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			if got.CheckWakeRow != nil {
				return fmt.Errorf("more than one check_wake row for %s", r.key)
			}
			var due time.Time
			if err := rows.Scan(&due); err != nil {
				return err
			}
			got.CheckWakeRow = &teardownEffectsWakeRow{DueAt: due.UTC()}
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

// Observe actual bytes on one HTTP hop: the pane's to the real daemon, where only the tool routes
// are observed (boot, registration, and asynchronous lifecycle calls are not tool answers), or the
// dispatch CLI's to Dispatch, where every request is observed. asked counts each observed request,
// and records holds each that came back, with its answer, so a request that got no answer (a
// transport failure) shows as a gap between the two.
type teardownEffectsProxy struct {
	mu      sync.Mutex
	url     string
	asked   int
	records []teardownEffectsHTTP
}

func teardownEffectsStartProxy(t *testing.T, target string, observe func(path string) bool) *teardownEffectsProxy {
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
		if observe(req.URL.Path) {
			p.mu.Lock()
			p.asked++
			p.mu.Unlock()
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

// observed is the number of requests the proxy observed, and each of them that came back, with its
// answer, in order.
func (p *teardownEffectsProxy) observed() (int, []teardownEffectsHTTP) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.asked, slices.Clone(p.records)
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
	// A pane that does not take or answer the call is the harness's fault: a replay writes its
	// HARNESS receipt (r.fail) rather than none.
	select {
	case r.model.next <- args:
	case <-time.After(paneReady):
		r.fail(fmt.Errorf("pane did not ask for call %d", seq))
	}
	var result toolResult
	select {
	case result = <-r.model.result:
	case <-time.After(paneReady):
		r.fail(fmt.Errorf("no result of call %d", seq))
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

// controls makes lane D's controls on the cases of the case file at path, every control
// teardownEffectsPromised promises: each trace lane D replays, replayed from its case's starting
// state as a run is replayed (receipt), into the two records a run gets, at
// teardownEffectsControlDir; each check one of its actions shows; and the known-good completion
// that teardownEffectsPublishable names, made before the trace publishes its head. It fails, naming
// each, where a control it promised did not execute, and writes daemon-answers.json, the answers
// the checks capture (fixture row F3), only where every control did.
func (h teardownEffectsHarness) controls(t *testing.T, path string) {
	var file struct{ Cases []teardownEffectsCase }
	teardownEffectsReadJSON(t, path, &file)
	if len(file.Cases) == 0 {
		t.Fatal("no cases")
	}
	promised, err := teardownEffectsPromised(file.Cases)
	if err != nil {
		t.Fatalf("the case file cannot show the controls --controls makes: %v", err)
	}
	fmt.Printf("--controls promises %d controls\n", len(promised))
	capture := teardownEffectsCapture{Version: 1}
	capture.Captured.Command = "bash scripts/e2e/teardown-effects.sh --controls --cases " + os.Getenv("LEGION_E2E_TEARDOWN_CASES_LABEL")
	capture.Captured.At = time.Now().UTC()
	capture.Captured.Commit = os.Getenv("LEGION_E2E_TEARDOWN_COMMIT")
	executed := map[teardownEffectsControl]bool{}
	ran := func(control teardownEffectsControl, what string) {
		executed[control] = true
		fmt.Printf("ok control %s (%s)\n", control, what)
	}
	for _, c := range file.Cases {
		if slices.ContainsFunc(promised, func(p teardownEffectsControl) bool { return p.Case == c.ID && p.Check == teardownEffectsLocalOnly }) {
			t.Run(c.ID+"-publish-after-completion", func(t *testing.T) { h.publishAfter(t, c, ran, &capture) })
		}
		for _, name := range teardownEffectsTraces(c) {
			t.Run(c.ID+"-"+name, func(t *testing.T) { h.control(t, c, name, promised, ran, &capture) })
		}
	}
	if missed := teardownEffectsMissed(promised, executed); len(missed) > 0 {
		t.Fatalf("%d of the %d controls --controls promised did not execute:\n%s", len(missed), len(promised), strings.Join(missed, "\n"))
	}
	teardownEffectsWriteJSON(t, filepath.Join(h.out, "daemon-answers.json"), capture)
}

// control replays the trace `name` of case c, as a run is replayed (receipt), into the trace's two
// records at teardownEffectsControlDir, and makes the checks promised on that trace, each on the
// action that shows it: a comment's around the change it makes, each other check on its call.
func (h teardownEffectsHarness) control(t *testing.T, c teardownEffectsCase, name string, promised []teardownEffectsControl, ran func(teardownEffectsControl, string), capture *teardownEffectsCapture) {
	trace := c.trace(name)
	var checks []teardownEffectsControl
	for _, p := range promised {
		if p.Case == c.ID && p.Trace == name && p.Check != teardownEffectsReplayed && p.Check != teardownEffectsLocalOnly && p.Check != teardownEffectsNoPullRequest {
			checks = append(checks, p)
		}
	}
	out := teardownEffectsControlDir(h.out, c.ID, name)
	var seqs []int
	_, turned := h.receipt(t, trace.ID, out, func(_ *teardownEffectsReplayRecord, harness func(error)) (*teardownEffectsRun, teardownEffectsScript) {
		r := h.start(t, c.World, c.Role, c.Task, nil, "", harness)
		script, at := r.traceScript(c.ID, name, trace)
		seqs = at
		for _, p := range checks {
			change := script.applied(p.Seq)
			if p.Check != teardownEffectsCommentNoWake || change == nil {
				continue
			}
			apply := change.apply
			change.apply = func() (*teardownEffectsRefusal, error) {
				turned := teardownEffectsTurned{call: teardownEffectsReplayCall{State: teardownEffectsState{Phase: string(r.issue(r.key).Phase)}}, before: r.productionCheck()}
				refused, err := apply()
				if err != nil {
					return nil, err
				}
				turned.call.PhaseAfter, turned.after = string(r.issue(r.key).Phase), r.productionCheck()
				if err := teardownEffectsShows(p, c, turned); err != nil {
					t.Errorf("%s: %v", p, err)
					return refused, nil
				}
				ran(p, "the comment left the phase and the production check as they were")
				return refused, nil
			}
		}
		return r, script
	})
	ran(teardownEffectsControl{Case: c.ID, Trace: name, Check: teardownEffectsReplayed}, "replay.json and effects.json in "+out)
	for _, p := range checks {
		if p.Check == teardownEffectsCommentNoWake {
			continue
		}
		i := slices.Index(seqs, p.Seq)
		if i < 0 || i >= len(turned) {
			t.Errorf("%s: the replay made no call of that action", p)
			continue
		}
		if err := teardownEffectsShows(p, c, turned[i]); err != nil {
			t.Errorf("%s: %v", p, err)
			continue
		}
		if name == "good" {
			capture.take(p, c, turned[i])
		}
		ran(p, fmt.Sprintf("the call was answered %s, the phase %s after it, the production check %s", teardownEffectsAnswers(turned[i].replies), turned[i].call.PhaseAfter, teardownEffectsCheckText(turned[i].after)))
	}
}

// publishAfter makes case c's known-good completion, at the action teardownEffectsPublishable names,
// before the trace publishes its head, in a replay of its own from the case's starting state: the
// trace's handoff writes, then the completion, which the tool refuses while the head is in the
// workspace alone (local-only-implementer-refused); then the trace's pushes, and the completion
// again, which the daemon refuses HANDOFF_NO_PULL_REQUEST while no pull request is open
// (pushed-without-pull-request-refused). Each check writes its effect record to
// <out>/<case id>/<check>/effects.json.
func (h teardownEffectsHarness) publishAfter(t *testing.T, c teardownEffectsCase, ran func(teardownEffectsControl, string), capture *teardownEffectsCapture) {
	seq, _ := teardownEffectsPublishable(c)
	good := c.Traces.Good
	var complete teardownEffectsTraceAction
	for _, a := range good.Actions {
		if a.Seq == seq {
			complete = a
		}
	}
	r := h.start(t, c.World, c.Role, c.Task, nil, "", nil)
	made := func(kind, command string) {
		for _, a := range good.Actions {
			if a.Kind == kind && a.Command == command {
				if _, err := r.applyTraceAction(a); err != nil {
					t.Fatalf("%s's known-good trace, action %d: %v", c.ID, a.Seq, err)
				}
			}
		}
	}
	attempt := func(check string) {
		control := teardownEffectsControl{Case: c.ID, Trace: "good", Check: check, Seq: seq}
		turned := teardownEffectsTurned{call: teardownEffectsReplayCall{State: teardownEffectsState{Phase: string(r.issue(r.key).Phase)}}, before: r.productionCheck()}
		answers, replies, handoffs, result := r.call(0, r.clock.Now(), teardownEffectsParams(complete.Op, complete.Args))
		turned.call.PhaseAfter, turned.after = string(r.issue(r.key).Phase), r.productionCheck()
		turned.replies, turned.client, turned.handoffs = replies, teardownEffectsAnswered{isError: result.isError, text: result.text}, handoffs
		teardownEffectsWriteNew(t, filepath.Join(h.out, c.ID, check, "effects.json"), r.evidence(good.ID, answers, nil, handoffs))
		if err := teardownEffectsShows(control, c, turned); err != nil {
			t.Fatalf("%s: %v", control, err)
		}
		capture.take(control, c, turned)
		ran(control, fmt.Sprintf("the tool answered %q, the daemon %s, the phase %s after it", result.text, teardownEffectsAnswers(replies), turned.call.PhaseAfter))
	}
	made("dispatch", "handoff-write")
	attempt(teardownEffectsLocalOnly)
	made("push", "")
	attempt(teardownEffectsNoPullRequest)
}

// take adds to the captured answers the answer control's check shows, with the request fields
// daemon-standin.ts matches it by: the check report's verdict, wake and look, or the completion's
// commit and handoff record. A check that shows no answer of the daemon adds none.
func (capture *teardownEffectsCapture) take(control teardownEffectsControl, c teardownEffectsCase, turned teardownEffectsTurned) {
	var a teardownEffectsTraceAction
	for _, each := range c.trace(control.Trace).Actions {
		if each.Seq == control.Seq {
			a = each
		}
	}
	found := teardownEffectsCompletions(turned.replies)
	if len(found) != 1 {
		return
	}
	var request struct {
		Commit    string `json:"commit"`
		HandoffID string `json:"handoffId"`
	}
	_ = json.Unmarshal(found[0].Body, &request)
	switch control.Check {
	case teardownEffectsWakeRefused:
		capture.add("check-wake-required", found[0], map[string]any{"verdict": "pending", "wakeAt": nil, "checkOpenedAt": a.Args["check_opened_at"]})
	case teardownEffectsWakeAccepted:
		capture.add("pending-accepted", found[0], map[string]any{"verdict": "pending", "wakeAt": a.Args["wake_at"], "checkOpenedAt": a.Args["check_opened_at"]})
	case teardownEffectsAccepted:
		capture.add("implementer-accepted", found[0], map[string]any{"commit": request.Commit, "handoffId": request.HandoffID})
	case teardownEffectsNoPullRequest:
		capture.add("implementer-no-pull-request", found[0], map[string]any{"commit": request.Commit, "handoffId": request.HandoffID})
	}
}
func (capture *teardownEffectsCapture) add(name string, res teardownEffectsHTTP, match map[string]any) {
	capture.Answers = append(capture.Answers, teardownEffectsFixtureAnswer{Name: name, Path: res.Path, Method: res.Method, Match: match, Status: res.Status, Body: res.Answer})
}

// teardownEffectsAnswers names each answer of replies, as a log line names them.
func teardownEffectsAnswers(replies []teardownEffectsHTTP) string {
	var named []string
	for _, x := range replies {
		named = append(named, x.route()+" "+teardownEffectsStatusText(x.status()))
	}
	return "[" + strings.Join(named, ", ") + "]"
}

// teardownEffectsCheckText names a production check's wake and check_wake row, as a log line names
// them.
func teardownEffectsCheckText(check *teardownEffectsCheck) string {
	if check == nil {
		return "none"
	}
	text := "wake none"
	if check.WakeAt != nil {
		text = "wake " + check.WakeAt.UTC().Format(time.RFC3339)
	}
	if check.CheckWakeRow != nil {
		return text + ", check_wake row due " + check.CheckWakeRow.DueAt.UTC().Format(time.RFC3339)
	}
	return text + ", no check_wake row"
}

// teardownEffectsTurn is one legion call a replay makes: the call (its op and arguments, and what
// the role was answered), the case time it is made at, the changes made before it, and how its
// answers are judged: the stand-in's answers replay.json names for it, each difference of the real
// daemon's, or an error, which makes the replay HARNESS.
type teardownEffectsTurn struct {
	call   teardownEffectsCall
	at     time.Time
	before []teardownEffectsApplied
	judge  func(replies []teardownEffectsHTTP, client teardownEffectsAnswered, seq int) ([]teardownEffectsAnswerRef, []teardownEffectsDifference, error)
}

// teardownEffectsScript is what a replay makes again, in order: its turns, and then the changes
// after the last call that it makes too (a control's; a run's replay makes none).
type teardownEffectsScript struct {
	turns []teardownEffectsTurn
	end   []teardownEffectsApplied
}

// applied is the change of s that a trace's action seq makes, nil where none does.
func (s *teardownEffectsScript) applied(seq int) *teardownEffectsApplied {
	for i := range s.turns {
		for j := range s.turns[i].before {
			if s.turns[i].before[j].OutcomeSeq == seq {
				return &s.turns[i].before[j]
			}
		}
	}
	for j := range s.end {
		if s.end[j].OutcomeSeq == seq {
			return &s.end[j]
		}
	}
	return nil
}

// receipt replays one record as id into the directory out, and writes there the two records of a
// replay: replay.json for every replay, HARNESS with its reason and typed cause
// (teardownEffectsFaultOf) where its records or the harness fail it, and effects.json beside it
// for a replay that is not HARNESS, never over a file that exists. prepare boots the daemon on the
// record's starting state and gives the script to make again (play); a failure it hands harness is
// the replay's HARNESS verdict. A replay that stops before it writes its receipt still leaves one:
// HARNESS, of the harness's own records (teardownEffectsStoppedReceipt), whose reason sends the
// reader to the test log. It answers the effect record and each call as it was made.
func (h teardownEffectsHarness) receipt(t *testing.T, id, out string, prepare func(record *teardownEffectsReplayRecord, harness func(error)) (*teardownEffectsRun, teardownEffectsScript)) (teardownEffectsRecord, []teardownEffectsTurned) {
	for _, name := range []string{"effects.json", "replay.json"} {
		if _, err := os.Stat(filepath.Join(out, name)); err == nil {
			t.Fatalf("%s already holds %s: a replay never overwrites another's record", out, name)
		}
	}
	record := teardownEffectsNewReplayRecord(id)
	// From here on, a replay that stops before it writes its own receipt still leaves one, so an
	// unreadable world, launch or clock file leaves one too. out held no receipt before this replay
	// (checked above), and a receipt that harness() or the end of the replay wrote is never written
	// again.
	written := false
	defer func() {
		// A panic is no t.Failed() until the test framework recovers it, after this defer.
		p := recover()
		if err := teardownEffectsStoppedReceipt(filepath.Join(out, "replay.json"), record, p != nil || t.Failed(), written); err != nil {
			t.Errorf("write the HARNESS receipt of %s: %v", id, err)
		}
		if p != nil {
			panic(p)
		}
	}()
	harness := func(err error) {
		t.Helper()
		record.Verdict, record.Reason, record.Fault = "HARNESS", err.Error(), teardownEffectsFaultOf(err)
		teardownEffectsWriteNew(t, filepath.Join(out, "replay.json"), record)
		written = true
		t.Fatalf("HARNESS %s: %v", id, err)
	}
	r, script := prepare(&record, harness)
	answers, differences, names, turned := r.play(script, &record, harness)
	got := r.evidence(id, answers, differences, names)
	record.Verdict = "ok"
	if len(differences) > 0 {
		record.Verdict = "DIVERGED"
	}
	teardownEffectsWriteNew(t, filepath.Join(out, "effects.json"), got)
	teardownEffectsWriteNew(t, filepath.Join(out, "replay.json"), record)
	written = true
	if len(differences) > 0 {
		first := differences[0]
		t.Errorf("DIVERGED %s: %d answers differ; the first, legion call %s (outcome action %d) %s: the stand-in answered %s and the real daemon %s",
			id, len(differences), first.Call, first.OutcomeSeq, first.Route, teardownEffectsStatusText(first.Standin), teardownEffectsStatusText(first.Daemon))
	}
	return got, turned
}

// play makes script again on r and records it in record: before each call the changes before it
// (teardownEffectsMake), then the call through the real pane and daemon, judged by its turn; after
// the first difference no later call is replayed, and otherwise the changes after the last call are
// made too. A change the replay made as a refused write is recorded with its refusal, and the
// replay goes on to the next action. A failure is handed to harness, the replay's HARNESS verdict.
// It answers the effect record's answers, differences and handoff records, and each call as it was
// made.
func (r *teardownEffectsRun) play(script teardownEffectsScript, record *teardownEffectsReplayRecord, harness func(error)) ([]teardownEffectsAnswer, []teardownEffectsDifference, []teardownEffectsHandoff, []teardownEffectsTurned) {
	var answers []teardownEffectsAnswer
	var differences []teardownEffectsDifference
	var names []teardownEffectsHandoff
	var turned []teardownEffectsTurned
	for i, turn := range script.turns {
		if len(differences) > 0 {
			record.NotReplayed = append(record.NotReplayed, turn.call.ID)
			continue
		}
		before, err := teardownEffectsMake(turn.before, fmt.Sprintf("before legion call %d (%s)", i+1, turn.call.ID))
		if err != nil {
			harness(err)
		}
		replayed := teardownEffectsReplayCall{Call: turn.call.ID, Op: turn.call.Op, At: turn.at, Before: before, State: r.state(), Standin: []teardownEffectsAnswerRef{}, Daemon: []teardownEffectsAnswerRef{}}
		check := r.productionCheck()
		seq := len(answers)
		more, replies, handoffs, result := r.call(seq, turn.at, turn.call.Args)
		answers = append(answers, more...)
		names = append(names, handoffs...)
		for k, x := range replies {
			at := seq + k
			replayed.Daemon = append(replayed.Daemon, teardownEffectsAnswerRef{Seq: &at, Route: x.route(), teardownEffectsStatus: x.status()})
		}
		client := teardownEffectsAnswered{isError: result.isError, text: result.text}
		replayed.Client = teardownEffectsClientRef{Run: teardownEffectsClientAnswer{IsError: turn.call.IsError, Text: turn.call.Result}, Replay: teardownEffectsClientAnswer{IsError: result.isError, Text: result.text}}
		replayed.PhaseAfter = string(r.issue(r.key).Phase)
		standin, found, err := turn.judge(replies, client, seq)
		replayed.Standin = append(replayed.Standin, standin...)
		record.Calls = append(record.Calls, replayed)
		if err != nil {
			harness(err)
		}
		differences = append(differences, found...)
		turned = append(turned, teardownEffectsTurned{call: replayed, replies: replies, client: client, handoffs: handoffs, before: check, after: r.productionCheck()})
	}
	if len(differences) == 0 {
		end, err := teardownEffectsMake(script.end, "after the last legion call")
		record.End = append(record.End, end...)
		if err != nil {
			harness(err)
		}
	}
	return answers, differences, names, turned
}

// runScript is a run's replay order (plan, from teardownEffectsOrder) as play makes it again: each
// legion call at its case time (clock is the case's start and launched the launch's wall-clock
// instant), the changes the run recorded before it, made as applyChange makes them (own is the
// run's session, runDir its directory), and its answers judged against the stand-in's
// (teardownEffectsJudge). The changes after the last call are not made: no call saw them.
func (r *teardownEffectsRun) runScript(plan teardownEffectsPlan, clock, launched time.Time, own, runDir string) teardownEffectsScript {
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
	var script teardownEffectsScript
	for i, step := range plan.Steps {
		turn := teardownEffectsTurn{call: step.Call, at: clock.Add(step.Call.At.Sub(launched)).UTC()}
		for _, c := range step.Before {
			turn.before = append(turn.before, teardownEffectsApplied{teardownEffectsChange: c, apply: func() (*teardownEffectsRefusal, error) { return nil, r.applyChange(c, own, runDir, &plan.Readback) }})
		}
		turn.judge = func(replies []teardownEffectsHTTP, client teardownEffectsAnswered, seq int) ([]teardownEffectsAnswerRef, []teardownEffectsDifference, error) {
			standin := []teardownEffectsAnswerRef{}
			for _, s := range step.Standin {
				standin = append(standin, teardownEffectsAnswerRef{OutcomeSeq: s.OutcomeSeq, Route: s.route(), teardownEffectsStatus: s.status()})
			}
			found, err := teardownEffectsJudge(i, step.Call, step.Standin, replies, client, seq, same)
			return standin, found, err
		}
		script.turns = append(script.turns, turn)
	}
	return script
}

// traceScript is the trace `name` of case caseID as play makes it again: each daemon action one
// legion call through the real pane at the action's case time, held to the answer the trace records
// (teardownEffectsTraceAnswer); each push, GitHub change and Dispatch write a change made before the
// next call, or after the last one, by the run replay's own writers at the action's case time
// (applyTraceAction), a request-approval among them, which the replay makes as a refused write and
// records with its refusal under the action's own seq and case time. A staging action, and a GitHub
// call that changes no pull request (a read: teardownEffectsTraceRead), change nothing the replay
// holds, and are not made. It also answers the seq of each call's action, in the order of the calls.
func (r *teardownEffectsRun) traceScript(caseID, name string, trace teardownEffectsTrace) (teardownEffectsScript, []int) {
	var script teardownEffectsScript
	var seqs []int
	var pending []teardownEffectsApplied
	for _, a := range trace.Actions {
		source := fmt.Sprintf("%s %s trace, action %d", caseID, name, a.Seq)
		switch {
		case a.Kind == "staging", a.Kind == "github" && teardownEffectsTraceRead(a):
		case a.Kind == "daemon":
			call := teardownEffectsCall{ID: source, Op: a.Op, Args: teardownEffectsParams(a.Op, a.Args), IsError: a.Answer.Code != nil}
			judge := func(replies []teardownEffectsHTTP, _ teardownEffectsAnswered, _ int) ([]teardownEffectsAnswerRef, []teardownEffectsDifference, error) {
				ref, err := teardownEffectsTraceAnswer(a.Seq, a.Op, a.Answer.Code, replies)
				if err != nil {
					return nil, nil, fmt.Errorf("%s: %w", source, err)
				}
				return []teardownEffectsAnswerRef{ref}, nil, nil
			}
			script.turns = append(script.turns, teardownEffectsTurn{call: call, at: a.At.UTC(), before: pending, judge: judge})
			seqs = append(seqs, a.Seq)
			pending = nil
		default:
			what := a.Command
			switch {
			case a.Kind == "push":
				what = a.Ref + " " + a.Commit
			case a.Kind == "github" && a.Command == "":
				what = a.Method + " " + a.Path
			}
			change := teardownEffectsChange{Kind: a.Kind, Source: source, What: what, At: a.At.UTC(), OutcomeSeq: a.Seq}
			pending = append(pending, teardownEffectsApplied{teardownEffectsChange: change, apply: func() (*teardownEffectsRefusal, error) { return r.applyTraceAction(a) }})
		}
	}
	script.end = pending
	return script, seqs
}

// applyTraceAction makes a trace's push, GitHub change or Dispatch write again at its case time, with
// the run replay's own writers: a push moves the remote's ref from the head it holds now
// (applyPush); a GitHub change is the trace's own `change`, or the one gh-standin keeps of the
// request the action records (teardownEffectsTraceChange), its commits mapped and what it leaves to
// the state filled from the replay's own (teardownEffectsScratch.resolve), made by the rule a run's
// change is made by (teardownEffectsScratch.change); a Dispatch write is made by the role's session
// as the run replay makes one (teardownEffectsTraceWrite, applyDispatch), but a request-approval,
// which the role's session makes through the real dispatch CLI as a refused write
// (requestApproval). It answers that write's refusal, and nil for every change it made.
func (r *teardownEffectsRun) applyTraceAction(a teardownEffectsTraceAction) (*teardownEffectsRefusal, error) {
	r.clock.advance(a.At)
	switch a.Kind {
	case "push":
		ref := a.Ref
		if !strings.HasPrefix(ref, "refs/") {
			ref = "refs/heads/" + ref
		}
		old := r.remoteHead(ref)
		if old == "" {
			old = teardownEffectsNoCommit
		}
		return nil, r.applyPush(teardownEffectsPush{Ref: ref, Old: old, New: r.sha(a.Commit)}, "")
	case "github":
		change, err := teardownEffectsTraceChange(a)
		if err != nil {
			return nil, err
		}
		if change == nil {
			return nil, fmt.Errorf("the trace's GitHub action %d changes no pull request, and the replay makes no read", a.Seq)
		}
		p := *change
		p.SHA, p.Commit = r.sha(p.SHA), r.sha(p.Commit)
		g := r.github
		g.mu.Lock()
		defer g.mu.Unlock()
		scratch := r.scratch()
		if p, err = scratch.resolve(p, a.At); err != nil {
			return nil, err
		}
		return nil, scratch.change(p)
	case "dispatch":
		if a.Command == teardownEffectsApprovalCommand {
			return r.requestApproval(a)
		}
		e, readback, err := teardownEffectsTraceWrite(r.key, r.session, a, r.sha, r.nextVersion)
		if err != nil {
			return nil, err
		}
		return nil, r.applyDispatch(e, r.session, &readback)
	}
	return nil, fmt.Errorf("the replay makes no %s action", a.Kind)
}

// requestApproval makes the trace's request-approval a again as the role's session makes one: the
// real dispatch CLI, run as recordResult runs it, with the document and the summary the trace names
// (teardownEffectsApprovalRequest), through a proxy that keeps each request the CLI sends Dispatch
// and Dispatch's answer to it. The replay makes a request-approval only as a refused write: it
// answers the refusal, typed from those answers (teardownEffectsApprovalRefusal), and an error,
// which makes the replay HARNESS, where Dispatch took the request or another write, failed, or
// refused for another cause.
func (r *teardownEffectsRun) requestApproval(a teardownEffectsTraceAction) (*teardownEffectsRefusal, error) {
	artifact, summary, err := teardownEffectsApprovalRequest(r.key, a)
	if err != nil {
		return nil, err
	}
	dir := r.t.TempDir()
	args := []string{teardownEffectsApprovalCommand, "--issue", r.key}
	for _, field := range []struct{ flag, value string }{{"artifact", artifact}, {"summary", summary}} {
		file := filepath.Join(dir, field.flag)
		writeProofFile(r.t, file, field.value)
		args = append(args, "--"+field.flag+"-file", file)
	}
	proxy := teardownEffectsStartProxy(r.t, r.dispatch.base, func(string) bool { return true })
	shell := cliShell{t: r.t, dir: dir, session: r.session, base: proxy.url, token: r.dispatch.token}
	out, exit := shell.bun(nil, dispatchCLI(r.harness.root), args...)
	asked, sent := proxy.observed()
	refusal, err := teardownEffectsApprovalRefusal(r.key, artifact, exit, string(out.stdout)+string(out.stderr), asked, sent)
	if err != nil {
		return nil, fmt.Errorf("the trace's request-approval at action %d: %w", a.Seq, err)
	}
	return refusal, nil
}

// nextVersion is the version the next handoff record of phase takes: one more than the newest the
// replay holds of it.
func (r *teardownEffectsRun) nextVersion(phase string) int {
	newest := 0
	for _, h := range r.handoffs {
		if h.Phase == phase {
			newest = max(newest, h.Version)
		}
	}
	return newest + 1
}

// replay drives each run of root (a run directory, or a directory of them) on the real daemon from
// the case's starting state. Before each of the run's legion calls it applies the Git, GitHub and
// Dispatch changes the run recorded before that call (teardownEffectsOrder), then lets the real
// pane and daemon answer the call and make its effects. It writes replay.json for every run, and
// effects.json for a run whose every call it judged: `ok`, or `DIVERGED` where the real daemon
// answered a request otherwise than the stand-in, after which no later call is replayed. A run
// whose records cannot place a change, pair a request or join a difference is `HARNESS`, and has
// no effects.json; its replay.json types the cause (teardownEffectsFaultOf): the role's own
// Dispatch write that the replay does not make, or the harness's own records. A replay that fails
// before it writes its receipt still writes one (receipt). A replay never overwrites another's
// record, nor any answers file.
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
			got, _ := h.receipt(t, outcome.ID, filepath.Join(h.out, outcome.ID), func(record *teardownEffectsReplayRecord, harness func(error)) (*teardownEffectsRun, teardownEffectsScript) {
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
				return r, r.runScript(plan, world.Clock, launched, own, dir)
			})
			if len(got.Diverged) == 0 {
				fmt.Println("ok replay " + outcome.ID)
			}
		})
	}
}

var teardownEffectsUUID = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// teardownEffectsWriteNew writes value to path, which must not exist: a replay's record is never
// written over another's (teardownEffectsCreateNew).
func teardownEffectsWriteNew(t *testing.T, path string, value any) {
	t.Helper()
	if err := teardownEffectsCreateNew(path, value); err != nil {
		t.Fatalf("write %s: %v", path, err)
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

// applyChange makes one recorded change again, before the call it preceded in the run.
func (r *teardownEffectsRun) applyChange(c teardownEffectsChange, own, runDir string, readback *teardownEffectsReadback) error {
	switch {
	case c.push != nil:
		return r.applyPush(*c.push, runDir)
	case c.pull != nil:
		return r.applyGitHub(*c.pull)
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
// A trace's push names no run directory (runDir ""): its commit comes from the replay's workspace.
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
		sources := []string{r.workspace}
		if runDir != "" {
			sources = append(sources, filepath.Join(runDir, "remote.git"), filepath.Join(runDir, "workspace"))
		}
		for _, source := range sources {
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
			return fmt.Errorf("the push of %s to %s needs that commit, and none of %s holds it", p.New, p.Ref, strings.Join(sources, ", "))
		}
	}
	if out, err := r.git.Try(r.workspace, "jj", "git", "fetch", "--remote", r.remoteName); err != nil {
		return fmt.Errorf("jj git fetch after the push to %s: %v\n%s", p.Ref, err, out)
	}
	return nil
}

// applyGitHub makes a GitHub change again from the change gh-standin kept of the call (calls.jsonl's
// `pull`), and from nothing else of it, by the rule each change has (teardownEffectsScratch.change):
// a pull request it opened must stand at the head the replay's remote holds on its branch, and a
// change the scratch GitHub cannot hold is refused by name.
func (r *teardownEffectsRun) applyGitHub(p teardownEffectsPullMutation) error {
	g := r.github
	g.mu.Lock()
	defer g.mu.Unlock()
	return r.scratch().change(p)
}

// scratch is the replay's scratch GitHub as a pull request change reads and makes it. Its caller
// holds the GitHub fixture's lock.
func (r *teardownEffectsRun) scratch() teardownEffectsScratch {
	remote := func(branch string) string { return r.remoteHead("refs/heads/" + branch) }
	return teardownEffectsScratch{pulls: r.github.pulls, branch: r.branch, base: r.base, remote: remote}
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
	if fault, _ := teardownEffectsDispatchFault(b, e); fault != "" {
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

// rest is the pull request as GitHub's REST API answers it. Its auto-merge and its place in the
// merge queue are not served: the daemon reads neither.
func (p teardownEffectsPull) rest() map[string]any {
	state := p.State
	if state == "merged" {
		state = "closed"
	}
	return map[string]any{"number": p.Number, "state": state, "draft": p.Draft, "title": p.Title, "body": p.Body, "head": map[string]string{"ref": p.Head.Ref, "sha": p.Head.SHA},
		"base": map[string]string{"ref": p.Base}, "merged": p.State == "merged", "merge_commit_sha": p.MergeCommit, "mergeable": true, "mergeable_state": "clean"}
}
