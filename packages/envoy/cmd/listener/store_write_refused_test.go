package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/logging"
	"github.com/sjawhar/envoy/internal/session"
)

// A store write the stream refuses for the message itself reaches the store write routes' answer
// as the 409 a send or publish gets (writeStoreError, writeBusUnavailable): the role claim bucket's
// stream is sealed, which a server refuses every write to, and a claim through POST /v1/roles/set
// is answered 409 under streamRefusedPrefix with no Retry-After, not the bus-unavailable 503 that
// asks its sender to send it again. The claim's reads still answer, so the refusal is the write's.
func TestAStoreWriteASealedBucketRefusesIsAnsweredAsARefusal(t *testing.T) {
	harness := newListenerDeliveryHarness(t)
	deps := withRoleClaimDrain(t, &listenerDeps{client: harness.client, registry: harness.registry, sessions: harness.sessions})
	const (
		sessionID = "ses_sealed_claimant"
		role      = "sealed-claim"
	)
	if err := harness.sessions.Put(sessionID, session.SessionEntry{MachineID: "test-machine", SelfSubscribed: true}); err != nil {
		t.Fatalf("register %s: %v", sessionID, err)
	}
	stream := "KV_" + bus.RoleClaimBucket
	info, err := harness.client.JS().StreamInfo(stream)
	if err != nil {
		t.Fatalf("read the role claim bucket's stream %s: %v", stream, err)
	}
	config := info.Config
	config.Sealed = true
	if _, err := harness.client.JS().UpdateStream(&config); err != nil {
		t.Fatalf("seal %s: %v", stream, err)
	}

	recorder := httptest.NewRecorder()
	roleSetHandler(deps, "test-machine", logging.New("test")).ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/v1/roles/set",
		strings.NewReader(fmt.Sprintf(`{"session_id":%q,"role":%q}`, sessionID, role))))

	if recorder.Code != http.StatusConflict || recorder.Header().Get("Retry-After") != "" {
		t.Fatalf("a claim the sealed claim bucket refuses: status %d, Retry-After %q, body %s; want 409 with no Retry-After",
			recorder.Code, recorder.Header().Get("Retry-After"), recorder.Body.String())
	}
	if !strings.Contains(recorder.Body.String(), streamRefusedPrefix) {
		t.Fatalf("the 409's body %q does not name the stream's refusal (%s)", recorder.Body.String(), streamRefusedPrefix)
	}
}
