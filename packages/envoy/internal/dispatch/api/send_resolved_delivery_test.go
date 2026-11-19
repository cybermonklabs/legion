package api

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/clock/clocktest"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/envoy"
	"github.com/sjawhar/envoy/internal/testwait"
)

// The dashboard reads a failed attempt row that carries an envelope id as one its session answered
// with an error (rowAnsweredWithError in packages/dispatch/web/src/features/conversation/delivery.ts),
// and offers it no same-mode Retry. That holds only while every failure sendResolvedDelivery reports
// comes with no envelope id: one beside a failure would read as the session's own error answer.
func TestSendResolvedDeliveryReportsNoEnvelopeWithAnyFailure(t *testing.T) {
	session := "s1"
	live := ResolvedMention{Target: "session:s1", Delivery: "steer", SessionID: &session}
	// holding is the late listener's word that it has read the send, so the test ends the
	// client's window once the send has landed and never before.
	holding := make(chan struct{}, 1)
	for _, tc := range []struct {
		name   string
		target ResolvedMention
		answer http.HandlerFunc
		// late is the listener that answers only once the client's window has ended, which the
		// test ends once that listener holds the send. No other case's window ends: the
		// listener's answer, however long a loaded machine takes to carry it, is the outcome.
		late         bool
		wantEnvelope bool
		wantDup      bool
		wantFailure  bool
		wantQueued   bool
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
		// The send lands and its answer, envelope and all, misses the client's window. The body is
		// read first: net/http watches a request's connection for the caller going away only once
		// its body has been read, so the context ends when the client's window ends the call.
		{name: "the listener answered too late", target: live, answer: func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.Copy(io.Discard, r.Body)
			holding <- struct{}{}
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
			url := "http://127.0.0.1:1"
			if tc.answer != nil {
				listener := httptest.NewServer(tc.answer)
				t.Cleanup(listener.Close)
				url = listener.URL
			}
			// The client's window runs on this clock, which moves only for the late listener: a
			// listener that never answers costs the test nothing, and one that answers is never
			// cut short by a move.
			clock := clocktest.New(time.Now())
			s := &server{deps: Deps{Envoy: envoy.New(url, envoy.WithClock(clock))}, clock: clock}
			answered := make(chan sendOutcome, 1)
			go func() {
				answered <- s.sendResolvedDelivery(
					context.Background(), tc.target, "body", "m1:steer", nil, contracts.ExpectsReplyOptional, []byte(`{}`), false)
			}()
			if tc.late {
				// The window is armed before the request is written (envoy.Client.do), so it is
				// pending once the listener holds the send, CallWindow from the clock's start.
				testwait.Receive(t, "the late listener to hold the send", holding)
				awaitArmedAt(t, clock, "the send's window", clock.Now().Add(envoy.CallWindow))
				clock.Advance(envoy.CallWindow)
			}
			outcome := testwait.Receive(t, "sendResolvedDelivery to answer", answered)
			if (outcome.envelopeID != nil) != tc.wantEnvelope || outcome.duplicate != tc.wantDup ||
				outcome.queued != tc.wantQueued || (outcome.failure != "") != tc.wantFailure {
				t.Fatalf("sendResolvedDelivery = %+v, want envelope %v, duplicate %v, queued %v, failure %v",
					outcome, tc.wantEnvelope, tc.wantDup, tc.wantQueued, tc.wantFailure)
			}
		})
	}
}
