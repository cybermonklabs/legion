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
// passed over under a debug line, and the next pass reads it again. A lane its cursor has
// finished costs its two reads and no goroutine. Each of those gives its guard and its slot back
// at once.
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
		release, held := cfg.roleKeepDrains.tryHold(role)
		if !held {
			<-slots
			continue
		}
		records, takenRole := taken[role]
		if !takenRole && !laneHasCopiesLeft(ctx, cfg, role) {
			release()
			<-slots
			continue
		}
		go func() {
			from := handOverSweptLane(ctx, cfg, role, records)
			release()
			if from != 0 {
				raiseKeptRoleWake(ctx, cfg, role, from)
			}
			<-slots
		}()
	}
}

// laneHasCopiesLeft reports whether role's kept lane holds a copy past its drain cursor, which a
// handover would offer its holder (readKeptLaneHead), read within roleKeepDrainWindow. A lane whose
// head cannot be read is reported as holding none, logged at debug, and the sweep's next run reads
// it again. A role with takeover records to pass is handed over whatever its lane holds, so the
// sweep asks this of the other lanes alone.
func laneHasCopiesLeft(ctx context.Context, cfg listenerDeliveryHandlerConfig, role string) bool {
	headCtx, cancelHead := context.WithTimeout(ctx, roleKeepDrainWindow)
	head, err := readKeptLaneHead(headCtx, cfg.client, role)
	cancelHead()
	if err != nil {
		cfg.logger.LogEvent(slog.LevelDebug, "listener kept role sweep could not read a role's kept lane",
			slog.String("role", role),
			slog.String("error", err.Error()))
		return false
	}
	if head.left {
		cfg.logger.LogEvent(slog.LevelDebug, "listener kept role sweep is handing over a role's kept lane",
			slog.String("role", role),
			slog.Uint64("kept", head.cursor.Kept),
			slog.Uint64("last", head.last))
	}
	return head.left
}

// handOverSweptLane is one handover the sweep started, for a caller holding role's guard: it hands
// role's kept lane over within roleKeepHandoverWindow on the listener's clock (handOverKeptRole),
// after the passes of records, the role's takeover records that are not finished as the sweep
// listed them (handOverTakenRole), logs it where every failed handover is logged
// (reportKeptRoleDrain), and returns the sequence the role's wake read starts from
// (raiseKeptRoleWake), 0 for a handover after which the role's wake report is not raised again.
// That sequence is the copy inside its hour the handover's walk with no holder stopped at, when
// the handover's last pass resolved no live holder - roles.Unclaimed or roles.Lapsed
// (roleKeepable). A resolution that did not answer raises none, since it says nothing about
// whether the role has a live holder; the handovers the claim route, a keep and a registration
// start raise none either, so the reports that follow the one a keep raises are the sweep's
// alone, one for each interval. It makes no read for the wake itself: its caller gives the role's
// guard back first, so no handover of the role waits on that read, and keeps its sweep slot
// through the read and gives it back after it (sweepKeptRoleLanes).
func handOverSweptLane(ctx context.Context, cfg listenerDeliveryHandlerConfig, role string, records []bus.StoredTakeoverRecord) uint64 {
	handoverCtx, cancel := context.WithTimeout(ctx, roleKeepHandoverWindow)
	defer cancel()
	handover, err := handOverTakenRole(handoverCtx, cfg, role, records)
	reportKeptRoleDrain(cfg, role, err)
	if !roleKeepable(roles.Resolution{State: handover.resolved}) {
		return 0
	}
	return handover.waiting
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
