package bus

import (
	"testing"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/testnats"
)

// LEGION-271. A Dispatch publish carries a JetStream MsgId built from the envelope's dedupe key,
// so the stream recognises a repeat of a key it already holds: it stores nothing new, and the
// publish still reaches the agent's subject. Publish throws that verdict away, which is why
// Dispatch cannot tell a retry the stream already held from one it stored.
// PublishReportingDuplicate returns it. Publish itself keeps its signature: it is an interface
// method in cistore, outbox and webhook.
func TestPublishReportingDuplicateReportsTheStreamsVerdict(t *testing.T) {
	client, err := ConnectOwningStream([]string{testnats.URL(t)})
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(client.Close)

	envelope := func(eventID string) contracts.Envelope {
		return contracts.Envelope{
			EventID:        eventID,
			Source:         "dispatch",
			Topic:          "notifications.agent.ses_dup",
			DedupeKey:      "agent.ses_dup.message-1:steer",
			IssuedAt:       contracts.NowMillis(),
			PayloadSummary: "a retried targeted message",
			TraceID:        "trace-" + eventID,
		}
	}

	duplicate, err := client.PublishReportingDuplicate(envelope("first"))
	if err != nil {
		t.Fatalf("first publish: %v", err)
	}
	if duplicate {
		t.Fatalf("first publish reported a duplicate; the stream held nothing")
	}

	duplicate, err = client.PublishReportingDuplicate(envelope("second"))
	if err != nil {
		t.Fatalf("second publish: %v", err)
	}
	if !duplicate {
		t.Fatalf("a repeat of a dedupe key the stream already holds must report a duplicate")
	}

	// The control that keeps the first assertion honest: a different key is not a duplicate,
	// so "duplicate" is not simply what the second publish always answers.
	other := envelope("third")
	other.DedupeKey = "agent.ses_dup.message-1:btw"
	duplicate, err = client.PublishReportingDuplicate(other)
	if err != nil {
		t.Fatalf("third publish: %v", err)
	}
	if duplicate {
		t.Fatalf("a different dedupe key must not report a duplicate")
	}

	// An agent-sourced envelope under a key its caller chose is stored under its own event id
	// (envelopeMsgID), because that key names no event (contracts.DedupeKeyNamesTheUpstreamEvent)
	// and two distinct envelopes can share it: a second envelope under the same key is no
	// duplicate, and a repeat of the same envelope, the same event id, is.
	agent := envelope("fourth")
	agent.Source = "agent"
	agent.Topic = "notifications.agent.ses_dup"
	another := agent
	another.EventID = "fifth"
	for _, publish := range []struct {
		what      string
		item      contracts.Envelope
		duplicate bool
	}{
		{"an agent-sourced publish", agent, false},
		{"another envelope under the same key its caller chose", another, false},
		{"a repeat of the first envelope", agent, true},
	} {
		duplicate, err = client.PublishReportingDuplicate(publish.item)
		if err != nil {
			t.Fatalf("%s: %v", publish.what, err)
		}
		if duplicate != publish.duplicate {
			t.Fatalf("%s reported duplicate %t, want %t", publish.what, duplicate, publish.duplicate)
		}
	}
}
