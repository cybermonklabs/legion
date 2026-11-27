// going_undo_test.go holds the undo of one command-bar run to R6: every row of the catalogue
// states its undo, and the undo of a run walks its landed rows newest first, as the person who
// pressed it - writing back what the run left unless another writer changed it since, closing
// what it created, rejecting its open suggestions and naming its messages sent, not undone -
// while History lists the run, and sends nothing twice.
package api

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sjawhar/envoy/going/agent/agenttest"
	"github.com/sjawhar/envoy/going/agentwire"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// goingUndoKinds is the undo R6 gives each action of the catalogue: a reverse where Dispatch
// serves one, a write back where the write wrote a value, the close of an issue the run created,
// and not_undone for every other write.
var goingUndoKinds = map[string]goingUndoKind{
	"dispatch_issue":             goingUndoCloseCreated,
	"dispatch_issue_update":      goingUndoWriteBack,
	"dispatch_claim":             goingUndoReverse,
	"dispatch_ask":               goingUndoReverse,
	"dispatch_edit_ask":          goingUndoWriteBack,
	"dispatch_resolve_ask":       goingUndoNotUndone,
	"dispatch_resolve_comment":   goingUndoReverse,
	"dispatch_follow":            goingUndoReverse,
	"dispatch_comment":           goingUndoNotUndone,
	"dispatch_suggest":           goingUndoReverse,
	"dispatch_message":           goingUndoNotUndone,
	"dispatch_send_to_agent":     goingUndoNotUndone,
	"dispatch_doc_edit":          goingUndoWriteBack,
	"dispatch_request_approval":  goingUndoReverse,
	"dispatch_plan_approve":      goingUndoNotUndone,
	"dispatch_result":            goingUndoNotUndone,
	"dispatch_artifact":          goingUndoNotUndone,
	"dispatch_project":           goingUndoNotUndone,
	"dispatch_architecture_sync": goingUndoNotUndone,
	"dispatch_handoff_write":     goingUndoNotUndone,
}

// TestGoingUndoEveryRowStatesItsUndo: every row the builder holds, counted against it, states one
// of the four undos, the one R6 gives its action, the same on every row of one action, and writes
// only through rows the served table classes as writes.
func TestGoingUndoEveryRowStatesItsUndo(t *testing.T) {
	t.Parallel()
	catalogue, err := BuildGoingCatalogue()
	if err != nil {
		t.Fatal(err)
	}
	mutating := mutatingRoutes((&server{}).routes())
	rows, stated := 0, 0
	for _, action := range catalogue.Actions() {
		want, listed := goingUndoKinds[action.Action]
		if !listed {
			t.Errorf("%s: R6 gives it no undo here", action.Action)
		}
		for _, route := range action.Routes {
			rows++
			if route.Undo.Kind == "" {
				t.Errorf("%s %s %s states no undo", action.Action, route.Method, route.Pattern)
				continue
			}
			stated++
			if route.Undo.Kind != want {
				t.Errorf("%s %s %s is undone by %s, want %s", action.Action, route.Method, route.Pattern, route.Undo.Kind, want)
			}
			if (route.Undo.Kind == goingUndoNotUndone) != (len(route.Undo.Routes) == 0) {
				t.Errorf("%s %s %s: a %s naming routes %v", action.Action, route.Method, route.Pattern, route.Undo.Kind, route.Undo.Routes)
			}
			for _, undo := range route.Undo.Routes {
				if !mutating[undo] {
					t.Errorf("%s %s %s is undone through %s, which is no write of the table", action.Action, route.Method, route.Pattern, undo)
				}
			}
		}
	}
	if stated != rows || rows == 0 {
		t.Fatalf("%d of the builder's %d rows state an undo, want every one", stated, rows)
	}
	if len(goingUndoKinds) != len(catalogue.Actions()) {
		t.Fatalf("R6's table names %d actions and the catalogue holds %d", len(goingUndoKinds), len(catalogue.Actions()))
	}
}

// ran is a run of alice's words whose model proposes changes as its plan, which she approves
// whole, answered once it ended done.
func (rig goingPlanRig) ran(t *testing.T, words string, changes ...map[string]any) string {
	t.Helper()
	rig.loop.script.set(words, func(n int) agenttest.Reply {
		if n == 1 {
			return proposing(changes...)
		}
		return answered("Done.")
	})
	run := rig.begin(t, goingLoopRun{login: "alice", words: words})
	rig.waiting(t, run)
	rig.answer(t, run, "approve", map[string]any{"plan": 1})
	if record := rig.ended(t, run, time.Minute); record.state != "done" {
		t.Fatalf("the run ended %s with %q, want done; its calls were told %q", record.state, record.reason, rig.toolResults(t, run))
	}
	return run
}

// undo presses Undo on run as login, and answers the status and, for a 200, the answer.
func (rig goingPlanRig) undo(t *testing.T, login, run string) (int, goingUndoAnswer) {
	t.Helper()
	response := dispatchRequest(t, rig.mux, http.MethodPost, "/api/v1/going/runs/"+run+"/undo", nil, login)
	if response.Code != http.StatusOK {
		return response.Code, goingUndoAnswer{}
	}
	return response.Code, decodeBody[goingUndoAnswer](t, response)
}

// undone presses Undo on run as alice, which must answer one line for each landed row of the run,
// newest first, of the tools want names.
func (rig goingPlanRig) undone(t *testing.T, run string, want ...string) []goingUndoLine {
	t.Helper()
	status, answer := rig.undo(t, "alice", run)
	if status != http.StatusOK {
		t.Fatalf("Undo of %s answered %d, want 200", run, status)
	}
	tools := make([]string, 0, len(answer.Lines))
	for _, line := range answer.Lines {
		tools = append(tools, line.Tool)
	}
	if !slices.Equal(tools, want) {
		t.Fatalf("Undo of %s answered lines of %v (%+v), want %v", run, tools, answer.Lines, want)
	}
	return answer.Lines
}

// aged makes run age old.
func (rig goingPlanRig) aged(t *testing.T, run string, age time.Duration) {
	t.Helper()
	rig.exec(t, `update going_runs set created_at = now() - make_interval(secs => $2) where id = $1::uuid`, run, age.Seconds())
}

// followUp records a follow-up of run in its conversation, age old, done.
func (rig goingPlanRig) followUp(t *testing.T, run string, age time.Duration) {
	t.Helper()
	rig.exec(t, `
		insert into going_runs (id, author_login, words, conversation_id, continues, level, mode, state, shape, created_at)
		select gen_random_uuid(), author_login, 'and then this', conversation_id, id, 'standard', 'ask', 'done', $2, now() - make_interval(secs => $3)
		from going_runs where id = $1::uuid
	`, run, goingRunShape, age.Seconds())
}

// documentText is the markdown the document id holds now.
func (rig goingPlanRig) documentText(t *testing.T, id string) string {
	t.Helper()
	return decodeBody[struct {
		Markdown string `json:"markdown"`
	}](t, rig.fixture(t, "alice", http.MethodGet, "/api/v1/artifacts/"+id+"/text", nil)).Markdown
}

// replaceText is the edit of a document that replaces find with with.
func replaceText(find, with string) []any {
	return []any{map[string]any{"op": "replace", "find": find, "with": with}}
}

// TestUndoOfOneRun (R6): Undo of a run, pressed by its person, walks the rows the run landed newest
// first. A reverse calls each row's reverse route in that order; a write back keeps a value another
// person left since and names them, and writes the rest back; an issue the run created is closed
// and holds no message; a suggestion still open is rejected; a message is named sent, not undone.
// A run is undone from what its routes recorded beside their landings, so a write whose process
// ended after it landed is undone all the same, and a document another person changed since is
// undone by nothing, saying why, whether the undo's read finds the change or the upload's token
// check finds it after that read. Undo is offered while History lists the run's conversation, so a
// run 8 days old in a conversation whose newest run is 1 day old is undone, and one whose
// conversation's newest run is 8 days old answers 404. A second press answers each row whose undo
// landed from that landing, sending nothing twice, and resumes where the first press ended. Two
// presses made at once undo a call once, and the press whose record of the undo finds the other's
// landing answers that landing's line (recordUndo). An undo whose request its route answered as
// done while it committed nothing says it changed nothing, never that it reversed the change, and
// records that as its landing: a second press answers it from there and sends nothing, and the run
// counts the call in no count. A keyed press whose process ended mid-walk, after it recorded a
// call, is finished by a repeat under its key, which walks the run again rather than being told
// the write landed (KEY_COMMITTED).
func TestUndoOfOneRun(t *testing.T) {
	t.Parallel()
	rig := newGoingPlanRig(t)
	ctx := context.Background()

	t.Run("the reverse calls run newest first", func(t *testing.T) {
		first, second := rig.issue(t, "Claimed first"), rig.issue(t, "Claimed second")
		run := rig.ran(t, "claim the two issues",
			goingChange("dispatch_claim", map[string]any{"issue": first}), goingChange("dispatch_claim", map[string]any{"issue": second}))
		var mu sync.Mutex
		var released []string
		for _, issue := range []string{first, second} {
			rig.hooks.set(t, http.MethodDelete, "/api/v1/issues/"+issue+"/claim", func(w http.ResponseWriter, r *http.Request, inner http.Handler) {
				mu.Lock()
				released = append(released, issue)
				mu.Unlock()
				inner.ServeHTTP(w, r)
			})
		}
		lines := rig.undone(t, run, "dispatch_claim", "dispatch_claim")
		if !slices.Equal(released, []string{second, first}) {
			t.Fatalf("Undo released %v, want the newest claim's issue first: %v", released, []string{second, first})
		}
		for _, line := range lines {
			if !line.Outcome.OK || line.Undo != goingUndoReverse {
				t.Fatalf("a claim's undo answered %+v, want it reversed", line)
			}
		}
		if rig.issueRead(t, first).Claim != nil || rig.issueRead(t, second).Claim != nil {
			t.Fatalf("a claim the run took still stands after its Undo")
		}
	})

	t.Run("a later value of another writer is kept and named", func(t *testing.T) {
		issue := rig.issue(t, "Renamed twice")
		before := rig.issueRead(t, issue)
		run := rig.ran(t, "rename Renamed twice and start it",
			goingChange("dispatch_issue_update", map[string]any{"issue": issue, "title": "Ours", "status": "in_progress"}))
		rig.fixture(t, "bob", http.MethodPatch, "/api/v1/issues/"+issue, map[string]any{"title": "Bob's"})
		line := rig.undone(t, run, "dispatch_issue_update")[0]
		if after := rig.issueRead(t, issue); after.Title != "Bob's" || after.Status != before.Status {
			t.Fatalf("after Undo the issue is titled %q and %s, want Bob's title kept and its status %s back", after.Title, after.Status, before.Status)
		}
		fields := map[string]goingUndoField{}
		for _, field := range line.Fields {
			fields[field.Field] = field
		}
		if kept := fields["title"]; kept.OK || !strings.Contains(kept.Message, `"Bob's"`) || !strings.Contains(kept.Message, "user bob") {
			t.Fatalf("the title's undo answered %+v, want it kept, naming Bob's title and bob", kept)
		}
		// The bar words the kept value as the page shows it, so the value and its writer come as
		// data beside the message.
		if kept := fields["title"]; !goingSameJSON(kept.Now, json.RawMessage(`"Bob's"`)) || kept.By == nil || *kept.By != (goingHolder{Kind: "user", ID: "bob"}) {
			t.Fatalf("the title's undo answered now %s by %+v, want Bob's title as JSON and user bob", kept.Now, kept.By)
		}
		if written := fields["status"]; written.Now != nil || written.By != nil {
			t.Fatalf("the status's undo answered now %s by %+v, want neither on a field written back", written.Now, written.By)
		}
		if !fields["status"].OK || line.Outcome.Code != codeGoingUndoChanged || !strings.Contains(line.Outcome.Message, "status") {
			t.Fatalf("the line answered %+v, want %s naming the kept title and the status written back", line, codeGoingUndoChanged)
		}
	})

	t.Run("a created issue is closed and holds no message", func(t *testing.T) {
		run := rig.ran(t, "file Made by the run", goingChange("dispatch_issue", map[string]any{"project": "DEMO", "title": "Made by the run", "force": true,
			"spec": "An issue the run files.\n"}))
		line := rig.undone(t, run, "dispatch_issue")[0]
		if !line.Outcome.OK || line.Undo != goingUndoCloseCreated || rig.issueRead(t, line.Item).Status != "done" {
			t.Fatalf("Undo of the create answered %+v and left %s %s, want it closed", line, line.Item, rig.issueRead(t, line.Item).Status)
		}
		if messages, comments := rig.count(t, `select count(*) from messages where issue_key = $1`, line.Item),
			rig.count(t, `select count(*) from comments where issue_key = $1`, line.Item); messages != 0 || comments != 0 {
			t.Fatalf("the closed issue holds %d messages and %d comments, want none", messages, comments)
		}
	})

	t.Run("an open suggestion is rejected", func(t *testing.T) {
		issue := decodeBody[model.Issue](t, rig.fixture(t, "alice", http.MethodPost, "/api/v1/issues", map[string]any{
			"project": "DEMO", "title": "Suggested on", "spec": "The spec says hello to everyone.\n", "force": true,
		})).Key
		run := rig.ran(t, "suggest hi on Suggested on",
			goingChange("dispatch_suggest", map[string]any{"issue": issue, "artifact": "spec", "quote": "says hello", "replace_with": "says hi"}))
		line := rig.undone(t, run, "dispatch_suggest")[0]
		read := decodeBody[struct {
			Comment struct {
				Suggestion *model.Suggestion `json:"suggestion"`
			} `json:"comment"`
		}](t, rig.fixture(t, "alice", http.MethodGet, "/api/v1/comments/"+line.Item, nil))
		if suggestion := read.Comment.Suggestion; !line.Outcome.OK || suggestion == nil || suggestion.Accepted == nil || *suggestion.Accepted {
			t.Fatalf("Undo of the suggestion answered %+v and left it %+v, want it rejected", line, suggestion)
		}
	})

	t.Run("a message is named sent, not undone", func(t *testing.T) {
		issue := rig.issue(t, "Messaged")
		run := rig.ran(t, "ask on Messaged where we are", goingChange("dispatch_message", map[string]any{"issue": issue, "body": "Where are we?"}))
		line := rig.undone(t, run, "dispatch_message")[0]
		if line.Undo != goingUndoNotUndone || !line.Outcome.OK || line.Outcome.Message != "sent, not undone" {
			t.Fatalf("Undo of the message answered %+v, want it named sent, not undone", line)
		}
		if messages := rig.count(t, `select count(*) from messages where issue_key = $1 and body = 'Where are we?'`, issue); messages != 1 {
			t.Fatalf("the issue holds %d of the run's messages after Undo, want the one sent", messages)
		}
	})

	t.Run("a run whose conversation's newest run is 8 days old answers 404", func(t *testing.T) {
		issue := rig.issue(t, "Long ago")
		run := rig.ran(t, "rename Long ago", goingChange("dispatch_issue_update", retitle(issue, "Renamed long ago")))
		rig.aged(t, run, 9*24*time.Hour)
		rig.followUp(t, run, 8*24*time.Hour)
		if status, _ := rig.undo(t, "alice", run); status != http.StatusNotFound {
			t.Fatalf("Undo of a run whose conversation's newest run is 8 days old answered %d, want 404", status)
		}
		if title := rig.title(t, issue); title != "Renamed long ago" {
			t.Fatalf("the issue is titled %q after the refused Undo, want the run's title kept", title)
		}
	})

	t.Run("a run 8 days old in a conversation whose newest run is 1 day old keeps its Undo", func(t *testing.T) {
		issue := rig.issue(t, "Followed up")
		run := rig.ran(t, "rename Followed up", goingChange("dispatch_issue_update", retitle(issue, "Renamed and followed up")))
		rig.aged(t, run, 8*24*time.Hour)
		rig.followUp(t, run, 24*time.Hour)
		if status, _ := rig.undo(t, "bob", run); status != http.StatusNotFound {
			t.Fatalf("bob's Undo of alice's run answered %d, want 404", status)
		}
		if line := rig.undone(t, run, "dispatch_issue_update")[0]; !line.Outcome.OK || rig.title(t, issue) != "Followed up" {
			t.Fatalf("Undo answered %+v and the issue is titled %q, want the title written back", line, rig.title(t, issue))
		}
	})

	t.Run("a second press after the first landed sends nothing twice", func(t *testing.T) {
		issue := rig.issue(t, "Released once")
		run := rig.ran(t, "claim Released once", goingChange("dispatch_claim", map[string]any{"issue": issue}))
		var released atomic.Int32
		rig.hooks.set(t, http.MethodDelete, "/api/v1/issues/"+issue+"/claim", func(w http.ResponseWriter, r *http.Request, inner http.Handler) {
			if released.Add(1) == 1 {
				// The release commits, and its answer is lost: the route answers 503 in its place.
				inner.ServeHTTP(httptest.NewRecorder(), r)
				writeError(w, "SERVICE_UNAVAILABLE", http.StatusServiceUnavailable, "the release committed and its answer was lost")
				return
			}
			inner.ServeHTTP(w, r)
		})
		first := rig.undone(t, run, "dispatch_claim")
		second := rig.undone(t, run, "dispatch_claim")
		if !first[0].Outcome.OK || !reflect.DeepEqual(first, second) {
			t.Fatalf("the first press answered %+v and the second %+v, want the release landed, answered alike", first, second)
		}
		if sent := released.Load(); sent != 1 || rig.issueRead(t, issue).Claim != nil {
			t.Fatalf("the release was sent %d times, want once, the claim gone", sent)
		}
	})

	t.Run("two presses at once undo the call once, and both answer its undo", func(t *testing.T) {
		issue := rig.issue(t, "Renamed and undone by two presses")
		run := rig.ran(t, "rename Renamed and undone by two presses", goingChange("dispatch_issue_update", retitle(issue, "Renamed by the run")))
		var sent atomic.Int32
		rig.hooks.set(t, http.MethodPatch, "/api/v1/issues/"+issue, func(w http.ResponseWriter, r *http.Request, inner http.Handler) {
			sent.Add(1)
			inner.ServeHTTP(w, r)
		})
		// The press the test makes reads the issue once it found no undo of the call landed. Before
		// that read is served, a second press runs whole: its write back lands and records the
		// call's undo. The first press then reads the title written back, keeps it as a change made
		// since the run, and records that undo, where the second press's landing stands already.
		var pressed atomic.Bool
		var racing *httptest.ResponseRecorder
		rig.hooks.set(t, http.MethodGet, "/api/v1/issues/"+issue, func(w http.ResponseWriter, r *http.Request, inner http.Handler) {
			if pressed.CompareAndSwap(false, true) {
				racing = dispatchRequest(t, rig.mux, http.MethodPost, "/api/v1/going/runs/"+run+"/undo", nil, "alice")
			}
			inner.ServeHTTP(w, r)
		})
		lines := rig.undone(t, run, "dispatch_issue_update")
		if racing == nil || racing.Code != http.StatusOK {
			t.Fatalf("the press made beside the first answered %+v, want 200", racing)
		}
		raced := decodeBody[goingUndoAnswer](t, racing).Lines
		if len(raced) != 1 || !raced[0].Outcome.OK || !reflect.DeepEqual(lines, raced) {
			t.Fatalf("the press whose write back landed answered %+v and the press beside it %+v, want the write back, answered alike", raced, lines)
		}
		if title := rig.title(t, issue); title != "Renamed and undone by two presses" || sent.Load() != 1 {
			t.Fatalf("after the two presses the issue is titled %q with %d write backs sent, want its title back, written once", title, sent.Load())
		}
		if landings := rig.count(t, `select count(*) from going_landings where run_id = $1::uuid and phase = 'undo'`, run); landings != 1 {
			t.Fatalf("the run holds %d undo landings, want the one its write back recorded", landings)
		}
	})

	t.Run("an undo whose request changed nothing says so, once", func(t *testing.T) {
		issue := rig.issue(t, "Released by hand before its undo")
		run := rig.ran(t, "claim Released by hand before its undo", goingChange("dispatch_claim", map[string]any{"issue": issue}))
		release := "/api/v1/issues/" + issue + "/claim"
		var sent atomic.Int32
		rig.hooks.set(t, http.MethodDelete, release, func(w http.ResponseWriter, _ *http.Request, _ http.Handler) {
			// alice releases the claim by hand first, off the bar, so the undo's release finds the
			// issue as it asks, and its route answers as PUT /api/v1/asks/{id}/followers/{session_id}
			// answers a session that follows the ask already: 204, with nothing committed.
			sent.Add(1)
			if response := dispatchRequest(t, rig.mux, http.MethodDelete, release, nil, "alice"); response.Code >= http.StatusMultipleChoices {
				t.Errorf("alice's own release answered %d %s", response.Code, response.Body.String())
			}
			w.WriteHeader(http.StatusNoContent)
		})
		counts := func(t *testing.T) goingUndoCounts {
			t.Helper()
			response := dispatchRequest(t, rig.mux, http.MethodGet, "/api/v1/going/runs/"+run, nil, "alice")
			if response.Code != http.StatusOK {
				t.Fatalf("GET the run %s answered %d, want 200", run, response.Code)
			}
			return goingUndoCountsOf(decodeBody[goingRun](t, response))
		}
		if before := counts(t); before != (goingUndoCounts{undoable: 1}) {
			t.Fatalf("before Undo the run counts %+v, want one change to take back", before)
		}
		first := rig.undone(t, run, "dispatch_claim")
		line := first[0]
		if !line.Outcome.OK || line.Outcome.Status != http.StatusNoContent || line.Outcome.Message != goingUndoChangedNothing {
			t.Fatalf("Undo answered %+v, want it ok, 204, saying the undo changed nothing", line)
		}
		for _, field := range line.Fields {
			if !field.OK || field.Message != goingUndoChangedNothing {
				t.Fatalf("the line's field answered %+v, want it saying the undo changed nothing", field)
			}
		}
		if claim := rig.issueRead(t, issue).Claim; claim != nil {
			t.Fatalf("the issue is held by %+v, want nobody after alice's release", claim)
		}
		second := rig.undone(t, run, "dispatch_claim")
		if !reflect.DeepEqual(first, second) || sent.Load() != 1 {
			t.Errorf("the second press answered %+v with the release sent %d times; want the first press's %+v, sent once", second, sent.Load(), first)
		}
		if after := counts(t); after != (goingUndoCounts{}) {
			t.Errorf("after Undo the run counts %+v, want nothing to take back, nothing taken back and nothing standing", after)
		}
		if unchanged := rig.textOf(t, `select coalesce(answer->>'unchanged', 'absent') from going_landings where run_id = $1::uuid and phase = 'undo'`, run); unchanged != "true" {
			t.Fatalf("the run's undo landing records unchanged %s, want true", unchanged)
		}
	})

	t.Run("an undo of a write whose process ended after it landed undoes from what its route recorded", func(t *testing.T) {
		issue := rig.issue(t, "Landed as its process ended")
		run := rig.ran(t, "rename Landed as its process ended", goingChange("dispatch_issue_update", retitle(issue, "Landed and never recorded")))
		// The process ended once the write's route committed: its row says only the key that ran
		// it, and holds no read of the item it was approved over.
		rig.exec(t, `
			update going_plan_rows set state = 'running', approval = null, result = jsonb_build_object('step', result->'step', 'call', result->'call')
			where run_id = $1::uuid
		`, run)
		if line := rig.undone(t, run, "dispatch_issue_update")[0]; !line.Outcome.OK || rig.title(t, issue) != "Landed as its process ended" {
			t.Fatalf("Undo answered %+v and the issue is titled %q, want the title its route found written back", line, rig.title(t, issue))
		}
	})

	t.Run("a document's edit is written back", func(t *testing.T) {
		issue := rig.issue(t, "Holds notes to put back")
		notes := uploadDocument(t, rig.mux, "/api/v1/issues/"+issue, "notes.md", "# Notes\n\nThe first line.\n")
		run := rig.ran(t, "edit the notes to put back", goingChange("dispatch_doc_edit", map[string]any{
			"issue": issue, "artifact": notes.Artifact.Slug, "ops": replaceText("first line", "opening line"),
		}))
		line := rig.undone(t, run, "dispatch_doc_edit")[0]
		if text := rig.documentText(t, notes.Artifact.ID); !line.Outcome.OK || !strings.Contains(text, "The first line.") {
			t.Fatalf("Undo of the edit answered %+v and the document reads %q, want the first line back", line, text)
		}
	})

	t.Run("a document that changed since is undone by nothing, saying why", func(t *testing.T) {
		issue := rig.issue(t, "Holds notes to edit")
		owner := "/api/v1/issues/" + issue
		notes := uploadDocument(t, rig.mux, owner, "notes.md", "# Notes\n\nThe first line.\n")
		run := rig.ran(t, "edit the notes", goingChange("dispatch_doc_edit", map[string]any{
			"issue": issue, "artifact": notes.Artifact.Slug, "ops": replaceText("first line", "opening line"),
		}))
		rig.fixture(t, "bob", http.MethodPost, owner+"/artifacts/"+notes.Artifact.Slug+"/edits", map[string]any{
			"ops": replaceText("opening line", "line bob wrote"),
		})
		line := rig.undone(t, run, "dispatch_doc_edit")[0]
		if line.Outcome.OK || line.Outcome.Code != codeGoingUndoChanged || !strings.Contains(line.Outcome.Message, "version") ||
			!strings.Contains(line.Outcome.Message, "user bob") {
			t.Fatalf("Undo of the edit answered %+v, want %s naming the document's version now and bob", line, codeGoingUndoChanged)
		}
		if text := rig.documentText(t, notes.Artifact.ID); !strings.Contains(text, "line bob wrote") {
			t.Fatalf("the document reads %q after Undo, want bob's line kept", text)
		}
	})

	t.Run("a document another writer changes after the undo's read is kept, nothing written back", func(t *testing.T) {
		issue := rig.issue(t, "Holds notes edited mid-undo")
		owner := "/api/v1/issues/" + issue
		notes := uploadDocument(t, rig.mux, owner, "notes.md", "# Notes\n\nThe first line.\n")
		run := rig.ran(t, "edit the notes edited mid-undo", goingChange("dispatch_doc_edit", map[string]any{
			"issue": issue, "artifact": notes.Artifact.Slug, "ops": replaceText("first line", "opening line"),
		}))
		var writes atomic.Int32
		rig.hooks.set(t, http.MethodPost, owner+"/artifacts", func(w http.ResponseWriter, r *http.Request, inner http.Handler) {
			// bob edits the document after the undo's read, which found the token the run's edit
			// left, and before its write back, which names that token.
			if writes.Add(1) == 1 {
				if response := dispatchRequest(t, rig.mux, http.MethodPost, owner+"/artifacts/"+notes.Artifact.Slug+"/edits",
					map[string]any{"ops": replaceText("opening line", "line bob wrote")}, "bob"); response.Code >= http.StatusMultipleChoices {
					t.Errorf("bob's own edit answered %d %s", response.Code, response.Body.String())
				}
			}
			inner.ServeHTTP(w, r)
		})
		first := rig.undone(t, run, "dispatch_doc_edit")
		line := first[0]
		if line.Outcome.OK || line.Outcome.Code != codeGoingUndoChanged || line.Outcome.Message != goingUndoDocumentChanged {
			t.Fatalf("Undo of the edit answered %+v, want %s saying %q: the document changed after the undo's read",
				line, codeGoingUndoChanged, goingUndoDocumentChanged)
		}
		// The upload route's refusal tells an uploader to read a fresh token and upload again; the
		// line a person reads in the bar says none of it.
		if encoded, err := json.Marshal(line); err != nil || strings.Contains(string(encoded), "dispatch doc-read") {
			t.Fatalf("Undo of the edit answered %s (%v), want no word of the upload route's advice to run dispatch doc-read", encoded, err)
		}
		if text := rig.documentText(t, notes.Artifact.ID); !strings.Contains(text, "line bob wrote") {
			t.Fatalf("the document reads %q after Undo, want bob's line kept and nothing written back", text)
		}
		if second := rig.undone(t, run, "dispatch_doc_edit"); !reflect.DeepEqual(first, second) || writes.Load() != 1 {
			t.Fatalf("the second press answered %+v with the write back sent %d times; want the first press's %+v, sent once", second, writes.Load(), first)
		}
	})

	t.Run("a created issue whose undo's process ended resumes at the close", func(t *testing.T) {
		renamed := rig.issue(t, "Renamed beside a create")
		run := rig.ran(t, "file one issue and rename another",
			goingChange("dispatch_issue", map[string]any{"project": "DEMO", "title": "Made before the undo ended", "force": true, "spec": "An issue the run files.\n"}),
			goingChange("dispatch_issue_update", retitle(renamed, "Renamed by the run")))
		var created string
		if err := rig.database.Pool.QueryRow(ctx, `select key from issues where title = 'Made before the undo ended'`).Scan(&created); err != nil {
			t.Fatal(err)
		}
		var renames atomic.Int32
		rig.hooks.set(t, http.MethodPatch, "/api/v1/issues/"+renamed, func(w http.ResponseWriter, r *http.Request, inner http.Handler) {
			renames.Add(1)
			inner.ServeHTTP(w, r)
		})
		var ended atomic.Bool
		rig.hooks.set(t, http.MethodPatch, "/api/v1/issues/"+created, func(w http.ResponseWriter, r *http.Request, inner http.Handler) {
			if ended.CompareAndSwap(false, true) {
				panic("the process ends as the close is sent")
			}
			inner.ServeHTTP(w, r)
		})
		func() {
			defer func() {
				if recover() == nil {
					t.Fatal("the first Undo ran to its end, want it to end as the close is sent")
				}
			}()
			dispatchRequest(t, rig.mux, http.MethodPost, "/api/v1/going/runs/"+run+"/undo", nil, "alice")
		}()
		if title, status := rig.title(t, renamed), rig.issueRead(t, created).Status; title != "Renamed beside a create" || status == "done" {
			t.Fatalf("after the Undo that ended the renamed issue is %q and the created one %s, want the title back and the issue open", title, status)
		}
		lines := rig.undone(t, run, "dispatch_issue_update", "dispatch_issue")
		if !lines[0].Outcome.OK || !lines[1].Outcome.OK || rig.issueRead(t, created).Status != "done" || renames.Load() != 1 {
			t.Fatalf("the second Undo answered %+v with the rename sent %d times, want the close made and the rename sent once", lines, renames.Load())
		}
		if messages := rig.count(t, `select count(*) from messages where issue_key = $1`, created); messages != 0 {
			t.Fatalf("the closed issue holds %d messages, want none", messages)
		}
	})

	t.Run("a repeat under the key of a press whose process ended mid-walk finishes the walk", func(t *testing.T) {
		older, kept := rig.issue(t, "Renamed before the kept one"), rig.issue(t, "Renamed and retitled by bob")
		run := rig.ran(t, "rename two issues, one bob retitles",
			goingChange("dispatch_issue_update", retitle(older, "Ours, older")),
			goingChange("dispatch_issue_update", retitle(kept, "Ours, kept")))
		rig.fixture(t, "bob", http.MethodPatch, "/api/v1/issues/"+kept, map[string]any{"title": "Bob's"})
		var ended atomic.Bool
		var sent atomic.Int32
		rig.hooks.set(t, http.MethodPatch, "/api/v1/issues/"+older, func(w http.ResponseWriter, r *http.Request, inner http.Handler) {
			if ended.CompareAndSwap(false, true) {
				panic("the process ends as the older rename's write back is sent")
			}
			sent.Add(1)
			inner.ServeHTTP(w, r)
		})
		key := "undo-" + run
		path := "/api/v1/going/runs/" + run + "/undo"
		// The walk goes newest first: it keeps bob's title, which records the call's undo and
		// marks the key committed, and the process ends as it sends the older rename's write back.
		func() {
			defer func() {
				if recover() == nil {
					t.Fatal("the first press ran to its end, want it to end as the older write back is sent")
				}
			}()
			keyedRequest(t, rig.mux, http.MethodPost, path, nil, "alice", key)
		}()
		// A process that dies releases nothing: its lease lapses, as the key layer's own tests
		// leave a dead holder's.
		rig.exec(t, `update request_keys set leased_at = now() - interval '2 minutes' where idempotency_key = $1`, key)
		if committed := rig.textOf(t, `select (committed_at is not null)::text || ' ' || coalesce(recovery->>'run', 'none') from request_keys where idempotency_key = $1`, key); committed != "true "+run {
			t.Fatalf("the press left its key %q, want it committed with the run as its recovery point", committed)
		}
		if rig.title(t, older) != "Ours, older" || sent.Load() != 0 {
			t.Fatalf("after the press that ended the older issue is titled %q with %d write backs sent, want the rename standing and none sent", rig.title(t, older), sent.Load())
		}
		repeat := keyedRequest(t, rig.mux, http.MethodPost, path, nil, "alice", key)
		if repeat.Code != http.StatusOK || !replayed(repeat) {
			t.Fatalf("the repeat under the key answered %d %s, want the walk replayed", repeat.Code, repeat.Body.String())
		}
		lines := decodeBody[goingUndoAnswer](t, repeat).Lines
		if len(lines) != 2 || lines[0].Outcome.Code != codeGoingUndoChanged || lines[0].Item != kept || !lines[1].Outcome.OK || lines[1].Item != older {
			t.Fatalf("the repeat answered %+v, want bob's title kept and the older rename written back", lines)
		}
		if rig.title(t, older) != "Renamed before the kept one" || rig.title(t, kept) != "Bob's" || sent.Load() != 1 {
			t.Fatalf("after the repeat the issues are titled %q and %q with %d write backs sent, want the older title back, bob's kept, one sent",
				rig.title(t, older), rig.title(t, kept), sent.Load())
		}
		again := keyedRequest(t, rig.mux, http.MethodPost, path, nil, "alice", key)
		if again.Code != http.StatusOK || !reflect.DeepEqual(decodeBody[goingUndoAnswer](t, again).Lines, lines) || sent.Load() != 1 {
			t.Fatalf("a later repeat answered %d %s with %d write backs sent, want the same lines and nothing sent again", again.Code, again.Body.String(), sent.Load())
		}
	})
}

// TestARunSaysWhetherUndoTakesItBack: a run's read, GET /api/v1/going/runs/{id} and History
// alike, says whether Undo can still take a change of it back (undoable) and when its Undo landed
// (undone_at), so History offers Undo only while it takes something back and an undone run says
// when after a reload; and it counts its changes Undo can still take back, took back, and tried
// and could not take back, so no page infers them from the two. A run whose write landed is
// undoable and was never undone; once its Undo lands it is not undoable, and it says the moment
// that undo landed. A run that wrote nothing, a run whose one write is a message, which Undo names
// sent, not undone, and a reasoned close of which only the reason landed are not undoable, before
// Undo and after it, and never undone. So, decided as Undo decides it from the call's record, are
// a claim its route answered changed nothing, a create its route answered 200 for an issue
// Dispatch already held, a write whose record cannot be read, and an update whose recorded
// last_seq does not decode: Undo sends nothing for any of them, and History offered it on every
// reload. Of those, only a write Undo refuses stands; a message and a reason, sent, not undone by
// design, and a claim and a create that changed nothing are in no count. A change Undo keeps
// because another writer changed it since the run stands once Undo kept it, and a second press
// answers it as the first did. A reasoned close of which only the reason landed, left unticked
// while its person approves the other row of its plan, keeps the reason as its row's: the run
// counts the other row's change alone, and Undo takes that back and names the reason, on the
// close's row, sent, not undone.
func TestARunSaysWhetherUndoTakesItBack(t *testing.T) {
	t.Parallel()
	rig := newGoingPlanRig(t)
	// read is run as alice reads it by id, failing t unless History lists it saying the same.
	read := func(t *testing.T, run string) goingRun {
		t.Helper()
		response := dispatchRequest(t, rig.mux, http.MethodGet, "/api/v1/going/runs/"+run, nil, "alice")
		if response.Code != http.StatusOK {
			t.Fatalf("GET the run %s answered %d, want 200", run, response.Code)
		}
		got := decodeBody[goingRun](t, response)
		for _, conversation := range goingHistory(t, rig.mux, "alice", "").Rows {
			for _, listed := range conversation.Runs {
				if listed.ID != run {
					continue
				}
				if listed.Undoable != got.Undoable || (listed.UndoneAt == nil) != (got.UndoneAt == nil) ||
					listed.UndoneAt != nil && !listed.UndoneAt.Equal(*got.UndoneAt) {
					t.Fatalf("History lists run %s undoable %v, undone at %v; its read says %v, %v", run, listed.Undoable, listed.UndoneAt, got.Undoable, got.UndoneAt)
				}
				if goingUndoCountsOf(listed) != goingUndoCountsOf(got) {
					t.Fatalf("History lists run %s with the counts %+v; its read says %+v", run, goingUndoCountsOf(listed), goingUndoCountsOf(got))
				}
				return got
			}
		}
		t.Fatalf("History does not list run %s", run)
		return goingRun{}
	}
	// neverUndoable fails t unless run reads not undoable and never undone, with nothing to take
	// back, nothing taken back and standing of its changes standing.
	neverUndoable := func(t *testing.T, run, when string, standing int) {
		t.Helper()
		got := read(t, run)
		if got.Undoable || got.UndoneAt != nil {
			t.Fatalf("%s the run reads undoable %v, undone at %v; want it not undoable and never undone", when, got.Undoable, got.UndoneAt)
		}
		if counts := goingUndoCountsOf(got); counts != (goingUndoCounts{standing: standing}) {
			t.Fatalf("%s the run counts %+v, want nothing to take back, nothing taken back and %d standing", when, counts, standing)
		}
	}

	t.Run("a run whose write landed is undoable until its Undo lands, and then says when", func(t *testing.T) {
		issue := rig.issue(t, "Renamed, then undone")
		run := rig.ran(t, "rename Renamed, then undone", goingChange("dispatch_issue_update", retitle(issue, "Renamed by the run")))
		if got := read(t, run); !got.Undoable || got.UndoneAt != nil || goingUndoCountsOf(got) != (goingUndoCounts{undoable: 1}) {
			t.Fatalf("before Undo the run reads undoable %v, undone at %v, counts %+v; want it undoable, never undone, one change to take back",
				got.Undoable, got.UndoneAt, goingUndoCountsOf(got))
		}
		if line := rig.undone(t, run, "dispatch_issue_update")[0]; !line.Outcome.OK {
			t.Fatalf("Undo answered %+v, want the title written back", line)
		}
		var landed time.Time
		if err := rig.database.Pool.QueryRow(context.Background(), `
			select landed_at from going_landings where run_id = $1::uuid and phase = 'undo'
		`, run).Scan(&landed); err != nil {
			t.Fatalf("read when the run's undo landed: %v", err)
		}
		if got := read(t, run); got.Undoable || got.UndoneAt == nil || !got.UndoneAt.Equal(landed) || goingUndoCountsOf(got) != (goingUndoCounts{undone: 1}) {
			t.Fatalf("after Undo the run reads undoable %v, undone at %v, counts %+v; want it not undoable, undone at %s, one change taken back",
				got.Undoable, got.UndoneAt, goingUndoCountsOf(got), landed)
		}
	})

	t.Run("a run that wrote nothing is not undoable", func(t *testing.T) {
		run := rig.doneRun(t, "alice", "what is open on the board", time.Minute)
		neverUndoable(t, run, "before Undo", 0)
		rig.undone(t, run)
		neverUndoable(t, run, "after Undo", 0)
	})

	t.Run("a run whose one write is a message is not undoable", func(t *testing.T) {
		issue := rig.issue(t, "Messaged, not undoable")
		run := rig.ran(t, "ask on Messaged, not undoable where we are", goingChange("dispatch_message", map[string]any{"issue": issue, "body": "Where are we?"}))
		neverUndoable(t, run, "before Undo", 0)
		rig.undone(t, run, "dispatch_message")
		neverUndoable(t, run, "after Undo", 0)
	})

	t.Run("a claim its route answered changed nothing is not undoable", func(t *testing.T) {
		issue := rig.issue(t, "Claimed by alice already")
		rig.fixture(t, "alice", http.MethodPost, "/api/v1/issues/"+issue+"/claim", map[string]any{})
		rig.landsBeside(t, http.MethodPost, "/api/v1/issues/"+issue+"/claim")
		run := rig.ran(t, "claim Claimed by alice already", goingChange("dispatch_claim", map[string]any{"issue": issue}))
		if changed := rig.textOf(t, `select answer->'body'->>'claim_changed' from going_landings where run_id = $1::uuid and phase = 'do'`, run); changed != "false" {
			t.Fatalf("the claim's landing records claim_changed %s, want false", changed)
		}
		neverUndoable(t, run, "before Undo", 0)
		if line := rig.undone(t, run, "dispatch_claim")[0]; !line.Outcome.OK || line.Outcome.Code != "" {
			t.Fatalf("Undo answered %+v, want a successful no-op", line)
		}
		if claim := rig.issueRead(t, issue).Claim; claim == nil || claim.Actor.Kind != "user" || claim.Actor.ID != "alice" {
			t.Fatalf("claim after Undo = %+v, want alice's pre-existing claim", claim)
		}
		if writes := rig.count(t, `select count(*) from going_landings where run_id = $1::uuid and phase = 'undo'`, run); writes != 0 {
			t.Fatalf("Undo recorded %d landings, want none", writes)
		}
		neverUndoable(t, run, "after Undo", 0)
	})

	t.Run("a create its route answered for an issue Dispatch already held is not undoable", func(t *testing.T) {
		held := decodeBody[model.Issue](t, rig.fixture(t, "alice", http.MethodPost, "/api/v1/issues", map[string]any{
			"project": "DEMO", "title": "Tracked upstream as acme/held#9", "external": "acme/held#9", "force": true,
		})).Key
		rig.landsBeside(t, http.MethodPost, "/api/v1/issues")
		run := rig.ran(t, "file Held upstream again", goingChange("dispatch_issue", map[string]any{
			"project": "DEMO", "title": "Held upstream again", "external": "acme/held#9", "force": true, "spec": "The issue held upstream, filed again.\n",
		}))
		if landed := rig.count(t, `select count(*) from going_landings where run_id = $1::uuid and phase = 'do' and answer->>'status' = '200'`, run); landed != 1 {
			t.Fatalf("the create recorded %d landings answered 200, want 1", landed)
		}
		neverUndoable(t, run, "before Undo", 0)
		if line := rig.undone(t, run, "dispatch_issue")[0]; !line.Outcome.OK || !strings.Contains(line.Outcome.Message, held) || rig.issueRead(t, held).Status == "done" {
			t.Fatalf("Undo answered %+v and left %s %s, want nothing closed, naming %s", line, held, rig.issueRead(t, held).Status, held)
		}
		neverUndoable(t, run, "after Undo", 0)
	})

	t.Run("a write whose record cannot be read is not undoable", func(t *testing.T) {
		issue := rig.issue(t, "Renamed with no record")
		run := rig.ran(t, "rename Renamed with no record", goingChange("dispatch_issue_update", retitle(issue, "Renamed")))
		rig.exec(t, `update going_landings set answer = answer - 'body' where run_id = $1::uuid and phase = 'do'`, run)
		neverUndoable(t, run, "before Undo", 1)
		if line := rig.undone(t, run, "dispatch_issue_update")[0]; line.Outcome.Code != codeGoingUndoAfterUnread || rig.title(t, issue) != "Renamed" {
			t.Fatalf("Undo answered %+v and the title is %q, want nothing put back, %s", line, rig.title(t, issue), codeGoingUndoAfterUnread)
		}
		neverUndoable(t, run, "after Undo", 1)
	})

	t.Run("a write whose recorded last_seq does not decode is not undoable", func(t *testing.T) {
		issue := rig.issue(t, "Renamed with an unreadable sequence")
		run := rig.ran(t, "rename Renamed with an unreadable sequence", goingChange("dispatch_issue_update", retitle(issue, "Renamed")))
		// The route writes last_seq as a number, so only a record changed since holds another.
		rig.exec(t, `update going_landings set answer = jsonb_set(answer, '{body,last_seq}', '"seven"') where run_id = $1::uuid and phase = 'do'`, run)
		neverUndoable(t, run, "before Undo", 1)
		if line := rig.undone(t, run, "dispatch_issue_update")[0]; line.Outcome.Code != codeGoingUndoAfterUnread || rig.title(t, issue) != "Renamed" {
			t.Fatalf("Undo answered %+v and the title is %q, want nothing put back, %s", line, rig.title(t, issue), codeGoingUndoAfterUnread)
		}
		neverUndoable(t, run, "after Undo", 1)
	})

	// A run of three changes: one Undo takes back, one whose record Undo refuses, and a message,
	// which Undo does not take back by design. Before Undo the read counts one to take back and the
	// refused one standing; after it, one taken back and the same one standing, the message in no
	// count, and a second press changes no count.
	t.Run("a run of three changes counts what Undo takes back, took back and leaves standing", func(t *testing.T) {
		undone, refused, messaged := rig.issue(t, "Renamed and undone"), rig.issue(t, "Renamed and refused"), rig.issue(t, "Messaged and standing")
		run := rig.ran(t, "rename two issues and ask on a third",
			goingChange("dispatch_issue_update", retitle(undone, "Renamed by the run")),
			goingChange("dispatch_issue_update", retitle(refused, "Renamed for good")),
			goingChange("dispatch_message", map[string]any{"issue": messaged, "body": "Where are we?"}))
		// The second rename's record says nothing of what it left, so its undo is refused (409
		// GOING_UNDO_AFTER_UNREAD) with nothing sent, and its change stands.
		rig.exec(t, `update going_landings set answer = answer - 'body' where run_id = $1::uuid and phase = 'do' and path = $2`, run, "/api/v1/issues/"+refused)
		if got := read(t, run); !got.Undoable || got.UndoneAt != nil || goingUndoCountsOf(got) != (goingUndoCounts{undoable: 1, standing: 1}) {
			t.Fatalf("before Undo the run reads undoable %v, undone at %v, counts %+v; want one change to take back and the refused one standing",
				got.Undoable, got.UndoneAt, goingUndoCountsOf(got))
		}
		lines := rig.undone(t, run, "dispatch_message", "dispatch_issue_update", "dispatch_issue_update")
		if lines[0].Outcome.Message != goingSentNotUndone || lines[1].Outcome.Code != codeGoingUndoAfterUnread || !lines[2].Outcome.OK {
			t.Fatalf("Undo answered %+v, want the message sent, not undone, the second rename refused and the first written back", lines)
		}
		if rig.title(t, undone) != "Renamed and undone" || rig.title(t, refused) != "Renamed for good" {
			t.Fatalf("after Undo the issues are titled %q and %q, want the first title back and the second rename standing", rig.title(t, undone), rig.title(t, refused))
		}
		undid := func(t *testing.T, when string) {
			t.Helper()
			if got := read(t, run); got.Undoable || got.UndoneAt == nil || goingUndoCountsOf(got) != (goingUndoCounts{undone: 1, standing: 1}) {
				t.Fatalf("%s the run reads undoable %v, undone at %v, counts %+v; want one change taken back and the refused one standing",
					when, got.Undoable, got.UndoneAt, goingUndoCountsOf(got))
			}
		}
		undid(t, "after Undo")
		rig.undone(t, run, "dispatch_message", "dispatch_issue_update", "dispatch_issue_update")
		undid(t, "after a second press")
	})

	// Three renames another writer changed after the run: one whose undo's read finds the title
	// changed, so it sends nothing; one that also moved the status, whose undo writes the status
	// back and keeps the title; and one whose title changes between the undo's read and its write,
	// which its route refuses. Each is undoable before Undo; after it each stands, kept, and none is
	// undoable, and a second press answers the first's lines and sends nothing again.
	t.Run("a change Undo keeps as another writer changed it stands once Undo kept it", func(t *testing.T) {
		kept, partly, raced := rig.issue(t, "Retitled by bob since"), rig.issue(t, "Retitled by bob and moved back"), rig.issue(t, "Retitled by bob mid-undo")
		status := rig.issueRead(t, partly).Status
		run := rig.ran(t, "rename three issues",
			goingChange("dispatch_issue_update", retitle(kept, "Ours, kept")),
			goingChange("dispatch_issue_update", map[string]any{"issue": partly, "title": "Ours, partly", "status": "in_progress"}),
			goingChange("dispatch_issue_update", retitle(raced, "Ours, raced")))
		rig.fixture(t, "bob", http.MethodPatch, "/api/v1/issues/"+kept, map[string]any{"title": "Bob's"})
		rig.fixture(t, "bob", http.MethodPatch, "/api/v1/issues/"+partly, map[string]any{"title": "Bob's"})
		var writes atomic.Int32
		rig.hooks.set(t, http.MethodPatch, "/api/v1/issues/"+raced, func(w http.ResponseWriter, r *http.Request, inner http.Handler) {
			// bob retitles the issue after the undo's read and before its write back.
			if writes.Add(1) == 1 {
				if response := dispatchRequest(t, rig.mux, http.MethodPatch, "/api/v1/issues/"+raced, map[string]any{"title": "Bob's"}, "bob"); response.Code >= http.StatusMultipleChoices {
					t.Errorf("bob's own retitle answered %d %s", response.Code, response.Body.String())
				}
			}
			inner.ServeHTTP(w, r)
		})
		if got := read(t, run); !got.Undoable || goingUndoCountsOf(got) != (goingUndoCounts{undoable: 3}) {
			t.Fatalf("before Undo the run reads undoable %v, counts %+v; want three changes to take back", got.Undoable, goingUndoCountsOf(got))
		}
		first := rig.undone(t, run, "dispatch_issue_update", "dispatch_issue_update", "dispatch_issue_update")
		for _, line := range first {
			if line.Outcome.OK || line.Outcome.Code != codeGoingUndoChanged {
				t.Fatalf("Undo answered %+v, want each rename kept, %s", first, codeGoingUndoChanged)
			}
		}
		if rig.title(t, kept) != "Bob's" || rig.title(t, partly) != "Bob's" || rig.title(t, raced) != "Bob's" || rig.issueRead(t, partly).Status != status {
			t.Fatalf("after Undo the titles are %q, %q and %q and the moved issue is %s; want bob's titles kept and the status %s back",
				rig.title(t, kept), rig.title(t, partly), rig.title(t, raced), rig.issueRead(t, partly).Status, status)
		}
		stood := func(t *testing.T, when string) {
			t.Helper()
			if got := read(t, run); got.Undoable || goingUndoCountsOf(got) != (goingUndoCounts{standing: 3}) {
				t.Fatalf("%s the run reads undoable %v, counts %+v; want it not undoable, three changes standing", when, got.Undoable, goingUndoCountsOf(got))
			}
		}
		stood(t, "after Undo")
		second := rig.undone(t, run, "dispatch_issue_update", "dispatch_issue_update", "dispatch_issue_update")
		if !reflect.DeepEqual(first, second) || writes.Load() != 1 {
			t.Fatalf("the second press answered %+v with the raced write back sent %d times; want the first press's %+v, sent once", second, writes.Load(), first)
		}
		stood(t, "after a second press")
	})

	// A reasoned close of which only the reason landed waits for its person again beside a rename,
	// the other row of its plan, and the person approves the rename and leaves the close unticked.
	// The reason's landing stays the close's row, which keeps the key of that earlier try (its
	// resumes, goingLandedWrites), so the run counts the rename alone to take back, and Undo writes
	// the rename back and names the reason, on the close's row, sent, not undone, with no line of a
	// call that names no row. The test stops the rig's agent loop and is the run's agent until the
	// close waits again, as partlyLandedWaiting is, then starts the loop again to run the rename.
	t.Run("a row left unticked after only its reason landed keeps the reason, sent, not undone", func(t *testing.T) {
		issue, other := rig.issue(t, "Left unticked after its reason landed"), rig.issue(t, "Renamed beside an unticked close")
		const words = "close Left unticked after its reason landed, saying why, and rename Renamed beside an unticked close"
		reason := "Shipped in the last release."
		closing := map[string]any{"issue": issue, "status": "done", "reason": reason}
		rig.loop.script.set(words, func(n int) agenttest.Reply {
			if n == 1 {
				return proposing(goingChange("dispatch_issue_update", closing), goingChange("dispatch_issue_update", retitle(other, "Renamed by the run")))
			}
			return answered("Renamed, and the close left out.")
		})
		run := rig.begin(t, goingLoopRun{login: "alice", words: words})
		rig.waiting(t, run)
		rig.loop.stop()
		// Bob renames the other issue after the plan showed it, so the approval leaves the rename
		// waiting again and runs the close alone.
		rig.fixture(t, "bob", http.MethodPatch, "/api/v1/issues/"+other, map[string]any{"title": "Bob's"})
		if plan := rig.answer(t, run, "approve", map[string]any{"plan": 1}); !slices.Equal(rowStates(plan), []string{"approved", "waiting"}) {
			t.Fatalf("the first approval left rows %v, want the close approved and the rename waiting again", rowStates(plan))
		}
		// The close's reason lands and its close fails in the server; Bob moves the issue, so the
		// retry of the key makes the close wait again, the reason already written.
		claimed := rig.claim(t, "i-unticked-"+issue)
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
		if err := rig.client.End(context.Background(), agentwire.EndRequest{Run: run, LeaseToken: claimed.LeaseToken, State: agentwire.StateWaiting}); err != nil {
			t.Fatalf("end the turn waiting: %v", err)
		}
		plan := rig.answer(t, run, "approve", map[string]any{"plan": 1, "rows": []map[string]any{{"row": 0, "tick": false}, {"row": 1, "tick": true}}})
		if !slices.Equal(rowStates(plan), []string{"unticked", "approved"}) {
			t.Fatalf("the second approval left rows %v, want the close unticked and the rename approved", rowStates(plan))
		}
		rig.loop.start(rig.goingAgentRig)
		if record := rig.ended(t, run, time.Minute); record.state != "done" {
			t.Fatalf("after the second approval the run ended %s with %q, want done; its calls were told %q", record.state, record.reason, rig.toolResults(t, run))
		}
		if reasons, status, title := rig.goingReasonsOn(t, issue, reason), rig.issueRead(t, issue).Status, rig.title(t, other); reasons != 1 || status != "in_progress" || title != "Renamed by the run" {
			t.Fatalf("%s holds %d reasons and status %s, and %s is titled %q; want the one reason, Bob's status and the rename", issue, reasons, status, other, title)
		}
		if got := read(t, run); !got.Undoable || got.UndoneAt != nil || goingUndoCountsOf(got) != (goingUndoCounts{undoable: 1}) {
			t.Fatalf("before Undo the run reads undoable %v, undone at %v, counts %+v; want the rename to take back and the reason in no count",
				got.Undoable, got.UndoneAt, goingUndoCountsOf(got))
		}
		lines := rig.undone(t, run, "dispatch_issue_update", "dispatch_issue_update")
		at := slices.IndexFunc(lines, func(line goingUndoLine) bool { return line.Undo == goingUndoWriteBack })
		if at < 0 || lines[at].Plan != 1 || lines[at].Row != 1 || !lines[at].Outcome.OK || rig.title(t, other) != "Bob's" {
			t.Fatalf("the undo lines are %+v and %s is titled %q, want the rename of row 1 of plan 1 written back to Bob's", lines, other, rig.title(t, other))
		}
		goingReasonOnlyLine(t, lines[1-at], issue)
		if got := read(t, run); got.Undoable || got.UndoneAt == nil || goingUndoCountsOf(got) != (goingUndoCounts{undone: 1}) {
			t.Fatalf("after Undo the run reads undoable %v, undone at %v, counts %+v; want the rename taken back and the reason in no count",
				got.Undoable, got.UndoneAt, goingUndoCountsOf(got))
		}
	})

	// Last: partlyLandedWaiting stops the rig's agent loop.
	t.Run("a reasoned close of which only the reason landed is not undoable", func(t *testing.T) {
		run, issue, _ := rig.partlyLandedWaiting(t, "Only its reason landed")
		rig.answer(t, run, "reject", map[string]any{"plan": 1})
		neverUndoable(t, run, "before Undo", 0)
		goingReasonOnlyLine(t, rig.undone(t, run, "dispatch_issue_update")[0], issue)
		neverUndoable(t, run, "after Undo", 0)
	})
}

// goingUndoCounts is what a run's read counts of its changes: how many Undo can still take back,
// how many an Undo took back, and how many Undo tried and could not take back.
type goingUndoCounts struct {
	undoable, undone, standing int
}

// goingUndoCountsOf is run's counts.
func goingUndoCountsOf(run goingRun) goingUndoCounts {
	return goingUndoCounts{undoable: run.UndoableCount, undone: run.UndoneCount, standing: run.StandingCount}
}

// landsBeside makes the request method path of a run record its landing even where its route
// commits nothing, as a route that committed beside its answer would: after the route answers,
// the request commits a write of its own, whose commit step records the landing with what the
// route answered (TestAChangeNamesTheIssueAnEarlierCreateMakes).
func (rig goingPlanRig) landsBeside(t *testing.T, method, path string) {
	t.Helper()
	rig.hooks.set(t, method, path, func(w http.ResponseWriter, r *http.Request, inner http.Handler) {
		inner.ServeHTTP(w, r)
		tx, err := rig.database.Pool.Begin(r.Context())
		if err != nil {
			t.Errorf("begin a write of the request: %v", err)
			return
		}
		defer func() { _ = tx.Rollback(r.Context()) }()
		for _, statement := range []string{`create temp table beside_scratch (n int) on commit drop`, `insert into beside_scratch values (1)`} {
			if _, err := tx.Exec(r.Context(), statement); err != nil {
				t.Errorf("%s: %v", statement, err)
				return
			}
		}
		if err := tx.Commit(r.Context()); err != nil {
			t.Errorf("commit a write of the request: %v", err)
		}
	})
}

// TestAKeptValueNamesWhoChangedIt: a value Undo keeps because another writer changed it after the
// run names who changed that value after the call landed - the writer of that field's change, of
// that part of the ask, of that version of the document, of that move of the issue - and nobody
// where no event after the landing names one. Before, it named whoever wrote the item's newest
// event of any kind, read when Undo was pressed, so a comment, a change of another field or a
// review made since named the wrong person, and the bar said they changed it after this run.
func TestAKeptValueNamesWhoChangedIt(t *testing.T) {
	t.Parallel()
	rig := newGoingPlanRig(t)
	bob := goingHolder{Kind: "user", ID: "bob"}
	// kept is the field of line named field, failing t unless Undo kept it.
	kept := func(t *testing.T, line goingUndoLine, field string) goingUndoField {
		t.Helper()
		for _, each := range line.Fields {
			if each.Field == field {
				if each.OK || line.Outcome.Code != codeGoingUndoChanged {
					t.Fatalf("Undo answered %+v, want %s kept", line, field)
				}
				return each
			}
		}
		t.Fatalf("Undo answered %+v, which names no field %s", line, field)
		return goingUndoField{}
	}
	// comment posts a comment on issue as carol, the item's newest event.
	comment := func(t *testing.T, issue string) {
		t.Helper()
		rig.fixture(t, "carol", http.MethodPost, "/api/v1/issues/"+issue+"/comments", map[string]any{"body": "Looks right to me."})
	}

	t.Run("an issue's field names who changed it, not who changed another field or commented since", func(t *testing.T) {
		issue := rig.issue(t, "Retitled, then commented on")
		run := rig.ran(t, "retitle Retitled, then commented on", goingChange("dispatch_issue_update", retitle(issue, "Ours")))
		rig.fixture(t, "bob", http.MethodPatch, "/api/v1/issues/"+issue, map[string]any{"title": "Bob's"})
		rig.fixture(t, "carol", http.MethodPatch, "/api/v1/issues/"+issue, map[string]any{"priority": 1})
		comment(t, issue)
		field := kept(t, rig.undone(t, run, "dispatch_issue_update")[0], "title")
		if field.By == nil || *field.By != bob || string(field.Now) != `"Bob's"` {
			t.Fatalf("the kept title answered %+v, want Bob's title and Bob as its writer", field)
		}
	})

	t.Run("an issue's field no event after the run names is kept naming nobody", func(t *testing.T) {
		issue := rig.issue(t, "Retitled, then changed in the store")
		run := rig.ran(t, "retitle Retitled, then changed in the store", goingChange("dispatch_issue_update", retitle(issue, "Ours")))
		rig.exec(t, `update issues set title = 'Changed in the store' where key = $1`, issue)
		comment(t, issue)
		field := kept(t, rig.undone(t, run, "dispatch_issue_update")[0], "title")
		if field.By != nil || string(field.Now) != `"Changed in the store"` {
			t.Fatalf("the kept title answered %+v, want the stored title with no writer", field)
		}
	})

	t.Run("an ask's part names who changed it, not who changed another part since", func(t *testing.T) {
		ask := rig.ask(t, rig.issue(t, "Holds an ask reworded twice"), "Ship it on Friday?")
		run := rig.ran(t, "move the ask to Monday", goingChange("dispatch_edit_ask", map[string]any{"ask": ask, "question": "Ship it on Monday?"}))
		rig.fixture(t, "bob", http.MethodPatch, "/api/v1/asks/"+ask, map[string]any{"question": "Ship it on Tuesday?"})
		rig.fixture(t, "carol", http.MethodPatch, "/api/v1/asks/"+ask, map[string]any{"urgency": "high"})
		field := kept(t, rig.undone(t, run, "dispatch_edit_ask")[0], "question")
		if field.By == nil || *field.By != bob || string(field.Now) != `"Ship it on Tuesday?"` {
			t.Fatalf("the kept question answered %+v, want Bob's question and Bob as its writer", field)
		}
	})

	t.Run("an issue the run created and another moved names who moved it", func(t *testing.T) {
		run := rig.ran(t, "file Moved after the run", goingChange("dispatch_issue", map[string]any{"project": "DEMO", "title": "Moved after the run", "force": true,
			"spec": "An issue another person moves after the run.\n"}))
		created := rig.textOf(t, `select key from issues where title = 'Moved after the run'`)
		rig.fixture(t, "bob", http.MethodPatch, "/api/v1/issues/"+created, map[string]any{"status": "in_progress"})
		comment(t, created)
		line := rig.undone(t, run, "dispatch_issue")[0]
		field := kept(t, line, "issue")
		if field.By == nil || *field.By != bob || rig.issueRead(t, created).Status != "in_progress" {
			t.Fatalf("Undo of the create answered %+v, want Bob's move to in_progress kept", line)
		}
	})

	t.Run("a document's version names who wrote it, not who reviewed it since", func(t *testing.T) {
		issue := rig.issue(t, "Holds notes edited, then reviewed")
		owner := "/api/v1/issues/" + issue
		notes := uploadDocument(t, rig.mux, owner, "notes.md", "# Notes\n\nThe first line.\n")
		run := rig.ran(t, "edit the notes before their review", goingChange("dispatch_doc_edit", map[string]any{
			"issue": issue, "artifact": notes.Artifact.Slug, "ops": replaceText("first line", "opening line"),
		}))
		rig.fixture(t, "bob", http.MethodPost, owner+"/artifacts/"+notes.Artifact.Slug+"/edits", map[string]any{
			"ops": replaceText("opening line", "line bob wrote"),
		})
		rig.fixture(t, "carol", http.MethodPost, "/api/v1/artifacts/"+notes.Artifact.ID+"/reviews", map[string]any{"state": "approved"})
		line := rig.undone(t, run, "dispatch_doc_edit")[0]
		field := kept(t, line, "content")
		if field.By == nil || *field.By != bob || rig.documentText(t, notes.Artifact.ID) != "# Notes\n\nThe line bob wrote.\n" {
			t.Fatalf("Undo of the edit answered %+v, want Bob's version kept", line)
		}
	})
}

// TestAnEditsUndoWritesBackTheDocumentItEditedNotOneWhoseSlugIsItsName: the undo of a document
// edit uploads the earlier content to the owner's upload route, and that route finds the document
// by the name the upload gives, taking first the one whose slug that name is. An owner can hold a
// document whose name is another document's slug: uploads made before the route's lookup read
// slugs left such pairs. Undo of an edit of such a document writes back that document and leaves
// the other as it was. Before, it sent the edited document's name, which found the other one: the
// undo was refused as another writer's change, or, where the other held the very content the edit
// left, the earlier content was written over the other.
func TestAnEditsUndoWritesBackTheDocumentItEditedNotOneWhoseSlugIsItsName(t *testing.T) {
	t.Parallel()
	rig := newGoingPlanRig(t)
	for _, other := range []struct{ holding, content string }{
		{"other content", "# Other\n\nAnother document's words.\n"},
		{"the content the edit leaves", "# Notes\n\nThe opening line.\n"},
	} {
		t.Run("beside a document holding "+other.holding, func(t *testing.T) {
			issue := rig.issue(t, "Holds a draft beside notes holding "+other.holding)
			owner := "/api/v1/issues/" + issue
			named := uploadDocument(t, rig.mux, owner, "notes.md", other.content)
			edited := uploadDocument(t, rig.mux, owner, "draft.md", "# Notes\n\nThe first line.\n")
			// The edited document takes the other's slug as its name, as an older upload could have
			// stored it; no route makes such a pair now.
			rig.exec(t, `update artifacts set name = $2 where id = $1::uuid`, edited.Artifact.ID, named.Artifact.Slug)
			versions := func() int {
				return rig.count(t, `select count(*) from artifact_versions where artifact_id = $1::uuid`, named.Artifact.ID)
			}
			held, before := rig.documentText(t, named.Artifact.ID), versions()
			run := rig.ran(t, "edit the draft beside notes holding "+other.holding, goingChange("dispatch_doc_edit", map[string]any{
				"issue": issue, "artifact": edited.Artifact.Slug, "ops": replaceText("first line", "opening line"),
			}))
			line := rig.undone(t, run, "dispatch_doc_edit")[0]
			if text := rig.documentText(t, edited.Artifact.ID); !line.Outcome.OK || line.Item != edited.Artifact.ID || !strings.Contains(text, "The first line.") {
				t.Fatalf("Undo of the edit answered %+v and the edited document reads %q, want %s written back to its first line", line, text, edited.Artifact.ID)
			}
			if text, after := rig.documentText(t, named.Artifact.ID), versions(); text != held || after != before {
				t.Fatalf("after Undo the document whose slug is the edited one's name reads %q at %d versions, want it untouched: %q at %d", text, after, held, before)
			}
		})
	}
}

// TestAnEditsUndoThatItsRecordSettlesWritesNothingBack: the undo of a document edit writes back
// the content of the read its row was approved over. Where the approval holds no read, holds a
// read whose token is not the one the edit's route answered it replaced, or holds a read that does
// not decode, what the edit replaced is unknown: Undo answers 409 GOING_UNDO_BEFORE_UNREAD, in
// words that say which of the three it is, and writes nothing back. Before, all three said another
// writer changed the document, which is true of the second alone. An edit whose landed request no
// row of its tool sends is settled before the document's undo is reached, by the check that names
// a request sent before a call's own (goingUndoer.plan): it is named sent, not undone, and nothing
// is written back. History offers Undo on none of them.
func TestAnEditsUndoThatItsRecordSettlesWritesNothingBack(t *testing.T) {
	t.Parallel()
	rig := newGoingPlanRig(t)
	unread := func(why string) goingWriteOutcome {
		return goingWriteOutcome{Status: http.StatusConflict, Code: codeGoingUndoBeforeUnread,
			Message: why + ", so what it replaced is unknown and nothing was written back: read the document to see what it holds"}
	}
	for _, each := range []struct {
		name  string
		shape string
		undo  goingUndoKind
		want  goingWriteOutcome
	}{
		{"an approval that holds no read", `update going_plan_rows set approval = null where run_id = $1::uuid`,
			goingUndoWriteBack, unread("the edit's approval holds no read of the document")},
		{"a read whose token is not the one the edit replaced",
			`update going_plan_rows set approval = jsonb_set(approval, '{seen,token}', '"another token"') where run_id = $1::uuid`,
			goingUndoWriteBack, unread("another writer changed the document between the read the edit was approved over and the edit")},
		{"a read that does not decode",
			`update going_plan_rows set approval = jsonb_set(approval, '{seen,now}', '"not a document"') where run_id = $1::uuid`,
			goingUndoWriteBack, unread("the read the edit was approved over does not hold the document's content")},
		{"a landing no row of the tool sends", `update going_landings set method = 'PATCH' where run_id = $1::uuid and phase = 'do'`,
			goingUndoNotUndone, goingWriteOutcome{OK: true, Status: http.StatusOK, Message: goingSentNotUndone}},
	} {
		t.Run(each.name, func(t *testing.T) {
			issue := rig.issue(t, "Holds notes whose edit has "+each.name)
			notes := uploadDocument(t, rig.mux, "/api/v1/issues/"+issue, "notes.md", "# Notes\n\nThe first line.\n")
			run := rig.ran(t, "edit the notes whose edit has "+each.name, goingChange("dispatch_doc_edit", map[string]any{
				"issue": issue, "artifact": notes.Artifact.Slug, "ops": replaceText("first line", "opening line"),
			}))
			rig.exec(t, each.shape, run)
			versions := func() int {
				return rig.count(t, `select count(*) from artifact_versions where artifact_id = $1::uuid`, notes.Artifact.ID)
			}
			before := versions()
			if response := dispatchRequest(t, rig.mux, http.MethodGet, "/api/v1/going/runs/"+run, nil, "alice"); response.Code != http.StatusOK || decodeBody[goingRun](t, response).Undoable {
				t.Fatalf("before Undo the run answered %d %s, want it not undoable", response.Code, response.Body.String())
			}
			line := rig.undone(t, run, "dispatch_doc_edit")[0]
			if line.Undo != each.undo || line.Outcome != each.want {
				t.Fatalf("Undo of the edit answered %+v, want the undo %s answering %+v", line, each.undo, each.want)
			}
			for _, field := range line.Fields {
				if field.OK != each.want.OK || field.Message != each.want.Message {
					t.Fatalf("the field %q answered %+v, want the line's outcome %+v", field.Field, field, each.want)
				}
			}
			if text, after := rig.documentText(t, notes.Artifact.ID), versions(); !strings.Contains(text, "The opening line.") || after != before {
				t.Fatalf("after Undo the document reads %q at %d versions, want the edit kept at %d: nothing written back", text, after, before)
			}
			if landed := rig.count(t, `select count(*) from going_landings where run_id = $1::uuid and phase = 'undo'`, run); landed != 0 {
				t.Fatalf("Undo landed %d requests, want none", landed)
			}
		})
	}
}

// TestARowThatAddsALabelKeepsTheOthersAndItsUndoPutsThemBack: a plan row of dispatch_issue_update
// with labels_add, proposed beside a priority as the preview's "add the label accessibility and set
// its priority to P2" was, reads the field labels with the labels the issue holds; once it runs,
// the issue holds those labels and the one added, and its Undo puts back the labels it held and
// its priority. Before, the tool offered labels alone, which replaced the set, so the row removed
// the label it did not name. The person's edit of the labels on such a row, the whole set the
// label picker closed on, takes labels_add's place and writes that set.
func TestARowThatAddsALabelKeepsTheOthersAndItsUndoPutsThemBack(t *testing.T) {
	t.Parallel()
	rig := newGoingPlanRig(t)
	labelled := func(t *testing.T, title string) string {
		t.Helper()
		issue := rig.issue(t, title)
		rig.fixture(t, "alice", http.MethodPatch, "/api/v1/issues/"+issue, map[string]any{"labels": []string{"board", "api"}})
		return issue
	}
	planned := func(t *testing.T, words string, issue string) (string, goingPlanAnswer) {
		t.Helper()
		rig.loop.script.set(words, func(n int) agenttest.Reply {
			if n == 1 {
				return proposing(goingChange("dispatch_issue_update", map[string]any{"issue": issue, "labels_add": []any{"accessibility"}, "priority": 2}))
			}
			return answered("Done.")
		})
		run := rig.begin(t, goingLoopRun{login: "alice", words: words})
		return run, rig.waiting(t, run)
	}

	t.Run("it adds the label, keeps the others, and its Undo puts them back", func(t *testing.T) {
		issue := labelled(t, "Labelled twice")
		run, plan := planned(t, "add the label accessibility to Labelled twice and set its priority to P2", issue)
		var now struct {
			Labels []string `json:"labels"`
		}
		if row := plan.Rows[0]; !slices.Equal(row.Fields, []string{"labels", "priority"}) || json.Unmarshal(row.Now, &now) != nil || !slices.Equal(now.Labels, []string{"board", "api"}) {
			t.Fatalf("the row reads the fields %v holding %s, want labels and priority, with the labels board and api", row.Fields, row.Now)
		}
		rig.answer(t, run, "approve", map[string]any{"plan": 1})
		if record := rig.ended(t, run, time.Minute); record.state != "done" {
			t.Fatalf("the run ended %s with %q, want done", record.state, record.reason)
		}
		if after := rig.issueRead(t, issue); !slices.Equal(after.Labels, []string{"board", "api", "accessibility"}) || after.Priority == nil || *after.Priority != 2 {
			t.Fatalf("after the row ran the issue holds the labels %v and priority %v, want board, api and accessibility at P2", after.Labels, after.Priority)
		}
		line := rig.undone(t, run, "dispatch_issue_update")[0]
		if !line.Outcome.OK {
			t.Fatalf("Undo of the row answered %+v, want it written back", line)
		}
		if after := rig.issueRead(t, issue); !slices.Equal(after.Labels, []string{"board", "api"}) || after.Priority != nil {
			t.Fatalf("after Undo the issue holds the labels %v and priority %v, want board and api with no priority", after.Labels, after.Priority)
		}
	})

	t.Run("an edit of its labels writes the set the person chose", func(t *testing.T) {
		issue := labelled(t, "Labelled by hand")
		run, _ := planned(t, "add the label accessibility to Labelled by hand and set its priority to P2", issue)
		rig.answer(t, run, "approve", map[string]any{"plan": 1, "rows": []map[string]any{
			{"row": 0, "tick": true, "values": map[string]any{"labels": []any{"board", "accessibility"}}},
		}})
		if record := rig.ended(t, run, time.Minute); record.state != "done" {
			t.Fatalf("the run ended %s with %q, want done", record.state, record.reason)
		}
		if labels := rig.issueRead(t, issue).Labels; !slices.Equal(labels, []string{"board", "accessibility"}) {
			t.Fatalf("after the edited row ran the issue holds the labels %v, want the set the person chose, board and accessibility", labels)
		}
	})
}

// textOf is the one text query answers off the store.
func (rig goingPlanRig) textOf(t *testing.T, query string, arguments ...any) string {
	t.Helper()
	var text string
	if err := rig.database.Pool.QueryRow(context.Background(), query, arguments...).Scan(&text); err != nil {
		t.Fatalf("%s: %v", query, err)
	}
	return text
}

// undoneOnce runs change as alice's approved plan, presses Undo on it and answers its one line,
// failing t unless the run's read offered Undo before the press (undoable, decided from the part
// of each answer the read takes, goingRunUndoColumns), the line says the change was undone, and
// its undo landed - the run's read then counts it taken back: so every stated undo the cases run
// holds the run's read to the undo's own decision, and its route records the landing a second
// press, and the run's counts, read whether it landed by.
func (rig goingPlanRig) undoneOnce(t *testing.T, words string, change map[string]any) goingUndoLine {
	t.Helper()
	tool, _ := change["tool"].(string)
	run := rig.ran(t, words, change)
	response := dispatchRequest(t, rig.mux, http.MethodGet, "/api/v1/going/runs/"+run, nil, "alice")
	if response.Code != http.StatusOK || !decodeBody[goingRun](t, response).Undoable {
		t.Fatalf("before Undo the run of %s answered %d %s, want it undoable", tool, response.Code, response.Body.String())
	}
	line := rig.undone(t, run, tool)[0]
	if !line.Outcome.OK || line.Outcome.Message == goingUndoChangedNothing {
		t.Fatalf("Undo of %s answered %+v, want it undone", tool, line)
	}
	response = dispatchRequest(t, rig.mux, http.MethodGet, "/api/v1/going/runs/"+run, nil, "alice")
	if response.Code != http.StatusOK || goingUndoCountsOf(decodeBody[goingRun](t, response)) != (goingUndoCounts{undone: 1}) {
		t.Fatalf("after Undo the run of %s answered %d %s, want its one change counted taken back", tool, response.Code, response.Body.String())
	}
	return line
}

// thread posts a thread on issue as alice, resolved where resolved is true, and answers its id.
func (rig goingPlanRig) thread(t *testing.T, issue, body string, resolved bool) string {
	t.Helper()
	comment := decodeBody[model.Comment](t, rig.fixture(t, "alice", http.MethodPost, "/api/v1/issues/"+issue+"/comments", map[string]any{"body": body})).ID
	if resolved {
		rig.fixture(t, "alice", http.MethodPost, "/api/v1/comments/"+comment+"/resolve", nil)
	}
	return comment
}

// goingUndoRuns is, by write tool, one run of the tool and its Undo: each makes what the tool
// writes, runs one change of it as alice's approved plan, presses Undo and fails t unless the
// item, read off the store or its route, holds what it held before the change.
var goingUndoRuns = map[string]func(t *testing.T, rig goingPlanRig){
	"dispatch_issue": func(t *testing.T, rig goingPlanRig) {
		line := rig.undoneOnce(t, "file Made and undone", goingChange("dispatch_issue", map[string]any{"project": "DEMO", "title": "Made and undone", "force": true,
			"spec": "An issue the run files and its Undo closes.\n"}))
		if status := rig.issueRead(t, line.Item).Status; status != "done" {
			t.Fatalf("the issue the run made is %s after Undo, want it closed", status)
		}
	},
	"dispatch_issue_update": func(t *testing.T, rig goingPlanRig) {
		issue := rig.issue(t, "Retitled and undone")
		rig.undoneOnce(t, "retitle Retitled and undone", goingChange("dispatch_issue_update", retitle(issue, "Retitled by the run")))
		if title := rig.title(t, issue); title != "Retitled and undone" {
			t.Fatalf("the issue is titled %q after Undo, want its title back", title)
		}
	},
	"dispatch_claim": func(t *testing.T, rig goingPlanRig) {
		issue := rig.issue(t, "Claimed and undone")
		rig.undoneOnce(t, "claim Claimed and undone", goingChange("dispatch_claim", map[string]any{"issue": issue}))
		if claim := rig.issueRead(t, issue).Claim; claim != nil {
			t.Fatalf("the issue is held by %+v after Undo, want nobody", claim)
		}
	},
	"dispatch_ask": func(t *testing.T, rig goingPlanRig) {
		issue := rig.issue(t, "Asked and undone")
		line := rig.undoneOnce(t, "ask on Asked and undone", goingChange("dispatch_ask", map[string]any{"issue": issue, "question": "Ship it as it stands?"}))
		if state := rig.textOf(t, `select state from asks where id = $1::uuid`, line.Item); state != "resolved" {
			t.Fatalf("the ask the run opened is %s after Undo, want it retracted", state)
		}
	},
	"dispatch_edit_ask": func(t *testing.T, rig goingPlanRig) {
		ask := rig.ask(t, rig.issue(t, "Holds an ask to edit"), "Ship it on Friday?")
		rig.undoneOnce(t, "move the ask to Monday", goingChange("dispatch_edit_ask", map[string]any{"ask": ask, "question": "Ship it on Monday?"}))
		if question := rig.textOf(t, `select question from asks where id = $1::uuid`, ask); question != "Ship it on Friday?" {
			t.Fatalf("the ask asks %q after Undo, want its question back", question)
		}
	},
	"dispatch_resolve_comment": func(t *testing.T, rig goingPlanRig) {
		comment := rig.thread(t, rig.issue(t, "Holds a thread to resolve"), "A thread the run resolves.", false)
		rig.undoneOnce(t, "resolve the thread", goingChange("dispatch_resolve_comment", map[string]any{"comment": comment}))
		if resolved := rig.textOf(t, `select resolved::text from comments where id = $1::uuid`, comment); resolved != "false" {
			t.Fatalf("the thread is resolved %s after Undo, want it open again", resolved)
		}
	},
	"dispatch_reopen_comment": func(t *testing.T, rig goingPlanRig) {
		comment := rig.thread(t, rig.issue(t, "Holds a thread to reopen"), "A thread the run reopens.", true)
		rig.undoneOnce(t, "reopen the thread", goingChange("dispatch_reopen_comment", map[string]any{"comment": comment}))
		if resolved := rig.textOf(t, `select resolved::text from comments where id = $1::uuid`, comment); resolved != "true" {
			t.Fatalf("the thread is resolved %s after Undo, want it resolved again", resolved)
		}
	},
	"dispatch_suggest": func(t *testing.T, rig goingPlanRig) {
		issue := decodeBody[model.Issue](t, rig.fixture(t, "alice", http.MethodPost, "/api/v1/issues", map[string]any{
			"project": "DEMO", "title": "Suggested on and undone", "spec": "The spec says hello to everyone.\n", "force": true,
		})).Key
		line := rig.undoneOnce(t, "suggest hi on Suggested on and undone",
			goingChange("dispatch_suggest", map[string]any{"issue": issue, "artifact": "spec", "quote": "says hello", "replace_with": "says hi"}))
		if accepted := rig.textOf(t, `select coalesce(suggestion->>'accepted', 'open') from comments where id = $1::uuid`, line.Item); accepted != "false" {
			t.Fatalf("the suggestion the run posted is %s after Undo, want it rejected", accepted)
		}
	},
	"dispatch_doc_edit": func(t *testing.T, rig goingPlanRig) {
		issue := rig.issue(t, "Holds notes edited and undone")
		notes := uploadDocument(t, rig.mux, "/api/v1/issues/"+issue, "notes.md", "# Notes\n\nThe first line.\n")
		rig.undoneOnce(t, "edit the notes and undo it", goingChange("dispatch_doc_edit", map[string]any{
			"issue": issue, "artifact": notes.Artifact.Slug, "ops": replaceText("first line", "opening line"),
		}))
		if text := rig.documentText(t, notes.Artifact.ID); !strings.Contains(text, "The first line.") {
			t.Fatalf("the document reads %q after Undo, want the first line back", text)
		}
	},
	"dispatch_request_approval": func(t *testing.T, rig goingPlanRig) {
		issue := decodeBody[model.Issue](t, rig.fixture(t, "alice", http.MethodPost, "/api/v1/issues", map[string]any{
			"project": "DEMO", "title": "Asks approval and undoes it", "spec": "The spec to approve.\n", "force": true,
		})).Key
		line := rig.undoneOnce(t, "ask for the spec's approval", goingChange("dispatch_request_approval", map[string]any{
			"issue": issue, "artifact": "spec", "summary": "Ready for review.",
		}))
		if state := rig.textOf(t, `select state from asks where id = $1::uuid`, line.Item); state != "resolved" {
			t.Fatalf("the approval request's ask is %s after Undo, want it retracted", state)
		}
	},
	"dispatch_edit_comment": goingPostCases[0].undoneEdit,
	"dispatch_edit_message": goingPostCases[1].undoneEdit,
}

// goingPostCase is one kind of post the bar's four correction tools edit and delete: its kind, the
// one argument that names it, its route's path, its edit and delete tools, the read tool that reads
// it by that argument, the refusal its route answers an edit whose condition it no longer meets,
// and how the rig makes one of alice's on a new issue, answering its id and the issue.
type goingPostCase struct {
	kind, path, edit, delete, read, changed string
	make                                    func(t *testing.T, rig goingPlanRig, body string) (string, string)
}

// goingPostCases are a comment and an issue message, in that order.
var goingPostCases = []goingPostCase{
	{kind: "comment", path: "/api/v1/comments/", edit: "dispatch_edit_comment", delete: "dispatch_delete_comment", read: "dispatch_read_comment",
		changed: codeCommentChanged, make: func(t *testing.T, rig goingPlanRig, body string) (string, string) {
			issue := rig.issue(t, "Holds a comment of alice's")
			return rig.thread(t, issue, body, false), issue
		}},
	{kind: "message", path: "/api/v1/messages/", edit: "dispatch_edit_message", delete: "dispatch_delete_message", read: "dispatch_read",
		changed: codeMessageChanged, make: func(t *testing.T, rig goingPlanRig, body string) (string, string) {
			issue := rig.issue(t, "Holds a message of alice's")
			return createIssueMessage(t, rig.mux, issue, map[string]any{"body": body}, "alice").ID, issue
		}},
}

// body is the body the post id holds now, off the store.
func (c goingPostCase) body(t *testing.T, rig goingPlanRig, id string) string {
	t.Helper()
	return rig.textOf(t, `select body from `+c.kind+`s where id = $1::uuid`, id)
}

// editing is the change of a plan that edits the post id to body.
func (c goingPostCase) editing(id, body string) map[string]any {
	return goingChange(c.edit, map[string]any{c.kind: id, "body": body})
}

// deleting is the change of a plan that deletes the post id.
func (c goingPostCase) deleting(id string) map[string]any {
	return goingChange(c.delete, map[string]any{c.kind: id})
}

// remove deletes the post id as alice, off the bar.
func (c goingPostCase) remove(t *testing.T, rig goingPlanRig, id string) {
	t.Helper()
	rig.fixture(t, "alice", http.MethodDelete, c.path+id, nil)
}

// undoneEdit is goingUndoRuns' case of the post's edit: Undo writes back the body the edit
// replaced.
func (c goingPostCase) undoneEdit(t *testing.T, rig goingPlanRig) {
	id, _ := c.make(t, rig, "The ferry leaves at noon.")
	rig.undoneOnce(t, "move the ferry in the "+c.kind+" to one", c.editing(id, "The ferry leaves at one."))
	if body := c.body(t, rig, id); body != "The ferry leaves at noon." {
		t.Fatalf("the %s reads %q after Undo, want the body the edit replaced", c.kind, body)
	}
}

// TestAnEditOfAPostIsUndoneUnlessItChangedSince (R6, the architect's ruling of 12:50Z): Undo of a
// run's edit of a comment or an issue message writes the body it replaced back while the post holds
// the body the run wrote. A body its author wrote after the run is kept, and the line names the
// author as its writer and holds that body; a body written between Undo's read and its write is
// kept too, the route refusing the write back with its own code (COMMENT_CHANGED or
// MESSAGE_CHANGED); and a post its author deleted after the run is named deleted, by them, and no
// word of its body is in Undo's answer. Each kept change stands in the run's counts.
func TestAnEditOfAPostIsUndoneUnlessItChangedSince(t *testing.T) {
	t.Parallel()
	rig := newGoingPlanRig(t)
	alice := goingHolder{Kind: "user", ID: "alice"}
	standing := func(t *testing.T, run string) {
		t.Helper()
		response := dispatchRequest(t, rig.mux, http.MethodGet, "/api/v1/going/runs/"+run, nil, "alice")
		if got := decodeBody[goingRun](t, response); got.Undoable || goingUndoCountsOf(got) != (goingUndoCounts{standing: 1}) {
			t.Fatalf("after Undo the run reads undoable %v, counts %+v; want it not undoable, its one change standing", got.Undoable, goingUndoCountsOf(got))
		}
	}
	for _, c := range goingPostCases {
		t.Run("a "+c.kind+" its author edited again keeps that body and names them", func(t *testing.T) {
			id, _ := c.make(t, rig, "The ferry leaves at noon.")
			run := rig.ran(t, "move the ferry in the "+c.kind+" to one", c.editing(id, "The ferry leaves at one."))
			rig.fixture(t, "alice", http.MethodPatch, c.path+id, map[string]any{"body": "The ferry leaves at two."})
			line := rig.undone(t, run, c.edit)[0]
			if line.Outcome.OK || line.Outcome.Code != codeGoingUndoChanged || len(line.Fields) != 1 {
				t.Fatalf("Undo answered %+v, want the body kept, %s", line, codeGoingUndoChanged)
			}
			field := line.Fields[0]
			if field.Field != "body" || field.OK || string(field.Now) != `"The ferry leaves at two."` || field.By == nil || *field.By != alice {
				t.Fatalf("the body answered %+v, want it kept holding the body alice wrote since, naming her", field)
			}
			if body := c.body(t, rig, id); body != "The ferry leaves at two." {
				t.Fatalf("after Undo the %s reads %q, want alice's later body kept", c.kind, body)
			}
			standing(t, run)
		})

		t.Run("a "+c.kind+" edited between Undo's read and its write is kept by its route", func(t *testing.T) {
			id, _ := c.make(t, rig, "The ferry leaves at noon.")
			run := rig.ran(t, "move the ferry in the raced "+c.kind+" to one", c.editing(id, "The ferry leaves at one."))
			var writes atomic.Int32
			rig.hooks.set(t, http.MethodPatch, c.path+id, func(w http.ResponseWriter, r *http.Request, inner http.Handler) {
				// alice edits the post after the undo's read and before its write back.
				if writes.Add(1) != 1 {
					inner.ServeHTTP(w, r)
					return
				}
				if response := dispatchRequest(t, rig.mux, http.MethodPatch, c.path+id, map[string]any{"body": "The ferry leaves at three."}, "alice"); response.Code >= http.StatusMultipleChoices {
					t.Errorf("alice's own edit answered %d %s", response.Code, response.Body.String())
				}
				answer := httptest.NewRecorder()
				inner.ServeHTTP(answer, r)
				if code := goingAnswerCode(t, answer.Body.String()); answer.Code != http.StatusConflict || code != c.changed {
					t.Errorf("the undo PATCH answered %d %s, want 409 %s", answer.Code, code, c.changed)
				}
				maps.Copy(w.Header(), answer.Header())
				w.WriteHeader(answer.Code)
				_, _ = w.Write(answer.Body.Bytes())
			})
			line := rig.undone(t, run, c.edit)[0]
			if line.Outcome.OK || line.Outcome.Code != codeGoingUndoChanged {
				t.Fatalf("Undo answered %+v, want the body kept on the route's %s", line, c.changed)
			}
			if body := c.body(t, rig, id); body != "The ferry leaves at three." {
				t.Fatalf("after Undo the %s reads %q, want the body alice wrote mid-undo kept", c.kind, body)
			}
			standing(t, run)
		})

		t.Run("a "+c.kind+" its author deleted is named deleted with no word of it", func(t *testing.T) {
			const word = "capstanword"
			id, _ := c.make(t, rig, "The "+word+" turns at noon.")
			run := rig.ran(t, "move the "+word+" in the "+c.kind+" to one", c.editing(id, "The "+word+" turns at one."))
			c.remove(t, rig, id)
			response := dispatchRequest(t, rig.mux, http.MethodPost, "/api/v1/going/runs/"+run+"/undo", nil, "alice")
			if response.Code != http.StatusOK || strings.Contains(response.Body.String(), word) {
				t.Fatalf("Undo answered %d %s, want 200 with no word of the deleted %s", response.Code, response.Body.String(), c.kind)
			}
			lines := decodeBody[goingUndoAnswer](t, response).Lines
			if len(lines) != 1 || lines[0].Outcome.Code != codeGoingUndoChanged || len(lines[0].Fields) != 1 {
				t.Fatalf("Undo answered %+v, want one kept field", lines)
			}
			field := lines[0].Fields[0]
			if field.Field != "body" || field.OK || field.Now != nil || field.By == nil || *field.By != alice {
				t.Fatalf("Undo answered %+v, want the body kept with no text and Alice as its deleter", field)
			}
			if deleted := rig.textOf(t, `select (deleted_at is not null)::text from `+c.kind+`s where id = $1::uuid`, id); deleted != "true" {
				t.Fatalf("after Undo the %s is deleted %s, want it left deleted", c.kind, deleted)
			}
			standing(t, run)
		})
	}
}

// TestADeleteOfAPostIsNotUndone (R6, the architect's ruling of 12:50Z): a run's delete of a comment
// or an issue message is undone by nothing - nothing restores a deleted post - so Undo's line names
// it "deleted, not undone", sends nothing, and the run counts it in no count, before Undo and
// after it, and is never undoable.
func TestADeleteOfAPostIsNotUndone(t *testing.T) {
	t.Parallel()
	rig := newGoingPlanRig(t)
	never := func(t *testing.T, run, when string) {
		t.Helper()
		got := decodeBody[goingRun](t, dispatchRequest(t, rig.mux, http.MethodGet, "/api/v1/going/runs/"+run, nil, "alice"))
		if got.Undoable || got.UndoneAt != nil || goingUndoCountsOf(got) != (goingUndoCounts{}) {
			t.Fatalf("%s the run reads undoable %v, undone at %v, counts %+v; want it never undoable and in no count", when, got.Undoable, got.UndoneAt, goingUndoCountsOf(got))
		}
	}
	for _, c := range goingPostCases {
		t.Run("a "+c.kind, func(t *testing.T) {
			id, _ := c.make(t, rig, "The ferry leaves at noon.")
			run := rig.ran(t, "delete the "+c.kind+" of the ferry", c.deleting(id))
			never(t, run, "before Undo")
			line := rig.undone(t, run, c.delete)[0]
			if line.Undo != goingUndoNotUndone || line.Outcome != (goingWriteOutcome{OK: true, Status: http.StatusOK, Message: goingDeletedNotUndone}) {
				t.Fatalf("Undo answered %+v, want %q", line, goingDeletedNotUndone)
			}
			if landed := rig.count(t, `select count(*) from going_landings where run_id = $1::uuid and phase = 'undo'`, run); landed != 0 {
				t.Fatalf("Undo landed %d requests, want none", landed)
			}
			never(t, run, "after Undo")
		})
	}
}

// TestADeployRecordWaitsAndIsNotUndone (the architect's ruling on dispatch_deploy, R5, R6, R12): a
// run's record of a deploy is a row of its plan that waits for the person in Auto-review, as a
// check result does, and reads the commit and the URL it records beside the program's current
// deploy, with nothing sent. Once approved it lands POST /api/v1/deploys with the call's body, the
// command's own print flag (json) left out, and the record is the program's current deploy. Undo
// names it "recorded, not undone", sends nothing, and the run counts it in no count.
func TestADeployRecordWaitsAndIsNotUndone(t *testing.T) {
	t.Parallel()
	rig := newGoingPlanRig(t)
	current, recorded := strings.Repeat("c", 40), strings.Repeat("d", 40)
	rig.fixture(t, "alice", http.MethodPost, "/api/v1/deploys", map[string]any{"repo": "acme/arch", "program": "worker", "commit": current})
	body := map[string]any{"repo": "acme/arch", "program": "worker", "commit": recorded, "url": "https://worker.example.com"}
	call := maps.Clone(body)
	call["json"] = true
	const words = "record the worker's deploy"
	rig.loop.script.set(words, func(n int) agenttest.Reply {
		switch n {
		case 1:
			return proposing(goingChange("dispatch_deploy", call))
		case 2:
			return toolCall(toolReviewPlanName, map[string]any{"asked": []int{0}})
		}
		return answered("The worker's deploy is recorded.")
	})
	var mu sync.Mutex
	var sent [][]byte
	rig.hooks.set(t, http.MethodPost, "/api/v1/deploys", func(w http.ResponseWriter, r *http.Request, inner http.Handler) {
		read, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read the deploy's body: %v", err)
		}
		mu.Lock()
		sent = append(sent, read)
		mu.Unlock()
		r.Body = io.NopCloser(bytes.NewReader(read))
		inner.ServeHTTP(w, r)
	})
	sends := func() [][]byte {
		mu.Lock()
		defer mu.Unlock()
		return slices.Clone(sent)
	}
	currentCommit := func() string {
		return rig.textOf(t, `select commit_sha from deploys where repo = 'acme/arch' and program = 'worker' order by id desc limit 1`)
	}
	never := func(run, when string) {
		t.Helper()
		got := decodeBody[goingRun](t, dispatchRequest(t, rig.mux, http.MethodGet, "/api/v1/going/runs/"+run, nil, "alice"))
		if got.Undoable || goingUndoCountsOf(got) != (goingUndoCounts{}) {
			t.Fatalf("%s the run reads undoable %v, counts %+v; want it never undoable and in no count", when, got.Undoable, goingUndoCountsOf(got))
		}
	}

	run := rig.begin(t, goingLoopRun{login: "alice", words: words, mode: goingModeAutoReview})
	plan := rig.waiting(t, run)
	if len(plan.Rows) != 1 {
		t.Fatalf("the plan holds %d rows, want the deploy's", len(plan.Rows))
	}
	row := plan.Rows[0]
	if row.Tool != "dispatch_deploy" || row.State != "waiting" || row.Class != string(goingClassResult) || !slices.Equal(row.Waits, []string{string(goingClassResult)}) {
		t.Fatalf("the row of %s is %s, of class %s, waiting for %v; want the deploy's row waiting for the person as a %s", row.Tool, row.State, row.Class, row.Waits, goingClassResult)
	}
	if want := `{"commit":"` + current + `","url":""}`; !slices.Equal(row.Fields, []string{"commit", "url"}) || !goingJSONEqual(row.Now, json.RawMessage(want)) {
		t.Fatalf("the row shows the fields %v now %s, want commit and url now %s, the program's current deploy", row.Fields, row.Now, want)
	}
	if bodies, commit := sends(), currentCommit(); len(bodies) != 0 || commit != current {
		t.Fatalf("before the approval the bar sent %d deploys and the current one is %s, want none sent and %s current", len(bodies), commit, current)
	}

	rig.answer(t, run, "approve", map[string]any{"plan": 1})
	if record := rig.ended(t, run, time.Minute); record.state != "done" {
		t.Fatalf("after the person's approval the run ended %s with %q, want done; its calls were told %q", record.state, record.reason, rig.toolResults(t, run))
	}
	want, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	if bodies := sends(); len(bodies) != 1 || !goingJSONEqual(bodies[0], want) {
		t.Fatalf("the approved row sent %d deploys (%q), want one whose body is %s", len(bodies), bodies, want)
	}
	if landed := rig.count(t, `
		select count(*) from going_landings where run_id = $1::uuid and phase = 'do' and method = 'POST' and path = '/api/v1/deploys'`, run); landed != 1 {
		t.Fatalf("the run landed %d records of a deploy, want one", landed)
	}
	if commit := currentCommit(); commit != recorded {
		t.Fatalf("after the approval the current deploy is %s, want the recorded %s", commit, recorded)
	}
	if plan := rig.plan(t, run); plan.Rows[0].State != "done" {
		t.Fatalf("after the approval the deploy's row is %s, want done", plan.Rows[0].State)
	}
	never(run, "before Undo")

	line := rig.undone(t, run, "dispatch_deploy")[0]
	if line.Undo != goingUndoNotUndone || line.Outcome != (goingWriteOutcome{OK: true, Status: http.StatusOK, Message: goingRecordedNotUndone}) {
		t.Fatalf("Undo answered %+v, want %q", line, goingRecordedNotUndone)
	}
	if landed := rig.count(t, `select count(*) from going_landings where run_id = $1::uuid and phase = 'undo'`, run); landed != 0 {
		t.Fatalf("Undo landed %d requests, want none", landed)
	}
	if bodies, commit := sends(), currentCommit(); len(bodies) != 1 || commit != recorded {
		t.Fatalf("after Undo the bar has sent %d deploys and the current one is %s, want the one approved and %s current", len(bodies), commit, recorded)
	}
	never(run, "after Undo")
}

// goingTakesTheCallingSession reports whether every row of the write tool name fills a
// placeholder of its path with the calling session (goingCallerSession).
func goingTakesTheCallingSession(name string) bool {
	tool := goingWriteTools[name]
	var spec contracts.DispatchTool
	for _, each := range contracts.DispatchTools {
		if each.Name == name {
			spec = each
		}
	}
	rows, _ := goingToolRows(spec, tool.Routes, tool.UndoRoutes)
	for _, row := range rows {
		takes := false
		for _, argument := range row.Path {
			takes = takes || argument == goingCallerSession
		}
		if !takes {
			return false
		}
	}
	return len(rows) > 0
}

// TestEveryStatedUndoRuns (R6): every write tool whose stated undo is not not_undone, whether the
// answer run's catalogue offers it or the agent finds it by find_tools, has that undo run here (by
// goingUndoRuns), and the undo reverses the change: a tool that states one with no case here fails,
// and so does a case of a tool that states none. A tool every row of whose path takes the calling
// session is left out, since a run acts as a person and no session, so no run writes it.
func TestEveryStatedUndoRuns(t *testing.T) {
	t.Parallel()
	var stated []string
	for _, name := range slices.Sorted(maps.Keys(goingWriteTools)) {
		if goingWriteTools[name].Undo != goingUndoNotUndone && !goingTakesTheCallingSession(name) {
			stated = append(stated, name)
		}
	}
	for name := range goingUndoRuns {
		if !slices.Contains(stated, name) {
			t.Errorf("a case runs the undo of %s, which states no undo a run's change has", name)
		}
	}
	rig := newGoingPlanRig(t)
	for _, name := range stated {
		run, held := goingUndoRuns[name]
		if !held {
			t.Errorf("%s states a %s undo, and no case here runs it", name, goingWriteTools[name].Undo)
			continue
		}
		t.Run(name, func(t *testing.T) { run(t, rig) })
	}
}
