package api

import (
	"fmt"
	"net/http"
	"testing"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// TestAKeyedRepeatOnAnObjectOfNoProjectIsReplayed: an object that belongs to no project - a
// session's file, a direct message - is neither hidden nor missing to a keyed repeat. The server's
// read finds its row (projectOfRow's found), and no project's access list keeps a caller from it,
// so the repeat is answered from the key as it was before any project could be private. Only a read
// that finds no row is missing. PRIV is private, so neither caller below sees every object, and
// each repeat is judged.
//
//   - A session's file: alice uploads a file into session s1's conversation and comments on it at
//     her browser under a key, which is refused 400 ARTIFACT_AGENT_OWNED (a conversation's file
//     takes no comment, and no keyed write of /api/v1/artifacts/{id} lands on one); her repeat is
//     replayed that refusal whole, as a found object's refusal is, not answered the words a
//     missing object's refusal gets (keyedRefusalReplayed).
//   - A direct message's accept: alice sends s1 a direct message; the shared token, acting as s1,
//     accepts its first attempt under a key and repeats the accept, as envoy-client's retry does
//     (accept-user-turn.ts), and is replayed the first 200, which the session reads as its turn.
//
// Control: missing read from the project the server's read answered (missing = found == ""), as it
// was, under which the accept's repeat is refused 409 KEY_COMMITTED and the comment's repeat is
// answered the missing object's words.
func TestAKeyedRepeatOnAnObjectOfNoProjectIsReplayed(t *testing.T) {
	t.Parallel()
	live := true
	sent := []map[string]any{}
	listener := sessionListener(t, &live, &sent)
	t.Cleanup(listener.Close)
	seed := seedPrivateProjectWith(t, testServerOptions{envoyURL: listener.URL})
	// While a project is private, a session passes a viewer only once Dispatch has placed it in a
	// project and tied it to a person (access.SessionClause, access.SessionContentClause): s1 wrote
	// in PUB for alice, who is on no private project's list, so every caller here sees it.
	for _, statement := range []struct{ sql, value string }{
		{`insert into session_projects (session_id, project_key) values ('s1', $1)`, seedPublic},
		{`insert into session_owners (session_id, login) values ('s1', $1)`, seedAlice},
	} {
		if _, err := seed.database.Pool.Exec(t.Context(), statement.sql, statement.value); err != nil {
			t.Fatalf("place s1 in %s for %s: %v", seedPublic, seedAlice, err)
		}
	}
	replayed := func(t *testing.T, what string, first, again int, firstBody, againBody, replayHeader string) {
		t.Helper()
		if again != first || againBody != firstBody || replayHeader == "" {
			t.Errorf("%s = %d %s (replayed %q), want the first %d replayed", what, again, againBody, replayHeader, first)
		}
	}

	t.Run("a session's file", func(t *testing.T) {
		uploaded := multipartRequest(t, seed.handler, "/api/v1/agents/s1/artifacts", map[string]string{"name": "notes.txt"},
			"notes.txt", "text/plain", []byte("Plain notes in s1's conversation."), seedAlice)
		if uploaded.Code != http.StatusCreated {
			t.Fatalf("alice's upload into s1's conversation = %d %s, want 201", uploaded.Code, uploaded.Body.String())
		}
		file := decodeBody[uploadedArtifact](t, uploaded).Artifact
		if file.IssueKey != nil || file.Project != "" {
			t.Fatalf("the session's file = %+v, want one of no issue and no project: the case proves nothing", file)
		}
		target := "/api/v1/artifacts/" + file.ID + "/comments"
		comment := map[string]any{"body": "A note on the session's file."}
		first := keyedSeedRequest(t, seed, callerAlice, http.MethodPost, target, comment, "comment-session-file")
		if code, _ := keyedAnswerOf(t, first); first.Code != http.StatusBadRequest || code != "ARTIFACT_AGENT_OWNED" {
			t.Fatalf("alice's keyed comment on the session's file = %d %s, want 400 ARTIFACT_AGENT_OWNED", first.Code, first.Body.String())
		}
		again := keyedSeedRequest(t, seed, callerAlice, http.MethodPost, target, comment, "comment-session-file")
		replayed(t, "alice's repeat of the refused comment", first.Code, again.Code, first.Body.String(), again.Body.String(),
			again.Header().Get(contracts.IdempotencyReplayedHeader))
	})

	t.Run("a direct message's accept", func(t *testing.T) {
		created := postAgentMessage(t, seed.handler, "s1", map[string]any{"body": "Where is the dashboard?", "delivery": "aside"}, seedAlice)
		if created.Code != http.StatusCreated {
			t.Fatalf("alice's direct message = %d %s, want 201", created.Code, created.Body.String())
		}
		message := decodeBody[model.Message](t, created)
		if message.IssueKey != nil {
			t.Fatalf("the direct message = %+v, want one of no issue: the case proves nothing", message)
		}
		target := fmt.Sprintf("/api/v1/messages/%s/deliveries/1/accept", message.ID)
		accept := map[string]any{"actor": map[string]any{"kind": "session", "id": "s1"}}
		first := keyedSeedRequest(t, seed, callerSharedToken, http.MethodPost, target, accept, "accept-direct")
		if first.Code != http.StatusOK {
			t.Fatalf("the shared token's keyed accept as s1 = %d %s, want 200", first.Code, first.Body.String())
		}
		again := keyedSeedRequest(t, seed, callerSharedToken, http.MethodPost, target, accept, "accept-direct")
		replayed(t, "the shared token's repeat of the accept", first.Code, again.Code, first.Body.String(), again.Body.String(),
			again.Header().Get(contracts.IdempotencyReplayedHeader))
	})
}
