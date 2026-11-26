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
)

// ErrKnownSessionsUnavailable is Read's answer from a store whose bucket is not open, a nil store
// among them: it holds nothing it could answer from, so whether it knows the session was not read.
// It is also what a registration's admission and its write answer from a non-nil store whose
// bucket is not open (AdmitRegistration, RecordRegistrationWithin), which record no principal.
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
// ErrPrincipalUnrecorded, which the registration route answers 503 with Retry-After: 1. The
// client registers again, and that registration's admission reads the record again. So a
// registration whose write is delayed past another registration's admission can never put back a
// principal that admission cleared, or write over the principal the other registration recorded.
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
// Any other write that failed is ErrPrincipalUnrecorded unless the bucket's leader confirms the
// record it holds is one the handover gate reads rightly (principalMayDiffer): one naming this
// registration's principal, recorded, one naming none recorded, or none at all, which the gate
// refuses or cannot judge, so that failure costs only the record's topics and last_seen and is
// the plain error. A record naming another principal, and a leader that did not answer, leave the
// gate reading a principal this session did not last register as, or one nobody confirmed it
// did, so the registration route answers 503 and the client registers again.
//
// A store whose bucket is not open writes nothing, and its registration is
// ErrKnownSessionsUnavailable, as AdmitRegistration answers it: the route admits such a
// registration with no principal recorded, and the claim route refuses every handover while the
// bucket is not open. A nil store, the listener's that could build no JetStream context at all
// and a test's, answers nil, as every one of its writes does.
func (s *KnownSessions) RecordRegistrationWithin(ctx context.Context, admission Admission, record KnownSession) error {
	if s != nil && record.SessionID != "" && s.watch() == nil {
		return ErrKnownSessionsUnavailable
	}
	record.PrincipalRecorded = true
	err := s.writeWithin(ctx, record, true, &admission)
	if err != nil && !errors.Is(err, ErrPrincipalUnrecorded) && s.principalMayDiffer(ctx, record.SessionID, record.Principal) {
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
// is not open can read and write nothing, and answers ErrKnownSessionsUnavailable: the route
// admits that registration with no principal recorded and a warning, as the bucket stays shut
// until the listener's grant covers it, and every handover is refused on that listener meanwhile
// (cmd/listener's handoverRefusal). A nil store, the listener's that could build no JetStream
// context at all and a test's, admits every registration, as each of its writes answers nil.
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
		return Admission{}, ErrKnownSessionsUnavailable
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
// other field is kept as the record held it, last_seen included, since nothing of the session
// changed but the principal it may be registered as; and a record stored in chunks is joined from
// the leader's chunks first (kvchunk.Read), so it is written again with every topic it held. A
// record whose chunks are gone (kvchunk.ErrDamaged), which no reader can join, is written whole
// from its manifest's view instead, which carries every field but the topics it could not fit
// (store.ChunkedRecordView), so it costs that session those topics rather than refuse each of its
// registrations until it ages out. A write another one beat, and a record whose chunks a later
// write replaced, write nothing and are the error.
func (s *KnownSessions) clearPrincipal(ctx context.Context, watcher *kvwatch.Watcher, sessionID string, stored bus.LeaderEntry) (uint64, error) {
	kv, epoch := watcher.Handle()
	value, layout, revision, err := kvchunk.Read(ctx, kv, sessionID, stored.Value, stored.Revision)
	switch {
	case errors.Is(err, kvchunk.ErrDamaged):
		value, layout, revision = stored.Value, kvchunk.Layout{}, stored.Revision
	case err != nil:
		return 0, fmt.Errorf("read the record whole: %w", err)
	}
	if revision != stored.Revision {
		return 0, fmt.Errorf("the record was written again after its read at revision %d", stored.Revision)
	}
	var record KnownSession
	if err := json.Unmarshal(value, &record); err != nil {
		return 0, fmt.Errorf("decode the record: %w", err)
	}
	record.SessionID = sessionID
	record.Principal, record.PrincipalRecorded = "", false
	buf, err := json.Marshal(record)
	if err != nil {
		return 0, err
	}
	written, err := s.writeRecord(ctx, watcher, kv, epoch, record, buf, layout, func(value []byte) (uint64, error) {
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
// registration's admission or write among them) and wrote nothing (RecordRegistrationWithin); and
// a registration's write that failed while the bucket's leader names another principal, recorded,
// than the one the session just registered as, or did not answer. The registration route answers
// it 503 with Retry-After: 1 rather than 200, so the client registers again until the record
// names its principal.
var ErrPrincipalUnrecorded = errors.New("session: the registration's principal is not recorded, and the known-session record may name another")

// principalMayDiffer reports whether sessionID's record, as the bucket's leader holds it, names a
// recorded principal other than principal, and whether the leader did not answer within ctx,
// which confirms nothing: this listener's copy can be behind the leader, so it is not asked. A
// record past the retention, a key that holds none, and a record whose principal no registration
// of this build recorded name none.
func (s *KnownSessions) principalMayDiffer(ctx context.Context, sessionID, principal string) bool {
	stored, found, err := s.Read(ctx, sessionID)
	if err != nil {
		return true
	}
	return found && stored.PrincipalRecorded && stored.Principal != principal
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
