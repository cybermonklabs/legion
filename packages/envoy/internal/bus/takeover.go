// The takeover lane: where the listener records that a session took a role from a different
// session, so that the role messages the previous holder did not read are copied to the role's
// kept lane for whoever holds the role next (cmd/listener/role_takeover.go).
//
// The lane is the listener's own, as the session resume lane is (session_resume.go): the stream
// carries its prefix (listenerOwnStreamLanes, streamCarriedSubjects) where no consumer filter takes
// it (StreamSubjects), no session may register a topic reaching it (ReachesPrivateStreamSubject,
// privateStreamSubjects) and the publish route refuses a topic under it, and a record is no
// envelope at all. The listener alone writes it and reads it.

package bus

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/nats-io/nats.go"
)

// TakeoverSubjectPrefix begins the listener's takeover lane (TakeoverSubject).
const TakeoverSubjectPrefix = NotificationTopicPrefix + "envoy.takeover."

// TakeoverSubject is the subject of one takeover record: role was taken from previous, by the try
// of a claim write named attempt. Every try of a claim write gets a subject of its own, and every
// later write of a record goes to that record's own subject, so a record's newest message is the
// one that stands, and a pass of an older takeover never writes over a newer one. role is a role
// token (one subject token, roles.PatternString) and previous a session id a session consumer can
// be named for (SessionConsumerName), so each is one token.
func TakeoverSubject(role, previous, attempt string) string {
	return TakeoverSubjectPrefix + role + "." + previous + "." + attempt
}

// TakeoverRecord is the body of one takeover record (TakeoverSubject): what a pass of the
// listener's copy needs to go on from where the last one stopped. A writer writes every field at
// once, since a reader takes the whole body as one answer.
type TakeoverRecord struct {
	// Role is the role that was taken, and Previous the session it was taken from.
	Role     string `json:"role"`
	Previous string `json:"previous"`
	// ClaimedAt is when the claim route wrote the record, before its claim write, in milliseconds
	// since the epoch on the listener's clock: the time the copy's hour and the record's end are
	// counted from.
	ClaimedAt int64 `json:"claimed_at"`
	// Next is the stream sequence on the previous holder's agent subject the next pass reads from,
	// and 0 while no pass has found where to start.
	Next uint64 `json:"next,omitempty"`
	// FloorMissed is set by a pass that found neither the previous holder's session consumer nor
	// a resume record of its floor, so the pass after it starts from the hour before the claim
	// when it finds neither again.
	FloorMissed bool `json:"floor_missed,omitempty"`
	// Finished is set by the pass that ends the record: one that started more than the record's
	// life after ClaimedAt and read to the end of the previous holder's subject. No pass runs for
	// a finished record, which ages out with the stream.
	Finished bool `json:"finished,omitempty"`
	// HolderSeenAt is when a pass last found the role's claim naming Previous again, in
	// milliseconds since the epoch on the listener's clock, and 0 while none has: the claim did not
	// land, or the role went back to Previous. When it is later than ClaimedAt, the copy's hour
	// counts from it instead, since Previous held the role and read its messages until then.
	HolderSeenAt int64 `json:"holder_seen_at,omitempty"`
}

// StoredTakeoverRecord is a takeover record as the stream holds it: its subject, its newest body,
// when the stream stored that body, and the stream sequence it stored it at, which a write of the
// record names as the one it replaces (WriteTakeoverRecord).
type StoredTakeoverRecord struct {
	Subject  string
	Record   TakeoverRecord
	Written  time.Time
	Sequence uint64
}

// ErrTakeoverRecordMoved is wrapped by the error of a WriteTakeoverRecord the stream refused
// because the record's subject no longer held the message the writer read: another pass wrote
// the record since, or the stream removed it (its age limit, a purge). The write stored nothing,
// and the writer reads the record again before it decides anything more of it.
var ErrTakeoverRecordMoved = errors.New("the takeover record changed under this write")

// WriteTakeoverRecord publishes record on subject, a takeover record's own subject
// (TakeoverSubject), only while the stream's last sequence on subject is expected, and returns the
// sequence the stream stored it at. expected is the sequence of the record the writer read
// (StoredTakeoverRecord.Sequence), and 0 for a new record, which stores only while the subject
// holds nothing (nats.ExpectLastSequencePerSubject). So a write lands only over the body its
// writer read: a pass that read an older body, or one whose write comes late, after another pass
// wrote the record or the stream removed it, is refused with ErrTakeoverRecordMoved rather than
// bringing back what was written or removed since. Every other failure is the publish path's
// (publishStoredRaw).
func (c *Client) WriteTakeoverRecord(ctx context.Context, subject string, record TakeoverRecord, expected uint64) (uint64, error) {
	data, err := json.Marshal(record)
	if err != nil {
		return 0, err
	}
	ack, err := c.publishStoredRaw(ctx, subject, data, nats.ExpectLastSequencePerSubject(expected))
	if err != nil {
		var apiErr *nats.APIError
		if errors.As(err, &apiErr) && apiErr.ErrorCode == nats.JSErrCodeStreamWrongLastSequence {
			err = fmt.Errorf("%w: %w", ErrTakeoverRecordMoved, err)
		}
		return 0, fmt.Errorf("bus: write the takeover record %s: %w", subject, err)
	}
	return ack.Sequence, nil
}

// TakeoverRecords reads every takeover record the stream holds for role, or for every role when
// role is empty, sorted by subject: one STREAM.INFO with the lane as its filter, as KeptRoleLanes
// reads the kept lanes, and one read of the newest message on each subject it names. A record no
// read can decode is left out, since no pass can act on it. Its errors are the stream's own, named
// with the lane. The STREAM.INFO waits under a watch of the connection it went out on
// (RequestOnConnection), as each stored-message read waits under its own (requestConfirmed):
// nats.go fails no request whose connection drops, so a read whose answer was lost with its
// connection fails at the reconnect with ErrRequestLostWithConnection rather than holding its
// caller to ctx's end, which for the claim route's handover (cmd/listener's runTakenRoleDrain) is a
// window of 10 minutes under the role's guard.
func (c *Client) TakeoverRecords(ctx context.Context, role string) ([]StoredTakeoverRecord, error) {
	if err := c.ensureConnWithContext(ctx); err != nil {
		return nil, err
	}
	filter := TakeoverSubjectPrefix + ">"
	if role != "" {
		filter = TakeoverSubjectPrefix + role + ".>"
	}
	info, err := RequestOnConnection(ctx, c, func(ctx context.Context) (*nats.StreamInfo, error) {
		return c.JS().StreamInfo(Stream, &nats.StreamInfoRequest{SubjectsFilter: filter}, nats.Context(ctx))
	})
	if err != nil {
		return nil, fmt.Errorf("bus: read %s's takeover records: %w", Stream, err)
	}
	subjects := make([]string, 0, len(info.State.Subjects))
	for subject, held := range info.State.Subjects {
		if held > 0 && strings.HasPrefix(subject, TakeoverSubjectPrefix) {
			subjects = append(subjects, subject)
		}
	}
	slices.Sort(subjects)
	records := make([]StoredTakeoverRecord, 0, len(subjects))
	for _, subject := range subjects {
		stored, err := c.LastStoredMessage(ctx, subject)
		if errors.Is(err, nats.ErrMsgNotFound) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("bus: read the takeover record %s: %w", subject, err)
		}
		var record TakeoverRecord
		if err := json.Unmarshal(stored.Data, &record); err != nil || record.Role == "" || record.Previous == "" {
			continue
		}
		records = append(records, StoredTakeoverRecord{Subject: subject, Record: record, Written: stored.Time, Sequence: stored.Sequence})
	}
	return records, nil
}

// SessionReadFloor is the stream sequence up to which session sessionID had every message of its
// session consumer acknowledged, and whether one is known: the consumer's own floor
// (CONSUMER.INFO, read by ackFloor's rules, which every restart of a session consumer reads its
// floor by) while the consumer exists, else the floor the listener recorded when it deleted the
// consumer (the session resume lane), read with the stream identity check the resume lane's own
// readers make. A consumer that does not exist and no record that counts is no floor known, which
// is no error. Every other failure of a read is its error, ErrConsumerNotGranted for a consumer
// read the connection's user may not make among them.
func (c *Client) SessionReadFloor(ctx context.Context, sessionID string) (uint64, bool, error) {
	name, err := sessionConsumerNameOf(sessionID)
	if err != nil {
		return 0, false, err
	}
	consumers := sessionConsumersOn(c, 0, nil)
	info, err := consumers.info(ctx, name)
	if err == nil {
		return ackFloor(info), true, nil
	}
	if !errors.Is(err, ErrSessionConsumerNotFound) {
		return 0, false, err
	}
	state, err := consumers.streamState(ctx)
	if err != nil {
		return 0, false, err
	}
	return consumers.resumeFloor(ctx, sessionID, state.id)
}

// FirstStoredMessageSince reads the first message the notification stream holds on subject that
// it stored at or after since: a stored-message read by start time (start_time with next_by_subj,
// which nats-server answers from 2.11 on), over the stored-message API subject every other read
// here uses. Its errors are NextStoredMessage's, nats.ErrMsgNotFound for a subject holding no
// such message.
func (c *Client) FirstStoredMessageSince(ctx context.Context, subject string, since time.Time) (StoredMessage, error) {
	return c.storedMessage(ctx, struct {
		StartTime time.Time `json:"start_time"`
		NextFor   string    `json:"next_by_subj"`
	}{StartTime: since.UTC(), NextFor: subject})
}
