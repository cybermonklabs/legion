package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/nats-io/nats.go"
	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/bus/bustest"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/testnats"
	"github.com/sjawhar/envoy/listenerwire"
)

// A claim that takes a role from another session writes a takeover record first, so a claim that
// lands always has its record (R2). A record the stream did not store - here because the stream
// holds its byte limit (bus.WithStreamMaxBytes, bustest.FillStream), so the row reads the refusal
// the server gives the record's own write and not one a seam made - ends the claim with nothing
// written: the claim route answers 503 with Retry-After: 1 and no code, the previous holder keeps
// the role, and the claimant's interest record keeps no topic of it. Every try while the stream is
// full is refused, and the first try after the stream has room again (its fill purged, as age
// makes room in a deployment) lands, with its record.
//
// The control is the same claim on the same rig with room in the stream: it lands, with its
// record, so the 503 rows come from the record that did not store and from nothing else of the
// rig. A record write that failed another way (a seam) is the same 503.
func TestATakeoverClaimWhoseRecordDidNotStoreWritesNoClaim(t *testing.T) {
	t.Parallel()
	client := setupPublishTestClient(t, bus.WithStreamMaxBytes(16<<10))
	rig := newSessionConsumerRigOn(t, client, testnats.URL(t))
	const previous, claimant = "ses_full_stream_previous", "ses_full_stream_claimant"
	rig.subscribe(t, consumerRegistration(previous, "epoch-1"))
	rig.subscribe(t, consumerRegistration(claimant, "epoch-1"))
	claim := func(sessionID, role string) *httptest.ResponseRecorder {
		return postRoleClaim(rig.mux, sessionID, role, "", false)
	}
	holder := func(role string) string {
		t.Helper()
		claimed, err := rig.harness.registry.RoleHolder(role)
		if err != nil {
			t.Fatalf("read role %s's claim: %v", role, err)
		}
		return claimed
	}
	records := func(role string) int {
		t.Helper()
		stored, err := rig.harness.client.TakeoverRecords(context.Background(), role)
		if err != nil {
			t.Fatalf("read the takeover records of %s: %v", role, err)
		}
		return len(stored)
	}
	wroteNothing := func(t *testing.T, recorder *httptest.ResponseRecorder, role string) {
		t.Helper()
		var body listenerwire.APIError
		_ = json.Unmarshal(recorder.Body.Bytes(), &body)
		if recorder.Code != http.StatusServiceUnavailable || recorder.Header().Get("Retry-After") != "1" || body.Code != "" {
			t.Fatalf("the takeover claim whose record did not store: %d (Retry-After %q, code %q) %s; want 503 with Retry-After: 1 and no code",
				recorder.Code, recorder.Header().Get("Retry-After"), body.Code, recorder.Body.String())
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
	}

	t.Run("control: with room in the stream, the takeover claim lands with its record", func(t *testing.T) {
		const role = "full-stream-control"
		if recorder := claim(previous, role); recorder.Code != http.StatusOK {
			t.Fatalf("the previous holder's claim: %d %s", recorder.Code, recorder.Body.String())
		}
		if recorder := claim(claimant, role); recorder.Code != http.StatusOK {
			t.Fatalf("the takeover claim: %d %s; want 200", recorder.Code, recorder.Body.String())
		}
		if got := holder(role); got != claimant {
			t.Fatalf("role %s is held by %q, want %q", role, got, claimant)
		}
		if got := records(role); got != 1 {
			t.Fatalf("the stream holds %d takeover records of %s, want 1", got, role)
		}
	})

	t.Run("a record write that failed another way writes no claim and is answered 503", func(t *testing.T) {
		const role = "full-stream-other-failure"
		if recorder := claim(previous, role); recorder.Code != http.StatusOK {
			t.Fatalf("the previous holder's claim: %d %s", recorder.Code, recorder.Body.String())
		}
		setTakeoverFaults(t, rig.harness.config, takeoverFaultSet{claimRecordWrite: func(r, _ string) error {
			if r != role {
				return nil
			}
			return errors.New("the stream did not answer the takeover record's write")
		}})
		wroteNothing(t, claim(claimant, role), role)
	})

	t.Run("a takeover claim whose record the full stream refuses writes no claim on any try while the stream is full, and lands on the first try after it has room", func(t *testing.T) {
		const role = "full-stream-refused"
		if recorder := claim(previous, role); recorder.Code != http.StatusOK {
			t.Fatalf("the previous holder's claim: %d %s", recorder.Code, recorder.Body.String())
		}
		bustest.FillStream(t, client)
		for try := range 3 {
			recorder := claim(claimant, role)
			if !strings.Contains(recorder.Body.String(), bus.ErrStreamFull.Error()) {
				t.Fatalf("try %d answered %s; want the full stream's refusal of the record named", try, recorder.Body.String())
			}
			wroteNothing(t, recorder, role)
		}
		if got := records(role); got != 0 {
			t.Fatalf("the stream holds %d takeover records of %s, want none: the stream refused each", got, role)
		}
		// Age makes room in a deployment; the purge of the fill makes it here.
		if err := client.JS().PurgeStream(bus.Stream, &nats.StreamPurgeRequest{Subject: bustest.FillSubject + ".>"}); err != nil {
			t.Fatalf("purge the fill: %v", err)
		}
		if recorder := claim(claimant, role); recorder.Code != http.StatusOK {
			t.Fatalf("the takeover claim once the stream has room: %d %s; want 200", recorder.Code, recorder.Body.String())
		}
		if got := holder(role); got != claimant {
			t.Fatalf("role %s is held by %q, want %q", role, got, claimant)
		}
		if got := records(role); got != 1 {
			t.Fatalf("the stream holds %d takeover records of %s, want 1: the claim that landed wrote its record first", got, role)
		}
	})
}
