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
// The issues, their pull requests and their trees' slots and lifecycles are seeded through the
// store, as every daemon test seeds its fixture, beside the Dispatch issues the steps write to.
// They are what the daemon's start path reads, and it writes none of them. The claims, their
// phases rows and their entries are the start path's own, so every holder a step tells stands
// where production's holders stand (seed). Each root is recorded at admitted and its architect is
// started as admission's promotion starts it (workflow's Engine.StartArchitect, which raises the
// architect's entry and queues its start). The root is then moved to the phase its step needs, and
// each member's phase worker is started as the promotion of a mid-phase member starts it
// (Engine.StartPromotedWorker, which raises that role's entry). No claim is written by hand: the
// booted daemon's outbox runs each start, creates the claim and queues its task at the entry the
// start carries. The boot checks the result in SETUP (standAtTheirEntries): a claim that does not
// stand at an entry of 1 or more of its role's phases row, given that entry, fails the run, and
// the FAIL line names the claim and both numbers.
//
// One state the start path cannot reach here is the holder row of the told record (the
// design_news_told table), where the schema holds it. The start's raise writes that row from its
// tree's gate as the gate stands then (Engine.enter), and no gate row exists when the starts are
// seeded: each gate is registered and opened in its own step (openTree), after the boot. So the
// driver writes each such row by hand, at version 1 of each document its tree registers, and
// checks it in SETUP against the row the start path writes for the same holder while its tree's
// gate stands approved at version 1 (seedHolderRow). A difference fails the run, and the FAIL line
// names the issue, the field and both values. A schema without the table, which is a release from
// before that record, is seeded and checked nothing there. The driver uses no Go symbol the change
// adds: the start path's functions are main's own, and it reads and writes the told record in SQL
// alone, so the script can build it against a base commit's daemon, which is the run's negative
// control.
//
// No session of part one runs the legion plugin or writes a transcript, so part one has no
// contract refusal of the plugin to read, which part two fails a run on (sessionsContractRefusal).
//
// Every check prints `   assert <step>: <what held>` once it holds, which the script counts, and
// a check that does not hold prints `FAIL <step>: <what did not hold>` and ends the run. A failure
// outside the checks, such as the harness's read of a daemon that died, prints the step's FAIL
// line in words of no check (run). Every wait is bounded, and its failure names what it waited
// for and what it last saw. Every check that nothing arrives watches a window in which a delivery
// the same run makes does arrive.
package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

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
	// repo is the default repository of the project of the run in hand, and rt and built the
	// stand-ins its daemon was booted with.
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
// its parent ("" for the root), the phase it is seeded in, the roles whose claims the daemon's
// start path starts at the seed (the root's architect and the member's phase worker), and its pull
// request's number (0 for none).
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
// project of its own whose default repository is repo, set through the route the Dispatch serves
// (setDefaultRepository); makes fixture's trees on Dispatch and seeds them into the store through
// the daemon's start path; boots the whole daemon; brings every claim its starts launch to ready as
// its pane would, and waits for each to take the task its start queued; and checks in SETUP that
// each stands at its entry (standAtTheirEntries).
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
	route := r.setDefaultRepository(cfg.Project, repo)
	fixture()
	r.seed()
	o := fakeRuntime(r.rt, r.built)
	o.workflowTokens = &workflowTokenRecorder{}
	r.d = startDaemonWithin(t, cfg, o, r.log(), 2*time.Minute)
	r.note("daemon booted under gates.design: %s, project %s with default repository %s, set through %s, dispatch_url %s", gates, cfg.Project, repo, route, r.dispatch.base)
	// The boot's run of each seeded start is its claim's first launch, so none before it counts.
	for _, tree := range r.trees {
		for _, member := range tree.members {
			for _, role := range member.roles {
				r.connect(member, role, nil)
			}
		}
	}
	for _, tree := range r.trees {
		for _, member := range tree.members {
			for _, role := range member.roles {
				r.claimant(member, role).awaitStartTaken()
			}
		}
	}
	r.standAtTheirEntries()
}

// rootIssuesFixture is the first run's trees, each named for the step that drives it. No root
// stands in planning through accepting while a child of it is open: the first admission pass would
// move such a root to awaiting_children and stop its worker (dispatch://INFRA-3320 R9), so a step
// would read a phase the boot left. A root with children stands in a phase that wait does not
// hold instead: merging (a6), retro (r6), or admitted, where its own first phase waits for its
// children (a5-child, whose root's checks A5 changes, while a5's own root tests alone).
func (r *versionNewsLive) rootIssuesFixture() {
	architect, implementer, tester := claim.RoleArchitect, claim.RoleImplementer, claim.RoleTester
	reviewer, merger := claim.RoleReviewer, claim.RoleMerger
	r.tree("a2", false, false, versionNewsMember{phase.Reviewing, []claim.Role{architect, reviewer}})
	r.tree("a2-unmoved", false, false, versionNewsMember{phase.Reviewing, []claim.Role{architect, reviewer}})
	r.tree("a1", true, false, versionNewsMember{phase.Admitted, []claim.Role{architect}})
	r.tree("a3-accepting", true, false, versionNewsMember{phase.Accepting, []claim.Role{architect}})
	r.tree("a3-implementing", true, true, versionNewsMember{phase.Implementing, []claim.Role{architect, implementer}})
	r.tree("a5", true, false, versionNewsMember{phase.Testing, []claim.Role{architect, tester}})
	r.tree("a5-child", true, false, versionNewsMember{phase.Admitted, []claim.Role{architect}},
		versionNewsMember{phase.Testing, []claim.Role{tester}})
	r.tree("a6", false, false, versionNewsMember{phase.Merging, []claim.Role{architect, merger}},
		versionNewsMember{phase.Reviewing, []claim.Role{reviewer}})
	r.tree("a7", true, false, versionNewsMember{phase.Retro, []claim.Role{architect, implementer}})
	r.tree("a7-open", true, false, versionNewsMember{phase.Retro, []claim.Role{architect, implementer}})
	r.tree("r6", true, false, versionNewsMember{phase.Retro, []claim.Role{architect, implementer}},
		versionNewsMember{phase.Implementing, []claim.Role{implementer}}, versionNewsMember{phase.Testing, []claim.Role{tester}})
}

// offFixture is the second run's trees: one whose holders must hear nothing, and one whose
// tester's pass is the delivery the same window sees. The silent tree's root stands in retro, a
// phase the wait on its open child does not hold (rootIssuesFixture), so nothing but the approved
// version could send its holders anything.
func (r *versionNewsLive) offFixture() {
	architect := claim.RoleArchitect
	r.tree("off", false, false, versionNewsMember{phase.Retro, []claim.Role{architect, claim.RoleImplementer}},
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

// run runs one step between its `== name` and `ok name` lines, and prints the step's FAIL line when
// it stops on a failure that printed none of its own, so that every stopped step is named. That is
// a failure outside the step's checks, such as the harness failing a read of a daemon that died,
// and its line holds no check's words: the script's negative control bites only on the words of
// A2's one-approval check (awaitQueued), so a harness failure there is no control.
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
		BootToken: launch.BootToken, SessionID: session, OmpSessionFile: "/sessions/" + session + ".jsonl", AgentID: session, PluginContract: versionNewsPluginContract(),
	}, false)
	if status != http.StatusOK {
		r.fail("register %s = %d %s, want 200", token, status, body)
	}
	var registered claim.RegisterResponse
	if err := json.Unmarshal(body, &registered); err != nil {
		r.fail("decode the registration of %s: %v", token, err)
	}
	c.secret = registered.Secret
	c.driver = newTakeDriver(r.d.post, func(frame shimwire.Frame) error { c.send(frame); return nil },
		claim.AgentCaller{ClaimToken: token, SessionID: session, Secret: registered.Secret}, launch.Generation)
	c.driver.started = r.d.turnRead(token)
	c.closed = make(chan struct{})
	go c.serve()
	if status, body := r.d.request(http.MethodPost, "/legion/v1/claims/ready", claim.ReadyRequest{
		ClaimToken: token, SessionID: session, Secret: registered.Secret, Generation: launch.Generation,
	}, false); status != http.StatusNoContent {
		r.fail("ready %s = %d %s, want 204", token, status, body)
	}
	r.claimants[token] = c
	go c.takes()
	return c
}

// versionNewsClaimant is one role's claim as its pane reaches the daemon: the shim on the worker
// stream, the registered session, a reader that answers what the daemon asks of the shim, and the
// plugin's take of each task value the daemon writes (takes), which keeps every take the daemon
// accepted with the moment it was accepted. A take accepted between turns is worked at once, its
// turn started and ended and its report sent as a worker does, so it never holds the next task
// back; while a turn of the agent's own runs (startTurn) nothing is taken, since a worker asks
// only once its host is idle.
type versionNewsClaimant struct {
	r       *versionNewsLive
	token   claim.Token
	tree    string
	issue   string
	role    claim.Role
	session string
	secret  string
	sh      *shim
	driver  *takeDriver
	// closed is closed once the reader's connection is gone, which ends the takes too.
	closed chan struct{}
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
// protocol negotiation and the state probe (streaming while a turn runs, the agent's own or a
// take's).
func (c *versionNewsClaimant) serve() {
	defer close(c.closed)
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
			streaming := c.turn || c.driver.inATurn()
			c.mu.Unlock()
			data, _ := json.Marshal(shimwire.StateData{IsStreaming: &streaming})
			c.send(shimwire.Response{ID: f.ID, Command: shimwire.TypeGetState, Success: true, Data: data})
		}
	}
}

// takes is the plugin taking each task value the daemon writes the claim, every 50 ms until the
// connection is gone: a value it has not reported is reported seen, and one it has not asked at is
// taken while no turn of the agent's own runs (takeDriver.poll). Each take the daemon accepts is
// kept, named by the delivery the claim held when it was accepted, and its turn is run and
// reported at once, as Oh My Pi runs the text it took. A read the daemon did not answer — it is
// restarting, or it has stopped — is tried again.
func (c *versionNewsClaimant) takes() {
	tick := time.NewTicker(50 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-c.closed:
			return
		case <-tick.C:
		}
		c.mu.Lock()
		own := c.turn
		c.mu.Unlock()
		if own {
			continue
		}
		_, answer, asked, err := c.driver.poll()
		if err != nil || !asked || answer.Outcome != string(supervise.TakeOutcomeAccepted) {
			continue
		}
		delivery, _ := c.r.d.pendingID(c.token)
		c.mu.Lock()
		c.prompts = append(c.prompts, versionNewsPrompt{at: time.Now(), delivery: delivery, message: answer.Text})
		c.mu.Unlock()
		select {
		case c.arrived <- struct{}{}:
		default:
		}
		if err := c.driver.startTurn(); err == nil {
			_ = c.driver.endTurn()
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
