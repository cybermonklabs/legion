package api

import (
	"fmt"
	"net/http"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

var _ = declareRoute(routeDecl{
	Method:      http.MethodGet,
	Pattern:     "/api/v1/going/runs/{id}/plan",
	Auth:        authAny,
	Description: fmt.Sprintf("The caller's own command-bar run's latest plan: {run, plan, state (waiting, approved, running, done or rejected), summary, mode (ask or auto_review), rows: [{row, tool, class, arguments, needs, state (waiting, approved, unticked, running, done, failed or skipped), item, issue, fields, now, claim, waits, changed, deleted?, result {status, code, message, delivery}}], version, withdrawn?}. Each row is one change the run's model proposed: the write tool and the arguments it runs with, the rows before it that it needs, the item it changes, the fields it writes and their value now, the holder of its issue's claim, why it waits for the person in every mode (its class, delivers_to_session, claimed_by_session), and changed for a row that waits again because its item changed after the person saw it. result is null until the row's write answered, and its delivery is whether a row that sends a message or a broadcast was sent, queued (a delivery not yet confirmed included) or refused, null for any other row. A row of an edit or a delete of a comment or an issue message is a row of the post: item is the post, issue its issue, fields [body] and now {author, body}; once its author deletes the post, whatever the row's state, now is the delete's mark {author, deleted_at, deleted_by} with no part of the body, and deleted, true, is on the row alone. version names the plan as this answer shows it, and moves whenever what it shows moves; an approval names it. While the run waits, each waiting row's item is read again as the caller, and the plan shows that read from then on. withdrawn, true, is on the plan of a run another writer's delete withdrew alone (GET /api/v1/going/runs/{id}), which may quote the deleted post, so its summary is empty, each row's arguments {} and its now null, no item is read again, and it is approved and revised by nobody (409 %s). The plan of a run its own deletes alone withdrew (a stop_reason starting %s) is answered as an ended run's, its rows and their results as they stand, with an empty summary, which its model wrote. 404 %s for a run that is not the caller's or that the record does not hold, 404 %s for a run that proposed no plan.", codeGoingRunWithdrawn, contracts.GoingErrorAnswerWithdrawnByRun, codeGoingRunNotFound, codeGoingPlanNotFound),
	Handler:     (*server).getGoingPlan,
	ID:          &idRoute{Reason: "a going run's id, which the route reads for its own author alone and answers for itself"},
	OAuthScopes: routeScopeSet{model.ScopePerson},
	Read:        readEntry{Writes: readWritesState, WriteNote: "While the run waits for its person, the read reads each waiting row's item again as the caller through the in-process GET routes, and where what it reads differs from what the row stored it updates the row's shown in going_plan_rows on the request's context (storeRowShown); a withdrawn run that has not ended is first stopped on the request's context (readOwnGoingRun, stopGoingRun), which updates going_runs, inserts its end frame into going_run_events, appends going_plan.updated to events for a run that was waiting, and notifies GoingRunsChannel. The in-process document reads may queue a compaction, as docsCompactionNote says."},
	Shape:       routeShape{Status: http.StatusOK, Response: goingPlanAnswer{}},
})
