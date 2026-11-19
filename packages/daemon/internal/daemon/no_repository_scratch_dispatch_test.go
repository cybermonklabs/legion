package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nats-io/nats.go/jetstream"

	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/runtime/fake"
	"github.com/sjawhar/legion/daemon/internal/testnats"
	"github.com/sjawhar/legion/daemon/internal/testwait"
)

// A daemon boots and serves for a project that has no repository of its own — no default and no
// row in Dispatch's repo-projects table — since it reads that table for nothing: an issue's
// repository is Dispatch's resolution of the issue, and the repositories it prepares are the ones
// its records name, of which a new project has none (check a-go, item 1). The first case runs the
// real Dispatch (startScratchDispatchWithNATS, the harness of
// blockers_scratch_dispatch_test.go); the second a stand-in Dispatch that answers 404 to every
// request under /api/v1/settings/repo-projects and counts them, and that publishes Dispatch's own
// settings.repo_project.updated event on the project's subject after the boot: the count is 0 at
// the boot and after the event. Control: with the boot's read of Dispatch's repo-projects table
// put back, the first case refuses its boot for a project that table names no repository for,
// and the second counts the boot's request and fails on its 404.
func TestADaemonBootsAndServesForAProjectWithNoRepository(t *testing.T) {
	t.Run("the real Dispatch, a project with no default and no repository row", func(t *testing.T) {
		// The NATS server is taken first: testnats.URL empties it of every stream, and Dispatch
		// reconciles ENVOY_NOTIFICATIONS on it on its first connection.
		natsURL := testnats.URL(t)
		baseURL, token := startScratchDispatchWithNATS(t, natsURL)
		cfg := workflowConfig(t, natsURL)
		// Dispatch takes a project key of at most ten characters; workflowConfig's is longer.
		cfg.Project = "N" + randomSuffix(t)[:9]
		cfg.Projects = map[string]config.Project{cfg.Project: {}}
		cfg.PostgresDSN = blockersDatabase(t)
		cfg.DispatchURL = baseURL
		if err := os.WriteFile(cfg.DispatchTokenFile, []byte(token+"\n"), 0o600); err != nil {
			t.Fatalf("write the scratch Dispatch bearer: %v", err)
		}
		// The project alone: no PUT under /api/v1/settings/repo-projects, so Dispatch holds no
		// default repository for it and no row naming it.
		dispatchCall(t, baseURL, http.MethodPost, "/api/v1/projects", map[string]string{"key": cfg.Project, "name": "No repository"}, nil)
		o := fakeRuntime(fake.NewRuntime(), &built{})
		o.workflowTokens = &workflowTokenRecorder{}
		o.githubAPI = blockersGitHub(t)
		servesWithNoRepository(t, startDaemon(t, cfg, o))
	})

	t.Run("a Dispatch that answers 404 under /api/v1/settings/repo-projects", func(t *testing.T) {
		var settings atomic.Int64
		natsURL := workflowNATS(t)
		cfg := workflowConfigOn(t, natsURL, func(t *testing.T, project string) string {
			return dispatchBoard(t, func(w http.ResponseWriter, r *http.Request) {
				switch {
				case strings.HasPrefix(r.URL.Path, "/api/v1/settings/repo-projects"):
					settings.Add(1)
					http.NotFound(w, r)
				case servedBootCall(t, w, r, project):
				case r.Method == http.MethodGet && r.URL.Path == "/api/v1/issues":
					w.Header().Set("Content-Type", "application/json")
					if err := json.NewEncoder(w).Encode([]any{}); err != nil {
						t.Errorf("write Dispatch issues: %v", err)
					}
				default:
					http.NotFound(w, r)
				}
			})
		})
		o := fakeRuntime(fake.NewRuntime(), &built{})
		o.workflowTokens = &workflowTokenRecorder{}
		servesWithNoRepository(t, startDaemon(t, cfg, o))
		if n := settings.Load(); n != 0 {
			t.Fatalf("the boot asked Dispatch %d times under /api/v1/settings/repo-projects, want 0", n)
		}

		js, err := jetstream.New(testnats.Connect(t, natsURL))
		if err != nil {
			t.Fatalf("open JetStream: %v", err)
		}
		if _, err := js.Publish(context.Background(), "notifications.dispatch.project."+cfg.Project+".settings.repo_project.updated", repoProjectEnvelope(t, cfg.Project)); err != nil {
			t.Fatalf("publish Dispatch's settings event: %v", err)
		}
		// Nothing of the daemon hears the event, so there is no moment its handling ends to wait
		// for: the daemon is given two seconds, far beyond a delivery and a read, to ask anything.
		time.Sleep(2 * time.Second)
		if n := settings.Load(); n != 0 {
			t.Fatalf("Dispatch was asked %d times under /api/v1/settings/repo-projects after its settings event, want 0", n)
		}
	})
}

// servesWithNoRepository waits for d to answer /readyz 200 and requires its state to list no
// repository: a project with no record names none.
func servesWithNoRepository(t *testing.T, d *daemon) {
	t.Helper()
	if !testwait.Holds(testwait.Bounded(t, 30*time.Second, 10*time.Second), 20*time.Millisecond, func() bool {
		status, _ := d.request(http.MethodGet, "/readyz", nil, false)
		return status == http.StatusOK
	}) {
		status, body := d.request(http.MethodGet, "/readyz", nil, false)
		t.Fatalf("GET /readyz = %d %s, want 200: the daemon serves a project with no repository", status, body)
	}
	status, body := d.request(http.MethodGet, "/legion/v1/state", nil, false)
	if status != http.StatusOK {
		t.Fatalf("GET the state = %d; body %s", status, body)
	}
	var state api.State
	if err := json.Unmarshal(body, &state); err != nil {
		t.Fatalf("decode the state %s: %v", body, err)
	}
	if len(state.Repositories.Live) != 0 {
		t.Fatalf("the state's repositories.live = %+v, want none: no record names a repository", state.Repositories.Live)
	}
}

// repoProjectEnvelope is Dispatch's settings.repo_project.updated event for project, a row of its
// repo-projects table naming acme/widgets, as Envoy carries it on the project's subject.
func repoProjectEnvelope(t *testing.T, project string) []byte {
	t.Helper()
	event, err := json.Marshal(map[string]any{"id": 70, "issue_key": nil, "artifact_id": nil, "project": project, "seq": 0, "type": "settings.repo_project.updated",
		"actor": map[string]string{"kind": "user", "id": "smoke"}, "notify": true, "created_at": "2026-10-16T18:00:00Z",
		"payload": map[string]any{"mapping": map[string]any{"repo": "acme/widgets", "project": project, "default": true}, "deleted": false}})
	if err != nil {
		t.Fatal(err)
	}
	envelope, err := json.Marshal(map[string]any{"event_id": fmt.Sprintf("dispatch-70-%s", project), "source": "dispatch", "source_event_id": "70", "topic": "settings",
		"dedupe_key": fmt.Sprintf("dispatch-70-%s", project), "issued_at": 1, "payload_summary": "repository settings updated", "payload": string(event), "trace_id": "settings"})
	if err != nil {
		t.Fatal(err)
	}
	return envelope
}
