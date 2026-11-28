//go:build e2e

// The release before's side of the two live proofs of dispatch://INFRA-4730's cut-over and
// rollback (k31 and k44 of its Verification; R21 and R22 of dispatch://INFRA-3320).
// scripts/e2e/tree-cutover.sh and scripts/e2e/tree-rollback.sh take the tree of the release before
// (scripts/e2e/lib/tree-proof.sh: `git merge-base HEAD origin/main` while that commit does not
// carry this release's conversion, and main before the commit that added it once it does) with
// `git archive`, lay this one file over it, and build the daemon package's `-tags e2e` test binary
// there, as busy-relaunch-delivery.sh's negative control does, so every test here runs the daemon
// of that commit: the release before this one. The proof's own driver
// (tree_cutover_live_test.go or tree_rollback_live_test.go), built from this checkout, holds
// Dispatch, the broker's address and the database, and runs this binary as a child process for
// each part the release before plays, handing it the rig and reading back what it made through
// the file LEGION_E2E_TREE_WORLD names (treeWorld).
//
// So this file names only what both releases carry: the package's test rig as both have it (the
// fake runtime, workflowConfig, startDaemonLogging, the scratch Dispatch's request helpers, the
// Envoy listener's stand-in newNoticeListener), the API's request types, the store's tables in
// SQL, and no Go symbol either release added or removed. This checkout compiles it too (`go vet
// -tags e2e ./...`), and the head drivers use its helpers. One read of the fake runtime goes
// through reflection (treeCleanups), since only the release before's fake records a tree's cleanup
// (fake.Call's Tree).
//
// What the release before cannot be made to leave in the minutes a run has, it is not asked to:
// a tree that lingers with its deadline two days ahead, its reserved cleanup, the hand-over rows a
// linger queues, an operator's hold of a tree, an open production check and a decision kept for an
// architect are written here, with the daemon stopped, as that release's own code writes them
// (seedLinger and the seeds after it), on records and claims that release made itself. The rest it
// makes live: admission, the launches and registrations, the releases of children, the closes, the
// changes of parent and the waits.
package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/dispatch"
	"github.com/sjawhar/legion/daemon/internal/notify"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/runtime/fake"
	"github.com/sjawhar/legion/daemon/internal/store/migrations"
	"github.com/sjawhar/legion/daemon/internal/testwait"
)

// treeWorldEnv names the file the head driver and this binary hand each other the run through.
const treeWorldEnv = "LEGION_E2E_TREE_WORLD"

// treeOldToolRootEnv names the root of the oldest plugin contract's checkout in the fleet, with
// its `bun install` run and this checkout's drivers (treeOldPluginDriver, memberRootDriver) copied
// in (scripts/e2e/lib/old-plugin-root.sh).
const treeOldToolRootEnv = "LEGION_E2E_OLD_TOOL_ROOT"

// treeOldPluginDriver runs one call of the plugin's own daemon client under bun, relative to a
// plugin root.
const treeOldPluginDriver = "packages/pi-legion/test/daemon-client-driver.ts"

// treeProofCap is both releases' admission cap: above every count a run reaches, so no wait of
// the proof is the cap's.
const treeProofCap = 16

// treeWorld is the run as the two drivers hand it to each other. The head driver writes the rig
// (Project, DSN, DispatchURL) before it first runs this binary; each part of the release before
// adds what it made; the head driver adds what this release made before the release before runs
// again.
type treeWorld struct {
	Project     string `json:"project"`
	DSN         string `json:"dsn"`
	DispatchURL string `json:"dispatchUrl"`
	// Issues is each issue of the scenario by the name the drivers give it.
	Issues map[string]string `json:"issues,omitempty"`
	// Claims is each claim a session registered, by its token, with what the session acts with.
	Claims map[claim.Token]treeClaim `json:"claims,omitempty"`
	// OutboxKinds is every kind the release before's own outbox_kind_check admits, read from the
	// database that release migrated (k44).
	OutboxKinds []string `json:"outboxKinds,omitempty"`
	// Queued is the id of the effect of each of OutboxKinds the head driver queued (k44).
	Queued map[string]int64 `json:"queued,omitempty"`
	// HeldPrinted is what the rollback step printed of the session writes this release held
	// (k44).
	HeldPrinted []string `json:"heldPrinted,omitempty"`
	// Migrations is the version of every migration the release before embeds (k44): a rollback
	// across a release the release before does not carry runs that release's step too.
	Migrations []int `json:"migrations,omitempty"`
	// Labels is the `legion.dev/tree` label of each issue's Sandbox once the rollback step's
	// relabel ran over the fake runtime's labels (k44).
	Labels map[string]string `json:"labels,omitempty"`
}

// treeClaim is one registered claim: its issue, role and generation as the registration answered
// them, the tree that registration named, the session and the secret it acts with, and whether the
// oldest contract's plugin registered it.
type treeClaim struct {
	Issue      string     `json:"issue"`
	Role       claim.Role `json:"role"`
	Generation uint64     `json:"generation"`
	Tree       string     `json:"tree"`
	Session    string     `json:"session"`
	Secret     string     `json:"secret"`
	OldPlugin  bool       `json:"oldPlugin,omitempty"`
}

// readTreeWorld reads the world file path names.
func readTreeWorld(t *testing.T, path string) *treeWorld {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read the run's world %s: %v", path, err)
	}
	world := &treeWorld{}
	if err := json.Unmarshal(data, world); err != nil {
		t.Fatalf("decode the run's world %s: %v", path, err)
	}
	if world.Issues == nil {
		world.Issues = map[string]string{}
	}
	if world.Claims == nil {
		world.Claims = map[claim.Token]treeClaim{}
	}
	return world
}

// save writes the world to path.
func (w *treeWorld) save(t *testing.T, path string) {
	t.Helper()
	data, err := json.MarshalIndent(w, "", "  ")
	if err != nil {
		t.Fatalf("encode the run's world: %v", err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("write the run's world %s: %v", path, err)
	}
}

// key is the issue the scenario calls name, which the world must hold.
func (w *treeWorld) key(t *testing.T, name string) string {
	t.Helper()
	key, ok := w.Issues[name]
	if !ok {
		t.Fatalf("the run's world holds no issue called %s: %v", name, w.Issues)
	}
	return key
}

// token is the claim token of role on key in the world's project.
func (w *treeWorld) token(t *testing.T, key string, role claim.Role) claim.Token {
	t.Helper()
	project, err := claim.ProjectToken(w.Project)
	if err != nil {
		t.Fatalf("the project token of %s: %v", w.Project, err)
	}
	token, err := claim.NewToken(project, key, role)
	if err != nil {
		t.Fatalf("the %s claim token of %s: %v", role, key, err)
	}
	return token
}

// treeDaemon is one boot of a release's daemon over the world's rig: its configuration, the fake
// runtime it launches on, its JSON log, the database it runs on, the boot tokens of the launches a
// session has registered, and tune, which, when set, adjusts its test overrides at each boot.
type treeDaemon struct {
	t          *testing.T
	world      *treeWorld
	cfg        config.Config
	pool       *pgxpool.Pool
	rt         *fake.Runtime
	d          *daemon
	logs       *syncBuffer
	registered map[string]bool
	tune       func(*overrides)
}

// treeConfig is a daemon's configuration over the world's rig, the same for both releases: the
// world's project, database and Dispatch, the script's broker, the proof's cap, no controller tick
// (an hour before the first), and no design step gated, so an architect's registration opens its
// own gate. EnvoyURL stays workflowConfig's unreachable default, so every notice stays queued; the
// last part of the rollback gives its one boot an Envoy that takes the state events alone
// (TestTreeRollbackReleaseBeforeAfterTheStep).
func treeConfig(t *testing.T, world *treeWorld, natsURL string) config.Config {
	t.Helper()
	cfg := workflowConfig(t, natsURL)
	cfg.Project = world.Project
	cfg.Projects = map[string]config.Project{world.Project: {}}
	cfg.PostgresDSN = world.DSN
	cfg.AdmissionCap = treeProofCap
	cfg.ControllerWakeInterval = time.Hour
	cfg.Gates.Design = config.DesignGateOff.Steps()
	cfg.DispatchURL = world.DispatchURL
	if err := os.WriteFile(cfg.DispatchTokenFile, []byte(scratchAgentToken+"\n"), 0o600); err != nil {
		t.Fatalf("write the scratch Dispatch bearer: %v", err)
	}
	return cfg
}

// releaseBefore reads the world the head driver wrote and readies the release before's daemon
// over it, without booting it. Run with no broker named, it skips, as every live proof of this
// package does; run by a script with no world named, it fails.
func releaseBefore(t *testing.T) (*treeDaemon, string) {
	t.Helper()
	natsURL := os.Getenv(liveNATSURLEnv)
	if natsURL == "" {
		t.Skipf("%s is unset: the head driver of scripts/e2e/tree-cutover.sh or tree-rollback.sh runs this", liveNATSURLEnv)
	}
	path := os.Getenv(treeWorldEnv)
	if path == "" {
		t.Fatalf("%s is unset: the head driver names the run's world there", treeWorldEnv)
	}
	world := readTreeWorld(t, path)
	return &treeDaemon{t: t, world: world, cfg: treeConfig(t, world, natsURL), pool: openTreePool(t, world.DSN), logs: &syncBuffer{},
		registered: map[string]bool{}}, path
}

// openTreePool opens dsn, the daemon's database, closed when the test ends.
func openTreePool(t *testing.T, dsn string) *pgxpool.Pool {
	t.Helper()
	pool, err := pgxpool.New(t.Context(), dsn)
	if err != nil {
		t.Fatalf("open the daemon's database: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// overrides is a boot's test overrides on a fresh fake runtime whose pods provision their own
// workspaces, with the GitHub stand-in an issue's branch is made on and no controller tick, tuned
// by tune. No process can be relaunched onto a newer plugin (overrides.pluginRelaunchable), as a
// pod of the fleet runs the plugin its image linked, so a plugin of an older contract is answered
// in its narrowing rather than relaunched.
func (r *treeDaemon) overrides() overrides {
	r.rt = fake.NewRuntime()
	r.rt.InPod = true
	o := fakeRuntime(r.rt, &built{})
	o.workflowTokens = &workflowTokenRecorder{}
	o.githubAPI = blockersGitHub(r.t)
	o.controllerFirstWake = time.Hour
	relaunchable := false
	o.pluginRelaunchable = &relaunchable
	if r.tune != nil {
		r.tune(&o)
	}
	return o
}

// boot starts the daemon on overrides with a JSON log at debug, which is appended to the file
// liveDaemonLogEnv names when the test ends, under name.
func (r *treeDaemon) boot(name string) {
	t := r.t
	t.Helper()
	o := r.overrides()
	logs := &syncBuffer{}
	r.logs = logs
	if path := os.Getenv(liveDaemonLogEnv); path != "" {
		t.Cleanup(func() { appendTreeLog(t, path, name, logs.String()) })
	}
	r.registered = map[string]bool{}
	r.d = startDaemonLogging(t, r.cfg, o, slog.New(slog.NewJSONHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
}

// appendTreeLog appends one boot's log to the run's daemon log under its name.
func appendTreeLog(t *testing.T, path, name, logs string) {
	file, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err == nil {
		_, err = fmt.Fprintf(file, "== %s\n%s", name, logs)
		err = errors.Join(err, file.Close())
	}
	if err != nil {
		t.Errorf("write the daemon's log to %s: %v", path, err)
	}
}

// caughtUp waits until the daemon's Dispatch consumer has handled every message of the stream it
// reads up to the stream's last one, which the state's consumers entry of it says by reading
// waiting 0 with no read failure: a failed read of the consumer's position reads waiting 0 too.
func (r *treeDaemon) caughtUp() {
	r.t.Helper()
	settled(r.t, r.d, "catch its Dispatch consumer up with the stream", func(s api.State) bool {
		for _, consumer := range s.Consumers {
			if strings.HasSuffix(consumer.Name, "-dispatch") {
				return consumer.Waiting == 0 && consumer.Unavailable == ""
			}
		}
		return false
	})
}

// stop registers every launch no session has registered yet, waits for the daemon's Dispatch
// consumer to catch up, and stops the daemon.
func (r *treeDaemon) stop() {
	r.t.Helper()
	r.registerAll()
	r.caughtUp()
	r.d.stop()
}

// labelled creates an issue of the world's project carrying the legion label, as a session does,
// under parent ("" for none) and waiting on waits, and names it in the world.
func (r *treeDaemon) labelled(name, title, parent string, waits ...string) string {
	r.t.Helper()
	key := createBlockersIssue(r.t, r.world.DispatchURL, r.world.Project, title, parent, waits)
	r.world.Issues[name] = key
	return key
}

// unhanded creates an issue of the world's project that nobody hands to Legion, as the person, at
// status, under parent ("" for none), and names it in the world.
func (r *treeDaemon) unhanded(name, title, status, parent string) string {
	r.t.Helper()
	body := map[string]any{"project": r.world.Project, "title": title}
	if parent != "" {
		body["parent"] = parent
	}
	key := createDispatchIssue(r.t, r.world.DispatchURL, title, body, asPerson)
	setDispatchStatus(r.t, r.world.DispatchURL, key, status)
	r.world.Issues[name] = key
	return key
}

// launches is every launch of the boot's runtime, in order.
func (r *treeDaemon) launches() []runtime.SpawnSpec {
	var specs []runtime.SpawnSpec
	for _, call := range r.rt.Calls() {
		if call.Method == "Spawn" || call.Method == "Resume" {
			specs = append(specs, call.Spec)
		}
	}
	return specs
}

// awaitLaunch waits for a launch of token and answers the newest.
func (r *treeDaemon) awaitLaunch(token claim.Token) runtime.SpawnSpec {
	r.t.Helper()
	testwait.Eventually(r.t, "the daemon to launch "+string(token), func() bool {
		return slices.ContainsFunc(r.launches(), func(spec runtime.SpawnSpec) bool { return spec.Claim == token })
	})
	return lastLaunch(r.t, r.rt, token)
}

// registerAll registers, as its plugin does once the agent has booted, every launch of the boot no
// session has registered yet, and keeps each claim in the world.
func (r *treeDaemon) registerAll() {
	r.t.Helper()
	for _, spec := range r.launches() {
		if !r.registered[spec.BootToken] {
			r.register(spec)
		}
	}
}

// register registers launch's session, "ses_<claim token>", which a relaunch of the claim in its
// session registers again, and keeps the claim in the world.
func (r *treeDaemon) register(launch runtime.SpawnSpec) claim.RegisterResponse {
	r.t.Helper()
	session := "ses_" + string(launch.Claim)
	status, body := r.d.request(http.MethodPost, "/legion/v1/claims/register", claim.RegisterRequest{
		BootToken: launch.BootToken, SessionID: session, OmpSessionFile: "/sessions/" + session + ".jsonl",
		AgentID: "agent", PluginContract: api.DaemonAPIVersion(),
	}, false)
	var registered claim.RegisterResponse
	if status != http.StatusOK || json.Unmarshal(body, &registered) != nil {
		r.t.Fatalf("register the session of %s = %d; body %s", launch.Claim, status, body)
	}
	r.keep(launch, session, registered, false)
	return registered
}

// keep records launch's registration in the world.
func (r *treeDaemon) keep(launch runtime.SpawnSpec, session string, registered claim.RegisterResponse, old bool) {
	r.registered[launch.BootToken] = true
	r.world.Claims[launch.Claim] = treeClaim{Issue: registered.Issue, Role: registered.Role, Generation: registered.Generation,
		Tree: registered.Tree, Session: session, Secret: registered.Secret, OldPlugin: old}
}

// grant mints one grant for the registered claim token, naming the tree its registration answered,
// as the claim's plugin does before each command.
func (r *treeDaemon) grant(token claim.Token) string {
	r.t.Helper()
	held, ok := r.world.Claims[token]
	if !ok {
		r.t.Fatalf("no session registered %s", token)
	}
	status, body := r.d.request(http.MethodPost, "/legion/v1/grants", api.GrantRequest{
		SessionID: held.Session, Secret: held.Secret, Tree: held.Tree, Issue: held.Issue,
	}, false)
	var minted api.GrantResponse
	if status != http.StatusOK || json.Unmarshal(body, &minted) != nil {
		r.t.Fatalf("the grant of %s on %s = %d; body %s", token, held.Tree, status, body)
	}
	return minted.GrantID
}

// ready reports the registered claim token ready, as its plugin does once its agent has booted;
// the daemon answers an architect's ready with the catch-up of its tree.
func (r *treeDaemon) ready(token claim.Token) {
	r.t.Helper()
	held, ok := r.world.Claims[token]
	if !ok {
		r.t.Fatalf("no session registered %s", token)
	}
	if status, body := r.d.request(http.MethodPost, "/legion/v1/claims/ready", claim.ReadyRequest{
		ClaimToken: token, SessionID: held.Session, Secret: held.Secret, Generation: held.Generation,
	}, false); status != http.StatusNoContent {
		r.t.Fatalf("the ready of %s = %d; body %s", token, status, body)
	}
}

// state is the daemon's state, read once.
func (r *treeDaemon) state() (api.State, string) {
	r.t.Helper()
	return settled(r.t, r.d, "answer its state", func(api.State) bool { return true })
}

// placed is the lists of the state that name key: admission.active, admission.waiting and
// admission.held, which R21 gives each issue in Legion's work one place in.
func placed(state api.State, key string) []string {
	var lists []string
	if slices.Contains(state.Admission.Active, key) {
		lists = append(lists, "active")
	}
	if slices.Contains(state.Admission.Waiting, key) {
		lists = append(lists, "waiting")
	}
	if slices.ContainsFunc(state.Admission.Held, func(held api.HeldIssue) bool { return held.Issue == key }) {
		lists = append(lists, "held")
	}
	return lists
}

// admitRoot hands key, a labelled root, to Legion as the person's todo, waits for it to take a
// slot, for its architect's launch and for Dispatch to show the daemon's in_progress, and
// registers what the boot launched.
func (r *treeDaemon) admitRoot(key string) claim.Token {
	r.t.Helper()
	setDispatchStatus(r.t, r.world.DispatchURL, key, "todo")
	settled(r.t, r.d, "admit "+key+" into a slot", func(s api.State) bool { return slices.Contains(s.Admission.Active, key) })
	architect := r.world.token(r.t, key, claim.RoleArchitect)
	r.awaitLaunch(architect)
	r.registerAll()
	awaitDispatchStatus(r.t, r.world.DispatchURL, key, "in_progress")
	return architect
}

// openGate registers key's own document at version 1 as its architect, which with no design step
// gated opens key's gate, so a child of it can start.
func (r *treeDaemon) openGate(key string, architect claim.Token) {
	r.t.Helper()
	if status, answer := r.d.request(http.MethodPost, "/legion/v1/gates/register", api.GateRegisterRequest{
		GrantID: r.grant(architect), Issue: key, ArtifactID: dispatchPrimaryArtifact(r.t, r.world.DispatchURL, key), Version: 1,
	}, false); status != http.StatusOK {
		r.t.Fatalf("the design-gate registration of %s = %d; body %s", key, status, answer)
	}
	settled(r.t, r.d, "open the design gate of "+key, func(s api.State) bool {
		gate := s.Issues[key].DesignGate
		return gate != nil && gate.ApprovedVersion != nil
	})
}

// child creates a labelled child of parent, a root at work whose gate is open, and sets it todo
// as the person, which starts its first phase, as a person's todo under a live tree does; it waits
// for its planner's launch and registers what the boot launched. With old, the oldest plugin
// contract's plugin registers the planner (oldPluginRegister).
func (r *treeDaemon) child(name, title, parent string, old bool) string {
	r.t.Helper()
	key := r.labelled(name, title, parent)
	setDispatchStatus(r.t, r.world.DispatchURL, key, "todo")
	planner := r.awaitLaunch(r.world.token(r.t, key, claim.RolePlanner))
	if old {
		r.oldPluginRegister(planner)
	}
	r.registerAll()
	return key
}

// waitingChild creates a labelled child of parent waiting on waits, sets it todo as the person,
// and waits for the daemon to record it at admitted, held on what it waits on.
func (r *treeDaemon) waitingChild(name, title, parent string, waits ...string) string {
	r.t.Helper()
	key := r.labelled(name, title, parent, waits...)
	setDispatchStatus(r.t, r.world.DispatchURL, key, "todo")
	settled(r.t, r.d, "record "+key+" under "+parent, func(s api.State) bool {
		recorded, ok := s.Issues[key]
		return ok && recorded.Phase == phase.Admitted
	})
	return key
}

// reparent sets key under parent ("" takes its parent off) as the person, and waits for the
// daemon's copy of Dispatch's issues to read it.
func (r *treeDaemon) reparent(key, parent string) {
	r.t.Helper()
	var value any
	if parent != "" {
		value = parent
	}
	var written struct {
		LastSeq int64 `json:"last_seq"`
	}
	dispatchCall(r.t, r.world.DispatchURL, http.MethodPatch, "/api/v1/issues/"+key, map[string]any{"parent": value}, &written)
	testwait.Eventually(r.t, fmt.Sprintf("the daemon's copy to read %s under %q at Dispatch's sequence %d", key, parent, written.LastSeq), func() bool {
		var copied string
		var seq int64
		if err := r.pool.QueryRow(r.t.Context(), `select coalesce(parent, ''), dispatch_seq from dispatch_issues where key = $1`, key).
			Scan(&copied, &seq); err != nil {
			return false
		}
		return copied == parent && seq >= written.LastSeq
	})
}

// closed sets key done as the person and waits for every claim of it to retire, its sessions kept.
func (r *treeDaemon) closed(key string) {
	r.t.Helper()
	setDispatchStatus(r.t, r.world.DispatchURL, key, "done")
	testwait.Eventually(r.t, "every claim of "+key+" to retire", func() bool {
		var live int
		if err := r.pool.QueryRow(r.t.Context(), `select count(*) from claims where issue = $1 and state <> 'retired'`, key).Scan(&live); err != nil {
			return false
		}
		return live == 0
	})
}

// blocked waits for the daemon to name key in admission.blocked as waiting on waits.
func (r *treeDaemon) blocked(key string, waits ...string) {
	r.t.Helper()
	settled(r.t, r.d, "name "+key+" blocked", func(s api.State) bool {
		by, ok := blockedOn(s, key)
		return ok && slices.Equal(by, waits)
	})
}

// daemonWrite writes status on key in Dispatch as the daemon's own session, the actor every
// status write of the daemon carries (dispatch.DaemonSession), with the legion label: the write a
// linger's hand-over makes.
func (r *treeDaemon) daemonWrite(key, status string) {
	r.t.Helper()
	body := map[string]any{"status": status, "labels_add": []string{"legion"},
		"actor": map[string]string{"kind": "session", "id": dispatch.DaemonSession(key)}}
	answer, reply := dispatchAnswerAs(r.t, r.world.DispatchURL, http.MethodPatch, "/api/v1/issues/"+key, body, asSession)
	if answer != http.StatusOK {
		r.t.Fatalf("the daemon's own write of %s on %s = %d: %s", status, key, answer, reply)
	}
	awaitDispatchStatus(r.t, r.world.DispatchURL, key, status)
}

// exec runs statement over the daemon's database, which no daemon runs on while it does.
func (r *treeDaemon) exec(what, statement string, args ...any) {
	r.t.Helper()
	if _, err := r.pool.Exec(r.t.Context(), statement, args...); err != nil {
		r.t.Fatalf("write %s as the release before writes it: %v", what, err)
	}
}

// generation is the run key stands at in the daemon's record.
func (r *treeDaemon) generation(key string) uint64 {
	r.t.Helper()
	var generation uint64
	if err := r.pool.QueryRow(r.t.Context(), `select generation from issues where key = $1`, key).Scan(&generation); err != nil {
		r.t.Fatalf("read the run of %s: %v", key, err)
	}
	return generation
}

// projectToken is the world's project as the claim store keys it.
func (r *treeDaemon) projectToken() string {
	r.t.Helper()
	project, err := claim.ProjectToken(r.world.Project)
	if err != nil {
		r.t.Fatalf("the project token of %s: %v", r.world.Project, err)
	}
	return string(project)
}

// seedLinger leaves root's tree as the release before's linger leaves a closed root's: the root
// done with its linger deadline two days ahead, the slots of the root and of handed freed, as a
// root's close frees every slot its tree's issues hold, and the tree's close rows queued for it (its
// linger_close at the deadline, the tree close of its architect's claim and its workspace's
// removal), the tree's cleanup reserved and not confirmed, each of handed, members still at work,
// handed over (its handovers row and its hand-over status row, queued long after any run ends), and
// the tree's draw of an experiment. The root's wait on its open children, which the release before
// keeps while a child of it is open, ends with the close, and so does its task of the run, done at
// the place it stood, as that release's leave writes both (workflow's endTaskWithLeave on main:
// moveTask to done with leaveState's done for a close at done). Dispatch is the caller's: the
// person's close of the root and the hand-over writes that landed.
func (r *treeDaemon) seedLinger(root string, handed ...string) {
	r.t.Helper()
	generation := r.generation(root)
	project := r.projectToken()
	r.exec("the linger of "+root, `update issues set phase = 'done', status = 'done', dispatch_status = 'done',
		linger_until = now() + interval '48 hours', children_wait_start = null, children_wait_task = null,
		children_wait_reason = null, children_wait_since = null, children_wait_children = null where key = $1`, root)
	r.exec("the task of "+root+" its close ends", `update plan_tasks set state = 'done', stopped_reason = ''
		where issue = $1 and generation = $2 and state not in ('done', 'left')`, root, generation)
	r.exec("the slots the close of "+root+" frees", `delete from slots where issue = any($1)`, append([]string{root}, handed...))
	r.exec("the reserved cleanup of "+root, `update tree_lifecycles set cleanup_started = true where project = $1 and tree = $2`, project, root)
	r.exec("the close rows of "+root, `insert into outbox (kind, issue, payload, attempts, next_at, last_error) values
		('linger_close', $1, jsonb_build_object('generation', $2::bigint), 0, now() + interval '48 hours', ''),
		('supervise', $1, jsonb_build_object('op', 'tree_close', 'tree', $1::text, 'role', 'architect', 'generation', $2::bigint, 'linger', $2::bigint), 0, '2031-01-01T00:00:00Z', ''),
		('workspace_remove', $1, jsonb_build_object('linger', $2::bigint, 'tree', $1::text), 0, '2031-01-01T00:00:00Z', '')`, root, generation)
	for _, key := range handed {
		r.exec("the hand-over of "+key, `insert into handovers (issue, tree, linger_generation) values ($1, $2, $3)`, key, root, generation)
		r.exec("the hand-over write of "+key, `insert into outbox (kind, issue, payload, attempts, next_at, last_error) values
			('dispatch_status', $1, jsonb_build_object('kind', 'hand-over', 'status', 'todo', 'observedStatus', 'in_progress',
				'handOverTree', $2::text, 'handOverLinger', $3::bigint), 0, '2031-01-01T00:00:00Z', '')`, key, root, generation)
	}
	r.exec("the tree draw of "+root, `insert into experiment_assignments (issue_key, experiment, unit, variant, mode, drawn_at)
		values ($1, 'prompt-style', 'tree', 'terse', 'draw', now())`, root)
}

// seedOperatorHold gives root's tree lifecycle to the operator, as an operator's hold of a tree
// leaves it.
func (r *treeDaemon) seedOperatorHold(root string) {
	r.t.Helper()
	r.exec("the operator's hold of "+root, `update tree_lifecycles set authority = 'operator' where project = $1 and tree = $2`,
		r.projectToken(), root)
}

// seedProductionCheck opens the production check of key, a closed child of tree, on a merged pull
// request, as the close at a merge opens it, with its bound row queued at the bound.
func (r *treeDaemon) seedProductionCheck(key, tree string) {
	r.t.Helper()
	r.exec("the production check of "+key, `insert into production_checks (issue, project, tree, repo, pr_number, merge_sha, opened_at,
		bound_at, role) values ($1, $2, $3, 'acme/widgets', 7, 'c0ffee0123456789abcdef0123456789abcdef01', now(), now() + interval '14 days', 'tester')`,
		key, r.world.Project, tree)
	r.exec("the production check's bound of "+key, `insert into outbox (kind, issue, payload, attempts, next_at, last_error)
		values ('check_bound', $1, '{}'::jsonb, 0, now() + interval '14 days', '')`, key)
}

// treeDecision is the decision a person made on a parked tree that seedDecision keeps for its
// architect.
const treeDecision = "Two regions take the first wave."

// seedDecision keeps a person's decision on key for the next start or catch-up of its tree, as a
// parked-decision notice leaves it once the controller relayed it.
func (r *treeDaemon) seedDecision(key string) {
	r.t.Helper()
	r.exec("the kept decision on "+key, `insert into parked_decisions (issue, ref, decision, actor, at) values ($1, 'wave-ask', $2, 'smoke', now())`,
		key, treeDecision)
}

// oldPluginAnswer is what treeOldPluginDriver printed of one call: the answer, or the daemon's
// refusal.
type oldPluginAnswer struct {
	OK     bool            `json:"ok"`
	Answer json.RawMessage `json:"answer"`
	Status int             `json:"status"`
	Code   string          `json:"code"`
	Error  string          `json:"error"`
}

// oldPluginCall runs method of the daemon client of the oldest plugin contract in the fleet
// against d with input, through treeOldPluginDriver under bun in that contract's root, and answers
// what it printed.
func oldPluginCall(t *testing.T, d *daemon, method string, input any) oldPluginAnswer {
	t.Helper()
	encoded, err := json.Marshal(map[string]any{"daemonUrl": d.base, "method": method, "input": input})
	if err != nil {
		t.Fatalf("encode the old plugin's %s: %v", method, err)
	}
	stdout, stderr := runBunDriver(t, "the old plugin's "+method, oldToolRoot(t), treeOldPluginDriver, t.TempDir(), 2*time.Minute, encoded)
	var answer oldPluginAnswer
	if err := json.Unmarshal(stdout, &answer); err != nil {
		t.Fatalf("decode the old plugin's %s answer %s: %v\nstderr %s", method, stdout, err, stderr)
	}
	t.Logf("the oldest contract's plugin: %s answered %s", method, stdout)
	return answer
}

// oldToolRoot is the root of the oldest plugin contract's checkout the script names
// (treeOldToolRootEnv), failing t where it names none.
func oldToolRoot(t *testing.T) string {
	t.Helper()
	root := os.Getenv(treeOldToolRootEnv)
	if root == "" {
		t.Fatalf("%s is unset; the script makes the oldest contract's plugin root and names it there", treeOldToolRootEnv)
	}
	return root
}

// runBunDriver runs driver, a script under root, with bun in root's packages/pi-legion, input on
// its stdin, within deadline, and answers what it printed on stdout and on stderr. Nothing of the
// caller's environment reaches bun but PATH: its home and temporary directory are home. what names
// the run in a failure.
func runBunDriver(t *testing.T, what, root, driver, home string, deadline time.Duration, input []byte) ([]byte, []byte) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), deadline)
	defer cancel()
	command := exec.CommandContext(ctx, "bun", filepath.Join(root, driver))
	command.WaitDelay = time.Second
	command.Dir = filepath.Join(root, "packages", "pi-legion")
	command.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + home, "TMPDIR=" + home,
		"XDG_CONFIG_HOME=" + filepath.Join(home, "config"), "XDG_STATE_HOME=" + filepath.Join(home, "state"),
		"XDG_CACHE_HOME=" + filepath.Join(home, "cache")}
	command.Stdin = bytes.NewReader(input)
	var stdout, stderr bytes.Buffer
	command.Stdout, command.Stderr = &stdout, &stderr
	if err := command.Run(); err != nil {
		t.Fatalf("%s failed: %v\nstdout %s\nstderr %s", what, err, stdout.Bytes(), stderr.Bytes())
	}
	return stdout.Bytes(), stderr.Bytes()
}

// oldPluginRegister registers launch's session through the oldest contract's plugin, as that
// plugin's boot does, and keeps the claim in the world as that plugin's.
func (r *treeDaemon) oldPluginRegister(launch runtime.SpawnSpec) claim.RegisterResponse {
	r.t.Helper()
	session := "ses_" + string(launch.Claim)
	answer := oldPluginCall(r.t, r.d, "register", map[string]any{
		"bootToken": launch.BootToken, "sessionId": session, "ompSessionFile": "/sessions/" + session + ".jsonl", "agentId": "agent",
	})
	var registered claim.RegisterResponse
	if !answer.OK || json.Unmarshal(answer.Answer, &registered) != nil || registered.Secret == "" {
		r.t.Fatalf("the oldest contract's plugin's registration of %s answered %+v", launch.Claim, answer)
	}
	r.keep(launch, session, registered, true)
	return registered
}

// treeCleanups is the tree of every cleanup of a tree the runtime was asked for. Only the release
// before's runtime has such a call (fake.Call's Tree); this file builds against this release too,
// whose fake has no Tree, so it is read by name.
func treeCleanups(rt *fake.Runtime) []string {
	var trees []string
	for _, call := range rt.CallsOf("CleanupTree") {
		if field := reflect.ValueOf(call).FieldByName("Tree"); field.IsValid() {
			trees = append(trees, field.String())
		}
	}
	return trees
}

// outboxKinds is every kind the outbox_kind_check of the database admits, read off the
// constraint's own text.
func outboxKinds(t *testing.T, pool *pgxpool.Pool) []string {
	t.Helper()
	var definition string
	if err := pool.QueryRow(t.Context(), `select pg_get_constraintdef(oid) from pg_constraint where conname = 'outbox_kind_check'`).
		Scan(&definition); err != nil {
		t.Fatalf("read the outbox's kind check: %v", err)
	}
	var kinds []string
	for _, part := range strings.Split(definition, "'")[1:] {
		if part != "" && !strings.ContainsAny(part, " ,()[]:") {
			kinds = append(kinds, part)
		}
	}
	slices.Sort(kinds)
	kinds = slices.Compact(kinds)
	if len(kinds) == 0 {
		t.Fatalf("the outbox's kind check %q names no kind", definition)
	}
	return kinds
}

// refusedRows is each row of a kind in kinds the outbox holds that the daemon's own decoder refuses
// (record.DecodeOutboxPayload, which decodes strictly), as "<id> <kind> <issue>: <why>". A row of a
// kind outside kinds, the kinds the daemon's own kind check admits, is a row no lane of the daemon
// lists, so it runs none and refuses none.
func refusedRows(t *testing.T, pool *pgxpool.Pool, kinds []string) []string {
	t.Helper()
	rows, err := pool.Query(t.Context(), `select id, kind, issue, payload from outbox where kind = any($1) order by id`, kinds)
	if err != nil {
		t.Fatalf("read the outbox: %v", err)
	}
	defer rows.Close()
	var refused []string
	for rows.Next() {
		var row record.OutboxRow
		var kind string
		if err := rows.Scan(&row.ID, &kind, &row.Issue, &row.Payload); err != nil {
			t.Fatalf("read an outbox row: %v", err)
		}
		row.Kind = record.OutboxKind(kind)
		if _, err := record.DecodeOutboxPayload(row); err != nil {
			refused = append(refused, fmt.Sprintf("%d %s %s: %v", row.ID, kind, row.Issue, err))
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read the outbox: %v", err)
	}
	return refused
}

// TestTreeCutoverReleaseBefore is the release before's part of k31 (tree-cutover.sh): it drives
// the release before over the world's rig until it leaves every case of the cut-over, stops it,
// and writes the cases that take time as that release writes them (see the file's comment). The
// world it writes names each issue:
//
//   - A, a root at work whose architect registered and whose gate is open; A1, a child of it in
//     planning whose planner the oldest contract's plugin registered, on which a person's decision
//     is kept for A's architect; A2, a child of it in planning whose parent the person took off; A3,
//     a child of it the person closed, whose claims retired with their sessions kept, and whose
//     production check is open;
//   - B, a root at work the person set under A after it started;
//   - C, a root waiting on X, an issue nobody hands to Legion;
//   - E, a root at work whose tree the operator holds;
//   - L, a root the person closed whose tree lingers with its cleanup reserved; L1 and L2, children
//     of it in planning that its linger handed over, L1's write landed in Dispatch and L2's not; L3,
//     a child of it the person closed.
func TestTreeCutoverReleaseBefore(t *testing.T) {
	r, path := releaseBefore(t)
	r.boot("the release before")
	url := r.world.DispatchURL

	a := r.labelled("A", "Harbour crane retrofit", "")
	architectA := r.admitRoot(a)
	r.openGate(a, architectA)
	a1 := r.child("A1", "Quay lighting survey", a, true)
	a2 := r.child("A2", "Bollard paint refresh", a, false)
	a3 := r.child("A3", "Fender timber swap", a, false)

	b := r.labelled("B", "Gantry motor rewind", "")
	r.admitRoot(b)
	r.reparent(b, a)
	r.reparent(a2, "")
	r.closed(a3)

	x := r.unhanded("X", "Dredging permit renewal", "in_progress", "")
	c := r.labelled("C", "Silt barrier install", "", x)
	setDispatchStatus(t, url, c, "todo")
	r.blocked(c, x)

	e := r.labelled("E", "Pilot boat refit", "")
	r.admitRoot(e)

	l := r.labelled("L", "Breakwater tide logger", "")
	architectL := r.admitRoot(l)
	r.openGate(l, architectL)
	l1 := r.child("L1", "Mooring chain gauge", l, false)
	l2 := r.child("L2", "Anchor winch grease", l, false)
	l3 := r.child("L3", "Slipway rail weld", l, false)
	r.closed(l3)
	r.stop()

	r.seedLinger(l, l1, l2)
	r.seedOperatorHold(e)
	r.seedProductionCheck(a3, a)
	r.seedDecision(a1)
	// Dispatch, while no daemon runs: the person's close of L, which the parent rule this rig runs
	// without allows beside its children at work, and the daemon's own hand-over write of L1, which
	// landed; L2's did not.
	setDispatchStatus(t, url, l, "done")
	r.daemonWrite(l1, "todo")
	r.world.save(t, path)
	t.Logf("the release before left %v, with %d claims registered", r.world.Issues, len(r.world.Claims))
}

// labelLaunches keeps, for each issue a launch of the boot's runtime names, the tree that first
// launch named, which is the `legion.dev/tree` label the Sandbox made for the issue's pod carries
// (the fake makes no Sandbox), unless the world already names one.
func (r *treeDaemon) labelLaunches() {
	if r.world.Labels == nil {
		r.world.Labels = map[string]string{}
	}
	for _, spec := range r.launches() {
		if _, labelled := r.world.Labels[spec.Issue]; !labelled {
			r.world.Labels[spec.Issue] = spec.Tree
		}
	}
}

// TestTreeRollbackReleaseBefore is the release before's first part of k44 (tree-rollback.sh): it
// drives the release before over the world's rig, stops it, and writes the cases that take time as
// that release writes them (see the file's comment). The world it writes names each issue:
//
//   - A, a root at work whose gate is open; A1 and U, children of it in planning; A2, a child of it
//     the release before recorded and holds waiting on X; A4, a child of it in backlog, which that
//     release records nothing of; A3, a child of it the person closed, whose production check is
//     open;
//   - E, D and W, roots at work: A2's parent to be, the root the person closes and reopens, and the
//     root the effects the head driver queues act on;
//   - C, a root waiting on X, an issue nobody hands to Legion;
//   - K, a labelled child of Q, an issue nobody hands to Legion that stands at in_progress, which the
//     release before admitted as a root of its own; the release before's consumer caught up before
//     it stopped, so Q's row is in its copy of Dispatch's issues;
//   - L, a root the person closed whose tree lingers; L1 and L2, children of it in planning that its
//     linger handed over, L1's write landed in Dispatch and L2's not; L3, a child of it the person
//     closed.
//
// It also names the `legion.dev/tree` label of each issue's pod the release before made (Labels)
// and every kind the release before's outbox_kind_check admits, read from the database it
// migrated (OutboxKinds).
func TestTreeRollbackReleaseBefore(t *testing.T) {
	r, path := releaseBefore(t)
	r.boot("the release before")
	url := r.world.DispatchURL

	a := r.labelled("A", "Granary roof survey", "")
	architectA := r.admitRoot(a)
	r.openGate(a, architectA)
	r.child("A1", "Silo vent cleaning", a, false)
	a3 := r.child("A3", "Hopper gate seal", a, false)
	r.child("U", "Weighbridge scale check", a, false)
	x := r.unhanded("X", "Tractor licence renewal", "in_progress", "")
	r.waitingChild("A2", "Conveyor belt splice", a, x)
	a4 := r.labelled("A4", "Loading dock ramp", a)
	setDispatchStatus(t, url, a4, "backlog")
	r.closed(a3)

	e := r.labelled("E", "Orchard drip irrigation", "")
	r.openGate(e, r.admitRoot(e))
	r.admitRoot(r.labelled("D", "Cider press gasket", ""))
	r.admitRoot(r.labelled("W", "Beehive frame repair", ""))
	c := r.labelled("C", "Barn door hinge", "", x)
	setDispatchStatus(t, url, c, "todo")
	r.blocked(c, x)
	q := r.unhanded("Q", "Vineyard trellis wiring", "in_progress", "")
	r.admitRoot(r.labelled("K", "Grape press motor", q))

	l := r.labelled("L", "Stable water trough", "")
	architectL := r.admitRoot(l)
	r.openGate(l, architectL)
	l1 := r.child("L1", "Paddock fence post", l, false)
	l2 := r.child("L2", "Hay loft ladder", l, false)
	l3 := r.child("L3", "Feed bin latch", l, false)
	r.closed(l3)
	r.labelLaunches()
	r.stop()
	var copied int
	if err := r.pool.QueryRow(t.Context(), `select count(*) from dispatch_issues where key = $1`, q).Scan(&copied); err != nil || copied != 1 {
		t.Fatalf("the release before's copy of Dispatch's issues holds %d rows of %s (%v), want its row", copied, q, err)
	}

	r.seedLinger(l, l1, l2)
	r.seedProductionCheck(a3, a)
	setDispatchStatus(t, url, l, "done")
	r.daemonWrite(l1, "todo")
	r.world.OutboxKinds = outboxKinds(t, r.pool)
	embedded, err := migrations.All()
	if err != nil {
		t.Fatalf("read the release before's migrations: %v", err)
	}
	for _, migration := range embedded {
		r.world.Migrations = append(r.world.Migrations, migration.Version)
	}
	r.world.save(t, path)
	t.Logf("the release before left %v, with %d claims registered; its outbox admits %v", r.world.Issues, len(r.world.Claims), r.world.OutboxKinds)
}

// activity is a claim's report of what its session does, in the shape every contract since the
// report began sends.
func activity(token claim.Token, held treeClaim) map[string]any {
	return map[string]any{"claimToken": string(token), "sessionId": held.Session, "secret": held.Secret, "activity": map[string]any{
		"at": time.Now().UTC().Format(time.RFC3339Nano), "backgroundWork": map[string]any{"running": false, "kinds": []string{}},
		"phase": map[string]any{"nextReminderAt": nil}}}
}

// TestTreeRollbackReleaseBeforeAfterTheStep is the release before's last part of k44: the head
// driver has run this release over the world, stopped it and run the rollback step of
// docs/kubernetes.md, and the release before boots on the database as it then is. It holds that
// the outbox carries no row its own decoder refuses, makes due each effect the head driver queued
// (Queued), boots with an Envoy that takes the project's state events and no other request, and
// holds: each claim acts, its activity taken and an architect's grant minted on the tree the
// release before stores for it, and the architect of F, a root this release recorded after its
// conversion, registers F's gate, which the release before refuses
// ISSUE_OUTSIDE_TREE where F's stored tree is not its claim's; the step printed K's held write and
// the release before writes K's reassert, so Dispatch shows K's recorded status again while K works
// on; a new root and its claims start; each queued effect runs and none is refused; A2, whose
// Dispatch parent changed since its record, stands in its old tree A, as U, which this release
// unlinked, does; N, whose parent changed after the conversion, is held by that release's ancestry
// hold; L1, the hand-over the conversion finished, stands as a root of its own; and the close of D,
// a root at work, cleans up no tree that labels the pod of an issue at work.
func TestTreeRollbackReleaseBeforeAfterTheStep(t *testing.T) {
	r, _ := releaseBefore(t)
	world := r.world
	if refused := refusedRows(t, r.pool, world.OutboxKinds); len(refused) != 0 {
		t.Fatalf("the outbox carries rows the release before refuses after the rollback step:\n%s", strings.Join(refused, "\n"))
	}
	queued := slices.Collect(maps.Values(world.Queued))
	r.exec("the queued effects come due", `update outbox set next_at = now() where id = any($1)`, queued)
	// This boot's Envoy takes the project's state events and answers every other request 503, as a
	// listener that is down does, which every caller treats as it treats treeConfig's unreachable
	// default, an outage to ask again about: so every notice stays queued as before, while the state
	// lane, which publishes strictly in sequence (record.LaneState), publishes the oldest state row,
	// which no earlier boot could, and every state row behind it, the queued ones among them.
	stateTopic := notify.StateTopic(r.projectToken())
	listener := newNoticeListener()
	envoy := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, req *http.Request) {
		body, err := io.ReadAll(req.Body)
		var published listenerPublish
		if err != nil || req.Method != http.MethodPost || req.URL.Path != "/v1/messages/publish" || json.Unmarshal(body, &published) != nil ||
			published.Topic != stateTopic {
			rw.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		req.Body = io.NopCloser(bytes.NewReader(body))
		listener.ServeHTTP(rw, req)
	}))
	t.Cleanup(envoy.Close)
	r.cfg.EnvoyURL = envoy.URL
	r.boot("the release before, after the rollback step")
	r.caughtUp()
	key := func(name string) string { return world.key(t, name) }
	a, a2, d, f, k, l1, n, u, w := key("A"), key("A2"), key("D"), key("F"), key("K"), key("L1"), key("N"), key("U"), key("W")

	for token, held := range world.Claims {
		var tree, state string
		if err := r.pool.QueryRow(t.Context(), `select tree, state from claims where token = $1`, string(token)).Scan(&tree, &state); err != nil {
			t.Fatalf("read the claim %s: %v", token, err)
		}
		if state == "retired" || held.Issue == w {
			continue
		}
		if status, body := r.d.request(http.MethodPost, "/legion/v1/claims/activity", activity(token, held), false); status != http.StatusNoContent {
			t.Fatalf("the release before refused the activity of %s with %d: %s", token, status, body)
		}
		if held.Role == claim.RoleArchitect {
			held.Tree = tree
			r.world.Claims[token] = held
			r.grant(token)
		}
		t.Logf("the claim %s (%s) acts under the release before in tree %s", token, state, tree)
	}
	architectF := world.token(t, f, claim.RoleArchitect)
	if status, answer := r.d.request(http.MethodPost, "/legion/v1/gates/register", api.GateRegisterRequest{
		GrantID: r.grant(architectF), Issue: f, ArtifactID: dispatchPrimaryArtifact(t, world.DispatchURL, f), Version: 1,
	}, false); status != http.StatusOK {
		t.Fatalf("the release before refused the gate registration of %s, a root this release recorded, with %d: %s", f, status, answer)
	}

	if !slices.ContainsFunc(world.HeldPrinted, func(line string) bool { return strings.HasPrefix(line, k+" ") }) {
		t.Fatalf("the rollback step printed no held write of %s: %q", k, world.HeldPrinted)
	}
	awaitDispatchStatus(t, world.DispatchURL, k, "in_progress")
	settled(t, r.d, "keep "+k+" at work", func(s api.State) bool {
		return s.Issues[k].Phase != phase.Done && slices.Contains(s.Admission.Active, k)
	})

	r.ready(r.admitRoot(r.labelled("H", "Greenhouse vent motor", "")))

	for kind, id := range world.Queued {
		var row record.OutboxRow
		var gone bool
		testwait.Eventually(t, fmt.Sprintf("the release before to run the queued %s row %d", kind, id), func() bool {
			err := r.pool.QueryRow(t.Context(), `select issue, payload, attempts, last_error from outbox where id = $1`, id).
				Scan(&row.Issue, &row.Payload, &row.Attempts, &row.LastError)
			gone = errors.Is(err, pgx.ErrNoRows)
			return gone || err == nil && row.Attempts > 0
		})
		if gone {
			t.Logf("the release before ran the queued %s row %d and finished it", kind, id)
			continue
		}
		row.ID, row.Kind = id, record.OutboxKind(kind)
		if _, err := record.DecodeOutboxPayload(row); err != nil || strings.Contains(row.LastError, "decode") {
			t.Fatalf("the release before refused the queued %s row %d: %v %s", kind, id, err, row.LastError)
		}
		t.Logf("the release before ran the queued %s row %d, %d attempts so far: %s", kind, id, row.Attempts, row.LastError)
	}

	state, body := settled(t, r.d, "hold "+n+" by its ancestry", func(s api.State) bool {
		return slices.ContainsFunc(s.Admission.Held, func(held api.HeldIssue) bool { return held.Issue == n && held.Reason == "ancestry" })
	})
	for issue, tree := range map[string]string{a2: a, u: a, l1: l1, f: f} {
		if got := state.Issues[issue].Tree; got != tree {
			t.Fatalf("the release before puts %s in the tree %s, want %s\nstate %s", issue, got, tree, body)
		}
	}

	setDispatchStatus(t, world.DispatchURL, d, "done")
	testwait.Eventually(t, "the release before to clean "+d+"'s tree up", func() bool { return slices.Contains(treeCleanups(r.rt), d) })
	state, body = r.state()
	for _, tree := range treeCleanups(r.rt) {
		for issue, label := range world.Labels {
			if label == tree && len(placed(state, issue)) > 0 {
				t.Fatalf("the release before cleaned up the tree %s, which labels the pod of %s, an issue at work\nstate %s", tree, issue, body)
			}
		}
	}
	t.Logf("the release before cleaned up the trees %v and no pod of an issue at work", treeCleanups(r.rt))
}
