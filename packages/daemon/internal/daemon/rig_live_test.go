//go:build e2e

// The rig of this change's checks w1 to w5: the whole daemon, as the daemon's other live proofs
// boot it, among the real things production runs around it. RIG.md beside this file is how a proof
// uses it; this file is the rig itself.
//
//   - NATS: the real nats-server, with production's grants (rig_nats_live_test.go).
//   - Envoy: the listener release production pins, built from its own commit
//     (testenvoy.PinnedBinary), or, for a check that sets testenvoy.ListenerFromCheckoutEnv (w2),
//     the listener of the checkout's own commit (testenvoy.RigBinary), with production's 5-minute
//     session age, its own NATS user, and a front every pane and the daemon reach it through
//     (rigFront, rig_runtime_live_test.go).
//   - The daemon: run, the function `legion start` calls, over a PostgreSQL of the run's own
//     (takeOwnDatabase), its own NATS user, and the rig's runtime (rigRuntime), each worker the
//     real `legion worker-shim` around a real Oh My Pi with this head's two plugins.
//   - The model: the gateway the operator names (rigModelBaseEnv and the key's), or, for a check
//     whose claims must each do what it names, a scripted stand-in (rigOptions.standin).
//   - Dispatch: the stand-in the other proofs use, and, where a check needs a workflow issue, a
//     real scratch Dispatch publishing on the rig's NATS as its own user (rigOptions.dispatch).
package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/appauth"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/fixtureenv"
	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/notify"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/stream"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/testenvoy"
	"github.com/sjawhar/legion/daemon/internal/treelifecycle"
)

// The model every rig pane's turns go to, unless the rig runs a stand-in: the base URL of an
// `anthropic-messages` endpoint (an issue pod's ANTHROPIC_BASE_URL when unset), the key as an Oh
// My Pi `!command` that prints it or as a file holding it, and the model id.
const (
	rigModelBaseEnv       = "LEGION_E2E_MODEL_BASE_URL"
	rigModelKeyCommandEnv = "LEGION_E2E_MODEL_KEY_COMMAND"
	rigModelKeyFileEnv    = "LEGION_E2E_MODEL_KEY_FILE"
	rigModelIDEnv         = "LEGION_E2E_MODEL"
	// rigModelDefault is the model a run names none of, which the shared gateway serves.
	rigModelDefault = "claude-sonnet-5-5"
	// rigModelProvider is the provider name the run's models.yml gives the endpoint.
	rigModelProvider = "rig-model"
	// rigSessionAge is the session bucket's age production's listener runs with:
	// session.BucketMaxAge at testenvoy.PinnedRevision, which the deployment repository sets
	// nothing over.
	rigSessionAge = 5 * time.Minute
)

// rigOptions are what a proof asks of its rig.
type rigOptions struct {
	// script is the proof's entry script under scripts/e2e, which a rig missing an input names.
	script string
	// standin answers every pane's turns with a scripted model of the run's own (paneStandin)
	// rather than the gateway, so each claim does what the check names (w5).
	standin bool
	// dispatch starts a scratch Dispatch that publishes its events on the rig's NATS (w5's
	// workflow issue). The daemon's own Dispatch stays the stand-in unless the proof points it
	// there before it boots (cfg.DispatchURL, cfg.DispatchTokenFile).
	dispatch bool
}

// rig is one run of the rig.
type rig struct {
	liveAgentProof
	opts     rigOptions
	built    *built
	runtime  *rigRuntime
	relay    *rigRelay
	nats     *rigNATS
	listener *testenvoy.Listener
	front    *rigFront
	scratch  *scratchDispatch
	standin  *paneStandin
	model    string
	home     string
	profile  string
	evidence string
	logger   *slog.Logger
	// failedAt is the assertion the running step failed at (fails), which step prints.
	failedAt string
	// root is the top-level test, whose life the daemon's and its held port's are: a daemon a
	// step restarts must outlive that step, which r.t is while it runs.
	root *testing.T
}

// startRig builds and starts everything but the daemon: the proof seeds what the daemon holds at
// its boot (seed) and then boots it (boot). An input the rig needs that is missing skips the test,
// naming it and the script that sets it, as every live proof of this package skips.
func startRig(t *testing.T, opts rigOptions) *rig {
	t.Helper()
	fixtureenv.Live(t)
	omp := os.Getenv(liveOmpPathEnv)
	if omp == "" {
		t.Skipf("%s is unset, so there is no Oh My Pi for a pane to run; %s requires it", liveOmpPathEnv, opts.script)
	}
	grantsFile, natsBinary := os.Getenv(rigGrantsEnv), os.Getenv(rigNATSBinEnv)
	if grantsFile == "" || natsBinary == "" {
		t.Skipf("%s or %s is unset: %s (through scripts/e2e/lib/live-rig.sh) reads the grants at the pinned listener and installs the nats-server", rigGrantsEnv, rigNATSBinEnv, opts.script)
	}
	listener := testenvoy.RigBinary(t)
	r := &rig{opts: opts, built: &built{}, root: t}
	r.evidence = t.TempDir()
	if daemonLog := os.Getenv(liveDaemonLogEnv); daemonLog != "" {
		r.evidence = filepath.Dir(daemonLog)
	}
	model := r.modelInputs(t)

	takeOwnDatabase(t)
	grants := readRigGrants(t, grantsFile)
	if grants.Revision != listener.Revision {
		t.Fatalf("the rig's grants were read at %s and its listener was built from %s: both come from one tree", grants.Revision, listener.Revision)
	}
	r.nats = startRigNATS(t, natsBinary, grants, r.evidence)
	r.listener = testenvoy.Start(t, listener.Path, r.nats.url, testenvoy.Options{
		MachineID: grants.Machine, NATSSeedFile: r.nats.listenerSeedFile, SessionAge: rigSessionAge,
	})
	r.front = newRigFront(t, r.listener.BaseURL())

	cfg := workflowConfig(t, r.nats.url)
	// testConfig leaves review_round_cap at zero, which no legion.yaml can hold (config.Load refuses
	// it, and its default is 3), and the plugins' contract refuses the state route's settings that
	// carry it: every `read_state` and `read_record` a rig pane made failed on it.
	cfg.ReviewRoundCap = 3
	// The daemon's API port is reserved for the run, as the NATS ports and the listener's are:
	// restartDaemon and w1's outages start the daemon again, after it closed its port, on the port
	// every pane was told (testenvoy.ReservePort). holdPortAgain holds it for the first boot; the
	// port testConfig held is left held, unused.
	cfg.Port = testenvoy.ReservePort(t)
	cfg.DaemonURL = "http://127.0.0.1:" + strconv.Itoa(cfg.Port)
	r.liveAgentProof = liveAgentProof{liveProof: liveProof{
		t: t, cfg: cfg, board: newLiveDispatch(t, cfg.Project), records: record.NewStore(), js: r.nats.js,
	}, omp: omp}
	r.holdPortAgain()
	r.cfg.DispatchURL = r.board.url
	r.cfg.EnvoyURL = r.front.url
	r.cfg.EnvoyTokenFile = r.listener.TokenFile(t)
	r.cfg.NatsDaemonNkeySeedFile = r.nats.daemonSeedFile
	var err error
	if r.project, err = claim.ProjectToken(r.cfg.Project); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if r.st, err = store.Open(ctx, r.cfg.PostgresDSN); err != nil {
		t.Fatalf("open the store: %v", err)
	}
	t.Cleanup(r.st.Close)
	if _, err := r.st.Migrate(ctx); err != nil {
		t.Fatalf("migrate the store: %v", err)
	}
	root := proofCheckoutRoot(t)
	if opts.dispatch {
		// The driver runs from a temporary directory, so the checkout is named rather than found
		// from the working directory.
		r.scratch = startScratchDispatchOn(t, scratchDispatchOptions{natsURL: r.nats.dispatchURL, root: root})
	}

	work := t.TempDir()
	r.legion = buildProofLegion(t, root, work)
	r.home = filepath.Join(work, "omp-home")
	r.profile = "legion-rig-" + strings.ToLower(r.cfg.Project)
	installProofPlugins(t, root, work, r.home, r.profile, omp)
	r.writeProfile(model)
	// Every pane of the profile opens the profile's agent.db at its start, and two that make it at
	// once race on its tables ("table settings already exists"), so one of the first launches died
	// and was charged a launch failure. `omp config list` makes it, once, before any pane starts.
	initialise := exec.Command(omp, "config", "list")
	initialise.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + r.home, "OMP_PROFILE=" + r.profile, "LANG=C.UTF-8"}
	if out, err := initialise.CombinedOutput(); err != nil {
		t.Fatalf("initialise the profile %s's agent.db: %v\n%s", r.profile, err, out)
	}
	overlay := filepath.Join(work, "rig-model.yml")
	writeRoleOverlay(t, overlay, r.model)

	r.relay = newRigRelay(t)
	local := newLocalRuntime(t, r.cfg.StateDir, r.legion, omp, r.home, r.profile)
	local.daemonURL, local.envoyURL, local.natsURLs = r.cfg.DaemonURL, r.front.url, r.cfg.NatsURLs
	if r.scratch != nil {
		// Panes reach the rig's own Dispatch where the rig runs one, as a pod's roles reach the
		// deployment's (DISPATCH_URL, DISPATCH_TOKEN_FILE); the stand-in the daemon reads otherwise
		// serves the daemon's own reads alone.
		local.dispatchURL, local.dispatchTokenFile = r.scratch.base, filepath.Join(work, "dispatch-token")
		writeProofFile(t, local.dispatchTokenFile, r.scratch.token+"\n")
	}
	local.overlays = func(claim.Token) []string { return []string{overlay} }
	r.runtime = newRigRuntime(t, local, r.relay)
	r.rt = local
	r.logger = r.log()
	r.daemon = newRecordingLog(r.logger)

	health := r.listenerHealth()
	r.note("the listener: %s, built from revision %s, at %s: %s, session_max_age %v, its NATS user holding core.ListenerHostPermissions(%s) of %s",
		listener.Source, listener.Revision, r.listener.BaseURL(),
		health["status"], health["session_max_age"], grants.Machine, grants.Revision)
	r.note("nats-server on %s, /connz on 127.0.0.1:%d, sessions admitted with no credential as members (core.SessionPermissions)",
		r.nats.url, r.nats.monitorPort)
	r.note("every pane's turns go to %s", r.model)
	return r
}

// modelInputs reads what the panes' model is, and skips a gateway run that names no endpoint or
// no key.
func (r *rig) modelInputs(t *testing.T) rigModelInputs {
	t.Helper()
	if r.opts.standin {
		return rigModelInputs{}
	}
	in := rigModelInputs{base: os.Getenv(rigModelBaseEnv), command: os.Getenv(rigModelKeyCommandEnv), file: os.Getenv(rigModelKeyFileEnv), id: os.Getenv(rigModelIDEnv)}
	if in.base == "" {
		in.base = os.Getenv("ANTHROPIC_BASE_URL")
	}
	if in.id == "" {
		in.id = rigModelDefault
	}
	if in.base == "" || (in.command == "" && in.file == "") {
		t.Skipf("the panes' model is not named: set %s (or ANTHROPIC_BASE_URL) and %s or %s; %s says how",
			rigModelBaseEnv, rigModelKeyCommandEnv, rigModelKeyFileEnv, r.opts.script)
	}
	return in
}

// rigModelInputs is the gateway a run names, its key as a command or a file, and the model.
type rigModelInputs struct{ base, command, file, id string }

// writeProfile writes the profile every pane runs: the one provider its turns go to and the
// settings a pane needs to run unattended.
func (r *rig) writeProfile(in rigModelInputs) {
	t := r.t
	t.Helper()
	agent := filepath.Join(r.home, ".omp", "profiles", r.profile, "agent")
	if err := os.MkdirAll(agent, 0o700); err != nil {
		t.Fatalf("make the profile's agent directory %s: %v", agent, err)
	}
	var provider string
	if r.opts.standin {
		r.standin = newPaneStandin(t, rigModelProvider)
		r.model = standinModel(rigModelProvider)
		provider = fmt.Sprintf("  %s:\n    baseUrl: %s\n    api: anthropic-messages\n    apiKey: a-literal-key-no-endpoint-checks\n"+
			"    models:\n      - id: %s\n        name: %s\n", rigModelProvider, r.standin.url, rigModelProvider+standinModelID, rigModelProvider)
	} else {
		key := "!" + in.command
		if in.command == "" {
			raw, err := os.ReadFile(in.file)
			if err != nil {
				t.Fatalf("read %s: %v", rigModelKeyFileEnv, err)
			}
			key = strings.TrimSpace(string(raw))
		}
		r.model = rigModelProvider + "/" + in.id
		// The gateway passes Anthropic's signatures through and signs nothing itself, so earlier
		// thinking goes as text, as a development host's own gateway provider sets it.
		provider = fmt.Sprintf("  %s:\n    baseUrl: %s\n    api: anthropic-messages\n    apiKey: %q\n    authHeader: true\n"+
			"    compat:\n      replayUnsignedThinking: false\n    models:\n      - id: %s\n        name: %s\n",
			rigModelProvider, in.base, key, in.id, in.id)
	}
	writeProofFile(t, filepath.Join(agent, "models.yml"),
		"# Written by packages/daemon/internal/daemon/rig_live_test.go.\nproviders:\n"+provider)
	writeProofFile(t, filepath.Join(agent, "config.yml"),
		"# Written by packages/daemon/internal/daemon/rig_live_test.go.\nenabledModels:\n  - "+r.model+"\n"+
			// A fresh profile opens Oh My Pi's setup wizard, and a pane waiting on it never starts.
			"startup:\n  setupWizard: false\n")
}

// listenerHealth is the listener's /healthz, read directly.
func (r *rig) listenerHealth() map[string]any {
	r.t.Helper()
	response, err := http.Get(r.listener.BaseURL() + "/healthz")
	if err != nil {
		r.t.Fatalf("read the listener's /healthz: %v", err)
	}
	defer response.Body.Close()
	var health map[string]any
	if err := json.NewDecoder(response.Body).Decode(&health); err != nil {
		r.t.Fatalf("decode the listener's /healthz: %v", err)
	}
	return health
}

// ---- what the daemon holds at its boot ----------------------------------------------------------

// rigSeed is one issue a proof seeds, with the worker of its phase mid-launch and the task that
// phase's start left pending on its claim: the state admission's repair of a slotted worker
// passes over (provider_error_retry_live_test.go's seed says why).
type rigSeed struct {
	issue string
	phase phase.Phase
	role  claim.Role
	task  string
}

// seed records issues as the daemon holds them at its boot, before it boots.
func (r *rig) seed(seeds ...rigSeed) {
	t := r.t
	t.Helper()
	ctx := context.Background()
	keys := make([]string, 0, len(seeds))
	for _, s := range seeds {
		r.board.show(s.issue, "in_progress")
		keys = append(keys, s.issue)
	}
	if err := r.st.Tx(ctx, func(tx pgx.Tx) error {
		for _, s := range seeds {
			if err := r.records.PutIssue(ctx, tx, record.Issue{
				Key: s.issue, Repo: liveRepo, DefaultBranch: "main", GitHubRepo: liveRepo, Tree: s.issue,
				Project: r.cfg.Project, Title: "the rig's issue " + s.issue, Phase: s.phase, Generation: 1, Status: "in_progress",
				DispatchStatus: "in_progress", Rank: "U", LastDispatchSeq: 1, HandedOver: true,
			}); err != nil {
				return err
			}
		}
		return r.admit(ctx, tx, keys...)
	}); err != nil {
		t.Fatalf("seed the issues: %v", err)
	}
	for _, s := range seeds {
		if _, err := r.st.OpenTreeLifecycle(ctx, r.project, s.issue, treelifecycle.AuthorityWorkflow); err != nil {
			t.Fatalf("open the lifecycle of %s: %v", s.issue, err)
		}
		token := r.token(s.issue, s.role)
		if _, err := r.st.AdmitClaim(ctx, supervise.Claim{
			Token: token, Project: r.project, Tree: s.issue, Issue: s.issue, Role: s.role,
			Generation: 1, State: supervise.StateLaunching,
			BootTokenHash: supervise.HashBootToken("interrupted-" + randomSuffix(t)), ServingGeneration: 1,
		}); err != nil {
			t.Fatalf("record the unfinished launch of %s: %v", token, err)
		}
		if err := r.st.PutDelivery(ctx, token, supervise.Delivery{
			ID: "seeded-start-" + randomSuffix(t), Task: s.task, Generation: 1, Phase: s.phase, QueuedAt: time.Now(),
		}); err != nil {
			t.Fatalf("record the task %s's start left pending: %v", token, err)
		}
		writePrompt(t, r.cfg, token)
		r.note("seeded %s (%s, %s), its claim %s mid-launch with its start's task pending", s.issue, s.phase, s.role, token)
	}
}

// token is the claim token of role on key, as the daemon spells it.
func (r *rig) token(key string, role claim.Role) claim.Token {
	r.t.Helper()
	token, err := claim.NewToken(r.project, key, role)
	if err != nil {
		r.t.Fatal(err)
	}
	return token
}

// ---- the daemon ---------------------------------------------------------------------------------

// boot starts the daemon over the rig, on the API port every pane was told. The daemon is the
// top-level test's, so one a step restarts runs on into the next step.
func (r *rig) boot() {
	r.t.Helper()
	r.d = startDaemonWithin(r.root, r.cfg, r.overrides(), slog.New(r.daemon), 2*time.Minute)
}

// restartDaemon stops the daemon and starts it again on the same API port and worker-stream
// socket, which every running pane was told; the runtime, its panes and the relay stay, as a
// daemon restart leaves a pod's processes. It answers when the old daemon stopped and when the
// new one answered /healthz.
func (r *rig) restartDaemon() (stopped, serving time.Time) {
	r.t.Helper()
	r.d.stop()
	stopped = time.Now()
	r.holdPortAgain()
	r.boot()
	return stopped, time.Now()
}

// holdPortAgain holds the daemon's API port again for its next boot (heldListen): a stopped
// daemon has closed it, and startRig's reservation keeps it from every other socket until then. It
// still waits up to a minute for a bind that fails, which a step's own hold of the port (w1's
// closed-port outage) can make until that hold ends.
func (r *rig) holdPortAgain() {
	r.t.Helper()
	address := net.JoinHostPort("127.0.0.1", strconv.Itoa(r.cfg.Port))
	deadline := time.Now().Add(time.Minute)
	for {
		listener, err := net.Listen("tcp", address)
		if err == nil {
			heldPorts.Lock()
			heldPorts.byAddress[address] = listener
			heldPorts.Unlock()
			r.root.Cleanup(func() {
				heldPorts.Lock()
				delete(heldPorts.byAddress, address)
				heldPorts.Unlock()
				_ = listener.Close()
			})
			return
		}
		if time.Now().After(deadline) {
			r.t.Fatalf("hold the daemon's port %s again: %v", address, err)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// overrides is the daemon the rig boots: the rig's runtime over the real worker stream, whose
// address the relay carries every shim to, the API listener the fixture holds, and the
// workflow's App tokens stood in for.
func (r *rig) overrides() overrides {
	return overrides{
		runtime: func(_ context.Context, listener *stream.Listener, address string, _ appauth.Tokens,
			_ func(context.Context, string) (ghrepo.Repository, string, error), _ *store.Store, _ func() []ghrepo.Repository) (runtime.Runtime, error) {
			r.built.mu.Lock()
			defer r.built.mu.Unlock()
			r.built.conns, r.built.address = listener, address
			r.runtime.conns, r.runtime.stream = listener, address
			r.relay.setUpstream(address)
			return r.runtime, nil
		},
		listen:         heldListen,
		workflowTokens: &workflowTokenRecorder{},
	}
}

// conn is the daemon's own connection to token's shim, as its worker stream holds it now.
func (r *rig) conn(token claim.Token) (runtime.Conn, bool) {
	r.built.mu.Lock()
	conns := r.built.conns
	r.built.mu.Unlock()
	if conns == nil {
		return nil, false
	}
	return conns.Conn(token)
}

// registerController registers a controller session for the project, as `legion controller
// start` registers the operator's own: the daemon queues no controller notice while none is
// registered (admit's wakeController). No controller process runs; a check reads what the daemon
// published on the controller's topic (controllerNotices).
func (r *rig) registerController() string {
	t := r.t
	t.Helper()
	status, body := r.d.request(http.MethodPost, "/legion/v1/controller/secret",
		api.ControllerSecretRequest{PluginContract: api.DaemonAPIVersion}, true)
	if status != http.StatusOK {
		t.Fatalf("the controller's boot secret = %d %s, want 200", status, body)
	}
	var capability api.ControllerSecretResponse
	if err := json.Unmarshal(body, &capability); err != nil {
		t.Fatalf("decode the controller's boot secret %s: %v", body, err)
	}
	session := "ses_controller_" + randomSuffix(t)
	if status, body := r.d.request(http.MethodPost, "/legion/v1/claims/register", claim.RegisterRequest{
		BootToken: capability.Secret, SessionID: session, OmpSessionFile: "/sessions/" + session + ".jsonl",
		AgentID: session, PluginContract: api.DaemonAPIVersion,
	}, false); status != http.StatusOK {
		t.Fatalf("register the controller = %d %s, want 200", status, body)
	}
	r.note("the project's controller is registered as %s, so a notice for it is published on %s", session, notify.ControllerTopic(r.project))
	return session
}

// ---- what the daemon told -----------------------------------------------------------------------

// rigNotice is one notice the daemon published through the listener, as the stream stored it:
// when, its kind, the claim it names and its reason.
type rigNotice struct {
	at     time.Time
	seq    uint64
	kind   string
	claim  string
	reason string
}

// controllerNotices is every notice of kind the stream holds on the project's controller topic,
// oldest first.
func (r *rig) controllerNotices(kind string) []rigNotice {
	r.t.Helper()
	var notices []rigNotice
	for _, stored := range r.nats.storedOn(notify.ControllerTopic(r.project)) {
		notice, ok := noticeIn(stored.data)
		if !ok || notice.kind != kind {
			continue
		}
		notice.at, notice.seq = stored.at, stored.seq
		notices = append(notices, notice)
	}
	return notices
}

// noticeIn finds the daemon's notice in a stored envelope: the first object, at any depth and
// inside any field that holds JSON text, that carries a `kind`. The envelope is the listener's,
// and the payload the daemon's (record.Notice), which this reads by its wire names alone.
func noticeIn(data []byte) (rigNotice, bool) {
	var value any
	if err := json.Unmarshal(data, &value); err != nil {
		return rigNotice{}, false
	}
	var find func(v any) (map[string]any, bool)
	find = func(v any) (map[string]any, bool) {
		switch v := v.(type) {
		case map[string]any:
			if kind, ok := v["kind"].(string); ok && kind != "" {
				return v, true
			}
			for _, inner := range v {
				if found, ok := find(inner); ok {
					return found, true
				}
			}
		case []any:
			for _, inner := range v {
				if found, ok := find(inner); ok {
					return found, true
				}
			}
		case string:
			if strings.HasPrefix(strings.TrimSpace(v), "{") {
				var inner any
				if json.Unmarshal([]byte(v), &inner) == nil {
					return find(inner)
				}
			}
		}
		return nil, false
	}
	found, ok := find(value)
	if !ok {
		return rigNotice{}, false
	}
	text := func(key string) string {
		s, _ := found[key].(string)
		return s
	}
	return rigNotice{kind: text("kind"), claim: text("claim"), reason: text("reason")}, true
}

// ---- how a step runs and fails ------------------------------------------------------------------

// step runs one step as a subtest of its own, between its `== name` and `ok name (<s>s)` lines,
// with the rig's reads failing that subtest rather than the whole run. A step that fails prints
// `FAIL name at <assertion> (<s>s)`, naming the assertion it failed at (fails), which a script's
// negative control reads back; the next step runs.
func (r *rig) step(name string, body func()) {
	parent := r.t
	parent.Run(name, func(t *testing.T) {
		r.t, r.failedAt = t, ""
		r.runtime.t = t
		started := time.Now()
		defer func() {
			r.t = parent
			r.runtime.t = parent
			if t.Failed() {
				at := r.failedAt
				if at == "" {
					at = "none of its named assertions (a wait or a read of its own)"
				}
				fmt.Printf("FAIL %s at %s (%ds)\n", name, at, int(time.Since(started).Seconds()))
			}
		}()
		fmt.Printf("== %s\n", name)
		body()
		fmt.Printf("ok %s (%ds)\n", name, int(time.Since(started).Seconds()))
	})
}

// fails ends the running step at its named assertion.
func (r *rig) fails(assertion, format string, args ...any) {
	r.t.Helper()
	r.failedAt = assertion
	r.t.Fatalf(assertion+": "+format, args...)
}

// clockAt is a moment as the run's records print it.
func clockAt(at time.Time) string {
	if at.IsZero() {
		return "never"
	}
	return at.UTC().Format("15:04:05.000Z")
}
