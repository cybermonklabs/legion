package daemon

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/notify"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/runtime/fake"
	"github.com/sjawhar/legion/daemon/internal/testwait"
)

// envoyRequest is one request an Envoy listener was asked: its method, its path, the bearer it
// carried and, for a publish, the topic it named.
type envoyRequest struct{ method, path, bearer, topic string }

// envoyRecorder is one Envoy listener as every reader of the daemon's Envoy holder reaches it: the
// publish route the outbox and the settings announcer take, the registry read and the session
// delete of a clean stop, and the role lookup of the controller's liveness probe, which it answers
// with nobody holding the role. It records every request and takes every one.
type envoyRecorder struct {
	url   string
	mu    sync.Mutex
	asked []envoyRequest
}

func newEnvoyRecorder(t *testing.T) *envoyRecorder {
	t.Helper()
	l := &envoyRecorder{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asked := envoyRequest{method: r.Method, path: r.URL.Path, bearer: r.Header.Get("Authorization")}
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/messages/publish":
			var published listenerPublish
			_ = json.NewDecoder(r.Body).Decode(&published)
			asked.topic = published.Topic
			_, _ = io.WriteString(w, `{}`)
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/v1/registry/"):
			_, _ = fmt.Fprintf(w, `{"session_id":%q,"client_instance_id":"inst_dead","last_seen":%d}`,
				strings.TrimPrefix(r.URL.Path, "/v1/registry/"), time.Now().Add(-time.Minute).UnixMilli())
		case r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, "/v1/sessions/"):
			_, _ = io.WriteString(w, `{"deleted":true}`)
		default:
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, `{"error":"unclaimed","reason":"unclaimed"}`)
		}
		l.mu.Lock()
		defer l.mu.Unlock()
		l.asked = append(l.asked, asked)
	}))
	t.Cleanup(server.Close)
	l.url = server.URL
	return l
}

// requests is every request the listener was asked that is the one wanted.
func (l *envoyRecorder) requests(wanted envoyRequest) []envoyRequest {
	l.mu.Lock()
	defer l.mu.Unlock()
	var matched []envoyRequest
	for _, asked := range l.asked {
		if asked.method == wanted.method && asked.path == wanted.path && (wanted.topic == "" || asked.topic == wanted.topic) {
			matched = append(matched, asked)
		}
	}
	return matched
}

// reachedOnlyTheNewListener waits for the request wanted to reach either listener, and fails the
// test unless it reached renewed alone, every time with bearer.
func reachedOnlyTheNewListener(t *testing.T, what string, old, renewed *envoyRecorder, bearer string, wanted envoyRequest) {
	t.Helper()
	if !testwait.Holds(10*time.Second, testwait.Poll, func() bool {
		return len(old.requests(wanted))+len(renewed.requests(wanted)) > 0
	}) {
		t.Fatalf("%s reached neither listener", what)
	}
	if asked := old.requests(wanted); len(asked) != 0 {
		t.Errorf("%s reached the listener the daemon booted on, %+v: its reader kept what boot built", what, asked)
	}
	for _, asked := range renewed.requests(wanted) {
		if asked.bearer != bearer {
			t.Errorf("%s reached the listener the change named with %q, want %q", what, asked.bearer, bearer)
		}
	}
}

// One whole daemon, booted on a legion.yaml with a workflow on real NATS, keeps every reader of a
// rebuilt holder on what the rebuild swapped in (rebuilder.go: a reader holds the holder, never
// the thing inside it), changed through the settings route an operator calls.
//
// A changed worker_rpc_timeout_seconds reopens the intake consumers, and the state's consumer lag
// reads the ones the rebuild opened rather than the ones it closed. A changed envoy_url and
// envoy_token_file reach every reader of the Envoy listener: the outbox's notice, the clean stop
// of a dead role's session, the controller's liveness probe, and the next launch's bearer.
func TestARunningDaemonsReadersFollowARebuiltHolder(t *testing.T) {
	natsURL := workflowNATS(t)
	cfg := workflowConfig(t, natsURL)
	old, renewed := newEnvoyRecorder(t), newEnvoyRecorder(t)
	dir := t.TempDir()
	oldToken, newToken := filepath.Join(dir, "old-envoy-token"), filepath.Join(dir, "new-envoy-token")
	for path, token := range map[string]string{oldToken: "old-envoy-bearer", newToken: "new-envoy-bearer"} {
		if err := os.WriteFile(path, []byte(token+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	project, err := claim.ProjectToken(cfg.Project)
	if err != nil {
		t.Fatal(err)
	}
	recordControllerSession(t, cfg, project, "ses_controller")
	path := filepath.Join(dir, "legion.yaml")
	body := fmt.Sprintf(`project: %[1]s
port: %[2]d
bind: 127.0.0.1
postgres_dsn: %[3]s
state_dir: %[4]s
runtime: tmux
daemon_url: http://127.0.0.1:%[2]d
operator_token_file: %[5]s
controller: operator
launch_failure_limit: 1
dispatch_url: %[6]s
dispatch_token_file: %[7]s
nats_urls: [%[8]s]
projects:
  %[1]s: {}
github_apps:
  implement: { app_id: "1", private_key: implement-test-key }
  review: { app_id: "2", private_key: review-test-key }
envoy_url: %[9]s
envoy_token_file: %[10]s
`, cfg.Project, cfg.Port, cfg.PostgresDSN, cfg.StateDir, cfg.OperatorTokenFile, cfg.DispatchURL, cfg.DispatchTokenFile, natsURL, old.url, oldToken)
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	live, err := config.NewLive(path, func(string) string { return "" }, nil)
	if err != nil {
		t.Fatalf("resolve %s: %v", path, err)
	}
	rt := fake.NewRuntime()
	o := fakeRuntime(rt, &built{})
	o.workflowTokens = &workflowTokenRecorder{}
	o.orphanSweep = 100 * time.Millisecond
	logs := &syncBuffer{}
	d := startDaemonOn(t, live, o, slog.New(slog.NewJSONHandler(logs, nil)), 30*time.Second)
	change := func(changes map[string]string) {
		t.Helper()
		request := api.SettingsChangeRequest{By: "ana@example.com", Changes: map[string]json.RawMessage{}}
		for key, value := range changes {
			request.Changes[key] = json.RawMessage(value)
		}
		status, answer := settingsRequest(t, d, testOperatorToken, request)
		for key := range changes {
			if status != http.StatusOK || answer[key].Applied != string(config.OutcomeRebuilt) {
				t.Fatalf("the settings route answered %s with %d %+v, want it rebuilt", key, status, answer[key])
			}
		}
	}

	// The NATS half: the route answers once the consumers are reopened.
	change(map[string]string{"worker_rpc_timeout_seconds": "9"})
	consumers := d.state().Consumers
	if len(consumers) == 0 {
		t.Fatal("the state shows no consumers, so this proves nothing")
	}
	for _, consumer := range consumers {
		if consumer.Unavailable != "" {
			t.Errorf("the state's lag of %s after the rebuild is unavailable (%s): it reads the consumers the rebuild closed", consumer.Name, consumer.Unavailable)
		}
	}

	// The Envoy half. The probe has asked the boot's listener before anything changes.
	lookup := envoyRequest{method: http.MethodGet, path: "/v1/roles/" + string(claim.ControllerToken(project))}
	testwait.Eventually(t, "the liveness probe to ask the listener the daemon booted on", func() bool { return len(old.requests(lookup)) > 0 })
	change(map[string]string{"envoy_url": strconv.Quote(renewed.url), "envoy_token_file": strconv.Quote(newToken)})
	const bearer = "Bearer new-envoy-bearer"
	for holder, variable := range map[string]string{config.HolderRuntime: "ENVOY_URL", config.HolderLaunchSpecs: "ENVOY_TOKEN"} {
		if !strings.Contains(logs.String(), `"msg":"legion settings: holder rebuilt","holder":"`+holder+`"`) {
			t.Errorf("the Envoy change did not rebuild the %s, which every launch reads %s from", holder, variable)
		}
	}

	// An operator's architect on a tree of its own, launched after the change and ready.
	hostWorkspace(t, cfg.StateDir, "acme/widgets", "OPS-1")
	architect := d.spawn(api.SpawnRequest{Tree: "OPS-1", Issue: "OPS-1", Role: claim.RoleArchitect, Prompt: "Reply ready and wait."})
	readyClaim(t, d, rt, architect)
	if got := lastLaunch(t, rt, architect).Secrets["ENVOY_TOKEN"]; got != "new-envoy-bearer" {
		t.Errorf("the architect launched after the change carries ENVOY_TOKEN %q, want the new bearer", got)
	}

	// The workflow's running tree, whose planner's one launch fails: the outbox tells that tree's
	// architect its issue is held.
	issue := cfg.Project + "-1"
	recordRunningTree(t, cfg.PostgresDSN, cfg.Project, issue)
	hostWorkspace(t, cfg.StateDir, "acme/widgets", issue)
	held, err := claim.NewToken(project, issue, claim.RoleArchitect)
	if err != nil {
		t.Fatal(err)
	}
	rt.ScriptSpawn(fake.SpawnResult{Err: errors.New("pane launch failed")})
	if status, body := d.request(http.MethodPost, "/legion/v1/operator/claims", api.SpawnRequest{Tree: issue, Issue: issue, Role: claim.RolePlanner, Prompt: "Plan it."}, true); status != http.StatusInternalServerError {
		t.Fatalf("spawn the planner = %d %s, want its one launch refused", status, body)
	}
	reachedOnlyTheNewListener(t, "the outbox's held notice", old, renewed, bearer, envoyRequest{
		method: http.MethodPost, path: "/v1/messages/publish", topic: notify.RoleTopicPrefix + string(held)})

	// The operator's architect's process dies: the clean stop reads its registration and deletes it.
	rt.Emit(runtime.Observation{Locator: *d.claim(architect).Locator, Kind: runtime.Gone})
	session := "ses_" + string(architect)
	reachedOnlyTheNewListener(t, "the clean stop's registry read", old, renewed, bearer, envoyRequest{method: http.MethodGet, path: "/v1/registry/" + session})
	reachedOnlyTheNewListener(t, "the clean stop's delete", old, renewed, bearer, envoyRequest{method: http.MethodDelete, path: "/v1/sessions/" + session})

	// The liveness probe: once it has asked the new listener, it never asks the old one again.
	if !testwait.Holds(5*time.Second, testwait.Poll, func() bool { return len(renewed.requests(lookup)) > 0 }) {
		t.Fatal("the controller's liveness probe never asked the listener the change named: it reads the URL the daemon booted on")
	}
	asked := len(old.requests(lookup))
	testwait.Eventually(t, "two more probes", func() bool { return len(renewed.requests(lookup)) >= 3 })
	if again := len(old.requests(lookup)); again != asked {
		t.Errorf("the liveness probe asked the listener the daemon booted on %d more times after it asked the new one", again-asked)
	}
	for _, probed := range renewed.requests(lookup) {
		if probed.bearer != bearer {
			t.Errorf("the liveness probe asked the new listener with %q, want %q", probed.bearer, bearer)
		}
	}
}
