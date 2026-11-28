// The takeover lane: where the listener records that a session took a role from a different
// session, so that the role messages the previous holder did not read are copied to the role's
// kept lane for whoever holds the role next, and that a claim named the session it replaces, so
// that session's unread messages go to the claimant as well (cmd/listener/role_takeover.go).
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
	"sync"
	"time"

	"github.com/nats-io/nats.go"
)

// TakeoverSubjectPrefix begins the listener's takeover lane (TakeoverSubject).
const TakeoverSubjectPrefix = NotificationTopicPrefix + "envoy.takeover."

// TakeoverSubject is the subject of one takeover record: role was taken from previous, by the try
// of a claim write named attempt. Every try of a claim write gets a subject of its own, and every
// later write of a record goes to that record's own subject, so a record's newest message is the
// one that stands, and a pass of an older takeover never writes over a newer one. role is the
// role's claim key (contracts.RoleClaimKey): its bare name, one subject token
// (roles.PatternString), or <project>.<role> for a free-form role claimed under a project, two
// tokens; previous is a session id a session consumer can be named for (SessionConsumerName) and
// attempt an id, one token each. So a subject's role is every token before its last two, and
// contracts.SubjectProject reads its project from that key.
func TakeoverSubject(role, previous, attempt string) string {
	return TakeoverSubjectPrefix + role + "." + previous + "." + attempt
}

// handoverAttemptPrefix begins the attempt token of a handover record's subject
// (HandoverSubject), which names the successor after it. No attempt id the claim route draws for
// a try (id.New, hex digits) begins with it.
const handoverAttemptPrefix = "handover-"

// HandoverSubject is the subject of the handover record of a claim of role that landed for
// successor and named previous as the session it replaces (the claim route's
// previous_session_id): TakeoverSubject with an attempt that names successor. Every claim of role
// by successor naming previous has this one subject, so the claim route writes one such record
// however often successor claims the role again, and its passes go on from where the last one
// stopped. previous and successor are session ids a session consumer can be named for
// (SessionConsumerName), so neither holds a dot, and the subject names all three of role,
// previous and successor (handoverOf).
//
// The body is a TakeoverRecord whose role and previous session are written empty (takeoverBody)
// and read back from the subject (TakeoverRecords). A listener from before handover records
// leaves out a record that names no role or no previous session, as one no pass can act on, so it
// never runs a handover record's passes as a takeover record's: such a pass would copy the role's
// own messages alone and move the record past every direct message it did not copy.
func HandoverSubject(role, previous, successor string) string {
	return TakeoverSubject(role, previous, handoverAttemptPrefix+successor)
}

// TakeoverSuccessor is the session the record on subject hands its previous session's messages
// to, and whether subject is a handover record's (HandoverSubject); a takeover record of a claim
// that named no session it replaces names none.
func TakeoverSuccessor(subject string) (string, bool) {
	_, _, successor, ok := handoverOf(subject)
	return successor, ok
}

// handoverOf is the role, the previous session and the successor a handover record's subject names
// (HandoverSubject), and whether subject is one: the successor after handoverAttemptPrefix in its
// last token, the previous session the token before it, and the role every token between the
// lane's prefix and those two, one token or two (<project>.<role>).
func handoverOf(subject string) (role, previous, successor string, ok bool) {
	rest, found := strings.CutPrefix(subject, TakeoverSubjectPrefix)
	last := strings.LastIndexByte(rest, '.')
	if !found || last < 0 {
		return "", "", "", false
	}
	successor, named := strings.CutPrefix(rest[last+1:], handoverAttemptPrefix)
	rest = rest[:last]
	before := strings.LastIndexByte(rest, '.')
	if !named || successor == "" || before <= 0 || before == len(rest)-1 {
		return "", "", "", false
	}
	return rest[:before], rest[before+1:], successor, true
}

// HandoverSuccessors is every session a handover record the stream holds names as the successor of
// previous (HandoverSubject), sorted and each once: the sessions that replaced previous, which its
// unread messages are copied to. It reads subjects alone and no record's body, in one STREAM.INFO
// for each form a role takes in a record's subject - one token, and two (<project>.<role>) - with
// previous in the place before the record's last token as its filter, and keeps each subject
// handoverOf reads as a handover record whose previous session is previous: a takeover record of
// a claim that named no session it replaces names no successor. A finished record still names its
// successor for as long as the stream keeps it, since the copies it made stay on the successor's
// subject. A previous no session consumer can be named for (SessionConsumerName) has no record,
// and answers none, as does a previous that no record names. Its errors are the stream's own,
// named with the lane and previous. Each STREAM.INFO waits under a watch of the connection it went
// out on (RequestOnConnection), as TakeoverRecords' does, so a read whose answer was lost with its
// connection fails at the reconnect with ErrRequestLostWithConnection rather than at ctx's end.
func (c *Client) HandoverSuccessors(ctx context.Context, previous string) ([]string, error) {
	if _, ok := SessionConsumerName(previous); !ok {
		return nil, nil
	}
	if err := c.ensureConnWithContext(ctx); err != nil {
		return nil, err
	}
	var successors []string
	for _, filter := range []string{TakeoverSubjectPrefix + "*." + previous + ".*", TakeoverSubjectPrefix + "*.*." + previous + ".*"} {
		info, err := RequestOnConnection(ctx, c, func(ctx context.Context) (*nats.StreamInfo, error) {
			return c.JS().StreamInfo(Stream, &nats.StreamInfoRequest{SubjectsFilter: filter}, nats.Context(ctx))
		})
		if err != nil {
			return nil, fmt.Errorf("bus: read %s's handover records of %s: %w", Stream, previous, err)
		}
		for subject, held := range info.State.Subjects {
			if _, named, successor, ok := handoverOf(subject); ok && held > 0 && named == previous {
				successors = append(successors, successor)
			}
		}
	}
	slices.Sort(successors)
	return slices.Compact(successors), nil
}

// TakeoverRecord is the body of one takeover record (TakeoverSubject): what a pass of the
// listener's copy needs to go on from where the last one stopped. A writer writes every field at
// once, since a reader takes the whole body as one answer.
type TakeoverRecord struct {
	// Role is the role that was taken, and Previous the session it was taken from. A handover
	// record's stored body holds both empty, its subject naming them (HandoverSubject).
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

// StoredTakeoverRecord is a takeover record as the stream holds it: its subject, the stream
// sequence of the message that was read, which a write of the record names as the one it replaces
// (WriteTakeoverRecordAt), its newest body, and when the stream stored that body.
type StoredTakeoverRecord struct {
	Subject  string
	Sequence uint64
	Record   TakeoverRecord
	Written  time.Time
}

// ErrTakeoverRecordMoved is the refusal of a write of a takeover record whose subject no longer
// holds the message its writer read (Client.WriteTakeoverRecordAt): another listener's pass wrote
// the record in between, or the stream removed it (its age limit, a purge), and this write would
// put an older state back, or bring back a record that was removed. The pass that met it writes
// nothing and reads the record again next time.
var ErrTakeoverRecordMoved = errors.New("bus: the takeover record changed under this write")

// ErrTakeoverRecordFinished is the refusal of a write over a takeover record its writer read
// finished (Client.WriteTakeoverRecordAt): no pass runs a finished record, and the read-once cache
// of finished records rests on no write landing over one (TakeoverRecords, its rule of
// finishedTakeovers).
var ErrTakeoverRecordFinished = errors.New("bus: the takeover record is finished")

// takeoverBody is record as the stream stores it on subject. A handover record's body names no
// role and no previous session: its subject names both (HandoverSubject), and a listener from
// before handover records leaves such a body out.
func takeoverBody(subject string, record TakeoverRecord) ([]byte, error) {
	if _, _, _, handover := handoverOf(subject); handover {
		record.Role, record.Previous = "", ""
	}
	return json.Marshal(record)
}

// WriteTakeoverRecord publishes record on subject, a takeover record's own subject
// (TakeoverSubject), only while that subject holds nothing (nats.ExpectLastSequencePerSubject(0)):
// the claim route's first write of the record, under a fresh attempt id. So no send of it lands
// over a record the subject holds, a finish a pass wrote there (WriteTakeoverRecordAt) among them,
// and the finish stays terminal, as TakeoverRecords' rule of finishedTakeovers needs.
//
// Every failure is the write's error, wrapping the publish's own, with one exception: a refusal of
// that precondition that wraps ErrUnacknowledged. Such a refusal answered a resend after a send
// nothing answered (sendStored), and the copy the resend met may be that first send's own, stored
// with its answer lost and perhaps since moved on by a pass. For it alone the subject's newest
// message is read once (heldTakeoverClaim), and the write returns nil when that message is a
// record of this same claim: the Role, Previous and ClaimedAt of record, which a pass writes back
// as it read them while it changes Next, FloorMissed, Finished and HolderSeenAt. A read that
// fails, a body that does not decode, or a record of another claim leaves the refusal as the
// error. A refusal that wraps no ErrUnacknowledged (preconditionLost) is another writer's record on
// the subject, and is the error too.
func (c *Client) WriteTakeoverRecord(ctx context.Context, subject string, record TakeoverRecord) error {
	data, err := takeoverBody(subject, record)
	if err != nil {
		return err
	}
	_, err = c.publishStoredRaw(ctx, subject, data, nats.ExpectLastSequencePerSubject(0))
	if err == nil {
		return nil
	}
	if IsCASConflict(err) && errors.Is(err, ErrUnacknowledged) && c.heldTakeoverClaim(ctx, subject, record) {
		return nil
	}
	return fmt.Errorf("bus: write the takeover record %s: %w", subject, err)
}

// heldTakeoverClaim reports whether subject's newest message is a takeover record of record's
// claim, one with its Role, Previous and ClaimedAt, read with one LastStoredMessage: the read
// TakeoverRecords makes of each subject, over the stored-message API subject the listener's grant
// allows (storedMessageAPI). A read that fails and a body that does not decode hold no claim.
func (c *Client) heldTakeoverClaim(ctx context.Context, subject string, record TakeoverRecord) bool {
	stored, err := c.LastStoredMessage(ctx, subject)
	if err != nil {
		return false
	}
	var held TakeoverRecord
	if err := json.Unmarshal(stored.Data, &held); err != nil {
		return false
	}
	return held.Role == record.Role && held.Previous == record.Previous && held.ClaimedAt == record.ClaimedAt
}

// WriteHandoverRecord publishes record on subject, a handover record's subject (HandoverSubject),
// while the subject holds nothing, and answers nil when it holds a record already: an earlier
// claim's, whose passes go on from where they stopped, or this write's own first send, which a
// resend after a send nothing answered (ErrUnacknowledged) meets. Either way the subject holds the
// handover record, which is all its writer asks, and no write lands over a record, a finished one
// included (TakeoverRecords' rule of finishedTakeovers). Its stored body names no role and no
// previous session (takeoverBody).
func (c *Client) WriteHandoverRecord(ctx context.Context, subject string, record TakeoverRecord) error {
	data, err := takeoverBody(subject, record)
	if err != nil {
		return err
	}
	if _, err := c.publishStoredRaw(ctx, subject, data, nats.ExpectLastSequencePerSubject(0)); err != nil {
		if IsCASConflict(err) {
			return nil
		}
		return fmt.Errorf("bus: write the handover record %s: %w", subject, err)
	}
	return nil
}

// WriteTakeoverRecordAt publishes record on stored's subject while that subject's newest message
// is the one stored was read at (StoredTakeoverRecord.Sequence). It returns the stream sequence of
// the message it stored, which the writer's next write names: the stream's sequences are its own
// and not a subject's, so a message stored on another subject in between leaves the next one of
// this subject further on.
//
// A stored record that is finished is ErrTakeoverRecordFinished, with nothing sent. A subject
// whose newest message is another, or that holds none since the stream removed the record, is
// ErrTakeoverRecordMoved, and nothing is written: either code JetStream answers a lost conditional
// write with (preconditionLost), so a write that meets another write of the record still being
// replicated, which a stream of more than one copy answers 10164 and not 10071, has lost all the
// same. A refusal that is no proof of that, a resend's after a send nothing answered, is returned
// as it is, not as moved: that first send may have stored this write.
//
// The two make a record's finish terminal: a pass that read the record unfinished and was then
// held - a stall, a slow read - cannot write that older state over the finish a later pass wrote,
// and no pass writes over a finish it read, so no reader after the finish sees the record
// unfinished again. TakeoverRecords' rule of finishedTakeovers rests on that.
func (c *Client) WriteTakeoverRecordAt(ctx context.Context, stored StoredTakeoverRecord, record TakeoverRecord) (uint64, error) {
	if stored.Record.Finished {
		return 0, fmt.Errorf("%w: %s", ErrTakeoverRecordFinished, stored.Subject)
	}
	data, err := takeoverBody(stored.Subject, record)
	if err != nil {
		return 0, err
	}
	ack, err := c.publishStoredRaw(ctx, stored.Subject, data, nats.ExpectLastSequencePerSubject(stored.Sequence))
	if err != nil {
		if preconditionLost(err) {
			return 0, fmt.Errorf("%w: %s after %d: %w", ErrTakeoverRecordMoved, stored.Subject, stored.Sequence, err)
		}
		return 0, fmt.Errorf("bus: write the takeover record %s: %w", stored.Subject, err)
	}
	return ack.Sequence, nil
}

// TakeoverRecords reads every takeover record the stream holds for role, or for every role when
// role is empty, sorted by subject: one STREAM.INFO with the lane as its filter, as KeptRoleLanes
// reads the kept lanes, and one read of the newest message on each subject it names, except a
// subject whose finished record this client holds (finishedTakeovers). A record no read can decode
// is left out, since no pass can act on it. Its errors are the stream's own, named with the lane.
// The STREAM.INFO waits under a watch of the connection it went out on (RequestOnConnection), as
// each stored-message read waits under its own (requestConfirmed): nats.go fails no request whose
// connection drops, so a read whose answer was lost with its connection fails at the reconnect
// with ErrRequestLostWithConnection rather than holding its caller to ctx's end, which for the
// claim route's handover (cmd/listener's runTakenRoleDrain) is a window of 10 minutes under the
// role's guard.
//
// The rule of finishedTakeovers, which ErrTakeoverRecordFinished and WriteTakeoverRecordAt point
// to: this client reads a finished record's subject again only when the STREAM.INFO count of its
// messages differs from the count at its last read. While that count stands, the subject's newest
// message is the one this client read: no write lands over a finished record
// (WriteTakeoverRecordAt), so all that changes the subject afterwards is the stream's age limit
// removing its older messages, and each removal moves the count and costs one read. A stream with
// a per-subject limit (MaxMsgsPerSubject) drops a message at each write past it and leaves the
// count where it stood, so on such a stream every subject is read each time.
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
	caches := info.Config.MaxMsgsPerSubject <= 0
	if role == "" {
		c.finishedTakeovers.keepListed(info.State.Subjects)
	}
	records := make([]StoredTakeoverRecord, 0, len(subjects))
	for _, subject := range subjects {
		held := info.State.Subjects[subject]
		if stored, ok := c.finishedTakeovers.read(subject, held); ok && caches {
			records = append(records, stored)
			continue
		}
		stored, err := c.LastStoredMessage(ctx, subject)
		if errors.Is(err, nats.ErrMsgNotFound) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("bus: read the takeover record %s: %w", subject, err)
		}
		var record TakeoverRecord
		err = json.Unmarshal(stored.Data, &record)
		if role, previous, _, handover := handoverOf(subject); handover {
			record.Role, record.Previous = role, previous
		}
		if err != nil || record.Role == "" || record.Previous == "" {
			continue
		}
		read := StoredTakeoverRecord{Subject: subject, Sequence: stored.Sequence, Record: record, Written: stored.Time}
		if record.Finished && caches {
			c.finishedTakeovers.hold(subject, held, read)
		}
		records = append(records, read)
	}
	return records, nil
}

// finishedTakeovers is, by subject, each finished takeover record a client read
// (Client.TakeoverRecords) and the count of messages the stream held on its subject at the read.
// Its zero value holds none.
type finishedTakeovers struct {
	mu   sync.Mutex
	held map[string]finishedTakeover
}

type finishedTakeover struct {
	messages uint64
	record   StoredTakeoverRecord
}

// read is the finished record held for subject, when its subject still holds messages messages. A
// subject whose count moved has its record dropped: the next read of that subject answers from the
// stream, and what it reads is held again when it is finished.
func (f *finishedTakeovers) read(subject string, messages uint64) (StoredTakeoverRecord, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	held, ok := f.held[subject]
	if !ok {
		return StoredTakeoverRecord{}, false
	}
	if held.messages != messages {
		delete(f.held, subject)
		return StoredTakeoverRecord{}, false
	}
	return held.record, true
}

// hold keeps record, read finished from subject while it held messages messages.
func (f *finishedTakeovers) hold(subject string, messages uint64, record StoredTakeoverRecord) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.held == nil {
		f.held = map[string]finishedTakeover{}
	}
	f.held[subject] = finishedTakeover{messages: messages, record: record}
}

// keepListed drops every held record whose subject listed, a read of the whole lane, does not name:
// the stream's age limit took it.
func (f *finishedTakeovers) keepListed(listed map[string]uint64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for subject := range f.held {
		if _, ok := listed[subject]; !ok {
			delete(f.held, subject)
		}
	}
}

// ProbeConsumerCalls is SessionConsumers.ProbeConsumerCalls on this client's connection: whether its
// NATS user may make the session consumers' calls, which a takeover pass's read of the previous
// holder's floor is one of (SessionReadFloor).
func (c *Client) ProbeConsumerCalls(ctx context.Context) error {
	return sessionConsumersOn(c, 0, nil).ProbeConsumerCalls(ctx)
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
