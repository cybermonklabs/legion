package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/logging"
	"github.com/sjawhar/envoy/internal/logtest"
	"github.com/sjawhar/envoy/internal/testwait"
	"github.com/sjawhar/envoy/internal/webhook"
)

type slowBodyPublisher struct {
	mu        sync.Mutex
	published []contracts.Envelope
}

func (p *slowBodyPublisher) PublishEachWithin(_ context.Context, e contracts.Envelope, published func(topic string, duplicate, resent bool)) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.published = append(p.published, e)
	if published != nil {
		published(e.Topic, false, false)
	}
	return nil
}

type noCI struct{}

func (noCI) Record(context.Context, contracts.CIObservation) error      { return nil }
func (noCI) RecordSuite(context.Context, contracts.CIObservation) error { return nil }

// slowBodyReadTimeout is the server read timeout these rows pass to newHTTPServer in place of
// production's 10 s, which INFRA-3149 allows as a caller-passed value no verdict depends on
// (dispatch://INFRA-3149/comment/5397eb29-525e-4bac-be18-c5dd7fdcfafa): each row waits for the
// server's own cut-off or answer, never for how long it took.
const slowBodyReadTimeout = 150 * time.Millisecond

// serveWebhook puts routes behind the listener's open webhook gate on its own HTTP server.
func serveWebhook(t *testing.T, readTimeout time.Duration, handlers map[string]http.Handler) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	gate := newStartingGate("webhook", logging.NewWithWriter("test", io.Discard))
	routes := http.NewServeMux()
	mux := http.NewServeMux()
	for path, handler := range handlers {
		routes.Handle(path, handler)
		mux.Handle(path, gate)
	}
	gate.open(routes)
	server := newHTTPServer(mux, readTimeout)
	go func() { _ = server.Serve(ln) }()
	t.Cleanup(func() { _ = server.Close() })
	return ln.Addr().String()
}

// startWebhookPOST sends the header and the available prefix together, leaving the connection open
// for the caller to send the remaining bytes or wait for the server to reject the partial body.
func startWebhookPOST(t *testing.T, addr, path string, headers map[string]string, length int, prefix []byte) (net.Conn, *bufio.Reader) {
	t.Helper()
	conn, err := (&net.Dialer{}).DialContext(testwait.Context(t), "tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	var request bytes.Buffer
	fmt.Fprintf(&request, "POST %s HTTP/1.1\r\nHost: listener\r\nContent-Length: %d\r\n", path, length)
	for k, v := range headers {
		fmt.Fprintf(&request, "%s: %s\r\n", k, v)
	}
	request.WriteString("\r\n")
	request.Write(prefix)
	writeWebhook(t, conn, "write the webhook header and body prefix", request.Bytes())
	return conn, bufio.NewReader(conn)
}

func writeWebhook(t *testing.T, conn net.Conn, what string, data []byte) {
	t.Helper()
	var err error
	testwait.Returns(t, what, func() { _, err = conn.Write(data) })
	if err != nil {
		t.Fatalf("%s: %v", what, err)
	}
}

func readWebhookStatus(t *testing.T, reader *bufio.Reader, what string) (int, error) {
	t.Helper()
	var status int
	var err error
	testwait.Returns(t, what, func() {
		var resp *http.Response
		resp, err = http.ReadResponse(reader, nil)
		if err == nil {
			status = resp.StatusCode
			err = resp.Body.Close()
		}
	})
	return status, err
}

func waitForSlackReadTimeout(t *testing.T, addr, secret string) {
	t.Helper()
	body := []byte(`{"type":"url_verification","challenge":"` + strings.Repeat("c", 4000) + `"}`)
	// The wall clock supplies Slack's signature timestamp, not a timing verdict.
	headers := slackHeaders(secret, time.Now(), body)
	headers["Expect"] = "100-continue"
	_, reader := startWebhookPOST(t, addr, "/webhook/slack", headers, len(body), body[:len(body)/2])
	if status, err := readWebhookStatus(t, reader, "Slack to begin reading the body"); err != nil || status != http.StatusContinue {
		t.Fatalf("Slack partial body: status = %d, read error = %v; want 100 Continue", status, err)
	}
	// Keep the sender connected without sending the rest. The Slack route still has the server's
	// read deadline, so it rejects the partial body or the server closes without an answer.
	status, err := readWebhookStatus(t, reader, "Slack read timeout to cut off the partial body")
	if status == 0 && (errors.Is(err, io.EOF) || errors.Is(err, syscall.ECONNRESET)) {
		return
	}
	if err != nil || status != http.StatusBadRequest {
		t.Fatalf("Slack partial body: status = %d, read error = %v; want 400 or a closed connection", status, err)
	}
}

// A GitHub delivery whose body takes longer than the server's read timeout to arrive (10 s in
// production; slowBodyReadTimeout here) is read whole and published, not cut off with no answer.
// No sleep paces it: the rest of the body goes out only once a later Slack request on the same
// server has been cut off by that read timeout, so the GitHub request's own has passed too.
func TestAGitHubDeliveryWhoseBodyArrivesOverMoreThanTenSecondsIsPublished(t *testing.T) {
	t.Parallel()
	const secret = "s"
	body := []byte(`{"ref":"refs/heads/main","after":"` + strings.Repeat("a", 40) + `","repository":{"name":"legion","owner":{"login":"sjawhar"},"full_name":"sjawhar/legion"},"pad":"` + strings.Repeat("x", 3<<20) + `"}`)
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	headers := map[string]string{
		"Content-Type":        "application/json",
		"Expect":              "100-continue",
		"X-GitHub-Delivery":   "delivery-slow-body",
		"X-GitHub-Event":      "push",
		"X-Hub-Signature-256": "sha256=" + hex.EncodeToString(mac.Sum(nil)),
	}
	pub := &slowBodyPublisher{}
	bodies := webhook.NewBodyBudget(webhook.DefaultBodyBudget, nil)
	addr := serveWebhook(t, slowBodyReadTimeout, map[string]http.Handler{
		"/webhook/github": webhook.GitHubHandler(secret, "@legion", "", "", pub, noCI{}, bodies),
		"/webhook/slack":  webhook.SlackHandler(secret, &slowBodyPublisher{}, bodies),
	})
	split := len(body) / 20
	conn, reader := startWebhookPOST(t, addr, "/webhook/github", headers, len(body), body[:split])
	if status, err := readWebhookStatus(t, reader, "GitHub to begin reading the body"); err != nil || status != http.StatusContinue {
		t.Fatalf("GitHub partial body: status = %d, read error = %v; want 100 Continue", status, err)
	}

	// GitHub was already reading its body before this Slack connection started. When the later
	// connection's read timeout cuts it off, GitHub's original server deadline has passed too.
	waitForSlackReadTimeout(t, addr, secret)
	writeWebhook(t, conn, "send the rest of the GitHub body after the server read timeout", body[split:])
	status, err := readWebhookStatus(t, reader, "GitHub's answer to the full body after the server read timeout")
	if err != nil || status != http.StatusOK {
		t.Fatalf("GitHub full body after the server read timeout: status = %d, read error = %v; want 200", status, err)
	}
	pub.mu.Lock()
	defer pub.mu.Unlock()
	if len(pub.published) == 0 {
		t.Fatal("published nothing; want the push")
	}
}

// A GitHub sender that goes away partway through its body fails the read, and the failure is
// logged with what the delivery was and how far its body got.
func TestAGitHubBodyReadFailureIsLogged(t *testing.T) {
	// Not parallel: it swaps the log package's output (log.SetOutput).
	logged := &lockedBuffer{}
	previous := log.Writer()
	log.SetOutput(logged)
	t.Cleanup(func() { log.SetOutput(previous) })
	addr := serveWebhook(t, 10*time.Second, map[string]http.Handler{
		"/webhook/github": webhook.GitHubHandler("s", "@legion", "", "", &slowBodyPublisher{}, noCI{}, webhook.NewBodyBudget(webhook.DefaultBodyBudget, nil)),
	})
	conn, _ := startWebhookPOST(t, addr, "/webhook/github", map[string]string{
		"X-GitHub-Delivery": "delivery-cut",
		"X-GitHub-Event":    "push",
	}, 1000, []byte(strings.Repeat("x", 300)))
	if err := conn.Close(); err != nil { // the sender goes away with 700 bytes still owed
		t.Fatal(err)
	}
	line := testwait.Until(t, "GitHub body read failure log", func() (string, bool) {
		lines := logged.String()
		for line := range strings.SplitSeq(lines, "\n") {
			if strings.Contains(line, "github body read failed") {
				return line, true
			}
		}
		return lines, false
	})
	for _, want := range []string{"github body read failed", "delivery=delivery-cut", "event=push", "content_length=1000", "read=300", "elapsed="} {
		if !strings.Contains(line, want) {
			t.Errorf("log = %q, want it to contain %q", line, want)
		}
	}
}

// Every other route keeps the server's read timeout on its body (10 s in production;
// slowBodyReadTimeout here): a Slack body that takes longer to arrive is cut off. The body is a
// signed url_verification, which the route would answer 200 had it arrived whole.
func TestASlackBodyStillHasTheServersReadTimeout(t *testing.T) {
	t.Parallel()
	const secret = "s"
	pub := &slowBodyPublisher{}
	addr := serveWebhook(t, slowBodyReadTimeout, map[string]http.Handler{
		"/webhook/slack": webhook.SlackHandler(secret, pub, webhook.NewBodyBudget(webhook.DefaultBodyBudget, nil)),
	})
	waitForSlackReadTimeout(t, addr, secret)
	pub.mu.Lock()
	defer pub.mu.Unlock()
	if len(pub.published) != 0 {
		t.Fatalf("published %d Slack envelopes for an incomplete body; want none", len(pub.published))
	}
}

// lockedBuffer is a log destination the server's goroutines and the test can share.
type lockedBuffer = logtest.Buffer
