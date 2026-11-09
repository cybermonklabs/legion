// The sweep of the stream's session consumers: the loop that runs every interval and once as the
// listener starts, and the pass it makes, which deletes the consumer of every session the session
// registry judges gone. It is the bound behind the deregistration's own delete
// (sessionDelivery.deleteConsumer, api.go), which reaches only a registration this listener
// removed.

package main

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/sjawhar/envoy/internal/logging"
	"github.com/sjawhar/envoy/internal/session"
)

// defaultSessionConsumerSweepInterval is how often the sweep runs when
// ENVOY_SESSION_CONSUMER_SWEEP_INTERVAL names no other period. It is the bound this sweep
// promises: a session that ended without deregistering holds its slot of the stream's consumer
// cap until one pass after the registry would answer gone, where the server's own inactivity
// threshold (bus.SessionConsumerInactiveThreshold) is hours away. Five minutes is the cadence
// the listener's other backstop loops run at, and a pass costs one consumer list on a fleet
// whose sessions are all live.
const defaultSessionConsumerSweepInterval = 5 * time.Minute

// sessionConsumerSweepCap is how many consumers one pass deletes. A hub that has been running
// since before this sweep existed holds a consumer for every session that ever ended without
// deregistering, and deleting thousands of them at once would send the server thousands of
// deletes - each a resume record, an info read and a delete - beside the consumer calls the live
// registrations are making. The pass takes the first 256 it confirms, in whatever order the list
// answered, and leaves the rest standing: the next pass finds them gone still and takes 256
// more, so the backlog drains over as many passes as it takes while every pass stays this small.
// 256 is the page the consumer list itself reads (bus.SessionConsumers.ListSessionConsumers).
const sessionConsumerSweepCap = 256

// sweptSessions is what the sweep asks of the session registry (session.SessionRegistry): the
// state the fleet's record puts a session in, a read of the bucket itself to confirm it, and
// whether this listener's copy of that record may be read at all. It is an interface because a
// copy whose cache is not ready is the one state a test cannot put a real registry into: a
// registry's readiness is released once and never taken back (kvwatch.Watcher.Ready), and that
// state is where the pass must delete nothing.
type sweptSessions interface {
	// CacheReady reports whether the copy's first read of the session bucket has completed.
	CacheReady() bool
	// Servable reports whether the copy may be served at all, nil when it may.
	Servable() error
	// NeedsRead reports whether the copy does not follow its bucket with no read under way.
	NeedsRead() bool
	// Lookup is the session's record and the state that record puts it in.
	Lookup(sessionID string) (session.SessionEntry, session.State)
	// Refresh reads the session's record from the bucket itself, answering nats.ErrKeyNotFound
	// when the bucket holds none.
	Refresh(ctx context.Context, sessionID string) (session.SessionEntry, error)
}

// sessionConsumerSweep deletes the consumers of sessions that have ended. A session that
// deregisters has its consumer deleted with its registration (deleteSessionHandler), and a
// session that simply ended - its pane closed, its box rebooted, its listener killed - never
// deregisters anything: without this sweep its consumer holds a slot of the stream's consumer cap
// until the server's inactivity threshold reaches it. This is also what retires the consumers
// already on a hub, left there by every session that ended before this sweep existed.
type sessionConsumerSweep struct {
	// delivery owns the session-consumer connection the pass lists and deletes through, and the
	// per-session lock its delete takes (sessionDelivery.deleteConsumer).
	delivery *sessionDelivery
	// sessions is the fleet's record of which sessions have ended.
	sessions sweptSessions
	// relays holds this task's machine durable binding (sessionRelays.owned), which the pass
	// requires: the task of a rolling deploy that relays nothing sweeps nothing either.
	relays *sessionRelays
	logger *logging.Logger
}

// startSessionConsumerSweep runs a pass every interval, and one as the listener starts. It runs
// under the context the listener's other KV loops run under and returns a channel closed once its
// goroutine has returned, as they do (store.Registry.StartReaper), so the shutdown ends it and
// waits for a pass in flight.
func startSessionConsumerSweep(ctx context.Context, sweep *sessionConsumerSweep, interval time.Duration) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			sweep.pass(ctx)
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
	return done
}

// pass deletes the consumer of every session the registry judges gone, at most
// sessionConsumerSweepCap of them. It reads every session consumer the stream holds in one call
// (bus.SessionConsumers.ListSessionConsumers), so a fleet whose sessions are all live costs one
// list and a cache read per consumer, with no round trip of its own for any of them.
//
// A session the registry answers live or unresponsive is left alone: an unresponsive session is
// not a dead one, its messages are stored for it, and its consumer is what delivers them when it
// answers again. A session the registry answers gone is confirmed against the bucket itself
// before anything is deleted (session.SessionRegistry.Refresh), where only nats.ErrKeyNotFound
// confirms it: a read that did not answer, and a record the bucket does hold, each leave that
// consumer standing for the next pass.
//
// The pass deletes the consumer of a session this listener never owned, and that is the normal
// case: a session's consumer outlives the listener that created it, so the consumers of a box
// whose listener died with its sessions are deleted by whichever listener sweeps next. Nothing is
// taken from a session that way, because the pass deletes only where the shared session bucket
// holds no registration at all: no listener owns such a session, no relay restarts its consumer,
// and the one call that creates it again is a fresh registration's (bus.SessionConsumers.Ensure),
// which creates it at the session's resume point - the floor the delete records before it removes
// the consumer (bus.SessionConsumers.Delete) - and so loses the session none of its stored
// messages. The session registry is the fleet's record rather than one listener's view, which is
// how the session reaper already treats it (internal/session/reaper.go).
//
// One line a pass names what it found, and a pass that lists no consumer at all writes none: a
// box whose listener holds no session costs no log line every interval.
func (s *sessionConsumerSweep) pass(ctx context.Context) {
	if !s.trusted() {
		return
	}
	started := time.Now()
	listCtx, cancel := context.WithTimeout(ctx, sessionConsumerCallTimeout)
	listed, err := s.delivery.consumers.ListSessionConsumers(listCtx)
	cancel()
	if err != nil {
		if ctx.Err() == nil {
			s.logger.LogEvent(slog.LevelWarn, "session consumer sweep could not list the session consumers",
				slog.String("error", err.Error()))
		}
		return
	}
	if len(listed) == 0 {
		return
	}
	gone, deleted := 0, 0
	for sessionID := range listed {
		if ctx.Err() != nil {
			return
		}
		if _, state := s.sessions.Lookup(sessionID); state != session.Gone {
			continue
		}
		gone++
		if deleted >= sessionConsumerSweepCap || !s.confirmGone(ctx, sessionID) {
			continue
		}
		s.delivery.deleteConsumer(ctx, sessionID)
		deleted++
	}
	// deleted counts the consumers the pass asked the server to delete; a delete that failed
	// writes a line of its own (sessionDelivery.deleteConsumer) and its consumer is reached again
	// next pass. gone above deleted is the backlog this pass left: the sessions its cap held
	// back, and those the bucket would not confirm.
	s.logger.Info("session consumer sweep pass",
		slog.Int("listed", len(listed)),
		slog.Int("gone", gone),
		slog.Int("deleted", deleted),
		slog.Int("left", len(listed)-deleted),
		slog.Int64("took_ms", time.Since(started).Milliseconds()))
}

// trusted reports whether this listener's copy of the session bucket may be swept on. A pass that
// read a cold or stale copy would find every session of the fleet gone and delete every consumer
// on the stream, so it runs only where all three hold: this task holds its machine's durable
// binding, so it is the task of its machine that serves the session consumers at all; the copy's
// first read of the bucket has completed (CacheReady); and the copy may be served and needs no
// read of its own (Servable, NeedsRead), which a listener whose watcher lost its connection fails
// until the read that restores it. A pass that is not trusted does nothing and says nothing: the
// states it waits on are each reported elsewhere, by /healthz and by the watcher's own lines.
func (s *sessionConsumerSweep) trusted() bool {
	if s.delivery == nil || s.delivery.consumers == nil {
		return false
	}
	if s.relays == nil || !s.relays.owned.Load() {
		return false
	}
	if !s.sessions.CacheReady() || s.sessions.NeedsRead() {
		return false
	}
	return s.sessions.Servable() == nil
}

// confirmGone reports whether the session bucket itself holds no record for sessionID, the one
// reading on which a consumer is deleted. The copy the pass judged the session by trails the
// bucket, so a session another listener registered a moment ago, or one this listener has not
// re-read since its connection came back, can be in the bucket and not in the copy. Only
// nats.ErrKeyNotFound confirms the judgement; a read that did not answer leaves the consumer for
// the next pass, with one line at debug, as does a record the bucket does hold, which is a
// session the copy has not caught up with and says nothing worth a line.
func (s *sessionConsumerSweep) confirmGone(ctx context.Context, sessionID string) bool {
	readCtx, cancel := context.WithTimeout(ctx, sessionConsumerCallTimeout)
	defer cancel()
	_, err := s.sessions.Refresh(readCtx, sessionID)
	if errors.Is(err, nats.ErrKeyNotFound) {
		return true
	}
	if err != nil && ctx.Err() == nil {
		s.logger.LogEvent(slog.LevelDebug, "session consumer sweep could not read a gone session's record; its consumer waits for the next pass",
			slog.String("session_id", sessionID),
			slog.String("error", err.Error()))
	}
	return false
}
