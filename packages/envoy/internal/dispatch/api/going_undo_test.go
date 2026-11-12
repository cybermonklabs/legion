// going_undo_test.go holds the undo of a command-bar turn to R4: every row of the catalogue
// states its undo, each kind of undo puts back what its write changed - or, for a trace, records
// the turn undone and erases nothing - a field another writer changed since is left as it is now
// while the rest is undone, a release is taken back only for the caller's own claim (decision 8),
// and a run past thirty days takes no undo.
package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/model"
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
		line := oneLine(t, rig.undo(t, "alice", run, nil), true)
		after := rig.issueRead(t, issue)
		if line.Undo != goingUndoWriteBack || after.Status != before.Status || after.Title != before.Title {
			t.Fatalf("the undo answered %+v and the issue reads %s %q, want %s %q back", line, after.Status, after.Title, before.Status, before.Title)
		}
	})
	t.Run("an ask's text", func(t *testing.T) {
		ask := rig.ask(t, issue, "Ship it on Friday?")
		run, _ := rig.runOne(t, "dispatch_edit_ask", map[string]any{"ask": ask, "question": "Ship it on Monday?"}, nil)
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
		if !between(t, moved, first, second) || !strings.Contains(line.Field, "rank") {
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
// the second waits for the first, reads what it recorded, and finds nothing left to undo.
// (awaitResponse's two seconds are too short for an undo waiting out another's three writes.)
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
		if response.Code != http.StatusOK {
			t.Fatalf("an undo answered %d %s", response.Code, response.Body.String())
		}
		lines += len(decodeBody[goingUndoAnswer](t, response).Lines)
	}
	undos := rig.record(t, "alice", run).Undos
	if lines != 3 || len(undos) != 3 {
		t.Fatalf("the two undos answered %d lines and recorded %+v, want each of the three writes undone once", lines, undos)
	}
	seen := map[string]bool{}
	for _, undo := range undos {
		if seen[undo.WriteID] || !undo.Outcome.OK {
			t.Fatalf("the run keeps the undos %+v, want each write undone once and ok", undos)
		}
		seen[undo.WriteID] = true
	}
}

// TestGoingUndoTakesBackAReleaseOnlyWhereTheCallerHeldTheClaim is decision 8: the undo of a
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
