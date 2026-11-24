package sandbox

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/rest"
	"k8s.io/klog/v2"

	"github.com/sjawhar/envoy/oauth2call/oauth2calltest"
	"github.com/sjawhar/legion/daemon/internal/testlog"
)

// kubeBearer is the bearer the runtime's clients carry to the Kubernetes API stand-in in every row
// of the client exits (kubeerror_rows_test.go), as the in-cluster ServiceAccount token is carried
// to the API server: one of the made-up credentials oauth2calltest.RequireNone looks for, so a
// line that holds it fails its row.
const kubeBearer = "kube-bearer-" + oauth2calltest.Token

// kubeSecretValue is the value a row's Secret write carries in its body: the made-up password
// oauth2calltest.RequireNone looks for (internal/redact/redacttest's Password).
const kubeSecretValue = "not-a-secret-password"

// kubeStandIn is a Kubernetes API in the API server's place, for the rows of the runtime's client
// exits. It answers the runtime's informers as an API server holding nothing answers them: a list
// answers no item, a watch that asks for its initial events (client-go's watch-list) gets the
// bookmark that ends them (k8s.io/initial-events-end), and a watch then sends nothing until its
// request or the stand-in ends. A row's answer, when it has one, answers a request first. Every
// request is kept, with the Authorization it carried, so a row can say what the stand-in was sent.
type kubeStandIn struct {
	URL string
	mu  sync.Mutex
	// answer is the row's answer to a request; it answers false to leave the request to the
	// stand-in's own answers.
	answer   func(w http.ResponseWriter, r *http.Request) bool
	sent     []string
	requests []kubeRequest
	done     chan struct{}
}

// kubeRequest is one request the stand-in was sent.
type kubeRequest struct {
	method, path string
	query        map[string][]string
}

func (r kubeRequest) watch() bool { return r.get("watch") == "true" }

func (r kubeRequest) watchList() bool { return r.watch() && r.get("sendInitialEvents") == "true" }

func (r kubeRequest) get(key string) string {
	if v := r.query[key]; len(v) > 0 {
		return v[0]
	}
	return ""
}

// newKubeStandIn serves the stand-in until t ends.
func newKubeStandIn(t *testing.T, answer func(w http.ResponseWriter, r *http.Request) bool) *kubeStandIn {
	t.Helper()
	k := &kubeStandIn{answer: answer, done: make(chan struct{})}
	server := httptest.NewServer(k)
	k.URL = server.URL
	t.Cleanup(server.Close)
	t.Cleanup(func() { close(k.done) })
	return k
}

// config is how the runtime reaches the stand-in: with kubeBearer, in JSON both ways, since
// client-go's typed clients prefer protobuf for the built-in group unless the configuration names
// a content type.
func (k *kubeStandIn) config() *rest.Config {
	return &rest.Config{Host: k.URL, BearerToken: kubeBearer, ContentConfig: rest.ContentConfig{
		ContentType: "application/json", AcceptContentTypes: "application/json",
	}}
}

func (k *kubeStandIn) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	k.mu.Lock()
	k.sent = append(k.sent, r.Header.Get("Authorization"))
	k.requests = append(k.requests, kubeRequest{method: r.Method, path: r.URL.Path, query: r.URL.Query()})
	answer := k.answer
	k.mu.Unlock()
	if answer != nil && answer(w, r) {
		return
	}
	kind, ok := kubeListKind(r.URL.Path)
	switch {
	case !ok || r.Method != http.MethodGet:
		kubeStatusAnswer(w, http.StatusNotFound, metav1.StatusReasonNotFound, "")
	case r.URL.Query().Get("watch") == "true":
		k.hold(w, r, kind)
	default:
		kubeObjectAnswer(w, http.StatusOK, map[string]any{"apiVersion": kind.apiVersion, "kind": kind.kind + "List",
			"metadata": map[string]any{"resourceVersion": "1"}, "items": []any{}})
	}
}

// hold answers a watch with no event of an object: the bookmark that ends a watch-list's initial
// events where the request asks for them, and then nothing until the request or the stand-in ends.
func (k *kubeStandIn) hold(w http.ResponseWriter, r *http.Request, kind kubeKind) {
	startWatch(w)
	if r.URL.Query().Get("sendInitialEvents") == "true" {
		kubeWatchEvent(w, "BOOKMARK", initialEventsEnd(kind))
	}
	select {
	case <-r.Context().Done():
	case <-k.done:
	}
}

// startWatch answers a watch's request: a stream of JSON events, which is what client-go decodes.
func startWatch(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
}

// endWatchList answers a watch-list of kind with no object, the bookmark that ends its initial
// events, then a bookmark of a later resource version, and ends the stream: the informer's store
// is synced, and the reflector, having had an event on the watch it goes on with, watches again
// from that resource version.
func endWatchList(w http.ResponseWriter, kind kubeKind) {
	startWatch(w)
	kubeWatchEvent(w, "BOOKMARK", initialEventsEnd(kind))
	kubeWatchEvent(w, "BOOKMARK", map[string]any{"apiVersion": kind.apiVersion, "kind": kind.kind,
		"metadata": map[string]any{"resourceVersion": "2"}})
}

// initialEventsEnd is the bookmark that ends a watch-list's initial events of kind.
func initialEventsEnd(kind kubeKind) map[string]any {
	return map[string]any{"apiVersion": kind.apiVersion, "kind": kind.kind,
		"metadata": map[string]any{"resourceVersion": "1", "annotations": map[string]any{metav1.InitialEventsAnnotationKey: "true"}}}
}

// bearers are the Authorizations the stand-in was sent.
func (k *kubeStandIn) bearers() []string {
	k.mu.Lock()
	defer k.mu.Unlock()
	return slices.Clone(k.sent)
}

// asked is every request the stand-in was sent.
func (k *kubeStandIn) asked() []kubeRequest {
	k.mu.Lock()
	defer k.mu.Unlock()
	return slices.Clone(k.requests)
}

// requireSentBearer fails t unless the stand-in was sent the bearer want on every request: the
// row's control that whatever answered in the API server's place held the credential the row
// looks for.
func (k *kubeStandIn) requireSentBearer(t *testing.T, want string) {
	t.Helper()
	sent := k.bearers()
	if len(sent) == 0 {
		t.Fatal("the stand-in was sent no request")
	}
	for _, s := range sent {
		if s != "Bearer "+want {
			t.Fatalf("the stand-in was sent the Authorization %q, want the bearer %q", s, want)
		}
	}
}

// kubeKind is the apiVersion and kind of one resource the runtime's informers list and watch.
type kubeKind struct{ apiVersion, kind string }

// kubeListKind is the kind of a list or watch path of the runtime's namespace, for the resources
// its informers read.
func kubeListKind(path string) (kubeKind, bool) {
	switch path {
	case "/apis/" + sandboxGVR.Group + "/" + sandboxGVR.Version + "/namespaces/" + testNamespace + "/sandboxes":
		return kubeKind{sandboxGVR.Group + "/" + sandboxGVR.Version, "Sandbox"}, true
	case "/api/v1/namespaces/" + testNamespace + "/pods":
		return kubeKind{"v1", "Pod"}, true
	case "/api/v1/namespaces/" + testNamespace + "/services":
		return kubeKind{"v1", "Service"}, true
	}
	return kubeKind{}, false
}

func kubeObjectAnswer(w http.ResponseWriter, code int, object any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(object)
}

// kubeStatusAnswer answers a Status of code and reason whose message is message.
func kubeStatusAnswer(w http.ResponseWriter, code int, reason metav1.StatusReason, message string) {
	kubeObjectAnswer(w, code, metav1.Status{TypeMeta: metav1.TypeMeta{Kind: "Status", APIVersion: "v1"},
		Status: metav1.StatusFailure, Reason: reason, Code: int32(code), Message: message})
}

// echoStatus is a Status of code and reason that echoes what the request carried, its
// Authorization, in every field of it a server writes words into: the message, the details' name,
// group and kind, and a cause's type, message and field.
func echoStatus(r *http.Request, code int, reason metav1.StatusReason) metav1.Status {
	carried := r.Header.Get("Authorization")
	return metav1.Status{TypeMeta: metav1.TypeMeta{Kind: "Status", APIVersion: "v1"}, Status: metav1.StatusFailure,
		Code: int32(code), Reason: reason, Message: "the request carried " + carried,
		Details: &metav1.StatusDetails{Name: carried, Group: carried, Kind: carried, Causes: []metav1.StatusCause{{
			Type: metav1.CauseType(carried), Message: carried, Field: carried,
		}}}}
}

// answerEcho answers a request with echoStatus of code and reason.
func answerEcho(w http.ResponseWriter, r *http.Request, code int, reason metav1.StatusReason) {
	kubeObjectAnswer(w, code, echoStatus(r, code, reason))
}

// answerTextEcho answers a request with code and a plain-text body that echoes its Authorization,
// as a proxy or a load balancer in the API server's place may.
func answerTextEcho(w http.ResponseWriter, r *http.Request, code int) {
	w.Header().Set("Content-Type", "text/plain")
	w.WriteHeader(code)
	_, _ = fmt.Fprintf(w, "refused: the request carried %s", r.Header.Get("Authorization"))
}

// kubeWatchEvent writes one watch event of kind on w and flushes it.
func kubeWatchEvent(w io.Writer, kind string, object any) {
	_ = json.NewEncoder(w).Encode(map[string]any{"type": kind, "object": object})
	if flusher, ok := w.(http.Flusher); ok {
		flusher.Flush()
	}
}

// klogCapture routes client-go's own logging (klog: the reflector's, the token source's) into a
// buffer for the rest of t, at the level the daemon's logger takes (Info: cmd/legion builds it
// with redact.HandlerOptions(nil), and no setting lowers it), and sets klog's verbosity to v for
// the leveled lines that read klog's own flag (k8s.io/utils/trace's), as no daemon sets it. It is
// process-wide, so no row that calls it runs in parallel.
func klogCapture(t *testing.T, v string) *testlog.Buffer {
	t.Helper()
	logged := &testlog.Buffer{}
	klog.SetSlogLogger(slog.New(slog.NewJSONHandler(logged, nil)))
	flags := klogFlags(t)
	if err := flags.Set("v", v); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		klog.Flush()
		_ = flags.Set("v", "0")
		klog.ClearLogger()
	})
	return logged
}

// requireNoneLogged captures what client-go logged under capture and fails t for each made-up
// credential it holds; it fails t first where nothing was logged, since a capture of nothing
// would pass as one that holds no credential.
func requireNoneLogged(t *testing.T, capture string, logged *testlog.Buffer, want string) {
	t.Helper()
	klog.Flush()
	text := logged.String()
	if !strings.Contains(text, want) {
		t.Fatalf("client-go logged no line holding %q; it logged:\n%s", want, text)
	}
	oauth2calltest.RequireNone(t, capture, text)
}
