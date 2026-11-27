// The takeover copy: when a session takes a role from a different session, the role messages the
// previous holder did not read stay on its own agent subject, where only that session reads them.
// The listener copies them to the role's kept lane (bus.KeptRoleSubject), whose walk hands them to
// whoever holds the role, so the role's next holder receives each at least once.
//
// The claim route writes a takeover record before each try of its claim write (publishTakeover,
// bus.TakeoverSubject), and a try whose record did not store writes no claim and answers 503
// (errTakeoverUnrecorded), which the Legion claim boot and controller claim again after
// (claimRoleAgainOnBusy in packages/pi-shared). A pass of a record reads the previous holder's
// subject from the acknowledgement floor of its session consumer on and copies each role message of
// the role under a minted dedupe key of its own (runTakeoverPasses, takenFrameDedupeKey), and writes
// the record when it changed it or once its last write is takeoverRecordRefresh old; the claim
// route's handover and the kept lane's sweep run the passes (handOverTakenRole). The session relay
// copies a role message whose role another session holds instead of delivering it
// (sessionRelays.takeOverFrame). Dispatch marks a send for a role route from its own identity alone
// (internal/dispatch/envoy, Send), which is what lets a copy find a message Dispatch sent.

package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/id"
	"github.com/sjawhar/envoy/internal/logging"
	"github.com/sjawhar/envoy/internal/store"
)

// takeoverRecordLife is how long a takeover record's passes run: a record is finished by the first
// pass that starts more than this long after the claim's time and reads to the end of the previous
// holder's subject, and no pass runs it again. It is the keep window (roleKeepWindow). A forward to
// the previous holder starts only on a resolution younger than roles.ForwardFenceAge, the keep
// window less one sweep interval, and every resolution that names the previous holder is read
// before the claim lands, inside the claim route's budget (sendRequestBudget), so each such forward
// is stored inside the hour the passes read from the claim on, with the rest of the hour for the
// forward's own wait; the pass that finishes the record has read it. A forward the stream did not
// acknowledge inside that wait is no delivery to its sender (finishRoleForward, and the outbox's
// and the publish route's own failures), so a copy stored later than the record's end is one its
// sender is told failed. A record lives that long and not the stream's age, so the passes of the
// three shared listeners read the previous holders' subjects of the last hour's takeovers alone,
// not of three days' (TestATakeoverRecordEndsWithinItsLifeWhilePassesKeepRunning).
const takeoverRecordLife = roleKeepWindow

// takeoverRecordWindow bounds the write of a takeover record before a try of a claim's write. The
// claim route writes it inside its own request budget (sendRequestBudget), and a record the stream
// did not store is a claim that writes nothing (errTakeoverUnrecorded).
const takeoverRecordWindow = time.Second

// errTakeoverUnrecorded wraps the failure of a claim whose takeover record the stream did not
// store - a full stream, a write that went unanswered within takeoverRecordWindow, a leader
// election - so the claim wrote nothing (takeoverTries.beforeTry). The claim route answers it 503
// with Retry-After: 1, and the client claims again. A record write nothing answered
// (takeoverRecordUnanswered) has that 503's error open with busUnansweredPrefix.
var errTakeoverUnrecorded = errors.New("the takeover record of this claim did not store, so the claim was not written")

// takeoverRecordUnanswered reports whether err, a claim's errTakeoverUnrecorded, is a takeover
// record write the bus did not answer. The bus answered a write that the stream refused because
// it is full (bus.ErrStreamFull) and one the server denied (bus.ErrPublishDenied), and this
// client refused the record's own bytes (bus.ErrRefused) before it sent them. Every other failure
// of the write did not answer, whatever it names, as storeAnswer reads the claim's own store work.
func takeoverRecordUnanswered(err error) bool {
	var unrecorded takeoverUnrecordedError
	if !errors.As(err, &unrecorded) {
		return false
	}
	cause := unrecorded.err
	return !errors.Is(cause, bus.ErrStreamFull) && !errors.Is(cause, bus.ErrPublishDenied) && !errors.Is(cause, bus.ErrRefused)
}

// takeoverUnrecordedError is errTakeoverUnrecorded for the claim of role whose try read previous
// as the holder: what the claim route's warning names (warnTakeoverUnrecorded).
type takeoverUnrecordedError struct {
	previous string
	err      error
}

func (e takeoverUnrecordedError) Error() string {
	return fmt.Sprintf("%s (previous holder %s): %s", errTakeoverUnrecorded, e.previous, e.err)
}

func (e takeoverUnrecordedError) Is(target error) bool { return target == errTakeoverUnrecorded }

func (e takeoverUnrecordedError) Unwrap() error { return e.err }

// takeoverUnrecordedWarnings holds back the claim route's warning for a role whose takeover claim
// was refused within the last minute, counting what it held back, so a stream that refuses every
// write for an hour logs one line a minute for each role and not one for each claim's backoff.
var takeoverUnrecordedWarnings = struct {
	mu   sync.Mutex
	last map[string]time.Time
	held map[string]int
}{last: map[string]time.Time{}, held: map[string]int{}}

// takeoverWarningForget is how long a role's entry in takeoverUnrecordedWarnings outlives its last
// line: a role refused again after it logs at once, and its count of held-back lines is dropped.
const takeoverWarningForget = 10 * time.Minute

// warnTakeoverUnrecorded logs the claim route's refusal of claimant's takeover claim of role whose
// record did not store (errTakeoverUnrecorded), at most once a minute for each role, naming the
// role, the previous holder the claim read, the claimant and the record's failure: while the stream
// refuses writes, the role stays with a holder that may have stopped, and this line is the
// listener's trace of it.
func warnTakeoverUnrecorded(logger *logging.Logger, role, claimant string, err error) {
	var refused takeoverUnrecordedError
	previous := ""
	if errors.As(err, &refused) {
		previous = refused.previous
	}
	w := &takeoverUnrecordedWarnings
	w.mu.Lock()
	now := time.Now()
	if last, ok := w.last[role]; ok && now.Sub(last) < time.Minute {
		w.held[role]++
		w.mu.Unlock()
		return
	}
	held := w.held[role]
	w.last[role], w.held[role] = now, 0
	// The roles whose last line is takeoverWarningForget old are forgotten, with the lines they
	// held back, so the two maps hold only the roles refused in that time, not every role ever.
	for other, at := range w.last {
		if now.Sub(at) >= takeoverWarningForget {
			delete(w.last, other)
			delete(w.held, other)
		}
	}
	w.mu.Unlock()
	logger.Warn("listener role claim refused: the takeover record of the claim did not store, so the role stays with its holder",
		slog.String("role", role),
		slog.String("previous_session_id", previous),
		slog.String("session_id", claimant),
		slog.Int("held_back", held),
		slog.String("error", err.Error()))
}

// keptFrameMark separates a role message's dedupe key, and the kept message id of the message
// (bus.KeptMsgID), from the stream sequence of the frame a takeover copied it from, in the copy's
// own dedupe key (takenFrameDedupeKey) and message id (takenFrameMsgID).
const keptFrameMark = ":frame:"

// takenFrameMsgID is the message id the copy of the frame stored at seq goes to role's kept lane
// under: bus.KeptMsgID of the frame's dedupe key without the forward mark, then keptFrameMark and
// seq. A second copy of the same frame - by a pass that stopped, by the sweep, by another listener,
// or by the relay of a previous holder that came back - is a duplicate the stream stores once, and
// the sequence keeps the id apart from every earlier kept copy of the same message, so no copy is
// dropped as a repeat of one that went to another holder.
func takenFrameMsgID(dedupeKey, role string, seq uint64) string {
	return bus.KeptMsgID(dedupeKey, role) + keptFrameMark + strconv.FormatUint(seq, 10)
}

// takenFrameOf is the frame msgID names when it is a takeover copy's message id on role's kept
// lane (takenFrameMsgID): the frame's dedupe key without the forward mark, the frame's stream
// sequence, and whether msgID is such an id. It is the one parse of that id's format, by which a
// takeover's copy (takenFrameOriginal) and a withdrawal mark (isWithdrawnFrame) are each told.
func takenFrameOf(msgID, role string) (key string, seq uint64, ok bool) {
	suffix := ":" + bus.KeptRoleSubject(role) + keptFrameMark
	at := strings.LastIndex(msgID, suffix)
	if at <= 0 {
		return "", 0, false
	}
	seq, err := strconv.ParseUint(msgID[at+len(suffix):], 10, 64)
	if err != nil {
		return "", 0, false
	}
	return msgID[:at], seq, true
}

// takenFrameDedupeKey is the dedupe key of the copy of the frame stored at seq, whose own dedupe
// key without the forward mark is dedupeKey: a key in the minted form
// (contracts.MintedDedupeKeyPattern), a prefix and 32 hex digits of the SHA-256 of dedupeKey,
// keptFrameMark and seq, so one frame's copies share it and no other message has it. The prefix
// is publishDedupePrefix for a frame whose key has it - the listener's own publish route forwarded
// it - and takenFrameKeyPrefix for any other, so the listener's own messages are told from
// Dispatch's outbox copies by the copy's key (isOutboxCopy) however many takeovers it went through.
// A minted key names its event (contracts.DedupeKeyNamesTheUpstreamEvent), so every walk - this
// release's, and the release before's, which reads a copy as any kept copy - forwards the copy
// under that key (roleForward.msgID), an id no earlier forward of the same message took, and
// records what a holder took by it. A role taken from A by B and back by A so gives A the copy of
// the frame B did not read, whatever key the message had, where the role lane's own forward of the
// message to A would have the stream drop it. The copy keeps the message's event id, by which a
// host that already showed the message drops it (R4).
func takenFrameDedupeKey(dedupeKey string, seq uint64) string {
	sum := sha256.Sum256([]byte(dedupeKey + keptFrameMark + strconv.FormatUint(seq, 10)))
	prefix := takenFrameKeyPrefix
	if strings.HasPrefix(dedupeKey, publishDedupePrefix) {
		prefix = publishDedupePrefix
	}
	return prefix + hex.EncodeToString(sum[:16])
}

// takenFrameKeyPrefix begins the dedupe key of a takeover's copy of a frame the publish route did
// not forward (takenFrameDedupeKey): the agent form of a minted key, under a name no session's own
// key takes.
const takenFrameKeyPrefix = "agent.takeover."

// takenFrameOriginal is item as an exception about it names it. A takeover's copy carries a
// minted dedupe key of its own (takenFrameDedupeKey), and a reader of the exception, such as the
// Legion daemon's re-hold, identifies the message by the message's own key, which the copy's
// message id on role's kept lane keeps (takenFrameMsgID: bus.KeptMsgID of that key, then
// keptFrameMark and the frame's sequence). For a copy whose key is the copy key of that key and
// sequence it is item under that key; any other kept copy is item as it is. A copy of a copy - a
// role that moved twice before its message was read - is named by the first copy's key, not the
// message's own: the second copy's message id keeps only the key of the frame it copied.
func takenFrameOriginal(item contracts.Envelope, msgID, role string) contracts.Envelope {
	key, seq, ok := takenFrameOf(msgID, role)
	if !ok || item.DedupeKey != takenFrameDedupeKey(key, seq) {
		return item
	}
	original := item
	original.DedupeKey = key
	return original
}

// takeoverFaultSet holds the takeover copy's test seams; production runs with none
// (takeoverFaultHooks). Each names a point a row forces: a crash or a refusal at that point.
type takeoverFaultSet struct {
	// beforeClaimTry runs after a try's takeover record and before the try's claim write, with
	// the role and the previous holder the record names; an error ends the claim with nothing
	// written.
	beforeClaimTry func(role, previous string) error
	// skipClaimHandover, when it reports true for a role, has the claim route start no handover
	// of the role it granted: the listener stopped after the claim write.
	skipClaimHandover func(role string) bool
	// beforeCopy runs before a pass copies the frame stored at frameSeq; an error ends the pass
	// there, with its record written at that frame.
	beforeCopy func(role, previous string, frameSeq uint64) error
	// beforeRecordWrite runs before a pass writes its record, with what it writes.
	beforeRecordWrite func(role, previous string, next uint64, finished bool)
	// floorRead, when it returns an error, is the answer of a pass's read of the previous
	// holder's floor in place of the read.
	floorRead func(previous string) error
	// relayClaimRead and relayCopy, when they return an error, are the answer of the session
	// relay's claim read and of its copy of a frame in place of either.
	relayClaimRead func(role string) error
	relayCopy      func(role string, frameSeq uint64) error
	// claimRecordWrite, when it returns an error, is the answer of the claim route's write of a
	// takeover record in place of the write.
	claimRecordWrite func(role, previous string) error
	// beforeWithdraw runs before the publish route withdraws a forward stored past its fence
	// (withdrawLateForward), with the role and the forward's stream sequence; withdrawPublish,
	// when it returns an error, is the answer of each publish of the withdrawal mark in place of
	// the publish.
	beforeWithdraw  func(role string, frameSeq uint64)
	withdrawPublish func(role string) error
}

// takeoverFaultHooks is one listener's takeover-copy seam set (listenerDeliveryHandlerConfig's
// takeoverFaults), which a test replaces while the listener runs. A nil holder, main.go's, forces
// no fault.
type takeoverFaultHooks struct {
	set atomic.Pointer[takeoverFaultSet]
}

// faults is the seam set in force on h, never nil.
func (h *takeoverFaultHooks) faults() *takeoverFaultSet {
	if h != nil {
		if set := h.set.Load(); set != nil {
			return set
		}
	}
	return &takeoverFaultSet{}
}

// takeoverTries is the claim route's hook on each try of one claim's write
// (store.RoleClaimRequest.BeforeTry, beforeTry): the claim of role by claimant.
type takeoverTries struct {
	cfg      listenerDeliveryHandlerConfig
	claimant string
	role     string
	// replaced is the session other than the claimant that the claim record the last try read
	// names as the holder, empty for none: a write lands only over the record its try read
	// (store.Registry.SetRoleWithPrevious), so once the claim lands this is the session it took the
	// role from, whose consumer the claim route narrows (roleSetHandler).
	replaced string
}

// beforeTry runs before each try of the claim's write: when the claim record that try read names a
// holder other than the claimant, it writes a takeover record for that holder first, so a claim
// that lands always has its record. A record that did not store, for any reason, ends the claim
// with nothing written (errTakeoverUnrecorded), which the claim route answers 503, and its client
// claims again: a full stream refuses the claim for as long as it refuses every other write, and
// the role stays with the holder the claim read until then.
func (t *takeoverTries) beforeTry(ctx context.Context, read store.RoleClaim) error {
	previous := read.HolderSessionID
	t.replaced = ""
	if previous == "" || previous == t.claimant {
		return nil
	}
	t.replaced = previous
	if err := publishTakeover(ctx, t.cfg, t.role, previous); err != nil {
		return takeoverUnrecordedError{previous: previous, err: err}
	}
	if hook := t.cfg.takeoverFaults.faults().beforeClaimTry; hook != nil {
		return hook(t.role, previous)
	}
	return nil
}

// narrowFormerHolder narrows former, the session a claim of role by claimant has just taken the
// role from (takeoverTries.replaced), when role is a Legion controller role whose notice lane
// former's consumer may filter on (bus.ControllerNoticeLane); a claim of any other role leaves
// former's consumer alone, since no role lane is ever in a consumer filter. Its known-session
// record is written from its stored row (recordFollowedTopics), and its consumer is filtered on
// that row again (storedFilterWrite), as an unsubscribe does: the claim took the role's topic off
// the row, and the filter built from it drops the lane, since the claim, read from the role
// bucket stream's leader, names claimant now (storedRowFilter). The route runs it before it
// answers, so a notice published once the claim is answered reaches the new holder alone.
//
// Both run within ctx's deadline, the claim route's budget (sendRequestBudget): the record
// within ctx itself, and the filter on a context detached from the request, so a client that
// hangs up does not cut the write short, bounded by that same deadline and by
// sessionConsumerCallTimeout, the wait for former's session lock included (storedFilterWrite). So
// the window in which a notice on the lane can still reach former runs from the claim's write to,
// at the latest, the end of the claim route's budget, 3.5 s after the request arrived. Every end
// that may leave the lane on former's consumer - the lock not free in time, the row or the claim
// unread, a consumer call the server refused or did not answer, the fence ending on an older row -
// is logged here at WARN, naming role, lane, former and claimant, since the lane then stays on
// former's consumer until its next registration (a heartbeat, at most 90 s,
// contracts.SessionHeartbeat) or its relay's restart. The filter write does not read former's
// session entry, so a session this listener's cache does not hold is narrowed all the same, and a
// session with no consumer is answered as having none to narrow.
func narrowFormerHolder(ctx context.Context, d *listenerDeps, logger *logging.Logger, role, claimant, former string) {
	lane, controller := bus.ControllerNoticeLane(role)
	if !controller {
		return
	}
	recordFollowedTopics(ctx, d, former, logger)
	narrowCtx := context.WithoutCancel(ctx)
	if deadline, bounded := ctx.Deadline(); bounded {
		var cancel context.CancelFunc
		narrowCtx, cancel = context.WithDeadline(narrowCtx, deadline)
		defer cancel()
	}
	if err := d.consumers.storedFilterWrite(narrowCtx, d.registry, former, filterAfterTakeover); err != nil {
		logger.Warn("listener role takeover left the former holder's consumer filter unnarrowed; the role's notice lane may reach that session until its next registration or relay restart",
			slog.String("role", role),
			slog.String("lane", lane),
			slog.String("session_id", former),
			slog.String("claimant_session_id", claimant),
			slog.String("error", err.Error()))
	}
}

// publishTakeover writes the takeover record of role taken from previous, on a subject of its own
// (bus.TakeoverSubject with a fresh attempt id), with the claim's time on the listener's clock. A
// previous holder whose id no session consumer can be named for (bus.SessionConsumerName) has no
// subject of its own to copy from, so it gets none. An edge listener writes none (cfg.edge): its
// grant publishes no takeover lane and no kept role lane (natsgrant.Edge), so its claim of a role
// another session holds lands with no record, and no pass copies what the previous holder had not
// read: that stays on the previous holder's own subject, which its relay sends it.
func publishTakeover(ctx context.Context, cfg listenerDeliveryHandlerConfig, role, previous string) error {
	if cfg.edge != nil {
		return nil
	}
	if _, ok := bus.SessionConsumerName(previous); !ok {
		return nil
	}
	if refuse := cfg.takeoverFaults.faults().claimRecordWrite; refuse != nil {
		if err := refuse(role, previous); err != nil {
			return fmt.Errorf("record role %s's takeover from %s: %w", role, previous, err)
		}
	}
	writeCtx, cancel := context.WithTimeout(ctx, takeoverRecordWindow)
	defer cancel()
	record := bus.TakeoverRecord{Role: role, Previous: previous, ClaimedAt: cfg.now().UnixMilli()}
	if err := cfg.client.WriteTakeoverRecord(writeCtx, bus.TakeoverSubject(role, previous, id.New()), record); err != nil {
		return fmt.Errorf("record role %s's takeover from %s: %w", role, previous, err)
	}
	return nil
}

// runTakenRoleDrain is runKeptRoleDrain for a role the claim route has just granted: under the
// role's guard and the handover's window, it runs the passes of the role's takeover records
// (passRecords) and then hands its kept lane over, which it does whether or not this listener
// runs passes (handOverTakenRole). An edge listener runs none (cfg.edge), as it hands no kept lane
// over (drainKeptRole): a pass writes the kept role lane and the takeover lane, which its grant
// publishes neither of (natsgrant.Edge).
func runTakenRoleDrain(cfg listenerDeliveryHandlerConfig, role string) {
	if cfg.edge != nil {
		return
	}
	if skip := cfg.takeoverFaults.faults().skipClaimHandover; skip != nil && skip(role) {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), roleKeepHandoverWindow)
	defer cancel()
	release, taken := cfg.roleKeepDrains.hold(ctx, role)
	if !taken {
		return
	}
	defer release()
	records, err := passRecords(ctx, cfg, role)
	if err != nil {
		cfg.logger.Warn("listener takeover copy could not read the role's takeover records",
			slog.String("role", role),
			slog.String("error", err.Error()))
	}
	_, err = handOverTakenRole(ctx, cfg, role, records)
	reportKeptRoleDrain(cfg, role, err)
}

// passRecords is the takeover records this listener runs passes of, for role or for every role
// when role is empty (bus.Client.TakeoverRecords). A listener whose NATS user holds none of the
// session consumers' calls (holdsConsumerGrant), Dispatch's and a host's, reads none and runs no
// pass: it cannot run a record's first copy, or the first after the previous holder came back,
// since each begins with a read of the previous holder's consumer floor (takeoverStart), which
// such a user may not make. So it leaves every pass to the listeners whose users may.
func passRecords(ctx context.Context, cfg listenerDeliveryHandlerConfig, role string) ([]bus.StoredTakeoverRecord, error) {
	if !cfg.holdsConsumerGrant(ctx) {
		return nil, nil
	}
	return cfg.client.TakeoverRecords(ctx, role)
}

// handOverTakenRole runs the passes of each of records, role's takeover records, that is not
// finished (runTakeoverPasses), so the role messages each previous holder did not read are on the
// role's kept lane, and then hands the lane over (handOverKeptRole), for a caller holding role's
// guard. A pass that failed is logged and leaves its record to the next pass; the lane is handed
// over whatever the passes did.
func handOverTakenRole(ctx context.Context, cfg listenerDeliveryHandlerConfig, role string, records []bus.StoredTakeoverRecord) (keptRoleHandover, error) {
	runTakeoverPasses(ctx, cfg, records)
	return handOverKeptRole(ctx, cfg, role, cfg.now)
}

// runTakeoverPasses runs passes of each of records that is not finished, each record's until one
// reads to the end of the previous holder's subject or copies nothing for the reason a pass gives
// (runTakeoverPass), fails, or ctx ends. A pass reads at most roleKeepScanMax messages and writes
// the record with the sequence it reached, so the next goes on from there, here or on any
// listener. A failed pass is logged at WARN, except a read of the previous holder's floor that this
// listener's NATS user may not make (bus.ErrConsumerNotGranted), which is logged at debug and ends
// every pass of records: the records came from passRecords, so the grant went away since, and
// every other record's first pass would be refused the same.
func runTakeoverPasses(ctx context.Context, cfg listenerDeliveryHandlerConfig, records []bus.StoredTakeoverRecord) {
	for _, stored := range records {
		for !stored.Record.Finished && ctx.Err() == nil {
			next, done, err := runTakeoverPass(ctx, cfg, stored)
			if err != nil {
				refused := errors.Is(err, bus.ErrConsumerNotGranted)
				level := slog.LevelWarn
				if refused {
					level = slog.LevelDebug
				}
				cfg.logger.LogEvent(level, "listener takeover copy pass failed; the next pass goes on from its record",
					slog.String("role", stored.Record.Role),
					slog.String("previous_session_id", stored.Record.Previous),
					slog.String("record", stored.Subject),
					slog.String("error", err.Error()))
				if refused {
					return
				}
				break
			}
			stored = next
			if done {
				break
			}
		}
	}
}

// takeoverRecordRefresh is how old a takeover record's last write may be before a pass that
// changed nothing of it writes it again (runTakeoverPass): one that read nothing new on the
// previous holder's subject, and one that copies nothing because the record's previous holder
// holds the role, which writes the time it found that holder (bus.TakeoverRecord.HolderSeenAt).
// It is well inside the stream's age limit less the record's life, so no record ages out of the
// stream while a pass may still need it for as long as some listener runs a pass at least every
// 72 hours less this; and short enough that the hour a record copies from once that holder loses
// the role starts at most this much and one sweep interval (roleWakeInterval) before the hour
// before it lost it. So a record nothing moves costs one write each time this has passed, whatever
// the number of listeners whose sweeps run its passes, and no write in the sweeps between.
const takeoverRecordRefresh = 15 * time.Minute

// runTakeoverPass is one pass of the takeover record stored, and returns the record as it stands
// after the pass, and whether this record's passes are done for now: the pass read to the end of
// the previous holder's subject, or copies nothing until a later pass, which the kept lane's sweep
// runs.
//
// It first reads the role's claim (readRoleClaimHolder). While the claim names the record's
// previous holder - its claim did not land yet or at all, or the role went back to it - the pass
// copies nothing: that holder receives its role messages from its own session consumer
// (sessionRelays.takeOverFrame delivers a role message to the session the claim names), and a copy
// would go back to the subject it was taken from, as a new frame the next pass would copy again.
// The first such pass of a record that had found where to start sets it back to finding it again
// (Next 0, FloorMissed clear): the messages that holder reads while it holds the role are behind
// its floor when it loses the role again, so a later pass starts from that floor, as a new
// takeover's record does, and does not copy them from the sequence this record had reached. A
// record that never moved keeps its state, since a claim of its own still pending lands with
// nothing read yet. Such a pass writes the record again only when it sets it back, once its last
// write is takeoverRecordRefresh old, or to mark it finished once the record's life has passed,
// each time with the time it found that holder (HolderSeenAt). A claim read that failed ends the
// pass with nothing written.
//
// Otherwise, on the record's first pass it finds where to start (takeoverStart): the floor of the
// previous holder's session consumer, else the floor the listener recorded when it deleted that
// consumer, else - on the sweep's pass after one that found neither, since a relay's restart
// deletes a consumer and creates it again within moments - the first message stored roleKeepWindow
// before the record's base time: the claim's time, or the last time a pass found the previous
// holder holding the role again (HolderSeenAt) where that is later, since it read its messages
// until then. It then reads the previous holder's agent subject from there, at most
// roleKeepScanMax messages, and copies each role message of the record's role stored roleKeepWindow
// before that base time or later to the role's kept lane (keepTakenRoleFrame). Such a pass writes
// the record when it changed it (a new sequence, a missed floor, the record finished), or once
// the record's last write is takeoverRecordRefresh old (settle), so the stream's age limit never
// removes a record a pass still needs, and the next pass reads only what is new; the first pass
// that starts more than takeoverRecordLife after the claim's time and reads to the end marks it
// finished.
//
// A read or a copy that failed ends the pass with the record written at the frame it stopped at,
// under the same rule; a failed read of where to start ends it with nothing written, and the next
// pass reads again.
func runTakeoverPass(ctx context.Context, cfg listenerDeliveryHandlerConfig, stored bus.StoredTakeoverRecord) (bus.StoredTakeoverRecord, bool, error) {
	record := stored.Record
	started := cfg.now()
	claimedAt := time.UnixMilli(record.ClaimedAt)
	lifeOver := started.Sub(claimedAt) > takeoverRecordLife
	written := func(record bus.TakeoverRecord, done bool, err error) (bus.StoredTakeoverRecord, bool, error) {
		sequence, writeErr := writeTakeoverRecord(ctx, cfg, stored, record)
		if err := errors.Join(err, writeErr); err != nil {
			return stored, false, err
		}
		return bus.StoredTakeoverRecord{Subject: stored.Subject, Sequence: sequence, Record: record, Written: started}, done, nil
	}
	settle := func(record bus.TakeoverRecord, done bool, err error) (bus.StoredTakeoverRecord, bool, error) {
		if record == stored.Record && started.Sub(stored.Written) < takeoverRecordRefresh {
			return stored, done && err == nil, err
		}
		return written(record, done, err)
	}
	holder, err := readRoleClaimHolder(ctx, cfg, record.Role)
	if err != nil {
		return stored, false, fmt.Errorf("read role %s's claim: %w", record.Role, err)
	}
	if holder == record.Previous {
		switch {
		case lifeOver:
			record.Finished = true
		case record.Next != 0 || record.FloorMissed:
			record.Next, record.FloorMissed = 0, false
		case started.Sub(stored.Written) < takeoverRecordRefresh:
			return stored, true, nil
		}
		record.HolderSeenAt = started.UnixMilli()
		return written(record, true, nil)
	}
	since := time.UnixMilli(max(record.ClaimedAt, record.HolderSeenAt)).Add(-roleKeepWindow)
	agent := contracts.LegacyAgentSubject(record.Previous)
	next := record.Next
	if next == 0 {
		start, missed, err := takeoverStart(ctx, cfg, record, agent, since)
		if err != nil {
			return stored, false, err
		}
		if missed {
			// The next read of the consumer is the sweep's next pass, not this handover's next
			// one: a relay's restart creates the consumer again within moments, and a read
			// inside them would start from the hour before the claim needlessly.
			record.FloorMissed = true
			return settle(record, true, nil)
		}
		if start == 0 {
			// Nothing on the subject from the hour before the claim on: the end, with nothing
			// to start from yet.
			record.Finished = lifeOver
			return settle(record, true, nil)
		}
		next = start
	}
	copied, reachedEnd := 0, false
	stop := func(err error) (bus.StoredTakeoverRecord, bool, error) {
		record.Next = next
		return settle(record, false, err)
	}
	for range roleKeepScanMax {
		message, err := cfg.client.NextStoredMessage(ctx, agent, next)
		if errors.Is(err, nats.ErrMsgNotFound) {
			reachedEnd = true
			break
		}
		if err != nil {
			return stop(fmt.Errorf("read %s after %d: %w", agent, next, err))
		}
		if !message.Time.Before(since) {
			if item, ok := takenRoleMessage(message.Data, record.Role); ok {
				if hook := cfg.takeoverFaults.faults().beforeCopy; hook != nil {
					if err := hook(record.Role, record.Previous, message.Sequence); err != nil {
						return stop(err)
					}
				}
				if err := keepTakenRoleFrame(ctx, cfg, record.Role, item, message.Sequence); err != nil {
					return stop(err)
				}
				copied++
			}
		}
		next = message.Sequence + 1
	}
	record.Next = next
	record.Finished = reachedEnd && lifeOver
	if copied > 0 || record.Finished {
		cfg.logger.Info("listener takeover copy pass copied the previous holder's role messages to the role's kept lane",
			slog.String("role", record.Role),
			slog.String("previous_session_id", record.Previous),
			slog.Int("copied", copied),
			slog.Uint64("next", next),
			slog.Bool("finished", record.Finished))
	}
	return settle(record, reachedEnd, nil)
}

// takeoverStart is where a record's first pass reads the previous holder's subject from: the
// sequence after the floor of its session consumer, or after the floor the listener recorded when
// it deleted that consumer (bus.Client.SessionReadFloor). With neither, missed is set on a record
// whose earlier pass did not miss yet, so the pass ends and the next one reads the consumer again;
// and on one that did, the start is the first message stored at or after since on the subject (a
// stored-message read by start time), or 0 for a subject that holds none: the floor is unknown,
// and what the previous holder read in that hour is copied too.
func takeoverStart(ctx context.Context, cfg listenerDeliveryHandlerConfig, record bus.TakeoverRecord, agent string, since time.Time) (uint64, bool, error) {
	if refuse := cfg.takeoverFaults.faults().floorRead; refuse != nil {
		if err := refuse(record.Previous); err != nil {
			return 0, false, err
		}
	}
	floor, known, err := cfg.client.SessionReadFloor(ctx, record.Previous)
	if err != nil {
		return 0, false, fmt.Errorf("read the floor of %s's session consumer: %w", record.Previous, err)
	}
	if known {
		return floor + 1, false, nil
	}
	if !record.FloorMissed {
		return 0, true, nil
	}
	first, err := cfg.client.FirstStoredMessageSince(ctx, agent, since)
	if errors.Is(err, nats.ErrMsgNotFound) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("read %s from %s: %w", agent, since.UTC().Format(time.RFC3339), err)
	}
	return first.Sequence, false, nil
}

// writeTakeoverRecord writes record on stored's subject while that subject's newest message is
// still the one stored was read at, and returns the sequence it stored
// (bus.Client.WriteTakeoverRecordAt), within takeoverRecordWindow of ctx. A write another pass
// moved under it is bus.ErrTakeoverRecordMoved: this pass writes nothing and the next one reads
// the record again, so a pass held since its read cannot write an older state over a later
// pass's, the finish among it.
func writeTakeoverRecord(ctx context.Context, cfg listenerDeliveryHandlerConfig, stored bus.StoredTakeoverRecord, record bus.TakeoverRecord) (uint64, error) {
	if hook := cfg.takeoverFaults.faults().beforeRecordWrite; hook != nil {
		hook(record.Role, record.Previous, record.Next, record.Finished)
	}
	writeCtx, cancel := context.WithTimeout(ctx, takeoverRecordWindow)
	defer cancel()
	return cfg.client.WriteTakeoverRecordAt(writeCtx, stored, record)
}

// roleOfLaneTopic is the key of the claim a role lane's topic names (store.RoleClaimKeyOf), and
// whether topic is a role's topic: the one rule the role lane's delivery (resolveRoleForward), the
// session relay and a takeover pass (readRoleFrame) read a role topic by, since the relay and the
// pass must read a frame's role as the lane that forwarded it did. The topic names the role in
// either form: the key is the bare name, or <project>.<role> for a free-form role under a project.
func roleOfLaneTopic(topic string) (string, bool) {
	token, ok := strings.CutPrefix(topic, contracts.RoleTopicPrefix)
	if !ok || token == "" {
		return "", false
	}
	role := store.RoleClaimKeyOf(token)
	return role, role != ""
}

// takenRoleMessage reports the envelope of data when data is a role message of role (readRoleFrame),
// read field by field (roleFrame.envelope): a message the listener's wire rules or the envelope's
// own refuse is still copied for the role's next holder, as the session relay and the kept lane's
// walk take it. A message whose role no read can tell is no role message to copy.
func takenRoleMessage(data []byte, role string) (contracts.Envelope, bool) {
	frame, err := readRoleFrame(data)
	if err != nil || frame.role != role {
		return contracts.Envelope{}, false
	}
	return frame.envelope(), true
}

// keepTakenRoleFrame copies item, the frame stored at seq on a session's own subject, to role's
// kept lane, as its envelope without the forward mark (contracts.RoleForwardDedupePrefix) and with
// a dedupe key of its own (takenFrameDedupeKey), under takenFrameMsgID. The stream's duplicate
// answer is a copy already made, which is no error. A clustered stream's refusal of an id still in
// process under another publish (bus.ErrDuplicateInProcess) is published again until the answer
// names that publish's copy, as the keep's own is (publishPastInProcess). A copy of a frame
// Dispatch's outbox forwarded, which carries no role mark, is an outbox copy to the walk
// (isOutboxCopy): it raises no wake and gets no dead letter. A copy of a send Dispatch's API marked
// with its role is the listener's own copy, which the sweep's wake report counts.
func keepTakenRoleFrame(ctx context.Context, cfg listenerDeliveryHandlerConfig, role string, item contracts.Envelope, seq uint64) error {
	key := strings.TrimPrefix(item.DedupeKey, contracts.RoleForwardDedupePrefix)
	kept := item
	kept.DedupeKey = takenFrameDedupeKey(key, seq)
	msgID := takenFrameMsgID(key, role, seq)
	if _, err := publishPastInProcess(ctx, func() (bus.StoredAck, error) {
		return cfg.keepPublish(ctx, bus.KeptRoleSubject(role), kept, msgID)
	}); err != nil {
		return fmt.Errorf("copy role %s's message %s to its kept lane: %w", role, item.EventID, err)
	}
	return nil
}

// withdrawnFrameKeyPrefix begins the dedupe key of a withdrawal mark on a role's kept lane
// (withdrawnFrame), which the walk ends and forwards to no one (judgeKeptRoleCopy).
const withdrawnFrameKeyPrefix = "withdrawn."

// withdrawnFrame is the withdrawal mark of forwarded, the role message stored at seq on a session's
// own subject, whose dedupe key without the forward mark is key: forwarded with the dedupe key of
// the takeover copy of that frame (takenFrameDedupeKey) behind withdrawnFrameKeyPrefix, and with no
// payload, as it is no message. It keeps the event id, the topic, the sender and the summary, so a
// reader of the kept lane sees which forward it withdrew; the forward itself stays on the
// session's subject.
func withdrawnFrame(forwarded contracts.Envelope, key string, seq uint64) contracts.Envelope {
	mark := forwarded
	mark.DedupeKey = withdrawnFrameKeyPrefix + takenFrameDedupeKey(key, seq)
	mark.Payload = ""
	return mark
}

// isWithdrawnFrame reports whether item, stored on role's kept lane under msgID, is a withdrawal
// mark (withdrawnFrame). A sender chooses its own dedupe key, so the key's prefix alone proves
// nothing. The mark is told by its message id, which only the listener sets: the id of a takeover
// copy of a frame (takenFrameMsgID), whose key and frame sequence the mark's dedupe key must name
// exactly. A keep stores a sender's copy under bus.KeptMsgID of its key, which ends with the kept
// lane's subject and never with a frame sequence, so no key a sender chooses lands in that space.
func isWithdrawnFrame(item contracts.Envelope, msgID, role string) bool {
	if !strings.HasPrefix(item.DedupeKey, withdrawnFrameKeyPrefix) {
		return false
	}
	key, seq, ok := takenFrameOf(msgID, role)
	return ok && item.DedupeKey == withdrawnFrameKeyPrefix+takenFrameDedupeKey(key, seq)
}

// withdrawLateForward withdraws forwarded, the role message stored late at seq on its holder's
// own subject (roles.ForwardStoredTooLate), from every takeover copy of it, when holder no longer
// holds the role. In sendToRole a late-stored forward is answered 200 when the role's claim, read
// from the leader, still names holder (not withdrawn, and no mark published); 503 maybe_stored
// when that claim cannot be read, or no mark can be published at all (err); 504 when its
// withdrawal mark lands first (withdrawn); and 200 when a takeover's copy landed first (not
// withdrawn).
//
// It publishes the frame's withdrawal mark (withdrawnFrame) to role's kept lane under the message
// id that a takeover pass's copy and the session relay's copy of that frame take
// (takenFrameMsgID, keepTakenRoleFrame). So the stream decides, in its own store of whichever
// comes first, which one stands:
//   - A mark stored first leaves no copy of the frame to store while the stream remembers the id
//     (contracts.DeliveryDuplicateWindow, 72 hours, longer than any takeover record's life,
//     takeoverRecordLife): withdrawn. The sender's retry resolves the holder afresh and is the
//     message's delivery, unless the role is back with holder by then: that retry's forward
//     takes the withdrawn frame's Nats-Msg-Id, the stream answers it as a duplicate of that
//     frame, and the send is answered 200 for a frame no takeover will copy, which no session
//     reads if holder's relay acknowledged it while another session held the role.
//   - A copy stored first answers the mark as a duplicate. The copy is already on the kept lane,
//     which the walk hands the role's holder: not withdrawn.
//
// A duplicate that may be this call's own mark is read back at its sequence: a send of this call
// that went out unanswered (bus.StoredAck.Resent), or a duplicate of the second publish below. It
// is the mark when the stored message's dedupe key says so. One that cannot be read back is taken
// for the mark, since answering it delivered would lose the message. A publish that failed is made
// once more, because a repeat under the id is itself the read of which came first. When the repeat
// fails too, the error is returned: the forward may still be copied by a later pass or not at all.
// That 503 is accepted deliberately. Its sender's retry gives the role's holder a second copy
// under a new event id if a later pass does copy the forward, and that duplicate is preferred to
// a loss nothing reports. Each publish has roleForwardTimeout of its own, as the send's window may
// have ended while the forward was stored.
//
// Nothing is deleted. The forward stays on its holder's subject, and the mark stays on the kept
// lane, where the walk ends it with a line of its own (judgeKeptRoleCopy), so a later reader sees
// both.
func withdrawLateForward(ctx context.Context, cfg listenerDeliveryHandlerConfig, role, holder string, forwarded contracts.Envelope, seq uint64) (bool, error) {
	key := strings.TrimPrefix(forwarded.DedupeKey, contracts.RoleForwardDedupePrefix)
	mark := withdrawnFrame(forwarded, key, seq)
	subject := bus.KeptRoleSubject(role)
	msgID := takenFrameMsgID(key, role, seq)
	faults := cfg.takeoverFaults.faults()
	if faults.beforeWithdraw != nil {
		faults.beforeWithdraw(role, seq)
	}
	// A holder that still holds the role reads the forward from its own subject, and should it
	// lose the role later, a takeover copies the forward, since no mark stands. Withdrawing it
	// would strand it: a takeover's copy would meet the mark, and nothing else reads the frame.
	// So the forward is withdrawn only once the role's claim, read from the leader, names another
	// session or none. A claim that cannot be read is no withdrawal and no answer of delivered.
	current, err := readRoleClaimHolder(context.WithoutCancel(ctx), cfg, role)
	if err != nil {
		return false, fmt.Errorf("read role %s's claim before withdrawing its late forward: %w", role, err)
	}
	if current == holder {
		return false, nil
	}
	var failed error
	for attempt := range 2 {
		publishCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), roleForwardTimeout)
		ack, err := publishPastInProcess(publishCtx, func() (bus.StoredAck, error) {
			if faults.withdrawPublish != nil {
				if err := faults.withdrawPublish(role); err != nil {
					return bus.StoredAck{}, err
				}
			}
			return cfg.keepPublish(publishCtx, subject, mark, msgID)
		})
		if err != nil {
			cancel()
			failed = err
			continue
		}
		withdrawn := !ack.Duplicate
		if ack.Duplicate && (ack.Resent || attempt > 0) {
			withdrawn = keptMarkAt(publishCtx, cfg, role, ack.Sequence)
		}
		cancel()
		cfg.logger.Info("listener role send's forward was stored past its fence",
			slog.String("role", role),
			slog.String("event_id", forwarded.EventID),
			slog.Uint64("forward_sequence", seq),
			slog.Uint64("kept_sequence", ack.Sequence),
			slog.Bool("withdrawn", withdrawn))
		return withdrawn, nil
	}
	cfg.logger.Warn("listener could not withdraw a role send's forward stored past its fence; the send is answered 503 maybe_stored, and a takeover may still copy it",
		slog.String("role", role),
		slog.String("event_id", forwarded.EventID),
		slog.Uint64("forward_sequence", seq),
		slog.String("error", failed.Error()))
	return false, failed
}

// keptMarkAt reports whether the message stored at seq on role's kept lane is a withdrawal mark
// (isWithdrawnFrame), and true for one it cannot read (withdrawLateForward).
func keptMarkAt(ctx context.Context, cfg listenerDeliveryHandlerConfig, role string, seq uint64) bool {
	stored, err := cfg.client.NextStoredMessage(ctx, bus.KeptRoleSubject(role), seq)
	if err != nil || stored.Sequence != seq {
		return true
	}
	frame, err := readRoleFrame(stored.Data)
	return err != nil || isWithdrawnFrame(frame.envelope(), stored.MsgID, role)
}

// takeOverFrame judges a frame of session sessionID's consumer before the relay sends it, and
// reports whether it took the frame, which the relay then does not send. It reads the frame's role
// first (readRoleFrame), from its topic and its role mark alone, each by its exact key, keeping its
// event id beside them and no other field, so that read makes no allocation for each key a frame
// holds: a frame that is no role message is not taken, at the cost of that read, and nor is a
// frame whose role that read cannot tell - one that is not a JSON object, or whose topic or role
// mark is not a string and names no role - which the relay sends to the session as it sends every
// frame that is no role message, as it did before it told role messages apart. For a role message
// the relay reads the role's claim from the role bucket (readRoleClaimHolder, within the role
// routes' bound on a claim read): a claim naming sessionID leaves the frame to the relay, which
// sends it as it sends every frame, so a holder that continues while it still holds its role
// receives its role messages, and nothing more of it is read here. A claim naming another session,
// or none, has every field of the frame read, the envelope field by field (roleFrame.envelope, as a
// takeover pass and the kept lane's walk read it), and the frame copied to the role's kept lane
// (keepTakenRoleFrame) and only then acknowledged, with one line naming the session, the role, the
// holder and the event, and a handover of the lane started
// (runKeptRoleDrain), so the role's holder receives it and not this session. That holds for an
// envelope the listener's wire rules or the envelope's own refuse: the walk forwards its copy, with
// each value the envelope can hold (roleFrame.envelope says which it leaves out), and the holder's
// reader judges it. The field-by-field read takes every role message, so no role message the relay
// tells is ended here. A claim read or a copy that failed has the frame given back to the server
// with the delay its deliveries earn (relayNakDelayFor), neither sent nor acknowledged, so it is
// copied or sent once they answer.
//
// The hand-over is at least once, not exactly once. The copy and the acknowledgement are two
// steps, so a listener that stops between them, or an acknowledgement that fails, leaves the frame
// to be delivered to this relay again. A second copy under the same message id (takenFrameMsgID)
// is a duplicate the stream stores once, but a relay that by then reads a claim naming this session
// sends the frame itself, beside the copy the walk forwards. The holder can then receive the event
// twice, under the same event id.
//
// An edge listener takes no frame (cfg.edge): its grant publishes no kept role lane
// (natsgrant.Edge), so a copy would never store and the frame would be given back at every
// delivery. The frame goes to the session as every frame does.
func (m *sessionRelays) takeOverFrame(ctx context.Context, sessionID string, msg bus.SessionMessage) bool {
	if m.cfg.edge != nil {
		return false
	}
	frame, err := readRoleFrame(msg.Data)
	if err != nil || frame.role == "" {
		return false
	}
	role := frame.role
	// The id the relay's lines name; one that is not a string reads as none.
	eventID, _ := fieldText("event_id", frame.eventID)
	giveBack := func(why string, err error) bool {
		nakErr := msg.NakWithDelay(relayNakDelayFor(msg.Delivered))
		m.logger.Warn("listener session relay gave a role message back to the server: "+why,
			slog.String("session_id", sessionID),
			slog.String("role", role),
			slog.String("event_id", eventID),
			slog.Uint64("stream_seq", msg.StreamSeq),
			slog.String("error", errors.Join(err, nakErr).Error()))
		return true
	}
	holder, err := relayClaimHolder(ctx, m.cfg, role)
	if err != nil {
		return giveBack("its role's claim could not be read", err)
	}
	if holder == sessionID {
		return false
	}
	item := frame.envelope()
	if err := relayCopyFrame(ctx, m.cfg, role, item, msg.StreamSeq); err != nil {
		return giveBack("its copy to the role's kept lane did not store", err)
	}
	if err := msg.Ack(); err != nil {
		m.logger.Warn("listener session relay could not acknowledge a role message it copied to the role's kept lane",
			slog.String("session_id", sessionID),
			slog.String("role", role),
			slog.String("event_id", eventID),
			slog.String("error", err.Error()))
	}
	m.logger.Info("listener session relay copied a role message to the role's kept lane: another session holds the role, or none does",
		slog.String("session_id", sessionID),
		slog.String("role", role),
		slog.String("holder", holder),
		slog.String("event_id", eventID),
		slog.Uint64("stream_seq", msg.StreamSeq))
	startHandover(m.cfg, role, runKeptRoleDrain)
	return true
}

// relayClaimHolder is the session role's claim names for the session relay
// (readRoleClaimHolder), or the error the relayClaimRead seam forces.
func relayClaimHolder(ctx context.Context, cfg listenerDeliveryHandlerConfig, role string) (string, error) {
	if refuse := cfg.takeoverFaults.faults().relayClaimRead; refuse != nil {
		if err := refuse(role); err != nil {
			return "", err
		}
	}
	return readRoleClaimHolder(ctx, cfg, role)
}

// relayCopyFrame is the session relay's copy of a frame (keepTakenRoleFrame) within
// roleKeepPublishWindow, or the error the relayCopy seam forces.
func relayCopyFrame(ctx context.Context, cfg listenerDeliveryHandlerConfig, role string, item contracts.Envelope, seq uint64) error {
	if refuse := cfg.takeoverFaults.faults().relayCopy; refuse != nil {
		if err := refuse(role, seq); err != nil {
			return err
		}
	}
	copyCtx, cancel := context.WithTimeout(ctx, roleKeepPublishWindow)
	defer cancel()
	return keepTakenRoleFrame(copyCtx, cfg, role, item, seq)
}
