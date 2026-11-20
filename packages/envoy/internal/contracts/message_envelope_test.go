package contracts

import (
	"encoding/json"
	"strings"
	"testing"
)

// The copy Dispatch queues itself for a session no store of the listener's holds and the send the
// listener's route stores are one message to the stream and to the session's host only while
// both go on the session's agent subject under the one dedupe key of that delivery, which the
// stream counts as naming the event; each build is still an event of its own.
func TestAgentSendEnvelopeIsTheSessionsSendUnderItsKey(t *testing.T) {
	frame := `{"delivery":{"id":"att-1"}}`
	send := Message{
		Source:       "dispatch",
		Text:         "Please look at the failing check",
		Payload:      &frame,
		Urgency:      "high",
		ExpectsReply: ExpectsReplyRequired,
	}
	first := AgentSendEnvelope("ses_a", "msg-1:send", send)
	again := AgentSendEnvelope("ses_a", "msg-1:send", send)

	if first.Topic != "notifications.agent.ses_a" {
		t.Errorf("topic = %q, want the session's agent subject notifications.agent.ses_a", first.Topic)
	}
	if first.DedupeKey != "agent.ses_a.msg-1:send" {
		t.Errorf("dedupe key = %q, want agent.ses_a.msg-1:send", first.DedupeKey)
	}
	if again.Topic != first.Topic || again.DedupeKey != first.DedupeKey {
		t.Errorf("a second build of one delivery = %q under %q, want %q under %q", again.Topic, again.DedupeKey, first.Topic, first.DedupeKey)
	}
	if again.EventID == first.EventID || again.SourceEventID == first.SourceEventID || again.TraceID == first.TraceID {
		t.Errorf("a second build of one delivery reuses an id of the first (event %q, source event %q, trace %q)", again.EventID, again.SourceEventID, again.TraceID)
	}
	if !DedupeKeyNamesTheUpstreamEvent(first) {
		t.Errorf("the stream would not take dedupe key %q from %q as naming its event, so it would store both copies", first.DedupeKey, first.Source)
	}
	if err := first.Validate(); err != nil {
		t.Errorf("Validate() = %v, want the envelope accepted", err)
	}
	if first.Source != "dispatch" || first.PayloadSummary != send.Text || first.Payload != frame ||
		first.Urgency != "high" || first.ExpectsReply != ExpectsReplyRequired {
		t.Errorf("envelope = source %q, summary %q, payload %q, urgency %q, expects_reply %q; want what the sender gave",
			first.Source, first.PayloadSummary, first.Payload, first.Urgency, first.ExpectsReply)
	}
}

// A send that names no idempotency key is given one of its own, in the shape the stream and every
// host count as a minted key, so a re-send of that envelope is dropped as a repeat and another
// send is not.
func TestAgentSendEnvelopeMintsAKeyForASendThatNamesNone(t *testing.T) {
	first := AgentSendEnvelope("ses_a", "", Message{Text: "hi"})
	second := AgentSendEnvelope("ses_a", "", Message{Text: "hi"})
	if !strings.HasPrefix(first.DedupeKey, "agent.ses_a.") || !mintedDedupeKey.MatchString(first.DedupeKey) {
		t.Errorf("dedupe key = %q, want agent.ses_a. and a minted key", first.DedupeKey)
	}
	if !DedupeKeyNamesTheUpstreamEvent(first) {
		t.Errorf("the stream would not take minted dedupe key %q as naming its event", first.DedupeKey)
	}
	if first.DedupeKey == second.DedupeKey {
		t.Errorf("two sends that name no key share dedupe key %q, so the second would be dropped", first.DedupeKey)
	}
}

// A sender that names no source is a session's own message, and an urgency, an expects_reply or a
// payload it did not name is absent from the envelope on the wire, as the listener has always
// written it.
func TestMessageEnvelopeLeavesOutWhatTheSenderDidNotName(t *testing.T) {
	item := AgentSendEnvelope("ses_a", "k", Message{Text: "hi"})
	if item.Source != "agent" {
		t.Errorf("source = %q, want agent", item.Source)
	}
	if err := item.Validate(); err != nil {
		t.Errorf("Validate() = %v, want the envelope accepted", err)
	}
	raw, err := json.Marshal(item)
	if err != nil {
		t.Fatalf("marshal the envelope: %v", err)
	}
	if err := ValidateWire(raw); err != nil {
		t.Errorf("ValidateWire(%s) = %v, want it accepted", raw, err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		t.Fatalf("decode the envelope: %v", err)
	}
	for _, name := range []string{"urgency", "expects_reply", "payload", "in_reply_to", "supersedes", "expires_at", "source_session"} {
		if value, present := fields[name]; present {
			t.Errorf("the envelope carries %s = %s, want it absent", name, value)
		}
	}
}

// The summary is the text's first non-empty line, cut to 160 runes; the payload is the text whole
// where the summary is not all of it, unless the sender gave a payload, which is kept as given,
// an empty one too.
func TestMessageEnvelopeReadsTheSummaryAndPayloadFromTheText(t *testing.T) {
	empty, frame := "", `{"delivery":{"id":"att-1"}}`
	long := strings.Repeat("x", 200)
	for _, test := range []struct {
		name        string
		text        string
		payload     *string
		wantSummary string
		wantPayload string
	}{
		{name: "one short line", text: "one short line", wantSummary: "one short line", wantPayload: ""},
		{name: "many lines", text: "first line\nsecond line", wantSummary: "first line", wantPayload: "first line\nsecond line"},
		{name: "a line past 160 runes", text: long, wantSummary: strings.Repeat("x", 159) + "…", wantPayload: long},
		{name: "a payload given", text: "first line\nsecond line", payload: &frame, wantSummary: "first line", wantPayload: frame},
		{name: "an empty payload given", text: "first line\nsecond line", payload: &empty, wantSummary: "first line", wantPayload: ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			item := MessageEnvelope(Message{Text: test.text, Payload: test.payload}, "notifications.agent.ses_a", "agent.ses_a.k")
			if item.PayloadSummary != test.wantSummary || item.Payload != test.wantPayload {
				t.Errorf("summary %q, payload %q; want summary %q, payload %q", item.PayloadSummary, item.Payload, test.wantSummary, test.wantPayload)
			}
		})
	}
}
