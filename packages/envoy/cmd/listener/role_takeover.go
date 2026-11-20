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
	"encoding/json"
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
// holder's subject. It is one hour short of the stream's age limit (streamDuplicateWindow, 72
// hours), which every pass's write of the record starts again, so no record a pass still needs ages
// out of the stream; and a forward to the previous holder stored within it is copied (a forward
// starts only on a resolution younger than roles.ForwardFenceAge, but one sent can be stored late).
const takeoverRecordLife = contracts.DeliveryDuplicateWindow - time.Hour

// takeoverRecordWindow bounds the write of a takeover record before a try of a claim's write. The
// claim route writes it inside its own request budget (sendRequestBudget), and a record the stream
// did not store is a claim that writes nothing (errTakeoverUnrecorded).
const takeoverRecordWindow = time.Second

// errTakeoverUnrecorded wraps the failure of a claim whose takeover record the stream did not
// store - a full stream, a write that went unanswered within takeoverRecordWindow, a leader
// election - so the claim wrote nothing (takeoverTries.beforeTry). The claim route answers it 503
// with Retry-After: 1, and the client claims again.
var errTakeoverUnrecorded = errors.New("the takeover record of this claim did not store, so the claim was not written")

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
	suffix := ":" + bus.KeptRoleSubject(role) + keptFrameMark
	at := strings.LastIndex(msgID, suffix)
	if at <= 0 {
		return item
	}
	seq, err := strconv.ParseUint(msgID[at+len(suffix):], 10, 64)
	if err != nil || item.DedupeKey != takenFrameDedupeKey(msgID[:at], seq) {
		return item
	}
	original := item
	original.DedupeKey = msgID[:at]
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
}

// beforeTry runs before each try of the claim's write: when the claim record that try read names a
// holder other than the claimant, it writes a takeover record for that holder first, so a claim
// that lands always has its record. A record that did not store, for any reason, ends the claim
// with nothing written (errTakeoverUnrecorded), which the claim route answers 503, and its client
// claims again: a full stream refuses the claim for as long as it refuses every other write, and
// the role stays with the holder the claim read until then.
func (t takeoverTries) beforeTry(ctx context.Context, read store.RoleClaim) error {
	previous := read.HolderSessionID
	if previous == "" || previous == t.claimant {
		return nil
	}
	if err := publishTakeover(ctx, t.cfg, t.role, previous); err != nil {
		return takeoverUnrecordedError{previous: previous, err: err}
	}
	if hook := t.cfg.takeoverFaults.faults().beforeClaimTry; hook != nil {
		return hook(t.role, previous)
	}
	return nil
}

// publishTakeover writes the takeover record of role taken from previous, on a subject of its own
// (bus.TakeoverSubject with a fresh attempt id), with the claim's time on the listener's clock. A
// previous holder whose id no session consumer can be named for (bus.SessionConsumerName) has no
// subject of its own to copy from, so it gets none.
func publishTakeover(ctx context.Context, cfg listenerDeliveryHandlerConfig, role, previous string) error {
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
// role's guard and the handover's window, it runs the passes of the role's takeover records and
// then hands its kept lane over (handOverTakenRole).
func runTakenRoleDrain(cfg listenerDeliveryHandlerConfig, role string) {
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
	records, err := cfg.client.TakeoverRecords(ctx, role)
	if err != nil {
		cfg.logger.Warn("listener takeover copy could not read the role's takeover records",
			slog.String("role", role),
			slog.String("error", err.Error()))
	}
	_, err = handOverTakenRole(ctx, cfg, role, records)
	reportKeptRoleDrain(cfg, role, err)
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
// listener's NATS user may not make (bus.ErrConsumerNotGranted, as Dispatch's listener and the host
// listeners hold no CONSUMER.INFO on session consumers), which is logged at debug: the record is
// left to the listeners that hold the grant, and the line would repeat at every sweep.
func runTakeoverPasses(ctx context.Context, cfg listenerDeliveryHandlerConfig, records []bus.StoredTakeoverRecord) {
	for _, stored := range records {
		for !stored.Record.Finished && ctx.Err() == nil {
			next, done, err := runTakeoverPass(ctx, cfg, stored)
			if err != nil {
				level := slog.LevelWarn
				if errors.Is(err, bus.ErrConsumerNotGranted) {
					level = slog.LevelDebug
				}
				cfg.logger.LogEvent(level, "listener takeover copy pass failed; the next pass goes on from its record",
					slog.String("role", stored.Record.Role),
					slog.String("previous_session_id", stored.Record.Previous),
					slog.String("record", stored.Subject),
					slog.String("error", err.Error()))
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
		if err := errors.Join(err, writeTakeoverRecord(ctx, cfg, stored.Subject, record)); err != nil {
			return stored, false, err
		}
		return bus.StoredTakeoverRecord{Subject: stored.Subject, Record: record, Written: started}, done, nil
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

// writeTakeoverRecord writes record on its own subject (bus.Client.WriteTakeoverRecord), within
// takeoverRecordWindow of ctx.
func writeTakeoverRecord(ctx context.Context, cfg listenerDeliveryHandlerConfig, subject string, record bus.TakeoverRecord) error {
	if hook := cfg.takeoverFaults.faults().beforeRecordWrite; hook != nil {
		hook(record.Role, record.Previous, record.Next, record.Finished)
	}
	writeCtx, cancel := context.WithTimeout(ctx, takeoverRecordWindow)
	defer cancel()
	return cfg.client.WriteTakeoverRecord(writeCtx, subject, record)
}

// roleOfMessage is what the session relay and a takeover pass read of a frame to tell a role
// message: its topic and its role mark, and the event id the relay's lines name.
type roleOfMessage struct {
	EventID string `json:"event_id"`
	Topic   string `json:"topic"`
	Role    string `json:"role"`
}

// role is the role the frame is a role message of, and whether it is one: a frame whose topic is
// a role's topic (roleOfLaneTopic), which the listener or Dispatch's outbox forwarded to its holder
// as the role's holder, or whose role mark names a role, which Dispatch's API sent for a route to
// that role, read in either form (contracts.CanonicalRole), as a claim is keyed by the bare name.
func (m roleOfMessage) role() (string, bool) {
	if role, ok := roleOfLaneTopic(m.Topic); ok {
		return role, true
	}
	if m.Role != "" {
		_, role, _ := contracts.CanonicalRole(m.Role)
		return role, role != ""
	}
	return "", false
}

// roleOfLaneTopic is the role a role lane's topic names, in its bare form, and whether topic is a
// role's topic: the one rule the role lane's delivery (resolveRoleForward), the session relay and a
// takeover pass read a role topic by, since the relay and the pass must read a frame's role as the
// lane that forwarded it did. The topic names the role in either form (contracts.CanonicalRole);
// a claim is keyed by the bare name.
func roleOfLaneTopic(topic string) (string, bool) {
	token, ok := strings.CutPrefix(topic, contracts.RoleTopicPrefix)
	if !ok || token == "" {
		return "", false
	}
	_, role, _ := contracts.CanonicalRole(token)
	return role, role != ""
}

// decodeRoleMark reads data's role mark (roleOfMessage) alone: the role, the mark, and whether
// data is a role message. It is what the session relay reads of every frame before the role's
// claim, so a frame its own holder takes pays for this one decode and no more.
func decodeRoleMark(data []byte) (string, roleOfMessage, bool) {
	var mark roleOfMessage
	if err := json.Unmarshal(data, &mark); err != nil {
		return "", mark, false
	}
	role, ok := mark.role()
	return role, mark, ok
}

// takenRoleMessage decodes data and reports the envelope when it is a role message of role whose
// whole envelope decodes (decodeStoredEnvelope). A message no read can decode is no role message,
// as the walk ends a copy no read can decode.
func takenRoleMessage(data []byte, role string) (contracts.Envelope, bool) {
	of, _, ok := decodeRoleMark(data)
	if !ok || of != role {
		return contracts.Envelope{}, false
	}
	var item contracts.Envelope
	if err := decodeStoredEnvelope(data, &item); err != nil {
		return contracts.Envelope{}, false
	}
	return item, true
}

// keepTakenRoleFrame copies item, the frame stored at seq on a session's own subject, to role's
// kept lane, as its envelope without the forward mark (contracts.RoleForwardDedupePrefix) and with
// a dedupe key of its own (takenFrameDedupeKey), under takenFrameMsgID. The stream's duplicate
// answer is a copy already made, which is no error. A clustered stream's refusal of an id still in
// process under another publish (bus.ErrDuplicateInProcess) is published again
// roleKeepInProcessPause later, as the keep's own is (storeKeptRoleCopy). A copy of a frame
// Dispatch's outbox forwarded, which carries no role mark, is an outbox copy to the walk
// (isOutboxCopy): it raises no wake and gets no dead letter. A copy of a send Dispatch's API marked
// with its role is the listener's own copy, which the sweep's wake report counts.
func keepTakenRoleFrame(ctx context.Context, cfg listenerDeliveryHandlerConfig, role string, item contracts.Envelope, seq uint64) error {
	key := strings.TrimPrefix(item.DedupeKey, contracts.RoleForwardDedupePrefix)
	kept := item
	kept.DedupeKey = takenFrameDedupeKey(key, seq)
	msgID := takenFrameMsgID(key, role, seq)
	if _, _, err := publishPastInProcess(ctx, cfg.client.PublishStoredAck, bus.KeptRoleSubject(role), kept, msgID); err != nil {
		return fmt.Errorf("copy role %s's message %s to its kept lane: %w", role, item.EventID, err)
	}
	return nil
}

// takeOverFrame judges a frame of session sessionID's consumer before the relay sends it, and
// reports whether it took the frame, which the relay then does not send. It reads the frame's role
// mark alone first (decodeRoleMark): a frame that is no role message is not taken, at the cost of
// that decode. For a role message the relay reads the role's claim from the role bucket
// (readRoleClaimHolder, within the role routes' bound on a claim read): a claim naming sessionID
// leaves the frame to the relay, which sends it as it sends every frame, so a holder that
// continues while it still holds its role receives its role messages, and its envelope is never
// decoded here. A claim naming another session, or none, has the envelope decoded and the frame
// copied to the role's kept lane (keepTakenRoleFrame) and only then acknowledged, with one line
// naming the session, the role, the holder and the event, and a handover of the lane started
// (runKeptRoleDrain), so the role's holder receives it and not this session. A frame whose
// envelope does not decode (decodeStoredEnvelope) under such a claim reaches no session: it is not
// this session's to receive, and a reader that applies fewer rules than the listener's decode
// could act on a role message its session does not hold. It is acknowledged, logged once with the
// session, the role, the holder, its stream sequence and the decode error, and counted
// (envoy_role_kept_undecodable_total{path="relay"}), as the durable path ends a message it cannot
// decode and the kept lane's walk ends a copy no read decodes. A claim read or a copy that failed
// has the frame given back to the server with the delay its deliveries earn (relayNakDelayFor),
// neither sent nor acknowledged, so it is copied or sent once they answer.
func (m *sessionRelays) takeOverFrame(ctx context.Context, sessionID string, msg bus.SessionMessage) bool {
	role, mark, ok := decodeRoleMark(msg.Data)
	if !ok {
		return false
	}
	giveBack := func(why string, err error) bool {
		nakErr := msg.NakWithDelay(relayNakDelayFor(msg.Delivered))
		m.logger.Warn("listener session relay gave a role message back to the server: "+why,
			slog.String("session_id", sessionID),
			slog.String("role", role),
			slog.String("event_id", mark.EventID),
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
	var item contracts.Envelope
	if err := decodeStoredEnvelope(msg.Data, &item); err != nil {
		if ackErr := msg.Ack(); ackErr != nil {
			m.logger.Warn("listener session relay could not acknowledge a role message that cannot be decoded",
				slog.String("session_id", sessionID),
				slog.String("role", role),
				slog.String("event_id", mark.EventID),
				slog.String("error", ackErr.Error()))
		}
		m.cfg.roleKeptUndecodable.Inc([2]string{"path", "relay"})
		m.logger.Error("listener session relay ended a role message that cannot be decoded: another session holds the role, or none does",
			slog.String("session_id", sessionID),
			slog.String("role", role),
			slog.String("holder", holder),
			slog.String("event_id", mark.EventID),
			slog.Uint64("stream_seq", msg.StreamSeq),
			slog.String("error", err.Error()))
		return true
	}
	if err := relayCopyFrame(ctx, m.cfg, role, item, msg.StreamSeq); err != nil {
		return giveBack("its copy to the role's kept lane did not store", err)
	}
	if err := msg.Ack(); err != nil {
		m.logger.Warn("listener session relay could not acknowledge a role message it copied to the role's kept lane",
			slog.String("session_id", sessionID),
			slog.String("role", role),
			slog.String("event_id", mark.EventID),
			slog.String("error", err.Error()))
	}
	m.logger.Info("listener session relay copied a role message to the role's kept lane: another session holds the role, or none does",
		slog.String("session_id", sessionID),
		slog.String("role", role),
		slog.String("holder", holder),
		slog.String("event_id", mark.EventID),
		slog.Uint64("stream_seq", msg.StreamSeq))
	go runKeptRoleDrain(m.cfg, role)
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
