package session

import (
	"errors"

	"github.com/nats-io/nats.go"
	"github.com/sjawhar/envoy/internal/store"
)

// AgentOutcome is what a listener does with a message on a session's agent subject
// (HandleAgentMessage). The session's own consumer delivers every such message, so no outcome
// delivers it, and none tries it again or gives it up. Each outcome says only whether the
// listener acknowledges the message or waits for the session.
type AgentOutcome uint8

const (
	// AgentHasRecord is a session with a record, live or unresponsive, whatever its consumer
	// holds. Its consumer delivers the message, or the consumer its next registration starts
	// does. The listener acknowledges the message and reports no wake.
	AgentHasRecord AgentOutcome = iota
	// AgentOwnedElsewhere is a session whose stored interest row names another listener. That
	// listener reads the same message from its own durable and decides it, so this one
	// acknowledges it and reports nothing.
	AgentOwnedElsewhere
	// AgentNoRecord is a session with no record, or with a record past the gone horizon. The
	// listener waits once, and then it reports one wake, so that a released Legion pod comes
	// back. A row that names this listener does not change that: a released role's plugin
	// deletes its record, and its row stands until the reaper deletes it, 10 to 20 minutes
	// later.
	AgentNoRecord
	// AgentRecordUnread is a record read that did not answer: an error or a timeout. It is not a
	// record, and it is not the lack of one. The listener sends the message back for the wait and
	// decides again at its next delivery.
	AgentRecordUnread
)

// HandleAgentMessage decides a message on the agent subject of a session from the two reads its
// caller made. interest is the session's stored interest row, nil when there is none. readErr is
// the answer of the read of the session's record: nil for a record, live or unresponsive, and
// nats.ErrKeyNotFound for no record or a record past the gone horizon. Any other error is a read
// that did not answer.
//
// A row that names another listener makes the message that listener's, whatever the record says:
// the row names the listener the session registered through most recently (store's
// mergeForUpsert), and that listener reads this message from its own durable too. A row that
// names this listener, or no row, leaves the decision to the record.
func HandleAgentMessage(machineID string, interest *store.Interest, readErr error) AgentOutcome {
	if interest != nil && interest.MachineID != "" && interest.MachineID != machineID {
		return AgentOwnedElsewhere
	}
	switch {
	case readErr == nil:
		return AgentHasRecord
	case errors.Is(readErr, nats.ErrKeyNotFound):
		return AgentNoRecord
	default:
		return AgentRecordUnread
	}
}
