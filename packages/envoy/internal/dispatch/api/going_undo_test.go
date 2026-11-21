// going_undo_test.go holds the undo of one command-bar run to R6: every row of the catalogue
// states its undo, and the undo of a run walks its landed rows newest first, as the person who
// pressed it - writing back what the run left unless another writer changed it since, closing
// what it created, rejecting its open suggestions and naming its messages sent, not undone -
// while History lists the run, and sends nothing twice.
package api

import (
	"context"
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
// undone by nothing, saying why. Undo is offered while History lists the run's conversation, so a
// run 8 days old in a conversation whose newest run is 1 day old is undone, and one whose
// conversation's newest run is 8 days old answers 404. A second press answers each row whose undo
// landed from that landing, sending nothing twice, and resumes where the first press ended.
func TestUndoOfOneRun(t *testing.T) {
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
		if !fields["status"].OK || line.Outcome.Code != codeGoingUndoChanged || !strings.Contains(line.Outcome.Message, "status") {
			t.Fatalf("the line answered %+v, want %s naming the kept title and the status written back", line, codeGoingUndoChanged)
		}
	})

	t.Run("a created issue is closed and holds no message", func(t *testing.T) {
		run := rig.ran(t, "file Made by the run", goingChange("dispatch_issue", map[string]any{"project": "DEMO", "title": "Made by the run", "force": true}))
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
			goingChange("dispatch_suggest", map[string]any{"issue": issue, "quote": "says hello", "replace_with": "says hi"}))
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

	t.Run("a created issue whose undo's process ended resumes at the close", func(t *testing.T) {
		renamed := rig.issue(t, "Renamed beside a create")
		run := rig.ran(t, "file one issue and rename another",
			goingChange("dispatch_issue", map[string]any{"project": "DEMO", "title": "Made before the undo ended", "force": true}),
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
// failing t unless the line says the change was undone.
func (rig goingPlanRig) undoneOnce(t *testing.T, words string, change map[string]any) goingUndoLine {
	t.Helper()
	tool, _ := change["tool"].(string)
	line := rig.undone(t, rig.ran(t, words, change), tool)[0]
	if !line.Outcome.OK {
		t.Fatalf("Undo of %s answered %+v, want it undone", tool, line)
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
		line := rig.undoneOnce(t, "file Made and undone", goingChange("dispatch_issue", map[string]any{"project": "DEMO", "title": "Made and undone", "force": true}))
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
			goingChange("dispatch_suggest", map[string]any{"issue": issue, "quote": "says hello", "replace_with": "says hi"}))
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
