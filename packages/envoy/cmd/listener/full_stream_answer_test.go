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

// A send or publish the stream refused because it holds its byte limit (bus.ErrStreamFull) was
// answered: the server said the stream is full. So it is not a bus that did not answer, and the
// answer must not say it was. The body opens with streamFullPrefix, not with busUnansweredPrefix,
// which sends an operator looking for an outage, and the request record's outcome is stream_full,
// not bus_unavailable, the bucket an operator reads as "the bus went quiet". The refusal is still
// worth retrying once age or an operator has made room, so it keeps the 503 and the Retry-After
// that ask for that retry, and its body names the full stream.
//
// What it does not prove: that the server's refusal arrives as bus.ErrStreamFull. That is
// internal/bus's own ground (streamFull), and here the error is built as streamFull builds it.
func TestAFullStreamIsAnsweredAsFullNotAsABusThatDidNotAnswer(t *testing.T) {
	subject := contracts.RoleTopicPrefix + "reviewer"
	full := fmt.Errorf("%w: %w", bus.ErrStreamFull,
		&nats.APIError{Code: http.StatusServiceUnavailable, ErrorCode: 10077, Description: "maximum bytes exceeded"})

	for _, route := range []struct {
		name    string
		line    string
		laneKey string
		lane    string
	}{
		{"publish", "listener publish", "topic", subject},
		{"send", "listener send", "target_session", "ses_full_target"},
	} {
		for _, failure := range []struct {
			name string
			err  error
		}{
			{"the publish refused", full},
		} {
			t.Run(route.name+"/"+failure.name, func(t *testing.T) {
				recorder, record := serveRecorded(t, func(logger *logging.Logger) http.HandlerFunc {
					return func(w http.ResponseWriter, r *http.Request) {
						rec := &requestRecord{logger: logger, line: route.line, laneKey: route.laneKey, lane: route.lane}
						rec.refuseBusUnavailable(w, failure.err)
					}
				}, postJSON("/v1/messages/publish", "{}"))
				if recorder.Code != http.StatusServiceUnavailable {
					t.Fatalf("status = %d, want 503: a full stream takes the retry once it has room; body = %s", recorder.Code, recorder.Body.String())
				}
				if got := recorder.Header().Get("Retry-After"); got == "" {
					t.Fatalf("Retry-After is unset, want one: the same send is taken once the stream has room")
				}
				var refusal listenerwire.APIError
				if err := json.Unmarshal(recorder.Body.Bytes(), &refusal); err != nil {
					t.Fatalf("decode the refusal %s: %v", recorder.Body.String(), err)
				}
				if strings.HasPrefix(refusal.Error, busUnansweredPrefix) {
					t.Fatalf("the refusal reads %q: it says the bus did not answer, but the bus answered that the stream is full", refusal.Error)
				}
				if !strings.HasPrefix(refusal.Error, streamFullPrefix) {
					t.Fatalf("the refusal reads %q, want it to open with %q", refusal.Error, streamFullPrefix)
				}
				if !strings.Contains(refusal.Error, bus.ErrStreamFull.Error()) {
					t.Fatalf("the refusal reads %q, want it to name %q", refusal.Error, bus.ErrStreamFull)
				}
				if record["outcome"] != "stream_full" {
					t.Fatalf("the request record's outcome is %v, want stream_full: a full stream answered, and bus_unavailable reads as a bus that did not answer; record = %v", record["outcome"], record)
				}
			})
		}
	}
}

// A send or publish the stream refuses for the message itself (bus.StreamRefusesMessage) was
// answered, and the answer is final: the 409 under streamRefusedPrefix, with no Retry-After, and
// the request record's outcome stream_refused, so an operator reading it looks at the stream's
// configuration or the message's size rather than for an outage or a full stream. The record reads
// the 409 back from the answer: a refusal of a resend after a send nothing answered is the
// maybe-stored 503, since the first send may have stored the message, and is recorded as that 503
// is, bus_unavailable, never stream_refused beside a 503.
func TestAStreamRefusalIsAnsweredAndRecordedAsARefusal(t *testing.T) {
	t.Run("an unanswered send, then the stream's refusal of the resend", func(t *testing.T) {
		failure := fmt.Errorf("%w: %w", bus.ErrUnacknowledged,
			&nats.APIError{Code: http.StatusBadRequest, ErrorCode: 10109, Description: "invalid operation on sealed stream"})
		recorder, record := serveRecorded(t, func(logger *logging.Logger) http.HandlerFunc {
			return func(w http.ResponseWriter, r *http.Request) {
				rec := &requestRecord{logger: logger, line: "listener publish", laneKey: "topic", lane: contracts.RoleTopicPrefix + "reviewer"}
				rec.refuseBusUnavailable(w, failure)
			}
		}, postJSON("/v1/messages/publish", "{}"))
		var answer listenerwire.APIError
		if err := json.Unmarshal(recorder.Body.Bytes(), &answer); err != nil {
			t.Fatalf("decode the answer %s: %v", recorder.Body.String(), err)
		}
		if recorder.Code != http.StatusServiceUnavailable || answer.Code != listenerwire.CodeMaybeStored {
			t.Fatalf("answered %d %s, want the 503 with code %s", recorder.Code, recorder.Body.String(), listenerwire.CodeMaybeStored)
		}
		if record["outcome"] != "bus_unavailable" || record["status"] != float64(http.StatusServiceUnavailable) {
			t.Fatalf("the request record's outcome is %v with status %v, want bus_unavailable with 503, as the answer was; record = %v", record["outcome"], record["status"], record)
		}
	})
	subject := contracts.RoleTopicPrefix + "reviewer"
	for _, route := range []struct {
		name    string
		line    string
		laneKey string
		lane    string
	}{
		{"publish", "listener publish", "topic", subject},
		{"send", "listener send", "target_session", "ses_refused_target"},
	} {
		for _, failure := range []struct {
			name string
			err  error
		}{
			{"a sealed stream", fmt.Errorf("%w: %w", bus.ErrForwardNotStored,
				&nats.APIError{Code: http.StatusBadRequest, ErrorCode: 10109, Description: "invalid operation on sealed stream"})},
			{"a message past the stream's maximum size", fmt.Errorf("%w: %w", bus.ErrForwardNotStored,
				&nats.APIError{Code: http.StatusBadRequest, ErrorCode: 10054, Description: "message size exceeds maximum allowed"})},
		} {
			t.Run(route.name+"/"+failure.name, func(t *testing.T) {
				recorder, record := serveRecorded(t, func(logger *logging.Logger) http.HandlerFunc {
					return func(w http.ResponseWriter, r *http.Request) {
						rec := &requestRecord{logger: logger, line: route.line, laneKey: route.laneKey, lane: route.lane}
						rec.refuseBusUnavailable(w, failure.err)
					}
				}, postJSON("/v1/messages/publish", "{}"))
				if recorder.Code != http.StatusConflict {
					t.Fatalf("status = %d, want 409: no retry gets past the stream's refusal; body = %s", recorder.Code, recorder.Body.String())
				}
				if got := recorder.Header().Get("Retry-After"); got != "" {
					t.Fatalf("Retry-After is %q, want none: no wait changes the stream's answer", got)
				}
				var refusal listenerwire.APIError
				if err := json.Unmarshal(recorder.Body.Bytes(), &refusal); err != nil {
					t.Fatalf("decode the refusal %s: %v", recorder.Body.String(), err)
				}
				if !strings.HasPrefix(refusal.Error, streamRefusedPrefix) {
					t.Fatalf("the refusal reads %q, want it to open with %q", refusal.Error, streamRefusedPrefix)
				}
				if record["outcome"] != "stream_refused" || record["status"] != float64(http.StatusConflict) {
					t.Fatalf("the request record's outcome is %v with status %v, want stream_refused with 409; record = %v", record["outcome"], record["status"], record)
				}
			})
		}
	}
}
