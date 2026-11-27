package supervise

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/runtime"
)

// The cases of this file are dispatch://INFRA-3916's R6: a phase task waits for another claim of
// its issue only while that claim's turn runs, or while a prompt of it is on its way to one
// (sendPending's hold, issueInTurn), and a hold that lasts more than issueHoldTold is told to the
// issue's architect once (heldForTurn, Deps.IssueHoldReached).

// heldTesterTask is the tester's phase task the cases below hold behind the implementer's turn.
const heldTesterTask = "test the branch"

// testerBot is the App identity the tester's task adopts the working copy as.
var testerBot = runtime.GitIdentity{Name: "legion-tester[bot]", Email: "7+legion-tester[bot]@users.noreply.github.com"}

// claimsInTurn answers Deps.IssueInTurn over the machines of the claims given, as the daemon
// answers it over every machine it supervises (internal/daemon's claimInTurn): a claim of the
// issue other than except whose published view is in a turn (ClaimView.InTurn). It reads each
// machine's view at the moment it is asked, so the answer moves with whatever those machines
// decided.
func claimsInTurn(claims ...*harness) func(string, claim.Token) (claim.Token, bool) {
	return func(issue string, except claim.Token) (claim.Token, bool) {
		for _, h := range claims {
			view := h.m.View()
			if view.Token == except || view.Issue != issue {
				continue
			}
			if view.InTurn() {
				return view.Token, true
			}
		}
		return "", false
	}
}

// readyTester is the tester of the implementer's issue, ready, with the dependencies set changes,
// whose machine reads who is in a turn through inTurn. Its deliveries adopt the working copy as
// testerBot, so its phase tasks are the tasks sendPending's hold reads.
func readyTester(t *testing.T, inTurn func(string, claim.Token) (claim.Token, bool), set ...func(*Deps)) *harness {
	t.Helper()
	h := newHarnessOf(t, testerClaim())
	h.deps.Identity = func(context.Context, claim.Role, string) (runtime.GitIdentity, error) { return testerBot, nil }
	h.deps.IssueInTurn = inTurn
	for _, change := range set {
		change(&h.deps)
	}
	h.restart()
	h.reach(StateReady)
	return h
}

// wantHeld fails the case unless the tester holds its task, unsent and with no adoption made,
// behind the implementer's claim, as the hold's own line names it.
func (h *harness) wantHeld(why string) {
	h.t.Helper()
	if prompts := h.prompts(); len(prompts) != 0 {
		h.t.Fatalf("%s: the tester was sent %+v, want its task held", why, prompts)
	}
	h.wantCalls("AdoptWorkingCopy", 0)
	if p := h.pending(); p.Prompted || !p.ConfirmedAt.IsZero() {
		h.t.Fatalf("%s: pending %+v, want the task held, unprompted", why, p)
	}
	if lines := h.logs.lines("supervise: a claim of this issue is in a turn", "inTurn="+string(testToken)); len(lines) == 0 {
		h.t.Fatalf("%s: no hold line names the implementer's claim %s", why, testToken)
	}
}

// wantSentOnce fails the case unless the tester's task went once, behind one adoption.
func (h *harness) wantSentOnce(why string) {
	h.t.Helper()
	prompts := h.prompts()
	if len(prompts) != 1 || !strings.HasSuffix(prompts[0].Message, heldTesterTask) {
		h.t.Fatalf("%s: the tester was sent %+v, want its task once", why, prompts)
	}
	if adoptions := h.wantCalls("AdoptWorkingCopy", 1); adoptions[0].Identity != testerBot {
		h.t.Fatalf("%s: adoptions = %+v, want one for %v", why, adoptions, testerBot)
	}
}

// refusedLoop is the plugin's report of one loop of the turn the claim's task runs that the
// provider refused, as pi-legion sent it before cybermonklabs/legion#5271 for a loop Oh My Pi
// retried: marked as a prompt's turn, with the 429 the gateway answered (cyberPluginTurn's shape).
func (h *harness) refusedLoop(sequence uint64) {
	h.t.Helper()
	h.must(RequestTurnOutcome{Claim: h.token, Generation: h.generation(), Sequence: sequence, Refused: true,
		Message: "429 rate_limit_error: synthetic refusal text", HTTPStatus: 429})
}

// The implementer's turn ends while the tester holds a `testing` task, and the tester's task goes
// at the next sweep, however the turn ended: by the stream's own turn end of a turn its provider
// refused, by a turn's end that left the implementer's next prompt on the provider-error backoff
// (TimerPromptRetry), and by a turn's end that left its stale-plugin relaunch waiting for the
// refresh gate. None of those is a turn that runs, and none is a send on its way to one. The
// plugin's report of a refused loop with no stream turn end is no end of the turn: Oh My Pi goes
// on with that run (OSSV-343 on 2026-11-16, Read 3 of dispatch://INFRA-3916/artifact/measurements-md),
// so the task stays held, as it does behind a turn nothing reported on at all.
//
// The rows of an ended turn pass at main; they hold that a claim whose stream turn ended names
// nothing to the hold, which a machine that kept such a claim published working or sending, or a
// rule of who is in a turn (ClaimView.InTurn, which the daemon's claimInTurn reads) that named it,
// would break. The row of the plugin's report holds that the report ends no hold.
func TestATurnEndedByAProviderRefusalHoldsNoOtherClaimsPhaseTask(t *testing.T) {
	for _, tc := range []struct {
		name string
		// implementer is the implementer, in a turn.
		implementer func(t *testing.T) *harness
		// end is what happens to that turn while the tester's task is held.
		end  func(t *testing.T, impl *harness)
		sent bool
	}{
		{
			name:        "the plugin's report of a refused loop alone, with no stream turn end",
			implementer: func(t *testing.T) *harness { return workingClaim(t) },
			end:         func(_ *testing.T, impl *harness) { impl.refusedLoop(1) },
		},
		{
			name: "the stream's turn end of a turn its provider refused",
			implementer: func(t *testing.T) *harness {
				impl := idleClaim(t)
				// A turn of the agent's own (a phase reminder), which no prompt of the daemon's ran.
				impl.must(StreamTurnStart{Claim: impl.token})
				return impl
			},
			end: func(t *testing.T, impl *harness) {
				impl.pluginTurn(1, true, 429)
				impl.mustQuiet(StreamTurnEnd{Claim: impl.token})
			},
			sent: true,
		},
		{
			name:        "a turn's end that leaves the implementer's next prompt on the provider-error backoff",
			implementer: func(t *testing.T) *harness { return workingClaim(t) },
			end: func(t *testing.T, impl *harness) {
				impl.errorTurn(StreamPromptResult{Error: "rate_limit_error", HTTPStatus: 429})
				if !impl.armed(TimerPromptRetry) {
					t.Fatal("the implementer's next prompt does not wait on the backoff after the provider error")
				}
			},
			sent: true,
		},
		{
			name: "a turn's end that leaves the implementer's stale-plugin relaunch waiting for the gate",
			implementer: func(t *testing.T) *harness {
				impl, gate := gatedHarness(t, wakeRate)
				impl.reach(StateWorking)
				impl.m.deps.PluginContract = refreshContract
				impl.mustRefreshPass(gate, "sweep")
				if impl.m.staleRelaunch == nil {
					t.Fatal("no relaunch waits for the implementer's turn")
				}
				// Every credit of the gate is out over other claims by the time the turn ends.
				gate.refusing(true)
				return impl
			},
			end: func(t *testing.T, impl *harness) {
				// One loop of the turn was refused and retried, and then the turn ended.
				impl.refusedLoop(1)
				impl.must(StreamTurnEnd{Claim: impl.token})
				impl.wantCalls("Resume", 0)
				if lines := impl.logs.lines("supervise: this claim's stale-plugin relaunch reached the refresh gate with no credit of its own and the gate is full"); len(lines) != 1 {
					t.Fatalf("the implementer's relaunch logged %v, want it waiting for the gate", lines)
				}
			},
			sent: true,
		},
		{
			name:        "the implementer's turn still running, with no report",
			implementer: func(t *testing.T) *harness { return workingClaim(t) },
			end:         func(*testing.T, *harness) {},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			impl := tc.implementer(t)
			tester := readyTester(t, claimsInTurn(impl))

			tester.must(RequestDeliver{Claim: testerToken, Task: heldTesterTask, Phase: phase.Testing, Generation: 5})
			tester.wantHeld("while the implementer's turn runs")

			tc.end(t, impl)
			tester.observe(runtime.Alive)

			if tc.sent {
				tester.wantSentOnce("at the sweep after the implementer's turn ended")
				return
			}
			tester.wantHeld("at the sweep while the implementer's turn still runs")
		})
	}
}

// holdTold is one call of Deps.IssueHoldReached as a case records it: the held claim, its task's
// delivery id and phase, the claim in the turn and how long the task had waited.
type holdTold struct {
	held   claim.Token
	task   string
	phase  phase.Phase
	inTurn claim.Token
	waited time.Duration
}

// holdCalls records the calls of Deps.IssueHoldReached, which the machine makes inside its
// decision.
type holdCalls struct {
	mu    sync.Mutex
	calls []holdTold
}

func (r *holdCalls) record(d *Deps) {
	d.IssueHoldReached = func(c Claim, inTurn claim.Token, held time.Duration) {
		r.mu.Lock()
		defer r.mu.Unlock()
		told := holdTold{held: c.Token, inTurn: inTurn, waited: held}
		if c.Pending != nil {
			told.task, told.phase = c.Pending.ID, c.Pending.Phase
		}
		r.calls = append(r.calls, told)
	}
}

func (r *holdCalls) told() []holdTold {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]holdTold(nil), r.calls...)
}

// holdLine is the Warn line a hold past issueHoldTold writes.
const holdLine = "supervise: a phase task has waited more than 10 minutes behind another claim of its issue in a turn"

// holdRig is the tester whose task the implementer's turn holds, with the switch that ends that
// turn and the record of what the machine told.
type holdRig struct {
	*harness
	implementing bool
	calls        *holdCalls
}

// newHoldRig is the tester, ready, whose machine reads the implementer's claim as in a turn while
// implementing is set.
func newHoldRig(t *testing.T) *holdRig {
	t.Helper()
	r := &holdRig{implementing: true, calls: &holdCalls{}}
	r.harness = readyTester(t, func(issue string, except claim.Token) (claim.Token, bool) {
		if issue != testerClaim().Issue || except != testerToken || !r.implementing {
			return "", false
		}
		return testToken, true
	}, r.calls.record)
	return r
}

// deliver queues the tester's phase task, which the implementer's turn holds.
func (r *holdRig) deliver() {
	r.t.Helper()
	r.must(RequestDeliver{Claim: testerToken, Task: heldTesterTask, Phase: phase.Testing, Generation: 5})
}

// waitAndSweep moves the clock on by d and runs the sweep's observation, which asks the hold again.
func (r *holdRig) waitAndSweep(d time.Duration) {
	r.t.Helper()
	r.advance(d)
	r.observe(runtime.Alive)
}

// wantTold fails the case unless the hold has written n Warn lines and made n calls, the last of
// them naming the tester's claim, its held task and the implementer's claim in the turn, after a
// wait of more than issueHoldTold.
func (r *holdRig) wantTold(n int, why string) {
	r.t.Helper()
	lines := r.logs.lines(holdLine)
	calls := r.calls.told()
	if len(lines) != n || len(calls) != n {
		r.t.Fatalf("%s: the hold wrote %d lines %v and made %d calls %+v, want %d of each", why, len(lines), lines, len(calls), calls, n)
	}
	if n == 0 {
		return
	}
	p := r.pending()
	last := calls[n-1]
	if last.held != testerToken || last.inTurn != testToken || last.task != p.ID || last.phase != phase.Testing || last.waited <= issueHoldTold {
		r.t.Fatalf("%s: the call was %+v, want it to name the tester %s, its testing task %s and the implementer %s after more than %s",
			why, last, testerToken, p.ID, testToken, issueHoldTold)
	}
	for _, want := range []string{"delivery=" + p.ID, "inTurn=" + string(testToken), "phase=testing"} {
		if !strings.Contains(lines[n-1], want) {
			r.t.Fatalf("%s: the line %q does not name %q", why, lines[n-1], want)
		}
	}
}

// A hold lasts while every sweep, hello, turn end or ready holds the task again, and the hold of
// more than issueHoldTold writes one Warn line and makes one call of Deps.IssueHoldReached naming
// the held claim, its task and the claim in the turn; it tells nothing more however long it then
// lasts. A task sent, dropped or replaced ends its hold, and a task held after it is a new hold
// whose time starts again. The hold is memory only: a machine rebuilt from the store during a
// hold starts its time again from the rebuild.
func TestAHoldOverTenMinutesTellsTheArchitectOnce(t *testing.T) {
	for _, tc := range []struct {
		name string
		run  func(r *holdRig)
	}{
		{"a hold of 9 minutes 59 seconds, then the task sent", func(r *holdRig) {
			r.deliver()
			r.waitAndSweep(9*time.Minute + 59*time.Second)
			r.wantHeld("at 9 minutes 59 seconds")
			r.wantTold(0, "at 9 minutes 59 seconds")

			r.implementing = false
			r.observe(runtime.Alive)
			r.wantSentOnce("once the implementer's turn ended")
			r.wantTold(0, "once the task was sent")
		}},
		{"a hold of 10 minutes and 1 second", func(r *holdRig) {
			r.deliver()
			r.waitAndSweep(10*time.Minute + time.Second)
			r.wantHeld("at 10 minutes and 1 second")
			r.wantTold(1, "at 10 minutes and 1 second")
		}},
		{"the hold at 20 minutes", func(r *holdRig) {
			r.deliver()
			r.waitAndSweep(10*time.Minute + time.Second)
			r.wantTold(1, "at 10 minutes and 1 second")
			r.waitAndSweep(10*time.Minute - time.Second)
			r.wantHeld("at 20 minutes")
			r.wantTold(1, "at 20 minutes")
		}},
		{"the task sent, then a new hold of a task", func(r *holdRig) {
			r.deliver()
			r.waitAndSweep(8 * time.Minute)
			r.implementing = false
			r.observe(runtime.Alive)
			r.wantSentOnce("once the implementer's turn ended")
			r.must(StreamTurnStart{Claim: testerToken})
			r.must(StreamTurnEnd{Claim: testerToken})

			r.implementing = true
			r.must(RequestDeliver{Claim: testerToken, Task: "test the branch again", Phase: phase.Testing, Generation: 5})
			r.waitAndSweep(8 * time.Minute)
			r.wantTold(0, "8 minutes into the new hold, 16 after the first began")
			r.waitAndSweep(2*time.Minute + time.Second)
			r.wantTold(1, "10 minutes and 1 second into the new hold")
		}},
		{"the task sent and refused, then the same task held again", func(r *holdRig) {
			r.deliver()
			r.waitAndSweep(8 * time.Minute)
			queued := r.pending().QueuedAt
			r.conn.RefusePrompt("the model provider refused the request")
			r.implementing = false
			r.observe(runtime.Alive)
			r.wantSentOnce("once the implementer's turn ended")

			// The agent answered the send with a refusal, so the task waits for the next send, and
			// the implementer is in a turn again by the next sweep.
			r.implementing = true
			r.observe(runtime.Alive)
			if p := r.pending(); !p.QueuedAt.Equal(queued) || !p.ConfirmedAt.IsZero() {
				r.t.Fatalf("the claim holds %+v, want the refused task, queued at %s, waiting again", p, queued)
			}
			r.waitAndSweep(8 * time.Minute)
			r.wantTold(0, "8 minutes into the new hold, 16 after the first began")
			r.waitAndSweep(2*time.Minute + time.Second)
			r.wantTold(1, "10 minutes and 1 second into the new hold")
		}},
		{"the task dropped, then a new hold of a task", func(r *holdRig) {
			// The issue's run moves on: the held task, of run 5, is dropped at the next sweep, and
			// the task of run 6 is held in its place.
			run := uint64(5)
			r.m.deps.PhaseHolds = func(_ context.Context, _ string, _ phase.Phase, generation uint64) (bool, error) {
				return generation == run, nil
			}
			r.deliver()
			r.waitAndSweep(8 * time.Minute)
			run = 6
			r.observe(runtime.Alive)
			if p := r.claim().Pending; p != nil {
				r.t.Fatalf("the claim holds %+v, want the task of the run the issue left dropped", p)
			}
			r.must(RequestDeliver{Claim: testerToken, Task: heldTesterTask, Phase: phase.Testing, Generation: 6})
			r.waitAndSweep(8 * time.Minute)
			r.wantTold(0, "8 minutes into the new hold, 16 after the first began")
			r.waitAndSweep(2*time.Minute + time.Second)
			r.wantTold(1, "10 minutes and 1 second into the new hold")
		}},
		{"the task replaced, then a new hold of a task", func(r *holdRig) {
			// A task of news alone holds no work, and an entry's task takes its place (supersedes).
			news := newsAloneTask(testerToken, 3, 5, phase.Testing)
			r.must(news)
			r.wantCalls("AdoptWorkingCopy", 0)
			r.waitAndSweep(8 * time.Minute)
			entry := startTask(testerToken, 15, 5, 2)
			entry.Phase, entry.Task = phase.Testing, heldTesterTask
			r.must(entry)
			if p := r.pending(); p.ID != entry.ID {
				r.t.Fatalf("the claim holds %s, want the entry's task %s in the place of the news task", p.ID, entry.ID)
			}
			r.waitAndSweep(8 * time.Minute)
			r.wantTold(0, "8 minutes into the new hold, 16 after the first began")
			r.waitAndSweep(2*time.Minute + time.Second)
			r.wantTold(1, "10 minutes and 1 second into the new hold")
		}},
		{"the machine rebuilt from the store during a hold", func(r *holdRig) {
			r.deliver()
			r.waitAndSweep(8 * time.Minute)
			r.restart()
			r.observe(runtime.Alive)
			r.wantHeld("at the rebuilt machine's first sweep")
			r.waitAndSweep(8 * time.Minute)
			r.wantTold(0, "8 minutes after the rebuild, 16 after the hold began")
			r.waitAndSweep(2*time.Minute + time.Second)
			r.wantTold(1, "10 minutes and 1 second after the rebuild")
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tc.run(newHoldRig(t))
		})
	}
}
