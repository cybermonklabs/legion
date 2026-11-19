package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/bus/bustest"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/testnats"
)

// takeoverUnrecordedLine and takeoverUnrecordedSeries are what an operator reads of a takeover
// claim that went on with no record (takeoverBeforeTry).
const (
	takeoverUnrecordedLine   = "listener role claim goes on with no takeover record: the stream is full, so the previous holder's unread role messages are not copied for the role's next holder"
	takeoverUnrecordedSeries = "envoy_role_takeover_unrecorded_total"
)

// A claim that takes a role from another session writes a takeover record first, and the stream
// refuses every write while it holds its byte limit (bus.ErrStreamFull). Such a claim lands all the
// same: refusing it would keep the role with a holder that may have stopped for as long as the
// stream stays full. It writes one Error line naming the role, the previous holder and the
// claimant, and counts on envoy_role_takeover_unrecorded_total, since the previous holder's unread
// role messages are then not copied for the role's next holder. The stream is really full here
// (bus.WithStreamMaxBytes, bustest.FillStream), so the row reads the refusal the server gives the
// record's own write, not one a seam made.
//
// The control is a record write that failed for any other reason, on the same rig before its
// stream is full: the claim writes nothing and is answered 503 with Retry-After: 1, so its client
// claims again, and nothing is counted. It shows the full-stream row's 200 comes from the refusal
// being the stream's byte limit and from nothing else of the rig.
func TestATakeoverClaimLandsWhenTheStreamIsFullAndSaysSo(t *testing.T) {
	client := setupPublishTestClient(t, bus.WithStreamMaxBytes(16<<10))
	rig := newSessionConsumerRigOn(t, client, testnats.URL(t))
	rig.deps.delivery.roleTakeoverUnrecorded = rig.harness.metrics.NewCounter(takeoverUnrecordedSeries, "test")
	const previous, claimant = "ses_full_stream_previous", "ses_full_stream_claimant"
	rig.subscribe(t, consumerRegistration(previous, "epoch-1"))
	rig.subscribe(t, consumerRegistration(claimant, "epoch-1"))
	claim := func(sessionID, role string) *httptest.ResponseRecorder {
		t.Helper()
		recorder := httptest.NewRecorder()
		rig.mux.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/v1/roles/set",
			strings.NewReader(`{"session_id":"`+sessionID+`","role":"`+role+`"}`)))
		return recorder
	}
	holder := func(role string) string {
		t.Helper()
		claimed, err := rig.harness.registry.RoleHolder(role)
		if err != nil {
			t.Fatalf("read role %s's claim: %v", role, err)
		}
		return claimed
	}
	unrecorded := func() int64 {
		return max(0, counterValue(t, metricsBody(t, rig.harness), takeoverUnrecordedSeries))
	}

	t.Run("control: a record write that failed another way writes no claim, is answered 503 and counts nothing", func(t *testing.T) {
		const role = "full-stream-control"
		if recorder := claim(previous, role); recorder.Code != http.StatusOK {
			t.Fatalf("the previous holder's claim: %d %s", recorder.Code, recorder.Body.String())
		}
		setTakeoverFaults(t, takeoverFaultSet{claimRecordWrite: func(r, _ string) error {
			if r != role {
				return nil
			}
			return errors.New("the stream did not store the takeover record")
		}})
		before := unrecorded()
		recorder := claim(claimant, role)
		if recorder.Code != http.StatusServiceUnavailable || recorder.Header().Get("Retry-After") != "1" {
			t.Fatalf("the takeover claim whose record failed: %d (Retry-After %q) %s; want 503 with Retry-After: 1",
				recorder.Code, recorder.Header().Get("Retry-After"), recorder.Body.String())
		}
		if got := holder(role); got != previous {
			t.Fatalf("role %s is held by %q, want %q: the claim wrote nothing", role, got, previous)
		}
		interest, err := rig.harness.registry.Get(context.Background(), claimant)
		if err != nil {
			t.Fatalf("read %s's interest record: %v", claimant, err)
		}
		for _, topic := range interest.Topics {
			if topic == contracts.RoleTopicPrefix+role {
				t.Fatalf("%s's interest record keeps %s after a claim that wrote nothing: %v", claimant, topic, interest.Topics)
			}
		}
		if got := unrecorded() - before; got != 0 {
			t.Fatalf("%s rose by %d, want 0: the record failed for another reason than a full stream", takeoverUnrecordedSeries, got)
		}
	})

	t.Run("a takeover claim whose record the full stream refuses lands, logs the role and the previous holder, and is counted", func(t *testing.T) {
		const role = "full-stream-lands"
		if recorder := claim(previous, role); recorder.Code != http.StatusOK {
			t.Fatalf("the previous holder's claim: %d %s", recorder.Code, recorder.Body.String())
		}
		bustest.FillStream(t, client, 64)
		before := unrecorded()
		recorder := claim(claimant, role)
		if recorder.Code != http.StatusOK {
			t.Fatalf("the takeover claim under a full stream: %d %s; want 200, the claim landed", recorder.Code, recorder.Body.String())
		}
		if got := holder(role); got != claimant {
			t.Fatalf("role %s is held by %q, want %q", role, got, claimant)
		}
		if lines := linesIn(rig.harness.logs.String(), takeoverUnrecordedLine, map[string]any{
			"role": role, "previous_session_id": previous, "session_id": claimant,
		}); len(lines) != 1 {
			t.Fatalf("found %d lines naming the role, the previous holder and the claimant, want one: %q", len(lines), takeoverUnrecordedLine)
		}
		if got := unrecorded() - before; got != 1 {
			t.Fatalf("%s rose by %d, want 1", takeoverUnrecordedSeries, got)
		}
		records, err := rig.harness.client.TakeoverRecords(context.Background(), role)
		if err != nil {
			t.Fatalf("read the takeover records of %s: %v", role, err)
		}
		if len(records) != 0 {
			t.Fatalf("the stream holds %d takeover records of %s, want none: the stream refused the record", len(records), role)
		}
	})
}
