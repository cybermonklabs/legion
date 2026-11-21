package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"
	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/cmdtest"
	"github.com/sjawhar/envoy/internal/testnats"
	"github.com/sjawhar/envoy/listenerwire"
)

// hubListenerLoopback is the address the probes of the session listener's pod that
// gcp:ClusteredNatsHub renders ask: the container's own loopback, on the listener's port.
const hubListenerLoopback = "http://127.0.0.1:9020"

// hubProbes is the three probes of that pod, read out of its golden in listenerPodGoldens and
// keyed by their field names, and the path that names the golden from the repository root, for a
// failure read in CI. Each is required to be an exec, which is what the rendering holds and what
// these tests run.
func hubProbes(t *testing.T) (map[string]listenerProbe, string) {
	t.Helper()
	var probes map[string]listenerProbe
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
			probes = map[string]listenerProbe{}
			for name, probe := range map[string]*listenerProbe{"startupProbe": c.StartupProbe, "livenessProbe": c.LivenessProbe, "readinessProbe": c.ReadinessProbe} {
				if probe == nil || probe.Exec == nil || len(probe.Exec.Command) == 0 {
					t.Fatalf("container %s in %s has no %s.exec command; these tests run the probe's command", c.Name, path, name)
				}
				probes[name] = *probe
			}
		}
		if probes == nil {
			t.Fatalf("%s has no container named %s", path, golden.container)
		}
	}
	if found != 1 {
		t.Fatalf("listenerPodGoldens lists %d entries for clustered-hub-listener-pod.golden, want 1", found)
	}
	return probes, path
}

// hubProbeRoute is the route of hubListenerLoopback that probe asks, and the index of the one
// argument of its command that names it; a command that names the loopback in no argument, or in
// more than one, fails the test.
func hubProbeRoute(t *testing.T, path, name string, probe listenerProbe) (string, int) {
	t.Helper()
	at := -1
	for i, arg := range probe.Exec.Command {
		if !strings.HasPrefix(arg, hubListenerLoopback+"/") {
			continue
		}
		if at >= 0 {
			t.Fatalf("the %s of %s, %q, names %s in more than one argument", name, path, probe.Exec.Command, hubListenerLoopback)
		}
		at = i
	}
	if at < 0 {
		t.Fatalf("the %s of %s, %q, asks no route of %s", name, path, probe.Exec.Command, hubListenerLoopback)
	}
	return strings.TrimPrefix(probe.Exec.Command[at], hubListenerLoopback), at
}

// requireHubProbeRoutes fails the test unless the hub pod's startup and liveness probes ask
// /livez and its readiness probe asks /healthz.
func requireHubProbeRoutes(t *testing.T, probes map[string]listenerProbe, path string) {
	t.Helper()
	for name, want := range map[string]string{"startupProbe": "/livez", "livenessProbe": "/livez", "readinessProbe": "/healthz"} {
		if route, _ := hubProbeRoute(t, path, name, probes[name]); route != want {
			t.Fatalf("the %s of %s asks %s, want %s", name, path, route, want)
		}
	}
}

// runHubProbe runs probe's command, as the hub pod's golden records it, against the listener on
// port in place of the pod's own 9020, with this machine's PATH alone in its environment, so no
// proxy setting reaches curl, and returns its exit status and what it wrote to stderr. The test
// needs the command's program, curl, on this machine, as the listener's image carries it, and
// fails, never skips, without it; a command that does not start fails the test too.
func runHubProbe(t *testing.T, path, name string, probe listenerProbe, port int) (int, string) {
	t.Helper()
	route, at := hubProbeRoute(t, path, name, probe)
	command := slices.Clone(probe.Exec.Command)
	command[at] = "http://127.0.0.1:" + strconv.Itoa(port) + route
	if _, err := exec.LookPath(command[0]); err != nil {
		t.Fatalf("%s, which the %s of %s runs, is not on PATH; the listener's image carries it, and this test fails rather than skips", command[0], name, path)
	}
	cmd := exec.Command(command[0], command[1:]...)
	cmd.Env = []string{"PATH=" + os.Getenv("PATH")}
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		var exit *exec.ExitError
		if !errors.As(err, &exit) {
			t.Fatalf("run the %s %q: %v", name, command, err)
		}
		return exit.ExitCode(), stderr.String()
	}
	return 0, stderr.String()
}

// curlHTTPError is curl's exit status with -f when the server answered with an HTTP error, a
// status of 400 or more: the listener answered, and its answer was not a 2xx.
const curlHTTPError = 22

// hubInterestsServed reports whether the listener on port serves GET /v1/interests/ with a 200
// and its /metrics scrape carries envoy_active_interests' sample line, and fails the test on a
// scrape that lacks the gauge's TYPE line, which every scrape carries. Before /v1 opens the list
// read answers the refilling 503 (startingGate) and the gauge writes no sample
// (registerListGauges); once /v1 serves the interest list, both do.
func hubInterestsServed(t *testing.T, listener *listenerProcess) (listed, sampled bool) {
	t.Helper()
	status, _ := callListener(t, listener.port, http.MethodGet, "/v1/interests/", "")
	code, scrape := do(t, mustRequest(t, http.MethodGet, "http://127.0.0.1:"+strconv.Itoa(listener.port)+"/metrics"))
	if code != http.StatusOK || !strings.Contains(scrape, "# TYPE envoy_active_interests gauge\n") {
		t.Fatalf("/metrics answered %d without the TYPE line of envoy_active_interests:\n%s", code, scrape)
	}
	for line := range strings.SplitSeq(scrape, "\n") {
		if strings.HasPrefix(line, "envoy_active_interests ") {
			sampled = true
		}
	}
	return status == http.StatusOK, sampled
}

// serveListRig serves rig's routes on a port of this machine's loopback for the rest of the test,
// as a listener serves its own on 9020, and returns the port, for runHubProbe.
func serveListRig(t *testing.T, rig *listRig) int {
	t.Helper()
	server := httptest.NewServer(rig.handler)
	t.Cleanup(server.Close)
	return server.Listener.Addr().(*net.TCPAddr).Port
}

// rigHealth is the status code of rig's /healthz answer and the status its body names.
func rigHealth(t *testing.T, rig *listRig) (int, string) {
	t.Helper()
	response := rig.get("/healthz")
	var body listenerwire.Health
	if err := json.NewDecoder(response.Body).Decode(&body); err != nil {
		t.Fatalf("decode the /healthz answer (status %d): %v", response.StatusCode, err)
	}
	return response.StatusCode, body.Status
}

// TestHubListenerIsReadyOnlyOnceItsV1Serves ties the readiness probe gcp:ClusteredNatsHub renders
// for this command's pod to this listener: the probe's command, read out of the pod's golden, is
// run against the listener's own port in place of the pod's 9020.
//
// A listener started before its NATS answers: while NATS is away the start waits for it, /v1 stays
// closed, so GET /v1/interests/ answers the refilling 503 and /metrics carries
// envoy_active_interests' HELP and TYPE lines and no sample, and the probe exits 22, curl -f's
// status for the 503 "starting" /healthz answers: the pod is not Ready, so its Service sends it
// nothing and the rolling update keeps the old pod serving. Once NATS answers, the probe passes,
// and at its first pass GET /v1/interests/ answers 200 and the scrape carries the sample, since
// main publishes what /healthz reads only after openV1Routes.
//
// A listener whose /v1 is open before its interest copy has completed a first read, as the start
// leaves it when its 30 s wait for the interest watcher runs out or that watcher's first scan timed
// out: with that read held (newListRig), GET /v1/interests/<session_id> answers 200 from the bucket,
// so /v1 is open, while the list answers the refilling 503 and /metrics has no sample, and the
// probe exits 22, /healthz answering 503 "starting"; once the read completes the list answers 200
// and the probe passes. cybermonklabs/legion#4534 held both gates in the startup probe, a scrape of
// /metrics with 60 failures allowed; here the readiness probe holds them, because a listener whose
// NATS refuses or does not answer at its start stays up and not ready for as long as that lasts,
// which such a startup probe would end with a restart every ten minutes
// (TestHubListenerProbesNeverRestartAListenerWhoseNATSRefusesItsStart).
func TestHubListenerIsReadyOnlyOnceItsV1Serves(t *testing.T) {
	probes, path := hubProbes(t)
	requireHubProbeRoutes(t, probes, path)
	ready := probes["readinessProbe"]

	t.Run("NATS away at the start", func(t *testing.T) {
		server, uri := testnats.Start(t)
		server.Stop(t)
		listener := startListenerProcess(t, cmdtest.Build(t, "envoy-listener"), uri, "box-hub-ready")
		listener.WaitForOutput(t, "envoy-listener listening")
		if status, stderr := runHubProbe(t, path, "readinessProbe", ready, listener.port); status != curlHTTPError {
			t.Fatalf("the readiness probe of %s exits %d while NATS is away (stderr %q), want %d:\n%s", path, status, stderr, curlHTTPError, listener.Output.String())
		}
		if listed, sampled := hubInterestsServed(t, listener); listed || sampled {
			t.Fatalf("while NATS is away GET /v1/interests/ answered 200 (%v) or /metrics has the envoy_active_interests sample (%v), want neither", listed, sampled)
		}
		select {
		case <-listener.Exited:
			t.Fatalf("the listener exited while NATS was away:\n%s", listener.Output.String())
		default:
		}

		server.Start(t)
		deadline := time.Now().Add(30 * time.Second)
		for {
			status, stderr := runHubProbe(t, path, "readinessProbe", ready, listener.port)
			if status == 0 {
				break
			}
			if status != curlHTTPError {
				t.Fatalf("the readiness probe of %s exits %d (stderr %q) after NATS answered, want %d until it passes:\n%s", path, status, stderr, curlHTTPError, listener.Output.String())
			}
			if time.Now().After(deadline) {
				t.Fatalf("the readiness probe of %s still exits %d 30 s after NATS answered:\n%s", path, status, listener.Output.String())
			}
			time.Sleep(100 * time.Millisecond)
		}
		if listed, sampled := hubInterestsServed(t, listener); !listed || !sampled {
			t.Fatalf("at the readiness probe's first pass GET /v1/interests/ answered 200: %v, and /metrics has the envoy_active_interests sample: %v; want both", listed, sampled)
		}
	})

	t.Run("the interest copy's first read held", func(t *testing.T) {
		const gauge, sessionID = "envoy_active_interests", "ses_hub_first_read"
		held, release := newRelease(t)
		rig := newListRig(t, listRigOptions{release: map[string]<-chan struct{}{"interest": held}, receiving: "box-hub-first-read"})
		rig.register(t, sessionID)
		port := serveListRig(t, rig)
		if response := rig.get("/v1/interests/" + sessionID); response.StatusCode != http.StatusOK {
			body, _ := io.ReadAll(response.Body)
			t.Fatalf("GET /v1/interests/%s answered %d %s while the interest copy's first read is held, want 200: /v1 is open", sessionID, response.StatusCode, body)
		}
		assertListRefilling(t, rig.get("/v1/interests/"))
		rig.assertNoSample(t, gauge)
		if status, stderr := runHubProbe(t, path, "readinessProbe", ready, port); status != curlHTTPError {
			code, word := rigHealth(t, rig)
			t.Fatalf("the readiness probe of %s exits %d while /v1 is open and the interest copy's first read is held (stderr %q, /healthz %d %q), want %d", path, status, stderr, code, word, curlHTTPError)
		}
		if code, word := rigHealth(t, rig); code != http.StatusServiceUnavailable || word != listenerwire.HealthStarting {
			t.Fatalf("/healthz answered %d %q while the interest copy's first read is held, want 503 %q", code, word, listenerwire.HealthStarting)
		}

		release()
		waitFor(t, 5*time.Second, "GET /v1/interests/ to answer 200 once the interest copy's first read completed", func() bool {
			return rig.get("/v1/interests/").StatusCode == http.StatusOK
		})
		rig.assertSample(t, gauge, 1)
		if status, stderr := runHubProbe(t, path, "readinessProbe", ready, port); status != 0 {
			code, word := rigHealth(t, rig)
			t.Fatalf("the readiness probe of %s exits %d once the interest copy's first read completed (stderr %q, /healthz %d %q), want 0", path, status, stderr, code, word)
		}
	})
}

// heldRewatch is a cache whose Rewatch reads its bucket again only once release is closed, or
// fails with its run's context, so the read a reconnect hook makes stays due for as long as the
// test holds it, as a long read of a large bucket would.
type heldRewatch struct {
	kvCache
	release <-chan struct{}
}

func (c heldRewatch) Rewatch(ctx context.Context, conn *natsgo.Conn) error {
	select {
	case <-c.release:
		return c.kvCache.Rewatch(ctx, conn)
	case <-ctx.Done():
		return ctx.Err()
	}
}

// A pod that has been Ready stays Ready while its interest copy reads its bucket again: only the
// copy's first read gates /healthz (interestCopyRead). The NATS server goes away and comes back, as
// a blip does to every pod of the machine at once; the copy stops following its bucket at the
// disconnect and is past its stale bound when NATS answers again, and the reconnect hook's read of
// the interest bucket is held (heldRewatch). Through the outage /healthz answers 503, as it does
// whenever NATS does not answer its flush. Once NATS answers, /healthz answers 200, never
// "starting", and the readiness probe gcp:ClusteredNatsHub renders, read out of the pod's golden,
// exits 0, while GET /v1/interests/ answers its own refilling 503 with no envoy_active_interests
// sample. Once the read completes the list answers 200 with every session, and the probe still
// passes. Readiness that followed the copy at all times would leave the machine's Service with no
// endpoints after such a blip, and every route would fail, not only the list.
func TestAReadyListenerStaysReadyWhileItsInterestCopyReadsAgainAfterAReconnect(t *testing.T) {
	probes, path := hubProbes(t)
	requireHubProbeRoutes(t, probes, path)
	ready := probes["readinessProbe"]
	const gauge = "envoy_active_interests"

	held, release := newRelease(t)
	rig := newListRig(t, listRigOptions{receiving: "box-hub-reread", hookCaches: func(caches []listenerCache) []listenerCache {
		hooked := slices.Clone(caches)
		for i, c := range hooked {
			if c.name == "interest" {
				hooked[i].cache = heldRewatch{kvCache: c.cache, release: held}
			}
		}
		return hooked
	}})
	ids := []string{"ses_reread_a", "ses_reread_b"}
	rig.seed(t, ids...)
	port := serveListRig(t, rig)
	if status, stderr := runHubProbe(t, path, "readinessProbe", ready, port); status != 0 {
		code, word := rigHealth(t, rig)
		t.Fatalf("the readiness probe of %s exits %d once the interest copy has been read (stderr %q, /healthz %d %q), want 0", path, status, stderr, code, word)
	}

	rig.server.Stop(t)
	rig.awaitDisconnect(t)
	rig.clock.Advance(staleBound + time.Second)
	if code, word := rigHealth(t, rig); code != http.StatusServiceUnavailable {
		t.Fatalf("/healthz answered %d %q while NATS is away, want 503: the flush fails", code, word)
	}
	rig.server.Start(t)

	deadline := time.Now().Add(30 * time.Second)
	for {
		code, word := rigHealth(t, rig)
		if word == listenerwire.HealthStarting {
			t.Fatalf("/healthz answered %d %q after NATS answered again while the pod's interest copy reads its bucket again, want no \"starting\": only the copy's first read gates readiness", code, word)
		}
		if code == http.StatusOK {
			status, _ := runHubProbe(t, path, "readinessProbe", ready, port)
			if status == 0 {
				break
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("30 s after NATS answered again /healthz still answers %d %q, want 200 and the readiness probe of %s passing", code, word, path)
		}
		time.Sleep(100 * time.Millisecond)
	}
	assertListRefilling(t, rig.get("/v1/interests/"))
	rig.assertNoSample(t, gauge)

	release()
	waitFor(t, 30*time.Second, "GET /v1/interests/ to answer 200 once the reconnect's read of the interest bucket completed", func() bool {
		return rig.get("/v1/interests/").StatusCode == http.StatusOK
	})
	if got := rig.interestIDs(t); !slices.Equal(got, ids) {
		t.Fatalf("GET /v1/interests/ lists %v once the read completed, want %v", got, ids)
	}
	if status, stderr := runHubProbe(t, path, "readinessProbe", ready, port); status != 0 {
		code, word := rigHealth(t, rig)
		t.Fatalf("the readiness probe of %s exits %d once the interest copy's read completed (stderr %q, /healthz %d %q), want 0", path, status, stderr, code, word)
	}
}

// hubProbeWindow is the count of tries cybermonklabs/legion#4534's startup probe allowed /v1 to
// open in, 60 tries 10 s apart, after which the kubelet restarts the container.
const hubProbeWindow = 60

// A listener whose NATS refuses its start stays up and not ready for as long as the refusal lasts
// (the start rule, README.md "Start: what ends it and what it waits out"), and the probes of the
// pod gcp:ClusteredNatsHub renders, read out of its golden, never restart it. The NATS grant here
// refuses the session bucket, which the listener tries again at each backoff step and names at
// ERROR (storeBucketRefusedLine). Through more than hubProbeWindow runs of each probe and at least
// three refused tries, the startup and liveness probes, curl -sS of /livez, exit 0 at every run, so
// the kubelet never counts a failure toward a restart, and the readiness probe exits 22 at every
// run, so the pod stays out of its Service; the process never exits. The window is compressed:
// the runs follow one another instead of coming 10 s apart, which changes nothing a run answers,
// since /livez answers 204 without reading NATS from the listener's first answer on and /healthz
// answers 503 "starting" until the start completes, whenever it is asked.
func TestHubListenerProbesNeverRestartAListenerWhoseNATSRefusesItsStart(t *testing.T) {
	probes, path := hubProbes(t)

	seed, public := testnats.User(t)
	_, uri := testnats.StartConfig(t, bucketDeniedConfig(public, bus.SessionBucket), []string{"-js"}, false)
	listener := startListenerProcess(t, cmdtest.Build(t, "envoy-listener"), uri, "box-hub-refused", "NATS_NKEY_SEED="+seed)
	listener.WaitForOutput(t, "envoy-listener listening")
	refusals := func() int {
		return len(linesIn(listener.Output.String(), storeBucketRefusedLine,
			map[string]any{"level": "ERROR", "bucket": bus.SessionBucket, "grant": listenerGrant}))
	}

	runs := 0
	for deadline := time.Now().Add(60 * time.Second); runs <= hubProbeWindow || refusals() < 3; runs++ {
		if time.Now().After(deadline) {
			t.Fatalf("within 60 s the probes ran %d times and the listener named the refused %s %d times, want more than %d runs and 3 refusals:\n%s",
				runs, bus.SessionBucket, refusals(), hubProbeWindow, listener.Output.String())
		}
		select {
		case <-listener.Exited:
			t.Fatalf("the listener exited while the NATS grant refused its %s bucket, at probe run %d:\n%s", bus.SessionBucket, runs+1, listener.Output.String())
		default:
		}
		for _, name := range []string{"startupProbe", "livenessProbe"} {
			if status, stderr := runHubProbe(t, path, name, probes[name], listener.port); status != 0 {
				t.Fatalf("the %s of %s exits %d at run %d while the NATS grant refuses the start (stderr %q), want 0: a failure counts toward the container's restart:\n%s",
					name, path, status, runs+1, stderr, listener.Output.String())
			}
		}
		if status, stderr := runHubProbe(t, path, "readinessProbe", probes["readinessProbe"], listener.port); status != curlHTTPError {
			t.Fatalf("the readiness probe of %s exits %d at run %d while the NATS grant refuses the start (stderr %q), want %d:\n%s",
				path, status, runs+1, stderr, curlHTTPError, listener.Output.String())
		}
	}
	t.Logf("%d runs of each probe of %s through %d refused tries of the %s bucket: startup and liveness passed at each, readiness failed at each", runs, path, refusals(), bus.SessionBucket)
}
