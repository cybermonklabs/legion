package api

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// TestASwitchWhoseMarkerReadFailsAnswersTheDatabasesError: a switch to private that cannot read
// session_projects_filled, the backfill marker it waits for, answers that failure as the server's
// error (500), never as a store that cannot hold a private project yet (409
// PRIVATE_PROJECTS_INCOMPLETE), which would send its owner to run a backfill that has run. PRIV
// stays public and alice's, nothing is appended, and no proof is made.
//
// Control: privateProjectIncomplete wrapping a failed marker read as errPrivateProjectsIncomplete,
// as it did before the switch read the store report's own marker check: the switch answers 409.
func TestASwitchWhoseMarkerReadFailsAnswersTheDatabasesError(t *testing.T) {
	admission := &recordingAdmission{}
	handler, database := newPrivateProjectsServer(t, testServerOptions{privateProjects: []string{"PRIV"}}, admission)
	if _, err := database.Pool.Exec(context.Background(), `alter table session_projects_filled rename to session_projects_filled_unreadable`); err != nil {
		t.Fatalf("make session_projects_filled unreadable: %v", err)
	}
	before := allEvents(t, database)
	response := dispatchRequest(t, handler, http.MethodPatch, "/api/v1/projects/PRIV/access",
		map[string]string{"visibility": model.ProjectPrivate}, "alice")
	if body := response.Body.String(); response.Code != http.StatusInternalServerError || strings.Contains(body, "PRIVATE_PROJECTS_INCOMPLETE") {
		t.Errorf("PATCH PRIV private with its marker unreadable = %d %s, want 500, the database's error", response.Code, body)
	}
	if visibility, owner, appended := visibilityOf(t, database, "PRIV"), projectOwnerOf(t, database, "PRIV"), allEvents(t, database)-before; visibility != model.ProjectPublic || owner != "alice" || appended != 0 {
		t.Errorf("after the refused switch PRIV is %s, owned by %s, with %d events appended; want public, alice's, none", visibility, owner, appended)
	}
	if calls := admission.taken(); len(calls) != 0 {
		t.Errorf("the refused switch made the proof: %+v", calls)
	}
}
