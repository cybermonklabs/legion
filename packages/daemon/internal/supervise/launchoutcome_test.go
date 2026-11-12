package supervise

// R6 of dispatch://INFRA-2883, as this package decides it: one closed set of launch outcomes
// (LaunchEnding) and one decision over it (chargesLaunchBudget), read at the one site that
// charges the launch budget (relaunchAfterFailure). What the daemon reads each outcome from —
// the registrations and readies its own API refused, the moment its routes first served, and the
// run of generations it has already waived — is internal/api's, and the rows that drive those
// readings are in internal/api's own tests; these rows drive the decision itself, over a daemon
// that answers each outcome in turn.

import (
	"testing"

	"github.com/sjawhar/legion/daemon/internal/runtime"
)

// waived is one call of Deps.LaunchWaived: the claim and the generation whose charge was waived.
type waived struct {
	token      string
	generation uint64
}

// launchEndings is a daemon that reads every ending as answer, recording the waivers the machine
// then tells it of (Deps.LaunchWaived). It is the whole of what internal/supervise needs of the
// daemon: one answer in, one record out.
type launchEndings struct {
	answer func(c Claim, limit int) LaunchEnding
	// limits is the limit the machine passed with each reading, so a row can hold the machine to
	// passing its own configured bound rather than one of this package's keeping.
	limits []int
	waived []waived
}

func (e *launchEndings) ended(c Claim, limit int) LaunchEnding {
	e.limits = append(e.limits, limit)
	return e.answer(c, limit)
}

func (e *launchEndings) record(c Claim) {
	e.waived = append(e.waived, waived{token: string(c.Token), generation: c.Generation})
}

// newEndingHarness is a machine over a queued claim whose daemon answers every launch ending with
// answer; a nil answer is a daemon that reads no endings at all (Deps.LaunchEnded nil), which is
// every daemon before this change and every test harness that wires none.
func newEndingHarness(t *testing.T, answer func(c Claim, limit int) LaunchEnding) (*harness, *launchEndings) {
	t.Helper()
	h := newBareHarness(t)
	endings := &launchEndings{answer: answer}
	if answer != nil {
		h.deps.LaunchEnded = endings.ended
	}
	h.deps.LaunchWaived = endings.record
	c := queuedClaim()
	h.token = c.Token
	if err := h.store.PutClaim(h.ctx, c); err != nil {
		t.Fatal(err)
	}
	h.start(c)
	return h, endings
}

// A launch that ended with no refusal of the daemon's behind it is charged exactly as it was
// before the waiver existed: its claim's launch budget runs out and the claim fails, which is
// what holds the issue and tells the architect. Both readings of such an ending are driven — a
// daemon that answers nothing about its endings, which is a Deps with neither field, and one
// that answers this ending ordinary — because the first is what every caller that wires none
// gets and the second is what the shipped daemon answers for a launch it served.
func TestALaunchThatDiedWithNoRefusalIsChargedAsBefore(t *testing.T) {
	for name, answer := range map[string]func(c Claim, limit int) LaunchEnding{
		"a daemon that reads no endings at all": nil,
		"an ending the daemon reads as ordinary": func(Claim, int) LaunchEnding {
			return LaunchEndedOrdinary
		},
	} {
		t.Run(name, func(t *testing.T) {
			h, endings := newEndingHarness(t, answer)
			limit := testLimits().LaunchFailures
			h.launch()

			for range limit {
				h.observe(runtime.Gone)
			}

			h.wantState(StateFailed)
			h.wantBudgets(Budgets{LaunchFailures: limit})
			h.wantCalls("Spawn", limit)
			if len(endings.waived) != 0 {
				t.Fatalf("waived %+v, want nothing waived for a launch no refusal ended", endings.waived)
			}
			if stored := h.store.load(testToken); stored.State != StateFailed || stored.Budgets.LaunchFailures != limit {
				t.Fatalf("stored claim %s with budgets %+v, want it failed at %d launch failures",
					stored.State, stored.Budgets, limit)
			}
		})
	}
}

// A launch the daemon's own refusal ended is relaunched at no charge, told to the daemon — which
// is what moves the run of waived generations and forgets the refusal of a generation that is
// over — and logged in one line naming the outcome and the claim. The line is what an operator
// reading a claim that keeps relaunching has to tell the waiver from a charge by, so it carries
// both names; the charge writes its own line beside it, and neither launch goes unlogged.
//
// It is driven past the launch failure limit, since what the waiver is for is a claim whose
// generations the daemon ends faster than that budget: charged, the third of them would have
// failed the claim.
func TestTheWaiverIsRecordedAndLogged(t *testing.T) {
	h, endings := newEndingHarness(t, func(Claim, int) LaunchEnding { return LaunchEndedAfterRefusal })
	limit := testLimits().LaunchFailures
	h.launch()

	for range limit + 1 {
		h.observe(runtime.Gone)
	}

	h.wantState(StateLaunching)
	h.wantBudgets(Budgets{})
	h.wantCalls("Spawn", limit+2)
	want := []waived{}
	for generation := range uint64(limit + 1) {
		want = append(want, waived{token: string(testToken), generation: generation + 1})
	}
	if len(endings.waived) != len(want) {
		t.Fatalf("waived %+v, want one record per waived generation %+v", endings.waived, want)
	}
	for i, record := range endings.waived {
		if record != want[i] {
			t.Fatalf("waiver %d = %+v, want %+v", i, record, want[i])
		}
	}
	for _, limitRead := range endings.limits {
		if limitRead != limit {
			t.Fatalf("the daemon was asked with limit %d, want the machine's own %d", limitRead, limit)
		}
	}
	lines := h.records.of("supervise: waived the launch failure of a launch this daemon did not answer")
	if len(lines) != limit+1 {
		t.Fatalf("logged %d waiver lines, want one for each of the %d waived launches", len(lines), limit+1)
	}
	ending, named := lines[0].attr("ending")
	claimed, onClaim := lines[0].attr("claim")
	if !named || ending.String() != LaunchEndedAfterRefusal.String() || !onClaim || claimed.String() != string(testToken) {
		t.Fatalf("the waiver line names ending %v and claim %v, want %q and %s",
			ending, claimed, LaunchEndedAfterRefusal, testToken)
	}
	if charged := h.records.of("supervise: charged a launch failure"); len(charged) != 0 {
		t.Fatalf("logged %d charge lines, want none: every one of these launches was waived", len(charged))
	}
}

// An ending outside the closed set is charged, and its log line names it for what it is. Nothing
// produces such a value today: it is the decision's default, and what it is for is the change
// that adds an outcome and forgets the decision — a policy that exempted what it could not name
// would relaunch a crash-looping process for ever, which is worse than the defect the waiver
// fixes (dispatch://INFRA-2883/comment/2a5ed273-3c5c-4860-9b39-7690718ce9ad).
func TestAnEndingTheClosedSetCannotNameIsChargedAllTheSame(t *testing.T) {
	unnamed := LaunchEnding(42)
	h, endings := newEndingHarness(t, func(Claim, int) LaunchEnding { return unnamed })
	limit := testLimits().LaunchFailures
	h.launch()

	for range limit {
		h.observe(runtime.Gone)
	}

	h.wantState(StateFailed)
	h.wantBudgets(Budgets{LaunchFailures: limit})
	if len(endings.waived) != 0 {
		t.Fatalf("waived %+v, want an ending the set cannot name charged rather than waived", endings.waived)
	}
	lines := h.records.of("supervise: charged a launch failure")
	if len(lines) != limit {
		t.Fatalf("logged %d charge lines, want %d", len(lines), limit)
	}
	if ending, named := lines[0].attr("ending"); !named || ending.String() != unnamed.String() {
		t.Fatalf("the charge line names ending %v, want %q", ending, unnamed)
	}
}
