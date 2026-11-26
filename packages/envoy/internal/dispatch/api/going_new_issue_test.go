package api

// going_new_issue_test.go holds the line File as a new issue as its person meets it (R9, the
// server's half of check c10): Enter starts a new-issue run, the agent's real loop, on a fake chat
// model, proposes the issue's create, and the server files it at once (going_new_issue.go).

import (
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/going/agent/agenttest"
	"github.com/sjawhar/envoy/going/agentwire"
)

// TestTheNewIssueLineFilesAtOnce: Enter on the line File as a new issue starts a Fast run that
// files the person's words as one new issue at once, waiting for nobody: the issue holds the title,
// project, parent, priority and spec its agent wrote, the label of the deployment's new issues and,
// where the deployment names a session for them, that session's route, and nothing is posted on it;
// the run's end names the issue and offers Undo. A run whose create is refused, and one whose agent
// proposes no issue, file nothing, and their end says why; a create whose labels are no list is
// refused, not given the deployment's label in their place; and a run its person stops after its
// create landed names the issue in its end and offers Undo.
func TestTheNewIssueLineFilesAtOnce(t *testing.T) {
	t.Parallel()
	rig := newGoingPlanRig(t)
	const session = "0199e2a0-1111-7000-8000-00000000c10a"
	rig.served.newIssueOverride.Store(&newIssueSettings{Label: "from-the-bar", Session: session})
	t.Cleanup(func() { rig.served.newIssueOverride.Store(nil) })

	// start presses Enter on the line with words, on the page of issue page, and answers the run.
	start := func(t *testing.T, words, page string) goingRun {
		t.Helper()
		response := dispatchRequest(t, rig.mux, http.MethodPost, "/api/v1/going/runs", map[string]any{
			"words": words, "kind": agentwire.RunNewIssue,
			"context": map[string]any{"path": "/issues/" + page, "project": "DEMO", "issue": page},
		}, "alice")
		if response.Code != http.StatusCreated {
			t.Fatalf("POST /api/v1/going/runs answered %d %s", response.Code, response.Body.String())
		}
		run := decodeBody[goingRun](t, response)
		if run.Level != goingNewIssueLevel {
			t.Fatalf("the new-issue run is %s, want fast", run.Level)
		}
		return run
	}
	// waited counts the turns of run that ended waiting for its person.
	waited := func(t *testing.T, run string) int {
		t.Helper()
		return rig.count(t, `select count(*) from going_run_events where run_id = $1::uuid and name = $2 and data->>'state' = 'waiting'`,
			run, agentwire.FrameEnd)
	}
	issues := func(t *testing.T) int {
		t.Helper()
		return rig.count(t, `select count(*) from issues`)
	}

	t.Run("Enter files the issue at once with its title, project, parent, priority, spec and route, and posts no message", func(t *testing.T) {
		parent := rig.issue(t, "Board exports")
		const words = "file a P1 bug: the pdf export times out on boards with images"
		const spec = "The PDF export of a board that holds images runs past its time limit and fails.\n"
		rig.loop.script.set(words, func(n int) agenttest.Reply {
			if n == 1 {
				return proposing(goingChange(goingIssueCreateTool, map[string]any{
					"project": "DEMO", "title": "PDF export times out on boards with images", "parent": parent, "priority": 1, "spec": spec,
				}))
			}
			return answered("No further call of this run was scripted.")
		})
		run := start(t, words, parent)
		if record := rig.ended(t, run.ID, time.Minute); record.state != "done" {
			t.Fatalf("the run ended %s with %q, want done", record.state, record.reason)
		}
		frame := rig.endFrame(t, run.ID)
		if frame.Issue == "" || !slices.Contains(frame.Offers, agentwire.OfferUndo) {
			t.Fatalf("the run's end is %+v, want it to name the issue it filed and offer undo", frame)
		}
		filed := rig.issueRead(t, frame.Issue)
		for _, row := range []struct {
			field     string
			got, want any
		}{
			{"title", filed.Title, "PDF export times out on boards with images"},
			{"project", filed.Project, "DEMO"},
			{"parent", deref(filed.Parent), parent},
			{"priority", goingNumberOf(filed.Priority), 1},
			{"route", deref(filed.Route), "session:" + session},
			{"label", slices.Contains(filed.Labels, "from-the-bar"), true},
			{"spec", strings.Contains(documentMarkdown(t, rig.mux, filed.PrimaryArtifactID), strings.TrimSpace(spec)), true},
		} {
			if row.got != row.want {
				t.Errorf("the filed issue's %s is %v, want %v", row.field, row.got, row.want)
			}
		}
		if messages := rig.count(t, `select count(*) from messages where issue_key = $1`, frame.Issue); messages != 0 {
			t.Errorf("the filed issue holds %d messages, want none", messages)
		}
		if turns := waited(t, run.ID); turns != 0 {
			t.Errorf("the run waited for its person %d times, want never", turns)
		}
		if calls := len(rig.loop.fake.Asking(words)); calls != 1 {
			t.Errorf("the run made %d model calls, want the one that proposed the issue", calls)
		}
		if plan := rig.plan(t, run.ID); len(plan.Rows) != 1 || plan.Rows[0].Class != string(goingClassCreateText) || plan.Rows[0].State != "done" {
			t.Errorf("the run's plan is %+v, want its one create with a spec, done", plan.Rows)
		}
	})

	t.Run("with no session named for new issues, the issue takes no route", func(t *testing.T) {
		rig.served.newIssueOverride.Store(&newIssueSettings{})
		t.Cleanup(func() { rig.served.newIssueOverride.Store(&newIssueSettings{Label: "from-the-bar", Session: session}) })
		const words = "new issue: show the upload size limit next to the button"
		rig.loop.script.set(words, func(int) agenttest.Reply {
			return proposing(goingChange(goingIssueCreateTool, map[string]any{"project": "DEMO", "title": "Show the upload size limit",
				"spec": "The upload button shows the largest file it takes.\n"}))
		})
		run := start(t, words, rig.issue(t, "Uploads"))
		if record := rig.ended(t, run.ID, time.Minute); record.state != "done" {
			t.Fatalf("the run ended %s with %q, want done", record.state, record.reason)
		}
		if filed := rig.issueRead(t, rig.endFrame(t, run.ID).Issue); filed.Route != nil || slices.Contains(filed.Labels, "from-the-bar") {
			t.Errorf("the issue filed with no settings has the route %v and the labels %v, want neither", deref(filed.Route), filed.Labels)
		}
	})

	t.Run("a run whose create is refused files nothing and says why", func(t *testing.T) {
		// The parent names no issue: nothing reads a parent the change writes until its create
		// runs, and the create's route refuses it then.
		const missing = "DEMO-9999"
		const words = "file a bug under DEMO-9999: the export button is grey"
		rig.loop.script.set(words, func(int) agenttest.Reply {
			return proposing(goingChange(goingIssueCreateTool, map[string]any{"project": "DEMO", "title": "The export button is grey", "parent": missing,
				"spec": "The export button shows grey, as if it were off.\n"}))
		})
		page := rig.issue(t, "Export buttons")
		before := issues(t)
		run := start(t, words, page)
		record := rig.ended(t, run.ID, time.Minute)
		if record.state != "failed" || !strings.HasPrefix(record.reason, goingNewIssueNotFiled) || !strings.Contains(record.reason, "parent issue "+missing+" not found") {
			t.Fatalf("the run ended %s with %q, want failed with %s naming the create's refusal", record.state, record.reason, goingNewIssueNotFiled)
		}
		if frame := rig.endFrame(t, run.ID); frame.State != "failed" || frame.Reason != record.reason || frame.Issue != "" || slices.Contains(frame.Offers, agentwire.OfferUndo) {
			t.Errorf("the run's end is %+v, want it failed with its reason, naming no issue and offering no undo", frame)
		}
		if after := issues(t); after != before {
			t.Errorf("Dispatch holds %d issues after the run, %d before, want none filed", after, before)
		}
	})

	t.Run("a run whose agent proposes no issue files nothing and says why", func(t *testing.T) {
		const words = "file whatever you think fits"
		rig.loop.script.set(words, func(int) agenttest.Reply { return answered("Which project is it for?") })
		page := rig.issue(t, "Vague")
		before := issues(t)
		run := start(t, words, page)
		if record := rig.ended(t, run.ID, time.Minute); record.state != "failed" || record.reason != goingNewIssueNotFiled+": the run proposed no issue to file" {
			t.Fatalf("the run ended %s with %q, want failed saying it proposed no issue", record.state, record.reason)
		}
		if after := issues(t); after != before {
			t.Errorf("Dispatch holds %d issues after the run, %d before, want none filed", after, before)
		}
	})

	t.Run("a create whose labels are no list is refused, not given the deployment's label in their place", func(t *testing.T) {
		const words = "file a bug labelled bug: the export button is grey"
		rig.loop.script.set(words, func(n int) agenttest.Reply {
			if n == 1 {
				return proposing(goingChange(goingIssueCreateTool, map[string]any{"project": "DEMO", "title": "The export button is grey", "labels": "bug",
					"spec": "The export button shows grey, as if it were off.\n"}))
			}
			return answered("The create was refused.")
		})
		page := rig.issue(t, "Exports")
		before := issues(t)
		run := start(t, words, page)
		rig.ended(t, run.ID, time.Minute)
		asked := rig.loop.fake.Asking(words)
		if len(asked) < 2 || !slices.ContainsFunc(asked[1].ToolResults(), func(result string) bool {
			return strings.Contains(result, "labels") && strings.Contains(result, "must be an array")
		}) {
			t.Errorf("the model was not told its labels must be an array: it made %d calls", len(asked))
		}
		if after := issues(t); after != before {
			t.Errorf("Dispatch holds %d issues after the run, %d before, want none filed", after, before)
		}
	})

	t.Run("a run stopped after its create landed names the issue it filed and offers undo", func(t *testing.T) {
		const words = "file a bug: the board's legend covers its first column"
		release := make(chan struct{})
		t.Cleanup(func() { close(release) })
		rig.loop.script.set(words, func(n int) agenttest.Reply {
			if n == 1 {
				return proposing(goingChange(goingIssueCreateTool, map[string]any{"project": "DEMO", "title": "The board's legend covers its first column",
					"spec": "The legend of a board hides the cards of its first column.\n"}))
			}
			// Held until the test ends, so the run still works when its person stops it.
			return agenttest.Reply{Hold: release, Text: "Filed."}
		})
		page := rig.issue(t, "Boards")
		before := issues(t)
		run := start(t, words, page)
		goingEventually(t, "the run's create to land", func() bool { return issues(t) > before })
		if response := dispatchRequest(t, rig.mux, http.MethodPost, "/api/v1/going/runs/"+run.ID+"/stop", nil, "alice"); response.Code != http.StatusOK {
			t.Fatalf("stop the run answered %d %s", response.Code, response.Body.String())
		}
		frame := rig.endFrame(t, run.ID)
		if frame.State != agentwire.StateStopped || frame.Issue == "" || !slices.Contains(frame.Offers, agentwire.OfferUndo) {
			t.Fatalf("the stopped run's end is %+v, want it stopped, naming the issue it filed and offering undo", frame)
		}
		if filed := rig.issueRead(t, frame.Issue); filed.Title != "The board's legend covers its first column" {
			t.Errorf("the end names %s, titled %q, want the issue the run filed", frame.Issue, filed.Title)
		}
	})

	t.Run("a new-issue run at a level but Fast is refused", func(t *testing.T) {
		response := dispatchRequest(t, rig.mux, http.MethodPost, "/api/v1/going/runs",
			map[string]any{"words": "file it", "kind": agentwire.RunNewIssue, "level": "deep"}, "alice")
		if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), codeGoingInput) {
			t.Errorf("a deep new-issue run answered %d %s, want 400 %s", response.Code, response.Body.String(), codeGoingInput)
		}
	})
}

// goingNumberOf is a number a pointer holds, -1 for none.
func goingNumberOf(number *int) int {
	if number == nil {
		return -1
	}
	return *number
}
