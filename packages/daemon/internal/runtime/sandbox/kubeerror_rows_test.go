package sandbox

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/rest"
	"k8s.io/klog/v2"

	"github.com/sjawhar/envoy/oauth2call/oauth2calltest"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/testlog"
)

// klogFlagSet holds klog's own flags, registered once, so a row can set its verbosity.
var klogFlagSet = sync.OnceValue(func() *flag.FlagSet {
	flags := flag.NewFlagSet("klog", flag.ContinueOnError)
	klog.InitFlags(flags)
	return flags
})

func klogFlags(*testing.T) *flag.FlagSet { return klogFlagSet() }

// requestRuntime is the runtime's request side over rc as startInformers builds it, with no
// informer: its dynamic and typed clients (clientsOf over requestConfig) each sent through resend
// (patientDynamic, patientKube), logging to log. raw is the same clients with nothing between them
// and client-go: the boundary before this change, which a row's control reads.
func requestRuntime(t *testing.T, rc *rest.Config, log *slog.Logger) (r *Runtime, raw kubeClients) {
	t.Helper()
	c, err := clientsOf(requestConfig(rc))
	if err != nil {
		t.Fatal(err)
	}
	r = &Runtime{namespace: testNamespace, project: testProject, log: log, bootTimeout: 2 * time.Second,
		busyBackoff: time.Millisecond, now: time.Now}
	r.dyn, r.kube = patientDynamic{c.dyn, r}, patientKube{c.kube, r}
	return r, c
}

// requireRawLeaks is a row's control: the error client-go handed the runtime for the same answer,
// with nothing between them, holds the made-up credential the stand-in echoed. A row whose control
// held none would pass whatever the runtime printed.
func requireRawLeaks(t *testing.T, err error) {
	t.Helper()
	if err == nil || !strings.Contains(err.Error(), oauth2calltest.Token) {
		t.Fatalf("client-go's own error for the stand-in's answer is %v, want the echoed credential in it", err)
	}
}

// eventually waits until done reports true, failing t naming what it waited for when it does not
// within a bound no correct run comes near.
func eventually(t *testing.T, what string, done func() bool) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for !done() {
		if time.Now().After(deadline) {
			t.Fatalf("waited 30s for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func secretPath(name string) string {
	return "/api/v1/namespaces/" + testNamespace + "/secrets/" + name
}

func sandboxPath(name string) string {
	return "/apis/" + sandboxGVR.Group + "/" + sandboxGVR.Version + "/namespaces/" + testNamespace + "/sandboxes/" + name
}

// rowSecret is the Secret a row's write asks for: owned by the Sandbox of uid-sandbox, holding
// a value the API server may echo.
func rowSecret(name string) corev1.Secret {
	return corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: name, OwnerReferences: []metav1.OwnerReference{{UID: "uid-sandbox"}}},
		Data:       map[string][]byte{"token": []byte(kubeSecretValue)},
	}
}

// TestAKubernetesRefusalOfARuntimeRequestPrintsNoneOfWhatTheRequestCarried: the API server, or
// whatever answers in its place, refuses a read of the runtime's (resend, through patientKube's
// Secret Get and patientDynamic's Sandbox Get) with words that echo the bearer the request carried
// (kubeBearer): a Status whose message, details and cause echo it (status), a plain-text 403 and
// 500 (text-403, text-500), a Status of a reason no Kubernetes release writes (unknown-reason), a
// Status that does not decode, sent as text (malformed-status), and a 200 whose body is no object
// client-go can decode (decode). client-go's own error for each holds the bearer (the control);
// the runtime's (writeSecret, readSandbox) holds none of it, and names the status code where the
// answer had one. Captures kubeerror.answer/<case>.
func TestAKubernetesRefusalOfARuntimeRequestPrintsNoneOfWhatTheRequestCarried(t *testing.T) {
	for _, c := range []struct {
		name    string
		answer  func(w http.ResponseWriter, r *http.Request)
		sandbox bool
		want    string
	}{
		{name: "status", want: "403", answer: func(w http.ResponseWriter, r *http.Request) {
			answerEcho(w, r, http.StatusForbidden, metav1.StatusReasonForbidden)
		}},
		{name: "text-403", want: "403", answer: func(w http.ResponseWriter, r *http.Request) { answerTextEcho(w, r, http.StatusForbidden) }},
		{name: "text-500", want: "500", answer: func(w http.ResponseWriter, r *http.Request) {
			answerTextEcho(w, r, http.StatusInternalServerError)
		}},
		{name: "unknown-reason", want: "500", answer: func(w http.ResponseWriter, r *http.Request) {
			status := echoStatus(r, http.StatusInternalServerError, metav1.StatusReason(kubeBearer))
			kubeObjectAnswer(w, http.StatusInternalServerError, status)
		}},
		{name: "malformed-status", want: "500", answer: func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/plain")
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = fmt.Fprintf(w, `{"kind":"Status","apiVersion":"v1","status":"Failure","code":"invalid","message":%q}`,
				"the request carried "+r.Header.Get("Authorization"))
		}},
		{name: "decode", sandbox: true, answer: func(w http.ResponseWriter, r *http.Request) {
			kubeObjectAnswer(w, http.StatusOK, map[string]any{"metadata": map[string]any{"name": r.Header.Get("Authorization")}})
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			k := newKubeStandIn(t, func(w http.ResponseWriter, r *http.Request) bool {
				if r.URL.Path != secretPath("s") && r.URL.Path != sandboxPath("s") {
					return false
				}
				c.answer(w, r)
				return true
			})
			r, raw := requestRuntime(t, k.config(), slog.New(slog.DiscardHandler))
			ctx := context.Background()
			var rawErr, err error
			if c.sandbox {
				_, rawErr = raw.dyn.Resource(sandboxGVR).Namespace(testNamespace).Get(ctx, "s", metav1.GetOptions{})
				_, err = r.readSandbox(ctx, "s")
			} else {
				_, rawErr = raw.kube.CoreV1().Secrets(testNamespace).Get(ctx, "s", metav1.GetOptions{})
				err = r.writeSecret(ctx, rowSecret("s"))
			}
			requireRawLeaks(t, rawErr)
			if err == nil {
				t.Fatal("the runtime's read answered no error")
			}
			k.requireSentBearer(t, kubeBearer)
			oauth2calltest.RequireNone(t, "kubeerror.answer/"+c.name, err.Error())
			if !strings.Contains(err.Error(), c.want) {
				t.Errorf("the runtime's error is %q, want it to name %q", err, c.want)
			}
		})
	}
}

// TestALaunchsRequestTheAPIServerAnsweredOverloadedLogsNoneOfWhatItCarried: a launch's read
// (launching) the API server answers overloaded, 503 ServiceUnavailable, and then refuses, 403
// Forbidden, each answer a Status that echoes the bearer the request carried: resend logs that it
// sends the read again, and that the server answered it, and the launch fails on the refusal.
// Neither line nor the error holds the bearer. Captures kubeerror.resend-log.
func TestALaunchsRequestTheAPIServerAnsweredOverloadedLogsNoneOfWhatItCarried(t *testing.T) {
	var asked atomic.Int64
	k := newKubeStandIn(t, func(w http.ResponseWriter, r *http.Request) bool {
		switch r.URL.Path {
		case secretPath("control"):
			answerEcho(w, r, http.StatusServiceUnavailable, metav1.StatusReasonServiceUnavailable)
		case secretPath("s"):
			if asked.Add(1) == 1 {
				answerEcho(w, r, http.StatusServiceUnavailable, metav1.StatusReasonServiceUnavailable)
			} else {
				answerEcho(w, r, http.StatusForbidden, metav1.StatusReasonForbidden)
			}
		default:
			return false
		}
		return true
	})
	logged := &testlog.Buffer{}
	r, raw := requestRuntime(t, k.config(), slog.New(slog.NewJSONHandler(logged, nil)))
	_, rawErr := raw.kube.CoreV1().Secrets(testNamespace).Get(context.Background(), "control", metav1.GetOptions{})
	requireRawLeaks(t, rawErr)

	err := r.writeSecret(launching(context.Background()), rowSecret("s"))

	if err == nil || asked.Load() != 2 {
		t.Fatalf("the launch's read was asked %d times and answered %v, want it sent again once and refused", asked.Load(), err)
	}
	k.requireSentBearer(t, kubeBearer)
	lines := logged.String()
	for _, want := range []string{"sending it again after a backoff", "answered a launch's request it had answered overloaded"} {
		if !strings.Contains(lines, want) {
			t.Fatalf("resend logged no line %q; it logged:\n%s", want, lines)
		}
	}
	oauth2calltest.RequireNone(t, "kubeerror.resend-log", lines+"\n"+err.Error())
}

// TestAnOverloadedOrAdmissionDeniedAnswerToALaunchIsDecidedAsBefore: resend decides whether a
// launch's request is sent again from the answer as the API server gave it (serverBusy), so the
// projection of its words changes no decision: a 503 that names no reason is overload, judged by
// its code, and sent again until the server answers it (unreasoned-503); a 500 whose message says
// an admission webhook denied the request is a refusal, sent once (admission-denied).
func TestAnOverloadedOrAdmissionDeniedAnswerToALaunchIsDecidedAsBefore(t *testing.T) {
	t.Run("unreasoned-503", func(t *testing.T) {
		var gets atomic.Int64
		k := newKubeStandIn(t, func(w http.ResponseWriter, r *http.Request) bool {
			switch {
			case r.URL.Path == secretPath("s") && r.Method == http.MethodGet:
				if gets.Add(1) <= 2 {
					kubeStatusAnswer(w, http.StatusServiceUnavailable, "", "")
				} else {
					kubeStatusAnswer(w, http.StatusNotFound, metav1.StatusReasonNotFound, "")
				}
			case r.URL.Path == "/api/v1/namespaces/"+testNamespace+"/secrets" && r.Method == http.MethodPost:
				kubeObjectAnswer(w, http.StatusCreated, map[string]any{"apiVersion": "v1", "kind": "Secret",
					"metadata": map[string]any{"name": "s", "namespace": testNamespace}})
			default:
				return false
			}
			return true
		})
		r, _ := requestRuntime(t, k.config(), slog.New(slog.DiscardHandler))
		if err := r.writeSecret(launching(context.Background()), rowSecret("s")); err != nil || gets.Load() != 3 {
			t.Fatalf("the write read the Secret %d times and answered %v, want two overloaded reads sent again and the create made", gets.Load(), err)
		}
	})
	t.Run("admission-denied", func(t *testing.T) {
		var creates atomic.Int64
		k := newKubeStandIn(t, func(w http.ResponseWriter, r *http.Request) bool {
			switch {
			case r.URL.Path == secretPath("s") && r.Method == http.MethodGet:
				kubeStatusAnswer(w, http.StatusNotFound, metav1.StatusReasonNotFound, "")
			case r.URL.Path == "/api/v1/namespaces/"+testNamespace+"/secrets" && r.Method == http.MethodPost:
				creates.Add(1)
				kubeStatusAnswer(w, http.StatusInternalServerError, metav1.StatusReasonInternalError,
					`admission webhook "deny.example.com" denied the request: no Secrets here`)
			default:
				return false
			}
			return true
		})
		r, _ := requestRuntime(t, k.config(), slog.New(slog.DiscardHandler))
		err := r.writeSecret(launching(context.Background()), rowSecret("s"))
		if err == nil || creates.Load() != 1 || !apierrors.IsInternalError(err) {
			t.Fatalf("the create was sent %d times and answered %v, want it sent once and refused as the server's 500", creates.Load(), err)
		}
	})
}

// TestARotatedBearerTheAPIServerEchoesPrintsNoneOfIt: what the API server echoes is whatever the
// request it answers carried, not only a bearer the configuration names. The runtime's clients
// read their bearer from a file the kubelet rotates (BearerTokenFile, as the in-cluster
// configuration does), which client-go reads again once the bearer it holds is 50 s old
// (transport.NewCachedFileTokenSource); the configuration's own BearerToken is empty. Once the
// file is rotated, a read carries the new bearer, which the server echoes in a Status: the
// runtime's error holds neither bearer. It waits out client-go's own period, about 50 s. Captures
// kubeerror.carried/rotated-bearer.
func TestARotatedBearerTheAPIServerEchoesPrintsNoneOfIt(t *testing.T) {
	const rotated = "rotated-bearer-" + oauth2calltest.Token
	k := newKubeStandIn(t, func(w http.ResponseWriter, r *http.Request) bool {
		if r.URL.Path != secretPath("s") {
			return false
		}
		answerEcho(w, r, http.StatusForbidden, metav1.StatusReasonForbidden)
		return true
	})
	file := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(file, []byte(kubeBearer), 0o600); err != nil {
		t.Fatal(err)
	}
	rc := k.config()
	rc.BearerToken, rc.BearerTokenFile = "", file
	r, _ := requestRuntime(t, rc, slog.New(slog.DiscardHandler))
	if err := r.writeSecret(context.Background(), rowSecret("s")); err == nil {
		t.Fatal("the first read answered no error")
	}
	if err := os.WriteFile(file, []byte(rotated), 0o600); err != nil {
		t.Fatal(err)
	}

	var err error
	for deadline := time.Now().Add(90 * time.Second); ; time.Sleep(500 * time.Millisecond) {
		err = r.writeSecret(context.Background(), rowSecret("s"))
		if sent := k.bearers(); sent[len(sent)-1] == "Bearer "+rotated {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("no read carried the rotated bearer within 90s; the stand-in was sent %q", k.bearers())
		}
	}

	if err == nil {
		t.Fatal("the read that carried the rotated bearer answered no error")
	}
	oauth2calltest.RequireNone(t, "kubeerror.carried/rotated-bearer", err.Error())
}

// TestASecretValueTheAPIServerEchoesPrintsNoneOfIt: a Secret's create and its update carry the
// Secret's value in their body, which the API server, or whatever answers in its place, echoes in
// its refusal (create-body, update-body). The runtime's error (writeSecret) holds none of it.
// Captures kubeerror.carried/<case>.
func TestASecretValueTheAPIServerEchoesPrintsNoneOfIt(t *testing.T) {
	for _, c := range []struct {
		name     string
		existing bool
	}{{"create-body", false}, {"update-body", true}} {
		t.Run(c.name, func(t *testing.T) {
			var echoed atomic.Bool
			k := newKubeStandIn(t, func(w http.ResponseWriter, r *http.Request) bool {
				switch {
				case r.URL.Path == secretPath("s") && r.Method == http.MethodGet && c.existing:
					kubeObjectAnswer(w, http.StatusOK, map[string]any{"apiVersion": "v1", "kind": "Secret", "metadata": map[string]any{
						"name": "s", "namespace": testNamespace, "uid": "uid-secret",
						"ownerReferences": []any{map[string]any{"apiVersion": "v1", "kind": "Sandbox", "name": "s", "uid": "uid-sandbox"}},
					}})
				case r.URL.Path == secretPath("s") && r.Method == http.MethodGet:
					kubeStatusAnswer(w, http.StatusNotFound, metav1.StatusReasonNotFound, "")
				case r.Method == http.MethodPost || r.Method == http.MethodPut:
					var sent corev1.Secret
					if err := json.NewDecoder(r.Body).Decode(&sent); err != nil {
						t.Errorf("the write's body does not decode: %v", err)
					}
					value := string(sent.Data["token"])
					echoed.Store(value == kubeSecretValue)
					code, reason := http.StatusUnprocessableEntity, metav1.StatusReasonInvalid
					if c.existing {
						code, reason = http.StatusConflict, metav1.StatusReasonConflict
					}
					kubeStatusAnswer(w, code, reason, "the Secret's value "+value+" is refused")
				default:
					return false
				}
				return true
			})
			r, _ := requestRuntime(t, k.config(), slog.New(slog.DiscardHandler))

			err := r.writeSecret(context.Background(), rowSecret("s"))

			if !echoed.Load() || err == nil {
				t.Fatalf("the stand-in echoed the Secret's value %t and the write answered %v, want the value echoed and the write refused", echoed.Load(), err)
			}
			oauth2calltest.RequireNone(t, "kubeerror.carried/"+c.name, err.Error())
		})
	}
}

// TestAKubernetesRefusalPrintsNoneOfWhatItCarriedUnderAnyPrinter: the runtime's error for a refusal
// that echoes the bearer (writeSecret, a 403 Status), printed every way a caller may print it:
// Error, every fmt verb, wrapped with %w, in slog's JSON and text lines, as JSON, each error it
// unwraps to, and what errors.As finds in it, the APIStatus whose Status every Kubernetes decision
// reads and a *StatusError, which client-go makes of every Status. None holds the bearer; no
// unwrap, As or Status reaches the answer's words. Captures kubeerror.printers/<form>.
func TestAKubernetesRefusalPrintsNoneOfWhatItCarriedUnderAnyPrinter(t *testing.T) {
	k := newKubeStandIn(t, func(w http.ResponseWriter, r *http.Request) bool {
		if r.URL.Path != secretPath("s") {
			return false
		}
		answerEcho(w, r, http.StatusForbidden, metav1.StatusReasonForbidden)
		return true
	})
	r, raw := requestRuntime(t, k.config(), slog.New(slog.DiscardHandler))
	_, rawErr := raw.kube.CoreV1().Secrets(testNamespace).Get(context.Background(), "s", metav1.GetOptions{})
	requireRawLeaks(t, rawErr)
	err := r.writeSecret(context.Background(), rowSecret("s"))
	if err == nil {
		t.Fatal("the runtime's read answered no error")
	}
	forms := map[string]string{"Error()": err.Error(), "%w": fmt.Errorf("launch: %w", err).Error()}
	for _, verb := range []string{"%s", "%v", "%+v", "%#v", "%q", "%x", "%X"} {
		forms[verb] = fmt.Sprintf(verb, err)
	}
	var jsonLine, textLine strings.Builder
	slog.New(slog.NewJSONHandler(&jsonLine, nil)).Error("refused", "error", err)
	slog.New(slog.NewTextHandler(&textLine, nil)).Error("refused", "error", err)
	forms["slog-json"], forms["slog-text"] = jsonLine.String(), textLine.String()
	encoded, _ := json.Marshal(err)
	forms["json"] = string(encoded)
	for i, unwrapped := 0, errors.Unwrap(err); unwrapped != nil; i, unwrapped = i+1, errors.Unwrap(unwrapped) {
		forms[fmt.Sprintf("unwrap-%d", i)] = fmt.Sprintf("%v %#v", unwrapped, unwrapped)
	}
	var status apierrors.APIStatus
	if !errors.As(err, &status) {
		t.Fatal("the runtime's error holds no APIStatus, which every Kubernetes decision reads")
	}
	statusJSON, _ := json.Marshal(status.Status())
	forms["As-APIStatus"] = string(statusJSON)
	var statusErr *apierrors.StatusError
	if errors.As(err, &statusErr) {
		original, _ := json.Marshal(statusErr.ErrStatus)
		forms["As-StatusError"] = statusErr.Error() + " " + string(original)
	}
	for form, text := range forms {
		oauth2calltest.RequireNone(t, "kubeerror.printers/"+form, text)
	}
	if got := status.Status(); got.Code != http.StatusForbidden || got.Reason != metav1.StatusReasonForbidden {
		t.Errorf("the runtime's error's status is %d %s, want the server's 403 Forbidden", got.Code, got.Reason)
	}
}

// TestAPodLogOrEventsReadPrintsNoneOfWhatItsRequestCarried: a read of a pod's log, as a death's
// detail quotes it (logTail, through readLogOnce's Stream) and as the image probe judges it
// (probeLog), and a read of its events (events, through podEvents' List), each refused with a
// Status that echoes the bearer the request carried. The text each gives, or its error, holds none
// of it. Captures kubeerror.reads/<case>.
func TestAPodLogOrEventsReadPrintsNoneOfWhatItsRequestCarried(t *testing.T) {
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "p", Namespace: testNamespace, UID: "uid-pod"}}
	for _, c := range []struct {
		name, path string
		read       func(ctx context.Context, r *Runtime) (string, error)
		raw        func(ctx context.Context, raw kubeClients) error
	}{
		{"log-tail", "/api/v1/namespaces/" + testNamespace + "/pods/p/log",
			func(ctx context.Context, r *Runtime) (string, error) {
				return r.logTail(ctx, "p", runtime.IssuePodContainer), nil
			},
			func(ctx context.Context, raw kubeClients) error {
				_, err := raw.kube.CoreV1().Pods(testNamespace).GetLogs("p", &corev1.PodLogOptions{}).Stream(ctx)
				return err
			}},
		{"probe-log", "/api/v1/namespaces/" + testNamespace + "/pods/p/log",
			func(ctx context.Context, r *Runtime) (string, error) { return r.probeLog(ctx, "p") },
			func(ctx context.Context, raw kubeClients) error {
				_, err := raw.kube.CoreV1().Pods(testNamespace).GetLogs("p", &corev1.PodLogOptions{}).Stream(ctx)
				return err
			}},
		{"events", "/api/v1/namespaces/" + testNamespace + "/events",
			func(ctx context.Context, r *Runtime) (string, error) { return r.events(ctx, pod), nil },
			func(ctx context.Context, raw kubeClients) error {
				_, err := raw.kube.CoreV1().Events(testNamespace).List(ctx, metav1.ListOptions{})
				return err
			}},
	} {
		t.Run(c.name, func(t *testing.T) {
			k := newKubeStandIn(t, func(w http.ResponseWriter, r *http.Request) bool {
				if r.URL.Path != c.path {
					return false
				}
				answerEcho(w, r, http.StatusForbidden, metav1.StatusReasonForbidden)
				return true
			})
			r, raw := requestRuntime(t, k.config(), slog.New(slog.DiscardHandler))
			requireRawLeaks(t, c.raw(context.Background(), raw))

			text, err := c.read(context.Background(), r)

			if err != nil {
				text += " " + err.Error()
			}
			k.requireSentBearer(t, kubeBearer)
			oauth2calltest.RequireNone(t, "kubeerror.reads/"+c.name, text)
			if !strings.Contains(text, "403") {
				t.Errorf("the read gave %q, want it to name the server's 403", text)
			}
		})
	}
}

// TestAResetPodLogPrintsOnlyTheTransportsOwnWords: a log stream whose connection is reset
// mid-body, after bytes that hold a made-up credential, fails its read (readLogOnce's tailBytes,
// which reads a body cut short as the log's end) with the network's own words for the reset,
// which hold nothing the stream sent: the death's quote (logTail) says the log could not be read
// and prints none of it. It prints as before this change, which projects no body read: such an
// error holds no server text (R4).
func TestAResetPodLogPrintsOnlyTheTransportsOwnWords(t *testing.T) {
	k := newKubeStandIn(t, func(w http.ResponseWriter, r *http.Request) bool {
		if r.URL.Path != "/api/v1/namespaces/"+testNamespace+"/pods/p/log" {
			return false
		}
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprintf(w, "roles: the request carried %s\n", r.Header.Get("Authorization"))
		w.(http.Flusher).Flush()
		conn, _, err := http.NewResponseController(w).Hijack()
		if err != nil {
			t.Errorf("hijack the log's connection: %v", err)
			return true
		}
		if tcp, ok := conn.(*net.TCPConn); ok {
			_ = tcp.SetLinger(0)
		}
		_ = conn.Close()
		return true
	})
	r, _ := requestRuntime(t, k.config(), slog.New(slog.DiscardHandler))

	text := r.logTail(context.Background(), "p", runtime.IssuePodContainer)

	if !strings.HasPrefix(text, "(the log could not be read: ") || !strings.HasSuffix(text, ": connection reset by peer)") {
		t.Errorf("the quote is %q, want the reset read named by the network's own words", text)
	}
	oauth2calltest.RequireNone(t, "kubeerror.reads/reset-log", text)
}

// TestTheInstallationChecksPrintNoneOfWhatTheirRequestsCarried: the boot's checks, each with
// clients of its own (CheckInstalled, CensusLegacyIssueSandboxes), read the Sandbox CRD and the
// controller's Deployment, and list the project's Sandboxes; the API server refuses each with a
// Status that echoes the bearer the request carried: the CRD 403 (forbidden: absent, or not this
// identity's), the Deployment 404 (not installed), the list 403. The boot refusal names each
// piece and the status as before, and holds none of the bearer. Captures kubeerror.install/<case>.
func TestTheInstallationChecksPrintNoneOfWhatTheirRequestsCarried(t *testing.T) {
	ref := InstallRef{CRD: "sandboxes.agents.x-k8s.io", ControllerNamespace: "agent-sandbox-system", ControllerName: "agent-sandbox-controller"}
	k := newKubeStandIn(t, func(w http.ResponseWriter, r *http.Request) bool {
		switch r.URL.Path {
		case "/apis/apiextensions.k8s.io/v1/customresourcedefinitions/" + ref.CRD,
			"/apis/" + sandboxGVR.Group + "/" + sandboxGVR.Version + "/namespaces/" + testNamespace + "/sandboxes":
			answerEcho(w, r, http.StatusForbidden, metav1.StatusReasonForbidden)
		case "/apis/apps/v1/namespaces/" + ref.ControllerNamespace + "/deployments/" + ref.ControllerName:
			answerEcho(w, r, http.StatusNotFound, metav1.StatusReasonNotFound)
		default:
			return false
		}
		return true
	})
	_, raw := requestRuntime(t, k.config(), slog.New(slog.DiscardHandler))
	_, rawErr := raw.kube.AppsV1().Deployments(ref.ControllerNamespace).Get(context.Background(), ref.ControllerName, metav1.GetOptions{})
	requireRawLeaks(t, rawErr)

	t.Run("installed", func(t *testing.T) {
		err := CheckInstalled(context.Background(), k.config(), ref)
		if err == nil {
			t.Fatal("CheckInstalled answered no refusal")
		}
		oauth2calltest.RequireNone(t, "kubeerror.install/installed", err.Error())
		for _, want := range []string{"the Sandbox CRD " + ref.CRD + " cannot be read (403)", "agent-sandbox-controller is not installed (404)"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("the refusal is %q, want it to name %q", err, want)
			}
		}
	})
	t.Run("census", func(t *testing.T) {
		err := CensusLegacyIssueSandboxes(context.Background(), k.config(), testNamespace, testProject, IssueDisks{}, slog.New(slog.DiscardHandler))
		if err == nil {
			t.Fatal("the census answered no refusal")
		}
		oauth2calltest.RequireNone(t, "kubeerror.install/census", err.Error())
		if !strings.Contains(err.Error(), "403") {
			t.Errorf("the refusal is %q, want it to name the server's 403", err)
		}
	})
	k.requireSentBearer(t, kubeBearer)
}

// TestTheImageProbesRefusalAndDetailPrintNoneOfWhatItsRequestCarried: the image probe's create of
// its Sandbox (createProbe) refused with a Status that echoes the bearer: a 403 is a definitive
// verdict on the image (apiOutcome, imageRefusal), a 500 a transient one whose detail the boot's
// retries print (Outcome.Detail). Neither holds the bearer. Captures kubeerror.probe/<case>.
func TestTheImageProbesRefusalAndDetailPrintNoneOfWhatItsRequestCarried(t *testing.T) {
	for _, c := range []struct {
		name   string
		code   int
		reason metav1.StatusReason
	}{{"refusal", http.StatusForbidden, metav1.StatusReasonForbidden}, {"detail", http.StatusInternalServerError, metav1.StatusReasonInternalError}} {
		t.Run(c.name, func(t *testing.T) {
			k := newKubeStandIn(t, func(w http.ResponseWriter, r *http.Request) bool {
				if r.Method != http.MethodPost {
					return false
				}
				answerEcho(w, r, c.code, c.reason)
				return true
			})
			r, _ := requestRuntime(t, k.config(), slog.New(slog.DiscardHandler))
			manifest := func() (*unstructured.Unstructured, error) {
				return &unstructured.Unstructured{Object: map[string]any{"apiVersion": sandboxGVR.Group + "/" + sandboxGVR.Version,
					"kind": "Sandbox", "metadata": map[string]any{"name": "probe"}}}, nil
			}

			_, outcome, created := r.createProbe(context.Background(), "probe", "sha256:abc", manifest, time.Second)

			if created {
				t.Fatal("the probe's create was made, want it refused")
			}
			text := outcome.Detail
			if outcome.Refusal != nil {
				text += " " + outcome.Refusal.Error()
			}
			if (c.name == "refusal") != (outcome.Refusal != nil) || text == "" {
				t.Fatalf("the outcome is %+v, want a %s", outcome, c.name)
			}
			oauth2calltest.RequireNone(t, "kubeerror.probe/"+c.name, text)
		})
	}
}

// TestATransportFailureOfAQueryBearingRequestPrintsNoQuery: a request that reaches no server fails
// with net/http's *url.Error, which quotes the request's URL, query and all. The runtime's
// query-bearing requests carry a made-up credential in a query value here: the events' field
// selector its pod's uid (events), the log's options its container (log-tail), the informers' label
// selector its project (informer-list, informer-watch). The runtime's text for each names the
// failure's class and holds none of the query. Captures kubeerror.transport/<case>.
func TestATransportFailureOfAQueryBearingRequestPrintsNoQuery(t *testing.T) {
	dead := httptest.NewServer(http.NotFoundHandler())
	dead.Close()
	rc := &rest.Config{Host: dead.URL, ContentConfig: rest.ContentConfig{ContentType: "application/json"}}
	t.Run("events", func(t *testing.T) {
		r, raw := requestRuntime(t, rc, slog.New(slog.DiscardHandler))
		pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "p", Namespace: testNamespace, UID: oauth2calltest.Token}}
		_, rawErr := raw.kube.CoreV1().Events(testNamespace).List(context.Background(), metav1.ListOptions{FieldSelector: "involvedObject.uid=" + oauth2calltest.Token})
		requireRawLeaks(t, rawErr)
		text := r.events(context.Background(), pod)
		oauth2calltest.RequireNone(t, "kubeerror.transport/events", text)
		requireConnectionRefused(t, text)
	})
	t.Run("log-tail", func(t *testing.T) {
		r, raw := requestRuntime(t, rc, slog.New(slog.DiscardHandler))
		_, rawErr := raw.kube.CoreV1().Pods(testNamespace).GetLogs("p", &corev1.PodLogOptions{Container: oauth2calltest.Token}).Stream(context.Background())
		requireRawLeaks(t, rawErr)
		text := r.logTail(context.Background(), "p", oauth2calltest.Token)
		oauth2calltest.RequireNone(t, "kubeerror.transport/log-tail", text)
		requireConnectionRefused(t, text)
	})
	// The informers' rows: a watch-list is answered 400, so the informers list and then watch, and
	// the project, which the Sandbox and pod informers select on, is the made-up credential.
	informerRuntime := func(t *testing.T, refused func(*http.Request) bool, watch func(w http.ResponseWriter, r *http.Request)) (*Runtime, error) {
		k := newKubeStandIn(t, func(w http.ResponseWriter, r *http.Request) bool {
			switch {
			case r.URL.Query().Get("sendInitialEvents") == "true":
				kubeStatusAnswer(w, http.StatusBadRequest, metav1.StatusReasonBadRequest, "")
			case r.URL.Query().Get("watch") == "true" && watch != nil:
				watch(w, r)
			default:
				return false
			}
			return true
		})
		rc := k.config()
		rc.WrapTransport = refusingTransport(refused)
		opts := testOptions()
		opts.Project = oauth2calltest.Token
		// The informers run for the context New is given, so it is the subtest's.
		ctx, stop := context.WithCancel(context.Background())
		t.Cleanup(stop)
		return New(ctx, rc, opts)
	}
	t.Run("informer-list", func(t *testing.T) {
		_, err := informerRuntime(t, func(r *http.Request) bool { return r.URL.Query().Get("watch") != "true" }, nil)
		if err == nil {
			t.Fatal("New built a runtime though every list its informers made failed")
		}
		oauth2calltest.RequireNone(t, "kubeerror.transport/informer-list", err.Error())
		requireConnectionRefused(t, err.Error())
	})
	t.Run("informer-watch", func(t *testing.T) {
		// refusing closes once New returned: the watch each informer holds then ends, and from then
		// on the transport refuses every watch, as a closed port refuses its dial.
		refusing := make(chan struct{})
		refused := func(r *http.Request) bool {
			select {
			case <-refusing:
				return r.URL.Query().Get("watch") == "true"
			default:
				return false
			}
		}
		r, err := informerRuntime(t, refused, func(w http.ResponseWriter, r *http.Request) {
			startWatch(w)
			select {
			case <-refusing:
			case <-r.Context().Done():
			}
		})
		if err != nil {
			t.Fatalf("New refused a runtime whose lists and watches answered: %v", err)
		}
		close(refusing)
		eventually(t, "the pod feed to fail", func() bool { return r.podFeed.check() != nil })
		text := r.podFeed.check().Error()
		oauth2calltest.RequireNone(t, "kubeerror.transport/informer-watch", text)
		requireConnectionRefused(t, text)
	})
}

// refusingTransport is a client's transport that refuses each request refused selects as a closed
// port refuses its dial, and sends every other.
func refusingTransport(refused func(*http.Request) bool) func(http.RoundTripper) http.RoundTripper {
	return func(next http.RoundTripper) http.RoundTripper {
		return roundTripper(func(r *http.Request) (*http.Response, error) {
			if refused(r) {
				return nil, &net.OpError{Op: "dial", Net: "tcp", Err: os.NewSyscallError("connect", syscall.ECONNREFUSED)}
			}
			return next.RoundTrip(r)
		})
	}
}

type roundTripper func(*http.Request) (*http.Response, error)

func (f roundTripper) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// requireConnectionRefused fails t unless text names the refused connection, as net/http's error
// names it: a projection that dropped the class would say nothing an operator can act on.
func requireConnectionRefused(t *testing.T, text string) {
	t.Helper()
	if !strings.Contains(text, "connection refused") {
		t.Errorf("the text is %q, want it to name the refused connection", text)
	}
}

// TestASetupTokenFileFailurePrintsItsPathAndNoCredential: a client whose bearer file cannot be read
// is refused at its construction (clientsOf: client-go reads the file before any request), naming
// the file, as before this change: the error holds no server text and no bearer, only the path the
// configuration gave and the file system's words (R4).
func TestASetupTokenFileFailurePrintsItsPathAndNoCredential(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "token")

	_, err := clientsOf(requestConfig(&rest.Config{Host: "https://192.0.2.1", BearerTokenFile: missing}))

	if err == nil {
		t.Fatal("clientsOf built clients over a bearer file that does not exist")
	}
	want := fmt.Sprintf(`sandbox runtime: dynamic client: failed to read token file %q: open %s: no such file or directory`, missing, missing)
	if err.Error() != want {
		t.Errorf("the refusal is %q, want %q", err, want)
	}
	oauth2calltest.RequireNone(t, "kubeerror.setup/token-file", err.Error())
}
