package supervise

// The start by Oh My Pi's own lookup (dispatch://INFRA-4108 R3). A pod's launcher older than the
// lookup has not said in its hello that it can look a session up by its id, so the runtime answers
// a resume by the id alone runtime.ErrSessionLookupUnavailable, having started nothing, and the
// same try resumes the session a second time with ResumeByAgent: the agent's own command carries
// `--resume=<session id>`, which Oh My Pi resolves. The claim marks that start's generation
// (Claim.AgentResumeGeneration) in the write that records its process. Where the start registers
// as the session, the claim records its file again and the mark goes. Where it does not — its
// process ends first, or its registration deadline passes — that end is charged nothing, and the
// next try forgets the session, charged nothing either.

import (
	"errors"
	"log/slog"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/runtime/fake"
)

// byIDQueued is the claim queued at generation 2, the generation of its last launch, recording
// idSession and no file, with no start of Oh My Pi's own lookup outstanding, carrying failures
// launch failures.
func byIDQueued(failures int) Claim {
	c := byIDClaim(StateQueued, 2)
	c.Budgets.LaunchFailures = failures
	return c
}

// markedClaim is the claim after a start by Oh My Pi's own lookup that did not register, as the
// store holds it for that start's next try: queued at the start's generation, 3, with the mark
// naming it, its session and no file. It carries failures launch failures and what a forget starts
// again, the outcomes of its session's turns and its run of provider errors.
func markedClaim(failures int) Claim {
	c := byIDClaim(StateQueued, 3)
	c.AgentResumeGeneration = 3
	c.Budgets.LaunchFailures = failures
	c.TurnOutcomes = []TurnOutcome{{Generation: 3, Sequence: 1, Refused: true, Message: "overloaded"}}
	c.ProviderErrorRun = 2
	return c
}

// startByAgentLookup launches the rig's queued claim as row 1 says the launch goes through a
// launcher without the lookup: a Resume of the session by its id, which the runtime refuses, then
// one in the same generation that Oh My Pi resolves, and the mark at that generation. It answers
// the generation. A launch that went any other way ends the case here, since every row that starts
// from row 1 reads what follows it.
func (r *byIDRig) startByAgentLookup() uint64 {
	r.t.Helper()
	from := len(r.rt.Calls())
	r.must(RequestSpawn{Claim: r.token})
	starts, g := r.starts(from), r.generation()
	if len(starts) != 2 || !resumedByID(starts[0]) || !resumedByAgent(starts[1]) || r.claim().AgentResumeGeneration != g {
		r.t.Fatalf("the launch through a launcher without the lookup made %s, and the claim's mark is %d at generation %d; want a Resume of %s by its id, then one with ResumeByAgent, and the mark at the launch's generation (row 1)",
			startsText(starts), r.claim().AgentResumeGeneration, g, idSession)
	}
	return g
}

// wantUnchargedEnd is what rows 3 and 4 say of the end of a start by Oh My Pi's own lookup that did
// not register, at generation g: one line for it, nothing charged — the claim keeps its failures
// launch failures, counts no death and does not fail — and the relaunch made through launch, which
// waits no backoff, where relaunchAfterFailure's charged relaunch waits one (backOff). slept is how
// many backoffs the machine had waited before the end.
func (r *byIDRig) wantUnchargedEnd(g uint64, failures, slept int) {
	r.t.Helper()
	if ends := r.records.prefixed(unchargedEndLine); len(ends) != 1 {
		r.t.Errorf("the end was logged %d times by %q; want once", len(ends), unchargedEndLine)
	}
	r.wantBudgets(Budgets{LaunchFailures: failures})
	r.wantNoFailure()
	if waits := r.slept()[slept:]; len(waits) != 0 {
		r.t.Errorf("the relaunch waited %v first; want no wait, since the uncharged relaunch goes through launch", waits)
	}
	if now := r.generation(); now <= g {
		r.t.Fatalf("the claim is at generation %d after the end of generation %d; want it relaunched", now, g)
	}
}

// wantForgotAfterAgentLookup is row 7, from the runtime's call numbered from on: the try after a
// start by Oh My Pi's own lookup that did not register asks the launcher for the lookup once more,
// is refused it, and forgets the session in the same try with the cause not-resumed-by-id. It
// makes no second Resume with ResumeByAgent and spawns one fresh agent, built with that cause and
// resuming nothing. The workflow is told once, the claim abandons the session, the mark goes, the
// outcomes of the session's turns and its run of provider errors start again, as at every forget,
// and the claim keeps its failures launch failures.
func (r *byIDRig) wantForgotAfterAgentLookup(from, failures int) {
	r.t.Helper()
	r.wantNoSpawnWhileRecorded()
	starts := r.starts(from)
	if len(starts) != 2 || !resumedByID(starts[0]) || starts[1].Method != "Spawn" {
		r.t.Fatalf("the next try made %s; want one Resume of %s by its id, which the launcher refuses, then one Spawn and no Resume with ResumeByAgent", startsText(starts), idSession)
	}
	if spawned := starts[1].Spec; spawned.ResumeSession != "" || spawned.ResumeSessionFile != "" || spawned.ResumeByAgent {
		r.t.Errorf("the fresh spawn names session %q, file %q and ResumeByAgent %t; want it to resume nothing", spawned.ResumeSession, spawned.ResumeSessionFile, spawned.ResumeByAgent)
	}
	if built := r.specs.last(r.t); built.Generation != starts[1].Spec.Generation || built.SessionLost != SessionLossNotResumedByID {
		r.t.Errorf("the last spec was built at generation %d with SessionLost %q; want the fresh spawn's, at %d, with %q",
			built.Generation, built.SessionLost, starts[1].Spec.Generation, SessionLossNotResumedByID)
	}
	r.wantLost(SessionLossNotResumedByID)
	c := r.claim()
	if c.Abandoned.Session != idSession || c.Session != "" || c.SessionFile != "" || c.AgentResumeGeneration != 0 {
		r.t.Errorf("the claim abandoned %q, records session %q and file %q, and is marked %d; want %s abandoned, no session and no mark",
			c.Abandoned.Session, c.Session, c.SessionFile, c.AgentResumeGeneration, idSession)
	}
	if len(c.TurnOutcomes) != 0 || c.ProviderErrorRun != 0 {
		r.t.Errorf("the claim keeps turn outcomes %+v and a run of %d provider errors; want both started again with the fresh session", c.TurnOutcomes, c.ProviderErrorRun)
	}
	r.wantBudgets(Budgets{LaunchFailures: failures})
}

// wantResumedThroughTheLauncher is row 8's resume, from the runtime's call numbered from on: one
// Resume by the session id through a launcher that can now make the lookup, with no ResumeByAgent
// and no Spawn.
func (r *byIDRig) wantResumedThroughTheLauncher(from int) {
	r.t.Helper()
	if starts := r.starts(from); len(starts) != 1 || !resumedByID(starts[0]) {
		r.t.Fatalf("the next try made %s; want one Resume of %s by its id through the launcher, and no Spawn", startsText(starts), idSession)
	}
}

// Each subtest is one row of the plan's table "The start by Oh My Pi's own lookup, by state and
// event" (dispatch://INFRA-4108), and asserts what the row's last column says. G is the generation
// of the start by Oh My Pi's own lookup, and the mark is Claim.AgentResumeGeneration.
func TestAResumeByOhMyPisOwnLookupThatDoesNotRegisterIsForgottenAtTheNextTry(t *testing.T) {
	// seeded is the launch failures a claim carries in the rows that charge nothing: one below
	// the harness's limit of three, so a charge would fail the claim.
	const seeded = 2

	t.Run("row 1: a launch whose first Resume the launcher cannot look up resumes by Oh My Pi's own lookup", func(t *testing.T) {
		r := newByIDRig(t, byIDQueued(seeded), olderLauncher)

		r.must(RequestSpawn{Claim: testToken})

		g := r.generation()
		starts := r.starts(0)
		if len(starts) != 2 || !resumedByID(starts[0]) || !resumedByAgent(starts[1]) {
			t.Fatalf("the launch made %s; want a Resume of %s by its id, then one with ResumeByAgent, and no Spawn", startsText(starts), idSession)
		}
		first, second := starts[0], starts[1]
		if first.Spec.Generation != g || second.Spec.Generation != g {
			t.Errorf("the two Resumes are at generations %d and %d; want both at the launch's generation %d", first.Spec.Generation, second.Spec.Generation, g)
		}
		if (first.Previous == nil) != (second.Previous == nil) || (first.Previous != nil && *first.Previous != *second.Previous) {
			t.Errorf("the two Resumes wait out %+v and %+v; want the same previous process", first.Previous, second.Previous)
		}
		builds := r.builds()
		if len(builds) != 2 {
			t.Fatalf("the launch built %d specs; want two, one for each Resume", len(builds))
		}
		if builds[0].AgentResumeGeneration != 0 {
			t.Errorf("the first Resume's spec was built for a claim marked %d; want none, so only the second Resume's prompt says the workspace may have been recreated", builds[0].AgentResumeGeneration)
		}
		if builds[1].AgentResumeGeneration != g || builds[1].Generation != g {
			t.Errorf("the second Resume's spec was built at generation %d for a claim marked %d; want both %d, which is what adds the recreated sentence to its prompt",
				builds[1].Generation, builds[1].AgentResumeGeneration, g)
		}
		if write, ok := r.launchWrite(g); !ok || write.AgentResumeGeneration != g {
			t.Errorf("the write that records the process at generation %d is marked %d (written %t); want it marked %d", g, write.AgentResumeGeneration, ok, g)
		}
		if stored := r.store.load(testToken); stored.AgentResumeGeneration != g {
			t.Errorf("the store holds the claim marked %d; want %d", stored.AgentResumeGeneration, g)
		}
		launched := r.records.of(launchedLine)
		if len(launched) != 1 {
			t.Fatalf("the launch logged %d %q lines; want one", len(launched), launchedLine)
		}
		if by, _ := launched[0].attr("resumedBy"); by.String() != "agent-lookup" {
			t.Errorf("the launched line says resumedBy %q; want %q", by, "agent-lookup")
		}
		if resumed, _ := launched[0].attr("resumed"); resumed.Kind() != slog.KindBool || !resumed.Bool() {
			t.Errorf("the launched line says resumed %v; want true", resumed)
		}
		r.wantBudgets(Budgets{LaunchFailures: seeded})
		if c := r.claim(); c.Session != idSession || c.SessionFile != "" || c.Abandoned.Session != "" {
			t.Errorf("the claim records session %q and file %q and abandoned %q; want %s kept, no file and nothing abandoned", c.Session, c.SessionFile, c.Abandoned.Session, idSession)
		}
		r.wantNothingLost()
	})

	t.Run("row 2: the agent registers as the session with its file", func(t *testing.T) {
		r := newByIDRig(t, byIDQueued(0), olderLauncher)
		r.startByAgentLookup()
		r.connect()

		if err := r.registerAs(idSession, idSessionFile, r.deps.PluginContract); err != nil {
			t.Fatalf("the registration as %s on %s = %v; want it accepted", idSession, idSessionFile, err)
		}

		r.wantState(StateRegistered)
		for what, c := range map[string]Claim{"the claim": r.claim(), "the store": r.store.load(testToken)} {
			if c.Session != idSession || c.SessionFile != idSessionFile || c.AgentResumeGeneration != 0 {
				t.Errorf("%s records session %q and file %q, marked %d; want %s, %s and no mark", what, c.Session, c.SessionFile, c.AgentResumeGeneration, idSession, idSessionFile)
			}
		}
	})

	t.Run("row 3: a probe reports the process gone", func(t *testing.T) {
		r := newByIDRig(t, byIDQueued(seeded), olderLauncher)
		g := r.startByAgentLookup()
		slept := len(r.slept())

		r.observe(runtime.Gone)

		r.wantUnchargedEnd(g, seeded, slept)
	})

	t.Run("row 4: the registration deadline passes", func(t *testing.T) {
		r := newByIDRig(t, byIDQueued(seeded), olderLauncher)
		g := r.startByAgentLookup()
		alive := r.locator()
		slept := len(r.slept())

		r.advance(deadline)

		if suspends := r.wantCalls("Suspend", 1); suspends[0].Locator != alive {
			t.Errorf("suspended %+v; want the process that never registered, %+v", suspends[0].Locator, alive)
		}
		r.wantUnchargedEnd(g, seeded, slept)
	})

	t.Run("row 5: the agent registers as another session", func(t *testing.T) {
		r := newByIDRig(t, byIDQueued(seeded), olderLauncher)
		g := r.startByAgentLookup()
		r.connect()

		// The older launcher's pod carries the earlier release's plugin, so the registration
		// speaks an older contract: the mark names this generation, which is what keeps the
		// roll edge's rule from it.
		if err := r.registerAs(otherSession, otherSessionFile, r.olderContract()); !errors.Is(err, claim.SameAgentRefusal) {
			t.Fatalf("the registration as %s = %v; want the same-agent refusal, 409, as today", otherSession, err)
		}
		if lines := r.records.prefixed(rollEdgeLine); len(lines) != 0 {
			t.Errorf("the refusal wrote %d roll-edge lines; want none for a start by Oh My Pi's own lookup", len(lines))
		}
		if c := r.claim(); c.Session != idSession || c.SessionFile != "" {
			t.Errorf("the refused registration left the claim recording %q and %q; want %s and no file", c.Session, c.SessionFile, idSession)
		}
		slept := len(r.slept())

		r.observe(runtime.Gone)

		r.wantUnchargedEnd(g, seeded, slept)
	})

	t.Run("row 6: the daemon restarts, then the start ends", func(t *testing.T) {
		for _, end := range []struct {
			name string
			end  func(r *byIDRig)
		}{
			{"as row 3, a probe reports the process gone", func(r *byIDRig) { r.observe(runtime.Gone) }},
			{"as row 4, the registration deadline passes", func(r *byIDRig) { r.advance(deadline) }},
		} {
			t.Run(end.name, func(t *testing.T) {
				r := newByIDRig(t, byIDQueued(seeded), olderLauncher)
				g := r.startByAgentLookup()

				r.restart()

				if mark := r.claim().AgentResumeGeneration; mark != g {
					t.Fatalf("the machine built again from the store reads the mark %d; want %d", mark, g)
				}
				slept := len(r.slept())
				end.end(r)
				r.wantUnchargedEnd(g, seeded, slept)
			})
		}
	})

	t.Run("row 7: the next try's first Resume is refused the lookup again", func(t *testing.T) {
		r := newByIDRig(t, markedClaim(seeded), olderLauncher)

		r.must(RequestSpawn{Claim: testToken})

		r.wantForgotAfterAgentLookup(0, seeded)
	})

	t.Run("row 8: the next try goes through a launcher that can now make the lookup", func(t *testing.T) {
		t.Run("the launcher finds the session", func(t *testing.T) {
			r := newByIDRig(t, markedClaim(seeded), nil)

			r.must(RequestSpawn{Claim: testToken})

			r.wantResumedThroughTheLauncher(0)
			r.connect()
			if err := r.registerAs(idSession, idSessionFile, r.deps.PluginContract); err != nil {
				t.Fatalf("the registration as %s = %v; want it accepted", idSession, err)
			}
			if c := r.claim(); c.Session != idSession || c.SessionFile != idSessionFile || c.AgentResumeGeneration != 0 {
				t.Errorf("the claim records session %q and file %q, marked %d; want %s, %s and no mark", c.Session, c.SessionFile, c.AgentResumeGeneration, idSession, idSessionFile)
			}
			r.wantNothingLost()
			r.wantBudgets(Budgets{LaunchFailures: seeded})
		})
		t.Run("the launcher finds the session nowhere", func(t *testing.T) {
			r := newByIDRig(t, markedClaim(seeded), nil)
			r.rt.ScriptResume(fake.SpawnResult{Err: noSuchSession()})

			r.must(RequestSpawn{Claim: testToken})

			r.wantNoSpawnWhileRecorded()
			starts := r.starts(0)
			if len(starts) != 2 || !resumedByID(starts[0]) || starts[1].Method != "Spawn" {
				t.Fatalf("the next try made %s; want a Resume of %s by its id, then one Spawn", startsText(starts), idSession)
			}
			if built := r.specs.last(t); built.SessionLost != SessionLossGone {
				t.Errorf("the fresh spawn was built with SessionLost %q; want %q", built.SessionLost, SessionLossGone)
			}
			r.wantLost(SessionLossGone)
			if c := r.claim(); c.Abandoned.Session != idSession || c.Session != "" || c.AgentResumeGeneration != 0 {
				t.Errorf("the claim abandoned %q, records %q and is marked %d; want %s abandoned, no session and no mark", c.Abandoned.Session, c.Session, c.AgentResumeGeneration, idSession)
			}
			r.wantBudgets(Budgets{LaunchFailures: seeded})
		})
		t.Run("the start ends before it registers", func(t *testing.T) {
			r := newByIDRig(t, markedClaim(0), nil)
			r.must(RequestSpawn{Claim: testToken})
			r.wantResumedThroughTheLauncher(0)
			slept := len(r.slept())

			r.observe(runtime.Gone)

			// The mark names an earlier generation than this start's, so its end is charged as
			// today: one launch failure, and the relaunch waits its backoff.
			r.wantBudgets(Budgets{LaunchFailures: 1})
			if waits := r.slept()[slept:]; len(waits) != 1 {
				t.Errorf("the relaunch waited %v first; want the one backoff of a charged relaunch", waits)
			}
		})
	})

	t.Run("row 9: the runtime refuses the Resume with ResumeByAgent", func(t *testing.T) {
		r := newByIDRig(t, byIDQueued(0), olderLauncher)
		// A Resume the launcher cannot look up is refused before the scripted answers, so this
		// answer is the second Resume's.
		r.rt.ScriptResume(fake.SpawnResult{Err: errBoom})

		r.must(RequestSpawn{Claim: testToken})

		starts := r.starts(0)
		if len(starts) != 4 || !resumedByID(starts[0]) || !resumedByAgent(starts[1]) || !resumedByID(starts[2]) || !resumedByAgent(starts[3]) {
			t.Fatalf("the launch made %s; want the refused try's Resume by id and Resume with ResumeByAgent, then the retry's two, and no Spawn", startsText(starts))
		}
		refused, retried := starts[0].Spec.Generation, starts[2].Spec.Generation
		if starts[1].Spec.Generation != refused || starts[3].Spec.Generation != retried || retried != refused+1 {
			t.Errorf("the Resumes are at generations %s; want the refused try's two at one generation and the retry's two at the next", startsText(starts))
		}
		r.wantBudgets(Budgets{LaunchFailures: 1})
		for _, write := range r.store.history() {
			if write.Generation == refused && write.AgentResumeGeneration != 0 {
				t.Errorf("a write at the refused try's generation %d is marked %d; want the mark left at 0", refused, write.AgentResumeGeneration)
			}
		}
		if mark := r.claim().AgentResumeGeneration; mark != retried {
			t.Errorf("the claim is marked %d after the retry; want the retry's generation %d, as row 1 marks it", mark, retried)
		}
	})

	t.Run("row 10: the agent registers as the session and never says it is ready", func(t *testing.T) {
		r := newByIDRig(t, byIDQueued(0), olderLauncher)
		g := r.startByAgentLookup()
		r.connect()
		if err := r.registerAs(idSession, idSessionFile, r.deps.PluginContract); err != nil {
			t.Fatalf("the registration as %s = %v; want it accepted", idSession, err)
		}
		from := len(r.rt.Calls())

		r.advance(deadline)

		r.wantBudgets(Budgets{LaunchFailures: 1})
		if cause := r.claim().PendingCause; cause != CauseNeverRegistered {
			t.Errorf("the claim's pending cause is %q; want %q", cause, CauseNeverRegistered)
		}
		starts := r.starts(from)
		if len(starts) == 0 || starts[0].Method != "Resume" || starts[0].Spec.ResumeSessionFile != idSessionFile || starts[0].Spec.Generation != g+1 {
			t.Fatalf("the relaunch made %s; want a Resume of the registered file %s at generation %d", startsText(starts), idSessionFile, g+1)
		}
	})

	t.Run("row 11: an operator suspends the claim and then resumes it", func(t *testing.T) {
		for _, tc := range []struct {
			name  string
			newer bool
		}{
			{"through the same launcher, the resume is row 7", false},
			{"through a launcher that can now make the lookup, the resume is row 8", true},
		} {
			t.Run(tc.name, func(t *testing.T) {
				r := newByIDRig(t, byIDQueued(seeded), olderLauncher)
				r.startByAgentLookup()

				// As today, a suspension of a launch that has not registered is held until the
				// agent is ready, and its registration deadline ends the hold in the suspension.
				if err := r.handle(RequestSuspend{Claim: testToken, Reason: "the operator suspended it", By: SuspendedByOperator}); !errors.Is(err, ErrSuspendHeld) {
					t.Fatalf("the operator's suspend = %v; want it held until the agent is ready, %v", err, ErrSuspendHeld)
				}
				r.advance(deadline)
				r.wantState(StateSuspended)
				if c := r.claim(); c.SuspendedBy != SuspendedByOperator || c.Session != idSession {
					t.Fatalf("the claim is suspended by %q recording %q; want the operator's suspension with %s kept", c.SuspendedBy, c.Session, idSession)
				}
				r.wantBudgets(Budgets{LaunchFailures: seeded})
				if tc.newer {
					r.rt.RefuseLookupByID = false
				}
				from := len(r.rt.Calls())

				r.must(RequestResume{Claim: testToken, By: SuspendedByOperator})

				if tc.newer {
					r.wantResumedThroughTheLauncher(from)
					r.wantNothingLost()
					return
				}
				r.wantForgotAfterAgentLookup(from, seeded)
			})
		}
	})

	t.Run("row 12: the fresh agent of the forget registers on an older plugin contract", func(t *testing.T) {
		r := newByIDRig(t, markedClaim(0), func(h *harness) {
			olderLauncher(h)
			h.deps.PluginRelaunchable = true
		})
		r.must(RequestSpawn{Claim: testToken})
		r.wantForgotAfterAgentLookup(0, 0)
		r.connect()

		// The forget ran in a pod kept from before the boot, which carries the earlier release's
		// plugin: the stale-plugin relaunch resumes the fresh agent's new file.
		from := len(r.rt.Calls())
		err := r.registerAs(newSession, newSessionFile, r.olderContract())
		var refusal claim.Refusal
		if !errors.As(err, &refusal) || refusal != claim.StalePluginContract(r.olderContract(), r.deps.PluginContract) {
			t.Fatalf("the fresh agent's registration on contract %d = %v; want the stale-plugin refusal after its relaunch", r.olderContract(), err)
		}
		starts := r.starts(from)
		if len(starts) != 1 || starts[0].Method != "Resume" || starts[0].Spec.ResumeSessionFile != newSessionFile {
			t.Fatalf("the stale-plugin relaunch made %s; want one Resume of the fresh agent's file %s", startsText(starts), newSessionFile)
		}
		if built := r.specs.last(t); built.SessionLost != SessionLossNotResumedByID {
			t.Errorf("the stale-plugin relaunch was built with SessionLost %q; want the forget's cause %q, kept until the claim's first ready", built.SessionLost, SessionLossNotResumedByID)
		}
		if c := r.claim(); c.Session != newSession || c.Abandoned.Session != idSession {
			t.Errorf("the claim records %q and abandoned %q; want the fresh agent's %s, and %s still the one abandoned", c.Session, c.Abandoned.Session, newSession, idSession)
		}
		r.wantLost(SessionLossNotResumedByID)

		r.connect()
		if err := r.registerAs(newSession, newSessionFile, r.deps.PluginContract); err != nil {
			t.Fatalf("the relaunched agent's registration on the daemon's contract = %v; want it accepted", err)
		}
		r.readyAs(newSession)
		r.observe(runtime.Gone)

		if built := r.specs.last(t); built.SessionLost != "" || built.Session != newSession {
			t.Errorf("the first launch after the ready was built for session %q with SessionLost %q; want %s and no cause", built.Session, built.SessionLost, newSession)
		}
		r.wantLost(SessionLossNotResumedByID)
	})
}

// An end the policy charges nothing is no end of the controller's session (SessionRun,
// dispatch://INFRA-2045 R1). At the roll edge the controller's running process, which an earlier
// release spawned, registers on that release's older plugin as another session; the fence refuses
// it, and the end of that process is charged nothing (endOutcome), whether it is reported gone or
// its registration deadline passes. The next launch resumes the same session, and its run counts
// neither end, since died and registrationDeadline count an end only where the one policy charges
// it (Charges): counted, two such ends in a row would replace a session that did nothing wrong.
// The control: count the end at either site without asking the policy, and its row fails.
func TestAnEndThePolicyChargesNothingIsNoEndOfTheControllersSession(t *testing.T) {
	for _, end := range []struct {
		name string
		end  func(r *byIDRig)
	}{
		{"its process is reported gone", func(r *byIDRig) { r.observe(runtime.Gone) }},
		{"its registration deadline passes", func(r *byIDRig) { r.advance(deadline) }},
	} {
		t.Run(end.name, func(t *testing.T) {
			c := byIDClaim(StateLaunching, 3)
			c.Token, c.Tree, c.Issue, c.Role = controllerClaim().Token, "", "", claim.RoleController
			c.Locator.Claim = c.Token
			r := newByIDRig(t, c, nil)
			r.connect()
			if err := r.registerAs(otherSession, otherSessionFile, r.olderContract()); !errors.Is(err, claim.SameAgentRefusal) {
				t.Fatalf("the registration as %s on the earlier release's contract = %v; want the same-agent refusal, 409", otherSession, err)
			}
			from := len(r.rt.Calls())

			end.end(r)

			r.wantBudgets(Budgets{})
			r.wantFirstStartResumesByID(from)
			if s := r.claim().Session; s != idSession {
				t.Fatalf("the claim records session %q after the uncharged end; want %s resumed", s, idSession)
			}
			r.wantEnds(0)
		})
	}
}
