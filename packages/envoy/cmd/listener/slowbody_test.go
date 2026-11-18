package main

import (
	"bufio"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"strings"
	"sync"
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

func (p *slowBodyPublisher) PublishWithin(_ context.Context, e contracts.Envelope) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.published = append(p.published, e)
	return nil
}

type noCI struct{}

func (noCI) Record(context.Context, contracts.CIObservation) error      { return nil }
func (noCI) RecordSuite(context.Context, contracts.CIObservation) error { return nil }

// serveWebhook serves handler at path the way main does: the outer mux sends the path to an open
// webhook startingGate, whose mux holds the handler, all on the listener's own HTTP server
// (newHTTPServer). It returns the server's address.
func serveWebhook(t *testing.T, path string, handler http.Handler) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	gate := newStartingGate("webhook", logging.NewWithWriter("test", io.Discard))
	routes := http.NewServeMux()
	routes.Handle(path, handler)
	gate.open(routes)
	mux := http.NewServeMux()
	mux.Handle(path, gate)
	server := newHTTPServer(mux)
	go func() { _ = server.Serve(ln) }()
	t.Cleanup(func() { _ = server.Close() })
	return ln.Addr().String()
}

// serveSlowly serves handler at path (serveWebhook) and sends one POST whose body goes out in 20
// pieces, as a proxy sends a large delivery over a slow link. The pacing sleep is the stand-in's
// own: it copies that slow sender, and it spreads the body over the listener's own read timeout
// (newHTTPServer's ReadTimeout, the production setting under test here) and two seconds more, so
// a body no route excuses is still arriving when that bound passes. It reports the status the
// server answered, or 0 when the connection closed without an answer, and how many bytes of the
// body had gone out by then.
func serveSlowly(t *testing.T, path string, handler http.Handler, headers map[string]string, body []byte) (status, sent int) {
	t.Helper()
	addr := serveWebhook(t, path, handler)
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	var head strings.Builder
	fmt.Fprintf(&head, "POST %s HTTP/1.1\r\nHost: listener\r\nContent-Type: application/json\r\nContent-Length: %d\r\n", path, len(body))
	for k, v := range headers {
		fmt.Fprintf(&head, "%s: %s\r\n", k, v)
	}
	head.WriteString("\r\n")
	if _, err := io.WriteString(conn, head.String()); err != nil {
		t.Fatal(err)
	}
	over := newHTTPServer(nil).ReadTimeout + 2*time.Second
	const pieces = 20
	size := (len(body) + pieces - 1) / pieces
	for piece := range pieces {
		from := min(piece*size, len(body))
		if from == len(body) {
			break
		}
		time.Sleep(over / pieces)
		written, err := conn.Write(body[from:min(from+size, len(body))])
		sent += written
		if err != nil {
			break // the server closed the connection: read whatever it answered
		}
	}
	// The read of the answer is bound by the shared wait's own deadline, not one of this test's.
	if deadline, ok := testwait.Context(t).Deadline(); ok {
		_ = conn.SetReadDeadline(deadline)
	}
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		return 0, sent
	}
	resp.Body.Close()
	return resp.StatusCode, sent
}

// A GitHub delivery whose body takes longer than the server's read timeout to arrive is read
// whole and published, not cut off with no answer: the route sets a deadline of its own
// (githubBodyDeadline) for the body it is owed.
func TestAGitHubDeliveryWhoseBodyArrivesOverMoreThanTenSecondsIsPublished(t *testing.T) {
	if testing.Short() {
		t.Skip("sends a body over longer than the listener's read timeout")
	}
	const secret = "s"
	body := []byte(`{"ref":"refs/heads/main","after":"` + strings.Repeat("a", 40) + `","repository":{"name":"legion","owner":{"login":"sjawhar"},"full_name":"sjawhar/legion"},"pad":"` + strings.Repeat("x", 3<<20) + `"}`)
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	pub := &slowBodyPublisher{}
	status, sent := serveSlowly(t, "/webhook/github", webhook.GitHubHandler(secret, "@legion", "", pub, noCI{}), map[string]string{
		"X-GitHub-Delivery":   "delivery-slow-body",
		"X-GitHub-Event":      "push",
		"X-Hub-Signature-256": "sha256=" + hex.EncodeToString(mac.Sum(nil)),
	}, body)
	if status != http.StatusOK {
		t.Fatalf("status = %d after %d of the body's %d bytes went out, want 200 (0 is a connection closed with no answer)", status, sent, len(body))
	}
	if sent != len(body) {
		t.Fatalf("the server took %d of the body's %d bytes before it closed the connection; want the whole body read", sent, len(body))
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
	logged := &lockedBuffer{}
	previous := log.Writer()
	log.SetOutput(logged)
	t.Cleanup(func() { log.SetOutput(previous) })
	addr := serveWebhook(t, "/webhook/github", webhook.GitHubHandler("s", "@legion", "", &slowBodyPublisher{}, noCI{}))
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.WriteString(conn, "POST /webhook/github HTTP/1.1\r\nHost: listener\r\nX-GitHub-Delivery: delivery-cut\r\nX-GitHub-Event: push\r\nContent-Length: 1000\r\n\r\n"+strings.Repeat("x", 300))
	conn.Close() // the sender goes away with 700 bytes still owed
	line := testwait.Until(t, "the line the cut body's failed read logs", func() (string, bool) {
		logged := logged.Lines("github body read failed")
		return logged, logged != ""
	})
	for _, want := range []string{"delivery=delivery-cut", "event=push", "content_length=1000", "read=300", "elapsed="} {
		if !strings.Contains(line, want) {
			t.Errorf("log = %q, want it to contain %q", line, want)
		}
	}
}

// Every other route keeps the server's own read timeout on its body: a Slack body still arriving
// when that bound passes is cut off with no answer, rather than read whole and served. The bound
// is newHTTPServer's, which this row pins, since it is the one the GitHub route excuses itself
// from (githubBodyDeadline) and every other route's body is read under.
func TestASlackBodyStillHasTheServersReadTimeout(t *testing.T) {
	if testing.Short() {
		t.Skip("sends a body over longer than the listener's read timeout")
	}
	if got := newHTTPServer(nil).ReadTimeout; got != 10*time.Second {
		t.Fatalf("the listener's HTTP server reads a body within %s, want 10s: the span serveSlowly spreads its body over, and the bound this row is about, are both read from it", got)
	}
	pub := &slowBodyPublisher{}
	status, sent := serveSlowly(t, "/webhook/slack", webhook.SlackHandler("s", pub), nil, []byte(`{"type":"url_verification","challenge":"`+strings.Repeat("c", 4000)+`"}`))
	if status == http.StatusOK {
		t.Fatalf("the Slack request was served 200 with %d of its %d body bytes out; want the server's read timeout to cut off a body that is still arriving when it passes", sent, 4000+len(`{"type":"url_verification","challenge":""}`))
	}
}

// lockedBuffer is a log destination the server's goroutines and the test can share.
type lockedBuffer = logtest.Buffer
