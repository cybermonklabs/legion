package api

// going_plan_test.go holds a run's plan as its person meets it: the agent's real loop, on a fake
// chat model, proposes a plan; the person reads and answers it through the plan routes; and the
// agent runs the approved rows through the server (going_plan.go, going_plan_run.go).

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
	"unicode"

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

// goingChangeNumbered matches a row named as the model's plan names it, a change by its number,
// which no page shows either.
var goingChangeNumbered = regexp.MustCompile(`\bchanges? \d`)

// goingChange is one change of a proposed plan: tool with arguments, needing the changes needs. A
// change that needs none leaves needs out, as propose_plan's schema takes no null for it.
func goingChange(tool string, arguments map[string]any, needs ...int) map[string]any {
	change := map[string]any{"tool": tool, "arguments": arguments}
	if len(needs) > 0 {
		change["needs"] = needs
	}
	return change
}

// requireRefusalNames fails t unless refused is a CALL_REFUSED refusal whose words name each of
// names as a word of its own: the facts the refusal must give - a change's number, an issue's
// key, a field - whatever sentence it gives them in.
func requireRefusalNames(t *testing.T, refused *agentwire.Error, names ...string) {
	t.Helper()
	if refused == nil || refused.Code != agentwire.CodeCallRefused {
		t.Fatalf("the plan call answered %+v, want %s", refused, agentwire.CodeCallRefused)
	}
	words := strings.FieldsFunc(refused.Message, func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r) && !strings.ContainsRune("_-$", r)
	})
	for _, name := range names {
		if !slices.Contains(words, name) {
			t.Errorf("the refusal says %q, which does not name %s", refused.Message, name)
		}
	}
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
	t.Parallel()
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
	t.Parallel()
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

	// The ruling of 12:50Z on R5: a post's body is text, so an edit and a delete of a comment or an
	// issue message each wait for the person in Ask and in Auto-review, never run unseen.
	for _, mode := range []string{goingModeAsk, goingModeAutoReview} {
		t.Run("an edit and a delete of a comment or an issue message wait in "+mode, func(t *testing.T) {
			var changes []map[string]any
			var tools []string
			for _, c := range goingPostCases {
				edited, _ := c.make(t, rig, "The ferry leaves at noon.")
				deleted, _ := c.make(t, rig, "The ferry leaves at one.")
				changes = append(changes, c.editing(edited, "The ferry leaves at two."), c.deleting(deleted))
				tools = append(tools, c.edit, c.delete)
			}
			words := "correct the ferry's posts in " + mode
			rig.loop.script.set(words, func(n int) agenttest.Reply {
				if n == 1 {
					return proposing(changes...)
				}
				return toolCall(toolReviewPlanName, map[string]any{"asked": everyRow(len(changes))})
			})
			plan := rig.waiting(t, rig.begin(t, goingLoopRun{login: "alice", words: words, mode: mode}))
			if len(plan.Rows) != len(tools) {
				t.Fatalf("the plan holds %d rows, want %d", len(plan.Rows), len(tools))
			}
			for index, row := range plan.Rows {
				if row.Tool != tools[index] || row.State != "waiting" || row.Class != string(goingClassText) {
					t.Errorf("in %s the row of %s is a %s row of class %s, %s; want %s waiting, of class %s",
						mode, tools[index], row.Tool, row.Class, row.State, tools[index], goingClassText)
				}
			}
		})
	}

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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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

// TestAnUploadRowOverADocumentWithNoTokenWritesNothingOverAVersionWrittenSinceItsRead: a row that
// uploads over a document whose tree is outside the Proof schema - which renders no markdown and
// has no token for an upload to name - is held to the version its read found. Another person's
// version, written once the row's read is done and before the upload's route takes the owner's
// lock, is not written over: the upload is refused, 409 DOCUMENT_CHANGED, the row fails so, and the
// document holds that person's text.
func TestAnUploadRowOverADocumentWithNoTokenWritesNothingOverAVersionWrittenSinceItsRead(t *testing.T) {
	t.Parallel()
	rig := newGoingPlanRig(t)
	issue := rig.issue(t, "Holds a broken document")
	broken := createCorruptedDocument(t, rig.mux, rig.database, rig.served.deps, issue, "broken.md", outsideSchemaCorruptions[0].Corrupt)
	const words = "repair broken.md on Holds a broken document"
	rig.loop.script.set(words, func(n int) agenttest.Reply {
		if n == 1 {
			return proposing(goingChange("dispatch_artifact", map[string]any{
				"issue": issue, "name": "broken.md", "content": "# Repaired\n\nBy the plan.\n", "force": true,
			}))
		}
		return answered("Repaired, or not.")
	})
	run := rig.begin(t, goingLoopRun{login: "alice", words: words})
	rig.waiting(t, run)
	// Bob repairs the document as the row's upload reaches its route: after the row's read.
	var bobs atomic.Bool
	rig.hooks.set(t, http.MethodPost, "/api/v1/issues/"+issue+"/artifacts", func(w http.ResponseWriter, r *http.Request, inner http.Handler) {
		if bobs.CompareAndSwap(false, true) {
			rig.fixture(t, "bob", http.MethodPost, "/api/v1/issues/"+issue+"/artifacts", map[string]any{"name": "broken.md", "content": "# Repaired\n\nBy Bob.\n"})
		}
		inner.ServeHTTP(w, r)
	})
	rig.answer(t, run, "approve", map[string]any{"plan": 1})
	if record := rig.ended(t, run, time.Minute); record.state != "done" {
		t.Fatalf("the run ended %s with %q, want done; its calls were told %q", record.state, record.reason, rig.toolResults(t, run))
	}
	if !bobs.Load() {
		t.Fatal("the row's upload never reached its route, so Bob wrote no version")
	}
	if text := rig.documentText(t, broken.ID); !strings.Contains(text, "By Bob.") {
		t.Fatalf("broken.md reads %q, want Bob's version kept: the row's upload wrote over it", text)
	}
	if row := rig.plan(t, run).Rows[0]; row.State != "failed" || row.Result == nil || row.Result.Status != http.StatusConflict || row.Result.Code != "DOCUMENT_CHANGED" {
		t.Fatalf("the row is %s with %+v, want it failed, 409 DOCUMENT_CHANGED", row.State, row.Result)
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
	t.Parallel()
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
	t.Parallel()
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
	goingNamesNone(t, "the row", row.Result.Message, "POST", "/api/v1/", "ISSUE_CLOSED", "409")
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

// goingNamesNone fails t where text names one of tokens as a whole token: a token that begins or
// ends with a letter, a digit or an underscore is not found inside a longer run of them, so the
// digits of a time the words carry, 08:37:52.516409Z, never name 409, while a status code the
// words give still does. A token that ends with an underscore, dispatch_, names each word it
// begins.
func goingNamesNone(t *testing.T, what, text string, tokens ...string) {
	t.Helper()
	word := func(b byte) bool {
		return b == '_' || '0' <= b && b <= '9' || 'a' <= b && b <= 'z' || 'A' <= b && b <= 'Z'
	}
	for _, token := range tokens {
		pattern := regexp.QuoteMeta(token)
		if word(token[0]) {
			pattern = `\b` + pattern
		}
		if last := token[len(token)-1]; word(last) && last != '_' {
			pattern += `\b`
		}
		if regexp.MustCompile(pattern).MatchString(text) {
			t.Errorf("%s says %q, which names %q", what, text, token)
		}
	}
}

// TestAnUntickedRowsLandedReasonStaysTheRowsToUndo: a reasoned close whose reason landed and that
// waits for its person again (partlyLandedWaiting) is unticked: Undo of the run answers the row's
// line for the reason that landed, sent, not undone, and no line of a call that names no row.
func TestAnUntickedRowsLandedReasonStaysTheRowsToUndo(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
	rig := newGoingPlanRig(t)
	run, issue, _ := rig.partlyLandedWaiting(t, "Undone before its next try")
	rig.answer(t, run, "approve", map[string]any{"plan": 1})
	goingReasonOnlyLine(t, rig.undone(t, run, "dispatch_issue_update")[0], issue)
}

// TestAFailedChangeSkipsWhatNeedsIt: a row whose write its route refuses fails, the row that needs
// it is skipped and says so, and a row that needs neither runs.
func TestAFailedChangeSkipsWhatNeedsIt(t *testing.T) {
	t.Parallel()
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

// TestAChangeNamesTheIssueAnEarlierCreateMakes: a change names the issue a create of its plan
// makes as "$N", in an argument that takes an issue key. A split - a new issue, and a move of a
// sub-issue under it that needs the create - is kept, approved and run, and the move lands under
// the new issue's key, while the plan keeps the "$0" the person approved; Undo puts the old parent
// back and closes the new issue. A move whose create failed is skipped, and no write of it is
// sent; so is a move whose create found an issue Dispatch already held, whose sub-issue keeps its
// parent. A text that reads as "$N" is no new issue, and is kept and sent as written. A "$N" a
// change may not hold is refused when the plan is made, and the model is told which change, which
// argument and which value; an approval whose edit puts one in is refused 400 GOING_PLAN_VALUES,
// in words that name the row and each new issue as the page does, never by number.
func TestAChangeNamesTheIssueAnEarlierCreateMakes(t *testing.T) {
	t.Parallel()
	rig := newGoingPlanRig(t)
	// split is an epic with a sub-issue, and the create and the move under the new issue that
	// split it.
	split := func(t *testing.T, title string) (epic, child string, changes []map[string]any) {
		t.Helper()
		epic = rig.issue(t, "Epic of "+title)
		child = decodeBody[model.Issue](t, rig.fixture(t, "alice", http.MethodPost, "/api/v1/issues", map[string]any{
			"project": "DEMO", "title": "Sub-issue of " + title, "parent": epic, "force": true,
		})).Key
		return epic, child, []map[string]any{
			goingChange("dispatch_issue", map[string]any{"project": "DEMO", "title": title, "parent": epic, "force": true}),
			goingChange("dispatch_issue_update", map[string]any{"issue": child, "parent": "$0"}, 0),
		}
	}
	parent := func(t *testing.T, issue string) string {
		t.Helper()
		if held := rig.issueRead(t, issue).Parent; held != nil {
			return *held
		}
		return ""
	}

	t.Run("a move under a new issue lands under its key, and Undo puts the old parent back", func(t *testing.T) {
		epic, child, changes := split(t, "CSV export")
		run := rig.ran(t, "split the epic of CSV export", changes...)
		var created string
		if err := rig.database.Pool.QueryRow(context.Background(), `select key from issues where title = 'CSV export'`).Scan(&created); err != nil {
			t.Fatalf("read the issue the run made: %v", err)
		}
		plan := rig.plan(t, run)
		if states := rowStates(plan); !slices.Equal(states, []string{"done", "done"}) {
			t.Fatalf("the rows are %v, want both done; the calls were told %q", states, rig.toolResults(t, run))
		}
		if moved := parent(t, child); moved != created {
			t.Fatalf("the sub-issue's parent is %q, want %s, the issue the plan made", moved, created)
		}
		if arguments := string(plan.Rows[1].Arguments); !strings.Contains(arguments, `"$0"`) {
			t.Errorf("the move's arguments read %s, want the \"$0\" the person approved", arguments)
		}
		lines := rig.undone(t, run, "dispatch_issue_update", "dispatch_issue")
		if !lines[0].Outcome.OK || !lines[1].Outcome.OK {
			t.Fatalf("Undo answered %+v, want the move and the create undone", lines)
		}
		if back := parent(t, child); back != epic || rig.issueRead(t, created).Status != "done" {
			t.Fatalf("after Undo the sub-issue's parent is %q and %s is %s, want %s back and the new issue closed",
				back, created, rig.issueRead(t, created).Status, epic)
		}
	})

	t.Run("a move whose create failed is skipped, and nothing of it is sent", func(t *testing.T) {
		epic, child, changes := split(t, "PDF export")
		const words = "split the epic of PDF export"
		rig.loop.script.set(words, func(n int) agenttest.Reply {
			if n == 1 {
				return proposing(changes...)
			}
			return answered("The create failed.")
		})
		rig.hooks.set(t, http.MethodPost, "/api/v1/issues", func(w http.ResponseWriter, _ *http.Request, _ http.Handler) {
			writeError(w, "CREATE_REFUSED", http.StatusConflict, "the create is refused for this test")
		})
		var sent atomic.Int32
		rig.hooks.set(t, http.MethodPatch, "/api/v1/issues/"+child, func(w http.ResponseWriter, r *http.Request, inner http.Handler) {
			sent.Add(1)
			inner.ServeHTTP(w, r)
		})
		run := rig.begin(t, goingLoopRun{login: "alice", words: words})
		rig.waiting(t, run)
		rig.answer(t, run, "approve", map[string]any{"plan": 1})
		if record := rig.ended(t, run, time.Minute); record.state != "done" {
			t.Fatalf("the run ended %s with %q, want done", record.state, record.reason)
		}
		plan := rig.plan(t, run)
		if states := rowStates(plan); !slices.Equal(states, []string{"failed", "skipped"}) {
			t.Fatalf("the rows are %v, want failed, skipped", states)
		}
		if skipped := plan.Rows[1].Result; skipped == nil || skipped.Code != agentwire.CodeRowSkipped || !strings.Contains(skipped.Message, "issue PDF export") {
			t.Errorf("the move's result is %+v, want ROW_SKIPPED naming the create it needs, issue PDF export", skipped)
		}
		if sent.Load() != 0 || parent(t, child) != epic {
			t.Errorf("the move sent %d writes and the sub-issue's parent is %q, want none sent and %s kept", sent.Load(), parent(t, child), epic)
		}
	})

	for _, found := range []struct {
		name, external, title string
		// landed makes the create's request record a landing beside the issue it found. The route
		// commits nothing for such an issue, so its request records none; a write of the request's
		// own after the route answered makes it record one, as a route that committed beside its 200
		// would.
		landed bool
	}{
		{name: "as its route answered it", external: "acme/held#1", title: "Held upstream"},
		{name: "as its landing recorded it", external: "acme/held#2", title: "Held and landed", landed: true},
	} {
		t.Run("a move under a create that found an issue Dispatch already held, "+found.name+", is skipped, and its parent kept", func(t *testing.T) {
			held := decodeBody[model.Issue](t, rig.fixture(t, "alice", http.MethodPost, "/api/v1/issues", map[string]any{
				"project": "DEMO", "title": "Tracked upstream as " + found.external, "external": found.external, "force": true,
			})).Key
			epic, child, _ := split(t, found.title)
			var sent atomic.Int32
			rig.hooks.set(t, http.MethodPatch, "/api/v1/issues/"+child, func(w http.ResponseWriter, r *http.Request, inner http.Handler) {
				sent.Add(1)
				inner.ServeHTTP(w, r)
			})
			if found.landed {
				rig.hooks.set(t, http.MethodPost, "/api/v1/issues", func(w http.ResponseWriter, r *http.Request, inner http.Handler) {
					inner.ServeHTTP(w, r)
					tx, err := rig.database.Pool.Begin(r.Context())
					if err != nil {
						t.Errorf("begin a write of the create's request: %v", err)
						return
					}
					defer func() { _ = tx.Rollback(r.Context()) }()
					for _, statement := range []string{`create temp table held_scratch (n int) on commit drop`, `insert into held_scratch values (1)`} {
						if _, err := tx.Exec(r.Context(), statement); err != nil {
							t.Errorf("%s: %v", statement, err)
							return
						}
					}
					if err := tx.Commit(r.Context()); err != nil {
						t.Errorf("commit a write of the create's request: %v", err)
					}
				})
			}
			run := rig.ran(t, "split the epic of "+found.title,
				goingChange("dispatch_issue", map[string]any{"project": "DEMO", "title": found.title, "external": found.external, "parent": epic, "force": true}),
				goingChange("dispatch_issue_update", map[string]any{"issue": child, "parent": "$0"}, 0))
			plan := rig.plan(t, run)
			if states := rowStates(plan); !slices.Equal(states, []string{"done", "skipped"}) {
				t.Fatalf("the rows are %v, want the create done and the move skipped; the calls were told %q", states, rig.toolResults(t, run))
			}
			landed := rig.count(t, `select count(*) from going_landings where run_id = $1::uuid and path = '/api/v1/issues' and answer->>'status' = '200'`, run) == 1
			if landed != found.landed {
				t.Fatalf("the create's request recorded a landing answered 200: %v, want %v", landed, found.landed)
			}
			if skipped := plan.Rows[1].Result; skipped == nil || skipped.Code != agentwire.CodeRowSkipped ||
				!strings.Contains(skipped.Message, "issue "+found.title) || !strings.Contains(skipped.Message, held) {
				t.Errorf("the move's result is %+v, want ROW_SKIPPED naming the create, issue %s, and %s, the issue it found", skipped, found.title, held)
			}
			if sent.Load() != 0 || parent(t, child) != epic {
				t.Errorf("the move sent %d writes and the sub-issue's parent is %q, want none sent and %s kept", sent.Load(), parent(t, child), epic)
			}
			if made := rig.count(t, `select count(*) from issues where title = $1`, found.title); made != 0 {
				t.Errorf("the create made %d issues titled %s, want none: Dispatch held %s for its external issue", made, found.title, held)
			}
		})
	}

	t.Run("a text that reads as \"$N\" is kept and sent as written", func(t *testing.T) {
		epic, child, _ := split(t, "Priced")
		run := rig.ran(t, "split the epic of Priced, priced",
			goingChange("dispatch_issue", map[string]any{"project": "DEMO", "title": "$100", "parent": epic, "force": true}),
			goingChange("dispatch_issue_update", map[string]any{"issue": child, "title": "$0", "parent": "$0"}, 0))
		var created string
		if err := rig.database.Pool.QueryRow(context.Background(), `select key from issues where title = $1`, "$100").Scan(&created); err != nil {
			t.Fatalf("read the issue titled $100 the run made: %v; the calls were told %q", err, rig.toolResults(t, run))
		}
		if states := rowStates(rig.plan(t, run)); !slices.Equal(states, []string{"done", "done"}) {
			t.Fatalf("the rows are %v, want both done; the calls were told %q", states, rig.toolResults(t, run))
		}
		if moved := rig.issueRead(t, child); moved.Title != "$0" || parent(t, child) != created {
			t.Errorf("the sub-issue is titled %q under %q, want its title $0 as written and its parent %s, the issue the plan made",
				moved.Title, parent(t, child), created)
		}
	})

	t.Run("an approval whose edit names a new issue the move may not name is refused", func(t *testing.T) {
		epic, child, _ := split(t, "Edited halves")
		const words = "split the epic of Edited halves in two"
		rig.loop.script.set(words, func(n int) agenttest.Reply {
			if n == 1 {
				return proposing(
					goingChange("dispatch_issue", map[string]any{"project": "DEMO", "title": "First edited half", "parent": epic, "force": true}),
					goingChange("dispatch_issue", map[string]any{"project": "DEMO", "title": "Second edited half", "parent": epic, "force": true}),
					goingChange("dispatch_issue_update", map[string]any{"issue": child, "parent": "$0"}, 0))
			}
			return answered("Split.")
		})
		run := rig.begin(t, goingLoopRun{login: "alice", words: words})
		rig.waiting(t, run)
		for _, edit := range []struct {
			parent string
			// title is the person's edit of the first new issue's title in the same approval, ""
			// for none.
			title string
			// shown are the new issues the words must name, as the page shows them.
			shown []string
		}{
			{parent: "$5", shown: []string{"First edited half (new)"}},
			{parent: "$1", title: "First renamed half", shown: []string{"Second edited half (new)", "First renamed half (new)"}},
		} {
			rows := []map[string]any{{"row": 2, "tick": true, "values": map[string]any{"parent": edit.parent}}}
			if edit.title != "" {
				rows = append([]map[string]any{{"row": 0, "tick": true, "values": map[string]any{"title": edit.title}}}, rows...)
			}
			refused := dispatchRequest(t, rig.mux, http.MethodPost, "/api/v1/going/runs/"+run+"/plan/approve", map[string]any{"plan": 1, "rows": rows}, "alice")
			said := decodeBody[errorBody](t, refused)
			facts := append([]string{"issue update " + child + ": ", `"parent"`, strconv.Quote(edit.parent)}, edit.shown...)
			unnamed := slices.DeleteFunc(facts, func(fact string) bool { return strings.Contains(said.Error, fact) })
			if refused.Code != http.StatusBadRequest || said.Code != codeGoingPlanValues || len(unnamed) > 0 ||
				goingRowNumbered.MatchString(said.Error) || goingChangeNumbered.MatchString(said.Error) || strings.Contains(said.Error, "needs") {
				t.Errorf("an approval setting parent to %s answered %d %+v, want 400 %s naming the row, the argument, the value and %v as the page does, "+
					"no row or change by number, and no needs, which an approval cannot send; it does not name %v",
					edit.parent, refused.Code, said, codeGoingPlanValues, edit.shown, unnamed)
			}
		}
		if plan := rig.plan(t, run); plan.State != "waiting" || !slices.Equal(rowStates(plan), []string{"waiting", "waiting", "waiting"}) {
			t.Fatalf("after the refusals the plan is %s with rows %v, want it waiting whole", plan.State, rowStates(plan))
		}
	})

	for _, refused := range []struct {
		name    string
		changes func(epic, child string) []map[string]any
		// facts are what the model must be told: the change refused, the argument and its value,
		// and the change it names.
		facts []string
	}{
		{
			name: "a new issue the move does not need",
			changes: func(epic, child string) []map[string]any {
				return []map[string]any{
					goingChange("dispatch_issue", map[string]any{"project": "DEMO", "title": "First half", "parent": epic, "force": true}),
					goingChange("dispatch_issue", map[string]any{"project": "DEMO", "title": "Second half", "parent": epic, "force": true}),
					goingChange("dispatch_issue_update", map[string]any{"issue": child, "parent": "$1"}, 0),
				}
			},
			facts: []string{"change 2 (dispatch_issue_update)", `"parent"`, `"$1"`, "change 1", "needs"},
		},
		{
			name: "a change that creates no issue",
			changes: func(epic, child string) []map[string]any {
				return []map[string]any{
					goingChange("dispatch_issue_update", retitle(epic, "Renamed epic")),
					goingChange("dispatch_issue_update", map[string]any{"issue": child, "parent": "$0"}, 0),
				}
			},
			facts: []string{"change 1 (dispatch_issue_update)", `"parent"`, `"$0"`, "change 0", "creates no issue"},
		},
		{
			name: "a change the plan does not hold",
			changes: func(epic, child string) []map[string]any {
				return []map[string]any{
					goingChange("dispatch_issue", map[string]any{"project": "DEMO", "title": "Only half", "parent": epic, "force": true}),
					goingChange("dispatch_issue_update", map[string]any{"issue": child, "parent": "$5"}, 0),
				}
			},
			facts: []string{"change 1 (dispatch_issue_update)", `"parent"`, `"$5"`, "change 5"},
		},
		{
			name: "a new issue as the item a change changes",
			changes: func(epic, _ string) []map[string]any {
				return []map[string]any{
					goingChange("dispatch_issue", map[string]any{"project": "DEMO", "title": "Renamed at once", "parent": epic, "force": true}),
					goingChange("dispatch_issue_update", retitle("$0", "Renamed new issue"), 0),
				}
			},
			facts: []string{"change 1 (dispatch_issue_update)", `"issue"`, `"$0"`, "item"},
		},
		{
			name: "a new issue inside an object",
			changes: func(epic, child string) []map[string]any {
				return []map[string]any{
					goingChange("dispatch_issue", map[string]any{"project": "DEMO", "title": "Wrapped half", "parent": epic, "force": true}),
					goingChange("dispatch_issue_update", map[string]any{"issue": child, "parent": map[string]any{"key": "$0"}}, 0),
				}
			},
			facts: []string{"change 1 (dispatch_issue_update)", `"parent"`, `"$0"`, "inside"},
		},
		{
			name: "a new issue in a list within a list",
			changes: func(epic, child string) []map[string]any {
				return []map[string]any{
					goingChange("dispatch_issue", map[string]any{"project": "DEMO", "title": "Nested blocker", "parent": epic, "force": true}),
					goingChange("dispatch_issue_update", map[string]any{"issue": child, "blocked_by": []any{[]any{"$0"}}}, 0),
				}
			},
			facts: []string{"change 1 (dispatch_issue_update)", `"blocked_by"`, `"$0"`, "inside"},
		},
	} {
		t.Run("a plan naming "+refused.name+" is refused", func(t *testing.T) {
			epic, child, _ := split(t, refused.name)
			words := "split the epic of " + refused.name
			rig.loop.script.set(words, func(n int) agenttest.Reply {
				if n == 1 {
					return proposing(refused.changes(epic, child)...)
				}
				return answered("The plan was refused.")
			})
			run := rig.begin(t, goingLoopRun{login: "alice", words: words})
			if record := rig.ended(t, run, time.Minute); record.state != "done" {
				t.Fatalf("the run ended %s with %q, want done with the plan refused", record.state, record.reason)
			}
			told := rig.toolResults(t, run)
			if len(told) != 1 || !strings.Contains(told[0], "the call was refused ("+agentwire.CodeCallRefused+")") {
				t.Fatalf("the model was told %q, want CALL_REFUSED", told)
			}
			for _, fact := range refused.facts {
				if !strings.Contains(told[0], fact) {
					t.Errorf("the model was told %q, which does not name %s", told[0], fact)
				}
			}
			if plans := rig.count(t, `select count(*) from going_plans where run_id = $1::uuid`, run); plans != 0 {
				t.Errorf("the run kept %d plans, want none", plans)
			}
		})
	}
}

// TestARowWhoseItemChangedWaitsAgain: a value another person changed after the approval and
// before the row's write makes the row wait again, showing the new value, while the other rows
// run; the person's approval of it again runs it. And where two rows of a plan write one item,
// another writer's change right after the first lands and before the second runs makes the second
// wait again, though the plan's own first write changed the item too.
func TestARowWhoseItemChangedWaitsAgain(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
	rig := newGoingPlanRig(t)
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
		goingNamesNone(t, "the row", row.Result.Message, "dispatch_", "409", "ISSUE_CLOSED")
	})
	t.Run("an undo line of a write whose record holds nothing of what it left", func(t *testing.T) {
		issue := rig.issue(t, "Renamed unrecorded")
		run := rig.ran(t, "rename Renamed unrecorded", goingChange("dispatch_issue_update", retitle(issue, "Renamed")))
		rig.exec(t, `update going_landings set answer = answer - 'body' where run_id = $1::uuid and phase = 'do'`, run)
		line := rig.undone(t, run, "dispatch_issue_update")[0]
		if line.Outcome.OK || line.Outcome.Code != codeGoingUndoAfterUnread || rig.title(t, issue) != "Renamed" {
			t.Fatalf("Undo answered %+v and the title is %q, want nothing put back, %s", line.Outcome, rig.title(t, issue), codeGoingUndoAfterUnread)
		}
		goingNamesNone(t, "the undo line", line.Outcome.Message, "dispatch_", "PATCH", "/api/v1/")
	})
}

// TestThePlanStates: the states of a plan and of its rows, each reached through the routes and
// the agent - shown (waiting), approved, running, waiting again, done and rejected - and each
// event: approve, untick (which unticks each row that needs it), edit, revise, item changed and
// row failed.
func TestThePlanStates(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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

// TestAReopenBesideOtherFieldsIsProposedAsTwoChanges: change-16 of the eval proposed one row that
// reopened the done OPS-7 and set its priority, and the row failed 409 ISSUE_CLOSED when it ran:
// the issue route takes a closed issue's status alone, with its rank, its components and links it
// already holds, and refuses every other field beside it. A person on the issue page reopens the
// issue first, then changes its fields. So the plan call refuses such a row before any of the plan
// is kept, telling the model to propose the reopen alone and the other fields as a second change
// that needs it; a reopen of its status alone, the same row on an open issue, and a reopen beside
// the links the closed issue holds are kept, and a reopen beside a link it lacks is refused as the
// priority is. The two changes, approved, both land: the issue is open again with its new priority.
func TestAReopenBesideOtherFieldsIsProposedAsTwoChanges(t *testing.T) {
	t.Parallel()
	rig := newGoingPlanRig(t)
	// propose makes a plan call of a working run of alice's with one change, the update of issue
	// with arguments, and answers the server's refusal, nil where it kept the plan. The run is
	// stopped once its call is answered, so alice's runs that work stay under the 3 one person
	// may have, and the last row's run is claimed.
	propose := func(t *testing.T, arguments map[string]any) *agentwire.Error {
		t.Helper()
		run, token := rig.working(t, "alice", "reopen it and set it to P0")
		defer rig.exec(t, `update going_runs set state = 'stopped', lease_token = null, lease_owner = null, lease_ends_at = null where id = $1::uuid`, run)
		encoded, err := json.Marshal(arguments)
		if err != nil {
			t.Fatal(err)
		}
		_, err = rig.call(agentwire.CallRequest{Run: run, LeaseToken: token, Step: 2, Call: 1, Kind: agentwire.KindPlan,
			Proposal: &agentwire.PlanProposal{Summary: "Reopen it.", Changes: []agentwire.PlanChange{{Tool: "dispatch_issue_update", Arguments: encoded}}}})
		var refused *agentwire.Error
		if errors.As(err, &refused) {
			return refused
		}
		if err != nil {
			t.Fatalf("the plan call: %v", err)
		}
		return nil
	}
	closed := rig.issue(t, "Closed before its reopen")
	rig.fixture(t, "alice", http.MethodPatch, "/api/v1/issues/"+closed, map[string]any{"status": "done"})

	t.Run("a reopen beside a priority on a closed issue is refused, saying to propose two changes", func(t *testing.T) {
		requireRefusalNames(t, propose(t, map[string]any{"issue": closed, "status": "todo", "priority": 0}), "0", closed, "priority")
	})

	t.Run("a reopen of its status alone is kept", func(t *testing.T) {
		if refused := propose(t, map[string]any{"issue": closed, "status": "todo"}); refused != nil {
			t.Fatalf("the plan call answered %+v, want the plan kept", refused)
		}
	})

	t.Run("the same change on an open issue is kept", func(t *testing.T) {
		open := rig.issue(t, "Open all along")
		if refused := propose(t, map[string]any{"issue": open, "status": "todo", "priority": 0}); refused != nil {
			t.Fatalf("the plan call answered %+v, want the plan kept", refused)
		}
	})

	t.Run("a reopen beside a link the closed issue lacks is refused, and beside the links it holds is kept", func(t *testing.T) {
		const held, added = "https://example.com/pull/held", "https://example.com/pull/added"
		linked := rig.issue(t, "Closed with its link")
		rig.fixture(t, "alice", http.MethodPatch, "/api/v1/issues/"+linked, map[string]any{"external_links": []map[string]string{{"url": held}}})
		rig.fixture(t, "alice", http.MethodPatch, "/api/v1/issues/"+linked, map[string]any{"status": "done"})
		requireRefusalNames(t, propose(t, map[string]any{"issue": linked, "status": "todo", "external_links": []any{held, added}}), "0", linked, "external_links")
		if refused := propose(t, map[string]any{"issue": linked, "status": "todo", "external_links": []any{held}}); refused != nil {
			t.Fatalf("the plan call of a reopen beside the link the issue holds answered %+v, want the plan kept", refused)
		}
	})

	t.Run("the reopen, then the priority that needs it, both land", func(t *testing.T) {
		issue := rig.issue(t, "Reopened in two changes")
		rig.fixture(t, "alice", http.MethodPatch, "/api/v1/issues/"+issue, map[string]any{"status": "done"})
		words := "reopen " + issue + " as todo and set it to P0"
		rig.loop.script.set(words, func(n int) agenttest.Reply {
			if n == 1 {
				return proposing(goingChange("dispatch_issue_update", map[string]any{"issue": issue, "status": "todo"}),
					goingChange("dispatch_issue_update", map[string]any{"issue": issue, "priority": 0}, 0))
			}
			return answered("Reopened and set to P0.")
		})
		run := rig.begin(t, goingLoopRun{login: "alice", words: words})
		if states := rowStates(rig.waiting(t, run)); !slices.Equal(states, []string{"waiting", "waiting"}) {
			t.Fatalf("the plan's rows are %v, want the reopen and the priority waiting", states)
		}
		rig.answer(t, run, "approve", map[string]any{"plan": 1})
		if record := rig.ended(t, run, time.Minute); record.state != "done" {
			t.Fatalf("after its approval the run ended %s with %q, want done; its calls were told %q", record.state, record.reason, rig.toolResults(t, run))
		}
		if states := rowStates(rig.plan(t, run)); !slices.Equal(states, []string{"done", "done"}) {
			t.Errorf("after the run the plan's rows are %v, want both done", states)
		}
		if read := rig.issueRead(t, issue); read.Status != "todo" || read.ClosedAt != nil || read.Priority == nil || *read.Priority != 0 {
			t.Errorf("%s is %s (closed at %v) with priority %v, want todo, open, at P0", issue, read.Status, read.ClosedAt, read.Priority)
		}
	})
}

// TestAWritePlacedByItsRefIsTheRowOfTheCallNamingItsPlace: a comment, a suggestion and an edit of a
// document, whose specs let a dispatch:// ref name their place, are read as the place it names
// (goingRefPlace): each such change is kept as the same row - its tool, its arguments and what it
// shows its person - as the change that names that issue, project or document. A ref the row
// cannot show or the route cannot take - the link of a comment, of a block or of a version of a
// document, or one naming another place than the change names besides it - refuses the plan,
// naming the change, and keeps nothing.
func TestAWritePlacedByItsRefIsTheRowOfTheCallNamingItsPlace(t *testing.T) {
	t.Parallel()
	rig := newGoingPlanRig(t)
	issue := decodeBody[model.Issue](t, rig.fixture(t, "alice", http.MethodPost, "/api/v1/issues", map[string]any{
		"project": "DEMO", "title": "Placed by its link", "spec": "# The mill\n\nThe quernstone grinds the oats.\n", "force": true,
	})).Key
	runbook := decodeBody[struct {
		Artifact model.Artifact `json:"artifact"`
	}](t, rig.fixture(t, "alice", http.MethodPost, "/api/v1/projects/DEMO/artifacts", map[string]any{
		"name": "Mill runbook", "content": "# Mill runbook\n\nOil the spindle weekly.\n",
	})).Artifact.Slug
	edit := []any{map[string]any{"op": "insert", "markdown": "Grease the gears monthly.", "after": "end"}}
	// kept is the one row the plan of a change of tool with arguments keeps, failing t where the
	// plan is refused.
	kept := func(t *testing.T, tool string, arguments map[string]any) goingPlanRowStored {
		t.Helper()
		run, refused := rig.proposedPlan(t, goingChange(tool, arguments))
		if refused != nil {
			t.Fatalf("%s %v was refused %+v, want its plan kept", tool, arguments, refused)
		}
		rows := rig.keptRows(t, run)
		if len(rows) != 1 {
			t.Fatalf("%s %v kept %d rows, want 1", tool, arguments, len(rows))
		}
		return rows[0]
	}
	for _, c := range []struct {
		name, tool   string
		byRef, named map[string]any
	}{
		{"a comment on an issue", "dispatch_comment",
			map[string]any{"ref": "dispatch://" + issue, "body": "Reads well."},
			map[string]any{"issue": issue, "body": "Reads well."}},
		{"a comment on a quote of an issue's spec", "dispatch_comment",
			map[string]any{"ref": "dispatch://" + issue + "/spec", "quote": "grinds the oats", "body": "Which oats?"},
			map[string]any{"issue": issue, "artifact": "spec", "quote": "grinds the oats", "body": "Which oats?"}},
		{"a suggestion in an issue's spec", "dispatch_suggest",
			map[string]any{"ref": "dispatch://" + issue + "/spec", "quote": "grinds the oats", "replace_with": "grinds the barley"},
			map[string]any{"issue": issue, "artifact": "spec", "quote": "grinds the oats", "replace_with": "grinds the barley"}},
		{"an edit of a project's document", "dispatch_doc_edit",
			map[string]any{"ref": "dispatch://DEMO/artifact/" + runbook, "ops": edit},
			map[string]any{"project": "DEMO", "artifact": runbook, "ops": edit}},
	} {
		t.Run(c.name, func(t *testing.T) {
			placed, named := kept(t, c.tool, c.byRef), kept(t, c.tool, c.named)
			if placed.tool != named.tool || !goingJSONEqual(placed.arguments, named.arguments) || !placed.shown.unchanged(named.shown) {
				t.Errorf("the change given by its ref is kept as %s %s showing %+v, want the row of the change naming its place, %s %s showing %+v",
					placed.tool, placed.arguments, placed.shown, named.tool, named.arguments, named.shown)
			}
		})
	}

	for _, c := range []struct {
		name, tool string
		arguments  map[string]any
	}{
		{"a comment given the link of a comment", "dispatch_comment",
			map[string]any{"ref": "dispatch://" + issue + "/comment/01234567-0000-4000-8000-000000000001", "body": "Agreed."}},
		{"a suggestion given the link of a block", "dispatch_suggest",
			map[string]any{"ref": "dispatch://" + issue + "/spec#b-intro", "quote": "grinds the oats", "replace_with": "grinds the barley"}},
		{"an edit given the link of a version", "dispatch_doc_edit",
			map[string]any{"ref": "dispatch://DEMO/artifact/" + runbook + "@v1", "ops": edit}},
		{"a comment whose link names another issue than it names", "dispatch_comment",
			map[string]any{"issue": issue, "ref": "dispatch://DEMO-99999", "body": "Reads well."}},
	} {
		t.Run(c.name, func(t *testing.T) {
			run, refused := rig.proposedPlan(t, goingChange(c.tool, c.arguments))
			requireRefusalNames(t, refused, "0", c.tool)
			if kept := rig.count(t, `select count(*) from going_plans where run_id = $1::uuid`, run); kept != 0 {
				t.Fatalf("the refused call kept %d plans, want none", kept)
			}
		})
	}
}

// TestAPlanRowOfAPostShowsItsAuthorAndBodyUntilItIsDeleted: a plan's row of an edit or a delete of
// a comment or an issue message is a row of the post - its item the post, its issue the post's, its
// one field body - whose value now is the post's author and body. Once the author deletes the post
// before the approval, its row reads deleted: its value now is the delete's mark, the author and
// who deleted it when, with no word of the body, the plan holds no word of it, and an approval that
// ticks the row is refused, 409 POST_DELETED, with nothing written - every row still waits, and
// the other post stands (INFRA-1523 R3).
func TestAPlanRowOfAPostShowsItsAuthorAndBodyUntilItIsDeleted(t *testing.T) {
	t.Parallel()
	rig := newGoingPlanRig(t)
	alice := goingHolder{Kind: "user", ID: "alice"}
	for _, c := range goingPostCases {
		t.Run("a "+c.kind, func(t *testing.T) {
			const word = "bollardword"
			edited, issue := c.make(t, rig, "The "+word+" holds the ferry.")
			deleted, _ := c.make(t, rig, "The ferry leaves at noon.")
			words := "correct the two " + c.kind + "s about the ferry"
			rig.loop.script.set(words, func(n int) agenttest.Reply {
				if n == 1 {
					return proposing(c.editing(edited, "The bollard holds the ferry fast."), c.deleting(deleted))
				}
				return answered("Both wait for you.")
			})
			run := rig.begin(t, goingLoopRun{login: "alice", words: words})
			plan := rig.waiting(t, run)
			for index, want := range []struct{ id, body string }{{edited, "The " + word + " holds the ferry."}, {deleted, "The ferry leaves at noon."}} {
				row := plan.Rows[index]
				var now goingPostNow
				if err := json.Unmarshal(row.Now, &now); err != nil || row.Item != want.id || row.Issue == "" || !slices.Equal(row.Fields, []string{"body"}) ||
					now.Author != alice || now.Body == nil || *now.Body != want.body || now.DeletedAt != nil || row.Deleted {
					t.Fatalf("the row of %s reads item %s of %s, fields %v, now %s, deleted %v; want the %s %s, field body, alice's body %q",
						row.Tool, row.Item, row.Issue, row.Fields, row.Now, row.Deleted, c.kind, want.id, want.body)
				}
			}
			if plan.Rows[0].Issue != issue {
				t.Fatalf("the edit's row names the issue %s, want the %s's own %s", plan.Rows[0].Issue, c.kind, issue)
			}

			c.remove(t, rig, edited)
			response := dispatchRequest(t, rig.mux, http.MethodGet, "/api/v1/going/runs/"+run+"/plan", nil, "alice")
			if response.Code != http.StatusOK || strings.Contains(response.Body.String(), word) {
				t.Fatalf("after the delete the plan answered %d %s, want 200 with no word of the deleted %s", response.Code, response.Body.String(), c.kind)
			}
			row := decodeBody[goingPlanAnswer](t, response).Rows[0]
			var now goingPostNow
			if err := json.Unmarshal(row.Now, &now); err != nil || !row.Deleted || row.State != "waiting" || now.Author != alice || now.Body != nil ||
				now.DeletedAt == nil || now.DeletedBy == nil || *now.DeletedBy != alice {
				t.Fatalf("after the delete the edit's row reads %s, now %s, deleted %v; want it waiting, deleted, alice's delete's mark and no body", row.State, row.Now, row.Deleted)
			}

			approval := dispatchRequest(t, rig.mux, http.MethodPost, "/api/v1/going/runs/"+run+"/plan/approve", map[string]any{"plan": 1}, "alice")
			if refusal := decodeBody[refusalAnswer](t, approval); approval.Code != http.StatusConflict || refusal.Code != codePostDeleted || strings.Contains(approval.Body.String(), word) {
				t.Fatalf("the approval of the deleted %s's row answered %d %s, want 409 %s with no word of it", c.kind, approval.Code, approval.Body.String(), codePostDeleted)
			}
			if states := rowStates(rig.plan(t, run)); !slices.Equal(states, []string{"waiting", "waiting"}) {
				t.Fatalf("after the refused approval the plan's rows are %v, want both waiting", states)
			}
			if standing := rig.textOf(t, `select (deleted_at is null)::text from `+c.kind+`s where id = $1::uuid`, deleted); standing != "true" {
				t.Fatalf("after the refused approval the other %s stands %s, want nothing written", c.kind, standing)
			}
		})
	}
}

// TestThePlanCallGivesTheRunNoWordOfAPostItsRowsRead: a plan of an edit and a delete of a comment
// and of an issue message reads each post as its person, whose plan shows the post's author and
// body. The plan call's answer, which the run's agent is given and keeps as a step of the run,
// names each row's post and its field body but carries no value now of it, so no word of a post
// reaches the run through its plan: only through the run's own reads, each of which records the
// post as the run's (recordGoingRunPosts), so that a later delete of it withdraws the run.
func TestThePlanCallGivesTheRunNoWordOfAPostItsRowsRead(t *testing.T) {
	t.Parallel()
	rig := newGoingPlanRig(t)
	run, token := rig.working(t, "alice", "correct the ferry's posts")
	defer rig.exec(t, `update going_runs set state = 'stopped', lease_token = null, lease_owner = null, lease_ends_at = null where id = $1::uuid`, run)
	words := []string{"harbourpierword", "quaypierword", "jettypierword", "wharfpierword"}
	var changes []agentwire.PlanChange
	for _, c := range goingPostCases {
		for _, change := range []func(id string) map[string]any{
			func(id string) map[string]any { return c.editing(id, "The ferry leaves at two.") },
			c.deleting,
		} {
			id, _ := c.make(t, rig, "The "+words[len(changes)]+" stands by the ferry.")
			proposed := change(id)
			encoded, err := json.Marshal(proposed["arguments"])
			if err != nil {
				t.Fatal(err)
			}
			changes = append(changes, agentwire.PlanChange{Tool: proposed["tool"].(string), Arguments: encoded})
		}
	}
	answer, err := rig.call(agentwire.CallRequest{Run: run, LeaseToken: token, Step: 2, Call: 1, Kind: agentwire.KindPlan,
		Proposal: &agentwire.PlanProposal{Summary: "Correct the ferry's posts.", Changes: changes}})
	if err != nil {
		t.Fatalf("the plan call: %v", err)
	}
	var plan agentwire.PlanAnswer
	if err := json.Unmarshal(answer.Body, &plan); err != nil || len(plan.Rows) != len(changes) {
		t.Fatalf("the plan call answered %s (%v), want its %d rows", answer.Body, err, len(changes))
	}
	for index, row := range plan.Rows {
		if row.Tool != changes[index].Tool || row.Item == "" || !slices.Equal(row.Fields, []string{"body"}) || len(row.Now) != 0 {
			t.Errorf("the plan call's row of %s reads item %q, fields %v, now %s; want its post, its field body and no value now",
				row.Tool, row.Item, row.Fields, row.Now)
		}
	}
	shown, err := json.Marshal(rig.plan(t, run))
	if err != nil {
		t.Fatal(err)
	}
	for _, word := range words {
		if strings.Contains(string(answer.Body), word) {
			t.Errorf("the plan call's answer holds %q, a word of a post its rows read: %s", word, answer.Body)
		}
		if !strings.Contains(string(shown), word) {
			t.Errorf("the plan alice reads holds no %q: the control decides nothing", word)
		}
	}
}
