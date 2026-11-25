package api

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// TestAKeptListServesNoReaderWhoseViewMovedUnderIt: a comment list's kept answer is filed under
// its owner, its ETag and what its reader sees (commentListViewOf), and the sessions its mentions
// name are left out by access.SessionClause, which reads a session's pairs beside the viewer. A
// pair moves with no write to the list, so with no move of its ETag. alice, an open member who
// sees PUB alone, reads PUB's comments, one of which mentions session-moved, which has acted in
// PUB alone; then session-moved acts in PRIV (its pair is recorded), which hides it from her and
// writes nothing PUB's list shows. Her next read must leave the mention out: while a project is
// private a reader that does not see every object is handed no kept answer and keeps none
// (keepsListAnswerFor). The same holds for the issue's event feed, whose comment.created payload
// names the session under mentions.
//
// It reports what that costs: a hot comment list of twenty comments read a hundred times by alice,
// each read built, against the same reads by alice once PRIV is public, when she sees every object
// and each read after the first is the kept one.
//
// Control: the kept answer handed to every reader, as before, and alice's second read names
// session-moved.
func TestAKeptListServesNoReaderWhoseViewMovedUnderIt(t *testing.T) {
	seed := seedPrivateProject(t)
	const moved = "session-moved"
	ctx := context.Background()
	if _, err := seed.database.Pool.Exec(ctx, `insert into session_projects (session_id, project_key) values ($1, $2)`, moved, seedPublic); err != nil {
		t.Fatalf("record %s's work in PUB: %v", moved, err)
	}
	issue := seed.issue[seedPublic]
	for index := range 20 {
		body := map[string]any{"body": "A comment of a hot list."}
		if index == 0 {
			body["mentions"] = []map[string]string{{"target": "session:" + moved}}
		}
		seed.mustRequest(t, callerCarol, http.MethodPost, "/api/v1/issues/"+issue+"/comments", body, http.StatusCreated)
	}
	awaitBackgroundWrites(t, seed.server)
	reads := []string{commentListOf(issue), "/api/v1/issues/" + issue + "/events"}
	for _, target := range reads {
		if first := seed.mustRequest(t, callerAlice, http.MethodGet, target, nil, http.StatusOK); !strings.Contains(first.Body.String(), moved) {
			t.Fatalf("alice's first read of %s names no %s, so the row tests nothing: %s", target, moved, first.Body.String())
		}
	}
	if _, err := seed.database.Pool.Exec(ctx, `insert into session_projects (session_id, project_key) values ($1, $2)`, moved, seedPrivate); err != nil {
		t.Fatalf("record %s's work in PRIV: %v", moved, err)
	}
	for _, target := range reads {
		if again := seed.mustRequest(t, callerAlice, http.MethodGet, target, nil, http.StatusOK); strings.Contains(again.Body.String(), moved) {
			t.Errorf("alice's read of %s after %s acted in PRIV names it, which she no longer sees: %s", target, moved, again.Body.String())
		}
	}

	timed := func(what string) time.Duration {
		t.Helper()
		answered := seed.server.listAnswers.answered()
		started := time.Now()
		for range 100 {
			seed.mustRequest(t, callerAlice, http.MethodGet, commentListOf(issue), nil, http.StatusOK)
		}
		took := time.Since(started)
		t.Logf("%s: 100 reads of %s's comment list took %v, %v a read; %d answered from the kept answers",
			what, issue, took, took/100, seed.server.listAnswers.answered()-answered)
		return took
	}
	built := timed("PRIV private, alice sees part")
	seed.setVisibility(t, seedPrivate, model.ProjectPublic)
	kept := timed("PRIV public, alice sees every object")
	t.Logf("a hot comment list read by a reader who sees part costs %.2fx what the kept answer costs", float64(built)/float64(kept))
}
