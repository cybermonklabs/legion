// The takeover copy: when a session takes a role from a different session, the role messages the
// previous holder did not read stay on its own agent subject, where only that session reads them.
// The listener copies them to the role's kept lane (bus.KeptRoleSubject), whose walk hands them to
// whoever holds the role, so the role's next holder receives each at least once.
//
// The claim route writes a takeover record before each try of its claim write (publishTakeover,
// bus.TakeoverSubject); a pass of a record reads the previous holder's subject from the
// acknowledgement floor of its session consumer on and copies each role message of the role
// (runTakeoverPasses); the claim route's handover and the kept lane's sweep run the passes
// (handOverTakenRole). The session relay copies a role message whose role another session holds
// instead of delivering it (sessionRelays.takeOverFrame).

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/id"
	"github.com/sjawhar/envoy/internal/store"
)

// takeoverRecordLife is how long a takeover record's passes run: a record is finished by the first
// pass that starts more than this long after the claim's time and reads to the end of the previous
// holder's subject. It is one hour short of the stream's age limit (streamDuplicateWindow, 72
// hours), which every pass's write of the record starts again, so no record a pass still needs ages
// out of the stream; and a forward to the previous holder stored within it is copied (a forward
// starts only on a resolution younger than roles.ForwardFenceAge, but one sent can be stored late).
const takeoverRecordLife = 71 * time.Hour

// takeoverRecordWindow bounds the write of a takeover record before a try of a claim's write. The
// claim route writes it inside its own request budget (sendRequestBudget), and a record the stream
// did not store is a try that writes no claim.
const takeoverRecordWindow = time.Second

// keptFrameMark separates the kept message id of a role message (bus.KeptMsgID) from the stream
// sequence of the frame it was copied from, in the message id of the copy (takenFrameMsgID), and
// the forward id of the walk's forward of that copy (walkKeptRole) from the same sequence.
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

// keptFrameSequence is the stream sequence of the frame a kept copy was copied from, read from the
// copy's message id (takenFrameMsgID), and whether the copy is one; a copy the role lane, the
// publish route or Dispatch's outbox kept names none.
func keptFrameSequence(msgID string) (string, bool) {
	at := strings.LastIndex(msgID, keptFrameMark)
	if at < 0 {
		return "", false
	}
	seq := msgID[at+len(keptFrameMark):]
	if _, err := strconv.ParseUint(seq, 10, 64); err != nil {
		return "", false
	}
	return seq, true
}

// takeoverFaultSet holds the takeover copy's test seams; production runs with none
// (takeoverFaults). Each names a point a row forces: a crash or a refusal at that point.
type takeoverFaultSet struct {
	// beforeClaimTry runs after a try's takeover record and before the try's claim write, with
	// the role, the previous holder the record names and the try's number from 0; an error ends
	// the claim with nothing written.
	beforeClaimTry func(role, previous string, try int) error
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
}

// takeoverFaults is the takeover copy's seam set, nil outside the tests that force a fault.
var takeoverFaults atomic.Pointer[takeoverFaultSet]

// faults is the seam set in force, never nil.
func faults() *takeoverFaultSet {
	if set := takeoverFaults.Load(); set != nil {
		return set
	}
	return &takeoverFaultSet{}
}

// takeoverBeforeTry is the claim route's hook on each try of a claim's write
// (store.RoleClaimRequest.BeforeTry): when the claim record that try read names a holder other than
// the claimant, it writes a takeover record for that holder first, so a claim that lands always
// has its record. A record that did not store ends the claim with nothing written.
func takeoverBeforeTry(cfg listenerDeliveryHandlerConfig, claimant, role string) func(context.Context, store.RoleClaim) error {
	try := 0
	return func(ctx context.Context, read store.RoleClaim) error {
		previous := read.HolderSessionID
		defer func() { try++ }()
		if previous == "" || previous == claimant {
			return nil
		}
		if err := publishTakeover(ctx, cfg, role, previous); err != nil {
			return err
		}
		if hook := faults().beforeClaimTry; hook != nil {
			return hook(role, previous, try)
		}
		return nil
	}
}

// publishTakeover writes the takeover record of role taken from previous, on a subject of its own
// (bus.TakeoverSubject with a fresh attempt id), with the claim's time on the listener's clock. A
// previous holder whose id no session consumer can be named for (bus.SessionConsumerName) has no
// subject of its own to copy from, so it gets none.
func publishTakeover(ctx context.Context, cfg listenerDeliveryHandlerConfig, role, previous string) error {
	if _, ok := bus.SessionConsumerName(previous); !ok {
		return nil
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
	if skip := faults().skipClaimHandover; skip != nil && skip(role) {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), roleKeepHandoverWindow)
	defer cancel()
	release, taken := cfg.roleKeepDrains.hold(ctx, role)
	if !taken {
		return
	}
	defer release()
	_, err := handOverTakenRole(ctx, cfg, role)
	reportKeptRoleDrain(cfg, role, err)
}

// handOverTakenRole runs the passes of every takeover record of role that is not finished
// (runTakeoverPasses), so the role messages each previous holder did not read are on the role's
// kept lane, and then hands the lane over (handOverKeptRole), for a caller holding role's guard.
// The claim route's handover of the role it granted runs it, and so does the kept lane's sweep for
// every role a record names. A pass that failed is logged and leaves its record to the next pass;
// the lane is handed over whatever the passes did.
func handOverTakenRole(ctx context.Context, cfg listenerDeliveryHandlerConfig, role string) (keptRoleHandover, error) {
	runTakeoverPasses(ctx, cfg, role)
	return handOverKeptRole(ctx, cfg, role, cfg.now)
}

// runTakeoverPasses runs passes of each takeover record of role that is not finished, each record's
// until one reads to the end of the previous holder's subject, fails, or ctx ends. A pass reads at
// most roleKeepScanMax messages and writes the record with the sequence it reached, so the next
// goes on from there, here or on any listener.
func runTakeoverPasses(ctx context.Context, cfg listenerDeliveryHandlerConfig, role string) {
	records, err := cfg.client.TakeoverRecords(ctx, role)
	if err != nil {
		cfg.logger.Warn("listener takeover copy could not read the role's takeover records",
			slog.String("role", role),
			slog.String("error", err.Error()))
		return
	}
	for _, stored := range records {
		record := stored.Record
		for !record.Finished && ctx.Err() == nil {
			next, reachedEnd, err := runTakeoverPass(ctx, cfg, stored.Subject, record)
			if err != nil {
				cfg.logger.Warn("listener takeover copy pass failed; the next pass goes on from its record",
					slog.String("role", record.Role),
					slog.String("previous_session_id", record.Previous),
					slog.String("record", stored.Subject),
					slog.String("error", err.Error()))
				break
			}
			record = next
			if reachedEnd {
				break
			}
		}
	}
}

// runTakeoverPass is one pass of the takeover record on subject, and returns the record as it
// wrote it and whether it read to the end of the previous holder's subject.
//
// On the record's first pass it finds where to start (takeoverStart): the floor of the previous
// holder's session consumer, else the floor the listener recorded when it deleted that consumer,
// else - on the pass after one that found neither, a relay's restart deleting a consumer and
// creating it again within moments - the first message stored roleKeepWindow before the claim's
// time. It then reads the previous holder's agent subject from there, at most roleKeepScanMax
// messages, and copies each role message of the record's role stored roleKeepWindow before the
// claim's time or later to the role's kept lane (keepTakenRoleFrame). It copies whether or not the
// takeover's claim landed: a record is a conservative intent. Every pass writes the record again
// with the sequence it reached, so the stream's age limit never removes a record a pass still
// needs, and the next pass reads only what is new; the first pass that starts more than
// takeoverRecordLife after the claim's time and reads to the end marks it finished.
//
// A read or a copy that failed ends the pass with the record written at the frame it stopped at;
// a failed read of where to start ends it with nothing written, and the next pass reads again.
func runTakeoverPass(ctx context.Context, cfg listenerDeliveryHandlerConfig, subject string, record bus.TakeoverRecord) (bus.TakeoverRecord, bool, error) {
	started := cfg.now()
	claimedAt := time.UnixMilli(record.ClaimedAt)
	since := claimedAt.Add(-roleKeepWindow)
	agent := contracts.LegacyAgentSubject(record.Previous)
	next := record.Next
	if next == 0 {
		start, missed, err := takeoverStart(ctx, cfg, record, agent, since)
		if err != nil {
			return record, false, err
		}
		if missed {
			record.FloorMissed = true
			return record, false, writeTakeoverRecord(ctx, cfg, subject, record)
		}
		if start == 0 {
			// Nothing on the subject from the hour before the claim on: the end, with nothing
			// to start from yet.
			record.Finished = started.Sub(claimedAt) > takeoverRecordLife
			return record, true, writeTakeoverRecord(ctx, cfg, subject, record)
		}
		next = start
	}
	copied, reachedEnd := 0, false
	stop := func(err error) (bus.TakeoverRecord, bool, error) {
		record.Next = next
		return record, false, errors.Join(err, writeTakeoverRecord(ctx, cfg, subject, record))
	}
	for range roleKeepScanMax {
		stored, err := cfg.client.NextStoredMessage(ctx, agent, next)
		if errors.Is(err, nats.ErrMsgNotFound) {
			reachedEnd = true
			break
		}
		if err != nil {
			return stop(fmt.Errorf("read %s after %d: %w", agent, next, err))
		}
		if !stored.Time.Before(since) {
			if item, ok := takenRoleMessage(stored.Data, record.Role); ok {
				if hook := faults().beforeCopy; hook != nil {
					if err := hook(record.Role, record.Previous, stored.Sequence); err != nil {
						return stop(err)
					}
				}
				if err := keepTakenRoleFrame(ctx, cfg, record.Role, item, stored.Sequence); err != nil {
					return stop(err)
				}
				copied++
			}
		}
		next = stored.Sequence + 1
	}
	record.Next = next
	record.Finished = reachedEnd && started.Sub(claimedAt) > takeoverRecordLife
	if copied > 0 || record.Finished {
		cfg.logger.Info("listener takeover copy pass copied the previous holder's role messages to the role's kept lane",
			slog.String("role", record.Role),
			slog.String("previous_session_id", record.Previous),
			slog.Int("copied", copied),
			slog.Uint64("next", next),
			slog.Bool("finished", record.Finished))
	}
	return record, reachedEnd, writeTakeoverRecord(ctx, cfg, subject, record)
}

// takeoverStart is where a record's first pass reads the previous holder's subject from: the
// sequence after the floor of its session consumer, or after the floor the listener recorded when
// it deleted that consumer (bus.Client.SessionReadFloor). With neither, missed is set on a record
// whose earlier pass did not miss yet, so the pass ends and the next one reads the consumer again;
// and on one that did, the start is the first message stored at or after since on the subject (a
// stored-message read by start time), or 0 for a subject that holds none: the floor is unknown,
// and what the previous holder read in that hour is copied too.
func takeoverStart(ctx context.Context, cfg listenerDeliveryHandlerConfig, record bus.TakeoverRecord, agent string, since time.Time) (uint64, bool, error) {
	if refuse := faults().floorRead; refuse != nil {
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
	if hook := faults().beforeRecordWrite; hook != nil {
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
// a role's topic, which the listener or Dispatch's outbox forwarded to its holder as the role's
// holder, or whose role mark names a role, which Dispatch's API sent for a route to that role. Each
// is read in either form (contracts.CanonicalRole), as a claim is keyed by the bare name.
func (m roleOfMessage) role() (string, bool) {
	if token, ok := strings.CutPrefix(m.Topic, contracts.RoleTopicPrefix); ok && token != "" {
		_, role, _ := contracts.CanonicalRole(token)
		return role, role != ""
	}
	if m.Role != "" {
		_, role, _ := contracts.CanonicalRole(m.Role)
		return role, role != ""
	}
	return "", false
}

// takenRoleMessage decodes data and reports the envelope when it is a role message of role. A
// message no read can decode is no role message, as the walk ends a copy no read can decode.
func takenRoleMessage(data []byte, role string) (contracts.Envelope, bool) {
	var mark roleOfMessage
	if err := json.Unmarshal(data, &mark); err != nil {
		return contracts.Envelope{}, false
	}
	if of, ok := mark.role(); !ok || of != role {
		return contracts.Envelope{}, false
	}
	var item contracts.Envelope
	if err := decodeStoredEnvelope(data, &item); err != nil {
		return contracts.Envelope{}, false
	}
	return item, true
}

// keepTakenRoleFrame copies item, the frame stored at seq on a session's own subject, to role's
// kept lane, as its envelope without the forward mark (contracts.RoleForwardDedupePrefix), under
// takenFrameMsgID. The stream's duplicate answer is a copy already made, which is no error. A
// clustered stream's refusal of an id still in process under another publish
// (bus.ErrDuplicateInProcess) is published again roleKeepInProcessPause later, as the keep's own is
// (storeKeptRoleCopy). A copy of a Dispatch message is an outbox copy to the walk (isOutboxCopy): it
// raises no wake and gets no dead letter.
func keepTakenRoleFrame(ctx context.Context, cfg listenerDeliveryHandlerConfig, role string, item contracts.Envelope, seq uint64) error {
	kept := item
	kept.DedupeKey = strings.TrimPrefix(item.DedupeKey, contracts.RoleForwardDedupePrefix)
	msgID := takenFrameMsgID(kept.DedupeKey, role, seq)
	for {
		_, err := cfg.client.PublishStoredAck(ctx, bus.KeptRoleSubject(role), kept, msgID)
		if errors.Is(err, bus.ErrDuplicateInProcess) {
			if waitErr := waitUnlessDone(ctx, roleKeepInProcessPause); waitErr != nil {
				return fmt.Errorf("copy role %s's message %s to its kept lane: %w (after %w)", role, item.EventID, waitErr, err)
			}
			continue
		}
		if err != nil {
			return fmt.Errorf("copy role %s's message %s to its kept lane: %w", role, item.EventID, err)
		}
		return nil
	}
}

// takeOverFrame judges a frame of session sessionID's consumer before the relay sends it, and
// reports whether it took the frame, which the relay then does not send. A frame that is no role
// message (roleOfMessage.role) is not taken, at the cost of its decode alone. For a role message
// the relay reads the role's claim from the role bucket (readRoleClaimHolder, within the role
// routes' bound on a claim read): a claim naming sessionID leaves the frame to the relay, which
// sends it as it sends every frame, so a holder that continues while it still holds its role
// receives its role messages; a claim naming another session, or none, has the frame copied to
// the role's kept lane (keepTakenRoleFrame) and only then acknowledged, with one line naming the
// session, the role, the holder and the event, and a handover of the lane started
// (runKeptRoleDrain), so the role's holder receives it and not this session. A claim read or a
// copy that failed has the frame given back to the server with the delay its deliveries earn
// (relayNakDelayFor), neither sent nor acknowledged, so it is copied or sent once they answer.
func (m *sessionRelays) takeOverFrame(ctx context.Context, sessionID string, msg bus.SessionMessage) bool {
	var mark roleOfMessage
	if err := json.Unmarshal(msg.Data, &mark); err != nil {
		return false
	}
	role, ok := mark.role()
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
		// No copy can be made of a frame no read decodes, and none of the role's holders could
		// read it either: it goes to this session as every such frame does.
		return false
	}
	if err := relayCopyFrame(ctx, m.cfg, role, item, msg.StreamSeq); err != nil {
		return giveBack("its copy to the role's kept lane did not store", err)
	}
	if err := msg.Ack(); err != nil {
		m.logger.Warn("listener session relay could not acknowledge a role message it copied to the role's kept lane",
			slog.String("session_id", sessionID),
			slog.String("role", role),
			slog.String("event_id", item.EventID),
			slog.String("error", err.Error()))
	}
	m.logger.Info("listener session relay copied a role message to the role's kept lane: another session holds the role, or none does",
		slog.String("session_id", sessionID),
		slog.String("role", role),
		slog.String("holder", holder),
		slog.String("event_id", item.EventID),
		slog.Uint64("stream_seq", msg.StreamSeq))
	go runKeptRoleDrain(m.cfg, role)
	return true
}

// relayClaimHolder is the session role's claim names for the session relay
// (readRoleClaimHolder), or the error the relayClaimRead seam forces.
func relayClaimHolder(ctx context.Context, cfg listenerDeliveryHandlerConfig, role string) (string, error) {
	if refuse := faults().relayClaimRead; refuse != nil {
		if err := refuse(role); err != nil {
			return "", err
		}
	}
	return readRoleClaimHolder(ctx, cfg, role)
}

// relayCopyFrame is the session relay's copy of a frame (keepTakenRoleFrame) within
// roleKeepPublishWindow, or the error the relayCopy seam forces.
func relayCopyFrame(ctx context.Context, cfg listenerDeliveryHandlerConfig, role string, item contracts.Envelope, seq uint64) error {
	if refuse := faults().relayCopy; refuse != nil {
		if err := refuse(role, seq); err != nil {
			return err
		}
	}
	copyCtx, cancel := context.WithTimeout(ctx, roleKeepPublishWindow)
	defer cancel()
	return keepTakenRoleFrame(copyCtx, cfg, role, item, seq)
}
