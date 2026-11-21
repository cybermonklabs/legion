// The sweep of the listener's kept role lanes: the loop that runs every interval and once as the
// listener starts, and the pass it makes, which hands over every lane the stream holds a kept
// copy on that its cursor has not finished, whether or not a session claims the role. It is the
// backstop behind the handovers a running listener starts for itself - the claim route's, for a
// claim it grants and for each role of a claimant it read not live before its refresh
// (roleSetHandler), the keep's (role_keep.go), and the registration's that makes a holder live
// again (subscribeHandler): it ends each copy whose hour ended, and it raises the role's wake
// report again while a copy the listener kept waits for a live holder.

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
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
// nothing starts another while the holder stays live: a keep happens only while a role has no live
// holder, a live holder claims no second time, and a heartbeat of a holder that was live already
// starts no handover, since only the registration that makes a holder live again
// (subscribeHandler) and the claim route's refresh of a claimant that was not live
// (roleSetHandler) start one. A role nobody claims is handed nothing at all, and its copies must
// still end at the end of their hour, with a dead letter to their senders. Every interval, and
// once as the listener starts, this hands over every lane with copies left (sweepKeptRoleLanes,
// startSweep), at most roleKeepSweepHandovers at a time across the passes of this one loop.
func startKeptRoleSweep(ctx context.Context, cfg listenerDeliveryHandlerConfig, interval time.Duration) <-chan struct{} {
	slots := make(chan struct{}, roleKeepSweepHandovers)
	return startSweep(ctx, interval, func(ctx context.Context) { sweepKeptRoleLanes(ctx, cfg, slots) })
}

// sweepKeptRoleLanes hands over every role the stream holds a kept copy on that its cursor has not
// finished, and every role a takeover record that is not finished names (bus.TakeoverRecords). It
// reads every kept role subject the stream holds a copy on in one call (bus.Client.KeptRoleLanes),
// and every takeover record in another, so a deployment where no role has a kept copy and no
// takeover record pays two round trips a pass and no per-role work at all; a role that has a kept
// copy costs the read of its lane head (readKeptLaneHead, two stream reads), and a lane with
// copies left a handover of its own. A role with a takeover record that is not finished is handed
// over whatever its lane head says, its record's passes first (handOverTakenRole), since a pass
// can put copies on a lane that held none: a release of the role, the reaper's delete of its
// claim and a claim no read answers leave the record where it is, so none of them ends a copy. The
// sweep reads no claim record: whether the role has a live holder, and whether it has any, is the
// handover's own resolution to make, and a role with no live holder is walked all the same, with no
// holder, so its copies end at the end of their hour.
//
// The pass waits for a slot of slots, the pool of roleKeepSweepHandovers its loop shares across
// passes (startKeptRoleSweep), before it takes a role's guard: a pass whose pool is full waits for
// the sweep's work on a role to end, or for ctx, and holds no guard while it waits, so a handover
// of a role that the claim route, a keep or a registration started waits only for the sweep's
// read of that role's lane head and for the handover the sweep starts on it, never for the whole
// pool nor for the wake read after that handover. A role whose handover is already running on
// this listener is left to it: the sweep takes that role's guard or passes the role over, and it
// never waits for one (roleKeepGuards.tryHold), so one slow handover holds up no other role's
// sweep; such a lane raises no wake report in this pass. A lane whose head read did not answer is
// passed over under a debug line, and the next pass reads it again; one whose read lost its
// connection, or found it down, is read again once it is back (sweptLaneHead). A lane its cursor
// has finished costs its two reads and no goroutine. Each of those gives its guard and its slot
// back at once.
//
// A lane with copies left is handed over in a goroutine of its own, under the guard and the slot
// the sweep took. The handover reads the lane head again rather than taking this one, as each of
// its passes opens by reading it (handOverKeptRole). Each handover runs under
// roleKeepHandoverWindow on the listener's clock (handOverSweptLane). The goroutine gives the
// role's guard back once the handover has ended, and only then reads for the role's wake report,
// which it raises again when the handover found no live holder (raiseKeptRoleWake): that read, of
// up to roleKeepDrainWindow, and the report's publish hold no guard, so a handover of the role
// that the claim route, a keep or a registration started waits for the sweep's handover alone.
// The goroutine gives its slot back last, after the wake read and the report: the pool bounds the
// sweep's own work, the wake read included, and a pass that has every slot of its pool back knows
// that every handover it started, and every wake report behind one, has ended.
func sweepKeptRoleLanes(ctx context.Context, cfg listenerDeliveryHandlerConfig, slots chan struct{}) {
	lanes, err := cfg.client.KeptRoleLanes(ctx)
	if err != nil {
		if ctx.Err() == nil {
			cfg.logger.LogEvent(slog.LevelWarn, "listener kept role sweep could not read the kept lanes",
				slog.String("error", err.Error()))
		}
		return
	}
	taken := takenRoles(ctx, cfg)
	for role := range taken {
		if !slices.Contains(lanes, role) {
			lanes = append(lanes, role)
		}
	}
	slices.Sort(lanes)
	for _, role := range lanes {
		if ctx.Err() != nil {
			return
		}
		select {
		case slots <- struct{}{}:
		case <-ctx.Done():
			return
		}
		records, takenRole := taken[role]
		release, ok := sweptLaneHead(ctx, cfg, role, takenRole)
		if !ok {
			<-slots
			continue
		}
		// The handover runs on a goroutine of its own, counted for the shutdown
		// (roleKeepGuards.start), under the sweep's own context, which the shutdown ends first.
		cfg.roleKeepDrains.start(func(context.Context) {
			from := handOverSweptLane(ctx, cfg, role, records, release)
			if from != 0 {
				raiseKeptRoleWake(ctx, cfg, role, from)
			}
			<-slots
		})
	}
}

// sweptLaneHead takes role's guard for the sweep (roleKeepGuards.tryHold), and answers the guard's
// release when the sweep hands the role over: always for a role with a takeover record that is not
// finished (taken), whose passes can put copies on a lane that held none, and otherwise when its
// lane has copies left, which it reads under the guard. Any other answer gives the guard back, and
// its ok is false. The guard is passed over when a handover of the role holds it.
//
// The head is read under the guard, so the main client is asked first, within the probe's budget
// (bus.Client.LostNow), the read is made under a context marked so (bus.WithLockHeld), and the
// guard waits for no handshake. A read that found the connection down or lost it gives the guard
// back, waits for the connection with nothing held (bus.Reconnected), and takes the guard and reads
// the head again, within one roleKeepDrainWindow; a false "down" from a slow probe costs the sweep
// its probe and nothing else. Any other failed read is passed over under a debug line, and the
// next sweep reads the lane again.
func sweptLaneHead(ctx context.Context, cfg listenerDeliveryHandlerConfig, role string, taken bool) (func(), bool) {
	headCtx, cancel := context.WithTimeout(ctx, roleKeepDrainWindow)
	defer cancel()
	// Every read made under the guard is made under a context marked so (bus.WithLockHeld); the
	// wait for a lost connection, made with the guard released, is not.
	locked := bus.WithLockHeld(headCtx)
	for {
		release, held := cfg.roleKeepDrains.tryHold(role)
		if !held {
			return nil, false
		}
		if taken {
			return release, true
		}
		var head keptLaneHead
		err := cfg.client.LostNow()
		if err == nil {
			head, err = readKeptLaneHead(locked, cfg.client, role)
		}
		if err == nil && head.left {
			cfg.logger.LogEvent(slog.LevelDebug, "listener kept role sweep is handing over a role's kept lane",
				slog.String("role", role),
				slog.Uint64("kept", head.cursor.Kept),
				slog.Uint64("last", head.last))
			return release, true
		}
		release()
		if err == nil {
			return nil, false
		}
		if errors.Is(err, bus.ErrConnectionLost) && bus.Reconnected(headCtx, err) {
			continue
		}
		cfg.logger.LogEvent(slog.LevelDebug, "listener kept role sweep could not read a role's kept lane",
			slog.String("role", role),
			slog.String("error", err.Error()))
		return nil, false
	}
}

// handOverSweptLane is one handover the sweep started, for a caller holding role's guard, which it
// gives back through release before it returns: it hands role's kept lane over within
// roleKeepHandoverWindow on the listener's clock (handOverKeptRole), after the passes of records,
// the role's takeover records that are not finished as the sweep listed them (handOverTakenRole),
// logs it where every failed handover is logged (reportKeptRoleDrain), and returns the sequence the
// role's wake read starts from (raiseKeptRoleWake), 0 for a handover after which the role's wake
// report is not raised again. That sequence is the copy inside its hour the handover's walk with no
// holder stopped at, when the handover's last pass resolved no live holder - roles.Unclaimed or
// roles.Lapsed (roleKeepable). A resolution that did not answer raises none, since it says nothing
// about whether the role has a live holder; the handovers the claim route, a keep and a
// registration start raise none either, so the reports that follow the one a keep raises are the
// sweep's alone, one for each interval. It makes no read for the wake itself: it gives the role's
// guard back first, so no handover of the role waits on that read, and its caller keeps its sweep
// slot through the read and gives it back after it (sweepKeptRoleLanes).
//
// A handover that found the main client's connection down gave it up before it waited for it
// (handOverKeptRole, handOverTakenRole): as drainKeptRole does, the guard is given back, the wait
// for the connection is made with nothing held (bus.Reconnected), and the handover is made again
// under the guard, within the same window, the role's takeover records read again under it when
// the sweep listed any. A handover of the role that took the guard meanwhile is the one that hands
// the lane over, and this one ends. What each handover owes is settled with the guard given back,
// once that wait is over (keptRoleOwed), and a handover that ended on a cursor write it could not
// read back is made again once that write is settled.
func handOverSweptLane(ctx context.Context, cfg listenerDeliveryHandlerConfig, role string, records []bus.StoredTakeoverRecord, release func()) uint64 {
	handoverCtx, cancel := context.WithTimeout(ctx, roleKeepHandoverWindow)
	defer cancel()
	owed := newKeptRoleOwed(handoverCtx, cfg, role)
	for {
		handover, err := handOverTakenRole(handoverCtx, cfg, role, records, owed)
		release()
		again := errors.Is(err, bus.ErrConnectionLost) && bus.Reconnected(handoverCtx, err)
		if owed.settle(cfg) && handoverCtx.Err() == nil {
			again = true
		}
		if again {
			var taken bool
			if release, taken = cfg.roleKeepDrains.tryHold(role); taken {
				// The records are read again under the guard; a read that fails keeps the ones
				// already read, and one that fails other than by the connection's loss is logged.
				if records != nil {
					reread, err := readTakenRecords(handoverCtx, cfg, role)
					warnTakenRecordsUnread(cfg, role, err)
					if err == nil {
						records = reread
					}
				}
				continue
			}
			return 0
		}
		reportKeptRoleDrain(cfg, role, err)
		if !roleKeepable(roles.Resolution{State: handover.resolved}) {
			return 0
		}
		return handover.waiting
	}
}

// takenRoles is the takeover records that are not finished (bus.TakeoverRecords), by the role
// each names, read once a sweep pass, for the sweep to run their passes. A read that failed is
// logged and names none, and the next pass reads again.
func takenRoles(ctx context.Context, cfg listenerDeliveryHandlerConfig) map[string][]bus.StoredTakeoverRecord {
	records, err := cfg.client.TakeoverRecords(ctx, "")
	if err != nil {
		if ctx.Err() == nil {
			cfg.logger.LogEvent(slog.LevelWarn, "listener kept role sweep could not read the takeover records",
				slog.String("error", err.Error()))
		}
		return nil
	}
	taken := map[string][]bus.StoredTakeoverRecord{}
	for _, stored := range records {
		if !stored.Record.Finished {
			taken[stored.Record.Role] = append(taken[stored.Record.Role], stored)
		}
	}
	return taken
}

// raiseKeptRoleWake raises role's wake report again when role's kept lane, read from the copy at
// from - the first copy still inside its hour, where the handover's walk with no holder stopped -
// holds a copy the listener kept with at least twenty minutes of its hour left
// (keptLaneWakesItsRole). The report is published once, under the message id of the interval the
// listener's clock falls in (publishSweptRoleWake), so every listener that sweeps the lane in that
// interval names the same report and the stream stores one. A lane whose copies read nothing that
// answers is passed over under a debug line, and the next sweep reads it again. It runs after the
// sweep's handover of the role has given back the role's guard, so neither this read, which can
// take up to roleKeepDrainWindow, nor the report's publish holds up a handover of the role that
// the claim route, a keep or a registration started; it runs under that handover's sweep slot,
// which is given back after it (sweepKeptRoleLanes).
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
