package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/appauth"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/ghconfig"
	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/ompsessions"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	legionruntime "github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/runtime/sandbox"
	"github.com/sjawhar/legion/daemon/internal/sessionproxy"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/stream"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/testnats"
	"github.com/sjawhar/legion/daemon/internal/testtmp"
	"github.com/sjawhar/legion/daemon/internal/testwait"
	"github.com/sjawhar/legion/daemon/internal/treelifecycle"
	"github.com/sjawhar/legion/daemon/internal/workspace"
)

// idleRigImage is the worker image the rig's configuration names, pinned by digest as the runtime
// requires. Every pod the rig's controller makes runs it, so no pod is ever held back for an old
// image.
const idleRigImage = "ghcr.io/example/worker@sha256:" + "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

// idleRigIdleMinutes and idleRigBackstopMinutes are the rig's `runtime.kubernetes.idle_release`.
// The idle period is long enough that a pod a wake brought back is not released again while the
// test still reads it, and the release wave costs nothing for it: the rule measures the period
// against the claim rows, which the rig back-dates half an hour (idleWakeRig.quiet), so the first
// release comes at the first tick either way. The backstop is long enough that no pod is woken by one it did not ask
// for: the subtest that drives the backstop brings its own pod's due time forward instead
// (idleWakeRig.dueBackstop), as the stuck-resume pass's ten minutes is driven by the recorded
// wake time (idleWakeRig.ageResume). Both are the clock those passes read, and nothing else.
const (
	idleRigIdleMinutes     = 5
	idleRigBackstopMinutes = 10
)

// idleRigTick is how often the rig's daemon decides every pod and fires the backstops that came
// due (overrides.idleTick, the daemon's own minute in production). Every moment the rig drives is
// a row it back-dates — the quiet period, a backstop's due time, a wake's age — so the tick's
// period buys nothing but the wait for it, and a quarter of a second takes minutes off the run
// while leaving each pass as far apart as the work it does.
const idleRigTick = 250 * time.Millisecond

// idleWakeRig is the acceptance rig of INFRA-945's A3: the Go daemon on a real Postgres, a real
// nats-server, a real Envoy listener and the Sandbox runtime over a fake Kubernetes API whose
// controller stand-in starts a real `legion launcher` for every pod it reports Ready, each
// launcher running a scripted stand-in for Oh My Pi that registers, claims its role and records
// every message it is given.
//
// Where each part comes from:
//
//   - the whole-daemon boot, its Dispatch and GitHub stand-ins, its store seeding and its shim
//     protocol: packages/daemon/internal/daemon/check_wake_test.go's checkWakeRig;
//   - the fake Kubernetes API, its Sandbox-controller stand-in and its pods:
//     packages/daemon/internal/runtime/sandbox/rig_test.go, rebuilt over HTTP
//     (idle_rig_kube_test.go);
//   - the real launcher per Ready pod: packages/daemon/cmd/legion/launcher_test.go's in-process
//     launcher (idle_rig_pod_test.go);
//   - the scripted Oh My Pi: packages/daemon/internal/capabilities/image_test.go's fakeOmp, grown
//     a registration, a role claim and a record of every delivery (idle_rig_agent_test.go);
//   - Postgres through internal/testpg's database (isolatedOutboxPool) and NATS through
//     internal/testnats. This pod has no Docker, so testnats serves the nats-server
//     LEGION_TEST_NATS_URL and LEGION_TEST_NATS_MONITOR_URL name rather than a container, which is
//     what that package does when both are set;
//   - the real Envoy listener, built from packages/envoy/cmd/listener and run as its own process
//     (idle_rig_listener_test.go).
type idleWakeRig struct {
	// t is the test the rig's waits and reads fail, and root, once a step of the test runs as a
	// subtest (step), the test the rig was made for, which owns every cleanup (owner).
	t       *testing.T
	root    *testing.T
	cfg     config.Config
	pool    *store.Pool
	natsURL string
	js      jetstream.JetStream
	board   *liveDispatch
	// kube is the stand-in for the cluster every rig boots its daemon on first, and second the
	// stand-in for another cluster, made for a test that moves a project between the two
	// (idleRigOptions.secondCluster), nil otherwise. kubeconfigs is the kubeconfig that reaches
	// each; the one the configuration names is the cluster the next boot reaches (aimAt).
	kube        *issueSandboxAPI
	second      *issueSandboxAPI
	kubeconfigs map[*issueSandboxAPI]string
	pods        *idleRigPods
	agents      *idleRigAgents
	listener    *idleRigListener
	records     record.Store
	project     string
	logs        *syncBuffer
	built       *built
	api         *daemon
	options     idleRigOptions
	// work is the remote every pod's workspace is provisioned from and those workspaces, nil
	// unless the test asked for them (idleRigOptions.workspaces).
	work *idleRigWorkspaces
	// issues are the rig's issues, in the order they were seeded.
	issues []idleRigIssue
	// daemonLog is where a copy of the daemon's log is kept.
	daemonLog *os.File
	// baseline is how many tasks each role had been given when markPrompts was called, so a
	// subtest counts the tasks a wake gave and not the phase start the daemon's boot gave.
	baseline map[string]int
	// toolsPath holds the rig's `legion` and `omp` stand-ins, made once.
	toolsPath string

	// stop ends the running daemon, and done answers when it has.
	stopMu sync.Mutex
	stop   context.CancelFunc
	done   chan error
}

// idleRigIssue is one issue of the rig: its key, the pod its claims run in, the role that owns its
// phase, and every role it has a claim of, that one first.
type idleRigIssue struct {
	// name is what the test calls the issue; key is the issue key its project gives it.
	name  string
	key   string
	pod   string
	role  claim.Role
	roles []claim.Role
	// heldBack is open while the rig starts no launcher in the issue's pod (idleRigSeed.heldBack)
	// and closed once the test lets them start (letStart); nil for an issue the rig never holds
	// back.
	heldBack chan struct{}
}

// idleRigOptions is what a test asks of the rig beyond what the idle release's tests run on, each
// left off by those tests: one daemon on one cluster, on a clock whose timers never fire, whose
// pods hold no workspace.
type idleRigOptions struct {
	// holdStream gives the worker stream one port every boot binds again, and the API one port
	// every restart holds again, as a deployment's worker_stream_port and port are fixed in its
	// configuration: a launcher of a pod a restart did not end reconnects to the restarted daemon,
	// and its role process reaches the restarted daemon's API, which is how a restart adopts a pod.
	holdStream bool
	// wallClock boots the daemon on the wall clock, as a deployment runs, so a timer a machine
	// arms fires: a suspension held for a turn ends at its deadline.
	wallClock bool
	// orphanSweep is how often the daemon's sweep runs (overrides.orphanSweep), which carries the
	// pod update rounds; zero is the daemon's minute.
	orphanSweep time.Duration
	// entrySweep is how often the daemon converges the phase entries with no fact arriving
	// (overrides.entrySweep); zero is the daemon's minute.
	entrySweep time.Duration
	// workspaces gives each issue pod a workspace made by the real provisioning from a local
	// remote (idleRigWorkspaces), which the pod's role processes read and push.
	workspaces bool
	// secondCluster makes the stand-in for a second cluster beside the first, and aimSecond boots
	// the daemon on it first.
	secondCluster, aimSecond bool
	// configure changes the configuration before the first boot.
	configure func(*config.Config)
	// controllerPlan is the plan the project controller's role process follows, set before the
	// first boot, which launches it where configure names `controller: daemon`; nil leaves the
	// rig's own.
	controllerPlan *idleRigPlan
}

// idleRigSeed is one issue the rig seeds: what the test calls it, every role it has a claim of
// with the one that owns its phase first, whether it has a pull request open, the plan each
// of its role processes follows, set before the daemon's boot starts any of them (nil leaves the
// rig's own), and whether the rig holds its roles back.
type idleRigSeed struct {
	name  string
	roles []claim.Role
	pull  bool
	plan  *idleRigPlan
	// heldBack keeps every role of the issue from starting until the test lets it (letStart). Its
	// claims are seeded suspended by an operator, so no boot starts one and launchAll passes the
	// issue over, and a pod made for it meanwhile starts no launcher, as a pod whose init
	// containers have not finished starts none. A claim of it the test resumes meanwhile stands
	// launching in a pod no role process ever started in.
	heldBack bool
}

// newIdleWakeRig readies every part, boots the daemon, and waits until every seeded issue's pod
// runs its role, in order, with one issue per name given. Each issue is its own tree root, in the
// workflow at `implementing` with its implementer's claim suspended by the daemon, a source the
// boot's resume by rule acts on (supervise.SuspendSource.ByRule): the boot itself resumes each
// claim, and that launch is what makes the pod, its launcher and its role process, which can all
// start before the boot returns.
//
// plans is what the role process of each issue it names does, by the name the test calls the
// issue; an issue it does not name, and every issue when plans is nil, runs the default plan. They
// are set before the boot (idleRigPlan says why), as the plan of a seed is (idleRigSeed.plan). A
// name in plans that names no issue of issues fails the test.
func newIdleWakeRig(t *testing.T, plans map[string]idleRigPlan, issues ...string) *idleWakeRig {
	t.Helper()
	for name := range plans {
		if !slices.Contains(issues, name) {
			t.Fatalf("the rig seeded no issue called %s", name)
		}
	}
	seeds := make([]idleRigSeed, len(issues))
	for i, name := range issues {
		seeds[i] = idleRigSeed{name: name, roles: []claim.Role{claim.RoleImplementer}, pull: name == idleRigChecks}
		if plan, ok := plans[name]; ok {
			seeds[i].plan = &plan
		}
	}
	rig := newIdleRigWith(t, idleRigOptions{}, seeds...)
	rig.launchAll()
	return rig
}

// newIdleRigWith readies every part and boots the daemon, as newIdleWakeRig does, with what
// options asks beyond it and with the roles and plan each seed names. The boot starts the phase
// owner of each seed the rig does not hold back (idleRigSeed.heldBack), and that process can
// start before the boot returns, so a plan it reads at its start belongs on the seed
// (idleRigSeed.plan). It waits for no pod: its caller waits for them with launchAll.
func newIdleRigWith(t *testing.T, options idleRigOptions, seeds ...idleRigSeed) *idleWakeRig {
	t.Helper()
	pool := isolatedOutboxPool(t)
	if _, err := pool.Exec(context.Background(), idleRigSessionTable()); err != nil {
		t.Fatalf("make the session table the launchers look a resume up in: %v", err)
	}
	natsURL := testnats.URL(t)
	// The listener owns the notification stream: it creates it with its own subjects before the
	// daemon binds a consumer on it, which is the order a deployment rolls them in.
	listener := startIdleRigListener(t, natsURL, "idlerig")
	cfg := workflowConfig(t, natsURL)
	cfg.PostgresDSN = pool.Config().ConnString()
	cfg.EnvoyURL = listener.url
	// One admission slot per issue of the rig, and a cap with room for them all: an issue the
	// daemon has not admitted is one its boot lingers closed.
	cfg.AdmissionCap = len(seeds) + 4
	if options.holdStream {
		cfg.WorkerStreamPort = idleRigFreePort(t)
	}
	// Every launcher of the rig reaches that database through its generation's proxy, which
	// carries the launcher's own startup packet to it unchanged, so the password the rig's DSN
	// names has to be in the environment before the first launcher starts
	// (idleRigSessionPassword).
	idleRigSessionPassword(t, pool)
	cfg.Runtime = config.Runtime{Name: "kubernetes", Kubernetes: &config.Kubernetes{
		Namespace: sandboxNamespace, Image: idleRigImage, StorageClass: "standard", IssueVolume: "1Gi",
		Resources: config.DefaultResources(), RepositoryCache: config.DefaultRepositoryCache(),
		SessionStore: ompsessions.Store{Kind: ompsessions.StoreCloudSQLIAM,
			Database: ompsessions.Database{Instance: "legion-test:us-central1:sessions", Name: idleRigDatabase(t, pool), User: idleRigUser(t, pool)}},
		IdleRelease: config.IdleReleaseConfig{IdleMinutes: idleRigIdleMinutes, BackstopMinutes: idleRigBackstopMinutes},
	}}
	if options.configure != nil {
		options.configure(&cfg)
	}
	project, err := claim.ProjectToken(cfg.Project)
	if err != nil {
		t.Fatal(err)
	}
	// The rig's board lists the issues it holds: the daemon's boot lingers closed every recorded
	// issue Dispatch's listing does not name.
	board := newLiveDispatch(t, cfg.Project)
	board.lists = true
	cfg.DispatchURL = board.url
	rig := &idleWakeRig{t: t, cfg: cfg, board: board, pool: pool, natsURL: natsURL, js: workflowJetStream(t, natsURL),
		kube: newIssueSandboxAPI(), kubeconfigs: map[*issueSandboxAPI]string{}, agents: newIdleRigAgents(t, pool),
		listener: listener, records: record.NewStore(), project: project, logs: &syncBuffer{}, built: &built{}, options: options}
	rig.pods = newIdleRigPods(t, rig.address, cfg.Runtime.Kubernetes.SessionStore.Database,
		func(context.Context, sessionproxy.Signer) (sessionproxy.Dialer, error) {
			return idleRigDialer{address: idleRigHostPort(t, pool)}, nil
		})
	keys := make([]string, len(seeds))
	for i := range seeds {
		keys[i] = cfg.Project + "-" + strconv.Itoa(i+1)
	}
	if options.workspaces {
		rig.work = newIdleRigWorkspaces(t, keys)
		rig.agents.setWorkspaces(func(pod string) (idleRigWorkspaceAnswer, bool) {
			// A role process names its pod by its issue (LEGION_ISSUE), the one name the runtime
			// gives an issue's roles.
			for _, seeded := range rig.issues {
				if seeded.key == pod {
					pod = seeded.pod
				}
			}
			live, ok := rig.work.running(pod)
			if !ok {
				return idleRigWorkspaceAnswer{}, false
			}
			issue := ""
			for _, seeded := range rig.issues {
				if seeded.pod == pod {
					issue = seeded.key
				}
			}
			return idleRigWorkspaceAnswer{Dir: live.dir, Issue: issue, Env: rig.work.env(), Remote: rig.work.remote,
				URL: workspace.GitHubURL(rig.work.repo)}, true
		})
	}
	rig.kube.prefix = "a-"
	rig.serveCluster(rig.kube)
	if options.secondCluster {
		rig.second = newIssueSandboxAPI()
		rig.second.prefix = "b-"
		rig.serveCluster(rig.second)
	}
	// Boot reads runtime.kubernetes' kubeconfig whatever runtime it is given (readSandbox,
	// kubeClient), and with none it reads the pod's in-cluster credentials, so the rig names its
	// own stand-in API: it runs the same way outside a cluster as in one, and never holds a
	// credential of the cluster it runs in. The runtime the rig builds reaches its cluster through
	// that same kubeconfig (newRuntime), so the kubeconfig the boot reads decides the cluster.
	rig.aimAt(rig.kube)
	if options.aimSecond {
		rig.aimAt(rig.second)
	}
	t.Setenv(idleRigURLVariable, rig.agents.url)

	for i, seed := range seeds {
		rig.issues = append(rig.issues, rig.seed(seed, keys[i], i))
	}
	if options.controllerPlan != nil {
		rig.agents.setPlan(idleRigControllerPod, string(claim.RoleController), *options.controllerPlan)
	}
	rig.boot()
	rig.awaitListenerReady()
	return rig
}

// serveCluster serves the stand-in for one cluster, writes the kubeconfig that reaches it, and runs
// its controller stand-in, whose pods start the rig's launchers.
func (r *idleWakeRig) serveCluster(cluster *issueSandboxAPI) {
	r.t.Helper()
	cluster.serve(r.t)
	r.kubeconfigs[cluster] = writeKubeconfig(r.t, cluster.url, "test")
	cluster.onPodReady = func(pod, uid string, roles map[string]string) { r.podReady(cluster, pod, uid, roles) }
	cluster.onPodGone = func(pod string) { r.podGone(cluster, pod) }
	cluster.run(r.t)
}

// aimAt names cluster's kubeconfig in the configuration, so the daemon's next boot reaches that
// cluster, as an operator moves a daemon by changing runtime.kubernetes.kubeconfig. It is called
// while no daemon runs, and gives the configuration a Kubernetes section of its own, since the
// stopped daemon's may still be read by what it left running.
func (r *idleWakeRig) aimAt(cluster *issueSandboxAPI) {
	r.t.Helper()
	kubeconfig, ok := r.kubeconfigs[cluster]
	if !ok {
		r.t.Fatal("the rig serves no such cluster")
	}
	k := *r.cfg.Runtime.Kubernetes
	k.Kubeconfig = kubeconfig
	r.cfg.Runtime.Kubernetes = &k
}

// aimed is the cluster the configuration names, the one the running daemon reaches.
func (r *idleWakeRig) aimed() *issueSandboxAPI {
	r.t.Helper()
	for cluster, kubeconfig := range r.kubeconfigs {
		if kubeconfig == r.cfg.Runtime.Kubernetes.Kubeconfig {
			return cluster
		}
	}
	r.t.Fatalf("the configuration names the kubeconfig %s, which reaches no cluster of the rig", r.cfg.Runtime.Kubernetes.Kubeconfig)
	return nil
}

// listenerReports is every row of listener_reports, by the project each is filed under.
func (r *idleWakeRig) listenerReports() map[string]string {
	rows, err := r.pool.Query(context.Background(), "select project, ready_at from listener_reports")
	if err != nil {
		return map[string]string{"error": err.Error()}
	}
	defer rows.Close()
	held := map[string]string{}
	for rows.Next() {
		var project string
		var at time.Time
		if err := rows.Scan(&project, &at); err != nil {
			return map[string]string{"error": err.Error()}
		}
		held[project] = at.UTC().Format(time.RFC3339Nano)
	}
	return held
}

// awaitListenerReady waits until the daemon has read a `listener_ready` report of the Envoy
// listener, which is R7's gate: it releases nothing until it has. The listener publishes one at
// its start and every five minutes after, so a listener that came up before this daemon's durable
// consumer existed is started again rather than waited out.
func (r *idleWakeRig) awaitListenerReady() {
	r.t.Helper()
	defer func() {
		if r.t.Failed() {
			r.t.Logf("the Envoy listener's log:\n%s", r.listener.log())
			r.t.Logf("listener_reports holds %v; this daemon's project token is %q and its project key is %q",
				r.listenerReports(), r.project, r.cfg.Project)
		}
	}()
	restarted := false
	began := time.Now()
	idleRigEventually(r.t, idleRigListenerReportWait, "the daemon reading the Envoy listener's ready report", func() bool {
		var found bool
		if err := r.pool.QueryRow(context.Background(),
			"select exists(select 1 from listener_reports where project = $1)", r.project).Scan(&found); err != nil {
			r.t.Fatalf("read the listener's report: %v", err)
		}
		if found {
			return true
		}
		// A durable consumer this daemon has just created starts at the next message
		// (intake's createConsumer), so the report the listener published at its own start is
		// behind it. The listener is started again on the same port, which publishes a fresh
		// one that the consumer does see and leaves every URL already handed out valid.
		if !restarted && time.Since(began) > 2*time.Second {
			restarted = true
			port := r.listener.port
			r.listener.stop()
			r.listener = startIdleRigListenerOn(r.t, r.natsURL, "idlerig", port)
		}
		return false
	})
}

// issue is the seeded issue the test calls name.
func (r *idleWakeRig) issue(name string) idleRigIssue {
	r.t.Helper()
	for _, issue := range r.issues {
		if issue.name == name {
			return issue
		}
	}
	r.t.Fatalf("the rig seeded no issue called %s", name)
	return idleRigIssue{}
}

// launchAll waits until every seeded issue's pod runs its roles, in order, passing over an issue
// the rig holds back (idleRigSeed.heldBack), and resumes each role the boot does not start
// (launch). It is apart from newIdleRigWith so a caller can do slow work meanwhile (buildLegion),
// and can set the plan of a role the boot does not start before launch resumes it;
// newIdleWakeRig calls it before it returns.
func (r *idleWakeRig) launchAll() {
	r.t.Helper()
	for _, issue := range r.issues {
		if issue.heldBack != nil {
			continue
		}
		r.launch(issue)
	}
}

// idleRigDatabase and idleRigUser are the rig's own database and login, which its launchers'
// session proxies carry the generation's session lookup to.
func idleRigDatabase(t *testing.T, pool *store.Pool) string {
	t.Helper()
	return pool.Config().ConnConfig.Database
}

func idleRigUser(t *testing.T, pool *store.Pool) string {
	t.Helper()
	return pool.Config().ConnConfig.User
}

// idleRigSessionPassword puts the rig's database password where a launcher's own session client
// finds it, libpq's PGPASSWORD, which is the only place left for one: that client builds its URL
// from the endpoint alone (ompsessions.Endpoint.ClientURL), which carries no password, and the
// generation's proxy is a byte copy that hands the client's startup packet to the server
// unchanged, so a server that asks for one refuses the login (`failed SASL auth`, SQLSTATE
// 28P01) however the proxy's upstream was dialed. A pod's Cloud SQL asks for none — the
// connector mints the login as it dials and no password is ever written into a pod
// (internal/sessionproxy) — so this is the rig's stand-in for that identity and no production
// path reads it, as internal/launcher's sessionDatabaseAt is for that package's tests. A
// trust-authenticated server, which this repository's own pod serves, names no password, and an
// empty PGPASSWORD is no setting at all (pgx's parseEnvSettings keeps only what is non-empty),
// so the rig runs the same against either.
func idleRigSessionPassword(t *testing.T, pool *store.Pool) {
	t.Helper()
	t.Setenv("PGPASSWORD", pool.Config().ConnConfig.Password)
}

// idleRigHostPort is the rig's Postgres as a dialable address, which the session proxy's upstream
// dialer connects to.
func idleRigHostPort(t *testing.T, pool *store.Pool) string {
	t.Helper()
	conn := pool.Config().ConnConfig
	return conn.Host + ":" + strconv.Itoa(int(conn.Port))
}

// address is the worker stream the daemon bound, which every launcher dials.
func (r *idleWakeRig) address() string {
	r.built.mu.Lock()
	defer r.built.mu.Unlock()
	return r.built.address
}

// idleRigControllerPod is what the rig calls the project controller's pod among its role processes
// (idleRigAgents' keys): a controller's role is given no issue to name its pod by, only
// LEGION_CONTROLLER (sandbox's controllerPod.agentEnv), and a project runs one.
const idleRigControllerPod = "controller"

// podReady starts the pod's launchers, for the roles the rig gave the issue a claim of — or the
// controller's, for the project controller's pod — once the pod's workspace is provisioned where
// the rig makes workspaces, and records where each launcher writes its generations so the role
// process can be handed its boot token. The pod of an issue the rig holds back
// (idleRigSeed.heldBack) readies nothing and starts no launcher until the test lets it start
// (letStart), or at all when the pod goes first.
func (r *idleWakeRig) podReady(cluster *issueSandboxAPI, pod, uid string, roles map[string]string) {
	key := pod
	var issue *idleRigIssue
	for i := range r.issues {
		if r.issues[i].pod == pod {
			issue = &r.issues[i]
			key = issue.key
		}
	}
	keep := []claim.Role{claim.RoleImplementer}
	switch {
	case issue != nil:
		keep = issue.roles
	case pod == sandbox.SandboxName(claim.ControllerToken(r.project)):
		key, keep = idleRigControllerPod, []claim.Role{claim.RoleController}
	}
	wanted := map[string]string{}
	for _, role := range keep {
		if token, ok := roles[string(role)]; ok {
			wanted[string(role)] = token
		}
	}
	var prepare func(context.Context) error
	if r.work != nil && issue != nil {
		disk, issueKey := cluster.diskID(pod), issue.key
		prepare = func(ctx context.Context) error { return r.work.provision(ctx, cluster.prefix, disk, pod, issueKey) }
	}
	if issue != nil && issue.heldBack != nil {
		ready, heldBack := prepare, issue.heldBack
		prepare = func(ctx context.Context) error {
			select {
			case <-heldBack:
			case <-ctx.Done():
				return ctx.Err()
			}
			if ready == nil {
				return nil
			}
			return ready(ctx)
		}
	}
	r.pods.podReady(cluster.prefix, pod, uid, wanted, prepare, func(role, dir string) { r.agents.setPrivate(key, role, dir) })
}

// podGone stops the launchers of the cluster's pod, and its workspace is no live pod's any more.
func (r *idleWakeRig) podGone(cluster *issueSandboxAPI, pod string) {
	r.pods.podGone(cluster.prefix, pod)
	if r.work != nil {
		r.work.podGone(cluster.prefix, pod)
	}
}

// seed records one issue, its tree's lifecycle and a suspended claim of each of its roles, as
// check_wake_test.go's rig seeds a claim the daemon will resume. The issue is its own tree root,
// in the workflow at `implementing`, so the idle rule reads a live tree and the backstop has a
// phase worker to arm for (workflow.BackstopRole); the daemon's boot starts its implementer, the
// phase's owner, and a role of any other kind waits for the operator's resume (launch). The
// claims of an issue the rig holds back (idleRigSeed.heldBack) are suspended by an operator, which
// no boot starts.
func (r *idleWakeRig) seed(seed idleRigSeed, key string, index int) idleRigIssue {
	r.t.Helper()
	r.board.show(key, "in_progress")
	ctx := context.Background()
	st, err := store.Open(ctx, r.cfg.PostgresDSN)
	if err != nil {
		r.t.Fatalf("open the store: %v", err)
	}
	defer st.Close()
	if err := pgx.BeginFunc(ctx, st.Pool(), func(tx pgx.Tx) error {
		if err := r.records.PutIssue(ctx, tx, record.Issue{
			Key: key, Repo: liveRepo, DefaultBranch: "main", GitHubRepo: liveRepo, Project: r.cfg.Project,
			Tree: key, Title: "An idle pod", Phase: phase.Implementing, Generation: 1, Status: "in_progress",
			HandedOver: true, Rank: "U",
		}); err != nil {
			return err
		}
		if err := r.records.PutSlot(ctx, tx, record.Slot{Issue: key, Index: index, AdmittedAt: time.Now()}); err != nil {
			return err
		}
		if !seed.pull {
			return nil
		}
		return r.records.PutPullRequest(ctx, tx, record.PullRequest{
			Issue: key, Repo: liveRepo, Number: idleRigPullNumber(key), Branch: "legion/" + key,
			HeadSHA: idleRigHead(key), HeadUpdatedAt: time.Now().Add(-time.Hour).UTC(), State: record.PullRequestOpen,
		})
	}); err != nil {
		r.t.Fatalf("record issue %s: %v", key, err)
	}
	if _, err := st.OpenTreeLifecycle(ctx, r.project, key, treelifecycle.AuthorityWorkflow); err != nil {
		r.t.Fatalf("open the lifecycle of %s: %v", key, err)
	}
	suspendedBy := supervise.SuspendedByDaemon
	if seed.heldBack {
		suspendedBy = supervise.SuspendedByOperator
	}
	var pod string
	for _, role := range seed.roles {
		token, err := claim.NewToken(r.project, key, role)
		if err != nil {
			r.t.Fatal(err)
		}
		if _, err := st.AdmitClaim(ctx, supervise.Claim{
			Token: token, Project: r.project, Tree: key, Issue: key, Role: role, Generation: 1,
			State: supervise.StateSuspended, SuspendedBy: suspendedBy,
		}); err != nil {
			r.t.Fatalf("record the suspended %s of %s: %v", role, key, err)
		}
		writePrompt(r.t, r.cfg, token)
		if seed.plan != nil {
			r.agents.setPlan(key, string(role), *seed.plan)
		}
		pod = sandbox.SandboxName(token)
	}
	issue := idleRigIssue{name: seed.name, key: key, pod: pod, role: seed.roles[0], roles: slices.Clone(seed.roles)}
	if seed.heldBack {
		issue.heldBack = make(chan struct{})
	}
	return issue
}

// idleRigPullNumber is the pull request number of an issue, derived from its key so each issue has
// one of its own.
func idleRigPullNumber(key string) int {
	sum := 0
	for _, b := range []byte(key) {
		sum = (sum*31 + int(b)) % 9000
	}
	return 1000 + sum
}

// idleRigHead is the commit at the head of an issue's pull request, which its CI settlement names.
func idleRigHead(key string) string {
	head := ""
	for len(head) < 40 {
		head += fmt.Sprintf("%x", key)
	}
	return head[:40]
}

// boot runs the daemon over the rig until the test ends, returning once it answers /healthz. The
// runtime it is given is the real Sandbox runtime over the rig's Kubernetes API.
func (r *idleWakeRig) boot() {
	r.t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	// The log's writer is taken here, on the test's own goroutine, and not in the goroutine
	// below. Taking it registers the cleanup that prints the log's tail, and cleanups run in the
	// reverse of the order they were registered in: registered before r.stopDaemon below, the
	// tail is printed after the daemon has stopped, so it carries the shutdown's own lines and it
	// sees a failure the stop itself raised. Taken in the goroutine, it would usually be
	// registered after r.stopDaemon and so run before it, printing a log that ends where the
	// shutdown begins, or none at all when the stop is what failed.
	logger := slog.New(slog.NewJSONHandler(io.MultiWriter(r.logs, r.logFile()), &slog.HandlerOptions{Level: slog.LevelDebug}))
	o := overrides{
		listen: heldListen, runtime: r.newRuntime, clock: stillClock{}, idleTick: idleRigTick, orphanSweep: r.options.orphanSweep,
		entrySweep:     r.options.entrySweep,
		workflowTokens: idleRigAppTokens{workflowTokenRecorder: &workflowTokenRecorder{}}, githubAPI: repositoriesGitHub(r.owner()),
	}
	if r.options.wallClock {
		o.clock = nil
	}
	go func() {
		done <- run(ctx, r.cfg, logger, o)
	}()
	r.stopMu.Lock()
	r.stop, r.done = cancel, done
	r.stopMu.Unlock()
	r.owner().Cleanup(r.stopDaemon)
	awaitHealthz(r.t, r.cfg, done)
	transport := &http.Transport{}
	r.owner().Cleanup(transport.CloseIdleConnections)
	r.api = &daemon{t: r.t, cfg: r.cfg, transport: transport, client: &http.Client{Transport: transport, Timeout: 150 * time.Second},
		base: "http://127.0.0.1:" + strconv.Itoa(r.cfg.Port)}
	r.api.stopped.Store(true)
}

// owner is the test the rig was made for, which every cleanup of the rig is registered on: a step
// of that test run as a subtest of its own (step) is where a failure is reported, but what the
// step starts — a daemon, a listener's connections, a log — outlives it.
func (r *idleWakeRig) owner() *testing.T {
	if r.root != nil {
		return r.root
	}
	return r.t
}

// step runs one step of the test that made the rig as a named subtest of it, with the rig's waits
// and reads failing that subtest. What the step starts is still the owner's, and outlives it.
func (r *idleWakeRig) step(name string, f func(t *testing.T)) bool {
	owner := r.owner()
	return owner.Run(name, func(t *testing.T) {
		r.root, r.t, r.api.t = owner, t, t
		defer func() { r.t, r.api.t = owner, owner }()
		f(t)
	})
}

// logFile keeps a copy of the daemon's log beside the test, under the test's own temporary
// directory, which Go removes when the test ends whether it passed or failed: a finished run
// leaves no log behind. The file is still worth naming while the test runs, and the cleanup
// closes it and, when the test failed, prints the end of it into the test's own output, which Go
// shows for a failing test. That is where a failure's diagnosis is read now, the file itself
// being gone by the time anyone looks. It is called on the test's own goroutine, because the
// order its cleanup runs in is the order that call is made in (boot).
func (r *idleWakeRig) logFile() io.Writer {
	r.t.Helper()
	if r.daemonLog == nil {
		// TempDir registers the directory's removal the first time it is called, and cleanups run
		// in the reverse of the order they were registered in, so the cleanup below is registered
		// after this call: it reads the file while the directory holding it is still there.
		file, err := os.CreateTemp(r.t.TempDir(), "lgdaemon-*.log")
		if err != nil {
			r.t.Fatal(err)
		}
		r.t.Cleanup(func() {
			_ = file.Close()
			if !r.t.Failed() {
				return
			}
			tail, omitted, err := idleRigLogTail(file.Name())
			switch {
			case err != nil:
				r.t.Logf("the daemon's log %s could not be read back: %v", file.Name(), err)
			case tail == "":
				r.t.Logf("the daemon's log %s is empty: the daemon wrote nothing to it", file.Name())
			case omitted > 0:
				r.t.Logf("the daemon's log, its last %d bytes, %d earlier bytes left out; the rig's own diagnosis prints the last 40 lines of this same log, which are the end of what follows:\n%s",
					len(tail), omitted, tail)
			default:
				r.t.Logf("the daemon's log, whole, %d bytes:\n%s", len(tail), tail)
			}
		})
		r.t.Logf("the daemon's log: %s, a file the run removes when the test ends; a test that fails prints its last %d bytes instead",
			file.Name(), idleRigLogTailBytes)
		r.daemonLog = file
	}
	return r.daemonLog
}

// idleRigLogTailBytes is how much of the daemon's log a failing test prints. The whole log of a
// long run is a quarter of a megabyte, which nobody reads in CI output, while the last 32 KiB is
// the stretch the failure happened in and is still several times the 40 lines diagnose prints of
// the same log, so the two read as a wide window and the close one inside it.
const idleRigLogTailBytes = 32 << 10

// idleRigLogTail reads the last idleRigLogTailBytes of the named file, returning them and how
// many bytes it left out before them. It starts after the first newline in that window, so the
// text begins with a whole log line rather than half of one, and the count it returns says how
// much of the log the reader is not being shown.
func idleRigLogTail(name string) (tail string, omitted int64, err error) {
	file, err := os.Open(name)
	if err != nil {
		return "", 0, err
	}
	defer func() { _ = file.Close() }()
	size, err := file.Seek(0, io.SeekEnd)
	if err != nil {
		return "", 0, err
	}
	at := size - idleRigLogTailBytes
	if at < 0 {
		at = 0
	}
	window := make([]byte, size-at)
	if _, err := file.ReadAt(window, at); err != nil {
		return "", 0, err
	}
	text := string(window)
	if at > 0 {
		if newline := strings.IndexByte(text, '\n'); newline >= 0 {
			at += int64(newline) + 1
			text = text[newline+1:]
		}
	}
	return text, at, nil
}

// stopDaemon stops the running daemon, if one runs, and waits for it.
func (r *idleWakeRig) stopDaemon() {
	r.stopMu.Lock()
	stop, done := r.stop, r.done
	r.stop, r.done = nil, nil
	r.stopMu.Unlock()
	if stop == nil {
		return
	}
	stop()
	select {
	case err := <-done:
		if err != nil {
			r.t.Errorf("run: %v", err)
		}
	case <-time.After(30 * time.Second):
		r.t.Error("the daemon did not stop")
	}
}

// restart stops the daemon and boots it again on the same store, as a deployment's restart does.
// The wakes the listener reported while it was down are on its durable consumer, which is what
// makes them wait for it rather than be lost. A rig that holds its ports (idleRigOptions.holdStream)
// boots again on the API port the stopped daemon served, which its role processes still name;
// any other boots on a fresh one.
func (r *idleWakeRig) restart() {
	r.t.Helper()
	r.stopDaemon()
	if r.options.holdStream {
		holdPort(r.t, r.owner(), "127.0.0.1", r.cfg.Port)
	} else {
		rebindHeldPorts(r.t, &r.cfg)
	}
	r.boot()
}

// newRuntime is the runtime factory the daemon boots with: the real Sandbox runtime
// (sandbox.New) over the rig's Kubernetes API, with the worker stream, store, connection
// directory and project release mark the boot hands it, and the rig's own tools — a `legion`
// that is the role process stand-in, and an agent command it never has to run.
func (r *idleWakeRig) newRuntime(ctx context.Context, listener *stream.Listener, address string, tokens appauth.Source,
	_ func(context.Context, string) (ghrepo.Repository, string, error), st *store.Store, _ func() []ghrepo.Repository,
	held func() bool,
) (legionruntime.Runtime, error) {
	r.built.mu.Lock()
	r.built.address = address
	r.built.mu.Unlock()
	k := *r.cfg.Runtime.Kubernetes
	opts, err := sandboxOptions(r.cfg, k, r.project, "placeholder-dispatch-token", lookup(nil), slog.New(slog.NewJSONHandler(r.logs, nil)))
	if err != nil {
		return nil, fmt.Errorf("sandboxOptions: %w", err)
	}
	tools := r.toolsDir()
	opts.Conns, opts.StreamURL, opts.Store, opts.ProjectReleaseHolds = listener, address, st, held
	// The two halves of a release's saved work, as the daemon's own factory hands them to the
	// runtime it builds (sandboxRuntime): what the launcher of the pod that replaces a released
	// one is sent before any role of that pod starts, and what the launcher's confirmation
	// drops. Without them no pod of this rig is ever offered a bundle. They cost a rig whose
	// releases save none one indexed read per role start, which answers no row: the save of a
	// workspace with nothing unpushed stores nothing and deletes what the issue had
	// (outbox.bundleIssueWorkspace), and this rig's role processes bundle an empty workspace.
	opts.SavedWorkspaceBundle, opts.WorkspaceBundleRestored = savedWorkspaceBundle(st, opts.Project), st.DeleteRestoredWorkspaceBundle
	opts.Tokens = idleRigTokens{}
	opts.GitHubCredential = func(_ context.Context, role claim.Role, repo ghrepo.Repository) (ghconfig.Bundle, error) {
		rendered := ghconfig.Render("ghs_idle_rig", string(appauth.AppRoleFor(role)), time.Now().Add(time.Hour))
		owner := strings.ToLower(repo.Owner())
		return ghconfig.Bundle{Default: owner, Dirs: map[string]ghconfig.Rendered{owner: rendered}}, nil
	}
	// The controller's own bundle, which its launch and its refresh both ask for by owner
	// (sandbox.Options.ControllerCredential, appauth.ControllerCredential): the review App's, over
	// liveRepo's owner, the rig's one.
	opts.ControllerCredential = func(_ context.Context, defaultOwner string) (ghconfig.Bundle, error) {
		rendered := ghconfig.Render("ghs_idle_rig", string(appauth.Review), time.Now().Add(time.Hour))
		owner := strings.Split(liveRepo, "/")[0]
		if defaultOwner != "" {
			owner = defaultOwner
		}
		return ghconfig.Bundle{Default: owner, Dirs: map[string]ghconfig.Rendered{owner: rendered}}, nil
	}
	// The directories the pods' gh is built from, which sandbox.New now requires
	// (checkSupervision) and the runtime reads a running pod's gh projection against: the rig's
	// issues are all on liveRepo, so its owner's directory is the whole set.
	opts.GitHubDirs = func(ghrepo.Repository, []appauth.AppRole) []string {
		return []string{strings.Split(liveRepo, "/")[0]}
	}
	opts.CacheRepositories = func() []ghrepo.Repository { return nil }
	opts.Tools.Legion = idleRigLegionTool(r.t, tools)
	opts.Agent = idleRigAgentTool(r.t, tools)
	opts.DaemonURL = r.cfg.DaemonURL
	opts.EnvoyURL = r.cfg.EnvoyURL
	// The runtime reaches the cluster the configuration's kubeconfig names, through kubeClient as
	// the daemon's own boot reaches it (readSandbox), so aiming the rig's configuration at another
	// cluster (aimAt) is what moves the daemon. The stand-in decodes and encodes JSON alone
	// (sandboxRESTConfig).
	restConfig, err := kubeClient(k)
	if err != nil {
		return nil, err
	}
	restConfig.ContentType, restConfig.AcceptContentTypes = "application/json", "application/json"
	rt, err := sandbox.New(ctx, restConfig, opts)
	if err != nil {
		return nil, fmt.Errorf("sandbox.New: %w", err)
	}
	listener.SetLauncherResolver(rt.LauncherResolver())
	_ = tokens
	return rt, nil
}

// toolsDir holds the rig's `legion` and `omp` stand-ins, made once for the test. The longest
// path written under it is the `legion` stand-in (idleRigLegionTool).
func (r *idleWakeRig) toolsDir() string {
	r.t.Helper()
	r.stopMu.Lock()
	defer r.stopMu.Unlock()
	if r.toolsPath == "" {
		r.toolsPath = testtmp.Dir(r.t, "legion")
	}
	return r.toolsPath
}

// idleRigAppTokens is the package's workflow token source with one thing filled in: each lease's
// git identity carries an email as well as a login. The daemon asks a role's shim to adopt the
// working copy under that identity before it prompts it, and shimwire refuses an
// `adopt-working-copy` whose identity has no email ("adopt-working-copy carries no jj identity"),
// which loses every prompt. A real App's lease carries `<id>+<login>@users.noreply.github.com`
// (appauth.GitIdentity).
type idleRigAppTokens struct{ *workflowTokenRecorder }

func (t idleRigAppTokens) Token(ctx context.Context, role appauth.AppRole, owner string) (appauth.Lease, error) {
	lease, err := t.workflowTokenRecorder.Token(ctx, role, owner)
	if err != nil {
		return lease, err
	}
	lease.Identity.Email = "1+" + lease.Identity.Name + "@users.noreply.github.com"
	return lease, nil
}

// idleRigTokens mints the provisioning token a pod's workspace fetch clones with; a public
// repository needs none, and no pod of this rig clones anything.
type idleRigTokens struct{}

func (idleRigTokens) Token(context.Context, ghrepo.Repository) (string, error) {
	return "idle-rig-provision", nil
}

// launch waits until the issue's pod runs each of its roles, registered, ready and having told the
// daemon it runs no background work: the state the idle rule releases. The daemon starts the
// implementer of an admitted issue whose phase is its own, so nothing here asks for that launch; a
// claim still suspended — another role, which no boot starts, or one a boot left suspended — is
// nudged with the operator's resume.
func (r *idleWakeRig) launch(issue idleRigIssue) {
	r.t.Helper()
	defer func() {
		if r.t.Failed() {
			r.t.Log(r.diagnose())
		}
	}()
	nudged := map[claim.Role]time.Time{}
	idleRigEventually(r.t, idleRigPodWait, "the pod of "+issue.key+" running its roles, registered and ready", func() bool {
		claims := r.api.claims()
		up := true
		for _, role := range issue.roles {
			token := r.roleToken(issue, role)
			state := listedClaim(r.t, claims, token)
			if idleRigWorking(state.State) && r.agents.role(issue.key, string(role)).Registered > 0 {
				continue
			}
			up = false
			if state.State == string(supervise.StateSuspended) && time.Since(nudged[role]) > 3*time.Second {
				nudged[role] = time.Now()
				r.api.request(http.MethodPost, "/legion/v1/operator/claims/"+string(token)+"/resume", nil, true)
			}
		}
		return up
	})
}

// diagnose is what a failure of the rig prints: the launchers' own log and the last of the
// daemon's, which is where a pod that never came up says why.
func (r *idleWakeRig) diagnose() string {
	daemonLog := strings.Split(strings.TrimSpace(r.logs.String()), "\n")
	if len(daemonLog) > 40 {
		daemonLog = daemonLog[len(daemonLog)-40:]
	}
	return "launchers:\n" + r.pods.log() + "\ndaemon:\n" + strings.Join(daemonLog, "\n")
}

// idleRigWorking is whether a claim's state is one a live role process is behind: waiting for
// work, in a turn, or between turns. A woken role passes through all three, and its pod becomes
// idle again a minute later, so a wait on one of them alone would miss it.
func idleRigWorking(state string) bool {
	switch supervise.ClaimState(state) {
	case supervise.StateReady, supervise.StateWorking, supervise.StateIdle:
		return true
	}
	return false
}

// token is the claim token of the issue's role, the one that owns its phase.
func (r *idleWakeRig) token(issue idleRigIssue) claim.Token {
	r.t.Helper()
	return r.roleToken(issue, issue.role)
}

// roleToken is the claim token of one role of the issue.
func (r *idleWakeRig) roleToken(issue idleRigIssue, role claim.Role) claim.Token {
	r.t.Helper()
	token, err := claim.NewToken(r.project, issue.key, role)
	if err != nil {
		r.t.Fatal(err)
	}
	return token
}

// markPrompts records how many tasks each role has been given, which newPrompts counts from: the
// daemon's boot starts the implementer of an admitted issue whose phase is its own and gives it
// that phase's task, so a role of the rig has already been prompted once before its pod is ever
// released.
func (r *idleWakeRig) markPrompts() {
	r.baseline = map[string]int{}
	for _, issue := range r.issues {
		r.baseline[issue.key] = len(r.agents.prompts(issue.key, string(issue.role)))
	}
}

// newPrompts is every task the issue's role has been given since markPrompts.
func (r *idleWakeRig) newPrompts(issue idleRigIssue) []idleRigMessage {
	prompts := r.agents.prompts(issue.key, string(issue.role))
	from := r.baseline[issue.key]
	if from > len(prompts) {
		return nil
	}
	return prompts[from:]
}

// adoptionsAhead is every adopt-working-copy frame the issue's role was sent ahead of the task at
// index i of its prompts: after the task before that one was given, and before that one was.
func (r *idleWakeRig) adoptionsAhead(issue idleRigIssue, i int) []idleRigAdoption {
	var ahead []idleRigAdoption
	for _, adoption := range r.agents.adoptions(issue.key, string(issue.role)) {
		if adoption.Ahead == i {
			ahead = append(ahead, adoption)
		}
	}
	return ahead
}

// newAdoptions is every adopt-working-copy frame the issue's role was sent after the tasks
// markPrompts counted: the frames ahead of each task newPrompts holds, and any ahead of a task
// still to come. None of the frames the rig's start brought is among them.
func (r *idleWakeRig) newAdoptions(issue idleRigIssue) []idleRigAdoption {
	var since []idleRigAdoption
	for _, adoption := range r.agents.adoptions(issue.key, string(issue.role)) {
		if adoption.Ahead >= r.baseline[issue.key] {
			since = append(since, adoption)
		}
	}
	return since
}

// quiet back-dates when every claim of the project was last written, so the next pass of the
// daemon's idle tick reads a pod that has done nothing for longer than the idle period. The
// session's own report is left as the session wrote it, because the rule reads it against that
// session's last turn and not against the clock. The rule reads the claim rows and nothing else
// (pods.IdleView), so the rows are the clock it is measured against.
func (r *idleWakeRig) quiet() {
	r.t.Helper()
	if _, err := r.pool.Exec(context.Background(),
		`update claims set updated_at = now() - interval '30 minutes'
		 where project = $1 and activity_at is not null`, r.project); err != nil {
		r.t.Fatalf("back-date the project's claims: %v", err)
	}
}

// idleRigGateAgeing is how often the gate is shut again while a test keeps it shut: a tenth of
// the rig's tick, so the daemon reads a report this test has aged on every pass it makes.
const idleRigGateAgeing = idleRigTick / 10

// keepListenerGateShut ages the project's `listener_reports` row past the window the daemon
// reads it in, which shuts the one gate the idle release passes before it takes a pod
// (workflowRuntime.listenerLive, listenerReadyWindow), and goes on ageing it until the test
// ends. The release of a pod no claim owns runs outside that gate, so a pass made with the gate
// shut is the one case that tells the two rules apart: it releases a pod no role can use and
// holds every idle one.
//
// The ageing runs on a goroutine of its own, and that is what makes it sound. The rig runs a
// real Envoy listener and cannot leave it out — the listener owns the notification stream the
// daemon binds its consumer on — and that listener publishes a ready report every five minutes
// into this very row, while the daemon decides every pod on a ticker of its own (idleRigTick).
// Ageing the row on the test's own poll turns would leave the gate open from the moment a
// report lands until the next turn, which is long enough for one idle pass to take the pods a
// test has just waited to make idle-release candidates. Every idleRigGateAgeing the gate is
// shut again instead, which no tick of the daemon fits inside. The ageing is the one quiet
// makes of the claim rows: the daemon reads this row against its own clock and nothing else.
func (r *idleWakeRig) keepListenerGateShut() {
	r.t.Helper()
	aged, err := r.ageListenerReport()
	if err != nil {
		r.t.Fatalf("age the Envoy listener's report of %s: %v", r.project, err)
	}
	if aged == 0 {
		r.t.Fatalf("there is no listener report of %s to age: the daemon has read none, so a pass made now is held by a gate this test did not shut",
			r.project)
	}
	stop, stopped := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(stopped)
		ageing := time.NewTicker(idleRigGateAgeing)
		defer ageing.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ageing.C:
				if _, err := r.ageListenerReport(); err != nil {
					r.t.Errorf("age the Envoy listener's report of %s: %v", r.project, err)
					return
				}
			}
		}
	}()
	// The pool is closed by a cleanup the rig's own boot registered, which runs after this one.
	r.t.Cleanup(func() {
		close(stop)
		<-stopped
	})
}

// ageListenerReport back-dates the project's listener report and answers how many rows it moved,
// which is zero while the daemon has read no report at all. It is the one statement behind the
// gate, run from the test's own goroutine and from the one that keeps the gate shut, so it
// fails nothing itself: a `Fatalf` off the test's goroutine would stop the wrong one.
func (r *idleWakeRig) ageListenerReport() (int64, error) {
	aged, err := r.pool.Exec(context.Background(),
		`update listener_reports set ready_at = now() - interval '30 minutes' where project = $1`, r.project)
	if err != nil {
		return 0, err
	}
	return aged.RowsAffected(), nil
}

// awaitReleased waits for every issue's claim to be suspended with the source `released` and its
// pod, and the pod's Sandbox, to be gone. One pass of the daemon's own idle tick decides them
// all, so they are waited for together.
//
// A failure names where every pod of the rig stands, beside the launchers' log and the daemon's.
// The pass this waits for walks the claims that are not already failed, retired or suspended
// (store.IdlePods) and logs a line for each pod it decides, so a claim that never reached
// `released` and a claim released whose pod outlived it are two different faults that leave the
// daemon's log equally silent, and the wait running out cannot tell them apart on its own.
func (r *idleWakeRig) awaitReleased(issues ...idleRigIssue) {
	r.t.Helper()
	seen := make([]string, len(issues))
	defer func() {
		if r.t.Failed() {
			r.t.Logf("the pods of the rig, as the wait last read them:\n%s", strings.Join(seen, "\n"))
			r.t.Log(r.diagnose())
		}
	}()
	var quieted time.Time
	// The span is the rig's longest (idleRigReleaseWait): this is every launch undone at once.
	idleRigEventually(r.t, idleRigReleaseWait, "every pod of the rig released", func() bool {
		// The pods are back-dated rather than waited out: the idle rule reads the claim rows and
		// nothing else (pods.IdleView), so those rows are the clock it is measured against.
		if time.Since(quieted) > 2*time.Second {
			quieted = time.Now()
			r.quiet()
		}
		// One listing for every pod of the rig, not one each: the operator's route answers every
		// claim at once (daemon.claims, which daemon.claim reads to find one), and the pass this
		// waits for reads the same rows through the same pool, so a wait that asked once for each
		// pod every poll would be taking the daemon's own Postgres away from the release it is
		// waiting for.
		claims := r.api.claims()
		released := true
		for i, issue := range issues {
			state := listedClaim(r.t, claims, r.token(issue))
			pod, sandbox := r.kube.pod(issue.pod), r.kube.sandbox(issue.pod) != nil
			seen[i] = fmt.Sprintf("%s: claim %s (suspended by %q), pod %t, sandbox %t",
				issue.key, state.State, state.SuspendedBy, pod, sandbox)
			if state.State != string(supervise.StateSuspended) || state.SuspendedBy != string(supervise.SuspendedByReleased) ||
				pod || sandbox {
				released = false
			}
		}
		return released
	})
}

// claimLines is every claim of the issue as the operator's listing answers it, one line each in
// token order. It is what a release that suspends no role must leave exactly as it found it, so
// it carries what such a release could change and nothing that moves on its own: the claim's
// state and who suspended it, the session it records, whether it records a process, and the
// task it holds.
func (r *idleWakeRig) claimLines(issue idleRigIssue) []string {
	r.t.Helper()
	var lines []string
	for _, c := range r.api.claims() {
		if c.Issue != issue.key {
			continue
		}
		task := ""
		if c.Pending != nil {
			task = c.Pending.ID
		}
		lines = append(lines, fmt.Sprintf("%s: %s by %q, session %q, process %t, task %q",
			c.Token, c.State, c.SuspendedBy, c.Session, c.Locator != nil, task))
	}
	slices.Sort(lines)
	return lines
}

// awaitUnownedReleased waits for the issue's pod, that pod's disk and its Sandbox to be gone
// while every claim of the issue stays exactly as it was. It is the wait of the second release
// rule (workflow.UnownedPodRelease), beside awaitReleased: that one walks the claims a release
// suspended, and this release suspends none — there is no live claim to stop, so no claim state
// is a thing to wait for and the claims are what must not move.
//
// The claim rows are back-dated every couple of seconds, for the reason awaitReleased
// back-dates them: they are the clock the daemon reads the quiet period against. The listener's
// report is the other clock, and the test's own goroutine keeps it aged for as long as the test
// runs (keepListenerGateShut), so this wait does nothing about it. A failure names where the
// pod stood when the wait last read it, beside the launchers' log and the daemon's.
func (r *idleWakeRig) awaitUnownedReleased(issue idleRigIssue) {
	r.t.Helper()
	before := r.claimLines(issue)
	seen := "never read"
	defer func() {
		if r.t.Failed() {
			r.t.Logf("the pod of %s, as the wait last read it: %s", issue.key, seen)
			r.t.Log(r.diagnose())
		}
	}()
	var quieted time.Time
	idleRigEventually(r.t, idleRigReleaseWait, "the pod of "+issue.key+" released with every claim of it left as it was", func() bool {
		if time.Since(quieted) > 2*time.Second {
			quieted = time.Now()
			r.quiet()
		}
		if got := r.claimLines(issue); !slices.Equal(got, before) {
			r.t.Fatalf("the claims of %s read\n  %s\nwant the ones it stood at\n  %s\nthis release stops no role and changes no claim",
				issue.key, strings.Join(got, "\n  "), strings.Join(before, "\n  "))
		}
		pod, sandbox, disk := r.kube.pod(issue.pod), r.kube.sandbox(issue.pod) != nil, r.kube.disk(issue.pod)
		seen = fmt.Sprintf("pod %t, sandbox %t, disk %t", pod, sandbox, disk)
		return !pod && !sandbox && !disk
	})
}

// awaitResumed waits for the issue's role to be back: its claim live again, a second pod started
// for it, and its role process registered again.
func (r *idleWakeRig) awaitResumed(issue idleRigIssue, registrations int) {
	r.t.Helper()
	token := r.token(issue)
	idleRigEventually(r.t, idleRigPodWait, "the pod of "+issue.key+" back with its role registered again", func() bool {
		return r.agents.role(issue.key, string(issue.role)).Registered >= registrations &&
			idleRigWorking(r.api.claim(token).State)
	})
}

// retry is the operator's `legion claims retry` of the issue's role, over the route that command
// posts to: POST /legion/v1/operator/claims/{token}/retry with the body that says which retry it
// is (api.RetryRequest). fresh is `--fresh-session`, which has the claim leave the session it
// recorded behind and come back in a new one; a retry without it resumes the recorded session, or
// a new one once supervise's Machine.forgetSession has dropped it.
// A phase worker's retry is its issue's phase started again, which answers 202: the start it
// queues is an outbox row that runs behind the answer (api's restartWorkedPhase).
func (r *idleWakeRig) retry(issue idleRigIssue, fresh bool) {
	r.t.Helper()
	token := r.token(issue)
	status, body := r.api.request(http.MethodPost, "/legion/v1/operator/claims/"+string(token)+"/retry",
		api.RetryRequest{FreshSession: fresh}, true)
	if status != http.StatusOK && status != http.StatusAccepted {
		r.t.Fatalf("the operator's retry of %s = %d %s", token, status, body)
	}
}

// publish puts one Envoy envelope from source on the stream the daemon consumes.
func (r *idleWakeRig) publish(subject, source, eventID, payload string) {
	r.t.Helper()
	data, err := json.Marshal(map[string]any{
		"event_id": eventID, "source": source, "source_event_id": eventID + "-source", "topic": subject,
		"dedupe_key": eventID, "issued_at": time.Now().UnixMilli(), "payload_summary": "idle rig event",
		"payload": payload, "trace_id": eventID + "-trace",
	})
	if err != nil {
		r.t.Fatalf("encode the %s envelope: %v", subject, err)
	}
	if _, err := r.js.Publish(context.Background(), subject, data); err != nil {
		r.t.Fatalf("publish %s: %v", subject, err)
	}
}

// publishSettlement publishes the CI settlement of the issue's pull request head, as Envoy relays
// GitHub's check report.
func (r *idleWakeRig) publishSettlement(issue idleRigIssue) {
	r.t.Helper()
	group := func() map[string]any { return map[string]any{"names": []string{}, "truncated": false} }
	payload, err := json.Marshal(map[string]any{
		"kind": "checks", "repo": liveRepo, "number": strconv.Itoa(idleRigPullNumber(issue.key)), "sha": idleRigHead(issue.key),
		"check_runs": []map[string]any{{"name": "build", "id": 1}}, "generation": 1,
		"snapshot": issue.key + "-settled", "settled_at": time.Now().UnixMilli(),
		"failed": group(), "passed": group(), "running": group(), "queued": group(), "cancelled": group(), "skipped": group(),
	})
	if err != nil {
		r.t.Fatalf("encode the settlement payload: %v", err)
	}
	subject := fmt.Sprintf("notifications.github.%s.pr.%d.checks",
		strings.ReplaceAll(liveRepo, "/", "."), idleRigPullNumber(issue.key))
	r.publish(subject, "github", "checks-"+issue.key, string(payload))
}

// releaseRecords is the deployment's record of every release, resume and hold, read through the
// route a claim of the pod reads them on (POST /legion/v1/claims/release-records).
func (r *idleWakeRig) releaseRecords(issue idleRigIssue) api.ReleaseRecordsResponse {
	r.t.Helper()
	seen := r.agents.role(issue.key, string(issue.role))
	status, body := r.api.request(http.MethodPost, "/legion/v1/claims/release-records",
		api.ReleaseRecordsRequest{ClaimToken: claim.Token(seen.Claim), Secret: seen.Secret,
			Since: time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)}, false)
	if status != http.StatusOK {
		r.t.Fatalf("read the release records = %d %s", status, body)
	}
	var records api.ReleaseRecordsResponse
	if err := json.Unmarshal(body, &records); err != nil {
		r.t.Fatalf("decode the release records: %v", err)
	}
	return records
}

// logged is how many of the daemon's log lines say msg with each of attributes, as
// check_wake_test.go's rig reads them.
func (r *idleWakeRig) logged(msg string, attributes map[string]string) int {
	count := 0
	for _, entry := range r.loggedEntries(msg) {
		matches := true
		for key, want := range attributes {
			matches = matches && fmt.Sprint(entry[key]) == want
		}
		if matches {
			count++
		}
	}
	return count
}

// loggedEntries is every line of the daemon's log that says msg, decoded, read in one pass over
// the log. A line that does not decode as JSON is skipped.
func (r *idleWakeRig) loggedEntries(msg string) []map[string]any {
	var entries []map[string]any
	for _, line := range strings.Split(r.logs.String(), "\n") {
		if line == "" {
			continue
		}
		var entry map[string]any
		if json.Unmarshal([]byte(line), &entry) != nil {
			continue
		}
		if entry["msg"] == msg {
			entries = append(entries, entry)
		}
	}
	return entries
}

// idleRigDeadlineMargin is how long before the test binary's own deadline a wait of the rig
// gives up, so a failure names what it waited for rather than arriving as the binary's panic. It
// is the second testwait.Eventually leaves (internal/testwait), widened for the launchers' log
// and the daemon's, which this rig prints on its way out.
const idleRigDeadlineMargin = 5 * time.Second

// Every wait of this rig carries a span of its own, one of those below, and idleRigWaiting
// clamps it by the run's own deadline. The spans are what the work behind each wait takes on a
// loaded runner with every pod of the rig and a launcher for each, a nats-server, a Postgres and
// an Envoy listener on it; they are the bound a fault is reported at, so a release that never
// happens is a failure naming the pods minutes into the run and not at the binary's timeout
// panic, which would take the rest of internal/daemon with it.
const (
	// idleRigListenerStartWait bounds the Envoy listener's own process coming up far enough to
	// answer /healthz: one binary started and one port bound, with nothing of the daemon behind
	// it.
	idleRigListenerStartWait = 60 * time.Second
	// idleRigListenerReportWait bounds the daemon's read of that listener's ready report, which
	// is the listener's publish, NATS, the daemon's durable consumer and the row it writes; a
	// listener that came up before the consumer existed is started again, so the span covers
	// two of those starts.
	idleRigListenerReportWait = 90 * time.Second
	// idleRigPodWait bounds a pod up and running its role: the Sandbox, its launcher, the role
	// process, its registration and its first report. It bounds a pod coming back for a wake
	// too, which is the same work again, and a send that is retried until a live session takes
	// it, which waits on that same pod.
	idleRigPodWait = 150 * time.Second
	// idleRigReleaseWait bounds the release of every pod of the rig by one pass of the idle
	// tick: a launch undone for every pod of the rig at once, and so the longest span here.
	idleRigReleaseWait = 240 * time.Second
	// idleRigRecordWait bounds a row the daemon writes, a task it gives or a message it
	// delivers once the pod behind it is already up: a tick or two of the rig and the write
	// that follows, with no process start inside it.
	idleRigRecordWait = 60 * time.Second
	// idleRigWarningWait bounds the warning the daemon logs about a resume with no first turn,
	// which its wake pass decides on the tick after the rig back-dates the resume, behind
	// whatever else that pass is deciding for every other pod of the rig.
	idleRigWarningWait = 90 * time.Second
	// idleRigStuckWait bounds the stuck flag reaching the resume's record once that warning is
	// out: the same pass writes both, so this is the write alone.
	idleRigStuckWait = 30 * time.Second
)

// idleRigPoll is how often a wait of the rig asks its condition: the rig's own 250 ms tick rather
// than testwait's ten milliseconds, because a condition here is a listing of the daemon's claims
// over its API and not a read of a variable. Asked a hundred times a second, as testwait's pace
// would ask it, the waiting test takes the runner from the daemon whose work it is waiting for;
// four times a second it does not.
const idleRigPoll = idleRigTick

// idleRigWaiting waits for condition until it holds or the wait gives up, asking it once every
// pace, and fails naming what it waited for. The bound is within from now, or the test binary's
// own deadline less the margin above when that is sooner, which is the clamp testwait.Eventually
// makes (internal/testwait). A run whose remaining deadline is shorter than the span — a package
// `-timeout` smaller than it, or a run far enough in that less than the span is left — fails the
// wait rather than the binary, and a run with no deadline at all (`go test -timeout 0`) fails it
// at the span itself. idleRigEventually and idleRigUntil are this wait, each at the pace its
// callers need.
func idleRigWaiting(t *testing.T, within time.Duration, what string, pace time.Duration, condition func() bool) {
	t.Helper()
	testwait.WithinPolling(t, testwait.Bounded(t, within, idleRigDeadlineMargin), pace, what, condition)
}

// idleRigEventually waits for condition within the given span, failing with what it waited for.
func idleRigEventually(t *testing.T, within time.Duration, what string, condition func() bool) {
	t.Helper()
	idleRigWaiting(t, within, what, idleRigPoll, condition)
}
