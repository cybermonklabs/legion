// The principal a known-session record names: the principal of the credential its session last
// registered on (KnownSession.Principal, PrincipalRecorded), which cmd/listener's handover gate and
// successor read hold a handover to. Only a registration writes it, admitted first
// (AdmitRegistration) and then written at its admission's revision (RecordRegistrationWithin);
// every other write keeps the one the bucket holds (writeWithin, known.go). Each read that decides
// who may read a session's messages is made at the bucket's stream leader (Read).

package session

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/kvchunk"
	"github.com/sjawhar/envoy/internal/kvwatch"
	"github.com/sjawhar/envoy/internal/store"
)

// ErrKnownSessionsUnavailable is Read's answer from a store whose bucket is not open, a nil store
// among them: it holds nothing it could answer from, so whether it knows the session was not read.
// It is also what a registration's admission answers from a non-nil store whose bucket is not open
// (AdmitRegistration), and that registration's write (RecordRegistrationWithin, Admission.notOpen),
// which record no principal.
var ErrKnownSessionsUnavailable = errors.New("session: the known-session store does not answer: its bucket is not open")

// Read returns sessionID's record as the bucket's stream leader holds it now
// (bus.KeyValue.ReadFromLeader), and whether the bucket holds one that has not aged out of
// KnownSessionRetention. It is the read for a caller that decides who may read the session's
// messages (cmd/listener's handover gate): this listener's copy (Get) can be behind a
// registration another listener wrote a moment ago, and a stream copy a direct get reaches can be
// behind the leader. A record stored in chunks is read from its manifest, which carries every
// field but the topics it could not fit (store.ChunkedRecordView), the principal among them. A
// key that holds no record, or a delete marker, is no record. A store whose bucket is not open is
// ErrKnownSessionsUnavailable, and a read the leader did not answer within ctx, or a record this
// build cannot decode, is the error: neither says the session is unknown.
func (s *KnownSessions) Read(ctx context.Context, sessionID string) (KnownSession, bool, error) {
	watcher := s.watch()
	if watcher == nil {
		return KnownSession{}, false, ErrKnownSessionsUnavailable
	}
	kv, _ := watcher.Handle()
	entry, err := kv.ReadFromLeader(ctx, sessionID)
	if err != nil {
		return KnownSession{}, false, fmt.Errorf("session: read the known-session record of %s: %w", sessionID, err)
	}
	if entry.Kind != bus.LeaderPut {
		return KnownSession{}, false, nil
	}
	var record KnownSession
	if err := json.Unmarshal(entry.Value, &record); err != nil {
		return KnownSession{}, false, fmt.Errorf("session: decode the known-session record of %s: %w", sessionID, err)
	}
	if record.LastSeen < s.cutoff() {
		return KnownSession{}, false, nil
	}
	record.SessionID = sessionID
	return record, true, nil
}

// RecordRegistrationWithin is RecordWithin for a registration, whose record's principal is the
// principal of the credential the session registered on, none included: a registration on a
// credential that proves no principal writes a record naming none, whatever the stored record
// named, since the record names the principal the session last registered as. It marks the
// principal recorded (KnownSession.PrincipalRecorded), none included.
//
// The write is a compare-and-swap at exactly the revision the registration's admission read or
// wrote (AdmitRegistration, Admission), the one record the admission left naming this principal,
// none recorded, or nothing. A record written since is left as it is - by a registration of the
// session as another principal, whose admission cleared this principal or whose own write
// recorded its own; by a registration of this session that was admitted after this one; by an
// interest change or a clean stop - and this registration writes nothing and is
// ErrPrincipalUnrecorded and ErrRecordWrittenSince, which the registration route answers 503 with
// Retry-After: 1. The client registers again, and that registration's admission reads the record
// again. So a registration whose write is delayed past another registration's admission can never
// put back a principal that admission cleared, or write over the principal the other registration
// recorded.
//
// The refresh-age skip of an unchanged record holds for a registration only where the record its
// admission read at the bucket's leader names this principal, recorded (Admission), and the
// leader still holds that very revision when the skip is taken (leaderAt): this listener's copy
// can be behind a registration of the session as another principal through another listener,
// before this admission or after it, and a skip on that copy alone would answer the registration
// as recorded while the other principal stands as the one the session last registered as. A skip
// the leader does not confirm is the compare-and-swap write instead, which a record written since
// the admission refuses as above.
//
// Any other write that failed is read against the record the bucket's leader holds after it
// (leaderRecordOf). Where the leader holds the session's record naming this registration's
// principal, recorded, it is ErrRecordStands wrapping the write's failure: the failure cost the
// record only its topics and last_seen. Where the leader holds a record naming another principal,
// recorded, it is ErrPrincipalUnrecorded wrapping the write's failure: the handover gate would read
// a principal this session did not last register as. Where the leader read failed, it is
// ErrPrincipalUnrecorded wrapping the read's failure, with the write's failure as text alone:
// nobody confirmed which principal the record names, and the read's failure, not the write's, is
// what a retry can change. Where the leader holds no record of the session, or one naming no
// recorded principal, it is the write's failure as it came: the gate refuses or cannot judge such
// a record, and nothing records the session as this principal. How the registration route answers
// each is its own (cmd/listener's answerRegistration).
//
// A registration admitted while the store's bucket was not open (Admission.notOpen) writes
// nothing and is ErrKnownSessionsUnavailable, as its admission was, whether or not the bucket
// opened since: the admission read no record to fence the write on, so a compare-and-swap at
// its revision would be made over a record nobody read. The route admits such a registration
// with no principal recorded, and the claim route refuses every handover while the bucket is not
// open. A nil store, the listener's that could build no JetStream context at all and a test's,
// answers nil, as every one of its writes does.
func (s *KnownSessions) RecordRegistrationWithin(ctx context.Context, admission Admission, record KnownSession) error {
	if admission.notOpen {
		return ErrKnownSessionsUnavailable
	}
	record.PrincipalRecorded = true
	err := s.writeWithin(ctx, record, true, &admission)
	if err == nil || errors.Is(err, ErrPrincipalUnrecorded) {
		return err
	}
	held, readErr := s.leaderRecordOf(ctx, record.SessionID, record.Principal)
	switch {
	case readErr != nil:
		return fmt.Errorf("%w: the leader read after the failed write: %w (the write: %v)", ErrPrincipalUnrecorded, readErr, err)
	case held == leaderNamesPrincipal:
		return fmt.Errorf("%w: %w", ErrRecordStands, err)
	case held == leaderNamesAnother:
		return fmt.Errorf("%w: %w", ErrPrincipalUnrecorded, err)
	}
	return err
}

// Admission is what a registration's admission read or wrote of its session's known-session
// record at the bucket's leader (AdmitRegistration), which the registration's own write is fenced
// on (RecordRegistrationWithin). The zero Admission is a key the bucket's stream holds no message
// for, which a nil store's admission answers too.
type Admission struct {
	// revision is the revision of the key's newest message at the leader once the admission was
	// made: the record the read found, the record the admission wrote with its principal cleared,
	// a delete marker, or 0 where the stream holds no message for the key. The registration's
	// write is an update at exactly this revision, 0 included: nats.go's Update sends an expected
	// last subject sequence of 0, which the stream holds only while it holds no message for the key.
	revision uint64
	// holds says the record at revision names the admitted principal, recorded: with the leader
	// still at revision, the confirmation a registration's skip of a record its copy holds
	// unchanged needs (writeWithin).
	holds bool
	// notOpen says the admission found the store's bucket not open and read nothing
	// (ErrKnownSessionsUnavailable), so the registration's write writes nothing either. A store
	// whose bucket was open at the admission keeps it open, so no other admission's write meets
	// a bucket that is not open.
	notOpen bool
}

// AdmitRegistration is the step a registration of sessionID on a credential acting for principal
// takes before it changes anything of the session - its entry, its consumer, its relay, the role
// claims it renews (cmd/listener's registration route) - so that no registration as another
// principal than the bucket names is admitted while the handover gate on any listener still reads
// the bucket's principal as the one the session last registered as: that gate would go on copying
// another session's unread messages to the session, now another principal's, and the successor
// read would go on naming it.
//
// It reads the record from the bucket's leader. A record that names principal, one whose
// principal no registration of this build recorded, and none at all admit the registration as
// they are: the gate refuses or cannot judge each of the last two. A record naming another
// principal, recorded, has that principal cleared before the registration is admitted
// (clearPrincipal): the record is written again over the revision the read found, with no
// principal recorded and everything else as it was - its topics, title, capabilities and
// last_seen - so from then on the gate refuses every handover to or from the session until the
// registration's own write records the new principal, while a send to the session and a
// publish's count still find the record if that write never comes: a registration the route
// refuses after its admission, of a session that is stopped, leaves the session known as it was.
// The answer is the revision the read found or the clear wrote (Admission), which the
// registration's own write is fenced on.
//
// A record the bucket's leader did not answer for, and a clear the bucket refused (another write
// landed since the read, or the bucket takes none), are ErrPrincipalUnrecorded, which the route
// answers 503 with Retry-After: 1, with nothing of the registration admitted. A store whose bucket
// is not open can read and write nothing, and answers ErrKnownSessionsUnavailable with an
// Admission whose write writes nothing (notOpen): the route admits that registration with no
// principal recorded and a warning, and every handover is refused on that listener until the
// bucket opens (cmd/listener's handoverRefusal). It opens once an attempt of the store's own
// succeeds (repairUntilStopped, at most knownBucketRetryMax apart), which on a listener whose
// NATS grant does not carry the bucket is not before the grant does, so a 503 would refuse every
// registration and heartbeat for as long as that lasts. A nil store, the listener's that could
// build no JetStream context at all and a test's, admits every registration, as each of its
// writes answers nil.
//
// Of two registrations of one session that are each admitted before the other writes its record,
// at most one writes it: each write is a compare-and-swap at its own admission's revision, and
// whichever lands first moves the record past the other's. The other writes nothing and is
// answered 503, and the record names the first one's principal until the other registers again,
// which its client does at once (Retry-After: 1). Until then the other's session entry can be the
// one that stands, written after the first's, beside a record naming the first one's principal.
// Nor does any step fence a registration whose listener stops between its entry's write and its
// record's write: the record another registration wrote since this one was admitted then stands
// beside this one's entry, until its client registers again.
func (s *KnownSessions) AdmitRegistration(ctx context.Context, sessionID, principal string) (Admission, error) {
	if s == nil || sessionID == "" {
		return Admission{}, nil
	}
	watcher := s.watch()
	if watcher == nil {
		return Admission{notOpen: true}, ErrKnownSessionsUnavailable
	}
	kv, _ := watcher.Handle()
	stored, err := kv.ReadFromLeader(ctx, sessionID)
	if err != nil {
		return Admission{}, fmt.Errorf("%w: read the known-session record of %s: %w", ErrPrincipalUnrecorded, sessionID, err)
	}
	held, recorded := leaderPrincipal(stored)
	if !recorded || held == principal {
		return Admission{revision: stored.Revision, holds: recorded}, nil
	}
	revision, err := s.clearPrincipal(ctx, watcher, sessionID, stored)
	if err != nil {
		return Admission{}, fmt.Errorf("%w: clear the principal of the known-session record of %s, which names another: %w", ErrPrincipalUnrecorded, sessionID, err)
	}
	return Admission{revision: revision}, nil
}

// clearPrincipal writes sessionID's record, as the bucket's leader held it in stored, again with
// no principal recorded, over stored's revision, and answers the revision it wrote at. Every
// other field is kept as the record held it, last_seen included, and so is every key of it this
// build does not name (store.DecodeKeeping), as every write of the store keeps them, since nothing
// of the session changed but the principal it may be registered as; and a record stored in chunks
// is joined from the leader's chunks first (kvchunk.Read), so it is written again with every topic
// it held. A
// record whose chunks are gone (kvchunk.ErrDamaged), which no reader can join, is written whole
// from its manifest's view instead, which carries every field but the topics it could not fit
// (store.ChunkedRecordView), so it costs that session those topics rather than refuse each of its
// registrations until it ages out. A write another one beat, and a record whose chunks a later
// write replaced, write nothing and are the error.
func (s *KnownSessions) clearPrincipal(ctx context.Context, watcher *kvwatch.Watcher, sessionID string, stored bus.LeaderEntry) (uint64, error) {
	kv, epoch := watcher.Handle()
	value, layout, revision, err := kvchunk.Read(ctx, kv, sessionID, stored.Value, stored.Revision)
	damaged := errors.Is(err, kvchunk.ErrDamaged)
	switch {
	case damaged:
		value, layout, revision = stored.Value, kvchunk.Layout{}, stored.Revision
	case err != nil:
		return 0, fmt.Errorf("read the record whole: %w", err)
	}
	if revision != stored.Revision {
		return 0, fmt.Errorf("the record was written again after its read at revision %d", stored.Revision)
	}
	// A record read whole keeps the keys of it this build does not name; a damaged record's
	// manifest carries none beside its own layout, which the record written whole from it drops.
	var record KnownSession
	var unknown map[string]json.RawMessage
	if damaged {
		err = json.Unmarshal(value, &record)
	} else {
		unknown, err = store.DecodeKeeping(value, &record, knownSessionKeys)
	}
	if err != nil {
		return 0, fmt.Errorf("decode the record: %w", err)
	}
	record.SessionID = sessionID
	record.Principal, record.PrincipalRecorded = "", false
	buf, err := store.EncodeKeeping(record, unknown)
	if err != nil {
		return 0, err
	}
	written, err := s.writeRecord(ctx, watcher, kv, epoch, record, buf, unknown, layout, func(value []byte) (uint64, error) {
		return kv.Update(sessionID, value, stored.Revision)
	})
	if err != nil {
		return 0, err
	}
	if !written.Stored {
		return 0, fmt.Errorf("the record was written again after its read at revision %d", stored.Revision)
	}
	return written.Revision, nil
}

// ErrPrincipalUnrecorded wraps a registration the known-session record may name another principal
// for: one AdmitRegistration could not read or clear the record of before the registration was
// admitted; one whose own write found the record written since its admission (another
// registration's admission or write among them) and wrote nothing (RecordRegistrationWithin,
// ErrRecordWrittenSince); and a registration's write that failed while the bucket's leader names
// another principal, recorded, than the one the session just registered as, or did not answer.
// The registration route answers it 503 rather than 200, so the client registers again until the
// record names its principal.
var ErrPrincipalUnrecorded = errors.New("session: the registration's principal is not recorded, and the known-session record may name another")

// ErrRecordWrittenSince wraps, beside ErrPrincipalUnrecorded, a registration's write that wrote
// nothing because the record was written since the registration's admission: the bucket answered
// it, and refused the compare-and-swap (RecordRegistrationWithin).
var ErrRecordWrittenSince = errors.New("the known-session record was written since this registration was admitted")

// ErrRecordStands wraps a registration's write that failed while the bucket's leader holds the
// session's record naming that registration's principal, recorded (RecordRegistrationWithin): the
// failure cost the record only its topics and last_seen, and the session stays recorded as the
// principal it registers as.
var ErrRecordStands = errors.New("session: the known-session record was not written, and the record the bucket holds names this registration's principal")

// leaderRecord is what the bucket's leader holds of a session's record after a registration's
// write of it failed (leaderRecordOf).
type leaderRecord int

const (
	// leaderNamesNone: no record, one past the retention, or one whose principal no registration
	// of this build recorded.
	leaderNamesNone leaderRecord = iota
	// leaderNamesPrincipal: a record naming the registration's principal, recorded.
	leaderNamesPrincipal
	// leaderNamesAnother: a record naming another principal, recorded.
	leaderNamesAnother
)

// leaderRecordOf reads sessionID's record as the bucket's leader holds it and says which principal
// it names against principal (leaderRecord), or the read's failure: a leader that did not answer
// within ctx confirms nothing. This listener's copy can be behind the leader, so it is not asked.
func (s *KnownSessions) leaderRecordOf(ctx context.Context, sessionID, principal string) (leaderRecord, error) {
	stored, found, err := s.Read(ctx, sessionID)
	switch {
	case err != nil:
		return leaderNamesNone, err
	case !found || !stored.PrincipalRecorded:
		return leaderNamesNone, nil
	case stored.Principal == principal:
		return leaderNamesPrincipal, nil
	}
	return leaderNamesAnother, nil
}

// leaderAt reports whether the bucket's leader holds sessionID's newest message at revision, the
// confirmation a registration's skip of a record its copy holds unchanged needs beside its
// admission's (writeWithin): a record written since the admission, by another registration of the
// session through any listener, is one the registration's write would be refused over, so its
// skip may not answer as if it had written. A leader read that did not answer within ctx confirms
// nothing, and reports false.
func (s *KnownSessions) leaderAt(ctx context.Context, watcher *kvwatch.Watcher, sessionID string, revision uint64) bool {
	kv, _ := watcher.Handle()
	stored, err := kv.ReadFromLeader(ctx, sessionID)
	return err == nil && stored.Revision == revision
}

// leaderPrincipal is the principal the leader's newest message for a record names, and whether a
// registration of this build recorded it (KnownSession.PrincipalRecorded): nothing recorded for a
// key that holds no record, a delete marker, or a value this build cannot decode.
func leaderPrincipal(stored bus.LeaderEntry) (string, bool) {
	var principal struct {
		Principal         string `json:"principal"`
		PrincipalRecorded bool   `json:"principal_recorded"`
	}
	if stored.Kind != bus.LeaderPut || json.Unmarshal(stored.Value, &principal) != nil {
		return "", false
	}
	return principal.Principal, principal.PrincipalRecorded
}
