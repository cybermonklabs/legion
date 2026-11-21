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
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/docker/docker/api/types/container"
	dockernetwork "github.com/docker/docker/api/types/network"
	"github.com/sjawhar/envoy/internal/ciphertoken"
	"github.com/sjawhar/envoy/internal/ciphertoken/ciphertokentest"
	"github.com/sjawhar/envoy/internal/testnats"
	"github.com/testcontainers/testcontainers-go"
	tcnats "github.com/testcontainers/testcontainers-go/modules/nats"
	"github.com/testcontainers/testcontainers-go/network"
	"github.com/testcontainers/testcontainers-go/wait"
)

// capped limits a smoke container to cpus CPUs and mib MiB, as every container the repository's
// tests start is capped on a shared host. NATS holds the smoke's few messages; the listener serves
// one client.
func capped(cpus, mib int64) testcontainers.CustomizeRequestOption {
	return testcontainers.WithHostConfigModifier(func(hostConfig *container.HostConfig) {
		hostConfig.NanoCPUs = cpus * 1_000_000_000
		hostConfig.Memory = mib << 20
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

	// Shared Docker network for container-to-container communication.
	smokeNet, err := network.New(ctx)
	if err != nil {
		t.Fatalf("create network: %v", err)
	}
	t.Cleanup(func() { smokeNet.Remove(ctx) })

	// The secrets broker the listener checks a session's broker token with: the stub broker,
	// reached from the container at this network's gateway, which is this host's own address on
	// the network.
	broker := ciphertokentest.NewStubBroker(t)
	caller := ciphertokentest.NewCaller(t)
	broker.Says(smokeSessionToken, ciphertoken.Introspection{
		Active:       true,
		Session:      smokeSession,
		Person:       "ada@example.com",
		ProjectScope: ciphertoken.ProjectScopePerson,
		ClientID:     ciphertoken.ExchangeClientID,
		Thumbprint:   caller.JKT(),
	})
	brokerURL := serveAtGateway(ctx, t, smokeNet, broker.Server.URL)

	// Start NATS using the same module the rest of the project uses, on the server the hub runs:
	// the listener refuses to start on nats:2.10 (natsversion.Refusal).
	natsC, err := tcnats.Run(ctx, testnats.HubImage,
		network.WithNetwork([]string{"nats"}, smokeNet),
		capped(1, 512),
	)
	testcontainers.CleanupContainer(t, natsC)
	if err != nil {
		t.Fatalf("start NATS: %v", err)
	}

	listenerC, err := testcontainers.Run(ctx, image,
		network.WithNetwork([]string{"listener"}, smokeNet),
		capped(1, 512),
		testcontainers.WithEnv(map[string]string{
			"NATS_URLS": "nats://nats:4222",
			// The listener owns ENVOY_NOTIFICATIONS on a NATS that is not its own container, as
			// every deployed listener does; without this the connect refuses it
			// (internal/bus/reach.go).
			"ENVOY_ALLOW_REMOTE_NATS": "1",
			"ENVOY_LISTEN_HOST":       "0.0.0.0",
			"ENVOY_MACHINE_ID":        "smoke-test",
			"PORT":                    "9020",
			// A bind off loopback starts only with a credential the listener verifies, and its
			// /v1 serves no caller that presents none. The broker settings are the session
			// listener's credential: the listener checks each session's broker token with the
			// broker, presenting its own account token, and checks the proof against its origin.
			"ENVOY_AGENT_SECRETS_URL":                brokerURL,
			"ENVOY_AGENT_SECRETS_ACCOUNT_TOKEN_FILE": smokeAccountTokenFile,
			"ENVOY_PUBLIC_URL":                       smokeListenerOrigin,
		}),
		testcontainers.WithFiles(testcontainers.ContainerFile{
			Reader:            strings.NewReader(smokeAccountToken + "\n"),
			ContainerFilePath: smokeAccountTokenFile,
			FileMode:          0o644,
		}),
		testcontainers.WithExposedPorts("9020/tcp"),
		// /healthz answers 200 from the moment the port binds: "starting" until the durable consumer
		// binds, then "healthy" (a liveness answer, docs/solutions/envoy/async-health-startup-pattern.md).
		// /v1 answers 503 "service starting" until NATS is connected and the interest and session
		// caches are warm, so the subtests below start once the body says "healthy"; a 200 alone
		// released them into that window.
		testcontainers.WithWaitStrategy(
			wait.ForHTTP("/healthz").
				WithPort("9020/tcp").
				WithStatusCodeMatcher(func(status int) bool {
					return status == http.StatusOK
				}).
				WithResponseMatcher(func(body io.Reader) bool {
					var health struct {
						Status string `json:"status"`
					}
					return json.NewDecoder(body).Decode(&health) == nil && health.Status == "healthy"
				}).
				WithStartupTimeout(60*time.Second),
		),
	)
	testcontainers.CleanupContainer(t, listenerC)
	if err != nil {
		t.Fatalf("start listener: %v", err)
	}

	host, err := listenerC.Host(ctx)
	if err != nil {
		t.Fatalf("get host: %v", err)
	}
	port, err := listenerC.MappedPort(ctx, "9020")
	if err != nil {
		t.Fatalf("get mapped port: %v", err)
	}
	baseURL := fmt.Sprintf("http://%s:%s", host, port.Port())

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

	t.Run("sessions_require_a_credential", func(t *testing.T) {
		for _, authorization := range []string{"", "Bearer unknown"} {
			req, err := http.NewRequest(http.MethodGet, baseURL+"/v1/sessions", nil)
			if err != nil {
				t.Fatalf("build GET /v1/sessions: %v", err)
			}
			if authorization != "" {
				req.Header.Set("Authorization", authorization)
			}
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatalf("GET /v1/sessions: %v", err)
			}
			resp.Body.Close()
			if resp.StatusCode != http.StatusUnauthorized {
				t.Errorf("GET /v1/sessions with Authorization %q: got %d, want %d", authorization, resp.StatusCode, http.StatusUnauthorized)
			}
		}
	})

	t.Run("sessions", func(t *testing.T) {
		resp, err := apiRequest(t, caller, baseURL, http.MethodGet, "/v1/sessions", "")
		if err != nil {
			t.Fatalf("GET /v1/sessions: %v", err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			t.Errorf("GET /v1/sessions: got %d, want %d", resp.StatusCode, http.StatusOK)
		}
		// The listener reached the broker from its container, as its own account.
		if bearers := broker.Bearers(); len(bearers) == 0 || bearers[0] != "Bearer "+smokeAccountToken {
			t.Errorf("the broker was presented %q, want the listener's own account token", bearers)
		}
	})

	t.Run("subscribe_and_verify", func(t *testing.T) {
		// No port: this listener delivers nothing to a session's port and refuses a registration
		// that names one (subscribeHandler). A session is registered only with a consumer of its
		// own, which the delivery field asks for, and a notifications.team.<name> topic is one
		// the stream stores: the route leaves a notifications. topic no stream subject captures
		// out of the row, with a warning naming it, since nothing would be delivered on it.
		subBody := `{"session_id":"` + smokeSession + `","topics":["notifications.team.smoke"],"port":0,"self_subscribed":true,"delivery":{"consumer":true,"epoch":"smoke"}}`
		resp, err := apiRequest(t, caller, baseURL, http.MethodPost, "/v1/interests/subscribe", subBody)
		if err != nil {
			t.Fatalf("POST /v1/interests/subscribe: %v", err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			t.Errorf("POST /v1/interests/subscribe: got %d, want %d", resp.StatusCode, http.StatusOK)
		}

		sessResp, err := apiRequest(t, caller, baseURL, http.MethodGet, "/v1/sessions", "")
		if err != nil {
			t.Fatalf("GET /v1/sessions: %v", err)
		}
		defer sessResp.Body.Close()

		bodyBytes, err := io.ReadAll(sessResp.Body)
		if err != nil {
			t.Fatalf("read /v1/sessions: %v", err)
		}

		if !strings.Contains(string(bodyBytes), smokeSession) {
			t.Error("registered session not visible in /v1/sessions")
		}
	})
}

const (
	// smokeSession is the session the caller's broker token names, and smokeSessionToken that
	// token.
	smokeSession      = "ses_smoke_test"
	smokeSessionToken = "access-token-of-the-smoke-session"
	// smokeAccountToken is the listener's own account token, which it presents to the broker, and
	// smokeAccountTokenFile where its container holds it.
	smokeAccountToken     = "smoke-listener-account-token"
	smokeAccountTokenFile = "/tmp/envoy-broker-token"
	// smokeListenerOrigin is the listener's own origin (ENVOY_PUBLIC_URL), which a proof names: the
	// listener checks a proof's target against it and the request's path, never against the Host a
	// request reaches it by.
	smokeListenerOrigin = "http://listener:9020"
)

// apiRequest calls the /v1 route path of the listener at baseURL as the smoke session: its broker
// token under the DPoP scheme, with a fresh proof the caller's real client signs for the listener's
// own origin. body is JSON when non-empty.
func apiRequest(t *testing.T, caller *ciphertokentest.Caller, baseURL, method, path, body string) (*http.Response, error) {
	t.Helper()
	req, err := http.NewRequest(method, baseURL+path, strings.NewReader(body))
	if err != nil {
		return nil, err
	}
	signed := caller.Signed(t, method, smokeListenerOrigin+path, smokeSessionToken)
	req.Header.Set("Authorization", signed.Header.Get("Authorization"))
	req.Header.Set("DPoP", signed.Header.Get("DPoP"))
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	return http.DefaultClient.Do(req)
}

// serveAtGateway serves target, by a reverse proxy, at the gateway address of the Docker network
// smokeNet, and answers that origin. The gateway is this host's own address on the network's
// bridge, so a container on it reaches what the test serves with no container of its own between
// them, and the test binds none of the host's external addresses.
func serveAtGateway(ctx context.Context, t *testing.T, smokeNet *testcontainers.DockerNetwork, target string) string {
	t.Helper()
	cli, err := testcontainers.NewDockerClientWithOpts(ctx)
	if err != nil {
		t.Fatalf("connect to Docker: %v", err)
	}
	defer cli.Close()
	inspect, err := cli.NetworkInspect(ctx, smokeNet.ID, dockernetwork.InspectOptions{})
	if err != nil {
		t.Fatalf("inspect the smoke network: %v", err)
	}
	gateway := ""
	for _, config := range inspect.IPAM.Config {
		if ip := net.ParseIP(config.Gateway); ip != nil && ip.To4() != nil {
			gateway = config.Gateway
			break
		}
	}
	if gateway == "" {
		t.Fatalf("the smoke network %s has no IPv4 gateway: %+v", smokeNet.Name, inspect.IPAM.Config)
	}
	targetURL, err := url.Parse(target)
	if err != nil {
		t.Fatalf("parse %s: %v", target, err)
	}
	ln, err := net.Listen("tcp", net.JoinHostPort(gateway, "0"))
	if err != nil {
		t.Fatalf("listen at the smoke network's gateway %s: %v", gateway, err)
	}
	server := &http.Server{Handler: httputil.NewSingleHostReverseProxy(targetURL), ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = server.Serve(ln) }()
	t.Cleanup(func() { _ = server.Close() })
	return "http://" + ln.Addr().String()
}
