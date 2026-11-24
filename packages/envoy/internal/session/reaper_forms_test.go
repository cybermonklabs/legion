package session

import (
	"context"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/metrics"
)

// A session's agent subject has two forms while every reader takes both (dispatch://INFRA-3862):
// the session alone and the session under a project (`_` for none). The count on a reaped
// session's log line is every message stored for it after its last registration, in either form,
// since its consumer filters on both; a message for another session in either form is not counted.
func TestAReapedSessionsStoredCountTakesEitherFormOfItsSubject(t *testing.T) {
	client := setupNATS(t)
	now := &clock{}
	r := reapingRegistry(t, client, now)
	const sessionID = "ses_counted_forms"
	publish := func(eventID, topic string) {
		t.Helper()
		item := contracts.Envelope{
			EventID:        eventID,
			Source:         "agent",
			SourceSession:  "ses_sender",
			Topic:          topic,
			DedupeKey:      eventID,
			IssuedAt:       time.Now().UnixMilli(),
			PayloadSummary: "stored for a session that stopped answering",
		}
		if err := client.Publish(item); err != nil {
			t.Fatalf("publish %s: %v", eventID, err)
		}
	}
	if err := r.Put(sessionID, SessionEntry{Port: 1, MachineID: "m1", SelfSubscribed: true}); err != nil {
		t.Fatalf("register %s: %v", sessionID, err)
	}
	time.Sleep(10 * time.Millisecond)
	publish("old-form", contracts.LegacyAgentSubject(sessionID))
	publish("no-project", contracts.AgentSubject(contracts.NoProject, sessionID))
	publish("a-project", contracts.AgentSubject("INFRA", sessionID))
	publish("another-session", contracts.AgentSubject("INFRA", "ses_other"))
	now.advance(3 * time.Minute)

	met := metrics.New()
	stored := met.NewCounter("stored", "test")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if reaped := r.ReapSessions(ctx, SessionReaper{Client: client, StoredAfterLastSeen: stored}); reaped != 1 {
		t.Fatalf("the pass reaped %d records, want the one past the horizon", reaped)
	}
	if got := counterValue(t, met, "stored"); got != 3 {
		t.Fatalf("messages stored after the last registration = %d, want the 3 published for it in either form", got)
	}
}
