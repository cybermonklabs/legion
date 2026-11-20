package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/nats-io/nats.go"
	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/logging"
	"github.com/sjawhar/envoy/listenerwire"
)

// A send or publish the notifications stream did not take because it carries no subject capturing
// the message's (bus.ErrStreamLacksSubject) was answered: the bus said so. Its answer is the 503
// with Retry-After that asks the sender to send it again, since the same message is stored once a
// build that stores the subject sets the stream, but it opens with streamLacksSubjectPrefix and
// names the subject and the stream's generation, and its record's outcome is stream_lacks_subject:
// a body opening with busUnansweredPrefix, or the bus_unavailable record, would have a monitor
// count it as an outage. A bus that did not answer keeps its own words and record, in the same
// test, so the two are told apart rather than merely renamed.
//
// What it does not prove: that the bus names the refusal so. That is internal/bus's ground
// (lacksSubject), and the send route's own test drives it end to end over a real stream
// (TestASendIsAnsweredOnlyOnceTheStreamHoldsIt); here the error is built as lacksSubject builds it.
func TestAStreamThatLacksTheSubjectIsAnsweredAsSuchNotAsABusThatDidNotAnswer(t *testing.T) {
	subject := contracts.LegacyAgentSubject("ses_left_out_target")
	lacks := fmt.Errorf("%w: %s at generation 1000000 carries no subject capturing %q: %w",
		bus.ErrStreamLacksSubject, bus.Stream, subject, nats.ErrNoStreamResponse)
	unanswered := fmt.Errorf("bus: publish to %q: %w", subject, nats.ErrConnectionClosed)

	for _, route := range []struct {
		name    string
		line    string
		laneKey string
		lane    string
	}{
		{"publish", "listener publish", "topic", subject},
		{"send", "listener send", "target_session", "ses_left_out_target"},
	} {
		for _, tc := range []struct {
			name        string
			err         error
			wantBody    string
			wantOutcome string
		}{
			{"the stream does not capture the subject", lacks, streamLacksSubjectPrefix + ": " + lacks.Error(), "stream_lacks_subject"},
			{"a bus that did not answer", unanswered, busUnansweredPrefix + ": " + unanswered.Error(), "bus_unavailable"},
		} {
			t.Run(route.name+"/"+tc.name, func(t *testing.T) {
				recorder, record := serveRecorded(t, func(logger *logging.Logger) http.HandlerFunc {
					return func(w http.ResponseWriter, r *http.Request) {
						rec := &requestRecord{logger: logger, line: route.line, laneKey: route.laneKey, lane: route.lane}
						rec.refuseBusUnavailable(w, tc.err)
					}
				}, postJSON("/v1/messages/publish", "{}"))
				if recorder.Code != http.StatusServiceUnavailable {
					t.Fatalf("status = %d, want 503; body = %s", recorder.Code, recorder.Body.String())
				}
				if got := recorder.Header().Get("Retry-After"); got != "1" {
					t.Fatalf("Retry-After = %q, want 1: the same message is taken once the stream stores its subject", got)
				}
				var refusal listenerwire.APIError
				if err := json.Unmarshal(recorder.Body.Bytes(), &refusal); err != nil {
					t.Fatalf("decode the refusal %s: %v", recorder.Body.String(), err)
				}
				// The body and the record are each reported, not the first alone, so a change to the
				// one decision both come from (writeBusUnavailable) shows in both at once.
				if refusal.Error != tc.wantBody {
					t.Errorf("the refusal reads %q, want %q", refusal.Error, tc.wantBody)
				}
				if tc.err == lacks && (strings.HasPrefix(refusal.Error, busUnansweredPrefix) || !strings.Contains(refusal.Error, subject) || !strings.Contains(refusal.Error, "generation 1000000")) {
					t.Errorf("the refusal reads %q: want it to name %s and the generation, and not to say the bus did not answer", refusal.Error, subject)
				}
				assertRecordFields(t, record, map[string]any{
					"msg":         route.line,
					"level":       "WARN",
					"outcome":     tc.wantOutcome,
					"status":      float64(http.StatusServiceUnavailable),
					route.laneKey: route.lane,
				})
			})
		}
	}
}
