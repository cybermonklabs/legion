package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/sjawhar/envoy/oauth2call/oauth2calltest"
	"github.com/sjawhar/legion/daemon/internal/testlog"
)

// bootKubeBearer is the bearer the kubeconfig of every row here gives the daemon's Kubernetes
// clients, as a pod's ServiceAccount token is given: one of the made-up credentials
// oauth2calltest.RequireNone looks for, so a printed line that holds it fails its row.
const bootKubeBearer = "kube-bearer-" + oauth2calltest.Token

// The objects probe-boot's cluster check and image probe ask the Kubernetes API about: the
// installation the daemon checks (internal/daemon's agentSandbox) and the Sandboxes of the
// namespace kubernetesCheckRuntime names.
const (
	bootCRDName        = "sandboxes.agents.x-k8s.io"
	bootCRDPath        = "/apis/apiextensions.k8s.io/v1/customresourcedefinitions/" + bootCRDName
	bootControllerPath = "/apis/apps/v1/namespaces/agent-sandbox-system/deployments/agent-sandbox-controller"
	bootSandboxesPath  = "/apis/agents.x-k8s.io/v1beta1/namespaces/legion/sandboxes"
)

var (
	bootCRDResource     = schema.GroupVersionResource{Group: "apiextensions.k8s.io", Version: "v1", Resource: "customresourcedefinitions"}
	bootSandboxResource = schema.GroupVersionResource{Group: "agents.x-k8s.io", Version: "v1beta1", Resource: "sandboxes"}
)

// bootListKinds are the apiVersion and kind of each list the runtime's census and informers read
// in the namespace, by path.
var bootListKinds = map[string][2]string{
	bootSandboxesPath:                    {"agents.x-k8s.io/v1beta1", "Sandbox"},
	"/api/v1/namespaces/legion/pods":     {"v1", "Pod"},
	"/api/v1/namespaces/legion/services": {"v1", "Service"},
}

// bootKubeAPI is the Kubernetes API as probe-boot meets it, in the API server's place. It passes
// the cluster check: the Sandbox CRD serves the version the runtime speaks, and the controller's
// Deployment has an available replica. It answers the census and the runtime's informers as an API
// server holding nothing answers them: a list answers no item, and a watch that asks for its
// initial events (client-go's watch-list) gets the bookmark that ends them
// (k8s.io/initial-events-end) and nothing after. A request a row names in echoes is answered with
// a Status that echoes the Authorization the request carried (bootEchoStatus). Any other request is
// one probe-boot does not make: the API answers it 404 and the row fails on it (requireServed).
// Every request is kept with its Authorization, so a row can say what the API was sent. It is
// served over TLS: client-go gives a kubeconfig user's token only to a server it reaches over
// TLS, and sends no Authorization to an http:// one.
type bootKubeAPI struct {
	url string
	// authority is the server's certificate, PEM-encoded: the kubeconfig's certificate authority.
	authority []byte
	// echoes are the requests, "<method> <path>", the API answers with an echo of their code and
	// reason.
	echoes   map[string]bootEcho
	mu       sync.Mutex
	requests []bootRequest
	unserved []string
	// done ends every watch still open, before the server waits out its requests.
	done chan struct{}
}

// bootEcho is the code and reason of a Status that echoes a request's Authorization.
type bootEcho struct {
	code   int
	reason metav1.StatusReason
}

// bootRequest is one request the API was sent: its method and path, and its Authorization.
type bootRequest struct{ call, authorization string }

// newBootKubeAPI serves the API over TLS until t ends.
func newBootKubeAPI(t *testing.T, echoes map[string]bootEcho) *bootKubeAPI {
	t.Helper()
	a := &bootKubeAPI{echoes: echoes, done: make(chan struct{})}
	server := httptest.NewTLSServer(a)
	a.url = server.URL
	a.authority = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw})
	t.Cleanup(server.Close)
	t.Cleanup(func() { close(a.done) })
	return a
}

func (a *bootKubeAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	call := r.Method + " " + r.URL.Path
	a.mu.Lock()
	a.requests = append(a.requests, bootRequest{call: call, authorization: r.Header.Get("Authorization")})
	a.mu.Unlock()
	if echo, ok := a.echoes[call]; ok {
		bootAnswer(w, echo.code, bootEchoStatus(r, echo.code, echo.reason))
		return
	}
	kind, listed := bootListKinds[r.URL.Path]
	switch {
	case call == "GET "+bootCRDPath:
		bootAnswer(w, http.StatusOK, map[string]any{"apiVersion": "apiextensions.k8s.io/v1", "kind": "CustomResourceDefinition",
			"metadata": map[string]any{"name": bootCRDName},
			"spec":     map[string]any{"versions": []any{map[string]any{"name": bootSandboxResource.Version, "served": true}}}})
	case call == "GET "+bootControllerPath:
		bootAnswer(w, http.StatusOK, map[string]any{"apiVersion": "apps/v1", "kind": "Deployment",
			"metadata": map[string]any{"name": "agent-sandbox-controller", "namespace": "agent-sandbox-system"},
			"status":   map[string]any{"replicas": 1, "availableReplicas": 1}})
	case listed && r.Method == http.MethodGet && r.URL.Query().Get("watch") == "true":
		a.hold(w, r, kind)
	case listed && r.Method == http.MethodGet:
		bootAnswer(w, http.StatusOK, map[string]any{"apiVersion": kind[0], "kind": kind[1] + "List",
			"metadata": map[string]any{"resourceVersion": "1"}, "items": []any{}})
	default:
		a.mu.Lock()
		a.unserved = append(a.unserved, r.Method+" "+r.URL.RequestURI())
		a.mu.Unlock()
		bootAnswer(w, http.StatusNotFound, metav1.Status{TypeMeta: metav1.TypeMeta{Kind: "Status", APIVersion: "v1"},
			Status: metav1.StatusFailure, Reason: metav1.StatusReasonNotFound, Code: http.StatusNotFound})
	}
}

// hold answers a watch with no event of an object: the bookmark that ends a watch-list's initial
// events where the request asks for them, and then nothing until the request or the API ends.
func (a *bootKubeAPI) hold(w http.ResponseWriter, r *http.Request, kind [2]string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	if r.URL.Query().Get("sendInitialEvents") == "true" {
		_ = json.NewEncoder(w).Encode(map[string]any{"type": "BOOKMARK", "object": map[string]any{
			"apiVersion": kind[0], "kind": kind[1],
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

// requireSentBearer fails t unless every request the API was sent carried bootKubeBearer, and
// the API was sent call, the request a row's answer echoes, exactly want times: the row's control
// that whatever answered in the API server's place held the credential the row looks for, and was
// asked what the row's path asks.
func (a *bootKubeAPI) requireSentBearer(t *testing.T, call string, want int) {
	t.Helper()
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(a.requests) == 0 {
		t.Fatal("the Kubernetes API was sent no request")
	}
	asked := 0
	for _, r := range a.requests {
		if r.authorization != "Bearer "+bootKubeBearer {
			t.Fatalf("the Kubernetes API was sent %s with the Authorization %q, want the bearer %q", r.call, r.authorization, bootKubeBearer)
		}
		if r.call == call {
			asked++
		}
	}
	if asked != want {
		t.Fatalf("the Kubernetes API was sent %s %d times, want %d", call, asked, want)
	}
}

// requireServed fails t naming every request the API answered 404 as one probe-boot does not make.
func (a *bootKubeAPI) requireServed(t *testing.T) {
	t.Helper()
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(a.unserved) > 0 {
		t.Errorf("probe-boot asked the Kubernetes API for what its cluster check and image probe do not ask: %s",
			strings.Join(a.unserved, "; "))
	}
}

func bootAnswer(w http.ResponseWriter, code int, object any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(object)
}

// bootEchoStatus is a Status of code and reason that echoes what the request carried, its
// Authorization, in every field of it a server writes words into: the message, the details' name,
// group and kind, and a cause's type, message and field.
func bootEchoStatus(r *http.Request, code int, reason metav1.StatusReason) metav1.Status {
	carried := r.Header.Get("Authorization")
	return metav1.Status{TypeMeta: metav1.TypeMeta{Kind: "Status", APIVersion: "v1"}, Status: metav1.StatusFailure,
		Code: int32(code), Reason: reason, Message: "the request carried " + carried,
		Details: &metav1.StatusDetails{Name: carried, Group: carried, Kind: carried, Causes: []metav1.StatusCause{{
			Type: metav1.CauseType(carried), Message: carried, Field: carried,
		}}}}
}

// bootKubeconfig writes the kubeconfig config names (kubernetesCheckRuntime's ./kubeconfig): its
// context's server the API, trusted by the API's own certificate, and its user's token
// bootKubeBearer. It answers the file's path.
func bootKubeconfig(t *testing.T, config string, api *bootKubeAPI) string {
	t.Helper()
	kubeconfig := "apiVersion: v1\nkind: Config\n" +
		"clusters: [{name: example, cluster: {server: \"" + api.url + "\", certificate-authority-data: " +
		base64.StdEncoding.EncodeToString(api.authority) + "}}]\n" +
		"users: [{name: legion-daemon, user: {token: " + bootKubeBearer + "}}]\n" +
		"contexts: [{name: legion-daemon, context: {cluster: example, user: legion-daemon, namespace: legion}}]\n" +
		"current-context: legion-daemon\n"
	path := filepath.Join(filepath.Dir(config), "kubeconfig")
	if err := os.WriteFile(path, []byte(kubeconfig), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// bootRawClient is a dynamic client of the kubeconfig at path, built as the daemon builds its
// Kubernetes client (internal/daemon's kubeClient: the context runtime.kubernetes names), with
// nothing of the runtime between it and client-go: a row's control reads the API's answer through
// it as client-go hands it back.
func bootRawClient(t *testing.T, path string) dynamic.Interface {
	t.Helper()
	rc, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
		&clientcmd.ClientConfigLoadingRules{ExplicitPath: path}, &clientcmd.ConfigOverrides{CurrentContext: "legion-daemon"}).ClientConfig()
	if err != nil {
		t.Fatal(err)
	}
	if rc.BearerToken != bootKubeBearer {
		t.Fatalf("the kubeconfig gives client-go the bearer %q (host %q), want %q", rc.BearerToken, rc.Host, bootKubeBearer)
	}
	dyn, err := dynamic.NewForConfig(rc)
	if err != nil {
		t.Fatal(err)
	}
	return dyn
}

// requireEchoed fails t unless err, the error client-go alone makes of the API's answer, holds the
// made-up credential: a row whose answer echoed nothing would print none of it however its
// printers read, so this is what says the row's check could see one.
func requireEchoed(t *testing.T, err error) {
	t.Helper()
	if err == nil || !strings.Contains(err.Error(), oauth2calltest.Token) {
		t.Fatalf("client-go's error for the API's answer is %v, want one holding %q", err, oauth2calltest.Token)
	}
}

// bootControlSandbox is the Sandbox a transient row's control creates through client-go alone,
// which the API refuses as it refuses the image probe's.
func bootControlSandbox() *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": bootSandboxResource.GroupVersion().String(), "kind": "Sandbox",
		"metadata": map[string]any{"name": "control"},
	}}
}

// The transient failure's lines on stderr: bootprobe.Run's retry line, the detail it carries (the
// image probe's create of its Sandbox, internal/runtime/sandbox's createProbe), and the refusal
// probe-boot prints when the attempts ran out or the command was stopped while it waited.
const (
	bootRetryLine     = `"msg":"boot probe failed transiently; waiting to run it again"`
	bootRetryDetail   = `"detail":"create probe sandbox legion-probe-demo-`
	bootExhausted     = "legion probe-boot: the worker image probe never completed within its retry budget (6 attempts): create probe sandbox legion-probe-demo-"
	bootAbandonedLine = "legion probe-boot: the worker image probe was abandoned: "
)

// The cluster check's read of the Sandbox CRD is refused 403 with a Status that echoes the bearer
// the request carried, in its message, its details and its cause. client-go makes an error of that
// answer whose text is the message; the runtime keeps it as its projection (sandbox's
// projectKubeError, in the installation check's refusal), and probe-boot prints its refusal
// through printRefusal: the refusal names the CRD and the 403, and none of the bearer.
func TestProbeBootPrintsAKubernetesRefusalWithNoCredentialItsAnswerEchoed(t *testing.T) {
	legionState(t)
	api := newBootKubeAPI(t, map[string]bootEcho{"GET " + bootCRDPath: {http.StatusForbidden, metav1.StatusReasonForbidden}})
	config := kubernetesCheckConfig(t, "")
	kubeconfig := bootKubeconfig(t, config, api)
	_, err := bootRawClient(t, kubeconfig).Resource(bootCRDResource).Get(context.Background(), bootCRDName, metav1.GetOptions{})
	requireEchoed(t, err)

	var out bytes.Buffer
	var errb testlog.Buffer
	code := run(context.Background(), []string{"legion", "probe-boot", "--config", config}, &out, &errb)

	stderr := errb.String()
	oauth2calltest.RequireNone(t, "kubeerror.legion/boot-refusal", stderr)
	api.requireSentBearer(t, "GET "+bootCRDPath, 2)
	if code != 1 || out.Len() != 0 || !strings.HasPrefix(stderr, "legion probe-boot: ") ||
		!strings.Contains(stderr, "the Sandbox CRD "+bootCRDName+" cannot be read (403)") {
		t.Fatalf("exit code = %d, stdout %q, stderr %q; want 1, no OK line, and a refusal naming the Sandbox CRD's 403", code, out.String(), stderr)
	}
	api.requireServed(t)
}

// The cluster check passes, and the image probe's create of its Sandbox is answered 500
// InternalError with a Status that echoes the bearer. The attempt is transient (createProbe's
// apiOutcome), so bootprobe.Run logs its retry line on stderr, through the logger probe-boot
// builds (redact.HandlerOptions), with the attempt's detail; the command is then stopped while it
// waits to run the probe again, as a SIGINT stops it (main's signal.NotifyContext cancels the
// context run is handed), and prints the refusal of an abandoned probe. The runtime keeps the
// create's error as its projection (resend's projectKubeError) before either holds it: neither
// holds the bearer. Waiting out every attempt instead takes bootprobe.Image's 310 s.
func TestProbeBootLogsATransientKubernetesFailureWithNoCredentialWhenStoppedWaiting(t *testing.T) {
	legionState(t)
	api := newBootKubeAPI(t, map[string]bootEcho{"POST " + bootSandboxesPath: {http.StatusInternalServerError, metav1.StatusReasonInternalError}})
	config := kubernetesCheckConfig(t, "")
	kubeconfig := bootKubeconfig(t, config, api)
	_, err := bootRawClient(t, kubeconfig).Resource(bootSandboxResource).Namespace("legion").
		Create(context.Background(), bootControlSandbox(), metav1.CreateOptions{})
	requireEchoed(t, err)

	ctx, stop := context.WithCancel(context.Background())
	defer stop()
	var out bytes.Buffer
	var errb testlog.Buffer
	ended := make(chan int, 1)
	go func() { ended <- run(ctx, []string{"legion", "probe-boot", "--config", config}, &out, &errb) }()
	// The first retry line comes once the first attempt's create is refused; its wait is 10 s, so a
	// poll of the log every 20 ms stops the command inside it. Two minutes bound a run that never
	// logs one.
	poll := time.NewTicker(20 * time.Millisecond)
	defer poll.Stop()
	bound := time.NewTimer(2 * time.Minute)
	defer bound.Stop()
	for !strings.Contains(errb.String(), bootRetryLine) {
		select {
		case code := <-ended:
			t.Fatalf("probe-boot ended with %d before it logged a transient failure; stderr:\n%s", code, errb.String())
		case <-bound.C:
			stop()
			<-ended
			t.Fatalf("probe-boot logged no transient failure in two minutes; stderr:\n%s", errb.String())
		case <-poll.C:
		}
	}
	stop()
	code := <-ended

	stderr := errb.String()
	oauth2calltest.RequireNone(t, "kubeerror.legion/boot-transient-stopped", stderr)
	api.requireSentBearer(t, "POST "+bootSandboxesPath, 1+1)
	if retries, details := strings.Count(stderr, bootRetryLine), strings.Count(stderr, bootRetryDetail); code != 1 || out.Len() != 0 ||
		retries != 1 || details != 1 || !strings.Contains(stderr, bootAbandonedLine) {
		t.Fatalf("exit code = %d, stdout %q, %d retry lines with %d create details, stderr:\n%s\nwant 1, no OK line, one retry line with the create's detail, and the abandoned probe's refusal",
			code, out.String(), retries, details, stderr)
	}
	api.requireServed(t)
}
