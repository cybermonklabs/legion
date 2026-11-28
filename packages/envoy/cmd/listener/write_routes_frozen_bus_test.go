package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/clock"
	"github.com/sjawhar/envoy/internal/clock/clocktest"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/logging"
	"github.com/sjawhar/envoy/internal/session"
	"github.com/sjawhar/envoy/internal/store"
	"github.com/sjawhar/envoy/internal/testnats"
	"github.com/sjawhar/envoy/internal/testwait"
	"github.com/sjawhar/envoy/listenerwire"
)

// writeRouteRig is a listener's stores and /v1 routes on a bus reached through the relay
// TestASendAndAPublishAnswerABusThatStoppedAnsweringInsideTheBudget freezes.
type writeRouteRig struct {
	relay    *listenerRelay
	registry *store.Registry
	sessions *session.SessionRegistry
	mux      *http.ServeMux
}

func newWriteRouteRig(t *testing.T, clk clock.Clock) *writeRouteRig {
	t.Helper()
	relay := startListenerRelay(t, testnats.URL(t))
	client, err := bus.ConnectOwningStream([]string{relay.relayed}, bus.WithReplicas(1))
	if err != nil {
		t.Fatalf("connect the listener's bus through the relay: %v", err)
	}
	t.Cleanup(client.Close)
	stores := openStampRig(t, client)
	mux := http.NewServeMux()
	// The session consumers connect to the server directly, not through the relay: the freeze
	// under test is the store's, and a registration reaches the store only once it has its
	// consumer.
	consumers := setupSessionConsumers(t, client)
	consumers.clock = clk
	registerV1Routes(mux, withRoleClaimDrain(t, &listenerDeps{
		client: client, registry: stores.registry, sessions: stores.sessions, consumers: consumers, clock: clk,
	}), "test-machine", logging.New("test"))
	return &writeRouteRig{relay: relay, registry: stores.registry, sessions: stores.sessions, mux: mux}
}

func (rig *writeRouteRig) register(t *testing.T, ids ...string) {
	t.Helper()
	for _, id := range ids {
		if err := rig.sessions.Put(id, session.SessionEntry{SelfSubscribed: true, MachineID: "test-machine", Dir: "/test/" + id, Title: id}); err != nil {
			t.Fatalf("register %s: %v", id, err)
		}
		if _, err := rig.sessions.Get(id); err != nil {
			t.Fatalf("the listener's session cache does not hold %s (%v)", id, err)
		}
	}
}

func (rig *writeRouteRig) answer(t *testing.T, method, path, body string) <-chan *httptest.ResponseRecorder {
	t.Helper()
	return routeAnswer(t, rig.mux.ServeHTTP, httptest.NewRequest(method, path, strings.NewReader(body)))
}

// Expire only after the relay has swallowed this request's bus call.
func expireFrozenRoute(t *testing.T, fake *clocktest.Fake, relay *listenerRelay, route string, deadline time.Time) {
	t.Helper()
	fake.AwaitArmedAt(t, route+" budget", deadline)
	testwait.Receive(t, "the relay to swallow the bus call for "+route, relay.swallowed)
	fake.Set(deadline)
}

// wantWriteBusUnanswered asserts the named 503 with Retry-After: 1.
func wantWriteBusUnanswered(t *testing.T, route string, recorder *httptest.ResponseRecorder) {
	t.Helper()
	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("%s with the bus silent: status %d, body %s; want 503", route, recorder.Code, recorder.Body.String())
	}
	var refusal listenerwire.APIError
	if err := json.Unmarshal(recorder.Body.Bytes(), &refusal); err != nil {
		t.Fatalf("decode the refusal %s: %v", recorder.Body.String(), err)
	}
	if !strings.HasPrefix(refusal.Error, busUnansweredPrefix) {
		t.Fatalf("the refusal reads %q, want it to begin %q", refusal.Error, busUnansweredPrefix)
	}
	if got := recorder.Header().Get("Retry-After"); got != "1" {
		t.Fatalf("Retry-After = %q, want 1", got)
	}
	t.Logf("%s with the bus silent: 503: %s", route, refusal.Error)
}

// Every /v1 route that writes - subscribe, unsubscribe, DELETE /v1/interests/<session_id>, POST
// /v1/roles/set and DELETE /v1/sessions/<session_id> - does its store work within
// sendRequestBudget of the request's own context. nats.go's KV reads and writes take no context
// and wait out the JetStream MaxWait (10 s), as long as this listener's HTTP write timeout, so on a
// bus that went silent without closing the connection each route must still answer the named 503
// with Retry-After: 1 inside the caller's 5 s attempt.
//
// The record each frozen request needs is not in the interest cache, so the read it makes first
// goes to the bucket; for subscribe and unsubscribe a second request names a session whose record
// is cached, so the plan or the unsubscribe's first read is answered from memory and the write's
// own work is what meets the silent bus - the row read a registration's compare-and-swap is built
// from and fenced on (store.Registry.UpsertPlanned), and the unsubscribe's write itself. POST
// /v1/roles/set first reads the claimant's session from the bucket, and DELETE
// /v1/sessions/<session_id> reads before it deletes. Each row controls for a bad request by
// serving the same route before the freeze for a session of its own.
func TestTheWriteRoutesAnswerABusThatStoppedAnsweringInsideTheBudget(t *testing.T) {
	t.Parallel()
	const (
		topic      = "notifications.team.write-routes"
		otherTopic = "notifications.team.write-routes-other"
		control    = "ses_write_routes_control"
		target     = "ses_write_routes_frozen"
	)
	subscribe := func(id, topic string) string {
		return `{"session_id":"` + id + `","topics":["` + topic + `"],"self_subscribed":true,"delivery":{"consumer":true,"epoch":"e"}}`
	}
	unsubscribe := func(id, topic string) string {
		return `{"session_id":"` + id + `","topics":["` + topic + `"]}`
	}

	interest := func(id string) string { return "/v1/interests/" + id }
	sessionPath := func(id string) string { return "/v1/sessions/" + id }
	claim := func(id string) string { return `{"session_id":"` + id + `","role":"write-routes"}` }
	t.Run("frozen", func(t *testing.T) {
		for _, row := range []struct {
			name, method string
			path, body   func(string) string
			want         int
			registered   bool
			cached       bool
		}{
			{"subscribe", http.MethodPost, func(string) string { return "/v1/interests/subscribe" }, func(id string) string { return subscribe(id, topic) }, http.StatusOK, false, false},
			{"subscribe a cached session", http.MethodPost, func(string) string { return "/v1/interests/subscribe" }, func(id string) string { return subscribe(id, "notifications.team.write-routes-new") }, http.StatusOK, false, true},
			{"unsubscribe", http.MethodPost, func(string) string { return "/v1/interests/unsubscribe" }, func(id string) string { return unsubscribe(id, topic) }, http.StatusOK, false, false},
			{"unsubscribe a cached session", http.MethodPost, func(string) string { return "/v1/interests/unsubscribe" }, func(id string) string { return unsubscribe(id, otherTopic) }, http.StatusOK, false, true},
			{"delete interests", http.MethodDelete, interest, func(string) string { return "" }, http.StatusNoContent, false, false},
			{"claim a role", http.MethodPost, func(string) string { return "/v1/roles/set" }, claim, http.StatusOK, true, false},
			{"delete a session", http.MethodDelete, sessionPath, func(string) string { return "" }, http.StatusOK, true, false},
		} {
			t.Run(row.name, func(t *testing.T) {
				t.Parallel()
				fake := clocktest.New(time.Unix(0, 0))
				rig := newWriteRouteRig(t, fake)
				rig.register(t, control)
				if row.registered {
					rig.register(t, target)
				}
				seed := func(id string) {
					t.Helper()
					if _, err := rig.registry.Upsert(store.Interest{SessionID: id, MachineID: "test-machine", Dir: "/test/" + id},
						[]string{contracts.LegacyAgentSubject(id), topic, otherTopic}); err != nil {
						t.Fatalf("write %s's interest record: %v", id, err)
					}
				}
				seed(control)
				if row.cached {
					seed(target)
				}
				if recorder := testwait.Receive(t, "the control "+row.name+" to answer", rig.answer(t, row.method, row.path(control), row.body(control))); recorder.Code != row.want {
					t.Fatalf("%s %s with the bus answering: status %d, body %s; want %d", row.method, row.path(control), recorder.Code, recorder.Body.String(), row.want)
				}
				if _, cached := rig.registry.Cached(target); cached != row.cached {
					t.Fatalf("%s's interest record cached = %t, want %t", target, cached, row.cached)
				}

				rig.relay.arm(target, 1)
				route := row.method + " " + row.path(target)
				deadline := fake.Now().Add(sendRequestBudget)
				answered := rig.answer(t, row.method, row.path(target), row.body(target))
				expireFrozenRoute(t, fake, rig.relay, route, deadline)
				wantWriteBusUnanswered(t, route, testwait.Receive(t, route+" to refuse the silent bus at its budget", answered))
			})
		}
	})
}

// POST /v1/roles/set's claim (store.Registry.SetRoleWithPrevious) and a soft claim's read of the
// role's holder are role-bucket calls the claimant's registration write does not reach, so each
// is frozen on its own here: the relay passes everything up to the first protocol line naming the
// role bucket and swallows that line and every one after it. For the claim's read that is the
// direct get of the claim, published to $JS.API.DIRECT.GET; for the claim's write it is the
// hard claim's conditional publish of the claim to its own subject, which nats.go sends after
// the read and the claimant's interest write have answered, so the write itself is what meets the
// silent bus, and the rollback that follows a write that did not answer must not take the request
// past its budget either. The negative control is a hard claim of another role
// (role-store-control) by the same claimant on the same rig before the freeze: it is served.
func TestAClaimWhoseRoleStoreStopsAnsweringIsAnsweredInsideTheBudget(t *testing.T) {
	t.Parallel()
	const (
		claimant = "ses_role_store_claimant"
		path     = "/v1/roles/set"
	)
	for _, freeze := range []struct {
		name, trigger, body string
		onClock             bool
	}{
		{"the claim's read", "role-store-read", `{"session_id":"` + claimant + `","role":"role-store-read"}`, false},
		{"a soft claim's read", "role-store-soft", `{"session_id":"` + claimant + `","role":"role-store-soft","soft":true}`, true},
		{"the claim's write", "PUB $KV." + bus.RoleClaimBucket + ".role-store-write", `{"session_id":"` + claimant + `","role":"role-store-write"}`, false},
	} {
		t.Run(freeze.name, func(t *testing.T) {
			t.Parallel()
			var clk clock.Clock = clock.Real()
			fake := clocktest.New(time.Unix(0, 0))
			if freeze.onClock {
				clk = fake
			}
			rig := newWriteRouteRig(t, clk)
			rig.register(t, claimant)
			if recorder := testwait.Receive(t, "the control role claim to answer", rig.answer(t, http.MethodPost, path, `{"session_id":"`+claimant+`","role":"role-store-control"}`)); recorder.Code != http.StatusOK {
				t.Fatalf("POST /v1/roles/set with the bus answering: status %d, body %s; want 200", recorder.Code, recorder.Body.String())
			}
			rig.relay.arm(freeze.trigger, 1)
			const route = "POST " + path
			if freeze.onClock {
				deadline := fake.Now().Add(sendRequestBudget)
				answered := rig.answer(t, http.MethodPost, path, freeze.body)
				expireFrozenRoute(t, fake, rig.relay, route, deadline)
				wantWriteBusUnanswered(t, route, testwait.Receive(t, "the soft claim to refuse the unanswered role read at its budget", answered))
				return
			}
			// The store's tries and detached rollback use real deadlines (INFRA-6348).
			// Shorten the caller's attempt as an input to the route, not the test's wait.
			request := httptest.NewRequest(http.MethodPost, path, strings.NewReader(freeze.body))
			answered := routeAnswer(t, func(w http.ResponseWriter, r *http.Request) {
				ctx, cancel := context.WithTimeout(r.Context(), 500*time.Millisecond)
				defer cancel()
				rig.mux.ServeHTTP(w, r.WithContext(ctx))
			}, request)
			testwait.Receive(t, "the relay to swallow "+freeze.name, rig.relay.swallowed)
			recorder := testwait.Receive(t, "the hard claim to refuse the unanswered role store", answered)
			wantWriteBusUnanswered(t, route, recorder)
		})
	}
}

// store.Registry.SetRoleWithPrevious joins a claim that lost its conditional write
// (nats.ErrKeyExists) with its holder read and its rollback (errors.Join). errors.Is finds a store
// answer in any part of a join, so a lost claim joined with a call the bus did not answer was
// answered as the store's own, a 500 with no Retry-After. A join is a store answer only when every
// part of it is; anything else is the bus-unavailable 503 with Retry-After: 1. The controls: a join
// of store answers, a store answer wrapped alone, and a store answer wrapped with its detail by one
// fmt.Errorf (an unreadable record and its decode error) are each still the store's answer.
func TestAJoinedStoreErrorIsTheStoresAnswerOnlyWhenEveryPartIs(t *testing.T) {
	t.Parallel()
	dropped := &net.OpError{Op: "read", Net: "tcp", Err: errors.New("connection reset by peer")}
	for _, c := range []struct {
		name     string
		err      error
		answered bool
	}{
		{"a lost claim joined with a dropped connection", errors.Join(nats.ErrKeyExists, dropped), false},
		{"a lost claim joined with no responders", errors.Join(nats.ErrKeyExists, nats.ErrNoResponders), false},
		{"a lost claim joined with a join that holds no responders", errors.Join(nats.ErrKeyExists, errors.Join(nats.ErrKeyNotFound, nats.ErrNoResponders)), false},
		{"a wrapped join of a lost claim and a dropped connection", fmt.Errorf("claim: %w", errors.Join(nats.ErrKeyExists, dropped)), false},
		{"a dropped connection alone", dropped, false},
		{"a lost claim joined with no key", errors.Join(nats.ErrKeyExists, nats.ErrKeyNotFound), true},
		{"a lost claim alone", fmt.Errorf("write role r's claim: %w", nats.ErrKeyExists), true},
		{"an unreadable record and its decode error", fmt.Errorf("%w: %w", store.ErrMalformedInterest, errors.New("unexpected end of JSON input")), true},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := storeAnswered(context.Background(), c.err); got != c.answered {
				t.Fatalf("storeAnswered(%q) = %t, want %t", c.err, got, c.answered)
			}
			recorder := httptest.NewRecorder()
			writeStoreError(context.Background(), recorder, c.err, http.StatusInternalServerError)
			code, retry := recorder.Code, recorder.Header().Get("Retry-After")
			if c.answered && (code == http.StatusServiceUnavailable || retry != "") {
				t.Fatalf("the store's answer %q was answered %d with Retry-After %q, want the route's own status", c.err, code, retry)
			}
			if !c.answered && (code != http.StatusServiceUnavailable || retry != "1") {
				t.Fatalf("%q was answered %d with Retry-After %q (%s), want 503 with Retry-After: 1", c.err, code, retry, recorder.Body.String())
			}
		})
	}
}
