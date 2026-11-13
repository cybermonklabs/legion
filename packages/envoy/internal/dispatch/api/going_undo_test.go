// going_undo_test.go holds the undo of a command-bar turn to R4: every row of the catalogue
// states its undo, each kind of undo puts back what its write changed - or, for a trace, records
// the turn undone and erases nothing - a field another writer changed since is left as it is now
// while the rest is undone, a release is taken back only for the caller's own claim,
// and a run past thirty days takes no undo.
package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// goingUndoKinds is the undo R4 gives each action of the catalogue: a reverse where Dispatch
// serves one, a write back where the write wrote a value, the close of an issue the turn created,
// and a trace for every other write.
var goingUndoKinds = map[string]goingUndoKind{
	"dispatch_issue":             goingUndoCloseCreated,
	"dispatch_issue_update":      goingUndoWriteBack,
	"dispatch_claim":             goingUndoReverse,
	"dispatch_ask":               goingUndoReverse,
	"dispatch_edit_ask":          goingUndoWriteBack,
	"dispatch_resolve_ask":       goingUndoTrace,
	"dispatch_resolve_comment":   goingUndoReverse,
	"dispatch_follow":            goingUndoReverse,
	"dispatch_comment":           goingUndoTrace,
	"dispatch_suggest":           goingUndoTrace,
	"dispatch_message":           goingUndoTrace,
	"dispatch_send_to_agent":     goingUndoTrace,
	"dispatch_doc_edit":          goingUndoWriteBack,
	"dispatch_request_approval":  goingUndoReverse,
	"dispatch_plan_approve":      goingUndoTrace,
	"dispatch_result":            goingUndoTrace,
	"dispatch_artifact":          goingUndoTrace,
	"dispatch_project":           goingUndoTrace,
	"dispatch_architecture_sync": goingUndoTrace,
	"dispatch_handoff_write":     goingUndoTrace,
}

// TestGoingUndoEveryRowStatesItsUndo: every row the builder holds, counted against it, states one
// of the four undos, the one R4 gives its action, the same on every row of one action, and writes
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
			t.Errorf("%s: R4 gives it no undo here", action.Action)
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
			if (route.Undo.Kind == goingUndoTrace) != (len(route.Undo.Routes) == 0) {
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
		t.Fatalf("R4's table names %d actions and the catalogue holds %d", len(goingUndoKinds), len(catalogue.Actions()))
	}
}

// undo undoes login's run through the route and answers its lines.
func (rig goingWritesRig) undo(t *testing.T, login, run string, body any) goingUndoAnswer {
	t.Helper()
	response := rig.browser(t, login, http.MethodPost, "/api/v1/going/runs/"+run+"/undo", body)
	if response.Code != http.StatusOK {
		t.Fatalf("undo %s: status=%d body=%s", run, response.Code, response.Body.String())
	}
	return decodeBody[goingUndoAnswer](t, response)
}

// oneLine is the one line of an undo, which must have gone as ok says.
func oneLine(t *testing.T, answer goingUndoAnswer, ok bool) goingUndoLine {
	t.Helper()
	if len(answer.Lines) != 1 || answer.Lines[0].Outcome.OK != ok {
		t.Fatalf("the undo answered %+v, want one line, ok %v", answer.Lines, ok)
	}
	return answer.Lines[0]
}

// runOne records a run of alice's proposing action with arguments, writes it as the person and
// answers the run's id and the write's line.
func (rig goingWritesRig) runOne(t *testing.T, action string, arguments map[string]any, fill any) (string, goingWriteLine) {
	t.Helper()
	run := rig.proposed(t, "alice", false, action, arguments)
	body := map[string]any{"offer": action, "invocation": 0, "approved": goingApprovedPerson}
	if fill != nil {
		body["fill"] = fill
	}
	return run, rig.wrote(t, "alice", run, body)
}

// issueRead is the issue as its own route reads it.
func (rig goingWritesRig) issueRead(t *testing.T, key string) model.Issue {
	t.Helper()
	return decodeBody[model.Issue](t, rig.fixture(t, "alice", http.MethodGet, "/api/v1/issues/"+key, nil))
}

// TestGoingUndoWritesBackAnIssuesFieldsAnAsksTextAndADocumentsContent: a write_back undo writes
// the value Dispatch read before the write back over it.
func TestGoingUndoWritesBackAnIssuesFieldsAnAsksTextAndADocumentsContent(t *testing.T) {
	rig := newGoingWritesRig(t)
	issue := createInteractionIssue(t, rig.mux, "DEMO", "Written back", "The spec.\n").Key
	before := rig.issueRead(t, issue)

	t.Run("an issue's fields", func(t *testing.T) {
		run, _ := rig.runOne(t, "dispatch_issue_update", map[string]any{"issue": issue, "status": "in_progress", "title": "Renamed by the bar"}, nil)
		if written := rig.issueRead(t, issue); written.Status != "in_progress" || written.Title != "Renamed by the bar" {
			t.Fatalf("the write left the issue %s %q, want it in_progress and renamed", written.Status, written.Title)
		}
		line := oneLine(t, rig.undo(t, "alice", run, nil), true)
		after := rig.issueRead(t, issue)
		if line.Undo != goingUndoWriteBack || after.Status != before.Status || after.Title != before.Title {
			t.Fatalf("the undo answered %+v and the issue reads %s %q, want %s %q back", line, after.Status, after.Title, before.Status, before.Title)
		}
	})
	t.Run("an ask's text", func(t *testing.T) {
		ask := rig.ask(t, issue, "Ship it on Friday?")
		run, _ := rig.runOne(t, "dispatch_edit_ask", map[string]any{"ask": ask, "question": "Ship it on Monday?"}, nil)
		if written := decodeBody[goingAskRead](t, rig.fixture(t, "alice", http.MethodGet, "/api/v1/asks/"+ask, nil)); written.Ask.Question != "Ship it on Monday?" {
			t.Fatalf("the edit left the ask reading %q, want its new words", written.Ask.Question)
		}
		oneLine(t, rig.undo(t, "alice", run, nil), true)
		read := decodeBody[goingAskRead](t, rig.fixture(t, "alice", http.MethodGet, "/api/v1/asks/"+ask, nil))
		if read.Ask.Question != "Ship it on Friday?" {
			t.Fatalf("the ask reads %q after the undo, want its words back", read.Ask.Question)
		}
	})
	t.Run("a document's content", func(t *testing.T) {
		notes := uploadDocument(t, rig.mux, "/api/v1/issues/"+issue, "notes.md", "# Notes\n\nThe first line.\n")
		original := decodeBody[artifactText](t, rig.fixture(t, "alice", http.MethodGet, "/api/v1/artifacts/"+notes.Artifact.ID+"/text", nil)).Markdown
		run, line := rig.runOne(t, "dispatch_doc_edit", map[string]any{
			"issue": issue, "artifact": notes.Artifact.Slug,
			"ops": []any{map[string]any{"op": "replace", "find": "first line", "with": "opening line"}},
		}, nil)
		if line.Item != notes.Artifact.ID {
			t.Fatalf("the edit's line names %q, want the document %s", line.Item, notes.Artifact.ID)
		}
		edited := decodeBody[artifactText](t, rig.fixture(t, "alice", http.MethodGet, "/api/v1/artifacts/"+notes.Artifact.ID+"/text", nil)).Markdown
		if edited == original {
			t.Fatal("the edit changed nothing")
		}
		oneLine(t, rig.undo(t, "alice", run, nil), true)
		restored := decodeBody[artifactText](t, rig.fixture(t, "alice", http.MethodGet, "/api/v1/artifacts/"+notes.Artifact.ID+"/text", nil)).Markdown
		if restored != original {
			t.Fatalf("the document reads %q after the undo, want %q", restored, original)
		}
	})
}

// TestGoingUndoReversesAClaimAFollowACommentResolveAndAnOpenedAsk: a reverse undo runs the write's
// reverse, as the caller: the claim released, the follow unfollowed, the thread reopened, and an
// ask the turn opened - an approval request's among them - retracted.
func TestGoingUndoReversesAClaimAFollowACommentResolveAndAnOpenedAsk(t *testing.T) {
	rig := newGoingWritesRig(t)
	issue := createInteractionIssue(t, rig.mux, "DEMO", "Reversed", "The spec.\n").Key
	askState := func(t *testing.T, id string) string {
		t.Helper()
		return decodeBody[goingAskRead](t, rig.fixture(t, "alice", http.MethodGet, "/api/v1/asks/"+id, nil)).Ask.State
	}

	t.Run("a claim is released", func(t *testing.T) {
		run, _ := rig.runOne(t, "dispatch_claim", map[string]any{"issue": issue}, nil)
		if rig.issueRead(t, issue).Claim == nil {
			t.Fatal("the claim did not land")
		}
		oneLine(t, rig.undo(t, "alice", run, nil), true)
		if claim := rig.issueRead(t, issue).Claim; claim != nil {
			t.Fatalf("the claim is held by %+v after the undo", claim.Actor)
		}
	})
	t.Run("a follow is unfollowed", func(t *testing.T) {
		ask := rig.ask(t, issue, "Which region first?")
		run, _ := rig.runOne(t, "dispatch_follow", map[string]any{"ask": ask, "action": "follow"}, goingPlannerTitle)
		followers := func() []model.AskFollower {
			return decodeBody[askFollowersRead](t, rig.fixture(t, "alice", http.MethodGet, "/api/v1/asks/"+ask+"/followers", nil)).Followers
		}
		if len(followers()) == 0 {
			t.Fatal("the follow did not land")
		}
		oneLine(t, rig.undo(t, "alice", run, nil), true)
		for _, follower := range followers() {
			if follower.SessionID == goingPlanner {
				t.Fatalf("%s still follows after the undo", goingPlanner)
			}
		}
	})
	t.Run("a resolved thread is reopened", func(t *testing.T) {
		comment := decodeBody[model.Comment](t, rig.fixture(t, "alice", http.MethodPost, "/api/v1/issues/"+issue+"/comments", map[string]any{"body": "A thread."})).ID
		run, _ := rig.runOne(t, "dispatch_resolve_comment", map[string]any{"comment": comment}, nil)
		oneLine(t, rig.undo(t, "alice", run, nil), true)
		read := decodeBody[commentRead](t, rig.fixture(t, "alice", http.MethodGet, "/api/v1/comments/"+comment, nil))
		if read.Comment.Resolved {
			t.Fatal("the thread is still resolved after the undo")
		}
	})
	t.Run("an opened ask is retracted", func(t *testing.T) {
		run, line := rig.runOne(t, "dispatch_ask", map[string]any{"issue": issue, "question": "Ship on Friday?"}, nil)
		oneLine(t, rig.undo(t, "alice", run, nil), true)
		if state := askState(t, line.Item); state != "resolved" {
			t.Fatalf("the ask the turn opened is %s after the undo, want resolved", state)
		}
	})
	t.Run("an approval request is retracted", func(t *testing.T) {
		run, line := rig.runOne(t, "dispatch_request_approval", map[string]any{"issue": issue, "artifact": "spec", "summary": "Ready."}, nil)
		oneLine(t, rig.undo(t, "alice", run, nil), true)
		if state := askState(t, line.Item); state != "resolved" {
			t.Fatalf("the approval ask is %s after the undo, want resolved", state)
		}
	})
}

// TestGoingUndoClosesAnIssueTheTurnCreatedWithTheReason: an issue the turn created is closed by its
// undo, the undo's reason posted on it as a message first.
func TestGoingUndoClosesAnIssueTheTurnCreatedWithTheReason(t *testing.T) {
	rig := newGoingWritesRig(t)
	createInteractionIssue(t, rig.mux, "DEMO", "An older issue", "The spec.\n")
	run, line := rig.runOne(t, "dispatch_issue", map[string]any{"project": "DEMO", "title": "Made by mistake", "force": true}, nil)
	const reason = "Filed by mistake from the bar."
	oneLine(t, rig.undo(t, "alice", run, map[string]any{"reason": reason}), true)
	if status := rig.issueRead(t, line.Item).Status; status != "done" {
		t.Fatalf("the created issue is %s after the undo, want done", status)
	}
	threads := decodeBody[[]messageRead](t, rig.fixture(t, "alice", http.MethodGet, "/api/v1/issues/"+line.Item+"/messages", nil))
	if len(threads) == 0 || threads[len(threads)-1].Message.Body != reason {
		t.Fatalf("the created issue's messages are %+v, want the reason posted", threads)
	}
}

// TestGoingUndoOfACreatedIssueResumesAfterTheReasonItPosted (review round 3, finding 1, in the
// undo): an undo that closes an issue the turn created posts its reason and then closes it. Where
// the reason landed and the close was refused 503, 408 or 429, the undo is recorded refused, with
// the reason recorded landed, and stays undoable; the next undo resumes at the close, posting no
// second reason, and closes the issue.
func TestGoingUndoOfACreatedIssueResumesAfterTheReasonItPosted(t *testing.T) {
	rig := newGoingWritesRig(t)
	createInteractionIssue(t, rig.mux, "DEMO", "An older issue", "The spec.\n")
	inner := rig.served.inProcess
	t.Cleanup(func() { rig.served.inProcess = inner })
	for _, tc := range []struct {
		status int
		code   string
	}{
		{http.StatusServiceUnavailable, "SERVICE_UNAVAILABLE"},
		{http.StatusRequestTimeout, "REQUEST_TIMEOUT"},
		{http.StatusTooManyRequests, "RATE_LIMITED"},
	} {
		t.Run(tc.code, func(t *testing.T) {
			run, line := rig.runOne(t, "dispatch_issue", map[string]any{"project": "DEMO", "title": "Made by mistake " + tc.code, "force": true}, nil)
			reason := "Filed by mistake, " + tc.code + "."
			refused := false
			rig.served.inProcess = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodPatch && r.URL.Path == "/api/v1/issues/"+line.Item && !refused {
					refused = true
					w.Header().Set("Retry-After", "1")
					writeError(w, tc.code, tc.status, "the close was refused")
					return
				}
				inner.ServeHTTP(w, r)
			})
			first := oneLine(t, rig.undo(t, "alice", run, map[string]any{"reason": reason}), false)
			if first.Outcome.Status != tc.status {
				t.Fatalf("the undo whose close was refused answered %+v, want the route's %d", first.Outcome, tc.status)
			}
			oneLine(t, rig.undo(t, "alice", run, map[string]any{"reason": reason}), true)
			rig.served.inProcess = inner
			if status := rig.issueRead(t, line.Item).Status; status != "done" {
				t.Fatalf("the created issue is %s after the second undo, want done", status)
			}
			var posted int
			if err := rig.database.Pool.QueryRow(context.Background(),
				`select count(*) from messages where issue_key = $1 and body = $2`, line.Item, reason).Scan(&posted); err != nil || posted != 1 {
				t.Fatalf("the created issue holds %d messages saying %q (%v), want the reason posted once", posted, reason, err)
			}
			// Read off the stored record itself, how each request of each undo went.
			var undos []struct {
				Outcome  goingWriteOutcome `json:"outcome"`
				Requests []struct {
					Outcome goingWriteOutcome `json:"outcome"`
				} `json:"requests"`
			}
			if err := rig.database.Pool.QueryRow(context.Background(), `select undos from going_runs where id = $1::uuid`, run).Scan(&undos); err != nil {
				t.Fatalf("read the run's undos: %v", err)
			}
			if len(undos) != 2 || undos[0].Outcome.OK || len(undos[0].Requests) != 2 || !undos[0].Requests[0].Outcome.OK ||
				!undos[1].Outcome.OK || len(undos[1].Requests) != 2 || !undos[1].Requests[1].Outcome.OK {
				t.Fatalf("the run keeps the undos %+v, want the refused undo with its reason landed, then the undo that closed it", undos)
			}
		})
	}
}

// TestGoingUndoOfATraceLeavesTheWriteInPlaceAndMarksItUndone: a comment, a message and an artifact
// the turn made stay where they are; the undo records the turn undone, so the run is no longer
// undoable.
func TestGoingUndoOfATraceLeavesTheWriteInPlaceAndMarksItUndone(t *testing.T) {
	rig := newGoingWritesRig(t)
	issue := createInteractionIssue(t, rig.mux, "DEMO", "Traced", "The spec.\n").Key
	for _, write := range []struct {
		action    string
		arguments map[string]any
		read      string
	}{
		{"dispatch_comment", map[string]any{"issue": issue, "body": "Stays."}, "/api/v1/comments/"},
		{"dispatch_message", map[string]any{"issue": issue, "body": "Stays too."}, "/api/v1/messages/"},
		{"dispatch_artifact", map[string]any{"issue": issue, "name": "kept.md", "content": "# Kept\n"}, "/api/v1/artifacts/"},
	} {
		t.Run(write.action, func(t *testing.T) {
			run, line := rig.runOne(t, write.action, write.arguments, nil)
			undone := oneLine(t, rig.undo(t, "alice", run, nil), true)
			if undone.Undo != goingUndoTrace {
				t.Fatalf("the undo of %s is %s, want a trace", write.action, undone.Undo)
			}
			if read := dispatchRequest(t, rig.mux, http.MethodGet, write.read+line.Item, nil, "alice"); read.Code != http.StatusOK {
				t.Fatalf("what %s made reads %d after the undo, want it in place", write.action, read.Code)
			}
			if again := rig.undo(t, "alice", run, nil); len(again.Lines) != 0 {
				t.Fatalf("a second undo of the run undid %+v again", again.Lines)
			}
			listed := decodeBody[goingRunsPage](t, rig.browser(t, "alice", http.MethodGet, "/api/v1/me/going-runs?undoable=true", nil))
			for _, row := range listed.Rows {
				if row.ID == run {
					t.Fatalf("the run reads undoable after its undo")
				}
			}
		})
	}
}

// TestGoingUndoRefusesAFieldAnotherWriterChangedAndUndoesTheRest: a field another writer changed
// after the turn's write is left as that writer left it, refused by name, and every other field
// and every other write of the turn is undone.
func TestGoingUndoRefusesAFieldAnotherWriterChangedAndUndoesTheRest(t *testing.T) {
	rig := newGoingWritesRig(t)
	first := createInteractionIssue(t, rig.mux, "DEMO", "First", "The spec.\n").Key
	second := rig.issue(t, "Second")
	beforeFirst, beforeSecond := rig.issueRead(t, first), rig.issueRead(t, second)
	run := rig.proposed(t, "alice", false, "dispatch_issue_update",
		map[string]any{"issue": first, "status": "in_progress", "title": "First, renamed"},
		map[string]any{"issue": second, "status": "in_progress"})
	for invocation := range 2 {
		rig.wrote(t, "alice", run, map[string]any{"offer": "dispatch_issue_update", "invocation": invocation, "approved": goingApprovedPerson})
	}
	rig.fixture(t, "bob", http.MethodPatch, "/api/v1/issues/"+first, map[string]any{"title": "Bob's title"})

	answer := rig.undo(t, "alice", run, nil)
	if len(answer.Lines) != 2 {
		t.Fatalf("the undo answered %+v, want a line for each write", answer.Lines)
	}
	changed, clean := answer.Lines[1], answer.Lines[0]
	if changed.Item != first || changed.Outcome.OK || changed.Outcome.Code != codeGoingUndoChanged ||
		!strings.Contains(changed.Outcome.Message, "title") || !strings.Contains(changed.Outcome.Message, "Bob's title") {
		t.Fatalf("the first issue's undo answered %+v, want title refused naming what it holds now", changed)
	}
	if !clean.Outcome.OK || clean.Item != second {
		t.Fatalf("the second issue's undo answered %+v, want it undone", clean)
	}
	afterFirst, afterSecond := rig.issueRead(t, first), rig.issueRead(t, second)
	if afterFirst.Title != "Bob's title" || afterFirst.Status != beforeFirst.Status || afterSecond.Status != beforeSecond.Status {
		t.Fatalf("after the undo the first reads %s %q and the second %s, want bob's title kept and both statuses back",
			afterFirst.Status, afterFirst.Title, afterSecond.Status)
	}
	fields := map[string]bool{}
	for _, field := range changed.Fields {
		fields[field.Field] = field.OK
	}
	if len(fields) != 2 || !fields["status"] || fields["title"] {
		t.Fatalf("the first issue's undo reports its fields as %+v, want status put back and title left", changed.Fields)
	}

	// The refusal is the turn's verdict on that write: a second undo tries nothing again, and the
	// run no longer reads undoable.
	if again := rig.undo(t, "alice", run, nil); len(again.Lines) != 0 {
		t.Fatalf("a second undo tried %+v again", again.Lines)
	}
	undos := rig.record(t, "alice", run).Undos
	if len(undos) != 2 || !undos[0].Settled || !undos[1].Settled || len(undos[1].Fields) != 2 {
		t.Fatalf("the run keeps the undos %+v, want both settled and the refused one field by field", undos)
	}
	listed := decodeBody[goingRunsPage](t, rig.browser(t, "alice", http.MethodGet, "/api/v1/me/going-runs?undoable=true", nil))
	for _, row := range listed.Rows {
		if row.ID == run {
			t.Fatal("the run reads undoable after its undo was settled")
		}
	}
}

// TestGoingUndoPutsARankBackBetweenItsOldNeighbours: an issue's rank is a field the undo writes
// back, by naming the issues it stood between before the write; one another writer moved again
// since is refused for its rank, naming where it stands now, while the turn's other fields are
// undone.
func TestGoingUndoPutsARankBackBetweenItsOldNeighbours(t *testing.T) {
	rig := newGoingWritesRig(t)
	first := createInteractionIssue(t, rig.mux, "DEMO", "First in line", "The spec.\n").Key
	second, third, moved := rig.issue(t, "Second in line"), rig.issue(t, "Third in line"), rig.issue(t, "Moved about")
	rank := func(key string) string {
		t.Helper()
		return rig.issueRead(t, key).Rank
	}
	between := func(t *testing.T, key, after, before string) bool {
		t.Helper()
		at := rank(key)
		return (after == "" || rank(after) < at) && (before == "" || at < rank(before))
	}
	if !between(t, moved, third, "") {
		t.Fatalf("%s is not last in its project to begin with", moved)
	}

	t.Run("a move is undone between the old neighbours", func(t *testing.T) {
		run, line := rig.runOne(t, "dispatch_issue_update", map[string]any{"issue": moved, "after": first, "before": second}, nil)
		if !between(t, moved, first, second) || !slices.Contains(line.Fields, "rank") {
			t.Fatalf("the move answered %+v and did not put %s between %s and %s", line, moved, first, second)
		}
		oneLine(t, rig.undo(t, "alice", run, nil), true)
		if !between(t, moved, third, "") {
			t.Fatalf("after the undo %s stands at %s, want after %s (%s) again", moved, rank(moved), third, rank(third))
		}
	})

	t.Run("a move another writer made since is refused for rank, the rest undone", func(t *testing.T) {
		status := rig.issueRead(t, moved).Status
		run, _ := rig.runOne(t, "dispatch_issue_update", map[string]any{
			"issue": moved, "status": "in_progress", "after": first, "before": second,
		}, nil)
		rig.fixture(t, "bob", http.MethodPatch, "/api/v1/issues/"+moved, map[string]any{"rank": map[string]any{"after": second, "before": third}})
		line := oneLine(t, rig.undo(t, "alice", run, nil), false)
		fields := map[string]goingUndoField{}
		for _, field := range line.Fields {
			fields[field.Field] = field
		}
		if line.Outcome.Code != codeGoingUndoChanged || fields["rank"].OK || !fields["status"].OK ||
			!strings.Contains(fields["rank"].Message, second) || !strings.Contains(fields["rank"].Message, third) {
			t.Fatalf("the undo answered %+v, want rank refused naming where bob left it and status put back", line)
		}
		if !between(t, moved, second, third) || rig.issueRead(t, moved).Status != status {
			t.Fatalf("after the undo %s is %s at %s, want bob's place kept and the status back to %s",
				moved, rig.issueRead(t, moved).Status, rank(moved), status)
		}
	})
}

// TestGoingUndoTwoUndosOfOneRunTakeTurns: two undos of one run sent at once undo each write once:
// one claims the run's writes, and the other either finds them claimed - 429 with a Retry-After -
// or comes after and finds nothing left to undo; a retry once both answered undoes nothing more.
func TestGoingUndoTwoUndosOfOneRunTakeTurns(t *testing.T) {
	rig := newGoingWritesRig(t)
	issue := createInteractionIssue(t, rig.mux, "DEMO", "Twice", "The spec.\n").Key
	run := rig.proposed(t, "alice", false, "dispatch_issue_update",
		map[string]any{"issue": issue, "labels": []any{"one"}},
		map[string]any{"issue": issue, "title": "Twice, renamed"},
		map[string]any{"issue": issue, "status": "in_progress"})
	for invocation := range 3 {
		rig.wrote(t, "alice", run, map[string]any{"offer": "dispatch_issue_update", "invocation": invocation, "approved": goingApprovedPerson})
	}
	cookie := rig.cookie(t, "alice")
	answers := make(chan *httptest.ResponseRecorder, 2)
	for range 2 {
		go func() {
			answers <- goingRequestTo(t, rig.network, http.MethodPost, "/api/v1/going/runs/"+run+"/undo", nil, pageOrigin, cookie)
		}()
	}
	lines := 0
	for range 2 {
		var response *httptest.ResponseRecorder
		select {
		case response = <-answers:
		case <-time.After(30 * time.Second):
			t.Fatal("an undo never answered")
		}
		switch {
		case response.Code == http.StatusOK:
			lines += len(decodeBody[goingUndoAnswer](t, response).Lines)
		case response.Code != http.StatusTooManyRequests || !strings.Contains(response.Body.String(), codeGoingUndoInProgress) ||
			response.Header().Get("Retry-After") == "":
			t.Fatalf("an undo answered %d %s, want its lines or 429 %s with a Retry-After", response.Code, response.Body.String(), codeGoingUndoInProgress)
		}
	}
	if again := rig.undo(t, "alice", run, nil); len(again.Lines) != 0 {
		t.Fatalf("an undo after both undid %+v again", again.Lines)
	}
	undos := rig.record(t, "alice", run).Undos
	if lines != 3 || len(undos) != 3 {
		t.Fatalf("the two undos answered %d lines and recorded %+v, want each of the three writes undone once", lines, undos)
	}
	seen := map[string]bool{}
	for _, undo := range undos {
		if seen[undo.WriteID] || !undo.Outcome.OK || undo.Pending {
			t.Fatalf("the run keeps the undos %+v, want each write undone once, ok and recorded", undos)
		}
		seen[undo.WriteID] = true
	}
}

// TestGoingUndoTakesBackAReleaseOnlyWhereTheCallerHeldTheClaim: the undo of a
// release takes the claim back where the caller held it before, and refuses, naming the holder,
// where someone else did, since taking it would make the caller a holder it never was.
func TestGoingUndoTakesBackAReleaseOnlyWhereTheCallerHeldTheClaim(t *testing.T) {
	rig := newGoingWritesRig(t)
	own := createInteractionIssue(t, rig.mux, "DEMO", "Alice's", "The spec.\n").Key
	others := rig.issue(t, "Bob's")
	rig.fixture(t, "alice", http.MethodPost, "/api/v1/issues/"+own+"/claim", map[string]any{})
	rig.fixture(t, "bob", http.MethodPost, "/api/v1/issues/"+others+"/claim", map[string]any{})

	run, _ := rig.runOne(t, "dispatch_claim", map[string]any{"issue": own, "release": true}, nil)
	if rig.issueRead(t, own).Claim != nil {
		t.Fatal("the release did not land")
	}
	oneLine(t, rig.undo(t, "alice", run, nil), true)
	if claim := rig.issueRead(t, own).Claim; claim == nil || model.CanonicalLogin(claim.Actor.ID) != "alice" {
		t.Fatalf("alice's own claim reads %+v after the undo, want hers again", claim)
	}

	run, _ = rig.runOne(t, "dispatch_claim", map[string]any{"issue": others, "release": true}, nil)
	line := oneLine(t, rig.undo(t, "alice", run, nil), false)
	if line.Outcome.Code != codeGoingUndoNotCallers || !strings.Contains(line.Outcome.Message, "bob") {
		t.Fatalf("the undo of a release of bob's claim answered %+v, want it refused naming bob", line)
	}
	if claim := rig.issueRead(t, others).Claim; claim != nil {
		t.Fatalf("bob's released claim is held by %+v after the refused undo", claim.Actor)
	}
}

// TestGoingUndoOfARunPastThirtyDaysIsRefusedAndOneInsideThemIsUndone: a turn is undone for as long
// as its run is kept; a run of thirty-one days is gone, and one of twenty-nine is undone.
func TestGoingUndoOfARunPastThirtyDaysIsRefusedAndOneInsideThemIsUndone(t *testing.T) {
	rig := newGoingWritesRig(t)
	issue := createInteractionIssue(t, rig.mux, "DEMO", "Aged", "The spec.\n").Key
	age := func(run string, by time.Duration) {
		t.Helper()
		if _, err := rig.database.Pool.Exec(context.Background(),
			`update going_runs set created_at = now() - $2::interval where id = $1::uuid`, run, by.String()); err != nil {
			t.Fatalf("age the run: %v", err)
		}
	}
	gone, _ := rig.runOne(t, "dispatch_issue_update", map[string]any{"issue": issue, "labels": []any{"gone"}}, nil)
	kept, _ := rig.runOne(t, "dispatch_issue_update", map[string]any{"issue": issue, "labels": []any{"kept"}}, nil)
	age(gone, 31*24*time.Hour)
	age(kept, 29*24*time.Hour)
	if response := rig.browser(t, "alice", http.MethodPost, "/api/v1/going/runs/"+gone+"/undo", nil); response.Code != http.StatusNotFound ||
		!strings.Contains(response.Body.String(), codeGoingRunNotFound) {
		t.Fatalf("the undo of a 31-day run: status=%d body=%s, want 404 %s", response.Code, response.Body.String(), codeGoingRunNotFound)
	}
	oneLine(t, rig.undo(t, "alice", kept, nil), true)
	if labels := rig.issueRead(t, issue).Labels; len(labels) != 1 || labels[0] != "gone" {
		t.Fatalf("the issue's labels read %v after the 29-day run's undo, want the labels before it", labels)
	}
}

// TestGoingUndoOfAWriteWhoseActionIsGoneIsRefusedAsUnknown: a write whose action the catalogue no
// longer holds has no undo to run, so its undo is refused with a code of its own, and the issue
// is left as the write left it.
func TestGoingUndoOfAWriteWhoseActionIsGoneIsRefusedAsUnknown(t *testing.T) {
	rig := newGoingWritesRig(t)
	issue := createInteractionIssue(t, rig.mux, "DEMO", "Orphaned", "The spec.\n").Key
	run, _ := rig.runOne(t, "dispatch_issue_update", map[string]any{"issue": issue, "labels": []any{"left"}}, nil)
	if _, err := rig.database.Pool.Exec(context.Background(),
		`update going_runs set writes = jsonb_set(writes, '{0,action}', '"dispatch_retired"') where id = $1::uuid`, run); err != nil {
		t.Fatalf("retire the write's action: %v", err)
	}
	line := oneLine(t, rig.undo(t, "alice", run, nil), false)
	if line.Outcome.Status != http.StatusConflict || line.Outcome.Code != codeGoingUndoUnknown {
		t.Fatalf("the undo of a retired action answered %+v, want 409 %s", line.Outcome, codeGoingUndoUnknown)
	}
	if labels := rig.issueRead(t, issue).Labels; len(labels) != 1 || labels[0] != "left" {
		t.Fatalf("the issue's labels read %v, want the write's left in place", labels)
	}
}

// TestGoingUndoOfAClaimTheCallerAlreadyHeldReleasesNothing: a bar turn that claims an issue the
// person already held changed nothing, so its undo leaves the claim with them.
func TestGoingUndoOfAClaimTheCallerAlreadyHeldReleasesNothing(t *testing.T) {
	rig := newGoingWritesRig(t)
	issue := createInteractionIssue(t, rig.mux, "DEMO", "Held by hand", "The spec.\n").Key
	rig.fixture(t, "alice", http.MethodPost, "/api/v1/issues/"+issue+"/claim", map[string]any{})
	run, _ := rig.runOne(t, "dispatch_claim", map[string]any{"issue": issue}, nil)
	oneLine(t, rig.undo(t, "alice", run, nil), true)
	if claim := rig.issueRead(t, issue).Claim; claim == nil || model.CanonicalLogin(claim.Actor.ID) != "alice" {
		t.Fatalf("alice's own claim reads %+v after the undo of a turn that changed nothing, want it hers still", claim)
	}
}

// TestGoingUndoOfAnApprovalRequestRetractsOnlyTheAskItOpened: a second request for approval of a
// document renews the ask the first opened; the second's undo leaves that ask open, and the
// first's, which opened it, retracts it.
func TestGoingUndoOfAnApprovalRequestRetractsOnlyTheAskItOpened(t *testing.T) {
	rig := newGoingWritesRig(t)
	issue := createInteractionIssue(t, rig.mux, "DEMO", "Approved twice", "The spec.\n").Key
	askState := func(id string) string {
		t.Helper()
		return decodeBody[goingAskRead](t, rig.fixture(t, "alice", http.MethodGet, "/api/v1/asks/"+id, nil)).Ask.State
	}
	first, opened := rig.runOne(t, "dispatch_request_approval", map[string]any{"issue": issue, "artifact": "spec", "summary": "Ready."}, nil)
	second, renewed := rig.runOne(t, "dispatch_request_approval", map[string]any{"issue": issue, "artifact": "spec", "summary": "Ready."}, nil)
	if renewed.Item != opened.Item {
		t.Fatalf("the second request answered the ask %s, want the first's %s renewed", renewed.Item, opened.Item)
	}
	oneLine(t, rig.undo(t, "alice", second, nil), true)
	if state := askState(opened.Item); state != "open" {
		t.Fatalf("the approval ask is %s after the undo of the request that renewed it, want open", state)
	}
	oneLine(t, rig.undo(t, "alice", first, nil), true)
	if state := askState(opened.Item); state != "resolved" {
		t.Fatalf("the approval ask is %s after the undo of the request that opened it, want resolved", state)
	}
}

// TestGoingUndoRefusedForACauseThatClearsStaysUndoable: an undo its route refused 429 settles
// nothing - the run still reads undoable - and a second undo of the run undoes the write.
func TestGoingUndoRefusedForACauseThatClearsStaysUndoable(t *testing.T) {
	rig := newGoingWritesRig(t)
	issue := createInteractionIssue(t, rig.mux, "DEMO", "Busy", "The spec.\n").Key
	status := rig.issueRead(t, issue).Status
	run, _ := rig.runOne(t, "dispatch_issue_update", map[string]any{"issue": issue, "status": "in_progress"}, nil)
	inner := rig.served.inProcess
	t.Cleanup(func() { rig.served.inProcess = inner })
	limited := false
	rig.served.inProcess = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPatch && !limited {
			limited = true
			w.Header().Set("Retry-After", "1")
			writeError(w, "RATE_LIMITED", http.StatusTooManyRequests, "too many writes from this caller: try again shortly")
			return
		}
		inner.ServeHTTP(w, r)
	})
	refused := oneLine(t, rig.undo(t, "alice", run, nil), false)
	if refused.Outcome.Status != http.StatusTooManyRequests {
		t.Fatalf("the undo answered %+v, want the route's 429", refused.Outcome)
	}
	listed := decodeBody[goingRunsPage](t, rig.browser(t, "alice", http.MethodGet, "/api/v1/me/going-runs?undoable=true", nil))
	if !slices.ContainsFunc(listed.Rows, func(row goingRun) bool { return row.ID == run }) {
		t.Fatal("the run no longer reads undoable after an undo refused 429")
	}
	oneLine(t, rig.undo(t, "alice", run, nil), true)
	if now := rig.issueRead(t, issue).Status; now != status {
		t.Fatalf("the issue is %s after the second undo, want %s back", now, status)
	}
}

// TestGoingUndoSettlesOnlyAVerdictARetryWouldRepeat: ok, another writer's change and a refusal
// that is the route's final word settle a write; the server's own failure, a refusal that says
// only this attempt was not heard (401, 408, 429) and a contended claim do not.
func TestGoingUndoSettlesOnlyAVerdictARetryWouldRepeat(t *testing.T) {
	for _, tc := range []struct {
		outcome goingWriteOutcome
		settles bool
	}{
		{goingWriteOutcome{OK: true, Status: http.StatusOK}, true},
		{goingWriteOutcome{Status: http.StatusConflict, Code: codeGoingUndoChanged}, true},
		{goingWriteOutcome{Status: http.StatusConflict, Code: codeGoingUndoNotCallers}, true},
		{goingWriteOutcome{Status: http.StatusForbidden, Code: "OAUTH_SCOPE_REQUIRED"}, true},
		{goingWriteOutcome{Status: http.StatusConflict, Code: codeClaimContended}, false},
		{goingWriteOutcome{Status: http.StatusUnauthorized, Code: "UNAUTHENTICATED"}, false},
		{goingWriteOutcome{Status: http.StatusRequestTimeout, Code: "REQUEST_TIMEOUT"}, false},
		{goingWriteOutcome{Status: http.StatusTooManyRequests, Code: "RATE_LIMITED"}, false},
		{goingWriteOutcome{Status: http.StatusInternalServerError, Code: codeInternal}, false},
		{goingWriteOutcome{Status: http.StatusNotImplemented, Code: codeGoingUndoUnserved}, false},
	} {
		if got := goingUndoSettles(tc.outcome); got != tc.settles {
			t.Errorf("an undo answered %d %s settles its write: %v, want %v", tc.outcome.Status, tc.outcome.Code, got, tc.settles)
		}
	}
}

// TestGoingUndoOfARankWhoseNeighboursCannotBeReadFailsUnsettled: a failed read of the issues a
// rank stood between is the undo's failure, answered 500 and settling nothing, never another
// writer's change.
func TestGoingUndoOfARankWhoseNeighboursCannotBeReadFailsUnsettled(t *testing.T) {
	rig := newGoingWritesRig(t)
	issue := createInteractionIssue(t, rig.mux, "DEMO", "Ranked", "The spec.\n").Key
	neighbour := rig.issue(t, "Its neighbour")
	// A context holding a connection already: the pool refuses the read a second one.
	held, release := store.HoldsConnection(context.Background())
	defer release()
	observer := goingObserver{s: rig.served}
	restore, why, err := observer.rankRestore(held, issue, goingRankPlace{After: &neighbour})
	if err == nil || why != "" || restore != nil {
		t.Fatalf("rankRestore on a read that failed answered %v, %q, %v; want its error and no reason", restore, why, err)
	}

	undoer := goingUndoer{s: rig.served}
	write := goingRunWrite{ID: "w-rank", Action: "dispatch_issue_update", Target: issue, Fields: []string{goingRank},
		Before: json.RawMessage(`{"rank":{"rank":"a"}}`), After: json.RawMessage(`{"rank":{"rank":"b"}}`), Outcome: goingWriteOutcome{OK: true}}
	read := func([]string) (map[string]any, error) {
		return map[string]any{goingRank: map[string]any{"rank": "b"}}, nil
	}
	_, outcome, _ := undoer.writeBack(context.Background(), write, read, goingIssuePatchRoute, map[string]string{"key": issue},
		func(string, json.RawMessage, json.RawMessage, json.RawMessage) (any, string, error) {
			return nil, "", err
		})
	if outcome.OK || outcome.Status != http.StatusInternalServerError || goingUndoSettles(outcome) {
		t.Fatalf("the write back answered %+v, want the server's own 500, which settles nothing", outcome)
	}
}

// TestGoingUndoOfARowWhoseUndoThisServerCannotRunIsRefused: a write recorded through a row whose
// undo kind names no undo this server runs for its action is refused, 501 GOING_UNDO_UNSERVED,
// which settles nothing, rather than recorded as undone with nothing undone.
func TestGoingUndoOfARowWhoseUndoThisServerCannotRunIsRefused(t *testing.T) {
	catalogue, err := BuildGoingCatalogue()
	if err != nil {
		t.Fatal(err)
	}
	undoer := goingUndoer{s: &server{going: catalogue}}
	// A row added later whose undo is stated as a reverse, with no reverse written for it here.
	line := undoer.undo(context.Background(), goingRunWrite{ID: "w-1", Action: "dispatch_resolve_ask", Target: "ask-1", Fields: []string{"state"},
		Outcome: goingWriteOutcome{OK: true}, Undo: goingUndoReverse}, nil)
	if line.Outcome.OK || line.Outcome.Status != http.StatusNotImplemented || line.Outcome.Code != codeGoingUndoUnserved || goingUndoSettles(line.Outcome) {
		t.Fatalf("the undo of a reverse row with no reverse answered %+v, want 501 %s, unsettled", line.Outcome, codeGoingUndoUnserved)
	}
}

// TestGoingUndoRunsTheUndoOfTheRowTheWriteWentThrough (review finding 4): the rows of one action
// may state different undos, so a write is undone by the undo of the row it went through, as its
// record keeps it, and never by the action's first row's; a write whose record names no undo is
// refused, 409 GOING_UNDO_UNKNOWN, with nothing undone.
func TestGoingUndoRunsTheUndoOfTheRowTheWriteWentThrough(t *testing.T) {
	catalogue, err := BuildGoingCatalogue()
	if err != nil {
		t.Fatal(err)
	}
	// The resolve's first row now states a reverse this server has none of; the write went
	// through a row stating a trace.
	resolve := catalogue.byAction["dispatch_resolve_ask"]
	catalogue.actions[resolve].Routes[0].Undo = GoingUndo{Kind: goingUndoReverse, Routes: []string{goingAskResolveRoute}}
	undoer := goingUndoer{s: &server{going: catalogue}}
	write := goingRunWrite{ID: "w-1", Action: "dispatch_resolve_ask", Target: "ask-1", Fields: []string{"state"},
		Outcome: goingWriteOutcome{OK: true}, Undo: goingUndoTrace}
	if line := undoer.undo(context.Background(), write, nil); !line.Outcome.OK || line.Undo != goingUndoTrace {
		t.Fatalf("the undo of a write through a trace row answered %+v by %s, want it recorded undone by its trace", line.Outcome, line.Undo)
	}
	write.Undo = ""
	if line := undoer.undo(context.Background(), write, nil); line.Outcome.OK || line.Outcome.Code != codeGoingUndoUnknown {
		t.Fatalf("the undo of a write whose record names no undo answered %+v, want 409 %s", line.Outcome, codeGoingUndoUnknown)
	}
}

// TestGoingWriteRecordsTheUndoOfTheRowItWentThrough: each write the route makes keeps, on the run,
// the undo the catalogue row it went through states: a claim's reverse and an update's write back.
func TestGoingWriteRecordsTheUndoOfTheRowItWentThrough(t *testing.T) {
	rig := newGoingWritesRig(t)
	issue := createInteractionIssue(t, rig.mux, "DEMO", "Recorded undo", "The spec.\n").Key
	for _, tc := range []struct {
		action    string
		arguments map[string]any
		undo      goingUndoKind
	}{
		{"dispatch_claim", map[string]any{"issue": issue}, goingUndoReverse},
		{"dispatch_issue_update", map[string]any{"issue": issue, "title": "Renamed by the bar"}, goingUndoWriteBack},
	} {
		run, _ := rig.runOne(t, tc.action, tc.arguments, nil)
		if writes := rig.record(t, "alice", run).Writes; len(writes) != 1 || writes[0].Undo != tc.undo {
			t.Fatalf("the run of %s records %+v, want one write undone by %s", tc.action, writes, tc.undo)
		}
	}
}

// TestGoingUndoOfAnApprovalRequestWhoseRecordDoesNotDecodeRetractsNothing (review round 2,
// finding D): an approval request whose record does not say, in a form that decodes, whether it
// opened its ask cannot tell whether it may retract it, so its undo is the server's own failure,
// 500, which settles nothing, and the ask stays open.
func TestGoingUndoOfAnApprovalRequestWhoseRecordDoesNotDecodeRetractsNothing(t *testing.T) {
	rig := newGoingWritesRig(t)
	issue := createInteractionIssue(t, rig.mux, "DEMO", "Approval, garbled", "The spec.\n").Key
	run, opened := rig.runOne(t, "dispatch_request_approval", map[string]any{"issue": issue, "artifact": "spec", "summary": "Ready."}, nil)
	if _, err := rig.database.Pool.Exec(context.Background(),
		`update going_runs set writes = jsonb_set(writes, '{0,after,opened}', '"not a yes or a no"') where id = $1::uuid`, run); err != nil {
		t.Fatalf("garble the record's value after: %v", err)
	}
	line := oneLine(t, rig.undo(t, "alice", run, nil), false)
	if line.Outcome.Status != http.StatusInternalServerError || goingUndoSettles(line.Outcome) {
		t.Fatalf("the undo of a request whose record does not decode answered %+v, want the server's own 500, unsettled", line.Outcome)
	}
	if state := decodeBody[goingAskRead](t, rig.fixture(t, "alice", http.MethodGet, "/api/v1/asks/"+opened.Item, nil)).Ask.State; state != "open" {
		t.Fatalf("the approval ask is %s after the failed undo, want open", state)
	}
}

// TestGoingUndoMadeAndNotRecordedKeepsItsClaim (review round 2, finding E): an undo that put the
// newest write back and whose record the database refused keeps that write's undo claimed, as a
// write whose record failed keeps its claim, while the claim of the write it never reached is
// given up; so the next undo of the run is told one is under way and undoes nothing a second
// time.
func TestGoingUndoMadeAndNotRecordedKeepsItsClaim(t *testing.T) {
	rig := newGoingWritesRig(t)
	issue := createInteractionIssue(t, rig.mux, "DEMO", "Undone, not recorded", "The spec.\n").Key
	before := rig.issueRead(t, issue)
	run := rig.proposed(t, "alice", false, "dispatch_issue_update",
		map[string]any{"issue": issue, "labels": []any{"one"}},
		map[string]any{"issue": issue, "title": "Renamed by the bar"})
	titled := ""
	for invocation := range 2 {
		line := rig.wrote(t, "alice", run, map[string]any{"offer": "dispatch_issue_update", "invocation": invocation, "approved": goingApprovedPerson})
		titled = line.WriteID
	}
	// The record takes a claim and its release, and refuses every undo recorded as made.
	allow := rig.refuseRecordedUndos(t)
	if first := rig.browser(t, "alice", http.MethodPost, "/api/v1/going/runs/"+run+"/undo", nil); first.Code != http.StatusInternalServerError {
		t.Fatalf("the undo whose record failed answered %d %s, want 500", first.Code, first.Body.String())
	}
	allow()
	if title := rig.issueRead(t, issue).Title; title != before.Title {
		t.Fatalf("the issue is titled %q after the undo whose record failed, want %q put back", title, before.Title)
	}
	undos := rig.record(t, "alice", run).Undos
	if len(undos) != 1 || undos[0].WriteID != titled || !undos[0].Pending {
		t.Fatalf("the run keeps the undos %+v, want the title's undo claimed alone, the labels' given up", undos)
	}

	again := rig.browser(t, "alice", http.MethodPost, "/api/v1/going/runs/"+run+"/undo", nil)
	if again.Code != http.StatusTooManyRequests || !strings.Contains(again.Body.String(), codeGoingUndoInProgress) {
		t.Fatalf("the next undo answered %d %s, want 429 %s", again.Code, again.Body.String(), codeGoingUndoInProgress)
	}
	if undos := rig.record(t, "alice", run).Undos; len(undos) != 1 || !undos[0].Pending {
		t.Fatalf("the run keeps the undos %+v after the next undo, want the one claim and nothing undone again", undos)
	}
}

// refuseRecordedUndos makes the record refuse every undo recorded as made, while it still takes a
// claim and its release, until the func it answers lets it take them again.
func (rig goingWritesRig) refuseRecordedUndos(t *testing.T) (allow func()) {
	t.Helper()
	ctx := context.Background()
	if _, err := rig.database.Pool.Exec(ctx, `
		create function going_test_refuse_undo() returns trigger language plpgsql as $$
		begin
		  if exists (select 1 from jsonb_array_elements(new.undos) u where not coalesce((u->>'pending')::boolean, false)) then
		    raise exception 'the record refuses this undo';
		  end if;
		  return new;
		end $$;
		create trigger going_test_refuse_undo before update on going_runs for each row execute function going_test_refuse_undo();
	`); err != nil {
		t.Fatalf("make the record refuse an undo: %v", err)
	}
	return func() {
		t.Helper()
		if _, err := rig.database.Pool.Exec(ctx, `drop trigger going_test_refuse_undo on going_runs; drop function going_test_refuse_undo()`); err != nil {
			t.Fatalf("let the record take an undo again: %v", err)
		}
	}
}

// TestGoingUndoMadeAndNotRecordedRunsNoSecondTimePastTheBound (review round 3, finding 2; check
// c21): an undo that released the claim a bar turn took, and whose record the database refused,
// is never made a second time, however long after: once its claim is older than any request can
// still run (goingClaimBound), the next undo of the run answers that write unsettled and undoes
// nothing, so the claim the person took again by hand is released once, by the first undo alone.
func TestGoingUndoMadeAndNotRecordedRunsNoSecondTimePastTheBound(t *testing.T) {
	rig := newGoingWritesRig(t)
	issue := createInteractionIssue(t, rig.mux, "DEMO", "Released, not recorded", "The spec.\n").Key
	run, _ := rig.runOne(t, "dispatch_claim", map[string]any{"issue": issue}, nil)
	allow := rig.refuseRecordedUndos(t)
	if first := rig.browser(t, "alice", http.MethodPost, "/api/v1/going/runs/"+run+"/undo", nil); first.Code != http.StatusInternalServerError {
		t.Fatalf("the undo whose record failed answered %d %s, want 500", first.Code, first.Body.String())
	}
	allow()
	if claim := rig.issueRead(t, issue).Claim; claim != nil {
		t.Fatalf("the claim is held by %+v after the undo whose record failed, want it released", claim.Actor)
	}
	// The person takes the claim again by hand, and the undo's claim grows older than the bound.
	rig.fixture(t, "alice", http.MethodPost, "/api/v1/issues/"+issue+"/claim", map[string]any{})
	aged := time.Now().UTC().Add(-(rig.served.goingClaimBound() + time.Minute))
	if _, err := rig.database.Pool.Exec(context.Background(), `update going_runs set undos = jsonb_set(undos, '{0,at}', to_jsonb($2::text)) where id = $1::uuid`,
		run, aged.Format(time.RFC3339Nano)); err != nil {
		t.Fatalf("age the undo's claim: %v", err)
	}

	line := oneLine(t, rig.undo(t, "alice", run, nil), false)
	if line.Outcome.Status != http.StatusConflict || line.Outcome.Code != "GOING_UNDO_UNSETTLED" {
		t.Fatalf("the undo past the bound answered %+v, want the write's undo unsettled, 409 GOING_UNDO_UNSETTLED", line.Outcome)
	}
	if claim := rig.issueRead(t, issue).Claim; claim == nil || model.CanonicalLogin(claim.Actor.ID) != "alice" {
		t.Fatalf("the claim alice took again reads %+v after the undo past the bound, want it hers: released once", claim)
	}
	if again := oneLine(t, rig.undo(t, "alice", run, nil), false); again.Outcome.Code != "GOING_UNDO_UNSETTLED" {
		t.Fatalf("a further undo answered %+v, want the write's undo unsettled still", again.Outcome)
	}
}

// TestGoingUndoOfAClaimTakenByHandAsTheBarClaimedReleasesNothing (the claim race): a person takes
// an issue's claim by hand between the bar's read before its claim and the claim itself, so the
// claim route finds the claim already theirs and changes nothing; the turn's undo, told so by
// the route rather than by the read before, which saw nobody, leaves the claim with them.
func TestGoingUndoOfAClaimTakenByHandAsTheBarClaimedReleasesNothing(t *testing.T) {
	rig := newGoingWritesRig(t)
	issue := createInteractionIssue(t, rig.mux, "DEMO", "Claimed by hand meanwhile", "The spec.\n").Key
	inner := rig.served.inProcess
	t.Cleanup(func() { rig.served.inProcess = inner })
	taken := false
	rig.served.inProcess = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == "/api/v1/issues/"+issue+"/claim" && !taken {
			taken = true
			rig.fixture(t, "alice", http.MethodPost, "/api/v1/issues/"+issue+"/claim", map[string]any{})
		}
		inner.ServeHTTP(w, r)
	})
	run, _ := rig.runOne(t, "dispatch_claim", map[string]any{"issue": issue}, nil)
	rig.served.inProcess = inner
	if !taken {
		t.Fatal("the bar's claim was never sent")
	}
	oneLine(t, rig.undo(t, "alice", run, nil), true)
	if claim := rig.issueRead(t, issue).Claim; claim == nil || model.CanonicalLogin(claim.Actor.ID) != "alice" {
		t.Fatalf("the claim alice took by hand reads %+v after the undo of a turn whose claim changed nothing, want it hers", claim)
	}
}

// TestGoingUndoOfAnApprovalRequestThatRenewedAnAskOpenedAsItRanRetractsNothing (the approval
// race): an approval ask is opened on the document between the bar's read before its request and
// the request itself, so the request renews that ask rather than opening one; the turn's undo,
// told so by the route rather than by the read before, which saw no ask open, leaves it open.
func TestGoingUndoOfAnApprovalRequestThatRenewedAnAskOpenedAsItRanRetractsNothing(t *testing.T) {
	rig := newGoingWritesRig(t)
	issue := createInteractionIssue(t, rig.mux, "DEMO", "Approval asked meanwhile", "The spec.\n").Key
	inner := rig.served.inProcess
	t.Cleanup(func() { rig.served.inProcess = inner })
	opened := ""
	rig.served.inProcess = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/approval-requests") && opened == "" {
			other := decodeBody[struct {
				Ask *struct {
					ID string `json:"id"`
				} `json:"ask"`
			}](t, rig.fixture(t, "bob", http.MethodPost, r.URL.Path, map[string]any{"summary": "Ready."}))
			if other.Ask == nil {
				t.Fatal("the request made meanwhile opened no ask")
			}
			opened = other.Ask.ID
		}
		inner.ServeHTTP(w, r)
	})
	run, line := rig.runOne(t, "dispatch_request_approval", map[string]any{"issue": issue, "artifact": "spec", "summary": "Ready."}, nil)
	rig.served.inProcess = inner
	if opened == "" || line.Item != opened {
		t.Fatalf("the bar's request answered the ask %s, want the ask %s opened meanwhile, renewed", line.Item, opened)
	}
	oneLine(t, rig.undo(t, "alice", run, nil), true)
	if state := decodeBody[goingAskRead](t, rig.fixture(t, "alice", http.MethodGet, "/api/v1/asks/"+opened, nil)).Ask.State; state != "open" {
		t.Fatalf("the ask opened meanwhile is %s after the undo of the request that only renewed it, want open", state)
	}
}
