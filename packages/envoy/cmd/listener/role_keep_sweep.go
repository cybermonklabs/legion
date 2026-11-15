// The sweep of the listener's kept role lanes: the loop that runs every interval and once as the
// listener starts, and the pass it makes, which hands over every lane the stream holds a kept
// copy on that its cursor has not finished, whether or not a session claims the role. It is the
// backstop behind the two handovers a running listener starts for itself, the claim's and the
// keep's (role_keep.go): it ends each copy whose hour ended, and it raises the role's wake report
// again while a copy the listener kept waits for a live holder.

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/roles"
)

// roleKeepSweepHandovers is how many handovers one listener's sweep runs at once. A pass reads
// every lane on the stream, and a lane with copies left costs a goroutine for its handover, which
// can run for roleKeepHandoverWindow when its holder's bus is not answering; a deployment that
// gathered copies on hundreds of lanes would otherwise start hundreds at once on the first pass
// after a restart. Sixteen keep a pass of short handovers - most of them a walk with no holder
// that reads one copy and stops - well inside one sweep interval, and a pass whose handovers are
// all running waits for one to end before it starts the next.
const roleKeepSweepHandovers = 16

// roleKeepWakeAge is how old a copy the listener kept may be for the sweep to raise its role's
// wake report again (raiseKeptRoleWake): forty minutes, so a role woken by the report has at
// least twenty of the copy's hour left to claim the role before the walk drops the copy. A copy
// whose first forty minutes have passed raises no more reports.
const roleKeepWakeAge = roleKeepWindow - 20*time.Minute

// startKeptRoleSweep is the backstop under which no kept copy waits past its hour. A handover runs
// in a goroutine of this process, so a listener restarted or replaced mid-handover ends it, and
// nothing starts another: a keep happens only while a role has no live holder, and a live holder
// claims no second time. A role nobody claims is handed nothing at all, and its copies must still
// end at the end of their hour, with a dead letter to their senders. Every interval, and once as
// the listener starts, this hands over every lane with copies left (sweepKeptRoleLanes,
// startSweep), at most roleKeepSweepHandovers at a time across the passes of this one loop.
func startKeptRoleSweep(ctx context.Context, cfg listenerDeliveryHandlerConfig, interval time.Duration) <-chan struct{} {
	slots := make(chan struct{}, roleKeepSweepHandovers)
	return startSweep(ctx, interval, func(ctx context.Context) { sweepKeptRoleLanes(ctx, cfg, slots) })
}

// sweepKeptRoleLanes hands over every role the stream holds a kept copy on that its cursor has not
// finished. It reads every kept role subject the stream holds a copy on in one call
// (bus.Client.KeptRoleLanes), so a deployment where no role has a kept copy pays one round trip a
// pass and no per-role work at all; a role that has one costs the read of its lane head
// (readKeptLaneHead, two stream reads), and a lane with copies left a handover of its own. The
// sweep reads no claim record: whether the role has a live holder, and whether it has any, is the
// handover's own resolution to make, and a role with no live holder is walked all the same, with no
// holder, so its copies end at the end of their hour.
//
// The pass waits for a slot of slots, the pool of roleKeepSweepHandovers its loop shares across
// passes (startKeptRoleSweep), before it takes a role's guard: a pass whose pool is full waits for
// a handover to end, or for ctx, and holds no guard while it waits, so a claim's or a keep's
// handover of a role waits only for the sweep's read of that role's lane head and for the
// handover the sweep starts on it, never for the whole pool. A role whose handover is already
// running on this listener is left to it: the sweep takes that role's guard or passes the role
// over, and it never waits for one (roleKeepGuards.tryHold), so one slow handover holds up no
// other role's sweep; such a lane raises no wake report in this pass. A lane whose head read did
// not answer is passed over under a debug line, and the next pass reads it again. A lane its
// cursor has finished costs its two reads and no goroutine. Each of those gives its guard and its
// slot back at once.
//
// A lane with copies left is handed over in a goroutine of its own, under the guard and the slot
// the sweep took. The handover reads the lane head again rather than taking this one, as each of
// its passes opens by reading it (handOverKeptRole). Each handover runs under
// roleKeepHandoverWindow on the listener's clock and raises the role's wake report again when it
// found no live holder (handOverSweptLane).
func sweepKeptRoleLanes(ctx context.Context, cfg listenerDeliveryHandlerConfig, slots chan struct{}) {
	lanes, err := cfg.client.KeptRoleLanes(ctx)
	if err != nil {
		if ctx.Err() == nil {
			cfg.logger.LogEvent(slog.LevelWarn, "listener kept role sweep could not read the kept lanes",
				slog.String("error", err.Error()))
		}
		return
	}
	for _, role := range lanes {
		if ctx.Err() != nil {
			return
		}
		select {
		case slots <- struct{}{}:
		case <-ctx.Done():
			return
		}
		release, taken := cfg.roleKeepDrains.tryHold(role)
		if !taken {
			<-slots
			continue
		}
		headCtx, cancelHead := context.WithTimeout(ctx, roleKeepDrainWindow)
		head, err := readKeptLaneHead(headCtx, cfg.client, role)
		cancelHead()
		if err != nil {
			release()
			<-slots
			cfg.logger.LogEvent(slog.LevelDebug, "listener kept role sweep could not read a role's kept lane",
				slog.String("role", role),
				slog.String("error", err.Error()))
			continue
		}
		if !head.left {
			release()
			<-slots
			continue
		}
		cfg.logger.LogEvent(slog.LevelDebug, "listener kept role sweep is handing over a role's kept lane",
			slog.String("role", role),
			slog.Uint64("kept", head.cursor.Kept),
			slog.Uint64("last", head.last))
		go func() {
			defer func() { <-slots }()
			defer release()
			handOverSweptLane(ctx, cfg, role)
		}()
	}
}

// handOverSweptLane is one handover the sweep started, for a caller holding role's guard: it hands
// role's kept lane over within roleKeepHandoverWindow on the listener's clock (handOverKeptRole),
// logs it where every failed handover is logged (reportKeptRoleDrain), and raises the role's wake
// report again when the handover's last pass resolved no live holder - roles.Unclaimed or
// roles.Lapsed - and its walk with no holder stopped at a copy inside its hour
// (raiseKeptRoleWake). A resolution that did not answer raises none, since it says nothing about
// whether the role has a live holder; a claim's handover and a keep's raise none either, so the
// reports that follow the one a keep raises are the sweep's alone, one for each interval.
func handOverSweptLane(ctx context.Context, cfg listenerDeliveryHandlerConfig, role string) {
	handoverCtx, cancel := context.WithTimeout(ctx, roleKeepHandoverWindow)
	defer cancel()
	handover, err := handOverKeptRole(handoverCtx, cfg, role, cfg.now)
	reportKeptRoleDrain(cfg, role, err)
	if handover.waiting == 0 || !roleKeepable(roles.Resolution{State: handover.resolved}) {
		return
	}
	raiseKeptRoleWake(ctx, cfg, role, handover.waiting)
}

// raiseKeptRoleWake raises role's wake report again when role's kept lane, read from the copy at
// from - the first copy still inside its hour, where the handover's walk with no holder stopped -
// holds a copy the listener kept with at least twenty minutes of its hour left
// (keptLaneWakesItsRole). The report is published once, under the message id of the interval the
// listener's clock falls in (publishSweptRoleWake), so every listener that sweeps the lane in that
// interval names the same report and the stream stores one. A lane whose copies read nothing that
// answers is passed over under a debug line, and the next sweep reads it again.
func raiseKeptRoleWake(ctx context.Context, cfg listenerDeliveryHandlerConfig, role string, from uint64) {
	now := cfg.now()
	readCtx, cancel := context.WithTimeout(ctx, roleKeepDrainWindow)
	wakes, err := keptLaneWakesItsRole(readCtx, cfg.client, role, from, now)
	cancel()
	if err != nil {
		if ctx.Err() == nil {
			cfg.logger.LogEvent(slog.LevelDebug, "listener kept role sweep could not read the copies that wake a role",
				slog.String("role", role),
				slog.String("error", err.Error()))
		}
		return
	}
	if wakes {
		publishSweptRoleWake(ctx, cfg.client, cfg.logger, role, now)
	}
}

// keptLaneWakesItsRole reports whether role's kept lane holds, from the copy stored at from on and
// within roleKeepScanMax copies, a copy the listener kept (isOutboxCopy) whose age at now is at
// most roleKeepWakeAge. The read goes forward because the stream reads no message before a
// sequence; a lane's copies grow younger along it, so the read ends at the first copy that wakes
// the role. An outbox copy wakes nothing, as the outbox's own keep raises no report, and a copy
// no read can decode wakes nothing either.
func keptLaneWakesItsRole(ctx context.Context, client *bus.Client, role string, from uint64, now time.Time) (bool, error) {
	subject := bus.KeptRoleSubject(role)
	next := from
	for range roleKeepScanMax {
		stored, err := client.NextStoredMessage(ctx, subject, next)
		if errors.Is(err, nats.ErrMsgNotFound) {
			return false, nil
		}
		if err != nil {
			return false, fmt.Errorf("read role %s's kept lane from %d: %w", role, next, err)
		}
		next = stored.Sequence + 1
		var item contracts.Envelope
		if now.Sub(stored.Time) <= roleKeepWakeAge && json.Unmarshal(stored.Data, &item) == nil && !isOutboxCopy(item) {
			return true, nil
		}
	}
	return false, nil
}
