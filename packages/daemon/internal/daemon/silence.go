package daemon

import (
	"context"
	"fmt"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// silenceSource is the source every silence the sweep reads is applied under: the claims of the
// supervisor are where it read them, which is the source a claim's own terminal facts take
// (terminalSource). The two share no id — each of these begins "worker-silent:"
// (workerSilentEventID) — and processed_events is keyed on the pair.
const silenceSource = terminalSource

// workerSilentEventID is the identity of one silence, which tellSilentPhaseWorkers' own comment
// says what rests on: the claim token and the moment of that claim's last step. Nothing of the
// bound is in it, so an operator who shortens the bound while a claim is quiet is owed no
// second notice of the same silence.
func workerSilentEventID(token claim.Token, lastStep time.Time) string {
	return fmt.Sprintf("worker-silent:%s:%s", token, lastStep.UTC().Format(time.RFC3339Nano))
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
// The identity of each fact is the claim token and the moment of that claim's last step
// (workerSilentEventID), and that is what makes one silence one telling. A duplicate applies
// nothing, so however many of the minute's sweeps read the same quiet claim, the architect
// hears of it one time; and a worker that takes a step and then goes silent again has a new
// moment, so it is a new fact and the architect hears of it again. That is R5 of
// dispatch://INFRA-3163.
//
// The run the fact names is the run whose task this claim's worker took (ServingRun), which is
// the issue generation the handler holds the record's own against, and not the claim's
// Generation, which counts this claim's launches: a worker displaced into a new pod is on its
// second launch of the same run, and that worker is exactly the one this pass is for. It is the
// reading the release's backstop makes of a claim's run as well (armBackstops).
func (s *supervision) tellSilentPhaseWorkers(ctx context.Context, claims []supervise.Claim) {
	bound := s.cfg.WorkerSilence
	if bound <= 0 || s.applyFacts == nil {
		return
	}
	now := s.plan.clock.Now()
	var facts []intake.Identified
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
		facts = append(facts, intake.Identified{
			ID: workerSilentEventID(c.Token, last),
			Fact: intake.WorkerSilent{Issue: c.Issue, Role: c.Role, Generation: c.ServingRun(),
				LastStep: last, Bound: bound},
		})
	}
	if len(facts) == 0 {
		return
	}
	// Every failure is logged where it failed (workflowRuntime.applyFacts): an apply that failed
	// by its source and its id, and a sweep dropped whole by its source and the number of facts
	// it carried. The next sweep reads the same silence and applies the identical fact again,
	// which is the retry; a line here would say the same thing twice.
	_ = s.applyFacts(ctx, silenceSource, facts)
}
