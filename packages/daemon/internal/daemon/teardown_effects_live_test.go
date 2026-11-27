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

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/appauth"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/fixturegit"
	"github.com/sjawhar/legion/daemon/internal/githubtest"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/testnats"
	"github.com/sjawhar/legion/daemon/internal/testpg"
	"github.com/sjawhar/legion/daemon/internal/treelifecycle"
	"github.com/sjawhar/legion/daemon/internal/workflow"
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
	// claimIssue and tree are the role's claim, on claimIssue in tree (teardownEffectsClaimOf).
	claimIssue, tree string
	role             claim.Role
	token            claim.Token
	git              fixturegit.Runner
	commits          map[string]string
	handoffs         map[string]teardownEffectsHandoff
	// gateIDs is the id the replay's Dispatch gives each gate step's document, by step, and
	// artifacts the id the replay's Dispatch gives each document a case's records or a run name by
	// an id of their own (the world's gate artifactId, lane R's scratch id), by that id.
	gateIDs, artifacts map[string]string
	documents          map[string]string // name to replay artifact id
	comments           map[string]string // recorded comment id to replay comment id
	worldReviews       map[int]int       // pull request number to the number of reviews the case's world gives it
	engine             *workflow.Engine  // judges a review as the daemon's workflow does (phaseAfter)
	before             phase.Phase
	fail               func(error)
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
// Dispatch records, and each typed part of it (teardown_effects_world_test.go). A replay (plan, from
// runDir) seeds Dispatch from the run's own record of that state and builds its Git with lane R's
// builder; harness, given, ends the run on a setup failure. A typed part the replay has no seeding
// for, or holds a fact it cannot seed (teardownEffectsSeedFaults: the refusals lane R's daemon
// stand-in makes of the same facts among them), is that setup failure, naming the part, before
// anything is booted.
func (h teardownEffectsHarness) start(t *testing.T, world teardownEffectsWorld, role, task string, plan *teardownEffectsPlan, runDir string, harness func(error)) *teardownEffectsRun {
	t.Helper()
	if world.Daemon == nil {
		t.Fatal("world has no daemon record")
	}
	failed := harness
	if failed == nil {
		failed = func(err error) { t.Helper(); t.Fatal(err) }
	}
	if len(world.unserved) > 0 {
		failed(fmt.Errorf("the case's world states %s, which lane D seeds nothing of, so the replay cannot start from it", strings.Join(world.unserved, ", ")))
	}
	if faults := teardownEffectsSeedFaults(world); len(faults) > 0 {
		failed(fmt.Errorf("the replay cannot seed the case's world: %s", strings.Join(faults, "; ")))
	}
	key := world.Dispatch.Issue.Key
	claimIssue, claimTree, _ := teardownEffectsClaimOf(world)
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
	// The deployment the world's admission and gate stand under: its cap, and the steps its design
	// gate holds, in the order the daemon gates them.
	if admission := world.Daemon.Admission; admission != nil {
		cfg.AdmissionCap = admission.Cap
	}
	if rows := world.Daemon.Gate; len(rows) > 0 {
		cfg.Gates.Design = []record.DesignStep{}
		for _, step := range teardownEffectsGateSteps {
			if slices.ContainsFunc(rows, func(row teardownEffectsGateRow) bool { return row.Step == string(step) }) {
				cfg.Gates.Design = append(cfg.Gates.Design, step)
			}
		}
	}
	projectToken, err := claim.ProjectToken(project)
	if err != nil {
		t.Fatal(err)
	}
	token := claim.ControllerToken(projectToken)
	if roleOfClaim != claim.RoleController {
		if token, err = claim.NewToken(projectToken, claimIssue, roleOfClaim); err != nil {
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
		clock: &teardownEffectsClock{now: world.Clock}, git: fixturegit.Isolated(t, h.home), commits: map[string]string{}, handoffs: map[string]teardownEffectsHandoff{}, repo: liveRepo, base: "main", remoteName: "origin", fail: failed,
		claimIssue: claimIssue, tree: claimTree, gateIDs: map[string]string{}, artifacts: map[string]string{}}
	r.documents, r.comments, r.worldReviews = map[string]string{}, map[string]string{}, map[int]int{}
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
		r.github.settings = world.GitHub.Settings
		if rules := world.GitHub.BranchRules; rules != nil {
			r.github.rules = map[string][]json.RawMessage{}
			for _, branch := range *rules {
				r.github.rules[branch.Branch] = branch.Rules
			}
		}
	}
	if world.GitHub != nil {
		for _, pull := range world.GitHub.PullRequests {
			pull.Head.SHA, pull.MergeCommit = r.sha(pull.Head.SHA), r.sha(pull.MergeCommit)
			for i := range pull.Reviews {
				pull.Reviews[i].Commit = r.sha(pull.Reviews[i].Commit)
			}
			r.github.pulls[pull.Number] = pull
			r.worldReviews[pull.Number] = len(pull.Reviews)
		}
	}
	githubURL := githubtest.Server(t, githubtest.Spec{Handle: r.github.serve})
	r.setupIssue(world)
	// A record whose writer is the role's own session (daemon.claim.session) is written as the
	// replay's own session, once the pane has registered one, after the others.
	var ownSeed []teardownEffectsChange
	var ownHandoffs []int
	if plan != nil {
		// The seeded records go in before the issue takes its status: Dispatch refuses a handoff on a
		// closed issue (ISSUE_CLOSED).
		for _, c := range plan.Seed {
			if c.event.Actor.Kind == "session" && teardownEffectsOwnWrite(world, c.event.Actor.ID) {
				ownSeed = append(ownSeed, c)
				continue
			}
			if err := r.applyDispatch(*c.event, "", &plan.Readback); err != nil {
				r.fail(fmt.Errorf("seed %s (%s) of the case's starting state: %w", c.Source, c.What, err))
			}
		}
	} else {
		for _, doc := range world.Dispatch.Documents {
			r.document(doc.Name, teardownEffectsBarePointers(key, doc.Name, doc.Body))
		}
		for i, handoff := range world.Dispatch.Handoffs {
			if teardownEffectsOwnWrite(world, handoff.Author) {
				ownHandoffs = append(ownHandoffs, i)
				continue
			}
			r.handoff(handoff.Phase, handoff.Author, r.mapBody(handoff.Body))
		}
	}
	// The case's issue carries the labels its world states, else Legion's, as lane R's seed leaves
	// it; the parent carries Legion's label too, as the root handed over that seed records it as,
	// unless the world states it among its other issues, whose labels it then carries.
	labels := []string{"legion"}
	if stated := world.Dispatch.Issue.Labels; stated != nil {
		labels = *stated
	}
	if world.Dispatch.Issue.Parent == "" {
		r.dispatch.user(http.MethodPatch, "/api/v1/issues/"+r.key, map[string]any{"status": world.Dispatch.Issue.Status, "labels": labels}, http.StatusOK, nil)
	} else {
		// As lane R's seed sets a child's status (seed.ts seedCaseDispatch): each ancestor not in
		// work moves to in_progress with it, as Dispatch keeps a parent in line with its children.
		if world.Dispatch.Issue.Status != "triage" {
			r.dispatch.user(http.MethodPost, "/api/v1/issues/"+r.key+"/status-change", map[string]any{"status": world.Dispatch.Issue.Status, "include_parent": true}, http.StatusOK, nil)
		}
		if !slices.ContainsFunc(world.Dispatch.Issues, func(other teardownEffectsDispatchIssue) bool { return other.Key == world.Dispatch.Issue.Parent }) {
			r.dispatch.user(http.MethodPatch, "/api/v1/issues/"+world.Dispatch.Issue.Parent, map[string]any{"labels": []string{"legion"}}, http.StatusOK, nil)
		}
		r.dispatch.user(http.MethodPatch, "/api/v1/issues/"+r.key, map[string]any{"labels": labels}, http.StatusOK, nil)
	}
	r.seedDispatchParts(world)
	r.seed(world, task)
	if plan != nil {
		r.mapRunArtifacts(runDir)
	}
	o := r.rt.daemonOverrides(r.rt)
	tokens := &workflowTokenRecorder{}
	o.clock, o.githubAPI, o.workflowTokens = r.clock, githubURL, tokens
	// The daemon's judgement of a review, with the review App's login the daemon reads from the same
	// tokens at its boot (appLoginsAtBoot), over the daemon's own store (phaseAfter).
	reviewApp, err := tokens.AppLogin(t.Context(), appauth.Review)
	if err != nil {
		t.Fatal(err)
	}
	r.engine = workflow.New(r.records, workflow.Config{Project: cfg.Project, ReviewAppLogin: reviewApp}, quietLogger())
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
	_, _, own := teardownEffectsClaimOf(world)
	for _, c := range ownSeed {
		if err := r.applyDispatch(*c.event, own, &plan.Readback); err != nil {
			r.fail(fmt.Errorf("seed %s (%s) of the case's starting state as the replay's own session: %w", c.Source, c.What, err))
		}
	}
	for _, i := range ownHandoffs {
		handoff := world.Dispatch.Handoffs[i]
		r.handoff(handoff.Phase, r.session, r.mapBody(handoff.Body))
	}
	// A child stands in the tree the world records it in: daemon.issue's, else its Dispatch
	// parent's, which runs.
	if stated := world.Daemon.Issue; stated != nil {
		if s := r.state(); s.Tree != stated.Tree {
			r.fail(fmt.Errorf("the replay holds %s in tree %q, and the case's world records it in %s (daemon.issue)", key, s.Tree, stated.Tree))
		}
	} else if parent := world.Dispatch.Issue.Parent; parent != "" {
		if s := r.state(); s.Tree != parent || s.Parent != parent || !s.TreeRuns {
			r.fail(fmt.Errorf("the replay holds %s in tree %q (running: %t) under the Dispatch parent %q, and the case's world names the parent %s", key, s.Tree, s.TreeRuns, s.Parent, parent))
		}
	}
	r.readBackDaemon(world)
	return r
}

// setupIssue files the case's issue in the scratch Dispatch under its own key and, where the world
// names one, under its parent, as lane R's seed files them (seed.ts seedCaseDispatch): the parent
// first, titled as lane R titles it, then the issue naming it as its parent, so a child is no root
// here either. Each other issue of the world (world.dispatch.issues) is filed in its place among
// them, in key order, with its title (the world's, else lane R's: `Parent of <child>` for the case's
// parent, else the key), its labels and its parent. A parent in another project, or one that
// follows its child, is not set up, and neither is another issue in another project.
func (r *teardownEffectsRun) setupIssue(world teardownEffectsWorld) {
	project := r.cfg.Project
	r.dispatch.user(http.MethodPost, "/api/v1/projects", map[string]string{"key": project, "name": "Teardown effects"}, 201, nil)
	r.dispatch.user(http.MethodPut, "/api/v1/projects/"+project+"/default-repository", map[string]string{"repo": r.repo}, 200, nil)
	parent := world.Dispatch.Issue.Parent
	fields := map[string]map[string]any{r.key: {"title": world.Dispatch.Issue.Title, "spec": world.Dispatch.Issue.Spec}}
	if stated := world.Dispatch.Issue.Labels; stated != nil {
		fields[r.key]["labels"] = *stated
	}
	if parent != "" {
		if !strings.HasPrefix(parent, project+"-") || r.issueNumber(parent) >= r.issueNumber(r.key) {
			r.fail(fmt.Errorf("the case's issue %s has the parent %s, which is in another project or follows it, and which the replay does not set up", r.key, parent))
		}
		fields[parent] = map[string]any{"title": "Parent of " + r.key}
		fields[r.key]["parent"] = parent
	}
	for i, other := range world.Dispatch.Issues {
		if !strings.HasPrefix(other.Key, project+"-") {
			r.fail(fmt.Errorf("world.dispatch.issues[%d] %s is in another project than %s, which the replay does not set up", i, other.Key, project))
		}
		title := other.Title
		if title == "" {
			title = other.Key
			if other.Key == parent {
				title = "Parent of " + r.key
			}
		}
		filed := map[string]any{"title": title, "labels": other.Labels}
		if other.Parent != nil {
			if !strings.HasPrefix(*other.Parent, project+"-") || r.issueNumber(*other.Parent) >= r.issueNumber(other.Key) {
				r.fail(fmt.Errorf("world.dispatch.issues[%d].parent %s is in another project or follows %s, which the replay does not set up", i, *other.Parent, other.Key))
			}
			filed["parent"] = *other.Parent
		}
		fields[other.Key] = filed
	}
	keys := slices.Collect(maps.Keys(fields))
	slices.SortFunc(keys, func(a, b string) int { return r.issueNumber(a) - r.issueNumber(b) })
	for _, key := range keys {
		r.fileIssue(key, fields[key])
	}
}

// teardownEffectsSeededRun names the fields of a step record a world's step run does not name,
// each one fixed value for every run lane D writes, as lane R's seed writes them (seed.ts
// SEEDED_RUN): the agent and the model, and no type of work. Dispatch gives each run its attempt.
var teardownEffectsSeededRun = struct{ Agent, Model string }{Agent: "rig-seed", Model: "rig-seed"}

// seedDispatchParts writes the world's Dispatch parts beside the case's issue, as lane R's seed
// writes them (seed.ts seedCaseDispatch), after the case's status, so each takes the world's own:
// each other issue's status, `blocked_by` links and claim (its session's), the board's order (rank
// 1 before every other issue of the project, each next rank right after the one before), and each
// step run as the project's daemon writes it (`PUT /api/v1/issues/{key}/steps/{id}` and the
// retro's rating, as the session `legion-daemon:<PROJECT>`). Then it reads each back through
// Dispatch's own reads (teardownEffectsDispatchFaults), a fact Dispatch does not answer as the
// world states it failing the setup by its part.
func (r *teardownEffectsRun) seedDispatchParts(world teardownEffectsWorld) {
	others := world.Dispatch.Issues
	for _, other := range others {
		path := "/api/v1/issues/" + other.Key
		if other.Status != "triage" {
			r.dispatch.user(http.MethodPost, path+"/status-change", map[string]any{"status": other.Status}, http.StatusOK, nil)
		}
		if len(other.BlockedBy) > 0 {
			r.dispatch.user(http.MethodPatch, path, map[string]any{"blocked_by": other.BlockedBy}, http.StatusOK, nil)
		}
		if other.ClaimedBy != nil {
			r.agentSend(http.MethodPost, path+"/claim", map[string]any{"actor": map[string]string{"kind": "session", "id": *other.ClaimedBy}})
		}
	}
	ranked := slices.DeleteFunc(slices.Clone(others), func(other teardownEffectsDispatchIssue) bool { return other.Rank == nil })
	slices.SortFunc(ranked, func(a, b teardownEffectsDispatchIssue) int { return *a.Rank - *b.Rank })
	for i, other := range ranked {
		rank := map[string]string{}
		if i > 0 {
			rank["after"] = ranked[i-1].Key
		} else {
			ranks := r.projectRanks()
			head := ""
			for key, at := range ranks {
				if key != other.Key && (head == "" || at < ranks[head]) {
					head = key
				}
			}
			if head == "" {
				continue
			}
			rank["before"] = head
		}
		r.dispatch.user(http.MethodPatch, "/api/v1/issues/"+other.Key, map[string]any{"rank": rank}, http.StatusOK, nil)
	}
	generation := uint64(1)
	if world.Daemon != nil && world.Daemon.Issue != nil {
		generation = world.Daemon.Issue.Generation
	}
	for _, run := range world.Dispatch.Steps {
		path := "/api/v1/issues/" + r.key + "/steps/" + uuid.NewString()
		write := map[string]any{"actor": map[string]string{"kind": "session", "id": "legion-daemon:" + r.cfg.Project}, "task": run.Task, "step": run.Step, "kind": run.Kind, "role": *run.Role,
			"agent": teardownEffectsSeededRun.Agent, "model": teardownEffectsSeededRun.Model, "generation": generation, "started_at": run.StartedAt}
		if run.EndedAt != nil {
			write["ended_at"], write["result"] = *run.EndedAt, *run.Result
		}
		r.agentSend(http.MethodPut, path, write)
		if run.PlanRating != nil {
			rating := maps.Clone(write)
			rating["rating"], rating["rating_reason"] = *run.PlanRating, ""
			if run.PlanReason != nil {
				rating["rating_reason"] = *run.PlanReason
			}
			r.agentSend(http.MethodPut, path+"/rating", rating)
		}
	}
	issues := map[string]teardownEffectsIssueRead{}
	read := func(key string) {
		var answer teardownEffectsIssueRead
		r.dispatch.agentGet("/api/v1/issues/"+key, &answer)
		issues[key] = answer
	}
	read(r.key)
	for _, other := range others {
		read(other.Key)
	}
	var runs []teardownEffectsStepRead
	if world.Dispatch.Steps != nil {
		var listed struct {
			Tasks []struct{ Steps []teardownEffectsStepRead }
		}
		r.dispatch.agentGet("/api/v1/issues/"+r.key+"/steps", &listed)
		for _, task := range listed.Tasks {
			runs = append(runs, task.Steps...)
		}
	}
	if faults := teardownEffectsDispatchFaults(world, issues, r.projectRanks(), runs); len(faults) > 0 {
		r.fail(fmt.Errorf("the replay's Dispatch does not hold the case's world: %s", strings.Join(faults, "; ")))
	}
}

// projectRanks is the rank of every issue of the run's project, as Dispatch's listing answers it.
func (r *teardownEffectsRun) projectRanks() map[string]string {
	var listed []struct{ Key, Rank string }
	r.dispatch.agentGet("/api/v1/issues?project="+r.cfg.Project, &listed)
	ranks := make(map[string]string, len(listed))
	for _, issue := range listed {
		ranks[issue.Key] = issue.Rank
	}
	return ranks
}

// agentSend sends body to path with the agent's bearer token and requires Dispatch to take it, the
// harness's failure (r.fail) naming Dispatch's answer otherwise.
func (r *teardownEffectsRun) agentSend(method, path string, body any) []byte {
	r.t.Helper()
	encoded, err := json.Marshal(body)
	if err != nil {
		r.t.Fatal(err)
	}
	request, err := http.NewRequest(method, r.dispatch.base+path, bytes.NewReader(encoded))
	if err != nil {
		r.t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer "+r.dispatch.token)
	status, answer := r.dispatch.do(request)
	if status < 200 || status > 299 {
		r.fail(fmt.Errorf("Dispatch %s %s = %d %s", method, path, status, answer))
	}
	return answer
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
	var issue struct {
		Key               string
		PrimaryArtifactID string `json:"primary_artifact_id"`
	}
	r.dispatch.user(http.MethodPost, "/api/v1/issues", fields, http.StatusCreated, &issue)
	if issue.Key != key {
		t.Fatalf("created issue %s, want %s", issue.Key, key)
	}
	if key == r.key && issue.PrimaryArtifactID != "" {
		r.documents["spec.md"] = issue.PrimaryArtifactID
	}
}

// seed records the case's daemon state. A child (the world names its parent) stands in its parent's
// tree, as lane R's daemon stand-in holds it (`--tree`, the parent): its record, its claim and its
// production check name that tree, it was not handed over itself, and the tree's lifecycle stands
// open under the workflow. Where the world holds nothing of the parent's own daemon state, the
// parent is recorded with the least that makes its tree run, without which admission takes the
// child's tree for one that finished and releases its slot (admit's releaseSlots): a root handed
// over, in Dispatch's status, at admitted, with no slot, no claim and no phases row, so no worker of
// it is owed a start (admit's owedOf) and it lingers nowhere. A controller case's issue that is a
// child is not seeded: the daemon knows that issue and has not admitted it, and under a running
// tree a child the daemon has not admitted would be admitted into it.
//
// The typed daemon parts go in as the daemon's store holds them: the role's claim on claim.issue
// in claim.tree, admitted, at the generation of its issue's record; the case's record as
// daemon.issue states it; each of daemon.records, the parent's among them where the world records
// it, a root that stands in a phase of its own tree with that tree's lifecycle open; each architect
// claim, the case's and the records', in its state, but the role's own, which is the replay's claim;
// the role's phases row on daemon.entry's entry, with whether a move back raised it and the owed
// task as the entry's own task, and the claim's task at the turn's entry (delivery.Entry), so the
// daemon itself queues the owed task as the claim's next delivery (workflow's SendOwedEntry); an
// admission slot for each issue daemon.admission names active, its waiting line and blocked issues
// being what the daemon makes of those records and Dispatch's links; and each gate row of
// daemon.gate on the claim's tree root, naming the document the replay's Dispatch gives that file
// name (gateDocuments). readBackDaemon then holds the booted daemon's own reads to them.
func (r *teardownEffectsRun) seed(world teardownEffectsWorld, task string) {
	t := r.t
	t.Helper()
	parts := world.Daemon.teardownEffectsDaemonParts
	if task == "" {
		task = "Continue " + world.Dispatch.Issue.Title + ". Issue: " + r.key + ". Phase: " + string(world.Daemon.Phase) + "."
	}
	listed := r.dispatch.issue(r.key)
	now := r.clock.Now()
	delivery := supervise.Delivery{ID: "teardown-" + randomSuffix(t), Task: teardownEffectsTaskMarker + "\n" + task, Generation: 1, Phase: world.Daemon.Phase, QueuedAt: now}
	if parts.Entry != nil {
		delivery.Entry = parts.Entry.Turn
	}
	// The controller's claim holds no issue, no step and no tree: its case's issue is one the
	// daemon knows and has not admitted, and its wake is a delivery to the controller's claim,
	// which the daemon's keeper launches at boot and hands its start message after this one.
	controller := r.role == claim.RoleController
	if controller {
		delivery.Phase = ""
	}
	tree, parent := r.key, world.Dispatch.Issue.Parent
	var issueParent *string
	if parent != "" {
		if controller {
			r.fail(fmt.Errorf("the controller case's issue %s is a child of %s, which the replay does not seed", r.key, parent))
		}
		tree, issueParent = parent, &parent
	}
	generation := uint64(1)
	var linger *time.Time
	if stated := parts.Issue; stated != nil {
		tree, issueParent, generation, linger = stated.Tree, stated.Parent, stated.Generation, stated.LingerUntil
	}
	stated := map[string]teardownEffectsDaemonRecord{}
	for _, each := range parts.Records {
		stated[each.Key] = each
	}
	_, parentRecorded := stated[parent]
	// The trees whose lifecycle stands open under the workflow: the claim's, the case's where the
	// world states no record of it, and each whose root stands in a phase of its own.
	trees := map[string]bool{r.tree: true}
	if parts.Issue == nil {
		trees[tree] = true
	}
	runs := func(p phase.Phase) bool { return p != phase.Admitted && p != phase.Done }
	if tree == r.key && runs(world.Daemon.Phase) {
		trees[tree] = true
	}
	for _, each := range parts.Records {
		if each.Key == each.Tree && runs(each.Phase) {
			trees[each.Key] = true
		}
	}
	r.gateDocuments(world)
	ctx := t.Context()
	if err := store.Transact(ctx, r.st.Pool(), func(ctx context.Context, tx pgx.Tx) error {
		if parent != "" && !parentRecorded {
			root := r.dispatch.issue(parent)
			var status struct{ Status string }
			r.dispatch.agentGet("/api/v1/issues/"+parent, &status)
			if err := r.records.PutIssue(ctx, tx, record.Issue{Key: parent, Title: root.Title, Project: r.cfg.Project, Tree: parent, Repo: r.repo, GitHubRepo: r.repo, DefaultBranch: r.base, Phase: phase.Admitted,
				Generation: 1, Status: status.Status, DispatchStatus: status.Status, Rank: root.Rank, LastDispatchSeq: root.LastSeq, HandedOver: true}); err != nil {
				return err
			}
		}
		for _, each := range parts.Records {
			held, dispatchStatus := r.dispatchIssue(each.Key)
			if dispatchStatus == "" {
				dispatchStatus = each.Status
			}
			title := held.Title
			if title == "" {
				title = each.Key
			}
			if err := r.records.PutIssue(ctx, tx, record.Issue{Key: each.Key, Title: title, Project: r.cfg.Project, Tree: each.Tree, Parent: each.Parent, Repo: r.repo, GitHubRepo: r.repo, DefaultBranch: r.base, Phase: each.Phase,
				Generation: each.Generation, Status: each.Status, DispatchStatus: dispatchStatus, Rank: held.Rank, LastDispatchSeq: held.LastSeq, HandedOver: each.Key == each.Tree}); err != nil {
				return err
			}
		}
		issue := record.Issue{Key: r.key, Title: listed.Title, Project: r.cfg.Project, Tree: tree, Parent: issueParent, Repo: r.repo, GitHubRepo: r.repo, DefaultBranch: r.base, Phase: world.Daemon.Phase, Generation: generation,
			Status: world.Dispatch.Issue.Status, DispatchStatus: world.Dispatch.Issue.Status, Rank: listed.Rank, LastDispatchSeq: listed.LastSeq, HandedOver: tree == r.key}
		// A tree lingers after its root closes: the linger is the root's, never a child's. The
		// world's record of the issue, where it states one, says whether it lingers.
		if parts.Issue != nil {
			issue.LingerUntil = linger
		} else if issue.Phase == phase.Done && parent == "" {
			linger := now.Add(48 * time.Hour)
			if wall := time.Now().Add(time.Hour); linger.Before(wall) {
				linger = wall
			}
			issue.LingerUntil = &linger
		}
		if err := r.records.PutIssue(ctx, tx, issue); err != nil {
			return err
		}
		if step, entry := world.Daemon.Step, parts.Entry; (step != nil || entry != nil) && !controller {
			row := record.PhaseRow{Issue: r.key, Role: r.role, Claim: r.token}
			if step != nil {
				row.StepPlace, row.StepTask = step.Place, step.Name
			}
			if entry != nil {
				row.Entry, row.EntryMovedBack = entry.Phase, entry.MovedBack
				if entry.OwedTask != nil {
					row.EntryTask = *entry.OwedTask
				}
				row.Given = record.GivenTask{Task: delivery.Task, Entry: entry.Turn, Generation: generation}
			}
			if err := r.records.PutPhase(ctx, tx, row); err != nil {
				return err
			}
		}
		number := 0
		if pull, ok := r.github.forBranch(r.branch); ok {
			number = pull.Number
			if err := r.records.PutPullRequest(ctx, tx, record.PullRequest{Issue: r.key, Repo: r.repo, Number: number, Branch: r.branch, Base: pull.Base, HeadSHA: pull.Head.SHA, State: record.PullRequestState(pull.State), MergeSHA: pull.MergeCommit, Failing: []string{}, Mergeability: record.MergeabilityMergeable, MergeState: teardownEffectsMergeState(pull)}); err != nil {
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
		for _, row := range parts.Gate {
			if err := r.records.PutGate(ctx, tx, record.DesignGate{Issue: r.tree, Step: record.DesignStep(row.Step), ArtifactID: r.gateIDs[row.Step], LatestVersion: row.Current, ApprovedVersion: row.Approved, EverOpened: row.Approved != nil}); err != nil {
				return err
			}
		}
		if admission := parts.Admission; admission != nil {
			return r.admit(ctx, tx, admission.Active...)
		}
		if issue.Phase != phase.Done && !controller {
			return r.admit(ctx, tx, r.key)
		}
		return nil
	}); err != nil {
		t.Fatalf("seed daemon state: %v", err)
	}
	// The claim serves the run of its issue's record (HANDOFF_STALE_GENERATION otherwise).
	claimed := generation
	if each, ok := stated[r.claimIssue]; ok {
		claimed = each.Generation
	} else if r.claimIssue != r.key {
		claimed = 1
	}
	delivery.Generation = claimed
	c := supervise.Claim{Token: r.token, Project: r.project, Tree: r.tree, Issue: r.claimIssue, Role: r.role, Generation: claimed, State: supervise.StateLaunching, BootTokenHash: supervise.HashBootToken("seed-" + randomSuffix(t))}
	if delivery.CheckLook.Look == 0 {
		c.ServingGeneration = claimed
	}
	if controller {
		c.Tree, c.Issue = "", ""
		if err := r.st.PutClaim(ctx, c); err != nil {
			t.Fatal(err)
		}
	} else {
		for open := range trees {
			if _, err := r.st.OpenTreeLifecycle(ctx, r.project, open, treelifecycle.AuthorityWorkflow); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := r.st.AdmitClaim(ctx, c); err != nil {
			t.Fatal(err)
		}
	}
	// Each architect claim the world states, in its state, but the role's own, which is the
	// replay's claim, whose state its pane moves.
	architect := func(key, inTree string, at uint64, held *teardownEffectsArchitect) {
		if held == nil || r.role == claim.RoleArchitect && key == r.claimIssue {
			return
		}
		token, err := claim.NewToken(r.project, key, claim.RoleArchitect)
		if err != nil {
			t.Fatal(err)
		}
		if err := r.st.PutClaim(ctx, supervise.Claim{Token: token, Project: r.project, Tree: inTree, Issue: key, Role: claim.RoleArchitect, Generation: at, State: supervise.ClaimState(held.State),
			BootTokenHash: supervise.HashBootToken("seed-" + randomSuffix(t))}); err != nil {
			r.fail(fmt.Errorf("seed the architect claim of %s in state %s: %w", key, held.State, err))
		}
	}
	architect(r.key, tree, generation, parts.Architect)
	for _, each := range parts.Records {
		architect(each.Key, each.Tree, each.Generation, each.Architect)
	}
	if err := r.st.PutDelivery(ctx, r.token, delivery); err != nil {
		t.Fatal(err)
	}
	writePrompt(t, r.cfg, r.token)
}

// dispatchIssue is the replay's Dispatch's read of key, as the seed copies it into the daemon's
// record, and its status; both empty where Dispatch holds no such issue.
func (r *teardownEffectsRun) dispatchIssue(key string) (handoffDispatchIssue, string) {
	request, err := http.NewRequest(http.MethodGet, r.dispatch.base+"/api/v1/issues/"+key, nil)
	if err != nil {
		r.t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+r.dispatch.token)
	status, answer := r.dispatch.do(request)
	if status != http.StatusOK {
		return handoffDispatchIssue{}, ""
	}
	var held handoffDispatchIssue
	var state struct{ Status string }
	if err := json.Unmarshal(answer, &held); err != nil {
		r.t.Fatal(err)
	}
	if err := json.Unmarshal(answer, &state); err != nil {
		r.t.Fatal(err)
	}
	return held, state.Status
}

// gateDocuments finds the document of each step of the world's design gate (daemon.gate) among the
// documents the replay's Dispatch holds of the claim's tree root, by its file name, as lane R's
// seed finds it (seed.ts seedCaseDispatch): its id goes in gateIDs, by step, and in artifacts under
// the id the case names the document by. A document the root does not hold fails the setup, naming
// the part.
func (r *teardownEffectsRun) gateDocuments(world teardownEffectsWorld) {
	rows := world.Daemon.Gate
	if len(rows) == 0 {
		return
	}
	request, err := http.NewRequest(http.MethodGet, r.dispatch.base+"/api/v1/issues/"+r.tree, nil)
	if err != nil {
		r.t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+r.dispatch.token)
	status, answer := r.dispatch.do(request)
	var root teardownEffectsIssueRead
	if status != http.StatusOK || json.Unmarshal(answer, &root) != nil {
		r.fail(fmt.Errorf("world.daemon.gate: the replay's Dispatch holds no issue %s, the claim tree's root whose documents the gate names (%d %s)", r.tree, status, answer))
		return
	}
	for i, row := range rows {
		at := slices.IndexFunc(root.Artifacts, func(document struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		}) bool {
			return document.Name == row.Document
		})
		if at < 0 {
			r.fail(fmt.Errorf("world.daemon.gate[%d].document %s: the replay's Dispatch holds no document of %s by that name", i, row.Document, r.tree))
			return
		}
		r.gateIDs[row.Step] = root.Artifacts[at].ID
		if row.ArtifactID != "" {
			r.artifacts[row.ArtifactID] = root.Artifacts[at].ID
		}
	}
}

// mapRunArtifacts takes into artifacts the id lane R's scratch Dispatch gave each gate step's
// document, which the run's calls name it by (seed.ts's dispatch-fixture.json `gate`): the replay's
// call names the replay's own.
func (r *teardownEffectsRun) mapRunArtifacts(runDir string) {
	file := filepath.Join(runDir, "dispatch-fixture.json")
	if _, err := os.Stat(file); err != nil {
		return
	}
	var fixture struct{ Gate map[string]string }
	teardownEffectsReadJSON(r.t, file, &fixture)
	for step, id := range fixture.Gate {
		if given := r.gateIDs[step]; given != "" {
			r.artifacts[id] = given
		}
	}
}

// teardownEffectsMergeState is the merge state the daemon's record of pull holds at the seed: the
// world's (mergeStateStatus, in the record's lower case) where it states one the record names, else
// clean.
func teardownEffectsMergeState(pull teardownEffectsPull) record.MergeState {
	state := record.MergeState(strings.ToLower(pull.MergeStateStatus))
	switch state {
	case record.MergeStateClean, record.MergeStateUnstable, record.MergeStateBlocked, record.MergeStateDirty, record.MergeStateBehind, record.MergeStateDraft, record.MergeStateUnknown:
		return state
	}
	return record.MergeStateClean
}

// readBackDaemon holds the booted daemon's own reads (`read_record` of each issue, `read_state`'s
// admission) to the world's typed daemon parts (teardownEffectsDaemonFaults) for as long as the
// daemon's boot and first admission passes take to settle, and fails the setup, naming each part
// the daemon still answers otherwise, after thirty seconds.
func (r *teardownEffectsRun) readBackDaemon(world teardownEffectsWorld) {
	read := func(key string) *api.Issue {
		status, body := r.d.request(http.MethodGet, "/legion/v1/issues/"+key+"/record", nil, false)
		if status == http.StatusNotFound {
			return nil
		}
		var issue api.Issue
		if status != http.StatusOK || json.Unmarshal(body, &issue) != nil {
			r.fail(fmt.Errorf("read_record of %s answered %d %s", key, status, body))
		}
		return &issue
	}
	deadline := time.Now().Add(30 * time.Second)
	for {
		faults := teardownEffectsDaemonFaults(world, r.role, read, r.d.state().Admission, r.gateIDs)
		if len(faults) == 0 {
			return
		}
		if time.Now().After(deadline) {
			r.fail(fmt.Errorf("the real daemon does not answer the case's world: %s", strings.Join(faults, "; ")))
			return
		}
		time.Sleep(250 * time.Millisecond)
	}
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
	if err := r.uploadDocument(teardownEffectsActor{Kind: "user"}, name, body); err != nil {
		r.fail(err)
	}
}

// uploadDocument keeps the id Dispatch assigned, for later comments on the document's words.
func (r *teardownEffectsRun) uploadDocument(actor teardownEffectsActor, name, body string) error {
	var answer struct {
		Artifact struct{ ID string }
	}
	if err := r.dispatchPost(actor, "/api/v1/issues/"+r.key+"/artifacts", map[string]any{"name": name, "content": body}, &answer); err != nil {
		return err
	}
	if answer.Artifact.ID == "" {
		return fmt.Errorf("Dispatch's upload of %q answered no artifact id", name)
	}
	r.documents[name] = answer.Artifact.ID
	return nil
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

// call makes one legion call through the real pane at the case time at, with args, the call's op and
// its arguments. A document the call names by the case's id or lane R's (a register_gate's
// artifactId) is named by the id the replay's Dispatch gave it (artifacts).
func (r *teardownEffectsRun) call(seq int, at time.Time, args map[string]any) ([]teardownEffectsAnswer, []teardownEffectsHTTP, []teardownEffectsHandoff, toolResult) {
	t := r.t
	t.Helper()
	if id, ok := args["artifactId"].(string); ok && r.artifacts[id] != "" {
		args = maps.Clone(args)
		args["artifactId"] = r.artifacts[id]
	}
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

// teardownEffectsReviewSettle bounds phaseAfter's wait for the daemon to record the reviews the
// role submitted. The read that records them reads the pull request, follows its head and reads
// its reviews, and the daemon bounds each of those reads by issueRead, which bounds the wait too.
// The scratch GitHub answers each in milliseconds, and in a controls Job the read applied the
// review tens of milliseconds after the completion was answered. A review the daemon has not
// recorded by then is one the read did not apply, not one it is still reading, and the phase is
// read as it stands.
const teardownEffectsReviewSettle = issueRead

// teardownEffectsReviewPoll is how often that wait reads the daemon's store again: each time is a
// read of the database, so the wait does not ask a hundred times a second (testwait.Holds).
const teardownEffectsReviewPoll = 25 * time.Millisecond

// phaseAfter is the issue's phase after the legion call named call, whose requests to the daemon
// are replies (none for a change, which is no call). The scratch GitHub sends no webhook, so the
// daemon hears of a review the role submitted only from its own read of the issue's pull request,
// which a completion the daemon took queues and which runs after the completion has been answered
// (teardownEffectsReadQueued). Read at once, the phase would be the one the completion left,
// before that read applies the review and makes the move the review makes. So after such a
// completion the phase is read once the daemon has recorded each review the call left it to
// record (awaitedReviews, unrecorded), or as it stands at teardownEffectsReviewSettle, and at once
// where there is none; the wait is named in the test log alone (teardownEffectsSettle).
func (r *teardownEffectsRun) phaseAfter(call string, replies []teardownEffectsHTTP) string {
	var pr record.PullRequest
	var awaited []intake.PullRequestReview
	if teardownEffectsReadQueued(replies) {
		pr, awaited = r.awaitedReviews()
	}
	return teardownEffectsSettle{bound: teardownEffectsReviewSettle, poll: teardownEffectsReviewPoll, unrecorded: r.unrecorded(pr),
		phase: func() string { return string(r.issue(r.key).Phase) }, logf: r.t.Logf}.phaseAfter(call, awaited)
}

// awaitedReviews are the reviews teardownEffectsAwaitedReviews names of the pull request whose
// reviews the daemon's read of the issue judges (reviewedPull), judged by the session the
// reviewer's claim records now, and that pull request.
func (r *teardownEffectsRun) awaitedReviews() (record.PullRequest, []intake.PullRequestReview) {
	var session string
	if err := store.TransactWith(r.t.Context(), r.st.Pool(), pgx.TxOptions{AccessMode: pgx.ReadOnly}, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		session, err = r.engine.ReviewerSession(ctx, tx, r.key)
		return err
	}); err != nil {
		r.t.Fatalf("read the session of %s's reviewer: %v", r.key, err)
	}
	pr, reviews, ok := r.reviewedPull(r.recordedPull())
	if !ok {
		return pr, nil
	}
	return pr, teardownEffectsAwaitedReviews(pr, reviews, r.worldReviews[pr.Number], r.engine, session)
}

// unrecorded answers those of the reviews of pr it is given that the daemon has not recorded yet.
// The daemon has recorded a review once the pull request's record has seen it or a newer one
// (record.PullRequest.ReviewSeen), or once the daemon has applied the fact its read of the reviews
// made of it (readReviews' review-read event, under the id readReviews gives it, in
// processed_events), whatever the workflow made of that fact: a review of an earlier round is
// applied and records no order (workflow's earlierRound). Both are the daemon's own records, and
// the phase move a review makes commits in the transaction that writes them. The daemon gives no
// notice of a fact it applied, so the wait asks this again every teardownEffectsReviewPoll.
func (r *teardownEffectsRun) unrecorded(pr record.PullRequest) func([]intake.PullRequestReview) []intake.PullRequestReview {
	return func(left []intake.PullRequestReview) []intake.PullRequestReview {
		events := make([]string, len(left))
		for i, review := range left {
			events[i] = readEventID("review-read", r.cfg.Project, pr.Repo, pr.Number, strconv.FormatInt(review.ID, 10), clock(review.SubmittedAt))
		}
		applied, err := intake.Applied(r.t.Context(), r.st.Pool(), "github", events)
		if err != nil {
			r.t.Fatalf("read which of %s the daemon applied: %v", teardownEffectsReviewsText(left), err)
		}
		seen := pr.ReviewSeen
		if recorded := r.recordedPull(); recorded != nil && recorded.Number == pr.Number {
			seen = recorded.ReviewSeen
		}
		var still []intake.PullRequestReview
		for i, review := range left {
			if !applied[events[i]] && review.Order().After(seen) {
				still = append(still, review)
			}
		}
		return still
	}
}

// recordedPull is the issue's pull request as the daemon's store records it now, nil where it
// records none: its row alone, as the daemon's own read of the issue reads it (issueRecord,
// issuePullRequest).
func (r *teardownEffectsRun) recordedPull() *record.PullRequest {
	_, pr, err := issueRecord(r.t.Context(), r.st.Pool(), r.records, r.key)
	if err != nil {
		r.t.Fatalf("read the pull request the daemon records for %s: %v", r.key, err)
	}
	return pr
}

// reviewedPull is the pull request whose reviews the daemon's read of the issue judges
// (issuePullRequest), given recorded, the issue's pull request as the daemon records it: recorded
// where the record holds it open, and otherwise the newest the scratch GitHub holds open on the
// issue's branch, which the read records first, with the review the record has seen only where it
// reopens the one the record holds. Its head is the scratch GitHub's, which the read follows the
// record to before it reads the reviews (readIssuePullRequest), and reviews is the scratch
// GitHub's list of its reviews. It answers false where neither holds one open: the read then
// reads no review.
func (r *teardownEffectsRun) reviewedPull(recorded *record.PullRequest) (record.PullRequest, []teardownEffectsReview, bool) {
	g := r.github
	g.mu.Lock()
	defer g.mu.Unlock()
	var held teardownEffectsPull
	ok := false
	pr := record.PullRequest{Issue: r.key, Repo: r.repo, State: record.PullRequestOpen}
	if recorded != nil && recorded.State == record.PullRequestOpen {
		pr = *recorded
		held, ok = g.pulls[pr.Number]
	} else {
		for _, pull := range g.pulls {
			if pull.Head.Ref == r.branch && pull.State == "open" && pull.Number > held.Number {
				held, ok = pull, true
			}
		}
		pr.Number = held.Number
		if recorded != nil && recorded.Number == held.Number {
			pr.ReviewSeen = recorded.ReviewSeen
		}
	}
	pr.HeadSHA = held.Head.SHA
	return pr, held.Reviews, ok
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
		turned.call.PhaseAfter, turned.after = r.phaseAfter(control.String(), replies), r.productionCheck()
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
	// check holds the replay to what the trace states of the state its last write left, once each
	// change after the last call is made (the trace's documents: documentFaults); nil where the
	// trace states nothing of it.
	check func() error
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
		replayed.PhaseAfter = r.phaseAfter(turn.call.ID, replies)
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
		} else if script.check != nil {
			if err := script.check(); err != nil {
				harness(err)
			}
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
// records its refusal under the action's own seq and case time. A staging action, a GitHub call
// that changes no pull request (a read: teardownEffectsTraceRead) and an action of a carried kind
// (an Envoy message, a Dispatch read, a task agent's start, a probe of production, a diff read:
// teardownEffectsCarriedKinds) change nothing the replay holds, and are not made; each action that
// is made keeps its own seq and case time however many of them stand between. Where the trace
// states its documents, the script's check holds the real Dispatch to them after the trace's last
// write (documentFaults). It also answers the seq of each call's action, in the order of the calls.
func (r *teardownEffectsRun) traceScript(caseID, name string, trace teardownEffectsTrace) (teardownEffectsScript, []int) {
	var script teardownEffectsScript
	var seqs []int
	var pending []teardownEffectsApplied
	for _, a := range trace.Actions {
		source := fmt.Sprintf("%s %s trace, action %d", caseID, name, a.Seq)
		switch {
		case teardownEffectsMakesNothing(a):
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
	if len(trace.Documents) > 0 {
		script.check = func() error { return r.documentFaults(caseID, name, trace.Documents) }
	}
	return script, seqs
}

// documentFaults holds each document the trace `name` of case caseID states to the document of the
// issue the replay's Dispatch holds by its file name, after the trace's last write: its latest
// version and that version's text, read through Dispatch's own routes, with each id the replay's
// Dispatch gave a document the world names by an id of its own written as the world's (artifacts,
// turned round). A document not held as stated is an error, the replay's HARNESS verdict.
func (r *teardownEffectsRun) documentFaults(caseID, name string, stated []teardownEffectsDocument) error {
	get := func(path string, into any) error {
		request, err := http.NewRequest(http.MethodGet, r.dispatch.base+path, nil)
		if err != nil {
			return err
		}
		request.Header.Set("Authorization", "Bearer "+r.dispatch.token)
		status, answer := r.dispatch.do(request)
		if status != http.StatusOK {
			return fmt.Errorf("Dispatch GET %s = %d %s", path, status, answer)
		}
		return json.Unmarshal(answer, into)
	}
	var issue teardownEffectsIssueRead
	if err := get("/api/v1/issues/"+r.key, &issue); err != nil {
		return err
	}
	held := map[string]teardownEffectsDocument{}
	for _, artifact := range issue.Artifacts {
		var read struct {
			Kind     string `json:"kind"`
			Versions []struct {
				Number int `json:"number"`
			} `json:"versions"`
		}
		if err := get("/api/v1/artifacts/"+artifact.ID, &read); err != nil {
			return err
		}
		latest := 0
		for _, version := range read.Versions {
			latest = max(latest, version.Number)
		}
		if read.Kind != "doc" || latest == 0 {
			continue
		}
		var text struct {
			Markdown string `json:"markdown"`
		}
		if err := get(fmt.Sprintf("/api/v1/artifacts/%s/versions/%d", artifact.ID, latest), &text); err != nil {
			return err
		}
		held[artifact.Name] = teardownEffectsDocument{Name: artifact.Name, Version: latest, Body: text.Markdown}
	}
	ids := map[string]string{}
	for stated, given := range r.artifacts {
		ids[given] = stated
	}
	if faults := teardownEffectsDocumentFaults(stated, held, ids); len(faults) > 0 {
		return fmt.Errorf("%s %s trace: the replay's Dispatch does not hold the documents the trace states: %s", caseID, name, strings.Join(faults, "; "))
	}
	return nil
}

// applyTraceAction makes a trace's push, GitHub change or Dispatch write again at its case time, with
// the run replay's own writers: a push moves the remote's ref from the head it holds now
// (applyPush); a GitHub change is the trace's own `change`, or the one gh-standin keeps of the
// request the action records (teardownEffectsTraceChange), its commits mapped and what it leaves to
// the state filled from the replay's own (teardownEffectsScratch.resolve), made by the rule a run's
// change is made by (teardownEffectsScratch.change); a review's Legion footer names the replay's
// session and commit (teardownEffectsReviewFooter), since every action of a trace is its role's own;
// a Dispatch write is made by the role's session as the run replay makes one
// (teardownEffectsTraceWrite, applyDispatch), but a request-approval, which the role's session makes
// through the real dispatch CLI as a refused write (requestApproval). It answers that write's
// refusal, and nil for every change it made.
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
		if p.Action == "review" {
			p.Body = teardownEffectsReviewFooter(p.Body, func(string) bool { return true }, r.session, r.sha)
		}
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
		return r.applyGitHub(*c.pull, own)
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
// change the scratch GitHub cannot hold is refused by name. A review whose Legion footer names the
// run's own session (own) names the replay's pane's in its place (teardownEffectsReviewFooter), as
// applyDispatch writes that session's records; the run's commits are the replay's already.
func (r *teardownEffectsRun) applyGitHub(p teardownEffectsPullMutation, own string) error {
	if p.Action == "review" {
		p.Body = teardownEffectsReviewFooter(p.Body, func(named string) bool { return named == own }, r.session, func(commit string) string { return commit })
	}
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
		if err := r.dispatchPost(actor, prefix+"/handoffs/"+word, map[string]any{"body": body}, nil); err != nil {
			return err
		}
		got := r.dispatch.latest(r.key, word)
		if got.Version != version {
			return fmt.Errorf("the replay wrote the %s handoff version %d, and the run's was version %d", word, got.Version, version)
		}
		r.handoffs[got.ID] = teardownEffectsHandoff{Phase: word, ID: got.ID, Version: got.Version}
		return nil
	case "comment.created":
		body, err := teardownEffectsCommentBody(e, b, r.documents, r.comments)
		if err != nil {
			return err
		}
		var answer struct{ ID string }
		if err := r.dispatchPost(actor, prefix+"/comments", body, &answer); err != nil {
			return err
		}
		if id := text("id"); id != "" {
			if answer.ID == "" {
				return fmt.Errorf("Dispatch's POST of comment %s answered no comment id", id)
			}
			r.comments[id] = answer.ID
		}
		return nil
	case "result.recorded":
		result, check, _ := b.result(text("result_id"))
		return r.recordResult(actor.ID, check, result)
	case "ask.opened":
		for _, ask := range b.Asks {
			if ask.ID == text("id") {
				return r.dispatchPost(actor, prefix+"/asks", map[string]any{"question": ask.Question, "options": ask.Options}, nil)
			}
		}
	case "artifact.created":
		doc, _ := b.document(e.documentID())
		return r.uploadDocument(actor, doc.Name, doc.Text)
	}
	return fmt.Errorf("the replay writes no %s", e.Type)
}

// dispatchPost posts body as actor: a session through the agent's bearer, naming it, and a person
// as the scratch Dispatch's person (handoffRecordsUser), whose login stands for the run's. It
// decodes Dispatch's answer into into when it is not nil.
func (r *teardownEffectsRun) dispatchPost(actor teardownEffectsActor, path string, body map[string]any, into any) error {
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
	if into != nil {
		if err := json.Unmarshal(answer, into); err != nil {
			return fmt.Errorf("decode Dispatch's answer to POST %s: %w", path, err)
		}
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

// teardownEffectsGitHub is the replay's GitHub as the daemon reads it: the pull requests, the
// repository's settings (the world's, laid over the fixture's own members) and each branch's rules
// (rules, nil where the world states none, which GitHub's 404 then answers).
type teardownEffectsGitHub struct {
	t          *testing.T
	mu         sync.Mutex
	repo, base string
	pulls      map[int]teardownEffectsPull
	settings   map[string]json.RawMessage
	rules      map[string][]json.RawMessage
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
			state := "clean"
			if pull.MergeStateStatus != "" {
				state = pull.MergeStateStatus
			}
			node := liveBatchNode(n, pull.Head.SHA, pull.Head.Ref, pull.Base, true, state)
			node["state"] = strings.ToUpper(pull.State)
			if pull.NodeID != "" {
				node["id"] = pull.NodeID
			}
			if pull.AutoMergeRequest != nil {
				node["autoMergeRequest"] = pull.AutoMergeRequest
			}
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
			repository := map[string]any{"full_name": g.repo, "default_branch": g.base}
			for name, value := range g.settings {
				repository[name] = value
			}
			send(repository)
			return
		}
		if branch, ok := strings.CutPrefix(req.URL.Path, prefix+"/rules/branches/"); ok && g.rules != nil {
			rules := g.rules[branch]
			if rules == nil {
				rules = []json.RawMessage{}
			}
			send(rules)
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
						reviews = append(reviews, map[string]any{"id": teardownEffectsReviewID(i), "state": review.State, "user": map[string]string{"login": review.Author}, "body": review.Body, "commit_id": review.Commit, "submitted_at": review.At})
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

// rest is the pull request as GitHub's REST API answers it: its node id, its merge state and its
// auto-merge request the world's where it states them. Its place in the merge queue is not served:
// the daemon reads none.
func (p teardownEffectsPull) rest() map[string]any {
	state := p.State
	if state == "merged" {
		state = "closed"
	}
	rest := map[string]any{"number": p.Number, "state": state, "draft": p.Draft, "title": p.Title, "body": p.Body, "head": map[string]string{"ref": p.Head.Ref, "sha": p.Head.SHA},
		"base": map[string]string{"ref": p.Base}, "merged": p.State == "merged", "merge_commit_sha": p.MergeCommit, "mergeable": true, "mergeable_state": string(teardownEffectsMergeState(p))}
	if p.NodeID != "" {
		rest["node_id"] = p.NodeID
	}
	if p.AutoMergeRequest != nil {
		rest["auto_merge"] = p.AutoMergeRequest
	}
	return rest
}
