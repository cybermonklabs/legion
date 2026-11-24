package sandbox

import (
	"context"
	"log/slog"
	"net/http"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/klog/v2"

	"github.com/sjawhar/envoy/oauth2call/oauth2calltest"
	"github.com/sjawhar/legion/daemon/internal/testlog"
)

// TestClientGosOwnLinesPrintNoneOfAKubernetesErrorsWords: the daemon routes client-go's own
// logging through its logger (RouteClientLogs, as cmd/legion's start and probe-boot do), and
// client-go logs an error that reached it whole: apimachinery's error handler
// (utilruntime.HandleErrorWithContext, which the reflector and the rest of client-go log through)
// handed client-go's own error for a refusal whose Status echoes the bearer the request carried.
// The line client-go logs names the error as the runtime prints it and holds none of the bearer.
// Captures kubeerror.client-go-log.
func TestClientGosOwnLinesPrintNoneOfAKubernetesErrorsWords(t *testing.T) {
	k := newKubeStandIn(t, func(w http.ResponseWriter, r *http.Request) bool {
		if r.URL.Path != secretPath("s") {
			return false
		}
		answerEcho(w, r, http.StatusForbidden, metav1.StatusReasonForbidden)
		return true
	})
	_, raw := requestRuntime(t, k.config(), slog.New(slog.DiscardHandler))
	_, rawErr := raw.kube.CoreV1().Secrets(testNamespace).Get(context.Background(), "s", metav1.GetOptions{})
	requireRawLeaks(t, rawErr)
	logged := &testlog.Buffer{}
	RouteClientLogs(slog.New(slog.NewJSONHandler(logged, nil)))
	t.Cleanup(klog.ClearLogger)

	utilruntime.HandleErrorWithContext(context.Background(), rawErr, "Failed to watch", "reflector", "pods")

	klog.Flush()
	line := logged.String()
	if !strings.Contains(line, "Failed to watch") {
		t.Fatalf("client-go logged %q, want its line", line)
	}
	oauth2calltest.RequireNone(t, "kubeerror.client-go-log", line)
	if !strings.Contains(line, "kubernetes: client-go: the API server answered 403 Forbidden") {
		t.Errorf("client-go logged %q, want the error as the runtime prints it", line)
	}
}
