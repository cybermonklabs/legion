package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/sjawhar/legion/daemon/internal/bootprobe"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/promptrefs"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/runtime/fake"
	"github.com/sjawhar/legion/daemon/internal/runtime/sandbox"
	"github.com/sjawhar/legion/daemon/internal/shimwire"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/store/migrations"
	"github.com/sjawhar/legion/daemon/internal/stream"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/testlog"
	"github.com/sjawhar/legion/daemon/internal/testnkeys"
	"github.com/sjawhar/legion/daemon/internal/testpg/testdb"
	"github.com/sjawhar/legion/daemon/internal/testwait"
)

// kubernetesConfig is testConfig under runtime: kubernetes, its client a kubeconfig whose current
// context points at server, and every role's reservation at the loader's defaults, as a loaded
// configuration holds them.
func kubernetesConfig(t *testing.T, server string) config.Config {
	t.Helper()
	cfg := testConfig(t)
	cfg.Runtime = config.Runtime{Name: "kubernetes", Kubernetes: &config.Kubernetes{
		Namespace: "legion", Image: "ghcr.io/cybermonklabs/legion-worker@sha256:" + strings.Repeat("a", 64),
		StorageClass: "gp2", IssueVolume: "20Gi", Kubeconfig: writeKubeconfig(t, server, "test"),
		Resources: config.DefaultResources(), RepositoryCache: config.DefaultRepositoryCache(),
	}}
	return cfg
}

// defaultReservations are both pods' container requirements at the daemon's defaults, translated as
// sandboxOptions translates them: what sandbox.New requires of a test that builds Options by hand,
// since it refuses a pod with no reservation.
func defaultReservations(t *testing.T) sandbox.PodResources {
	t.Helper()
	defaults := config.DefaultResources()
	issue, err := requirements("runtime.kubernetes.resources.issue", defaults.Issue)
	if err != nil {
		t.Fatal(err)
	}
	controller, err := requirements("runtime.kubernetes.resources.controller", defaults.Controller)
	if err != nil {
		t.Fatal(err)
	}
	return sandbox.PodResources{Issue: issue, Controller: controller}
}

// defaultRepositoryCache is the repository cache at the daemon's defaults, translated as
// sandboxOptions translates it, for a test that builds Options by hand.
func defaultRepositoryCache(t *testing.T) sandbox.RepositoryCache {
	t.Helper()
	cache, err := repositoryCache(config.DefaultRepositoryCache())
	if err != nil {
		t.Fatal(err)
	}
	return cache
}

// noRepositories is a cache set that names none, for a test whose runtime makes no cache.
func noRepositories() []ghrepo.Repository { return nil }

// writeKubeconfig is a kubeconfig of one cluster at server and one context, its current context
// when current names it.
func writeKubeconfig(t *testing.T, server, current string) string {
	t.Helper()
	body := "apiVersion: v1\nkind: Config\n" +
		"clusters:\n- name: test\n  cluster:\n    server: " + server + "\n" +
		"users:\n- name: test\n  user:\n    token: kube-test-token\n" +
		"contexts:\n- name: test\n  context:\n    cluster: test\n    user: test\n"
	if current != "" {
		body += "current-context: " + current + "\n"
	}
	path := filepath.Join(t.TempDir(), "kubeconfig")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write the kubeconfig: %v", err)
	}
	return path
}

// A cluster without Agent Sandbox is refused by name (LEGION-206 Requirement 10) before the boot
// is recorded: the CRD and the controller's Deployment are each named, and nothing is launched.
func TestAKubernetesDaemonRefusesAClusterWithoutAgentSandboxBeforeItsBoot(t *testing.T) {
	t.Parallel()
	var mu sync.Mutex
	var requested []string
	apiServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		requested = append(requested, r.Method+" "+r.URL.Path)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_ = json.NewEncoder(w).Encode(map[string]any{"kind": "Status", "apiVersion": "v1", "status": "Failure", "reason": "NotFound", "code": 404})
	}))
	defer apiServer.Close()
	cfg := kubernetesConfig(t, apiServer.URL)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	err := run(ctx, cfg, quietLogger(), overrides{clock: stillClock{}, listen: heldListen})
	if err == nil {
		t.Fatal("the daemon booted on a cluster without Agent Sandbox")
	}
	for _, want := range []string{"sandboxes.agents.x-k8s.io", "agent-sandbox-system/agent-sandbox-controller"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("boot refusal = %q, want it to name %s", err, want)
		}
	}
	if count, _ := boots(t, cfg); count != 0 {
		t.Errorf("a refused boot was recorded %d times", count)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(requested) == 0 {
		t.Error("the daemon asked the cluster nothing")
	}
	for _, request := range requested {
		if !strings.HasPrefix(request, "GET ") {
			t.Errorf("the install check sent %s; it may only read", request)
		}
	}
}

// A cluster that still holds a per-claim Sandbox of the layout before issue pods is refused by
// name before the daemon opens its store: its Postgres is unreachable here, so a census that ran
// after the store opened, or after a migration, would be refused for the store instead. The
// cluster is only read.
func TestAKubernetesDaemonRefusesALegacyPerClaimSandboxBeforeItOpensItsStore(t *testing.T) {
	t.Parallel()
	const legacy = "legion-test-legion-208-tester"
	var mu sync.Mutex
	var requested []string
	apiServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		requested = append(requested, r.Method+" "+r.URL.Path)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		var body map[string]any
		switch r.URL.Path {
		case "/apis/apiextensions.k8s.io/v1/customresourcedefinitions/sandboxes.agents.x-k8s.io":
			body = map[string]any{"apiVersion": "apiextensions.k8s.io/v1", "kind": "CustomResourceDefinition",
				"metadata": map[string]any{"name": "sandboxes.agents.x-k8s.io"},
				"spec":     map[string]any{"versions": []any{map[string]any{"name": "v1beta1", "served": true}}}}
		case "/apis/apps/v1/namespaces/agent-sandbox-system/deployments/agent-sandbox-controller":
			body = map[string]any{"apiVersion": "apps/v1", "kind": "Deployment",
				"metadata": map[string]any{"name": "agent-sandbox-controller", "namespace": "agent-sandbox-system"},
				"status":   map[string]any{"availableReplicas": 1}}
		case "/apis/agents.x-k8s.io/v1beta1/namespaces/legion/sandboxes":
			body = map[string]any{"apiVersion": "agents.x-k8s.io/v1beta1", "kind": "SandboxList", "metadata": map[string]any{},
				"items": []any{map[string]any{"apiVersion": "agents.x-k8s.io/v1beta1", "kind": "Sandbox",
					"metadata": map[string]any{"name": legacy, "namespace": "legion"},
					"spec":     map[string]any{"podTemplate": map[string]any{"spec": map[string]any{"containers": []any{map[string]any{"name": "worker"}}}}}}}}
		default:
			w.WriteHeader(http.StatusNotFound)
			body = map[string]any{"kind": "Status", "apiVersion": "v1", "status": "Failure", "reason": "NotFound", "code": 404}
		}
		_ = json.NewEncoder(w).Encode(body)
	}))
	defer apiServer.Close()
	cfg := kubernetesConfig(t, apiServer.URL)
	cfg.PostgresDSN = "postgres://legion:legion@127.0.0.1:1/legion?sslmode=disable&connect_timeout=1"

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	err := run(ctx, cfg, quietLogger(), overrides{clock: stillClock{}, listen: heldListen})
	if err == nil || !strings.Contains(err.Error(), "legacy issue Sandbox "+legacy) {
		t.Fatalf("boot = %v, want the refusal naming the legacy Sandbox %s", err, legacy)
	}
	mu.Lock()
	defer mu.Unlock()
	for _, request := range requested {
		if !strings.HasPrefix(request, "GET ") {
			t.Errorf("the cluster check sent %s; it may only read", request)
		}
	}
}

// The store opens outside the start budget the cluster check spends, and the steps after the open
// get a budget of their own (c-boot-db-retry case 7). Here the real cluster check uses 22 s of the
// 30 s budget, and the database accepts the first connection and never answers it: the first
// attempt ends at its own limit, the second opens the store, and the fresh database is migrated
// to the binary's newest migration more than 30 s into the start. That is the start LINUX's
// daemon could not make on 2026-10-26, which opened its store on what the cluster check had left
// of the budget and exited with `context deadline exceeded`.
func TestAKubernetesDaemonOpensItsStoreAfterAClusterCheckThatUsedMostOfItsBudget(t *testing.T) {
	t.Parallel()
	const censusTakes = 22 * time.Second
	var census sync.Once
	apiServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		var body map[string]any
		switch r.URL.Path {
		case "/apis/apiextensions.k8s.io/v1/customresourcedefinitions/sandboxes.agents.x-k8s.io":
			body = map[string]any{"apiVersion": "apiextensions.k8s.io/v1", "kind": "CustomResourceDefinition",
				"metadata": map[string]any{"name": "sandboxes.agents.x-k8s.io"},
				"spec":     map[string]any{"versions": []any{map[string]any{"name": "v1beta1", "served": true}}}}
		case "/apis/apps/v1/namespaces/agent-sandbox-system/deployments/agent-sandbox-controller":
			body = map[string]any{"apiVersion": "apps/v1", "kind": "Deployment",
				"metadata": map[string]any{"name": "agent-sandbox-controller", "namespace": "agent-sandbox-system"},
				"status":   map[string]any{"availableReplicas": 1}}
		case "/apis/agents.x-k8s.io/v1beta1/namespaces/legion/sandboxes":
			// The census is the first read of the project's Sandboxes, and the only slow one.
			census.Do(func() {
				select {
				case <-time.After(censusTakes):
				case <-r.Context().Done():
				}
			})
			body = map[string]any{"apiVersion": "agents.x-k8s.io/v1beta1", "kind": "SandboxList", "metadata": map[string]any{}, "items": []any{}}
		default:
			w.WriteHeader(http.StatusNotFound)
			body = map[string]any{"kind": "Status", "apiVersion": "v1", "status": "Failure", "reason": "NotFound", "code": 404}
		}
		_ = json.NewEncoder(w).Encode(body)
	}))
	defer apiServer.Close()
	cfg := kubernetesConfig(t, apiServer.URL)
	database := testdb.Database(t, "legion_daemon_late_open")
	cfg.PostgresDSN = testdb.Outage(t, database, testdb.Silent).DSN
	all, err := migrations.All()
	if err != nil {
		t.Fatalf("read the embedded migrations: %v", err)
	}
	newest := all[len(all)-1].Version
	reader, err := store.Open(context.Background(), database)
	if err != nil {
		t.Fatalf("open the fresh database to read its schema version: %v", err)
	}
	t.Cleanup(reader.Close)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	started := time.Now()
	go func() { done <- run(ctx, cfg, quietLogger(), overrides{clock: stillClock{}, listen: heldListen}) }()
	// What follows the migration is not this test's: the runtime this configuration builds may
	// refuse it (it names no session store), and the test stops the start once it reads the
	// migration, so run may return before or after the stop, with anything but a refusal of the
	// store it opened.
	var version int
	var ended error
	returned := false
	testwait.Holds(2*time.Minute, 250*time.Millisecond, func() bool {
		if version, _ = reader.SchemaVersion(context.Background()); version == newest {
			return true
		}
		select {
		case ended = <-done:
			returned = true
			return true
		default:
			return false
		}
	})
	took := time.Since(started)
	if returned {
		// run may have returned between the migration's end and the read before it.
		version, _ = reader.SchemaVersion(context.Background())
	} else {
		cancel()
		select {
		case ended = <-done:
		case <-time.After(time.Minute):
			t.Fatal("run did not return within a minute of its stop")
		}
	}
	if version != newest {
		t.Fatalf("run returned %v %s into the start, and its database's schema version is %d, want %d", ended, took.Round(time.Second), version, newest)
	}
	if ended != nil && strings.Contains(ended.Error(), "connect to Postgres") {
		t.Errorf("run = %v, want no refusal of the store it opened", ended)
	}
	if took <= bootTimeout {
		t.Errorf("the database was migrated %s into the start, inside its first %s budget: the cluster check and the first attempt did not spend what this test is for", took, bootTimeout)
	}
}

// The worker image is proven after the runtime is built and before the boot is recorded; a refusal
// refuses the boot.
func TestAKubernetesDaemonRefusesTheBootItsWorkerImageProbeRefuses(t *testing.T) {
	t.Parallel()
	cfg := kubernetesConfig(t, "https://127.0.0.1:1")
	rt := fake.NewRuntime()
	o := fakeRuntime(rt, &built{})
	var probed runtime.Runtime
	o.probe = func(_ context.Context, built runtime.Runtime) (bootprobe.ImageReport, error) {
		probed = built
		return bootprobe.ImageReport{}, errors.New("worker image ghcr.io/cybermonklabs/legion-worker@sha256:aaaa refused: the plugin declares contract 3")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	err := run(ctx, cfg, quietLogger(), o)
	if err == nil || !strings.Contains(err.Error(), "the plugin declares contract 3") {
		t.Fatalf("boot = %v, want the image probe's refusal", err)
	}
	if probed != rt {
		t.Errorf("the probe was handed %v, want the runtime the daemon built", probed)
	}
	if count, _ := boots(t, cfg); count != 0 {
		t.Errorf("a boot whose image was refused was recorded %d times", count)
	}
}

// Under kubernetes the worker stream is TCP on the daemon's bind, at a port the listener resolves
// itself, the one address the runtime hands every pod's shim, and the runtime is given the
// workflow's App tokens, its repository of an issue, already set when the runtime is built — for
// an issue no record names, the repository Dispatch resolves the issue to, at GitHub's default
// branch (unrecordedRepository) — and the cache set, the repositories the project's live records
// name, which its repository caches follow: none for a project with no record. The host's own
// agent machinery never runs: no pane launcher is installed, no Dispatch token file is written,
// and the workflow's boot has no worker-bin stage.
func TestAKubernetesDaemonServesItsWorkerStreamOnTCPAndRunsNoHostPaneMachinery(t *testing.T) {
	t.Parallel()
	cfg := workflowConfigOn(t, workflowNATS(t), func(t *testing.T, project string) string {
		return dispatchBoard(t, func(w http.ResponseWriter, r *http.Request) {
			var body any
			switch {
			case servedBootCall(t, w, r, project):
				return
			case r.Method == http.MethodGet && r.URL.Path == "/api/v1/issues":
				body = []any{}
			case r.Method == http.MethodGet && r.URL.Path == "/api/v1/issues/LEGSMOKE-9":
				body = map[string]any{"key": "LEGSMOKE-9", "title": "an operator's claim", "status": "in_progress", "labels": []string{},
					"last_seq": 1, "project": project, "resolved_repo": "acme/widgets"}
			default:
				t.Errorf("Dispatch request = %s %s", r.Method, r.URL.Path)
				http.NotFound(w, r)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			if err := json.NewEncoder(w).Encode(body); err != nil {
				t.Errorf("write %s: %v", r.URL.Path, err)
			}
		})
	})
	cfg.Runtime = kubernetesConfig(t, "https://127.0.0.1:1").Runtime
	tokens := &workflowTokenRecorder{}
	record := &built{}
	o := fakeRuntime(fake.NewRuntime(), record)
	o.workflowTokens, o.githubAPI = tokens, repositoriesGitHub(t)
	var logged syncBuffer
	logger := slog.New(slog.NewJSONHandler(&logged, nil))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- run(ctx, cfg, logger, o) }()
	defer func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("run: %v", err)
		}
	}()
	awaitHealthz(t, cfg, done)

	record.mu.Lock()
	address, apps, repository, cached := record.address, record.apps, record.repository, record.cached
	record.mu.Unlock()
	if cached == nil {
		t.Fatal("the runtime was handed no cache set")
	}
	if got := cached(); len(got) != 0 {
		t.Errorf("the runtime's repository caches follow %v, want none: no live record of the project names a repository", got)
	}
	prefix := "tcp://" + cfg.Bind + ":"
	if !strings.HasPrefix(address, prefix) || strings.HasSuffix(address, ":0") {
		t.Fatalf("the runtime was told to have shims dial %q, want %s<bound port>", address, prefix)
	}
	if conn, err := net.DialTimeout("tcp", strings.TrimPrefix(address, "tcp://"), time.Second); err != nil {
		t.Errorf("the worker stream does not accept on %s: %v", address, err)
	} else {
		conn.Close()
	}
	// Both sides here are the listener's own Addr(), so this pins what p.newRuntime was told, not
	// the address itself: that the returned address names a real worker-stream socket is proved
	// by internal/stream's dial-and-hello tests, not by the dial above (a bare TCP connect, which
	// proves only that something is listening). A requested-rather-than-bound address fails the
	// :0 check above.
	if bound := bootField(t, logged.String(), "workerStream"); address != bound {
		t.Errorf("the runtime was told to have shims dial %q, want %q, the address the stream listener bound", address, bound)
	}
	if apps != tokens {
		t.Errorf("the runtime was handed App tokens %v, want the workflow's", apps)
	}
	if repository == nil {
		t.Fatal("the runtime was handed no repository rule, want the workflow's")
	}
	if repo, branch, err := repository(ctx, "LEGSMOKE-9"); err != nil || repo.String() != "acme/widgets" || branch != "main" {
		t.Errorf("the runtime's repository of an unrecorded issue = %s@%s, %v; want the one Dispatch resolves it to, acme/widgets@main", repo, branch, err)
	}
	for _, path := range []string{filepath.Join(cfg.StateDir, "bin"), filepath.Join(cfg.StateDir, "secrets", "dispatch-token")} {
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s exists (%v): the host's pane machinery ran under kubernetes", path, err)
		}
	}
	if strings.Contains(logged.String(), `"stage":"launcher"`) {
		t.Errorf("the boot installed the pane launcher:\n%s", logged.String())
	}
}

// bootField is one field of the "legion daemon started" line in a daemon's JSON log.
func bootField(t *testing.T, logged, field string) string {
	t.Helper()
	for _, line := range strings.Split(logged, "\n") {
		var entry map[string]any
		if json.Unmarshal([]byte(line), &entry) != nil || entry["msg"] != "legion daemon started" {
			continue
		}
		value, ok := entry[field].(string)
		if !ok {
			t.Fatalf("the boot line has no string %s: %s", field, line)
		}
		return value
	}
	t.Fatalf("no \"legion daemon started\" line in:\n%s", logged)
	return ""
}

// The worker stream's listen address derives from bind and worker_stream_port (prepareSandbox,
// kubernetes.go), not a value prepare hardcodes or leaves at the zero a test's own config happens to
// carry: a daemon configured with a distinctive port builds its plan's stream address from exactly
// that port, before anything binds, and from bind even when advertise_host names the host pods dial
// (openSupervision substitutes that once the real listener exists). The test above proves the bound
// address is real and dialable; it cannot catch worker_stream_port being ignored, since
// testConfig's own port is always 0 — this does, by configuring a nonzero one and checking
// prepare()'s plan directly, with no listener bound.
func TestPrepareDerivesTheWorkerStreamAddressFromWorkerStreamPort(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ name, bind, advertiseHost string }{
		{"no advertise_host", "192.0.2.30", ""},
		{"advertise_host set", "0.0.0.0", "legion-daemon-legsmoke.legion.svc"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := kubernetesConfig(t, "https://127.0.0.1:1")
			cfg.EnvoyTokenFile = filepath.Join(t.TempDir(), "envoy-token")
			if err := os.WriteFile(cfg.EnvoyTokenFile, []byte("envoy-bearer\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			cfg.NatsNkeySeedFile = testnkeys.SeedFile(t, testnkeys.UserSeed(t))
			cfg.Bind, cfg.AdvertiseHost, cfg.WorkerStreamPort = tc.bind, tc.advertiseHost, 47381

			p, err := prepare(cfg, quietLogger(), overrides{environ: []string{}})
			if err != nil {
				t.Fatalf("prepare: %v", err)
			}
			if want := "tcp://" + net.JoinHostPort(tc.bind, "47381"); p.stream != want {
				t.Errorf("prepare's worker stream address = %q, want %q (bind and worker_stream_port)", p.stream, want)
			}
		})
	}
}

// shimAddress is where advertise_host takes effect: the substitution openSupervision applies to the
// listener's bound address before handing it to the runtime factory. An address it cannot split
// beside an advertise_host is refused rather than handed to pods.
func TestShimAddressSubstitutesAdvertiseHostsHostKeepingTheBoundPort(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ name, bound, advertiseHost, want string }{
		{"no advertise_host, a real bind", "tcp://192.0.2.30:13371", "", "tcp://192.0.2.30:13371"},
		{"no advertise_host, a unix socket", "unix:///run/legion/stream.sock", "", "unix:///run/legion/stream.sock"},
		{"advertise_host over an IPv4 wildcard bind", "tcp://0.0.0.0:34353", "legion-daemon-legsmoke.legion.svc", "tcp://legion-daemon-legsmoke.legion.svc:34353"},
		{"advertise_host over an IPv6 wildcard bind", "tcp://[::]:34353", "legion-daemon-legsmoke.legion.svc", "tcp://legion-daemon-legsmoke.legion.svc:34353"},
		{"advertise_host over a real bind", "tcp://192.0.2.30:13371", "legion-daemon-legsmoke.legion.svc", "tcp://legion-daemon-legsmoke.legion.svc:13371"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := shimAddress(tc.bound, tc.advertiseHost)
			if err != nil || got != tc.want {
				t.Errorf("shimAddress(%q, %q) = %q, %v, want %q", tc.bound, tc.advertiseHost, got, err, tc.want)
			}
		})
	}
	for _, bound := range []string{"unix:///run/legion/stream.sock", "tcp://0.0.0.0"} {
		if got, err := shimAddress(bound, "legion-daemon-legsmoke.legion.svc"); err == nil || !strings.HasPrefix(err.Error(), "advertise_host legion-daemon-legsmoke.legion.svc") {
			t.Errorf("shimAddress(%q, advertise_host) = %q, %v, want a refusal naming advertise_host", bound, got, err)
		}
	}
}

// The full boot, through the real fake-runtime seam: with bind 0.0.0.0 and advertise_host
// configured, the address the runtime factory actually receives (record.address, the one
// prepareSandbox and openSupervision together produce) is advertise_host plus the listener's real,
// kernel-chosen port — never listener.Addr()'s own wildcard, which no pod could dial. The Service
// name resolves nowhere here, but the listener binds every interface, so 127.0.0.1 at the advertised
// port reaches it: the port is the stream listener's own.
func TestOpenSupervisionAdvertisesAdvertiseHostNotTheWildcardBind(t *testing.T) {
	t.Parallel()
	cfg := workflowConfig(t, workflowNATS(t))
	cfg.Runtime = kubernetesConfig(t, "https://127.0.0.1:1").Runtime
	cfg.Bind, cfg.AdvertiseHost = "0.0.0.0", "legion-daemon-legsmoke.legion.svc"
	cfg.Port = holdPortOn(t, "0.0.0.0")
	cfg.DaemonURL = "http://127.0.0.1:" + strconv.Itoa(cfg.Port)
	record := &built{}
	o := fakeRuntime(fake.NewRuntime(), record)
	o.workflowTokens, o.githubAPI = &workflowTokenRecorder{}, repositoriesGitHub(t)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- run(ctx, cfg, quietLogger(), o) }()
	defer func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("run: %v", err)
		}
	}()
	awaitHealthz(t, cfg, done)

	record.mu.Lock()
	address := record.address
	record.mu.Unlock()
	prefix := "tcp://legion-daemon-legsmoke.legion.svc:"
	if !strings.HasPrefix(address, prefix) || strings.HasSuffix(address, ":0") {
		t.Fatalf("the runtime was told to have shims dial %q, want %s<bound port>", address, prefix)
	}
	if conn, err := net.DialTimeout("tcp", "127.0.0.1:"+strings.TrimPrefix(address, prefix), time.Second); err != nil {
		t.Errorf("the worker stream does not accept on 127.0.0.1 at the advertised port of %s: %v", address, err)
	} else {
		conn.Close()
	}
}

// prepare refuses what the cluster would refuse only later: a kubeconfig with no current context
// when runtime.kubernetes.context names none, or a context the kubeconfig does not have.
func TestAKubernetesDaemonRefusesAConfigurationTheClusterWouldRefuseLater(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		change func(*config.Kubernetes, *testing.T)
		want   string
	}{
		{
			name: "no current context",
			change: func(k *config.Kubernetes, t *testing.T) {
				k.Kubeconfig = writeKubeconfig(t, "https://127.0.0.1:1", "")
			},
			want: "runtime.kubernetes.context is required: the kubeconfig",
		},
		{
			name: "a context the kubeconfig does not have",
			change: func(k *config.Kubernetes, t *testing.T) {
				k.Kubeconfig, k.Context = writeKubeconfig(t, "https://127.0.0.1:1", ""), "production"
			},
			want: `context "production" does not exist`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := kubernetesConfig(t, "https://127.0.0.1:1")
			tc.change(cfg.Runtime.Kubernetes, t)
			if _, err := prepare(cfg, quietLogger(), overrides{}); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("prepare = %v, want a refusal containing %q", err, tc.want)
			}
		})
	}
}

// Each pod's reservation reaches the runtime's Options as that pod's container requirements: its
// cpu and memory requests the file's where it set one below its limit, its limit otherwise — here
// the issue pod requests less than it may use, which the runtime renders as a Burstable pod, and the
// controller's keeps request == limit at sizes the file set — and its ephemeral storage the limit
// the file or the default set with the request under it; and the image probe carries its own fixed
// reservation (probeReservation), 500m and 1Gi over a 5Gi disk bound and a 1Gi disk request,
// translated the same way and no pod's share — a probe that reserved the controller's 1 CPU and
// 4Gi could not boot the daemon on a pool with room for less.
func TestEveryReservationReachesTheSandboxRuntimeAsRequestAndLimit(t *testing.T) {
	t.Parallel()
	cfg := kubernetesConfig(t, "https://127.0.0.1:1")
	cfg.Runtime.Kubernetes.Resources = config.PodResources{
		Issue:      config.Reservation{CPU: "4", CPURequest: "2", Memory: "32Gi", MemoryRequest: "8Gi", EphemeralStorage: "20Gi", EphemeralStorageRequest: "1Gi"},
		Controller: config.Reservation{CPU: "1500m", CPURequest: "1500m", Memory: "8Gi", MemoryRequest: "8Gi", EphemeralStorage: "40Gi", EphemeralStorageRequest: "2Gi"},
	}
	opts, err := sandboxOptions(cfg, *cfg.Runtime.Kubernetes, "test", sandbox.DispatchCredential{}, lookup(nil), quietLogger())
	if err != nil {
		t.Fatalf("sandboxOptions: %v", err)
	}
	for _, pod := range []struct {
		name        string
		reservation config.Reservation
		got         corev1.ResourceRequirements
	}{
		{"the issue pod", cfg.Runtime.Kubernetes.Resources.Issue, opts.Resources.Issue},
		{"the controller's pod", cfg.Runtime.Kubernetes.Resources.Controller, opts.Resources.Controller},
	} {
		want := corev1.ResourceRequirements{
			Requests: corev1.ResourceList{
				corev1.ResourceCPU: resource.MustParse(pod.reservation.CPURequest), corev1.ResourceMemory: resource.MustParse(pod.reservation.MemoryRequest),
				corev1.ResourceEphemeralStorage: resource.MustParse(pod.reservation.EphemeralStorageRequest),
			},
			Limits: corev1.ResourceList{
				corev1.ResourceCPU: resource.MustParse(pod.reservation.CPU), corev1.ResourceMemory: resource.MustParse(pod.reservation.Memory),
				corev1.ResourceEphemeralStorage: resource.MustParse(pod.reservation.EphemeralStorage),
			},
		}
		if !reflect.DeepEqual(pod.got, want) {
			t.Errorf("%s's requirements = %+v, want %+v (each request the reservation's, under or at its limit)", pod.name, pod.got, want)
		}
	}
	issue := opts.Resources.Issue
	if request, limit := issue.Requests[corev1.ResourceMemory], issue.Limits[corev1.ResourceMemory]; request.Cmp(resource.MustParse("8Gi")) != 0 || limit.Cmp(resource.MustParse("32Gi")) != 0 {
		t.Errorf("the issue pod requests memory %s under a limit of %s, want 8Gi under 32Gi", request.String(), limit.String())
	}
	if request, limit := issue.Requests[corev1.ResourceCPU], issue.Limits[corev1.ResourceCPU]; request.Cmp(resource.MustParse("2")) != 0 || limit.Cmp(resource.MustParse("4")) != 0 {
		t.Errorf("the issue pod requests cpu %s under a limit of %s, want 2 under 4", request.String(), limit.String())
	}
	probe, err := imageProbe(cfg, promptrefs.New())
	if err != nil {
		t.Fatalf("imageProbe: %v", err)
	}
	wantProbe, err := requirements("the image probe's reservation", probeReservation)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(probe.Resources, wantProbe) {
		t.Errorf("the image probe carries %+v, want its own reservation %+v", probe.Resources, wantProbe)
	}
	for kind, want := range map[corev1.ResourceName]string{corev1.ResourceCPU: "500m", corev1.ResourceMemory: "1Gi", corev1.ResourceEphemeralStorage: "1Gi"} {
		if request := probe.Resources.Requests[kind]; request.Cmp(resource.MustParse(want)) != 0 {
			t.Errorf("the image probe requests %s %s, want %s", kind, request.String(), want)
		}
	}
	for kind, want := range map[corev1.ResourceName]string{corev1.ResourceCPU: "500m", corev1.ResourceMemory: "1Gi", corev1.ResourceEphemeralStorage: "5Gi"} {
		if limit := probe.Resources.Limits[kind]; limit.Cmp(resource.MustParse(want)) != 0 {
			t.Errorf("the image probe is limited to %s %s, want %s", kind, limit.String(), want)
		}
	}
	if reflect.DeepEqual(probe.Resources, opts.Resources.Controller) {
		t.Errorf("the image probe carries the controller's reservation %+v; it must carry its own", opts.Resources.Controller)
	}
}

// runtime.kubernetes.setup reaches sandbox.Options unchanged: every role's launcher receives this
// argv, while leaving the setting out keeps the runtime setup-free.
func TestKubernetesSetupReachesTheSandboxRuntime(t *testing.T) {
	t.Parallel()
	cfg := config.Config{Project: "TEST", Projects: map[string]config.Project{"TEST": {}}}
	k := config.Kubernetes{
		Namespace: "legion", Image: "ghcr.io/cybermonklabs/legion-worker@sha256:" + strings.Repeat("a", 64),
		StorageClass: "gp2", IssueVolume: "20Gi", Resources: config.DefaultResources(), RepositoryCache: config.DefaultRepositoryCache(),
	}
	want := []string{"/bin/sh", "-c", "omp plugin install agent-skills"}
	k.Setup = want
	opts, err := sandboxOptions(cfg, k, "test", sandbox.DispatchCredential{}, lookup(nil), quietLogger())
	if err != nil {
		t.Fatalf("sandboxOptions: %v", err)
	}
	if !slices.Equal(opts.Setup, want) {
		t.Errorf("Options.Setup = %q, want %q", opts.Setup, want)
	}

	k.Setup = nil
	opts, err = sandboxOptions(cfg, k, "test", sandbox.DispatchCredential{}, lookup(nil), quietLogger())
	if err != nil {
		t.Fatalf("sandboxOptions without setup: %v", err)
	}
	if len(opts.Setup) != 0 {
		t.Errorf("Options.Setup = %q without runtime.kubernetes.setup, want none", opts.Setup)
	}
}

// runtime.kubernetes.sandbox reaches sandbox.Options member for member: the type, the canary list
// and the microvm block with its ComputeClass, its storage class (the one the loader settled,
// runtime.kubernetes.storage_class where the file names none), its keep_volume and its metadata
// endpoint, while leaving the block out runs every pod in gVisor.
func TestKubernetesSandboxReachesTheSandboxRuntime(t *testing.T) {
	t.Parallel()
	cfg := config.Config{Project: "TEST", Projects: map[string]config.Project{"TEST": {}}}
	k := config.Kubernetes{
		Namespace: "legion", Image: "ghcr.io/cybermonklabs/legion-worker@sha256:" + strings.Repeat("a", 64),
		StorageClass: "gp2", IssueVolume: "20Gi", Resources: config.DefaultResources(), RepositoryCache: config.DefaultRepositoryCache(),
	}
	k.Sandbox = config.SandboxConfig{
		Type: config.SandboxMicroVM, CanaryIssues: []string{"TEST-7"},
		MicroVM: &config.MicroVMConfig{ComputeClass: "example-microvm", StorageClass: "gp2", KeepVolume: true, Metadata: &config.MetadataConfig{
			WorkloadPool:     "example-project.svc.id.goog",
			IdentityProvider: "https://container.googleapis.com/v1/projects/example-project/locations/us-central1/clusters/example-cluster",
		}},
	}
	opts, err := sandboxOptions(cfg, k, "test", sandbox.DispatchCredential{}, lookup(nil), quietLogger())
	if err != nil {
		t.Fatalf("sandboxOptions: %v", err)
	}
	want := sandbox.SandboxConfig{
		Type: sandbox.SandboxMicroVM, CanaryIssues: []string{"TEST-7"},
		MicroVM: &sandbox.MicroVM{ComputeClass: "example-microvm", StorageClass: "gp2", KeepVolume: true, Metadata: &sandbox.MetadataEndpoint{
			WorkloadPool:     "example-project.svc.id.goog",
			IdentityProvider: "https://container.googleapis.com/v1/projects/example-project/locations/us-central1/clusters/example-cluster",
		}},
	}
	if !reflect.DeepEqual(opts.Sandbox, want) {
		t.Errorf("Options.Sandbox = %+v, want %+v", opts.Sandbox, want)
	}
	if opts.Sandbox.Of("TEST-7") != sandbox.SandboxGVisor || opts.Sandbox.Of("TEST-8") != sandbox.SandboxMicroVM {
		t.Errorf("Options.Sandbox runs TEST-7 in %s and TEST-8 in %s, want the canary in gVisor and every other issue in a microVM",
			opts.Sandbox.Of("TEST-7"), opts.Sandbox.Of("TEST-8"))
	}

	k.Sandbox = config.SandboxConfig{}
	opts, err = sandboxOptions(cfg, k, "test", sandbox.DispatchCredential{}, lookup(nil), quietLogger())
	if err != nil {
		t.Fatalf("sandboxOptions without a sandbox block: %v", err)
	}
	if opts.Sandbox.AnyMicroVM() || opts.Sandbox.MicroVM != nil {
		t.Errorf("Options.Sandbox = %+v without runtime.kubernetes.sandbox, want every pod in gVisor", opts.Sandbox)
	}
}

// runtime.kubernetes.repository_cache reaches the runtime's Options as its volume, its intervals and
// its container's requirements, a settled reservation's lower memory request under its limit.
func TestTheRepositoryCacheReachesTheSandboxRuntime(t *testing.T) {
	t.Parallel()
	cfg := kubernetesConfig(t, "https://127.0.0.1:1")
	reservation := config.Reservation{CPU: "2", CPURequest: "2", Memory: "8Gi", MemoryRequest: "4Gi", EphemeralStorage: "2Gi", EphemeralStorageRequest: "1Gi"}
	cfg.Runtime.Kubernetes.RepositoryCache = config.RepositoryCacheConfig{Volume: "50Gi", RefreshMinutes: 5, RetainHours: 24, Resources: reservation}
	opts, err := sandboxOptions(cfg, *cfg.Runtime.Kubernetes, "test", sandbox.DispatchCredential{}, lookup(nil), quietLogger())
	if err != nil {
		t.Fatalf("sandboxOptions: %v", err)
	}
	want, err := requirements("runtime.kubernetes.repository_cache.resources", reservation)
	if err != nil {
		t.Fatal(err)
	}
	cache := opts.RepositoryCache
	if cache.Volume.Cmp(resource.MustParse("50Gi")) != 0 || cache.Refresh != 5*time.Minute || cache.Retain != 24*time.Hour || !reflect.DeepEqual(cache.Resources, want) {
		t.Errorf("RepositoryCache = %+v, want 50Gi, 5m, 24h and %+v", cache, want)
	}
	if request, limit := cache.Resources.Requests[corev1.ResourceMemory], cache.Resources.Limits[corev1.ResourceMemory]; request.Cmp(resource.MustParse("4Gi")) != 0 || limit.Cmp(resource.MustParse("8Gi")) != 0 {
		t.Errorf("the cache's memory is requested %s under %s, want 4Gi under 8Gi", request.String(), limit.String())
	}
}

// runtime.kubernetes.volume_trim reaches the runtime's Options as the settings each issue pod's
// launcher is handed on the answer to its hello — the two marks, the minimum age and the
// interval, each in its own member — and `enabled: false` hands the runtime none at all, which
// the launcher reads as no settings (sandbox.Options.VolumeTrim). Every number differs from the
// others, so two members crossed read back wrong. The negative controls are Enabled inverted,
// which hands the enabled block's runtime nothing and the disabled one's the settings, and the
// two marks swapped.
func TestTheVolumeTrimReachesTheSandboxRuntime(t *testing.T) {
	t.Parallel()
	cfg := kubernetesConfig(t, "https://127.0.0.1:1")
	cfg.Runtime.Kubernetes.VolumeTrim = config.VolumeTrimConfig{Enabled: true, HighPercent: 90, LowPercent: 60, MinAgeMinutes: 45, IntervalSeconds: 30}
	opts, err := sandboxOptions(cfg, *cfg.Runtime.Kubernetes, "test", sandbox.DispatchCredential{}, lookup(nil), quietLogger())
	if err != nil {
		t.Fatalf("sandboxOptions: %v", err)
	}
	want := shimwire.LauncherVolumeTrim{HighPercent: 90, LowPercent: 60, MinAgeMinutes: 45, IntervalSeconds: 30}
	if opts.VolumeTrim == nil || *opts.VolumeTrim != want {
		t.Errorf("Options.VolumeTrim = %+v, want %+v", opts.VolumeTrim, want)
	}

	cfg.Runtime.Kubernetes.VolumeTrim.Enabled = false
	opts, err = sandboxOptions(cfg, *cfg.Runtime.Kubernetes, "test", sandbox.DispatchCredential{}, lookup(nil), quietLogger())
	if err != nil {
		t.Fatalf("sandboxOptions with the trimmer off: %v", err)
	}
	if opts.VolumeTrim != nil {
		t.Errorf("Options.VolumeTrim = %+v with volume_trim.enabled false, want none: no settings is what the launcher reads as the trimmer off", *opts.VolumeTrim)
	}
}

// The path that failed live (stage 4b's daemon-controller-liveness, 2026-10-10): a legion.yaml
// under `controller: daemon` whose controller reservation no node can hold (the checkpoint's
// `cpu: "100000"`, set so the relaunched controller pod is Unschedulable) is read by the loader as
// written — the controller's requirements carry the 100000 CPU — while the image probe the daemon
// builds from the same configuration carries its own 500m / 1Gi and nothing of the controller's,
// so the daemon's boot probe still schedules. This is the manifest-level proof: the probe pod's
// container takes ImageProbe.Resources as it is (TestManifestGoldens, probe.json), and a boot-level
// run needs a cluster.
func TestAnImpossibleControllerReservationLeavesTheImageProbeItsOwn(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	body := fmt.Sprintf(`project: DEMO
postgres_dsn: postgres://legion:legion@127.0.0.1:1/legion
state_dir: %s
bind: 10.0.0.5
daemon_url: http://10.0.0.5:13370
envoy_url: http://envoy-listener.internal.example:9020
envoy_token_file: ./envoy-token
operator_token_file: ./operator-token
dispatch_url: https://dispatch.internal.example
dispatch_token_file: ./dispatch-token
nats_urls: [nats://nats.internal.example:4222]
controller: daemon
projects:
  DEMO: {}
github_apps:
  implement: { app_id: "1", private_key_command: "exit 1" }
  review: { app_id: "2", private_key_command: "exit 1" }
runtime:
  kubernetes:
    namespace: legion
    image: ghcr.io/cybermonklabs/legion-worker@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef
    storage_class: gp2
    session_store: postgres
    session_dsn_secret: SESSION_DSN
    kubeconfig: ./kubeconfig
    context: legion-daemon
    resources:
      controller: { cpu: "100000", memory: 1Gi }
`, filepath.Join(dir, "state"))
	path := filepath.Join(dir, "legion.yaml")
	for name, contents := range map[string]string{
		path: body, "envoy-token": "envoy\n", "operator-token": "operator\n", "dispatch-token": "dispatch\n",
		"kubeconfig": `apiVersion: v1
kind: Config
clusters: [{name: example, cluster: {server: "https://192.0.2.20:6443"}}]
users: [{name: legion-daemon, user: {token: placeholder}}]
contexts: [{name: legion-daemon, context: {cluster: example, user: legion-daemon, namespace: legion}}]
current-context: legion-daemon
`,
	} {
		if !filepath.IsAbs(name) {
			name = filepath.Join(dir, name)
		}
		if err := os.WriteFile(name, []byte(contents), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	cfg, err := config.LoadForValidation(path, nil)
	if err != nil {
		t.Fatalf("load the liveness checkpoint's configuration: %v", err)
	}
	opts, err := sandboxOptions(cfg, *cfg.Runtime.Kubernetes, "demo", sandbox.DispatchCredential{}, lookup(nil), quietLogger())
	if err != nil {
		t.Fatalf("sandboxOptions: %v", err)
	}
	controller := opts.Resources.Controller
	if cpu := controller.Requests[corev1.ResourceCPU]; cpu.Cmp(resource.MustParse("100000")) != 0 {
		t.Fatalf("the controller's request reached the runtime as %s CPU, want the file's 100000", cpu.String())
	}
	probe, err := imageProbe(cfg, promptrefs.New())
	if err != nil {
		t.Fatalf("imageProbe: %v", err)
	}
	for kind, want := range map[corev1.ResourceName]string{corev1.ResourceCPU: "500m", corev1.ResourceMemory: "1Gi"} {
		request, limit := probe.Resources.Requests[kind], probe.Resources.Limits[kind]
		if request.Cmp(resource.MustParse(want)) != 0 || limit.Cmp(resource.MustParse(want)) != 0 {
			t.Errorf("the image probe carries %s request %s / limit %s beside a 100000-CPU controller, want its own %s as both",
				kind, request.String(), limit.String(), want)
		}
	}
	if reflect.DeepEqual(probe.Resources, controller) {
		t.Errorf("the image probe carries the controller's impossible reservation %+v", controller)
	}
}

// A Kubernetes daemon refuses, before its boot, an operator pod that collides with Legion's own —
// a mount at Legion's launcher token projection (the LEGION-270 plan's negative control), a provider key
// or pod variable a launch secret's pointer names (the Envoy bearer's, the NATS nkey seed's), and,
// under dispatch_token_broker, a provider key named for the Dispatch token — so no pod is ever
// built with it.
func TestAKubernetesDaemonRefusesAnOperatorPodCollidingWithLegionsBeforeItsBoot(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		change func(*config.Config)
		want   string
	}{
		{
			name: "a mount at Legion's launcher token projection",
			change: func(cfg *config.Config) {
				cfg.Runtime.Kubernetes.Pod = config.PodConfig{
					Volumes:      []corev1.Volume{{Name: "creds", VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{SecretName: "legion-creds"}}}},
					VolumeMounts: []corev1.VolumeMount{{Name: "creds", MountPath: "/var/run/legion/launcher", ReadOnly: true}},
				}
			},
			want: "runtime.kubernetes.pod.volume_mounts[0].mount_path /var/run/legion/launcher overlaps /var/run/legion/launcher, which Legion mounts in every pod: a mount may be neither at, under, nor above one of Legion's",
		},
		{
			name: "a provider key the Envoy bearer's pointer names",
			change: func(cfg *config.Config) {
				cfg.ProviderKeys = []config.ProviderKey{{Env: "ENVOY_TOKEN", Secret: "envoy"}}
			},
			want: "provider_keys names ENVOY_TOKEN, the launch secret every launch carries behind its ENVOY_TOKEN_FILE pointer: a provider key may not name a launch secret",
		},
		{
			name: "a provider key the NATS nkey seed's pointer names",
			change: func(cfg *config.Config) {
				cfg.ProviderKeys = []config.ProviderKey{{Env: "NATS_NKEY_SEED", Secret: "seed"}}
			},
			want: "provider_keys names NATS_NKEY_SEED, the launch secret every launch carries behind its NATS_NKEY_SEED_FILE pointer: a provider key may not name a launch secret",
		},
		{
			name: "a pod variable that is the NATS nkey seed's pointer",
			change: func(cfg *config.Config) {
				cfg.Runtime.Kubernetes.Pod = config.PodConfig{Env: map[string]string{"NATS_NKEY_SEED_FILE": "/etc/operator/seed"}}
			},
			want: "runtime.kubernetes.pod.env sets NATS_NKEY_SEED_FILE, which every launch sets (the pointer to the launch secret NATS_NKEY_SEED)",
		},
		{
			name: "a provider key reading the NATS nkey seed's providers Secret key",
			change: func(cfg *config.Config) {
				cfg.ProviderKeys = []config.ProviderKey{{Env: "FOO", Secret: "NATS_NKEY_SEED"}}
			},
			want: "provider_keys names FOO from the providers Secret's key NATS_NKEY_SEED, which the pod mounts as the launch secret NATS_NKEY_SEED: the shim would export that secret into Oh My Pi's environment as FOO",
		},
		{
			// The boot refuses this key with or without dispatch_token_broker, since the runtime
			// owns its pointer, DISPATCH_TOKEN_FILE. Under the setting it gives the broker's own
			// reason: each role trades its pod's identity for its own Dispatch token.
			name: "a provider key for the Dispatch token under dispatch_token_broker",
			change: func(cfg *config.Config) {
				cfg.DispatchTokenBroker = true
				cfg.Runtime.Kubernetes.AgentSecrets = &config.AgentSecretsConfig{URL: "https://secrets.internal.example", Audience: "agent-secrets", TokenExpirySeconds: 3600, ExchangeAudience: "legion-broker-exchange"}
				cfg.ProviderKeys = []config.ProviderKey{{Env: "DISPATCH_TOKEN", Secret: "dispatch"}}
			},
			want: "provider_keys names DISPATCH_TOKEN, a Dispatch credential, under dispatch_token_broker: each role trades its pod's identity for its own Dispatch token, so the providers Secret may hold no shared one; remove the key",
		},
		{
			// Every agent reads it, and the dispatch command turns Dispatch off over a value that is
			// not a URL, so the daemon refuses it here rather than leave each agent to find out.
			name: "a public Dispatch dashboard that is not a URL",
			change: func(cfg *config.Config) {
				cfg.Runtime.Kubernetes.Pod = config.PodConfig{Env: map[string]string{"DISPATCH_PUBLIC_URL": "dispatch.example.org"}}
			},
			want: `runtime.kubernetes.pod.env sets DISPATCH_PUBLIC_URL to "dispatch.example.org", which is not an http or https URL with no userinfo, a host that is a DNS name of ASCII letters, digits and hyphens, a dotted-quad IPv4 address or a bracketed IPv6 address, and any port from 1 to 65535: boot accepts only a dashboard address every agent's dispatch command reads`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := kubernetesConfig(t, "https://127.0.0.1:1")
			cfg.EnvoyTokenFile = filepath.Join(t.TempDir(), "envoy-token")
			if err := os.WriteFile(cfg.EnvoyTokenFile, []byte("envoy-bearer\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			cfg.NatsNkeySeedFile = testnkeys.SeedFile(t, testnkeys.UserSeed(t))
			tc.change(&cfg)
			if _, err := prepare(cfg, quietLogger(), overrides{environ: []string{}}); err == nil || err.Error() != tc.want {
				t.Fatalf("prepare = %v, want %q", err, tc.want)
			}
		})
	}
}

// The daemon holds the operator's pod to the exchange identity's names, before its boot and for
// `legion config check`, only where runtime.kubernetes.agent_secrets names an
// exchange_audience. With no agent_secrets block, or one that names no exchange_audience, an
// operator's AGENT_SECRETS_MACHINE_IDENTITY and AGENT_SECRETS_MACHINE_TOKEN_FILE, a volume named
// agent-secrets-exchange and a mount at /var/run/legion/agent-secrets-exchange are the operator's,
// as they were before the key existed (R9); with one, the first of them is refused by name.
func TestTheDaemonLeavesTheExchangeNamesToTheOperatorWhileTheExchangeIsOff(t *testing.T) {
	t.Parallel()
	emptyDir := corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}
	pod := config.PodConfig{
		Env: map[string]string{"AGENT_SECRETS_MACHINE_IDENTITY": "metadata", "AGENT_SECRETS_MACHINE_TOKEN_FILE": "/etc/operator/identity/token"},
		Volumes: []corev1.Volume{
			{Name: "agent-secrets-exchange", VolumeSource: emptyDir},
			{Name: "operator-identity", VolumeSource: emptyDir},
		},
		VolumeMounts: []corev1.VolumeMount{{Name: "operator-identity", MountPath: "/var/run/legion/agent-secrets-exchange"}},
	}
	enrolled := func(exchange string) *config.AgentSecretsConfig {
		return &config.AgentSecretsConfig{URL: "https://secrets.internal.example", Audience: "agent-secrets", TokenExpirySeconds: 3600, ExchangeAudience: exchange}
	}
	for name, tc := range map[string]struct {
		agentSecrets *config.AgentSecretsConfig
		want         string
	}{
		"no agent_secrets block":                  {nil, ""},
		"agent_secrets with no exchange_audience": {enrolled(""), ""},
		"exchange_audience set": {enrolled("legion-broker-exchange"),
			"runtime.kubernetes.pod.env sets AGENT_SECRETS_MACHINE_IDENTITY, which Legion's runtime sets in every enrolled pod under runtime.kubernetes.agent_secrets.exchange_audience"},
	} {
		t.Run(name, func(t *testing.T) {
			cfg := kubernetesConfig(t, "https://127.0.0.1:1")
			cfg.Runtime.Kubernetes.AgentSecrets, cfg.Runtime.Kubernetes.Pod = tc.agentSecrets, pod
			_, err := checkOperatorConfig(cfg, lookup(nil))
			switch {
			case tc.want == "" && err != nil:
				t.Fatalf("checkOperatorConfig = %v, want the operator's pod accepted as it was before the key existed", err)
			case tc.want != "" && (err == nil || err.Error() != tc.want):
				t.Fatalf("checkOperatorConfig = %v, want %q", err, tc.want)
			}
		})
	}
}

// At its boot the daemon refuses an operator's pod volume named as a volume of the slot of a role
// that exists now, and takes one named as the slot volume of an agent an issue's plan can name
// beside its role's own, such as state-implementer-2: it logs a warning for each such volume that
// names the volume and the form, and says that a later release refuses the name. A volume no agent
// of a plan can collide with, such as gh-cache, is taken with no warning.
func TestTheDaemonWarnsOfAnOperatorVolumeOfASlotsFormAndRefusesOneOfARolesSlot(t *testing.T) {
	boot := func(t *testing.T, names ...string) (string, error) {
		t.Helper()
		cfg := kubernetesConfig(t, "https://127.0.0.1:1")
		cfg.EnvoyTokenFile = filepath.Join(t.TempDir(), "envoy-token")
		if err := os.WriteFile(cfg.EnvoyTokenFile, []byte("envoy-bearer\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		cfg.NatsNkeySeedFile = testnkeys.SeedFile(t, testnkeys.UserSeed(t))
		for _, name := range names {
			cfg.Runtime.Kubernetes.Pod.Volumes = append(cfg.Runtime.Kubernetes.Pod.Volumes,
				corev1.Volume{Name: name, VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}})
		}
		var logged syncBuffer
		_, err := prepare(cfg, slog.New(slog.NewJSONHandler(&logged, nil)), overrides{environ: []string{}})
		return logged.String(), err
	}
	t.Run("a volume of the slot of a role", func(t *testing.T) {
		want := "runtime.kubernetes.pod.volumes[0].name launcher-implementer is a volume Legion puts in every pod"
		if _, err := boot(t, "launcher-implementer"); err == nil || err.Error() != want {
			t.Fatalf("prepare = %v, want %q", err, want)
		}
	})
	t.Run("volumes of the form of a slot's volume", func(t *testing.T) {
		logged, err := boot(t, "gh-cache", "operator-extra", "state-implementer-2")
		if err != nil {
			t.Fatalf("prepare = %v, want the volumes taken with a warning", err)
		}
		var warned []string
		for _, entry := range loggedMessages(t, logged, "runtime.kubernetes.pod.volumes[") {
			if entry["level"] == "WARN" {
				warned = append(warned, fmt.Sprint(entry["msg"]))
			}
		}
		want := []string{
			"runtime.kubernetes.pod.volumes[2].name state-implementer-2 has the form state-<slot> of a volume Legion gives each slot of a pod, and collides with the state volume of the slot of an agent named implementer-2 wherever an issue's plan names one; a later release refuses this name, so rename the volume",
		}
		if !slices.Equal(warned, want) {
			t.Errorf("the boot warned %q, want %q", warned, want)
		}
	})
}

// runtime.kubernetes.pod and provider_keys reach the runtime's Options, each piece of the pod
// as configured and each provider key as its variable's Secret key: the manifests the runtime
// builds from them are the sandbox package's to test, and this is the one place the daemon hands
// them over.
func TestTheOperatorsPodReachesTheSandboxRuntime(t *testing.T) {
	t.Parallel()
	cfg := kubernetesConfig(t, "https://127.0.0.1:1")
	cfg.ProviderKeys = []config.ProviderKey{{Env: "ANTHROPIC_API_KEY", Secret: "anthropic"}}
	pod := config.PodConfig{
		Env:            map[string]string{"PI_CONFIG_FILES": "/etc/legion-operator/overlay.yml"},
		ServiceAccount: "legion-worker",
		Volumes:        []corev1.Volume{{Name: "creds", VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{SecretName: "legion-creds"}}}},
		VolumeMounts:   []corev1.VolumeMount{{Name: "creds", MountPath: "/etc/legion-operator/creds", ReadOnly: true}},
	}
	cfg.Runtime.Kubernetes.Pod = pod
	opts, err := sandboxOptions(cfg, *cfg.Runtime.Kubernetes, "test", sandbox.DispatchCredential{}, lookup(nil), quietLogger())
	if err != nil {
		t.Fatalf("sandboxOptions: %v", err)
	}
	want := sandbox.Pod{Env: pod.Env, Volumes: pod.Volumes, VolumeMounts: pod.VolumeMounts, ServiceAccount: pod.ServiceAccount}
	if !reflect.DeepEqual(opts.Pod, want) {
		t.Errorf("the runtime's Pod is %+v, want %+v", opts.Pod, want)
	}
	if keys := map[string]string{"ANTHROPIC_API_KEY": "anthropic"}; !reflect.DeepEqual(opts.ProviderKeys, keys) {
		t.Errorf("the runtime's ProviderKeys are %v, want %v", opts.ProviderKeys, keys)
	}
}

// The NATS nkey seed reaches every pod from the providers Secret, never from a copy in a claim's
// Secret: whenever the daemon has one — from nats_nkey_seed_file, NATS_NKEY_SEED_FILE, or
// NATS_NKEY_SEED — it is a launch secret the runtime reads from the providers mount, and with none
// it is neither.
func TestTheNatsSeedReachesTheSandboxRuntimeAsTheProvidersSecrets(t *testing.T) {
	t.Parallel()
	seedFile := testnkeys.SeedFile(t, testnkeys.UserSeed(t))
	for _, tc := range []struct {
		name string
		key  string
		env  map[string]string
		want []string
	}{
		{"nats_nkey_seed_file", seedFile, nil, []string{"NATS_NKEY_SEED"}},
		{"NATS_NKEY_SEED_FILE", "", map[string]string{"NATS_NKEY_SEED_FILE": seedFile}, []string{"NATS_NKEY_SEED"}},
		{"NATS_NKEY_SEED", "", map[string]string{"NATS_NKEY_SEED": "SU…"}, []string{"NATS_NKEY_SEED"}},
		{"no seed", "", nil, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := kubernetesConfig(t, "https://127.0.0.1:1")
			cfg.NatsNkeySeedFile = tc.key
			opts, err := sandboxOptions(cfg, *cfg.Runtime.Kubernetes, "test", sandbox.DispatchCredential{}, lookup(tc.env), quietLogger())
			if err != nil {
				t.Fatalf("sandboxOptions: %v", err)
			}
			if !reflect.DeepEqual(opts.ProvidersSecrets, tc.want) {
				t.Errorf("ProvidersSecrets = %v, want %v", opts.ProvidersSecrets, tc.want)
			}
			if got := slices.Contains(opts.LaunchSecrets, "NATS_NKEY_SEED"); got != (tc.want != nil) {
				t.Errorf("LaunchSecrets = %v: carries NATS_NKEY_SEED %t, want %t", opts.LaunchSecrets, got, tc.want != nil)
			}
		})
	}
}

// awaitHealthz returns once the daemon answers /healthz, and fails if it exits first.
func awaitHealthz(t *testing.T, cfg config.Config, done chan error) {
	t.Helper()
	var err error
	if !testwait.Holds(15*time.Second, 20*time.Millisecond, func() bool {
		var response *http.Response
		response, err = pollClient.Get("http://127.0.0.1:" + strconv.Itoa(cfg.Port) + "/healthz")
		if err == nil {
			response.Body.Close()
			if response.StatusCode == http.StatusOK {
				return true
			}
		}
		select {
		case exited := <-done:
			done <- exited
			t.Fatalf("the daemon exited before it answered /healthz: %v", exited)
		default:
		}
		return false
	}) {
		t.Fatalf("the daemon never answered /healthz: %v", err)
	}
}

// Each duration key reaches the runtime option that takes it, under either runtime: every value
// here is distinct, so feeding an option from its neighbouring key fails a row.
// worker_stop_timeout_seconds in particular has no other test: it is tmux's stop grace and the
// sandbox's termination grace.
func TestEveryDurationKeyReachesTheRuntimeOptionThatTakesIt(t *testing.T) {
	t.Parallel()
	cfg := kubernetesConfig(t, "https://127.0.0.1:1")
	cfg.WorkerStopTimeout, cfg.WorkerBootTimeout, cfg.ProbeInterval, cfg.SlowCommandTimeout = 11*time.Second, 22*time.Second, 33*time.Second, 44*time.Second

	opts, err := sandboxOptions(cfg, *cfg.Runtime.Kubernetes, "test", sandbox.DispatchCredential{}, lookup(nil), quietLogger())
	if err != nil {
		t.Fatalf("sandboxOptions: %v", err)
	}
	panes := tmuxOptions(cfg, "test", nil, "", "", quietLogger())
	for _, row := range []struct {
		option    string
		got, want time.Duration
	}{
		{"sandbox TerminationGrace", opts.TerminationGrace, cfg.WorkerStopTimeout},
		{"sandbox BootTimeout", opts.BootTimeout, cfg.WorkerBootTimeout},
		{"sandbox ProbeInterval", opts.ProbeInterval, cfg.ProbeInterval},
		{"sandbox AdoptTimeout", opts.AdoptTimeout, cfg.SlowCommandTimeout},
		{"tmux StopGrace", panes.StopGrace, cfg.WorkerStopTimeout},
		{"tmux ProbeInterval", panes.ProbeInterval, cfg.ProbeInterval},
		{"tmux AdoptTimeout", panes.AdoptTimeout, cfg.SlowCommandTimeout},
	} {
		if row.got != row.want {
			t.Errorf("%s = %s, want %s", row.option, row.got, row.want)
		}
	}
}

// sandboxOptions carries runtime.kubernetes.agent_secrets straight into the runtime's Options,
// nil when the configuration sets no block, and the worker image's agent-secrets binary is
// always the tools path (Task 11 mounts it there whether or not the deployment enrolls).
func TestSandboxOptionsCarryTheAgentSecretsBlock(t *testing.T) {
	t.Parallel()
	cfg := kubernetesConfig(t, "https://127.0.0.1:1") // the file's fixture (`:32-41`): testConfig under runtime: kubernetes
	cfg.Runtime.Kubernetes.AgentSecrets = &config.AgentSecretsConfig{
		URL: "https://secrets.internal.example", Audience: "agent-secrets", TokenExpirySeconds: 1800, ExchangeAudience: "legion-broker-exchange",
	}
	opts, err := sandboxOptions(cfg, *cfg.Runtime.Kubernetes, "test", sandbox.DispatchCredential{}, lookup(nil), quietLogger())
	if err != nil {
		t.Fatal(err)
	}
	want := &sandbox.AgentSecrets{URL: "https://secrets.internal.example", Audience: "agent-secrets", TokenExpiry: 30 * time.Minute, ExchangeAudience: "legion-broker-exchange"}
	if opts.AgentSecrets == nil || *opts.AgentSecrets != *want {
		t.Fatalf("AgentSecrets = %+v, want %+v", opts.AgentSecrets, want)
	}
	if opts.Tools.AgentSecrets != "/opt/legion/bin/agent-secrets" {
		t.Fatalf("Tools.AgentSecrets = %q", opts.Tools.AgentSecrets)
	}
	cfg.Runtime.Kubernetes.AgentSecrets = nil
	opts, _ = sandboxOptions(cfg, *cfg.Runtime.Kubernetes, "test", sandbox.DispatchCredential{}, lookup(nil), quietLogger())
	if opts.AgentSecrets != nil {
		t.Fatalf("AgentSecrets = %+v without the block", opts.AgentSecrets)
	}
}

// sandboxOptions hands the runtime the project's slack_channels, which it tells the controller it
// launches as LEGION_SLACK_CHANNELS and compares with the list a running controller was launched
// under (sandbox's address record), so a changed list relaunches it; no channel is no list.
func TestSandboxOptionsCarryTheProjectsSlackChannels(t *testing.T) {
	t.Parallel()
	cfg := kubernetesConfig(t, "https://127.0.0.1:1")
	project := cfg.Projects[cfg.Project]
	project.SlackChannels = []string{"C00000000", "G00000001"}
	cfg.Projects = map[string]config.Project{cfg.Project: project}
	opts, err := sandboxOptions(cfg, *cfg.Runtime.Kubernetes, "test", sandbox.DispatchCredential{}, lookup(nil), quietLogger())
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"C00000000", "G00000001"}; !slices.Equal(opts.ControllerSlackChannels, want) {
		t.Fatalf("ControllerSlackChannels = %q, want %q", opts.ControllerSlackChannels, want)
	}
	project.SlackChannels = nil
	cfg.Projects[cfg.Project] = project
	if opts, _ = sandboxOptions(cfg, *cfg.Runtime.Kubernetes, "test", sandbox.DispatchCredential{}, lookup(nil), quietLogger()); len(opts.ControllerSlackChannels) != 0 {
		t.Fatalf("ControllerSlackChannels = %q with no channel, want none", opts.ControllerSlackChannels)
	}
}

// No credential material — no key, no launcher credential, no bearer, no ServiceAccount token —
// ever reaches a pod or a launch secret: the daemon's login reads its own token from token_file and
// holds the key it wins only in the *cipher.Client's process memory, which sandbox.Options
// carry no field for at all. newSecretsLogin hands the runtime only the broker URL, audience and
// pod token lifetime, and no launch's secrets ever name an AGENT_SECRETS_* variable.
func TestNoCredentialMaterialReachesAPod(t *testing.T) {
	t.Parallel()
	cfg := kubernetesConfig(t, "https://127.0.0.1:1")
	cfg.Runtime.Kubernetes.AgentSecrets = &config.AgentSecretsConfig{
		URL: "https://s", TokenFile: filepath.Join(t.TempDir(), "token"), Audience: "agent-secrets", TokenExpirySeconds: 3600,
	}
	for _, secret := range launchSecrets(cfg, lookup(nil)) {
		if strings.HasPrefix(secret.name, "AGENT_SECRETS") {
			t.Fatalf("launch secret %s: no daemon credential material may reach a pod", secret.name)
		}
	}
	opts, err := sandboxOptions(cfg, *cfg.Runtime.Kubernetes, "test", sandbox.DispatchCredential{}, lookup(nil), quietLogger())
	if err != nil {
		t.Fatal(err)
	}
	if opts.AgentSecrets == nil || opts.AgentSecrets.URL != "https://s" || opts.AgentSecrets.Audience != "agent-secrets" || opts.AgentSecrets.TokenExpiry != time.Hour {
		t.Fatalf("AgentSecrets = %+v, want only the URL, audience and token lifetime the pod's own client needs", opts.AgentSecrets)
	}
	enroller, registrar, client := newSecretsLogin(cfg, quietLogger())
	if enroller == nil || client == nil {
		t.Fatalf("enroller %v, client %v; want both", enroller, client)
	}
	if registrar != nil {
		t.Fatalf("registrar %v with no exchange_audience, want none: no pod session is registered without the key", registrar)
	}
	if client.URL != "https://s" || client.TokenFile != cfg.Runtime.Kubernetes.AgentSecrets.TokenFile {
		t.Fatalf("client = %+v, want only the configured URL and token file, never a key or a bearer", client)
	}
	cfg.Runtime.Kubernetes.AgentSecrets.ExchangeAudience = "legion-broker-exchange"
	if _, registrar, client := newSecretsLogin(cfg, quietLogger()); registrar == nil || client == nil {
		t.Fatalf("with exchange_audience: registrar %v, client %v; want both", registrar, client)
	}
	cfg.Runtime.Kubernetes.AgentSecrets = nil
	if enroller, registrar, client := newSecretsLogin(cfg, quietLogger()); enroller != nil || registrar != nil || client != nil {
		t.Fatalf("without the block: %v, %v, %v; want nil, nil, nil", enroller, registrar, client)
	}
}

// newSecretsLogin logs in at boot with the token token_file holds, posting it to the broker's token
// route, and logs that it did; no confirmation code exists, so nothing waits for a person.
func TestNewSecretsLoginLogsInWithTheDaemonsToken(t *testing.T) {
	t.Parallel()
	var mu sync.Mutex
	var tokens []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/launcher-credentials/token" {
			http.NotFound(w, r)
			return
		}
		var body struct {
			Token string `json:"token"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		mu.Lock()
		tokens = append(tokens, body.Token)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"credential_id": "cred-1", "expires_at": time.Now().Add(time.Hour).UTC().Format(time.RFC3339)})
	}))
	defer server.Close()

	tokenFile := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(tokenFile, []byte("the-daemon's-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := kubernetesConfig(t, "https://127.0.0.1:1")
	cfg.Runtime.Kubernetes.AgentSecrets = &config.AgentSecretsConfig{
		URL: server.URL, TokenFile: tokenFile, Audience: "agent-secrets", TokenExpirySeconds: 3600,
	}
	var logged lockedBuffer
	enroller, _, client := newSecretsLogin(cfg, slog.New(slog.NewJSONHandler(&logged, nil)))
	if enroller == nil || client == nil {
		t.Fatal("want an enroller and a client")
	}
	if !testwait.Holds(2*time.Second, time.Millisecond, func() bool {
		return strings.Contains(logged.String(), "agent-secrets: logged in with the daemon's ServiceAccount token") && client.LoginStatus().State == "issued"
	}) {
		t.Fatalf("log = %s, state %q; want the token login logged and issued", logged.String(), client.LoginStatus().State)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(tokens) != 1 || tokens[0] != "the-daemon's-token" {
		t.Fatalf("the broker saw tokens %q, want the token file's one", tokens)
	}
	if strings.Contains(logged.String(), "enter code") {
		t.Fatalf("log = %s, want no confirmation code", logged.String())
	}
}

// The machines' Enroller hands the broker the claim a process runs for beside the rest of its
// identity, so the broker files the pod's requests under the claim and the claim's next pod reads
// them.
func TestTheBrokerEnrollerSendsTheClaim(t *testing.T) {
	t.Parallel()
	var mu sync.Mutex
	var enrolled []map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/launcher-credentials/token":
			_ = json.NewEncoder(w).Encode(map[string]string{"credential_id": "cred-1", "expires_at": time.Now().Add(time.Hour).UTC().Format(time.RFC3339)})
		case r.Method == http.MethodPost && r.URL.Path == "/v1/enrollments":
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			mu.Lock()
			enrolled = append(enrolled, body)
			mu.Unlock()
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"enrollment_id":"enr-1","lease_expires_at":"2026-09-26T12:15:00Z"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	tokenFile := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(tokenFile, []byte("the-daemon's-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := kubernetesConfig(t, "https://127.0.0.1:1")
	cfg.Runtime.Kubernetes.AgentSecrets = &config.AgentSecretsConfig{
		URL: server.URL, TokenFile: tokenFile, Audience: "agent-secrets", TokenExpirySeconds: 3600,
	}
	enroller, _, client := newSecretsLogin(cfg, quietLogger())
	if !testwait.Holds(5*time.Second, time.Millisecond, func() bool { return client.LoginStatus().State == "issued" }) {
		t.Fatalf("login state %q, want issued", client.LoginStatus().State)
	}
	const claim = "legion-acme-acme-12-planner"
	id, err := enroller.Enroll(context.Background(), supervise.PodEnrollment{
		PodUID: "pod-uid-1", Slot: "planner-g1", Thumbprint: "tp", PodToken: "a.b.c", Session: "ses_planner", Claim: claim,
	})
	if err != nil || id != "enr-1" {
		t.Fatalf("Enroll = %q, %v; want enr-1", id, err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(enrolled) != 1 || enrolled[0]["claim"] != claim || enrolled[0]["session_id"] != "ses_planner" || enrolled[0]["slot"] != "planner-g1" {
		t.Fatalf("the broker was sent %v, want one enrollment stating claim %s beside its session and slot", enrolled, claim)
	}
}

// lockedBuffer is a log a test reads while a goroutine writes it.
type lockedBuffer = testlog.Buffer

// A hello carrying an agent-secrets identity maps to the machine's StreamHello with it; a hello
// with none maps to one with no identity.
func TestAHelloWithAnIdentityMapsToTheMachinesEvent(t *testing.T) {
	t.Parallel()
	ev, err := superviseEvent(stream.Hello{Claim: "legion-LEGION-209-implementer", Generation: 2,
		AgentSecrets: &stream.AgentSecretsIdentity{Thumbprint: "tp", PodToken: "a.b.c"}})
	if err != nil {
		t.Fatal(err)
	}
	hello := ev.(supervise.StreamHello)
	if hello.Generation != 2 || hello.AgentSecrets == nil || hello.AgentSecrets.Thumbprint != "tp" || hello.AgentSecrets.PodToken != "a.b.c" {
		t.Fatalf("mapped %+v", hello)
	}
	ev, _ = superviseEvent(stream.Hello{Claim: "legion-LEGION-209-implementer", Generation: 2})
	if ev.(supervise.StreamHello).AgentSecrets != nil {
		t.Fatal("a hello with no identity mapped to one")
	}
}

// ProbeBudget is the image probe's attempt budget: slow_command_timeout_seconds, or
// worker_boot_timeout_seconds plus probeBudgetMargin when that is larger — derived true only then
// — so the capacity stop (sandbox.Runtime.stuck) always has room, within one attempt, to see a
// probe pod's Unschedulable age pass the boot timeout (LEGION-636: a deployment whose
// worker_boot_timeout_seconds reached slow_command_timeout_seconds never got there). imageProbe
// carries the same budget onto its ImageProbe.
func TestProbeBudgetIsTheLargerOfTheTwoTimeoutsPlusAMargin(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		bootTimeout, slowCommand time.Duration
		wantBudget               time.Duration
		wantDerived              bool
	}{
		{120 * time.Second, 300 * time.Second, 300 * time.Second, false},
		{600 * time.Second, 300 * time.Second, 660 * time.Second, true},
		{240 * time.Second, 300 * time.Second, 300 * time.Second, false},
		{241 * time.Second, 300 * time.Second, 301 * time.Second, true},
	} {
		cfg := config.Config{WorkerBootTimeout: tc.bootTimeout, SlowCommandTimeout: tc.slowCommand}
		budget, derived := ProbeBudget(cfg)
		if budget != tc.wantBudget || derived != tc.wantDerived {
			t.Errorf("ProbeBudget(boot=%s, slow=%s) = %s, derived %v; want %s, derived %v",
				tc.bootTimeout, tc.slowCommand, budget, derived, tc.wantBudget, tc.wantDerived)
		}
		probe, err := imageProbe(cfg, promptrefs.New())
		if err != nil {
			t.Fatalf("imageProbe: %v", err)
		}
		if probe.Budget != tc.wantBudget {
			t.Errorf("imageProbe(boot=%s, slow=%s).Budget = %s, want %s", tc.bootTimeout, tc.slowCommand, probe.Budget, tc.wantBudget)
		}
	}
}

// The boot log carries the derived budget, naming both keys, only when ProbeBudget derives one
// above slow_command_timeout_seconds; a daemon whose worker_boot_timeout_seconds leaves it under
// that bound logs nothing of it.
func TestTheBootLogNamesTheDerivedProbeBudgetOnlyWhenItIsDerived(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name        string
		bootTimeout time.Duration
		wantLogged  bool
	}{
		{"derived", 600 * time.Second, true},
		{"not derived", 120 * time.Second, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			cfg := config.Config{WorkerBootTimeout: tc.bootTimeout, SlowCommandTimeout: 300 * time.Second}
			logProbeBudget(slog.New(slog.NewJSONHandler(&buf, nil)), cfg)
			got := buf.String()
			logged := strings.Contains(got, `"msg":"daemon: the image probe's attempt outlives slow_command_timeout_seconds"`)
			if logged != tc.wantLogged {
				t.Fatalf("logProbeBudget(worker_boot_timeout_seconds=%s) logged = %v, want %v:\n%s", tc.bootTimeout, logged, tc.wantLogged, got)
			}
			if tc.wantLogged {
				budget, _ := ProbeBudget(cfg)
				for _, want := range []string{
					fmt.Sprintf(`"budgetSeconds":%d`, int(budget.Seconds())),
					fmt.Sprintf(`"slow_command_timeout_seconds":%d`, int(cfg.SlowCommandTimeout.Seconds())),
					fmt.Sprintf(`"worker_boot_timeout_seconds":%d`, int(cfg.WorkerBootTimeout.Seconds())),
				} {
					if !strings.Contains(got, want) {
						t.Errorf("logProbeBudget wrote %q, want it to contain %q", got, want)
					}
				}
			}
		})
	}
}
