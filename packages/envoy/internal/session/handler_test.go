package session_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	session "github.com/sjawhar/envoy/internal/session"
	"github.com/sjawhar/envoy/internal/store"
)

// resolvedEntry is the session record a caller of session.Resolve reads first: the registry's
// entry for sessionID, or nil when it holds none. Resolve reads no registry itself, so every test
// of it resolves the entry the way its caller does.
func resolvedEntry(t *testing.T, sessions *session.SessionRegistry, sessionID string) *session.SessionEntry {
	t.Helper()
	entry, err := sessions.Get(sessionID)
	if err != nil {
		return nil
	}
	return &entry
}

// readRecord is the answer the listener hands HandleAgentMessage: its read of sessionID's record
// from the session bucket itself (SessionRegistry.Refresh), nil for a record and the bucket's own
// error otherwise.
func readRecord(t *testing.T, sessions *session.SessionRegistry, sessionID string) error {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := sessions.Refresh(ctx, sessionID)
	return err
}

// A message on the agent subject of a session is decided by the session's interest row and its
// record, and never by what the session's consumer holds. A row that names another listener makes
// the message that listener's, whatever the record says. Otherwise a record is acknowledged, no
// record waits for the wake, and a read that did not answer is neither: the listener sends the
// message back and decides again.
func TestHandleAgentMessageDecidesOnTheRowAndTheRecord(t *testing.T) {
	sessions := newSessionRegistry(t)
	// A record with no consumer of its own: the consumer the session's next registration starts
	// delivers the message, so the record alone decides.
	if err := sessions.Put("ses_recorded", session.SessionEntry{MachineID: "test-machine", Dir: "/test"}); err != nil {
		t.Fatalf("register ses_recorded: %v", err)
	}
	here := &store.Interest{SessionID: "ses_target", MachineID: "test-machine"}
	elsewhere := &store.Interest{SessionID: "ses_target", MachineID: "other-machine"}
	unanswered := fmt.Errorf("read session ses_unread from the bucket: %w", context.DeadlineExceeded)

	for _, tc := range []struct {
		name     string
		interest *store.Interest
		readErr  error
		want     session.AgentOutcome
	}{
		{"a record and no row", nil, readRecord(t, sessions, "ses_recorded"), session.AgentHasRecord},
		{"a record and a row here", here, readRecord(t, sessions, "ses_recorded"), session.AgentHasRecord},
		{"a record and a row elsewhere", elsewhere, readRecord(t, sessions, "ses_recorded"), session.AgentOwnedElsewhere},
		{"no record and no row", nil, readRecord(t, sessions, "ses_unknown"), session.AgentNoRecord},
		{"no record and a row here", here, readRecord(t, sessions, "ses_unknown"), session.AgentNoRecord},
		{"no record and a row elsewhere", elsewhere, readRecord(t, sessions, "ses_unknown"), session.AgentOwnedElsewhere},
		{"no record and a row naming no listener", &store.Interest{SessionID: "ses_target"}, readRecord(t, sessions, "ses_unknown"), session.AgentNoRecord},
		{"an unanswered read and no row", nil, unanswered, session.AgentRecordUnread},
		{"an unanswered read and a row here", here, unanswered, session.AgentRecordUnread},
		{"an unanswered read and a row elsewhere", elsewhere, unanswered, session.AgentOwnedElsewhere},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := session.HandleAgentMessage("test-machine", tc.interest, tc.readErr); got != tc.want {
				t.Fatalf("HandleAgentMessage(row %+v, read %v) = %d, want %d", tc.interest, tc.readErr, got, tc.want)
			}
		})
	}
	// The no-record rows above stand on the bucket's own word for a key it does not hold.
	if err := readRecord(t, sessions, "ses_unknown"); !errors.Is(err, nats.ErrKeyNotFound) {
		t.Fatalf("the read of a session the bucket holds no record of answered %v, want %v", err, nats.ErrKeyNotFound)
	}
}

// TestHandleAgentMessage_StoredInterestHereWithAnUnansweredRead tests that a session whose stored
// interest row names this listener, and whose record read did not answer, is an unread record:
// the caller sends the message back for its wait and decides again. That read leaves the listener
// with no record, as a record that is gone does, but it says nothing of the session. So the
// message is neither acknowledged as one for a session with a record, nor left to another
// listener, nor taken as one for a session with no record, which reports a wake.
func TestHandleAgentMessage_StoredInterestHereWithAnUnansweredRead(t *testing.T) {
	interest := &store.Interest{
		SessionID: "ses_target",
		Dir:       "/test",
		MachineID: "test-machine",
	}
	unanswered := fmt.Errorf("read session ses_target from the bucket: %w", context.DeadlineExceeded)

	if got := session.HandleAgentMessage("test-machine", interest, unanswered); got != session.AgentRecordUnread {
		t.Fatalf("HandleAgentMessage(row here, unanswered read) = %d, want AgentRecordUnread (%d)", got, session.AgentRecordUnread)
	}
}
