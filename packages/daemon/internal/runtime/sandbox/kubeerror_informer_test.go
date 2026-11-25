package sandbox

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/watch"

	"github.com/sjawhar/envoy/oauth2call/oauth2calltest"
	"github.com/sjawhar/legion/daemon/internal/runtime"
)

// podsPath is the path the pod informer lists and watches.
const podsPath = "/api/v1/namespaces/" + testNamespace + "/pods"

// newRuntimeOver is New over the stand-in k, which must sync its informers, ending with t.
func newRuntimeOver(t *testing.T, k *kubeStandIn) *Runtime {
	t.Helper()
	ctx, stop := context.WithCancel(context.Background())
	t.Cleanup(stop)
	opts := testOptions()
	opts.Log = slog.New(slog.DiscardHandler)
	r, err := New(ctx, k.config(), opts)
	if err != nil {
		t.Fatalf("New refused a runtime whose stand-in answers its informers: %v", err)
	}
	return r
}

// TestAnInformerRefusalPrintsNoneOfWhatItsRequestCarried: the runtime's informers list and watch
// through its callbacks (informer), whose outcome its feeds record, and client-go's reflector logs
// what they answer. The API server refuses with a Status that echoes the bearer the request
// carried: every list and watch of the pods before the stores synced, which refuses the boot
// (before-sync); the pods' watch once they synced, which the feed holds and an observation reads
// (watch-after-sync), and the Services' watch, which a repository cache's status reads
// (services-after-sync); and an error a watch delivers once it has opened, a Status on the typed
// pod watch (watch-event-status), a Status the dynamic Sandbox watch decodes as unstructured
// (watch-event-unstructured), and an object that is no Status on the Service watch
// (watch-event-object). Neither the runtime's text nor any line client-go logged holds the bearer.
// Captures kubeerror.informer/<case>.
func TestAnInformerRefusalPrintsNoneOfWhatItsRequestCarried(t *testing.T) {
	t.Run("before-sync", func(t *testing.T) {
		logged := klogCapture(t, "0")
		k := newKubeStandIn(t, func(w http.ResponseWriter, r *http.Request) bool {
			if r.URL.Path != podsPath {
				return false
			}
			answerEcho(w, r, http.StatusForbidden, metav1.StatusReasonForbidden)
			return true
		})
		_, raw := requestRuntime(t, k.config(), slog.New(slog.DiscardHandler))
		_, rawErr := raw.kube.CoreV1().Pods(testNamespace).List(context.Background(), metav1.ListOptions{})
		requireRawLeaks(t, rawErr)
		ctx, stop := context.WithCancel(context.Background())
		defer stop()
		opts := testOptions()
		opts.Log = slog.New(slog.DiscardHandler)

		_, err := New(ctx, k.config(), opts)

		if err == nil {
			t.Fatal("New built a runtime though every list and watch of its pods was refused")
		}
		eventually(t, "the reflector to log the failed list", func() bool { return strings.Contains(logged.String(), "Failed to watch") })
		oauth2calltest.RequireNone(t, "kubeerror.informer/before-sync", err.Error())
		requireNoneLogged(t, "kubeerror.informer/before-sync-log", logged, "Failed to watch")
	})
	for _, c := range []struct{ name, path string }{
		{"watch-after-sync", podsPath},
		{"services-after-sync", "/api/v1/namespaces/" + testNamespace + "/services"},
	} {
		t.Run(c.name, func(t *testing.T) {
			logged := klogCapture(t, "0")
			// refusing closes once New returned: from then on every watch of the path is refused,
			// and one held before ends, so the reflector watches again into the refusal.
			refusing := make(chan struct{})
			k := newKubeStandIn(t, func(w http.ResponseWriter, r *http.Request) bool {
				if r.URL.Path != c.path || r.URL.Query().Get("watch") != "true" {
					return false
				}
				kind, _ := kubeListKind(r.URL.Path)
				select {
				case <-refusing:
					answerEcho(w, r, http.StatusForbidden, metav1.StatusReasonForbidden)
					return true
				default:
				}
				if r.URL.Query().Get("sendInitialEvents") == "true" {
					endWatchList(w, kind)
					return true
				}
				startWatch(w)
				select {
				case <-refusing:
				case <-r.Context().Done():
				}
				return true
			})
			r := newRuntimeOver(t, k)
			close(refusing)
			var text string
			if c.path == podsPath {
				eventually(t, "the pod feed to fail", func() bool { return r.podFeed.check() != nil })
				_, err := r.view("s")
				observed := r.evaluate(context.Background(), runtime.Locator{Sandbox: &runtime.SandboxLocator{Name: "s"}})
				text = err.Error() + "\n" + observed.Detail
			} else {
				eventually(t, "the Service feed to fail", func() bool { return r.serviceFeed.check() != nil })
				refusal, _ := r.cacheServiceRefusal("s", "uid-s")
				text = refusal
			}
			eventually(t, "the reflector to log the failed watch", func() bool { return strings.Contains(logged.String(), "Failed to watch") })
			k.requireSentBearer(t, kubeBearer)
			oauth2calltest.RequireNone(t, "kubeerror.informer/"+c.name, text)
			if !strings.Contains(text, "403") {
				t.Errorf("the runtime's text is %q, want it to name the server's 403", text)
			}
			requireNoneLogged(t, "kubeerror.informer/"+c.name+"-log", logged, "Failed to watch")
		})
	}
	for _, c := range []struct {
		name, path string
		// event is the watch's error event's object, made of the request it answers.
		event func(r *http.Request) any
	}{
		{"watch-event-status", podsPath, func(r *http.Request) any { return echoStatus(r, http.StatusForbidden, metav1.StatusReasonForbidden) }},
		{"watch-event-unstructured", "/apis/" + sandboxGVR.Group + "/" + sandboxGVR.Version + "/namespaces/" + testNamespace + "/sandboxes",
			func(r *http.Request) any { return echoStatus(r, http.StatusForbidden, metav1.StatusReasonForbidden) }},
		{"watch-event-object", "/api/v1/namespaces/" + testNamespace + "/services", func(r *http.Request) any {
			return map[string]any{"apiVersion": "v1", "kind": "Pod", "metadata": map[string]any{"name": r.Header.Get("Authorization")}}
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			logged := klogCapture(t, "0")
			var opened atomic.Int64
			k := newKubeStandIn(t, func(w http.ResponseWriter, r *http.Request) bool {
				if r.URL.Path != c.path || r.URL.Query().Get("sendInitialEvents") != "true" || opened.Add(1) != 1 {
					return false
				}
				kind, _ := kubeListKind(r.URL.Path)
				startWatch(w)
				kubeWatchEvent(w, "BOOKMARK", initialEventsEnd(kind))
				kubeWatchEvent(w, "ERROR", c.event(r))
				return true
			})
			newRuntimeOver(t, k)

			eventually(t, "the reflector to log the watch's error", func() bool {
				return strings.Contains(logged.String(), "watch ended with error")
			})

			k.requireSentBearer(t, kubeBearer)
			requireNoneLogged(t, "kubeerror.informer/"+c.name, logged, "watch ended with error")
		})
	}
}

// podInformer is the runtime's pod informer (informer) over the stand-in k through rc, its
// callbacks recording into a feed of its own, run until t ends.
func podInformer(t *testing.T, k *kubeStandIn, wrap func(http.RoundTripper) http.RoundTripper) *feed {
	t.Helper()
	rc := k.config()
	rc.WrapTransport = wrap
	informers, _ := clientConfigs(rc)
	c, err := clientsOf(informers)
	if err != nil {
		t.Fatal(err)
	}
	r := &Runtime{namespace: testNamespace, now: time.Now}
	pods := c.kube.CoreV1().Pods(testNamespace)
	f := &feed{resource: "pod"}
	informer := r.informer(f, c.kube, &corev1.Pod{}, labelProject+"="+testProject,
		func(ctx context.Context, o metav1.ListOptions) (k8sruntime.Object, error) { return pods.List(ctx, o) },
		func(ctx context.Context, o metav1.ListOptions) (watch.Interface, error) { return pods.Watch(ctx, o) })
	ctx, stop := context.WithCancel(context.Background())
	t.Cleanup(stop)
	go informer.RunWithContext(ctx)
	return f
}

// podRequests are the requests of the pod informer k was sent, each a list, a watch-list (a watch
// asking for its initial events) or a watch.
func podRequests(k *kubeStandIn) (lists, watchLists, watches []kubeRequest) {
	for _, r := range k.asked() {
		switch {
		case r.path != podsPath:
		case r.watchList():
			watchLists = append(watchLists, r)
		case r.watch():
			watches = append(watches, r)
		default:
			lists = append(lists, r)
		}
	}
	return lists, watchLists, watches
}

// TestTheReflectorsDecisionsOnAProjectedErrorAreTheBases: client-go's reflector decides how to go
// on from the error the runtime's informer callbacks return (informer), and from a watch's error
// event, by its status and its class, which the projection keeps:
//   - a watch whose connection is refused is backed off and watched again, with no list
//     (isWatchErrorRetriable reads the refused connection's errno through errors.As) (refused-watch);
//   - a list refused for a resource version past the server's is listed again at once from no
//     resource version (isTooLargeResourceVersionError reads the Timeout status's
//     ResourceVersionTooLarge cause) (too-large);
//   - a watch-list whose stream says its resource version has expired is asked again from no
//     resource version, with no list (isExpiredError reads the event's Expired reason) (expired);
//   - a watch the server answers 429 is backed off and watched again, with no list or watch-list
//     (IsTooManyRequests reads its reason) (too-many-requests).
//
// Each passes as it did before this change, and fails against a projection that drops the status
// or the class it reads.
func TestTheReflectorsDecisionsOnAProjectedErrorAreTheBases(t *testing.T) {
	t.Run("refused-watch", func(t *testing.T) {
		var refused atomic.Int64
		k := newKubeStandIn(t, nil)
		podInformer(t, k, refusingTransport(func(r *http.Request) bool {
			if r.URL.Path == podsPath && r.URL.Query().Get("watch") == "true" {
				refused.Add(1)
				return true
			}
			return false
		}))
		eventually(t, "the refused watch to be sent again", func() bool { return refused.Load() >= 2 })
		if lists, _, _ := podRequests(k); len(lists) != 0 {
			t.Fatalf("the informer listed its pods %d times after a refused watch, want it to back off and watch again", len(lists))
		}
	})
	t.Run("too-large", func(t *testing.T) {
		var listed atomic.Int64
		k := newKubeStandIn(t, func(w http.ResponseWriter, r *http.Request) bool {
			switch {
			case r.URL.Path != podsPath:
				return false
			case r.URL.Query().Get("sendInitialEvents") == "true":
				kubeStatusAnswer(w, http.StatusBadRequest, metav1.StatusReasonBadRequest, "")
			case r.URL.Query().Get("watch") != "true" && listed.Add(1) == 1:
				kubeObjectAnswer(w, http.StatusGatewayTimeout, metav1.Status{TypeMeta: metav1.TypeMeta{Kind: "Status", APIVersion: "v1"},
					Status: metav1.StatusFailure, Code: http.StatusGatewayTimeout, Reason: metav1.StatusReasonTimeout,
					Message: "Too large resource version", Details: &metav1.StatusDetails{Causes: []metav1.StatusCause{{
						Type: metav1.CauseTypeResourceVersionTooLarge, Message: "Too large resource version"}}}})
			default:
				return false
			}
			return true
		})
		podInformer(t, k, nil)
		eventually(t, "a second list", func() bool { return listed.Load() >= 2 })
		lists, _, _ := podRequests(k)
		if first, second := lists[0].get("resourceVersion"), lists[1].get("resourceVersion"); first != "0" || second != "" {
			t.Fatalf("the informer listed at resource versions %q then %q, want %q then none: a too-large one is listed again from none at once", first, second, "0")
		}
	})
	t.Run("expired", func(t *testing.T) {
		var opened atomic.Int64
		k := newKubeStandIn(t, func(w http.ResponseWriter, r *http.Request) bool {
			if r.URL.Path != podsPath || r.URL.Query().Get("sendInitialEvents") != "true" || opened.Add(1) != 1 {
				return false
			}
			startWatch(w)
			kubeWatchEvent(w, "ERROR", metav1.Status{TypeMeta: metav1.TypeMeta{Kind: "Status", APIVersion: "v1"},
				Status: metav1.StatusFailure, Code: http.StatusGone, Reason: metav1.StatusReasonExpired, Message: "too old resource version"})
			return true
		})
		podInformer(t, k, nil)
		eventually(t, "a second watch-list", func() bool { _, watchLists, _ := podRequests(k); return len(watchLists) >= 2 })
		if lists, watchLists, _ := podRequests(k); len(lists) != 0 || watchLists[1].get("resourceVersion") != "" {
			t.Fatalf("after an expired watch-list the informer listed %d times and asked again at %q, want no list and a watch-list from no resource version",
				len(lists), watchLists[1].get("resourceVersion"))
		}
	})
	t.Run("too-many-requests", func(t *testing.T) {
		var watched atomic.Int64
		k := newKubeStandIn(t, func(w http.ResponseWriter, r *http.Request) bool {
			switch {
			case r.URL.Path != podsPath || r.URL.Query().Get("watch") != "true":
				return false
			case r.URL.Query().Get("sendInitialEvents") == "true":
				endWatchList(w, kubeKind{"v1", "Pod"})
			case watched.Add(1) == 1:
				kubeStatusAnswer(w, http.StatusTooManyRequests, metav1.StatusReasonTooManyRequests, "")
			default:
				return false
			}
			return true
		})
		podInformer(t, k, nil)
		eventually(t, "a second watch", func() bool { return watched.Load() >= 2 })
		if lists, watchLists, _ := podRequests(k); len(lists) != 0 || len(watchLists) != 1 {
			t.Fatalf("after a 429 the informer listed %d times and asked %d watch-lists, want it to back off and watch again with neither", len(lists), len(watchLists))
		}
	})
}

// TestASlowFailedListsTraceLinePrintsNoneOfWhatItCarried: client-go's reflector traces its list
// and logs the trace, the list's error formatted into it, when the list took over 10 s
// (reflector.go's initTrace.LogIfLong, k8s.io/utils/trace). The trace prints only at klog
// verbosity 2, which no daemon sets, so this row sets it. A list the API server answers after
// 10 s with a Status that echoes the bearer the request carried: the trace line holds none of it,
// since the runtime's list callback (informer) hands the reflector its error already projected.
// Captures kubeerror.informer/list-trace.
func TestASlowFailedListsTraceLinePrintsNoneOfWhatItCarried(t *testing.T) {
	logged := klogCapture(t, "2")
	k := newKubeStandIn(t, func(w http.ResponseWriter, r *http.Request) bool {
		switch {
		case r.URL.Path != podsPath:
			return false
		case r.URL.Query().Get("sendInitialEvents") == "true":
			kubeStatusAnswer(w, http.StatusBadRequest, metav1.StatusReasonBadRequest, "")
		case r.URL.Query().Get("watch") != "true":
			select {
			case <-time.After(10*time.Second + 500*time.Millisecond):
			case <-r.Context().Done():
				return true
			}
			answerEcho(w, r, http.StatusForbidden, metav1.StatusReasonForbidden)
		default:
			return false
		}
		return true
	})
	podInformer(t, k, nil)
	eventually(t, "the reflector's trace of the slow list", func() bool {
		return strings.Contains(logged.String(), "Objects listed")
	})
	k.requireSentBearer(t, kubeBearer)
	requireNoneLogged(t, "kubeerror.informer/list-trace", logged, "Objects listed")
}

// TestACanceledOrTimedOutRequestIsStillTheContextsError: a request whose context was canceled, or
// whose deadline passed, is the context's error to its caller, through errors.Is, as before this
// change: the runtime's waits tell a stop from a failure by it.
func TestACanceledOrTimedOutRequestIsStillTheContextsError(t *testing.T) {
	k := newKubeStandIn(t, nil)
	r, _ := requestRuntime(t, k.config(), slog.New(slog.DiscardHandler))
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if err := r.writeSecret(canceled, rowSecret("s")); !errors.Is(err, context.Canceled) {
		t.Errorf("a canceled write answered %v, want context.Canceled", err)
	}
	expired, cancelExpired := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancelExpired()
	if err := r.writeSecret(expired, rowSecret("s")); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("a write past its deadline answered %v, want context.DeadlineExceeded", err)
	}
}
