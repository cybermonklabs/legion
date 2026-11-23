package webhook

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/metrics"
)

// route is one webhook route under test: its handler over a publisher and a budget, and how it
// sends a delivery of body signed with secret, or unsigned when secret is empty.
type route struct {
	name    string
	handler func(secret string, publisher Publisher, bodies *BodyBudget) http.Handler
	request func(secret string, body io.Reader, size int) *http.Request
	// event is a body the route publishes once its signature checks.
	event []byte
}

// routes are the three webhook routes, each signing as its sender does.
var routes = []route{
	{
		name: "slack",
		handler: func(secret string, publisher Publisher, bodies *BodyBudget) http.Handler {
			return SlackHandler(secret, publisher, bodies)
		},
		request: func(secret string, body io.Reader, size int) *http.Request {
			data, signed := readSigned(body, secret != "")
			req := httptest.NewRequest(http.MethodPost, "/webhook/slack", body)
			ts := strconv.FormatInt(time.Now().Unix(), 10)
			req.Header.Set("X-Slack-Request-Timestamp", ts)
			if signed {
				req.Header.Set("X-Slack-Signature", slackSign(secret, ts, data))
				req.Body = io.NopCloser(bytes.NewReader(data))
			}
			req.ContentLength = int64(size)
			return req
		},
		event: []byte(`{"type":"event_callback","event_id":"Ev-budget","team_id":"T01234567",` +
			`"event":{"type":"message","channel":"C01234567","user":"U01234567","text":"hello","ts":"1700000000.000100"}}`),
	},
	{
		name: "github",
		handler: func(secret string, publisher Publisher, bodies *BodyBudget) http.Handler {
			return GitHubHandler(secret, "@legion", "", publisher, &mockRecorder{}, bodies)
		},
		request: func(secret string, body io.Reader, size int) *http.Request {
			data, signed := readSigned(body, secret != "")
			req := httptest.NewRequest(http.MethodPost, "/webhook/github", body)
			req.Header.Set("X-GitHub-Delivery", "d-budget")
			req.Header.Set("X-GitHub-Event", "push")
			if signed {
				req.Header.Set("X-Hub-Signature-256", githubSign(secret, data))
				req.Body = io.NopCloser(bytes.NewReader(data))
			}
			req.ContentLength = int64(size)
			return req
		},
		event: []byte(`{"ref":"refs/heads/main","after":"` + strings.Repeat("a", 40) +
			`","repository":{"name":"legion","owner":{"login":"sjawhar"},"full_name":"sjawhar/legion"}}`),
	},
	{
		name: "ghostwispr",
		handler: func(secret string, publisher Publisher, bodies *BodyBudget) http.Handler {
			return GhostWisprHandler(secret, publisher, bodies)
		},
		request: func(secret string, body io.Reader, size int) *http.Request {
			data, signed := readSigned(body, secret != "")
			req := httptest.NewRequest(http.MethodPost, "/webhook/ghostwispr", body)
			req.Header.Set("X-GhostWispr-Delivery", "d-budget")
			req.Header.Set("X-GhostWispr-Event", "summary_ready")
			if signed {
				req.Header.Set("X-GhostWispr-Signature", ghostWisprSign(secret, data))
				req.Body = io.NopCloser(bytes.NewReader(data))
			}
			req.ContentLength = int64(size)
			return req
		},
		event: []byte(`{"event_type":"summary_ready","payload":{"session_id":"20261122060000","type":"summary_ready"}}`),
	},
}

// readSigned reads body whole when the request is to be signed, since a signature covers the
// bytes; an unsigned request keeps body as it is, a reader that may stall or fail.
func readSigned(body io.Reader, sign bool) ([]byte, bool) {
	if !sign {
		return nil, false
	}
	data, _ := io.ReadAll(body)
	return data, true
}

// routeNamed is the route under test called name.
func routeNamed(t *testing.T, name string) route {
	t.Helper()
	for _, r := range routes {
		if r.name == name {
			return r
		}
	}
	t.Fatalf("no route %q", name)
	return route{}
}

// A route built with no secret has nothing to check a signature against, so it refuses every
// delivery 401 before it parses a byte, however the delivery is signed: a body that is not JSON
// gets the 401, never the 400 its JSON decode would answer. The listener never builds such a route,
// since its start refuses a route with no secret (LoadWebhookConfig), and this holds what each
// handler does if one is built anyway. The control rows build the route with a secret and sign the
// same body with it, which reaches the decode and is refused there, so the 401 says the signature
// check came first.
func TestARouteBuiltWithNoSecretRefusesEveryBodyUnparsed(t *testing.T) {
	notJSON := []byte("this is not JSON {")
	for _, r := range routes {
		for _, tc := range []struct {
			name, secret, signedWith string
			want                     int
		}{
			{name: "no secret, unsigned", want: http.StatusUnauthorized},
			{name: "no secret, signed with the empty key", signedWith: "", want: http.StatusUnauthorized},
			{name: "no secret, signed with some key", signedWith: "some-key", want: http.StatusUnauthorized},
			{name: "control: a secret, signed with it", secret: "s", signedWith: "s", want: http.StatusBadRequest},
		} {
			t.Run(r.name+", "+tc.name, func(t *testing.T) {
				pub := &mockPublisher{}
				req := r.request(tc.signedWith, bytes.NewReader(notJSON), len(notJSON))
				if tc.name == "no secret, signed with the empty key" {
					signEmpty(r.name, req, notJSON)
				}
				rr := httptest.NewRecorder()
				r.handler(tc.secret, pub, NewBodyBudget(DefaultBodyBudget, nil)).ServeHTTP(rr, req)
				t.Logf("answered %d %q", rr.Code, strings.TrimSpace(rr.Body.String()))
				if rr.Code != tc.want || len(pub.published) != 0 {
					t.Fatalf("status %d %q, published %d; want %d and nothing published", rr.Code, strings.TrimSpace(rr.Body.String()), len(pub.published), tc.want)
				}
			})
		}
	}
}

// signEmpty signs req's body with the empty key, as route name's sender would with no secret.
func signEmpty(name string, req *http.Request, body []byte) {
	switch name {
	case "slack":
		req.Header.Set("X-Slack-Signature", slackSign("", req.Header.Get("X-Slack-Request-Timestamp"), body))
	case "github":
		req.Header.Set("X-Hub-Signature-256", githubSign("", body))
	case "ghostwispr":
		req.Header.Set("X-GhostWispr-Signature", ghostWisprSign("", body))
	}
	req.Body = io.NopCloser(bytes.NewReader(body))
}

// Slack and Ghost Wispr bodies draw from the one budget GitHub bodies draw from, on the terms
// TestGitHubHandlerServesADeliveryBehindSlowSenders holds the GitHub route to: a body is charged as
// it arrives, at most twice what it has sent, and given back when its handler returns. Here three
// trickling GitHub senders and one that sent only headers hold their part of the budget when a
// Slack event and a Ghost Wispr event arrive on a slow link, each pausing after 4 KiB: each adds
// twice what it sent to the same budget, both are served once they resume, both give their part
// back, and a signed GitHub push is still served at once.
func TestSlackAndGhostWisprBodiesDrawFromTheBudgetGitHubBodiesDo(t *testing.T) {
	const (
		secret = "s"
		sent   = 4096
	)
	bodies := NewBodyBudget(DefaultBodyBudget, nil)
	pub := &mockPublisher{}
	github := GitHubHandler(secret, "@legion", "", pub, &mockRecorder{}, bodies)
	stall := make(chan struct{})
	var senders []*testBody
	for _, n := range []int{sent, sent, sent, 0} {
		senders = append(senders, &testBody{remaining: githubMaxBody, stallAfter: n, stall: stall})
	}
	_, done := serveUnsigned(t, github, senders, 20*time.Millisecond)
	defer func() {
		close(stall)
		<-done
	}()
	const trickling = 3 * 2 * sent
	awaitBudget(t, bodies, trickling, false)

	resume := make(chan struct{})
	var answered sync.WaitGroup
	held := int64(trickling)
	var recorders []*httptest.ResponseRecorder
	for _, r := range routes {
		if r.name == "github" {
			continue
		}
		// The event, padded so that it pauses with more of it still to come. The copy keeps the
		// route's own event as it is.
		body := append(bytes.Clone(bytes.TrimSuffix(r.event, []byte("}"))), []byte(`,"pad":"`+strings.Repeat("p", 3*sent)+`"}`)...)
		req := r.request(secret, bytes.NewReader(body), len(body))
		req.Body = io.NopCloser(&pausedBody{data: body, pauseAt: sent, resume: resume})
		rr := httptest.NewRecorder()
		recorders = append(recorders, rr)
		answered.Add(1)
		go func() {
			defer answered.Done()
			r.handler(secret, pub, bodies).ServeHTTP(rr, req)
		}()
		held += 2 * sent
		awaitBudget(t, bodies, held, false)
		t.Logf("a %s body paused after %d bytes: the budget holds %d bytes", r.name, sent, held)
	}
	close(resume)
	answered.Wait()
	for i, rr := range recorders {
		if rr.Code != http.StatusOK {
			t.Fatalf("event %d: status %d %q, want 200", i, rr.Code, strings.TrimSpace(rr.Body.String()))
		}
	}
	awaitBudget(t, bodies, trickling, false)
	pub.mu.Lock()
	published := len(pub.published)
	pub.mu.Unlock()
	if published != 2 {
		t.Fatalf("published %d envelopes, want the Slack event's and the Ghost Wispr event's", published)
	}
	took := requireServedPushes(t, github, secret, "three trickling senders, one that sent only headers and two served events", 1)
	t.Logf("push served in %s", took.Round(time.Millisecond))
}

// shortRoomWaits shortens every route's room wait to wait for the rest of the test.
func shortRoomWaits(t *testing.T, wait time.Duration) {
	t.Helper()
	slack, github, ghost := slackRoomWait, githubRoomWait, ghostWisprRoomWait
	slackRoomWait, githubRoomWait, ghostWisprRoomWait = wait, wait, wait
	t.Cleanup(func() { slackRoomWait, githubRoomWait, ghostWisprRoomWait = slack, github, ghost })
}

// A delivery whose body does not fit the budget waits its route's room wait for room, and is then
// answered 503 with Retry-After, having published nothing, and the budget counts the refusal by
// route. Here an unsigned GitHub sender has sent twice a 64 KiB budget and stalled, holding the
// budget past its limit, and a signed delivery to each route is refused. The room waits are
// shortened, and only a room wait answers 503 (errNoRoom), so the 503 itself says the room wait
// ended the wait. Each request carries a 5 s deadline, so a wait for room that nothing bounds ends
// in a failed status rather than a hung test. A delivery that waits while the holder finishes gets
// the room and is served.
func TestADeliveryThatFindsNoRoomIsAnswered503AfterItsRoomWait(t *testing.T) {
	const (
		secret = "s"
		budget = 64 << 10
	)
	shortRoomWaits(t, 50*time.Millisecond)
	refusedCount := metrics.New()
	bodies := NewBodyBudget(budget, refusedCount.NewCounter("refused", "test"))
	pub := &mockPublisher{}
	stall := make(chan struct{})
	stallOnce := sync.OnceFunc(func() { close(stall) })
	defer stallOnce()
	holder := GitHubHandler(secret, "@legion", "", pub, &mockRecorder{}, bodies)
	codes, done := serveUnsigned(t, holder, []*testBody{{remaining: githubMaxBody, stallAfter: 2 * budget, stall: stall}}, 0)
	awaitBudget(t, bodies, 4*budget, true)

	for _, r := range routes {
		t.Run(r.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			req := r.request(secret, bytes.NewReader(r.event), len(r.event)).WithContext(ctx)
			rr := httptest.NewRecorder()
			r.handler(secret, pub, bodies).ServeHTTP(rr, req)
			t.Logf("answered %d %q, Retry-After %q", rr.Code, strings.TrimSpace(rr.Body.String()), rr.Header().Get("Retry-After"))
			if rr.Code != http.StatusServiceUnavailable || rr.Header().Get("Retry-After") != bodyRetryAfter {
				t.Fatalf("status %d, Retry-After %q; want 503 with Retry-After %s", rr.Code, rr.Header().Get("Retry-After"), bodyRetryAfter)
			}
		})
	}
	pub.mu.Lock()
	refused := len(pub.published)
	pub.mu.Unlock()
	if refused != 0 {
		t.Fatalf("published %d envelopes from refused deliveries, want none", refused)
	}
	scrape := httptest.NewRecorder()
	refusedCount.Handler().ServeHTTP(scrape, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	for _, path := range []string{"/webhook/slack", "/webhook/github", "/webhook/ghostwispr"} {
		if want := `refused{path="` + path + `"} 1`; !strings.Contains(scrape.Body.String(), want) {
			t.Fatalf("the refusal counter reads\n%s\nwant a line %s", scrape.Body.String(), want)
		}
	}

	// The waiting delivery gets a room wait long enough to outlast the holder's end.
	githubRoomWait = 10 * time.Second
	github := routeNamed(t, "github")
	req := github.request(secret, bytes.NewReader(github.event), len(github.event))
	rr := httptest.NewRecorder()
	answered := make(chan struct{})
	go func() {
		github.handler(secret, pub, bodies).ServeHTTP(rr, req)
		close(answered)
	}()
	time.Sleep(100 * time.Millisecond)
	stallOnce()
	<-answered
	<-done
	t.Logf("the delivery waiting while the holder finished answered %d; the holder answered %d", rr.Code, codes[0])
	if rr.Code != http.StatusOK {
		t.Fatalf("status %d %q, want 200 once the holder finished", rr.Code, strings.TrimSpace(rr.Body.String()))
	}
	awaitBudget(t, bodies, 0, false)
}

// A delivery that ends before its handler publishes anything gives back everything its body
// charged, so the room it held goes to the next delivery and is not lost to the budget for good.
// Each row's body goes past a 16 KiB budget before it ends, so the budget held it (mostHeld), and
// the budget holds nothing once the handler has returned.
func TestADeliveryThatEndsEarlyGivesBackWhatItCharged(t *testing.T) {
	const budget = 16 << 10
	failed := make(chan struct{})
	close(failed)
	for _, r := range routes {
		for _, tc := range []struct {
			name string
			req  func() *http.Request
			want int
		}{
			{
				name: "a wrong signature",
				req: func() *http.Request {
					body := bytes.Repeat([]byte("x"), 4*budget)
					req := r.request("other-secret", bytes.NewReader(body), len(body))
					return req
				},
				want: http.StatusUnauthorized,
			},
			{
				name: "a body cut short",
				req: func() *http.Request {
					return r.request("", &testBody{remaining: 4 * budget, stallAfter: 2 * budget, stall: failed}, 4*budget)
				},
				want: http.StatusBadRequest,
			},
			{
				name: "an undeclared body over the route's cap",
				req: func() *http.Request {
					req := r.request("", &testBody{remaining: githubMaxBody + 1, stallAfter: -1}, 0)
					req.ContentLength = -1
					return req
				},
				want: http.StatusRequestEntityTooLarge,
			},
		} {
			t.Run(r.name+", "+tc.name, func(t *testing.T) {
				bodies := NewBodyBudget(budget, nil)
				rr := httptest.NewRecorder()
				r.handler("s", &mockPublisher{}, bodies).ServeHTTP(rr, tc.req())
				bodies.mu.Lock()
				held, taken := bodies.held, bodies.over != nil
				bodies.mu.Unlock()
				most := bodies.mostHeld()
				t.Logf("answered %d %q; held at most %d bytes, now %d", rr.Code, strings.TrimSpace(rr.Body.String()), most, held)
				if rr.Code != tc.want {
					t.Fatalf("status %d, want %d", rr.Code, tc.want)
				}
				if most <= budget {
					t.Fatalf("the body charged at most %d bytes, want past the %d-byte budget, or this row tests no release", most, budget)
				}
				if held != 0 || taken {
					t.Fatalf("after the handler returned the budget holds %d bytes with the right to go past it taken %t, want nothing", held, taken)
				}
			})
		}
	}
}
