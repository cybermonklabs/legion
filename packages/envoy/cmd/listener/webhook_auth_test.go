package main

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/cmdtest"
	"github.com/sjawhar/envoy/internal/logging"
	"github.com/sjawhar/envoy/internal/metrics"
	"github.com/sjawhar/envoy/internal/testwait"
	"github.com/sjawhar/envoy/internal/webhook"
)

// webhookTestSecret signs every delivery these tests send, on every route.
const webhookTestSecret = "webhook-test-secret"

// serveWebhookRoutes serves the three webhook routes as main does: the configuration read from
// the environment (webhook.LoadWebhookConfig) with every route enabled and env added, the routes
// built from it (webhookRoutes) behind an open webhook gate (openWebhooks), on the listener's own
// HTTP server (newHTTPServer). It returns the server's address and the publisher the routes
// publish through.
func serveWebhookRoutes(t *testing.T, env map[string]string) (string, *slowBodyPublisher, *metrics.Registry) {
	t.Helper()
	for name, value := range map[string]string{
		"ENVOY_WEBHOOKS":                  "github,slack,ghostwispr",
		"ENVOY_GITHUB_WEBHOOK_SECRET":     webhookTestSecret,
		"ENVOY_REVIEWER_APP_ID":           "1",
		"ENVOY_SLACK_SIGNING_SECRET":      webhookTestSecret,
		"ENVOY_GHOSTWISPR_SIGNING_SECRET": webhookTestSecret,
		"ENVOY_GITHUB_REPOSITORY_RENAMES": "",
		"ENVOY_WEBHOOK_BODY_BUDGET":       "",
	} {
		t.Setenv(name, value)
	}
	for name, value := range env {
		t.Setenv(name, value)
	}
	cfg, err := webhook.LoadWebhookConfig()
	if err != nil {
		t.Fatalf("load the webhook configuration: %v", err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	gate := newStartingGate("webhook", logging.NewWithWriter("test", io.Discard))
	refusals := metrics.New()
	hooks := webhookRoutes(cfg, refusals.NewCounter(webhookBodyRefusedMetric, "test"))
	mux := http.NewServeMux()
	for _, hook := range hooks {
		mux.Handle(hook.path, gate)
	}
	publisher := &slowBodyPublisher{}
	openWebhooks(gate, hooks, publisher, nil)
	server := newHTTPServer(mux)
	go func() { _ = server.Serve(ln) }()
	t.Cleanup(func() { _ = server.Close() })
	return ln.Addr().String(), publisher, refusals
}

// webhookAnswer is what a route answered one delivery, and how long it took.
type webhookAnswer struct {
	status     int
	retryAfter string
	body       string
	took       time.Duration
}

// postWebhook sends body to path with headers and returns the answer.
func postWebhook(t *testing.T, addr, path string, headers map[string]string, body []byte) webhookAnswer {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, "http://"+addr+path, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	for name, value := range headers {
		req.Header.Set(name, value)
	}
	client := &http.Client{Timeout: 15 * time.Second}
	start := time.Now()
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("POST %s: %v", path, err)
	}
	defer resp.Body.Close()
	answer, _ := io.ReadAll(resp.Body)
	return webhookAnswer{status: resp.StatusCode, retryAfter: resp.Header.Get("Retry-After"), body: string(answer), took: time.Since(start)}
}

func hmacHex(secret string, parts ...string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	for _, part := range parts {
		mac.Write([]byte(part))
	}
	return hex.EncodeToString(mac.Sum(nil))
}

// slackHeaders are the headers Slack sends with body, signed with secret at timestamp.
func slackHeaders(secret string, timestamp time.Time, body []byte) map[string]string {
	ts := strconv.FormatInt(timestamp.Unix(), 10)
	return map[string]string{
		"Content-Type":              "application/json",
		"X-Slack-Request-Timestamp": ts,
		"X-Slack-Signature":         "v0=" + hmacHex(secret, "v0:", ts, ":", string(body)),
	}
}

// githubHeaders are the headers GitHub sends with a push delivery whose body secret signed.
func githubHeaders(secret, delivery string, body []byte) map[string]string {
	return map[string]string{
		"X-GitHub-Delivery":   delivery,
		"X-GitHub-Event":      "push",
		"X-Hub-Signature-256": "sha256=" + hmacHex(secret, string(body)),
	}
}

// ghostWisprHeaders are the headers Ghost Wispr sends with a summary_ready event whose body secret
// signed.
func ghostWisprHeaders(secret, delivery string, body []byte) map[string]string {
	return map[string]string{
		"X-GhostWispr-Delivery":  delivery,
		"X-GhostWispr-Event":     "summary_ready",
		"X-GhostWispr-Signature": "sha256=" + hmacHex(secret, string(body)),
	}
}

// slackEvent is a Slack message event with event id id.
func slackEvent(id string) []byte {
	return []byte(`{"type":"event_callback","event_id":"` + id + `","team_id":"T01234567",` +
		`"event":{"type":"message","channel":"C01234567","user":"U01234567","text":"hello","ts":"1700000000.000100"}}`)
}

// Every route that takes a body from outside the listener checks its sender's signature on the
// bytes before it parses them, so a body that is not JSON and is not signed is a 401, never the
// 400 its JSON decode would answer. The signed rows are the control: the same body signed reaches
// the decode and is refused there as invalid JSON, so a 401 says the signature check came first.
func TestAnUnsignedWebhookBodyIsRefusedBeforeItIsParsed(t *testing.T) {
	addr, publisher, _ := serveWebhookRoutes(t, nil)
	notJSON := []byte("this is not JSON {")
	zeros := strings.Repeat("0", 64)
	now := time.Now()
	for _, tc := range []struct {
		name    string
		path    string
		headers map[string]string
		want    int
	}{
		{name: "slack, a wrong signature", path: "/webhook/slack", want: http.StatusUnauthorized, headers: map[string]string{
			"X-Slack-Request-Timestamp": strconv.FormatInt(now.Unix(), 10), "X-Slack-Signature": "v0=" + zeros}},
		{name: "slack, no signature", path: "/webhook/slack", want: http.StatusUnauthorized},
		{name: "github, a wrong signature", path: "/webhook/github", want: http.StatusUnauthorized, headers: map[string]string{
			"X-GitHub-Delivery": "d-unsigned", "X-GitHub-Event": "push", "X-Hub-Signature-256": "sha256=" + zeros}},
		{name: "github, no signature", path: "/webhook/github", want: http.StatusUnauthorized, headers: map[string]string{
			"X-GitHub-Delivery": "d-unsigned", "X-GitHub-Event": "push"}},
		{name: "ghostwispr, a wrong signature", path: "/webhook/ghostwispr", want: http.StatusUnauthorized, headers: map[string]string{
			"X-GhostWispr-Delivery": "d-unsigned", "X-GhostWispr-Event": "summary_ready", "X-GhostWispr-Signature": "sha256=" + zeros}},
		{name: "ghostwispr, no signature", path: "/webhook/ghostwispr", want: http.StatusUnauthorized, headers: map[string]string{
			"X-GhostWispr-Delivery": "d-unsigned", "X-GhostWispr-Event": "summary_ready"}},
		{name: "control: slack, signed", path: "/webhook/slack", want: http.StatusBadRequest, headers: slackHeaders(webhookTestSecret, now, notJSON)},
		{name: "control: github, signed", path: "/webhook/github", want: http.StatusBadRequest, headers: githubHeaders(webhookTestSecret, "d-signed", notJSON)},
		{name: "control: ghostwispr, signed", path: "/webhook/ghostwispr", want: http.StatusBadRequest, headers: ghostWisprHeaders(webhookTestSecret, "d-signed", notJSON)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			answer := postWebhook(t, addr, tc.path, tc.headers, notJSON)
			t.Logf("%s answered %d %q", tc.path, answer.status, strings.TrimSpace(answer.body))
			if answer.status != tc.want {
				t.Errorf("status = %d %q, want %d", answer.status, strings.TrimSpace(answer.body), tc.want)
			}
		})
	}
	publisher.mu.Lock()
	defer publisher.mu.Unlock()
	if len(publisher.published) != 0 {
		t.Fatalf("published %d envelopes from bodies that are not JSON, want none", len(publisher.published))
	}
}

// Slack signs its url_verification handshake as it signs every request, and its documentation
// says to verify the request before answering the challenge
// (https://docs.slack.dev/reference/events/url_verification). So the route answers the challenge
// only to a handshake whose signature checks, and anyone else who sends one gets a 401 that does
// not carry the challenge.
func TestSlackURLVerificationIsAnsweredOnlyOnceItsSignatureChecks(t *testing.T) {
	addr, _, _ := serveWebhookRoutes(t, nil)
	const challenge = "challenge-3eZbrw1aBm2rZgRNFdxV2595E9CY3gmdALWMmHkvFXO7tYXAYM8P"
	handshake := []byte(`{"token":"Jhj5dZrVaK7ZwHHjRyZWjbDl","challenge":"` + challenge + `","type":"url_verification"}`)
	now := time.Now()
	for _, tc := range []struct {
		name    string
		headers map[string]string
		want    int
	}{
		{name: "unsigned", want: http.StatusUnauthorized},
		{name: "signed with another secret", headers: slackHeaders("another-secret", now, handshake), want: http.StatusUnauthorized},
		{name: "signed ten minutes ago", headers: slackHeaders(webhookTestSecret, now.Add(-10*time.Minute), handshake), want: http.StatusUnauthorized},
		{name: "control: signed", headers: slackHeaders(webhookTestSecret, now, handshake), want: http.StatusOK},
	} {
		t.Run(tc.name, func(t *testing.T) {
			answer := postWebhook(t, addr, "/webhook/slack", tc.headers, handshake)
			t.Logf("answered %d %q", answer.status, strings.TrimSpace(answer.body))
			if answer.status != tc.want {
				t.Fatalf("status = %d %q, want %d", answer.status, strings.TrimSpace(answer.body), tc.want)
			}
			if tc.want != http.StatusOK {
				if strings.Contains(answer.body, challenge) {
					t.Fatalf("the refusal %q carries the challenge", answer.body)
				}
				return
			}
			var echoed struct {
				Challenge string `json:"challenge"`
			}
			if err := json.Unmarshal([]byte(answer.body), &echoed); err != nil || echoed.Challenge != challenge {
				t.Fatalf("answer %q, want the challenge %q echoed (%v)", answer.body, challenge, err)
			}
		})
	}
}

// holdGitHubBody opens a GitHub delivery to addr that declares a body of 1 MiB, sends sent bytes
// of it and then sends nothing more, as a sender that stalls does, and returns its connection. The
// test closes the connection to end the delivery.
func holdGitHubBody(t *testing.T, addr string, sent int) net.Conn {
	t.Helper()
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	head := fmt.Sprintf("POST /webhook/github HTTP/1.1\r\nHost: listener\r\nContent-Type: application/json\r\n"+
		"Content-Length: %d\r\nX-GitHub-Delivery: d-holder\r\nX-GitHub-Event: push\r\nX-Hub-Signature-256: sha256=00\r\n\r\n", 1<<20)
	if _, err := io.WriteString(conn, head+strings.Repeat("x", sent)); err != nil {
		t.Fatal(err)
	}
	return conn
}

// The listener reads every webhook route's bodies against one budget
// (ENVOY_WEBHOOK_BODY_BUDGET). Here a GitHub sender has sent four times a 16 KiB budget of a body
// and stalled, holding the budget past its limit, so a signed Slack event and a signed Ghost Wispr
// event each wait their route's room wait for room and are answered 503 with Retry-After, and
// published nothing. Once the GitHub sender goes away its delivery ends and gives its room back,
// and the same Slack event is published.
func TestEveryWebhookRouteReadsAgainstTheListenersOneBodyBudget(t *testing.T) {
	const budget = 16 << 10
	addr, publisher, refusals := serveWebhookRoutes(t, map[string]string{"ENVOY_WEBHOOK_BODY_BUDGET": strconv.Itoa(budget)})
	holder := holdGitHubBody(t, addr, 4*budget)

	// The sender's bytes reach the budget a moment after they are written, so a signed Slack
	// request that publishes nothing (an app_rate_limited notice) is sent until one is refused.
	notice := []byte(`{"type":"app_rate_limited","team_id":"T01234567","minute_rate_limited":1518467820}`)
	var probe webhookAnswer
	full := testwait.Holds(10*time.Second, 50*time.Millisecond, func() bool {
		probe = postWebhook(t, addr, "/webhook/slack", slackHeaders(webhookTestSecret, time.Now(), notice), notice)
		return probe.status != http.StatusOK
	})
	if !full {
		t.Fatalf("every signed Slack request was answered 200 for 10 s while a GitHub sender held %d bytes of a %d-byte budget: the Slack route does not read against the budget the GitHub route does", 4*budget, budget)
	}

	event := slackEvent("Ev-behind-a-full-budget")
	refused := postWebhook(t, addr, "/webhook/slack", slackHeaders(webhookTestSecret, time.Now(), event), event)
	t.Logf("slack behind the stalled GitHub sender: %d %q, Retry-After %q, after %s", refused.status, strings.TrimSpace(refused.body), refused.retryAfter, refused.took.Round(time.Millisecond))
	if refused.status != http.StatusServiceUnavailable || refused.retryAfter == "" {
		t.Fatalf("slack status = %d, Retry-After %q, want 503 with Retry-After", refused.status, refused.retryAfter)
	}

	summary := []byte(`{"event_type":"summary_ready","payload":{"session_id":"20261122060000","type":"summary_ready"}}`)
	ghost := postWebhook(t, addr, "/webhook/ghostwispr", ghostWisprHeaders(webhookTestSecret, "d-behind-a-full-budget", summary), summary)
	t.Logf("ghostwispr behind the stalled GitHub sender: %d %q, Retry-After %q, after %s", ghost.status, strings.TrimSpace(ghost.body), ghost.retryAfter, ghost.took.Round(time.Millisecond))
	if ghost.status != http.StatusServiceUnavailable || ghost.retryAfter == "" {
		t.Fatalf("ghostwispr status = %d, Retry-After %q, want 503 with Retry-After", ghost.status, ghost.retryAfter)
	}
	scrape := httptest.NewRecorder()
	refusals.Handler().ServeHTTP(scrape, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	for _, want := range []string{
		webhookBodyRefusedMetric + `{path="/webhook/slack"} 2`,
		webhookBodyRefusedMetric + `{path="/webhook/ghostwispr"} 1`,
	} {
		if !strings.Contains(scrape.Body.String(), want) {
			t.Fatalf("the listener's metrics read\n%s\nwant a line %s: the probe's and the event's refusals on Slack, the event's on Ghost Wispr", scrape.Body.String(), want)
		}
	}
	publisher.mu.Lock()
	refusedPublished := len(publisher.published)
	publisher.mu.Unlock()
	if refusedPublished != 0 {
		t.Fatalf("published %d envelopes from refused events, want none", refusedPublished)
	}

	_ = holder.Close()
	var served webhookAnswer
	if !testwait.Holds(10*time.Second, 50*time.Millisecond, func() bool {
		served = postWebhook(t, addr, "/webhook/slack", slackHeaders(webhookTestSecret, time.Now(), event), event)
		return served.status == http.StatusOK
	}) {
		t.Fatalf("slack status = %d %q 10 s after the GitHub sender went away, want 200: its delivery kept its room", served.status, strings.TrimSpace(served.body))
	}
	publisher.mu.Lock()
	defer publisher.mu.Unlock()
	if len(publisher.published) == 0 {
		t.Fatal("the served Slack event published nothing")
	}
}

// The listener states its webhook body budget at its start, the setting's value or the default,
// and refuses to start on a value that is not a positive whole number of bytes, naming the
// variable.
func TestTheListenerStatesItsWebhookBodyBudgetAtItsStart(t *testing.T) {
	binary := cmdtest.Build(t, "envoy-listener")
	t.Run("set", func(t *testing.T) {
		listener := startListenerProcess(t, binary, "nats://127.0.0.1:1", "body-budget-set", "ENVOY_WEBHOOK_BODY_BUDGET=1048576")
		listener.WaitForOutput(t, "listener webhook body budget: 1048576 bytes (1.0 MiB) held at once across every webhook route")
	})
	t.Run("unset", func(t *testing.T) {
		listener := startListenerProcess(t, binary, "nats://127.0.0.1:1", "body-budget-unset")
		listener.WaitForOutput(t, "listener webhook body budget: 67108864 bytes (64.0 MiB) held at once across every webhook route")
	})
	t.Run("refused", func(t *testing.T) {
		cmd := exec.Command(binary)
		cmd.Env = []string{
			"PORT=0",
			"ENVOY_LISTEN_HOST=127.0.0.1",
			"ENVOY_MACHINE_ID=body-budget-refused",
			"NATS_URLS=nats://127.0.0.1:1",
			"ENVOY_API_TOKEN=" + listenerTestToken,
			"ENVOY_WEBHOOK_BODY_BUDGET=64MiB",
		}
		process := refusedStart(t, cmd)
		if output := process.Output.String(); !strings.Contains(output, `invalid ENVOY_WEBHOOK_BODY_BUDGET "64MiB"`) {
			t.Fatalf("the refused start's output does not name the setting and its value:\n%s", output)
		}
	})
}
