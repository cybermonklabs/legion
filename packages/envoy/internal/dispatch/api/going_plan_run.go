package api

// going_plan_run.go runs a command-bar run's approved plan (R4). The agent runs the rows of its
// run's plan that the person approved, or that Auto-review let through, in order, each as a route
// call that names its row (agentwire.CallRequest.Row), through the same in-process path and the
// same landings as every call of the run (going_agent_call.go), as the person the run's grant
// resolves to at that call. The server holds each such call to its row (rowGate): a write that
// names no row, or a row with another tool or other arguments, or a row that is not approved, is
// refused WRITE_NOT_APPROVED, so a run writes nothing outside its approved rows. Before each write
// of a row its item is read again as the person - before its first try, and before a second try
// of its call key that finds the key's own write not landed, which writes too: a row whose item
// changed after its approval - another value, or another holder of its issue's claim - waits for
// the person again, ROW_WAITS, saying what of its call already landed and stands, and a row whose
// needed row failed or was skipped is skipped, ROW_SKIPPED; the rows that need neither run on. A
// row's call key is kept on the row, so a second try of that key - the agent died after the call,
// or the server between the write's commit and its answer - answers what the first landed and
// sends nothing twice, and no other key runs the row; the next try of a row that waited again
// after part of its call landed takes over what landed, by its digest, and sends only the rest.
// A row's words name each row by its action and item, as the page does (goingRowLabel). A row that
// names the issue an earlier create of its plan makes, as "$N", runs with that issue's key put in
// its place by the server (resolveNewIssues), while the row as approved, which the agent's call
// names, keeps "$N".
//
// Auto-review (going_modes.go) is a review call of the agent's (agentwire.KindReview), once its
// model call named the rows the person's words ask for: the server lets through those its mode
// policy lets through and answers the rows to run.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net/http"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/going/agentwire"
)

// goingRowNotApproved is the refusal of a write that names no approved row of its run's plan.
func goingRowNotApproved(tool, why string) *agentwire.Error {
	return goingAgentRefusal(agentwire.CodeWriteNotApproved,
		"%s was not sent: %s; a change runs only as a row of a plan the person approved, which propose_plan proposes", tool, why)
}

// rowGate holds a route call of a write tool to the row of its run's latest plan it names, and
// answers the row with no refusal where the call may send: an approved row whose item reads as it
// did at its approval and whose needed rows ran, which it marks running under the call's key; or a
// row that key already ran, whose second try answers what it landed, and whose item is read again
// first where nothing of the key landed (landed), since that try writes (rowItemChanged); routeCall
// reads it again where part of the call landed and the rest is to be sent. The call names the row
// as approved, a new issue of the plan as "$N", and the row answered holds the key of each such
// issue in its place (resolveNewIssues), which the call is sent with. A row the key marked waiting
// or skipped is answered that refusal again. doer reads the row's item as the run's person
// (login).
func (a *AgentListener) rowGate(ctx context.Context, request agentwire.CallRequest, login string, doer GoingDoer,
	landed []*goingLandedRequest,
) (goingPlanRowStored, *agentwire.Error) {
	if request.Row == nil {
		return goingPlanRowStored{}, goingRowNotApproved(request.Tool, "call_tool runs no tool that changes something")
	}
	plan, err := readGoingPlan(ctx, a.s.deps.Store.Pool, request.Run, 0)
	if err != nil {
		return goingPlanRowStored{}, goingAgentFailed(agentwire.CallCall, err)
	}
	if plan == nil || plan.plan != request.Row.Plan {
		return goingPlanRowStored{}, goingRowNotApproved(request.Tool, fmt.Sprintf("plan %d is not this run's latest plan", request.Row.Plan))
	}
	at := slices.IndexFunc(plan.rows, func(row goingPlanRowStored) bool { return row.row == request.Row.Row })
	if at < 0 {
		return goingPlanRowStored{}, goingRowNotApproved(request.Tool, fmt.Sprintf("plan %d holds no row %d", plan.plan, request.Row.Row))
	}
	row := plan.rows[at]
	if row.tool != request.Tool || !goingJSONEqual(row.arguments, request.Arguments) {
		return row, goingRowNotApproved(request.Tool, fmt.Sprintf("row %d of plan %d is %s with other arguments", row.row, plan.plan, row.tool))
	}
	ours := row.result != nil && row.result.Step == request.Step && row.result.Call == request.Call
	switch row.state {
	case "approved":
		return a.beforeRowWrite(ctx, request, plan, row, login, doer)
	case "running", "done", "failed":
		if !ours {
			return row, goingRowNotApproved(request.Tool, fmt.Sprintf("row %d of plan %d ran under another call", row.row, plan.plan))
		}
		var refusal *agentwire.Error
		if row, refusal = a.resolveNewIssues(ctx, request, plan, row); refusal != nil {
			return row, refusal
		}
		if goingLastLanded(landed) == nil {
			// Nothing of the key landed - its earlier try failed in the server, or its route
			// refused the write or changed nothing - so this try writes, and the row's item is
			// read again first.
			return row, a.rowItemChanged(ctx, request, row, login, doer, nil)
		}
		return row, nil
	case "waiting", "skipped":
		if ours && row.result.Code != "" {
			return row, goingAgentRefusal(row.result.Code, "%s", row.result.Message)
		}
	}
	return row, goingRowNotApproved(request.Tool, fmt.Sprintf("row %d of plan %d is %s", row.row, plan.plan, row.state))
}

// beforeRowWrite is the gate of an approved row's first try: a needed row that failed or was
// skipped skips it, and one that waits makes it wait; each new issue it names is put in as its key
// (resolveNewIssues), and its item read again as the person that no longer reads as it did at its
// approval makes it wait again (rowItemChanged); and otherwise it is marked running under the
// call's key, in a statement that holds the run's lease, keeping the keys of the row's earlier
// tries whose landings stay the row's (goingRowRecord.Resumes). It answers the row with its keys
// put in. Its words name each row it needs as the page names it (goingRowLabel), never by its
// number.
func (a *AgentListener) beforeRowWrite(ctx context.Context, request agentwire.CallRequest, plan *goingPlanStored, row goingPlanRowStored,
	login string, doer GoingDoer,
) (goingPlanRowStored, *agentwire.Error) {
	key := goingRowRecord{Step: request.Step, Call: request.Call}
	for _, need := range row.needs {
		at := slices.IndexFunc(plan.rows, func(stored goingPlanRowStored) bool { return stored.row == need })
		if at < 0 {
			continue
		}
		needed := plan.rows[at]
		switch needed.state {
		case "done":
		case "failed", "skipped":
			why := fmt.Sprintf("this change was skipped: it needs %s, which %s", goingRowLabel(needed), goingNeededState(needed.state))
			if needed.result != nil && needed.result.Message != "" {
				why += ": " + needed.result.Message
			}
			key.Code, key.Message = agentwire.CodeRowSkipped, why
			return row, a.settleRow(ctx, request, row, "skipped", key, nil)
		case "waiting":
			key.Code, key.Message = agentwire.CodeRowWaits, fmt.Sprintf("this change waits for approval: it needs %s, which waits for approval", goingRowLabel(needed))
			return row, a.settleRow(ctx, request, row, "waiting", key, nil)
		default:
			return row, goingRowNotApproved(request.Tool, fmt.Sprintf("row %d needs row %d, which has not run", row.row, need))
		}
	}
	row, refusal := a.resolveNewIssues(ctx, request, plan, row)
	if refusal != nil {
		return row, refusal
	}
	if refusal := a.rowItemChanged(ctx, request, row, login, doer, nil); refusal != nil {
		return row, refusal
	}
	tag, err := a.s.deps.Store.Pool.Exec(ctx, `
		update going_plan_rows set state = 'running', result = jsonb_strip_nulls(jsonb_build_object('resumes', result->'resumes')) || $4::jsonb
		where run_id = $1::uuid and plan = $2 and row = $3 and state = 'approved'
		  and exists (select 1 from going_runs where id = $1::uuid and lease_token = $5::uuid and state = 'working')
	`, request.Run, plan.plan, row.row, goingRecordJSON(key), request.LeaseToken)
	if err != nil {
		return row, goingAgentFailed(agentwire.CallCall, err)
	}
	if tag.RowsAffected() == 0 {
		if _, stale := a.callRun(ctx, request); stale != nil {
			return row, stale
		}
		return row, goingAgentRefusal(agentwire.CodeCallUnknown, "row %d of plan %d was taken by another call meanwhile", row.row, plan.plan)
	}
	return row, a.refreshPlanState(ctx, request.Run, plan.plan)
}

// goingCreatedKeysSQL reads, for each row $3 of plan $2 of run $1 that is a create of an issue and
// is done, the key of the issue its route answered and the status it answered with: what the
// landing of the row's call key recorded beside the create's one request, POST /api/v1/issues
// (goingRecordAnswer). The route answers 201 for an issue it made, and 200 for an issue Dispatch
// already held for the external issue the create names (respondWithExistingExternalIssue), of
// which it made nothing.
const goingCreatedKeysSQL = `
	select r.row, l.answer->'body'->>'key', coalesce((l.answer->>'status')::int, 0)
	from going_plan_rows r
	join going_landings l on l.run_id = r.run_id and l.phase = 'do'
	  and l.step = (r.result->>'step')::int and l.call = (r.result->>'call')::int
	  and l.method = 'POST' and l.path = '/api/v1/issues'
	where r.run_id = $1::uuid and r.plan = $2 and r.row = any($3) and r.state = 'done'
	  and r.change->>'tool' = '` + goingIssueCreateTool + `' and l.answer->'body'->>'key' is not null
`

// resolveNewIssues is row with the key of each new issue it names as "$N" (goingNewIssuesNamed)
// in place of the name: the issue row N of plan made, read off that create's landing
// (goingCreatedKeysSQL). The server puts the key in, so the read of the row's item before its
// write, the write, its landings and its undo all name the issue itself, while the row as the
// person approved it keeps "$N", which the agent's call names (rowGate). A row that names none is
// answered as it is. Only a create whose route answered 201 made a new issue, and a row whose row
// N made none is skipped, ROW_SKIPPED, so no write is sent with a "$N" in it and none is made to
// or under an issue the plan did not make. Row N made none where it found an issue Dispatch
// already held (foundIssue), which the row's words name by its key, and where the record
// holds no key of it, which the plan's own refusals (goingNewIssueRefusal) and the gate of the
// rows a row needs (beforeRowWrite) keep from coming here.
func (a *AgentListener) resolveNewIssues(ctx context.Context, request agentwire.CallRequest, plan *goingPlanStored, row goingPlanRowStored,
) (goingPlanRowStored, *agentwire.Error) {
	arguments, err := goingRowArguments(row.arguments)
	if err != nil {
		return row, goingAgentFailed(agentwire.CallCall, err)
	}
	named := goingNewIssuesNamed(row.tool, arguments)
	if len(named) == 0 {
		return row, nil
	}
	read, err := a.s.deps.Store.Pool.Query(ctx, goingCreatedKeysSQL, request.Run, plan.plan, named)
	if err != nil {
		return row, goingAgentFailed(agentwire.CallCall, fmt.Errorf("read the issues %s names: %w", goingRowLabel(row), err))
	}
	keys, held := map[int]string{}, map[int]string{}
	for read.Next() {
		var created, status int
		var key string
		if err := read.Scan(&created, &key, &status); err != nil {
			read.Close()
			return row, goingAgentFailed(agentwire.CallCall, err)
		}
		if status == http.StatusCreated {
			keys[created] = key
		} else {
			held[created] = key
		}
	}
	if err := read.Err(); err != nil {
		return row, goingAgentFailed(agentwire.CallCall, fmt.Errorf("read the issues %s names: %w", goingRowLabel(row), err))
	}
	for _, created := range named {
		if keys[created] != "" {
			continue
		}
		creates := "an issue of this plan"
		why := ""
		if at := slices.IndexFunc(plan.rows, func(stored goingPlanRowStored) bool { return stored.row == created }); at >= 0 {
			creates = goingRowLabel(plan.rows[at])
			if issue, found := a.foundIssue(ctx, plan.rows[at], held[created]); found {
				why = fmt.Sprintf("this change was skipped: %s made no new issue, since Dispatch already held %s for the external issue it names, "+
					"and no change is made to or under an issue this plan did not make", creates, issue)
			}
		}
		if why == "" {
			why = fmt.Sprintf("this change was skipped: it needs the key of %s, which this run did not record", creates)
		}
		skipped := goingRowRecord{Step: request.Step, Call: request.Call, Code: agentwire.CodeRowSkipped, Message: why}
		return row, a.settleRow(ctx, request, row, "skipped", skipped, nil)
	}
	resolved, err := json.Marshal(goingWithNewIssues(row.tool, arguments, keys))
	if err != nil {
		return row, goingAgentFailed(agentwire.CallCall, err)
	}
	row.arguments = resolved
	return row, nil
}

// foundIssue is the issue Dispatch already held that create, a row of a create of an issue,
// found in place of making one, and whether it found one: the create route answers 201 for an
// issue it made and 200 for the issue it holds for the external issue the create names
// (respondWithExistingExternalIssue). landed is the key the create's landing recorded beside a
// status other than 201 (goingCreatedKeysSQL), "" for none, and is the issue where there is one.
// A create that found its issue commits nothing, so its request records no landing
// (goingRunLanding.step), and the record holds no key of that issue: whether it found one is read
// off the row's own record, its status (recordRow), and the key is read now, off the external
// issue the create's arguments name (resolveIssueRef), which is the issue Dispatch holds for it
// now. Where the arguments do not read or that external issue no longer resolves, the issue is
// named "an issue".
func (a *AgentListener) foundIssue(ctx context.Context, create goingPlanRowStored, landed string) (string, bool) {
	if landed != "" {
		return landed, true
	}
	if create.state != "done" || create.result == nil || create.result.Status == 0 || create.result.Status == http.StatusCreated {
		return "", false
	}
	if arguments, err := goingRowArguments(create.arguments); err == nil {
		if key, err := a.s.resolveIssueRef(ctx, goingText(arguments, "external")); err == nil {
			return key, true
		}
	}
	return "an issue", true
}

// goingNewIssuesNamed is each row whose new issue arguments of tool name as "$N"
// (goingNewIssueRef), in the arguments that take one (goingNewIssueArguments), as an argument's
// value or an item of its list, in order.
func goingNewIssuesNamed(tool string, arguments map[string]any) []int {
	var named []int
	for _, argument := range goingNewIssueArguments[tool] {
		for _, item := range goingArgumentItems(arguments[argument]) {
			if created, held := goingNewIssueRef(item); held {
				named = append(named, created)
			}
		}
	}
	slices.Sort(named)
	return slices.Compact(named)
}

// goingWithNewIssues is arguments of tool with each "$N" goingNewIssuesNamed reads replaced by
// keys[N], and every other value as it is; arguments itself is left as it is.
func goingWithNewIssues(tool string, arguments map[string]any, keys map[int]string) map[string]any {
	put := func(value any) any {
		if created, held := goingNewIssueRef(value); held {
			return keys[created]
		}
		return value
	}
	resolved := maps.Clone(arguments)
	for _, argument := range goingNewIssueArguments[tool] {
		value, given := arguments[argument]
		if !given {
			continue
		}
		list, isList := value.([]any)
		if !isList {
			resolved[argument] = put(value)
			continue
		}
		items := make([]any, len(list))
		for index, item := range list {
			items[index] = put(item)
		}
		resolved[argument] = items
	}
	return resolved
}

// rowItemChanged reads the item of row, an approved row, again as the person (login, through doer)
// before a write of it, and makes the row wait again, showing the new read, where its item no
// longer reads as it did at its approval: another value, or another holder of its issue's claim. A
// read refused as the person (goingReadRefused) is passed over, since the write's own preparation
// or route refuses it the same, and that refusal is kept on the row; any other failure of the read
// ends the call failed, since a read that failed is no reason to write unchecked. landed is what
// of the call already landed under request's key, in words (goingLandedWords), none for a try
// before any of it landed: a row that waits again says it stands, and adds the key to the keys
// of its earlier tries, whose landings stay the row's (goingRowRecord.Resumes).
func (a *AgentListener) rowItemChanged(ctx context.Context, request agentwire.CallRequest, row goingPlanRowStored,
	login string, doer GoingDoer, landed []string,
) *agentwire.Error {
	if row.approval == nil {
		return nil
	}
	arguments, err := goingRowArguments(row.arguments)
	if err != nil {
		return goingAgentFailed(agentwire.CallCall, err)
	}
	now, err := a.s.goingReadRow(ctx, doer, login, row.tool, arguments, row.sendIssue())
	switch {
	case err != nil && goingReadRefused(err):
		return nil
	case err != nil:
		return goingAgentFailed(agentwire.CallCall, fmt.Errorf("read the item of %s again before its write: %w", goingRowLabel(row), err))
	case row.approval.Seen.same(now):
		return nil
	}
	now.Changed = true
	var earlier []goingRowKey
	if row.result != nil {
		earlier = row.result.Resumes
	}
	return a.settleRow(ctx, request, row, "waiting", goingRowChanged(request, landed, earlier), &now)
}

// goingReadRefused reports whether err, the error of a row's read again before its write, is a
// refusal of the person's own call rather than a failure: the tool's schema refusing the row's
// arguments, or a read refused as the person (4xx) - an item they may no longer read (403, 404), or
// a call its translation refuses. The write's own preparation or route refuses the same. A failed
// read - a store error, a route's 5xx - is not one.
func goingReadRefused(err error) bool {
	var arguments GoingArgumentError
	var refused goingRefusal
	switch {
	case errors.As(err, &arguments):
		return true
	case errors.As(err, &refused):
		return refused.Status >= http.StatusBadRequest && refused.Status < http.StatusInternalServerError
	}
	return false
}

// goingRowChanged is the record of a row that waits for the person again under request's key,
// since its item changed after their approval: landed is what of its call already landed under
// that key, in words, which its words say stands and is not sent again, and for which the record
// adds the key to earlier, the keys of the row's earlier tries that landed partly (Resumes); none
// for a row none of whose call landed, whose record keeps earlier as it is (settleRow).
func goingRowChanged(request agentwire.CallRequest, landed []string, earlier []goingRowKey) goingRowRecord {
	record := goingRowRecord{Step: request.Step, Call: request.Call, Code: agentwire.CodeRowWaits,
		Message: "this change waits for approval again: what it changes was changed after it was approved"}
	switch len(landed) {
	case 0:
		return record
	case 1:
		record.Message += fmt.Sprintf("; %s was already written, and is not sent again", landed[0])
	default:
		record.Message += fmt.Sprintf("; %s were already written, and are not sent again", strings.Join(landed, " and "))
	}
	key := goingRowKey{Step: request.Step, Call: request.Call}
	record.Resumes = append(slices.DeleteFunc(slices.Clone(earlier), func(held goingRowKey) bool { return held == key }), key)
	return record
}

// goingNeededState is a needed row's state, failed or skipped, in the words of a row it skips.
func goingNeededState(state string) string {
	if state == "skipped" {
		return "was skipped"
	}
	return state
}

// goingRowLabel is how the page names a row (plan-rows.ts rowLabel): its action, the tool's name
// without its prefix in words (action-names.ts actionName), and its item, or for a new issue, which
// has no item before it is made, its title. A row's words name another row so, never by number.
func goingRowLabel(row goingPlanRowStored) string {
	action := strings.ReplaceAll(strings.TrimPrefix(row.tool, "dispatch_"), "_", " ")
	item := row.shown.Item
	if item == "" && row.tool == goingIssueCreateTool {
		if arguments, err := goingRowArguments(row.arguments); err == nil {
			item = goingText(arguments, "title")
		}
	}
	if item == "" {
		return action
	}
	return action + " " + item
}

// settleRow sets row of request's plan to state, waiting or skipped, with record and, for a row
// that waits again, what it shows now, under the run's lease, and answers the record's refusal. The
// row is an approved one, or one request's key ran (rowItemChanged, uploadChanged). The keys of the
// row's earlier tries (goingRowRecord.Resumes) stay where record names none.
func (a *AgentListener) settleRow(ctx context.Context, request agentwire.CallRequest, row goingPlanRowStored, state string,
	record goingRowRecord, shown *goingRowShown,
) *agentwire.Error {
	var shownJSON *string
	if shown != nil {
		encoded, err := json.Marshal(shown)
		if err != nil {
			return goingAgentFailed(agentwire.CallCall, err)
		}
		text := string(encoded)
		shownJSON = &text
	}
	tag, err := a.s.deps.Store.Pool.Exec(ctx, `
		update going_plan_rows set state = $4, result = jsonb_strip_nulls(jsonb_build_object('resumes', result->'resumes')) || $5::jsonb,
		  shown = coalesce($6::jsonb, shown),
		  approval = case when $4 = 'waiting' then null else approval end
		where run_id = $1::uuid and plan = $2 and row = $3
		  and (state = 'approved' or (state in ('running', 'done', 'failed')
		       and (result->>'step')::int = $8 and (result->>'call')::int = $9))
		  and exists (select 1 from going_runs where id = $1::uuid and lease_token = $7::uuid and state = 'working')
	`, request.Run, request.Row.Plan, row.row, state, goingRecordJSON(record), shownJSON, request.LeaseToken, request.Step, request.Call)
	if err != nil {
		return goingAgentFailed(agentwire.CallCall, err)
	}
	if tag.RowsAffected() == 0 {
		if _, stale := a.callRun(ctx, request); stale != nil {
			return stale
		}
	}
	if refusal := a.refreshPlanState(ctx, request.Run, request.Row.Plan); refusal != nil {
		return refusal
	}
	return goingAgentRefusal(record.Code, "%s", record.Message)
}

// goingRecordJSON is record as going_plan_rows.result holds it.
func goingRecordJSON(record goingRowRecord) string {
	encoded, _ := json.Marshal(record)
	return string(encoded)
}

// refreshPlanState sets plan's state from its rows (goingPlanStateSQL).
func (a *AgentListener) refreshPlanState(ctx context.Context, run string, plan int) *agentwire.Error {
	if _, err := a.s.deps.Store.Pool.Exec(ctx, goingPlanStateSQL, run, plan); err != nil {
		return goingAgentFailed(agentwire.CallCall, err)
	}
	return nil
}

// recordRow keeps what a row's call came to on its row, under the call's key: done where its
// route answered below 300, and failed where it answered a refusal, or where the tool's schema or
// translation refused it before anything was sent, with what became of the message of a row that
// sends one (goingRowDelivery). A call that failed in the server, or whose lease is not the run's,
// keeps nothing: its next try runs the row on under the same key. A call that panicked is kept
// failed (recordRowPanicked). The record keeps how the row's upload was sent (goingRowUpload),
// which a second try of the key sends again, and the key of an earlier try whose landings a try of
// this key takes over (goingRowRecord.Resumes). A row done carries what its write left to the later
// rows of its item (carryRowWrite).
func (a *AgentListener) recordRow(ctx context.Context, request agentwire.CallRequest, answer agentwire.CallAnswer, refusal *agentwire.Error) {
	record := goingRowRecord{Step: request.Step, Call: request.Call}
	state := "done"
	switch {
	case refusal != nil && refusal.Code != agentwire.CodeCallRefused:
		return
	case refusal != nil:
		state, record.Code, record.Message = "failed", refusal.Code, refusal.Message
	case answer.Status >= 300:
		state, record.Status = "failed", answer.Status
		var refused errorBody
		if json.Unmarshal(answer.Body, &refused) == nil {
			record.Code, record.Message = refused.Code, refused.Error
		}
	default:
		record.Status = answer.Status
	}
	record.Delivery = a.s.goingRowDelivery(ctx, request, answer, state == "failed")
	if _, err := a.s.deps.Store.Pool.Exec(ctx, `
		update going_plan_rows set state = $4,
		  result = $5::jsonb || jsonb_strip_nulls(jsonb_build_object('upload', result->'upload', 'resumes', result->'resumes'))
		where run_id = $1::uuid and plan = $2 and row = $3 and state in ('running', 'done', 'failed')
		  and (result->>'step')::int = $6 and (result->>'call')::int = $7
	`, request.Run, request.Row.Plan, request.Row.Row, state, goingRecordJSON(record), request.Step, request.Call); err != nil {
		slog.Error("dispatch: record what a row of a command-bar plan came to", "run", request.Run, "plan", request.Row.Plan,
			"row", request.Row.Row, "error", err)
		return
	}
	if state == "done" {
		a.carryRowWrite(ctx, request, answer)
	}
	_ = a.refreshPlanState(ctx, request.Run, request.Row.Plan)
}

// codeGoingRouteFailed is the code a row is kept failed with, status 500, where its call panicked
// in the server - its route, or the server's own code around it - and goingRouteFailedText the
// words its person reads where nothing of the call is known to have landed: a next try of its key
// then reads the row's item again before it writes, as every try that finds nothing of its key
// landed does (rowGate).
const (
	codeGoingRouteFailed = "GOING_ROUTE_FAILED"
	goingRouteFailedText = "this change failed in the server, and nothing of it is known to have been written"
)

// recordRowPanicked keeps on request's row that its call panicked: failed, as a refusal its route
// answered is kept (recordRow), with status 500, codeGoingRouteFailed and the words of what of the
// call landed before the panic (goingPanickedText), read off the landings of its key; specs are the
// requests the call sends, nil where it panicked before it had them. The panic itself is answered
// and logged with its stack by runCall.
func (a *AgentListener) recordRowPanicked(ctx context.Context, request agentwire.CallRequest, specs []goingSend) {
	text := goingRouteFailedText
	landed, err := a.s.goingCallLandings(ctx, goingCallKey{run: request.Run, step: request.Step, call: request.Call})
	if err != nil {
		slog.Error("dispatch: read what a command-bar call that panicked landed", "run", request.Run, "step", request.Step,
			"call", request.Call, "error", err)
	} else {
		text = goingPanickedText(specs, landed)
	}
	body, _ := json.Marshal(errorBody{Code: codeGoingRouteFailed, Error: text})
	a.recordRow(ctx, request, agentwire.CallAnswer{Status: http.StatusInternalServerError, Body: body, ContentType: "application/json"}, nil)
}

// goingPanickedText is what the row of a call that panicked tells its person, from the requests of
// its key that landed (landed) among those it sends (specs): that nothing of it is known to have
// been written; or what was written (goingLandedWords) and that the rest was not; or that all of
// it was.
func goingPanickedText(specs []goingSend, landed []*goingLandedRequest) string {
	written := goingLandedWords(specs, landed)
	switch {
	case len(written) == 0:
		return goingRouteFailedText
	case len(written) == len(specs):
		return "this change failed in the server after all of it was written"
	case len(written) == 1:
		return fmt.Sprintf("this change failed in the server partway: %s was written and the rest was not", written[0])
	}
	return fmt.Sprintf("this change failed in the server partway: %s were written and the rest was not", strings.Join(written, " and "))
}

// goingLandedWords is each request of a call's key that landed (landed), among those the call
// sends (specs), in the words its person reads: its own words (goingSend.what), the call's own
// request as the change itself, and an earlier request with no words of its own as an earlier
// part of the change.
func goingLandedWords(specs []goingSend, landed []*goingLandedRequest) []string {
	var written []string
	for index, request := range landed {
		if request == nil {
			continue
		}
		what := "an earlier part of this change"
		switch {
		case index == len(specs)-1:
			what = "the change itself"
		case index < len(specs) && specs[index].what != "":
			what = specs[index].what
		}
		written = append(written, what)
	}
	return written
}

// goingPartlyLanded reports whether some of the requests a call sends (specs) landed under its key
// (landed), and its own request, the last, did not: the call has the rest to send.
func goingPartlyLanded(specs []goingSend, landed []*goingLandedRequest) bool {
	own := len(specs) - 1
	return goingLastLanded(landed) != nil && (own >= len(landed) || landed[own] == nil)
}

// adoptLandings takes over, for the call of request under key, what the newest earlier try of its
// row landed before the row waited for its person again (goingRowRecord.Resumes), where this key
// has landed nothing yet: each request of the earlier key that this call sends before its own
// (first), by its digest, in order from the first, is moved to this key. So it is answered from its
// landing and not sent twice - a close's reason is posted once - and the run's record holds the
// change under one key, as a second try and an undo read it. The first request this call sends
// otherwise - another reason, say, after the person edited it - ends what is taken over, and it and
// the rest are sent, while what was not taken over stays under the earlier key, the row's still.
// routeCall calls it once it has the call's requests, and a translation that reads what
// of its first requests landed before (goingCallInput.resumes) calls it first, so a refusal it
// answers then - a reasoned close of an issue another person closed - says what landed. It answers
// this key's landings with what it took over.
func (a *AgentListener) adoptLandings(ctx context.Context, request agentwire.CallRequest, key goingCallKey, row goingPlanRowStored,
	first []goingSend, landed []*goingLandedRequest,
) ([]*goingLandedRequest, *agentwire.Error) {
	if row.result == nil || len(row.result.Resumes) == 0 || goingLastLanded(landed) != nil {
		return landed, nil
	}
	newest := row.result.Resumes[len(row.result.Resumes)-1]
	earlier := goingCallKey{run: key.run, step: newest.Step, call: newest.Call}
	if earlier == key {
		return landed, nil
	}
	before, err := a.s.goingCallLandings(ctx, earlier)
	if err != nil {
		return nil, goingAgentFailed(agentwire.CallCall, err)
	}
	taken := 0
	for taken < len(first) && taken < len(before) && before[taken] != nil && before[taken].sends(first[taken]) {
		taken++
	}
	if taken == 0 {
		return landed, nil
	}
	tag, err := a.s.deps.Store.Pool.Exec(ctx, `
		update going_landings set step = $4, call = $5
		where run_id = $1::uuid and phase = 'do' and step = $2 and call = $3 and request < $6
		  and exists (select 1 from going_runs where id = $1::uuid and lease_token = $7::uuid and state = 'working')
	`, key.run, earlier.step, earlier.call, key.step, key.call, taken, request.LeaseToken)
	if err != nil {
		return nil, goingAgentFailed(agentwire.CallCall, fmt.Errorf("take over what an earlier try of the row landed: %w", err))
	}
	if tag.RowsAffected() == 0 {
		if _, stale := a.callRun(ctx, request); stale != nil {
			return nil, stale
		}
	}
	adopted, err := a.s.goingCallLandings(ctx, key)
	if err != nil {
		return nil, goingAgentFailed(agentwire.CallCall, err)
	}
	return adopted, nil
}

// rowUpload is how the upload of request's row is sent (goingRowUpload): as a try of its call's
// key decided it before, or, on that key's first try to reach it, as the upload's read before now
// finds its name - over the document or file of that name its owner holds, at the version that
// read found, naming the token that read answered where it answered one (goingObservation.token),
// or as a create alone - kept on the row under the call's key before anything of it is sent. So a
// second try of the key whose upload did not land sends the request the first decided, and one
// whose upload landed answers that landing before the call is prepared again (routeCall), as a
// second try of every call does. What the read finds is held to the read the row's approval was
// made over (uploadChanged): a document made, written - a new version, or text another writer
// changed that no version holds yet - or removed under the name after that read makes the row
// wait again, so an upload is sent over a document only where its approval showed that document
// with that text, and as a create alone otherwise. What another writer does after this read is
// refused by the upload's route under the owner's lock, and nothing is written: a document or
// file made under the name 409 DOCUMENT_EXISTS, a version written of the artifact read 409
// DOCUMENT_CHANGED - a file, and a document outside the Proof schema, have no token, so this is
// their only guard - and a document's text changed with no version 409 PRECONDITION_FAILED,
// since the token named is this read's.
func (a *AgentListener) rowUpload(ctx context.Context, request agentwire.CallRequest, row goingPlanRowStored, prepared goingPrepared,
	read goingReader,
) (goingRowUpload, *agentwire.Error) {
	var decided *goingRowUpload
	if err := a.s.deps.Store.Pool.QueryRow(ctx, `
		select result->'upload' from going_plan_rows
		where run_id = $1::uuid and plan = $2 and row = $3 and (result->>'step')::int = $4 and (result->>'call')::int = $5
	`, request.Run, request.Row.Plan, request.Row.Row, request.Step, request.Call).Scan(&decided); err != nil {
		return goingRowUpload{}, goingAgentFailed(agentwire.CallCall, fmt.Errorf("read how the row's upload is sent: %w", err))
	}
	if decided != nil {
		return *decided, nil
	}
	observer := goingObserver{read: read, s: a.s}
	before, err := observer.observe(ctx, prepared, nil, 0, goingObservation{})
	if err != nil {
		return goingRowUpload{}, goingCallRefusal(request.Tool, err)
	}
	if refusal := a.uploadChanged(ctx, request, row, before); refusal != nil {
		return goingRowUpload{}, refusal
	}
	upload := goingRowUpload{Over: before.held != nil}
	if upload.Over {
		upload.Base, upload.Artifact, upload.Version = before.token, before.held.ID, latestArtifactVersion(*before.held)
	}
	encoded, err := json.Marshal(upload)
	if err != nil {
		return goingRowUpload{}, goingAgentFailed(agentwire.CallCall, err)
	}
	tag, err := a.s.deps.Store.Pool.Exec(ctx, `
		update going_plan_rows set result = jsonb_set(result, '{upload}', $6::jsonb)
		where run_id = $1::uuid and plan = $2 and row = $3 and state = 'running'
		  and (result->>'step')::int = $4 and (result->>'call')::int = $5
		  and exists (select 1 from going_runs where id = $1::uuid and lease_token = $7::uuid and state = 'working')
	`, request.Run, request.Row.Plan, request.Row.Row, request.Step, request.Call, string(encoded), request.LeaseToken)
	if err != nil {
		return goingRowUpload{}, goingAgentFailed(agentwire.CallCall, err)
	}
	if tag.RowsAffected() == 0 {
		if _, stale := a.callRun(ctx, request); stale != nil {
			return goingRowUpload{}, stale
		}
		return goingRowUpload{}, goingAgentRefusal(agentwire.CodeCallUnknown, "row %d of plan %d was taken by another call meanwhile", request.Row.Row, request.Row.Plan)
	}
	return upload, nil
}

// uploadChanged holds what the read before an upload finds under its name (before) to the read its
// row's approval was made over (Seen), as rowItemChanged holds a row's item: a document there now
// that the approval did not show - made after it, written to another version, or holding other
// text (goingUploadBefore.Content) - or one it showed and that is gone makes the row wait again,
// showing what is there now.
func (a *AgentListener) uploadChanged(ctx context.Context, request agentwire.CallRequest, row goingPlanRowStored,
	before goingObservation,
) *agentwire.Error {
	var shown goingRowShown
	if row.approval != nil {
		shown = row.approval.Seen
	}
	var now json.RawMessage
	if before.value != nil {
		encoded, err := json.Marshal(before.value)
		if err != nil {
			return goingAgentFailed(agentwire.CallCall, err)
		}
		now = encoded
	}
	if goingJSONEqual(shown.Now, now) {
		return nil
	}
	shown.Item, shown.Now, shown.Token, shown.Changed = before.target, now, before.token, true
	return a.settleRow(ctx, request, row, "waiting", goingRowChanged(request, nil, nil), &shown)
}

// carryRowWrite moves each later approved row of request's plan on the item, or the issue, that the
// done row of request wrote, to what that write left of it as the write's own answer says (the
// route answers the item as its transaction left it, which goingObserver reads after a write): the
// change an earlier row of the plan makes is the plan's own and makes no later row wait again,
// while a change another writer made after it still does. Nothing is read again: a later row whose
// values or claim the answer does not say keeps the read it was approved over, so its gate waits
// it again where the earlier write changed them.
func (a *AgentListener) carryRowWrite(ctx context.Context, request agentwire.CallRequest, answer agentwire.CallAnswer) {
	plan, err := readGoingPlan(ctx, a.s.deps.Store.Pool, request.Run, request.Row.Plan)
	if err != nil || plan == nil {
		slog.Error("dispatch: read a command-bar plan after a row ran", "run", request.Run, "plan", request.Row.Plan, "error", err)
		return
	}
	at := slices.IndexFunc(plan.rows, func(row goingPlanRowStored) bool { return row.row == request.Row.Row })
	if at < 0 || plan.rows[at].approval == nil {
		return
	}
	done := plan.rows[at]
	after, read := a.s.goingRowAfter(ctx, done, answer)
	if !read {
		return
	}
	wrote := done.approval.Seen
	for _, row := range plan.rows[at+1:] {
		if row.state != "approved" || row.approval == nil {
			continue
		}
		seen := row.approval.Seen
		if (wrote.Item == "" || seen.Item != wrote.Item) && (wrote.Issue == "" || seen.Issue != wrote.Issue) {
			continue
		}
		carried, moved := goingCarryWrite(seen, after)
		if !moved {
			continue
		}
		approval := *row.approval
		approval.Seen = carried
		encoded, err := json.Marshal(approval)
		if err != nil {
			continue
		}
		if _, err := a.s.deps.Store.Pool.Exec(ctx, `
			update going_plan_rows set approval = $4::jsonb where run_id = $1::uuid and plan = $2 and row = $3 and state = 'approved'
		`, request.Run, plan.plan, row.row, string(encoded)); err != nil {
			slog.Error("dispatch: carry a row's write to the later rows of its command-bar plan", "run", request.Run, "plan", plan.plan,
				"row", row.row, "error", err)
		}
	}
}

// goingNoReads is the Doer of the read after a row's write that carryRowWrite makes: it refuses
// every request, so what a write left is taken from its route's answer alone.
type goingNoReads struct{}

var errGoingNoReads = errors.New("what a row's write left is read from its answer alone")

func (goingNoReads) Do(*http.Request) (*http.Response, error) { return nil, errGoingNoReads }

// goingRowAfter is what the done row's write left of its item, as its route's answer says
// (goingObserver's read after a write, made without a request), and whether the answer said it:
// an answer that was cut, that does not read, or whose tool reads what it left with a request
// says nothing.
func (s *server) goingRowAfter(ctx context.Context, done goingPlanRowStored, answer agentwire.CallAnswer) (goingObservation, bool) {
	if answer.Cut {
		return goingObservation{}, false
	}
	arguments, err := goingRowArguments(done.arguments)
	if err != nil {
		return goingObservation{}, false
	}
	seen := done.approval.Seen
	before := goingObservation{target: seen.Item, fields: seen.Fields, issue: seen.Issue}
	if len(seen.Now) > 0 {
		if err := json.Unmarshal(seen.Now, &before.value); err != nil {
			return goingObservation{}, false
		}
	}
	observer := goingObserver{read: goingReader{doer: goingNoReads{}}, s: s,
		landed: func(context.Context) (bool, error) { return false, errGoingNoReads }}
	after, err := observer.observe(ctx, goingPrepared{action: done.tool, arguments: arguments}, answer.Body, answer.Status, before)
	if err != nil || after.beforeUnread {
		return goingObservation{}, false
	}
	return after, true
}

// goingCarryWrite is seen, a later row's read of the item an earlier row wrote, with what that
// write left (after) in place of the values it read: each of its fields the write's answer says,
// and the claim a claim's or a release's answer says. moved reports whether anything was put in.
func goingCarryWrite(seen goingRowShown, after goingObservation) (goingRowShown, bool) {
	moved := false
	if left, fields := after.value.(map[string]any); fields {
		var now map[string]any
		if json.Unmarshal(seen.Now, &now) == nil && now != nil {
			for _, field := range seen.Fields {
				if value, said := left[field]; said {
					now[field], moved = value, true
				}
			}
			if moved {
				if encoded, err := json.Marshal(now); err == nil {
					seen.Now = encoded
				} else {
					moved = false
				}
			}
		}
	}
	if slices.Equal(after.fields, goingFields(goingWriteFields["dispatch_claim"])) && !after.unchanged {
		seen.Claim, moved = nil, true
		if holder, held := after.value.(*goingHolder); held && holder != nil {
			copied := *holder
			seen.Claim = &copied
		}
		if slices.Equal(seen.Fields, after.fields) {
			seen.Now = goingJSONOrNull(seen.Claim)
		}
	}
	return seen, moved
}

// reviewCall is a review call (agentwire.KindReview) of a run in Auto-review: Auto-review's
// model call named the rows of the run's latest plan that the person's words ask for (Asked), and
// the server approves each of them that Auto-review may let through (autoReviewLets) whose
// needed rows it approves too, once for the plan, in one transaction that holds the run's lease.
// It answers the rows to run, the same a claim of the run would (goingPendingRows), and how many
// rows wait for the person. A second review of the plan approves nothing more.
func (a *AgentListener) reviewCall(ctx context.Context, request agentwire.CallRequest, run goingCallRun) (agentwire.CallAnswer, *agentwire.Error) {
	if run.mode != goingModeAutoReview {
		return agentwire.CallAnswer{}, goingAgentRefusal(agentwire.CodeCallRefused, "this run's mode is %s, which no review lets a change through in", run.mode)
	}
	tx, err := a.s.deps.Store.Pool.Begin(ctx)
	if err != nil {
		return agentwire.CallAnswer{}, goingAgentFailed(agentwire.CallCall, err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if held, err := goingLeaseHeld(ctx, tx, run.id, request.LeaseToken); err != nil {
		return agentwire.CallAnswer{}, goingAgentFailed(agentwire.CallCall, err)
	} else if !held {
		return agentwire.CallAnswer{}, goingStaleLease(run.id)
	}
	var plan int
	var reviewed bool
	err = tx.QueryRow(ctx, `
		select plan, approved_by is not null from going_plans where run_id = $1::uuid order by plan desc limit 1 for update
	`, run.id).Scan(&plan, &reviewed)
	if errors.Is(err, pgx.ErrNoRows) {
		return agentwire.CallAnswer{}, goingAgentRefusal(agentwire.CodeCallRefused, "this run has proposed no plan to review")
	}
	if err != nil {
		return agentwire.CallAnswer{}, goingAgentFailed(agentwire.CallCall, err)
	}
	if !reviewed {
		stored, err := readGoingPlan(ctx, tx, run.id, plan)
		if err != nil {
			return agentwire.CallAnswer{}, goingAgentFailed(agentwire.CallCall, err)
		}
		lets := map[int]bool{}
		for _, row := range stored.rows {
			if row.state != "waiting" || !slices.Contains(request.Asked, row.row) || !row.autoReviewLets() {
				continue
			}
			if slices.ContainsFunc(row.needs, func(need int) bool { return !lets[need] }) {
				continue
			}
			lets[row.row] = true
			approval, err := json.Marshal(goingRowApproval{By: goingApprovedByAutoReview, Seen: row.shown})
			if err != nil {
				return agentwire.CallAnswer{}, goingAgentFailed(agentwire.CallCall, err)
			}
			if _, err := tx.Exec(ctx, `
				update going_plan_rows set state = 'approved', approval = $4::jsonb where run_id = $1::uuid and plan = $2 and row = $3
			`, run.id, plan, row.row, string(approval)); err != nil {
				return agentwire.CallAnswer{}, goingAgentFailed(agentwire.CallCall, err)
			}
		}
		if err := recordGoingPlanApproval(ctx, tx, run.id, plan, map[string]string{"by": "auto_review"}); err != nil {
			return agentwire.CallAnswer{}, goingAgentFailed(agentwire.CallCall, err)
		}
		if _, err := tx.Exec(ctx, goingPlanStateSQL, run.id, plan); err != nil {
			return agentwire.CallAnswer{}, goingAgentFailed(agentwire.CallCall, err)
		}
	}
	review := agentwire.ReviewAnswer{}
	if review.Run, err = goingPendingRows(ctx, tx, run.id); err != nil {
		return agentwire.CallAnswer{}, goingAgentFailed(agentwire.CallCall, err)
	}
	if err := tx.QueryRow(ctx, `
		select count(*) from going_plan_rows where run_id = $1::uuid and plan = $2 and state = 'waiting'
	`, run.id, plan).Scan(&review.Waiting); err != nil {
		return agentwire.CallAnswer{}, goingAgentFailed(agentwire.CallCall, err)
	}
	var state string
	if err := tx.QueryRow(ctx, `select state from going_plans where run_id = $1::uuid and plan = $2`, run.id, plan).Scan(&state); err != nil {
		return agentwire.CallAnswer{}, goingAgentFailed(agentwire.CallCall, err)
	}
	if !reviewed {
		if err := appendGoingPlanFrame(ctx, tx, run.id, plan, state); err != nil {
			return agentwire.CallAnswer{}, goingAgentFailed(agentwire.CallCall, err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return agentwire.CallAnswer{}, goingAgentFailed(agentwire.CallCall, err)
	}
	if review.Run == nil {
		review.Run = []agentwire.PlanRun{}
	}
	body, err := json.Marshal(review)
	if err != nil {
		return agentwire.CallAnswer{}, goingAgentFailed(agentwire.CallCall, err)
	}
	return agentwire.CallAnswer{Status: 200, Body: body, ContentType: "application/json"}, nil
}

// goingRowsWaitSQL reports whether run $1's latest plan, unless rejected, holds a row that waits
// for the person: a run whose turn ends with one waits for them, whatever its agent ended it as.
const goingRowsWaitSQL = `
	select exists (
	  select 1 from going_plan_rows r join going_plans p on p.run_id = r.run_id and p.plan = r.plan
	  where r.run_id = $1::uuid and p.state <> 'rejected' and r.state = 'waiting'
	    and r.plan = (select max(plan) from going_plans where run_id = $1::uuid)
	)
`
