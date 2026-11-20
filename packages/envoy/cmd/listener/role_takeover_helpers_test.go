package main

// The test helpers the takeover copy's rows share: a role claim and a role's release through the
// listener's own routes, and what the stream holds on a subject, each message with its envelope.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/store"
)

// postRoleClaim is handler's answer to POST /v1/roles/set of role by sessionID, outright or soft,
// naming instance as its process when it is not empty. It touches no testing.T, so a row may make
// it on a goroutine of its own.
func postRoleClaim(handler http.Handler, sessionID, role, instance string, soft bool) *httptest.ResponseRecorder {
	fields := map[string]any{"session_id": sessionID, "role": role}
	if soft {
		fields["soft"] = true
	}
	if instance != "" {
		fields["client_instance_id"] = instance
	}
	body, err := json.Marshal(fields)
	if err != nil {
		panic(err)
	}
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, postJSON("/v1/roles/set", string(body)))
	return recorder
}

// grantRoleClaim is postRoleClaim, and fails t unless the claim is granted.
func grantRoleClaim(t testing.TB, handler http.Handler, sessionID, role, instance string, soft bool) {
	t.Helper()
	if recorder := postRoleClaim(handler, sessionID, role, instance, soft); recorder.Code != http.StatusOK {
		t.Fatalf("%s claims %s (process %q, soft %v): status %d %s; want 200", sessionID, role, instance, soft, recorder.Code, recorder.Body.String())
	}
}

// releaseRoleThrough has sessionID's process instance hand role back through handler's POST
// /v1/interests/unsubscribe, naming the role's lane topic, which frees a claim naming that
// process, and fails t unless the route answers 200 and no claim of role stands after it, as
// registry reads it.
func releaseRoleThrough(t testing.TB, handler http.Handler, registry *store.Registry, sessionID, role, instance string) {
	t.Helper()
	body, err := json.Marshal(map[string]any{
		"session_id":         sessionID,
		"topics":             []string{contracts.RoleTopicPrefix + role},
		"client_instance_id": instance,
	})
	if err != nil {
		t.Fatal(err)
	}
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, postJSON("/v1/interests/unsubscribe", string(body)))
	if recorder.Code != http.StatusOK {
		t.Fatalf("%s releases %s: status %d %s; want 200", sessionID, role, recorder.Code, recorder.Body.String())
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	claim, err := registry.RoleClaim(ctx, role)
	if err != nil {
		t.Fatalf("read role %s's claim after %s released it: %v", role, sessionID, err)
	}
	if claim.HolderSessionID != "" {
		t.Fatalf("role %s's claim names %s after %s released it; want none", role, claim.HolderSessionID, sessionID)
	}
}

// storedEnvelope is one message the stream holds on a subject: its stream sequence, when the
// stream stored it, and its envelope.
type storedEnvelope struct {
	seq  uint64
	at   time.Time
	item contracts.Envelope
}

// storedEnvelopes is every message the stream holds on subject, in stream order (readStored), with
// its envelope. A read that fails, and a message no envelope decodes, fail t.
func storedEnvelopes(t testing.TB, client *bus.Client, subject string) []storedEnvelope {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	messages, err := readStored(ctx, client, subject)
	if err != nil {
		t.Fatalf("read what the stream holds on %s: %v", subject, err)
	}
	held := make([]storedEnvelope, 0, len(messages))
	for _, message := range messages {
		var item contracts.Envelope
		if err := json.Unmarshal(message.Data, &item); err != nil {
			t.Fatalf("decode the message at %d on %s: %v", message.Sequence, subject, err)
		}
		held = append(held, storedEnvelope{seq: message.Sequence, at: message.Time, item: item})
	}
	return held
}

// copiesOf is how many messages of held carry eventID.
func copiesOf(held []storedEnvelope, eventID string) int {
	copies := 0
	for _, message := range held {
		if message.item.EventID == eventID {
			copies++
		}
	}
	return copies
}
