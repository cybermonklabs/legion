// priority_history_test.go drives GET /api/v1/priorities/{id}/history (priorities.go) against
// the database store/storetest's Open clones from this package's migrated template, through the
// handler the neighbouring tests drive (newTestHandlerWithStore, dispatchRequest).
//
// What it reads is the margin of the priority page: every act stated about one priority, newest
// first, each naming what was stated, the value it wrote or the issue at its other end, whether
// it asserts or retracts, who stated it and when. The acts are written the way a person writes
// them — the create, three attribute writes, a move, an issue that advances the priority and the
// retraction of that relation — so the reading is of the route and of the log beneath it alone,
// and the acts of the second priority this one is moved below are no part of it.
package api

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/prioritystore"
)

func TestPriorityHistory(t *testing.T) {
	handler, _ := newTestHandlerWithStore(t)
	createTestProject(t, handler, "CORE")
	alice := model.Actor{Kind: "user", ID: "alice"}
	bob := model.Actor{Kind: "user", ID: "bob"}
	// The window every act of this test was written in, a minute either side of it, which is
	// what each act's moment is read against.
	opened := time.Now().UTC().Add(-time.Minute)

	made := createTestPriority(t, handler, map[string]any{
		"id": "ship-the-daemon", "title": "Ship the daemon", "owner": "alice",
	}, "alice")
	// A second priority, for the move below to land this one under. Its own acts are its own:
	// no act of it is in this priority's history.
	createTestPriority(t, handler, map[string]any{"id": "cut-the-queue", "title": "Cut the queue"}, "bob")

	renamed := patchTestPriority(t, handler, made.ID, map[string]any{"title": "Ship the daemon everywhere"}, "bob")
	described := patchTestPriority(t, handler, made.ID, map[string]any{
		"description": "# Why\n\nThe daemon runs every workspace.",
	}, "bob")
	owned := patchTestPriority(t, handler, made.ID, map[string]any{"owner": "bob"}, "alice")
	moved := patchTestPriority(t, handler, made.ID, map[string]any{
		"rank": map[string]any{"after": "cut-the-queue"},
	}, "alice")

	// An issue of one project advances the priority, and its relation is then retracted by
	// somebody else: two acts of the same relation, asserted and not.
	issue := createAdvancingIssue(t, handler, map[string]any{
		"project": "CORE", "title": "Daemon work", "advances": []string{made.ID},
	}, "alice")
	retracted := dispatchRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+issue.Key, map[string]any{
		"advances": []string{}, "advances_revision": readTestIssue(t, handler, issue.Key).AdvancesRevision,
	}, "bob")
	if retracted.Code != http.StatusOK {
		t.Fatalf("retract %s's relation to %s: status=%d body=%s",
			issue.Key, made.ID, retracted.Code, retracted.Body.String())
	}

	// The whole history, newest first: the two acts of the relation, the four writes above them
	// in the order they were made, and the create's own six acts — the two the store states
	// (its visibility and the state it was made in) and the four attributes it was written
	// with, which Create states in the name order of the attributes.
	acts := readPriorityHistory(t, handler, made.ID)
	want := []model.PriorityAct{
		{Field: prioritystore.RelationAdvances, Issue: issue.Key, Asserted: false, Actor: bob},
		{Field: prioritystore.RelationAdvances, Issue: issue.Key, Asserted: true, Actor: alice},
		{Field: prioritystore.AttributeRank, Value: moved.Rank, Asserted: true, Actor: alice},
		{Field: prioritystore.AttributeOwner, Value: owned.Owner, Asserted: true, Actor: alice},
		{Field: prioritystore.AttributeDescription, Value: described.Description, Asserted: true, Actor: bob},
		{Field: prioritystore.AttributeTitle, Value: renamed.Title, Asserted: true, Actor: bob},
		{Field: prioritystore.AttributeTitle, Value: made.Title, Asserted: true, Actor: alice},
		{Field: prioritystore.AttributeRank, Value: made.Rank, Asserted: true, Actor: alice},
		{Field: prioritystore.AttributeOwner, Value: "alice", Asserted: true, Actor: alice},
		{Field: prioritystore.AttributeDescription, Value: "", Asserted: true, Actor: alice},
		{Field: prioritystore.AttributeState, Value: prioritystore.StateLive, Asserted: true, Actor: alice},
		{Field: prioritystore.AttributeVisibility, Value: prioritystore.VisibilityProject, Asserted: true, Actor: alice},
	}
	if len(acts) != len(want) {
		t.Fatalf("the history of %s holds %d acts, want the %d written: %+v", made.ID, len(acts), len(want), acts)
	}
	for index, act := range acts {
		wanted := want[index]
		if act.Field != wanted.Field || act.Value != wanted.Value || act.Issue != wanted.Issue ||
			act.Asserted != wanted.Asserted || act.Actor != wanted.Actor {
			t.Errorf("act %d = %+v, want %+v", index, act, wanted)
		}
		if act.At.IsZero() || act.At.Before(opened) || act.At.After(time.Now().UTC().Add(time.Minute)) {
			t.Errorf("act %d carries the moment %v, want the moment it was written", index, act.At)
		}
		if index > 0 && act.At.After(acts[index-1].At) {
			t.Errorf("act %d was written at %v, later than the act above it at %v: the history is newest first",
				index, act.At, acts[index-1].At)
		}
	}
	// The moved priority's own order is the one its newest rank act states, and the first key
	// it took is the one the create states, so the two rank acts are the move and the write
	// before it rather than one act read twice.
	if moved.Rank == made.Rank {
		t.Fatalf("the move left %s where it was (%q), so the history holds one order and not two", made.ID, moved.Rank)
	}

	// The second priority's acts are its own: nothing of it is in this history.
	for _, act := range acts {
		if act.Value == "Cut the queue" {
			t.Errorf("the history of %s holds an act of the other priority: %+v", made.ID, act)
		}
	}

	// An id no priority carries is the 404 the two writes of an id answer.
	missing := dispatchRequest(t, handler, http.MethodGet, "/api/v1/priorities/no-such-priority/history", nil, "alice")
	code, text := refusalOf(t, missing)
	if missing.Code != http.StatusNotFound || code != codePriorityNotFound {
		t.Fatalf("the history of an id no priority carries: status=%d code=%s error=%q, want 404 %s",
			missing.Code, code, text, codePriorityNotFound)
	}
	if !strings.Contains(text, "no-such-priority") {
		t.Fatalf("the refusal %q does not name the id it was asked for", text)
	}
}

// readPriorityHistory reads GET /api/v1/priorities/{id}/history: one priority's own acts, newest
// first.
func readPriorityHistory(t *testing.T, handler http.Handler, id string) []model.PriorityAct {
	t.Helper()
	response := dispatchRequest(t, handler, http.MethodGet, "/api/v1/priorities/"+id+"/history", nil, "alice")
	if response.Code != http.StatusOK {
		t.Fatalf("read the history of priority %s: status=%d body=%s", id, response.Code, response.Body.String())
	}
	return decodeBody[[]model.PriorityAct](t, response)
}
