package workflow

import (
	"context"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
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
func stepWork(p phase.Phase) (role, typ string, ok bool) {
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
// its count of starts is kept under, its place in the task (from 1 for a step, 0 for a retro or a
// merge, which no plan names, so the kind tells those two apart), its role and its type ("" for a
// retro or a merge). A working phase's step is the first step of steps whose role works it, at one
// plus its index. A phase no step of steps works, and a phase that runs no step — admitted,
// awaiting a person, held, done — answers ok false.
func stepIn(steps []string, p phase.Phase) (place record.StepPlace, step int, role, typ string, ok bool) {
	switch p {
	case phase.Retro:
		return record.StepPlace{Task: defaultTask, Kind: "retro"}, 0, "implementer", "", true
	case phase.Merging:
		return record.StepPlace{Task: defaultTask, Kind: "merge"}, 0, "merger", "", true
	}
	role, typ, ok = stepWork(p)
	if !ok {
		return record.StepPlace{}, 0, "", "", false
	}
	for i, named := range steps {
		if stepRole, _, _ := strings.Cut(named, ":"); stepRole == role {
			return record.StepPlace{Task: defaultTask, Kind: "step", Place: i + 1}, i + 1, role, typ, true
		}
	}
	return record.StepPlace{}, 0, "", "", false
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

// rateRetro writes the rating the retro of issue's run gave the plan onto that retro's record, in
// the transaction of the completion that ended the retro: a step record row whose id is the id of
// the retro's latest run (StepID, from the count of its starts, which the retro's end left as it
// was), carrying every identity field the run's start and end carry, the agent its start named
// among them, so the write makes the record where neither has landed yet, and the rating and its
// reason. It carries no moment and no result, which are the end's. Dispatch sets each field once,
// so the end and the rating merge in either order. A record key that is no issue key writes nothing
// (recordsSteps).
func (e *Engine) rateRetro(ctx context.Context, tx pgx.Tx, issue record.Issue, rating int, reason string) error {
	place, step, role, typ, _ := stepOf(phase.Retro)
	if !recordsSteps(issue) {
		return nil
	}
	start, err := e.store.StepStarts(ctx, tx, issue.Key, place)
	if err != nil {
		return err
	}
	write := stepWrite(issue, place, step, role, typ, start.Agent, start.Starts)
	write.Rating, write.RatingReason = new(rating), new(reason)
	return e.enqueue(ctx, tx, issue.Key, write)
}
