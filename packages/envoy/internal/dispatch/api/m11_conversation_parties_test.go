package api

import (
	"context"
	"net/http"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// TestASwitchToPrivateCountsADirectConversationThatNamesNoParty: a direct message, one on no
// issue, is read by the parties its node names (access.MessageClause, PartyClause). One whose node
// the store does not hold, or whose node names no party, passes no viewer but the server's once a
// project is private, its own parties included, so a switch to private is refused while one
// exists: 409 PRIVATE_PROJECTS_INCOMPLETE naming the message, with its cure, PRIV left public and
// alice's, no event appended and no writer called. A direct message whose node names its parties,
// as every message the route writes does, refuses nothing: once the broken one is gone or mended,
// the same switch makes PRIV private.
//
// Control: privateProjectIncomplete without its read of conversationsNamingNoParty; each refused
// switch answers 200.
func TestASwitchToPrivateCountsADirectConversationThatNamesNoParty(t *testing.T) {
	private := map[string]string{"visibility": model.ProjectPrivate}
	for _, broken := range []struct {
		name string
		// breaks leaves one direct message whose conversation names no party, given the id of the
		// direct message the route wrote, and answers the line the refusal names it by and the SQL
		// that puts the store back as it was.
		breaks func(t *testing.T, database *store.Store, sent string) (line, mend string)
	}{
		{"a direct message the store holds no node of", func(t *testing.T, database *store.Store, _ string) (string, string) {
			var id string
			if err := database.Pool.QueryRow(context.Background(), `
				insert into messages (author, body, target) values ('{"kind":"user","id":"alice"}', 'No node.', 'session:s1')
				returning id::text`).Scan(&id); err != nil {
				t.Fatalf("store a direct message with no node: %v", err)
			}
			return "message " + id + ": missing; run envoy-dispatch backfill-graph-store", `delete from messages where id = '` + id + `'`
		}},
		{"a direct message whose node names no party", func(t *testing.T, database *store.Store, sent string) (string, string) {
			var parties string
			if err := database.Pool.QueryRow(context.Background(), `
				with before as (select parties from graph_nodes where kind = 'message' and id = $1)
				update graph_nodes set parties = null where kind = 'message' and id = $1
				returning (select parties::text from before)`, sent).Scan(&parties); err != nil {
				t.Fatalf("take the parties off %s's node: %v", sent, err)
			}
			return "message " + sent + ": its node names no party; envoy-dispatch census --store names its cure",
				`update graph_nodes set parties = '` + parties + `'::text[] where kind = 'message' and id = '` + sent + `'`
		}},
	} {
		t.Run(broken.name, func(t *testing.T) {
			live := true
			sends := []map[string]any{}
			listener := sessionListener(t, &live, &sends)
			t.Cleanup(listener.Close)
			admission := &recordingAdmission{}
			handler, database := newPrivateProjectsServer(t, testServerOptions{privateProjects: []string{"PRIV"}, envoyURL: listener.URL}, admission)
			// alice writes to s1, a session of no project, which she reaches through Dispatch's record
			// of the listener's list: a list of the sessions stores it.
			if listed := dispatchRequest(t, handler, http.MethodGet, "/api/v1/agents", nil, "alice"); listed.Code != http.StatusOK {
				t.Fatalf("list the agents: %d %s", listed.Code, listed.Body.String())
			}
			sent := createdID(t, postAgentMessage(t, handler, "s1", map[string]any{"body": "alice asks s1 something.", "delivery": "aside"}, "alice"))
			line, mend := broken.breaks(t, database, sent)

			before := allEvents(t, database)
			response := dispatchRequest(t, handler, http.MethodPatch, "/api/v1/projects/PRIV/access", private, "alice")
			requireRefused(t, database, response, "PRIV", before, http.StatusConflict, "PRIVATE_PROJECTS_INCOMPLETE", line)
			if calls := admission.taken(); len(calls) != 0 {
				t.Fatalf("%s: the refused switch made the proof: %+v", broken.name, calls)
			}

			if _, err := database.Pool.Exec(context.Background(), mend); err != nil {
				t.Fatalf("mend %s: %v", broken.name, err)
			}
			if response := dispatchRequest(t, handler, http.MethodPatch, "/api/v1/projects/PRIV/access", private, "alice"); response.Code != http.StatusOK {
				t.Fatalf("with every direct message naming its parties, PATCH PRIV private = %d %s, want 200", response.Code, response.Body.String())
			}
			if got := visibilityOf(t, database, "PRIV"); got != model.ProjectPrivate {
				t.Fatalf("with every direct message naming its parties, PRIV is %s after the switch, want private", got)
			}
		})
	}
}
