package daemon

import (
	"context"
	"fmt"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/record"
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
// token, the claim's launch and the moment of the report. An Oh My Pi session reports when its
// reading changes and at the end of each of its turns (packages/pi-legion/extensions/legion.ts:
// its five-second interval sends a reading when it changed, or when the last one did not reach
// the daemon, and its `session_stop` handler sends one whatever it says, unless that stop was
// aborted), and a Claude Code worker at its Stop hook alone
// (packages/claude-envoy/src/legion-phase-hook.ts, sendActivity). So a hung job of a worker that
// takes no turn leaves one report standing and one notice, and a turn the worker takes while the
// job runs leaves a report of a later moment, which is a notice of its own once it too has stood
// past the bound. It is not the last step, which a hung job freezes too: an id keyed on it would
// be spent the first minute the silence bound passed, hours before the background bound, and
// nobody would be told.
func workerBackgroundEventID(token claim.Token, generation uint64, reported time.Time) string {
	return fmt.Sprintf("worker-background:%s:%d:%s", token, generation, reported.UTC().Format(time.RFC3339Nano))
}

// tellSilentPhaseWorkers applies, over the claims the orphan sweep already read, one
// intake.WorkerSilent for each claim that is up and between turns, in no update round, with its
// last step past this deployment's bound (config.Config.WorkerSilence) and no report of its
// session's own explaining that quiet (workflow.ReportExplainsQuiet), and one
// intake.WorkerBackgroundStalled for each of those whose background report stands past the longer
// bound (backgroundStalledFact).
//
// The sweep filters on the live claim and the handler decides: the record can have moved since
// these claims were read, so each handler re-reads it inside the fact's own transaction and
// decides there (workflow.SilentPhaseWorker and the handlers in workflow's silence.go). The fact
// carries what the sweep read, the moment and the bound, so the handler judges against the bound
// the sweep used rather than one an operator changed since.
//
// A claim of no issue (the controller's) is skipped, since its fact would name no record. The
// fact's run is the claim's ServingRun, the issue generation its task came from, and not its
// Generation, which counts launches and goes into the id instead.
//
// An architect's claim is the owner of its issue's steps, and its silence is an owner's: it is
// minted only over a run of its issue that started and has not ended (openRuns), past the bound
// from both the last step and that run's start, so the owner had the bound to answer the run's
// own notice. Its id is the run's too (ownerSilentEventID): a run that starts while the owner
// stays silent is a silence of its own. The handler tells the owner the step again where the run
// is a step of the owner's (workflow's ownerSilent); a worker's run read here is spent once.
func (s *supervision) tellSilentPhaseWorkers(ctx context.Context, claims []supervise.Claim) {
	if s.applySweepFacts == nil {
		return
	}
	bound := s.cfg.WorkerSilence
	now := s.plan.clock.Now()
	var facts []sweptFact
	var owners []supervise.Claim
	for _, c := range claims {
		if c.Issue == "" || c.Role == "" {
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
		if c.Role == claim.RoleArchitect {
			if !workflow.ReportExplainsQuiet(c.ActivityBackground, c.ActivityNextReminderAt, now) {
				owners = append(owners, c)
			}
			continue
		}
		// The handler refuses a claim whose report explains its quiet, so none is minted for it.
		if !workflow.ReportExplainsQuiet(c.ActivityBackground, c.ActivityNextReminderAt, now) {
			facts = append(facts, sweptFact{
				ID: workerSilentEventID(c.Token, c.Generation, last),
				Fact: intake.WorkerSilent{Issue: c.Issue, Role: c.Role, Claim: c.Token, Generation: c.ServingRun(),
					LastStep: last, Bound: bound},
			})
		}
		if fact, told := backgroundStalledFact(c, s.cfg.WorkerBackground, now); told {
			facts = append(facts, fact)
		}
	}
	facts = append(facts, s.ownersSilent(ctx, owners, bound, now)...)
	if len(facts) == 0 {
		return
	}
	// workflowRuntime.applySweepFacts logs every failure, and the next sweep retries it.
	_ = s.applySweepFacts(ctx, facts)
}

// ownersSilent is the fact of each architect of owners, every one idle past bound since its last
// step, whose issue holds a run that started and has not ended and started before bound too: the
// owner of that run had the bound to answer the run's notice. A failed read mints none and is
// logged, and the next sweep reads again.
func (s *supervision) ownersSilent(ctx context.Context, owners []supervise.Claim, bound time.Duration, now time.Time) []sweptFact {
	if len(owners) == 0 || s.openRuns == nil {
		return nil
	}
	issues := make([]string, 0, len(owners))
	for _, c := range owners {
		issues = append(issues, c.Issue)
	}
	runs, err := s.openRuns(ctx, issues)
	if err != nil {
		s.log.Warn("the silence sweep could not read the open runs of the idle architects' issues; none is told and the next sweep reads again",
			"issues", len(issues), "error", err)
		return nil
	}
	var facts []sweptFact
	for _, c := range owners {
		run, open := runs[c.Issue]
		if !open {
			continue
		}
		last := c.Step.LastStep()
		since := last
		if run.StartedAt.After(since) {
			since = run.StartedAt
		}
		if !workflow.PastBound(since, bound, now) {
			continue
		}
		facts = append(facts, sweptFact{
			ID: ownerSilentEventID(c.Token, c.Generation, last, run),
			Fact: intake.WorkerSilent{Issue: c.Issue, Role: c.Role, Generation: c.ServingRun(),
				LastStep: last, Bound: bound},
		})
	}
	return facts
}

// ownerSilentEventID is the identity of one silence of an owner over one run: the claim token, the
// claim's launch, the moment of its last step, as workerSilentEventID, and the run, its place and
// its count of starts, so a run that starts while the owner stays silent is a silence of its own.
func ownerSilentEventID(token claim.Token, generation uint64, lastStep time.Time, run record.IssueStepRun) string {
	return fmt.Sprintf("owner-silent:%s:%d:%s:%s/%s/%d/%d", token, generation, lastStep.UTC().Format(time.RFC3339Nano),
		run.Place.Task, run.Place.Kind, run.Place.Place, run.Starts)
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
		Fact: intake.WorkerBackgroundStalled{Issue: c.Issue, Role: c.Role, Claim: c.Token, Generation: c.ServingRun(),
			ReportedAt: reported, Kinds: c.ActivityKinds, Bound: bound},
	}, true
}
