// The listener's kept lane for role messages: the copy it stores when a role message has no live
// holder to take it (bus.KeptRoleSubject), and the walk that ends each of those copies
// (bus.KeptRoleCursorSubject): it hands a copy inside its hour to the role's next live holder,
// and drops a copy whose hour ended, with a dead letter to the session that sent it. Both subjects
// are the listener's own: no session may register a topic under them, and no consumer of the
// notification stream delivers one (bus.StreamSubjects carries no kept prefix), so a kept copy
// never re-enters the delivery path it was taken out of. The sweep that walks every lane on the
// stream, and raises a role's wake report again while one of its copies waits, is in
// role_keep_sweep.go.

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
	"unicode"

	"github.com/nats-io/nats.go"
	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/id"
	"github.com/sjawhar/envoy/internal/logging"
	"github.com/sjawhar/envoy/internal/metrics"
	"github.com/sjawhar/envoy/internal/roles"
	"github.com/sjawhar/envoy/internal/session"
	"github.com/sjawhar/envoy/internal/store"
)

// roleKeepWindow is a kept copy's hour: how old it may be, counted from the time the stream stored
// it, and still be forwarded to the role's next holder. A role token names a tree role rather than
// one run, so the holder a later claim hands a kept message to can be a generation the sender never
// addressed: handing it a day-old control notice is the regression sjawhar/legion#753 removed. One
// hour bounds that, and a forwarded copy carries the time it was kept (contracts.Envelope.KeptAt)
// so its holder can judge the message's age itself. The keep's answer names the end of that hour
// (keptRoleCopy.until, the publish route's kept_until), and a repeat of a dedupe key whose copy is
// still inside it stores nothing new (storeKeptRoleCopy). At its end the first walk that reads the
// copy drops it, whether or not any session holds the role, and sends its sender a dead letter
// (judgeKeptRoleCopy).
const roleKeepWindow = time.Hour

// keptCopyHourEnd is when the hour of a copy the stream stored at stored ends: stored plus
// roleKeepWindow. It is the one rule for that end. The keep's answer names it (keptRoleCopy.until,
// the publish route's kept_until), and the test of a copy whose hour ended reads it
// (keptCopyEnded), so the end the answer names is the moment the walk starts to drop the copy.
func keptCopyHourEnd(stored time.Time) time.Time {
	return stored.Add(roleKeepWindow)
}

// keptCopyEnded reports whether the hour of a copy the stream stored at stored has ended at now,
// the listener's clock: now is past keptCopyHourEnd, so the copy is older than roleKeepWindow. It
// is the one test of that end: the walk drops a copy by it (judgeKeptRoleCopy), and a keep judges
// by it the copy a repeat of a dedupe key names (heldKeptRoleCopy).
func keptCopyEnded(now, stored time.Time) bool {
	return now.After(keptCopyHourEnd(stored))
}

// roleKeepDrainMax is how many kept copies one walk of a role's lane forwards. The claim answers
// its caller at once and the drain runs behind it, so one walk is bounded work rather than however
// much a role's kept lane has gathered; the handover goes on in a walk of its own while the role's
// holder is still live (drainKeptRole), since nothing else would start another drain for it.
const roleKeepDrainMax = 20

// roleKeepScanMax bounds the walk itself, where roleKeepDrainMax bounds the forwards it makes. A
// copy past roleKeepWindow, and one no read can decode, is ended rather than forwarded, and a walk
// that counted those against its forward budget would walk twenty dead copies and hand the role's
// new holder nothing. The stream keeps a kept copy for 72 h while the keep window is one hour, so a
// role's lane can hold nothing but copies past the window: this is what still ends that walk,
// which the handover's next walk, or the next sweep's, carries on from.
const roleKeepScanMax = 200

// roleKeepStaleAge is how old a copy no walk has claimed may be and still get a dead letter when a
// walk drops it: two of roleKeepWindow, so its hour ended at most one hour before that walk. An
// older copy is dropped as stale with no dead letter (judgeKeptRoleCopy): its sender is told of a
// send it made hours before, which it can no longer act on, and the copies a lane gathered while
// no listener walked it end in one sweep without a message each. A copy a walk has claimed is never
// stale, whatever its age: that walk began to end it, and the walk that takes the claim over
// finishes what it began.
const roleKeepStaleAge = 2 * roleKeepWindow

// The values of the kept_by label of envoy_role_kept_expired_total and of the drop line's kept_by
// attribute: who kept the copy a walk dropped (keptCopyKeptBy).
const (
	keptByListener       = "listener"
	keptByDispatchOutbox = "dispatch_outbox"
)

// deadLetterReason is why a walk drops a copy with a dead letter, as the dead letter's lines name
// it (keptRoleDrop.reason): the copy's hour ended, or the role's live holder does not take the
// copy's delivery mode. The walk writes it into its claim of the copy as well, with the qualifier
// of a copy whose forward a walk claimed and no walk confirmed (unconfirmedClaimSuffix,
// keptRoleDrop.claimReason, bus.KeptRoleCursor.DeadLetter, endKeptCopyWithDeadLetter), so the
// walk that takes that claim over sends the dead letter again for the same reason and with the
// same qualifier (keptCopyClaimOf).
type deadLetterReason string

const (
	deadLetterExpired      deadLetterReason = "expired"
	deadLetterDeliveryMode deadLetterReason = "delivery_mode"
)

// unconfirmedClaimSuffix is what a walk adds to the reason it writes into its claim of a copy it
// ends with a dead letter, whichever reason that is, when a walk before it claimed the copy to
// forward it and no walk confirmed that forward (keptRoleDrop.forwarded, keptRoleDrop.claimReason):
// the claim then reads expired_unconfirmed or delivery_mode_unconfirmed. The claim carries that
// qualifier because the walk that takes the claim over reads the claim and nothing else
// (keptCopyClaimOf), and its dead letter must still say that the message may have reached the
// session that held the role then: the forward's claim that said so is overwritten by this one.
// It is a part of the claim alone: the dead letter's own reason, its line and its label stay
// deadLetterExpired or deadLetterDeliveryMode.
const unconfirmedClaimSuffix = "_unconfirmed"

// The walk's lines for a copy it drops: the drop itself, which keeps the text a production check
// and the tests read, the two outcomes of the copy's dead letter, and the two outcomes of the
// expiry notice a copy Dispatch answers for - an outbox copy, or a copy of Dispatch's delivery of a
// post - is told to Dispatch by instead (endKeptCopyWithExpiryNotice).
const (
	roleKeptDroppedLine       = "listener dropped a kept role message older than the keep window"
	roleDeadLetterStoredLine  = "listener stored a dead letter for a kept role message"
	roleDeadLetterRefusedLine = "listener could not send a kept role message's dead letter"
	roleExpiryNoticeLine      = "listener told Dispatch a kept role message ended"
	roleExpiryRefusedLine     = "listener could not tell Dispatch a kept role message ended"
)

// roleKeepPublishWindow bounds the whole of one keep (storeKeptRoleCopy): every publish of its
// chain, the reads that judge the copy a duplicate names, and the pauses a clustered stream's
// refusal of an id still in process earns. The keep happens on a send that is already out of time
// or on a lane delivery that answers nobody, so it takes a context of its own rather than whatever
// is left of its caller's, and that context is short: a bus that does not store the copy promptly
// is a keep that failed, which its caller answers for. One link of the chain costs a publish and
// two reads, a few milliseconds on the cluster's NATS, so the window holds hundreds of links, and
// a chain that runs out of it is a keep that failed as well.
const roleKeepPublishWindow = 2 * time.Second

// roleKeepDrainWindow bounds one walk of a role's kept lane, which runs off the claim route, the
// registration, the keep or the sweep that started the handover. It covers roleKeepDrainMax
// forwards of roleForwardTimeout each, and the read of the role's claim record, the stream read,
// the claim and the dead letter of each of the roleKeepScanMax copies the walk may read between
// them, so a walk whose forwards or dead letters are slow to be stored still ends rather than
// outliving the claim it serves. A handover's pass, and the sweep, take it for the lane-head read
// (handOverKeptRole, sweepKeptRoleLanes, readKeptLaneHead); the handover's read is the one read of
// that lane a pass makes, which it hands to the walk it starts (walkKeptRole).
//
// It is bus.KeptRoleClaimWindow, whose doc says why a walk's life and its claim's hold are one
// duration and what the window assumes of the two clocks it compares.
const roleKeepDrainWindow = bus.KeptRoleClaimWindow

// roleKeepDrainRetryPause is how long a handover waits before its next walk when the one before it
// failed: a read, a cursor write, a forward or a dead letter that did not answer. It is short
// because the copies behind it have senders already answered 202 kept and a holder waiting, and it
// is not zero because a bus that is failing must not be walked again at once. It is the first of
// the pauses a run of failures earns, which double from here to roleKeepDrainRetryCap
// (roleKeepDrainBackoff).
const roleKeepDrainRetryPause = time.Second

// roleKeepDrainRetryCap is the widest pause a run of failed walks earns (roleKeepDrainBackoff):
// the cost of a handover whose bus is not answering must fall to about one walk a half-minute
// rather than one a second. What such a handover retries, and until when, is the rule
// handOverKeptRole states; roleKeepHandoverWindow is what ends the handover.
const roleKeepDrainRetryCap = 30 * time.Second

// roleKeepDrainIdleRounds is how many walks that made no progress one handover takes before it
// gives the role's lane up: the third such walk is its last. A walk made progress when it moved
// the role's cursor - it forwarded a copy, or dropped, passed over or ended one with a dead
// letter - because the copies behind it are then nearer the role's holder, however long the lane
// is; a walk that moved nothing found the lane held by another listener's claim or the role moved
// to a holder this listener cannot read, and repeating that without end would be a drain that
// never stops. A walk that failed counts no round here: it is waited out instead, by the rule
// handOverKeptRole states, since giving a lane up for a bus that is unwell would strand copies a
// retry would deliver. Giving up costs the lane the wait for its next handover: the role's next
// claim, a keep, the registration that makes a session holding the role live again
// (subscribeHandler) or the claim route's refresh that does (roleSetHandler), or the sweep
// (sweepKeptRoleLanes).
const roleKeepDrainIdleRounds = 3

// roleKeepHandoverWindow bounds one handover of a role's kept lane: every walk it makes for the
// claim route, the registration, the keep or the sweep that started it, the pauses between them
// included (drainKeptRole). Each walk is bounded by roleKeepDrainWindow, the walks that make no
// progress by roleKeepDrainIdleRounds and the walks that failed by their widening pauses, so this
// is the last bound rather than the usual one - a handover ends when the lane is drained or when
// its walk with no live holder stops at a copy still inside its hour - and it is wide enough for
// the walks a lane of thousands of copies takes.
const roleKeepHandoverWindow = 10 * time.Minute

// roleKeepHolderWindow bounds a handover's resolution of the role's holder: the claim read and the
// holder's session read, since no resolution releases a lapsed claim. Every pass that resolves at
// all resolves within it, which is every pass whose lane holds copies to hand over
// (handOverKeptRole). It answers no caller - the keep's own answer is written by
// then - so it ends rather than waiting out a bus that is not answering, and what a resolution that
// did not answer costs is the rule handOverKeptRole states.
const roleKeepHolderWindow = 5 * time.Second

// roleKeptDeliveryStatus is the delivery_status of a role message kept instead of delivered: the
// label the role lane counts under messagesDelivered and logs, and the one the publish route's own
// kept answer reads as (sendKept.deliveryStatus), so route and lane say the same word for it.
const roleKeptDeliveryStatus = "kept"

// roleKeepReason is why the role lane kept a message (keepRoleLaneMessage), as the lane's kept line
// names it: one of the three constants below. It is a type of its own rather than a roles.State,
// because one of the three is no state of a resolution, and rather than a plain string, which takes
// any string a caller has to hand: a value of another string type, a roles.State among them,
// needs a conversion to become one. A resolution's state becomes one only through roleKeptReason,
// which names the two states the listener keeps for, so a state it does not keep for is never a
// reason.
type roleKeepReason string

const (
	// roleKeptUnclaimed is a resolution that found no claim for the role, named as the resolution
	// names it (roleKeptReason).
	roleKeptUnclaimed = roleKeepReason(roles.Unclaimed)
	// roleKeptLapsed is a resolution that found a claim whose holder is not live - unresponsive or
	// gone - named as the resolution names it (roleKeptReason). The kept line names that holder.
	roleKeptLapsed = roleKeepReason(roles.Lapsed)
	// roleClaimUnread is a resolution whose claim read did not answer (resolveCoreRoleHolder). It is
	// a reason of the lane's keep alone and no roles.State: a read that failed resolved nothing, so
	// it is never one of the listener's answers - the lookup's 404 reason or a publish's recorded
	// outcome - and only the kept line names it.
	roleClaimUnread roleKeepReason = "claim_unread"
)

// roleKeepFailedLine is logged by both give-up paths - the publish route's and the role lane's -
// when the copy could not be kept, beside the answer each gives when nothing was kept.
const roleKeepFailedLine = "listener kept role message publish failed"

// roleKeepLaneCursor is one walk's hold on a role's drain cursor: everything each write of it
// needs - the role, the holder the walk forwards to (empty for a walk with no live holder), the
// cursor's subject - and the sequence the next compare-and-swap must name, which every write keeps
// itself. A walk writes the cursor three times for a copy it forwards or ends with a dead letter
// and once for every other copy it ends, and each of those asks the same question of the answer,
// which write, confirm and release answer once here.
type roleKeepLaneCursor struct {
	cfg     listenerDeliveryHandlerConfig
	role    string
	holder  string
	subject string
	// seq is the stream sequence of the cursor message this hold last saw, 0 for a role whose
	// lane has never been drained, which the next write names as its compare-and-swap.
	seq uint64
}

// write writes cursor as the role's drain cursor under the compare-and-swap of the sequence this
// hold last saw (bus.Client.PublishCursorAt, 0 for a role whose lane has never been drained), and
// keeps the sequence it was stored at for the next write. A write the stream refused because
// another listener's walk took the cursor comes back wrapping bus.ErrCursorMoved: that listener
// owns this role's lane now, which is nothing wrong, so it is logged at debug here and ends the
// walk as roleKeepLaneBusy where the write is read (roleKeepWriteFailed). Every other failure is
// the error it is, and neither moves the sequence this hold names next.
func (c *roleKeepLaneCursor) write(ctx context.Context, cursor bus.KeptRoleCursor) error {
	body, err := json.Marshal(cursor)
	if err != nil {
		return fmt.Errorf("encode role %s's kept-lane cursor at %d: %w", c.role, cursor.Kept, err)
	}
	next, err := c.cfg.client.PublishCursorAt(ctx, c.subject, body, c.seq)
	if err != nil {
		if errors.Is(err, bus.ErrCursorMoved) {
			c.cfg.logger.LogEvent(slog.LevelDebug, "listener kept role drain lost the cursor to another listener",
				slog.String("role", c.role),
				slog.String("session_id", c.holder))
			return err
		}
		return fmt.Errorf("advance role %s's kept-lane cursor to kept %d claiming %d: %w", c.role, cursor.Kept, cursor.Claiming, err)
	}
	c.seq = next
	return nil
}

// confirm writes cursor as write does, for the last write of a copy this walk claimed and then
// forwarded or sent the dead letter of, and reports whether the stream holds that write - whether
// the copy is this walk's to report - with the error that ends the walk, if any.
//
// A write the stream refused because the cursor moved (bus.ErrCursorMoved) did not land, as write
// says. Every other failure is read back once, because a publish can be stored with its answer
// lost - a time-out, or a connection that dropped between the store and the answer - and a walk
// that took such a write for one that failed would end before it reported the copy while the next
// walk starts past it, so the copy would get no count and no line. The walk's claim of the copy
// is still outstanding when it confirms, since the walk is bounded inside
// bus.KeptRoleClaimWindow, so no other walk can have moved the cursor past the copy, and the
// cursor read back says which it is:
//
//   - kept at cursor.Kept with nothing claimed is this write, or the release of a walk that
//     started from this write, claimed the copy after it and gave that claim up
//     ({Kept: cursor.Kept}, with or without that copy named forward-unconfirmed, release): no
//     walk writes that cursor unless this write landed, so either is proof of it. Its sequence
//     is the one the next write names, so confirm adopts it, logs it at debug, and reports it
//     landed with no error, and the walk goes on.
//   - kept past cursor.Kept, or at it with a claim of a later copy, is this write with another
//     walk gone on from it: it is reported landed, so the walk reports the copy, with an error
//     wrapping bus.ErrCursorMoved, which ends the walk as roleKeepLaneBusy (roleKeepWriteFailed).
//   - a read that failed, and a cursor short of cursor.Kept, which still holds this walk's claim,
//     did not land: the answer is the write's own error, and the copy is the next walk's to
//     forward or send the dead letter of again.
func (c *roleKeepLaneCursor) confirm(ctx context.Context, cursor bus.KeptRoleCursor) (bool, error) {
	err := c.write(ctx, cursor)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, bus.ErrCursorMoved) {
		return false, err
	}
	stored, readErr := c.cfg.client.ReadKeptRoleCursor(ctx, c.role)
	switch {
	case readErr != nil || stored.Cursor.Kept < cursor.Kept:
		return false, err
	case stored.Cursor.Kept == cursor.Kept && stored.Cursor.Claiming == 0:
		c.seq = stored.Sequence
		c.cfg.logger.LogEvent(slog.LevelDebug, "listener kept role drain found its cursor write stored though the stream's answer was lost",
			slog.String("role", c.role),
			slog.String("session_id", c.holder),
			slog.Uint64("kept", cursor.Kept),
			slog.String("error", err.Error()))
		return true, nil
	}
	c.cfg.logger.LogEvent(slog.LevelDebug, "listener kept role drain found its cursor write stored and another listener's walk gone on from it",
		slog.String("role", c.role),
		slog.String("session_id", c.holder),
		slog.Uint64("kept", cursor.Kept),
		slog.Uint64("stored_kept", stored.Cursor.Kept),
		slog.Uint64("claiming", stored.Cursor.Claiming),
		slog.String("error", err.Error()))
	return true, fmt.Errorf("role %s's kept-lane cursor went on from kept %d, which this walk wrote: %w", c.role, cursor.Kept, bus.ErrCursorMoved)
}

// release gives up the claim this walk holds on one copy, so the role's next walk reads that copy
// again at once: it writes the cursor the claim replaced, kept where the lane was finished and
// nothing claimed. It is the end of a walk that claimed a copy it will not forward after all, and
// answers nobody: a release the cursor refused or failed leaves a claim the next walk takes over
// once bus.KeptRoleClaimWindow has passed, so it is logged at debug and changes nothing the walk
// says.
//
// forwardUnconfirmed is the claimed copy's sequence where the stream may hold a forward of it, so
// the copy may have reached the role's holder - the claim this walk took over was a forward's, or
// this walk's own forward may have stored the copy (mayHaveStored) - and 0 where no forward of it
// can have been stored. The cursor written names it (bus.KeptRoleCursor.ForwardUnconfirmed), so
// the next walk, which reads the copy at once since nothing is claimed, still judges it as one
// under a forward's claim (keptCopyClaimOf): its dead letter or its expiry notice, when its hour
// ends or the holder refuses its mode, says that it may have reached a holder, as the claim given
// up would have made it say.
func (c *roleKeepLaneCursor) release(ctx context.Context, kept, forwardUnconfirmed uint64) {
	if err := c.write(ctx, bus.KeptRoleCursor{Kept: kept, ForwardUnconfirmed: forwardUnconfirmed}); err != nil {
		c.cfg.logger.LogEvent(slog.LevelDebug, "listener kept role drain could not release its claim",
			slog.String("role", c.role),
			slog.String("session_id", c.holder),
			slog.String("error", err.Error()))
	}
}

// roleKeepWriteFailed is what a walk ends with when a cursor write failed, which every write of
// the cursor asks the same way: a write the stream refused because the cursor moved
// (bus.ErrCursorMoved) leaves the lane to the listener whose walk took it, which is nothing wrong
// and ends this walk without an error, and every other failure ends it with the error it is.
func roleKeepWriteFailed(err error) (roleKeepDrainEnd, error) {
	if errors.Is(err, bus.ErrCursorMoved) {
		return roleKeepLaneBusy, nil
	}
	return roleKeepDrainFailed, err
}

// roleKeepGuards holds one guard per role, so two handovers of one role inside this listener do
// not walk its kept lane together and read the same copy twice. A role's guard is kept for the
// set's life, which costs one channel per role the listener it belongs to has seen claimed. One set
// is built per process (main.go), as listenerDeliveryHandlerConfig.roleKeepDrains.
//
// It also counts, per role, the handovers started in a goroutine of their own that have not yet
// returned (begin, startHandover). Such a handover takes the guard only once its goroutine runs,
// so between its start and then neither hold nor tryHold sees it; the count does, from the start
// until the goroutine returns. Nothing in the listener decides by the count: it is how a test
// tells a handover not yet begun from one that has ended, where the guard alone reads both as
// free (roleKeepGuards.idle, in the tests).
type roleKeepGuards struct {
	guards  sync.Map
	started sync.Map
}

func newRoleKeepGuards() *roleKeepGuards {
	return &roleKeepGuards{}
}

// begin counts one handover of role as started, and returns role's count, which the handover's
// goroutine lowers by one as it returns (startHandover). The count is made on the first handover
// of that role, as guard makes the guard: a load first, so a role already seen allocates nothing.
// A config built with no guards counts nothing, and begin returns nil.
func (g *roleKeepGuards) begin(role string) *atomic.Int64 {
	if g == nil {
		return nil
	}
	count, ok := g.started.Load(role)
	if !ok {
		count, _ = g.started.LoadOrStore(role, new(atomic.Int64))
	}
	started := count.(*atomic.Int64)
	started.Add(1)
	return started
}

// guard is role's own guard, made on the first handover of that role: a channel of one, which the
// handover running on this listener holds a token in.
func (g *roleKeepGuards) guard(role string) chan struct{} {
	if guard, ok := g.guards.Load(role); ok {
		return guard.(chan struct{})
	}
	guard, _ := g.guards.LoadOrStore(role, make(chan struct{}, 1))
	return guard.(chan struct{})
}

// hold takes role's guard for the whole of one handover and returns the release of it, or reports
// false when ctx ended before the guard was free. It waits for the handover of that role running
// on this listener, which is what every start that runs a handover in a goroutine of its own does
// (startHandover) - the claim route's, a registration's and a keep's: the handover ahead of
// them drains the lane, so the one behind it opens on a lane with nothing left and ends on its two
// stream reads, with no holder resolved at all. The wait ends with ctx - the handover's own
// window (roleKeepHandoverWindow) - because a handover whose window has run out walks nothing
// whatever it finds: waiting past it would queue one goroutine per keep behind a handover that is
// spending that window on a bus which is not answering.
func (g *roleKeepGuards) hold(ctx context.Context, role string) (func(), bool) {
	guard := g.guard(role)
	select {
	case guard <- struct{}{}:
		return func() { <-guard }, true
	case <-ctx.Done():
		return nil, false
	}
}

// tryHold is hold for a caller that will not wait: it reports false when a handover of role is
// already running on this listener. It is the sweep's test (sweepKeptRoleLanes), which leaves a
// lane that is being handed over to the handover holding it rather than starting a second one or
// blocking its own pass behind it.
func (g *roleKeepGuards) tryHold(role string) (func(), bool) {
	guard := g.guard(role)
	select {
	case guard <- struct{}{}:
		return func() { <-guard }, true
	default:
		return nil, false
	}
}

// roleKeepable reports whether a resolution that found no live holder for a role is one the
// listener keeps a copy for: a role nobody claims, and a claim whose holder is not live
// (roles.Lapsed: unresponsive, or gone), on the publish route and the role lane alike. No
// resolution releases a lapsed claim (dispatch://INFRA-392), and the copy kept for it still
// reaches a live holder: a released Legion session claims its role again when it comes back, and
// the role route starts a handover of the lane for a claim it grants, after the role's takeover
// passes (runTakenRoleDrain); a holder
// that answers again with its claim still standing registers and claims nothing, and the
// registration that makes it live again starts a handover of each role whose claim record its
// renewal read for the session, renewed or not, so a session a new process resumed is handed its
// copies too (subscribeHandler); that process can claim its role before it registers, and the
// role route starts a handover of every role held by a claimant it read not live before its
// refresh, whatever its answer to the claim (roleSetHandler); the role claim reaper frees a claim
// whose session is gone, so the role's next claim starts one; and the sweep hands the lane over to
// a holder that reads live again (sweepKeptRoleLanes). No walk forwards a copy past its hour
// (roleKeepWindow): a copy no live holder took in that hour is dropped, and its sender is sent a
// dead letter (judgeKeptRoleCopy).
// The publish route's 202 names the lapsed holder and its state (roleSend.kept), so its sender
// still learns what the 404 holder_lapsed told it.
//
// The rule is roleKeptReason's, which also names the reason the role lane keeps under; this is its
// answer for a caller that names no reason: the publish route, whose 202 names the resolution's
// own state (roleSend.noHolder, roleSend.kept), and the sweep (handOverSweptLane).
func roleKeepable(result roles.Resolution) bool {
	_, keepable := roleKeptReason(result)
	return keepable
}

// roleKeptReason is the reason the role lane keeps a message under for result, a resolution that
// found no live holder for the role, and whether the listener keeps for it at all (roleKeepable):
// roleKeptUnclaimed for a role nobody claims, roleKeptLapsed for a claim whose holder is not live,
// and false for every other state, so a state the listener does not keep for never becomes a
// reason. It is the one conversion of a resolution's state to a roleKeepReason
// (resolveCoreRoleHolder).
func roleKeptReason(result roles.Resolution) (roleKeepReason, bool) {
	switch result.State {
	case roles.Unclaimed:
		return roleKeptUnclaimed, true
	case roles.Lapsed:
		return roleKeptLapsed, true
	}
	return "", false
}

// keptRoleCopy is what a keep left on a role's kept lane for a message (keepRoleMessage): the
// copy the stream holds for it, which is the one the keep's answer and lines name.
type keptRoleCopy struct {
	// item is the envelope the stream holds: the one this keep stored, or, for a repeat of a
	// dedupe key whose copy is still on the lane inside its hour, that earlier copy's own, with
	// its own event id.
	item contracts.Envelope
	// until is when the copy's hour ends, in milliseconds since the epoch: keptCopyHourEnd of the
	// time the stream stored it, the end at which the walk drops it (keptCopyEnded).
	until int64
	// sequence is the copy's stream sequence on the role's kept subject.
	sequence uint64
	// repeat is whether the keep stored nothing new because the lane already held this
	// message's copy inside its hour (storeKeptRoleCopy), which the publish route's sender limit
	// charges nothing for (roleSend.kept). A copy the keep's own earlier send may have stored is
	// no repeat.
	repeat bool
}

// keptRolePublish is the stored publish a keep makes (storeKeptRoleCopy):
// bus.Client.PublishStoredAck in the listener, and a function of the test's own where a test
// stands in for an answer of the stream's.
type keptRolePublish func(ctx context.Context, subject string, item contracts.Envelope, msgID string) (bus.StoredAck, error)

// keepRoleMessage stores item on role's kept lane, the copy the role's next live holder is handed
// (drainKeptRole), within roleKeepPublishWindow, and returns what the lane holds for it, or the
// keep's failure (storeKeptRoleCopy), which a caller answering a sender reads: a keep whose publish
// went unacknowledged may have stored the copy (bus.ErrUnacknowledged). A keep that failed is
// logged here, the one failure line both give-up paths write (the publish route's noHolder and the
// role lane's keepRoleLaneMessage), so neither carries a copy of it.
func keepRoleMessage(cfg listenerDeliveryHandlerConfig, role string, item contracts.Envelope) (keptRoleCopy, error) {
	ctx, cancel := context.WithTimeout(context.Background(), roleKeepPublishWindow)
	defer cancel()
	kept, err := storeKeptRoleCopy(ctx, cfg, role, item, cfg.keepPublish)
	if err != nil {
		cfg.logger.Error(roleKeepFailedLine,
			slog.String("role", role),
			slog.String("event_id", item.EventID),
			slog.String("error", err.Error()))
		return keptRoleCopy{}, err
	}
	return kept, nil
}

// storeKeptRoleCopy is the keep itself, within ctx, through publish.
//
// It publishes under bus.KeptMsgID, the one id every listener that keeps this message writes it
// under: each takes the same raw role publish on its own role-lane subscription, so several may
// find the role undeliverable and keep it, and the stream's duplicate window collapses their
// copies into the single stored one; Dispatch's outbox keeps under that id too. The envelope is
// stored as it is, so its topic stays the role topic it was published on and the next holder is
// handed the message as its sender sent it.
//
// The stream remembers a message id for 72 hours and a copy's hour is one, so a duplicate is read
// before it is answered (heldKeptRoleCopy). A copy that is still on the lane and inside its hour is
// the answer, and nothing new is stored. A copy that has ended is followed by a new one, under
// bus.KeptMsgID + ":after:" + that copy's sequence, whose own duplicate is judged the same way, for
// as many links as it takes: every listener makes the same chain, because each link is named by
// the sequence the stream gave. A clustered stream's refusal of an id still in process under
// another publish (bus.ErrDuplicateInProcess) is published again until the answer names the copy
// that publish stored (publishPastInProcess). Every other failure, of a publish or of a read, and
// a chain that runs out of ctx, is a keep that failed.
//
// The copy a duplicate names is a repeat only when no send of this keep under the same id may
// have stored it: a duplicate that comes back Resent, which answers the bus's resend after a
// closed connection or a publish made again after an in-process refusal of a send nothing
// answered (publishPastInProcess), may name the copy this keep's own first send stored.
//
// A new copy's hour counts from its store time, read back at its acknowledged sequence, which is
// what the walk measures its age against. A read that fails counts it from the listener's clock
// just before the publish, logged at debug: the store time is no earlier than that, so the answer
// never promises more than the walk keeps.
func storeKeptRoleCopy(ctx context.Context, cfg listenerDeliveryHandlerConfig, role string, item contracts.Envelope, publish keptRolePublish) (keptRoleCopy, error) {
	subject := bus.KeptRoleSubject(role)
	first := bus.KeptMsgID(item.DedupeKey, role)
	msgID := first
	for {
		published := cfg.now()
		ack, err := publishPastInProcess(ctx, func() (bus.StoredAck, error) { return publish(ctx, subject, item, msgID) })
		if err != nil {
			return keptRoleCopy{}, fmt.Errorf("keep the role %s message on its kept lane under %s: %w", role, msgID, err)
		}
		if !ack.Duplicate {
			return keptRoleCopy{item: item, until: keptRoleCopyUntil(ctx, cfg, role, item, ack.Sequence, published), sequence: ack.Sequence}, nil
		}
		// A duplicate that came back Resent may name the copy a send of this keep under msgID
		// stored, so a read of it that fails says so, and a copy still inside its hour is this
		// keep's own rather than a repeat.
		held, inside, err := heldKeptRoleCopy(ctx, cfg, role, ack.Sequence)
		if err != nil {
			err = fmt.Errorf("keep the role %s message under %s: %w", role, msgID, err)
			if ack.Resent && !mayHaveStored(err) {
				err = fmt.Errorf("%w: %w", bus.ErrUnacknowledged, err)
			}
			return keptRoleCopy{}, err
		}
		if inside {
			held.repeat = !ack.Resent
			return held, nil
		}
		msgID = first + ":after:" + strconv.FormatUint(ack.Sequence, 10)
	}
}

// keptRoleCopyUntil is when the hour of the copy a keep has just stored at seq on role's kept lane
// ends (keptCopyHourEnd): its store time, read back at that sequence, plus roleKeepWindow. A read
// that fails, or answers another sequence, counts the hour from published, the listener's clock
// just before the publish, which is no later than the store time.
func keptRoleCopyUntil(ctx context.Context, cfg listenerDeliveryHandlerConfig, role string, item contracts.Envelope, seq uint64, published time.Time) int64 {
	stored, err := cfg.client.NextStoredMessage(ctx, bus.KeptRoleSubject(role), seq)
	if err == nil && stored.Sequence != seq {
		err = fmt.Errorf("the lane's next copy from %d is at %d", seq, stored.Sequence)
	}
	if err != nil {
		cfg.logger.LogEvent(slog.LevelDebug, "listener kept role message's store time could not be read back, so its hour counts from before its publish",
			slog.String("role", role),
			slog.String("event_id", item.EventID),
			slog.Uint64("sequence", seq),
			slog.String("error", err.Error()))
		return keptCopyHourEnd(published).UnixMilli()
	}
	return keptCopyHourEnd(stored.Time).UnixMilli()
}

// heldKeptRoleCopy judges the copy the stream holds at seq on role's kept lane, which a keep's
// publish was answered a duplicate of, and returns it when it is still on the lane and inside its
// hour: its sequence is past the lane cursor's Kept, and its hour has not ended by the listener's
// clock (cfg.now) against its store time (keptCopyEnded), the walk's own test of a copy it may
// still forward (judgeKeptRoleCopy). A copy a walk has claimed is judged by the same two tests.
// Every other copy has ended and reports false: one past its hour, one the cursor has moved past,
// one the stream no longer holds at seq (the read answers another sequence, or none), and one whose
// role no read can tell (readRoleFrame), which the walk ends. A copy is read field by field, as the
// walk reads it (roleFrame.envelope), so a copy the walk forwards is held here too, an expects_reply
// word this build does not know among it. A read that did not answer is its error.
func heldKeptRoleCopy(ctx context.Context, cfg listenerDeliveryHandlerConfig, role string, seq uint64) (keptRoleCopy, bool, error) {
	stored, err := cfg.client.NextStoredMessage(ctx, bus.KeptRoleSubject(role), seq)
	if errors.Is(err, nats.ErrMsgNotFound) {
		return keptRoleCopy{}, false, nil
	}
	if err != nil {
		return keptRoleCopy{}, false, fmt.Errorf("read role %s's kept copy at %d: %w", role, seq, err)
	}
	if stored.Sequence != seq {
		return keptRoleCopy{}, false, nil
	}
	if keptCopyEnded(cfg.now(), stored.Time) {
		return keptRoleCopy{}, false, nil
	}
	frame, err := readRoleFrame(stored.Data)
	if err != nil {
		return keptRoleCopy{}, false, nil
	}
	item := frame.envelope()
	cursor, err := cfg.client.ReadKeptRoleCursor(ctx, role)
	if err != nil {
		return keptRoleCopy{}, false, err
	}
	if seq <= cursor.Cursor.Kept {
		return keptRoleCopy{}, false, nil
	}
	return keptRoleCopy{item: item, until: keptCopyHourEnd(stored.Time).UnixMilli(), sequence: seq}, true, nil
}

// keepForNextHolder keeps item for role's next live holder and hands the lane to whoever holds the
// role now (runKeptRoleDrain), and returns what it kept and whether it kept anything
// (keepRoleMessage). It is the one keep both give-up paths take - the publish route's
// (roleSend.noHolder) and the role lane's (keepRoleLaneMessage) - so no give-up path can store a
// copy without offering it to the role's current holder: a holder whose claim landed, or who
// answered again, while the keep's caller was resolving the holder it did not find. The handover
// resolves that holder itself and, when the role has no live holder, walks the lane with no holder
// and stops at the copy it finds still inside its hour, so a keep for a role nobody holds live
// costs one resolution and one read of the lane past its cursor.
//
// One message is not kept at all, and leaves its caller's give-up exactly as it was before the
// kept lane: one whose role no claim can be keyed by (roles.ValidClaimKey: a role token of the
// pattern POST /v1/roles/set accepts, under the project a free-form role's key names), which both
// give-up paths must agree on, and Dispatch's outbox with them (keptOrSkipped,
// internal/dispatch/outbox/role.go), since a copy kept under such a token waits for a claim that
// cannot be made and expires unread.
//
// A sender that keeps a queue of its own and redelivers from it - the Legion daemon's notice
// queue - opts out on the publish route's own request field instead (keep false, read by
// roleSend.noHolder), which is the one give-up a sender can address: nothing publishes a raw role
// message it would redeliver, so the role lane always keeps.
//
// An edge listener keeps nothing (cfg.edge) and answers errEdgeKeepsNothing: its grant publishes
// no kept role lane (natsgrant.Edge), so its give-up is the one a keep that kept nothing leaves,
// the publish route's 404 with its refusal's exception.
func keepForNextHolder(cfg listenerDeliveryHandlerConfig, role string, item contracts.Envelope) (keptRoleCopy, error) {
	if !roles.ValidClaimKey(role) {
		return keptRoleCopy{}, errRoleNotKeepable
	}
	if cfg.edge != nil {
		return keptRoleCopy{}, errEdgeKeepsNothing
	}
	kept, err := keepRoleMessage(cfg, role, item)
	if err != nil {
		return keptRoleCopy{}, err
	}
	startHandover(cfg, role, runKeptRoleDrain)
	return kept, nil
}

// errRoleNotKeepable is keepForNextHolder's answer for a role no claim can be keyed by
// (roles.ValidClaimKey), which it keeps nothing for.
var errRoleNotKeepable = errors.New("no claim can be keyed by this role, so nothing is kept for it")

// errEdgeKeepsNothing is keepForNextHolder's answer at an edge listener, which keeps nothing: its
// grant publishes no kept role lane (natsgrant.Edge).
var errEdgeKeepsNothing = errors.New("an edge listener keeps no role message: its grant publishes no kept role lane")

// keepRoleLaneMessage keeps the role lane's copy of a message it found no live holder for, or
// whose holder it could not read, and reports whether it was kept. reason is why, as the lane's
// kept line names it: roleKeptUnclaimed for a role nobody claims, roleKeptLapsed for a claim whose
// holder is not live, and roleClaimUnread for a claim read that did not answer. holder is the
// session the claim names, which the kept line names as its session_id: the lapsed holder, and
// empty for the other two, since an unclaimed role has none and a read that failed found none.
// Kept, it writes the lane's line, which names the copy the lane holds, its stream sequence and
// when its hour ends (keptRoleCopy), counts the delivery under roleKeptDeliveryStatus and
// publishes no delivery exception: the message is not lost, and the handover the keep starts
// forwards it once a resolution finds the role's holder live (keepForNextHolder). A keep that kept
// nothing is reported false, and the caller's own give-up, its exception, its metric and its line,
// stands exactly as it was.
func keepRoleLaneMessage(cfg listenerDeliveryHandlerConfig, role string, item contracts.Envelope, reason roleKeepReason, holder string) bool {
	kept, err := keepForNextHolder(cfg, role, item)
	if err != nil {
		return false
	}
	applyDeliveryOutcome(cfg, item, deliveryOutcome{
		metricStatus: roleKeptDeliveryStatus,
		log: func(logger *logging.Logger) {
			logger.DeliveryLog(slog.LevelInfo, "listener kept role message for the role's next holder",
				holder, item.Topic, kept.item.EventID, roleKeptDeliveryStatus,
				slog.String("role", role),
				slog.String("reason", string(reason)),
				slog.Uint64("sequence", kept.sequence),
				slog.Int64("kept_until", kept.until))
		},
	})
	return true
}

// runKeptRoleDrain hands role's kept lane over to whoever holds the role, under a context of its
// own (roleKeepHandoverWindow), and logs a handover that failed. It is what the starts of a
// handover run in a goroutine of their own (startHandover) - the claim route's for each role of a
// claimant its refresh makes live again (roleSetHandler; the claimed role's own, when the claim
// lands, runs runTakenRoleDrain, which runs the role's takeover passes first), the
// registration's that makes a holder live again (subscribeHandler), the keep's
// (keepForNextHolder) and the session relay's once it copied a frame of a role another session
// holds (sessionRelays.takeOverFrame) - since no route's answer, keep's caller or relay waits for
// a stream read or a forward. The sweep starts its handovers itself, under the guard it already
// took (sweepKeptRoleLanes). A copy's age is measured by the listener's clock (cfg.now).
func runKeptRoleDrain(cfg listenerDeliveryHandlerConfig, role string) {
	ctx, cancel := context.WithTimeout(context.Background(), roleKeepHandoverWindow)
	defer cancel()
	reportKeptRoleDrain(cfg, role, drainKeptRole(ctx, cfg, role, cfg.now))
}

// startHandover runs run, runKeptRoleDrain or runTakenRoleDrain, for role in a goroutine of its
// own, counted on cfg's guards as started from before that goroutine exists until it returns
// (roleKeepGuards.begin). Every start of a handover that does not wait for it goes through here.
func startHandover(cfg listenerDeliveryHandlerConfig, role string, run func(listenerDeliveryHandlerConfig, string)) {
	started := cfg.roleKeepDrains.begin(role)
	go func() {
		if started != nil {
			defer started.Add(-1)
		}
		run(cfg, role)
	}()
}

// reportKeptRoleDrain logs a handover that failed: the one line every start of a handover writes,
// the claim route's, the registration's and the keep's (runKeptRoleDrain) and the sweep's
// (sweepKeptRoleLanes).
func reportKeptRoleDrain(cfg listenerDeliveryHandlerConfig, role string, err error) {
	if err == nil {
		return
	}
	cfg.logger.Error("listener kept role drain failed",
		slog.String("role", role),
		slog.String("error", err.Error()))
}

// roleKeepDrainEnd is what ended one walk of a role's kept lane (walkKeptRole), which decides
// whether the handover walks it again and after how long (the switch in handOverKeptRole).
type roleKeepDrainEnd int

const (
	// roleKeepLaneDrained: every copy the lane held was forwarded or ended. The handover is
	// finished.
	roleKeepLaneDrained roleKeepDrainEnd = iota
	// roleKeepWorkLeft: the walk's own bounds ended it (roleKeepDrainMax forwards, or
	// roleKeepScanMax copies read), with copies behind them.
	roleKeepWorkLeft
	// roleKeepLaneBusy: another listener's walk has the lane - its claim of a copy is younger
	// than bus.KeptRoleClaimWindow, or it took the cursor from under this one.
	roleKeepLaneBusy
	// roleKeepHolderMoved: the role's holder is no longer the session this walk forwards to.
	roleKeepHolderMoved
	// roleKeepCopyWaits: a walk with no live holder reached a copy still inside its hour, which
	// waits on the lane for the role's next live holder. The handover is finished, as a drained
	// lane's is, unless the resolution behind that walk did not answer (handOverKeptRole).
	roleKeepCopyWaits
	// roleKeepDrainFailed: a read, a cursor write, a forward or a dead letter's publish failed,
	// and the walk returned the error with it.
	roleKeepDrainFailed
)

// roleKeepDrainResult is what one walk of a role's kept lane did: what ended it, how many copies
// it forwarded, and how many it ended without forwarding - one past roleKeepWindow, with a dead
// letter or with none, one no read can decode, one beforeForward refused. Either number above zero
// is the progress the handover's bound on walks that made none counts (roleKeepDrainIdleRounds),
// because both leave the copies behind them nearer the role's holder.
type roleKeepDrainResult struct {
	end       roleKeepDrainEnd
	forwarded int
	ended     int
	// waiting is the stream sequence of the copy a walk with no live holder stopped at
	// (roleKeepCopyWaits), 0 for every other end: the first copy of the lane still inside its
	// hour, from which the sweep reads for a copy that wakes the role (raiseKeptRoleWake).
	waiting uint64
	// unconfirmed is a copy this walk forwarded, or sent the dead letter of, and could not confirm
	// (roleKeepLaneCursor.confirm), which the next walk forwards or sends again, so this walk's
	// failure is worth reporting however the walk ended (handOverKeptRole).
	unconfirmed bool
}

// roleKeepDrainBackoff is the pause a handover waits after a failure it retries, a walk's or a
// read's (handOverKeptRole): roleKeepDrainRetryPause for the first of a run, doubling to
// roleKeepDrainRetryCap, where last is the pause this handover waited before the failure it has
// just met and 0 is a handover that has not failed yet or has made progress since. A run of
// failures costs the bus less the longer it lasts, and ends with roleKeepHandoverWindow.
func roleKeepDrainBackoff(last time.Duration) time.Duration {
	if last <= 0 {
		return roleKeepDrainRetryPause
	}
	return min(2*last, roleKeepDrainRetryCap)
}

// drainKeptRole hands role's kept lane to the session that holds the role. It is the whole of a
// handover under role's own guard: runKeptRoleDrain and runTakenRoleDrain are the goroutines its
// starts run it in (the claim route, the registration route, the keep and the session relay), and
// tests call it directly for one role's lane. How the handover ended is the sweep's to read
// (keptRoleHandover), and no caller of this one reads it.
//
// The guard is held for the handover rather than for each of its walks, so two handovers of one
// role inside this listener run one after the other and the sweep can tell that a role's lane is
// being handed over here already without waiting for it (roleKeepGuards.tryHold). A handover
// whose context ended while it waited gives that wait up and walks nothing (roleKeepGuards.hold).
//
// An edge listener hands no lane over (cfg.edge): a handover writes the lane's drain cursor, which
// its grant may not publish (natsgrant.Edge). The shared listeners' sweeps hand the lane of a role
// held at the edge to its holder's agent subject (startKeptRoleSweep), as they hand every lane.
func drainKeptRole(ctx context.Context, cfg listenerDeliveryHandlerConfig, role string, now func() time.Time) error {
	if cfg.edge != nil {
		return nil
	}
	release, taken := cfg.roleKeepDrains.hold(ctx, role)
	if !taken {
		return nil
	}
	defer release()
	_, err := handOverKeptRole(ctx, cfg, role, now)
	return err
}

// keptRoleHandover is how one handover of a role's kept lane ended (handOverKeptRole): what its
// last pass found, which the sweep reads to decide whether the role's wake report is raised again
// (sweepKeptRoleLanes).
type keptRoleHandover struct {
	// resolved is the state the last pass's resolution of the role's holder found, empty when
	// that pass resolved nothing: its lane held nothing left, or the resolution did not answer.
	resolved roles.State
	// waiting is the stream sequence of the copy the last pass's walk with no live holder stopped
	// at (roleKeepDrainResult.waiting), 0 when that walk stopped at none.
	waiting uint64
}

// handOverKeptRole is the handover itself, for a caller holding role's guard (drainKeptRole,
// sweepKeptRoleLanes), and returns how it ended with the error it reports.
//
// Each pass opens by reading the role's kept lane and its drain cursor (readKeptLaneHead) within
// roleKeepDrainWindow, resolves the role's holder as every other read of this build does
// (roles.ResolveHolder), with the claim and the holder's session record read from the buckets'
// stream leaders (leaderRoleClaims, leaderHolderSessions), within roleKeepHolderWindow only when
// that lane holds copies its cursor has not reached, and walks the lane for the holder it found,
// handing that walk the head it read (walkKeptRole). The lane is read before the holder, so a pass
// that finds the lane empty ends the handover with no resolution at all: every claim the claim
// route grants starts a handover, as do that route's refresh of a claimant it read not live and
// every registration that makes a holder live again, for each role of that session, and most roles
// have nothing kept for them, which would otherwise cost each of those starts a claim read and a
// session read, made from a goroutine its own answer has already outlived. The resolution is taken
// after the pause the walk before it earned rather than before it: a holder resolved before that
// pause can have released the role while it ran, and a walk started for it would find that out
// only at its first read of the role's claim record (roleKeepDrainHolds), ending as a walk that
// forwarded nothing and spending one of the handover's idle walks.
//
// A resolution that finds no live holder - roles.Unclaimed, or roles.Lapsed - walks the lane with
// no holder instead: the walk ends each copy whose hour ended, and a copy under a dead-letter claim
// it takes over, and stops at the first other copy, which is inside its hour and waits for the
// role's next live holder (roleKeepCopyWaits). That stop ends the handover, as a drained lane does,
// which is what the keep that stored a copy for a role nobody holds wants: the walk reads the copy
// it kept and stops there. So a copy ends at the end of its hour whether or not any session holds
// the role, and the sweep reads where the walk stopped to decide whether the role's wake report is
// raised again (keptRoleHandover).
//
// One walk of the lane is bounded - roleKeepDrainMax forwards, roleKeepScanMax copies read,
// roleKeepDrainWindow of time - and nothing but a claim the claim route grants, that route's
// refresh of a claimant that was not live, a keep, a registration that makes a holder live again
// or a sweep ever starts a handover. A keep happens only while a role has no live holder, a live
// holder claims no second time and its heartbeats start no handover, so every bound a walk
// stopped on used to strand the copies behind it on the lane until they aged out unread, with
// every one of their senders answered 202 kept. The handover walks the lane again instead, for
// the role's holder as it is then, until the lane is drained, a walk with no holder stops at a
// copy inside its hour, or the walks stop making progress (roleKeepDrainIdleRounds). What ended
// the walk says how long it waits first: a drained lane, and a copy a walk with no holder stopped
// at, end the handover; a walk the budget or the scan ended is followed at once, since the copies
// behind it have senders answered 202 kept; one that found the lane busy waits the claim out,
// since the walk holding it can still be forwarding; and one whose holder moved is followed at
// once, for the holder the next pass resolves.
//
// A walk that failed is not one that made no progress: it is waited out on a widening cadence
// (roleKeepDrainBackoff) and its error is carried out of the handover. A walk or a read the
// server refused under the listener's grant (bus.ErrPublishDenied) is the exception: the server
// refuses it again however often it is sent, so the handover ends with the walk or the read that
// met the refusal, carrying it out, rather than holding the role's guard until its window ends,
// and the lane is left to the role's next claim and to the sweep. A refused cursor write is named
// only when its publish's own wait ends (sendStored), so that walk still costs that wait. A walk
// whose read of the role's claim record did not answer is a walk that failed (roleKeepDrainHolds),
// so the pass after its pause resolves the holder again rather than forwarding on a claim no read
// answered. A read that did not answer - the lane head's, on whichever pass it was made, or the
// holder's resolution behind one - says nothing about the holder, so the role's claim record
// decides whether the handover reads again (retry, readRoleClaimHolder): a record naming a
// session, live or lapsed, is a lane
// whose copies have a holder waiting, so the read is waited out the same way, on any pass
// including the first, and its failure is carried out too, which lifts a handover that ends still
// failing above debug; a role no record names - and a record that could not be read either, which
// shows no holder waiting - is the keep's own case, a copy kept for a role nobody holds or for a
// claim no read answered, so the handover ends with nothing to report, leaving the lane to the
// role's next claim and to the sweep (sweepKeptRoleLanes), and says so at WARN, naming the read
// that failed - one that did not answer, or answered something that does not decode - the window
// it was given and its error. A resolution that failed still walks the lane with no holder before
// the record decides, since a copy whose hour ended needs no holder to end; that walk ends the
// handover only where it drained the lane, and there it writes a WARN line naming the resolution
// that failed, since no read of the record follows to say why the walk had no holder. That record
// is read under roleHolderReadTimeout and not under ctx, since a handover whose window ended inside
// a read still decides by the record whether that read's failure is carried out; the listener's
// shutdown, which cancels ctx, is asked before the read and after it.
//
// The one failure carried out of nothing is a read's, or a walk's that forwarded nothing it did
// not confirm, that this handover's own cancellation ended: the listener is shutting the sweep's
// handovers down (sweepKeptRoleLanes), where an Error line would raise one on every deploy. A
// deadline is not a shutdown, and this window usually ends inside a read, whose failure it carries
// out as any other pass does.
//
// It returns the error of its last walk, the walk with no holder included, or, while that walk has
// not failed, that of the read it was still retrying when its time ran out, so either is logged
// where every failed handover is (reportKeptRoleDrain): a pass that reads again drops the read
// before it. ctx - roleKeepHandoverWindow in the listener - bounds the whole handover, pauses
// included.
//
// now is the clock a copy's age is measured against, the listener's own (cfg.now, which main.go
// sets to time.Now) wherever the claim route, a registration, a keep or the sweep starts the
// handover: a test moves it rather than keeping a copy for an hour, since the age is taken from
// the stream's own store time, which no caller may choose. An outstanding claim's age is measured
// the same way, against the time the stream stored the cursor that carries it.
func handOverKeptRole(ctx context.Context, cfg listenerDeliveryHandlerConfig, role string, now func() time.Time) (keptRoleHandover, error) {
	var (
		err, readErr   error
		idle           int
		backoff, pause time.Duration
		// outcome is what the pass under way has found, which the handover returns at its end.
		outcome keptRoleHandover
	)
	// ended is what this handover reports at every exit, by the rule above.
	ended := func() (keptRoleHandover, error) {
		if err != nil {
			return outcome, err
		}
		return outcome, readErr
	}
	// waitOut earns the next of this handover's widening pauses (roleKeepDrainBackoff).
	waitOut := func() {
		backoff = roleKeepDrainBackoff(backoff)
		pause = backoff
	}
	// shutDown reports whether the listener's shutdown, and not a deadline, ended ctx.
	shutDown := func() bool { return errors.Is(ctx.Err(), context.Canceled) }
	// retry is what a pass does with a read that failed - it did not answer, or answered something
	// that does not decode: it writes line for that read and reports whether this handover reads
	// again, which the rule above decides. Where it does not read again because no claim record
	// names a holder, the handover returns no error of that read's (ended), so that exit logs the
	// read at WARN: the role's next claim and the sweep then take the lane over with nothing else to
	// say why. bound is the window the read was given, not how long it took: ctx, or a holder
	// session read's roleHolderReadTimeout, can end it sooner, and a read that does not decode ends
	// at once. A lane whose cursor does not decode, which any publisher on notifications.> can
	// write, has this WARN written at every handover of a role no record names, the one each keep
	// for that role starts included.
	retry := func(line string, bound time.Duration, cause error) bool {
		cfg.logger.LogEvent(slog.LevelDebug, line,
			slog.String("role", role),
			slog.String("error", cause.Error()))
		if errors.Is(cause, bus.ErrPublishDenied) {
			readErr = cause
			return false
		}
		if shutDown() {
			return false
		}
		claimed, claimErr := readRoleClaimHolder(context.WithoutCancel(ctx), cfg, role)
		if shutDown() {
			return false
		}
		if claimErr == nil && claimed != "" {
			readErr = cause
			waitOut()
			return true
		}
		cfg.logger.LogEvent(slog.LevelWarn, "listener kept role drain left the lane to the role's next claim",
			slog.String("role", role),
			slog.String("read", line),
			slog.Duration("bound", bound),
			slog.String("error", cause.Error()),
			slog.Bool("claim_read_failed", claimErr != nil))
		return false
	}
	// unresolved is the line a pass writes for a resolution of the role's holder that failed
	// (retry), which the WARN of a walk with no holder that drained the lane names too.
	const unresolved = "listener kept role message could not resolve the role's holder"
	for {
		if ctx.Err() != nil {
			return ended()
		}
		if pause > 0 && waitUnlessDone(ctx, pause) != nil {
			return ended()
		}
		// Past that pause, the read before this pass is behind this handover, answered or not,
		// and so is what the pass before this one found.
		readErr, outcome = nil, keptRoleHandover{}
		headCtx, cancelHead := context.WithTimeout(ctx, roleKeepDrainWindow)
		head, headErr := readKeptLaneHead(headCtx, cfg.client, role)
		cancelHead()
		if headErr != nil {
			if !retry("listener kept role message could not read the role's kept lane", roleKeepDrainWindow, headErr) {
				return ended()
			}
			continue
		}
		if !head.left {
			return ended()
		}
		holderCtx, cancelHolder := context.WithTimeout(ctx, roleKeepHolderWindow)
		resolved, holderErr := roles.ResolveHolder(holderCtx, leaderRoleClaims{cfg.registry}, leaderHolderSessions{cfg.sessions}, role)
		cancelHolder()
		// The walk forwards to a live holder alone, and walks with no holder for every other
		// resolution, one that did not answer included.
		var (
			holder string
			entry  session.SessionEntry
		)
		if holderErr == nil {
			outcome.resolved = resolved.State
			if resolved.State == roles.Live {
				holder, entry = resolved.Holder, resolved.Entry
			}
		}
		var result roleKeepDrainResult
		result, err = walkKeptRole(ctx, cfg, role, holder, entry, head, now)
		outcome.waiting = result.waiting
		// A walk the listener's shutdown cancelled is as quiet as a read it cancelled (retry),
		// unless it left a copy it forwarded or sent the dead letter of unconfirmed, which the
		// next walk forwards or sends again.
		if err != nil && !result.unconfirmed && shutDown() {
			return outcome, nil
		}
		// A walk the grant refused ends the handover whatever else this pass found, a holder
		// resolution that did not answer included: the grant refuses the walk again however often
		// the lane is walked.
		if errors.Is(err, bus.ErrPublishDenied) {
			return ended()
		}
		progressed := result.forwarded > 0 || result.ended > 0
		if progressed {
			idle, backoff = 0, 0
		}
		if holderErr != nil {
			if result.end != roleKeepLaneDrained {
				if !retry(unresolved, roleKeepHolderWindow, holderErr) {
					return ended()
				}
				continue
			}
			// A walk with no holder that drained the lane ends the handover with no read of the
			// role's claim record (retry), so this line is the one that says why it had no holder:
			// the dead letter it sent for a copy whose hour ended can say that no session held the
			// role while one did.
			cfg.logger.LogEvent(slog.LevelWarn, "listener kept role drain walked the role's copies with no holder because the holder lookup failed",
				slog.String("role", role),
				slog.String("read", unresolved),
				slog.Duration("bound", roleKeepHolderWindow),
				slog.String("error", holderErr.Error()))
		}
		switch result.end {
		case roleKeepLaneDrained, roleKeepCopyWaits:
			return ended()
		case roleKeepDrainFailed:
			waitOut()
			continue
		case roleKeepLaneBusy:
			pause = bus.KeptRoleClaimWindow
		case roleKeepWorkLeft, roleKeepHolderMoved:
			pause = 0
		}
		if progressed {
			continue
		}
		if idle++; idle >= roleKeepDrainIdleRounds {
			cfg.logger.LogEvent(slog.LevelDebug, "listener kept role drain gave the lane up with copies left on it",
				slog.String("role", role),
				slog.String("session_id", holder),
				slog.Int("idle_walks", idle))
			return ended()
		}
	}
}

// keptLaneHead is the opening read of a role's kept lane: what the stream holds on it, where the
// role's drain cursor stands, and whether there is anything left for a walk to do
// (readKeptLaneHead).
type keptLaneHead struct {
	// last is the stream sequence of the newest copy on the lane, 0 when the stream holds none.
	last uint64
	// cursorSeq is the stream sequence of the cursor message itself, which the walk's next
	// compare-and-swap write of the cursor must name (roleKeepLaneCursor.write); 0 for a lane
	// whose cursor has never been written.
	cursorSeq uint64
	// cursor is the drain cursor's body, its zero value for a lane that has never been drained.
	cursor bus.KeptRoleCursor
	// written is the time the stream stored the cursor, which is how old the claim it carries
	// is; the zero time when there is no cursor.
	written time.Time
	// left reports copies the cursor has not reached: the one test of whether a walk of this
	// lane has anything to do.
	left bool
}

// readKeptLaneHead reads role's kept lane and its drain cursor: the one pair of reads a pass of
// the handover makes of that lane, which answers "anything left on it?" and carries the cursor
// to the walk the pass starts (handOverKeptRole, walkKeptRole), and the pair the sweep makes to
// decide whether a lane is worth a handover at all (sweepKeptRoleLanes).
//
// The lane is read first, and a role nothing was ever kept for ends here, on one stream read
// rather than two (nats.ErrMsgNotFound): the cursor says nothing a lane with no message can act
// on. A lane the cursor has already been advanced past ends on the second read, as a
// cursor-first read would. A role whose lane has never been drained holds no cursor, and comes
// back the zero keptLaneHead its fields describe.
//
// The cursor is read through bus.Client.ReadKeptRoleCursor, which Dispatch's outbox reads it
// through as well (internal/dispatch/outbox/role.go, keptCopyLeadsTheLane), so the two writers
// of a role's kept lane decode one body one way and neither carries a second copy of it.
func readKeptLaneHead(ctx context.Context, client *bus.Client, role string) (keptLaneHead, error) {
	last, err := client.LastStoredMessage(ctx, bus.KeptRoleSubject(role))
	if errors.Is(err, nats.ErrMsgNotFound) {
		return keptLaneHead{}, nil
	}
	if err != nil {
		return keptLaneHead{}, fmt.Errorf("read role %s's kept lane: %w", role, err)
	}
	stored, err := client.ReadKeptRoleCursor(ctx, role)
	if err != nil {
		return keptLaneHead{}, err
	}
	return keptLaneHead{
		last:      last.Sequence,
		cursorSeq: stored.Sequence,
		cursor:    stored.Cursor,
		written:   stored.Written,
		left:      stored.Cursor.Kept < last.Sequence,
	}, nil
}

// walkKeptRole is one walk of role's kept lane for holder, the role's live holder, or with no
// holder when holder is empty: the walk a pass makes when its resolution found no live holder or
// did not answer (handOverKeptRole). It walks the copies stored after the cursor of head, the
// lane-head read the pass that started it made (handOverKeptRole, readKeptLaneHead) - at most
// roleKeepScanMax of them, and until roleKeepDrainMax have been forwarded - judges each one
// (judgeKeptRoleCopy), and reports what ended it and how far it moved the role's cursor, so the
// handover around it knows whether the lane has copies left and whether this walk brought them
// nearer (handOverKeptRole).
//
// A copy still inside roleKeepWindow that beforeForward allows is forwarded to the holder's agent
// subject exactly as a live role send forwards it (roleForwardDedupePrefix,
// contracts.LegacyAgentSubject, the old form every producer writes until slice C4). A copy whose
// hour ended is dropped, and so is one whose delivery mode the holder does not take; either is sent
// to its sender as a dead letter where it is owed one. A copy no read can decode, and one
// beforeForward refuses as an echo or a repeat, is passed over. A walk with no holder forwards
// nothing: it ends what needs no holder to end and stops at the first copy it cannot end, which is
// inside its hour and waits for the role's next live holder (roleKeepCopyWaits). No walk forwards a
// copy whose hour ended.
//
// It bounds itself at roleKeepDrainWindow, taken when the walk starts rather than when the
// handover around it was: the handover holds the role's guard, so no part of a walk's window is
// spent waiting for the walks of another handover of the same role, which would leave a claim of
// a copy behind for bus.KeptRoleClaimWindow by failing mid-walk.
//
// Every step the walk takes with a copy it writes the role's cursor for, under the
// compare-and-swap of the cursor's own last sequence (roleKeepLaneCursor.write), and that write is
// what decides which walk owns the copy: the loser of it stops, where a cursor written once at
// the end of the walk let two listeners forward everything they read.
//
// A copy the walk sends nothing for - one no read can decode, one past roleKeepWindow that is owed
// no dead letter, one beforeForward refuses that is owed none - is finished in one write, to
// {kept: its sequence, claiming: 0}, before its line, its counter and its delivery outcome, so a
// walk that lost that write neither counts nor logs a copy another walk reports; it is why a copy
// past the window is counted expired once (envoy_role_kept_expired_total) however many walks read
// the lane. That one write is a compare-and-swap as well, and the copy has no side effect to
// protect between two writes, so it takes no claim.
//
// A copy that will be forwarded takes three: the cursor is written to {kept: unchanged, claiming:
// its sequence}, the copy is forwarded, and the cursor is written to {kept: its sequence,
// claiming: 0}. What the claim buys is that a copy still reaches exactly one holder
// (bus.KeptRoleClaimWindow), and a walk whose own claim the cursor refused stops where it is.
// What the unmoved kept buys is that no copy is lost whatever ends the walk - a process killed,
// a forward that failed, a context that ran out - because the walk starts at kept + 1, so a copy
// claimed and never confirmed is simply the next copy a walk reads: the next one of all, where
// the walk released its claim on its way out (roleKeepLaneCursor.release), and the next one past
// bus.KeptRoleClaimWindow where nothing was left to release it. What that costs is one more copy
// when a walk ends between its forward and its confirm, which the walk reports
// (roleKeepDrainResult): the repeat carries the same stable id (bus.ForwardMsgID of the
// forward's dedupe key and the agent subject), so for the same holder the stream's duplicate
// window keeps one stored copy and the holder's own dedupe_key drops the second.
//
// A copy owed a dead letter takes three writes the same way (endKeptCopyWithDeadLetter), its
// claim naming why the copy gets one (bus.KeptRoleCursor.DeadLetter). A claim another walk left,
// and that is no longer outstanding, names the first copy after the cursor's kept, the one copy a
// walk claims, and this walk takes it over there (keptCopyClaimOf): a dead letter's claim is sent
// again under the same message id for the reason the claim names - the copy's hour ended, or the
// holder did not take its delivery mode - whatever the copy's age and whoever holds the role, and
// a forward's claim on a copy whose hour ended is owed a dead letter however old the copy is,
// since that forward may have reached the holder before the hour ended. Where a copy under a
// forward's claim is dropped with a dead letter, at the end of its hour or inside it because the
// role's live holder does not take its delivery mode, the dead letter says that the message may
// have reached the session that held the role then. The claim of that dead letter says so as
// well, whichever reason it names (keptRoleDrop.claimReason), so every walk that takes it over
// after this one sends the dead letter that says so too. A walk whose forward failed gives a
// forward's claim up without losing that either: its release names the copy as one whose forward
// may have reached the holder (bus.KeptRoleCursor.ForwardUnconfirmed) where it took over a
// forward's claim or its own forward may have stored the copy, and the next walk reads that as a
// forward's claim (keptCopyClaimOf).
//
// The last write of those three is a confirm (roleKeepLaneCursor.confirm), whether the copy was
// forwarded or ended with a dead letter. A confirm the stream stored with its answer lost is read
// back rather than taken for one that failed: a cursor that holds it is this walk's, so the walk
// reports the copy and goes on from it, or, where another walk has gone on from it already,
// reports the copy and ends as one that found the lane busy. Only a confirm the cursor does not
// hold leaves the copy to the next walk, unreported here, which forwards it or sends its dead
// letter again.
//
// Before each copy a walk for a holder reads the role's claim record and goes on only while that
// record names the session it forwards to (roleKeepDrainHolds): this walk forwards to the session
// the pass that started it resolved, so without that it would spend its budget on a session that
// no longer holds the role. A record that names another session or nobody leaves the lane to the
// role's next holder, whose copies the next walk forwards, and a read that did not answer ends the
// walk as a failed one, which the handover waits out before it resolves the holder again. A walk
// with no holder reads no record: it forwards to nobody, and what it ends is ended whoever holds
// the role.
//
// A forward's whole answer is the stream's acknowledgement that it stored the copy on the
// holder's agent subject (startRoleForward), which the holder's own session consumer delivers:
// no holder answers a receipt, so a forward is acknowledged or it failed, and there is no third
// outcome the walk goes on past. A forward that failed ends the walk and is returned, with its
// copy released for the role's next walk and the rest of the lane behind it. A cursor write the
// stream refused because another listener's walk has the lane
// (bus.ErrCursorMoved) is nothing wrong: that listener owns this role's lane now, and this walk
// stops. A read or a write the listener's shutdown cancelled ends the walk with its error all
// the same, which the handover around it is what quiets (handOverKeptRole).
func walkKeptRole(ctx context.Context, cfg listenerDeliveryHandlerConfig, role, holder string, entry session.SessionEntry, head keptLaneHead, now func() time.Time) (roleKeepDrainResult, error) {
	ctx, cancel := context.WithTimeout(ctx, roleKeepDrainWindow)
	defer cancel()

	// A copy another walk holds a claim of is that walk's to forward or to send the dead letter
	// of, so this one leaves the lane to it rather than doing it a second time; the claim is
	// judged by the rule both writers of a kept lane judge one by
	// (bus.KeptRoleCursor.ClaimOutstanding), which reads the clock only while a claim is named,
	// so now is asked for once per copy in the walk below and once more only while a claim is
	// named.
	if head.cursor.ClaimOutstanding(head.written, now) {
		cfg.logger.LogEvent(slog.LevelDebug, "listener kept role drain left the lane to the drain holding a claim",
			slog.String("role", role),
			slog.String("session_id", holder),
			slog.Uint64("claiming", head.cursor.Claiming))
		return roleKeepDrainResult{end: roleKeepLaneBusy}, nil
	}

	keptSubject := bus.KeptRoleSubject(role)
	lane := &roleKeepLaneCursor{cfg: cfg, role: role, holder: holder, subject: bus.KeptRoleCursorSubject(role), seq: head.cursorSeq}
	subject := contracts.LegacyAgentSubject(holder)
	// The record the role lane judges a forward against, read the same way here: what the holder
	// already took, and what is already on its way to it. A walk with no holder judges nothing
	// against it.
	seen := func(key, recipient string) bool {
		return cfg.dedupeCache.Seen(key, recipient) || cfg.attemptCache.Seen(key, recipient)
	}
	result := roleKeepDrainResult{}
	kept := head.cursor.Kept
	for scanned := 0; ; scanned++ {
		if scanned >= roleKeepScanMax || result.forwarded >= roleKeepDrainMax {
			result.end = roleKeepWorkLeft
			return result, nil
		}
		if holder != "" {
			held, err := roleKeepDrainHolds(ctx, cfg, role, holder)
			if err != nil {
				result.end = roleKeepDrainFailed
				return result, fmt.Errorf("read role %s's claim before its kept lane after %d: %w", role, kept, err)
			}
			if !held {
				cfg.logger.LogEvent(slog.LevelInfo, "listener kept role drain left the lane: the role's claim no longer names its holder",
					slog.String("role", role),
					slog.String("session_id", holder))
				result.end = roleKeepHolderMoved
				return result, nil
			}
		}
		stored, err := cfg.client.NextStoredMessage(ctx, keptSubject, kept+1)
		if errors.Is(err, nats.ErrMsgNotFound) {
			result.end = roleKeepLaneDrained
			return result, nil
		}
		if err != nil {
			result.end = roleKeepDrainFailed
			return result, fmt.Errorf("read role %s's kept lane after %d: %w", role, kept, err)
		}
		claim := keptCopyClaimOf(head.cursor, stored.Sequence)
		judged := judgeKeptRoleCopy(cfg, role, holder, entry, stored, claim, now, seen)
		switch judged.step {
		case keptCopyWait:
			result.end, result.waiting = roleKeepCopyWaits, stored.Sequence
			return result, nil
		case keptCopyEnd:
			if err := lane.write(ctx, bus.KeptRoleCursor{Kept: stored.Sequence}); err != nil {
				result.end, err = roleKeepWriteFailed(err)
				return result, err
			}
			kept = stored.Sequence
			result.ended++
			judged.report()
			continue
		case keptCopyDeadLetter, keptCopyExpiryNotice:
			end := endKeptCopyWithDeadLetter
			if judged.step == keptCopyExpiryNotice {
				end = endKeptCopyWithExpiryNotice
			}
			ended, unconfirmed, err := end(ctx, lane, kept, stored, judged)
			if ended {
				kept = stored.Sequence
				result.ended++
			}
			if err != nil {
				result.unconfirmed = unconfirmed
				result.end, err = roleKeepWriteFailed(err)
				return result, err
			}
			continue
		}
		// Every other copy is keptCopyForward: the holder takes it, in three writes.
		item := judged.item
		if err := lane.write(ctx, bus.KeptRoleCursor{Kept: kept, Claiming: stored.Sequence}); err != nil {
			result.end, err = roleKeepWriteFailed(err)
			return result, err
		}
		handed := roleForward{item: item, sessionID: holder}
		// The copy goes under the envelope the lane's own forward of it carries
		// (roleForward.forwarded), with the time the stream stored it beside it.
		forward := handed.forwarded()
		keptAt := stored.Time.UnixMilli()
		forward.KeptAt = &keptAt
		// The copy is on its way to this holder, which the record the lane's own forward keeps
		// says the same way (roleTopicDelivery), so a role message under this key is not sent to
		// it twice while this forward is outstanding. A forward that failed clears it again, as
		// the lane's failure does (finishRoleForward), since the copy stays on the lane and the
		// next walk must offer it rather than pass it over as one the holder already took.
		cfg.attemptCache.Record(item.DedupeKey, holder)
		// The forward is named as the lane's own forward of this message to this holder is
		// (roleForward.msgID), so the stream keeps one copy of it for this holder however many
		// walks forward it; a takeover's copy carries a dedupe key of its own
		// (takenFrameDedupeKey), so its forward takes no id an earlier forward took.
		msgID := handed.msgID(subject)
		_, forwarded := cfg.forwardAndWait(subject, forward, msgID, roleForwardTimeout)
		if forwarded != nil {
			// This walk is ending with the copy claimed and unforwarded, so it releases the
			// claim: the cursor goes back to what it was before the claim, and the role's next
			// walk reads this copy again at once rather than waiting the claim out. The release
			// still names the copy as one whose forward may have reached the holder when the
			// stream may hold a forward of it: the claim this walk took over was a forward's,
			// or this walk's own forward may have stored it (mayHaveStored). A release
			// that cannot be written leaves the claim, which the next walk takes over once it
			// is older than bus.KeptRoleClaimWindow; either way the copy is not lost, and
			// neither is what its forward may have done.
			cfg.attemptCache.Clear(item.DedupeKey, holder)
			var unconfirmed uint64
			if claim.forwardUnconfirmed() || mayHaveStored(forwarded) {
				unconfirmed = stored.Sequence
			}
			lane.release(ctx, kept, unconfirmed)
			result.end = roleKeepDrainFailed
			return result, fmt.Errorf("forward role %s's kept message %s to %s: %w", role, item.EventID, holder, forwarded)
		}
		landed, err := lane.confirm(ctx, bus.KeptRoleCursor{Kept: stored.Sequence})
		if !landed {
			result.unconfirmed = true
			result.end, err = roleKeepWriteFailed(err)
			return result, err
		}
		kept = stored.Sequence
		result.forwarded++
		// The copy reached the role's holder, the stream's acknowledgement of the forward being
		// the whole of that outcome (startRoleForward), so it is reported where every role
		// forward's outcome is reported (finishRoleForward): counted in
		// envoy_messages_delivered_total, recorded in the dedupe cache, and logged on the role
		// forward's own line, with the role, the copy's sender and the time the copy was kept
		// beside it.
		finishRoleForward(cfg, handed, forwarded,
			slog.String("role", role),
			slog.String("source_session", item.SourceSession),
			slog.String("dedupe_key", forward.DedupeKey),
			slog.Int64("kept_at", keptAt))
		// A confirm another walk has gone on from is reported above all the same, and ends this
		// walk as one that found the lane busy (roleKeepLaneCursor.confirm).
		if err != nil {
			result.end, err = roleKeepWriteFailed(err)
			return result, err
		}
	}
}

// roleKeepDrainHolds reads role's claim record (readRoleClaimHolder) and reports whether it names
// holder, so this walk may go on forwarding to it. It is read before each copy a walk for a holder
// reads, the copies the walk ends included: a copy refused as an echo, as a repeat or for a
// delivery mode the holder does not take is refused for that holder, so none is ended for a
// session that no longer holds the role. That is at most roleKeepScanMax record reads a walk,
// inside roleKeepDrainWindow, and a walk runs only for a lane that holds kept copies.
//
// Only a record naming holder lets the walk go on. A record naming another session, or no record
// at all, ends the walk as one whose holder moved (roleKeepHolderMoved), and the handover's next
// pass resolves whoever holds the role then. A read that did not answer is returned as its error,
// which ends the walk as a failed one: the handover waits it out and resolves the holder again,
// and no copy is forwarded on a claim no read answered.
func roleKeepDrainHolds(ctx context.Context, cfg listenerDeliveryHandlerConfig, role, holder string) (bool, error) {
	claimed, err := readRoleClaimHolder(ctx, cfg, role)
	if err != nil {
		return false, err
	}
	return claimed == holder, nil
}

// readRoleClaimHolder reads role's claim record from the role bucket stream's leader
// (store.Registry.RoleClaimFromLeader, within ctx and roleHolderReadTimeout, whichever ends
// first), and returns the session it names, empty for a role no record claims. It is the read the
// session relay makes before it delivers a role message (takeOverFrame) and the read the kept lane
// makes of who holds a role outside a resolution - the handover's retry and the drain hold - so
// each decides on the record, and none on the registry's watched copy of the bucket, which can
// lack a claim the bucket holds or keep one the bucket dropped. It is never a direct get: on a
// replicated bucket a copy that has not applied a takeover answers the previous holder's claim,
// and the relay would hand that holder a role message the new holder never receives.
func readRoleClaimHolder(ctx context.Context, cfg listenerDeliveryHandlerConfig, role string) (string, error) {
	readCtx, cancel := context.WithTimeout(ctx, roleHolderReadTimeout)
	defer cancel()
	claim, err := cfg.registry.RoleClaimFromLeader(readCtx, role)
	if err != nil {
		return "", err
	}
	return claim.HolderSessionID, nil
}

// leaderRoleClaims is the claim side (roles.ClaimStore) of each resolution of a role's holder that
// must not read the claim from before a takeover: each claim read is the role bucket stream
// leader's (store.Registry.RoleClaimFromLeader), since on a replicated bucket a direct get can
// answer the claim as it stood before a takeover, naming the previous holder. The kept lane
// handover reads through it, with leaderHolderSessions (handOverKeptRole), where a stale claim
// would walk the lane for the previous holder, whose drain hold (roleKeepDrainHolds, a leader
// read) refuses every forward, and give the lane up with the copy left on it until the next
// sweep, minutes later. The resolutions that act on a miss no later read takes back read through
// it too, with leaderOnlyHolderSessions (resolveRoleHolderFromLeaders).
type leaderRoleClaims struct{ registry *store.Registry }

func (c leaderRoleClaims) RoleClaim(ctx context.Context, role string) (store.RoleClaim, error) {
	return c.registry.RoleClaimFromLeader(ctx, role)
}

// keptCopyKeptBy is who kept item, as the kept_by label and attribute name it:
// keptByDispatchOutbox for an outbox copy (bus.KeptCopyOutbox), keptByListener for every other, a
// copy of Dispatch's delivery of a post the publish route kept (bus.KeptCopyDispatchDelivery)
// among them.
func keptCopyKeptBy(item contracts.Envelope) string {
	if bus.ClassifyKeptCopy(item) == bus.KeptCopyOutbox {
		return keptByDispatchOutbox
	}
	return keptByListener
}

// keptCopyClaim is the claim a walk takes over on a copy it reads: the claim its lane-head read
// found on the role's cursor no longer outstanding (bus.KeptRoleCursor.ClaimOutstanding), which
// names the first copy after the cursor's kept, since a walk claims only that copy. It holds the
// two things a claim says, which keptCopyClaimOf reads from the cursor together: what the walk
// that made it claimed the copy for (kind), and the qualifier a dead letter's claim carries
// (unconfirmed). judgeKeptRoleCopy takes the whole of it, so no drop it builds under a claim loses
// the qualifier that claim carried (forwardUnconfirmed).
type keptCopyClaim struct {
	// kind is what the walk that made the claim claimed the copy for, keptCopyUnclaimed where no
	// walk's claim names the copy.
	kind keptCopyClaimKind
	// unconfirmed is the qualifier of a dead letter's claim, whichever reason the claim names
	// (unconfirmedClaimSuffix): a walk had claimed the copy to forward it before the dead
	// letter's claim replaced that claim, and no walk confirmed the forward. No claim of another
	// kind sets it.
	unconfirmed bool
}

// keptCopyClaimKind is what the walk that made a claim claimed the copy for.
type keptCopyClaimKind uint8

const (
	// keptCopyUnclaimed: no walk's claim names the copy.
	keptCopyUnclaimed keptCopyClaimKind = iota
	// keptCopyForwardClaimed: a walk claimed the copy to forward it, and no walk confirmed the
	// forward - the claim is on the cursor still, or a walk whose forward failed gave it up with
	// the copy marked as one whose forward may have reached the holder
	// (bus.KeptRoleCursor.ForwardUnconfirmed, roleKeepLaneCursor.release).
	keptCopyForwardClaimed
	// keptCopyModeClaimed: a walk claimed the copy to end it because the role's live holder does
	// not take its delivery mode - with a dead letter, or, for a copy of Dispatch's delivery of a
	// post, with an expiry notice (endKeptCopyWithExpiryNotice) - and no walk confirmed that end.
	keptCopyModeClaimed
	// keptCopyExpiryClaimed: a walk claimed the copy, because its hour ended, to end it with a dead
	// letter (endKeptCopyWithDeadLetter) or, for a copy Dispatch answers for (an outbox copy, or a
	// copy of Dispatch's delivery of a post), with an expiry notice (endKeptCopyWithExpiryNotice),
	// and no walk confirmed that end. A walk that takes the claim over judges the copy expired again
	// (judgeKeptRoleCopy), which sends a listener's copy's dead letter again and the notice of a
	// copy Dispatch answers for again.
	keptCopyExpiryClaimed
)

// forwardUnconfirmed reports whether a walk claimed the copy to forward it and no walk confirmed
// that forward, so the stream may hold the forward and the copy may have reached the session that
// held the role then: c is a forward's claim, or a dead letter's claim that carries the qualifier
// a walk wrote when it took such a claim over. It is the one rule keptRoleDrop.forwarded is set
// by, for a drop of either reason (judgeKeptRoleCopy).
func (c keptCopyClaim) forwardUnconfirmed() bool {
	return c.kind == keptCopyForwardClaimed || c.unconfirmed
}

// keptCopyClaimOf is the claim cursor, the cursor a walk started from, holds on the copy stored at
// seq. A forward's claim names the copy with no reason, and a forward's claim a walk gave up when
// its forward failed while the stream may hold a forward of the copy names it as
// bus.KeptRoleCursor.ForwardUnconfirmed beside no claim (roleKeepLaneCursor.release): both are a
// forward's claim, so the walk that reads either judges the copy as one whose forward may have
// reached the holder, however many walks claimed and released it before. A dead letter's claim
// names the reason the walk that made it ended the copy for, and whether the copy's forward may
// have reached the holder (bus.KeptRoleCursor.DeadLetter, which endKeptCopyWithDeadLetter writes
// as keptRoleDrop.claimReason makes it), so the walk that takes it over sends the dead letter
// again for that reason and with that qualifier. Both are read
// here: the value with unconfirmedClaimSuffix cut from its end is the reason,
// deadLetterDeliveryMode for a mode claim and deadLetterExpired for an expiry claim, and the
// suffix is the qualifier of either. A value that names neither reason is an expiry claim with no
// qualifier, whatever its end; no walk writes one. This is the one place a claim is read back, and
// claimReason the one place a drop is written into one, for a dead letter's drop and an expiry
// notice's alike. judgeKeptRoleCopy builds the drop again from the claim this returns. Both are
// read from the claim and from nothing else: the
// copy's age on the clock of the walk taking the claim over need not be what it was on the clock
// of the walk that made it, the holder that refused the copy's mode may no longer hold the role,
// and the forward's claim that showed the copy may have reached the holder is gone from the
// cursor once a dead letter's claim replaced it.
func keptCopyClaimOf(cursor bus.KeptRoleCursor, seq uint64) keptCopyClaim {
	switch {
	case cursor.Claiming == 0 && cursor.ForwardUnconfirmed == seq:
		return keptCopyClaim{kind: keptCopyForwardClaimed}
	case cursor.Claiming != seq:
		return keptCopyClaim{kind: keptCopyUnclaimed}
	case cursor.DeadLetter == "":
		return keptCopyClaim{kind: keptCopyForwardClaimed}
	}
	reason, unconfirmed := strings.CutSuffix(cursor.DeadLetter, unconfirmedClaimSuffix)
	switch deadLetterReason(reason) {
	case deadLetterDeliveryMode:
		return keptCopyClaim{kind: keptCopyModeClaimed, unconfirmed: unconfirmed}
	case deadLetterExpired:
		return keptCopyClaim{kind: keptCopyExpiryClaimed, unconfirmed: unconfirmed}
	}
	return keptCopyClaim{kind: keptCopyExpiryClaimed}
}

// keptCopyStep is what a walk does with one copy of a role's kept lane (judgeKeptRoleCopy).
type keptCopyStep uint8

const (
	// keptCopyForward: the walk forwards the copy to the role's live holder, in three cursor
	// writes.
	keptCopyForward keptCopyStep = iota
	// keptCopyEnd: the walk ends the copy in one cursor write and sends nothing for it.
	keptCopyEnd
	// keptCopyDeadLetter: the walk ends the copy with a dead letter to its sender, in three
	// cursor writes (endKeptCopyWithDeadLetter).
	keptCopyDeadLetter
	// keptCopyExpiryNotice: the walk ends a copy Dispatch answers for with an expiry notice to
	// Dispatch - a copy Dispatch's outbox kept, whose hour ended, and a copy of Dispatch's
	// delivery of a post (bus.KeptCopyDispatchDelivery) whose hour ended or whose live holder does
	// not take its delivery mode - in the same three cursor writes under the same claim a dead
	// letter takes (endKeptCopyWithExpiryNotice), so a walk that takes the claim over judges the
	// copy as the first did and stores the notice again under the same message id.
	keptCopyExpiryNotice
	// keptCopyWait: the copy is inside its hour and the walk has no live holder to forward it to,
	// so the walk stops at it.
	keptCopyWait
)

// keptCopyVerdict is judgeKeptRoleCopy's judgement of one copy.
type keptCopyVerdict struct {
	step keptCopyStep
	// item is the decoded copy, its zero value for a copy no read can decode.
	item contracts.Envelope
	// at is the walk's clock when it judged the copy, which a dead letter names as the time
	// Envoy dropped the copy and dates itself by.
	at time.Time
	// report is what the copy is owed beyond its dead letter's or notice's own line - its line, its
	// counter and its delivery outcome - which the walk runs only once its last cursor write for
	// the copy has landed, so two walks never report one copy twice. It is nil for a copy the walk
	// forwards or stops at, and for a delivery mode's claim taken over, whatever the copy's age,
	// whose one line is its dead letter's or its notice's.
	report func()
	// drop is why a copy the walk ends with a dead letter or an expiry notice is dropped: its
	// reason, and whether its forward may have reached the holder, which the claim written for the
	// end (keptRoleDrop.claimReason) and the notice (bus.KeptCopyExpiry.ForwardUnconfirmed) carry.
	drop keptRoleDrop
}

// judgeKeptRoleCopy judges one copy of role's kept lane, stored at stored and under claim
// (keptCopyClaimOf), for holder (empty for a walk with no live holder), before any cursor is
// written for it, in this order:
//
//  1. A copy whose role no read can tell (readRoleFrame, the read the session relay and a
//     takeover pass make: not a JSON object, or a topic or role mark that is not a string and
//     names no role) reaches no holder however often it is read, so it is ended rather than every
//     later walk stopping on it, logged with its stream sequence and counted
//     (envoy_role_kept_undecodable_total). Every other copy's envelope is read field by field
//     (roleFrame.envelope), so a field the envelope's type for it does not take, or a value the
//     wire rules or the envelope's own refuse, ends no copy.
//  2. A withdrawal mark (isWithdrawnFrame) is no message: the publish route wrote it in place of
//     the takeover copy of a forward it answered as failed (withdrawLateForward), so it is ended,
//     forwarded to no one, owed no dead letter and counted as no expired copy whatever its age,
//     with one line naming its event, so a reader of the lane sees what became of that forward.
//  3. A copy under a delivery mode's claim that this walk takes over (keptCopyModeClaimed) is a
//     delivery mode drop whose end was not confirmed, and it is owed that end again, whatever its
//     age now, whoever holds the role now and whether anyone does: its dead letter, or, for a copy
//     of Dispatch's delivery of a post (bus.KeptCopyDispatchDelivery), its expiry notice with the
//     delivery mode as its reason. Its dead letter's line or its notice's line is its one report:
//     it was dropped inside its hour for its delivery mode, so it is no expired copy however old
//     it is when its end is sent again, and nothing is counted on envoy_role_kept_expired_total
//     for it.
//  4. A copy whose hour ended (keptCopyEnded), and a copy under an expiry's claim that this walk
//     takes over (keptCopyExpiryClaimed) whatever its age by this walk's clock, since the walk
//     that made the claim found its hour ended, is dropped, counted and logged
//     (keptRoleDroppedReport). It is owed a dead letter when the listener kept it and its publish
//     named a sender session (bus.KeptCopySession), unless no walk claimed it and its hour ended
//     more than an hour before (roleKeepStaleAge): a copy a walk claimed is owed one whatever its
//     age. A walk for a holder names that holder in the dead letter as the session that holds the
//     role now (keptRoleDrop.holder), since telling the sender no session held the role would be
//     false; the dead letter of a copy whose forward may have reached the holder (below) says that
//     instead. A copy Dispatch's outbox kept (bus.KeptCopyOutbox) is owed an expiry notice to
//     Dispatch in place of a dead letter (keptCopyExpiryNotice, endKeptCopyWithExpiryNotice),
//     whatever its age: Dispatch decides what becomes of the event, and a copy no walk reached for
//     hours is the very one it must hear of, where a stale dead letter would tell a session of a
//     message it sent hours ago. So is a copy of Dispatch's delivery of a post, whatever its age:
//     Dispatch settles the delivery's attempt by the notice, however late the walk that reads the
//     copy is.
//  5. A walk with no holder stops at every other copy.
//  6. A walk for a holder applies beforeForward (keptRoleForwardAllowed): an allowed copy is
//     forwarded; an echo or a repeat is passed over; and a copy whose delivery mode the holder
//     does not take is dropped as the lane refuses it, and owed a dead letter naming that holder
//     when the listener kept it and its publish named a sender session. A copy of Dispatch's
//     delivery of a post whose delivery mode the holder does not take is owed an expiry notice
//     instead, with the delivery mode as its reason, so its attempt does not stay queued for good;
//     an outbox copy's is owed nothing.
//
// Every copy owed a dead letter or an expiry notice, by rule 3, 4 or 6, is dropped as a copy whose
// forward may have reached the holder (keptRoleDrop.forwarded) when the claim this walk takes
// over is a forward's claim, or a dead letter's or notice's claim a walk wrote with that
// qualifier (keptCopyClaim.forwardUnconfirmed), however many walks took the claim over before
// this one, and its dead letter says so beside its reason, as its notice does
// (bus.KeptCopyExpiry.ForwardUnconfirmed). The verdict of such a copy is made in one of two places
// below (deadLetter and notice), so no rule builds a drop without the qualifier its claim carried.
//
// The forward claim of a copy inside its hour is no reason of its own: that copy is judged as any
// copy, and the walk claims it again, for the forward or for the dead letter that judgement gives
// it. Nor is a dead letter's claim on a copy owed no dead letter, which no walk writes. now is
// called once per copy that decodes, whatever the verdict.
func judgeKeptRoleCopy(cfg listenerDeliveryHandlerConfig, role, holder string, entry session.SessionEntry, stored bus.StoredMessage, claim keptCopyClaim, now func() time.Time, seen func(key, recipient string) bool) keptCopyVerdict {
	frame, err := readRoleFrame(stored.Data)
	if err != nil {
		return keptCopyVerdict{step: keptCopyEnd, report: func() {
			cfg.roleKeptUndecodable.Inc()
			cfg.logger.Error("listener kept role message cannot be decoded",
				slog.String("role", role),
				slog.Uint64("sequence", stored.Sequence),
				slog.String("error", err.Error()))
		}}
	}
	item := frame.envelope()
	at := now()
	if isWithdrawnFrame(item, stored.MsgID, role) {
		return keptCopyVerdict{step: keptCopyEnd, item: item, report: func() {
			cfg.logger.Info("listener kept role lane holds a withdrawn forward; nothing is forwarded for it",
				slog.String("role", role),
				slog.String("event_id", item.EventID),
				slog.String("source_session", item.SourceSession),
				slog.String("dedupe_key", item.DedupeKey),
				slog.Uint64("sequence", stored.Sequence))
		}}
	}
	// A copy Dispatch answers for is told to Dispatch and owed no dead letter: an outbox copy, and
	// a copy of Dispatch's delivery of a post, which names no source session. Every other copy is
	// the listener's, owed a dead letter where it names a sender.
	kind := bus.ClassifyKeptCopy(item)
	owed := kind == bus.KeptCopySession
	// deadLetter is the verdict of a copy owed a dead letter for drop, with the report the copy
	// is owed beside it. Every such verdict is made here, so every drop carries the qualifier of
	// the claim this walk took over, whatever its reason (keptCopyClaim.forwardUnconfirmed).
	deadLetter := func(report func(), drop keptRoleDrop) keptCopyVerdict {
		drop.forwarded = claim.forwardUnconfirmed()
		return keptCopyVerdict{step: keptCopyDeadLetter, item: item, at: at, report: report, drop: drop}
	}
	// notice is the verdict of a copy Dispatch answers for, told to Dispatch for reason, with the
	// report the copy is owed beside the notice. It carries the qualifier of the claim this walk
	// took over as deadLetter does, so the claim the notice is written under keeps the reason and
	// the qualifier for the walk that takes it over, and Dispatch hears whether the copy may have
	// reached a holder.
	notice := func(report func(), reason deadLetterReason) keptCopyVerdict {
		drop := keptRoleDrop{reason: reason, forwarded: claim.forwardUnconfirmed()}
		return keptCopyVerdict{step: keptCopyExpiryNotice, item: item, at: at, report: report, drop: drop}
	}
	if claim.kind == keptCopyModeClaimed {
		switch kind {
		case bus.KeptCopySession:
			// The walk that made the claim knew the holder that did not take the copy, and the
			// cursor does not keep it; the mode is the copy's own, read as that walk read it.
			mode, err := payloadDeliveryMode(item.Payload)
			return deadLetter(nil, keptRoleDrop{reason: deadLetterDeliveryMode, mode: mode, unreadable: err != nil})
		case bus.KeptCopyDispatchDelivery:
			// A delivery mode refusal whose notice was not confirmed: it ended inside its hour, so
			// it is reported as no expired copy, whatever its age now.
			return notice(nil, deadLetterDeliveryMode)
		}
	}
	if claim.kind == keptCopyExpiryClaimed || keptCopyEnded(at, stored.Time) {
		age := at.Sub(stored.Time)
		stale := claim.kind == keptCopyUnclaimed && age > roleKeepStaleAge
		report := keptRoleDroppedReport(cfg, role, holder, stored, item, age, stale)
		if kind.IsDispatch() {
			return notice(report, deadLetterExpired)
		}
		if !owed || stale {
			return keptCopyVerdict{step: keptCopyEnd, item: item, report: report}
		}
		return deadLetter(report, keptRoleDrop{reason: deadLetterExpired, holder: holder})
	}
	if holder == "" {
		return keptCopyVerdict{step: keptCopyWait, item: item}
	}
	verdict, report := keptRoleForwardAllowed(cfg, role, holder, entry, item, takenFrameOriginal(item, stored.MsgID, role), seen)
	switch verdict.kind {
	case forwardAllowed:
		return keptCopyVerdict{step: keptCopyForward, item: item}
	case forwardModeUnadvertised, forwardModeUnreadable:
		switch kind {
		case bus.KeptCopySession:
			return deadLetter(report, keptRoleDrop{reason: deadLetterDeliveryMode, holder: holder, mode: verdict.mode, unreadable: verdict.kind == forwardModeUnreadable})
		case bus.KeptCopyDispatchDelivery:
			// The notice's claim names the delivery mode (keptRoleDrop.claimReason), so a walk that
			// takes it over ends the copy with the same notice, as a refusal and no expired copy.
			return notice(report, deadLetterDeliveryMode)
		}
	}
	return keptCopyVerdict{step: keptCopyEnd, item: item, report: report}
}

// keptRoleDroppedReport is what a copy dropped at the end of its hour is owed once the walk's last
// cursor write for it has landed: one count on envoy_role_kept_expired_total, labelled by who kept
// it (keptCopyKeptBy) and whether it is stale (roleKeepStaleAge), and one drop line naming the
// role, the event id, the sender's session, both labels, the copy's age, its dedupe key and its
// stream sequence. holder is the role's live holder the walk is for, empty for a walk with no
// holder, which the line names as its session_id.
func keptRoleDroppedReport(cfg listenerDeliveryHandlerConfig, role, holder string, stored bus.StoredMessage, item contracts.Envelope, age time.Duration, stale bool) func() {
	keptBy := keptCopyKeptBy(item)
	return func() {
		// No role label: a role token names a tree role of one issue, so a deployment sees
		// unboundedly many of them and each would be a series of its own, kept for the
		// process's life. The role is on the log line below.
		cfg.roleKeptExpired.Inc([2]string{"kept_by", keptBy}, [2]string{"stale", strconv.FormatBool(stale)})
		cfg.logger.DeliveryLog(slog.LevelWarn, roleKeptDroppedLine,
			holder, item.Topic, item.EventID, "expired",
			slog.String("role", role),
			slog.String("source_session", item.SourceSession),
			slog.String("kept_by", keptBy),
			slog.Bool("stale", stale),
			slog.String("age", age.String()),
			slog.String("dedupe_key", item.DedupeKey),
			slog.Uint64("sequence", stored.Sequence))
	}
}

// keptRoleDrop is why a walk drops a copy it ends with a dead letter, which the dead letter's text
// says (keptRoleDeadLetter) and its line names.
type keptRoleDrop struct {
	// reason is deadLetterExpired or deadLetterDeliveryMode.
	reason deadLetterReason
	// forwarded is a copy a walk claimed to forward inside its hour and whose forward no walk
	// confirmed: the stream may have stored that forward, so the copy may have reached the
	// session that held the role then. It qualifies a drop of either reason, since such a copy
	// is dropped at the end of its hour or, inside it, because the role's live holder now does
	// not take its delivery mode (keptCopyClaim.forwardUnconfirmed). The claim of the copy's dead
	// letter carries it (claimReason), so every walk that takes that claim over drops the copy
	// as forwarded again (keptCopyClaimOf).
	forwarded bool
	// holder is, for a delivery mode drop, the live session that held the role and does not take
	// the copy's delivery mode, empty where a walk sends a dead letter again and no longer knows
	// that session. For an expired copy it is the live session a walk for a holder found when it
	// dropped the copy, which holds the role now, and empty for a walk with no live holder.
	holder string
	// mode is the delivery mode that session does not take, empty where none could be read.
	mode string
	// unreadable is a copy whose delivery mode cannot be read unambiguously
	// (forwardModeUnreadable).
	unreadable bool
}

// claimReason is the reason a walk writes into its claim of the copy it ends with a dead letter
// or an expiry notice for d (bus.KeptRoleCursor.DeadLetter, endKeptCopyWithDeadLetter,
// endKeptCopyWithExpiryNotice): d's reason, with unconfirmedClaimSuffix after it for a copy whose
// forward may have reached the holder (d.forwarded), whichever reason that is. It is the one place
// a drop is written into a claim. keptCopyClaimOf is the one place a claim is read back, so a walk
// that takes the claim over drops the copy for the reason, and with the qualifier, this drop has:
// a delivery mode refusal stays one, and is not reported as an expired copy, and a forward that
// may have reached the holder is still told.
func (d keptRoleDrop) claimReason() string {
	if d.forwarded {
		return string(d.reason) + unconfirmedClaimSuffix
	}
	return string(d.reason)
}

// summary is the reason the dead letter's payload_summary gives, a phrase that ends its one line.
// A delivery mode drop's is its delivery mode's whether or not its forward may have reached the
// holder (d.forwarded), which the dead letter's text says after it (explanation).
func (d keptRoleDrop) summary() string {
	switch {
	case d.reason == deadLetterDeliveryMode && d.unreadable:
		return "Envoy could not read its delivery mode"
	case d.reason == deadLetterDeliveryMode:
		return "the session that held the role does not take its delivery mode"
	case d.forwarded:
		return "its hour ended before Envoy confirmed that the session that held the role got it"
	case d.holder != "":
		return "its hour ended before the session that holds the role now took it"
	}
	return "no session held the role in the hour that Envoy kept it"
}

// explanation is the reason the dead letter's payload gives, in sentences. A copy whose forward
// may have reached the holder (d.forwarded) is told of that forward whichever reason it was
// dropped for: an expired one in the sentences about its hour, and one dropped for its delivery
// mode in the sentences that end the text, after the ones about the delivery mode.
func (d keptRoleDrop) explanation() string {
	const readRole = "You can use envoy_role_get to see which session holds the role before you send the message again."
	if d.reason == deadLetterExpired {
		if d.forwarded {
			return "Envoy started to send the message to the session that held the role. " +
				"The hour ended before Envoy confirmed that send. The message may have reached that session. " + readRole
		}
		if d.holder != "" {
			return "The hour ended before session " + d.holder + ", which holds the role now, took the message. " + readRole
		}
		return "No session held the role in the hour that Envoy kept the message. " + readRole
	}
	subject, object, held := "The session that held the role", "the session that held the role", ""
	if d.holder != "" {
		subject, object, held = "Session "+d.holder, "session "+d.holder, "Session "+d.holder+" held the role. "
	}
	var text string
	switch {
	case d.unreadable:
		text = held + "Envoy could not read the delivery mode of the message, so Envoy did not send it to " + object + "."
	case d.mode != "":
		text = held + subject + " does not take the delivery mode " + d.mode + "."
	default:
		text = held + subject + " does not take the delivery mode of the message."
	}
	if d.forwarded {
		text += " Before that, Envoy started to send the message to the session that held the role then. " +
			"Envoy did not confirm that send. The message may have reached that session."
	}
	return text
}

// keyedEventID is the event id of an envelope several listeners publish under one message id, key:
// the first 32 hex characters of key's SHA-256, the length of an id.New. A kept copy's dead letter
// and the sweep's repeated wake report are each named so, and the stream keeps the first copy
// that reaches it, so the lines of every listener that sent one name the event id the stream
// holds.
func keyedEventID(key string) string {
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:16])
}

// keptRoleDeadLetter is the dead letter of the copy item, stored at stored on role's kept lane and
// dropped at dropped for drop, and the message id it is published under. It is a stored envelope
// from source envoy on the copy sender's agent subject, with no source_session of its own, so the
// sender's host shows it as a message from Envoy, and source_event_id names the copy. Its dedupe
// key and its message id are both envoy.dead-letter.<role>.<the copy's sequence>, which no other
// copy has, so the stream holds one dead letter for each copy however many walks and listeners
// send it, and its event id is that key's (keyedEventID). issued_at is the walk's clock when it
// judged the copy. Its text names the role, the copy's event id, the message (keptCopyNamed: a
// Dispatch comment or message by its reference and none of its body, any other Dispatch message
// by its event id alone, a message from any other source by its first line), why Envoy dropped
// it, and when Envoy kept it and dropped it, in UTC.
func keptRoleDeadLetter(role string, stored bus.StoredMessage, item contracts.Envelope, dropped time.Time, drop keptRoleDrop) (contracts.Envelope, string) {
	key := "envoy.dead-letter." + role + "." + strconv.FormatUint(stored.Sequence, 10)
	text := strings.Join([]string{
		"Envoy dropped a message that you published to role " + role + ".",
		"The event ID of the message is " + item.EventID + ".",
		keptCopyNamed(item),
		"Envoy kept the message at " + stored.Time.UTC().Format(time.RFC3339) +
			". Envoy dropped the message at " + dropped.UTC().Format(time.RFC3339) + ".",
		drop.explanation(),
	}, "\n")
	return contracts.Envelope{
		EventID:        keyedEventID(key),
		Source:         "envoy",
		SourceEventID:  item.EventID,
		Topic:          contracts.LegacyAgentSubject(item.SourceSession),
		DedupeKey:      key,
		IssuedAt:       dropped.UnixMilli(),
		PayloadSummary: "Envoy dropped your message to role " + role + ": " + drop.summary() + ".",
		Payload:        text,
		TraceID:        id.New(),
	}, key
}

// dispatchPostUnquoted is the sentence a dead letter gives after naming a Dispatch comment or
// message by its id and its reference, in place of its first line.
const dispatchPostUnquoted = "Envoy names a comment or a message from Dispatch by its ID and its reference and quotes none of it, because its author can edit or delete it."

// dispatchUnquoted is the line a dead letter gives in place of the first line of any other
// Dispatch message, which the letter names by its event id alone.
const dispatchUnquoted = "Envoy does not quote a message from Dispatch, and names this one by its event ID alone."

// keptCopyNamed is the line of item's dead letter that says which message item is. A message from
// any source but dispatch is named by its first line: its summary cut by contracts.OneLineSummary,
// the rule the publish route cuts every summary it stores by, so a copy the route kept is quoted
// as it was stored, and a raw role publish's at the same 160 runes. A Dispatch frame's summary is
// the head of what the event carries (internal/dispatch/outbox payloadSummary), and the line
// quotes none of it. A comment's or a message's frame carries the head of the post's body, and the
// letter is written as late as the end of the copy's hour, after the post's author may have edited
// or deleted the post, so the line names the post by its id and its reference (dispatchPostRef),
// which gives the post as it is now, and says why (dispatchPostUnquoted). Every other Dispatch
// frame - an ask's, an issue's, a delivery receipt, and a post's whose event names no reference -
// is named by its event id alone, and the line says only that (dispatchUnquoted).
func keptCopyNamed(item contracts.Envelope) string {
	if item.Source != "dispatch" {
		return "The first line of the message is: " + contracts.OneLineSummary(item.PayloadSummary)
	}
	kind, postID, ref := dispatchPostRef(item.Payload)
	if ref == "" {
		return dispatchUnquoted
	}
	return "The message is Dispatch " + kind + " " + postID + ": " + ref + ". " + dispatchPostUnquoted +
		" Read the reference to see the " + kind + " as it is now."
}

// dispatchPostKinds is the kind of post a Dispatch event carries, by the event type's first
// token, as a reference names it: a suggestion is a comment (model.CommentEventPayload).
var dispatchPostKinds = map[string]string{
	"message":    "message",
	"comment":    "comment",
	"suggestion": "comment",
}

// dispatchEvent is what dispatchPostRef reads of a Dispatch frame's payload, the whole Dispatch
// event (internal/dispatch/outbox envelope, model.Event): the event's type and issue key, and of
// the event's own payload the post's id and, for a comment on a project document, the document's
// project and slug (model.CommentEventPayload's project_key and artifact_slug).
type dispatchEvent struct {
	Type     string `json:"type"`
	IssueKey string `json:"issue_key"`
	Payload  struct {
		ID           string `json:"id"`
		ProjectKey   string `json:"project_key"`
		ArtifactSlug string `json:"artifact_slug"`
	} `json:"payload"`
}

// dispatchPostRef is the kind, the id and the reference of the post that payload, a Dispatch
// event, carries. The reference is written as Dispatch's reference graph addresses an item
// (internal/dispatch/refs itemRef): dispatch://<issue key>/<kind>/<id> for a post of an issue, and
// dispatch://<project>/artifact/<slug>/comment/<id> for a comment on a project document. All three
// are empty for a payload that does not decode as an event, for an event of no message or
// comment, and for one that lacks the post's id or its owner, so a dead letter names no reference
// it would have to guess.
func dispatchPostRef(payload string) (kind, postID, ref string) {
	var event dispatchEvent
	if json.Unmarshal([]byte(payload), &event) != nil {
		return "", "", ""
	}
	prefix, _, _ := strings.Cut(event.Type, ".")
	kind, postID = dispatchPostKinds[prefix], event.Payload.ID
	switch {
	case kind == "" || postID == "":
		return "", "", ""
	case event.IssueKey != "":
		return kind, postID, "dispatch://" + event.IssueKey + "/" + kind + "/" + postID
	case kind == "comment" && event.Payload.ProjectKey != "" && event.Payload.ArtifactSlug != "":
		return kind, postID, "dispatch://" + event.Payload.ProjectKey + "/artifact/" + event.Payload.ArtifactSlug + "/comment/" + postID
	}
	return "", "", ""
}

// deadLetterRefusal is why the stream refuses letter, the dead letter of a copy whose publish named
// sender, for good before anything is published, or nil. A sender that is not one subject token
// - empty, or holding a '.', a '*', a '>' or white space - names no agent subject of its own: the
// bus refuses only white space and an empty token (bus.ErrInvalidSubject), so a sender a.b would
// store the dead letter on another session's subject, and one holding a wildcard on a subject no
// session reads. An envelope contracts.Envelope.Validate refuses is refused the same way every
// time it is sent.
func deadLetterRefusal(sender string, letter contracts.Envelope) error {
	if sender == "" || strings.ContainsAny(sender, ".*>") || strings.ContainsFunc(sender, unicode.IsSpace) {
		return fmt.Errorf("source_session %q is not one subject token", sender)
	}
	return letter.Validate()
}

// endKeptCopyWithDeadLetter ends the copy stored at stored, which judged says is owed a dead letter,
// for a walk whose cursor stands at kept, through the one order every end of a copy with a
// publish takes (endKeptCopyWithPublish): the claim, written with the drop's reason, qualified,
// whichever reason it is, where the copy's forward may have reached the holder
// (keptRoleDrop.claimReason); the dead letter, published through cfg.publishDeadLetter
// (keptRoleDeadLetter); and the confirm. Its report is counted on envoy_role_dead_letters_total.
// It reports what endKeptCopyWithPublish reports.
//
// A refusal known before the publish (deadLetterRefusal) ends the copy in the one write {kept:
// the copy's sequence}, since there is nothing to claim the copy for - a plain write, which no
// claim of this walk's makes worth reading back - with one Error line and a count, and is not
// sent again.
func endKeptCopyWithDeadLetter(ctx context.Context, lane *roleKeepLaneCursor, kept uint64, stored bus.StoredMessage, judged keptCopyVerdict) (ended, unconfirmed bool, err error) {
	cfg, role, item := lane.cfg, lane.role, judged.item
	letter, msgID := keptRoleDeadLetter(role, stored, item, judged.at, judged.drop)
	report := keptCopyEndReport{
		counter:     cfg.roleDeadLetters,
		storedLine:  roleDeadLetterStoredLine,
		refusedLine: roleDeadLetterRefusedLine,
		attrs: []slog.Attr{
			slog.String("role", role),
			slog.String("event_id", item.EventID),
			slog.String("source_session", item.SourceSession),
			slog.String("reason", string(judged.drop.reason)),
			slog.String("dead_letter_event_id", letter.EventID),
			slog.Uint64("sequence", stored.Sequence),
		},
	}
	if refusal := deadLetterRefusal(item.SourceSession, letter); refusal != nil {
		if err := lane.write(ctx, bus.KeptRoleCursor{Kept: stored.Sequence}); err != nil {
			return false, false, err
		}
		reportKeptCopyEnd(cfg, judged, report, refusal)
		return true, false, nil
	}
	return endKeptCopyWithPublish(ctx, lane, kept, stored, keptCopyPublish{
		judged:  judged,
		claim:   judged.drop.claimReason(),
		publish: func(ctx context.Context) error { return cfg.publishDeadLetter(ctx, letter.Topic, letter, msgID) },
		what:    fmt.Sprintf("send the dead letter of role %s's kept message %s to %s", role, item.EventID, item.SourceSession),
		report:  report,
	})
}

// keptCopyPublish is what one end of a copy with a publish writes and says (endKeptCopyWithPublish):
// the verdict that ends the copy, the reason its claim names, the publish itself, what that
// publish is, for the error of one that failed, and what a copy whose confirm landed reports
// (reportKeptCopyEnd).
type keptCopyPublish struct {
	judged  keptCopyVerdict
	claim   string
	publish func(ctx context.Context) error
	what    string
	report  keptCopyEndReport
}

// endKeptCopyWithPublish ends the copy stored at stored, for a walk whose cursor stands at kept,
// with end's publish, in the order a forward is ended in. It reports whether the copy's last
// cursor write landed, which makes the copy one the walk ended; whether it left the copy's publish
// sent and unconfirmed; and the error that ends the walk. It is the one copy of that order for a
// dead letter (endKeptCopyWithDeadLetter) and an expiry notice (endKeptCopyWithExpiryNotice):
//
//  1. the cursor is written to {kept: unchanged, claiming: the copy's sequence, dead_letter:
//     end.claim}, so a walk that takes the claim over knows why it was made (keptCopyClaimOf);
//  2. end.publish stores the dead letter or the notice under the copy's own message id, so the
//     stream holds one however many walks store it;
//  3. the cursor is confirmed at {kept: the copy's sequence} (roleKeepLaneCursor.confirm).
//
// The copy's report, the publish's line and its count (reportKeptCopyEnd, end.report) come only
// once that confirm has landed, so a walk that lost a write reports nothing another walk reports,
// and a confirm that landed is reported even where another walk had gone on from it, which ends
// this walk.
//
// A walk that stops between two of these writes leaves a claim that a walk takes over once it is
// past bus.KeptRoleClaimWindow, and that walk publishes again under the same id: one that stopped
// after the claim and before the publish leaves nothing published, and one that stopped after the
// publish and before the confirm leaves a copy the stream drops. A publish that failed for a reason
// that can pass - a stream that is full, a time-out, no responders, a connection lost - is that
// stop: it is returned with the claim left where it is, and the walk ends as a failed one. A
// refusal for good (forwardRefused) ends the copy all the same, with one Error line and a count,
// and is not sent again: nothing a later walk sends would be taken.
func endKeptCopyWithPublish(ctx context.Context, lane *roleKeepLaneCursor, kept uint64, stored bus.StoredMessage, end keptCopyPublish) (ended, unconfirmed bool, err error) {
	if err := lane.write(ctx, bus.KeptRoleCursor{Kept: kept, Claiming: stored.Sequence, DeadLetter: end.claim}); err != nil {
		return false, false, err
	}
	published := end.publish(ctx)
	if published != nil && !forwardRefused(published) {
		return false, false, fmt.Errorf("%s: %w", end.what, published)
	}
	landed, err := lane.confirm(ctx, bus.KeptRoleCursor{Kept: stored.Sequence})
	if !landed {
		return false, true, err
	}
	reportKeptCopyEnd(lane.cfg, end.judged, end.report, published)
	return true, false, err
}

// keptCopyEndReport is what one end of a copy says once it has ended (reportKeptCopyEnd): the
// counter it is counted on, its lines for a publish stored and one refused for good, and the
// attributes both lines carry.
type keptCopyEndReport struct {
	counter     *metrics.Counter
	storedLine  string
	refusedLine string
	attrs       []slog.Attr
}

// reportKeptCopyEnd reports a copy a walk ended with a publish: the copy's own report
// (keptCopyVerdict.report), then the publish's line and its count on report.counter - stored, or,
// for refused, the error that refused it for good at Error. It is the one report of a dead letter
// and of an expiry notice.
func reportKeptCopyEnd(cfg listenerDeliveryHandlerConfig, judged keptCopyVerdict, report keptCopyEndReport, refused error) {
	if judged.report != nil {
		judged.report()
	}
	if refused != nil {
		report.counter.Inc([2]string{"result", "refused"})
		cfg.logger.Error(report.refusedLine, append(report.attrs, slog.String("error", refused.Error()))...)
		return
	}
	report.counter.Inc([2]string{"result", "stored"})
	cfg.logger.Info(report.storedLine, report.attrs...)
}

// keptCopyExpiry is the expiry notice of the copy item, stored at stored on role's kept lane and
// dropped at dropped: its event, the attempt its message id names (bus.KeptCopyAttempt), its key,
// its stream sequence, when the stream stored it and the walk dropped it, and whether the copy's
// forward may have reached the session that held the role (forwardUnconfirmed).
func keptCopyExpiry(role string, stored bus.StoredMessage, item contracts.Envelope, dropped time.Time, forwardUnconfirmed bool) bus.KeptCopyExpiry {
	return bus.KeptCopyExpiry{
		EventID:            item.EventID,
		SourceEventID:      item.SourceEventID,
		Role:               role,
		Attempt:            bus.KeptCopyAttempt(stored.MsgID, item.DedupeKey, role),
		DedupeKey:          item.DedupeKey,
		Sequence:           stored.Sequence,
		KeptAt:             stored.Time.UnixMilli(),
		DroppedAt:          dropped.UnixMilli(),
		ForwardUnconfirmed: forwardUnconfirmed,
	}
}

// endKeptCopyWithExpiryNotice ends the copy stored at stored, a copy Dispatch answers for
// (keptCopyExpiryNotice): an expired copy Dispatch's outbox kept, or a copy of Dispatch's delivery
// of a post whose hour ended or whose live holder does not take its delivery mode
// (bus.KeptCopyDispatchDelivery). It ends it for a walk whose cursor stands at kept, through the one
// order every end of a copy with a publish takes (endKeptCopyWithPublish): the claim, written with
// the drop's reason, qualified where the copy's forward may have reached the holder
// (keptRoleDrop.claimReason), so a walk that takes it over (keptCopyClaimOf) sends the notice again
// for the same reason and with the same qualifier, and a delivery mode refusal is not reported
// again as an expired copy; the notice, stored on the role's expiry subject under the copy's own
// message id through cfg.publishExpiryNotice (bus.Client.PublishKeptCopyExpiry, keptCopyExpiry),
// naming the copy's source event id, the outbox's event or the delivery's post, and whether the
// copy's forward may have reached a holder; and the confirm. Its report is counted on
// envoy_role_kept_expiry_notices_total, and its line names who kept the copy (keptCopyKeptBy).
// Dispatch's outbox reads the notice (internal/dispatch/outbox, readKeptExpiries): for an outbox
// copy it keeps the event again, or records it undelivered once it has kept it its bound of times;
// for a delivery's copy it settles that post's attempts the copy carried and nobody answered as
// undelivered, saying in the attempt's error that the message may have reached the role's holder
// where the notice says its forward may have. It reports what endKeptCopyWithPublish reports.
func endKeptCopyWithExpiryNotice(ctx context.Context, lane *roleKeepLaneCursor, kept uint64, stored bus.StoredMessage, judged keptCopyVerdict) (ended, unconfirmed bool, err error) {
	cfg, role := lane.cfg, lane.role
	notice := keptCopyExpiry(role, stored, judged.item, judged.at, judged.drop.forwarded)
	return endKeptCopyWithPublish(ctx, lane, kept, stored, keptCopyPublish{
		judged:  judged,
		claim:   judged.drop.claimReason(),
		publish: func(ctx context.Context) error { return cfg.publishExpiryNotice(ctx, notice) },
		what:    fmt.Sprintf("tell Dispatch role %s's kept message %s ended", role, judged.item.EventID),
		report: keptCopyEndReport{
			counter:     cfg.roleKeptExpiryNotices,
			storedLine:  roleExpiryNoticeLine,
			refusedLine: roleExpiryRefusedLine,
			attrs: []slog.Attr{
				slog.String("role", notice.Role),
				slog.String("event_id", notice.EventID),
				slog.String("kept_by", keptCopyKeptBy(judged.item)),
				slog.Int("attempt", notice.Attempt),
				slog.String("dedupe_key", notice.DedupeKey),
				slog.Uint64("sequence", stored.Sequence),
			},
		},
	})
}

// keptRoleRefusalLines are the drain's own lines for a copy beforeForward refused, which it writes
// under the status and the exception every forward path gives that refusal
// (refusedRoleForwardOutcome).
var keptRoleRefusalLines = roleForwardRefusalLines{
	echo:             "listener passed over a kept role message its own sender has claimed the role for",
	repeat:           "listener passed over a kept role message the role's new holder already took",
	modeUnreadable:   "listener kept role message's delivery mode cannot be read unambiguously",
	modeUnadvertised: "listener kept role message's new holder does not advertise delivery mode",
}

// keptRoleForwardAllowed judges one copy to the role's new holder, whose session entry is entry,
// by beforeForward - the rule the role lane and the publish route both apply before they forward -
// and returns the verdict and, for a copy the walk may not forward, what it is owed. The drain is
// the third forward path and reads the same record of what the holder already took as the lane
// does (seen), so an echo of a message the holder itself sent, a copy it already took and a
// payload its delivery modes do not take are refused here as they are there, each counted and
// answered as the lane counts and answers it (refusedRoleForwardOutcome) under the drain's own
// lines, which name the copy's sender beside the role. The copy is ended, and the report is the
// walk's to run once its last cursor write has moved the lane past the copy, so no later walk
// offers it again and no walk that lost that write reports it.
//
// reported is item as the refusal's exception names it: item itself, or for a takeover's copy the
// envelope under the message's own dedupe key (takenFrameOriginal), which a reader of the
// exception, such as the Legion daemon's re-hold, identifies the message by.
func keptRoleForwardAllowed(cfg listenerDeliveryHandlerConfig, role, holder string, entry session.SessionEntry, item, reported contracts.Envelope, seen func(key, recipient string) bool) (forwardVerdict, func()) {
	verdict := beforeForward(item, holder, entry, seen)
	if verdict.kind == forwardAllowed {
		return verdict, nil
	}
	outcome := refusedRoleForwardOutcome(verdict, reported, holder, keptRoleRefusalLines,
		slog.String("role", role),
		slog.String("source_session", item.SourceSession))
	return verdict, func() { applyDeliveryOutcome(cfg, reported, outcome) }
}
