package supervise_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	"github.com/sjawhar/envoy/oauth2call/oauth2calltest"
	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/runtime/fake"
	"github.com/sjawhar/legion/daemon/internal/runtime/sandbox"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// kubeRefusalBearer is the bearer the Sandbox runtime's Kubernetes clients carry to the API
// stand-in of this file's rows (refusingKubeAPI), as the in-cluster ServiceAccount token is
// carried to the API server: it holds one of the made-up credentials oauth2calltest.RequireNone
// looks for (oauth2calltest.Token), so a field or a line that holds it fails its row.
const kubeRefusalBearer = "kube-bearer-" + oauth2calltest.Token

// The uids the API server gave the claim's Sandbox and its pod in the death row's API.
const (
	kubeRefusalSandboxUID = "uid-sandbox-refusal"
	kubeRefusalPodUID     = "uid-pod-refusal"
)

// sandboxResource is the Sandbox resource the runtime reads, for the control's own client.
var sandboxResource = schema.GroupVersionResource{Group: "agents.x-k8s.io", Version: "v1beta1", Resource: "sandboxes"}

// refusingKubeAPI is a Kubernetes API in the API server's place that refuses the one request of
// the runtime's a row is about (refused) with a 403 Status that echoes the Authorization the
// request carried in every field of it a server writes words into (echoedStatus). It answers the
// runtime's informers as an API server holding the claim's Sandbox and pod answers them, when the
// row's API holds them: a list answers them, and a watch that asks for its initial events
// (client-go's watch-list) gets each as an ADDED event and then the bookmark that ends them
// (k8s.io/initial-events-end), and nothing after. A read of the held Sandbox by its name answers
// it. Any other request is one the row's path does not make: it is refused with no echo and kept
// for requireServed. Every Authorization it was sent is kept, for requireBearer.
type refusingKubeAPI struct {
	refused func(r *http.Request) bool
	sandbox map[string]any
	pod     *corev1.Pod

	mu       sync.Mutex
	sent     []string
	echoes   int
	unserved []string
	// done ends every watch still open, before the server waits out its requests.
	done chan struct{}
}

// newRefusingKubeAPI serves the API until t ends, and answers it and how a runtime reaches it:
// with kubeRefusalBearer, in JSON both ways, since client-go's typed clients prefer protobuf for
// the built-in group unless the configuration names a content type.
func newRefusingKubeAPI(t *testing.T, refused func(r *http.Request) bool, held map[string]any, pod *corev1.Pod) (*refusingKubeAPI, *rest.Config) {
	t.Helper()
	a := &refusingKubeAPI{refused: refused, sandbox: held, pod: pod, done: make(chan struct{})}
	server := httptest.NewServer(a)
	t.Cleanup(server.Close)
	t.Cleanup(func() { close(a.done) })
	return a, &rest.Config{Host: server.URL, BearerToken: kubeRefusalBearer, ContentConfig: rest.ContentConfig{
		ContentType: "application/json", AcceptContentTypes: "application/json",
	}}
}

func (a *refusingKubeAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	a.mu.Lock()
	a.sent = append(a.sent, r.Header.Get("Authorization"))
	a.mu.Unlock()
	if a.refused(r) {
		a.mu.Lock()
		a.echoes++
		a.mu.Unlock()
		kubeAnswer(w, http.StatusForbidden, echoedStatus(r))
		return
	}
	plural, name, ok := kubeObject(r.URL.Path)
	kind, served := kubeKinds[plural]
	switch {
	case !ok || !served || r.Method != http.MethodGet:
		a.refuse(w, r)
	case r.URL.Query().Get("watch") == "true":
		a.watch(w, r, kind, a.items(plural))
	case name == "":
		kubeAnswer(w, http.StatusOK, map[string]any{"apiVersion": kind.apiVersion, "kind": kind.kind + "List",
			"metadata": map[string]any{"resourceVersion": "1"}, "items": a.items(plural)})
	case plural == "sandboxes" && a.sandbox != nil && name == heldName(a.sandbox):
		kubeAnswer(w, http.StatusOK, a.sandbox)
	default:
		a.refuse(w, r)
	}
}

// items are the objects of plural the API holds.
func (a *refusingKubeAPI) items(plural string) []any {
	switch {
	case plural == "sandboxes" && a.sandbox != nil:
		return []any{a.sandbox}
	case plural == "pods" && a.pod != nil:
		return []any{a.pod}
	}
	return []any{}
}

// watch answers a watch with items, the bookmark that ends a watch-list's initial events after
// them where the request asks for them, and then nothing until the request or the API ends.
func (a *refusingKubeAPI) watch(w http.ResponseWriter, r *http.Request, kind kubeKind, items []any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	if r.URL.Query().Get("sendInitialEvents") == "true" {
		events := json.NewEncoder(w)
		for _, item := range items {
			_ = events.Encode(map[string]any{"type": "ADDED", "object": item})
		}
		_ = events.Encode(map[string]any{"type": "BOOKMARK", "object": map[string]any{
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
}

// refuse answers a request the row's path does not make, and keeps it for requireServed.
func (a *refusingKubeAPI) refuse(w http.ResponseWriter, r *http.Request) {
	a.mu.Lock()
	a.unserved = append(a.unserved, r.Method+" "+r.URL.RequestURI())
	a.mu.Unlock()
	kubeStatus(w, http.StatusForbidden, metav1.StatusReasonForbidden)
}

// requireServed fails t naming every request the API refused as one the row's path does not make.
func (a *refusingKubeAPI) requireServed(t *testing.T) {
	t.Helper()
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(a.unserved) > 0 {
		t.Errorf("the Sandbox runtime asked the Kubernetes API for what the row's path does not ask: %s", strings.Join(a.unserved, "; "))
	}
}

// requireEchoed fails t unless the API answered the row's request with its echo at least once
// before the control's own client asked it: a runtime that never met the echo would hold none of
// the credential however it printed what it met.
func (a *refusingKubeAPI) requireEchoed(t *testing.T) {
	t.Helper()
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.echoes == 0 {
		t.Fatal("the Sandbox runtime never made the request the API refuses with its echo")
	}
}

// requireBearer fails t unless the API was sent the bearer on every request: the row's control
// that what answered in the API server's place held the credential the row looks for.
func (a *refusingKubeAPI) requireBearer(t *testing.T) {
	t.Helper()
	a.mu.Lock()
	sent := slices.Clone(a.sent)
	a.mu.Unlock()
	if len(sent) == 0 {
		t.Fatal("the API was sent no request")
	}
	for _, s := range sent {
		if s != "Bearer "+kubeRefusalBearer {
			t.Fatalf("the API was sent the Authorization %q, want the bearer %q", s, kubeRefusalBearer)
		}
	}
}

// echoedStatus is a 403 Forbidden Status that echoes the Authorization r carried in every field
// of it a server writes words into: the message, the details' name, group and kind, and a cause's
// type, message and field.
func echoedStatus(r *http.Request) metav1.Status {
	carried := r.Header.Get("Authorization")
	return metav1.Status{TypeMeta: metav1.TypeMeta{Kind: "Status", APIVersion: "v1"}, Status: metav1.StatusFailure,
		Code: http.StatusForbidden, Reason: metav1.StatusReasonForbidden, Message: "the request carried " + carried,
		Details: &metav1.StatusDetails{Name: carried, Group: carried, Kind: carried, Causes: []metav1.StatusCause{{
			Type: metav1.CauseType(carried), Message: carried, Field: carried,
		}}}}
}

// heldName is the name of a held object.
func heldName(object map[string]any) string {
	meta, _ := object["metadata"].(map[string]any)
	name, _ := meta["name"].(string)
	return name
}

// requireRawEcho is the row's control: client-go's own error for the API's answer, read by a
// client built from the runtime's configuration with no runtime in between, holds the echoed
// credential. A row whose answer held none would pass whatever the runtime and this package
// printed.
func requireRawEcho(t *testing.T, err error) {
	t.Helper()
	if err == nil || !strings.Contains(err.Error(), oauth2calltest.Token) {
		t.Fatalf("client-go's own error for the API's answer is %v, want the echoed credential in it", err)
	}
}

// unaskedTokens is a provisioning token source no row's path asks: each refusal comes before the
// launch's mint.
type unaskedTokens struct{}

func (unaskedTokens) Token(context.Context, ghrepo.Repository) (string, error) {
	return "", errors.New("no provisioning token is asked for before the refused request")
}

// sandboxPathOf is the path of the Sandbox named name in the runtime's namespace.
func sandboxPathOf(name string) string {
	return "/apis/" + sandboxResource.Group + "/" + sandboxResource.Version + "/namespaces/" + kubeNamespace + "/sandboxes/" + name
}

// sandboxReadRefusedLaunch is the error the Sandbox runtime's Spawn returns for c's first launch
// when the API server refuses the launch's first request, its read of the claim's Sandbox
// (relaunch's ensureSandbox, through resend), with a 403 echoing the bearer the request carried:
// the runtime itself (sandbox.New, Spawn), over an API that holds nothing, whose informers sync
// as an empty namespace's do. It fails t unless the runtime made that request, the API was asked
// nothing else, client-go's own error for the same answer holds the credential (the control), the
// API was sent the bearer on every request, and the error is the launch's wrap of the Sandbox's
// refused read.
func sandboxReadRefusedLaunch(t *testing.T, c supervise.Claim) error {
	t.Helper()
	name := sandbox.SandboxName(c.Token)
	path := sandboxPathOf(name)
	api, rc := newRefusingKubeAPI(t, func(r *http.Request) bool {
		return r.Method == http.MethodGet && r.URL.Path == path && r.URL.Query().Get("watch") != "true"
	}, nil, nil)
	prompt := filepath.Join(t.TempDir(), "role.md")
	if err := os.WriteFile(prompt, []byte("You are the "+string(c.Role)+".\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, stop := context.WithCancel(context.Background())
	defer stop()
	rt, err := sandbox.New(ctx, rc, sandboxOptions(unaskedTokens{}))
	if err != nil {
		t.Fatalf("build the Sandbox runtime: %v", err)
	}
	_, refused := rt.Spawn(ctx, runtime.SpawnSpec{
		Claim: c.Token, Project: c.Project, Tree: c.Tree, Issue: c.Issue, Role: c.Role, Generation: 1,
		BootToken: "boot-" + string(c.Token) + "-g1", Prompt: runtime.PromptParts{RolePromptPaths: []string{prompt}},
		Repository: refusedRepository, DefaultBranch: "main",
	})
	api.requireEchoed(t)
	api.requireServed(t)
	if refused == nil {
		t.Fatal("the Sandbox runtime launched the claim, want the refusal of its Sandbox's read")
	}

	raw, err := dynamic.NewForConfig(rc)
	if err != nil {
		t.Fatal(err)
	}
	_, rawErr := raw.Resource(sandboxResource).Namespace(kubeNamespace).Get(ctx, name, metav1.GetOptions{})
	requireRawEcho(t, rawErr)
	api.requireBearer(t)
	if wraps := "launch " + string(c.Token) + ": ensure its sandbox: "; !strings.HasPrefix(refused.Error(), wraps) {
		t.Fatalf("the launch returned %q, want the refused read of its Sandbox inside the launch's wrap %q", refused, wraps)
	}
	return refused
}

// refusedLogDeath is the Sandbox runtime's observation of c's process (Runtime.Probe) when the
// claim's pod has ended and the API server refuses the read of its log with a 403 echoing the
// bearer the request carried, beside the locator it observed. The API holds the claim's Sandbox
// and the pod it controls, Failed, its one container (runtime.IssuePodContainer) terminated; the
// runtime's informers sync them into its stores, and the probe judges the process gone (verdict,
// ended), quoting the role's last lines of the container's log (roleLog, readLog, readLogOnce),
// or, for a read the API refused, why they could not be read. A final verdict on a Sandbox that
// names no displaced pod is confirmed by one read of the Sandbox (evaluate, readSandbox), which
// the API answers with the Sandbox it holds. It fails t unless the probe made the log read the
// API refuses, the API was asked nothing else, client-go's own error for the same answer holds
// the credential (the control), the API was sent the bearer on every request, and the
// observation is the locator's process gone.
func refusedLogDeath(t *testing.T, c supervise.Claim) (runtime.Observation, runtime.Locator) {
	t.Helper()
	name := sandbox.SandboxName(c.Token)
	logPath := "/api/v1/namespaces/" + kubeNamespace + "/pods/" + name + "/log"
	controller := true
	held := map[string]any{"apiVersion": sandboxResource.GroupVersion().String(), "kind": "Sandbox", "metadata": map[string]any{
		"name": name, "namespace": kubeNamespace, "uid": kubeRefusalSandboxUID, "resourceVersion": "1", "generation": float64(1),
	}}
	pod := &corev1.Pod{
		TypeMeta: metav1.TypeMeta{Kind: "Pod", APIVersion: "v1"},
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: kubeNamespace, UID: kubeRefusalPodUID, ResourceVersion: "1",
			OwnerReferences: []metav1.OwnerReference{{APIVersion: sandboxResource.GroupVersion().String(), Kind: "Sandbox",
				Name: name, UID: kubeRefusalSandboxUID, Controller: &controller}}},
		Status: corev1.PodStatus{Phase: corev1.PodFailed, ContainerStatuses: []corev1.ContainerStatus{{
			Name: runtime.IssuePodContainer, State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{Reason: "Error", ExitCode: 1}},
		}}},
	}
	api, rc := newRefusingKubeAPI(t, func(r *http.Request) bool {
		return r.Method == http.MethodGet && r.URL.Path == logPath
	}, held, pod)
	ctx, stop := context.WithCancel(context.Background())
	defer stop()
	rt, err := sandbox.New(ctx, rc, sandboxOptions(unaskedTokens{}))
	if err != nil {
		t.Fatalf("build the Sandbox runtime: %v", err)
	}
	const generation = 1
	loc := runtime.Locator{
		Runtime: runtime.RuntimeSandbox, Claim: c.Token, Incarnation: runtime.SandboxIncarnation(kubeRefusalPodUID, generation),
		Sandbox: &runtime.SandboxLocator{Namespace: kubeNamespace, Name: name, PodUID: kubeRefusalPodUID,
			Container: runtime.IssuePodContainer, Generation: generation},
	}
	obs, err := rt.Probe(ctx, loc)
	if err != nil {
		t.Fatalf("probe the claim's process: %v", err)
	}
	api.requireEchoed(t)
	api.requireServed(t)

	raw, err := kubernetes.NewForConfig(rc)
	if err != nil {
		t.Fatal(err)
	}
	_, rawErr := raw.CoreV1().Pods(kubeNamespace).GetLogs(name, &corev1.PodLogOptions{Container: runtime.IssuePodContainer}).Stream(ctx)
	requireRawEcho(t, rawErr)
	api.requireBearer(t)
	if obs.Kind != runtime.Gone || obs.Locator.Incarnation != loc.Incarnation {
		t.Fatalf("the probe observed %s of %s (%q), want the recorded process %s gone", obs.Kind, obs.Locator.Incarnation, obs.Detail, loc.Incarnation)
	}
	return obs, loc
}

// TestAKubernetesRefusalTheSandboxRuntimeMetIsKeptOnTheClaimWithNoCredential: the two causes the
// machine keeps on a claim from what the Sandbox runtime answered (Claim.LastStartFailure and
// Claim.LastDeathCause), each of which read_state serves, the store persists and the machine's
// own lines log, hold no credential when the runtime met a Kubernetes refusal that echoed the
// bearer its request carried. Each row has the real runtime meet the refusal through its own
// Kubernetes clients, against a stand-in in the API server's place, and hands what the runtime
// answered to the machine as the runtime's answer reaches it (Deps.Runtime), the machine logging
// through the daemon's production logger (sinkHarness):
//
//   - last-start-failure: Spawn's error for a launch whose read of the claim's Sandbox the API
//     refused (sandboxReadRefusedLaunch) is the fake runtime's answer to the machine's spawn, under
//     a launch budget of one (launchRefusedByTheMint): launchNow keeps the error's text as the
//     last start failure (storetext.FarSideError), logs it ("supervise: launch failed", the
//     error in full after it) and fails the claim with it. The row reads the claim's
//     LastStartFailure and every line the machine logged.
//   - last-death-cause: Probe's observation of a working claim's process whose pod ended and whose
//     log read the API refused (refusedLogDeath) is handed to the machine as the runtime's
//     observation (RuntimeObservation), after the fake runtime answered the claim's launch with
//     the locator the probe observed, so the machine holds that process. died keeps the
//     observation's detail as the last death's cause (deathCause) and the record of what the
//     death ended (endDied), and logs the detail ("supervise: process died"). The row reads the
//     claim's LastDeathCause, its EndedWork.Cause and every line the machine logged.
//
// Captures kubeerror.supervise/<case>.
func TestAKubernetesRefusalTheSandboxRuntimeMetIsKeptOnTheClaimWithNoCredential(t *testing.T) {
	t.Run("last-start-failure", func(t *testing.T) {
		c := sinkClaim(t)
		refused := sandboxReadRefusedLaunch(t, c)
		h, log := launchRefusedByTheMint(t, c, refused)
		kept := h.Machine().Claim().LastStartFailure
		oauth2calltest.RequireNone(t, "kubeerror.supervise/last-start-failure", "LastStartFailure: "+kept+"\n"+log.String())

		want := "kubernetes: get sandboxes " + sandbox.SandboxName(c.Token) + ": the API server answered 403 Forbidden"
		if !strings.Contains(kept, want) {
			t.Errorf("the claim's last start failure is %q, want the refused read of its Sandbox, %q", kept, want)
		}
		if _, logged := sinkLines(t, log, "supervise: launch failed", "error"); len(logged) != 1 || !strings.Contains(logged[0], want) {
			t.Errorf("the machine logged the launch's failure as %q, want one line naming %q", logged, want)
		}
	})

	t.Run("last-death-cause", func(t *testing.T) {
		c := sinkClaim(t)
		obs, loc := refusedLogDeath(t, c)
		h, log := sinkHarness(t, c)
		h.Runtime().ScriptSpawn(fake.SpawnResult{Locator: loc})
		h.Reach(supervise.StateWorking)
		if held := h.Machine().Claim().Locator; held == nil || held.Incarnation != loc.Incarnation {
			t.Fatalf("the working claim holds the process %+v, want the one the probe observed, %s", held, loc.Incarnation)
		}
		h.Must(supervise.RuntimeObservation{Observation: obs})
		claim := h.Machine().Claim()
		oauth2calltest.RequireNone(t, "kubeerror.supervise/last-death-cause",
			"LastDeathCause: "+claim.LastDeathCause+"\nEndedWork.Cause: "+claim.EndedWork.Cause+"\n"+log.String())

		want := "kubernetes: read the log of pod " + sandbox.SandboxName(c.Token) + ": the API server answered 403 Forbidden"
		if !strings.Contains(claim.LastDeathCause, want) {
			t.Errorf("the claim's last death cause is %q, want the refused read of its pod's log, %q", claim.LastDeathCause, want)
		}
		if !strings.Contains(claim.EndedWork.Cause, want) {
			t.Errorf("the record of what the death ended is %q, want it to name %q", claim.EndedWork.Cause, want)
		}
		if _, logged := sinkLines(t, log, "supervise: process died", "detail"); len(logged) != 1 || !strings.Contains(logged[0], want) {
			t.Errorf("the machine logged the death as %q, want one line naming %q", logged, want)
		}
	})
}
