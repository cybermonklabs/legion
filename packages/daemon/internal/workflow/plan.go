package workflow

import (
	"context"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/sjawhar/envoy/steprecord"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// DefaultSteps is the steps of the one task of an issue whose owner gave no plan, in order, each a
// role or `role:key`: the one place they are stated. The plan each run writes when its work starts
// names them (defaultPlan), and the place and the role of the step a phase runs are read from them
// (stepOf). It answers a fresh copy each time, so no caller's change to its slice reaches another
// caller: Go has no constant of a slice, and a package variable would be state each caller shares.
func DefaultSteps() []string {
	return []string{"planner", "implementer", "tester", "reviewer", "acceptance:owner"}
}

// stepWork is the role whose step works phase p, as a step of a plan names it (the part before a
// step's `:key`), and the type of that work; ok is false for a phase no step of a plan works.
func stepWork(p phase.Phase) (role stepRole, typ stepType, ok bool) {
	switch p {
	case phase.Planning:
		return "planner", "planning", true
	case phase.Implementing:
		return "implementer", "implementation", true
	case phase.Testing:
		return "tester", "testing", true
	case phase.Reviewing:
		return "reviewer", "review", true
	case phase.Accepting:
		return "acceptance", "review", true
	}
	return "", "", false
}

// stepIn is the step phase p runs as a step of the task default whose steps are steps: the place
// its count of starts is kept under, whose Place is its place in the task (from 1 for a step, 0 for
// a retro or a merge, which no plan names, so the kind tells those two apart), its role and its
// type ("" for a retro or a merge). A working phase's step is the first step of steps whose role
// works it, at one plus its index. A phase no step of steps works, and a phase that runs no step —
// admitted, awaiting a person, held, done — answers ok false.
func stepIn(steps []string, p phase.Phase) (place record.StepPlace, role stepRole, typ stepType, ok bool) {
	switch p {
	case phase.Retro:
		return record.StepPlace{Task: defaultTask, Kind: steprecord.KindRetro}, "implementer", "", true
	case phase.Merging:
		return record.StepPlace{Task: defaultTask, Kind: steprecord.KindMerge}, "merger", "", true
	}
	role, typ, ok = stepWork(p)
	if !ok {
		return record.StepPlace{}, "", "", false
	}
	for i, named := range steps {
		if head, _, _ := strings.Cut(named, ":"); stepRole(head) == role {
			return record.StepPlace{Task: defaultTask, Kind: steprecord.KindStep, Place: i + 1}, role, typ, true
		}
	}
	return record.StepPlace{}, "", "", false
}

// planRecordNamespace is the namespace of every plan record's id.
var planRecordNamespace = uuid.NewSHA1(uuid.NameSpaceOID, []byte("legion-plan-record"))

// PlanID is the id of the plan one run of an issue carries out: a version 5 uuid of the issue and
// its run (record.Issue.Generation), so a write of the plan sent again lands on the same plan and
// the next run writes a plan of its own.
func PlanID(issue string, generation uint64) string {
	return uuid.NewSHA1(planRecordNamespace, []byte(fmt.Sprintf("%s/%d", issue, generation))).String()
}

// defaultPlan is the plan of issue's run whose owner gave none: the source default, and one task,
// default, whose steps are DefaultSteps.
func defaultPlan(issue record.Issue) record.PlanRecord {
	return record.PlanRecord{ID: PlanID(issue.Key, issue.Generation), Generation: issue.Generation, Source: "default",
		Tasks: []record.PlanRecordTask{{ID: defaultTask, Steps: DefaultSteps()}}}
}

// WritePlan queues the plan issue's run carries out, the default plan (defaultPlan), where the
// run's work starts: at the run's move out of admitted (transition's TriggerGateOpened), and at a
// hand-over's carry, which re-admits an orphan at a new generation in the phase it ran in
// elsewhere (admit's readmit), so that run never leaves admitted. A record key that is no issue
// key writes nothing (recordsSteps).
func (e *Engine) WritePlan(ctx context.Context, tx pgx.Tx, issue record.Issue) error {
	if !recordsSteps(issue) {
		return nil
	}
	return e.enqueue(ctx, tx, issue.Key, defaultPlan(issue))
}

// rateRetro writes the rating the retro of issue's run gave the plan onto that retro's record, in
// the transaction of the completion that ended the retro: a step record row whose id is the id of
// the retro's latest run (StepID, from the count of its starts, which the retro's end left as it
// was), carrying every identity field the run's start and end carry, the agent its start named
// among them, so the write makes the record where neither has landed yet, and the rating and its
// reason. It carries no moment and no result, which are the end's. Dispatch sets each field once,
// so the end and the rating merge in either order. The retro's step comes from stepAt, which every
// writer of a step's record takes, so a record key that is no issue key writes nothing.
func (e *Engine) rateRetro(ctx context.Context, tx pgx.Tx, issue record.Issue, rating record.PlanRating) error {
	step, recorded := stepAt(issue, phase.Retro)
	if !recorded {
		return nil
	}
	start, err := e.store.StepStarts(ctx, tx, issue.Key, step.place)
	if err != nil {
		return err
	}
	write := stepWrite(issue, step.place, step.role, step.typ, start.Agent, start.Starts)
	write.PlanRating = &rating
	return e.enqueue(ctx, tx, issue.Key, write)
}

// planRatingRefusal is the rule of which completion carries the rating of the plan, judged where
// the retro ends: handoff reads it on the issue as the completion's own transaction reads it, under
// the lock every fact applies under, which is the read its TriggerRetroCompleted ends the retro on.
// The route held what no state of the issue changes: the rating's form (api's planRatingOf) and
// whether the caller must rate (intake.HandoffComplete.MustRate). endsRetro is whether this
// completion ends the retro, and reported the phase it is of, which the refusal names. The
// completion that ends the retro with no rating from a caller that must rate is refused
// PLAN_RATING_REQUIRED, and any other completion that carries one PLAN_RATING_UNEXPECTED; nil is
// no refusal.
func planRatingRefusal(fact intake.HandoffComplete, endsRetro bool, reported phase.Phase) *intake.Refusal {
	what := fmt.Sprintf("this completion of phase %s of %s", reported, fact.Issue)
	switch {
	case endsRetro && fact.PlanRating == nil && fact.MustRate:
		return intake.PlanRatingRequired(what)
	case !endsRetro && fact.PlanRating != nil:
		return intake.PlanRatingUnexpected(what)
	}
	return nil
}
