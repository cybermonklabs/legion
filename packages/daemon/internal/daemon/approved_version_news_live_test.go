//go:build e2e

// The driver of part one of dispatch://INFRA-175's c-live: one approval request covers a root's
// spec and its Verification document, and an approved new version of either reaches the worker
// holding each affected issue's phase, naming what changed, without the daemon moving an issue or
// stopping a turn. scripts/e2e/approved-version-news.sh builds this driver with `-tags e2e` and
// owns the nats-server and the PostgreSQL it runs against.
//
// Nothing of the approval path is stood in for. Dispatch is the real server built from the
// checkout the script names (testbuild.RootEnv), started by the package's own scratch helper with
// its events on the script's broker, as startScratchDispatchWithNATS starts it, on a database of
// its own on LEGION_TEST_PG_DSN's server, with header identity: the person `smoke` approves every
// version through POST /api/v1/artifacts/{id}/reviews, and each tree's architect writes every new
// version through Dispatch's own edit route and opens its approval request through
// POST /api/v1/artifacts/{id}/approval-requests, the route `dispatch request-approval` calls. The
// daemon is the whole daemon (run, the function `legion start` calls, through startDaemonWithin),
// booted with dispatch_url at that server, on a database of its own on the same PostgreSQL, and it
// hears every document event Dispatch publishes over JetStream, as it does in production. Each
// role the proof acts as speaks the daemon's worker stream as `legion worker-shim` speaks it
// (shimwire frames) and calls the daemon's HTTP routes as the plugin does: registration,
// readiness, grants, register_gate, handoff_complete and request_backward_move.
//
// What is stood in for lies outside the approval path. The broker is a native nats-server the
// script starts and names in LEGION_E2E_NATS_URL. GitHub is the package's repositoriesGitHub,
// which startDaemonWithin boots the daemon with: each repository with default branch main, and
// every other request answered 404. Envoy's listener is the package's noticeListener, the two
// Apps' tokens are workflowTokenRecorder's placeholders, and no agent runs: each claim is launched
// by the package's fake runtime, which provisions each workspace in its own pod. A merged pull
// request is GitHub's own fact, an envelope on the ENVOY_NOTIFICATIONS stream as Envoy relays a
// webhook.
//
// The issues, their pull requests, their trees' slots and lifecycles and the claims the boot
// relaunches are seeded through the store, as every daemon test seeds its fixture, beside the
// Dispatch issues the steps write to. Where the schema holds the told record of a release that
// keeps one (the design_news_told table), each member seeded in a phase with work gets the holder
// row the store's upgrade writes for a tree running at the upgrade: the approved versions its work
// stood on, 1 of each document its tree registers. A schema without the table, which is a release
// from before that record, is seeded nothing there. The driver uses no Go symbol of that release:
// it reads the wire and the database alone, so the script can build it against a base commit's
// daemon, which is the run's negative control.
//
// Every check prints `   assert <step>: <what held>` once it holds, which the script counts, and
// a check that does not hold prints `FAIL <step>: <what did not hold>` and ends the run. Every wait
// is bounded, and its failure names what it waited for and what it last saw. Every check that
// nothing arrives watches a window in which a delivery the same run makes does arrive.
package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/runtime/fake"
	"github.com/sjawhar/legion/daemon/internal/shimwire"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/testbuild"
	"github.com/sjawhar/legion/daemon/internal/testnats"
	"github.com/sjawhar/legion/daemon/internal/testpg/testdb"
	"github.com/sjawhar/legion/daemon/internal/treelifecycle"
)

// The steps of part one, in the order they run: A2's approval of a new version is the run's first
// one-approval step, on a tree that registers its spec alone, because a daemon from before the
// change refuses A1's criteria registration under root-issues (GATE_STEP_NOT_CONFIGURED), and a
// run that began there would end before any delivery was tested, so the negative control would
// fail for a reason the change does not touch. The script counts each step's checks under these
// names.
const (
	versionNewsA2  = "a2-the-reviewer-is-told-the-changed-sections"
	versionNewsA1  = "a1-one-request-opens-both-rows"
	versionNewsA3  = "a3-a-checks-change-reaches-accepting-and-implementing"
	versionNewsA5  = "a5-the-tester-passes-and-the-reviewer-is-told"
	versionNewsA6  = "a6-a-merged-root-is-told-nothing"
	versionNewsA7  = "a7-the-merger-waits-then-reads-the-opening"
	versionNewsR6  = "r6-only-the-root-is-told-of-its-checks"
	versionNewsOff = "off-the-daemon-sends-nothing"
)

// versionNewsPerson is the person who approves every document, as Dispatch's trusted identity
// header names them: a person's approval is the only one the gate takes (intake's Actor.Person).
const versionNewsPerson = "smoke"

// versionNewsWait bounds every wait for something the daemon or Dispatch does on its own: an event
// on the bus, a reconcile's read of Dispatch and the outbox row it queues, a launch, a delivery.
// Each takes seconds; the bound is for a loaded pod.
const versionNewsWait = 2 * time.Minute

// The words each document is written with and then changed to. A spec's `## Summary` stays as it
// was, so a task that names it names a section that did not change; a spec's `## Design` changes.
const (
	versionNewsDesign      = "The reader takes one page at a time."
	versionNewsDesignAgain = "The reader takes two pages at a time, and keeps the second for the next read."
	versionNewsCheck       = "The read answers 200."
	versionNewsCheckAgain  = "The read answers 200 with the page it read."
)

// versionNewsAsk is the decision block the A3 implementing root's spec carries open, whose answer
// alone is the next version's change.
const versionNewsAsk = ":::ask{#wave urgency=\"med\" multiple=\"false\"}\nHow many regions take the first wave?\n\n" +
	"- Two: The two with the fewest tenants.\n- Five: Half of them.\n:::\n"

// versionNewsLive is one run of part one: the scratch Dispatch both daemon runs share, the booted
// daemon of the run in hand with its fixture, and the roles the driver acts as.
type versionNewsLive struct {
	liveProof
	natsURL  string
	dispatch *scratchDispatch
	client   *http.Client
	// repo is the repository the run in hand maps to its project, and rt and built the stand-ins
	// its daemon was booted with.
	repo  string
	rt    *fake.Runtime
	built *built
	// running is the step running now, which every check and every failure names, and named says
	// whether a failure of it has printed its FAIL line.
	running string
	named   bool
	// trees is the run's fixture by name, and claimants each role brought to ready, by token.
	trees     map[string]*versionNewsTree
	claimants map[claim.Token]*versionNewsClaimant
}

// versionNewsTree is one tree of the fixture: its members, the root first, the root's spec, and its
// Verification document and that document's one check ("" for a tree that registers its spec
// alone).
type versionNewsTree struct {
	name         string
	members      []*versionNewsIssue
	spec         string
	verification string
	check        string
}

// versionNewsIssue is one member of a tree: its key, its tree's root (its own key for the root),
// its parent ("" for the root), the phase it is seeded in, the roles whose claims the boot
// relaunches, and its pull request's number (0 for none).
type versionNewsIssue struct {
	key    string
	tree   string
	parent string
	phase  phase.Phase
	roles  []claim.Role
	pr     int
}

// versionNewsMember is the shape of one member a tree is made with.
type versionNewsMember struct {
	phase phase.Phase
	roles []claim.Role
}

// root is the tree's root member.
func (tree *versionNewsTree) root() *versionNewsIssue { return tree.members[0] }

// steps is the design-gate steps the tree registers: criteria beside spec when it has a
// Verification document.
func (tree *versionNewsTree) steps() []record.DesignStep {
	if tree.verification == "" {
		return []record.DesignStep{record.DesignStepSpec}
	}
	return []record.DesignStep{record.DesignStepSpec, record.DesignStepCriteria}
}

// TestAnApprovedVersionReachesItsHolderLive drives part one's steps, in the order the constants
// above give and say why, against one Dispatch: the first seven against a daemon gating
// `root-issues`, and the last against a second daemon, on a configuration and a database of its
// own, gating nothing.
func TestAnApprovedVersionReachesItsHolderLive(t *testing.T) {
	r := newVersionNewsLive(t)
	r.boot("root-issues", config.DesignGateRootIssues.Steps(), liveRepo, r.rootIssuesFixture)
	r.run(versionNewsA2, r.a2)
	r.run(versionNewsA1, r.a1)
	r.run(versionNewsA3, r.a3)
	r.run(versionNewsA5, r.a5)
	r.run(versionNewsA6, r.a6)
	r.run(versionNewsA7, r.a7)
	r.run(versionNewsR6, r.r6)
	r.d.stop()
	r.boot("off", config.DesignGateOff.Steps(), "acme/gadgets", r.offFixture)
	r.run(versionNewsOff, r.off)
}

// newVersionNewsLive takes the script's broker, makes the stream the daemon's intake consumes, and
// starts the run's Dispatch with its events on that broker. Unset, the broker skips the test, as
// every driver whose script owns a nats-server of its own skips.
func newVersionNewsLive(t *testing.T) *versionNewsLive {
	t.Helper()
	natsURL := os.Getenv(liveNATSURLEnv)
	if natsURL == "" {
		t.Skipf("%s is unset, so there is no broker to carry Dispatch's events; scripts/e2e/approved-version-news.sh starts a nats-server and sets it", liveNATSURLEnv)
	}
	// The checkout the script builds Dispatch from; unset (a plain `go test -tags e2e` in the
	// package directory), the checkout four directories up. The driver binary runs from the
	// caller's directory, which is why the script names it.
	checkout := os.Getenv(testbuild.RootEnv)
	if checkout == "" {
		checkout = filepath.Join("..", "..", "..", "..")
	}
	checkout, err := filepath.Abs(checkout)
	if err != nil {
		t.Fatal(err)
	}
	r := &versionNewsLive{natsURL: natsURL, client: &http.Client{Timeout: 30 * time.Second}}
	r.t = t
	r.running = "setup"
	js := workflowJetStream(t, natsURL)
	testnats.CreateStream(t, js, jetstream.StreamConfig{Name: "ENVOY_NOTIFICATIONS", Subjects: []string{"notifications.>"}})
	r.js = js
	// startScratchDispatchWithNATS's server, started through the options it wraps so that it runs
	// in the checkout the script names and writes its log beside the daemon's: the helper itself
	// finds the checkout four directories above the working directory, which is the package's,
	// and this binary runs from the caller's.
	options := scratchDispatchOptions{natsURL: natsURL, root: checkout}
	if daemonLog := os.Getenv(liveDaemonLogEnv); daemonLog != "" {
		file, err := os.OpenFile(filepath.Join(filepath.Dir(daemonLog), "dispatch.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
		if err != nil {
			t.Fatalf("open Dispatch's log: %v", err)
		}
		t.Cleanup(func() { file.Close() })
		options.log = file
	}
	r.dispatch = startScratchDispatchOn(t, options)
	r.note("Dispatch %s built from %s, its events on %s, header identity, the person %s", r.dispatch.base, checkout, natsURL, versionNewsPerson)
	return r
}

// boot readies a fresh configuration gating steps, on a database of its own, with a Dispatch
// project of its own mapped to repo; makes fixture's trees on Dispatch and seeds them into the
// store; boots the whole daemon; and brings every seeded claim to ready as its pane would.
func (r *versionNewsLive) boot(gates string, steps []record.DesignStep, repo string, fixture func()) {
	t := r.t
	t.Helper()
	r.running = "setup"
	cfg := workflowConfig(t, r.natsURL)
	// A Dispatch project key is at most ten characters (contracts ProjectKeyPattern), and the
	// daemon's project is its Dispatch project.
	cfg.Project = "AV" + randomSuffix(t)[:6]
	cfg.Projects = map[string]config.Project{cfg.Project: {}}
	cfg.AdmissionCap = 32
	cfg.Gates.Design = steps
	cfg.DispatchURL = r.dispatch.base
	// A database of the run's own: the daemon deduplicates a Dispatch event by its id alone
	// (processed_events), and a scratch Dispatch numbers its events from one, so a database an
	// earlier run left would take this run's events for ones it applied.
	cfg.PostgresDSN = testdb.Database(t, "legion_version_news")
	if err := os.WriteFile(cfg.DispatchTokenFile, []byte(r.dispatch.token+"\n"), 0o600); err != nil {
		t.Fatalf("write the Dispatch token file: %v", err)
	}
	notices := newNoticeListener()
	envoy := httptest.NewServer(notices)
	t.Cleanup(envoy.Close)
	cfg.EnvoyURL = envoy.URL
	project, err := claim.ProjectToken(cfg.Project)
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(context.Background(), cfg.PostgresDSN)
	if err != nil {
		t.Fatalf("open the store: %v", err)
	}
	t.Cleanup(st.Close)
	if _, err := st.Migrate(context.Background()); err != nil {
		t.Fatalf("migrate the store: %v", err)
	}
	r.liveProof = liveProof{t: t, cfg: cfg, js: r.js, notices: notices, st: st, records: record.NewStore(), project: project}
	r.repo = repo
	r.trees = map[string]*versionNewsTree{}
	r.claimants = map[claim.Token]*versionNewsClaimant{}
	r.rt, r.built = fake.NewRuntime(), &built{}
	r.rt.InPod = true
	r.dispatchAs(versionNewsPerson, http.MethodPost, "/api/v1/projects", map[string]string{"key": cfg.Project, "name": "Approved version news, " + gates}, nil)
	r.dispatchAs(versionNewsPerson, http.MethodPut, "/api/v1/settings/repo-projects/"+repo, map[string]any{"project": cfg.Project, "default": true}, nil)
	fixture()
	r.seed()
	o := fakeRuntime(r.rt, r.built)
	o.workflowTokens = &workflowTokenRecorder{}
	r.d = startDaemonWithin(t, cfg, o, r.log(), 2*time.Minute)
	r.note("daemon booted under gates.design: %s, project %s mapped to %s, dispatch_url %s", gates, cfg.Project, repo, r.dispatch.base)
	// The boot's relaunch of each seeded claim is its first launch, so none before it counts.
	for _, tree := range r.trees {
		for _, member := range tree.members {
			for _, role := range member.roles {
				r.connect(member, role, nil)
			}
		}
	}
}

// rootIssuesFixture is the first run's trees, each named for the step that drives it.
func (r *versionNewsLive) rootIssuesFixture() {
	architect, implementer, tester := claim.RoleArchitect, claim.RoleImplementer, claim.RoleTester
	reviewer, merger := claim.RoleReviewer, claim.RoleMerger
	r.tree("a2", false, false, versionNewsMember{phase.Reviewing, []claim.Role{architect, reviewer}})
	r.tree("a2-unmoved", false, false, versionNewsMember{phase.Reviewing, []claim.Role{architect, reviewer}})
	r.tree("a1", true, false, versionNewsMember{phase.Admitted, []claim.Role{architect}})
	r.tree("a3-accepting", true, false, versionNewsMember{phase.Accepting, []claim.Role{architect}})
	r.tree("a3-implementing", true, true, versionNewsMember{phase.Implementing, []claim.Role{architect, implementer}})
	r.tree("a5", true, false, versionNewsMember{phase.Testing, []claim.Role{architect, tester}},
		versionNewsMember{phase.Testing, []claim.Role{tester}})
	r.tree("a6", false, false, versionNewsMember{phase.Merging, []claim.Role{architect, merger}},
		versionNewsMember{phase.Reviewing, []claim.Role{reviewer}})
	r.tree("a7", true, false, versionNewsMember{phase.Retro, []claim.Role{architect, implementer}})
	r.tree("a7-open", true, false, versionNewsMember{phase.Retro, []claim.Role{architect, implementer}})
	r.tree("r6", true, false, versionNewsMember{phase.Implementing, []claim.Role{architect, implementer}},
		versionNewsMember{phase.Implementing, []claim.Role{implementer}}, versionNewsMember{phase.Testing, []claim.Role{tester}})
}

// offFixture is the second run's trees: one whose holders must hear nothing, and one whose
// tester's pass is the delivery the same window sees.
func (r *versionNewsLive) offFixture() {
	architect := claim.RoleArchitect
	r.tree("off", false, false, versionNewsMember{phase.Implementing, []claim.Role{architect, claim.RoleImplementer}},
		versionNewsMember{phase.Reviewing, []claim.Role{claim.RoleReviewer}})
	r.tree("off-control", false, false, versionNewsMember{phase.Testing, []claim.Role{architect, claim.RoleTester}})
}

// tree makes one tree on Dispatch, its root first and each child under it, each in progress with
// the legion label as a running daemon leaves them, the root's spec carrying a `## Summary` and a
// `## Design` (and the open decision block, for decision), and, for verification, the root's
// Verification document holding one check.
func (r *versionNewsLive) tree(name string, verification, decision bool, members ...versionNewsMember) {
	tree := &versionNewsTree{name: name}
	for i, shape := range members {
		title := fmt.Sprintf("Approved version news %s, member %d", name, i)
		body := map[string]any{"project": r.cfg.Project, "title": title, "force": true}
		member := &versionNewsIssue{phase: shape.phase, roles: shape.roles}
		if i == 0 {
			body["spec"] = versionNewsSpec(name, versionNewsDesign, decision)
		} else {
			member.parent = tree.root().key
			body["parent"] = member.parent
		}
		var created struct {
			Key     string `json:"key"`
			Primary string `json:"primary_artifact_id"`
		}
		r.dispatchAs(versionNewsPerson, http.MethodPost, "/api/v1/issues", body, &created)
		if created.Key == "" || (i == 0 && created.Primary == "") {
			r.fail("Dispatch created %s with key %q and spec %q", title, created.Key, created.Primary)
		}
		r.dispatchAs(versionNewsPerson, http.MethodPatch, "/api/v1/issues/"+created.Key, map[string]any{"status": "in_progress", "labels": []string{"legion"}}, nil)
		member.key, member.tree = created.Key, created.Key
		if i == 0 {
			tree.spec = created.Primary
		} else {
			member.tree = tree.root().key
		}
		tree.members = append(tree.members, member)
	}
	if verification {
		tree.check = "c-" + name
		var uploaded struct {
			Artifact struct {
				ID string `json:"id"`
			} `json:"artifact"`
			Version struct {
				Number int `json:"number"`
			} `json:"version"`
		}
		r.dispatchAs(versionNewsPerson, http.MethodPost, "/api/v1/issues/"+tree.root().key+"/artifacts",
			map[string]string{"name": "verification.md", "content": versionNewsVerification(tree.check, versionNewsCheck)}, &uploaded)
		if uploaded.Artifact.ID == "" || uploaded.Version.Number != 1 {
			r.fail("the Verification document of %s uploaded as %q at version %d, want a document at version 1", tree.root().key, uploaded.Artifact.ID, uploaded.Version.Number)
		}
		tree.verification = uploaded.Artifact.ID
	}
	r.trees[name] = tree
	keys := make([]string, len(tree.members))
	for i, member := range tree.members {
		keys[i] = fmt.Sprintf("%s %s", member.key, member.phase)
	}
	r.note("tree %s on Dispatch: %s; spec %s, Verification document %q", name, strings.Join(keys, ", "), tree.spec, tree.verification)
}

// versionNewsSpec is the root spec of tree, its `## Design` reading design, and its open decision
// block in a `## Decisions` section of its own when decision says so.
func versionNewsSpec(tree, design string, decision bool) string {
	spec := "# The " + tree + " tree\n\n## Summary\n\nWhat the " + tree + " tree of the approved version news proof builds.\n\n## Design\n\n" + design + "\n"
	if decision {
		spec += "\n## Decisions\n\n" + versionNewsAsk
	}
	return spec
}

// versionNewsVerification is a Verification document of one check, check, its words words.
func versionNewsVerification(check, words string) string {
	return "# Verification\n\n:::check{#" + check + ` surface="cli" run="go test ./internal/news/"}` + "\n" + words + "\n:::\n"
}

// seed records every member of every tree as the daemon would hold it mid-run: in its tree, in
// its phase, holding an admission slot, its tree's lifecycle open under the workflow, on an open
// pull request unless it stands in admitted, and each of its roles as a launch a previous daemon
// began and never recorded, serving run 1, which the boot relaunches. Every role's prompt is on
// disk, as the daemon's own launch writes it, so a launch the run makes later starts too. Where
// the schema holds the told record, each member in a phase with work takes the holder row the
// store's upgrade writes for it.
func (r *versionNewsLive) seed() {
	t := r.t
	t.Helper()
	ctx := context.Background()
	number, err := strconv.ParseInt(randomSuffix(t)[:5], 16, 64)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.st.Tx(ctx, func(tx pgx.Tx) error {
		for _, tree := range r.trees {
			for _, member := range tree.members {
				var listed struct {
					Rank    string `json:"rank"`
					LastSeq int64  `json:"last_seq"`
				}
				r.dispatchAgent(http.MethodGet, "/api/v1/issues/"+member.key, nil, http.StatusOK, &listed)
				issue := record.Issue{
					Key: member.key, Repo: r.repo, DefaultBranch: "main", GitHubRepo: r.repo, Tree: tree.root().key, Project: r.cfg.Project,
					Title: "Approved version news " + tree.name, Phase: member.phase, Generation: 1, Status: "in_progress",
					DispatchStatus: "in_progress", Rank: listed.Rank, LastDispatchSeq: listed.LastSeq,
				}
				if member.parent != "" {
					parent := member.parent
					issue.Parent = &parent
				}
				if err := r.records.PutIssue(ctx, tx, issue); err != nil {
					return err
				}
				if member.phase != phase.Admitted {
					number++
					member.pr = int(number)
					if err := r.records.PutPullRequest(ctx, tx, record.PullRequest{
						State: record.PullRequestOpen, Issue: member.key, Repo: r.repo, Number: member.pr, Branch: "legion/" + member.key,
						HeadSHA: versionNewsHead(member.key), Base: "main", Failing: []string{},
					}); err != nil {
						return err
					}
				}
				if err := r.admit(ctx, tx, member.key); err != nil {
					return err
				}
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("seed the issues: %v", err)
	}
	for _, tree := range r.trees {
		if _, err := r.st.OpenTreeLifecycle(ctx, r.project, tree.root().key, treelifecycle.AuthorityWorkflow); err != nil {
			t.Fatalf("open the lifecycle of %s: %v", tree.root().key, err)
		}
		for _, member := range tree.members {
			for _, role := range claim.Roles {
				writePrompt(t, r.cfg, r.token(member.key, role))
			}
			for _, role := range member.roles {
				if _, err := r.st.AdmitClaim(ctx, supervise.Claim{
					Token: r.token(member.key, role), Project: r.project, Tree: tree.root().key, Issue: member.key, Role: role, Generation: 1,
					State: supervise.StateLaunching, BootTokenHash: supervise.HashBootToken("interrupted-" + randomSuffix(t)), ServingGeneration: 1,
				}); err != nil {
					t.Fatalf("record the %s's unfinished launch on %s: %v", role, member.key, err)
				}
			}
		}
	}
	var told bool
	if err := r.st.Pool().QueryRow(ctx, "select to_regclass('design_news_told') is not null").Scan(&told); err != nil {
		t.Fatalf("read whether the schema holds the told record: %v", err)
	}
	if !told {
		r.note("the schema holds no design_news_told table, so no told record is seeded: this daemon keeps none")
		return
	}
	for _, tree := range r.trees {
		versions := map[string]int{}
		for _, step := range tree.steps() {
			versions[string(step)] = 1
		}
		encoded, err := json.Marshal(versions)
		if err != nil {
			t.Fatal(err)
		}
		for _, member := range tree.members {
			if member.phase == phase.Admitted {
				continue
			}
			if _, err := r.st.Pool().Exec(ctx, `insert into design_news_told (issue, audience, versions, opening_owed, told_at)
				values ($1, 'holder', $2::jsonb, false, now()) on conflict (issue, audience) do nothing`, member.key, string(encoded)); err != nil {
				t.Fatalf("seed the holder row of %s: %v", member.key, err)
			}
		}
		r.note("tree %s: each member in a phase with work took %s, as the store's upgrade records a running tree", tree.name, encoded)
	}
}

// versionNewsHead is the head of key's pull request.
func versionNewsHead(key string) string { return "head-" + strings.ToLower(key) }

// run runs one step between its `== name` and `ok name` lines, and prints the step's FAIL line when
// it stops on a failure that printed none of its own, so that every stopped step is named.
func (r *versionNewsLive) run(name string, body func()) {
	r.t.Helper()
	r.running, r.named = name, false
	defer func() {
		if r.t.Failed() && !r.named {
			fmt.Printf("FAIL %s: the step stopped on the failure the test log above names\n", name)
		}
	}()
	r.step(name, body)
}

// held prints one check of the step that held, which the script counts.
func (r *versionNewsLive) held(format string, args ...any) {
	fmt.Printf("   assert %s: %s\n", r.running, fmt.Sprintf(format, args...))
}

// fail prints the step's FAIL line, naming what did not hold, and ends the run.
func (r *versionNewsLive) fail(format string, args ...any) {
	r.t.Helper()
	message := fmt.Sprintf(format, args...)
	r.named = true
	fmt.Printf("FAIL %s: %s\n", r.running, message)
	r.t.Fatalf("%s: %s", r.running, message)
}

// await polls ready every 100 ms until it holds, and fails naming what after within, with what the
// last poll saw.
func (r *versionNewsLive) await(what string, within time.Duration, ready func() (bool, string)) {
	r.t.Helper()
	deadline := time.Now().Add(within)
	for {
		ok, seen := ready()
		if ok {
			return
		}
		if time.Now().After(deadline) {
			r.fail("waited %s for %s; last seen: %s", within, what, seen)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// token is the claim token of role on issue, as the daemon spells it.
func (r *versionNewsLive) token(issue string, role claim.Role) claim.Token {
	r.t.Helper()
	token, err := claim.NewToken(r.project, issue, role)
	if err != nil {
		r.t.Fatal(err)
	}
	return token
}

// claimant is role's claim on member, brought to ready by boot or connect.
func (r *versionNewsLive) claimant(member *versionNewsIssue, role claim.Role) *versionNewsClaimant {
	r.t.Helper()
	c, ok := r.claimants[r.token(member.key, role)]
	if !ok {
		r.fail("the %s of %s was never brought to ready", role, member.key)
	}
	return c
}

// launches is how many times the runtime has been asked to launch token's claim.
func (r *versionNewsLive) launches(token claim.Token, calls []fake.Call) int {
	count := 0
	for _, call := range calls {
		if (call.Method == "Spawn" || call.Method == "Resume") && call.Spec.Claim == token {
			count++
		}
	}
	return count
}

// connect waits for the daemon to launch role's claim on member once more than the calls before
// held, and brings it to ready as its pane does: the shim dials the worker stream with the
// launch's boot token, the session registers, the reader starts, and the session reports ready.
func (r *versionNewsLive) connect(member *versionNewsIssue, role claim.Role, before []fake.Call) *versionNewsClaimant {
	t := r.t
	t.Helper()
	token := r.token(member.key, role)
	already := r.launches(token, before)
	r.await("the launch of "+string(token), versionNewsWait, func() (bool, string) {
		calls := r.rt.Calls()
		return r.launches(token, calls) > already, fmt.Sprintf("%d launches of it, %d calls in all", r.launches(token, calls), len(calls))
	})
	launch := lastLaunch(t, r.rt, token)
	r.built.mu.Lock()
	address := r.built.address
	r.built.mu.Unlock()
	session := fmt.Sprintf("ses_%s_%s", strings.ToLower(member.key), role)
	c := &versionNewsClaimant{r: r, token: token, tree: member.tree, issue: member.key, role: role, session: session,
		sh: dialShim(t, address, launch.BootToken), arrived: make(chan struct{}, 1)}
	r.await("the shim of "+string(token)+" connected", versionNewsWait, func() (bool, string) {
		state := r.d.claim(token).State
		return state == string(supervise.StateShimConnected), "state " + state
	})
	status, body := r.d.request(http.MethodPost, "/legion/v1/claims/register", claim.RegisterRequest{
		BootToken: launch.BootToken, SessionID: session, OmpSessionFile: "/sessions/" + session + ".jsonl", AgentID: session, PluginContract: api.DaemonAPIVersion,
	}, false)
	if status != http.StatusOK {
		r.fail("register %s = %d %s, want 200", token, status, body)
	}
	var registered claim.RegisterResponse
	if err := json.Unmarshal(body, &registered); err != nil {
		r.fail("decode the registration of %s: %v", token, err)
	}
	c.secret = registered.Secret
	go c.serve()
	if status, body := r.d.request(http.MethodPost, "/legion/v1/claims/ready", claim.ReadyRequest{
		ClaimToken: token, SessionID: session, Secret: registered.Secret, Generation: launch.Generation,
	}, false); status != http.StatusNoContent {
		r.fail("ready %s = %d %s, want 204", token, status, body)
	}
	r.claimants[token] = c
	return c
}

// versionNewsClaimant is one role's claim as its pane reaches the daemon: the shim on the worker
// stream, the registered session, and a reader that answers what the daemon asks of the shim and
// keeps every prompt with the moment it arrived. A prompt that reaches it between turns is worked
// at once, its turn started and ended as Oh My Pi starts and ends one, so it never holds the next
// task back; the driver opens a turn of the agent's own (startTurn) where a step needs one running.
type versionNewsClaimant struct {
	r       *versionNewsLive
	token   claim.Token
	tree    string
	issue   string
	role    claim.Role
	session string
	secret  string
	sh      *shim
	// wmu orders the frames the reader and the step write; mu guards turn and prompts.
	wmu     sync.Mutex
	mu      sync.Mutex
	turn    bool
	prompts []versionNewsPrompt
	arrived chan struct{}
}

// versionNewsPrompt is one task the daemon sent a claim: the moment it arrived, the delivery it
// carries and its text.
type versionNewsPrompt struct {
	at       time.Time
	delivery string
	message  string
}

// serve reads every frame the daemon sends the shim until the connection closes: it answers the
// protocol negotiation and the state probe (streaming while the agent's own turn runs), and keeps
// each prompt, answering it and, between turns, working it as one turn.
func (c *versionNewsClaimant) serve() {
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
			c.mu.Lock()
			streaming := c.turn
			c.mu.Unlock()
			data, _ := json.Marshal(shimwire.StateData{IsStreaming: &streaming})
			c.send(shimwire.Response{ID: f.ID, Command: shimwire.TypeGetState, Success: true, Data: data})
		case shimwire.Prompt:
			c.send(shimwire.Response{ID: f.ID, Command: shimwire.TypePrompt, Success: true})
			c.mu.Lock()
			running := c.turn
			c.prompts = append(c.prompts, versionNewsPrompt{at: time.Now(), delivery: f.DeliveryID, message: f.Message})
			c.mu.Unlock()
			select {
			case c.arrived <- struct{}{}:
			default:
			}
			// A prompt into a running turn is kept and left unworked: the step watching that turn
			// fails on it, since the daemon sends nothing into a turn.
			if !running {
				c.send(shimwire.AgentStart{DeliveryID: f.DeliveryID})
				c.send(shimwire.AgentEnd{})
				c.send(shimwire.PromptResult{ID: f.ID, AgentInvoked: true})
			}
		}
	}
}

// send writes one frame to the daemon; the reader and the step both write. A frame that cannot be
// written is printed, not failed on: the reader answers frames until its connection closes, which
// the first daemon's stop does under it, and a frame a step needed shows in what the daemon then
// does, which the step waits for by name.
func (c *versionNewsClaimant) send(frame shimwire.Frame) {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	if err := c.sh.writer.WriteFrame(frame); err != nil {
		fmt.Printf("   %s could not send %s: %v\n", c.name(), frame.FrameType(), err)
	}
}

// name is the claim as a check names it.
func (c *versionNewsClaimant) name() string { return fmt.Sprintf("the %s of %s", c.role, c.issue) }

// startTurn begins a turn of the agent's own, as a session working on is in one, and returns once
// the daemon records the claim working.
func (c *versionNewsClaimant) startTurn() {
	c.r.t.Helper()
	c.mu.Lock()
	c.turn = true
	c.mu.Unlock()
	c.send(shimwire.AgentStart{})
	c.r.await(c.name()+" in a turn", versionNewsWait, func() (bool, string) {
		state := c.r.d.claim(c.token).State
		return state == string(supervise.StateWorking), "state " + state
	})
}

// endTurn ends the agent's own turn, and returns once the daemon records the claim out of it.
func (c *versionNewsClaimant) endTurn() {
	c.r.t.Helper()
	c.mu.Lock()
	c.turn = false
	c.mu.Unlock()
	c.send(shimwire.AgentEnd{})
	c.r.await(c.name()+" out of its turn", versionNewsWait, func() (bool, string) {
		state := c.r.d.claim(c.token).State
		return state != string(supervise.StateWorking), "state " + state
	})
}

// since is every prompt that arrived after mark, oldest first.
func (c *versionNewsClaimant) since(mark time.Time) []versionNewsPrompt {
	c.mu.Lock()
	defer c.mu.Unlock()
	var kept []versionNewsPrompt
	for _, prompt := range c.prompts {
		if prompt.at.After(mark) {
			kept = append(kept, prompt)
		}
	}
	return kept
}

// awaitPrompt waits for the first prompt after mark, and fails naming what it waited for.
func (c *versionNewsClaimant) awaitPrompt(what string, mark time.Time) versionNewsPrompt {
	c.r.t.Helper()
	deadline := time.Now().Add(versionNewsWait)
	for {
		if prompts := c.since(mark); len(prompts) > 0 {
			return prompts[0]
		}
		if time.Now().After(deadline) {
			c.mu.Lock()
			count := len(c.prompts)
			c.mu.Unlock()
			c.r.fail("waited %s for %s on %s's connection: it was sent %d prompts in all, none after %s",
				versionNewsWait, what, c.name(), count, mark.UTC().Format(time.RFC3339Nano))
		}
		select {
		case <-c.arrived:
		case <-time.After(100 * time.Millisecond):
		}
	}
}

// grant is a capability grant minted for this session now, as its `legion` tool mints one per
// operation.
func (c *versionNewsClaimant) grant() string {
	c.r.t.Helper()
	status, body := c.r.d.request(http.MethodPost, "/legion/v1/grants",
		api.GrantRequest{SessionID: c.session, Secret: c.secret, Tree: c.tree, Issue: c.issue}, false)
	if status != http.StatusOK {
		c.r.fail("the grant of %s = %d %s, want 200", c.token, status, body)
	}
	var granted api.GrantResponse
	if err := json.Unmarshal(body, &granted); err != nil {
		c.r.fail("decode the grant of %s: %v", c.token, err)
	}
	return granted.GrantID
}

// call posts body, given this session's grant, to the daemon's route path, and returns the status
// and the refusal, if any.
func (c *versionNewsClaimant) call(path string, body any) (int, api.Failure) {
	c.r.t.Helper()
	status, answer := c.r.d.request(http.MethodPost, path, body, false)
	var failure api.Failure
	if status != http.StatusOK {
		if err := json.Unmarshal(answer, &failure); err != nil {
			c.r.fail("decode the refusal of %s by %s: %s", path, c.name(), answer)
		}
	}
	return status, failure
}

// complete reports this role's completion of its phase, as the `legion` tool's handoff_complete
// does.
func (c *versionNewsClaimant) complete(req api.HandoffCompleteRequest) (int, api.Failure) {
	c.r.t.Helper()
	req.GrantID = c.grant()
	return c.call("/legion/v1/handoff/complete", req)
}

// backward asks for this role's issue to move back to to, as request_backward_move does.
func (c *versionNewsClaimant) backward(to phase.Phase, reason string) (int, api.Failure) {
	c.r.t.Helper()
	return c.call("/legion/v1/phase/backward", api.PhaseBackwardRequest{GrantID: c.grant(), To: to, Reason: reason})
}

// register is the architect's register_gate of step's document of its tree at version, failing
// the run unless the daemon takes it.
func (r *versionNewsLive) register(architect *versionNewsClaimant, step record.DesignStep, artifact string, version int) {
	r.t.Helper()
	status, failure := architect.call("/legion/v1/gates/register", api.GateRegisterRequest{
		GrantID: architect.grant(), Issue: architect.issue, ArtifactID: artifact, Version: version, Step: step,
	})
	if status != http.StatusOK {
		r.fail("the architect's register_gate of %s's %s document %s at version %d = %d %s: %s, want 200",
			architect.issue, step, artifact, version, status, failure.Code, failure.Error)
	}
	r.note("POST /legion/v1/gates/register: %s's %s document %s at version %d, taken", architect.issue, step, artifact, version)
}

// versionNewsApproval is the approval request Dispatch opened, as its route answers it: the ask
// and every document it names, each at the version it names.
type versionNewsApproval struct {
	ID       string `json:"id"`
	Approval struct {
		Documents []struct {
			ArtifactID string `json:"artifact_id"`
			Version    int    `json:"version"`
		} `json:"documents"`
	} `json:"approval"`
}

// requestApproval opens the architect's one approval request for the tree's spec, with its
// Verification document beside it when the tree has one (`dispatch request-approval
// --include-verification`'s route), and returns the ask Dispatch opened.
func (r *versionNewsLive) requestApproval(architect *versionNewsClaimant, tree *versionNewsTree) versionNewsApproval {
	r.t.Helper()
	body := map[string]any{"actor": map[string]string{"kind": "session", "id": architect.session}, "summary": "The design of the " + tree.name + " tree."}
	if tree.verification != "" {
		body["include_verification"] = true
	}
	var opened struct {
		Ask *versionNewsApproval `json:"ask"`
	}
	r.dispatchAgent(http.MethodPost, "/api/v1/artifacts/"+tree.spec+"/approval-requests", body, http.StatusCreated, &opened)
	if opened.Ask == nil {
		r.fail("the approval request of %s's spec opened no ask", tree.root().key)
	}
	return *opened.Ask
}

// approve is the person's review approving the document's latest version through Dispatch's own
// route, which answers the approval request open on it, when there is one, for every document the
// request names. It fails unless Dispatch reviewed version want.
func (r *versionNewsLive) approve(artifact string, want int) {
	r.t.Helper()
	var review struct {
		Version int    `json:"version"`
		State   string `json:"state"`
	}
	r.dispatchAs(versionNewsPerson, http.MethodPost, "/api/v1/artifacts/"+artifact+"/reviews", map[string]string{"state": "approved"}, &review)
	if review.Version != want || review.State != "approved" {
		r.fail("%s's review of %s = %s at version %d, want approved at version %d", versionNewsPerson, artifact, review.State, review.Version, want)
	}
	r.note("POST /api/v1/artifacts/%s/reviews as %s: approved version %d", artifact, versionNewsPerson, want)
}

// openTree is the setup each tree's step makes first, as its architect and its person make it
// before any of its work starts: the architect asks for one approval of the tree's documents and
// registers each (the Verification document first, then the spec), and the person approves; the
// gate opens at version 1 of every document.
func (r *versionNewsLive) openTree(tree *versionNewsTree) {
	r.t.Helper()
	architect := r.claimant(tree.root(), claim.RoleArchitect)
	r.requestApproval(architect, tree)
	if tree.verification != "" {
		r.register(architect, record.DesignStepCriteria, tree.verification, 1)
	}
	r.register(architect, record.DesignStepSpec, tree.spec, 1)
	r.approve(tree.spec, 1)
	r.awaitSteps(tree, "the gate of "+tree.root().key+" open at version 1", func(views map[record.DesignStep]api.GatedStepView) bool {
		for _, step := range tree.steps() {
			if view := views[step]; view.Status != api.GatedStepApproved || view.CurrentVersion != 1 {
				return false
			}
		}
		return len(views) == len(tree.steps())
	})
	r.held("the setup approval opened the gate of %s at version 1 of %s", tree.root().key, versionNewsStepNames(tree.steps()))
}

// versionNewsStepNames is steps as a check names them.
func versionNewsStepNames(steps []record.DesignStep) string {
	names := make([]string, len(steps))
	for i, step := range steps {
		names[i] = string(step)
	}
	return strings.Join(names, " and ")
}

// newVersion is the architect writing a new version of the step's document of its tree through
// Dispatch's edit route, replacing find with with, and fails unless Dispatch stored version want.
// Under a gated step it waits for the daemon to hold the step's row closed at that version.
func (r *versionNewsLive) newVersion(tree *versionNewsTree, step record.DesignStep, find, with string, want int) {
	r.t.Helper()
	architect := r.claimant(tree.root(), claim.RoleArchitect)
	artifact := tree.spec
	if step == record.DesignStepCriteria {
		artifact = tree.verification
	}
	var edited struct {
		Version *struct {
			Number int `json:"number"`
		} `json:"version"`
	}
	r.dispatchAgent(http.MethodPost, "/api/v1/artifacts/"+artifact+"/edits", map[string]any{
		"ops":   []map[string]string{{"op": "replace", "find": find, "with": with}},
		"actor": map[string]string{"kind": "session", "id": architect.session},
	}, http.StatusOK, &edited)
	if edited.Version == nil || edited.Version.Number != want {
		r.fail("the architect's edit of %s's %s document %s stored version %+v, want version %d", tree.root().key, step, artifact, edited.Version, want)
	}
	r.note("POST /api/v1/artifacts/%s/edits as %s: version %d of %s's %s document", artifact, architect.session, want, tree.root().key, step)
	if len(r.cfg.Gates.Design) == 0 {
		return
	}
	r.awaitSteps(tree, fmt.Sprintf("the %s row of %s closed at version %d", step, tree.root().key, want), func(views map[record.DesignStep]api.GatedStepView) bool {
		view := views[step]
		return view.CurrentVersion == want && view.Status != api.GatedStepApproved
	})
}

// awaitSteps waits for the root's gated steps, as GET /legion/v1/state shows them, to satisfy
// ready.
func (r *versionNewsLive) awaitSteps(tree *versionNewsTree, what string, ready func(map[record.DesignStep]api.GatedStepView) bool) map[record.DesignStep]api.GatedStepView {
	r.t.Helper()
	var views map[record.DesignStep]api.GatedStepView
	r.await(what, versionNewsWait, func() (bool, string) {
		views = r.gatedSteps(tree.root().key)
		return ready(views), fmt.Sprintf("gatedSteps %s", versionNewsSteps(views))
	})
	return views
}

// gatedSteps is key's gatedSteps in GET /legion/v1/state, by step.
func (r *versionNewsLive) gatedSteps(key string) map[record.DesignStep]api.GatedStepView {
	r.t.Helper()
	views := map[record.DesignStep]api.GatedStepView{}
	for _, view := range r.stateIssue(key).GatedSteps {
		views[view.Step] = view
	}
	return views
}

// versionNewsSteps is gated steps as a note says them.
func versionNewsSteps(views map[record.DesignStep]api.GatedStepView) string {
	var said []string
	for _, step := range record.DesignSteps {
		view, ok := views[step]
		if !ok {
			continue
		}
		approved := "none"
		if view.ApprovedVersion != nil {
			approved = strconv.Itoa(*view.ApprovedVersion)
		}
		said = append(said, fmt.Sprintf("%s %s at version %d, approved %s", step, view.Status, view.CurrentVersion, approved))
	}
	if len(said) == 0 {
		return "none"
	}
	return "[" + strings.Join(said, "; ") + "]"
}

// stateIssue is key as GET /legion/v1/state shows it now.
func (r *versionNewsLive) stateIssue(key string) api.Issue {
	r.t.Helper()
	issue, ok := r.d.state().Issues[key]
	if !ok {
		r.fail("GET /legion/v1/state records no issue %s", key)
	}
	return issue
}

// awaitPhase waits for key to stand in want.
func (r *versionNewsLive) awaitPhase(key string, want phase.Phase) {
	r.t.Helper()
	r.await(fmt.Sprintf("%s in %s", key, want), versionNewsWait, func() (bool, string) {
		got := r.stateIssue(key).Phase
		return got == want, "phase " + string(got)
	})
}

// holdPhase holds that key stays in want for liveSettle.
func (r *versionNewsLive) holdPhase(key string, want phase.Phase) {
	r.t.Helper()
	deadline := time.Now().Add(liveSettle)
	for time.Now().Before(deadline) {
		if got := r.stateIssue(key).Phase; got != want {
			r.fail("%s moved to %s, want it to stay in %s", key, got, want)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// awaitQueued waits for a task the daemon queued for c after mark that it has not sent, which is a
// task waiting behind the turn c is in, and returns it. A task it sent into that turn fails the
// run.
func (r *versionNewsLive) awaitQueued(c *versionNewsClaimant, mark time.Time) api.DeliveryView {
	r.t.Helper()
	var queued api.DeliveryView
	r.await("a task queued for "+c.name()+" behind its open turn", versionNewsWait, func() (bool, string) {
		pending := r.d.claim(c.token).Pending
		switch {
		case pending == nil:
			return false, "its claim holds no task"
		case !pending.QueuedAt.After(mark):
			return false, fmt.Sprintf("its claim holds task %s, queued %s, before the approval", pending.ID, pending.QueuedAt.UTC().Format(time.RFC3339Nano))
		case pending.DeliveredAt != nil:
			r.fail("the daemon sent task %s into %s's open turn", pending.ID, c.name())
		}
		queued = *pending
		return true, ""
	})
	return queued
}

// quiet holds that none of claimants was sent a prompt after mark until until, waiting until then:
// until is a moment a delivery the same run makes was seen after mark, and liveSettle past it, so
// the window these claimants were quiet in is one that sees a delivery.
func (r *versionNewsLive) quiet(claimants []*versionNewsClaimant, mark, until time.Time) {
	r.t.Helper()
	time.Sleep(time.Until(until))
	for _, c := range claimants {
		if prompts := c.since(mark); len(prompts) > 0 {
			r.fail("%s was sent %d prompts in the window, the first %q", c.name(), len(prompts), versionNewsFirstLine(prompts[0].message))
		}
	}
}

// versionNewsNames lists claimants as a check names them.
func versionNewsNames(claimants []*versionNewsClaimant) string {
	said := make([]string, len(claimants))
	for i, c := range claimants {
		said[i] = c.name()
	}
	return strings.Join(said, ", ")
}

// versionNewsFirstLine is the first line of a task, for a note that should not carry the whole of
// it.
func versionNewsFirstLine(task string) string {
	line, _, _ := strings.Cut(task, "\n")
	if len(line) > 160 {
		line = line[:160] + "…"
	}
	return line
}

// versionNewsNamesVersion says whether text names version n as a word of its own: "version 2",
// never the "2" of "version 21".
func versionNewsNamesVersion(text string, n int) bool {
	return regexp.MustCompile(`\bversion ` + strconv.Itoa(n) + `\b`).MatchString(text)
}

// Step A2. A tree registers its spec alone, and its gate opens on the setup approval of version 1.
// A new version that changes `## Design` closes it, and while the reviewer is in a turn the person
// approves it. The reviewer's task waits behind that turn, the daemon sending its connection
// nothing, and once the driver ends the turn the task arrives naming `Design` and the two versions,
// and not the `Summary` that did not change. The issue stays in reviewing until the reviewer's
// request_backward_move, which moves it to implementing. The in-run negative: a second tree whose
// gate the approval did not move was sent nothing, in the window the reviewer's task arrived in.
//
// The first five checks hold on a daemon from before the change as well; the sixth, the task
// queued behind the turn, is the run's first one-approval check, where the negative control fails.
func (r *versionNewsLive) a2() {
	tree, unmoved := r.trees["a2"], r.trees["a2-unmoved"]
	reviewer := r.claimant(tree.root(), claim.RoleReviewer)
	others := []*versionNewsClaimant{r.claimant(unmoved.root(), claim.RoleReviewer), r.claimant(unmoved.root(), claim.RoleArchitect)}
	r.openTree(tree)
	r.openTree(unmoved)
	r.newVersion(tree, record.DesignStepSpec, versionNewsDesign, versionNewsDesignAgain, 2)
	r.held("version 2 of %s's spec, changing its Design section, closed the gate", tree.root().key)
	reviewer.startTurn()
	r.held("%s is in a turn", reviewer.name())
	mark := time.Now()
	r.approve(tree.spec, 2)
	r.awaitSteps(tree, "the gate of "+tree.root().key+" open at version 2", func(views map[record.DesignStep]api.GatedStepView) bool {
		view := views[record.DesignStepSpec]
		return view.Status == api.GatedStepApproved && view.CurrentVersion == 2
	})
	r.held("the person's approval of version 2 opened the gate again")

	queued := r.awaitQueued(reviewer, mark)
	r.held("a task for %s is queued behind its open turn (delivery %s)", reviewer.name(), queued.ID)
	time.Sleep(liveSettle)
	if prompts := reviewer.since(mark); len(prompts) > 0 {
		r.fail("%s was sent %q while its turn ran", reviewer.name(), versionNewsFirstLine(prompts[0].message))
	}
	r.held("%s's connection was sent nothing while its turn ran (%s)", reviewer.name(), liveSettle)
	ended := time.Now()
	reviewer.endTurn()
	task := reviewer.awaitPrompt("the news task", ended)
	r.note("%s was prompted, once its turn ended:\n%s", reviewer.name(), task.message)
	if task.delivery != queued.ID {
		r.fail("%s's first task after its turn is delivery %q, want %s, the one queued behind the turn", reviewer.name(), task.delivery, queued.ID)
	}
	if !strings.Contains(task.message, `"Design"`) || strings.Contains(task.message, `"Summary"`) ||
		!versionNewsNamesVersion(task.message, 2) || !versionNewsNamesVersion(task.message, 1) {
		r.fail("%s's task names %s, want the changed section \"Design\", not the unchanged \"Summary\", and versions 2 and 1: %q",
			reviewer.name(), versionNewsFirstLine(task.message), task.message)
	}
	r.held("the task queued behind the turn (delivery %s) reached %s only after its turn ended, naming the section \"Design\" and versions 1 and 2, and not \"Summary\"",
		queued.ID, reviewer.name())
	r.holdPhase(tree.root().key, phase.Reviewing)
	r.held("%s stays in reviewing after the task (%s)", tree.root().key, liveSettle)
	status, failure := reviewer.backward(phase.Implementing, "version 2 of the spec changed the Design section this review read")
	if status != http.StatusOK {
		r.fail("%s's request_backward_move to implementing = %d %s: %s, want 200", reviewer.name(), status, failure.Code, failure.Error)
	}
	r.awaitPhase(tree.root().key, phase.Implementing)
	r.held("%s's request_backward_move moved %s to implementing", reviewer.name(), tree.root().key)
	r.quiet(others, mark, task.at.Add(liveSettle))
	r.held("%s, whose gate did not move, were sent nothing in the window the reviewer's task arrived in", versionNewsNames(others))
}

// Step A1. One approval request names both documents of a tree, the architect registers the
// Verification document as criteria and then the spec, and one approval of both opens both rows,
// which releases the root into planning; a new version of the Verification document then closes
// the criteria row, the spec row standing approved.
func (r *versionNewsLive) a1() {
	tree := r.trees["a1"]
	key := tree.root().key
	architect := r.claimant(tree.root(), claim.RoleArchitect)
	ask := r.requestApproval(architect, tree)
	named := map[string]int{}
	for _, document := range ask.Approval.Documents {
		named[document.ArtifactID] = document.Version
	}
	if len(named) != 2 || named[tree.spec] != 1 || named[tree.verification] != 1 {
		r.fail("the approval request (ask %s) names %v, want the spec %s and the Verification document %s, each at version 1", ask.ID, named, tree.spec, tree.verification)
	}
	r.held("one approval request (ask %s) names the spec and the Verification document, each at version 1", ask.ID)
	r.register(architect, record.DesignStepCriteria, tree.verification, 1)
	r.register(architect, record.DesignStepSpec, tree.spec, 1)
	r.awaitSteps(tree, "both rows of "+key+" registered and waiting", func(views map[record.DesignStep]api.GatedStepView) bool {
		return len(views) == 2 && views[record.DesignStepCriteria].Status == api.GatedStepWaiting && views[record.DesignStepSpec].Status == api.GatedStepWaiting
	})
	r.held("register_gate took criteria and then spec under root-issues, and both rows wait on the person")
	r.approve(tree.spec, 1)
	views := r.awaitSteps(tree, "both rows of "+key+" approved", func(views map[record.DesignStep]api.GatedStepView) bool {
		return views[record.DesignStepCriteria].Status == api.GatedStepApproved && views[record.DesignStepSpec].Status == api.GatedStepApproved
	})
	r.held("one approval opened both rows: %s", versionNewsSteps(views))
	r.awaitPhase(key, phase.Planning)
	r.held("the open gate released %s into planning", key)
	r.newVersion(tree, record.DesignStepCriteria, versionNewsCheck, versionNewsCheckAgain, 2)
	views = r.gatedSteps(key)
	criteria, spec := views[record.DesignStepCriteria], views[record.DesignStepSpec]
	if criteria.ApprovedVersion == nil || *criteria.ApprovedVersion != 1 || spec.Status != api.GatedStepApproved {
		r.fail("after version 2 of the Verification document the rows are %s, want criteria waiting at version 2 with version 1 approved, and spec approved", versionNewsSteps(views))
	}
	r.held("version 2 of the Verification document closed the criteria row, the spec row standing approved: %s", versionNewsSteps(views))
}

// Step A3. Two trees register both documents. An answer to the open decision block in the
// implementing root's spec, the whole of its next version, which Dispatch approves by carrying the
// person's approval to it, sends the implementer and the tree's architect nothing; in the same
// window a checks-only version of the accepting root's Verification document, approved, reaches
// the root architect, which holds that root, and the root stays in accepting. A checks-only version
// of the implementing root's Verification document then reaches its implementer.
func (r *versionNewsLive) a3() {
	accepting, implementing := r.trees["a3-accepting"], r.trees["a3-implementing"]
	owner := r.claimant(accepting.root(), claim.RoleArchitect)
	implementer := r.claimant(implementing.root(), claim.RoleImplementer)
	silent := []*versionNewsClaimant{implementer, r.claimant(implementing.root(), claim.RoleArchitect)}
	r.openTree(accepting)
	r.openTree(implementing)

	key := implementing.root().key
	var asks []struct {
		ID      string  `json:"id"`
		BlockID *string `json:"block_id"`
	}
	r.dispatchAs(versionNewsPerson, http.MethodGet, "/api/v1/issues/"+key+"/asks?state=all", nil, &asks)
	wave := ""
	for _, ask := range asks {
		if ask.BlockID != nil && *ask.BlockID == "wave" {
			wave = ask.ID
		}
	}
	if wave == "" {
		r.fail("Dispatch lists no ask of the decision block wave on %s: %+v", key, asks)
	}
	mark := time.Now()
	r.dispatchAs(versionNewsPerson, http.MethodPost, "/api/v1/asks/"+wave+"/answer", map[string]any{"selected": []string{"Two"}, "expected_edited_at": nil}, nil)
	r.note("POST /api/v1/asks/%s/answer as %s: Two", wave, versionNewsPerson)
	r.awaitSteps(implementing, "the answer's version of "+key+"'s spec approved in the daemon's gate", func(views map[record.DesignStep]api.GatedStepView) bool {
		view := views[record.DesignStepSpec]
		return view.CurrentVersion == 2 && view.Status == api.GatedStepApproved
	})
	r.held("the answer to the decision block wrote version 2 of %s's spec, which Dispatch approved by carrying the person's approval, and the gate stands open at it", key)

	r.newVersion(accepting, record.DesignStepCriteria, versionNewsCheck, versionNewsCheckAgain, 2)
	r.approve(accepting.verification, 2)
	task := owner.awaitPrompt("the news of the accepting root's checks", mark)
	r.note("%s was prompted:\n%s", owner.name(), task.message)
	if !strings.Contains(task.message, accepting.check) || !strings.Contains(task.message, accepting.root().key) ||
		!strings.Contains(task.message, "accepting") || !versionNewsNamesVersion(task.message, 2) || !versionNewsNamesVersion(task.message, 1) {
		r.fail("%s's task names %s, want the changed check %s of %s, which it holds in accepting, and versions 2 and 1: %q",
			owner.name(), versionNewsFirstLine(task.message), accepting.check, accepting.root().key, task.message)
	}
	r.held("a checks-only approval reached the root architect, which holds %s in accepting, naming the changed check %s and versions 1 and 2", accepting.root().key, accepting.check)
	r.holdPhase(accepting.root().key, phase.Accepting)
	r.held("%s stays in accepting (%s)", accepting.root().key, liveSettle)
	r.quiet(silent, mark, task.at.Add(liveSettle))
	r.held("the decision-only version sent %s nothing, in the window the accepting root's task arrived in", versionNewsNames(silent))

	mark = time.Now()
	r.newVersion(implementing, record.DesignStepCriteria, versionNewsCheck, versionNewsCheckAgain, 2)
	r.approve(implementing.verification, 2)
	task = implementer.awaitPrompt("the news of the implementing root's checks", mark)
	r.note("%s was prompted:\n%s", implementer.name(), task.message)
	if !strings.Contains(task.message, implementing.check) || strings.Contains(task.message, `"Design"`) ||
		!versionNewsNamesVersion(task.message, 2) || !versionNewsNamesVersion(task.message, 1) {
		r.fail("%s's task names %s, want the changed check %s and versions 2 and 1, and no spec section: %q",
			implementer.name(), versionNewsFirstLine(task.message), implementing.check, task.message)
	}
	r.held("a checks-only approval reached %s, naming the changed check %s and versions 1 and 2", implementer.name(), implementing.check)
}

// Step A5. The tester is in a turn when a checks-only version of its root's Verification document
// is approved; the task that carries the news waits behind that turn. In that same turn the tester
// completes with a pass, and its turn ends. The tester is sent nothing, since its phase is over,
// and the reviewer the pass starts is prompted with a start task naming the changed check and the
// two versions. The same version changes nothing the root's child holds, since a root's checks are
// the root's alone, so the reviewer the child's tester's pass starts is prompted with a start task
// that names no check and no change at all: the same read that found the root reviewer's change.
func (r *versionNewsLive) a5() {
	tree := r.trees["a5"]
	key := tree.root().key
	tester := r.claimant(tree.root(), claim.RoleTester)
	r.openTree(tree)
	tester.startTurn()
	r.held("%s is in a turn", tester.name())
	mark := time.Now()
	r.newVersion(tree, record.DesignStepCriteria, versionNewsCheck, versionNewsCheckAgain, 2)
	r.approve(tree.verification, 2)
	queued := r.awaitQueued(tester, mark)
	r.held("the news of version 2 is queued behind %s's open turn (delivery %s)", tester.name(), queued.ID)
	id, version := r.writeHandoff(key, "test", tester.session)
	before := r.rt.Calls()
	status, failure := tester.complete(api.HandoffCompleteRequest{Summary: "every check passes", Verdict: "pass", HandoffID: id, HandoffVersion: version})
	if status != http.StatusOK {
		r.fail("%s's completion with a pass = %d %s: %s, want 200", tester.name(), status, failure.Code, failure.Error)
	}
	tester.endTurn()
	r.awaitPhase(key, phase.Reviewing)
	r.held("%s completed with a pass in that turn, and %s moved to reviewing", tester.name(), key)
	reviewer := r.connect(tree.root(), claim.RoleReviewer, before)
	task := reviewer.awaitPrompt("the reviewer's start task", mark)
	r.note("%s was prompted:\n%s", reviewer.name(), task.message)
	if !strings.Contains(task.message, tree.check) || !versionNewsNamesVersion(task.message, 2) || !versionNewsNamesVersion(task.message, 1) {
		r.fail("%s's start task names %s, want the changed check %s and versions 2 and 1: %q", reviewer.name(), versionNewsFirstLine(task.message), tree.check, task.message)
	}
	r.held("%s's start task names the changed check %s and versions 1 and 2", reviewer.name(), tree.check)
	r.quiet([]*versionNewsClaimant{tester}, mark, task.at.Add(liveSettle))
	r.held("%s, whose phase ended before its task could go, was sent nothing in the window the reviewer's task arrived in", tester.name())

	child := tree.members[1]
	childTester := r.claimant(child, claim.RoleTester)
	id, version = r.writeHandoff(child.key, "test", childTester.session)
	before = r.rt.Calls()
	if status, failure := childTester.complete(api.HandoffCompleteRequest{Summary: "every check passes", Verdict: "pass", HandoffID: id, HandoffVersion: version}); status != http.StatusOK {
		r.fail("%s's completion with a pass = %d %s: %s, want 200", childTester.name(), status, failure.Code, failure.Error)
	}
	r.awaitPhase(child.key, phase.Reviewing)
	r.held("%s completed with a pass after the approval, and %s moved to reviewing", childTester.name(), child.key)
	childReviewer := r.connect(child, claim.RoleReviewer, before)
	start := childReviewer.awaitPrompt("the child reviewer's start task", mark)
	r.note("%s was prompted:\n%s", childReviewer.name(), start.message)
	// A change list names the check, or a "Checks:" line, under the news's own opening words.
	names := func(message string) bool {
		return strings.Contains(message, tree.check) || strings.Contains(message, "Checks:") || strings.Contains(message, "The approved design of")
	}
	if names(start.message) {
		r.fail("%s's start task names a change; want none, since version 2 changed nothing %s holds: %q", childReviewer.name(), child.key, start.message)
	}
	r.held("%s's start task names no check and no change, since version 2 changed nothing %s holds, where the same read finds %s in %s's",
		childReviewer.name(), child.key, tree.check, reviewer.name())
}

// writeHandoff records the handoff of key's phase word under session on Dispatch's own route, as
// `dispatch handoff-write` does, and returns the record's id and version.
func (r *versionNewsLive) writeHandoff(key, word, session string) (string, int) {
	r.t.Helper()
	r.dispatchAgent(http.MethodPost, "/api/v1/issues/"+key+"/handoffs/"+word, map[string]any{
		"actor": map[string]string{"kind": "session", "id": session},
		"body":  fmt.Sprintf("{\"schemaVersion\": 1, \"phase\": %q, \"issue\": %q, \"summary\": \"every check passes\"}\n", word, key),
	}, http.StatusCreated, nil)
	var latest struct {
		ID      string `json:"id"`
		Version int    `json:"version"`
	}
	r.dispatchAgent(http.MethodGet, "/api/v1/issues/"+key+"/handoffs/"+word+"/latest", nil, http.StatusOK, &latest)
	if latest.ID == "" || latest.Version < 1 {
		r.fail("the %s handoff of %s reads back as %+v", word, key, latest)
	}
	r.note("POST /api/v1/issues/%s/handoffs/%s as session %s: version %d (id %s)", key, word, session, latest.Version, latest.ID)
	return latest.ID, latest.Version
}

// Step A6. A root in merging whose pull request GitHub reports merged, which waits there for its
// child still in reviewing, is sent nothing for a spec change the person approves, and its
// merger's request_backward_move is refused naming the merge; the unmerged child's reviewer is sent
// the task, which is the delivery the root's window sees.
func (r *versionNewsLive) a6() {
	tree := r.trees["a6"]
	root, child := tree.root(), tree.members[1]
	merger := r.claimant(root, claim.RoleMerger)
	reviewer := r.claimant(child, claim.RoleReviewer)
	r.openTree(tree)
	r.publishMerged(root)
	r.await("the merge of "+root.key+"'s pull request recorded", versionNewsWait, func() (bool, string) {
		issue := r.stateIssue(root.key)
		if issue.PullRequest == nil {
			return false, "no pull request in the state"
		}
		return issue.PullRequest.State == string(record.PullRequestMerged), "pull request " + issue.PullRequest.State + " in phase " + string(issue.Phase)
	})
	r.holdPhase(root.key, phase.Merging)
	r.held("GitHub's merge of #%d is recorded and %s waits in merging for %s", root.pr, root.key, child.key)
	mark := time.Now()
	r.newVersion(tree, record.DesignStepSpec, versionNewsDesign, versionNewsDesignAgain, 2)
	r.approve(tree.spec, 2)
	task := reviewer.awaitPrompt("the news of the spec", mark)
	r.note("%s was prompted:\n%s", reviewer.name(), task.message)
	if !strings.Contains(task.message, `"Design"`) || !versionNewsNamesVersion(task.message, 2) || !versionNewsNamesVersion(task.message, 1) {
		r.fail("%s's task names %s, want the section \"Design\" and versions 2 and 1: %q", reviewer.name(), versionNewsFirstLine(task.message), task.message)
	}
	r.held("%s, unmerged in reviewing, was sent the task naming \"Design\" and versions 1 and 2", reviewer.name())
	r.quiet([]*versionNewsClaimant{merger}, mark, task.at.Add(liveSettle))
	r.held("%s, on a merged pull request, was sent nothing in the window %s's task arrived in", merger.name(), child.key)
	status, failure := merger.backward(phase.Implementing, "version 2 of the spec changed the Design section")
	if status == http.StatusOK || failure.Code != "BACKWARD_REFUSED" || !strings.Contains(failure.Error, "merged") {
		r.fail("%s's request_backward_move to implementing = %d %s: %s, want BACKWARD_REFUSED naming the merge", merger.name(), status, failure.Code, failure.Error)
	}
	r.note("POST /legion/v1/phase/backward as %s: %d %s: %s", merger.name(), status, failure.Code, failure.Error)
	r.holdPhase(root.key, phase.Merging)
	r.held("%s's request_backward_move was refused %s naming the merge, and %s stays in merging", merger.name(), failure.Code, root.key)
}

// publishMerged publishes GitHub's close of member's pull request as merged, as Envoy relays the
// webhook (intake's decodePullRequest reads it), and waits for the daemon to take it.
func (r *versionNewsLive) publishMerged(member *versionNewsIssue) {
	r.t.Helper()
	payload, err := json.Marshal(map[string]string{
		"action": "closed", "kind": "pr", "repo": r.repo, "number": strconv.Itoa(member.pr), "merged": "true",
		"merge_commit_sha": "merge-" + strings.ToLower(member.key), "head_sha": versionNewsHead(member.key), "base_ref": "main",
		"head_ref": "legion/" + member.key, "title": "Approved version news " + member.key,
		"url": "https://github.com/" + r.repo + "/pull/" + strconv.Itoa(member.pr), "updated_at": time.Now().UTC().Format(time.RFC3339),
	})
	if err != nil {
		r.t.Fatal(err)
	}
	subject := fmt.Sprintf("notifications.github.%s.pr.%d", strings.ReplaceAll(r.repo, "/", "."), member.pr)
	eventID := r.publish(subject, "github", fmt.Sprintf("pr-%d-merged", member.pr), string(payload))
	r.await("the daemon's processed_events row of GitHub event "+eventID, versionNewsWait, func() (bool, string) {
		return r.processed("github", eventID), "not processed"
	})
	r.note("published %s: pull request #%d of %s closed merged, taken as %s", subject, member.pr, member.key, eventID)
}

// processed says whether the daemon applied source's event eventID, which the intake records in
// the transaction that applies it. No route shows it.
func (r *versionNewsLive) processed(source, eventID string) bool {
	r.t.Helper()
	var taken bool
	if err := r.st.Pool().QueryRow(context.Background(),
		"select exists(select 1 from processed_events where source = $1 and event_id = $2)", source, eventID).Scan(&taken); err != nil {
		r.fail("read the processed event %s: %v", eventID, err)
	}
	return taken
}

// Step A7. A checks-only version closes one tree's gate while its retro runs, and the implementer
// of that tree and of a second, whose gate stands open, each complete retro at once. The open
// tree's merger is launched, and in that same window the closed tree's merger is not. The person's
// approval then releases it: the merger is launched and its start task leads with the gate's
// opening and names what changed.
func (r *versionNewsLive) a7() {
	closed, open := r.trees["a7"], r.trees["a7-open"]
	implementers := []*versionNewsClaimant{r.claimant(closed.root(), claim.RoleImplementer), r.claimant(open.root(), claim.RoleImplementer)}
	r.openTree(closed)
	r.openTree(open)
	r.newVersion(closed, record.DesignStepCriteria, versionNewsCheck, versionNewsCheckAgain, 2)
	r.held("version 2 of %s's Verification document closed its gate", closed.root().key)
	held, released := r.token(closed.root().key, claim.RoleMerger), r.token(open.root().key, claim.RoleMerger)
	before := r.rt.Calls()
	mark := time.Now()
	for _, implementer := range implementers {
		if status, failure := implementer.complete(api.HandoffCompleteRequest{Summary: "the retro is written", Commit: versionNewsHead(implementer.issue)}); status != http.StatusOK {
			r.fail("%s's completion of retro = %d %s: %s, want 200", implementer.name(), status, failure.Code, failure.Error)
		}
	}
	r.awaitPhase(closed.root().key, phase.Merging)
	r.awaitPhase(open.root().key, phase.Merging)
	r.held("both retros completed, and %s and %s are in merging", closed.root().key, open.root().key)
	var launchedAt time.Time
	r.await("the launch of "+string(released)+", whose gate is open", versionNewsWait, func() (bool, string) {
		count := r.launches(released, r.rt.Calls())
		launchedAt = time.Now()
		return count > r.launches(released, before), fmt.Sprintf("%d launches", count)
	})
	r.held("the merger of %s, whose gate is open, was launched", open.root().key)
	time.Sleep(time.Until(launchedAt.Add(liveSettle)))
	if count := r.launches(held, r.rt.Calls()); count != r.launches(held, before) {
		r.fail("the merger of %s was launched while its gate was closed", closed.root().key)
	}
	r.held("in the window %s's merger was launched in, the merger of %s, its gate closed, was not", open.root().key, closed.root().key)
	r.approve(closed.verification, 2)
	merger := r.connect(closed.root(), claim.RoleMerger, before)
	task := merger.awaitPrompt("the merger's start task", mark)
	r.note("%s was prompted:\n%s", merger.name(), task.message)
	opening := "The design gate of " + closed.root().key + " opened"
	if !strings.Contains(task.message, opening) || !strings.Contains(task.message, closed.check) || !versionNewsNamesVersion(task.message, 2) || !versionNewsNamesVersion(task.message, 1) {
		r.fail("%s's start task names %s, want %q and the changed check %s with versions 2 and 1: %q", merger.name(), versionNewsFirstLine(task.message), opening, closed.check, task.message)
	}
	r.held("the approval launched %s, whose start task reads %q and names the changed check %s and versions 1 and 2", merger.name(), opening, closed.check)
}

// Step R6. In a tree with children, an approved checks-only version of the root's Verification
// document reaches the worker holding the root's phase, and neither child's worker is sent a task,
// so none names the changed check, in the window the root's task arrived in.
func (r *versionNewsLive) r6() {
	tree := r.trees["r6"]
	holder := r.claimant(tree.root(), claim.RoleImplementer)
	children := []*versionNewsClaimant{r.claimant(tree.members[1], claim.RoleImplementer), r.claimant(tree.members[2], claim.RoleTester)}
	r.openTree(tree)
	mark := time.Now()
	r.newVersion(tree, record.DesignStepCriteria, versionNewsCheck, versionNewsCheckAgain, 2)
	r.approve(tree.verification, 2)
	task := holder.awaitPrompt("the news of the root's checks", mark)
	r.note("%s was prompted:\n%s", holder.name(), task.message)
	if !strings.Contains(task.message, tree.check) || !versionNewsNamesVersion(task.message, 2) || !versionNewsNamesVersion(task.message, 1) {
		r.fail("%s's task names %s, want the changed check %s and versions 2 and 1: %q", holder.name(), versionNewsFirstLine(task.message), tree.check, task.message)
	}
	r.held("the root's checks-only approval reached %s, naming the changed check %s and versions 1 and 2", holder.name(), tree.check)
	r.quiet(children, mark, task.at.Add(liveSettle))
	r.held("%s were sent no task, so none naming %s, in the window the root's task arrived in", versionNewsNames(children), tree.check)
}

// Step off. A second daemon, on a configuration and a database of its own, gates nothing, so the
// architect's registration opens each gate at once. A new version of one tree's spec, which the
// person approves and the daemon takes in, sends no holder of that tree anything, in the window in
// which the other tree's tester passes and the reviewer that pass starts is sent its task.
func (r *versionNewsLive) off() {
	tree, control := r.trees["off"], r.trees["off-control"]
	silent := []*versionNewsClaimant{
		r.claimant(tree.root(), claim.RoleArchitect), r.claimant(tree.root(), claim.RoleImplementer), r.claimant(tree.members[1], claim.RoleReviewer),
	}
	tester := r.claimant(control.root(), claim.RoleTester)
	for _, each := range []*versionNewsTree{tree, control} {
		r.register(r.claimant(each.root(), claim.RoleArchitect), record.DesignStepSpec, each.spec, 1)
		key := each.root().key
		r.await("the gate of "+key+" approved at its registration", versionNewsWait, func() (bool, string) {
			gate := r.stateIssue(key).DesignGate
			if gate == nil {
				return false, "no designGate"
			}
			return gate.ApprovedVersion != nil && *gate.ApprovedVersion == 1, fmt.Sprintf("designGate %+v", *gate)
		})
		r.held("under gates.design off the architect's registration opened the gate of %s at version 1, with no person asked", key)
	}
	key := tree.root().key
	var listed struct {
		LastSeq int64 `json:"last_seq"`
	}
	r.dispatchAgent(http.MethodGet, "/api/v1/issues/"+key, nil, http.StatusOK, &listed)
	mark := time.Now()
	r.newVersion(tree, record.DesignStepSpec, versionNewsDesign, versionNewsDesignAgain, 2)
	r.approve(tree.spec, 2)
	eventID := r.approvalEvent(key, listed.LastSeq, tree.spec, 2)
	r.await("the daemon's processed_events row of Dispatch event "+eventID, versionNewsWait, func() (bool, string) {
		return r.processed("dispatch", eventID), "not processed"
	})
	r.held("the daemon took in the person's approval of version 2 of %s's spec (%s)", key, eventID)
	id, version := r.writeHandoff(control.root().key, "test", tester.session)
	before := r.rt.Calls()
	if status, failure := tester.complete(api.HandoffCompleteRequest{Summary: "every check passes", Verdict: "pass", HandoffID: id, HandoffVersion: version}); status != http.StatusOK {
		r.fail("%s's completion with a pass = %d %s: %s, want 200", tester.name(), status, failure.Code, failure.Error)
	}
	reviewer := r.connect(control.root(), claim.RoleReviewer, before)
	task := reviewer.awaitPrompt("the control reviewer's start task", mark)
	r.held("in the same window %s's pass started %s, which was sent its task", tester.name(), reviewer.name())
	r.quiet(silent, mark, task.at.Add(liveSettle))
	r.held("under gates.design off the approved version sent %s nothing, in the window %s's task arrived in", versionNewsNames(silent), reviewer.name())
}

// approvalEvent is the id of the artifact.approved event of version of artifact in key's Dispatch
// event log after sequence after, as the daemon's processed_events names a Dispatch event
// (dispatch-<id>).
func (r *versionNewsLive) approvalEvent(key string, after int64, artifact string, version int) string {
	r.t.Helper()
	var found string
	r.await(fmt.Sprintf("the artifact.approved event of version %d of %s on %s", version, artifact, key), versionNewsWait, func() (bool, string) {
		var events []struct {
			ID      int64  `json:"id"`
			Type    string `json:"type"`
			Payload struct {
				ArtifactID string `json:"artifact_id"`
				Version    any    `json:"version"`
			} `json:"payload"`
		}
		r.dispatchAgent(http.MethodGet, fmt.Sprintf("/api/v1/issues/%s/events?after=%d&limit=200", key, after), nil, http.StatusOK, &events)
		types := make([]string, len(events))
		for i, event := range events {
			types[i] = event.Type
			if number, ok := event.Payload.Version.(float64); ok && event.Type == "artifact.approved" && event.Payload.ArtifactID == artifact && int(number) == version {
				found = fmt.Sprintf("dispatch-%d", event.ID)
				return true, ""
			}
		}
		return false, fmt.Sprintf("events %v", types)
	})
	return found
}

// dispatchAs sends one request to Dispatch as the person login, through the trusted identity
// header the scratch server takes, requires a 2xx answer and decodes it into into when it is not
// nil.
func (r *versionNewsLive) dispatchAs(login, method, path string, body, into any) {
	r.t.Helper()
	r.dispatchCall(method, path, body, 0, into, func(request *http.Request) { request.Header.Set("X-Dispatch-User", login) })
}

// dispatchAgent sends one request to Dispatch with the agent bearer, as a session or the daemon
// reaches it, requires want and decodes the answer into into when it is not nil.
func (r *versionNewsLive) dispatchAgent(method, path string, body any, want int, into any) {
	r.t.Helper()
	r.dispatchCall(method, path, body, want, into, func(request *http.Request) { request.Header.Set("Authorization", "Bearer "+r.dispatch.token) })
}

// dispatchCall is one request to Dispatch: want 0 takes any 2xx answer.
func (r *versionNewsLive) dispatchCall(method, path string, body any, want int, into any, identify func(*http.Request)) {
	r.t.Helper()
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			r.t.Fatal(err)
		}
		reader = bytes.NewReader(encoded)
	}
	request, err := http.NewRequest(method, r.dispatch.base+path, reader)
	if err != nil {
		r.t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/json")
	identify(request)
	response, err := r.client.Do(request)
	if err != nil {
		r.fail("Dispatch %s %s: %v", method, path, err)
	}
	defer response.Body.Close()
	answer, err := io.ReadAll(response.Body)
	if err != nil {
		r.fail("read Dispatch's answer to %s %s: %v", method, path, err)
	}
	if (want != 0 && response.StatusCode != want) || (want == 0 && response.StatusCode/100 != 2) {
		r.fail("Dispatch %s %s = %d %s", method, path, response.StatusCode, answer)
	}
	if into != nil {
		if err := json.Unmarshal(answer, into); err != nil {
			r.fail("decode Dispatch's answer to %s %s (%s): %v", method, path, answer, err)
		}
	}
}
