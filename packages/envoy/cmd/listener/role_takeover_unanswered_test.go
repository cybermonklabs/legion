package main

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/testnats"
	"github.com/sjawhar/envoy/listenerwire"
)

// A takeover claim whose record's write the bus does not answer writes no claim, and the claim
// route answers it as it answers the claim's own write the bus did not answer (writeStoreError):
// 503 with Retry-After: 1, and an error that opens with busUnansweredPrefix and then names the
// takeover record. The bus is real: the listener's connection goes through a relay that drops
// every publish on the takeover lane (bus.TakeoverSubjectPrefix), so the server never sees the
// record's write and nothing answers it, while every other call of the claim is answered. The
// previous holder keeps the role, and the stream holds no record of it.
func TestATakeoverRecordWriteTheBusDidNotAnswerIsAnsweredAsTheBusUnanswered(t *testing.T) {
	url := testnats.URL(t)
	relay := startSubjectStallRelay(t, url)
	client, err := bus.ConnectOwningStream([]string{relay.relayed}, bus.WithReplicas(1))
	if err != nil {
		t.Fatalf("connect the listener through the relay: %v", err)
	}
	t.Cleanup(client.Close)
	rig := newSessionConsumerRigOn(t, client, url)
	const role, previous, claimant = "unanswered-record", "ses_unanswered_previous", "ses_unanswered_claimant"
	rig.subscribe(t, consumerRegistration(previous, "epoch-1"))
	rig.subscribe(t, consumerRegistration(claimant, "epoch-1"))
	grantRoleClaim(t, rig.mux, previous, role, "", false)

	relay.stall(bus.TakeoverSubjectPrefix)
	recorder := postRoleClaim(rig.mux, claimant, role, "", false)
	var answer listenerwire.APIError
	if err := json.Unmarshal(recorder.Body.Bytes(), &answer); err != nil {
		t.Fatalf("decode the claim's answer %s: %v", recorder.Body.String(), err)
	}
	if recorder.Code != http.StatusServiceUnavailable || recorder.Header().Get("Retry-After") != "1" || answer.Code != "" {
		t.Fatalf("the takeover claim whose record nothing answered: %d (Retry-After %q, code %q) %s; want 503 with Retry-After: 1 and no code",
			recorder.Code, recorder.Header().Get("Retry-After"), answer.Code, recorder.Body.String())
	}
	if want := busUnansweredPrefix + ": " + errTakeoverUnrecorded.Error(); !strings.HasPrefix(answer.Error, want) {
		t.Fatalf("the claim's error reads %q, want it to begin %q", answer.Error, want)
	}
	t.Logf("the claim's answer: %d %s", recorder.Code, answer.Error)

	if got, err := rig.harness.registry.RoleHolder(role); err != nil || got != previous {
		t.Fatalf("role %s is held by %q (%v), want %q: the claim wrote nothing", role, got, err, previous)
	}
	records, err := rig.harness.client.TakeoverRecords(context.Background(), role)
	if err != nil {
		t.Fatalf("read the takeover records of %s: %v", role, err)
	}
	if len(records) != 0 {
		t.Fatalf("the stream holds %d takeover records of %s, want none: the relay dropped the record's write", len(records), role)
	}
}
