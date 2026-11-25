package main

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/metrics"
)

// hubStartupProbe is the exec command of the startup probe of the session listener's pod that
// gcp:ClusteredNatsHub renders, read out of that pod's golden in listenerPodGoldens as a
// listenerPod, and the path that names the golden from the repository root, for a failure read in
// CI. The command is a shell's -c of one script, which is what the rendering is required to hold.
func hubStartupProbe(t *testing.T) ([]string, string) {
	t.Helper()
	var command []string
	path, found := "", 0
	for _, golden := range listenerPodGoldens {
		if filepath.Base(golden.read) != "clustered-hub-listener-pod.golden" {
			continue
		}
		found++
		path = golden.path
		pod := readListenerPod(t, golden.read, path)
		for _, c := range pod.Spec.Containers {
			if c.Name != golden.container {
				continue
			}
			if c.StartupProbe == nil || c.StartupProbe.Exec == nil {
				t.Fatalf("container %s in %s has no startupProbe.exec; this test runs the probe's command", c.Name, path)
			}
			command = c.StartupProbe.Exec.Command
		}
		if command == nil {
			t.Fatalf("%s has no container named %s with a startup probe command", path, golden.container)
		}
	}
	if found != 1 {
		t.Fatalf("listenerPodGoldens lists %d entries for clustered-hub-listener-pod.golden, want 1", found)
	}
	if len(command) != 3 || command[1] != "-c" {
		t.Fatalf("the startup probe of %s is %q, not a shell's -c of one script; this test runs that script", path, command)
	}
	return command, path
}

// metricsScrape is the text handler answers GET /metrics with, failing the test on any status
// but 200.
func metricsScrape(t *testing.T, handler http.Handler) string {
	t.Helper()
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("GET /metrics answered %d: %s", recorder.Code, recorder.Body.String())
	}
	return recorder.Body.String()
}

// runHubStartupProbe runs command, the hub pod's startup probe as its golden records it, with a
// fake curl first on PATH (testdata/startup-probe/curl) that records its arguments, prints scrape
// and exits 0, so the script's status is its grep's verdict on scrape. It returns that status and
// the fake's arguments; a command that does not start fails the test. The fake is checked in rather
// than written here: a test that writes an executable and runs it can meet ETXTBSY when another
// test forks while the write's descriptor is open (Go issue 22315).
func runHubStartupProbe(t *testing.T, command []string, scrape string) (int, string) {
	t.Helper()
	bin, err := filepath.Abs(filepath.Join("testdata", "startup-probe"))
	if err != nil {
		t.Fatalf("find the fake curl's directory: %v", err)
	}
	dir := t.TempDir()
	scrapeFile, argsFile := filepath.Join(dir, "scrape"), filepath.Join(dir, "curl-args")
	if err := os.WriteFile(scrapeFile, []byte(scrape), 0o600); err != nil {
		t.Fatalf("write the scrape: %v", err)
	}
	cmd := exec.Command(command[0], command[1:]...)
	cmd.Env = []string{"PATH=" + bin + string(os.PathListSeparator) + os.Getenv("PATH"), "SCRAPE=" + scrapeFile, "CURL_ARGS=" + argsFile}
	var stderr strings.Builder
	cmd.Stderr = &stderr
	status := 0
	if err := cmd.Run(); err != nil {
		var exit *exec.ExitError
		if !errors.As(err, &exit) {
			t.Fatalf("run the startup probe %q: %v", command, err)
		}
		status = exit.ExitCode()
	}
	args, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatalf("the startup probe never ran curl (stderr %q): %v", stderr.String(), err)
	}
	return status, strings.TrimSpace(string(args))
}

// TestHubListenerStartupProbePassesOnlyOnceV1Serves ties the startup probe gcp:ClusteredNatsHub
// renders for this command's pod to this listener: the probe's script, read out of the pod's
// golden, is run on this listener's own /metrics scrapes, which a fake curl prints. Before
// openV1Routes has published the stores (served nil), and with /v1 open while the interest copy's
// first read is held, the scrape carries envoy_active_interests' HELP and TYPE lines and no
// sample, and the script exits 1, grep's status when it selects no line. Once the read is released
// and GET /v1/interests/ answers 200, the scrape carries the sample and the script exits 0. The
// infra module is a Go module of its own and cannot call registerListGauges, so this test is what
// holds the gauge name its component writes into the script (clusteredHubListenerServedGauge in
// packages/envoy/infra/gcp/clusteredhub_listener.go) to the one registerListGauges registers, and
// a rename on either side fails it.
func TestHubListenerStartupProbePassesOnlyOnceV1Serves(t *testing.T) {
	t.Parallel()
	command, path := hubStartupProbe(t)
	const gauge = "envoy_active_interests"
	run := func(t *testing.T, state, scrape string, want int) {
		t.Helper()
		status, args := runHubStartupProbe(t, command, scrape)
		if !strings.Contains(args, "http://127.0.0.1:9020/metrics") {
			t.Fatalf("the startup probe of %s ran curl %s, which does not ask the listener's /metrics on its port", path, args)
		}
		if status != want {
			t.Fatalf("the startup probe of %s exits %d %s, want %d; the scrape:\n%s", path, status, state, want, scrape)
		}
		t.Logf("the startup probe of %s exits %d %s", path, status, state)
	}

	met := metrics.New()
	var served atomic.Pointer[listenerDeps]
	registerListGauges(met, &served)
	if value, ok := gaugeSample(t, met.Handler(), gauge); ok {
		t.Fatalf("/metrics has %s %d before openV1Routes, want no sample", gauge, value)
	}
	run(t, "before /v1 opens", metricsScrape(t, met.Handler()), 1)

	interestHeld, releaseInterest := newRelease(t)
	rig := newListRig(t, listRigOptions{release: map[string]<-chan struct{}{"interest": interestHeld}})
	assertListRefilling(t, rig.get("/v1/interests/"))
	rig.assertNoSample(t, gauge)
	run(t, "while the interest copy's first read is held", metricsScrape(t, rig.handler), 1)

	releaseInterest()
	waitFor(t, 5*time.Second, "GET /v1/interests/ to answer 200 once the interest copy's read completed", func() bool {
		return rig.get("/v1/interests/").StatusCode == http.StatusOK
	})
	rig.assertSample(t, gauge, 0)
	run(t, "once /v1 serves the interest list", metricsScrape(t, rig.handler), 0)
}
