package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/logging"
	"github.com/sjawhar/envoy/listenerwire"
)

// A message the bus refused before it sent anything, because the deployed stream does not capture
// a topic this build stores (bus.ErrSubjectNotDeployed), never reached the bus, so its answer must
// not say the bus did not answer. Both routes answer it 503 with Retry-After: 1, since the same
// message is stored once the stream captures the topic again, with a body that opens with
// contracts.MessageNotSentPrefix and goes on to the bus's own reason, and with no maybe_stored
// code: envoy-client reads that prefix as a message nothing stored, which its sender may send
// again later, where every other 5xx is one after which the message may have arrived. The request
// record's outcome is subject_not_deployed, not bus_unavailable, the bucket an operator reads as
// the bus going quiet.
//
// The control: a refusal that follows a send nothing answered (bus.ErrUnacknowledged) may have
// stored the message, so it keeps the maybe_stored answer under busUnansweredPrefix and the
// bus_unavailable outcome, and says nothing of a message not sent. It fails a classification that
// looks at bus.ErrSubjectNotDeployed alone.
func TestARefusalBeforeAnythingWasSentSaysTheMessageWasNotSent(t *testing.T) {
	t.Parallel()
	topic := contracts.LegacyAgentSubject("ses_not_sent_target")
	// As internal/bus's subjectNotDeployed names it: the sentinel, the stream and the topic.
	notDeployed := fmt.Errorf("%w (stream %s, topic %s)", bus.ErrSubjectNotDeployed, bus.Stream, topic)
	afterUnanswered := errors.Join(fmt.Errorf("publish to %s: %w", topic, bus.ErrUnacknowledged), notDeployed)

	for _, route := range []struct {
		name    string
		line    string
		laneKey string
		lane    string
	}{
		{"publish", "listener publish", "topic", "notifications.team.not-sent"},
		{"send", "listener send", "target_session", "ses_not_sent_target"},
	} {
		for _, tc := range []struct {
			name        string
			err         error
			wantBody    string
			wantCode    string
			wantOutcome string
		}{
			{
				name:        "a message whose topic the deployed stream does not capture",
				err:         notDeployed,
				wantBody:    contracts.MessageNotSentPrefix + ": " + notDeployed.Error(),
				wantOutcome: "subject_not_deployed",
			},
			{
				name:        "the same refusal after a send nothing answered",
				err:         afterUnanswered,
				wantBody:    busUnansweredPrefix + ": " + afterUnanswered.Error(),
				wantCode:    listenerwire.CodeMaybeStored,
				wantOutcome: "bus_unavailable",
			},
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
					t.Fatalf("Retry-After = %q, want 1", got)
				}
				var refusal listenerwire.APIError
				if err := json.Unmarshal(recorder.Body.Bytes(), &refusal); err != nil {
					t.Fatalf("decode the refusal %s: %v", recorder.Body.String(), err)
				}
				if refusal.Error != tc.wantBody {
					t.Fatalf("the refusal reads %q, want %q", refusal.Error, tc.wantBody)
				}
				if tc.wantCode == "" && strings.HasPrefix(refusal.Error, busUnansweredPrefix) {
					t.Fatalf("the refusal reads %q: it says the bus did not answer, but the bus was never asked", refusal.Error)
				}
				if refusal.Code != tc.wantCode {
					t.Fatalf("the refusal's code is %q, want %q", refusal.Code, tc.wantCode)
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
