package api

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/agentstream"
)

// TestANewTieOrPairEndsAnAgentViewBeforeItsNextFrame: the live agent view asks its rule again when
// what the rule reads of the session changes, not only on an access change, the heartbeat or the
// re-arm. A proved write that ties the session to another person (a session_owners row) or records
// a project the session newly wrote in (a session_projects pair) can take the session from a viewer
// who watched it a moment before, and the next frame the session publishes must not reach that
// viewer. The re-arm and the heartbeat are set to an hour, so only the write can end the view.
//
//   - a new tie: alice, an open viewer not on PRIV's list, watches dora's session, which has no
//     listener list and which only dora, on no private project's list, proved. carol, who sees
//     PRIV, then writes as that session (her credential, a comment on PUB's issue), which ties it
//     to carol. alice's view must end (dropped) before the frame the session publishes next.
//   - a new pair: alice watches carol's session that lists PUB and wrote in PUB alone. carol's
//     credential then writes as it in PRIV, a project alice does not see. alice's view must end
//     (dropped) before the next frame.
//
// At cybermonklabs/legion#4177's 686719cf32 both rows fail: the frame published after the write
// reaches alice, and the view ends only on the next heartbeat (15 s by default). Found by the
// tester's drive of the agent view over /ws/live (Tester4177, round 3).
func TestANewTieOrPairEndsAnAgentViewBeforeItsNextFrame(t *testing.T) {
	t.Parallel()
	for _, row := range []struct {
		name, session, project string
	}{
		{"a new tie", contentDoraUnlistedSession, seedPublic},
		{"a new pair", contentPubListedSession, seedPrivate},
	} {
		t.Run(row.name, func(t *testing.T) {
			rig := newContentAgentRig(t, testServerOptions{agentWatchInterval: time.Hour, streamHeartbeat: time.Hour})
			client, got := agentVerdict(t, rig.seed, rig.url, callerAlice, row.session)
			if got != agentViewResponding {
				t.Fatalf("alice's agent view of %s answers %s before the write, want its responder: the row proves nothing", row.session, got)
			}
			_ = client.next(t, "alice's replay of "+row.session)
			rig.seed.asSessionOf(t, seedCarol, row.session, row.project, http.MethodPost,
				"/api/v1/issues/"+rig.seed.issue[row.project]+"/comments", map[string]any{"body": "carol, as this session."}, http.StatusCreated)
			after := `{"v":1,"kind":"turn","session":"` + row.session + `","after":"carol's write"}`
			rig.stream.Publish(row.session, agentstream.Frame(after))
			frame := client.next(t, "alice's agent view of "+row.session+" after carol's write")
			if ended, ok := frame.(LiveEnded); !ok || ended.Reason != liveEndedDropped {
				sent, _ := json.Marshal(frame)
				t.Errorf("%s: alice's agent view of %s after carol's write as it in %s sends %s, want it ended (dropped) before any frame",
					row.name, row.session, row.project, sent)
			}
		})
	}
}
