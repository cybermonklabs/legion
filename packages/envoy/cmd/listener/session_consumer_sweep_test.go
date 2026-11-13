package main

import (
	"context"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/session"
)

const (
	// sweepSessionWindow is both windows the rig's registry judges a record's age by, so a
	// record twice this old reads gone (session.StateOf).
	sweepSessionWindow = time.Minute
	// sweepRecordTTL is the maximum age the rig's session bucket carries, which is also what the
	// registry answers a record past as absent (session.SessionRegistry.Get). The sweep confirms
	// every judgement against the bucket (session.SessionRegistry.Refresh), so a session reads
	// gone to the pass only once its record is past this too, as a deployed listener's is: the
	// bucket drops the record of a session that stopped heartbeating.
	sweepRecordTTL = time.Minute
	// sweepLapse is how far a test moves the clock to end a session: past both windows and past
	// the record's own age, with no wait of the clock's own.
	sweepLapse = 10 * time.Minute
)

// sweepRig is a session-consumer rig whose registry crosses the session windows on a clock the
// test moves (stateClock, session_state_test.go), and the sweep over it. A test runs one pass
// itself: no row here waits on the sweep's ticker.
type sweepRig struct {
	*sessionConsumerRig
	sweep *sessionConsumerSweep
	now   *stateClock
}

func newSweepRig(t *testing.T) *sweepRig { return newSweepRigKeeping(t, sweepRecordTTL) }

// newSweepRigKeeping is newSweepRig whose session bucket keeps a record for ttl, so a test can
// put the pass in front of a session its copy reads gone whose record the bucket still holds.
func newSweepRigKeeping(t *testing.T, ttl time.Duration) *sweepRig {
	t.Helper()
	now := &stateClock{}
	rig := newSessionConsumerRig(t,
		session.WithSessionTTL(ttl),
		session.WithSessionClock(now.Now),
		session.WithSessionLiveWindow(sweepSessionWindow),
		session.WithSessionGoneHorizon(sweepSessionWindow),
	)
	// A pass runs only on the task holding its machine's durable binding (sessionRelays.owned).
	// The rig starts no relay - nothing here pulls a consumer - so the binding is held on a relay
	// set of its own, which the pass reads and nothing else touches.
	relays := &sessionRelays{}
	relays.owned.Store(true)
	return &sweepRig{
		sessionConsumerRig: rig,
		sweep: &sessionConsumerSweep{
			delivery: rig.deps.consumers,
			sessions: rig.harness.sessions,
			deps:     rig.deps,
			relays:   relays,
			logger:   rig.harness.config.logger,
		},
		now: now,
	}
}

// register registers sessionID through the rig's routes, asking for the session consumer this
// sweep is about, and fails the test unless the stream holds it.
func (rig *sweepRig) register(t *testing.T, sessionID string) {
	t.Helper()
	rig.subscribe(t, `{"session_id":"`+sessionID+`","self_subscribed":true,"topics":[],
		"delivery":{"consumer":true,"epoch":"e"}}`)
	rig.requireConsumer(t, sessionID)
}

// requireState fails the test unless the registry puts sessionID in want, so a row that proves
// nothing about the sweep - a session the clock did not end, say - fails where it went wrong
// rather than where the consumer was counted.
func (rig *sweepRig) requireState(t *testing.T, sessionID string, want session.State) {
	t.Helper()
	if _, state := rig.harness.sessions.Lookup(sessionID); state != want {
		t.Fatalf("the registry reads %s as %s, want %s", sessionID, state, want)
	}
}

// coldSessions is the session registry as a listener reads it while its copy of the session
// bucket has not been read: every answer is the real registry's but the cache, which is not
// ready. A registry's readiness is released once and never taken back
// (kvwatch.Watcher.Ready), so this is the only way to put a pass in front of a cold copy.
type coldSessions struct{ sweptSessions }

func (coldSessions) CacheReady() bool { return false }

// A session that ended without deregistering - its pane closed, its box gone, its listener
// killed - leaves its consumer holding a slot of the stream's consumer cap, which nothing but
// the server's inactivity threshold used to free. One pass after the registry reads it gone, the
// sweep deletes it.
func TestTheSweepDeletesTheConsumerOfASessionThatEndedWithoutDeregistering(t *testing.T) {
	rig := newSweepRig(t)
	const sessionID = "ses_sweep_ended"
	rig.register(t, sessionID)
	rig.now.advance(sweepLapse)
	rig.requireState(t, sessionID, session.Gone)

	rig.sweep.pass(context.Background())

	rig.requireNoConsumer(t, sessionID)
}

// A session that is still registering keeps its consumer: that consumer is what delivers its
// stored messages, and a sweep that took it would leave the session reading nothing until its
// next registration created it again.
func TestTheSweepKeepsTheConsumerOfALiveSession(t *testing.T) {
	rig := newSweepRig(t)
	const sessionID = "ses_sweep_live"
	rig.register(t, sessionID)
	rig.requireState(t, sessionID, session.Live)

	rig.sweep.pass(context.Background())

	rig.requireConsumer(t, sessionID)
}

// The pass judges the session registry's record, not this listener's ownership of the session:
// a record naming another machine is another listener's to relay, and its session is as alive as
// any other, so its consumer stays. The sweep deletes only where the shared bucket holds no
// record at all, which is why a listener may delete a consumer it never created.
func TestTheSweepKeepsTheConsumerOfASessionOnAnotherMachine(t *testing.T) {
	rig := newSweepRig(t)
	const sessionID = "ses_sweep_other_machine"
	rig.register(t, sessionID)
	entry, _ := rig.harness.sessions.Lookup(sessionID)
	entry.MachineID = "other-machine"
	if err := rig.harness.sessions.Put(sessionID, entry); err != nil {
		t.Fatalf("write %s's registration as another machine's listener wrote it: %v", sessionID, err)
	}
	rig.requireState(t, sessionID, session.Live)

	rig.sweep.pass(context.Background())

	rig.requireConsumer(t, sessionID)
}

// A task that does not hold its machine's durable binding serves no session consumer of that
// machine (sessionRelays.owned, the same binding the relays follow), so it sweeps none: its
// pass does nothing, however plainly its copy of the bucket says the session has ended. Every
// other row of this file holds the binding, so without this one the whole cmd/listener suite
// passes with that gate taken out of sessionConsumerSweep.trusted (measured at 726f91df8).
func TestTheSweepDeletesNothingWhileAnotherTaskHoldsTheDurableBinding(t *testing.T) {
	rig := newSweepRig(t)
	const sessionID = "ses_sweep_unbound_task"
	rig.register(t, sessionID)
	rig.now.advance(sweepLapse)
	rig.requireState(t, sessionID, session.Gone)
	// A relay set whose binding this task does not hold: owned is false, as it is on the task
	// of a machine whose other task took the binding.
	rig.sweep.relays = &sessionRelays{}

	rig.sweep.pass(context.Background())

	rig.requireConsumer(t, sessionID)
}

// A listener whose copy of the session bucket has not been read yet would find every session of
// the fleet gone and delete every consumer on the stream. A pass over such a copy deletes
// nothing, however plainly the copy says the session has ended.
func TestTheSweepDeletesNothingWhileTheSessionCacheIsNotReady(t *testing.T) {
	rig := newSweepRig(t)
	const sessionID = "ses_sweep_cold_cache"
	rig.register(t, sessionID)
	rig.now.advance(sweepLapse)
	rig.requireState(t, sessionID, session.Gone)
	rig.sweep.sessions = coldSessions{rig.harness.sessions}

	rig.sweep.pass(context.Background())

	rig.requireConsumer(t, sessionID)
}

// The copy the pass judges a session by trails the bucket, so a session this listener has not
// caught up with reads gone here while the bucket still holds its registration. The pass
// confirms every judgement against the bucket itself, with the leader of that bucket's stream
// answering (session.SessionRegistry.RefreshFromLeader), and deletes nothing on a record the
// bucket answers, so a session another listener registered keeps the consumer that delivers
// its messages.
func TestTheSweepKeepsTheConsumerOfASessionTheBucketStillHolds(t *testing.T) {
	// The bucket keeps its records for an hour, past the lapse below, so the record stands while
	// the windows the registry judges age by have long passed.
	rig := newSweepRigKeeping(t, time.Hour)
	const sessionID = "ses_sweep_bucket_holds"
	rig.register(t, sessionID)
	rig.now.advance(sweepLapse)
	rig.requireState(t, sessionID, session.Gone)
	if _, err := rig.harness.sessions.Refresh(context.Background(), sessionID); err != nil {
		t.Fatalf("the bucket answers no record for %s (%v); this row needs one it still holds", sessionID, err)
	}

	rig.sweep.pass(context.Background())

	rig.requireConsumer(t, sessionID)
}
