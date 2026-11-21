package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/broker/brokertest"
)

// dispatchCall is one call the broker made to Dispatch's access routes: its path, the credential it
// presented and the status Dispatch answered.
type dispatchCall struct {
	path, authorization string
	status              int
}

// dispatchCalls keeps every call the broker makes to Dispatch's access routes, on a rig built
// withBrokerGate, and the read token the last of them presented.
type dispatchCalls struct {
	mu        sync.Mutex
	calls     []dispatchCall
	readToken atomic.Pointer[string]
}

func (c *dispatchCalls) all() []dispatchCall {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]dispatchCall(nil), c.calls...)
}

// withDispatchCalls records into calls every call the broker makes to Dispatch's access routes.
func withDispatchCalls(calls *dispatchCalls) leaverOption {
	return func(c *leaverConfig) {
		c.toDispatch = append(c.toDispatch, func(_ *testing.T, _ *leaverRig, next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				authorization := req.Header.Get("Authorization")
				if token, ok := strings.CutPrefix(authorization, "DPoP "); ok {
					calls.readToken.Store(&token)
				}
				recorder := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
				next.ServeHTTP(recorder, req)
				calls.mu.Lock()
				calls.calls = append(calls.calls, dispatchCall{req.URL.Path, authorization, recorder.status})
				calls.mu.Unlock()
			})
		})
	}
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (s *statusRecorder) WriteHeader(status int) {
	s.status = status
	s.ResponseWriter.WriteHeader(status)
}

// delay is a wait a test turns on and off: while it is on, each call it holds waits until its
// caller hangs up, and done receives once that call has ended, so a test knows a held call no
// longer holds anything when it turns the next delay on.
type delay struct {
	on   atomic.Bool
	done chan struct{}
}

func newDelay() *delay { return &delay{done: make(chan struct{}, 8)} }

// hold waits while d is on, until req's caller hangs up, and reports whether it held req. It
// reads req's body first, so net/http notices the caller hang up: a server whose handler has not
// read a request's body never ends that request's context when its connection closes.
func (d *delay) hold(req *http.Request) bool {
	if !d.on.Load() {
		return false
	}
	_, _ = io.Copy(io.Discard, req.Body)
	<-req.Context().Done()
	d.done <- struct{}{}
	return true
}

// withPersonRouteDelay holds each of the broker's calls to Dispatch's person route while d is on:
// the standing check, inside the broker's own ask.
func withPersonRouteDelay(d *delay) leaverOption {
	return func(c *leaverConfig) {
		c.toDispatch = append(c.toDispatch, func(_ *testing.T, _ *leaverRig, next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				if req.URL.Path == "/api/v1/access/person" && d.hold(req) {
					return
				}
				next.ServeHTTP(w, req)
			})
		})
	}
}

// withReadTokenIntrospectionDelay holds each of Dispatch's introspections of the broker's read
// token (the token calls last saw presented) while d is on: the nested introspection Dispatch makes
// of the token the broker asks its person route with.
func withReadTokenIntrospectionDelay(d *delay, calls *dispatchCalls) leaverOption {
	return func(c *leaverConfig) {
		c.toBroker = append(c.toBroker, func(_ *testing.T, _ *leaverRig, next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				if req.Method == http.MethodPost && req.URL.Path == "/v1/oauth/introspect" && d.on.Load() {
					body, _ := io.ReadAll(req.Body)
					req.Body = io.NopCloser(bytes.NewReader(body))
					form, _ := url.ParseQuery(string(body))
					if token := calls.readToken.Load(); token != nil && form.Get("token") == *token && d.hold(req) {
						return
					}
				}
				next.ServeHTTP(w, req)
			})
		})
	}
}

// withTradeDelay puts a server of its own between the broker's read token and the broker's token
// endpoint, which holds each trade while d is on: the self-exchange inside the broker's own ask. It
// must come before withBrokerGate, whose broker reads where its trades connect (tradeDialURL).
func withTradeDelay(d *delay) leaverOption {
	return func(c *leaverConfig) {
		c.broker = append(c.broker, func(t *testing.T, r *leaverRig) []brokertest.Option {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				if d.hold(req) {
					return
				}
				target, err := url.Parse(r.broker.URL)
				if err != nil {
					http.Error(w, err.Error(), http.StatusBadGateway)
					return
				}
				httputil.NewSingleHostReverseProxy(target).ServeHTTP(w, req)
			}))
			t.Cleanup(server.Close)
			r.tradeDialURL = server.URL
			return nil
		})
	}
}

// introspectReadToken is the broker's answer about token at its introspection route, asked as
// Dispatch asks it, with the account token the broker rig lists for Dispatch.
func (r *leaverRig) introspectReadToken(t *testing.T, token string) map[string]any {
	t.Helper()
	request, err := http.NewRequestWithContext(t.Context(), http.MethodPost, r.broker.URL+"/v1/oauth/introspect", strings.NewReader(url.Values{"token": {token}}.Encode()))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+r.broker.UIToken)
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("introspect the broker's read token: %v", err)
	}
	defer response.Body.Close()
	var answer map[string]any
	if err := json.NewDecoder(response.Body).Decode(&answer); err != nil || response.StatusCode != http.StatusOK {
		t.Fatalf("introspect the broker's read token = %d, %v, want 200 with an answer", response.StatusCode, err)
	}
	return answer
}

// TestColdPersonAccessThroughSelfExchange: a broker that holds no read token and no answer about
// anyone asks Dispatch about bob, a person Dispatch holds no record of, through the production
// path with no DISPATCH_OIDC_* setting: its gate's ask trades the broker's projected token at its
// own token endpoint for a read token, presents it to Dispatch's person route under the DPoP scheme
// at Dispatch's internal address, signed for its canonical URL, and Dispatch, which checks the token
// at the broker, answers 200, so the gate serves bob. The token the broker presented is live, holds
// the scope read alone and the project scope none, names the broker's account as its service and
// its read entry's session, and names no person, which is why its own introspection asks Dispatch
// nothing. Dispatch checking a person-bearing broker token through this chain is every leaver-hold
// row built withBrokerGate (TestLeaverHoldBrokerCredentials, TestIAPLeaverHoldThroughTheRealBroker).
//
// Controls: the read entry dropped from the broker rig's exchange set: the trade is refused and the
// gate answers hold.ErrUncheckable. DISPATCH_ACCESS_ACCOUNTS naming another account: Dispatch
// answers 403. The read entry's decision naming a person: the claim assertion fails.
func TestColdPersonAccessThroughSelfExchange(t *testing.T) {
	calls := &dispatchCalls{}
	r := newLeaverRig(t, withBrokerGate(), withDispatchCalls(calls))
	if err := r.broker.Hold.Check(t.Context(), bobEmail); err != nil {
		t.Fatalf("the broker's gate, cold, asked about %s = %v, want nil: Dispatch answers him not held", bobEmail, err)
	}
	asked := calls.all()
	if len(asked) != 1 || asked[0].path != "/api/v1/access/person" || asked[0].status != http.StatusOK || !strings.HasPrefix(asked[0].authorization, "DPoP ") {
		t.Fatalf("the broker's calls to Dispatch = %+v, want one 200 GET of the person route under the DPoP scheme", asked)
	}
	token := strings.TrimPrefix(asked[0].authorization, "DPoP ")
	answer := r.introspectReadToken(t, token)
	if answer["active"] != true || answer["scope"] != "read" || answer["project_scope"] != "none" ||
		answer["service"] != brokertest.BrokerAccount || answer["session"] != brokertest.BrokerSession {
		t.Fatalf("the broker's read token introspects as %v, want a live read-only token of %s's read entry", answer, brokertest.BrokerAccount)
	}
	if person, named := answer["person"]; named && person != "" {
		t.Fatalf("the broker's read token names the person %v, want none: a token naming a person would be held to the answer it exists to get", person)
	}
	if projects, named := answer["projects"]; named && projects != nil {
		t.Fatalf("the broker's read token names the projects %v, want none", projects)
	}
}

// TestPersonAccessNestedIntrospectionIsBounded: with the broker's read token due for renewal and its
// answer about bob past the cache, Dispatch checking bob's own broker token asks the broker's
// introspection, whose hold asks Dispatch's person route through the self-exchange. Each nested
// step is held in turn until its caller hangs up — the self-exchange, Dispatch's introspection of the
// read token, and the person route itself — and each time the gate gives up at its 3-second ask,
// serves bob on the answer it kept, and Dispatch answers bob's token 200 within its own 5-second
// bound on the introspection, never 503 BROKER_UNAVAILABLE.
//
// Control: hold.askTimeout at 10 seconds: the introspection outlives Dispatch's bound and bob's
// token is answered 503 BROKER_UNAVAILABLE.
func TestPersonAccessNestedIntrospectionIsBounded(t *testing.T) {
	calls := &dispatchCalls{}
	trade, introspection, standing := newDelay(), newDelay(), newDelay()
	r := newLeaverRig(t, withTradeDelay(trade), withBrokerGate(), withDispatchCalls(calls),
		withReadTokenIntrospectionDelay(introspection, calls), withPersonRouteDelay(standing))
	bob := r.person(t, bobEmail)
	for _, c := range []struct {
		name  string
		delay *delay
	}{
		{"the broker's trade for its read token", trade},
		{"Dispatch's introspection of the broker's read token", introspection},
		{"Dispatch's person route", standing},
	} {
		// A token of bob's Dispatch has never checked, minted before anything is held, so Dispatch
		// asks the broker about it; then the broker's clock moves past its read token's renewal and
		// its answer about bob, so its hold asks Dispatch again through a fresh trade.
		token, status, code := r.exchange(t, bob)
		if status != http.StatusOK {
			t.Fatalf("%s: mint a fresh broker token of bob's = %d %s", c.name, status, code)
		}
		bob.brokerToken = token
		r.brokerAhead.Add(int64(11 * time.Minute))
		c.delay.on.Store(true)
		start := time.Now()
		status, code = r.brokerTokenAtDispatch(t, bob)
		elapsed := time.Since(start)
		c.delay.on.Store(false)
		t.Logf("%s held: bob's broker token at Dispatch answered %d %q in %s", c.name, status, code, elapsed)
		if status != http.StatusOK {
			t.Errorf("%s held: bob's broker token at Dispatch = %d %s, want 200: the gate's 3-second ask gives up inside Dispatch's 5-second introspection bound and serves bob on his kept answer", c.name, status, code)
		}
		select {
		case <-c.delay.done:
		case <-time.After(30 * time.Second):
			t.Fatalf("%s: the held call never ended, so it was never held or its caller never hung up", c.name)
		}
	}
}
