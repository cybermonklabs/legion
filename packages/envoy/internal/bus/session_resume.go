// The session resume lane: where the listener records how far a session's consumer had read
// before it deleted that consumer, and where a consumer created again for the session reads that
// position back (SessionConsumers.Delete, SessionConsumers.Ensure). One message per session, on
// the session's own subject under SessionResumeSubjectPrefix, and the newest stands.
//
// The lane is the listener's own, which is why it lives beside the session consumers rather than
// among the notification subjects (stream.go): the stream carries the prefix
// (listenerOwnStreamLanes, streamCarriedSubjects) where no consumer filter takes it
// (StreamSubjects), no session may register a topic reaching it (ReachesPrivateStreamSubject,
// privateStreamSubjects) and no replay hands one out, and a record is no envelope at all, so a
// consumer handed one would have nothing to deliver. The listener alone writes it and reads it.

package bus

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

// SessionResumeSubjectPrefix begins the listener's resume lane (SessionResumeSubject).
const SessionResumeSubjectPrefix = NotificationTopicPrefix + "envoy.resume."

// SessionResumeSubject is the lane the listener records session sessionID's consumer resume floor
// on before it deletes that consumer (SessionConsumers.Delete), one message per session, read back
// when a registration that carries no resume point creates the consumer again
// (SessionConsumers.Ensure). Without it a session that comes back with no resume point of its own
// would start at the stream's end and lose what was published while it was away.
//
// The record expires with the stream's own MaxAge (streamDuplicateWindow, 72 hours), which is the
// stream's retention, so a floor older than that named a sequence the stream no longer holds.
//
// The listener alone publishes under the prefix and reads it, no session may register a topic
// reaching it (ReachesPrivateStreamSubject), and no consumer of the notification stream delivers
// one: the stream carries the prefix (streamCarriedSubjects) where no consumer filter does
// (StreamSubjects).
func SessionResumeSubject(sessionID string) string {
	return SessionResumeSubjectPrefix + sessionID
}

// SessionResumeRecord is the body of a session's resume record (SessionResumeSubject): the stream
// sequence up to which the session's consumer had every message acked when that consumer was
// deleted (ackFloor), and the identity of the stream those sequences belong to
// (StreamIDMetadataKey). A consumer created again for the session with no start of its own starts
// at Floor + 1, and a record naming another stream's identity is ignored, since its sequences
// name messages of a stream this one is not.
type SessionResumeRecord struct {
	Floor    uint64 `json:"floor"`
	StreamID string `json:"stream_id"`
}

// recordResumeFloor publishes session sessionID's resume record at the ack floor of its consumer
// as info reports it (recordResumeFloorAt).
func (s *SessionConsumers) recordResumeFloor(ctx context.Context, sessionID string, info *jetstream.ConsumerInfo) error {
	return s.recordResumeFloorAt(ctx, sessionID, ackFloor(info))
}

// recordResumeFloorAt publishes session sessionID's resume record, floor and the stream's own
// identity, on the session's resume lane (SessionResumeSubject). The newest record stands, so the
// write takes no precondition (Client.publishStoredRaw).
func (s *SessionConsumers) recordResumeFloorAt(ctx context.Context, sessionID string, floor uint64) error {
	state, err := s.streamState(ctx)
	if err != nil {
		return err
	}
	record, err := json.Marshal(SessionResumeRecord{Floor: floor, StreamID: state.id})
	if err != nil {
		return err
	}
	if _, err := s.client.publishStoredRaw(ctx, SessionResumeSubject(sessionID), record, nil); err != nil {
		return fmt.Errorf("bus: record the resume floor of session %q: %w", sessionID, err)
	}
	return nil
}

// resumeFloor is the floor session sessionID's resume record holds (SessionResumeSubject), and
// whether the stream holds one that counts: a record naming an identity other than streamID, the
// stream's own, names sequences of a stream this one is not and is ignored, as is a session with
// no record at all, which is the ordinary answer (nats.ErrMsgNotFound) for a session whose
// consumer was never deleted. Any other failure of the read is returned, which leaves the
// registration behind it on core delivery, where the durable path serves it, rather than creating
// a consumer at the stream's end that would miss every message stored since the floor.
func (s *SessionConsumers) resumeFloor(ctx context.Context, sessionID, streamID string) (uint64, bool, error) {
	stored, err := s.client.LastStoredMessage(ctx, SessionResumeSubject(sessionID))
	if errors.Is(err, nats.ErrMsgNotFound) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("bus: read the resume record of session %q: %w", sessionID, err)
	}
	var record SessionResumeRecord
	if err := json.Unmarshal(stored.Data, &record); err != nil {
		return 0, false, fmt.Errorf("bus: read the resume record of session %q: %w", sessionID, err)
	}
	if record.StreamID != streamID {
		return 0, false, nil
	}
	return record.Floor, true, nil
}
