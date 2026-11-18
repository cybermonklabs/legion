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
//     internal/testnats, which runs the test's own nats-server in the test process, so the rig
//     needs no Docker;
//   - the real Envoy listener, built from packages/envoy/cmd/listener and run as its own process
//     (idle_rig_listener_test.go).
type idleWakeRig struct {
	t        *testing.T
	cfg      config.Config
	pool     *store.Pool
	natsURL  string
	js       jetstream.JetStream
	board    *liveDispatch
	kube     *issueSandboxAPI
	pods     *idleRigPods
	agents   *idleRigAgents
	listener *idleRigListener
	records  record.Store
	project  string
	logs     *syncBuffer
	built    *built
	api      *daemon
	// obs observes the daemon of the newest boot: the reports a test waits for in place of
	// reading a count the daemon writes later (observer, boot).
	obs *observer
	// passes runs a pass of a periodic loop of the newest boot's daemon, which is the only way a
	// pass of one runs here: the rig's clock fires no timer of the daemon's own (boot).
	passes *passer
	// gateShut is whether every pass this rig runs is to be made with the Envoy listener's
	// report aged out of its window (keepListenerGateShut).
	gateShut bool
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

// idleRigIssue is one issue of the rig: its key, the pod its claims run in, and the role that pod
// runs.
type idleRigIssue struct {
	// name is what the test calls the issue; key is the issue key its project gives it.
	name string
	key  string
	pod  string
	role claim.Role
}

// newIdleWakeRig readies every part and boots the daemon, with one issue per name given. Each
// issue is its own tree root, in the workflow at `implementing` with its implementer's claim
// suspended, which the rig then resumes: that launch is what makes the pod, its launcher and its
// role process.
func newIdleWakeRig(t *testing.T, issues ...string) *idleWakeRig {
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
	cfg.AdmissionCap = len(issues) + 4
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
		kube: newIssueSandboxAPI(), agents: newIdleRigAgents(t, pool), listener: listener,
		records: record.NewStore(), project: project, logs: &syncBuffer{}, built: &built{}}
	rig.pods = newIdleRigPods(t, rig.address, cfg.Runtime.Kubernetes.SessionStore.Database,
		func(context.Context, string) (sessionproxy.Dialer, error) {
			return idleRigDialer{address: idleRigHostPort(t, pool)}, nil
		})
	rig.kube.serve(t)
	// Boot reads runtime.kubernetes' kubeconfig whatever runtime it is given (readSandbox,
	// kubeClient), and with none it reads the pod's in-cluster credentials, so the rig names its
	// own stand-in API: it runs the same way outside a cluster as in one, and never holds a
	// credential of the cluster it runs in. Nothing dials this client, since the runtime the
	// boot is given is the rig's own (prepareSandbox returns before it builds one).
	rig.cfg.Runtime.Kubernetes.Kubeconfig = writeKubeconfig(t, rig.kube.url, "test")
	rig.kube.onPodReady = rig.podReady
	rig.kube.onPodGone = rig.pods.podGone
	rig.kube.run(t)
	t.Setenv(idleRigURLVariable, rig.agents.url)

	for i, name := range issues {
		rig.issues = append(rig.issues, rig.seed(name, cfg.Project+"-"+strconv.Itoa(i+1), i))
	}
	rig.boot()
	rig.awaitListenerReady()
	return rig
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
// listener, which is R7's gate: it releases nothing until it has.
//
// The listener publishes a report at its own start and every five minutes after, and it starts
// before the daemon, which is the order a deployment rolls them in. A durable consumer this
// daemon has just created starts at the next message (intake's createConsumer), so the report of
// the listener's own start is behind it and the next one is five minutes off — longer than any
// run of this rig. The daemon has opened its consumers by the moment it reports itself ready
// (intake.OpenConsumers runs before it serves), so the listener is started again here, on the
// socket the test holds for it, which publishes a fresh report that the consumer does see and
// leaves every URL already handed out valid: the port stays the test's between the two.
func (r *idleWakeRig) awaitListenerReady() {
	r.t.Helper()
	defer func() {
		if r.t.Failed() {
			r.t.Logf("the Envoy listener's log:\n%s", r.listener.log())
			r.t.Logf("listener_reports holds %v; this daemon's project token is %q and its project key is %q",
				r.listenerReports(), r.project, r.cfg.Project)
		}
	}()
	socket, port := r.listener.socket, r.listener.port
	r.listener.stop()
	r.listener = startIdleRigListenerOn(r.t, r.natsURL, "idlerig", socket, port)
	testwait.Eventually(r.t, "the daemon reading the Envoy listener's ready report", func() bool {
		var found bool
		if err := r.pool.QueryRow(context.Background(),
			"select exists(select 1 from listener_reports where project = $1)", r.project).Scan(&found); err != nil {
			r.t.Fatalf("read the listener's report: %v", err)
		}
		return found
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

// launchAll launches every seeded issue's pod, in order. It is apart from the rig's construction
// so a test can tell a role process what to do — the topics it takes with envoy_subscribe, whether
// it takes a turn — before the process starts.
func (r *idleWakeRig) launchAll() {
	r.t.Helper()
	for _, issue := range r.issues {
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

// podReady starts the pod's launchers, for the roles the rig gave the issue a claim, and records
// where each launcher writes its generations so the role process can be handed its boot token.
func (r *idleWakeRig) podReady(pod, uid string, roles map[string]string) {
	key := pod
	for _, issue := range r.issues {
		if issue.pod == pod {
			key = issue.key
		}
	}
	wanted := map[string]string{}
	for role, token := range roles {
		if role == string(claim.RoleImplementer) {
			wanted[role] = token
		}
	}
	r.pods.podReady(pod, uid, wanted, func(role, dir string) { r.agents.setPrivate(key, role, dir) })
}

// seed records one issue, its tree's lifecycle and its implementer's suspended claim, as
// check_wake_test.go's rig seeds a claim the daemon will resume. The issue is its own tree root,
// in the workflow at `implementing`, so the idle rule reads a live tree and the backstop has a
// phase worker to arm for (workflow.BackstopRole).
func (r *idleWakeRig) seed(name, key string, index int) idleRigIssue {
	r.t.Helper()
	r.board.show(key, "in_progress")
	token, err := claim.NewToken(r.project, key, claim.RoleImplementer)
	if err != nil {
		r.t.Fatal(err)
	}
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
		if name != idleRigChecks {
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
	if _, err := st.AdmitClaim(ctx, supervise.Claim{
		Token: token, Project: r.project, Tree: key, Issue: key, Role: claim.RoleImplementer, Generation: 1,
		State: supervise.StateSuspended, SuspendedBy: supervise.SuspendedByDaemon,
	}); err != nil {
		r.t.Fatalf("record the suspended implementer of %s: %v", key, err)
	}
	writePrompt(r.t, r.cfg, token)
	return idleRigIssue{name: name, key: key, pod: sandbox.SandboxName(token), role: claim.RoleImplementer}
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

// boot runs the daemon over the rig until the test ends, returning once it reports itself ready to
// serve. The runtime it is given is the real Sandbox runtime over the rig's Kubernetes API.
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
	// The rig's clock fires no timer of the daemon's own (stillClock), so no idle or wake pass
	// runs unless a test asks for it (passes): every moment the rig drives is a row it back-dates
	// — the quiet period, a backstop's due time, a wake's age — and each pass it reads after is
	// one it ran, with nothing of the daemon's own deciding the pods between.
	o := overrides{
		listen: heldListen, runtime: r.newRuntime, clock: newStillClock(),
		workflowTokens: idleRigAppTokens{workflowTokenRecorder: &workflowTokenRecorder{}}, githubAPI: repositoriesGitHub(r.t),
	}
	r.passes = triggering(&o)
	r.obs = observing(&o)
	go func() { done <- run(ctx, r.cfg, logger, o) }()
	r.stopMu.Lock()
	r.stop, r.done = cancel, done
	r.stopMu.Unlock()
	r.t.Cleanup(r.stopDaemon)
	r.obs.ready(r.t, done)
	transport := &http.Transport{}
	r.t.Cleanup(transport.CloseIdleConnections)
	r.api = &daemon{t: r.t, cfg: r.cfg, ctx: testwait.Context(r.t), transport: transport, client: &http.Client{Transport: transport},
		base: "http://127.0.0.1:" + strconv.Itoa(r.cfg.Port), stopped: true}
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
	if err := testwait.Receive(r.t, "the daemon's run to return once its context was cancelled", done); err != nil {
		r.t.Errorf("run: %v", err)
	}
}

// restart stops the daemon and boots it again on the same store, as a deployment's restart does.
// The wakes the listener reported while it was down are on its durable consumer, which is what
// makes them wait for it rather than be lost.
func (r *idleWakeRig) restart() {
	r.t.Helper()
	r.stopDaemon()
	rebindHeldPorts(r.t, &r.cfg)
	r.boot()
}

// newRuntime is the runtime factory the daemon boots with: the real Sandbox runtime
// (sandbox.New) over the rig's Kubernetes API, with the worker stream, store and connection
// directory the boot hands it, and the rig's own tools — a `legion` that is the role process
// stand-in, and an agent command it never has to run.
func (r *idleWakeRig) newRuntime(ctx context.Context, listener *stream.Listener, address string, tokens appauth.Source,
	_ func(context.Context, string) (ghrepo.Repository, string, error), st *store.Store, _ func() []ghrepo.Repository,
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
	opts.Conns, opts.StreamURL, opts.Store = listener, address, st
	// The two halves of a release's saved work, as the daemon's own factory hands them to the
	// runtime it builds (sandboxRuntime): what the launcher of the pod that replaces a released
	// one is sent before any role of that pod starts, and what the launcher's confirmation
	// drops. Without them no pod of this rig is ever offered a bundle. They cost a rig whose
	// releases save none one indexed read per role start, which answers no row: the save of a
	// workspace with nothing unpushed stores nothing and deletes what the issue had
	// (outbox.bundleIssueWorkspace), and this rig's role processes bundle an empty workspace.
	opts.SavedWorkspaceBundle, opts.WorkspaceBundleRestored = st.TakeWorkspaceBundleForRestore, st.DeleteRestoredWorkspaceBundle
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
	opts.Mapped = func() []ghrepo.Repository { return nil }
	opts.Tools.Legion = idleRigLegionTool(r.t, tools)
	opts.Agent = idleRigAgentTool(r.t, tools)
	opts.DaemonURL = r.cfg.DaemonURL
	opts.EnvoyURL = r.cfg.EnvoyURL
	rt, err := sandbox.New(ctx, sandboxRESTConfig(r.kube.url), opts)
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

// launch waits until the issue's pod runs its role, registered, ready and having told the daemon
// it runs no background work: the state the idle rule releases. The daemon's boot starts a
// suspended claim that owns its issue's phase, so most are resumed with no nudge; a claim a boot
// leaves suspended is nudged with the operator's resume, once, when the wait first reads it
// suspended.
//
// The nudge's answer is not read: a boot resuming the claim by rule may answer it 409 (the claim
// is already launching), which is no failure here — what this waits for is the pod, not the nudge,
// and a claim that reaches its role is one the resume, the boot's or this one, brought back.
func (r *idleWakeRig) launch(issue idleRigIssue) {
	r.t.Helper()
	defer func() {
		if r.t.Failed() {
			r.t.Log(r.diagnose())
		}
	}()
	token := r.token(issue)
	nudged := false
	testwait.Eventually(r.t, "the pod of "+issue.key+" running its role, registered and ready", func() bool {
		state := r.api.claim(token)
		if idleRigWorking(state.State) && r.agents.role(issue.key, string(issue.role)).Registered > 0 {
			return true
		}
		if !nudged && state.State == string(supervise.StateSuspended) {
			nudged = true
			r.api.request(http.MethodPost, "/legion/v1/operator/claims/"+string(token)+"/resume", nil, true)
		}
		return false
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

// token is the claim token of the issue's role.
func (r *idleWakeRig) token(issue idleRigIssue) claim.Token {
	r.t.Helper()
	token, err := claim.NewToken(r.project, issue.key, issue.role)
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

// quiet back-dates when every claim of the project was last written (its updated_at), so the next
// idle pass reads a pod that has done nothing for longer than the idle period. updated_at is the
// one column the idle rule measures against (pods.IdleView.QuietSince), and it is set on every
// claim, so this back-dates every claim and not only the ones that reported activity: a claim an
// operator stopped may hold no activity_at at all (it reported none before it was retired), and a
// release of its pod waits out the real idle period unless its updated_at is moved back here too.
//
// A claim the idle release suspended keeps its own time. That suspension is a write of the claim
// row, so in a deployment the pod it released reads busy for a whole idle period from then, and
// the rule for a pod no claim owns cannot take it again while its release is still finishing.
// Back-dated, the next pass read that pod as quiet for the period while the runtime's watch of
// Sandboxes still held the one the release was deleting, and released it a second time: a second
// release record, naming no role, beside the idle one (TestIdleReleaseHoldsForARunningSubagent,
// once in a whole-package run under 32 busy loops with -race).
func (r *idleWakeRig) quiet() {
	r.t.Helper()
	if _, err := r.pool.Exec(context.Background(),
		`update claims set updated_at = now() - interval '30 minutes'
		 where project = $1 and not (state = 'suspended' and suspended_by = 'released')`, r.project); err != nil {
		r.t.Fatalf("back-date the project's claims: %v", err)
	}
}

// keepListenerGateShut has every pass the rig runs from here made with the project's
// `listener_reports` row aged past the window the daemon reads it in, which shuts the one gate
// the idle release passes before it takes a pod (workflowRuntime.listenerLive,
// listenerReadyWindow). The release of a pod no claim owns runs outside that gate, so a pass
// made with the gate shut is the one case that tells the two rules apart: it releases a pod no
// role can use and holds every idle one.
//
// The row is aged inside idlePass, immediately before the pass it is for, and not on a goroutine
// of its own. The rig runs a real Envoy listener and cannot leave it out — the listener owns the
// notification stream the daemon binds its consumer on — and that listener publishes a ready
// report every five minutes into this very row. No pass of the daemon's runs between the ageing
// and the pass that reads it, because every pass here is one this rig asks for.
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
	r.gateShut = true
}

// idlePass runs one pass of the daemon's idle loop (tickIdle: the release of the pods no claim
// owns, then the release of the idle ones) and returns once that pass has ended, which is when
// what it decided can be read. The claim rows are back-dated first, because the idle rule reads
// them and nothing else (pods.IdleView), so those rows are the clock it is measured against; and
// the listener's report with them where a test holds that gate shut.
func (r *idleWakeRig) idlePass() {
	r.t.Helper()
	r.quiet()
	if r.gateShut {
		if _, err := r.ageListenerReport(); err != nil {
			r.t.Fatalf("age the Envoy listener's report of %s: %v", r.project, err)
		}
	}
	r.passes.pass(r.t, passIdle)
}

// wakePass runs one pass of the daemon's wake loop (tickWakes: the backstops that have come due,
// then the resumes with no first turn) and returns once that pass has ended. A backstop brought
// due (dueBackstop) or a resume aged (ageResume) is read by the next pass of this loop, and the
// rig's clock fires none on its own.
func (r *idleWakeRig) wakePass() {
	r.t.Helper()
	r.passes.pass(r.t, passWakes)
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
	testwait.Eventually(r.t, "every pod of the rig released", func() bool {
		// One pass of the daemon's idle loop per turn, each with the claim rows back-dated
		// first: a release the pass did not make is a release nothing made, and the rows are
		// the clock the idle rule is measured against (idlePass).
		r.idlePass()
		// One listing for the eleven pods, not one each: the operator's route answers every claim
		// at once (daemon.claims, which daemon.claim reads to find one), and the pass this waits
		// for reads the same rows through the same pool, so a wait that asked eleven times a poll
		// would be taking the daemon's own Postgres away from the release it is waiting for.
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
// The claim rows are back-dated before each pass, for the reason awaitReleased back-dates them:
// they are the clock the daemon reads the quiet period against (idlePass). The listener's report
// is the other clock, and a test that holds that gate shut has it aged before each pass too
// (keepListenerGateShut). A failure names where the pod stood when the wait last read it,
// beside the launchers' log and the daemon's.
func (r *idleWakeRig) awaitUnownedReleased(issue idleRigIssue) {
	r.t.Helper()
	before := r.claimLines(issue)
	teardowns := r.logged(idleRigUnownedTeardown, map[string]string{"issue": issue.key})
	seen := "never read"
	defer func() {
		if r.t.Failed() {
			r.t.Logf("the pod of %s, as the wait last read it: %s", issue.key, seen)
			r.t.Log(r.diagnose())
		}
	}()
	// Run idle passes until the daemon has torn the unowned pod down, which it logs once its
	// release row's run has removed the pod, the Sandbox and the disk (outbox.releaseIssue →
	// SuspendIssue, which nothing of the disk survives). idlePass is what each turn runs, not a
	// bare pass: it back-dates the claim rows first (quiet), which is the clock this release reads
	// (QuietSince), and ages the listener report where the gate is held shut. The loop stops at
	// the teardown line rather than polling the fake's pod and disk flags, which lag the teardown.
	testwait.Eventually(r.t, "the pod of "+issue.key+" released with every claim of it left as it was", func() bool {
		r.idlePass()
		if got := r.claimLines(issue); !slices.Equal(got, before) {
			r.t.Fatalf("the claims of %s read\n  %s\nwant the ones it stood at\n  %s\nthis release stops no role and changes no claim",
				issue.key, strings.Join(got, "\n  "), strings.Join(before, "\n  "))
		}
		return r.logged(idleRigUnownedTeardown, map[string]string{"issue": issue.key}) > teardowns
	})
	// The row whose run wrote that line, settled: the teardown is the row's own work, so its
	// settlement is here the moment the line is.
	r.obs.settled(r.t, rowOf{issue: issue.key, kind: record.OutboxKindIssueRelease, outcome: settledFinished})
	pod, sandbox, disk := r.kube.pod(issue.pod), r.kube.sandbox(issue.pod) != nil, r.kube.disk(issue.pod)
	seen = fmt.Sprintf("pod %t, sandbox %t, disk %t", pod, sandbox, disk)
	if pod || sandbox || disk {
		r.t.Fatalf("the pod of %s reads pod %t, sandbox %t, disk %t after its release row finished, want all three gone", issue.key, pod, sandbox, disk)
	}
}

// awaitResumed waits for the issue's role to be back: its claim live again, a second pod started
// for it, and its role process registered again.
func (r *idleWakeRig) awaitResumed(issue idleRigIssue, registrations int) {
	r.t.Helper()
	token := r.token(issue)
	testwait.Eventually(r.t, "the pod of "+issue.key+" back with its role registered again", func() bool {
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
	for _, line := range strings.Split(r.logs.String(), "\n") {
		if line == "" {
			continue
		}
		var entry map[string]any
		if json.Unmarshal([]byte(line), &entry) != nil {
			continue
		}
		matches := entry["msg"] == msg
		for key, want := range attributes {
			matches = matches && fmt.Sprint(entry[key]) == want
		}
		if matches {
			count++
		}
	}
	return count
}
