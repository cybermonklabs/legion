package daemon

// The whole-daemon rig of a project pause's checks k1 to k4 (dispatch://INFRA-3759, plan task t10):
// one daemon booted through run, on a database of its own, with the workflow newIdleWakeRig boots
// (idle_rig_test.go) — a Dispatch stand-in that lists the rig's issues, NATS from internal/testnats
// with the notification stream the boot's connect requires, the workflow's App tokens and a GitHub
// stand-in — under `runtime: kubernetes` with the idle release on, the wake pacing at eight a
// minute in bursts of one, and the daemon launching its own controller. Its runtime is the fake
// one, grown the issue-pod calls the idle release makes (pauseRuntime), and one waveClock is every
// clock the daemon reads: the machines', the workflow engine's, the idle and wake passes', the
// stuck warning's and the state's (overrides.clock).
//
// The test reaches the machines and the workflow's handlers through the one seam run hands them
// over (overrides.supervising): turns, readiness and a provider's refusal are the machines' own
// events, posted as internal/supervise's tests post them, and the listener's wakes, the CI
// settlements and the handoffs are the facts the daemon's consumers and its API apply, applied
// with the daemon's own handlers. The pause and the resume are the operator's, through the
// daemon's own routes and so through its pause service.

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/notify"
	"github.com/sjawhar/legion/daemon/internal/pause"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/runtime/fake"
	"github.com/sjawhar/legion/daemon/internal/shimwire"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/testwait"
)

// The rig's idle release (`runtime.kubernetes.idle_release`) and its pacing. The idle period is
// the one the checks advance the clock past; the backstop is longer than any test advances its
// clock, so the only backstop that comes due is one a test arms itself (k1's F). The tick is how
// often the daemon's idle and wake passes run on the wall clock, each reading the test clock.
//
// The outbox sweep is how often the daemon's outbox sweeps its lanes on the wall clock
// (overrides.outboxSweep): the checks let a lease they put on a row lapse, or make a held row due,
// behind the daemon, and the row runs at the next sweep, which at the daemon's own outboxLease is
// up to 30 s of each such check; a second keeps a check to its own work.
const (
	pauseRigIdleMinutes     = 10
	pauseRigBackstopMinutes = 120
	pauseRigTick            = 100 * time.Millisecond
	pauseRigPodUpdateGrace  = 15 * time.Minute
	pauseRigOutboxSweep     = time.Second
)

// The three lines of a pause the checks read (decision 23 of the plan): the pause service's line
// of the pause and its line for each held row a resume released, and the machine's line for each
// role it stopped.
const (
	pausePausedLine   = "pause: the project is paused"
	pauseStoppedLine  = supervise.PauseRoleStoppedMessage
	pauseReleasedLine = pause.HeldRowReleasedMessage
)

// pauseRigReason is the reason the rig's operator gives for every pause.
const pauseRigReason = "the model provider is refusing every request"

// pauseRuntime is the fake runtime with the moment of every launch on the test clock, as
// waveRuntime has it (wake_wave_test.go), and the calls an issue pod's idle release makes of a
// Kubernetes runtime, as releaseRuntime has them (idle_release_test.go): whether a pod runs the
// configured image (always), whether the runtime still holds an issue's Sandbox, the read of a
// pod's workspace (the embedded fake's WorkspaceWork), and the issue-level teardowns, a release's
// (ReleaseIssue) and a close's (SuspendIssue), whose every deletion it records. The release's
// executors ask for the release's teardown (runtime.IssueReleaser) and check nothing without it,
// so a rig without ReleaseIssue would read no release and no check, whatever the pause held.
// IssuePod and IssueKeepsVolume are the embedded fake's.
type pauseRuntime struct {
	*fake.Runtime
	clock *waveClock
	mu    sync.Mutex
	runs  []waveLaunch
	// sandboxes is every issue whose Sandbox (and pod) the runtime holds.
	sandboxes map[string]bool
	// deleted is every issue whose pod a teardown deleted, in order. The workspace reads a release
	// asked for are the embedded fake's (WorkspaceWork), which answers no work.
	deleted []string
}

func newPauseRuntime(clock *waveClock) *pauseRuntime {
	rt := &pauseRuntime{Runtime: fake.NewRuntime(), clock: clock, sandboxes: map[string]bool{}}
	// A pods runtime provisions each claim's workspace itself, so the daemon clones and fetches
	// nothing for a launch, as the Sandbox runtime does.
	rt.InPod = true
	rt.Now = clock.Now
	return rt
}

// Spawn and Resume record the launch once the embedded fake has answered, so the fake's own
// record of the call, which lastLaunch reads for the launch's boot token, is in place before a test
// that waits on launchesOf sees the launch. Recorded first, a test could read the claim's
// previous launch from the fake and dial its shim with that launch's boot token, which the daemon
// refuses as a stale worker generation. The launch's moment is the clock's at the ask.
func (r *pauseRuntime) Spawn(ctx context.Context, spec runtime.SpawnSpec) (runtime.Locator, error) {
	at := r.clock.Now()
	loc, err := r.Runtime.Spawn(ctx, spec)
	r.record(spec.Claim, at, false)
	return loc, err
}

func (r *pauseRuntime) Resume(ctx context.Context, prev *runtime.Locator, spec runtime.SpawnSpec) (runtime.Locator, error) {
	at := r.clock.Now()
	loc, err := r.Runtime.Resume(ctx, prev, spec)
	r.record(spec.Claim, at, true)
	return loc, err
}

func (r *pauseRuntime) record(token claim.Token, at time.Time, resumed bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.runs = append(r.runs, waveLaunch{claim: token, at: at, resumed: resumed})
}

// launches is every launch so far, in the order the runtime answered them.
func (r *pauseRuntime) launches() []waveLaunch {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.runs)
}

// launchesOf is every launch of token so far.
func (r *pauseRuntime) launchesOf(token claim.Token) []waveLaunch {
	var of []waveLaunch
	for _, launch := range r.launches() {
		if launch.claim == token {
			of = append(of, launch)
		}
	}
	return of
}

// The pause rig counts a launch only once the fake runtime holds its call, for a spawn and for a
// resume alike: a test that waits on launchesOf and then reads the launch's boot token from the
// fake (lastLaunch) reads that launch, never the claim's previous one.
// TestTheResumeLaunchesAnOperatorsSpawnThePauseStoppedOwingALaunch dials the relaunched spawn's
// shim with that token; with the previous launch's token the daemon refused the hello as a stale
// worker generation.
func TestThePauseRigCountsALaunchOnceTheRuntimeHoldsItsCall(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	rt := newPauseRuntime(newWaveClock())
	const token = claim.Token("legion-OTHERPROJECT-7-architect")
	for _, method := range []string{"Spawn", "Resume"} {
		before := len(rt.launchesOf(token))
		counted := -1
		inside := func(context.Context) error {
			counted = len(rt.launchesOf(token))
			return nil
		}
		rt.OnSpawn, rt.OnResume = inside, inside
		spec := runtime.SpawnSpec{Claim: token, BootToken: "boot-of-the-" + method}
		var err error
		if method == "Spawn" {
			_, err = rt.Spawn(ctx, spec)
		} else {
			_, err = rt.Resume(ctx, nil, spec)
		}
		if err != nil {
			t.Fatalf("the %s: %v", method, err)
		}
		if counted != before {
			t.Fatalf("while the runtime still ran the %s, the rig counted %d launches of %s, want the %d before it: a test that waits on the count reads the claim's previous launch from the runtime",
				method, counted, token, before)
		}
		if got := len(rt.launchesOf(token)); got != before+1 {
			t.Fatalf("after the %s the rig counts %d launches of %s, want %d", method, got, token, before+1)
		}
		if got := lastLaunch(t, rt.Runtime, token).BootToken; got != spec.BootToken {
			t.Fatalf("after the %s the runtime's latest launch of %s carries boot token %q, want %q", method, token, got, spec.BootToken)
		}
	}
}

// holdPod is the issue's pod standing, as a launch of a role of it leaves it.
func (r *pauseRuntime) holdPod(issue string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.sandboxes[issue] = true
}

func (r *pauseRuntime) IssuePodRunsConfiguredImage(string) bool { return true }

func (r *pauseRuntime) IssueSandbox(issue string) (string, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return "sandbox-" + strings.ToLower(issue), r.sandboxes[issue]
}

func (r *pauseRuntime) ReleaseIssue(ctx context.Context, issue, _ string, authorize func(context.Context) (bool, error),
	writing func(context.Context) error) (bool, error) {
	return r.teardown(ctx, issue, authorize, writing)
}

func (r *pauseRuntime) SuspendIssue(ctx context.Context, issue, _ string, authorize func(context.Context) (bool, error)) error {
	_, err := r.teardown(ctx, issue, authorize, nil)
	return err
}

// teardown is both teardowns: once the store authorizes it, the issue's pod is deleted and its
// Sandbox is gone. It answers that it released a Sandbox whenever the store authorizes it: a
// launch of this rig stands its pod up again without marking it (holdPod is the seed's), so an
// authorized teardown always has a pod to take, and calls writing before it takes it.
func (r *pauseRuntime) teardown(ctx context.Context, issue string, authorize func(context.Context) (bool, error),
	writing func(context.Context) error) (bool, error) {
	act, err := authorize(ctx)
	if err != nil || !act {
		return false, err
	}
	if writing != nil {
		if err := writing(ctx); err != nil {
			return false, err
		}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.deleted = append(r.deleted, issue)
	r.sandboxes[issue] = false
	return true, nil
}

// podReads is what the runtime was asked to do to the pods: the issues whose pod a teardown
// deleted and the issues whose workspace a release read.
func (r *pauseRuntime) podReads() (deleted, read []string) {
	for _, call := range r.CallsOf("WorkspaceWork") {
		read = append(read, call.Issue)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.deleted), read
}

// suspendedOf is how many times the runtime was asked to stop a process of token.
func (r *pauseRuntime) suspendedOf(token claim.Token) int {
	count := 0
	for _, call := range r.CallsOf("Suspend") {
		if call.Locator.Claim == token {
			count++
		}
	}
	return count
}

// pauseEnvoy is the Envoy listener's publish route as the daemon's outbox meets it. A role topic
// no session holds is refused 404 `holder_lapsed` with the holder `unresponsive`, which is what a
// listener before cybermonklabs/legion#4088 answered a role a release or a pause stopped: the
// daemon keeps the message and tries again (notify.ErrHolderUnresponsive). The production listener
// has answered such a role's session gone since #4088, which a test asks of a role's topic with
// gone. Every other publish is taken, each recorded with the topic and the moment on the test
// clock. A test says when a role's session holds its topic (hold). The registry read and the clean
// session stop are answered as a listener with no record of the session answers them.
type pauseEnvoy struct {
	clock      *waveClock
	url        string
	mu         sync.Mutex
	held       map[string]bool
	goneTopics map[string]bool
	sent       []pauseEnvoyPublish
}

// pauseEnvoyPublish is one publish the listener took.
type pauseEnvoyPublish struct {
	topic, message string
	at             time.Time
}

func newPauseEnvoy(t *testing.T, clock *waveClock) *pauseEnvoy {
	t.Helper()
	e := &pauseEnvoy{clock: clock, held: map[string]bool{}, goneTopics: map[string]bool{}}
	server := httptest.NewServer(http.HandlerFunc(e.serve))
	t.Cleanup(server.Close)
	e.url = server.URL
	return e
}

func (e *pauseEnvoy) serve(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	switch {
	case r.Method == http.MethodPost && r.URL.Path == "/v1/messages/publish":
		var body struct {
			Topic   string `json:"topic"`
			Message string `json:"message"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"error":"bad body"}`)
			return
		}
		e.mu.Lock()
		defer e.mu.Unlock()
		if strings.HasPrefix(body.Topic, notify.RoleTopicPrefix) && !e.held[body.Topic] {
			state := notify.StateUnresponsive
			if e.goneTopics[body.Topic] {
				state = "gone"
			}
			w.WriteHeader(http.StatusNotFound)
			_, _ = fmt.Fprintf(w, `{"error":"the role's holder is not live","reason":"holder_lapsed","holder":"a-stopped-session","holder_state":%q}`, state)
			return
		}
		e.sent = append(e.sent, pauseEnvoyPublish{topic: body.Topic, message: body.Message, at: e.clock.Now()})
		_, _ = io.WriteString(w, `{}`)
	case r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, "/v1/sessions/"):
		_, _ = io.WriteString(w, `{"deleted":true}`)
	default:
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, `{"error":"not found"}`)
	}
}

// hold is token's session holding its role topic, as a role's process does once it is up.
func (e *pauseEnvoy) hold(token claim.Token) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.held[notify.RoleTopicPrefix+string(token)] = true
}

// gone is token's role topic answering, while no session holds it, as the production listener
// answers a role whose session reads gone, which a stopped role's does since
// cybermonklabs/legion#4088: 404 holder_lapsed with holder_state gone, which notify reads as
// notify.ErrNoHolder. A topic not gone answers holder_state unresponsive
// (notify.ErrHolderUnresponsive).
func (e *pauseEnvoy) gone(token claim.Token) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.goneTopics[notify.RoleTopicPrefix+string(token)] = true
}

// publishedTo is every publish the listener took to token's role topic.
func (e *pauseEnvoy) publishedTo(token claim.Token) []pauseEnvoyPublish {
	e.mu.Lock()
	defer e.mu.Unlock()
	var to []pauseEnvoyPublish
	for _, sent := range e.sent {
		if sent.topic == notify.RoleTopicPrefix+string(token) {
			to = append(to, sent)
		}
	}
	return to
}

// pauseDaemonRig is the daemon of the pause's checks and everything it reads.
type pauseDaemonRig struct {
	t       *testing.T
	ctx     context.Context
	cfg     config.Config
	pool    *store.Pool
	st      *store.Store
	records *record.Postgres
	clock   *waveClock
	rt      *pauseRuntime
	envoy   *pauseEnvoy
	board   *liveDispatch
	logs    *syncBuffer
	// project is the project's claim-token spelling; cfg.Project is its Dispatch key.
	project string
	built   *built
	o       overrides
	d       *daemon

	mu    sync.Mutex
	sup   *supervisor
	apply func(ctx context.Context, source, eventID string, fact intake.Fact) (intake.Result, error)
}

// newPauseDaemonRig readies every part of the rig and the configuration its daemon boots with;
// tune changes the configuration before anything reads it. Nothing is booted yet: the test seeds
// its issues and claims first (seed), then boots (boot).
func newPauseDaemonRig(t *testing.T, tune func(*config.Config)) *pauseDaemonRig {
	t.Helper()
	ctx := context.Background()
	pool := isolatedOutboxPool(t)
	cfg := workflowConfig(t, workflowNATS(t))
	cfg.PostgresDSN = pool.Config().ConnString()
	// The rig's board lists the issues it holds: the daemon's boot lingers closed every recorded
	// issue Dispatch's listing does not name (newIdleWakeRig).
	board := newLiveDispatch(t, cfg.Project)
	board.lists = true
	cfg.DispatchURL = board.url
	// Boot reads runtime.kubernetes' kubeconfig whatever runtime it is given (readSandbox), so the
	// rig names an API address, which nothing dials: the runtime the boot is given is the rig's
	// own.
	cfg.Runtime = config.Runtime{Name: "kubernetes", Kubernetes: &config.Kubernetes{
		Namespace: "legion", Image: idleRigImage, StorageClass: "standard", IssueVolume: "1Gi",
		Kubeconfig: writeKubeconfig(t, "https://127.0.0.1:1", "test"), Resources: config.DefaultResources(),
		RepositoryCache: config.DefaultRepositoryCache(),
		IdleRelease:     config.IdleReleaseConfig{IdleMinutes: pauseRigIdleMinutes, BackstopMinutes: pauseRigBackstopMinutes},
	}}
	cfg.WorkerWakesPerMinute, cfg.WorkerWakeBurst = 8, 1
	cfg.ControllerLaunch = config.ControllerLaunchDaemon
	// No boot or probe timer comes due while a test drives the clock: a launch the rig makes waits
	// for its agent for as long as the test reads it, as waveConfig has it.
	cfg.WorkerBootTimeout = time.Hour
	cfg.ProbeInterval = time.Hour
	cfg.PodUpdateGrace = pauseRigPodUpdateGrace
	// An admission pass every second, besides the passes facts run (the pass rule, admit's
	// Admission.Apply), so a pass the pause did not stop would admit within the test's reads.
	cfg.ControllerWakeInterval = time.Second
	clock := newWaveClock()
	envoy := newPauseEnvoy(t, clock)
	cfg.EnvoyURL = envoy.url
	if tune != nil {
		tune(&cfg)
	}
	project, err := claim.ProjectToken(cfg.Project)
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(ctx, cfg.PostgresDSN)
	if err != nil {
		t.Fatalf("open the rig's store: %v", err)
	}
	t.Cleanup(st.Close)
	r := &pauseDaemonRig{t: t, ctx: ctx, cfg: cfg, pool: pool, st: st, records: record.NewStore(), clock: clock,
		rt: newPauseRuntime(clock), envoy: envoy, board: board, logs: &syncBuffer{}, project: project, built: &built{}}
	r.o = fakeRuntime(r.rt, r.built)
	r.o.clock = clock
	r.o.idleTick = pauseRigTick
	r.o.orphanSweep = time.Hour
	r.o.outboxSweep = pauseRigOutboxSweep
	r.o.controllerFirstWake = time.Second
	r.o.workflowTokens = idleRigAppTokens{workflowTokenRecorder: &workflowTokenRecorder{}}
	// GitHub answers each repository at main and an issue branch's create (startedAgainGitHub): the
	// first start of an issue the admission line admits waits for its branch.
	r.o.githubAPI = startedAgainGitHub(t)
	r.o.supervising = func(sup *supervisor, apply func(context.Context, string, string, intake.Fact) (intake.Result, error)) {
		r.mu.Lock()
		defer r.mu.Unlock()
		r.sup, r.apply = sup, apply
	}
	// Registered before any boot, so it runs after the daemon's own stop (cleanups run in the
	// reverse of their order) and the tail carries the shutdown's lines too.
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("the daemon's log, its last lines but the idle passes':\n%s", r.tail(80))
		}
	})
	return r
}

// issue is the key of the project's nth issue.
func (r *pauseDaemonRig) issue(n int) string { return fmt.Sprintf("%s-%d", r.cfg.Project, n) }

// token is role's claim of issue.
func (r *pauseDaemonRig) token(issue string, role claim.Role) claim.Token {
	r.t.Helper()
	token, err := claim.NewToken(r.project, issue, role)
	if err != nil {
		r.t.Fatal(err)
	}
	return token
}

// controller is the project controller's claim.
func (r *pauseDaemonRig) controller() claim.Token { return claim.ControllerToken(r.project) }

// root is issue as a tree root of the rig's project in ph, at generation 1, on the board's
// repository.
func (r *pauseDaemonRig) root(issue string, ph phase.Phase) record.Issue {
	status := "in_progress"
	if ph == phase.Reviewing {
		status = "needs_review"
	}
	return record.Issue{Key: issue, Repo: liveRepo, DefaultBranch: "main", GitHubRepo: liveRepo, Project: r.cfg.Project,
		Tree: issue, Title: "A paused issue", Phase: ph, Generation: 1, Status: status, DispatchStatus: status,
		HandedOver: true, Rank: "U"}
}

// pauseRigPodLocator is the process of token in issue's pod, as the Sandbox runtime records it: every role
// of an issue runs in its one pod.
func pauseRigPodLocator(token claim.Token, issue string) *runtime.Locator {
	pod := "pod-" + strings.ToLower(issue)
	return &runtime.Locator{Runtime: runtime.RuntimeSandbox, Claim: token, Incarnation: runtime.SandboxIncarnation(pod, 1),
		Sandbox: &runtime.SandboxLocator{Namespace: "legion", Name: "sandbox-" + strings.ToLower(issue), PodUID: pod,
			Container: runtime.IssuePodContainer, Generation: 1}}
}

// pauseRigBootToken is the boot token the rig seeds a running claim's launch with, so a test can
// dial that claim's shim (dialShim) as its pod's launcher would after the daemon's restart.
func pauseRigBootToken(token claim.Token) string { return "boot-" + string(token) }

// running is role's claim of issue with its process up in the issue's pod, in state, on the
// daemon's plugin contract, its session kept under the role's name, and its launch's boot token
// pauseRigBootToken's.
func (r *pauseDaemonRig) running(issue string, role claim.Role, state supervise.ClaimState) supervise.Claim {
	token := r.token(issue, role)
	return supervise.Claim{Token: token, Project: r.project, Tree: issue, Issue: issue, Role: role, Generation: 1,
		ServingGeneration: 1, State: state, Session: "ses-" + string(token), SessionFile: "/sessions/" + string(token) + ".jsonl",
		PluginContract: api.DaemonAPIVersion(), Locator: pauseRigPodLocator(token, issue),
		BootTokenHash: supervise.HashBootToken(pauseRigBootToken(token))}
}

// released is role's claim of issue as an idle release left it: suspended `released`, its
// session kept, serving generation 1.
func (r *pauseDaemonRig) released(issue string, role claim.Role) supervise.Claim {
	token := r.token(issue, role)
	return supervise.Claim{Token: token, Project: r.project, Tree: issue, Issue: issue, Role: role, Generation: 1,
		ServingGeneration: 1, State: supervise.StateSuspended, SuspendedBy: supervise.SuspendedByReleased,
		Session: "ses-" + string(token), SessionFile: "/sessions/" + string(token) + ".jsonl", PluginContract: api.DaemonAPIVersion()}
}

// unfinished is role's claim of issue as a daemon stopped mid-launch left it: launching, with no
// process recorded, which the boot relaunches through the wake pacing (relaunchUnfinished).
func (r *pauseDaemonRig) unfinished(issue string, role claim.Role) supervise.Claim {
	token := r.token(issue, role)
	return supervise.Claim{Token: token, Project: r.project, Tree: issue, Issue: issue, Role: role, Generation: 1,
		ServingGeneration: 1, State: supervise.StateLaunching, Session: "ses-" + string(token),
		SessionFile: "/sessions/" + string(token) + ".jsonl", PluginContract: api.DaemonAPIVersion(),
		BootTokenHash: supervise.HashBootToken(pauseRigBootToken(token))}
}

// inPodRound is c in an update round over its issue's pod (pauseRigPodLocator's), opened at opened and
// asked of c then, for the reason a pod that runs an outdated image gives.
func inPodRound(c supervise.Claim, opened time.Time) supervise.Claim {
	c.UpdateRound = supervise.PodRound("pod-" + strings.ToLower(c.Issue))
	c.UpdateReasons, c.UpdateOpenedAt, c.UpdateTaskedAt = "the pod runs an image the deployment has moved on from", &opened, &opened
	return c
}

// seed records root as an admitted tree with its claims through the rig's open store
// (seedWorkflowTreeIn), puts it on the board at its status, writes each claim's role prompt, and
// stands its pod up in the runtime.
func (r *pauseDaemonRig) seed(root record.Issue, claims ...supervise.Claim) {
	r.t.Helper()
	seedWorkflowTreeIn(r.t, r.st, r.project, root, claims...)
	r.board.show(root.Key, root.DispatchStatus)
	for _, c := range claims {
		writePrompt(r.t, r.cfg, c.Token)
	}
	r.rt.holdPod(root.Key)
}

// reportListener stamps the Envoy listener's `listener_ready` report at the test clock's now, as
// the listener publishes one every five minutes, so the idle release's gate (listenerLive) reads
// a live listener however far a test moves the clock.
func (r *pauseDaemonRig) reportListener() {
	r.t.Helper()
	if _, err := r.pool.Exec(r.ctx, `insert into listener_reports (project, ready_at) values ($1, $2)
		on conflict (project) do update set ready_at = excluded.ready_at`, r.project, r.clock.Now().UTC()); err != nil {
		r.t.Fatalf("stamp the listener's report: %v", err)
	}
}

// boot runs the daemon over the rig until the test ends or restarts it, returning once it answers
// /healthz and has handed the seam its supervisor.
func (r *pauseDaemonRig) boot() {
	r.t.Helper()
	r.mu.Lock()
	r.sup, r.apply = nil, nil
	r.mu.Unlock()
	r.reportListener()
	logger := slog.New(slog.NewJSONHandler(r.logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	r.d = startDaemonLogging(r.t, r.cfg, r.o, logger)
	testwait.Eventually(r.t, "run to hand the rig its supervisor", func() bool {
		r.mu.Lock()
		defer r.mu.Unlock()
		return r.sup != nil
	})
}

// restart stops the daemon and boots it again on the same store, as a deployment's restart does.
func (r *pauseDaemonRig) restart() {
	r.t.Helper()
	r.d.stop()
	rebindHeldPorts(r.t, &r.cfg)
	r.boot()
}

// tail is the last n lines of the daemon's log but the idle and wake passes' own, which a tick
// writes every pauseRigTick.
func (r *pauseDaemonRig) tail(n int) string {
	var lines []string
	for line := range strings.Lines(r.logs.String()) {
		if strings.Contains(line, `"msg":"idle release:`) || strings.Contains(line, `"msg":"unowned release:`) {
			continue
		}
		lines = append(lines, strings.TrimRight(line, "\n"))
	}
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}

// supervisor is the daemon's supervisor, which the seam handed over.
func (r *pauseDaemonRig) supervisor() *supervisor {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.sup
}

// machine is token's machine.
func (r *pauseDaemonRig) machine(token claim.Token) *supervise.Machine {
	r.t.Helper()
	m, ok := r.supervisor().Machine(token)
	if !ok {
		r.t.Fatalf("the daemon supervises no claim %s", token)
	}
	return m
}

// claim is token's claim as its machine holds it, read without waiting on a decision.
func (r *pauseDaemonRig) claim(token claim.Token) supervise.Claim {
	r.t.Helper()
	return r.machine(token).View().Claim
}

// handle posts ev to token's machine and waits for its decision, as internal/supervise's tests
// post every event.
func (r *pauseDaemonRig) handle(token claim.Token, ev supervise.Event) {
	r.t.Helper()
	if err := r.machine(token).Handle(r.ctx, ev); err != nil {
		r.t.Fatalf("handle %T for %s: %v", ev, token, err)
	}
}

// applyFact applies fact with the daemon's own handlers under eventID, as its consumers and its
// API apply one, and fails the test on a refusal.
func (r *pauseDaemonRig) applyFact(source, eventID string, fact intake.Fact) {
	r.t.Helper()
	r.mu.Lock()
	apply := r.apply
	r.mu.Unlock()
	result, err := apply(r.ctx, source, eventID, fact)
	if err != nil {
		r.t.Fatalf("apply %T %s: %v", fact, eventID, err)
	}
	if result.Refusal != nil {
		r.t.Fatalf("apply %T %s: refused %+v", fact, eventID, *result.Refusal)
	}
}

// awaitState waits for token's claim to reach state, both as its machine holds it and as the store
// holds it: a machine publishes its claim before the write that persists it (supervise's
// persistStop), and what reaches the claim through the workflow next — a listener's wake of a
// stopped role — reads the store.
func (r *pauseDaemonRig) awaitState(token claim.Token, state supervise.ClaimState, why string) supervise.Claim {
	r.t.Helper()
	var c supervise.Claim
	testwait.Eventually(r.t, fmt.Sprintf("%s to be %s: %s", token, state, why), func() bool {
		c = r.claim(token)
		return c.State == state && r.count(`select count(*) from claims where token = $1 and state = $2`, string(token), string(state)) == 1
	})
	return c
}

// parkRelaunch leaves token, a claim seeded unfinished, with a relaunch the wake pacing has
// parked: the boot launches it again, and the burst lets that one launch straight out; its process
// then dies (the runtime's observation, posted as the sweep posts one), and the relaunch the death
// raises waits in the pacing for the interval after the burst, since the test clock stands still.
func (r *pauseDaemonRig) parkRelaunch(token claim.Token) {
	r.t.Helper()
	var c supervise.Claim
	testwait.Eventually(r.t, fmt.Sprintf("the boot's launch of %s, which the burst lets out", token), func() bool {
		c = r.claim(token)
		return len(r.rt.launchesOf(token)) == 1 && c.State == supervise.StateLaunching && c.Locator != nil
	})
	r.handle(token, supervise.RuntimeObservation{Observation: runtime.Observation{Locator: *c.Locator, Kind: runtime.Gone, At: r.clock.Now()}})
	testwait.Eventually(r.t, fmt.Sprintf("the relaunch of %s to wait in the pacing", token), func() bool {
		return slices.ContainsFunc(r.state().WakesWaiting, func(w api.WakeWaiting) bool { return w.Claim == token })
	})
	if launched := r.rt.launchesOf(token); len(launched) != 1 {
		r.t.Fatalf("%s was launched %d times, want the boot's one launch: its relaunch waits in the pacing", token, len(launched))
	}
}

// pauseProject pauses the project through the operator's route, and so through the daemon's pause
// service, and answers the pause.
func (r *pauseDaemonRig) pauseProject() api.PauseView {
	r.t.Helper()
	status, body := r.d.request(http.MethodPost, "/legion/v1/operator/project/pause", api.OperatorPauseRequest{Reason: pauseRigReason}, true)
	if status != http.StatusOK {
		r.t.Fatalf("the operator's pause = %d; body %s", status, body)
	}
	var answer api.ProjectPauseResponse
	if err := json.Unmarshal(body, &answer); err != nil {
		r.t.Fatalf("decode the pause %s: %v", body, err)
	}
	return answer.Pause
}

// resumeProject resumes the project through the operator's route and answers the resume.
func (r *pauseDaemonRig) resumeProject() api.ProjectResumeResponse {
	r.t.Helper()
	status, body := r.d.request(http.MethodPost, "/legion/v1/operator/project/resume", nil, true)
	if status != http.StatusOK {
		r.t.Fatalf("the operator's resume = %d; body %s", status, body)
	}
	var answer api.ProjectResumeResponse
	if err := json.Unmarshal(body, &answer); err != nil {
		r.t.Fatalf("decode the resume %s: %v", body, err)
	}
	return answer
}

// advance moves the test clock on by d, the listener reporting all the while, and waits until the
// daemon's idle and wake passes have each run twice at the new moment.
func (r *pauseDaemonRig) advance(d time.Duration) {
	r.t.Helper()
	r.clock.advance(d)
	r.reportListener()
	r.awaitPasses()
}

// awaitPasses waits until the daemon's idle pass has run twice wholly after the call, at the
// clock's new moment. A pass writes one line for each pod it keeps (releaseIdlePod,
// releaseUnownedPod) and one for the unowned pass as a whole (releaseUnownedPods), so each such
// line comes once a pass: the first of one after the call may belong to a pass that read the
// clock before it moved, and its second and third to passes that began after it. The wake tick
// runs beside the idle tick on a ticker of its own and writes nothing when nothing is due, so two
// more of the tick's periods are slept for it to have read the clock's new moment too.
func (r *pauseDaemonRig) awaitPasses() {
	r.t.Helper()
	since := r.mark()
	testwait.Eventually(r.t, "two passes of the idle release at the clock's new moment", func() bool {
		seen := map[string]int{}
		for _, msg := range []string{"idle release: the pod is kept", "unowned release: the pod is kept",
			"unowned release: decided every pod of the project no claim owns"} {
			for _, line := range r.linesAfter(since, msg) {
				key := msg + " " + fmt.Sprint(line["issue"])
				seen[key]++
				if seen[key] == 3 {
					return true
				}
			}
		}
		return false
	})
	time.Sleep(2 * pauseRigTick)
}

// awaitFullSweep waits until one whole pass of the daemon's sweep has run since the call
// (supervision.reconcileOrphans, which decides the update rounds): two more passes have begun,
// each with its runtime's ReconcileOrphans, and passes run one after another, so the first of them
// read the clock after the call and has ended. A check that the sweep replaced
// nothing reads what that pass did not do.
func (r *pauseDaemonRig) awaitFullSweep() {
	r.t.Helper()
	before := len(r.rt.CallsOf("ReconcileOrphans"))
	testwait.Eventually(r.t, "a whole pass of the daemon's sweep after this moment", func() bool {
		return len(r.rt.CallsOf("ReconcileOrphans")) >= before+2
	})
}

// lines is every line of the daemon's log whose message is msg, decoded.
func (r *pauseDaemonRig) lines(msg string) []map[string]any {
	var found []map[string]any
	for line := range strings.Lines(r.logs.String()) {
		if !strings.Contains(line, msg) {
			continue
		}
		entry := map[string]any{}
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			r.t.Fatalf("decode log line %q: %v", line, err)
		}
		if entry["msg"] == msg {
			found = append(found, entry)
		}
	}
	return found
}

// linesOf is the lines of msg naming token as their claim.
func (r *pauseDaemonRig) linesOf(msg string, token claim.Token) []map[string]any {
	var of []map[string]any
	for _, line := range r.lines(msg) {
		if line["claim"] == string(token) {
			of = append(of, line)
		}
	}
	return of
}

// mark is how far the daemon's log has been written, for reading only what comes after it
// (linesAfter).
func (r *pauseDaemonRig) mark() int { return len(r.logs.String()) }

// linesAfter is every line of msg the daemon wrote after mark.
func (r *pauseDaemonRig) linesAfter(mark int, msg string) []map[string]any {
	var found []map[string]any
	for line := range strings.Lines(r.logs.String()[mark:]) {
		if !strings.Contains(line, msg) {
			continue
		}
		entry := map[string]any{}
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			r.t.Fatalf("decode log line %q: %v", line, err)
		}
		if entry["msg"] == msg {
			found = append(found, entry)
		}
	}
	return found
}

// dialShim is dialShim (daemon_test.go) over the worker stream a Kubernetes daemon binds, a TCP
// address its pods' launchers dial (prepareSandbox): it connects, says hello with bootToken and
// returns once the daemon's hello_ack arrives.
func (r *pauseDaemonRig) dialShim(bootToken string) *shim {
	r.t.Helper()
	r.built.mu.Lock()
	address := r.built.address
	r.built.mu.Unlock()
	conn, err := net.Dial("tcp", strings.TrimPrefix(address, "tcp://"))
	if err != nil {
		r.t.Fatalf("dial the worker stream %s: %v", address, err)
	}
	r.t.Cleanup(func() { _ = conn.Close() })
	s := &shim{t: r.t, conn: conn, lines: bufio.NewReader(conn), writer: shimwire.NewWriter(conn)}
	s.send(shimwire.Hello2{BootToken: bootToken})
	if frame := s.next(); frame.FrameType() != shimwire.TypeHelloAck {
		r.t.Fatalf("the daemon answered the hello with %#v, want hello_ack", frame)
	}
	return s
}

// controllerUp brings the daemon's own controller up: its launch's shim says hello, its agent
// registers with the launch's boot token and says it is ready, and it takes the start message the
// daemon hands it in a turn that ends, as controllerLaunched and registeredLaunch drive it
// (controller_launch_test.go). It answers the shim the daemon prompts the controller on.
func (r *pauseDaemonRig) controllerUp() *shim {
	r.t.Helper()
	token := r.controller()
	launch := controllerLaunched(r.t, r.rt.Runtime, token)
	sh := r.dialShim(launch.BootToken)
	r.awaitState(token, supervise.StateShimConnected, "the controller's shim said hello")
	status, body := r.d.request(http.MethodPost, "/legion/v1/claims/register", claim.RegisterRequest{
		BootToken: launch.BootToken, SessionID: "ses_controller", OmpSessionFile: "/sessions/ses_controller.jsonl",
		AgentID: "ses_controller", PluginContract: api.DaemonAPIVersion(),
	}, false)
	if status != http.StatusOK {
		r.t.Fatalf("register the controller = %d; body %s", status, body)
	}
	var registration api.ControllerRegisterResponse
	if err := json.Unmarshal(body, &registration); err != nil {
		r.t.Fatalf("decode the controller's registration %s: %v", body, err)
	}
	if status, body := r.d.request(http.MethodPost, "/legion/v1/claims/ready", claim.ReadyRequest{
		ClaimToken: token, SessionID: "ses_controller", Secret: registration.Secret, Generation: launch.Generation,
	}, false); status != http.StatusNoContent {
		r.t.Fatalf("the controller's ready = %d; body %s", status, body)
	}
	r.takeTurn(sh, ControllerStartMessage)
	r.awaitState(token, supervise.StateIdle, "the controller's start turn has ended")
	return sh
}

// takeTurn is the agent behind sh taking the next prompt, which must be want, in a turn that runs
// and ends, as Oh My Pi answers one: the prompt acknowledged, the turn's start and end, and the
// prompt's result.
func (r *pauseDaemonRig) takeTurn(sh *shim, want string) {
	r.t.Helper()
	prompt := r.startTurn(sh, want)
	sh.send(shimwire.AgentEnd{})
	sh.send(shimwire.PromptResult{ID: prompt.ID, AgentInvoked: true})
}

// startTurn is the agent behind sh taking the next prompt, which must end with want, and starting
// its turn on it. It answers the prompt.
func (r *pauseDaemonRig) startTurn(sh *shim, want string) shimwire.Prompt {
	r.t.Helper()
	prompt := sh.prompt()
	if !strings.HasSuffix(prompt.Message, want) {
		r.t.Fatalf("the agent was prompted %q, want a prompt ending %q", prompt.Message, want)
	}
	sh.send(shimwire.Response{ID: prompt.ID, Command: shimwire.TypePrompt, Success: true})
	sh.send(shimwire.AgentStart{})
	return prompt
}

// deliver is the operator's `legion claims deliver` of task to token, through the operator's
// route (the machine's RequestDeliver).
func (r *pauseDaemonRig) deliver(token claim.Token, task string) {
	r.t.Helper()
	status, body := r.d.request(http.MethodPost, "/legion/v1/operator/claims/"+string(token)+"/deliver", api.DeliverRequest{Task: task}, true)
	if status != http.StatusOK && status != http.StatusAccepted {
		r.t.Fatalf("the operator's delivery to %s = %d; body %s", token, status, body)
	}
}

// state is the daemon's state as the operator reads it.
func (r *pauseDaemonRig) state() api.State {
	r.t.Helper()
	return r.d.state()
}

// count is the one number sql counts on the rig's database.
func (r *pauseDaemonRig) count(sql string, args ...any) int {
	r.t.Helper()
	var n int
	if err := r.pool.QueryRow(r.ctx, sql, args...).Scan(&n); err != nil {
		r.t.Fatalf("count %q: %v", sql, err)
	}
	return n
}

// enqueue writes payload as an outbox row of issue on the rig's database, due now on both clocks
// that read it (dueNow).
func (r *pauseDaemonRig) enqueue(issue string, payload record.OutboxPayload) int64 {
	r.t.Helper()
	var id int64
	if err := store.Transact(r.ctx, r.pool, func(ctx context.Context, tx pgx.Tx) error {
		row, err := record.NewFencedRow(r.ctx, tx, r.records, issue, payload, r.dueNow())
		if err != nil {
			return err
		}
		id, err = r.records.EnqueueID(r.ctx, tx, row)
		return err
	}); err != nil {
		r.t.Fatalf("queue the outbox row of %s: %v", issue, err)
	}
	return id
}

// outboxRows is every outbox row keyed by issue, by id, each spelled `<kind>/<payload kind or op>`.
func (r *pauseDaemonRig) outboxRows(issue string) map[int64]string {
	r.t.Helper()
	rows, err := r.pool.Query(r.ctx, `select id, kind || '/' || coalesce(payload->>'kind', payload->>'op', '') from outbox where issue = $1`, issue)
	if err != nil {
		r.t.Fatalf("read the outbox rows of %s: %v", issue, err)
	}
	defer rows.Close()
	found := map[int64]string{}
	for rows.Next() {
		var id int64
		var what string
		if err := rows.Scan(&id, &what); err != nil {
			r.t.Fatalf("read the outbox rows of %s: %v", issue, err)
		}
		found[id] = what
	}
	if err := rows.Err(); err != nil {
		r.t.Fatalf("read the outbox rows of %s: %v", issue, err)
	}
	return found
}

// recordOf is the issues table's record of key, whatever project holds it, nil for none.
func (r *pauseDaemonRig) recordOf(key string) *record.Issue {
	r.t.Helper()
	var recorded *record.Issue
	if err := store.Transact(r.ctx, r.pool, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		recorded, err = r.records.Issue(r.ctx, tx, key)
		return err
	}); err != nil {
		r.t.Fatalf("read the record of %s: %v", key, err)
	}
	return recorded
}

// dueNow is the moment an outbox row is due now on both clocks that read it: the outbox judges a
// row due by the wall clock (newOutbox's now), and the state lists a held row as due by the
// daemon's one clock, the rig's test clock (source.now). The test clock stands still while the
// wall clock runs and is moved ahead of it by a test, so it is the earlier of the two.
func (r *pauseDaemonRig) dueNow() time.Time {
	at := time.Now().UTC()
	if now := r.clock.Now().UTC(); now.Before(at) {
		at = now
	}
	return at
}

// makeDue makes every row of kind and of issues still in the outbox due a minute ago on both
// clocks that read it, as dueNow reads them: the outbox's lanes judge a row due by the wall clock,
// and a resume marks the rows its pause held that are due by the daemon's clock, the rig's test
// clock (pause.Service.Resume, record.MarkPauseHeldRows). It waits first until no lane holds any
// of those rows, so that no run in flight settles its next attempt over the moment, and answers
// how many rows it moved.
func (r *pauseDaemonRig) makeDue(kind record.OutboxKind, issues ...string) int {
	r.t.Helper()
	of := fmt.Sprintf("the %s rows of %s", kind, strings.Join(issues, ", "))
	testwait.Eventually(r.t, "no lane to hold "+of, func() bool {
		return r.count(`select count(*) from outbox where kind = $1 and issue = any($2) and lease_token <> ''`, string(kind), issues) == 0
	})
	moved, err := r.pool.Exec(r.ctx, `update outbox set next_at = least(now(), $3) - interval '1 minute' where kind = $1 and issue = any($2)`,
		string(kind), issues, r.clock.Now().UTC())
	if err != nil {
		r.t.Fatalf("make %s due: %v", of, err)
	}
	return int(moved.RowsAffected())
}

// heldByPause is the pause outbox row id is marked with (outbox.held_by_pause), 0 for none, and -1
// once the row has ended.
func (r *pauseDaemonRig) heldByPause(id int64) int64 {
	r.t.Helper()
	var pause int64
	if err := r.pool.QueryRow(r.ctx, `select coalesce((select coalesce(held_by_pause, 0) from outbox where id = $1), -1)`, id).Scan(&pause); err != nil {
		r.t.Fatalf("read the pause outbox row %d is marked with: %v", id, err)
	}
	return pause
}

// later is the later of the test clock and the wall clock: the engine stamps the rows it queues
// with the test clock, and the outbox judges a row due by the wall clock, so a read of what is
// due at both moments reads it here.
func (r *pauseDaemonRig) later() time.Time {
	now, wall := r.clock.Now().UTC(), time.Now().UTC()
	if wall.After(now) {
		return wall
	}
	return now
}

// openPodHold is whether a pause holds issue's pod now.
func (r *pauseDaemonRig) openPodHold(issue string) bool {
	r.t.Helper()
	return r.count(`select count(*) from project_pause_pod_holds where issue = $1 and ended_at is null`, issue) > 0
}

// startRows is the supervise starts of issue's role the outbox still holds, each spelled
// `<wakeKind>/<pause>`.
func (r *pauseDaemonRig) startRows(issue string, role claim.Role) []string {
	r.t.Helper()
	rows, err := r.pool.Query(r.ctx, `select coalesce(payload->>'wakeKind', ''), coalesce(payload->>'pause', '0') from outbox
		where kind = 'supervise' and issue = $1 and payload->>'op' = 'start' and payload->>'role' = $2 order by id`, issue, string(role))
	if err != nil {
		r.t.Fatalf("read the starts of %s's %s: %v", issue, role, err)
	}
	defer rows.Close()
	var starts []string
	for rows.Next() {
		var kind, pause string
		if err := rows.Scan(&kind, &pause); err != nil {
			r.t.Fatal(err)
		}
		starts = append(starts, kind+"/"+pause)
	}
	if err := rows.Err(); err != nil {
		r.t.Fatalf("read the starts of %s's %s: %v", issue, role, err)
	}
	return starts
}

// heldIn is the rows of held of issue and of kind.
func heldIn(held []api.PauseHeldView, kind string, issue string) []api.PauseHeldView {
	var of []api.PauseHeldView
	for _, row := range held {
		if row.Issue == issue && row.Kind == kind {
			of = append(of, row)
		}
	}
	return of
}

// strandedRoles is the claims a state names stranded, each spelled `<issue> <role>`.
func strandedRoles(state api.State) []string {
	roles := make([]string, 0, len(state.StrandedClaims))
	for _, c := range state.StrandedClaims {
		roles = append(roles, c.Issue+" "+string(c.Role))
	}
	return roles
}
