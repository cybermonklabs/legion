package supervise

import (
	"fmt"
	"testing"
	"time"
)

// agedSession is an Oh My Pi session id (a UUIDv7) whose recorded start is age before now.
func agedSession(now time.Time, age time.Duration) string {
	ms := now.Add(-age).UnixMilli()
	return fmt.Sprintf("%08x-%04x-7abc-8def-0123456789ab", ms>>16, ms&0xffff)
}

// withSessionAge turns the controller's age rule on in h's machine and in every machine a restart
// builds (Deps.ControllerSessionMaxAge, dispatch://INFRA-2045 R3).
func (h *harness) withSessionAge(maxAge time.Duration) {
	h.t.Helper()
	h.deps.ControllerSessionMaxAge = maxAge
	h.m.deps.ControllerSessionMaxAge = maxAge
}

// upOn walks h's queued claim to ready on the session id given: its launch, its shim, a
// registration and a ready that name that session.
func (h *harness) upOn(id string) {
	h.t.Helper()
	h.launch()
	h.wantState(StateLaunching)
	h.connect()
	h.must(RequestRegister{Claim: h.token, Generation: h.generation(), Session: id, SessionFile: sessionFile,
		CapabilityHash: []byte("capability"), PluginContract: h.deps.PluginContract})
	h.must(RequestReady{Claim: h.token, Generation: h.generation(), Session: id})
	h.wantState(StateReady)
}

// The guards of R3 a controller harness alone cannot hold (dispatch://INFRA-2045 R3): the rule is
// the project controller's, so a worker's session older than the age is resumed at its relaunch
// with no R3 line; and the line for a controller session id that is no UUIDv7 is written once for
// that session, however many relaunches resume it. The control: without the role guard the
// worker's relaunch spawns a fresh agent, and without the dedupe the second relaunch writes a
// second line.
func TestTheAgeRuleIsTheControllersAndSaysOnceItCannotReadAnAge(t *testing.T) {
	t.Run("a worker's session 73 hours old is resumed at its relaunch", func(t *testing.T) {
		h := newHarness(t)
		h.withSessionAge(72 * time.Hour)
		old := agedSession(h.clock.Now(), 73*time.Hour)
		h.upOn(old)
		resumes, spawns := h.startsSoFar()
		h.dies()

		h.wantResumedSince(resumes, spawns, old)
		if lines := h.records.of(sessionAgedOutLine); len(lines) != 0 {
			t.Fatalf("the worker's relaunch wrote %d R3 lines, want none", len(lines))
		}
	})

	t.Run("a controller session id that is no UUIDv7 is named once across two relaunches", func(t *testing.T) {
		h := controllerUp(t)
		h.withSessionAge(72 * time.Hour)
		resumes, spawns := h.startsSoFar()
		h.dies()
		h.wantResumedSince(resumes, spawns, session)
		h.relaunched()
		// A finished turn, so the second end is not R1's second in a row, which would replace the
		// session and ask R3 nothing.
		h.must(StreamTurnStart{Claim: h.token})
		h.must(StreamTurnEnd{Claim: h.token})
		resumes, spawns = h.startsSoFar()
		h.dies()
		h.wantResumedSince(resumes, spawns, session)
		h.relaunched()

		if lines := h.records.of(sessionUnagedLine); len(lines) != 1 {
			t.Fatalf("the lines for an id the rule reads no age off are %d over two relaunches, want one", len(lines))
		}
	})
}
