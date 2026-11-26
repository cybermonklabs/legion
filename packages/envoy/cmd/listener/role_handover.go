// The handover: a claim that lands and names the session it replaces (previous_session_id, on a
// soft claim or an outright one) hands that session's unread messages to the claimant, whether or
// not the replaced session's claim of the role still stands - the role claim reaper frees a dead
// holder's claim, so a claim made after that reads no holder to take the role from. The claim
// route writes one handover record for the claimant and the session it names (claimHandover,
// publishHandover, bus.HandoverSubject) once the claim has landed. Its passes are a takeover
// record's passes (runTakeoverPass), run where every other record's are, and copy from the
// replaced session's acknowledgement floor on, for as long as that session is not live and for
// the stream's retention past the claim or the last pass that found it live (handoverRecordLife):
// each direct message to it goes to the claimant's own agent subject (handOverDirectFrame), and
// each role message of any role goes to that role's kept lane, which hands it to the role's next
// live holder, the claimant for each role it claims too. Delivery is at least once: a message the
// replaced session acknowledged above its floor is copied as well, and a copy keeps the message's
// event id. A claim hands over only between sessions of one principal: the claim route writes the
// record only when the claim's credential, the claimant's last registration and the replaced
// session's last registration all name one principal (handoverRefusal), and answers any other
// claim with a warning that no message of that session is handed over. Each pass holds the two
// sessions' last registrations to that rule again, at its start and before each copy
// (handoverFence, handoverStands), and the successor read Dispatch makes follows a chain of
// records only through sessions that last registered as the replaced session's principal
// (handoverSuccessors, recordedPrincipal), so a session that registers as another principal after
// the claim is handed nothing more and answers none of the replaced session's deliveries.

package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"strconv"
	"sync"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/ciphertoken"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/logging"
	"github.com/sjawhar/envoy/internal/session"
)

// handoverRecordLife is how long a handover record's passes run (runTakeoverPass): the record is
// finished by the first pass that starts more than this long after its base time - the claim's
// time, or the last pass that found the replaced session live or holding the role (HolderSeenAt),
// where that is later - and reads to the end of the replaced session's subject, as a takeover
// record is past takeoverRecordLife. It is the stream's retention (contracts.DeliveryDuplicateWindow,
// the stream's MaxAge), not the keep window: a direct message to the replaced session has no
// forward fence, and a sender can write to that session at any time after the claim - a person's
// answer to a Dispatch ask the session made, a peer's send to the session id it knows. So each
// message sent to the replaced session within that time is handed over, and the copies, which go
// to the successor's subject under one message id each (handedOverMsgID), stay inside the stream's
// duplicate window, which is its retention too. A message sent after the record finished stays on
// the replaced session's own subject for the stream's retention, where that session reads it if it
// comes back, as every message to a session that is not live did before handovers. While it lives,
// a record costs each listener that runs passes (passRecords), at each sweep (roleWakeInterval),
// one read of the role's claim, of the replaced session's state, of the two sessions' principals
// from the known-session bucket's leader and of the replaced session's subject, and one write of
// the record each time takeoverRecordRefresh has passed.
const handoverRecordLife = contracts.DeliveryDuplicateWindow

// errHandoverUnrecorded wraps the failure of the handover record of a claim that landed and named
// the session it replaces (publishHandover): a full stream, a write that went unanswered within
// takeoverRecordWindow, a leader election. The claim stands, but nothing would copy the replaced
// session's messages, so the claim route answers 503 with Retry-After: 1 and the client claims
// again, which writes the record: the claim it makes again lands, as the claimant already holds
// the role.
var errHandoverUnrecorded = errors.New("the claim landed, but the handover record of the session it replaces did not store, so claim again")

// publishHandover writes the handover record of a claim of role that landed for successor and
// named previous as the session it replaces (bus.HandoverSubject), with the claim's time on the
// listener's clock, so the record's passes (runTakeoverPass) copy what previous did not read to
// successor and to the kept lane of each role it is a message of. The record's subject is the same
// for every such claim, and a write that finds the subject already holds a record is an earlier
// claim's (or its own first send's), whose passes go on from where they stopped: that is no error,
// and no second record is written (bus.Client.WriteHandoverRecord), so a session that claims its
// role again at every registration costs one record. A successor id no session consumer can be
// named for (bus.SessionConsumerName) has no subject to copy to, so it gets none; the claim route
// refuses a previous_session_id of that kind before it writes anything (roleSetHandler), and
// calls this only on a listener that is no edge (claimHandover), whose grant publishes no takeover
// lane (natsgrant.Edge).
func publishHandover(ctx context.Context, cfg listenerDeliveryHandlerConfig, role, previous, successor string) error {
	if _, ok := bus.SessionConsumerName(successor); !ok {
		return nil
	}
	if refuse := cfg.takeoverFaults.faults().handoverRecordWrite; refuse != nil {
		if err := refuse(role, previous, successor); err != nil {
			return fmt.Errorf("%w: record the handover of %s's messages to %s for role %s: %w", errHandoverUnrecorded, previous, successor, role, err)
		}
	}
	writeCtx, cancel := context.WithTimeout(ctx, takeoverRecordWindow)
	defer cancel()
	record := bus.TakeoverRecord{Role: role, Previous: previous, ClaimedAt: cfg.now().UnixMilli()}
	if err := cfg.client.WriteHandoverRecord(writeCtx, bus.HandoverSubject(role, previous, successor), record); err != nil {
		return fmt.Errorf("%w: record the handover of %s's messages to %s for role %s: %w", errHandoverUnrecorded, previous, successor, role, err)
	}
	return nil
}

// requestPrincipal is the principal r's credential acts for, in the one form that names the kind
// of credential too (ciphertoken.Caller.Principal): a broker token's, which tells a person's own
// token from a token an operator's entry gave that person for a project, and from a service; for
// a request with no broker token, the identity token's subject (verifiedPrincipal,
// ciphertoken.IdentityPrincipal), a projected service account's or a Google account's; and empty
// for a credential that proves none, the shared token and a request on an open /v1. The
// registration route records it as the session's principal (session.KnownSession.Principal), and
// the claim route holds a handover to it (handoverRefusal).
func requestPrincipal(r *http.Request) string {
	if caller, ok := brokerCaller(r); ok {
		return caller.Principal()
	}
	return ciphertoken.IdentityPrincipal(verifiedPrincipal(r))
}

// errHandoverUnjudged wraps a claim that landed and named the session it replaces when the
// listener could not read whether the handover is one principal's (handoverRefusal): the
// known-session bucket's leader did not answer, or the claimant's own registration is not
// recorded with its principal yet. The claim stands, and the claim route answers 503 with
// Retry-After: 1, as for a handover record that did not store, so the client claims again and the
// handover is judged then, rather than refused on a read that did not answer. A bucket that is
// not open is no such read: it opens only once an attempt of the store's own succeeds
// (session.KnownSessions says how often it attempts), which on a listener whose NATS grant
// does not carry the bucket is not before the grant does, so the claim is refused instead
// (handoverRefusal) rather than answered a 503 its client would retry for as long as that lasts.
var errHandoverUnjudged = errors.New("the claim landed, but whether this claim may hand over the messages of the session it replaces could not be read, so claim again")

// handoverReadBound bounds each read of a known-session record a handover is judged by
// (recordedPrincipal, for handoverRefusal, handoverStands and handoverSuccessors), each from the
// bucket's leader: the bound a claim's read of a role's holder takes (roleHolderReadTimeout),
// inside the claim route's own budget.
const handoverReadBound = roleHolderReadTimeout

// handoverRefusal is the rule a claim that landed and names replaced as the session it replaces
// (previous_session_id) is held to before the claim route writes replaced's handover record
// (publishHandover): it answers why the claim hands nothing over, or "" where it may. The
// claimant's credential (requestPrincipal), the claimant's last registration and replaced's last
// registration must all name one principal, none equal to none, each registration read from the
// known-session bucket's leader (session.KnownSessions.Read), where this listener's copy can be
// behind a registration another listener took a moment ago. A refused claim stands and writes no
// handover record, so nothing copies replaced's unread messages to the claimant, and replaced's
// consumer keeps them for replaced itself.
//
// A claim names any session it likes as the one it replaces, and a handover record's passes copy
// that session's direct messages to the claimant. The broker token a session holds proves the
// claimant is itself (sessionNotBound), not that it continues the session it names, so without
// this rule any session could name a stopped session - a person's among them - and read its
// messages. A Legion claim's fresh session and the session it replaces register on credentials of
// one principal, so that handover stands. An agent session that names a person's session is
// refused by design: its credential acts for its service, for another principal, or for that
// person on a project an operator's entry names, which is another principal than the person's own
// token (ciphertoken.Caller.Principal). A credential that proves no principal acts for any session
// on every route already, and may name a session whose last registration proved none either,
// which is every handover on a listener its clients reach with no credential; it may not name a
// session that registered as a principal.
//
// A claim is refused where replaced's record does not say which principal replaced last
// registered as (recordedPrincipal): a session the bucket holds no record of (principalAbsent:
// none registered within the retention), one whose record names no principal a registration of
// this build recorded (principalUnrecorded, whatever wrote the record so: among those writes, a
// listener of an earlier build's, a registration as another principal that cleared it and has
// not written its own yet, and an interest change that created it after the session registered
// through a listener whose bucket was not open), and a store that cannot answer
// (principalUnavailable: its bucket is not open, a listener with no known-session store at all
// among them). The refused claim is answered 200 with a warning, and its client does not name
// replaced again, so replaced's messages stay on its own subject. On a listener with a store, a
// bucket that is not open is no lasting state: the store attempts it again on its own
// (session.KnownSessions says how often), and picks up a grant that lands later with no restart,
// so a claim refused in the window before an attempt opens it loses a handover a later claim
// would have made. What only a later claim can read is the error, wrapping errHandoverUnjudged: a
// read the leader did not answer, and a claimant whose own registration is not recorded with its
// principal - a registration whose record did not store, or one an earlier build took - which its
// next registration writes.
// Dispatch lets a session answer a delivery attempt of the session it replaced only where a
// handover record names it and the two still register as one principal (sessionSuccessorsHandler),
// so that reply rule is held to this one too.
func handoverRefusal(ctx context.Context, r *http.Request, known *session.KnownSessions, replaced, claimant string) (string, error) {
	previous, state, err := recordedPrincipal(ctx, known, replaced)
	if err != nil {
		return "", fmt.Errorf("%w: %w", errHandoverUnjudged, err)
	}
	switch state {
	case principalUnavailable:
		return knownBucketNotOpenRefusal, nil
	case principalAbsent:
		return fmt.Sprintf("this listener holds no record of the principal %s last registered as", replaced), nil
	case principalUnrecorded:
		return fmt.Sprintf("no registration through a listener of this build has recorded a principal for %s", replaced), nil
	}
	own, state, err := recordedPrincipal(ctx, known, claimant)
	if err != nil {
		return "", fmt.Errorf("%w: %w", errHandoverUnjudged, err)
	}
	if state != principalRecorded {
		return "", fmt.Errorf("%w: the registration of %s is not recorded with its principal yet", errHandoverUnjudged, claimant)
	}
	principal := requestPrincipal(r)
	switch {
	case principal != own:
		return fmt.Sprintf("this claim's credential acts for another principal than the one %s last registered as", claimant), nil
	case principal != previous:
		return fmt.Sprintf("this claim's credential acts for another principal than the one %s last registered as", replaced), nil
	}
	return "", nil
}

// knownBucketNotOpenRefusal is why handoverRefusal refuses every claim's handover on a listener
// whose known-session bucket is not open: it can read no principal a session registered as, and
// records none for the registrations it takes (subscribeHandler).
const knownBucketNotOpenRefusal = "this listener's known-session bucket (envoy_known_sessions) is not open, so it can read no principal any session registered as, and records none"

// knownBucketNotOpenWarning is the warning a registration through a listener whose known-session
// bucket is not open is answered with (subscribeHandler): the registration is admitted, but no
// principal is recorded for it, and the listener refuses every claim's handover until the bucket
// opens (knownBucketNotOpenRefusal).
const knownBucketNotOpenWarning = "this registration's principal is not recorded, since this listener's known-session bucket (envoy_known_sessions) is not open: until it opens, no claim through this listener is handed the unread messages of the session it replaces"

// knownBucketNotOpenWarnings holds back warnKnownBucketNotOpen's line for a minute after each one,
// counting what it held back: the bucket stays shut until an attempt of the store's own opens it,
// which on a listener whose NATS grant does not carry the bucket is not before the grant does, and
// every registration and heartbeat meets it meanwhile.
var knownBucketNotOpenWarnings struct {
	mu   sync.Mutex
	last time.Time
	held int
}

// warnKnownBucketNotOpen logs a registration of sessionID admitted with no principal recorded
// because the known-session bucket is not open, at most once a minute for the whole listener,
// naming the bucket, the session and how many such registrations it held back since its last line.
func warnKnownBucketNotOpen(logger *logging.Logger, sessionID string) {
	w := &knownBucketNotOpenWarnings
	w.mu.Lock()
	now := time.Now()
	if !w.last.IsZero() && now.Sub(w.last) < time.Minute {
		w.held++
		w.mu.Unlock()
		return
	}
	held := w.held
	w.last, w.held = now, 0
	w.mu.Unlock()
	logger.Warn("listener registered a session with no principal recorded: its known-session bucket is not open, so it hands over no replaced session's messages until the bucket opens",
		slog.String("bucket", bus.KnownSessionBucket),
		slog.String("session_id", sessionID),
		slog.Int("held_back", held))
}

// handoverRefusedWarning is the warning a claim of role that named replaced as the session it
// replaces is answered with when handoverRefusal refused it for why: no message of replaced is
// handed over. A claim that took role from replaced while replaced still held it (took) says
// which messages still move: the role's own, which replaced had not read, go to the role's
// holder as they do for every claim that takes a role from another session (publishTakeover),
// since they were sent to the role and not to replaced.
func handoverRefusedWarning(role, replaced string, took bool, why string) string {
	warning := fmt.Sprintf("no message of %s is handed over to this session: %s", replaced, why)
	if took {
		warning += fmt.Sprintf("; the messages of role %s that %s had not read go to the role's holder, as they do for every claim that takes a role", role, replaced)
	}
	return warning
}

// claimHandover is the claim route's handover of replaced's unread messages to claimant, for a
// claim of role that landed and named replaced as the session it replaces (previous_session_id;
// "" for a claim that names none, which hands nothing over and is answered nothing). On a
// listener that is no edge it judges the claim by the rule that holds a handover to one principal
// (handoverRefusal) and, where the rule allows it, writes replaced's handover record
// (publishHandover) before the claim's own handover reads the role's records, so the first pass
// runs there. It answers the warning the claim is answered with, or "", and the error the route
// answers 503 for the claim that stands, with Retry-After: 1 and listenerwire.CodeHandoverUnrecorded:
// a rule this listener could not read (errHandoverUnjudged) and a record that did not store
// (errHandoverUnrecorded) each leave nothing to copy replaced's messages, and the client's next
// claim judges and writes it. A claim the rule refuses stands, writes no record and is answered
// with the refusal as its warning, logged at INFO; took says the claim took role from replaced
// while replaced held it, whose own messages of role move all the same (handoverRefusedWarning).
// An edge listener writes no handover record and judges none, as its grant publishes no takeover
// lane (natsgrant.Edge): its warning says replaced's messages stay on replaced's own subject.
func claimHandover(ctx context.Context, r *http.Request, d *listenerDeps, logger *logging.Logger, role, replaced, claimant string, took bool) (string, error) {
	switch {
	case replaced == "":
		return "", nil
	case d.delivery.edge != nil:
		return fmt.Sprintf("this edge listener hands over no messages: the unread messages of %s stay on its own subject, and a claim through a shared listener hands them to this session", replaced), nil
	}
	why, err := handoverRefusal(ctx, r, d.known, replaced, claimant)
	switch {
	case err != nil:
		return "", err
	case why != "":
		logger.Info("listener role claim hands over no message of the session it replaces: the rule that holds a handover to one principal refused it, for the reason logged",
			slog.String("role", role),
			slog.String("previous_session_id", replaced),
			slog.String("session_id", claimant),
			slog.String("reason", why))
		return handoverRefusedWarning(role, replaced, took, why), nil
	}
	return "", publishHandover(ctx, *d.delivery, role, replaced, claimant)
}

// handoverStands is handoverRefusal's rule as a pass of previous's handover record to successor
// holds it after the claim (handoverFence): whether the two sessions' last registrations name one
// principal, each read from the known-session bucket's leader (recordedPrincipal), and that
// principal. A session that registers as another principal once the claim landed - a person who
// resumes an agent's session, say - is then no longer the same principal's, and nothing more of
// previous's is copied to successor. A session the bucket holds no record of, and one whose
// principal nobody recorded, stands for no handover. A read that did not answer is the error, for
// the caller to try again, and so is a store that cannot answer (principalUnavailable): a pass on a
// listener that can read no principal ends there without setting its record back (setBack), and a
// later pass, on a listener that can read them, judges it again.
func handoverStands(ctx context.Context, known *session.KnownSessions, previous, successor string) (string, bool, error) {
	principal, state, err := recordedPrincipal(ctx, known, previous)
	switch {
	case err != nil:
		return "", false, err
	case state == principalUnavailable:
		return "", false, fmt.Errorf("read the principal %s last registered as: %w", previous, session.ErrKnownSessionsUnavailable)
	case state != principalRecorded:
		return "", false, nil
	}
	other, state, err := recordedPrincipal(ctx, known, successor)
	return principal, state == principalRecorded && other == principal, err
}

// handoverFence is the gate a pass of previous's handover record to successor holds itself to
// (runTakeoverPass), at its start and again as the last step before each copy, after the
// beforeCopy seam: previous is not live (sessionLiveNow: this listener's cache, and the session
// bucket's leader where the cache does not read it live), and the two sessions last registered as
// one principal (handoverStands, from the known-session bucket's leader), the rule the claim route
// wrote the record under (handoverRefusal) - before each copy, the very principal the pass's
// first judge read. So a registration that lands while the pass runs, through this listener or
// another one, ends the pass before its next copy: one that makes previous live, and one of either
// session as another principal. This listener's copy of the known-session records is not asked,
// since it can be behind a registration through another listener. What no read can fence is a
// registration that lands between the fence's reads and the copy's publish, a stream write the
// buckets' writes are not ordered with: that one copy reaches the session, which is no more than
// the session already holds, since a principal that resumes a session reads every message that
// session was handed before.
type handoverFence struct {
	cfg       listenerDeliveryHandlerConfig
	previous  string
	successor string
	// principal is what the pass's first judge that answered passCopies read both sessions as,
	// and judged says one has.
	principal string
	judged    bool
}

// judge is the fence's verdict now: passWaits while previous is live, passRefused while the two
// sessions do not last register as one principal - after the pass's first passCopies, as the
// principal that judge read - and passCopies otherwise. A read that did not answer, and a
// known-session store that cannot answer (handoverStands), is the error, which ends the pass for
// the next one to judge again.
func (f *handoverFence) judge(ctx context.Context) (passVerdict, error) {
	live, err := sessionLiveNow(ctx, f.cfg, f.previous)
	if err != nil {
		return passCopies, fmt.Errorf("read %s's registration: %w", f.previous, err)
	}
	if live {
		return passWaits, nil
	}
	principal, stands, err := handoverStands(ctx, f.cfg.known, f.previous, f.successor)
	if err != nil {
		return passCopies, err
	}
	if !stands || (f.judged && principal != f.principal) {
		return passRefused, nil
	}
	f.principal, f.judged = principal, true
	return passCopies, nil
}

// principalRecord is what the known-session bucket's leader answers of the principal a session
// last registered as (recordedPrincipal), the one reading the handover rule is built on
// (handoverRefusal, handoverStands, handoverSuccessors).
type principalRecord uint8

const (
	// principalUnavailable: the store cannot answer, since its bucket is not open
	// (session.ErrKnownSessionsUnavailable), a nil store among them: whether the bucket holds a
	// record of the session was not read. It lasts until an attempt of the store's own opens the
	// bucket (session.KnownSessions), and on a nil store for good.
	principalUnavailable principalRecord = iota
	// principalAbsent: the bucket holds no record of the session, none registered within the
	// retention.
	principalAbsent
	// principalUnrecorded: the session's record names no principal a registration of this build
	// recorded (session.KnownSession.PrincipalRecorded), whatever wrote the record so: among those
	// writes, a listener of an earlier build's, a registration as another principal that had it
	// cleared and has not written its own, and a write that is no registration
	// (session.KnownSessions.RecordWithin) creating the record of a session whose registration
	// found the bucket not open.
	principalUnrecorded
	// principalRecorded: a registration of this build recorded the principal, none included.
	principalRecorded
)

// recordedPrincipal is what the known-session bucket's leader records of the principal sessionID
// last registered as (principalRecord), read within handoverReadBound, and that principal, which
// is "" for every answer but principalRecorded. A read that did not answer is the error.
func recordedPrincipal(ctx context.Context, known *session.KnownSessions, sessionID string) (string, principalRecord, error) {
	readCtx, cancel := context.WithTimeout(ctx, handoverReadBound)
	defer cancel()
	record, found, err := known.Read(readCtx, sessionID)
	switch {
	case errors.Is(err, session.ErrKnownSessionsUnavailable):
		return "", principalUnavailable, nil
	case err != nil:
		return "", principalUnavailable, fmt.Errorf("read the principal %s last registered as: %w", sessionID, err)
	case !found:
		return "", principalAbsent, nil
	case !record.PrincipalRecorded:
		return "", principalUnrecorded, nil
	}
	return record.Principal, principalRecorded, nil
}

// sessionLiveNow reports whether session answers now (sessionIsLive), read as a caller that takes
// something away from the session on a miss reads it (liveSessionEntry with the stream leader's
// read): a cached entry that reads live is taken at its word, and one that does not is read again
// from the session bucket's leader, so a copy that has not applied a registration made a moment
// ago does not read a live session as gone. A read that did not answer is its error, on which a
// pass copies nothing.
func sessionLiveNow(ctx context.Context, cfg listenerDeliveryHandlerConfig, sessionID string) (bool, error) {
	if cfg.sessions == nil {
		return false, errors.New("this listener holds no session registry")
	}
	_, _, err := liveSessionEntry(ctx, cfg.sessions, sessionID, roleHolderReadTimeout, sessionIsLive, cfg.sessions.RefreshFromLeader)
	switch {
	case errors.Is(err, nats.ErrKeyNotFound):
		return false, nil
	case err != nil:
		return false, err
	}
	return true, nil
}

// messageToSession reports whether frame's envelope names session's own agent subject as its topic,
// in either form (contracts.TopicForms): the old form (contracts.LegacyAgentSubject), which every
// direct send and dead letter is stored under, or the new form under a project
// (contracts.AgentSubject).
func messageToSession(frame roleFrame, session string) bool {
	topic, err := fieldText("topic", frame.topic)
	return err == nil && topic != "" && slices.Contains(contracts.TopicForms(topic), contracts.LegacyAgentSubject(session))
}

// handOverDirectFrame copies frame, a message to previous itself stored at seq on previous's own
// agent subject (takenFrame), to successor's own agent subject, where successor's session consumer
// delivers it as a message to successor: its envelope with successor's subject as its topic, which
// the edge's frame rule (edgeFrameVerdict) and the plugins read a message to the session by, and a
// dedupe key of its own (takenFrameDedupeKey), under handedOverMsgID. Everything else of the
// envelope is the message's own: its event id, its sender and source session, so a reply goes to
// whoever sent it. A message that names no event id is given one on its first copy
// (handedOverEventID), which every later copy of that copy keeps, so it is one message across
// hops as a message with an event id is. The stream drops a second copy of the message to
// successor for its whole duplicate window - by a pass that stopped, by another listener's, by the
// record of a second role successor claimed, or one that came back round two sessions that each
// replaced the other - and its answer is no error. A clustered stream's refusal of an id still in
// process under another publish (bus.ErrDuplicateInProcess) is published again until the answer
// names that publish's copy (publishPastInProcess).
func handOverDirectFrame(ctx context.Context, cfg listenerDeliveryHandlerConfig, previous, successor string, frame roleFrame, seq uint64) error {
	item := frame.envelope()
	handed := item
	handed.Topic = contracts.LegacyAgentSubject(successor)
	handed.DedupeKey = takenFrameDedupeKey(item.DedupeKey, seq)
	if handed.EventID == "" {
		handed.EventID = handedOverEventID(previous, seq)
	}
	msgID := handedOverMsgID(successor, handed.EventID)
	if _, err := publishPastInProcess(ctx, func() (bus.StoredAck, error) {
		return cfg.keepPublish(ctx, handed.Topic, handed, msgID)
	}); err != nil {
		return fmt.Errorf("hand %s's message %s over to %s: %w", previous, handed.EventID, successor, err)
	}
	return nil
}

// handedOverMsgID is the message id a copy of the message eventID names goes to successor's agent
// subject under (handOverDirectFrame): successor's subject, `:handover:` and the event id, so the
// stream keeps one copy of a message for a session however many passes, records and hops make it.
// Two sessions that each replaced the other copy a message to and fro once each and no more: the
// copy that comes back is a new frame of the same event, which the id the first copy took drops.
func handedOverMsgID(successor, eventID string) string {
	return contracts.LegacyAgentSubject(successor) + ":handover:" + eventID
}

// handedOverEventID is the event id the first copy of a message that names none takes
// (handOverDirectFrame): 32 hex digits, an event id's shape (id.New), drawn from the frame the
// message was first read from, previous's subject and seq, so every pass and listener that copies
// that frame names it alike, and every later hop keeps it.
func handedOverEventID(previous string, seq uint64) string {
	sum := sha256.Sum256([]byte("handover:" + previous + ":" + strconv.FormatUint(seq, 10)))
	return hex.EncodeToString(sum[:16])
}
