package supervise

import (
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/runtime/fake"
)

// The sink tests of clear_text_logging_test.go live in package supervise_test, because they build
// the error a refused mint leaves from appauth and runtime/sandbox themselves, and both import this
// package (runtime/sandbox directly, appauth through config and record), which an in-package test
// could not do without an import cycle. They drive this package's machine harness
// (harness_test.go) as its own tests do. The names below are what they reach for across that
// boundary, and nothing more: they are part of no package's surface but the test binary's.

// Harness is the machine harness the package's tests drive a claim with (harness).
type Harness = harness

// QueuedClaim is the harness's queued claim (queuedClaim).
var QueuedClaim = queuedClaim

// EndFailedCause is the cause a failure's let-go records what it ended under (endFailed).
var EndFailedCause = endFailed

// ErrHoldUnanswered is a session hold the store did not answer (errHoldUnanswered), which a launch
// tries again charging nothing.
var ErrHoldUnanswered = errHoldUnanswered

// NewHarness is newHarnessOf over the queued claim c, with each of set applied to its Deps before
// its machine is built from them: a test's own logger, say, or a launch budget of its own.
func NewHarness(t *testing.T, c Claim, set ...func(*Deps)) *Harness {
	t.Helper()
	h := newBareHarness(t)
	h.token = c.Token
	for _, s := range set {
		s(&h.deps)
	}
	if err := h.store.PutClaim(h.ctx, c); err != nil {
		t.Fatal(err)
	}
	h.start(c)
	return h
}

// Runtime is the harness's runtime double, for a test that scripts its answers.
func (h *harness) Runtime() *fake.Runtime { return h.rt }

// Machine is the harness's machine, for a test that reads its claim or posts an event under a
// context of its own.
func (h *harness) Machine() *Machine { return h.m }

// Handle is handle: one event, and the decision it starts waited for.
func (h *harness) Handle(ev Event) error { return h.handle(ev) }

// Must is must: Handle, failing the test on an error.
func (h *harness) Must(ev Event) {
	h.t.Helper()
	h.must(ev)
}

// WantState is wantState: the claim is in want, or the test fails.
func (h *harness) WantState(want ClaimState) {
	h.t.Helper()
	h.wantState(want)
}

// WantFailure is wantFailure: the claim's failure record is cause, a sentence naming detail, and a
// moment between since and the harness's clock now.
func (h *harness) WantFailure(cause FailureCause, detail string, since time.Time) {
	h.t.Helper()
	h.wantFailure(cause, detail, since)
}

// Now is the harness's clock now, for a test that bounds a moment the machine records.
func (h *harness) Now() time.Time { return h.clock.Now() }

// Reach is reach: the claim walked from where it is to state.
func (h *harness) Reach(state ClaimState) {
	h.t.Helper()
	h.reach(state)
}

// Advance is advance: the harness's clock moved by d, with what falls due fired.
func (h *harness) Advance(d time.Duration) { h.advance(d) }

// ReportActivity is reportActivity: the claim's process reporting the background work kinds name.
func (h *harness) ReportActivity(kinds ...string) {
	h.t.Helper()
	h.reportActivity(kinds...)
}

// Relaunched is relaunched: a claim the machine has just relaunched walked back to ready.
func (h *harness) Relaunched() {
	h.t.Helper()
	h.relaunched()
}

// Observe is observe: the runtime's observation of kind of the claim's process.
func (h *harness) Observe(kind runtime.ObservationKind) {
	h.t.Helper()
	h.observe(kind)
}

// RegisterFresh is registerFresh: the agent that replaced an abandoned session brought up to
// ready under a session of its own.
func (h *harness) RegisterFresh() {
	h.t.Helper()
	h.registerFresh()
}

// Fail is the machine's fail of the claim under cause and why, made under the machine's lock as a
// decision holds it and ended with unlock, as a decision ends, and the decision it starts waited
// for. It is for the sink tests of a let-go's record, which no decision of the machine reaches
// with a runtime's error while the claim holds a process (clear_text_logging_test.go says why).
func (h *harness) Fail(cause FailureCause, why string) error {
	h.m.mu.Lock()
	err := h.m.fail(h.ctx, cause, why)
	h.m.unlock()
	h.m.Wait()
	return err
}
