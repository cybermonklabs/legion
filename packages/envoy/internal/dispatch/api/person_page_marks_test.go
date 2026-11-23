package api

import (
	"net/http"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// personNodesMarksRead is GET /api/v1/people/{login}/nodes as this file reads it: each row's
// title and the marks an edit and a delete leave on it, under the JSON names every other read of
// a post answers them by.
type personNodesMarksRead struct {
	Rows []struct {
		ID        string       `json:"id"`
		Title     string       `json:"title"`
		EditedAt  *time.Time   `json:"edited_at"`
		DeletedAt *time.Time   `json:"deleted_at"`
		DeletedBy *model.Actor `json:"deleted_by"`
	} `json:"rows"`
	Total int `json:"total"`
}

// TestAPersonPageMarksAnEditedCommentAndADeletedOne holds the person page to R2 and R3 of
// dispatch://INFRA-1523: alice writes two comments, edits the first and deletes the second, each
// as their author, and bob reads her page. The edited comment's row carries the edit's time and
// its new first line; the deleted comment keeps its row, with the delete's time and actor and an
// empty title, which gives no part of the body the delete cleared.
func TestAPersonPageMarksAnEditedCommentAndADeletedOne(t *testing.T) {
	t.Parallel()
	handler := newTestHandler(t)
	issue := createInteractionIssue(t, handler, "PAGE", "A person's corrected comments", "# Spec\n\nA passage.\n")
	const person, reader = "alice", "bob"
	edited := createThreadComment(t, handler, issue.Key, map[string]any{"body": "Ship it on Friday.\nReview is done."}, person)
	deleted := createThreadComment(t, handler, issue.Key, map[string]any{"body": "Hold the deploy until Monday."}, person)
	editedAt := editThreadComment(t, handler, edited.ID, "Ship it on Thursday.\nReview is done.", person)
	deleteThreadComment(t, handler, deleted.ID, person)

	response := dispatchRequest(t, handler, http.MethodGet, "/api/v1/people/"+person+"/nodes", nil, reader)
	if response.Code != http.StatusOK {
		t.Fatalf("%s reads %s's page: status=%d body=%s", reader, person, response.Code, response.Body.String())
	}
	page := decodeBody[personNodesMarksRead](t, response)
	if page.Total != 2 || len(page.Rows) != 2 {
		t.Fatalf("%s's page = %s, want her two comments, the deleted one in its place", person, response.Body.String())
	}
	rows := map[string]int{}
	for index, row := range page.Rows {
		rows[row.ID] = index
	}
	editedIndex, editedListed := rows[edited.ID]
	deletedIndex, deletedListed := rows[deleted.ID]
	if !editedListed || !deletedListed {
		t.Fatalf("%s's page = %s, want the edited comment %s and the deleted comment %s", person, response.Body.String(), edited.ID, deleted.ID)
	}

	editedRow := page.Rows[editedIndex]
	if editedRow.EditedAt == nil || !editedRow.EditedAt.Equal(editedAt) {
		t.Errorf("the edited comment's row edited_at = %v, want the edit's time %s", editedRow.EditedAt, editedAt)
	}
	if editedRow.Title != "Ship it on Thursday." || editedRow.DeletedAt != nil || editedRow.DeletedBy != nil {
		t.Errorf("the edited comment's row = %+v, want its new first line standing, with no delete mark", editedRow)
	}

	deletedRow := page.Rows[deletedIndex]
	if deletedRow.DeletedAt == nil {
		t.Errorf("the deleted comment's row deleted_at is absent, want the time of the delete: %s", response.Body.String())
	}
	if deletedRow.DeletedBy == nil || deletedRow.DeletedBy.Kind != "user" || deletedRow.DeletedBy.ID != person {
		t.Errorf("the deleted comment's row deleted_by = %+v, want the user %s, who deleted it", deletedRow.DeletedBy, person)
	}
	if deletedRow.Title != "" {
		t.Errorf("the deleted comment's row title = %q, want no part of the body the delete cleared", deletedRow.Title)
	}
	if deletedRow.EditedAt != nil {
		t.Errorf("the deleted comment's row edited_at = %v on a comment nobody edited", deletedRow.EditedAt)
	}
}
