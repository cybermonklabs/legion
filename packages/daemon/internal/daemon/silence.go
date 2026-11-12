package daemon

import (
	"context"
	"fmt"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// workerSilentEventID is the identity of one silence, which tellSilentPhaseWorkers' own comment
// says what rests on: the claim token and the moment of that claim's last step. Nothing of the
// bound is in it, so an operator who shortens the bound while a claim is quiet is owed no
// second notice of the same silence.
func workerSilentEventID(token claim.Token, lastStep time.Time) string {
	return fmt.Sprintf("worker-silent:%s:%s", token, lastStep.UTC().Format(time.RFC3339Nano))
}

// workerBackgroundEventID is the identity of one standing report of background work: the claim
// token and the moment of that report, which tellSilentPhaseWorkers' own comment says what rests
// on. It is deliberately not the claim's last step, which a hung job leaves frozen as well: a
// fact keyed on that step is minted and spent the first minute the silence bound passes, hours
// before the background bound is reached, and the telling would never happen.
func workerBackgroundEventID(token claim.Token, reported time.Time) string {
	return fmt.Sprintf("worker-background:%s:%s", token, reported.UTC().Format(time.RFC3339Nano))
}

// tellSilentPhaseWorkers applies one intake.WorkerSilent for every claim of the sweep that is a
// phase worker nothing will prompt: its process is up and between turns, it holds no task, and
// its agent's last recorded step is older than the bound this deployment gives a phase worker
// (config.Config.WorkerSilence, `worker_silence_minutes`). It rides the orphan sweep, over the
// claims that sweep already read, as the stall backstop does (backstopStalledTurns).
//
// The pass decides nothing and calls no rule: the terms below are a cheap filter over the live
// claims, the record can have moved since it was read, and the handler re-reads the record
// inside the fact's own transaction and decides there, on the rule it and admission's tick list
// share (workflow.(*Engine).workerSilent, workflow.SilentPhaseWorker). So the fact carries what
// this pass read — the moment of the last step and the bound it judged that moment against —
// and nothing it concluded.
//
// One term below is this sweep's alone and is no term of that rule: a claim of an issue
// the daemon records none of (c.Issue == ""), which is the daemon's own controller claim, since
// a fact about no issue would name no record to re-read. A suspension held over a claim for its
// agent's turn is no term here either: supervise.Claim.SuspensionHeld is the machine's own
// memory and the claims this sweep reads come from the store (supervisor.Claims), which never
// writes it. Such a claim is swept like any other, and the outbox refuses the start the repair
// writes while the suspension holds, which the architect's notice says (workflow's
// workerSilentDid).
//
// The identity of each silence is the claim token and the moment of that claim's last step
// (workerSilentEventID), and that is what makes one silence one telling. A duplicate applies
// nothing, so however many of the minute's sweeps read the same quiet claim, the architect
// hears of it one time; and a worker that takes a step and then goes silent again has a new
// moment, so it is a new fact and the architect hears of it again. That is R5 of
// dispatch://INFRA-3163.
//
// Every claim the terms below hold is read a second time, for the other half of this pass: a
// worker whose own session reports background work of its own running with a report older than
// the longer bound the deployment sets is named to its architect and sent nothing at all
// (backgroundStalledFact, which holds what that half does and what it cannot do). The two facts
// of one claim are applied together, under ids of their own, and neither reads the other: a
// worker reporting background work is told of here and left alone by the silence handler, which
// refuses every claim whose session says such work is running (workflow.SilentPhaseWorker).
//
// The run the fact names is the run whose task this claim's worker took (ServingRun), which is
// the issue generation the handler holds the record's own against, and not the claim's
// Generation, which counts this claim's launches: a worker displaced into a new pod is on its
// second launch of the same run, and that worker is exactly the one this pass is for. It is the
// reading the release's backstop makes of a claim's run as well (armBackstops).
func (s *supervision) tellSilentPhaseWorkers(ctx context.Context, claims []supervise.Claim) {
	if s.applySweepFacts == nil {
		return
	}
	bound := s.cfg.WorkerSilence
	now := s.plan.clock.Now()
	var facts []sweptFact
	for _, c := range claims {
		if c.Issue == "" || c.Role == "" || c.Role == claim.RoleArchitect {
			continue
		}
		if c.State != supervise.StateReady && c.State != supervise.StateIdle {
			continue
		}
		if c.UpdateRound != "" {
			continue
		}
		last := c.Step.LastStep()
		if last.IsZero() || !last.Add(bound).Before(now) {
			continue
		}
		facts = append(facts, sweptFact{
			ID: workerSilentEventID(c.Token, last),
			Fact: intake.WorkerSilent{Issue: c.Issue, Role: c.Role, Generation: c.ServingRun(),
				LastStep: last, Bound: bound},
		})
		if fact, told := backgroundStalledFact(c, s.cfg.WorkerBackground, now); told {
			facts = append(facts, fact)
		}
	}
	if len(facts) == 0 {
		return
	}
	// Every failure is logged where it failed (workflowRuntime.applySweepFacts): an apply that
	// failed by its id, and a sweep dropped whole by the number of facts it carried. The next
	// sweep reads the same silence and applies the identical fact again, which is the retry; a
	// line here would say the same thing twice.
	_ = s.applySweepFacts(ctx, facts)
}

// backgroundStalledFact is the sweep's other half, for the claim it has just read as a phase
// worker whose silence stands: whether that claim's own session reports background work of its
// own running with a report older than the bound this deployment sets for it
// (config.Config.WorkerBackground, `worker_background_hours`), and the fact to apply when it
// does. A claim whose session has reported nothing is judged from no moment at all, as a claim
// with no step is, and the bound itself is the rule's to refuse where it is not positive
// (workflow's backgroundStalled).
//
// The silence repair leaves every claim reporting background work alone (workflow's
// SilentPhaseWorker), because nothing can tell a worker with a forty-minute job from one whose
// job hung. This is the other half of that: such a worker is told of once and sent nothing, and
// its architect, which can read what the work is, decides. That is R6 of
// dispatch://INFRA-3163.
//
// What it does not do, and cannot: the daemon never reads the job. It reads how long the
// session's own report has stood and nothing else, so a worker that runs job after job without
// its reading changing is told once, for the first of them, and a worker whose reading flickers
// — a kind added, a job ended while another runs — is told once for each report that stands
// past the bound. Neither is a mistake of this pass; nothing it could read would separate a job
// that hung from a long one, which is why what it writes is a telling and not a prompt.
//
// The identity is the claim token and the moment of the report (workerBackgroundEventID), and
// that is what makes one standing report one telling: a session sends a report only when its
// reading changes (packages/pi-legion/extensions/legion.ts, the five-second interval), so a job
// that hangs leaves one report standing however many sweeps read it, and the duplicate applies
// nothing. The run the fact names is the run whose task this claim's worker took (ServingRun),
// as the silence fact's is.
func backgroundStalledFact(c supervise.Claim, bound time.Duration, now time.Time) (sweptFact, bool) {
	reported := c.ActivityAt
	if reported.IsZero() || !c.ActivityBackground {
		return sweptFact{}, false
	}
	if !reported.Add(bound).Before(now) {
		return sweptFact{}, false
	}
	return sweptFact{
		ID: workerBackgroundEventID(c.Token, reported),
		Fact: intake.WorkerBackgroundStalled{Issue: c.Issue, Role: c.Role, Generation: c.ServingRun(),
			ReportedAt: reported, Kinds: c.ActivityKinds, Bound: bound},
	}, true
}
