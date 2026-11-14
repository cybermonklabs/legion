package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// goingBefore is a route table that, the first time it is sent `request`, lets meanwhile run
// first - another writer's change made between the bar's read and the bar's write - and then
// serves the request.
func goingBefore(inner http.Handler, request string, meanwhile func()) http.Handler {
	ran := false
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method+" "+r.URL.Path == request && !ran {
			ran = true
			meanwhile()
		}
		inner.ServeHTTP(w, r)
	})
}

// goingAfter is a route table that serves `request`, the first time it is sent, and then lets
// meanwhile run before the next request is served.
func goingAfter(inner http.Handler, request string, meanwhile func()) http.Handler {
	ran := false
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		inner.ServeHTTP(w, r)
		if r.Method+" "+r.URL.Path == request && !ran {
			ran = true
			meanwhile()
		}
	})
}

// holder is the login holding the issue's claim, "" for nobody.
func (rig goingWritesRig) holder(t *testing.T, issue string) string {
	t.Helper()
	claim := rig.issueRead(t, issue).Claim
	if claim == nil {
		return ""
	}
	return model.CanonicalLogin(claim.Actor.ID)
}

// TestGoingUndoOfAClaimReleasesOnlyTheClaimTheTurnSet (review round 4, finding 2; check c21): the
// undo of a claim releases the issue only while the holder the claim left still holds it, decided
// in the release itself: bob, who takes the claim between the undo's read and its release, or
// between the bar's claim and the read after it, keeps it.
func TestGoingUndoOfAClaimReleasesOnlyTheClaimTheTurnSet(t *testing.T) {
	rig := newGoingWritesRig(t)
	createInteractionIssue(t, rig.mux, "DEMO", "The project's first", "The spec.\n")
	inner := rig.served.inProcess
	t.Cleanup(func() { rig.served.inProcess = inner })
	bobTakes := func(t *testing.T, issue string) func() {
		return func() {
			rig.fixture(t, "bob", http.MethodPost, "/api/v1/issues/"+issue+"/claim", map[string]any{"force": true})
		}
	}

	t.Run("taken between the undo's read and its release", func(t *testing.T) {
		issue := rig.issue(t, "Taken as the undo released")
		run, _ := rig.runOne(t, "dispatch_claim", map[string]any{"issue": issue}, nil)
		rig.served.inProcess = goingBefore(inner, http.MethodDelete+" /api/v1/issues/"+issue+"/claim", bobTakes(t, issue))
		line := oneLine(t, rig.undo(t, "alice", run, nil), false)
		rig.served.inProcess = inner
		if holder := rig.holder(t, issue); holder != "bob" {
			t.Fatalf("the claim bob took reads held by %q after the undo answered %+v, want bob's kept", holder, line.Outcome)
		}
	})

	t.Run("taken between the bar's claim and the read after it", func(t *testing.T) {
		issue := rig.issue(t, "Taken as the bar read back")
		rig.served.inProcess = goingAfter(inner, http.MethodPost+" /api/v1/issues/"+issue+"/claim", bobTakes(t, issue))
		run, _ := rig.runOne(t, "dispatch_claim", map[string]any{"issue": issue}, nil)
		rig.served.inProcess = inner
		line := oneLine(t, rig.undo(t, "alice", run, nil), false)
		if holder := rig.holder(t, issue); holder != "bob" {
			t.Fatalf("the claim bob took reads held by %q after the undo answered %+v, want bob's kept", holder, line.Outcome)
		}
	})
}

// TestGoingUndoOfAReleaseTakesBackOnlyTheClaimTheTurnReleased (review round 4, finding 2; check
// c21): the undo of a release takes the claim again only for the holder the release itself
// answered it released, and only while nobody holds it, decided in the claim itself. Bob, who takes
// the claim between the bar's read before its release and the release, is the one the release
// released, so the undo gives alice nothing; and bob, who claims between the undo's read and its
// claim, keeps his claim.
func TestGoingUndoOfAReleaseTakesBackOnlyTheClaimTheTurnReleased(t *testing.T) {
	rig := newGoingWritesRig(t)
	createInteractionIssue(t, rig.mux, "DEMO", "The project's first", "The spec.\n")
	inner := rig.served.inProcess
	t.Cleanup(func() { rig.served.inProcess = inner })

	t.Run("taken between the bar's read and its release", func(t *testing.T) {
		issue := rig.issue(t, "Taken as the bar released")
		rig.fixture(t, "alice", http.MethodPost, "/api/v1/issues/"+issue+"/claim", map[string]any{})
		rig.served.inProcess = goingBefore(inner, http.MethodDelete+" /api/v1/issues/"+issue+"/claim", func() {
			rig.fixture(t, "bob", http.MethodPost, "/api/v1/issues/"+issue+"/claim", map[string]any{"force": true})
		})
		run, _ := rig.runOne(t, "dispatch_claim", map[string]any{"issue": issue, "release": true}, nil)
		rig.served.inProcess = inner
		line := oneLine(t, rig.undo(t, "alice", run, nil), false)
		if holder := rig.holder(t, issue); holder == "alice" {
			t.Fatalf("the undo answered %+v and gave alice a claim the turn took from bob, want nothing taken back", line.Outcome)
		}
	})

	t.Run("claimed between the undo's read and its claim", func(t *testing.T) {
		issue := rig.issue(t, "Claimed as the undo took it back")
		rig.fixture(t, "alice", http.MethodPost, "/api/v1/issues/"+issue+"/claim", map[string]any{})
		run, _ := rig.runOne(t, "dispatch_claim", map[string]any{"issue": issue, "release": true}, nil)
		rig.served.inProcess = goingBefore(inner, http.MethodPost+" /api/v1/issues/"+issue+"/claim", func() {
			rig.fixture(t, "bob", http.MethodPost, "/api/v1/issues/"+issue+"/claim", map[string]any{})
		})
		line := oneLine(t, rig.undo(t, "alice", run, nil), false)
		rig.served.inProcess = inner
		if holder := rig.holder(t, issue); holder != "bob" {
			t.Fatalf("the claim bob took reads held by %q after the undo answered %+v, want bob's kept", holder, line.Outcome)
		}
	})
}

// TestGoingUndoOfAClaimWhoseAnswerSaysNothingOfAChangePutsNothingBack (review round 4, finding 4):
// a claim answer that does not say whether the claim changed is read as nothing known of what the
// write left, as a value after that could not be read is, so its undo releases nothing: alice,
// who held the claim before the turn, keeps it.
func TestGoingUndoOfAClaimWhoseAnswerSaysNothingOfAChangePutsNothingBack(t *testing.T) {
	rig := newGoingWritesRig(t)
	issue := createInteractionIssue(t, rig.mux, "DEMO", "Held, answered vaguely", "The spec.\n").Key
	rig.fixture(t, "alice", http.MethodPost, "/api/v1/issues/"+issue+"/claim", map[string]any{})
	inner := rig.served.inProcess
	t.Cleanup(func() { rig.served.inProcess = inner })
	rig.served.inProcess = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/v1/issues/"+issue+"/claim" {
			inner.ServeHTTP(w, r)
			return
		}
		served := httptest.NewRecorder()
		inner.ServeHTTP(served, r)
		var answer map[string]json.RawMessage
		if err := json.Unmarshal(served.Body.Bytes(), &answer); err != nil {
			t.Errorf("read the claim's answer: %v", err)
		}
		delete(answer, "claim_changed")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(served.Code)
		_ = json.NewEncoder(w).Encode(answer)
	})
	run, _ := rig.runOne(t, "dispatch_claim", map[string]any{"issue": issue}, nil)
	rig.served.inProcess = inner
	rig.undo(t, "alice", run, nil)
	if holder := rig.holder(t, issue); holder != "alice" {
		t.Fatalf("alice's claim reads held by %q after the undo of a claim whose answer said nothing of a change, want it hers", holder)
	}
}

// TestGoingUndoOfACreatedIssueWhoseCloseIsRefusedNamesTheReasonItPosted (review round 4, finding
// 6): the undo of an issue the turn created posts its reason and closes it; where the close is
// refused after the reason landed, the undo's line names the reason message it left posted, as a
// write's refused line names what of it landed.
func TestGoingUndoOfACreatedIssueWhoseCloseIsRefusedNamesTheReasonItPosted(t *testing.T) {
	rig := newGoingWritesRig(t)
	createInteractionIssue(t, rig.mux, "DEMO", "An older issue", "The spec.\n")
	run, line := rig.runOne(t, "dispatch_issue", map[string]any{"project": "DEMO", "title": "Made, refused its close", "force": true}, nil)
	inner := rig.served.inProcess
	t.Cleanup(func() { rig.served.inProcess = inner })
	rig.served.inProcess = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPatch && r.URL.Path == "/api/v1/issues/"+line.Item {
			writeError(w, "ISSUE_CONFLICT", http.StatusConflict, "the close was refused")
			return
		}
		inner.ServeHTTP(w, r)
	})
	refused := oneLine(t, rig.undo(t, "alice", run, nil), false)
	if posted := "POST /api/v1/issues/" + line.Item + "/messages"; !strings.Contains(refused.Outcome.Message, posted) ||
		!strings.Contains(refused.Fields[0].Message, posted) {
		t.Fatalf("the undo whose close was refused answered %+v, want its line naming %s, which landed", refused, posted)
	}
}

// TestGoingUndoWritesBackOnlyWhatTheTurnLeftAsTheWriteLands (review round 4, principle 2; check
// c21): a write back puts the value before back only while the item still holds what the turn
// left, decided in the write itself: a change another writer makes between the undo's read and
// its write is kept, for an issue's fields, its rank, an ask's text and a document's content.
func TestGoingUndoWritesBackOnlyWhatTheTurnLeftAsTheWriteLands(t *testing.T) {
	rig := newGoingWritesRig(t)
	issue := createInteractionIssue(t, rig.mux, "DEMO", "Written back meanwhile", "The spec.\n").Key
	inner := rig.served.inProcess
	t.Cleanup(func() { rig.served.inProcess = inner })

	t.Run("an issue's fields", func(t *testing.T) {
		run, _ := rig.runOne(t, "dispatch_issue_update", map[string]any{"issue": issue, "title": "Renamed by the bar"}, nil)
		rig.served.inProcess = goingBefore(inner, http.MethodPatch+" /api/v1/issues/"+issue, func() {
			rig.fixture(t, "bob", http.MethodPatch, "/api/v1/issues/"+issue, map[string]any{"title": "Bob's title"})
		})
		line := oneLine(t, rig.undo(t, "alice", run, nil), false)
		rig.served.inProcess = inner
		if title := rig.issueRead(t, issue).Title; title != "Bob's title" {
			t.Fatalf("the issue is titled %q after the undo answered %+v, want bob's title kept", title, line.Outcome)
		}
	})

	t.Run("an issue's rank", func(t *testing.T) {
		first, second, third := rig.issue(t, "First"), rig.issue(t, "Second"), rig.issue(t, "Third")
		moved := rig.issue(t, "Moved")
		run, _ := rig.runOne(t, "dispatch_issue_update", map[string]any{"issue": moved, "after": first, "before": second}, nil)
		rig.served.inProcess = goingBefore(inner, http.MethodPatch+" /api/v1/issues/"+moved, func() {
			rig.fixture(t, "bob", http.MethodPatch, "/api/v1/issues/"+moved, map[string]any{"rank": map[string]any{"after": second, "before": third}})
		})
		line := oneLine(t, rig.undo(t, "alice", run, nil), false)
		rig.served.inProcess = inner
		at, after, before := rig.issueRead(t, moved).Rank, rig.issueRead(t, second).Rank, rig.issueRead(t, third).Rank
		if after >= at || at >= before {
			t.Fatalf("the issue stands at %s after the undo answered %+v, want bob's place between %s and %s kept", at, line.Outcome, second, third)
		}
	})

	t.Run("an ask's text", func(t *testing.T) {
		ask := rig.ask(t, issue, "Ship it on Friday?")
		run, _ := rig.runOne(t, "dispatch_edit_ask", map[string]any{"ask": ask, "question": "Ship it on Monday?"}, nil)
		rig.served.inProcess = goingBefore(inner, http.MethodPatch+" /api/v1/asks/"+ask, func() {
			rig.fixture(t, "alice", http.MethodPatch, "/api/v1/asks/"+ask, map[string]any{"question": "Ship it on Tuesday?"})
		})
		line := oneLine(t, rig.undo(t, "alice", run, nil), false)
		rig.served.inProcess = inner
		if question := decodeBody[goingAskRead](t, rig.fixture(t, "alice", http.MethodGet, "/api/v1/asks/"+ask, nil)).Ask.Question; question != "Ship it on Tuesday?" {
			t.Fatalf("the ask reads %q after the undo answered %+v, want the later words kept", question, line.Outcome)
		}
	})

	t.Run("a document's content", func(t *testing.T) {
		notes := uploadDocument(t, rig.mux, "/api/v1/issues/"+issue, "meanwhile.md", "# Notes\n\nThe first line.\n")
		run, _ := rig.runOne(t, "dispatch_doc_edit", map[string]any{
			"issue": issue, "artifact": notes.Artifact.Slug,
			"ops": []any{map[string]any{"op": "replace", "find": "first line", "with": "opening line"}},
		}, nil)
		rig.served.inProcess = goingBefore(inner, http.MethodPost+" /api/v1/issues/"+issue+"/artifacts", func() {
			rig.fixture(t, "alice", http.MethodPost, "/api/v1/artifacts/"+notes.Artifact.ID+"/edits", map[string]any{
				"ops": []any{map[string]any{"op": "replace", "find": "opening line", "with": "later line"}},
			})
		})
		line := oneLine(t, rig.undo(t, "alice", run, nil), false)
		rig.served.inProcess = inner
		text := decodeBody[artifactText](t, rig.fixture(t, "alice", http.MethodGet, "/api/v1/artifacts/"+notes.Artifact.ID+"/text", nil)).Markdown
		if !strings.Contains(text, "later line") {
			t.Fatalf("the document reads %q after the undo answered %+v, want the later edit kept", text, line.Outcome)
		}
	})

	t.Run("a created issue's close", func(t *testing.T) {
		run, made := rig.runOne(t, "dispatch_issue", map[string]any{"project": "DEMO", "title": "Made, then started by bob", "force": true}, nil)
		rig.served.inProcess = goingBefore(inner, http.MethodPatch+" /api/v1/issues/"+made.Item, func() {
			rig.fixture(t, "bob", http.MethodPatch, "/api/v1/issues/"+made.Item, map[string]any{"status": "in_progress"})
		})
		line := oneLine(t, rig.undo(t, "alice", run, nil), false)
		rig.served.inProcess = inner
		if status := rig.issueRead(t, made.Item).Status; status != "in_progress" {
			t.Fatalf("the created issue is %s after the undo answered %+v, want bob's in_progress kept", status, line.Outcome)
		}
		if posted := "POST /api/v1/issues/" + made.Item + "/messages"; line.Outcome.Code != codeGoingUndoChanged || !strings.Contains(line.Outcome.Message, posted) {
			t.Fatalf("the undo answered %+v, want it refused %s naming %s, the reason it left posted", line.Outcome, codeGoingUndoChanged, posted)
		}
	})

	t.Run("a resolved thread", func(t *testing.T) {
		comment := decodeBody[model.Comment](t, rig.fixture(t, "alice", http.MethodPost, "/api/v1/issues/"+issue+"/comments", map[string]any{"body": "A thread bob settles again."})).ID
		run, _ := rig.runOne(t, "dispatch_resolve_comment", map[string]any{"comment": comment}, nil)
		rig.served.inProcess = goingBefore(inner, http.MethodPost+" /api/v1/comments/"+comment+"/reopen", func() {
			rig.fixture(t, "bob", http.MethodPost, "/api/v1/comments/"+comment+"/reopen", nil)
			rig.fixture(t, "bob", http.MethodPost, "/api/v1/comments/"+comment+"/resolve", nil)
		})
		line := oneLine(t, rig.undo(t, "alice", run, nil), false)
		rig.served.inProcess = inner
		read := decodeBody[commentRead](t, rig.fixture(t, "alice", http.MethodGet, "/api/v1/comments/"+comment, nil))
		if !read.Comment.Resolved || read.Comment.ResolvedBy == nil || model.CanonicalLogin(read.Comment.ResolvedBy.ID) != "bob" {
			t.Fatalf("the thread reads resolved %v by %+v after the undo answered %+v, want bob's resolution kept", read.Comment.Resolved, read.Comment.ResolvedBy, line.Outcome)
		}
	})
}

// TestGoingUndoOfAWriteClaimedAfterAnotherUndoRecordedResumesFromThatRecord (review round 4,
// finding 3, in the undo): an undo reads the run, and before its claim lands another undo of the
// run posts the created issue's reason, is refused its close and records that. The first undo
// must not resume from the run it read, which records no reason posted: it resumes at the close.
func TestGoingUndoOfAWriteClaimedAfterAnotherUndoRecordedResumesFromThatRecord(t *testing.T) {
	rig := newGoingWritesRig(t)
	createInteractionIssue(t, rig.mux, "DEMO", "An older issue", "The spec.\n")
	run, line := rig.runOne(t, "dispatch_issue", map[string]any{"project": "DEMO", "title": "Undone in two tabs", "force": true}, nil)
	writeID := rig.record(t, "alice", run).Writes[0].ID
	inner := rig.served.inProcess
	t.Cleanup(func() { rig.served.inProcess = inner })
	sent := map[string]int{}
	rig.served.inProcess = goingCommittedThenRefused(inner, "", sent)
	messages := http.MethodPost + " /api/v1/issues/" + line.Item + "/messages"
	rig.holdGoingRun(t, run, func(held pgx.Tx) {
		if _, err := held.Exec(context.Background(), `
			update going_runs set undos = undos || jsonb_build_array(jsonb_build_object(
			  'write_id', $2::text, 'settled', false, 'pending', false, 'fields', '[]'::jsonb, 'at', now(),
			  'outcome', jsonb_build_object('ok', false, 'status', 503, 'code', 'SERVICE_UNAVAILABLE', 'message', 'refused'),
			  'requests', jsonb_build_array(
			    jsonb_build_object('method', 'POST', 'path', '/api/v1/issues/' || $3::text || '/messages', 'outcome', jsonb_build_object('ok', true, 'status', 201)),
			    jsonb_build_object('method', 'PATCH', 'path', '/api/v1/issues/' || $3::text,
			      'outcome', jsonb_build_object('ok', false, 'status', 503, 'code', 'SERVICE_UNAVAILABLE', 'message', 'refused')))))
			where id = $1::uuid`, run, writeID, line.Item); err != nil {
			t.Fatalf("record the other undo: %v", err)
		}
	}, func() {
		rig.browser(t, "alice", http.MethodPost, "/api/v1/going/runs/"+run+"/undo", nil)
	})
	if sent[messages] != 0 || rig.issueRead(t, line.Item).Status != "done" {
		t.Fatalf("the undo claimed after the other recorded sent %s %d times and left the issue %s, want it resumed at the close",
			messages, sent[messages], rig.issueRead(t, line.Item).Status)
	}
}

// TestGoingUndoOfACreatedIssueWhoseResumedCloseCommittedIsRecordedDone (review round 5, finding
// 1; check c21): an undo posts the created issue's reason and its close is refused 503 before it
// commits; the next undo resumes at the close, which commits and then answers 503. That close
// landed, so the undo is recorded done, the write is no longer undoable, and a third undo finds
// nothing to undo rather than calling the turn's own close another writer's.
func TestGoingUndoOfACreatedIssueWhoseResumedCloseCommittedIsRecordedDone(t *testing.T) {
	rig := newGoingWritesRig(t)
	createInteractionIssue(t, rig.mux, "DEMO", "An older issue", "The spec.\n")
	run, made := rig.runOne(t, "dispatch_issue", map[string]any{"project": "DEMO", "title": "Closed on the second undo", "force": true}, nil)
	inner := rig.served.inProcess
	t.Cleanup(func() { rig.served.inProcess = inner })
	closes := 0
	rig.served.inProcess = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPatch && r.URL.Path == "/api/v1/issues/"+made.Item {
			closes++
			if closes == 2 {
				inner.ServeHTTP(httptest.NewRecorder(), r)
			}
			if closes <= 2 {
				writeError(w, "SERVICE_UNAVAILABLE", http.StatusServiceUnavailable, "the close was refused")
				return
			}
		}
		inner.ServeHTTP(w, r)
	})
	if first := oneLine(t, rig.undo(t, "alice", run, nil), false); first.Outcome.Status != http.StatusServiceUnavailable {
		t.Fatalf("the first undo answered %+v, want the close's 503", first.Outcome)
	}
	second := oneLine(t, rig.undo(t, "alice", run, nil), true)
	if status := rig.issueRead(t, made.Item).Status; status != "done" {
		t.Fatalf("the created issue is %s after the second undo answered %+v, want done", status, second.Outcome)
	}
	if third := rig.undo(t, "alice", run, nil); len(third.Lines) != 0 {
		t.Fatalf("a third undo answered %+v, want nothing left to undo", third.Lines)
	}
	if closes != 2 {
		t.Fatalf("the close was sent %d times, want twice: refused, then committed", closes)
	}
}

// TestGoingUndoOfACreatedIssueSomeoneMovedSinceClosesNothing (review round 6, finding 2): the turn
// creates an issue, and before the undo bob moves it to in_progress. The undo expects the status
// the turn left, not the one it reads, so it closes nothing and posts no reason: the issue stays
// in_progress and the line is refused, naming the change.
func TestGoingUndoOfACreatedIssueSomeoneMovedSinceClosesNothing(t *testing.T) {
	rig := newGoingWritesRig(t)
	createInteractionIssue(t, rig.mux, "DEMO", "An older issue", "The spec.\n")
	run, made := rig.runOne(t, "dispatch_issue", map[string]any{"project": "DEMO", "title": "Made, then started by bob", "force": true}, nil)
	rig.fixture(t, "bob", http.MethodPatch, "/api/v1/issues/"+made.Item, map[string]any{"status": "in_progress"})
	const reason = "Undone after bob started it."
	line := oneLine(t, rig.undo(t, "alice", run, map[string]any{"reason": reason}), false)
	if status := rig.issueRead(t, made.Item).Status; status != "in_progress" {
		t.Fatalf("the created issue is %s after the undo answered %+v, want bob's in_progress kept", status, line.Outcome)
	}
	if line.Outcome.Code != codeGoingUndoChanged || !strings.Contains(line.Outcome.Message, "in_progress") {
		t.Fatalf("the undo answered %+v, want it refused %s naming in_progress", line.Outcome, codeGoingUndoChanged)
	}
	var posted int
	if err := rig.database.Pool.QueryRow(context.Background(),
		`select count(*) from messages where issue_key = $1 and body = $2`, made.Item, reason).Scan(&posted); err != nil || posted != 0 {
		t.Fatalf("the created issue holds %d messages saying %q (%v), want no reason posted", posted, reason, err)
	}
}

// TestGoingUndoOfAReleaseOfASessionsClaimClaimsNothingForTheSession (review round 4, finding 2):
// the person releases the claim a session holds; the turn's undo, made by the person, cannot take
// the claim back for that session and must not take it for the person, who never held it. The
// issue stays unclaimed, the line is refused naming the session, and the write is settled, so no
// later undo tries it again.
func TestGoingUndoOfAReleaseOfASessionsClaimClaimsNothingForTheSession(t *testing.T) {
	rig := newGoingWritesRig(t)
	issue := createInteractionIssue(t, rig.mux, "DEMO", "A session's claim", "The spec.\n").Key
	if _, err := rig.database.Pool.Exec(context.Background(),
		`update issues set claimed_by = jsonb_build_object('kind', 'session', 'id', $2::text), claimed_at = now() where key = $1`,
		issue, goingPlanner); err != nil {
		t.Fatalf("give the issue's claim to %s: %v", goingPlanner, err)
	}
	run, _ := rig.runOne(t, "dispatch_claim", map[string]any{"issue": issue, "release": true}, nil)
	if holder := rig.holder(t, issue); holder != "" {
		t.Fatalf("the release left the claim with %q", holder)
	}
	line := oneLine(t, rig.undo(t, "alice", run, nil), false)
	if holder := rig.holder(t, issue); holder != "" {
		t.Fatalf("the undo of a release of %s's claim gave the claim to %q, want the issue left unclaimed", goingPlanner, holder)
	}
	if line.Outcome.Status != http.StatusConflict || !strings.Contains(line.Outcome.Message, goingPlanner) {
		t.Fatalf("the undo answered %+v, want it refused naming %s", line.Outcome, goingPlanner)
	}
	if undos := rig.record(t, "alice", run).Undos; len(undos) != 1 || !undos[0].Settled {
		t.Fatalf("the run keeps the undos %+v, want the refusal settled", undos)
	}
}

// TestGoingUndoOfAReleaseClaimsOnlyAsTheHolderItReleased (review round 4, finding 2): the undo
// takes a released claim back only where the holder the turn released is the holder the claim
// route records the undo's own claim as. An undo made through a credential bound to a session
// records that session, so it does not take back the person's own claim, which would give the
// session a claim it never held: it sends no claim at all, and is refused naming the person.
func TestGoingUndoOfAReleaseClaimsOnlyAsTheHolderItReleased(t *testing.T) {
	doer := &countingDoer{}
	undoer := goingUndoer{doer: doer, login: "alice", session: goingPlanner, observer: goingObserver{read: goingReader{doer: doer}}}
	write := goingRunWrite{ID: "w-release", Action: "dispatch_claim", Target: "DEMO-1", Fields: []string{"claim"},
		Before: json.RawMessage(`{"kind":"user","id":"alice"}`), After: json.RawMessage(`null`), Outcome: goingWriteOutcome{OK: true}}
	_, outcome := undoer.claim(context.Background(), write)
	for _, request := range doer.requests {
		if request.Method != http.MethodGet {
			t.Fatalf("the undo sent %s %s, want no write: its claim would be recorded as %s, which never held the issue", request.Method, request.URL.Path, goingPlanner)
		}
	}
	if outcome.OK || outcome.Code != codeGoingUndoNotCallers || !strings.Contains(outcome.Message, "alice") || !goingUndoSettles(outcome) {
		t.Fatalf("the undo answered %+v, want it refused, settled, naming alice", outcome)
	}
}
