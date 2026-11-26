package api

// going_new_issue.go is the command bar's line File as a new issue (R9). Enter on the line starts
// a run of the kind new_issue (POST /api/v1/going/runs, agentwire.RunNewIssue): a Fast run whose
// one task is to file the person's words as one new issue, with its title, project, parent,
// priority and a short spec the agent writes from the words. Its plan is that issue's create alone
// (dispatch_issue), carrying a spec whatever its person's mode, or the plan call refuses it and
// tells the model to write one (goingNoSpec); and the Enter that started the run approved it, so
// the plan is kept with the create approved and the agent runs it at once: the one plan of the bar
// that waits for nobody.
// The create carries what the deployment names for a new issue: the label of
// DISPATCH_NEW_ISSUE_LABEL among its labels, and the route of DISPATCH_NEW_ISSUE_SESSION, which no
// argument of the tool holds, kept on the row's approval and put in the create's body
// (routeSends). Nothing is posted on the issue: a message to an agent is a row of a plan the person
// approves (going_messages.go). The run's end names the issue it filed and offers Undo, which
// closes it (going_undo.go); a run that filed nothing ends failed and says why.

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/going/agentwire"
)

// goingIssueCreateTool is the tool an issue is created with.
const goingIssueCreateTool = "dispatch_issue"

// goingNewIssueLevel is the level of every new-issue run.
const goingNewIssueLevel = "fast"

// goingNewIssueNotFiled is the code of the reason a new-issue run that filed nothing ends failed
// with.
const goingNewIssueNotFiled = "NEW_ISSUE_NOT_FILED"

// goingNewIssueProposal refuses a new-issue run's proposal that is not one create of an issue,
// CALL_REFUSED, which its model is told; nil for one that is.
func goingNewIssueProposal(proposal *agentwire.PlanProposal) *agentwire.Error {
	if len(proposal.Changes) != 1 || proposal.Changes[0].Tool != goingIssueCreateTool {
		return goingAgentRefusal(agentwire.CodeCallRefused,
			"this run files the person's words as one new issue: propose one change, %s, and no other", goingIssueCreateTool)
	}
	return nil
}

// goingNewIssueSettings puts what the deployment names for a new issue on a new-issue run's
// create, arguments: its label among the create's labels, where it names one the create does not
// carry; and answers the route the create takes, session:<id> of the session it names, "" for
// none (configuredNewIssue). It runs before the tool's schema reads arguments (goingReadRow), so
// labels that are no list, null included, are left as the model wrote them for the schema to
// refuse CALL_REFUSED, and not replaced by the deployment's label.
func (s *server) goingNewIssueSettings(arguments map[string]any) string {
	set := s.configuredNewIssue()
	if set.Label != "" {
		written, held := arguments["labels"]
		labels, isList := written.([]any)
		if (!held || isList) && !slices.Contains(labels, any(set.Label)) {
			arguments["labels"] = append(labels, set.Label)
		}
	}
	if set.Session == "" {
		return ""
	}
	return "session:" + set.Session
}

// goingNewIssueApprovedBy is who approved a new-issue run's plan, as going_plans.approved_by holds
// it: its person, login, by the Enter that started the run.
func goingNewIssueApprovedBy(login string) map[string]any {
	return map[string]any{"by": goingApprovedByPerson, "login": login, "on": "start"}
}

// newIssueRoute is the route the row's create of a new issue carries beside its arguments, as its
// approval names it: "" for every other row.
func (row goingPlanRowStored) newIssueRoute() string {
	if row.tool != goingIssueCreateTool || row.approval == nil {
		return ""
	}
	return row.approval.Route
}

// goingNewIssueEnd is what the end of run's turn makes of frame, where run is a new-issue run,
// read in tx: such a run waits for nobody, so a turn it would leave waiting ends done; a create
// that landed names the issue it filed and offers Undo, whatever state the turn ends in; and a
// turn that ends done having filed nothing ends failed with why, the refusal of its create, or
// that it proposed none. The turn of any other run is left as it is.
func goingNewIssueEnd(ctx context.Context, tx pgx.Tx, run string, frame *agentwire.EndFrame) error {
	var newIssue bool
	var issue *string
	if err := tx.QueryRow(ctx, `
		select coalesce(r.kind = 'new_issue', false),
		  (select l.answer->'body'->>'key' from going_landings l
		   where l.run_id = r.id and l.phase = 'do' and l.method = $2 and l.path = '/api/v1/issues'
		   order by l.step, l.call, l.request limit 1)
		from going_runs r where r.id = $1::uuid
	`, run, http.MethodPost).Scan(&newIssue, &issue); err != nil {
		return fmt.Errorf("read what new-issue run %s filed: %w", run, err)
	}
	if !newIssue {
		return nil
	}
	if frame.State == agentwire.StateWaiting {
		frame.State = agentwire.StateDone
	}
	if issue != nil && *issue != "" {
		frame.Issue = *issue
		frame.Offers = append(frame.Offers, agentwire.OfferUndo)
		return nil
	}
	if frame.State != agentwire.StateDone {
		return nil
	}
	why := "the run proposed no issue to file"
	var state, code, message string
	var status int
	err := tx.QueryRow(ctx, `
		select state, coalesce(result->>'code', ''), coalesce(result->>'message', ''), coalesce((result->>'status')::int, 0)
		from going_plan_rows where run_id = $1::uuid order by plan desc, row limit 1
	`, run).Scan(&state, &code, &message, &status)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
	case err != nil:
		return fmt.Errorf("read the create of new-issue run %s: %w", run, err)
	case message != "" && code != "":
		why = fmt.Sprintf("the issue was not filed: %s: %s", code, message)
	case message != "":
		why = "the issue was not filed: " + message
	case status != 0:
		why = fmt.Sprintf("the issue was not filed: its create answered %d", status)
	default:
		why = fmt.Sprintf("the issue was not filed: its create is %s and did not run", state)
	}
	frame.State, frame.Reason = agentwire.StateFailed, goingNewIssueNotFiled+": "+why
	return nil
}
