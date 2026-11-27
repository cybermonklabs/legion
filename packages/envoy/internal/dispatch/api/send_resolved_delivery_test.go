package api

import (
	"context"
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
)

// The dashboard reads a failed attempt row that carries an envelope id as one its session answered
// with an error (rowAnsweredWithError in packages/dispatch/web/src/features/conversation/delivery.ts),
// and offers it no same-mode Retry. That holds only while every failure sendResolvedDelivery reports
// comes with no envelope id: one beside a failure would read as the session's own error answer.
func TestSendResolvedDeliveryReportsNoEnvelopeWithAnyFailure(t *testing.T) {
	t.Parallel()
	session := "s1"
	live := ResolvedMention{Target: "session:s1", Delivery: "steer", SessionID: &session}
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
			s := &server{deps: Deps{Envoy: envoy.New(url, envoy.WithTimeout(150*time.Millisecond))}}
			outcome := s.sendResolvedDelivery(
				context.Background(), tc.target, "session:s1", "body", "m1:steer", nil, contracts.ExpectsReplyOptional, []byte(`{}`), false, "")
			if (outcome.envelopeID != nil) != tc.wantEnvelope || outcome.duplicate != tc.wantDup ||
				outcome.queued != tc.wantQueued || (outcome.failure != "") != tc.wantFailure {
				t.Fatalf("sendResolvedDelivery = %+v, want envelope %v, duplicate %v, queued %v, failure %v",
					outcome, tc.wantEnvelope, tc.wantDup, tc.wantQueued, tc.wantFailure)
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
			context.Background(), live, "session:s1", "body", "m1:steer", nil, contracts.ExpectsReplyOptional, []byte(`{}`), false, "")
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
				context.Background(), live, "session:s1", "body", "m1:steer", nil, contracts.ExpectsReplyOptional, []byte(`{}`), false)
			if outcome.failure == "" || outcome.envelopeID != nil {
				t.Fatalf("a send the listener answered %d = %+v, want a failure and no envelope", tc.status, outcome)
			}
			if got := retryableDeliveryFailure(outcome.failure); got != tc.retried {
				t.Fatalf("a send the listener answered %d recorded %q, retried %t; want retried %t", tc.status, outcome.failure, got, tc.retried)
			}
		})
	}
}
