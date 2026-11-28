package workflow

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// setWorkflow records the sequence of phases an issue's architect chose for it, with the reason
// the architect gave, on the issue's record (record.Issue.Workflow), where every later move of the
// issue reads it (dispatch://INFRA-3570). It is the architect's one door onto that choice, and it
// is applied as every architect write is: one fact, in one transaction, at most once.
//
// Two refusals come before the write, each a 409 that changes nothing, in the order a reader
// fixes them in. WORKFLOW_NOT_OFFERED: the project offers its architects only the sequences its
// deployment lists (`projects.<KEY>.workflows`, Config.Workflows), and a project that lists none
// offers none, the default's own name included, so every issue of it walks the project's default
// and this release changes nothing for a project nobody opted in. WORKFLOW_AFTER_START: an issue's
// line is chosen before its first phase starts (record.Issue.SequenceOpen), since a line changed
// under a running issue could leave it standing in a phase the new line does not walk, where no
// row would ever move it; a held issue is past that point too, for its hold came from a claim that
// failed inside a started phase. The route refuses a name the library does not hold before the
// fact (api's WORKFLOW_UNKNOWN), so a fact naming one is an error here, not a refusal.
//
// The choice is no movement of the issue (the six the rule in dispatchIssue names): it moves no
// phase, starts nothing and records no completion, so it marks nothing.
func (e *Engine) setWorkflow(ctx context.Context, tx pgx.Tx, fact intake.SetWorkflow) (intake.Result, error) {
	if _, ok := phase.SequenceFor(fact.Workflow); !ok {
		return intake.Result{}, fmt.Errorf("set the workflow of %s to %q: the library holds %s, and the route refuses every other name",
			fact.Issue, fact.Workflow, strings.Join(phase.SequenceNames(), ", "))
	}
	issue, err := e.store.Issue(ctx, tx, fact.Issue)
	if err != nil || issue == nil {
		return intake.Result{}, err
	}
	if !slices.Contains(e.cfg.Workflows, fact.Workflow) {
		return refused("WORKFLOW_NOT_OFFERED", fmt.Sprintf("project %s does not offer the workflow %s: it offers %s (`projects.%s.workflows` in legion.yaml), and an issue of it walks the project's default otherwise; this choice of %s's workflow changed nothing",
			e.cfg.Project, fact.Workflow, offeredNames(e.cfg.Workflows), e.cfg.Project, issue.Key)), nil
	}
	if !issue.SequenceOpen() {
		return refused("WORKFLOW_AFTER_START", fmt.Sprintf("%s stands in %s, so its workflow is fixed: an issue's workflow is chosen before the issue's first phase starts, at admitted or while its root waits on a person; this choice of %s changed nothing",
			issue.Key, standing(*issue), fact.Workflow)), nil
	}
	issue.Workflow, issue.WorkflowReason = fact.Workflow, fact.Reason
	if err := e.store.PutIssue(ctx, tx, *issue); err != nil {
		return intake.Result{}, err
	}
	e.logOnCommit(ctx, "workflow: the architect chose the issue's workflow", "issue", issue.Key, "workflow", fact.Workflow, "reason", fact.Reason)
	return intake.Result{}, nil
}

// offeredNames is the sequences a project offers, as a refusal names them: "none" for a project
// that offers none.
func offeredNames(offered []string) string {
	if len(offered) == 0 {
		return "none"
	}
	return strings.Join(offered, ", ")
}

// standing is where issue stands, as a refusal names it: its phase, and for a held issue the phase
// the hold came from too (record.StandsAt).
func standing(issue record.Issue) string {
	if at := record.StandsAt(issue); at != issue.Phase {
		return fmt.Sprintf("%s (held from %s)", issue.Phase, at)
	}
	return string(issue.Phase)
}
