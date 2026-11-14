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
// One reason for a quiet is the record's and not the live claim's: a merger the closed design
// gate holds, and a worker whose wait stands (dispatch://INFRA-2040 R1 and R3). The handler
// refuses both and leaves the fact unspent, so a fact minted for one would open a transaction on
// the fact lock every minute for as long as the hold or the wait stood. So the claims the filter
// lets through are put to the record once per sweep (supervision.quietByDesign, one read-only
// transaction of batched reads, made only where some claim got this far), and none of the claims
// it answers is minted a fact. A failed read mints nothing for the sweep and is logged: the next
// sweep reads again, and nothing of a silence is lost to it, since an unspent id is the same id.
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
	var quiet []supervise.Claim
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
		if !workflow.PastBound(c.Step.LastStep(), bound, now) {
			continue
		}
		quiet = append(quiet, c)
	}
	if len(quiet) == 0 {
		return
	}
	explained, err := s.quietExplained(ctx, quiet)
	if err != nil {
		s.log.Warn("the silence sweep could not read which claims a wait or the design gate explains; it tells the workflow of none and reads them again at the next sweep",
			"claims", len(quiet), "error", err)
		return
	}
	var facts []sweptFact
	for _, c := range quiet {
		if _, held := explained[c.Token]; held {
			continue
		}
		last := c.Step.LastStep()
		// The handler refuses a claim whose report explains its quiet, so none is minted for it.
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

// quietExplained is, of claims, the ones the record gives a reason of the daemon's own for their
// quiet (supervision.quietByDesign), by token; none where no workflow answers that, which leaves
// every such claim to its handler, which refuses it all the same.
func (s *supervision) quietExplained(ctx context.Context, claims []supervise.Claim) (map[claim.Token]struct{}, error) {
	if s.quietByDesign == nil {
		return nil, nil
	}
	keys := make([]record.RoleRunKey, 0, len(claims))
	for _, c := range claims {
		keys = append(keys, record.RoleRunKey{Token: c.Token, Issue: c.Issue, Role: c.Role, Generation: c.ServingRun()})
	}
	return s.quietByDesign(ctx, keys)
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
