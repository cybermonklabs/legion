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

// requireKeyTaken fails unless the answer is status with duplicate and kept's event_id and times
// (issued_at, expires_at, kept_at), and carries none of what kept's sender wrote or who it is: no
// payload, payload_summary, payload_ref, source, source_session or sender, and not words, which
// that sender wrote. It is the answer a caller that reads the key-taken answer
// (listenerwire.ReadsKeyTaken) gets for repeating another sender's key (requestSender.answer).
func (a heldAnswer) requireKeyTaken(t *testing.T, what string, status int, kept contracts.Envelope, words string) {
	t.Helper()
	a.requireStatus(t, what, status)
	if !a.Duplicate {
		t.Fatalf("%s answered %s, want duplicate", what, a.raw)
	}
	got := a.envelope
	if got.EventID != kept.EventID || got.IssuedAt != kept.IssuedAt ||
		!reflect.DeepEqual(got.ExpiresAt, kept.ExpiresAt) || !reflect.DeepEqual(got.KeptAt, kept.KeptAt) {
		t.Fatalf("%s answered %s, want the stored copy's event_id and times: %+v", what, a.raw, kept)
	}
	if got.Payload != "" || got.PayloadSummary != "" || got.PayloadRef != "" || got.Source != "" ||
		got.SourceSession != "" || got.Sender != nil || strings.Contains(a.raw, words) {
		t.Fatalf("%s answered %s, which carries what the other sender wrote or who it is", what, a.raw)
	}
}

// requireOwnEnvelope fails unless the answer is status with duplicate and the envelope the
// request built: an event_id that is not kept's, the asker's session and its own words asked,
// whole by the envelope schema every client release parses (listenerwire.Envelope.Validate, which
// @legion/contracts' EnvelopeSchema mirrors), and none of what kept's sender wrote (words); and
// unless the request's record line names kept's event_id, session and asked. It is the answer a
// caller that does not read the key-taken answer gets for repeating another sender's key
// (requestSender.answer), the one a listener before that answer gave every repeat.
func (a heldAnswer) requireOwnEnvelope(t *testing.T, what string, status int, kept contracts.Envelope, words, session, asked string) {
	t.Helper()
	a.requireStatus(t, what, status)
	got := a.envelope
	if !a.Duplicate || got.EventID == "" || got.EventID == kept.EventID || got.SourceSession != session || got.PayloadSummary != asked {
		t.Fatalf("%s answered %s, want duplicate and the envelope its own request built: a new event_id, session %q and its own words %q", what, a.raw, session, asked)
	}
	if err := got.Validate(); err != nil {
		t.Fatalf("%s answered %s, which the envelope schema every client parses refuses: %v", what, a.raw, err)
	}
	if strings.Contains(a.raw, words) {
		t.Fatalf("%s answered %s, which carries what the other sender wrote", what, a.raw)
	}
	if a.line["event_id"] != kept.EventID || a.line["source_session"] != session || a.line["payload_summary"] != asked {
		t.Fatalf("%s wrote the line %v, want the copy's event_id %s, the asker's session %q and its own words %q", what, a.line, kept.EventID, session, asked)
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
