package main

import (
	"bytes"
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/sjawhar/envoy/oauth2call/oauth2calltest"
	"github.com/sjawhar/legion/daemon/internal/bootprobe"
	"github.com/sjawhar/legion/daemon/internal/testlog"
)

// The cluster check passes, and every attempt of the image probe's create of its Sandbox is
// answered 500 InternalError with a Status that echoes the bearer. Each attempt is transient
// (createProbe's apiOutcome), so bootprobe.Run logs its retry line on stderr after each of the
// first five, through the logger probe-boot builds (redact.HandlerOptions), with the attempt's
// detail; after the sixth the attempts have run out, and probe-boot prints the probe's refusal,
// which carries the last attempt's detail (printRefusal). The runtime keeps the create's error as
// its projection (resend's projectKubeError) before any of them holds it: none holds the bearer.
// The row shortens the probe's waits (probeBootRetry) to the policy's six attempts over 1 ms
// doubling to 4 ms, where the daemon's own waits about five minutes. It names a seam the base has
// not, so it is a mutation row: resend's projection undone, the refusal prints the bearer.
func TestProbeBootPrintsAnExhaustedImageProbeWithNoCredential(t *testing.T) {
	legionState(t)
	retry := probeBootRetry
	probeBootRetry = bootprobe.Retry{Initial: time.Millisecond, Max: 4 * time.Millisecond, Attempts: bootprobe.Image.Attempts}
	t.Cleanup(func() { probeBootRetry = retry })
	api := newBootKubeAPI(t, map[string]bootEcho{"POST " + bootSandboxesPath: {http.StatusInternalServerError, metav1.StatusReasonInternalError}})
	config := kubernetesCheckConfig(t, "")
	kubeconfig := bootKubeconfig(t, config, api)
	_, err := bootRawClient(t, kubeconfig).Resource(bootSandboxResource).Namespace("legion").
		Create(context.Background(), bootControlSandbox(), metav1.CreateOptions{})
	requireEchoed(t, err)

	var out bytes.Buffer
	var errb testlog.Buffer
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	code := run(ctx, []string{"legion", "probe-boot", "--config", config}, &out, &errb)

	stderr := errb.String()
	oauth2calltest.RequireNone(t, "kubeerror.legion/boot-exhausted", stderr)
	attempts := bootprobe.Image.Attempts
	api.requireSentBearer(t, "POST "+bootSandboxesPath, 1+attempts)
	if retries, details := strings.Count(stderr, bootRetryLine), strings.Count(stderr, bootRetryDetail); code != 1 || out.Len() != 0 ||
		retries != attempts-1 || details != attempts-1 || !strings.Contains(stderr, bootExhausted) {
		t.Fatalf("exit code = %d, stdout %q, %d retry lines with %d create details, stderr:\n%s\nwant 1, no OK line, %d retry lines each with the create's detail, and the exhausted probe's refusal %q",
			code, out.String(), retries, details, stderr, attempts-1, bootExhausted)
	}
	api.requireServed(t)
}
