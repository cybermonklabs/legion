//go:build smoke

// Package smoke contains container-level smoke tests for the Envoy listener.
// Verifies critical endpoints work end-to-end with a real NATS server via testcontainers.
//
// ENVOY_SMOKE_IMAGE names the already-built listener image to run. This test never builds one:
// a build inside `go test` spends the test's deadline (LEGION-361). Unset,
// the test fails immediately and prints the build command, so a lane that loses its build step
// — a rename, a typo, a new workflow — goes red instead of quietly building again.
package smoke

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/docker/docker/api/types/container"
	"github.com/sjawhar/envoy/internal/testnats"
	"github.com/sjawhar/envoy/internal/testwait"
	"github.com/testcontainers/testcontainers-go"
)

// capped runs a smoke container on the host's network, limited to cpus CPUs and mib MiB, as every
// container the repository's tests start is capped on a shared host. NATS holds the smoke's few
// messages; the listener serves one client. On the host's network Docker publishes nothing:
// testcontainers publishes each port an image declares on Docker's bridge network (the listener
// image's 9020, NATS's 4222, 6222 and 8222), and a port Docker publishes is one its own allocator
// chooses, without asking the kernel whether another program holds it, so ten taken ports in a
// row fail the start (docs/testing.md, "A container a test starts publishes no port"). Each
// server here binds a loopback port the kernel chooses and logs it instead (loggedPort).
func capped(cpus, mib int64) testcontainers.CustomizeRequestOption {
	return testcontainers.WithHostConfigModifier(func(hostConfig *container.HostConfig) {
		hostConfig.NetworkMode = "host"
		hostConfig.NanoCPUs = cpus * 1_000_000_000
		hostConfig.Memory = mib << 20
	})
}

// The lines NATS (-p -1) and the listener (PORT=0) log once each has bound the port the kernel
// chose, with the port.
var (
	natsClientPortLine = regexp.MustCompile(`Listening for client connections on 127\.0\.0\.1:(\d+)`)
	listenerPortLine   = regexp.MustCompile(`"msg":"envoy-listener listening".*"addr":"127\.0\.0\.1:(\d+)"`)
)

// loggedPort waits for the log of ctr to hold line, and returns the port it names.
func loggedPort(t *testing.T, ctr testcontainers.Container, line *regexp.Regexp) string {
	t.Helper()
	return testwait.Until(t, fmt.Sprintf("container %s to log %q", ctr.GetContainerID(), line), func() (string, bool) {
		logs, err := ctr.Logs(context.Background())
		if err != nil {
			return err.Error(), false
		}
		defer logs.Close()
		text, err := io.ReadAll(logs)
		if err != nil {
			return err.Error(), false
		}
		found := line.FindSubmatch(text)
		if found == nil {
			return "no such line", false
		}
		return string(found[1]), true
	})
}

func TestSmoke(t *testing.T) {
	image := os.Getenv("ENVOY_SMOKE_IMAGE")
	if image == "" {
		t.Fatal("ENVOY_SMOKE_IMAGE is unset, and this test never builds the image itself.\n" +
			"From the repository root:\n" +
			"  img=$(docker buildx build --load -q " +
			"-f packages/envoy/docker/Dockerfile .)\n" +
			"then, in packages/envoy:\n" +
			"  ENVOY_SMOKE_IMAGE=$img go test -tags smoke -v ./internal/smoke/\n" +
			"`-q` prints the image id, and go's test cache keys on the value of this variable, " +
			"so rebuilding re-runs the test. A fixed tag would replay a cached PASS against an " +
			"image that had since changed. In CI the build is a step of its own; see the " +
			"envoy-smoke job's steps in .github/workflows/ci-job-envoy-smoke.yaml.")
	}

	ctx := context.Background()

	// NATS on the server the hub runs: the listener refuses to start on nats:2.10
	// (natsversion.Refusal). It listens on a loopback port the kernel chooses.
	natsC, err := testcontainers.Run(ctx, testnats.HubImage,
		testcontainers.WithCmd("-js", "-a", "127.0.0.1", "-p", "-1"),
		capped(1, 512),
	)
	testcontainers.CleanupContainer(t, natsC)
	if err != nil {
		t.Fatalf("start NATS: %v", err)
	}
	natsURL := "nats://127.0.0.1:" + loggedPort(t, natsC, natsClientPortLine)

	listenerC, err := testcontainers.Run(ctx, image,
		capped(1, 512),
		testcontainers.WithEnv(map[string]string{
			// On the host's network NATS is this machine's, at 127.0.0.1, as on the GCP hub, whose
			// listener runs on the host's network too, so the connect needs no ENVOY_ALLOW_REMOTE_NATS
			// (internal/bus/reach.go).
			"NATS_URLS":         natsURL,
			"ENVOY_LISTEN_HOST": "127.0.0.1",
			"ENVOY_MACHINE_ID":  "smoke-test",
			"PORT":              "0",
			// With a token the listener requires a bearer on every /v1 route, the path a deployed
			// listener serves.
			"ENVOY_API_TOKEN": smokeAPIToken,
		}),
	)
	testcontainers.CleanupContainer(t, listenerC)
	if err != nil {
		t.Fatalf("start listener: %v", err)
	}
	baseURL := "http://127.0.0.1:" + loggedPort(t, listenerC, listenerPortLine)

	// /healthz answers 200 from the moment the port binds: "starting" until the durable consumer
	// binds, then "healthy" (a liveness answer, docs/solutions/envoy/async-health-startup-pattern.md).
	// /v1 answers 503 "service starting" until NATS is connected and the interest and session
	// caches are warm, so the subtests below start once the body says "healthy"; a 200 alone
	// released them into that window.
	testwait.Until(t, "the listener at "+baseURL+" to answer /healthz 200 healthy", func() (string, bool) {
		request, err := http.NewRequestWithContext(testwait.Context(t), http.MethodGet, baseURL+"/healthz", nil)
		if err != nil {
			return err.Error(), false
		}
		resp, err := http.DefaultClient.Do(request)
		if err != nil {
			return err.Error(), false
		}
		defer resp.Body.Close()
		var health struct {
			Status string `json:"status"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&health); err != nil {
			return fmt.Sprintf("%d, a body that is no health answer: %v", resp.StatusCode, err), false
		}
		return fmt.Sprintf("%d %q", resp.StatusCode, health.Status), resp.StatusCode == http.StatusOK && health.Status == "healthy"
	})

	t.Run("healthz", func(t *testing.T) {
		resp, err := http.Get(baseURL + "/healthz")
		if err != nil {
			t.Fatalf("GET /healthz: %v", err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			t.Errorf("GET /healthz: got %d, want %d", resp.StatusCode, http.StatusOK)
		}

		var body map[string]interface{}
		if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
			t.Fatalf("decode /healthz: %v", err)
		}

		status, ok := body["status"].(string)
		if !ok {
			t.Fatal("/healthz missing 'status' field")
		}
		if status != "healthy" {
			t.Errorf("/healthz status=%q, want 'healthy'", status)
		}
	})

	t.Run("metrics", func(t *testing.T) {
		resp, err := http.Get(baseURL + "/metrics")
		if err != nil {
			t.Fatalf("GET /metrics: %v", err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			t.Errorf("GET /metrics: got %d, want %d", resp.StatusCode, http.StatusOK)
		}

		bodyBytes, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatalf("read /metrics: %v", err)
		}
		body := string(bodyBytes)

		for _, metric := range []string{
			"envoy_messages_received_total",
			"envoy_active_sessions",
			"envoy_active_interests",
		} {
			if !strings.Contains(body, metric) {
				t.Errorf("/metrics missing %q", metric)
			}
		}
	})

	t.Run("sessions_require_bearer", func(t *testing.T) {
		resp, err := http.Get(baseURL + "/v1/sessions")
		if err != nil {
			t.Fatalf("GET /v1/sessions: %v", err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("GET /v1/sessions without a bearer: got %d, want %d", resp.StatusCode, http.StatusUnauthorized)
		}
	})

	t.Run("sessions", func(t *testing.T) {
		resp, err := apiRequest(http.MethodGet, baseURL+"/v1/sessions", "")
		if err != nil {
			t.Fatalf("GET /v1/sessions: %v", err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			t.Errorf("GET /v1/sessions: got %d, want %d", resp.StatusCode, http.StatusOK)
		}
	})

	t.Run("subscribe_and_verify", func(t *testing.T) {
		// No port: this listener delivers nothing to a session's port and refuses a registration
		// that names one (subscribeHandler). A session is registered only with a consumer of its
		// own, which the delivery field asks for, and a notifications.team.<name> topic is one
		// the stream stores: the route leaves a notifications. topic no stream subject captures
		// out of the row, with a warning naming it, since nothing would be delivered on it.
		subBody := `{"session_id":"ses_smoke_test","topics":["notifications.team.smoke"],"port":0,"self_subscribed":true,"delivery":{"consumer":true,"epoch":"smoke"}}`
		resp, err := apiRequest(http.MethodPost, baseURL+"/v1/interests/subscribe", subBody)
		if err != nil {
			t.Fatalf("POST /v1/interests/subscribe: %v", err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			t.Errorf("POST /v1/interests/subscribe: got %d, want %d", resp.StatusCode, http.StatusOK)
		}

		sessResp, err := apiRequest(http.MethodGet, baseURL+"/v1/sessions", "")
		if err != nil {
			t.Fatalf("GET /v1/sessions: %v", err)
		}
		defer sessResp.Body.Close()

		bodyBytes, err := io.ReadAll(sessResp.Body)
		if err != nil {
			t.Fatalf("read /v1/sessions: %v", err)
		}

		if !strings.Contains(string(bodyBytes), "ses_smoke_test") {
			t.Error("registered session not visible in /v1/sessions")
		}
	})
}

const smokeAPIToken = "smoke-api-token"

// apiRequest calls a /v1 route with the listener's bearer; body is JSON when non-empty.
func apiRequest(method, url, body string) (*http.Response, error) {
	req, err := http.NewRequest(method, url, strings.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+smokeAPIToken)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	return http.DefaultClient.Do(req)
}
