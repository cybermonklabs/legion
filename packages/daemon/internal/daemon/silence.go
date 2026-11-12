package daemon

import (
	"context"
	"fmt"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// workerSilentEventID is the identity of one silence: the claim token, the claim's launch and the
// moment of its last step. intake.ApplyFact's applied mark then makes one silence one notice
// however many sweeps read it, and a step or a relaunch starts a new silence with a new notice.
// The launch is in it because a launch writes no step: without it, a relaunched worker's silence
// would reuse the id its last process spent and be skipped for good. The bound is not in it, so
// shortening the bound mid-silence tells nobody twice.
func workerSilentEventID(token claim.Token, generation uint64, lastStep time.Time) string {
	return fmt.Sprintf("worker-silent:%s:%d:%s", token, generation, lastStep.UTC().Format(time.RFC3339Nano))
}

// workerBackgroundEventID is the identity of one standing report of background work: the claim
// token, the claim's launch and the moment of the report. A session reports only when its reading
// changes (packages/pi-legion/extensions/legion.ts), so a hung job leaves one report and one
// notice. It is not the last step, which a hung job freezes too: an id keyed on it would be spent
// the first minute the silence bound passed, hours before the background bound, and nobody
// would be told.
func workerBackgroundEventID(token claim.Token, generation uint64, reported time.Time) string {
	return fmt.Sprintf("worker-background:%s:%d:%s", token, generation, reported.UTC().Format(time.RFC3339Nano))
}

// tellSilentPhaseWorkers applies, over the claims the orphan sweep already read, one
// intake.WorkerSilent for each claim that is up and between turns, in no update round, with its
// last step past this deployment's bound (config.Config.WorkerSilence), and one
// intake.WorkerBackgroundStalled for each of those whose background report stands past the longer
// bound (backgroundStalledFact).
//
// The sweep filters and decides nothing: the record can have moved since these claims were read,
// so each handler re-reads it inside the fact's own transaction and decides there
// (workflow.SilentPhaseWorker and the handlers in workflow's silence.go). The fact carries what
// the sweep read, the moment and the bound, so the handler judges against the bound the sweep
// used rather than one an operator changed since.
//
// A claim of no issue (the controller's) is skipped, since its fact would name no record. The
// fact's run is the claim's ServingRun, the issue generation its task came from, and not its
// Generation, which counts launches and goes into the id instead.
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
		if !workflow.PastBound(last, bound, now) {
			continue
		}
		// The handler would refuse this claim for its report, and a refused fact's id is spent
		// for good while the last step it is keyed on stands still.
		if !workflow.ReportExplainsQuiet(c.ActivityBackground, c.ActivityNextReminderAt, now) {
			facts = append(facts, sweptFact{
				ID: workerSilentEventID(c.Token, c.Generation, last),
				Fact: intake.WorkerSilent{Issue: c.Issue, Role: c.Role, Generation: c.ServingRun(),
					LastStep: last, Bound: bound},
			})
		}
		if fact, told := backgroundStalledFact(c, s.cfg.WorkerBackground, now); told {
			facts = append(facts, fact)
		}
	}
	if len(facts) == 0 {
		return
	}
	// workflowRuntime.applySweepFacts logs every failure, and the next sweep retries it.
	_ = s.applySweepFacts(ctx, facts)
}

// backgroundStalledFact is the fact for a claim whose session reports background work of its own
// running with that report past bound (config.Config.WorkerBackground), and false for any other.
// It tells and never prompts, since nothing the daemon reads tells a long job from a hung one.
func backgroundStalledFact(c supervise.Claim, bound time.Duration, now time.Time) (sweptFact, bool) {
	reported := c.ActivityAt
	if !c.ActivityBackground || !workflow.PastBound(reported, bound, now) {
		return sweptFact{}, false
	}
	return sweptFact{
		ID: workerBackgroundEventID(c.Token, c.Generation, reported),
		Fact: intake.WorkerBackgroundStalled{Issue: c.Issue, Role: c.Role, Generation: c.ServingRun(),
			ReportedAt: reported, Kinds: c.ActivityKinds, Bound: bound},
	}, true
}
