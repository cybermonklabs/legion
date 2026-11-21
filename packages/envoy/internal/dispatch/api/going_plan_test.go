package api

// going_plan_test.go holds a run's plan as its person meets it: the agent's real loop, on a fake
// chat model, proposes a plan; the person reads and answers it through the plan routes; and the
// agent runs the approved rows through the server (going_plan.go, going_plan_run.go).

import (
	"context"
	"encoding/json"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sjawhar/envoy/going/agent/agenttest"
	"github.com/sjawhar/envoy/going/agentwire"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// goingPlanRig is the agent rig with the agent's loop on a fake chat model, whose people are named
// by the header identity, which a plan's approval records as the run's grant.
type goingPlanRig struct {
	goingAgentRig
	loop *goingLoop
}

func newGoingPlanRig(t *testing.T) goingPlanRig {
	t.Helper()
	return newGoingPlanRigOver(t, newGoingWritesRig(t))
}

// newGoingPlanRigOver is the plan rig over writes, a writes rig the test chose.
func newGoingPlanRigOver(t *testing.T, writes goingWritesRig) goingPlanRig {
	t.Helper()
	rig := newGoingAgentRigOver(t, writes, nil)
	rig.served.deps.Identity = headerIdentity(rig.database)
	return goingPlanRig{goingAgentRig: rig, loop: newGoingLoop(t, rig)}
}

// goingRowNumbered matches a row named by its number, which no page shows.
var goingRowNumbered = regexp.MustCompile(`\brows? \d`)

// goingChange is one change of a proposed plan: tool with arguments, needing the changes needs. A
// change that needs none leaves needs out, as propose_plan's schema takes no null for it.
func goingChange(tool string, arguments map[string]any, needs ...int) map[string]any {
	change := map[string]any{"tool": tool, "arguments": arguments}
	if len(needs) > 0 {
		change["needs"] = needs
	}
	return change
}

// proposing is the reply that proposes changes as a plan.
func proposing(changes ...map[string]any) agenttest.Reply {
	return toolCall("propose_plan", map[string]any{"summary": "The changes the words ask for.", "changes": changes})
}

// plan is run's latest plan as alice reads it.
func (rig goingPlanRig) plan(t *testing.T, run string) goingPlanAnswer {
	t.Helper()
	response := dispatchRequest(t, rig.mux, http.MethodGet, "/api/v1/going/runs/"+run+"/plan", nil, "alice")
	if response.Code != http.StatusOK {
		t.Fatalf("GET the plan of %s answered %d %s", run, response.Code, response.Body.String())
	}
	return decodeBody[goingPlanAnswer](t, response)
}

// answer sends alice's answer to run's plan: approve, revise or reject, with body.
func (rig goingPlanRig) answer(t *testing.T, run, verb string, body any) goingPlanAnswer {
	t.Helper()
	response := dispatchRequest(t, rig.mux, http.MethodPost, "/api/v1/going/runs/"+run+"/plan/"+verb, body, "alice")
	if response.Code != http.StatusOK {
		t.Fatalf("%s the plan of %s answered %d %s", verb, run, response.Code, response.Body.String())
	}
	return decodeBody[goingPlanAnswer](t, response)
}

// waiting waits for run to end its turn waiting for alice, and answers its plan.
func (rig goingPlanRig) waiting(t *testing.T, run string) goingPlanAnswer {
	t.Helper()
	if record := rig.ended(t, run, time.Minute); record.state != "waiting" {
		t.Fatalf("the run ended %s with %q, want waiting for its person; its calls were told %q", record.state, record.reason, rig.toolResults(t, run))
	}
	return rig.plan(t, run)
}

// title is the title of issue now.
func (rig goingPlanRig) title(t *testing.T, issue string) string {
	t.Helper()
	return rig.issueRead(t, issue).Title
}

// rowStates is the state of each row of plan, in order.
func rowStates(plan goingPlanAnswer) []string {
	states := make([]string, 0, len(plan.Rows))
	for _, row := range plan.Rows {
		states = append(states, row.State)
	}
	return states
}

// retitle is a change of issue's title to title.
func retitle(issue, title string) map[string]any {
	return map[string]any{"issue": issue, "title": title}
}

// TestTheServerRefusesAChangeOutsideTheApprovedPlan (check c5): the model's tools hold no tool
// that changes something; after the person approves a priority change on issue A, the model calls
// call_tool with a write on issue B and with the planned write on A with another value, and the
// server refuses both, each saved as a refused step of the run; A holds the approved priority and
// B none.
func TestTheServerRefusesAChangeOutsideTheApprovedPlan(t *testing.T) {
	rig := newGoingPlanRig(t)
	a, b := rig.issue(t, "Issue A"), rig.issue(t, "Issue B")
	const words = "set the priority of issue A to 1"
	write := func(arguments map[string]any) agenttest.Call {
		return agenttest.Call{Name: "call_tool", Arguments: map[string]any{"tool": "dispatch_issue_update", "arguments": arguments}}
	}
	rig.loop.script.set(words, func(n int) agenttest.Reply {
		switch n {
		case 1:
			return proposing(goingChange("dispatch_issue_update", map[string]any{"issue": a, "priority": 1}))
		case 2:
			return agenttest.Reply{Calls: []agenttest.Call{
				write(map[string]any{"issue": b, "priority": 0}), write(map[string]any{"issue": a, "priority": 3}),
			}}
		}
		return answered("Done.")
	})
	run := rig.begin(t, goingLoopRun{login: "alice", words: words})
	rig.waiting(t, run)
	for _, tool := range goingToolNames(rig.loop.fake.Asking(words)[0]) {
		if _, writes := goingWriteTools[tool]; writes {
			t.Errorf("the model was given %s, a tool that changes something", tool)
		}
	}
	rig.answer(t, run, "approve", map[string]any{"plan": 1})
	if record := rig.ended(t, run, time.Minute); record.state != "done" {
		t.Fatalf("the run ended %s with %q, want done", record.state, record.reason)
	}
	if priority := rig.issueRead(t, a).Priority; priority == nil || *priority != 1 {
		shown := "none"
		if priority != nil {
			shown = strconv.Itoa(*priority)
		}
		t.Errorf("A's priority is %s, want the approved 1", shown)
	}
	if priority := rig.issueRead(t, b).Priority; priority != nil {
		t.Errorf("B's priority is %d, want none: no plan names B", *priority)
	}
	if refused := rig.count(t, `
		select count(*) from going_run_steps where run_id = $1::uuid and kind = 'tool' and (body->>'error')::boolean
		  and body->>'result' like '%call_tool does not run%'`, run); refused != 2 {
		t.Errorf("the run saved %d refused writes of call_tool, want both: %q", refused, rig.toolResults(t, run))
	}
}

// goingToolNames is the name of each tool request gives the model.
func goingToolNames(request *agenttest.Request) []string {
	var names []string
	tools, _ := request.Field("tools").([]any)
	for _, tool := range tools {
		if function, ok := tool.(map[string]any)["function"].(map[string]any); ok {
			names = append(names, function["name"].(string))
		}
	}
	return names
}

// TestWhatWaitsInEachMode (check c6): a new person's mode is Ask, and every row of a plan waits.
// In Auto-review, whose review lets every change through, a field change and a create with only a
// title run, and a field change the words did not ask for waits; and these wait: a review of a
// document, an answer to an ask, a kept decision, a check result, a change of a setting of
// Dispatch, a priority change on an issue routed to a session, the close of an issue whose claim
// a session holds, a message to a session that holds no claim on the issue, and a broadcast.
// Before the write of an approved plan a session takes a claim of its item, and that row waits
// again.
func TestWhatWaitsInEachMode(t *testing.T) {
	rig := newGoingPlanRig(t)
	everyRow := func(rows int) []any {
		asked := make([]any, rows)
		for index := range asked {
			asked[index] = index
		}
		return asked
	}

	t.Run("a new person's mode is Ask, and every row waits", func(t *testing.T) {
		settings := decodeBody[goingSettings](t, dispatchRequest(t, rig.mux, http.MethodGet, "/api/v1/me/going/settings", nil, "alice"))
		if settings.Mode != goingModeAsk {
			t.Fatalf("a new person's mode is %q, want ask", settings.Mode)
		}
		issue := rig.issue(t, "Asked of")
		const words = "rename Asked of"
		rig.loop.script.set(words, func(int) agenttest.Reply {
			return proposing(goingChange("dispatch_issue_update", retitle(issue, "Renamed")))
		})
		plan := rig.waiting(t, rig.begin(t, goingLoopRun{login: "alice", words: words, mode: settings.Mode}))
		if states := rowStates(plan); !slices.Equal(states, []string{"waiting"}) || rig.title(t, issue) != "Asked of" {
			t.Fatalf("in Ask the plan's rows are %v and the title %q, want the row waiting and nothing changed", states, rig.title(t, issue))
		}
	})

	t.Run("in Auto-review a field change and a create with only a title run", func(t *testing.T) {
		issue := rig.issue(t, "Reviewed")
		const words = "rename Reviewed and file Made by review"
		rig.loop.script.set(words, func(n int) agenttest.Reply {
			switch n {
			case 1:
				return proposing(goingChange("dispatch_issue_update", retitle(issue, "Renamed by review")),
					goingChange("dispatch_issue", map[string]any{"project": "DEMO", "title": "Made by review", "force": true}))
			case 2:
				return toolCall(toolReviewPlanName, map[string]any{"asked": everyRow(2)})
			}
			return answered("Both are done.")
		})
		run := rig.begin(t, goingLoopRun{login: "alice", words: words, mode: goingModeAutoReview})
		if record := rig.ended(t, run, time.Minute); record.state != "done" {
			t.Fatalf("the run ended %s with %q, want done with both rows run", record.state, record.reason)
		}
		if states := rowStates(rig.plan(t, run)); !slices.Equal(states, []string{"done", "done"}) || rig.title(t, issue) != "Renamed by review" {
			t.Fatalf("the plan's rows are %v and the title %q, want both done", states, rig.title(t, issue))
		}
	})

	t.Run("in Auto-review a field change the words did not ask for waits", func(t *testing.T) {
		asked, unasked := rig.issue(t, "Asked for"), rig.issue(t, "Not asked for")
		const words = "rename Asked for"
		rig.loop.script.set(words, func(n int) agenttest.Reply {
			switch n {
			case 1:
				return proposing(goingChange("dispatch_issue_update", retitle(asked, "Renamed as asked")),
					goingChange("dispatch_issue_update", retitle(unasked, "Renamed unasked")))
			case 2:
				return toolCall(toolReviewPlanName, map[string]any{"asked": []int{0}})
			}
			return answered("One is done, and one waits for you.")
		})
		plan := rig.waiting(t, rig.begin(t, goingLoopRun{login: "alice", words: words, mode: goingModeAutoReview}))
		if states := rowStates(plan); !slices.Equal(states, []string{"done", "waiting"}) || rig.title(t, unasked) != "Not asked for" {
			t.Fatalf("the plan's rows are %v and the unasked title %q, want the asked row done and the other waiting", states, rig.title(t, unasked))
		}
	})

	t.Run("the decisions of the person, a setting and a change delivered to a session wait in Auto-review", func(t *testing.T) {
		fixtures := newGoingRowFixtures(t, rig.goingWritesRig)
		cases := goingRowCases(fixtures)
		routed, claimed := rig.issue(t, "Routed to a session"), rig.issue(t, "Claimed by a session")
		rig.exec(t, `update issues set route = 'session:s-route' where key = $1`, routed)
		rig.exec(t, `update issues set claimed_by = '{"kind": "session", "id": "s-claim"}', claimed_at = now() where key = $1`, claimed)
		rows := []struct {
			change map[string]any
			waits  string
		}{
			{goingChange("dispatch_plan_approve", cases["dispatch_plan_approve POST /api/v1/artifacts/{id}/plan-reviews"].arguments), string(goingClassReview)},
			{goingChange("dispatch_resolve_ask", cases["dispatch_resolve_ask POST /api/v1/asks/{id}/resolve"].arguments), string(goingClassAskAnswer)},
			{goingChange("dispatch_keep_decision", map[string]any{
				"document": fixtures.notesID, "block": "b1", "text_sha256": strings.Repeat("0", 64),
			}), string(goingClassDecision)},
			{goingChange("dispatch_result", cases["dispatch_result POST /api/v1/results/{id}/retract"].arguments), string(goingClassResult)},
			{goingChange("dispatch_architecture_sync", cases["dispatch_architecture_sync POST /api/v1/projects/{key}/architecture-sources/sync"].arguments), string(goingClassSetting)},
			{goingChange("dispatch_issue_update", map[string]any{"issue": routed, "priority": 2}), goingWaitsDelivery},
			{goingChange("dispatch_issue_update", map[string]any{"issue": claimed, "status": "done", "reason": "Finished."}), goingWaitsClaimed},
			{goingChange("dispatch_message", cases["dispatch_message POST /api/v1/issues/{key}/messages"].arguments), string(goingClassMessage)},
			{goingChange("dispatch_broadcast", map[string]any{"session_ids": []string{"s-route"}, "body": "The deploy is paused.", "delivery": "aside"}),
				string(goingClassBroadcast)},
		}
		changes := make([]map[string]any, 0, len(rows))
		for _, row := range rows {
			changes = append(changes, row.change)
		}
		const words = "do all of these"
		rig.loop.script.set(words, func(n int) agenttest.Reply {
			if n == 1 {
				return proposing(changes...)
			}
			return toolCall(toolReviewPlanName, map[string]any{"asked": everyRow(len(changes))})
		})
		plan := rig.waiting(t, rig.begin(t, goingLoopRun{login: "alice", words: words, mode: goingModeAutoReview}))
		if len(plan.Rows) != len(rows) {
			t.Fatalf("the plan holds %d rows, want %d", len(plan.Rows), len(rows))
		}
		for index, row := range plan.Rows {
			if row.State != "waiting" || !slices.Contains(row.Waits, rows[index].waits) {
				t.Errorf("row %d, %s, is %s waiting for %v, want it waiting for %s", index, row.Tool, row.State, row.Waits, rows[index].waits)
			}
		}
		if requests := len(rig.loop.fake.Asking(words)); requests != 2 {
			t.Errorf("the run made %d model calls, want the plan's and the review's alone", requests)
		}
	})

	t.Run("a claim a session takes before the write makes the row wait again", func(t *testing.T) {
		issue := rig.issue(t, "Claimed meanwhile")
		const words = "rename Claimed meanwhile"
		rig.loop.script.set(words, func(n int) agenttest.Reply {
			if n == 1 {
				return proposing(goingChange("dispatch_issue_update", retitle(issue, "Renamed meanwhile")))
			}
			return answered("It waits for you again.")
		})
		run := rig.begin(t, goingLoopRun{login: "alice", words: words})
		rig.waiting(t, run)
		rig.loop.stop()
		rig.answer(t, run, "approve", map[string]any{"plan": 1})
		rig.exec(t, `update issues set claimed_by = '{"kind": "session", "id": "s-late"}', claimed_at = now() where key = $1`, issue)
		rig.loop.start(rig.goingAgentRig)
		plan := rig.waiting(t, run)
		if row := plan.Rows[0]; row.State != "waiting" || !row.Changed || rig.title(t, issue) != "Claimed meanwhile" {
			t.Fatalf("the row is %s (changed %v) and the title %q, want it waiting again with nothing written", row.State, row.Changed, rig.title(t, issue))
		}
	})
}

// toolReviewPlanName is the tool the review's model call names its rows with (going/agent).
const toolReviewPlanName = "review_plan"

// TestThePlanRunsInOrder: a plan whose row 2 sets a title that row 3 sets again ends with row 3's
// title, and its rows land in their order.
func TestThePlanRunsInOrder(t *testing.T) {
	rig := newGoingPlanRig(t)
	issue := rig.issue(t, "In order")
	const words = "comment, then rename In order twice"
	rig.loop.script.set(words, func(n int) agenttest.Reply {
		if n == 1 {
			return proposing(goingChange("dispatch_comment", map[string]any{"issue": issue, "body": "First of three."}),
				goingChange("dispatch_issue_update", retitle(issue, "Second")), goingChange("dispatch_issue_update", retitle(issue, "Third"), 1))
		}
		return answered("Done in order.")
	})
	run := rig.begin(t, goingLoopRun{login: "alice", words: words})
	rig.waiting(t, run)
	rig.answer(t, run, "approve", map[string]any{"plan": 1})
	if record := rig.ended(t, run, time.Minute); record.state != "done" {
		t.Fatalf("the run ended %s with %q, want done", record.state, record.reason)
	}
	if title := rig.title(t, issue); title != "Third" {
		t.Errorf("the issue is titled %q, want row 3's Third", title)
	}
	rows, err := rig.database.Pool.Query(context.Background(), `
		select method from going_landings where run_id = $1::uuid order by landed_at, step, request`, run)
	if err != nil {
		t.Fatal(err)
	}
	var methods []string
	for rows.Next() {
		var method string
		if err := rows.Scan(&method); err != nil {
			t.Fatal(err)
		}
		methods = append(methods, method)
	}
	if !slices.Equal(methods, []string{http.MethodPost, http.MethodPatch, http.MethodPatch}) {
		t.Errorf("the rows landed as %v, want the comment, then the two renames", methods)
	}
	if steps := rig.count(t, `
		select count(*) from going_run_steps a join going_run_steps b on a.run_id = b.run_id
		where a.run_id = $1::uuid and (a.body->'row'->>'row')::int < (b.body->'row'->>'row')::int and a.step > b.step`, run); steps != 0 {
		t.Errorf("%d pairs of rows ran out of their order", steps)
	}
}

// TestAPlanUploadsANewVersionOfADocument: a row that uploads under the name of a document its issue
// already holds at version 2 shows that document as its item, and once approved lands as the
// document's version 3.
func TestAPlanUploadsANewVersionOfADocument(t *testing.T) {
	rig := newGoingPlanRig(t)
	issue := rig.issue(t, "Holds notes")
	owner := "/api/v1/issues/" + issue
	uploadDocument(t, rig.mux, owner, "notes.md", "# Notes\n\nOne.\n")
	notes := uploadDocument(t, rig.mux, owner, "notes.md", "# Notes\n\nTwo.\n")
	const words = "replace notes.md on Holds notes"
	rig.loop.script.set(words, func(n int) agenttest.Reply {
		if n == 1 {
			return proposing(goingChange("dispatch_artifact", map[string]any{
				"issue": issue, "name": "notes.md", "content": "# Notes\n\nThe plan's.\n", "force": true,
			}))
		}
		return answered("Replaced.")
	})
	run := rig.begin(t, goingLoopRun{login: "alice", words: words})
	if plan := rig.waiting(t, run); len(plan.Rows) != 1 || plan.Rows[0].Item != notes.Artifact.ID {
		t.Fatalf("the plan shows %+v, want its one row on %s, the document the upload writes over", plan.Rows, notes.Artifact.ID)
	}
	rig.answer(t, run, "approve", map[string]any{"plan": 1})
	if record := rig.ended(t, run, time.Minute); record.state != "done" {
		t.Fatalf("the run ended %s with %q, want done", record.state, record.reason)
	}
	if versions := rig.count(t, `select max(number) from artifact_versions where artifact_id = $1::uuid`, notes.Artifact.ID); versions != 3 {
		t.Errorf("%s holds %d versions after the plan ran, want 3, the plan's upload its next version; the row's call was told %q",
			notes.Artifact.ID, versions, rig.toolResults(t, run))
	}
}

// TestAnUploadApprovedAsACreateWaitsAgainOverADocumentMadeSince: an upload whose row its person
// approved while its issue held no document of its name never writes over one another person makes
// under that name after the approval, even once the row's gate has read the name free: the row
// waits for the person again, showing that document, which keeps its one version, and their
// approval of it again lands the upload as the document's next version.
func TestAnUploadApprovedAsACreateWaitsAgainOverADocumentMadeSince(t *testing.T) {
	rig := newGoingPlanRig(t)
	issue := rig.issue(t, "Gets notes")
	owner := "/api/v1/issues/" + issue
	const words = "add notes.md to Gets notes"
	rig.loop.script.set(words, func(n int) agenttest.Reply {
		if n == 1 {
			return proposing(goingChange("dispatch_artifact", map[string]any{"issue": issue, "name": "notes.md", "content": "# Notes\n\nThe plan's.\n"}))
		}
		return answered("Added, or waiting.")
	})
	run := rig.begin(t, goingLoopRun{login: "alice", words: words})
	if plan := rig.waiting(t, run); len(plan.Rows) != 1 || !goingJSONEqual(plan.Rows[0].Now, nil) {
		t.Fatalf("the plan shows %+v, want its one row showing no document of the name", plan.Rows)
	}
	rig.loop.stop()
	rig.answer(t, run, "approve", map[string]any{"plan": 1})
	// Bob makes notes.md as the row's gate reads the claim of its issue, after the gate read the
	// name free: only the upload's own read before finds the document.
	var bobs atomic.Pointer[uploadedArtifact]
	rig.hooks.set(t, http.MethodGet, "/api/v1/issues/"+issue, func(w http.ResponseWriter, r *http.Request, inner http.Handler) {
		inner.ServeHTTP(w, r)
		if bobs.Load() == nil {
			made := decodeBody[uploadedArtifact](t, rig.fixture(t, "bob", http.MethodPost, owner+"/artifacts",
				map[string]any{"name": "notes.md", "content": "# Notes\n\nBob's.\n"}))
			bobs.Store(&made)
		}
	})
	rig.loop.start(rig.goingAgentRig)
	plan := rig.waiting(t, run)
	made := bobs.Load()
	if made == nil {
		t.Fatal("no read of the issue reached its route after the approval, so Bob made no document")
	}
	if row := plan.Rows[0]; row.State != "waiting" || !row.Changed || row.Item != made.Artifact.ID {
		t.Fatalf("the row is %s (changed %v) on %q, want it waiting again on Bob's notes.md, %s", row.State, row.Changed, row.Item, made.Artifact.ID)
	}
	const versions = `select count(*) from artifact_versions where artifact_id = $1::uuid`
	if count := rig.count(t, versions, made.Artifact.ID); count != 1 {
		t.Fatalf("Bob's notes.md holds %d versions, want its one: the upload approved as a create wrote over it", count)
	}
	rig.answer(t, run, "approve", map[string]any{"plan": 1})
	if record := rig.ended(t, run, time.Minute); record.state != "done" {
		t.Fatalf("after its approval again the run ended %s with %q, want done", record.state, record.reason)
	}
	if count := rig.count(t, versions, made.Artifact.ID); count != 2 {
		t.Errorf("after its approval again Bob's notes.md holds %d versions, want 2, the upload its next version", count)
	}
}

// liveEdit replaces find with with in document id's text as bob, as an edit in the browser does:
// through the document service, in a transaction it joins, writing no version - the document's
// settlement versions it later.
func (rig goingPlanRig) liveEdit(t *testing.T, id, find, with string) {
	t.Helper()
	ctx := context.Background()
	tx, err := rig.database.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	joined, ledger := rig.served.deps.Docs.Join(ctx, tx)
	defer ledger.Discard()
	if _, err := rig.served.deps.Docs.ApplyOps(joined, id, []model.EditOp{{Op: "replace", Find: find, With: with}},
		model.Actor{Kind: "user", ID: "bob"}, nil); err != nil {
		t.Fatalf("edit %s's text: %v", id, err)
	}
	if err := ledger.Commit(ctx); err != nil {
		t.Fatalf("commit the edit of %s's text: %v", id, err)
	}
}

// goingNowText is the text an upload row shows its document holding now, "" for none.
func goingNowText(row goingPlanRow) string {
	var now struct {
		Content *string `json:"content"`
	}
	if json.Unmarshal(row.Now, &now) != nil || now.Content == nil {
		return ""
	}
	return *now.Content
}

// TestAnUploadRowShowsTheDocumentsTextAndWaitsAgainOverAnEditSinceItsApproval: a row that uploads
// over a document shows the document's text now, which the page shows beside the text the upload
// writes. Text another person changed after the approval makes the row wait again though no
// version holds that text yet - an edit in the browser, which the document's settlement versions
// later, and which this server holds back - showing that text, and the document keeps it; the
// approval of the row again lands the upload over it.
func TestAnUploadRowShowsTheDocumentsTextAndWaitsAgainOverAnEditSinceItsApproval(t *testing.T) {
	rig := newGoingPlanRigOver(t, newGoingWritesRigServing(t, func(database *store.Store) *store.Store { return database }, time.Hour))
	issue := rig.issue(t, "Holds edited notes")
	notes := uploadDocument(t, rig.mux, "/api/v1/issues/"+issue, "notes.md", "# Notes\n\nOne.\n")
	const words = "replace notes.md on Holds edited notes"
	rig.loop.script.set(words, func(n int) agenttest.Reply {
		if n == 1 {
			return proposing(goingChange("dispatch_artifact", map[string]any{
				"issue": issue, "name": "notes.md", "content": "# Notes\n\nThe plan's.\n", "force": true,
			}))
		}
		return answered("Replaced, or waiting.")
	})
	run := rig.begin(t, goingLoopRun{login: "alice", words: words})
	if row := rig.waiting(t, run).Rows[0]; !strings.Contains(goingNowText(row), "One.") {
		t.Errorf("the row shows %s, want the document's text now, which holds One.", row.Now)
	}
	rig.loop.stop()
	rig.answer(t, run, "approve", map[string]any{"plan": 1})
	const versions = `select count(*) from artifact_versions where artifact_id = $1::uuid`
	rig.liveEdit(t, notes.Artifact.ID, "One.", "Bob's.")
	if count := rig.count(t, versions, notes.Artifact.ID); count != 1 {
		t.Fatalf("Bob's edit left %d versions, want the one the upload made: no version may hold the edit's text", count)
	}
	rig.loop.start(rig.goingAgentRig)
	row := rig.waiting(t, run).Rows[0]
	if row.State != "waiting" || !row.Changed || !strings.Contains(goingNowText(row), "Bob's.") {
		t.Fatalf("the row is %s (changed %v) showing %s, want it waiting again with Bob's text", row.State, row.Changed, row.Now)
	}
	if text := rig.documentText(t, notes.Artifact.ID); !strings.Contains(text, "Bob's.") {
		t.Fatalf("notes.md reads %q, want Bob's text kept: the upload wrote over it", text)
	}
	rig.answer(t, run, "approve", map[string]any{"plan": 1})
	if record := rig.ended(t, run, time.Minute); record.state != "done" {
		t.Fatalf("after its approval again the run ended %s with %q, want done; its calls were told %q", record.state, record.reason, rig.toolResults(t, run))
	}
	if text := rig.documentText(t, notes.Artifact.ID); !strings.Contains(text, "The plan's.") {
		t.Errorf("after its approval again notes.md reads %q, want the upload's text", text)
	}
}

// partlyLandedWaiting is a run of alice's whose one row, a reasoned close of a new issue titled
// title, landed its reason and then failed in the server on its close, and whose retry of the key
// found the issue's status changed by bob - so the row waits for alice again, its words saying the
// reason was already written - with the turn ended waiting and the agent's loop stopped. It
// answers the run, the issue and the reason.
func (rig goingPlanRig) partlyLandedWaiting(t *testing.T, title string) (run, issue, reason string) {
	t.Helper()
	issue = rig.issue(t, title)
	words := "close " + title + ", saying why"
	reason = "Shipped in the last release."
	closing := map[string]any{"issue": issue, "status": "done", "reason": reason}
	rig.loop.script.set(words, func(n int) agenttest.Reply {
		if n == 1 {
			return proposing(goingChange("dispatch_issue_update", closing))
		}
		return answered("Closed, or waiting.")
	})
	run = rig.begin(t, goingLoopRun{login: "alice", words: words})
	rig.waiting(t, run)
	rig.loop.stop()
	rig.answer(t, run, "approve", map[string]any{"plan": 1})
	// The test is the run's agent until the row waits again: it claims the run and makes the row's
	// call itself.
	claimed := rig.claim(t, "i-retries-"+issue)
	if claimed == nil || claimed.ID != run {
		t.Fatalf("the claim took %+v, want run %s", claimed, run)
	}
	var patches atomic.Int32
	rig.hooks.set(t, http.MethodPatch, "/api/v1/issues/"+issue, func(w http.ResponseWriter, r *http.Request, inner http.Handler) {
		if patches.Add(1) == 1 {
			panic("the close crashed")
		}
		inner.ServeHTTP(w, r)
	})
	call := goingRouteCall(run, claimed.LeaseToken, 7, 1, "dispatch_issue_update", closing)
	call.Row = &agentwire.PlanRow{Plan: 1, Row: 0}
	_, err := rig.call(call)
	requireAgentCode(t, err, agentwire.CodeCallPanicked, "the call whose close panicked")
	rig.fixture(t, "bob", http.MethodPatch, "/api/v1/issues/"+issue, map[string]any{"status": "in_progress"})
	_, err = rig.call(call)
	requireAgentCode(t, err, agentwire.CodeRowWaits, "the retry of the key after Bob's change")
	if row := rig.plan(t, run).Rows[0]; row.State != "waiting" || !row.Changed || row.Result == nil ||
		!strings.Contains(row.Result.Message, "the reason posted on "+issue+" was already written") || goingRowNumbered.MatchString(row.Result.Message) {
		t.Fatalf("after the retry the row is %s (changed %v) with %+v, want it waiting again, saying the reason on %s was already written",
			row.State, row.Changed, row.Result, issue)
	}
	if reasons, status, sent := rig.goingReasonsOn(t, issue, reason), rig.issueRead(t, issue).Status, patches.Load(); reasons != 1 || status != "in_progress" || sent != 1 {
		t.Fatalf("after the retry %s holds %d reasons and status %s, and the close was sent %d times; want one reason, Bob's status and the one send that panicked",
			issue, reasons, status, sent)
	}
	if err := rig.client.End(context.Background(), agentwire.EndRequest{Run: run, LeaseToken: claimed.LeaseToken, State: agentwire.StateWaiting}); err != nil {
		t.Fatalf("end the turn waiting: %v", err)
	}
	return run, issue, reason
}

// TestARetryOfAPartlyLandedRowReadsItsItemAgainAndPostsItsReasonOnce: a reasoned close whose reason
// landed before its close failed in the server reads its issue again before a retry of its key
// sends the close: another person's change of the issue's status since the approval makes the row
// wait again, saying the reason was already written and is not sent again, and nothing more is
// sent (partlyLandedWaiting). The person's approval of it again runs it under a new key, which takes
// the reason over: the issue holds the reason once and is closed, and the run's record holds the
// close and its reason under one call.
func TestARetryOfAPartlyLandedRowReadsItsItemAgainAndPostsItsReasonOnce(t *testing.T) {
	rig := newGoingPlanRig(t)
	run, issue, reason := rig.partlyLandedWaiting(t, "Closed after a crash")
	rig.answer(t, run, "approve", map[string]any{"plan": 1})
	rig.loop.start(rig.goingAgentRig)
	if record := rig.ended(t, run, time.Minute); record.state != "done" {
		t.Fatalf("after its approval again the run ended %s with %q, want done; its calls were told %q", record.state, record.reason, rig.toolResults(t, run))
	}
	if reasons, status := rig.goingReasonsOn(t, issue, reason), rig.issueRead(t, issue).Status; reasons != 1 || status != "done" {
		t.Fatalf("after its approval again %s holds %d reasons and status %s, want the one reason and the issue done", issue, reasons, status)
	}
	if calls := rig.count(t, `select count(distinct (step, call)) from going_landings where run_id = $1::uuid and phase = 'do'`, run); calls != 1 {
		t.Errorf("the run's record holds its landings under %d calls, want the close and its reason under one", calls)
	}
	if state := rig.plan(t, run).Rows[0].State; state != "done" {
		t.Errorf("after its approval again the row is %s, want done", state)
	}
}

// TestARowApprovedAgainAfterItsReasonLandedSaysSoWhenItsIssueClosedMeanwhile: a reasoned close whose
// reason landed and that waits for its person again (partlyLandedWaiting) is approved again, and
// another person closes its issue before it runs. Its run takes the reason over before the close
// is refused, so the row fails saying the reason was written and the close was not sent, in words,
// the issue holds the reason once, and the reason's landing is the row's own: Undo of the run
// answers one line, the row's, which names the reason sent, not undone.
func TestARowApprovedAgainAfterItsReasonLandedSaysSoWhenItsIssueClosedMeanwhile(t *testing.T) {
	rig := newGoingPlanRig(t)
	run, issue, reason := rig.partlyLandedWaiting(t, "Closed by another meanwhile")
	rig.answer(t, run, "approve", map[string]any{"plan": 1})
	rig.fixture(t, "bob", http.MethodPatch, "/api/v1/issues/"+issue, map[string]any{"status": "done"})
	rig.loop.start(rig.goingAgentRig)
	if record := rig.ended(t, run, time.Minute); record.state != "done" {
		t.Fatalf("after its approval again the run ended %s with %q, want done; its calls were told %q", record.state, record.reason, rig.toolResults(t, run))
	}
	row := rig.plan(t, run).Rows[0]
	if row.State != "failed" || row.Result == nil || row.Result.Code != agentwire.CodeCallRefused ||
		!strings.HasPrefix(row.Result.Message, "the reason posted on "+issue+" was written; the close was not sent: "+issue+" closed at ") {
		t.Fatalf("the row is %s with %+v, want it failed, refused, saying the reason on %s was written and the close was not sent", row.State, row.Result, issue)
	}
	for _, internal := range []string{"POST", "/api/v1/", "ISSUE_CLOSED", "409"} {
		if strings.Contains(row.Result.Message, internal) {
			t.Errorf("the row says %q, which names %q", row.Result.Message, internal)
		}
	}
	if reasons := rig.goingReasonsOn(t, issue, reason); reasons != 1 {
		t.Errorf("%s holds %d reasons, want the one", issue, reasons)
	}
	goingReasonOnlyLine(t, rig.undone(t, run, "dispatch_issue_update")[0], issue)
}

// goingReasonOnlyLine fails t unless line is the undo of a reasoned close's row - row 0 of plan 1 -
// on issue of which only the reason landed: named sent, not undone, as a message is.
func goingReasonOnlyLine(t *testing.T, line goingUndoLine, issue string) {
	t.Helper()
	if line.Plan != 1 || line.Row != 0 || line.Undo != goingUndoNotUndone || !line.Outcome.OK || line.Item != issue {
		t.Fatalf("the reason's undo line is %+v, want row 0 of plan 1 on %s, sent, not undone", line, issue)
	}
}

// TestAnUntickedRowsLandedReasonStaysTheRowsToUndo: a reasoned close whose reason landed and that
// waits for its person again (partlyLandedWaiting) is unticked: Undo of the run answers the row's
// line for the reason that landed, sent, not undone, and no line of a call that names no row.
func TestAnUntickedRowsLandedReasonStaysTheRowsToUndo(t *testing.T) {
	rig := newGoingPlanRig(t)
	run, issue, _ := rig.partlyLandedWaiting(t, "Unticked after its reason landed")
	rig.answer(t, run, "approve", map[string]any{"plan": 1, "rows": []map[string]any{{"row": 0, "tick": false}}})
	goingReasonOnlyLine(t, rig.undone(t, run, "dispatch_issue_update")[0], issue)
}

// TestARowApprovedAgainWithAnotherReasonKeepsTheFirstReasonToUndo: a reasoned close whose reason
// landed and that waits for its person again (partlyLandedWaiting) is approved again with another
// reason, which its next try posts with the close. Undo of the run answers two lines of the row:
// the close, written back, and the first reason, sent, not undone.
func TestARowApprovedAgainWithAnotherReasonKeepsTheFirstReasonToUndo(t *testing.T) {
	rig := newGoingPlanRig(t)
	run, issue, reason := rig.partlyLandedWaiting(t, "Approved again with another reason")
	edited := "Shipped, and announced in the release notes."
	rig.answer(t, run, "approve", map[string]any{"plan": 1, "rows": []map[string]any{{"row": 0, "tick": true, "values": map[string]any{"reason": edited}}}})
	rig.loop.start(rig.goingAgentRig)
	if record := rig.ended(t, run, time.Minute); record.state != "done" {
		t.Fatalf("after its approval again the run ended %s with %q, want done; its calls were told %q", record.state, record.reason, rig.toolResults(t, run))
	}
	if first, second, status := rig.goingReasonsOn(t, issue, reason), rig.goingReasonsOn(t, issue, edited), rig.issueRead(t, issue).Status; first != 1 || second != 1 || status != "done" {
		t.Fatalf("%s holds %d of the first reason, %d of the edited one and status %s, want one of each and the issue done", issue, first, second, status)
	}
	lines := rig.undone(t, run, "dispatch_issue_update", "dispatch_issue_update")
	at := slices.IndexFunc(lines, func(line goingUndoLine) bool { return line.Undo == goingUndoWriteBack })
	if at < 0 || lines[at].Plan != 1 || lines[at].Row != 0 || !lines[at].Outcome.OK || rig.issueRead(t, issue).Status != "in_progress" {
		t.Fatalf("the undo lines are %+v and the issue's status %s, want the close of row 0 of plan 1 written back to Bob's in_progress",
			lines, rig.issueRead(t, issue).Status)
	}
	goingReasonOnlyLine(t, lines[1-at], issue)
}

// TestARowApprovedAgainKeepsItsLandedReasonToUndoBeforeItRunsAgain: a reasoned close whose reason
// landed and that waits for its person again (partlyLandedWaiting) is approved again, and Undo is
// pressed before the row's next try: it answers the row's line for the reason, sent, not undone.
func TestARowApprovedAgainKeepsItsLandedReasonToUndoBeforeItRunsAgain(t *testing.T) {
	rig := newGoingPlanRig(t)
	run, issue, _ := rig.partlyLandedWaiting(t, "Undone before its next try")
	rig.answer(t, run, "approve", map[string]any{"plan": 1})
	goingReasonOnlyLine(t, rig.undone(t, run, "dispatch_issue_update")[0], issue)
}

// TestAFailedChangeSkipsWhatNeedsIt: a row whose write its route refuses fails, the row that needs
// it is skipped and says so, and a row that needs neither runs.
func TestAFailedChangeSkipsWhatNeedsIt(t *testing.T) {
	rig := newGoingPlanRig(t)
	held, free := rig.issue(t, "Held by bob"), rig.issue(t, "Free")
	rig.fixture(t, "bob", http.MethodPost, "/api/v1/issues/"+held+"/claim", map[string]any{})
	const words = "take Held by bob, rename it, and rename Free"
	rig.loop.script.set(words, func(n int) agenttest.Reply {
		if n == 1 {
			return proposing(goingChange("dispatch_claim", map[string]any{"issue": held}),
				goingChange("dispatch_issue_update", retitle(held, "Renamed held"), 0), goingChange("dispatch_issue_update", retitle(free, "Renamed free")))
		}
		return answered("One failed.")
	})
	run := rig.begin(t, goingLoopRun{login: "alice", words: words})
	rig.waiting(t, run)
	rig.answer(t, run, "approve", map[string]any{"plan": 1})
	if record := rig.ended(t, run, time.Minute); record.state != "done" {
		t.Fatalf("the run ended %s with %q, want done", record.state, record.reason)
	}
	plan := rig.plan(t, run)
	if states := rowStates(plan); !slices.Equal(states, []string{"failed", "skipped", "done"}) {
		t.Fatalf("the rows are %v, want failed, skipped, done", states)
	}
	if skipped := plan.Rows[1].Result; skipped == nil || skipped.Code != "ROW_SKIPPED" || !strings.Contains(skipped.Message, "claim "+held) ||
		goingRowNumbered.MatchString(skipped.Message) {
		t.Errorf("the skipped row's result is %+v, want ROW_SKIPPED naming the row it needs as the page does, claim %s, and no row by number", skipped, held)
	}
	if rig.title(t, held) != "Held by bob" || rig.title(t, free) != "Renamed free" {
		t.Errorf("the titles are %q and %q, want the skipped row's left and the free row's written", rig.title(t, held), rig.title(t, free))
	}
}

// TestARowWhoseItemChangedWaitsAgain: a value another person changed after the approval and
// before the row's write makes the row wait again, showing the new value, while the other rows
// run; the person's approval of it again runs it. And where two rows of a plan write one item,
// another writer's change right after the first lands and before the second runs makes the second
// wait again, though the plan's own first write changed the item too.
func TestARowWhoseItemChangedWaitsAgain(t *testing.T) {
	rig := newGoingPlanRig(t)
	moved, still := rig.issue(t, "Moved meanwhile"), rig.issue(t, "Still")
	const words = "rename Moved meanwhile and Still"
	rig.loop.script.set(words, func(n int) agenttest.Reply {
		if n == 1 {
			return proposing(goingChange("dispatch_issue_update", retitle(moved, "Ours")), goingChange("dispatch_issue_update", retitle(still, "Ours too")))
		}
		return answered("Done, or waiting.")
	})
	run := rig.begin(t, goingLoopRun{login: "alice", words: words})
	rig.waiting(t, run)
	rig.loop.stop()
	rig.answer(t, run, "approve", map[string]any{"plan": 1})
	rig.fixture(t, "bob", http.MethodPatch, "/api/v1/issues/"+moved, map[string]any{"title": "Bob's"})
	rig.loop.start(rig.goingAgentRig)
	plan := rig.waiting(t, run)
	if row := plan.Rows[0]; row.State != "waiting" || !row.Changed || !strings.Contains(string(row.Now), "Bob's") {
		t.Fatalf("the changed row is %s (changed %v) showing %s, want it waiting again with Bob's title", row.State, row.Changed, row.Now)
	}
	if plan.Rows[1].State != "done" || rig.title(t, still) != "Ours too" || rig.title(t, moved) != "Bob's" {
		t.Fatalf("the other row is %s and the titles %q and %q, want it run and Bob's title kept", plan.Rows[1].State, rig.title(t, still), rig.title(t, moved))
	}
	rig.answer(t, run, "approve", map[string]any{"plan": 1})
	if record := rig.ended(t, run, time.Minute); record.state != "done" || rig.title(t, moved) != "Ours" {
		t.Fatalf("after approving it again the run is %s and the title %q, want done with Ours", record.state, rig.title(t, moved))
	}

	twice := rig.issue(t, "Written twice")
	const again = "comment on Written twice, and rename it twice"
	rig.loop.script.set(again, func(n int) agenttest.Reply {
		if n == 1 {
			return proposing(goingChange("dispatch_comment", map[string]any{"issue": twice, "body": "First of three."}),
				goingChange("dispatch_issue_update", retitle(twice, "Ours first")), goingChange("dispatch_issue_update", retitle(twice, "Ours second")))
		}
		return answered("Two ran, and one waits.")
	})
	var renames atomic.Int32
	rig.hooks.set(t, http.MethodPatch, "/api/v1/issues/"+twice, func(w http.ResponseWriter, r *http.Request, inner http.Handler) {
		inner.ServeHTTP(w, r)
		if renames.Add(1) == 1 {
			rig.fixture(t, "bob", http.MethodPatch, "/api/v1/issues/"+twice, map[string]any{"title": "Bob's between"})
		}
	})
	run = rig.begin(t, goingLoopRun{login: "alice", words: again})
	rig.waiting(t, run)
	rig.answer(t, run, "approve", map[string]any{"plan": 1})
	plan = rig.waiting(t, run)
	if states := rowStates(plan); !slices.Equal(states, []string{"done", "done", "waiting"}) || !plan.Rows[2].Changed || rig.title(t, twice) != "Bob's between" {
		t.Fatalf("the rows are %v (the last changed %v) and the title %q, want the last waiting again over Bob's title", states, plan.Rows[2].Changed, rig.title(t, twice))
	}
}

// TestASecondTryOfARowReadsItsItemAgain: a second try of a row's call key that finds nothing of the
// key landed writes, so it reads the row's item again first, as a first try does: where another
// person changed the item after the approval, the row waits for its person again, showing the new
// value, and nothing is written. The first try kept nothing, since it failed in the server, or kept
// the refusal its route answered, and its agent died before it saved the step.
func TestASecondTryOfARowReadsItsItemAgain(t *testing.T) {
	rig := newGoingPlanRig(t)
	for _, first := range []struct{ name, state, record string }{
		{"after a try that failed in the server", "running", `{}`},
		{"after a try whose route refused the write", "failed", `{"status": 503, "code": "SERVICE_UNAVAILABLE", "message": "try again"}`},
	} {
		t.Run(first.name, func(t *testing.T) {
			issue := rig.issue(t, "Renamed meanwhile")
			words := "rename the issue renamed meanwhile, " + first.state
			rig.loop.script.set(words, func(n int) agenttest.Reply {
				if n == 1 {
					return proposing(goingChange("dispatch_issue_update", retitle(issue, "Ours")))
				}
				return answered("Renamed, or waiting.")
			})
			run := rig.begin(t, goingLoopRun{login: "alice", words: words})
			rig.waiting(t, run)
			rig.loop.stop()
			rig.answer(t, run, "approve", map[string]any{"plan": 1})
			// The row's first try, at the run's next step, left this on the row and landed nothing.
			step := rig.count(t, `select coalesce(max(step), 0) + 1 from going_run_steps where run_id = $1::uuid`, run)
			rig.exec(t, `update going_plan_rows set state = $2, result = $3::jsonb || jsonb_build_object('step', $4::int, 'call', 1) where run_id = $1::uuid`,
				run, first.state, first.record, step)
			rig.fixture(t, "bob", http.MethodPatch, "/api/v1/issues/"+issue, map[string]any{"title": "Bob's"})
			rig.loop.start(rig.goingAgentRig)
			plan := rig.waiting(t, run)
			if row := plan.Rows[0]; row.State != "waiting" || !row.Changed || !strings.Contains(string(row.Now), "Bob's") {
				t.Fatalf("the row is %s (changed %v) showing %s, want it waiting again with Bob's title", row.State, row.Changed, row.Now)
			}
			if title := rig.title(t, issue); title != "Bob's" {
				t.Fatalf("the issue is titled %q, want Bob's title kept: the second try wrote over it", title)
			}
		})
	}
}

// TestARowWhoseItemCannotBeReadAgainIsNotWritten: a read of an approved row's item again before its
// write that fails - its route answers 500 - ends the row's call failed with nothing written, and the
// row is left approved; it is never passed over to write the row unchecked.
func TestARowWhoseItemCannotBeReadAgainIsNotWritten(t *testing.T) {
	rig := newGoingPlanRig(t)
	issue := rig.issue(t, "Unreadable for a while")
	const words = "rename Unreadable for a while"
	rig.loop.script.set(words, func(n int) agenttest.Reply {
		if n == 1 {
			return proposing(goingChange("dispatch_issue_update", retitle(issue, "Ours")))
		}
		return answered("Renamed, or not.")
	})
	run := rig.begin(t, goingLoopRun{login: "alice", words: words})
	rig.waiting(t, run)
	rig.loop.stop()
	rig.answer(t, run, "approve", map[string]any{"plan": 1})
	var reads atomic.Int32
	rig.hooks.set(t, http.MethodGet, "/api/v1/issues/"+issue, func(w http.ResponseWriter, _ *http.Request, _ http.Handler) {
		reads.Add(1)
		writeError(w, codeInternal, http.StatusInternalServerError, "the issue could not be read")
	})
	rig.loop.start(rig.goingAgentRig)
	rig.ended(t, run, time.Minute)
	if reads.Load() == 0 {
		t.Fatal("no read of the issue reached its route after the approval")
	}
	if title := rig.title(t, issue); title != "Unreadable for a while" {
		t.Fatalf("the issue is titled %q, want its title kept: the row was written though its read failed; its call was told %q", title, rig.toolResults(t, run))
	}
	if state := rig.plan(t, run).Rows[0].State; state != "approved" {
		t.Errorf("the row is %s, want it approved still, its write never sent", state)
	}
}

// TestARowWhoseRouteCrashedFailsAndIsReadAgainBeforeItsRetry: a row whose route panics is kept
// failed, status 500 and GOING_ROUTE_FAILED, telling its person the change failed in the server
// with nothing of it known to have been written, and its call is answered CALL_PANICKED. A retry
// of its key reads the row's item again before it writes: a change another person made since makes
// the row wait for its person again, and that person's title is kept.
func TestARowWhoseRouteCrashedFailsAndIsReadAgainBeforeItsRetry(t *testing.T) {
	rig := newGoingPlanRig(t)
	issue := rig.issue(t, "Crashed on")
	const words = "rename Crashed on"
	rename := retitle(issue, "Ours")
	rig.loop.script.set(words, func(n int) agenttest.Reply {
		if n == 1 {
			return proposing(goingChange("dispatch_issue_update", rename))
		}
		return answered("Renamed, or not.")
	})
	run := rig.begin(t, goingLoopRun{login: "alice", words: words})
	rig.waiting(t, run)
	rig.loop.stop()
	rig.answer(t, run, "approve", map[string]any{"plan": 1})
	// The test is the run's agent from here: it claims the run and makes the row's call itself.
	claimed := rig.claim(t, "i-crashes")
	if claimed == nil || claimed.ID != run {
		t.Fatalf("the claim took %+v, want run %s", claimed, run)
	}
	var patches atomic.Int32
	rig.hooks.set(t, http.MethodPatch, "/api/v1/issues/"+issue, func(w http.ResponseWriter, r *http.Request, inner http.Handler) {
		if patches.Add(1) == 1 {
			panic("the route crashed")
		}
		inner.ServeHTTP(w, r)
	})
	call := goingRouteCall(run, claimed.LeaseToken, 7, 1, "dispatch_issue_update", rename)
	call.Row = &agentwire.PlanRow{Plan: 1, Row: 0}
	_, err := rig.call(call)
	requireAgentCode(t, err, agentwire.CodeCallPanicked, "the call whose route panicked")
	if row := rig.plan(t, run).Rows[0]; row.State != "failed" || row.Result == nil || row.Result.Status != http.StatusInternalServerError ||
		row.Result.Code != codeGoingRouteFailed || row.Result.Message != goingRouteFailedText {
		t.Fatalf("the row is %s with %+v, want it failed, 500 %s, saying %q", row.State, row.Result, codeGoingRouteFailed, goingRouteFailedText)
	}
	if title := rig.title(t, issue); title != "Crashed on" {
		t.Fatalf("the issue is titled %q after its route panicked, want its title kept", title)
	}
	rig.fixture(t, "bob", http.MethodPatch, "/api/v1/issues/"+issue, map[string]any{"title": "Bob's"})
	_, err = rig.call(call)
	requireAgentCode(t, err, agentwire.CodeRowWaits, "the retry of the key after Bob's change")
	if row := rig.plan(t, run).Rows[0]; row.State != "waiting" || !row.Changed || !strings.Contains(string(row.Now), "Bob's") {
		t.Fatalf("after the retry the row is %s (changed %v) showing %s, want it waiting again with Bob's title", row.State, row.Changed, row.Now)
	}
	if title, sent := rig.title(t, issue), patches.Load(); title != "Bob's" || sent != 1 {
		t.Fatalf("after the retry the issue is titled %q and the write was sent %d times, want Bob's title and the one send that panicked", title, sent)
	}
}

// TestARowWhoseRouteCrashedPartwaySaysWhatWasWritten: a reasoned close, whose call sends the reason
// and then the close, lands its reason and panics in the server on its close. Its row is kept
// failed, status 500 and GOING_ROUTE_FAILED, saying the reason was written and the rest was not;
// the issue holds the reason and is open. A retry of its key sends the close alone, and the issue
// holds the reason once and is closed.
func TestARowWhoseRouteCrashedPartwaySaysWhatWasWritten(t *testing.T) {
	rig := newGoingPlanRig(t)
	issue := rig.issue(t, "Closed partway")
	const words = "close Closed partway, saying why"
	reason := "Shipped in the last release."
	closing := map[string]any{"issue": issue, "status": "done", "reason": reason}
	rig.loop.script.set(words, func(n int) agenttest.Reply {
		if n == 1 {
			return proposing(goingChange("dispatch_issue_update", closing))
		}
		return answered("Closed, or not.")
	})
	run := rig.begin(t, goingLoopRun{login: "alice", words: words})
	rig.waiting(t, run)
	rig.loop.stop()
	rig.answer(t, run, "approve", map[string]any{"plan": 1})
	// The test is the run's agent from here: it claims the run and makes the row's call itself.
	claimed := rig.claim(t, "i-crashes-partway")
	if claimed == nil || claimed.ID != run {
		t.Fatalf("the claim took %+v, want run %s", claimed, run)
	}
	var patches atomic.Int32
	rig.hooks.set(t, http.MethodPatch, "/api/v1/issues/"+issue, func(w http.ResponseWriter, r *http.Request, inner http.Handler) {
		if patches.Add(1) == 1 {
			panic("the close crashed")
		}
		inner.ServeHTTP(w, r)
	})
	call := goingRouteCall(run, claimed.LeaseToken, 7, 1, "dispatch_issue_update", closing)
	call.Row = &agentwire.PlanRow{Plan: 1, Row: 0}
	_, err := rig.call(call)
	requireAgentCode(t, err, agentwire.CodeCallPanicked, "the call whose close panicked")
	if row := rig.plan(t, run).Rows[0]; row.State != "failed" || row.Result == nil || row.Result.Status != http.StatusInternalServerError ||
		row.Result.Code != codeGoingRouteFailed || !strings.HasPrefix(row.Result.Message, "this change failed in the server partway: ") ||
		!strings.Contains(row.Result.Message, "reason") || !strings.Contains(row.Result.Message, issue) {
		t.Fatalf("the row is %s with %+v, want it failed, 500 %s, saying the reason on %s was written and the rest was not",
			row.State, row.Result, codeGoingRouteFailed, issue)
	}
	if reasons, status := rig.goingReasonsOn(t, issue, reason), rig.issueRead(t, issue).Status; reasons != 1 || status == "done" {
		t.Fatalf("after the panic %s holds %d reasons and status %s, want the reason posted and the issue open", issue, reasons, status)
	}
	if answer, err := rig.call(call); err != nil || answer.Status != http.StatusOK {
		t.Fatalf("the retry answered %d, %v; want the close sent and answered 200", answer.Status, err)
	}
	if reasons, status := rig.goingReasonsOn(t, issue, reason), rig.issueRead(t, issue).Status; reasons != 1 || status != "done" {
		t.Fatalf("after the retry %s holds %d reasons and status %s, want one reason and the issue done", issue, reasons, status)
	}
	if state := rig.plan(t, run).Rows[0].State; state != "done" {
		t.Errorf("after the retry the row is %s, want done", state)
	}
}

// TestARowAndAnUndoLineSayWhatHappenedInWords: what a plan's row and an Undo line tell their person
// says what happened in words, and names no tool id, HTTP status, code, method or path, which stay
// in the row's and the line's own fields: a row its translation refused after its approval - a
// reasoned close of an issue another person closed meanwhile - fails saying the issue closed, and
// the undo of a write whose record holds nothing of what it left says nothing was put back.
func TestARowAndAnUndoLineSayWhatHappenedInWords(t *testing.T) {
	rig := newGoingPlanRig(t)
	namesNone := func(t *testing.T, what, text string, names ...string) {
		t.Helper()
		for _, name := range names {
			if strings.Contains(text, name) {
				t.Errorf("%s says %q, which names %q", what, text, name)
			}
		}
	}
	t.Run("a row its translation refused", func(t *testing.T) {
		issue := rig.issue(t, "Closed meanwhile")
		const words = "close Closed meanwhile, saying why"
		rig.loop.script.set(words, func(n int) agenttest.Reply {
			if n == 1 {
				return proposing(goingChange("dispatch_issue_update", map[string]any{"issue": issue, "status": "done", "reason": "Shipped in the last release."}))
			}
			return answered("Closed, or not.")
		})
		run := rig.begin(t, goingLoopRun{login: "alice", words: words})
		rig.waiting(t, run)
		rig.loop.stop()
		rig.answer(t, run, "approve", map[string]any{"plan": 1})
		rig.fixture(t, "bob", http.MethodPatch, "/api/v1/issues/"+issue, map[string]any{"status": "done"})
		rig.loop.start(rig.goingAgentRig)
		rig.ended(t, run, time.Minute)
		row := rig.plan(t, run).Rows[0]
		if row.State != "failed" || row.Result == nil || row.Result.Code != agentwire.CodeCallRefused || !strings.Contains(row.Result.Message, issue+" closed at") {
			t.Fatalf("the row is %s with %+v, want it failed, refused, saying %s closed", row.State, row.Result, issue)
		}
		namesNone(t, "the row", row.Result.Message, "dispatch_", "409", "ISSUE_CLOSED")
	})
	t.Run("an undo line of a write whose record holds nothing of what it left", func(t *testing.T) {
		issue := rig.issue(t, "Renamed unrecorded")
		run := rig.ran(t, "rename Renamed unrecorded", goingChange("dispatch_issue_update", retitle(issue, "Renamed")))
		rig.exec(t, `update going_landings set answer = answer - 'body' where run_id = $1::uuid and phase = 'do'`, run)
		line := rig.undone(t, run, "dispatch_issue_update")[0]
		if line.Outcome.OK || line.Outcome.Code != codeGoingUndoAfterUnread || rig.title(t, issue) != "Renamed" {
			t.Fatalf("Undo answered %+v and the title is %q, want nothing put back, %s", line.Outcome, rig.title(t, issue), codeGoingUndoAfterUnread)
		}
		namesNone(t, "the undo line", line.Outcome.Message, "dispatch_", "PATCH", "/api/v1/")
	})
}

// TestThePlanStates: the states of a plan and of its rows, each reached through the routes and
// the agent - shown (waiting), approved, running, waiting again, done and rejected - and each
// event: approve, untick (which unticks each row that needs it), edit, revise, item changed and
// row failed.
func TestThePlanStates(t *testing.T) {
	rig := newGoingPlanRig(t)
	start := func(t *testing.T, words string, changes func(n int) agenttest.Reply) string {
		t.Helper()
		rig.loop.script.set(words, changes)
		return rig.begin(t, goingLoopRun{login: "alice", words: words})
	}

	t.Run("shown, untick, edit, approved, running, done", func(t *testing.T) {
		x, y, z := rig.issue(t, "X"), rig.issue(t, "Y"), rig.issue(t, "Z")
		run := start(t, "rename X, Y and Z", func(n int) agenttest.Reply {
			if n == 1 {
				return proposing(goingChange("dispatch_issue_update", retitle(x, "X2")),
					goingChange("dispatch_issue_update", retitle(y, "Y2"), 0), goingChange("dispatch_issue_update", retitle(z, "Z2")))
			}
			return answered("Renamed Z.")
		})
		if plan := rig.waiting(t, run); plan.State != "waiting" || !slices.Equal(rowStates(plan), []string{"waiting", "waiting", "waiting"}) {
			t.Fatalf("the shown plan is %s with rows %v, want it and every row waiting", plan.State, rowStates(plan))
		}
		rig.loop.stop()
		plan := rig.answer(t, run, "approve", map[string]any{"plan": 1, "rows": []map[string]any{
			{"row": 0, "tick": false}, {"row": 2, "tick": true, "values": map[string]any{"title": "Z edited"}},
		}})
		if plan.State != "approved" || !slices.Equal(rowStates(plan), []string{"unticked", "unticked", "approved"}) {
			t.Fatalf("the approved plan is %s with rows %v, want approved with the untick's two rows unticked", plan.State, rowStates(plan))
		}
		hold := make(chan struct{})
		rig.hooks.set(t, http.MethodPatch, "/api/v1/issues/"+z, func(w http.ResponseWriter, r *http.Request, inner http.Handler) {
			<-hold
			inner.ServeHTTP(w, r)
		})
		rig.loop.start(rig.goingAgentRig)
		goingEventually(t, "the row to run", func() bool { return rig.plan(t, run).State == "running" })
		close(hold)
		if record := rig.ended(t, run, time.Minute); record.state != "done" {
			t.Fatalf("the run ended %s with %q, want done", record.state, record.reason)
		}
		if plan := rig.plan(t, run); plan.State != "done" || rig.title(t, z) != "Z edited" || rig.title(t, x) != "X" || rig.title(t, y) != "Y" {
			t.Fatalf("the plan is %s and the titles %q %q %q, want done with the edited title alone", plan.State, rig.title(t, x), rig.title(t, y), rig.title(t, z))
		}
	})

	t.Run("item changed: waiting again", func(t *testing.T) {
		x := rig.issue(t, "Changed at the approval")
		run := start(t, "rename Changed at the approval", func(int) agenttest.Reply {
			return proposing(goingChange("dispatch_issue_update", retitle(x, "Ours")))
		})
		rig.waiting(t, run)
		rig.fixture(t, "bob", http.MethodPatch, "/api/v1/issues/"+x, map[string]any{"title": "Bob's first"})
		plan := rig.answer(t, run, "approve", map[string]any{"plan": 1})
		if plan.State != "waiting" || plan.Rows[0].State != "waiting" || !plan.Rows[0].Changed {
			t.Fatalf("the plan approved over a changed item is %s with its row %s (changed %v), want the row waiting again", plan.State, plan.Rows[0].State, plan.Rows[0].Changed)
		}
	})

	t.Run("row failed", func(t *testing.T) {
		x := rig.issue(t, "Claimed by bob")
		rig.fixture(t, "bob", http.MethodPost, "/api/v1/issues/"+x+"/claim", map[string]any{})
		run := start(t, "take Claimed by bob", func(n int) agenttest.Reply {
			if n == 1 {
				return proposing(goingChange("dispatch_claim", map[string]any{"issue": x}))
			}
			return answered("Bob holds it.")
		})
		rig.waiting(t, run)
		rig.answer(t, run, "approve", map[string]any{"plan": 1})
		rig.ended(t, run, time.Minute)
		if plan := rig.plan(t, run); plan.Rows[0].State != "failed" || plan.Rows[0].Result == nil || plan.Rows[0].Result.Status != http.StatusConflict {
			t.Fatalf("the refused row is %s with %+v, want failed with its route's 409", plan.Rows[0].State, plan.Rows[0].Result)
		}
	})

	t.Run("revise: rejected, and the run proposes the next plan", func(t *testing.T) {
		x := rig.issue(t, "Revised")
		const words = "rename Revised"
		run := start(t, words, func(n int) agenttest.Reply {
			if n == 1 {
				return proposing(goingChange("dispatch_issue_update", retitle(x, "First try")))
			}
			return proposing(goingChange("dispatch_issue_update", retitle(x, "As the person said")))
		})
		rig.waiting(t, run)
		if plan := rig.answer(t, run, "revise", map[string]any{"plan": 1, "words": "call it As the person said"}); plan.State != "rejected" {
			t.Fatalf("the revised plan is %s, want rejected", plan.State)
		}
		plan := rig.waiting(t, run)
		if plan.Plan != 2 || plan.State != "waiting" || !strings.Contains(string(plan.Rows[0].Arguments), "As the person said") {
			t.Fatalf("after the revision the plan is %d, %s, %s; want plan 2 waiting with the revised change", plan.Plan, plan.State, plan.Rows[0].Arguments)
		}
		if text := rig.loop.fake.Asking(words)[1].Text(); !strings.Contains(text, "call it As the person said") || !strings.Contains(text, "First try") {
			t.Errorf("the model's next request does not hold the person's words beside the plan's rows: %s", text)
		}
	})

	t.Run("reject: rejected, and the run is done with nothing run", func(t *testing.T) {
		x := rig.issue(t, "Rejected")
		run := start(t, "rename Rejected", func(int) agenttest.Reply {
			return proposing(goingChange("dispatch_issue_update", retitle(x, "Never")))
		})
		rig.waiting(t, run)
		if plan := rig.answer(t, run, "reject", map[string]any{"plan": 1}); plan.State != "rejected" {
			t.Fatalf("the rejected plan is %s", plan.State)
		}
		if record := rig.record(t, run); record.state != "done" || record.reason != goingPlanRejected || rig.title(t, x) != "Rejected" {
			t.Fatalf("the run is %s with %q and the title %q, want done with nothing run", record.state, record.reason, rig.title(t, x))
		}
	})
}

// TestAnApprovalOfAPlanAnotherPageReadSinceIsRefused: an approval names the version of the plan
// its page read. Where another page read the plan since and the plan moved - its waiting row read
// again with the value another writer left - the older page's approval is refused, 409
// GOING_PLAN_CHANGED, with nothing written, so its person never approves a value their page did
// not show; the page that read the plan last approves it.
func TestAnApprovalOfAPlanAnotherPageReadSinceIsRefused(t *testing.T) {
	rig := newGoingPlanRig(t)
	issue := rig.issue(t, "Read in two tabs")
	const words = "rename Read in two tabs"
	rig.loop.script.set(words, func(n int) agenttest.Reply {
		if n == 1 {
			return proposing(goingChange("dispatch_issue_update", retitle(issue, "Ours")))
		}
		return answered("Renamed.")
	})
	run := rig.begin(t, goingLoopRun{login: "alice", words: words})
	older := rig.waiting(t, run)
	rig.fixture(t, "bob", http.MethodPatch, "/api/v1/issues/"+issue, map[string]any{"title": "Bob's"})
	newer := rig.plan(t, run)
	if newer.Version == older.Version || !strings.Contains(string(newer.Rows[0].Now), "Bob's") {
		t.Fatalf("the second page read version %s showing %s, the first %s: want the row read again with Bob's title, under another version",
			newer.Version, newer.Rows[0].Now, older.Version)
	}
	refused := dispatchRequest(t, rig.mux, http.MethodPost, "/api/v1/going/runs/"+run+"/plan/approve",
		map[string]any{"plan": 1, "version": older.Version}, "alice")
	if refused.Code != http.StatusConflict || responseCode(t, refused) != codeGoingPlanChanged {
		t.Fatalf("the first page's approval answered %d %s, want 409 %s", refused.Code, refused.Body.String(), codeGoingPlanChanged)
	}
	if plan := rig.plan(t, run); plan.State != "waiting" || plan.Rows[0].State != "waiting" || plan.Version != newer.Version || rig.title(t, issue) != "Bob's" {
		t.Fatalf("after the refusal the plan is %s with its row %s (version %s) and the title %q, want nothing written",
			plan.State, plan.Rows[0].State, plan.Version, rig.title(t, issue))
	}
	rig.answer(t, run, "approve", map[string]any{"plan": 1, "version": newer.Version})
	if record := rig.ended(t, run, time.Minute); record.state != "done" || rig.title(t, issue) != "Ours" {
		t.Fatalf("after the second page's approval the run is %s and the title %q, want done with Ours", record.state, rig.title(t, issue))
	}
}

// TestTheInboxNamesTheVersionOfEachWaitingPlan: the Inbox's row of a waiting plan (GET
// /api/v1/me/going-plans) names the plan's version as the plan's own answer does, so an approval
// from the Inbox holds the plan to what the Inbox showed: once another page read the plan again
// and it moved, the Inbox names the new version, an approval naming the one it named before is
// refused 409 GOING_PLAN_CHANGED with nothing written, and one naming the new version runs.
func TestTheInboxNamesTheVersionOfEachWaitingPlan(t *testing.T) {
	rig := newGoingPlanRig(t)
	issue := rig.issue(t, "Approved from the Inbox")
	const words = "rename Approved from the Inbox"
	rig.loop.script.set(words, func(n int) agenttest.Reply {
		if n == 1 {
			return proposing(goingChange("dispatch_issue_update", retitle(issue, "Ours")))
		}
		return answered("Renamed.")
	})
	run := rig.begin(t, goingLoopRun{login: "alice", words: words})
	first := rig.waiting(t, run)
	inbox := func(t *testing.T) goingWaitingPlan {
		t.Helper()
		response := dispatchRequest(t, rig.mux, http.MethodGet, "/api/v1/me/going-plans", nil, "alice")
		if response.Code != http.StatusOK {
			t.Fatalf("GET /api/v1/me/going-plans answered %d %s", response.Code, response.Body.String())
		}
		for _, row := range decodeBody[goingWaitingPlansPage](t, response).Rows {
			if row.Run == run {
				return row
			}
		}
		t.Fatalf("the Inbox lists no waiting plan of run %s", run)
		return goingWaitingPlan{}
	}
	shown := inbox(t)
	if shown.Version == "" || shown.Version != first.Version {
		t.Fatalf("the Inbox names version %q of the plan, whose own answer names %q: want the same version", shown.Version, first.Version)
	}
	rig.fixture(t, "bob", http.MethodPatch, "/api/v1/issues/"+issue, map[string]any{"title": "Bob's"})
	newer := rig.plan(t, run)
	if moved := inbox(t); moved.Version != newer.Version || moved.Version == shown.Version {
		t.Fatalf("after another page read the plan again the Inbox names version %q, the plan %q and the Inbox before %q: want the plan's new version",
			moved.Version, newer.Version, shown.Version)
	}
	refused := dispatchRequest(t, rig.mux, http.MethodPost, "/api/v1/going/runs/"+run+"/plan/approve",
		map[string]any{"plan": shown.Plan, "version": shown.Version}, "alice")
	if refused.Code != http.StatusConflict || responseCode(t, refused) != codeGoingPlanChanged || rig.title(t, issue) != "Bob's" {
		t.Fatalf("the approval naming the Inbox's earlier version answered %d %s and left the title %q, want 409 %s and nothing written",
			refused.Code, refused.Body.String(), rig.title(t, issue), codeGoingPlanChanged)
	}
	rig.answer(t, run, "approve", map[string]any{"plan": shown.Plan, "version": inbox(t).Version})
	if record := rig.ended(t, run, time.Minute); record.state != "done" || rig.title(t, issue) != "Ours" {
		t.Fatalf("after the approval naming the Inbox's version the run is %s and the title %q, want done with Ours", record.state, rig.title(t, issue))
	}
}
