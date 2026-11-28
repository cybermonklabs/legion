package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/clock/clocktest"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/logging"
	"github.com/sjawhar/envoy/internal/session"
	"github.com/sjawhar/envoy/internal/store"
	"github.com/sjawhar/envoy/internal/testnats"
	"github.com/sjawhar/envoy/internal/testwait"
	"github.com/sjawhar/envoy/listenerwire"
)

// The two routes that read one session's interest record - GET /v1/registry/<session_id> and GET
// /v1/interests/<session_id> - read a record the interest cache misses through to the bucket, and
// on a bus that went silent without closing the connection that read would wait out the JetStream
// MaxWait (10 s), as long as this listener's HTTP write timeout. Each must answer such a read as
// the named 503 with Retry-After: 1 inside the caller's 5 s attempt, and never as a session with
// no topics.
//
// GET /v1/sessions makes no such read: it joins the interest copy (store.Registry.Cached), so a
// silent bus holds nothing back and it answers the copies it has. That is what it must do here -
// serve, at once, with each session's cached topics - because a listing that read one record per
// row would hold the whole list, which every Dispatch mention and broadcast reads, behind a
// bucket that does not answer.
//
// Each row has its own relay and caches. One live session has no interest record, so a
// single-record read of it must reach the bucket. The other session's record is cached.
// The same requests before the freeze answer from the cache or from the reachable bucket.
func TestTheInterestReadRoutesAnswerABusThatStoppedAnsweringInsideTheBudget(t *testing.T) {
	t.Parallel()
	const (
		missed = "ses_interest_routes_subscribed_to_nothing"
		cached = "ses_interest_routes_cached_interest"
		topic  = "notifications.role.reviewer"
	)
	cachedTopics := []string{contracts.LegacyAgentSubject(cached), topic}
	wantTopics := func(t *testing.T, info listenerwire.SessionInfo, want []string) {
		t.Helper()
		if !slices.Equal(info.Topics, want) {
			t.Fatalf("%s's topics = %v, want %v", info.SessionID, info.Topics, want)
		}
	}
	for _, row := range []struct {
		name, path string
		missStatus int
		budget     time.Duration
	}{
		{"sessions", "/v1/sessions", http.StatusOK, 0},
		{"registry", "/v1/registry/", http.StatusOK, sessionInterestReadTimeout},
		{"interests", "/v1/interests/", http.StatusNotFound, sendRequestBudget},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			relay := startListenerRelay(t, testnats.URL(t))
			client, err := bus.ConnectOwningStream([]string{relay.relayed}, bus.WithReplicas(1))
			if err != nil {
				t.Fatalf("connect the listener's bus through the relay: %v", err)
			}
			t.Cleanup(client.Close)
			fake := clocktest.New(time.Unix(0, 0))
			stores := openStampRig(t, client)
			for _, id := range []string{missed, cached} {
				stores.register(t, id, session.SessionEntry{SelfSubscribed: true, MachineID: "test-machine", Dir: "/test/" + id, Title: id})
			}
			if _, err := stores.registry.Upsert(store.Interest{SessionID: cached, MachineID: "test-machine", Dir: "/test/" + cached}, cachedTopics); err != nil {
				t.Fatalf("write %s's interest record: %v", cached, err)
			}
			if _, ok := stores.registry.Cached(cached); !ok {
				t.Fatalf("the interest cache does not hold %s", cached)
			}
			if _, ok := stores.registry.Cached(missed); ok {
				t.Fatalf("the interest cache holds %s, want a read-through to the bucket", missed)
			}

			state := &listenerDeps{client: client, registry: stores.registry, sessions: stores.sessions, clock: fake}
			var handler http.HandlerFunc
			switch row.name {
			case "sessions":
				handler = sessionsHandler(state.registry, state.sessions, nil)
			case "registry":
				handler = sessionEntryHandler(state)
			case "interests":
				handler = adminInterestsHandler(state.clk(), state.registry, state.sessions, nil, nil, logging.New("test"))
			}
			get := func(path string) <-chan *httptest.ResponseRecorder {
				return routeAnswer(t, handler, httptest.NewRequest(http.MethodGet, path, nil))
			}
			wantStatus := func(rec *httptest.ResponseRecorder, what string, status int) {
				t.Helper()
				if rec.Code != status {
					t.Fatalf("%s: status %d, body %s; want %d", what, rec.Code, rec.Body.String(), status)
				}
			}

			if row.name == "sessions" {
				wantList := func(rec *httptest.ResponseRecorder, what string) {
					t.Helper()
					wantStatus(rec, what, http.StatusOK)
					var rows []listenerwire.SessionInfo
					if err := json.Unmarshal(rec.Body.Bytes(), &rows); err != nil {
						t.Fatalf("decode the session list %s: %v", rec.Body.String(), err)
					}
					if len(rows) != 2 {
						t.Fatalf("%s listed %d rows, want 2: %s", what, len(rows), rec.Body.String())
					}
					for _, info := range rows {
						if info.SessionID == cached {
							wantTopics(t, info, cachedTopics)
						} else {
							wantTopics(t, info, nil)
						}
					}
				}
				wantList(testwait.Receive(t, "the control session list to answer", get(row.path)), "GET /v1/sessions with the bus answering")
				const marker = "test.freeze.interest_reads"
				relay.arm(marker, 1)
				if err := client.Conn.Publish(marker, nil); err != nil {
					t.Fatalf("publish freeze marker: %v", err)
				}
				testwait.Receive(t, "the relay to swallow the freeze marker", relay.swallowed)
				sent := client.Conn.Stats().OutMsgs
				wantList(testwait.Receive(t, "the frozen-bus session list to answer without advancing the clock", get(row.path)), "GET /v1/sessions with the bus silent")
				if got := client.Conn.Stats().OutMsgs; got != sent {
					t.Fatalf("GET /v1/sessions published %d NATS messages with the bus silent, want none", got-sent)
				}
				return
			}

			missPath, hitPath := row.path+missed, row.path+cached
			control := testwait.Receive(t, "the cache miss with the bus answering", get(missPath))
			wantStatus(control, "GET "+missPath+" with the bus answering", row.missStatus)
			if row.name == "registry" {
				wantTopics(t, decodeSessionInfo(t, control), nil)
			}
			wantHit := func(what string) {
				t.Helper()
				hit := testwait.Receive(t, what, get(hitPath))
				wantStatus(hit, what, http.StatusOK)
				if row.name == "registry" {
					wantTopics(t, decodeSessionInfo(t, hit), cachedTopics)
				}
			}
			wantHit("GET " + hitPath + " with the bus answering")

			relay.arm(missed, 1)
			started := fake.Now()
			deadline := started.Add(row.budget)
			pending := get(missPath)
			if row.name == "registry" {
				routeEnd := started.Add(sendRequestBudget)
				fake.AwaitArmedAt(t, "the registry request budget", routeEnd)
				if !deadline.Before(routeEnd) {
					t.Fatalf("the registry interest read ends at %s, want it to end before the request budget at %s", deadline, routeEnd)
				}
			}
			expireFrozenRoute(t, fake, relay, "GET "+missPath, deadline)
			refused := testwait.Receive(t, "GET "+missPath+" to refuse the silent bus at its read budget", pending)
			wantWriteBusUnanswered(t, "GET "+missPath, refused)
			var refusal listenerwire.APIError
			if err := json.Unmarshal(refused.Body.Bytes(), &refusal); err != nil {
				t.Fatalf("decode the refusal %s: %v", refused.Body.String(), err)
			}
			if strings.Contains(refusal.Error, store.ErrRoleViewNotReady.Error()) {
				t.Fatalf("GET %s refused %q because the role claim view was not ready, want refusal of the frozen interest read", missPath, refusal.Error)
			}
			wantHit("GET " + hitPath + " with the bus silent and no further clock advance")
		})
	}
}
