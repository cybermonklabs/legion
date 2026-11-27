package supervise_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/rest"

	"github.com/sjawhar/envoy/oauth2call/oauth2calltest"
	"github.com/sjawhar/legion/daemon/internal/appauth"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/ghconfig"
	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/githubtest"
	"github.com/sjawhar/legion/daemon/internal/ompsessions"
	"github.com/sjawhar/legion/daemon/internal/redact"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/runtime/fake"
	"github.com/sjawhar/legion/daemon/internal/runtime/sandbox"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/testlog"
)

// refusedRepository is the repository the refused launch provisions, on the owner the mint is
// asked for.
var refusedRepository = ghrepo.MustParse("acme/widgets")

// sinkClaim is the harness's queued claim (supervise.QueuedClaim) under the token its project,
// issue and role derive (claim.NewToken): the token a launch's spec carries, which the Sandbox
// runtime takes from no other (runtime.ValidateSpawnSpec), so the claim the machine supervises and
// the one the runtime refused are one.
func sinkClaim(t *testing.T) supervise.Claim {
	t.Helper()
	c := supervise.QueuedClaim()
	token, err := claim.NewToken(c.Project, c.Issue, c.Role)
	if err != nil {
		t.Fatal(err)
	}
	c.Token = token
	return c
}

// refusedLaunch is the error the Sandbox runtime's Spawn returns for c's first launch when the
// mint of the launch's provisioning token is refused: the runtime itself (sandbox.New, Spawn),
// over a Kubernetes API that holds nothing yet (kubeAPI), with the daemon's provisioning token
// source over a real token manager (implementTokens, appauth.New) whose implement App is in token
// mode. Its token_command releases oauth2calltest.Token as the owner's installation token, and GitHub's
// stand-in refuses the identity read every release is checked by (appauth's viewerLogin, POST
// /graphql under that token) with a rate limit: 403 with X-RateLimit-Remaining 0, the
// Authorization echoed verbatim in the body and in every other rate-limit header, each of which
// GitHub would write as a number or a date. The echo sits in the kept field of the answer's words
// (RemoteText.carried: githubrest's limitHeaders reads each header through oauth2call.Header into
// Answer.Limit, and the body through Words into Answer.Body), carried on appauth.AnswerError, and
// the error is that refusal inside the two wraps of the runtime's own: provision's of the mint
// (runtime/sandbox's podkind.go) and the launch's (relaunch.go).
//
// It fails t unless the stand-in was asked, every Authorization it was sent held oauth2calltest.Token,
// the API was asked nothing the launch's path to the mint does not ask, and the error is that
// refusal as the runtime wraps it, with the echo in each of the answer's kept fields as the rule
// prints them, redacted: a row whose stand-in echoed nothing would hold none of the credential
// however its printers read, so this is what says each row's check could see one.
func refusedLaunch(t *testing.T, c supervise.Claim) error {
	t.Helper()
	var mu sync.Mutex
	var sent []string
	github := githubtest.Serve(t, githubtest.Spec{Handle: func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/graphql" {
			githubtest.Unexpected(t, w, r)
			return
		}
		authorization := r.Header.Get("Authorization")
		mu.Lock()
		sent = append(sent, authorization)
		mu.Unlock()
		for _, name := range []string{"Retry-After", "X-RateLimit-Limit", "X-RateLimit-Reset", "X-RateLimit-Resource"} {
			w.Header().Set(name, authorization)
		}
		w.Header().Set("X-RateLimit-Remaining", "0")
		w.WriteHeader(http.StatusForbidden)
		_, _ = fmt.Fprintf(w, `{"message":"API rate limit exceeded for %s"}`, authorization)
	}})
	release := filepath.Join(t.TempDir(), "release.json")
	if err := os.WriteFile(release, fmt.Appendf(nil, `{"installations":{%q:{"installation_id":7,"token":%q,"expires_at":%q}}}`,
		refusedRepository.Owner(), oauth2calltest.Token, time.Now().Add(time.Hour).UTC().Format(time.RFC3339)), 0o600); err != nil {
		t.Fatal(err)
	}
	apps := appauth.New(config.GitHubApps{Implement: config.GitHubApp{AppID: "12345", TokenCommand: "cat '" + release + "'"}},
		appauth.Options{BaseURL: github.URL, HTTPClient: github.Client(), Stderr: io.Discard, Log: slog.New(slog.DiscardHandler)})
	t.Cleanup(apps.Close)

	prompt := filepath.Join(t.TempDir(), "role.md")
	if err := os.WriteFile(prompt, []byte("You are the "+string(c.Role)+".\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	api, rc := newKubeAPI(t)
	ctx, stop := context.WithCancel(context.Background())
	defer stop()
	rt, err := sandbox.New(ctx, rc, sandboxOptions(implementTokens{apps: apps}))
	if err != nil {
		t.Fatalf("build the Sandbox runtime: %v", err)
	}
	_, refused := rt.Spawn(ctx, runtime.SpawnSpec{
		Claim: c.Token, Project: c.Project, Tree: c.Tree, Issue: c.Issue, Role: c.Role, Generation: 1,
		BootToken: "boot-" + string(c.Token) + "-g1", Prompt: runtime.PromptParts{RolePromptPaths: []string{prompt}},
		Repository: refusedRepository, DefaultBranch: "main",
	})
	api.requireServed(t)
	if refused == nil {
		t.Fatal("the Sandbox runtime launched the claim, want the mint's refusal")
	}

	mu.Lock()
	echoes := slices.Clone(sent)
	mu.Unlock()
	if len(echoes) == 0 {
		t.Fatalf("the launch asked GitHub's stand-in nothing (%v), so its refusal keeps no answer", refused)
	}
	for _, echo := range echoes {
		if !strings.Contains(echo, oauth2calltest.Token) {
			t.Errorf("the stand-in was sent %q, want the planted credential in it", echo)
		}
	}
	if wraps := "launch " + string(c.Token) + ": "; !strings.HasPrefix(refused.Error(), wraps) ||
		!strings.Contains(refused.Error(), "mint the provisioning token for "+refusedRepository.String()+": ") {
		t.Fatalf("the launch returned %q, want the refused mint inside provision's wrap and the launch's", refused)
	}
	var answered *appauth.AnswerError
	if !errors.As(refused, &answered) || answered.Answer == nil {
		t.Fatalf("the launch returned %q, which carries no GitHub answer of appauth's", refused)
	}
	answer := answered.Answer
	if answer.Status != http.StatusForbidden || !answer.RateLimited {
		t.Errorf("the answer is %d, rate limited %t; want the stand-in's 403 rate limit", answer.Status, answer.RateLimited)
	}
	for _, kept := range []struct{ field, text string }{
		{"Body", answer.Body}, {"Limit.Resource", answer.Limit.Resource}, {"Limit.RetryAfter", answer.Limit.RetryAfter},
		{"Limit.Limit", answer.Limit.Limit}, {"Limit.Reset", answer.Limit.Reset},
	} {
		if !strings.Contains(kept.text, "[redacted]") {
			t.Errorf("the answer's %s is %q, want the echoed Authorization in it, redacted", kept.field, kept.text)
		}
	}
	return refused
}

// sandboxOptions are the Sandbox runtime's options as a deployment's are shaped, with tokens as its
// provisioning token source. What the launch's path to the mint reads nothing of is the plainest
// value configure takes: a store that answers nothing (absentStore), gh credentials that are never
// asked for, and no repository cache yet.
func sandboxOptions(tokens sandbox.ProvisionTokens) sandbox.Options {
	reservation := corev1.ResourceRequirements{
		Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("1"), corev1.ResourceMemory: resource.MustParse("1Gi"),
			corev1.ResourceEphemeralStorage: resource.MustParse("1Gi")},
		Limits: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("1"), corev1.ResourceMemory: resource.MustParse("1Gi"),
			corev1.ResourceEphemeralStorage: resource.MustParse("2Gi")},
	}
	unasked := errors.New("no gh credential is asked for before the mint")
	return sandbox.Options{
		Namespace: kubeNamespace, Project: "legion", Store: absentStore{},
		Image:        "ghcr.io/example/worker@sha256:" + strings.Repeat("a", 64),
		StorageClass: "standard", IssueVolume: resource.MustParse("1Gi"),
		Resources:    sandbox.PodResources{Issue: reservation, Controller: reservation},
		StreamURL:    "tcp://127.0.0.1:13371",
		SessionStore: ompsessions.Store{Kind: ompsessions.StorePostgres, DSNSecret: "SESSION_DSN"},
		Tools: sandbox.Tools{GH: "/usr/bin/gh", Git: "/usr/bin/git", JJ: "/usr/bin/jj", Legion: "/opt/legion/bin/legion",
			AgentSecrets: "/opt/legion/bin/agent-secrets"},
		BootTimeout: 2 * time.Second, RegistrationIntervals: 3, BusyBackoff: time.Millisecond, TerminationGrace: time.Second,
		ProbeInterval: time.Hour, AdoptTimeout: time.Second,
		Tokens: tokens,
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

// implementTokens is the daemon's provisioning token source over apps, as internal/daemon's
// implementTokens is: the implement App's read lease for the owner of the repository asked for,
// its error returned as it came.
type implementTokens struct{ apps *appauth.Manager }

func (s implementTokens) Token(ctx context.Context, repo ghrepo.Repository) (string, error) {
	lease, err := s.apps.ReadToken(ctx, appauth.Implement, repo.Owner())
	if err != nil {
		return "", err
	}
	return lease.Token, nil
}

// absentStore is the Sandbox runtime's store holding nothing: no tree, no claim, no Sandbox's home.
type absentStore struct{}

func (absentStore) TreeLive(context.Context, string, string) (bool, error) { return false, nil }

func (absentStore) IssueRunsElsewhere(context.Context, string, string, string) (string, error) {
	return "", nil
}

func (absentStore) IssueSandboxHome(context.Context, string, string) (string, error) { return "", nil }

func (absentStore) ClaimUnretired(context.Context, claim.Token) (bool, error) { return false, nil }

func (absentStore) Claims(context.Context) ([]supervise.Claim, error) { return nil, nil }

// kubeNamespace is the namespace the Sandbox runtime works in.
const kubeNamespace = "legion"

// kubeAPI is the Kubernetes API as the Sandbox runtime meets it from its start to the mint of an
// issue's first launch, and nothing more. The runtime's informers list and watch the namespace's
// Sandboxes, pods and Services, of which it holds none: a list answers none, and a watch that
// asks for its initial events (client-go's watch-list) gets the bookmark that ends them
// (k8s.io/initial-events-end) and nothing after. The launch reads the claim's Sandbox, which is
// absent, creates it, which the API keeps as an API server does, with a uid and a
// resourceVersion, and reads the Sandbox's pod, which is absent. Any other request is one that
// path does not make: the API refuses it and its test fails on it (requireServed).
type kubeAPI struct {
	mu        sync.Mutex
	sandboxes map[string]map[string]any
	unserved  []string
	// done ends every watch still open, before the server waits out its requests.
	done chan struct{}
}

// kubeKind is the apiVersion and kind of one resource the API serves.
type kubeKind struct{ apiVersion, kind string }

var kubeKinds = map[string]kubeKind{
	"sandboxes": {"agents.x-k8s.io/v1beta1", "Sandbox"},
	"pods":      {"v1", "Pod"},
	"services":  {"v1", "Service"},
}

// newKubeAPI serves the API until t ends, and answers it and how a runtime reaches it: in JSON both
// ways, since client-go's typed clients prefer protobuf for the built-in group unless the
// configuration names a content type.
func newKubeAPI(t *testing.T) (*kubeAPI, *rest.Config) {
	t.Helper()
	a := &kubeAPI{sandboxes: map[string]map[string]any{}, done: make(chan struct{})}
	server := httptest.NewServer(a)
	t.Cleanup(server.Close)
	t.Cleanup(func() { close(a.done) })
	return a, &rest.Config{Host: server.URL, ContentConfig: rest.ContentConfig{
		ContentType: "application/json", AcceptContentTypes: "application/json",
	}}
}

func (a *kubeAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	plural, name, ok := kubeObject(r.URL.Path)
	kind, served := kubeKinds[plural]
	switch {
	case !ok || !served:
		a.refuse(w, r)
	case r.URL.Query().Get("watch") == "true":
		a.watch(w, r, kind)
	case r.Method == http.MethodGet && name == "":
		kubeAnswer(w, http.StatusOK, map[string]any{"apiVersion": kind.apiVersion, "kind": kind.kind + "List",
			"metadata": map[string]any{"resourceVersion": "1"}, "items": []any{}})
	case r.Method == http.MethodGet:
		a.mu.Lock()
		object, held := a.sandboxes[name]
		a.mu.Unlock()
		if plural != "sandboxes" || !held {
			kubeStatus(w, http.StatusNotFound, metav1.StatusReasonNotFound)
			return
		}
		kubeAnswer(w, http.StatusOK, object)
	case r.Method == http.MethodPost && plural == "sandboxes" && name == "":
		a.create(w, r, kind)
	default:
		a.refuse(w, r)
	}
}

// create keeps the Sandbox the request carries as an API server does: with a uid, a
// resourceVersion, its first generation and its creation time.
func (a *kubeAPI) create(w http.ResponseWriter, r *http.Request, kind kubeKind) {
	var object map[string]any
	err := json.NewDecoder(r.Body).Decode(&object)
	meta, _ := object["metadata"].(map[string]any)
	name, _ := meta["name"].(string)
	if err != nil || name == "" {
		a.refuse(w, r)
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if _, taken := a.sandboxes[name]; taken {
		kubeStatus(w, http.StatusConflict, metav1.StatusReasonAlreadyExists)
		return
	}
	meta["uid"], meta["resourceVersion"], meta["generation"] = "uid-sandbox-"+name, "2", float64(1)
	meta["creationTimestamp"] = time.Now().UTC().Format(time.RFC3339)
	object["apiVersion"], object["kind"] = kind.apiVersion, kind.kind
	a.sandboxes[name] = object
	kubeAnswer(w, http.StatusCreated, object)
}

// watch answers a watch with no event of an object: the bookmark that ends a watch-list's initial
// events where the request asks for them, and then nothing until the request or the API ends.
func (a *kubeAPI) watch(w http.ResponseWriter, r *http.Request, kind kubeKind) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	if r.URL.Query().Get("sendInitialEvents") == "true" {
		_ = json.NewEncoder(w).Encode(map[string]any{"type": "BOOKMARK", "object": map[string]any{
			"apiVersion": kind.apiVersion, "kind": kind.kind,
			"metadata": map[string]any{"resourceVersion": "1", "annotations": map[string]any{"k8s.io/initial-events-end": "true"}},
		}})
	}
	if flusher, ok := w.(http.Flusher); ok {
		flusher.Flush()
	}
	select {
	case <-r.Context().Done():
	case <-a.done:
	}
}

// refuse answers a request the launch's path does not make, and keeps it for requireServed.
func (a *kubeAPI) refuse(w http.ResponseWriter, r *http.Request) {
	a.mu.Lock()
	a.unserved = append(a.unserved, r.Method+" "+r.URL.RequestURI())
	a.mu.Unlock()
	kubeStatus(w, http.StatusForbidden, metav1.StatusReasonForbidden)
}

// requireServed fails t naming every request the API refused.
func (a *kubeAPI) requireServed(t *testing.T) {
	t.Helper()
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(a.unserved) > 0 {
		t.Errorf("the Sandbox runtime asked the Kubernetes API for what its launch's path to the mint does not ask: %s",
			strings.Join(a.unserved, "; "))
	}
}

// kubeObject is the plural resource and object name of a path in the runtime's namespace, for the
// two API roots the runtime uses: /api/v1/namespaces/<ns>/<resource>[/<name>] and
// /apis/<group>/<version>/namespaces/<ns>/<resource>[/<name>].
func kubeObject(path string) (plural, name string, ok bool) {
	parts := strings.Split(strings.Trim(path, "/"), "/")
	i := slices.Index(parts, "namespaces")
	if i < 0 || i+1 >= len(parts) || parts[i+1] != kubeNamespace {
		return "", "", false
	}
	switch tail := parts[i+2:]; len(tail) {
	case 1:
		return tail[0], "", true
	case 2:
		return tail[0], tail[1], true
	}
	return "", "", false
}

func kubeAnswer(w http.ResponseWriter, code int, object any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(object)
}

func kubeStatus(w http.ResponseWriter, code int, reason metav1.StatusReason) {
	kubeAnswer(w, code, metav1.Status{TypeMeta: metav1.TypeMeta{Kind: "Status", APIVersion: "v1"},
		Status: metav1.StatusFailure, Reason: reason, Code: int32(code)})
}

// sinkHarness is the harness over c (supervise.NewHarness) with set applied to its Deps, its
// machine logging through the logger the daemon builds in production (cmd/legion's start: a JSON
// handler under redact.HandlerOptions) into the buffer it returns.
func sinkHarness(t *testing.T, c supervise.Claim, set ...func(*supervise.Deps)) (*supervise.Harness, *testlog.Buffer) {
	t.Helper()
	logged := &testlog.Buffer{}
	log := slog.New(slog.NewJSONHandler(logged, redact.HandlerOptions(nil)))
	logging := func(d *supervise.Deps) { d.Log = log }
	return supervise.NewHarness(t, c, append([]func(*supervise.Deps){logging}, set...)...), logged
}

// launchRefusedByTheMint is a spawn of the queued claim c that the runtime refuses with refused,
// under a launch budget of one, as TestSpawnsTheRuntimeKeepsRefusingEndInFailed drives a refused
// spawn: launchNow charges it, and failLaunch fails the claim with the sentence it builds of the
// refusal ("launch failures ran out: …"), which fail keeps as a far side's error text
// (Claim.Failure).
func launchRefusedByTheMint(t *testing.T, c supervise.Claim, refused error) (*supervise.Harness, *testlog.Buffer) {
	t.Helper()
	h, log := sinkHarness(t, c, func(d *supervise.Deps) { d.Limits.LaunchFailures = 1 })
	h.Runtime().ScriptSpawn(fake.SpawnResult{Err: refused})
	since := h.Now()
	if err := h.Handle(supervise.RequestSpawn{Claim: c.Token}); !errors.Is(err, refused) {
		t.Fatalf("the refused spawn returned %v, want the runtime's refusal", err)
	}
	h.WantState(supervise.StateFailed)
	h.WantFailure(supervise.CauseLaunchRefused, "launch failures ran out", since)
	return h, log
}

// failedRecordingTheRefusal is the claim c failing, under the sentence refused failed a launch of
// it with (launchRefusedByTheMint), while it holds a process that ran a turn and reports
// background work, as claimInNoTurn leaves one: fail suspends that process and lets it go, and the
// let-go records what it ended under the failure's cause (endFailed, recordEnd). That record,
// untold, is what the next process of the claim is told.
//
// No decision of the machine reaches this state, which is the limit of the five rows that start
// here: a launch the runtime refuses fails a claim that holds no process. launchNow lets the old
// process go before its start (letGo), letGo moves the claim's locator off it, and recordEnd
// records nothing for a claim with no locator (endedwork.go). The only fail of a sentence that
// carries the runtime's error is failLaunch's, so recordEnd records nothing then; every other fail
// (budgets.go, machine.go, providererror.go, turnoutcome.go) carries no runtime error. CodeQL's
// flows into the sinks that read the record run from that failure's sentence through fail into it
// all the same, so this calls fail itself (Harness.Fail), under the machine's lock as a decision
// holds it, and every sink that reads the record is then driven by the events its own tests use.
func failedRecordingTheRefusal(t *testing.T, c supervise.Claim, refused error) (*supervise.Harness, *testlog.Buffer) {
	t.Helper()
	launched, _ := launchRefusedByTheMint(t, c, refused)
	failure := launched.Machine().Claim().Failure
	h, log := sinkHarness(t, c)
	h.Reach(supervise.StateIdle)
	h.Advance(time.Second)
	h.ReportActivity("async-job")
	if err := h.Fail(supervise.CauseLaunchRefused, failure); err != nil {
		t.Fatalf("fail the claim under the refused launch's sentence: %v", err)
	}
	h.WantState(supervise.StateFailed)
	if ended := h.Machine().Claim().EndedWork; ended.Cause != supervise.EndFailedCause(failure) || !ended.Tells() {
		t.Fatalf("the failure recorded %+v, want an untold record under %q", ended, supervise.EndFailedCause(failure))
	}
	return h, log
}

// retriedToTheRecord is the claim failedRecordingTheRefusal leaves, retried and brought up to
// ready, as abandonSession's retry and relaunched bring one up: it holds no task, so its ready
// queues the daemon's own delivery of the record and sends it (queueRelaunch, sendPending). It
// returns the record's cause, which the machine derived from the refusal.
func retriedToTheRecord(t *testing.T, c supervise.Claim, refused error) (*supervise.Harness, *testlog.Buffer, string) {
	t.Helper()
	h, log := failedRecordingTheRefusal(t, c, refused)
	cause := h.Machine().Claim().EndedWork.Cause
	h.Must(supervise.RequestRetry{Claim: c.Token})
	h.Relaunched()
	if p := h.Machine().Claim().Pending; p == nil || !p.Relaunch() {
		t.Fatalf("the retried claim holds %+v at its ready, want the daemon's own delivery of the record", p)
	}
	return h, log, cause
}

// sinkLines is every line log holds whose message is message, as the handler wrote it, and the
// value of key on each.
func sinkLines(t *testing.T, log *testlog.Buffer, message, key string) (lines, values []string) {
	t.Helper()
	for _, line := range strings.Split(log.String(), "\n") {
		if line == "" {
			continue
		}
		var record map[string]any
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			t.Fatalf("the handler wrote %q, which is no JSON line: %v", line, err)
		}
		if record[slog.MessageKey] != message {
			continue
		}
		value, _ := record[key].(string)
		lines, values = append(lines, line), append(values, value)
	}
	return lines, values
}

// loggedText is the text of a row's logged value, an error or a string, as a handler reads it.
func loggedText(logged any) string {
	if err, ok := logged.(error); ok {
		return err.Error()
	}
	return logged.(string)
}

// TestTheSinksOfAMintRefusalLogNoCredential: CodeQL's go/clear-text-logging reads a credential into
// each of this package's nine log calls below from the error the Sandbox runtime's launch returns
// when the mint of its provisioning token is refused (runtime/sandbox's issuePod.provision, inside
// its relaunch's wrap). That error carries GitHub's answer (appauth.AnswerError's
// githubrest.Answer), and the answer's Body and rate-limit headers (Limit) are read through
// oauth2call's RemoteText, whose kept field holds the credentials the request carried so that its
// printers subtract them (WithoutTokens, then String). Each row has the Sandbox runtime itself
// refuse a launch of the claim, its mint refused by a stand-in in GitHub's place that echoes the
// planted credential the request carried in its body and its rate-limit headers (refusedLaunch);
// has this package's fake runtime answer the machine's launch with that error, which is how the
// runtime's answer enters this package (Deps.Runtime); and drives the sink by the events this
// package's own tests drive it by, the machine logging through the daemon's production logger. The
// row's line is there, logs the row's value (logged: the error the sink logs, or the failure
// sentence or the let-go's cause the machine derived from it, as text), and neither the line nor
// that value holds a made-up credential (oauth2calltest.RequireNone); the row prints logged, the
// runtime's refusal it carries, and the GitHub answer that refusal keeps on appauth.AnswerError,
// under every fmt verb and a bad verb, at every holding, beside the echo the stand-in wrote as a
// plain string (oauth2calltest.KeptPrintTest). Captures sink/supervise/<file>/<function>, with the
// line's first words after it where one function holds two of the sinks, and
// kept/supervise/<file>/<function>/{logged,refusal,answer}/<holding>/<verb>.
//
// Two rows drive their sink with a runtime's answer no runtime gives, and their comments name
// that limit: launchNow's branch for a hold the store did not answer is taken for an error that
// is errHoldUnanswered, and promptRefused for one that is a runtime.PromptTooLarge. Each fake
// answer joins that error to the runtime's refusal, which is the flow CodeQL reads: the runtime's
// error, whole, into the line. The five rows of the record of what a let-go ended start from
// failedRecordingTheRefusal, whose comment names theirs.
func TestTheSinksOfAMintRefusalLogNoCredential(t *testing.T) {
	for _, row := range []struct {
		name, capture, message, key string
		// drive brings the sink's line about for the claim c with refused, the runtime's refusal
		// of its launch, and returns the log and logged, the value the line logs under key.
		drive func(t *testing.T, c supervise.Claim, refused error) (log *testlog.Buffer, logged any)
	}{
		{
			name:    "delivery.go:sendPending the daemon's own delivery of a relaunch's record has nothing to say",
			capture: "sink/supervise/delivery.go/sendPending",
			message: "supervise: the daemon's own delivery of a relaunch's record has nothing to say, since that record no longer tells; it is dropped rather than sent",
			key:     "cause",
			// As TestARelaunchSentenceIsNotToldToAFreshSession's claim holding the daemon's own
			// delivery of the record: the process dies before a turn takes it, its session is found
			// in no store, the launch forgets the session and with it the record (startFresh,
			// forgetSession), and the fresh agent's ready drops the delivery.
			drive: func(t *testing.T, c supervise.Claim, refused error) (*testlog.Buffer, any) {
				h, log, cause := retriedToTheRecord(t, c, refused)
				h.Runtime().ScriptResume(fake.SpawnResult{Err: runtime.ErrSessionFileMissing})
				h.Observe(runtime.Gone)
				h.WantState(supervise.StateLaunching)
				h.RegisterFresh()
				if got := h.Machine().Claim(); got.Pending != nil || got.EndedWork.Cause != cause || !got.EndedWork.Told {
					t.Fatalf("the fresh agent's ready left %+v pending under the record %+v, want the delivery dropped and the record told under %q",
						got.Pending, got.EndedWork, cause)
				}
				return log, cause
			},
		},
		{
			name:    "delivery.go:confirmCarrying a turn took the prompt that told the agent what its relaunch ended",
			capture: "sink/supervise/delivery.go/confirmCarrying",
			message: "supervise: a turn took the prompt that told the agent what its relaunch ended",
			key:     "cause",
			// As TestARelaunchSentenceIsTheWholeOfTheRelaunchDelivery: the turn that takes the
			// daemon's own delivery of the record tells it.
			drive: func(t *testing.T, c supervise.Claim, refused error) (*testlog.Buffer, any) {
				h, log, cause := retriedToTheRecord(t, c, refused)
				h.Must(supervise.StreamTurnStart{Claim: c.Token})
				if ended := h.Machine().Claim().EndedWork; !ended.Told || ended.Cause != cause {
					t.Fatalf("the turn left the record %+v, want it told under %q", ended, cause)
				}
				return log, cause
			},
		},
		{
			name:    "endedwork.go:recordEnd the relaunch ended work the agent had running",
			capture: "sink/supervise/endedwork.go/recordEnd",
			message: "supervise: the relaunch ended work the agent had running",
			key:     "cause",
			drive: func(t *testing.T, c supervise.Claim, refused error) (*testlog.Buffer, any) {
				h, log := failedRecordingTheRefusal(t, c, refused)
				return log, h.Machine().Claim().EndedWork.Cause
			},
		},
		{
			name:    "machine.go:launchNow launch cut short",
			capture: "sink/supervise/machine.go/launchNow/launch-cut-short",
			message: "supervise: launch cut short; its context ended",
			key:     "error",
			// As TestASpawnCutShortByItsContextIsChargedNothing: the launch's context ends while the
			// runtime holds the spawn, which then answers its refusal. The line logs the runtime's
			// answer as start returned it.
			drive: func(t *testing.T, c supervise.Claim, refused error) (*testlog.Buffer, any) {
				h, log := sinkHarness(t, c)
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				h.Runtime().OnSpawn = func(context.Context) error {
					cancel()
					return refused
				}
				if err := h.Machine().Handle(ctx, supervise.RequestSpawn{Claim: c.Token}); !errors.Is(err, refused) {
					t.Fatalf("the cut-short spawn returned %v, want the runtime's answer", err)
				}
				h.Machine().Wait()
				if got := h.Machine().Claim(); got.State != supervise.StateLaunching || got.Budgets != (supervise.Budgets{}) || got.Locator != nil {
					t.Fatalf("the cut-short spawn left %+v, want the claim launching, charged nothing, with no process", got)
				}
				return log, refused
			},
		},
		{
			name:    "machine.go:launchNow the store did not answer the session's hold",
			capture: "sink/supervise/machine.go/launchNow/the-store-did-not-answer",
			message: "supervise: the store did not answer the session's hold; trying the launch again at the probe interval, charging nothing",
			key:     "error",
			// As TestAnUnansweredHoldAtAResumeChargesNothing: the resume of a suspended claim whose
			// start answers errHoldUnanswered, here from the runtime's Resume and joined to its
			// refusal. That is the limit of this row: no runtime answers errHoldUnanswered. Only
			// this package's hold makes it, of the store's answer (sessionhold.go), and start holds
			// the session's names before it asks the runtime anything (machine.go's start, before
			// each Resume), so the branch's error never carries the runtime's. The line logs that
			// answer joined to stopPrevious's, which is nil: the suspension let the process go.
			drive: func(t *testing.T, c supervise.Claim, refused error) (*testlog.Buffer, any) {
				h, log := sinkHarness(t, c)
				h.Reach(supervise.StateReady)
				h.Must(supervise.RequestSuspend{Claim: c.Token, By: supervise.SuspendedByOperator})
				unanswered := errors.Join(supervise.ErrHoldUnanswered, refused)
				h.Runtime().ScriptResume(fake.SpawnResult{Err: unanswered})
				if err := h.Handle(supervise.RequestResume{Claim: c.Token}); !errors.Is(err, supervise.ErrHoldUnanswered) || !errors.Is(err, refused) {
					t.Fatalf("the resume returned %v, want the unanswered hold and the runtime's refusal", err)
				}
				if got := h.Machine().Claim(); got.State != supervise.StateLaunching || got.Budgets.LaunchFailures != 0 {
					t.Fatalf("the claim is %s with %d launch failures charged, want it launching with none", got.State, got.Budgets.LaunchFailures)
				}
				return log, errors.Join(unanswered, nil)
			},
		},
		{
			name:    "machine.go:promptRefused the role's system prompt is too large to launch",
			capture: "sink/supervise/machine.go/promptRefused",
			message: "supervise: the role's system prompt is too large to launch; failing the claim, charging no launch failure",
			key:     "error",
			// As TestAPromptPastTheStartFrameFailsTheClaimChargingNoLaunchFailure: a spawn the
			// runtime refuses with a runtime.PromptTooLarge, here joined to the runtime's refusal of
			// the mint. That is the limit of this row: the Sandbox runtime makes a PromptTooLarge
			// only when it measures the start frame (launcher.go's checkStartFrame), in prepare
			// (manifest.go), which relaunch runs before anything else, and again after the pod is
			// provisioned (relaunch.go), past the mint, whose refusal ends the launch first; so no
			// answer carries both. The line logs that answer joined to stopPrevious's, which is nil:
			// a queued claim's spawn lets no process go.
			drive: func(t *testing.T, c supervise.Claim, refused error) (*testlog.Buffer, any) {
				h, log := sinkHarness(t, c)
				tooLarge := errors.Join(&runtime.PromptTooLarge{Role: c.Role, Bytes: 1_100_000, Frame: 1_100_400}, refused)
				h.Runtime().ScriptSpawn(fake.SpawnResult{Err: tooLarge})
				var measured *runtime.PromptTooLarge
				if err := h.Handle(supervise.RequestSpawn{Claim: c.Token}); !errors.As(err, &measured) || !errors.Is(err, refused) {
					t.Fatalf("the spawn returned %v, want the runtime's PromptTooLarge and its refusal", err)
				}
				h.WantState(supervise.StateFailed)
				if got := h.Machine().Claim().Budgets; got != (supervise.Budgets{}) {
					t.Fatalf("budgets = %+v, want none charged", got)
				}
				return log, errors.Join(tooLarge, nil)
			},
		},
		{
			name:    "machine.go:forgetSession the claim abandons its session",
			capture: "sink/supervise/machine.go/forgetSession",
			message: "supervise: the claim abandons its session, so the record of what the old session's process ended is not told to the fresh agent; the abandoned session's sentence says that work is gone",
			key:     "cause",
			// As TestARelaunchSentenceGivesWayToTheAbandonedSession: the operator's forget of the
			// session of a claim that runs nothing ends the record, told by no delivery.
			drive: func(t *testing.T, c supervise.Claim, refused error) (*testlog.Buffer, any) {
				h, log := failedRecordingTheRefusal(t, c, refused)
				held := h.Machine().Claim()
				if held.Session == "" {
					t.Fatalf("the failed claim holds no session to forget: %+v", held)
				}
				h.Must(supervise.RequestForgetSession{Claim: c.Token, Session: held.Session})
				if ended := h.Machine().Claim().EndedWork; !ended.Told || ended.ToldBy != "" || ended.Cause != held.EndedWork.Cause {
					t.Fatalf("the forget left the record %+v, want it told by no delivery under %q", ended, held.EndedWork.Cause)
				}
				return log, held.EndedWork.Cause
			},
		},
		{
			name:    "machine.go:fail claim failed",
			capture: "sink/supervise/machine.go/fail",
			message: "supervise: claim failed",
			key:     "why",
			drive: func(t *testing.T, c supervise.Claim, refused error) (*testlog.Buffer, any) {
				h, log := launchRefusedByTheMint(t, c, refused)
				return log, h.Machine().Claim().Failure
			},
		},
		{
			name:    "relaunch.go:queueRelaunch the claim holds no task",
			capture: "sink/supervise/relaunch.go/queueRelaunch",
			message: "supervise: the claim holds no task; a delivery of the daemon's own tells its agent what the relaunch ended",
			key:     "cause",
			drive: func(t *testing.T, c supervise.Claim, refused error) (*testlog.Buffer, any) {
				_, log, cause := retriedToTheRecord(t, c, refused)
				return log, cause
			},
		},
	} {
		t.Run(row.name, func(t *testing.T) {
			c := sinkClaim(t)
			refused := refusedLaunch(t, c)
			log, logged := row.drive(t, c, refused)
			text := loggedText(logged)
			// A let-go's cause quotes the failure sentence that carries the refusal (endFailed), so
			// the refusal stands in it escaped.
			quoted := strings.Trim(strconv.Quote(refused.Error()), `"`)
			if !strings.Contains(text, refused.Error()) && !strings.Contains(text, quoted) {
				t.Fatalf("the row's value %q does not carry the runtime's refusal %q", text, refused)
			}
			lines, values := sinkLines(t, log, row.message, row.key)
			if len(lines) == 0 {
				t.Fatalf("the machine logged no line %q; it logged:\n%s", row.message, log.String())
			}
			want := redact.URLCredentials(text)
			for _, value := range values {
				if value != want {
					t.Errorf("the line logs %s %q, want the row's value as the handler's rule prints it, %q", row.key, value, want)
				}
			}
			oauth2calltest.RequireNone(t, row.capture, strings.Join(append(lines, text), "\n"))
			name := strings.TrimPrefix(row.capture, "sink/")
			echo := "token " + oauth2calltest.Token
			oauth2calltest.KeptPrintTest(t, name+"/logged", logged, echo)
			oauth2calltest.KeptPrintTest(t, name+"/refusal", refused, echo)
			// The refusal prints none of GitHub's answer (an AnswerError prints its Err alone), so
			// the answer it keeps is printed too: there the request's credential sits.
			var answered *appauth.AnswerError
			if !errors.As(refused, &answered) || answered.Answer == nil {
				t.Fatalf("the refusal %q keeps no GitHub answer of appauth's", refused)
			}
			oauth2calltest.KeptPrintTest(t, name+"/answer", answered.Answer, echo)
		})
	}
}
