package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/ciphertoken/ciphertokentest"
	"github.com/sjawhar/envoy/internal/logging"
)

// switchableSpentProofs is a spent-proof store that answers nothing and records nothing while it
// is down, as one that did not reach the bus leaves it, and the in-memory store otherwise.
type switchableSpentProofs struct {
	down atomic.Bool
	spentProofsInMemory
}

func (s *switchableSpentProofs) CheckAndSetDPoPProofUsed(ctx context.Context, jti, jkt, nonce, htm, htu string, exp time.Time) (bool, error) {
	if s.down.Load() {
		return false, errors.New("nats: timeout")
	}
	return s.spentProofsInMemory.CheckAndSetDPoPProofUsed(ctx, jti, jkt, nonce, htm, htu, exp)
}

// A 503 the listener answers before a proof is spent leaves the proof unspent, so the one retry
// Google's balancer sends of a body-less request after it, with the same proof, is served: the
// closed /v1 gate's 503 (a list read's refilling answer and every other request's "service
// starting") is answered before apiAuth reads the request, and a broker or a spent-proof store
// that did not answer is the 503 of a check that stopped before the proof was recorded.
func TestEdgeRetryOfAProofAfterAPreAuth503IsServed(t *testing.T) {
	t.Parallel()
	logger := logging.NewWithWriter("test-machine", io.Discard)
	caller := ciphertokentest.NewCaller(t)
	broker := edgeLBBroker(t, caller)
	served := http.NewServeMux()
	served.Handle("/v1/", noContent)
	retry := func(t *testing.T, handler http.Handler, p edgeLBPresentation, first int, between func()) {
		t.Helper()
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, p.through("35.191.7.7:40001", forwardedFor("203.0.113.9")))
		if recorder.Code != first {
			t.Fatalf("the first presentation: status %d body %s, want %d", recorder.Code, recorder.Body.String(), first)
		}
		between()
		recorder = httptest.NewRecorder()
		handler.ServeHTTP(recorder, p.through("130.211.1.2:40002", forwardedFor("203.0.113.9")))
		if recorder.Code != http.StatusNoContent {
			t.Fatalf("the balancer's retry of the same proof: status %d body %s, want 204", recorder.Code, recorder.Body.String())
		}
	}

	for _, path := range []string{"/v1/registry/ses_collaborator", "/v1/sessions"} {
		t.Run("the closed gate, GET "+path, func(t *testing.T) {
			gate := newStartingGate("v1", logger)
			handler := v1API(edgeLBAuth(edgeLBTokens(t, broker.Server.URL, &spentProofsInMemory{}), logger), gate)
			retry(t, handler, presentEdgeLB(t, caller, http.MethodGet, path, edgeLBToken, ""), http.StatusServiceUnavailable, func() { gate.open(served) })
		})
	}

	t.Run("the broker that did not answer", func(t *testing.T) {
		upstream, err := url.Parse(broker.Server.URL)
		if err != nil {
			t.Fatal(err)
		}
		var down atomic.Bool
		down.Store(true)
		proxy := httputil.NewSingleHostReverseProxy(upstream)
		front := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if down.Load() {
				w.WriteHeader(http.StatusServiceUnavailable)
				return
			}
			proxy.ServeHTTP(w, r)
		}))
		t.Cleanup(front.Close)
		gate := newStartingGate("v1", logger)
		gate.open(served)
		handler := v1API(edgeLBAuth(edgeLBTokens(t, front.URL, &spentProofsInMemory{}), logger), gate)
		retry(t, handler, presentEdgeLB(t, caller, http.MethodGet, "/v1/sessions", edgeLBToken, ""), http.StatusServiceUnavailable, func() { down.Store(false) })
	})

	t.Run("the spent-proof store that did not answer", func(t *testing.T) {
		store := &switchableSpentProofs{}
		store.down.Store(true)
		gate := newStartingGate("v1", logger)
		gate.open(served)
		handler := v1API(edgeLBAuth(edgeLBTokens(t, broker.Server.URL, store), logger), gate)
		retry(t, handler, presentEdgeLB(t, caller, http.MethodGet, "/v1/sessions", edgeLBToken, ""), http.StatusServiceUnavailable, func() { store.down.Store(false) })
	})
}

// edgeLBRig is an edge /v1 over the spent-proof bucket of a real bus, opened by the very call main
// makes (brokerTokens.open), whose routes are stand-ins: each answers its first request with the
// 503 a route of that kind answers a fault with, and every later one as served.
type edgeLBRig struct {
	handler http.Handler
	caller  *ciphertokentest.Caller
}

// faultOnce is a route that answers its first request with fault and every later one 204.
func faultOnce(fault func(http.ResponseWriter)) http.Handler {
	var once sync.Once
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		faulted := false
		once.Do(func() { faulted = true })
		if faulted {
			fault(w)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
}

// newEdgeLBRig is the rig as an edge listener of edgeLBProject, whose bucket handles read with
// stream message gets as an edge's do (bus.WithStreamMessageReads, edgeGrant.connectOptions), or
// with edge false, as a listener of no edge that takes broker tokens beside no bearer.
func newEdgeLBRig(t *testing.T, edge bool) *edgeLBRig {
	t.Helper()
	var options []bus.ConnectOption
	if edge {
		options = append(options, bus.WithStreamMessageReads())
	}
	client := setupPublishTestClient(t, options...)
	caller := ciphertokentest.NewCaller(t)
	broker := edgeLBBroker(t, caller)
	accountFile := filepath.Join(t.TempDir(), "account-token")
	if err := os.WriteFile(accountFile, []byte("listener-account-token\n"), 0o600); err != nil {
		t.Fatalf("write the listener's own account token: %v", err)
	}
	tokens := newBrokerTokens(listenerBrokerConfig{BrokerURL: broker.Server.URL, AccountTokenFile: accountFile, Origin: listenerBrokerOrigin})
	if err := tokens.open(t.Context(), client.Conn, 1); err != nil {
		t.Fatalf("open the spent-proof bucket: %v", err)
	}
	busFault := func(w http.ResponseWriter) { writeBusUnavailable(w, errors.New("nats: timeout")) }
	routes := http.NewServeMux()
	routes.Handle("GET /v1/sessions", faultOnce(writeListRefilling))
	routes.Handle("DELETE /v1/sessions/{session}", faultOnce(busFault))
	routes.Handle("POST /v1/messages/send", faultOnce(busFault))
	routes.Handle("GET /v1/registry/{session}", noContent)
	routes.Handle("GET /v1/interests/{session}", faultOnce(func(w http.ResponseWriter) { w.WriteHeader(http.StatusInternalServerError) }))
	gate := newStartingGate("v1", logging.NewWithWriter("test-machine", io.Discard))
	gate.open(routes)
	logger := logging.NewWithWriter("test-machine", io.Discard)
	auth := listenerAPIAuth{brokers: tokens, logger: logger}
	if edge {
		auth = edgeLBAuth(tokens, logger)
	}
	return &edgeLBRig{handler: v1API(auth, gate), caller: caller}
}

// send is p as Google's balancer delivers it, through the proxy peer.
func (r *edgeLBRig) send(p edgeLBPresentation, peer string) *httptest.ResponseRecorder {
	recorder := httptest.NewRecorder()
	r.handler.ServeHTTP(recorder, p.through(peer, forwardedFor("203.0.113.9")))
	return recorder
}

// A route's 503 after the proof was spent - a fault before anything was served - gives a
// body-less request its proof back on the edge, so the balancer's one retry of it, with that
// proof, is served, and a proof reused after a request it served is still refused: the refilling
// list, the bus that did not answer a DELETE. A proof signs the method and the path and the
// token, and not the query or the body: a request that carries a body keeps its proof spent,
// since a given-back proof would carry another body, and the balancer does not retry it; its
// client sends it again with a fresh proof. A given-back proof can be presented once more with
// another query on the same method and path, which needs the token and the proof in plaintext:
// TLS covers the client's hop to the balancer alone, and the hop from Google's proxy to the pod is
// HTTP inside Google's network, so that is the accepted risk. A request served first keeps its
// proof spent too, and so does one a route answered any other failing status than 503, a 500
// among them: only a 503 says the listener cannot tell whether it acted. The record is the bucket
// every replica shares, so the retry reaches the other replica's proxy. Controls: release on any
// status at or past 500, and the 500's retry is served; give no proof back, and the 503s' retries
// are refused.
func TestEdgeRetryOfAProofAfterAPostAuth503(t *testing.T) {
	t.Parallel()
	rig := newEdgeLBRig(t, true)
	for _, tc := range []struct {
		name, method, path, body string
		first                    int
		retried                  int
	}{
		{name: "the refilling list", method: http.MethodGet, path: "/v1/sessions", first: http.StatusServiceUnavailable, retried: http.StatusNoContent},
		{name: "a DELETE the bus did not answer", method: http.MethodDelete, path: "/v1/sessions/ses_collaborator", first: http.StatusServiceUnavailable, retried: http.StatusNoContent},
		{name: "a send the bus did not answer", method: http.MethodPost, path: "/v1/messages/send", body: `{"target_session":"ses_other","message":"hello"}`, first: http.StatusServiceUnavailable, retried: http.StatusUnauthorized},
		{name: "a served read", method: http.MethodGet, path: "/v1/registry/ses_collaborator", first: http.StatusNoContent, retried: http.StatusUnauthorized},
		{name: "a route's 500", method: http.MethodGet, path: "/v1/interests/ses_collaborator", first: http.StatusInternalServerError, retried: http.StatusUnauthorized},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := presentEdgeLB(t, rig.caller, tc.method, tc.path, edgeLBToken, tc.body)
			if got := rig.send(p, "35.191.7.7:40001"); got.Code != tc.first {
				t.Fatalf("the first presentation: status %d body %s, want %d", got.Code, got.Body.String(), tc.first)
			}
			if got := rig.send(p, "130.211.1.2:40002"); got.Code != tc.retried {
				t.Fatalf("the balancer's retry of the same proof: status %d body %s, want %d", got.Code, got.Body.String(), tc.retried)
			}
			if got := rig.send(p, "35.191.7.9:40003"); got.Code != http.StatusUnauthorized {
				t.Fatalf("the same proof once more: status %d body %s, want 401", got.Code, got.Body.String())
			}
		})
	}
}

// A listener that is no edge gives no proof back: no load balancer in front of it sends a retry
// with the same proof, so a release there would gain nothing and would leave a proof good for one
// more request. Its route's 503 keeps the proof spent, and its client sends the request again with
// a fresh proof.
func TestEdgeAloneGivesAProofBackOnA503(t *testing.T) {
	t.Parallel()
	rig := newEdgeLBRig(t, false)
	p := presentEdgeLB(t, rig.caller, http.MethodGet, "/v1/sessions", edgeLBToken, "")
	if got := rig.send(p, "35.191.7.7:40001"); got.Code != http.StatusServiceUnavailable {
		t.Fatalf("the first presentation: status %d body %s, want 503", got.Code, got.Body.String())
	}
	if got := rig.send(p, "130.211.1.2:40002"); got.Code != http.StatusUnauthorized {
		t.Fatalf("the same proof again at a listener of no edge: status %d body %s, want 401", got.Code, got.Body.String())
	}
}

// After a release leaves a delete marker on a proof's key, the edge's create (its bucket handles
// read with stream message gets, bus.WithStreamMessageReads) writes the key again against that
// marker's revision, and of two presentations doing so at once the one that loses gets the
// server's wrong-last-sequence error from that second write: 10071 on a bucket of one copy, 10164
// on a replicated one. Either reads as a proof another presentation holds, a replay, and not as a
// store that did not answer, whose 503 would be no refusal: bus.IsCASConflict reads both codes,
// and the store answers a create refused with either as spent. The race itself is not driven
// here: nothing orders the two writes of one create against another's, so the store's bucket
// answers the second write's refusal itself. Control: the store reading only nats.ErrKeyExists as
// spent; the lost create reads as a store that did not answer, and the row fails naming it.
func TestEdgeSpentProofsReadALostCreateAfterAReleaseAsSpent(t *testing.T) {
	t.Parallel()
	for _, code := range []nats.ErrorCode{nats.JSErrCodeStreamWrongLastSequence, nats.JSErrCodeStreamWrongLastSequenceConstant} {
		if err := fmt.Errorf("record the proof: %w", &nats.APIError{Code: http.StatusBadRequest, ErrorCode: code}); !bus.IsCASConflict(err) {
			t.Errorf("a create refused with error code %d reads as no replay", code)
		}
		store := spentProofs{kv: bus.NewKeyValue(lostCreate{code: code}, nil, nil)}
		if spent, err := store.CheckAndSetDPoPProofUsed(t.Context(), "jti-lost-create", "", "", http.MethodGet, listenerBrokerOrigin+"/v1/sessions", time.Time{}); err != nil || !spent {
			t.Errorf("a create that lost over a released proof's marker (error code %d) = spent %v, error %v; want spent, no error", code, spent, err)
		}
	}
	for _, err := range []error{errors.New("nats: timeout"), &nats.APIError{Code: http.StatusServiceUnavailable, ErrorCode: nats.JSErrCodeInsufficientResourcesErr}} {
		if bus.IsCASConflict(err) {
			t.Errorf("%v reads as a replay, want a store that did not answer", err)
		}
	}
}

// lostCreate is a spent-proof bucket whose every create is refused with code, as the second write
// of the edge's create is when another presentation wrote over the same delete marker first.
type lostCreate struct {
	nats.KeyValue
	code nats.ErrorCode
}

func (lostCreate) Bucket() string { return bus.DPoPProofBucket }

func (k lostCreate) Create(string, []byte) (uint64, error) {
	return 0, &nats.APIError{Code: http.StatusBadRequest, ErrorCode: k.code, Description: "wrong last sequence"}
}

// A release deletes the record its own request's presentation wrote, and no other: the delete is
// made against that record's revision. A record of the same proof written over the key after it -
// once that first record left the bucket, as one whose age ran out does - stays, so the proof stays
// spent. Control: the release's delete made against no revision; it deletes the later record, and
// the proof reads unspent.
func TestEdgeReleaseDeletesOnlyTheRecordItsRequestWrote(t *testing.T) {
	t.Parallel()
	client := setupPublishTestClient(t, bus.WithStreamMessageReads())
	js, err := client.Conn.JetStream()
	if err != nil {
		t.Fatalf("JetStream: %v", err)
	}
	kv, err := ensureSpentProofBucket(t.Context(), client.Conn, js, 1)
	if err != nil {
		t.Fatalf("open the spent-proof bucket: %v", err)
	}
	store := spentProofs{kv: kv}
	const jti, htu = "jti-release-own-record", listenerBrokerOrigin + "/v1/sessions"
	present := func(ctx context.Context) bool {
		t.Helper()
		spent, err := store.CheckAndSetDPoPProofUsed(ctx, jti, "", "", http.MethodGet, htu, time.Time{})
		if err != nil {
			t.Fatalf("record the proof: %v", err)
		}
		return spent
	}
	spend := &proofSpend{}
	if present(withProofSpend(t.Context(), spend)) {
		t.Fatal("the first presentation read as a replay")
	}
	if err := kv.Delete(spend.key); err != nil {
		t.Fatalf("let the first record leave the bucket: %v", err)
	}
	if present(t.Context()) {
		t.Fatal("the presentation after the first record left read as a replay")
	}
	// The first request's route answers 503 now. Its release is made against its own record's
	// revision, which the key is no longer at, so the bucket refuses it.
	_ = spend.release(t.Context())
	if !present(t.Context()) {
		t.Fatal("the proof reads unspent after the first request's release: the release deleted a record its request did not write")
	}
}

// Of two requests in flight at once on one edge, a 503 gives back its own request's proof alone:
// A's route waits, B is served, then A's route answers 503. B's proof, presented again, is a
// replay (401), and A's is served (the balancer's retry). Control: one proofSpend shared by every
// request; A's 503 gives back B's proof, which is served again, and A's retry is refused.
func TestEdgeGivesBackOnlyItsOwnRequestsProof(t *testing.T) {
	t.Parallel()
	client := setupPublishTestClient(t, bus.WithStreamMessageReads())
	caller := ciphertokentest.NewCaller(t)
	broker := edgeLBBroker(t, caller)
	accountFile := filepath.Join(t.TempDir(), "account-token")
	if err := os.WriteFile(accountFile, []byte("listener-account-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	tokens := newBrokerTokens(listenerBrokerConfig{BrokerURL: broker.Server.URL, AccountTokenFile: accountFile, Origin: listenerBrokerOrigin})
	if err := tokens.open(t.Context(), client.Conn, 1); err != nil {
		t.Fatal(err)
	}
	entered, release := make(chan struct{}), make(chan struct{})
	first := true
	routes := http.NewServeMux()
	routes.HandleFunc("GET /v1/sessions", func(w http.ResponseWriter, _ *http.Request) {
		if first {
			first = false
			close(entered)
			<-release
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	routes.Handle("GET /v1/registry/{session}", noContent)
	gate := newStartingGate("v1", logging.NewWithWriter("test-machine", io.Discard))
	gate.open(routes)
	rig := &edgeLBRig{handler: v1API(edgeLBAuth(tokens, logging.NewWithWriter("test-machine", io.Discard)), gate), caller: caller}

	a := presentEdgeLB(t, caller, http.MethodGet, "/v1/sessions", edgeLBToken, "")
	b := presentEdgeLB(t, caller, http.MethodGet, "/v1/registry/ses_collaborator", edgeLBToken, "")
	aDone := make(chan int)
	go func() { aDone <- rig.send(a, "35.191.7.7:40001").Code }()
	<-entered
	if got := rig.send(b, "35.191.7.7:40002").Code; got != http.StatusNoContent {
		t.Fatalf("B: %d, want 204", got)
	}
	close(release)
	if got := <-aDone; got != http.StatusServiceUnavailable {
		t.Fatalf("A: %d, want 503", got)
	}
	if got := rig.send(b, "130.211.1.2:40003").Code; got != http.StatusUnauthorized {
		t.Errorf("B's served proof presented again: %d, want 401", got)
	}
	if got := rig.send(a, "130.211.1.2:40004").Code; got != http.StatusNoContent {
		t.Errorf("the balancer's retry of A: %d, want 204", got)
	}
}
