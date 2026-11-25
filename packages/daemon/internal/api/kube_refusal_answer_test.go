package api

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	"github.com/sjawhar/envoy/oauth2call/oauth2calltest"
	"github.com/sjawhar/legion/daemon/internal/appauth"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/ghconfig"
	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/ompsessions"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/redact"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/runtime/fake"
	"github.com/sjawhar/legion/daemon/internal/runtime/sandbox"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/testlog"
)

// kubeRefusalBearer is the bearer the Sandbox runtime's clients carry to the Kubernetes API
// stand-in, as the in-cluster ServiceAccount token is carried to the API server: it holds one of
// the made-up credentials oauth2calltest.RequireNone looks for (redacttest's Token), so an answer
// or a line that holds it fails its row.
const kubeRefusalBearer = "kube-bearer-" + oauth2calltest.Token

// kubeRefusalNamespace is the namespace the Sandbox runtime works in.
const kubeRefusalNamespace = "legion"

// kubeRefusalOperatorLine is the message operatorFailure logs an operator request it answers 500
// under.
const kubeRefusalOperatorLine = "api: an operator request failed"

// kubeRefusalKind is the apiVersion and kind of one resource the runtime's informers list and
// watch.
type kubeRefusalKind struct{ apiVersion, kind string }

// kubeRefusalLists are the list and watch paths of the runtime's informers in its namespace: its
// Sandboxes, pods and Services.
var kubeRefusalLists = map[string]kubeRefusalKind{
	"/apis/agents.x-k8s.io/v1beta1/namespaces/" + kubeRefusalNamespace + "/sandboxes": {"agents.x-k8s.io/v1beta1", "Sandbox"},
	"/api/v1/namespaces/" + kubeRefusalNamespace + "/pods":                            {"v1", "Pod"},
	"/api/v1/namespaces/" + kubeRefusalNamespace + "/services":                        {"v1", "Service"},
}

// kubeRefusalAPI is a Kubernetes API in the API server's place that refuses every request of a
// launch. It answers the runtime's informers as an API server holding nothing answers them: a
// list answers no item, and a watch that asks for its initial events (client-go's watch-list)
// gets the bookmark that ends them (k8s.io/initial-events-end) and nothing after, until its
// request or the API ends. Every other request is refused 403 Forbidden with a Status that echoes
// the Authorization the request carried in every field a server writes words into: its message,
// its details' name, group and kind, and a cause's type, message and field. It keeps the
// Authorization of every request and each request it refused.
type kubeRefusalAPI struct {
	mu      sync.Mutex
	sent    []string
	refused []kubeRefusedRequest
	// done ends every watch still open, before the server waits out its requests.
	done   chan struct{}
	config *rest.Config
}

// kubeRefusedRequest is one request the API refused.
type kubeRefusedRequest struct{ method, path string }

// newKubeRefusalAPI serves the API until t ends, with the configuration a runtime reaches it by:
// kubeRefusalBearer, in JSON both ways, since client-go's typed clients prefer protobuf for the
// built-in group unless the configuration names a content type.
func newKubeRefusalAPI(t *testing.T) *kubeRefusalAPI {
	t.Helper()
	a := &kubeRefusalAPI{done: make(chan struct{})}
	server := httptest.NewServer(a)
	t.Cleanup(server.Close)
	t.Cleanup(func() { close(a.done) })
	a.config = &rest.Config{Host: server.URL, BearerToken: kubeRefusalBearer, ContentConfig: rest.ContentConfig{
		ContentType: "application/json", AcceptContentTypes: "application/json",
	}}
	return a
}

func (a *kubeRefusalAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	a.mu.Lock()
	a.sent = append(a.sent, r.Header.Get("Authorization"))
	a.mu.Unlock()
	kind, listed := kubeRefusalLists[r.URL.Path]
	switch {
	case listed && r.Method == http.MethodGet && r.URL.Query().Get("watch") == "true":
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		if r.URL.Query().Get("sendInitialEvents") == "true" {
			_ = json.NewEncoder(w).Encode(map[string]any{"type": "BOOKMARK", "object": map[string]any{
				"apiVersion": kind.apiVersion, "kind": kind.kind,
				"metadata": map[string]any{"resourceVersion": "1", "annotations": map[string]any{metav1.InitialEventsAnnotationKey: "true"}},
			}})
		}
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
		select {
		case <-r.Context().Done():
		case <-a.done:
		}
	case listed && r.Method == http.MethodGet:
		kubeRefusalAnswer(w, http.StatusOK, map[string]any{"apiVersion": kind.apiVersion, "kind": kind.kind + "List",
			"metadata": map[string]any{"resourceVersion": "1"}, "items": []any{}})
	default:
		a.mu.Lock()
		a.refused = append(a.refused, kubeRefusedRequest{method: r.Method, path: r.URL.Path})
		a.mu.Unlock()
		carried := r.Header.Get("Authorization")
		kubeRefusalAnswer(w, http.StatusForbidden, metav1.Status{
			TypeMeta: metav1.TypeMeta{Kind: "Status", APIVersion: "v1"}, Status: metav1.StatusFailure,
			Code: http.StatusForbidden, Reason: metav1.StatusReasonForbidden, Message: "the request carried " + carried,
			Details: &metav1.StatusDetails{Name: carried, Group: carried, Kind: carried, Causes: []metav1.StatusCause{{
				Type: metav1.CauseType(carried), Message: carried, Field: carried,
			}}},
		})
	}
}

func kubeRefusalAnswer(w http.ResponseWriter, code int, object any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(object)
}

// refusedRequests is every request the API refused, in order.
func (a *kubeRefusalAPI) refusedRequests() []kubeRefusedRequest {
	a.mu.Lock()
	defer a.mu.Unlock()
	return slices.Clone(a.refused)
}

// requireSentBearer fails t unless the API was sent a request and every request it was sent
// carried kubeRefusalBearer: what answered in the API server's place held the credential the rows
// look for.
func (a *kubeRefusalAPI) requireSentBearer(t *testing.T) {
	t.Helper()
	a.mu.Lock()
	sent := slices.Clone(a.sent)
	a.mu.Unlock()
	if len(sent) == 0 {
		t.Fatal("the Kubernetes API stand-in was sent no request")
	}
	for _, s := range sent {
		if s != "Bearer "+kubeRefusalBearer {
			t.Fatalf("the Kubernetes API stand-in was sent the Authorization %q, want the bearer %q", s, kubeRefusalBearer)
		}
	}
}

// kubeRefusalClaim is the claim every row refuses a launch of: the architect of the package's
// fixture root (spawnBody), under the token its project, issue and role derive (claim.NewToken),
// which the Sandbox runtime takes from no other (runtime.ValidateSpawnSpec), so the claim the
// runtime refused and the one the operator spawns are one.
func kubeRefusalClaim(t *testing.T) supervise.Claim {
	t.Helper()
	body := spawnBody()
	token, err := claim.NewToken(testProject, body.Issue, body.Role)
	if err != nil {
		t.Fatal(err)
	}
	return supervise.Claim{Token: token, Project: testProject, Tree: body.Tree, Issue: body.Issue, Role: body.Role}
}

// KubeRefusedLaunch is the error the Sandbox runtime's Spawn returns for c's first launch when the
// Kubernetes API refuses the launch's requests, each with a Status that echoes the bearer the
// request carried (kubeRefusalAPI): the runtime itself (sandbox.New, Spawn) over client-go, with
// its informers synced against the API and its launch's first request past them refused. It is
// exported for this package's external tests (kube_refusal_projection_test.go), which build the
// state through internal/projection, a package that imports this one.
//
// It fails t unless the API refused a request of the launch, the same request sent by client-go
// with nothing between them (a typed client built from the same configuration) fails with an
// error whose text holds the echoed credential, and every request the API was sent carried the
// bearer: a row whose stand-in echoed nothing would hold none of the credential however this
// package printed the refusal, so this is what says each row's check could see one.
func KubeRefusedLaunch(t *testing.T, c supervise.Claim) error {
	t.Helper()
	api := newKubeRefusalAPI(t)
	prompt := filepath.Join(t.TempDir(), "role.md")
	if err := os.WriteFile(prompt, []byte("You are the "+string(c.Role)+".\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, stop := context.WithCancel(context.Background())
	defer stop()
	rt, err := sandbox.New(ctx, api.config, kubeRefusalSandboxOptions())
	if err != nil {
		t.Fatalf("build the Sandbox runtime: %v", err)
	}
	_, refused := rt.Spawn(ctx, runtime.SpawnSpec{
		Claim: c.Token, Project: c.Project, Tree: c.Tree, Issue: c.Issue, Role: c.Role, Generation: 1,
		BootToken: "boot-" + string(c.Token) + "-g1", Prompt: runtime.PromptParts{RolePromptPaths: []string{prompt}},
		Repository: ghrepo.MustParse("acme/widgets"), DefaultBranch: "main",
	})
	if refused == nil {
		t.Fatal("the Sandbox runtime launched the claim, want the Kubernetes API's refusal")
	}
	requests := api.refusedRequests()
	if len(requests) == 0 {
		t.Fatalf("the launch returned %q, and the Kubernetes API refused it nothing", refused)
	}

	raw, err := kubernetes.NewForConfig(api.config)
	if err != nil {
		t.Fatal(err)
	}
	first := requests[0]
	rawErr := raw.CoreV1().RESTClient().Verb(first.method).AbsPath(first.path).Do(context.Background()).Error()
	if rawErr == nil || !strings.Contains(rawErr.Error(), oauth2calltest.Token) {
		t.Fatalf("client-go's own error for the API's answer to %s %s is %v, want the echoed credential in it",
			first.method, first.path, rawErr)
	}
	api.requireSentBearer(t)
	return refused
}

// kubeRefusalSandboxOptions are the Sandbox runtime's options as a deployment's are shaped. What
// the launch's path to its first refused request reads nothing of is the plainest value configure
// takes: a store that holds nothing (kubeRefusalStore), provisioning tokens and gh credentials
// that are never asked for, and no repository cache yet.
func kubeRefusalSandboxOptions() sandbox.Options {
	reservation := corev1.ResourceRequirements{
		Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("1"), corev1.ResourceMemory: resource.MustParse("1Gi"),
			corev1.ResourceEphemeralStorage: resource.MustParse("1Gi")},
		Limits: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("1"), corev1.ResourceMemory: resource.MustParse("1Gi"),
			corev1.ResourceEphemeralStorage: resource.MustParse("2Gi")},
	}
	unasked := errors.New("no gh credential is asked for before the launch's first Kubernetes request")
	return sandbox.Options{
		Namespace: kubeRefusalNamespace, Project: testProject, Store: kubeRefusalStore{},
		Image:        "ghcr.io/example/worker@sha256:" + strings.Repeat("a", 64),
		StorageClass: "standard", IssueVolume: resource.MustParse("1Gi"),
		Resources:    sandbox.PodResources{Issue: reservation, Controller: reservation},
		StreamURL:    "tcp://127.0.0.1:13371",
		SessionStore: ompsessions.Store{Kind: ompsessions.StorePostgres, DSNSecret: "SESSION_DSN"},
		Tools: sandbox.Tools{GH: "/usr/bin/gh", Git: "/usr/bin/git", JJ: "/usr/bin/jj", Legion: "/opt/legion/bin/legion",
			AgentSecrets: "/opt/legion/bin/agent-secrets"},
		BootTimeout: 2 * time.Second, RegistrationIntervals: 3, BusyBackoff: time.Millisecond, TerminationGrace: time.Second,
		ProbeInterval: time.Hour, AdoptTimeout: time.Second,
		Tokens: kubeRefusalTokens{},
		GitHubCredential: func(context.Context, claim.Role, ghrepo.Repository) (ghconfig.Bundle, error) {
			return ghconfig.Bundle{}, unasked
		},
		ControllerCredential: func(context.Context, string) (ghconfig.Bundle, error) { return ghconfig.Bundle{}, unasked },
		GitHubDirs:           func(ghrepo.Repository, []appauth.AppRole) []string { return nil },
		Conns:                fake.NewConns(),
		RepositoryCache: sandbox.RepositoryCache{Volume: resource.MustParse("1Gi"), Refresh: 10 * time.Minute, Retain: time.Hour,
			Resources: reservation},
		CacheRepositories: func() []ghrepo.Repository { return nil },
		Log:               slog.New(slog.DiscardHandler),
	}
}

// kubeRefusalTokens is a provisioning token source a launch refused before its pod is provisioned
// never asks.
type kubeRefusalTokens struct{}

func (kubeRefusalTokens) Token(context.Context, ghrepo.Repository) (string, error) {
	return "", errors.New("no provisioning token is asked for before the launch's first Kubernetes request")
}

// kubeRefusalStore is the Sandbox runtime's store holding nothing: no tree, no claim, no
// Sandbox's home.
type kubeRefusalStore struct{}

func (kubeRefusalStore) TreeLive(context.Context, string, string) (bool, error) { return false, nil }

func (kubeRefusalStore) IssueRunsElsewhere(context.Context, string, string, string) (string, error) {
	return "", nil
}

func (kubeRefusalStore) IssueSandboxHome(context.Context, string, string) (string, error) {
	return "", nil
}

func (kubeRefusalStore) ClaimUnretired(context.Context, claim.Token) (bool, error) { return false, nil }

func (kubeRefusalStore) Claims(context.Context) ([]supervise.Claim, error) { return nil, nil }

// kubeRefusalState is the daemon's state source as it reads the claims (internal/daemon's
// source.State: the store's claims through the route's own snapshot, store.ClaimsTx), with each
// claim's view built as internal/projection builds it (api.ClaimViewOf and api.UpdateStateOf) and
// placed on its issue: the architect's on the issue's Architect, a worker's under its role. It
// reads no issue record, so the issue carries its key and its tree alone.
type kubeRefusalState struct{ store *store.Store }

func (s kubeRefusalState) State(ctx context.Context, tx pgx.Tx) (State, []supervise.Claim, error) {
	claims, err := s.store.ClaimsTx(ctx, tx)
	if err != nil {
		return State{}, nil, err
	}
	state := State{Issues: map[string]Issue{}}
	for _, c := range claims {
		view := ClaimViewOf(c)
		view.UpdateState = UpdateStateOf(c)
		issue, ok := state.Issues[c.Issue]
		if !ok {
			issue = Issue{Key: c.Issue, Tree: c.Tree, Workers: map[claim.Role]PhaseView{}}
		}
		if c.Role == claim.RoleArchitect {
			issue.Architect = &view
		} else {
			issue.Workers[c.Role] = PhaseView{Claim: view}
		}
		state.Issues[c.Issue] = issue
	}
	return state, claims, nil
}

// kubeRefusalServer is h's server as the daemon serves it: the harness's supervision and store,
// the state route reading the store through its own snapshot (kubeRefusalState over the store's
// pool), and the logger cmd/legion's start builds, slog's JSON handler under
// redact.HandlerOptions, writing into the buffer it returns.
func kubeRefusalServer(h *harness) *testlog.Buffer {
	logged := &testlog.Buffer{}
	h.handler = NewServer("127.0.0.1", 8437, Options{
		Steps:      config.DesignGateRootIssues.Steps(),
		Supervisor: h.supervisor, BootTokens: h.tokens, Project: testProject, OperatorToken: testOperatorToken, Controller: h.store,
		Log:  slog.New(slog.NewJSONHandler(logged, redact.HandlerOptions(nil))),
		Pool: h.store.Pool(), Record: record.NewStore(), Releaser: h.runtime, Trees: h.store, ReleaseRecords: h.store,
		ClaimActivity: h.store, PluginRelaunchable: true, PromptAgents: testPromptAgents,
		State: kubeRefusalState{store: h.store}, StateTransactions: h.store.Pool(),
	}).Handler
	return logged
}

// kubeRefusalLines is every line logged holds whose message is message, as the handler wrote it,
// and the text each logged under "error".
func kubeRefusalLines(t *testing.T, logged *testlog.Buffer, message string) (lines, errs []string) {
	t.Helper()
	for _, line := range strings.Split(logged.String(), "\n") {
		if line == "" {
			continue
		}
		var fields map[string]any
		if err := json.Unmarshal([]byte(line), &fields); err != nil {
			t.Fatalf("the server logged %q, which is no JSON line: %v", line, err)
		}
		if fields[slog.MessageKey] != message {
			continue
		}
		text, _ := fields["error"].(string)
		lines, errs = append(lines, line), append(errs, text)
	}
	return lines, errs
}

// TestAKubernetesRefusalOfALaunchReachesTheAPIsAnswersWithNoCredential: the operator spawns the
// root's architect (POST /legion/v1/operator/claims), and every launch of it is refused by the
// Sandbox runtime itself, its Kubernetes API refusing the launch's request with a Status that
// echoes the bearer the request carried (KubeRefusedLaunch). That refusal is the fake runtime's
// answer to each of the claim's launches, which is how the runtime's answer enters this package
// (supervise.Deps.Runtime); the machine charges each, stores the refusal as the claim's last start
// failure (storetext.FarSideError) and fails the claim once its launch budget runs out. The
// runtime keeps the Kubernetes error as its projection (projectKubeError, in resend) before it
// returns it, so none of this package's printers of it holds the bearer:
//
//   - operator-refusal: the spawn route answers the machine's refusal through operatorFailure, a
//     500 whose sentence quotes the error (refuse: claim.Refusal, written by writeAnswerFrom), and
//     logs it under kubeRefusalOperatorLine (logFailure) through the daemon's production logger.
//     Capture kubeerror.api/operator-refusal, the answer's body and the line.
//   - claim-answer: the operator's listing (GET /legion/v1/operator/claims, list) answers the
//     claim as its machine holds it (operatorView): its lastStartFailure, and the failure its spent
//     budget wrote. Capture kubeerror.api/claim-answer, the answer's body.
//   - state-answer: the state (GET /legion/v1/state, stateRoute) answers the claim as the store
//     holds it, its view built by ClaimViewOf: its lastStartFailure and its failure. Capture
//     kubeerror.api/state-answer, the answer's body.
//
// Each row holds its answer to the refusal the runtime returned: an answer that held none of it
// would pass however the runtime printed it.
func TestAKubernetesRefusalOfALaunchReachesTheAPIsAnswersWithNoCredential(t *testing.T) {
	c := kubeRefusalClaim(t)
	refused := KubeRefusedLaunch(t, c)
	h := newHarness(t)
	logged := kubeRefusalServer(h)
	answer := fake.SpawnResult{Err: refused}
	h.runtime.ScriptSpawn(answer, answer, answer)

	spawned := h.operator(http.MethodPost, "/legion/v1/operator/claims", spawnBody())

	stored := h.stored(c.Token)
	if stored.State != supervise.StateFailed || stored.LastStartFailure == "" ||
		!strings.Contains(stored.LastStartFailure, refused.Error()) || !strings.Contains(stored.Failure, refused.Error()) {
		t.Fatalf("the store holds %s at %s, last start failure %q and failure %q; want it failed, each naming the runtime's refusal %q",
			c.Token, stored.State, stored.LastStartFailure, stored.Failure, refused)
	}

	t.Run("operator-refusal", func(t *testing.T) {
		lines, errs := kubeRefusalLines(t, logged, kubeRefusalOperatorLine)
		body := spawned.Body.String()
		oauth2calltest.RequireNone(t, "kubeerror.api/operator-refusal", body+"\n"+strings.Join(lines, "\n"))
		if spawned.Code != http.StatusInternalServerError {
			t.Errorf("the spawn the runtime refused = %d, want 500; body %s", spawned.Code, body)
		}
		var refusal claim.Refusal
		if err := json.Unmarshal(spawned.Body.Bytes(), &refusal); err != nil {
			t.Fatalf("the spawn's answer %q is no refusal: %v", body, err)
		}
		if want := "spawn " + string(c.Token) + ": "; !strings.HasPrefix(refusal.Message, want) || !strings.Contains(refusal.Message, refused.Error()) {
			t.Errorf("the spawn's refusal is %q, want it to begin %q and quote the runtime's refusal %q", refusal.Message, want, refused)
		}
		if len(lines) != 1 || !strings.Contains(errs[0], refused.Error()) {
			t.Errorf("the server logged %d lines %q, %q; want one, its error the runtime's refusal %q", len(lines), kubeRefusalOperatorLine, lines, refused)
		}
	})

	t.Run("claim-answer", func(t *testing.T) {
		listed := h.operator(http.MethodGet, "/legion/v1/operator/claims", nil)
		body := listed.Body.String()
		oauth2calltest.RequireNone(t, "kubeerror.api/claim-answer", body)
		if listed.Code != http.StatusOK {
			t.Fatalf("list the claims = %d, want 200; body %s", listed.Code, body)
		}
		var list OperatorClaims
		if err := json.Unmarshal(listed.Body.Bytes(), &list); err != nil {
			t.Fatalf("the listing %q does not decode: %v", body, err)
		}
		i := slices.IndexFunc(list.Claims, func(view OperatorClaim) bool { return view.Token == c.Token })
		if i < 0 {
			t.Fatalf("the listing carries no %s: %s", c.Token, body)
		}
		view := list.Claims[i]
		if view.LastStartFailure != stored.LastStartFailure {
			t.Errorf("the listing answers the last start failure %q, want %q, the stored one", view.LastStartFailure, stored.LastStartFailure)
		}
		if view.Failure == nil || view.Failure.Detail != stored.Failure {
			t.Errorf("the listing answers the failure %+v, want the stored sentence %q", view.Failure, stored.Failure)
		}
	})

	t.Run("state-answer", func(t *testing.T) {
		answered := h.request(http.MethodGet, "/legion/v1/state", nil, nil)
		body := answered.Body.String()
		oauth2calltest.RequireNone(t, "kubeerror.api/state-answer", body)
		if answered.Code != http.StatusOK {
			t.Fatalf("read the state = %d, want 200; body %s", answered.Code, body)
		}
		var state State
		if err := json.Unmarshal(answered.Body.Bytes(), &state); err != nil {
			t.Fatalf("the state %q does not decode: %v", body, err)
		}
		architect := state.Issues[c.Issue].Architect
		if architect == nil {
			t.Fatalf("the state carries no architect of %s: %s", c.Issue, body)
		}
		if architect.LastStartFailure != stored.LastStartFailure {
			t.Errorf("the state answers the last start failure %q, want %q, the stored one", architect.LastStartFailure, stored.LastStartFailure)
		}
		if architect.Failure == nil || architect.Failure.Detail != stored.Failure {
			t.Errorf("the state answers the failure %+v, want the stored sentence %q", architect.Failure, stored.Failure)
		}
	})
}
