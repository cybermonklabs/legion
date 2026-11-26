package supervise

import (
	"log/slog"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/runtime"
)

// controllerStart is the task the controller keeper hands every launch of the controller at its
// ready (internal/daemon's ControllerStartMessage), for the prompts these tests read.
const controllerStart = "Legion controller start: follow skill://legion-controller's start procedure now, then end the turn."

// controllerUp is the project controller's claim brought up on its session and through one
// finished turn, which makes it a session that has worked (SessionRun.TurnFinished): the shape
// every replaced session had in the outages of dispatch://INFRA-2045.
func controllerUp(t *testing.T) *harness {
	t.Helper()
	h := newHarnessOf(t, controllerClaim())
	h.reach(StateIdle)
	if run := h.claim().SessionRun; !run.TurnFinished || run.Ends != 0 {
		t.Fatalf("the controller after its first finished turn has the run %+v, want a finished turn and no end", run)
	}
	return h
}

// dies is the runtime reporting the claim's process gone, as it reports a process killed for
// memory.
func (h *harness) dies() {
	h.t.Helper()
	h.observe(runtime.Gone)
}

// startsSoFar is how many processes the runtime was asked to resume and to spawn for the claim.
func (h *harness) startsSoFar() (resumes, spawns int) {
	return len(h.calls("Resume")), len(h.calls("Spawn"))
}

// wantResumedSince is the claim's latest launch a resume of session, and the only start since
// the counts given: no fresh agent was spawned.
func (h *harness) wantResumedSince(resumes, spawns int, session string) {
	h.t.Helper()
	gotResumes, gotSpawns := h.startsSoFar()
	if gotResumes != resumes+1 || gotSpawns != spawns {
		h.t.Fatalf("the runtime was asked for %d resumes and %d spawns since, want one resume and no spawn", gotResumes-resumes, gotSpawns-spawns)
	}
	if c := h.claim(); c.Session != session {
		h.t.Fatalf("the claim records session %q after its relaunch, want %q resumed", c.Session, session)
	}
}

// wantFreshSince is the claim's latest launch a fresh agent in place of old, and the only start
// since the counts given: a spawn that resumes nothing, with the old session recorded as the one
// the claim left behind and why.
func (h *harness) wantFreshSince(resumes, spawns int, old string) {
	h.t.Helper()
	gotResumes, gotSpawns := h.startsSoFar()
	if gotResumes != resumes || gotSpawns != spawns+1 {
		h.t.Fatalf("the runtime was asked for %d resumes and %d spawns since, want one spawn and no resume", gotResumes-resumes, gotSpawns-spawns)
	}
	spawned := h.calls("Spawn")[gotSpawns-1].Spec
	if spawned.ResumeSessionFile != "" || spawned.ResumeSession != "" {
		h.t.Fatalf("the fresh launch resumes %q / %q, want nothing resumed", spawned.ResumeSessionFile, spawned.ResumeSession)
	}
	c := h.claim()
	if c.Session != "" || c.SessionFile != "" || c.Abandoned.Session != old || !strings.Contains(c.Abandoned.Why, "ended before it finished a turn") {
		h.t.Fatalf("the claim records session %q (%s), abandoned %+v; want no session and %q left behind for its ended launches",
			c.Session, c.SessionFile, c.Abandoned, old)
	}
	if stored := h.store.load(h.token); stored.Session != "" || stored.Abandoned.Session != old {
		h.t.Fatalf("the store records session %q, abandoned %q; want the replacement written", stored.Session, stored.Abandoned.Session)
	}
}

// wantEnds is the count of the session's run, in the machine and in the store.
func (h *harness) wantEnds(want int) {
	h.t.Helper()
	for where, got := range map[string]SessionRun{"the machine": h.claim().SessionRun, "the store": h.store.load(h.token).SessionRun} {
		if got.Ends != want {
			h.t.Fatalf("%s counts %d ends of the session's launches (%+v), want %d", where, got.Ends, got, want)
		}
	}
}

// After two launches of one controller session in a row end before the session finishes a turn,
// the next launch starts a fresh session (dispatch://INFRA-2045 R1). An end is a death the
// machine charges, before or after the controller says ready; a finished turn resets the count,
// and ready alone does not; an end the machine charges nothing is not counted; the count is the
// claim's record, so a daemon restart keeps it; and a session that has never finished a turn is
// not replaced again. The control: on main every row that wants a fresh session fails, because
// Machine.died resumes the recorded session at every relaunch.
func TestAControllerSessionIsReplacedAfterTwoLaunchesEndBeforeAFinishedTurn(t *testing.T) {
	t.Run("a: a resumed session dies before ready twice", func(t *testing.T) {
		h := controllerUp(t)
		// An operator's suspension and resume is a launch the machine charges nothing for: the
		// resumed session starts with no end counted.
		h.must(RequestSuspend{Claim: h.token, By: SuspendedByOperator})
		h.must(RequestResume{Claim: h.token})
		h.wantState(StateLaunching)
		h.wantEnds(0)

		h.dies()
		h.wantEnds(1)
		resumes, spawns := h.startsSoFar()
		h.wantState(StateLaunching)
		h.dies()

		h.wantFreshSince(resumes, spawns, session)
		h.wantState(StateLaunching)
		h.registerFresh()
		if c := h.claim(); c.Session != replacingSession {
			t.Fatalf("the fresh launch registered as %q, want a session id of its own, %q", c.Session, replacingSession)
		}
	})

	t.Run("b: a resumed session says ready and dies with its start message waiting, twice", func(t *testing.T) {
		h := controllerUp(t)
		h.must(RequestDeliver{Claim: h.token, Task: controllerStart})
		h.dies()
		h.wantEnds(1)
		h.relaunched()
		if p := h.claim().Pending; p == nil || p.Task != controllerStart {
			t.Fatalf("the relaunched controller holds %+v, want its start message still waiting", p)
		}
		resumes, spawns := h.startsSoFar()
		h.dies()

		h.wantFreshSince(resumes, spawns, session)
	})

	t.Run("c: ready alone does not reset the count", func(t *testing.T) {
		h := controllerUp(t)
		h.dies()
		h.relaunched()
		h.wantState(StateReady)
		h.wantEnds(1)
		resumes, spawns := h.startsSoFar()
		h.dies()

		h.wantFreshSince(resumes, spawns, session)
	})

	t.Run("d: a finished turn resets the count", func(t *testing.T) {
		h := controllerUp(t)
		h.dies()
		h.relaunched()
		h.must(RequestDeliver{Claim: h.token, Task: controllerStart})
		h.must(StreamTurnStart{Claim: h.token})
		h.must(StreamTurnEnd{Claim: h.token})
		h.wantEnds(0)
		resumes, spawns := h.startsSoFar()
		h.dies()

		h.wantResumedSince(resumes, spawns, session)
		h.wantEnds(1)
	})

	t.Run("e: the stale-plugin relaunch and an operator's suspension are not counted", func(t *testing.T) {
		h := controllerUp(t)
		h.deps.PluginRelaunchable = true
		h.restart()
		h.dies()
		h.wantEnds(1)

		// The relaunched process registers on an older plugin, and the machine relaunches it
		// onto the current one, charging nothing.
		resumes, spawns := h.startsSoFar()
		h.connect()
		if err := h.handle(RequestRegister{Claim: h.token, Generation: h.generation(), Session: session, SessionFile: sessionFile,
			CapabilityHash: []byte("capability"), PluginContract: h.deps.PluginContract - 1}); err == nil {
			t.Fatal("the stale registration was accepted, want it refused and relaunched")
		}
		h.wantResumedSince(resumes, spawns, session)
		h.wantEnds(1)

		// An operator suspends the claim and resumes it.
		h.relaunched()
		h.must(RequestSuspend{Claim: h.token, By: SuspendedByOperator})
		h.wantState(StateSuspended)
		resumes, spawns = h.startsSoFar()
		h.must(RequestResume{Claim: h.token})

		h.wantResumedSince(resumes, spawns, session)
		h.wantEnds(1)
	})

	t.Run("g: a daemon restart between the two ends keeps the count", func(t *testing.T) {
		h := controllerUp(t)
		h.dies()
		h.relaunched()
		h.wantEnds(1)

		h.restart()
		h.wantEnds(1)
		resumes, spawns := h.startsSoFar()
		h.dies()

		h.wantFreshSince(resumes, spawns, session)
	})

	t.Run("f: a fresh session that never finished a turn is not replaced again", func(t *testing.T) {
		h := controllerUp(t)
		h.dies()
		h.relaunched()
		h.dies()
		h.registerFresh()
		h.must(RequestDeliver{Claim: h.token, Task: controllerStart})

		h.dies()
		resumes, spawns := h.startsSoFar()
		h.dies()

		h.wantResumedSince(resumes, spawns, replacingSession)
		h.wantEnds(2)
		lines := h.records.of(sessionNotReplacedLine)
		if len(lines) != 1 || lines[0].level != slog.LevelError {
			t.Fatalf("the lines that say the session is not replaced are %+v, want one at Error", lines)
		}
		if got, _ := lines[0].attr("session"); got.String() != replacingSession {
			t.Errorf("the Error line names session %q, want the fresh one, %q", got, replacingSession)
		}
		// The fresh session started in place of the first one, and the line names it.
		if got, ok := lines[0].attr("oldSession"); !ok || got.String() != session {
			t.Errorf("the Error line names oldSession %q (present %t), want %q", got, ok, session)
		}

		// The launch budget runs out, the claim fails, and the keeper's retry (RequestRetry, made
		// on the keeper's waits) resumes the same fresh session.
		for h.state() != StateFailed {
			h.dies()
		}
		resumes, spawns = h.startsSoFar()
		h.must(RequestRetry{Claim: h.token})
		h.wantResumedSince(resumes, spawns, replacingSession)
		if lines := h.records.of(sessionNotReplacedLine); len(lines) != 1 {
			t.Errorf("%d lines say the session is not replaced, want the one", len(lines))
		}
		if lines := h.records.of(sessionReplacedLine); len(lines) != 1 {
			t.Errorf("%d replacement lines, want the one of the first session", len(lines))
		}
	})
}

// A replacement under R1 is logged, and the fresh session is told (dispatch://INFRA-2045 R4): one
// Warn line names the claim, the old session, the reason and how each of the two launches ended;
// one line at the fresh session's ready names the old and the new session; and the fresh
// session's first prompt names the old session and the reason, and says that messages sent to the
// old session may be lost, before the start message. The replacement under R3 is
// replaceAgedSession's; agedsession_test.go and internal/daemon's test of the same name read it.
// The control: on main no replacement of this kind exists, so none of the three is written.
func TestAControllerSessionReplacementIsLoggedAndTold(t *testing.T) {
	h := controllerUp(t)
	// The first launch dies after the controller said ready, and its relaunch dies before the
	// relaunched controller says it.
	h.dies()
	h.wantState(StateLaunching)
	h.dies()

	lines := h.records.of(sessionReplacedLine)
	if len(lines) != 1 || lines[0].level != slog.LevelWarn {
		t.Fatalf("the replacement lines are %+v, want one at Warn", lines)
	}
	line := lines[0]
	for key, want := range map[string]string{"claim": string(h.token), "session": session, "requirement": "R1"} {
		if got, ok := line.attr(key); !ok || got.String() != want {
			t.Errorf("the replacement line's %s is %q, want %q", key, got, want)
		}
	}
	reason, _ := line.attr("reason")
	endings, _ := line.attr("endings")
	for _, ended := range []string{"died after the controller was ready", "died before the controller was ready"} {
		if !strings.Contains(endings.String(), ended) || !strings.Contains(reason.String(), ended) {
			t.Errorf("the replacement line names endings %q and reason %q, want each to say the launch %s", endings, reason, ended)
		}
	}

	h.registerFresh()
	ready := h.records.of(freshSessionReadyLine)
	if len(ready) != 1 {
		t.Fatalf("the fresh session's ready lines are %+v, want one", ready)
	}
	for key, want := range map[string]string{"claim": string(h.token), "oldSession": session, "newSession": replacingSession} {
		if got, ok := ready[0].attr(key); !ok || got.String() != want {
			t.Errorf("the ready line's %s is %q, want %q", key, got, want)
		}
	}

	base := len(h.prompts())
	h.must(RequestDeliver{Claim: h.token, Task: controllerStart})
	first := h.wantPrompts(base + 1)[base].Message
	for _, want := range []string{
		strconv.Quote(session), "was replaced", "ended before it finished a turn", "died before the controller was ready",
		"Envoy messages sent to that session may be lost",
	} {
		if !strings.Contains(first, want) {
			t.Errorf("the fresh session's first prompt does not say %q:\n%s", want, first)
		}
	}
	if !strings.HasSuffix(first, "\n\n"+controllerStart) {
		t.Errorf("the fresh session's first prompt is %q, want the replacement told before the start message", first)
	}

	// The fresh session dies before its first prompt's turn, and comes back: the ready line is
	// written once for the replacement, not at each ready until the replacement is told.
	h.dies()
	h.registerFresh()
	if ready := h.records.of(freshSessionReadyLine); len(ready) != 1 {
		t.Fatalf("the fresh session's ready lines after its relaunch are %d, want the one", len(ready))
	}
}

// A controller record an earlier release wrote can hold an abandoned session that was already told
// (an operator's forget-session, say) and no ReadyNamed mark, which no migration writes. Its next
// ready is no fresh session's ready, and writes no line that names a replacement
// (dispatch://INFRA-2045 R4). The control: with the line written for any abandoned session that
// carries no mark, this record writes it.
func TestAToldReplacementAnEarlierReleaseRecordedNamesNoFreshSession(t *testing.T) {
	c := controllerClaim()
	c.Abandoned = AbandonedSession{Session: "ses_forgotten_by_hand", Why: "an operator cleared it with `legion claims forget-session`, so this claim starts fresh",
		At: epoch.Add(-time.Hour), Told: true}
	h := newHarnessOf(t, c)
	h.reach(StateReady)
	if ready := h.records.of(freshSessionReadyLine); len(ready) != 0 {
		t.Fatalf("the ready of a controller whose replacement was told before this release wrote %+v, want no line", ready)
	}
}
