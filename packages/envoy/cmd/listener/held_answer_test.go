// How the repeat-answer tests read an answer and the copy the stream holds for it.

package main

import (
	"context"
	"encoding/json"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/contracts"
)

// heldAnswer is a send's or a publish's answer as these tests read it: the limitedAnswer (status,
// body, the request's record line), with the envelope the body carries.
type heldAnswer struct {
	limitedAnswer
	envelope contracts.Envelope
}

// held reads the envelope out of answer's body. An answer that carries none, a refusal, reads as
// the zero envelope.
func held(t *testing.T, answer limitedAnswer) heldAnswer {
	t.Helper()
	read := heldAnswer{limitedAnswer: answer}
	if err := json.Unmarshal([]byte(answer.raw), &read.envelope); err != nil {
		t.Fatalf("decode the envelope of the answer %s: %v", answer.raw, err)
	}
	return read
}

// heldCopy is the one copy the stream holds on subject, read with client, and fails the test
// unless the stream holds exactly one.
func heldCopy(t *testing.T, client *bus.Client, subject string) contracts.Envelope {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	stored, err := readStored(ctx, client, subject)
	if err != nil {
		t.Fatalf("read what the stream holds on %s: %v", subject, err)
	}
	if len(stored) != 1 {
		t.Fatalf("the stream holds %d copies on %s, want the one copy the first request stored", len(stored), subject)
	}
	var kept contracts.Envelope
	if err := json.Unmarshal(stored[0].Data, &kept); err != nil {
		t.Fatalf("decode the copy the stream holds on %s: %v", subject, err)
	}
	return kept
}

// requireHeld fails unless the answer is a 200 whose envelope is kept, field for field (its
// event_id, its issued_at, its body and every other field), whose duplicate is duplicate, and
// whose record line names kept's event_id.
func (a heldAnswer) requireHeld(t *testing.T, what string, kept contracts.Envelope, duplicate bool) {
	t.Helper()
	a.requireStatus(t, what, http.StatusOK)
	if !reflect.DeepEqual(a.envelope, kept) {
		t.Fatalf("%s answered event_id %s issued_at %d, want the stored copy, event_id %s issued_at %d:\nanswer %+v\nstored %+v",
			what, a.envelope.EventID, a.envelope.IssuedAt, kept.EventID, kept.IssuedAt, a.envelope, kept)
	}
	if a.Duplicate != duplicate {
		t.Fatalf("%s answered duplicate %t, want %t; body %s", what, a.Duplicate, duplicate, a.raw)
	}
	if got := a.line["event_id"]; got != kept.EventID {
		t.Fatalf("%s: the request's line names event_id %v, want the stored copy's %s; line %v", what, got, kept.EventID, a.line)
	}
}

// heldForward is the one forward the stream holds on holder's agent subject, as the role route
// answers it: with the forward mark, which it must carry, taken off its dedupe key
// (roleForwardDedupePrefix).
func heldForward(t *testing.T, client *bus.Client, holder string) contracts.Envelope {
	t.Helper()
	forward := heldCopy(t, client, contracts.LegacyAgentSubject(holder))
	if !strings.HasPrefix(forward.DedupeKey, roleForwardDedupePrefix) {
		t.Fatalf("the forward stored for %s has dedupe key %q, want it under %s", holder, forward.DedupeKey, roleForwardDedupePrefix)
	}
	forward.DedupeKey = strings.TrimPrefix(forward.DedupeKey, roleForwardDedupePrefix)
	return forward
}

// expectsReplyOf is the expects_reply of answer's body, which must be a 200, as the body wrote
// it: "" where the answer omits the field.
func expectsReplyOf(t *testing.T, what string, answer limitedAnswer) string {
	t.Helper()
	answer.requireStatus(t, what, http.StatusOK)
	var fields map[string]json.RawMessage
	if err := json.Unmarshal([]byte(answer.raw), &fields); err != nil {
		t.Fatalf("%s: decode %s: %v", what, answer.raw, err)
	}
	return string(fields["expects_reply"])
}
