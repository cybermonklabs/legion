package supervise

// The model gateway's hold on the wakes this package paces (WakePace.Hold, dispatch://INFRA-5802
// R3): the rows of check c-hold-stops-starts that a machine decides. While a hold stands, a launch
// and a stale-plugin relaunch are reserved no moment before it ends; a wake whose turn comes while
// a renewed hold still stands is put back past it; the wakes a hold held leave at the pace, one at
// a time; and the controller, a request a person waits on and a running role's turn are not held.
// The hold is the daemon's own reading (internal/backpressure), fed answers on the harness's
// clock; the limiter is the real one (pacedDeps).

import (
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/backpressure"
	"github.com/sjawhar/legion/daemon/internal/claim"
)

const gatewayLease = 90 * time.Second

// holdHarness is a paced harness whose machines read hold.
func holdHarness(t *testing.T) (*harness, *backpressure.Hold) {
	t.Helper()
	hold := backpressure.New("team-a", nil)
	h := pacedHarness(t, wakeRate, func(d *Deps) { d.WakePace.Hold = hold })
	return h, hold
}

// holdFor has the gateway answer a hold of every team for lease from the harness's now, and
// returns when it ends.
func (h *harness) holdFor(hold *backpressure.Hold, lease time.Duration) time.Time {
	h.t.Helper()
	now := h.clock.Now()
	until := now.Add(lease)
	hold.Apply([]backpressure.Object{{Team: backpressure.AllTeams, State: backpressure.StateHold, HoldUntil: until, At: now, Reason: "organization refusing"}}, now)
	if got := hold.Until(now); !got.Equal(until) {
		h.t.Fatalf("the hold stands until %s, want %s", got, until)
	}
	return until
}

// wantHeldUntil is the claim's machine holding a wake of kind due at or after until, the end of
// the hold, and its runtime asked for no start beyond started.
func wantHeldUntil(h *harness, m *Machine, kind WakeKind, until time.Time, started int) WakeWaiting {
	h.t.Helper()
	held := m.View().WakeWaiting
	if held == nil || held.Kind != kind || held.At.Before(until) {
		h.t.Fatalf("%s holds wake %+v under the gateway's hold, want a %s wake due no earlier than the hold's end %s",
			m.Claim().Token, held, kind, until)
	}
	h.wantStarts(m.Claim().Token, started)
	return *held
}

// A launch raised while the hold stands waits for the hold's end, even with the pacing's burst
// unspent, and launches there.
func TestHoldStopsStartsALaunchUntilTheHoldEnds(t *testing.T) {
	h, hold := holdHarness(t)
	until := h.holdFor(hold, gatewayLease)

	h.must(RequestSpawn{Claim: h.token})

	h.wantState(StateLaunching)
	waiting := wantHeldUntil(h, h.m, WakeLaunch, until, 0)
	if !waiting.At.Equal(until) {
		t.Fatalf("the launch is due at %s, want the hold's end %s: the burst was unspent", waiting.At, until)
	}
	if lines := h.logs.lines(WakeHeldMessage, "claim="+string(h.token)); len(lines) != 1 {
		t.Fatalf("the held lines are %v, want one", lines)
	}
	h.advance(until.Sub(h.clock.Now()) - time.Nanosecond)
	h.wantStarts(h.token, 0)
	h.advance(time.Nanosecond)
	h.wantStarts(h.token, 1)
	h.wantWakeLine(h.token, "kind=launch", "waited=true", "waitedFor="+gatewayLease.String())
}

// The stale-plugin lane's relaunch is held as a launch is, under its own kind.
func TestHoldStopsStartsAPluginRefreshUntilTheHoldEnds(t *testing.T) {
	h, hold := holdHarness(t)
	h.reach(StateReady)
	h.m.deps.PluginContract = refreshContract
	until := h.holdFor(hold, gatewayLease)

	h.must(StalePlugin{Claim: h.token, Trigger: "sweep"})

	waiting := wantHeldUntil(h, h.m, WakePluginRefresh, until, 1)
	h.advance(waiting.At.Sub(h.clock.Now()) - time.Nanosecond)
	h.wantStarts(h.token, 1)
	h.advance(time.Nanosecond)
	h.wantStarts(h.token, 2)
	h.wantWakeLine(h.token, "kind=plugin-refresh", "waited=true")
}

// A turn the caller reserved before the hold began that falls inside it — the boot's relaunch of
// an unfinished launch (RequestSpawn.Turn) — is reserved again past the hold.
func TestHoldStopsStartsACarriedTurnThatFallsInsideTheHold(t *testing.T) {
	h, hold := holdHarness(t)
	turn := h.clock.Now().Add(10 * time.Second)
	until := h.holdFor(hold, gatewayLease)

	h.must(RequestSpawn{Claim: h.token, Turn: turn})

	wantHeldUntil(h, h.m, WakeLaunch, until, 0)
	h.advance(turn.Sub(h.clock.Now()))
	h.wantStarts(h.token, 0)
}

// The gateway renews its lease while the hold stands, so a wake reserved for the first lease's
// end comes due inside the hold: it launches nothing and is put back past the renewed end, at a
// fresh turn, and launches there.
func TestHoldStopsStartsAWakeWhoseTurnComesInsideARenewedHold(t *testing.T) {
	h, hold := holdHarness(t)
	first := h.holdFor(hold, gatewayLease)
	h.must(RequestSpawn{Claim: h.token})
	wantHeldUntil(h, h.m, WakeLaunch, first, 0)

	h.advance(time.Minute)
	renewed := h.holdFor(hold, gatewayLease)
	h.advance(first.Sub(h.clock.Now()))

	h.wantStarts(h.token, 0)
	waiting := wantHeldUntil(h, h.m, WakeLaunch, renewed, 0)
	if lines := h.logs.lines(WakeHeldMessage, "claim="+string(h.token)); len(lines) != 2 {
		t.Fatalf("the held lines are %v, want two: the gate's and the turn's", lines)
	}
	h.advance(waiting.At.Sub(h.clock.Now()))
	h.wantStarts(h.token, 1)
}

// The wakes a hold held leave from its end at the pace the operator set, one at a time: the
// first at the end, each next one an interval and the jitter's part after it. The end of a hold
// is not a burst.
func TestHoldStopsStartsAndReleasesAtThePaceOneAtATime(t *testing.T) {
	h, hold := holdHarness(t)
	until := h.holdFor(hold, gatewayLease)
	machines := []*Machine{h.m}
	for i := range 3 {
		machines = append(machines, h.machineFor(wakeClaim(waveToken(i), StateQueued)))
	}
	for _, m := range machines {
		h.mustPostTo(m, RequestSpawn{Claim: m.Claim().Token})
	}

	for i, m := range machines {
		want := until.Add(time.Duration(i) * (wakeInterval + wakePart))
		if held := m.View().WakeWaiting; held == nil || !held.At.Equal(want) {
			t.Fatalf("wake %d is %+v, want due at %s", i, held, want)
		}
	}
	for i, m := range machines {
		at := until.Add(time.Duration(i) * (wakeInterval + wakePart))
		h.clock.Advance(at.Sub(h.clock.Now()) - time.Nanosecond)
		for _, other := range machines {
			other.Wait()
		}
		started := 0
		for _, other := range machines {
			started += h.starts(other.Claim().Token)
		}
		if started != i {
			t.Fatalf("just before wake %d's turn %d claims have launched, want %d", i, started, i)
		}
		h.clock.Advance(time.Nanosecond)
		m.Wait()
		h.wantStarts(m.Claim().Token, 1)
	}
}

// The project controller launches at once under the hold, as it does under the pacing.
func TestHoldStopsStartsNothingOfTheController(t *testing.T) {
	h, hold := holdHarness(t)
	h.holdFor(hold, gatewayLease)
	token := claim.ControllerToken("legion")
	controller := h.machineFor(Claim{Token: token, Project: "legion", Role: claim.RoleController, State: StateQueued})

	h.mustPostTo(controller, RequestSpawn{Claim: token})

	h.wantStarts(token, 1)
	h.wantWakeLine(token, "waited=false", "paced=false")
	if lines := h.logs.lines(WakeHeldMessage); len(lines) != 0 {
		t.Fatalf("held lines %v, want none", lines)
	}
}

// A request a person waits on — the operator's spawn, resume and retry (exemptWake) — launches
// at once under the hold.
func TestHoldStopsStartsNothingAPersonWaitsOn(t *testing.T) {
	t.Run("spawn", func(t *testing.T) {
		h, hold := holdHarness(t)
		h.holdFor(hold, gatewayLease)
		h.must(RequestSpawn{Claim: h.token, Exempt: true})
		h.wantStarts(h.token, 1)
	})
	t.Run("resume", func(t *testing.T) {
		h, hold := holdHarness(t)
		h.reach(StateSuspended)
		h.holdFor(hold, gatewayLease)
		h.must(RequestResume{Claim: h.token})
		h.wantStarts(h.token, 2)
	})
	t.Run("retry", func(t *testing.T) {
		h, hold := holdHarness(t)
		second := h.machineFor(wakeClaim(secondToken, StateFailed))
		h.holdFor(hold, gatewayLease)
		h.mustPostTo(second, RequestOperatorRetry{Claim: secondToken})
		h.wantStarts(secondToken, 1)
	})
}

// A running role keeps its turn and is given its next task under the hold: the hold holds new
// sessions, not the work of one already running.
func TestHoldStopsStartsNothingOfARunningRolesTurn(t *testing.T) {
	h, hold := holdHarness(t)
	h.reach(StateIdle)
	sent := len(h.prompts())
	h.holdFor(hold, gatewayLease)

	h.must(RequestDeliver{Claim: h.token, ID: "row-7", Task: "review the change"})
	h.must(StreamTurnStart{Claim: h.token})

	h.wantPrompts(sent + 1)
	h.wantState(StateWorking)
	h.wantStarts(h.token, 1)
}

// A launch a project pause held, let out while the gateway's hold stands, is let out to a turn
// past the hold rather than at once.
func TestHoldStopsStartsALaunchAPauseLetsOutUnderTheHold(t *testing.T) {
	hold := backpressure.New("team-a", nil)
	h, flag, _ := pauseHarness(t, queuedClaim(), pacedDeps(wakeRate), func(d *Deps) { d.WakePace.Hold = hold })
	h.start(queuedClaim())
	flag.set(7)
	h.must(RequestSpawn{Claim: h.token})
	h.wantStarts(h.token, 0)
	until := h.holdFor(hold, gatewayLease)
	flag.set(0)

	h.must(ResumePause{Claim: h.token, Pause: 7})

	waiting := wantHeldUntil(h, h.m, WakeLaunch, until, 0)
	h.advance(waiting.At.Sub(h.clock.Now()))
	h.wantStarts(h.token, 1)
}

// With no hold source a paced launch is decided as it always was: the burst lets the first out at
// once. This is the row that tells the rows above apart from a harness that holds everything.
func TestHoldStopsStartsNothingWithNoHoldSource(t *testing.T) {
	h := pacedHarness(t, wakeRate)
	h.must(RequestSpawn{Claim: h.token})
	h.wantStarts(h.token, 1)
}
