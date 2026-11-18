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
	"log/slog"

	"github.com/nats-io/nats.go"
	"github.com/sjawhar/envoy/internal/id"
)

// SessionResumeSubjectPrefix begins the listener's resume lane (SessionResumeSubject).
const SessionResumeSubjectPrefix = NotificationTopicPrefix + "envoy.resume."

// SessionResumeSubject is the lane the listener records session sessionID's consumer resume floor
// on before it deletes that consumer (SessionConsumers.Delete), one message per session, read back
// when the consumer is created again, both for a create with no start and for one from a caller's
// start (SessionConsumers.Ensure, SessionConsumers.create): a create with no start of its own
// starts at the record's floor plus one, and a caller's start above that is lowered to it. A
// create at a pending start a restart left reads none. Without it a session that comes back with
// no resume point of its own would start at the stream's end and lose what was published while
// it was away; and one that comes back with a resume point past the stream's end, which a
// consumer created at a clamped start leaves its client holding, would be clamped again, to the
// stream's first sequence, and handed again every message the stream holds on its filters, the
// ones it acked included.
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
// deleted (ackFloor), or the sequence before the stream's first where that floor was past the
// stream's end (recordResumeFloorAt), and the identity of the stream those sequences belong to
// (StreamIDMetadataKey). A consumer created again for the session with no start of its own starts
// at Floor + 1, one created from a caller's start above Floor + 1 starts at Floor + 1 instead
// (SessionConsumers.create), and a record naming another stream's identity is ignored by both,
// since its sequences name messages of a stream this one is not.
type SessionResumeRecord struct {
	Floor    uint64 `json:"floor"`
	StreamID string `json:"stream_id"`
}

// recordResumeFloorAt publishes session sessionID's resume record, a floor and the stream's own
// identity, on the session's resume lane (SessionResumeSubject), and returns the floor it
// recorded. The newest record stands, so the write takes no precondition
// (Client.publishStoredRaw).
//
// The write carries a Nats-Msg-Id of its own, drawn here once (id.New, 128 random bits) and
// shared by the write's sends, so a send of it the bus makes again stores no second copy of one
// that landed: the bus sends it again when the connection closed under it, on the JetStream context
// of a connection the client had just replaced, and that replacement is up (sendStored,
// connForRequest). The id names this write and nothing else, so a later write of the session,
// however equal its record, is a message of its own and stands as the newest. A lost connection,
// and a replacement nats.go is still reconnecting, are answered at once rather than waited for
// with the caller's lock held (withLossAtOnce): the delete then deletes nothing (Delete), and its
// caller's retry once the connection is back, or the sweep's next pass, is such a later write. The
// id carries no subject, as ForwardMsgID's do, because the subject is there to keep one key's copies
// on several subjects apart, and this id is one write's alone. A reader of the record reads its
// body and no header (resumeFloor).
//
// The floor recorded is floor itself unless floor is past the stream's end: above the stream's last
// sequence, as the stream read this call makes reads it (streamState, whose read begins after this
// call, and so after the read of the consumer the caller took floor from). A floor past the end is
// recorded as the sequence before the stream's first (firstStart minus one, the stream's first
// sequence minus one: 0 for a stream that has stored nothing or whose first sequence is still 1,
// and the stream's last sequence for a stream that age or a purge has emptied, whose first sequence
// is then its last plus one). Such a floor is the ack floor of a consumer the
// stream's sequences went back under (ackFloor reads no lower than the floor the server reports,
// and at least the sequence before the start of a consumer that has acked nothing): one an earlier
// build created past the end, before create reset every start past the end, or one a one-copy
// stream's loss of every message left unreconciled (RestartAtFloor); or it is the sequence before
// the pending start a restart of such a consumer left when its create failed (recordDroppedStart).
// Every message the stream holds is then at a sequence below that consumer's next one, so none of
// them was ever handed to it, the ones stored after its start was fixed among them. From the
// sequence before the stream's first, the next create of the session starts at the stream's first
// sequence, whether its start is 0, which starts at the record's floor plus one, or a caller's
// start, which the record rule lowers to that floor plus one (SessionConsumers.create), as create's
// reset of a start past the end and the relay's restart of a consumer past the end do
// (RestartAtFloor), and it skips none of those messages. Where the stream holds messages, a record
// of its last sequence would start that create after them and skip every one. The cost is the
// reset's own: the session is handed again what it was handed before at the sequences the stream
// holds, which its plugin's ledger drops within 72 hours (DELIVERY_DUPLICATE_WINDOW_MS) and 20,000
// frames (DELIVERY_LEDGER_LIMIT, packages/envoy-client/src/delivery-ledger.ts).
//
// So a record this call writes never holds a floor past the stream's end, and a loss that later
// cuts the stream below a floor it recorded removes the record too, since the stream stored the
// record after the floor. Once the record's publish succeeded, and only then, a floor past the
// end is logged as a WARN line naming the session (session_id), the floor asked (floor), the floor
// recorded (recorded_floor), the stream's last sequence (stream_last_seq) and the stream's
// identity (stream_id). A call that fails, its stream read or its publish, records nothing, logs
// nothing and returns 0 with its error.
func (s *SessionConsumers) recordResumeFloorAt(ctx context.Context, sessionID string, floor uint64) (uint64, error) {
	state, err := s.streamState(ctx)
	if err != nil {
		return 0, err
	}
	recorded, pastTheEnd := floor, floor > state.lastSeq
	if pastTheEnd {
		recorded = state.firstStart() - 1
	}
	record, err := json.Marshal(SessionResumeRecord{Floor: recorded, StreamID: state.id})
	if err != nil {
		return 0, err
	}
	if _, err := s.client.publishStoredRaw(ctx, SessionResumeSubject(sessionID), record, id.New(), nil); err != nil {
		return 0, fmt.Errorf("bus: record the resume floor of session %q: %w", sessionID, err)
	}
	if pastTheEnd {
		s.logger.Warn("session resume floor past the stream's end, recorded at the sequence before the stream's first",
			slog.String("session_id", sessionID),
			slog.Uint64("floor", floor),
			slog.Uint64("recorded_floor", recorded),
			slog.Uint64("stream_last_seq", state.lastSeq),
			slog.String("stream_id", state.id))
	}
	return recorded, nil
}

// resumeFloor is the floor session sessionID's resume record holds (SessionResumeSubject), and
// whether the stream holds one that counts: a record naming an identity other than streamID, the
// stream's own, names sequences of a stream this one is not and is ignored, as is a session with
// no record at all, which is the ordinary answer (nats.ErrMsgNotFound) for a session whose
// consumer was never deleted. Any other failure of the read is returned, and the create that read
// it fails rather than create a consumer that could skip every message stored since the floor:
// both creates that read the record, one at start 0 and one at a caller's start (a pending start
// reads none), fail when the read fails (SessionConsumers.create). A registration is then refused
// 503 (cmd/listener sessionDelivery.refused), consumer_error unless the server refused the read
// for want of a grant (no_grant), and its next heartbeat tries again; a repair logs the failure
// (cmd/listener repairRegistered).
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
