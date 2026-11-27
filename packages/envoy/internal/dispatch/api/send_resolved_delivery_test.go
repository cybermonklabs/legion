package api

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/envoy"
	"github.com/sjawhar/envoy/internal/testport"
	"github.com/sjawhar/envoy/internal/testwait"
	"github.com/sjawhar/envoy/listenerwire"
)

// The dashboard reads a failed attempt row that carries an envelope id as one its session answered
// with an error (rowAnsweredWithError in packages/dispatch/web/src/features/conversation/delivery.ts),
// and offers it no same-mode Retry. That holds only while every failure sendResolvedDelivery reports
// comes with no envelope id: one beside a failure would read as the session's own error answer.
func TestSendResolvedDeliveryReportsNoEnvelopeWithAnyFailure(t *testing.T) {
	t.Parallel()
	session := "s1"
	live := ResolvedMention{Target: "session:s1", Delivery: "steer", SessionID: &session}
	kept := keptForPlanner()
	// published answers a publish to the role's topic with status and body, and fails the test for
	// any other request: a role kept for its next holder is published, never sent.
	published := func(status int, body string) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodPost || r.URL.Path != "/v1/messages/publish" {
				t.Errorf("unexpected request %s %s, want the role publish", r.Method, r.URL.Path)
			}
			w.WriteHeader(status)
			_, _ = w.Write([]byte(body))
		}
	}
	for _, tc := range []struct {
		name         string
		target       ResolvedMention
		answer       http.HandlerFunc
		wantEnvelope bool
		wantDup      bool
		wantFailure  bool
		wantQueued   bool
		late         bool
	}{
		{name: "the target did not resolve", target: ResolvedMention{Target: "role:planner", Delivery: "steer", ResolveError: "no live holder for role planner"}, wantFailure: true},
		{name: "no live session", target: ResolvedMention{Target: "session:s9", Delivery: "steer"}, wantFailure: true},
		{name: "the listener refused the send", target: live, answer: func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":"no live session s1"}`))
		}, wantFailure: true},
		{name: "the listener failed without a reason", target: live, answer: func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusBadGateway)
		}, wantFailure: true},
		{name: "the listener answered something unreadable", target: live, answer: func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`{"event_id":`))
		}, wantFailure: true},
		// The send lands and its answer, envelope and all, misses the client's window: the listener
		// answers only once the request's context has ended, which the client's window, the one
		// case marked late, ends. The body is read first, because net/http watches a request's
		// connection for the caller going away only once its body has been read.
		{name: "the listener answered too late", target: live, answer: func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.Copy(io.Discard, r.Body)
			<-r.Context().Done()
			_, _ = w.Write([]byte(`{"event_id":"envelope-late","recipient":"s1"}`))
		}, late: true, wantFailure: true},
		{name: "the listener published the send", target: live, answer: func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`{"event_id":"envelope-1","recipient":"s1"}`))
		}, wantEnvelope: true},
		{name: "the stream already held the send", target: live, answer: func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`{"event_id":"envelope-2","recipient":"s1","duplicate":true}`))
		}, wantDup: true},
		// A stopped target: the listener stored the message for the session and said so, so the
		// send carries its envelope and the attempt it settles is queued, never failed.
		{name: "the listener stored the send for a stopped session", target: live, answer: func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`{"event_id":"envelope-3","recipient":"s1","state":"stopped","last_seen":1700000000000}`))
		}, wantEnvelope: true, wantQueued: true},
		// A role kept for its next holder: the listener's 202 kept the message on the role's kept
		// lane, so the send carries the kept copy's envelope and the attempt it settles is queued.
		{name: "the listener kept the publish for the role's next holder", target: kept,
			answer:       published(http.StatusAccepted, `{"event_id":"envelope-4","kept":true,"kept_reason":"unclaimed","kept_until":1793003600000}`),
			wantEnvelope: true, wantQueued: true},
		{name: "a live holder of the role took the publish", target: kept,
			answer:       published(http.StatusOK, `{"event_id":"envelope-5","holder":"s2","holder_state":"live"}`),
			wantEnvelope: true},
		{name: "the listener kept nothing of the publish", target: kept,
			answer:      published(http.StatusNotFound, `{"error":"no holder for role planner","reason":"unclaimed"}`),
			wantFailure: true},
		// The publish lands and its answer misses the client's window as the send's does above:
		// the listener answers only once the request's context has ended, its body read first.
		{name: "the listener answered the publish too late", target: kept, answer: func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.Copy(io.Discard, r.Body)
			<-r.Context().Done()
			w.WriteHeader(http.StatusAccepted)
			_, _ = w.Write([]byte(`{"event_id":"envelope-late","kept":true}`))
		}, late: true, wantFailure: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// A listener that is not there: a port the test holds and nothing listens on, so a
			// dial to it is refused and no other program can answer it (testport.Refused).
			url := "http://" + testport.Refused(t)
			if tc.answer != nil {
				listener := httptest.NewServer(tc.answer)
				t.Cleanup(listener.Close)
				url = listener.URL
			}
			// A listener that answers gets a window past the run (pastTheRun), so a loaded machine
			// that carries a send or its answer slowly never turns the answer into a receipt timeout.
			window := pastTheRun
			if tc.late {
				// The late listener answers only once the client has given up, so this window
				// ends the call however long a loaded machine takes to end it.
				window = 150 * time.Millisecond
			}
			s := &server{deps: Deps{Envoy: envoy.New(url, envoy.WithTimeout(window), envoy.WithAccountTokenFile(testEnvoyAccountTokenFile(t, url)))}}
			ctx, cancel := context.WithCancel(context.Background())
			answered, returned := make(chan sendOutcome, 1), make(chan struct{})
			// Registered after the listener's Close, so it runs before it: the call's context ends,
			// which ends its request, before the listener waits out its handlers, and the worker has
			// returned before the subtest ends, whatever ended it.
			t.Cleanup(func() {
				cancel()
				testwait.Receive(t, "sendResolvedDelivery's worker to return once its context ended", returned)
			})
			go func() {
				defer close(returned)
				answered <- s.sendResolvedDelivery(
					ctx, tc.target, "session:s1", testDeliveredPost, "body", "m1:steer", nil, contracts.ExpectsReplyOptional, []byte(`{}`), false)
			}()
			outcome := testwait.Receive(t, "sendResolvedDelivery to answer", answered)
			if (outcome.envelopeID != nil) != tc.wantEnvelope || outcome.duplicate != tc.wantDup ||
				outcome.queued != tc.wantQueued || (outcome.failure != "") != tc.wantFailure {
				t.Fatalf("sendResolvedDelivery = %+v, want envelope %v, duplicate %v, queued %v, failure %v",
					outcome, tc.wantEnvelope, tc.wantDup, tc.wantQueued, tc.wantFailure)
			}
		})
	}
}

// keptForPlanner is the role planner as resolveDeliveryTarget resolves it when no live session
// holds it and the registry holds no entry for it while its project answers: the role itself,
// kept for its first holder, beside the listener's own no-holder answer.
func keptForPlanner() ResolvedMention {
	return ResolvedMention{
		Target: "role:planner", Delivery: "steer",
		redirect: &registryRedirect[ResolvedMention]{
			keepForRole:    "planner",
			listenerAnswer: ResolvedMention{Target: "role:planner", Delivery: "steer", ResolveError: "no holder for role planner"},
		},
	}
}

// testDeliveredPost is the post these tests' deliveries are of, which a publish for a role's first
// holder names (publishForRole).
var testDeliveredPost = listenerwire.DispatchPost{Kind: listenerwire.DispatchPostMessage, ID: "8f14e45f-ceea-467a-9c1e-000000000001"}

// A role kept for its next holder is published with the failures a send's are worded with: the
// listener's own text for a publish it kept nothing of, the receipt-timeout cause for one whose
// answer missed the window, since the listener keeps the copy before it answers, and the
// unconfirmed-store cause for a 503 coded maybe_stored (listenerwire.CodeMaybeStored). A listener
// that answers at once gets a window past the run (pastTheRun), and the late one answers only once
// the client's short window has ended its request, as in the table above.
func TestSendResolvedDeliveryWordsAKeptRolesFailuresAsASendsAre(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		answer  http.HandlerFunc
		failure string
		late    bool
	}{
		{name: "nothing kept", answer: func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":"no holder for role planner","reason":"unclaimed"}`))
		}, failure: "no holder for role planner"},
		{name: "answered too late", answer: func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.Copy(io.Discard, r.Body)
			<-r.Context().Done()
			w.WriteHeader(http.StatusAccepted)
		}, failure: receiptTimeoutText, late: true},
		{name: "a listener that is starting", answer: func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"error":"service starting"}`))
		}, failure: listenerUnreachablePrefix + "listener returned 503: service starting"},
		{name: "a publish the listener could not confirm stored", answer: func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"code":"maybe_stored","error":"envoy: the message bus did not answer: publish unacknowledged"}`))
		}, failure: storeUnconfirmedText},
	} {
		t.Run(tc.name, func(t *testing.T) {
			listener := httptest.NewServer(tc.answer)
			t.Cleanup(listener.Close)
			window := pastTheRun
			if tc.late {
				window = 150 * time.Millisecond
			}
			s := &server{deps: Deps{Envoy: envoy.New(listener.URL, envoy.WithTimeout(window), envoy.WithAccountTokenFile(testEnvoyAccountTokenFile(t, listener.URL)))}}
			outcome := s.sendResolvedDelivery(
				context.Background(), keptForPlanner(), "role:planner", testDeliveredPost, "body", "m1:steer", nil, contracts.ExpectsReplyOptional, []byte(`{}`), false)
			if outcome.failure != tc.failure || outcome.envelopeID != nil || outcome.queued {
				t.Fatalf("sendResolvedDelivery = %+v, want failure %q with no envelope", outcome, tc.failure)
			}
		})
	}
}

// A connected-only send asks for a session that is running now, and a role kept for its first
// holder has none, so it is not kept: nothing is published, and the attempt settles with the
// listener's own no-holder answer as refused, as a stopped session refuses it.
func TestAConnectedOnlySendToAKeptRoleIsNotKept(t *testing.T) {
	t.Parallel()
	listener := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected request %s %s: a connected-only send keeps nothing for a role", r.Method, r.URL.Path)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(listener.Close)
	s := &server{deps: Deps{Envoy: envoy.New(listener.URL)}}
	kept := keptForPlanner()
	outcome := s.sendResolvedDelivery(
		context.Background(), kept, "role:planner", testDeliveredPost, "body", "m1:steer", nil, contracts.ExpectsReplyOptional, []byte(`{}`), true)
	if outcome.failure != "no holder for role planner" || outcome.refused != &kept.redirect.listenerAnswer ||
		outcome.envelopeID != nil || outcome.queued {
		t.Fatalf("sendResolvedDelivery = %+v, want the listener's no-holder answer refused, with nothing kept", outcome)
	}
}

// A session the Legion agent registry named is sent to so that its daemon answers the message,
// and the listener answers a send it hands to that daemon with no state. A send it answers
// stopped it only stored for the stopped session, having handed it to no daemon - the project
// said it stopped answering after the registry was read - and that session does not run again to
// read it. So it settles as the listener's refusal does: the listener's own answer for the route,
// refused, with no envelope and nothing queued, and so does a take-over's repeat of it, which the
// stream recognises as a duplicate. A session the listener resolved itself and stored the send for
// is queued as ever ("the listener stored the send for a stopped session" above).
//
// Each send is made on its own route, a role's included, and carries no role mark: the registry
// names a role's session only while no live session holds the role and its newest entry's project
// answers, and the listener hands a send to that session to the agent's daemon on its session id,
// which needs no Dispatch identity. A client that presents no identity, as this one, refuses a
// marked send outright (envoy.ErrNoListenerIdentity). A recheck that finds the listener's record of
// that session names no redirect, and its send is marked; the listener hands that one off on the
// role token (handOffOrKeepRoleSend in cmd/listener).
func TestASendTheListenerOnlyStoresForARegistrySessionSettlesWithTheListenersAnswer(t *testing.T) {
	t.Parallel()
	stopped := "stopped-2"
	registered := func(answer ResolvedMention) ResolvedMention {
		return ResolvedMention{Target: answer.Target, Delivery: "steer", SessionID: &stopped, redirect: &registryRedirect[ResolvedMention]{listenerAnswer: answer}}
	}
	role := registered(ResolvedMention{Target: "role:planner", Delivery: "steer", ResolveError: "no holder for role planner"})
	session := registered(ResolvedMention{
		Target: "session:stopped-2", Delivery: "steer", ResolveError: contracts.NoLiveSession + " stopped-2",
		attemptSessionID: stopped,
	})
	const stored = `{"event_id":"stored-1","recipient":"stopped-2","recipient_state":"gone","state":"stopped"}`
	for _, tc := range []struct {
		name    string
		target  ResolvedMention
		answer  string
		refused bool
	}{
		{name: "a role's newest session", target: role, answer: stored, refused: true},
		{name: "a session the listener holds no record of", target: session, answer: stored, refused: true},
		{name: "a repeat of the stored send", target: session, refused: true,
			answer: `{"event_id":"stored-1","recipient":"stopped-2","state":"stopped","duplicate":true}`},
		// The control: the listener handed the send to the agent's daemon.
		{name: "a send handed to the daemon", target: role, answer: `{"event_id":"handed-off-1","recipient":"stopped-2"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			listener := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodPost || r.URL.Path != "/v1/messages/send" {
					t.Errorf("unexpected request %s %s, want the send", r.Method, r.URL.Path)
				}
				var sent listenerwire.SendRequest
				if err := json.NewDecoder(r.Body).Decode(&sent); err != nil || sent.Role != "" {
					t.Errorf("the send carried role %q (decode: %v), want no role mark on a send to a session the registry named", sent.Role, err)
				}
				_, _ = w.Write([]byte(tc.answer))
			}))
			t.Cleanup(listener.Close)
			// A window past the run (pastTheRun): the listener answers at once, and a loaded machine
			// must not turn its answer into the client's timeout.
			s := &server{deps: Deps{Envoy: envoy.New(listener.URL, envoy.WithTimeout(pastTheRun))}}
			outcome := s.sendResolvedDelivery(
				context.Background(), tc.target, tc.target.Target, testDeliveredPost, "body", "m1:steer", nil, contracts.ExpectsReplyOptional, []byte(`{}`), false)
			if !tc.refused {
				if outcome.failure != "" || outcome.refused != nil || outcome.queued || outcome.envelopeID == nil ||
					*outcome.envelopeID != "handed-off-1" {
					t.Fatalf("sendResolvedDelivery = %+v, want it sent with the hand-off's envelope", outcome)
				}
				return
			}
			answer := &tc.target.redirect.listenerAnswer
			if outcome.failure != answer.ResolveError || outcome.refused != answer || outcome.envelopeID != nil ||
				outcome.queued || outcome.duplicate {
				t.Fatalf("sendResolvedDelivery = %+v, want the listener's own answer %q refused, with no envelope and nothing queued",
					outcome, answer.ResolveError)
			}
		})
	}
}

// A listener 503 coded maybe_stored (listenerwire.CodeMaybeStored) is a send it published and could
// not confirm stored, which may have reached the session: the attempt records that cause in its own
// words (storeUnconfirmedText), not the receipt timeout's, and it is retryable under its key, as a
// receipt timeout is. The control is the same 503 with no code: an ordinary unreachable failure.
func TestASendTheListenerCouldNotConfirmStoredIsRecordedAsPossiblyDelivered(t *testing.T) {
	t.Parallel()
	session := "s1"
	live := ResolvedMention{Target: "session:s1", Delivery: "steer", SessionID: &session}
	send := func(body string) sendOutcome {
		t.Helper()
		listener := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(body))
		}))
		t.Cleanup(listener.Close)
		// A window past the run (pastTheRun): the listener answers at once, and a loaded machine
		// that carries that answer slowly must not turn it into a receipt timeout.
		s := &server{deps: Deps{Envoy: envoy.New(listener.URL, envoy.WithTimeout(pastTheRun))}}
		return s.sendResolvedDelivery(
			context.Background(), live, "session:s1", testDeliveredPost, "body", "m1:steer", nil, contracts.ExpectsReplyOptional, []byte(`{}`), false)
	}
	maybe := send(`{"code":"maybe_stored","error":"envoy: the message bus did not answer: publish unacknowledged"}`)
	if maybe.failure != storeUnconfirmedText || maybe.failure == receiptTimeoutText || maybe.envelopeID != nil {
		t.Fatalf("a 503 coded maybe_stored = %+v, want the failure %q and no envelope", maybe, storeUnconfirmedText)
	}
	if !retryableDeliveryFailure(maybe.failure) {
		t.Fatalf("%q is not retryable, want it retried under its key as a receipt timeout is", maybe.failure)
	}
	plain := send(`{"error":"envoy: the message bus did not answer: no responders"}`)
	if plain.failure == storeUnconfirmedText || !strings.HasPrefix(plain.failure, listenerUnreachablePrefix) {
		t.Fatalf("control: a 503 with no code = %+v, want an ordinary %q failure", plain, listenerUnreachablePrefix)
	}
}

// A send the listener answers with an outage - a 408 or a 429 from something in front of it (the
// listener answers no 408, and its own 429s reach no Bearer caller such as Dispatch), a 502, or a
// 501 or 505 from whatever sits in front of it - is a failure a later try can outlast: the
// attempt records it under listenerUnreachablePrefix, and the take-over retry asks again
// (retryableDeliveryFailure). The listener's own 500 (listenerwire.FaultStatus) and its 400 are
// its answers, which settle the attempt at once.
func TestASendTheListenerAnsweredWithAnOutageIsTriedAgain(t *testing.T) {
	session := "s1"
	live := ResolvedMention{Target: "session:s1", Delivery: "steer", SessionID: &session}
	for _, tc := range []struct {
		status  int
		body    string
		retried bool
	}{
		{http.StatusRequestTimeout, `<html>408 Request Timeout</html>`, true},
		{http.StatusTooManyRequests, `<html>429 Too Many Requests</html>`, true},
		{http.StatusBadGateway, `{"error":"upstream connect error"}`, true},
		{http.StatusNotImplemented, ``, true},
		{http.StatusHTTPVersionNotSupported, `{"error":"HTTP version not supported"}`, true},
		{http.StatusInternalServerError, `{"error":"envoy: the claim record of role legion-planner is unreadable: …"}`, false},
		{http.StatusBadRequest, `{"error":"message is required"}`, false},
	} {
		t.Run(fmt.Sprint(tc.status), func(t *testing.T) {
			listener := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer listener.Close()
			s := &server{deps: Deps{Envoy: envoy.New(listener.URL)}}
			outcome := s.sendResolvedDelivery(
				context.Background(), live, "session:s1", testDeliveredPost, "body", "m1:steer", nil, contracts.ExpectsReplyOptional, []byte(`{}`), false)
			if outcome.failure == "" || outcome.envelopeID != nil {
				t.Fatalf("a send the listener answered %d = %+v, want a failure and no envelope", tc.status, outcome)
			}
			if got := retryableDeliveryFailure(outcome.failure); got != tc.retried {
				t.Fatalf("a send the listener answered %d recorded %q, retried %t; want retried %t", tc.status, outcome.failure, got, tc.retried)
			}
		})
	}
}
