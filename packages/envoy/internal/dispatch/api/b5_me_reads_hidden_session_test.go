package api

import (
	"encoding/json"
	"maps"
	"net/http"
	"slices"
	"testing"

	"github.com/google/uuid"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// TestTheMeReadsHoldNoConversationOfAHiddenSession: carol broadcasts to session-priv, an agent of
// PRIV, and to session-pub, an agent of PUB; each answers its copy, and carol pins both. Her
// browser sees both projects, so GET /api/v1/me/replies?kind=agent holds both replies, GET
// /api/v1/me/agents/state counts each unread and names each pin, and her event log holds the
// user_agent_state.updated of each pin. Her token that lists no project and her token that lists
// PUB see PUB alone, so each of those reads holds session-pub alone: no row, total, unread count,
// pin or state event of session-priv's conversation, which the thread read answers as missing
// too. With PRIV public again, carol's browser and her token that lists no project see every
// object and read both, as every caller did before a project could be private (R9).
func TestTheMeReadsHoldNoConversationOfAHiddenSession(t *testing.T) {
	listener, _ := newBroadcastListener(t, privateRoster)
	seed := seedPrivateProjectWith(t, testServerOptions{envoyURL: listener.URL})
	seedSessionWork(t, seed)
	const callerCarolPubToken privateCaller = "carol's token that lists PUB"
	mintListingToken(t, seed, callerCarolPubToken, seedCarol, []string{seedPublic})

	sent := keyedRequest(t, seed.handler, http.MethodPost, "/api/v1/broadcasts", map[string]any{
		"body": "Status?", "delivery": "btw", "session_ids": []string{sessionOfPrivate, sessionOfPublic},
	}, seedCarol, uuid.NewString())
	if sent.Code != http.StatusCreated {
		t.Fatalf("carol broadcasts to both sessions: %d %s", sent.Code, sent.Body.String())
	}
	projectOf := map[string]string{sessionOfPrivate: seedPrivate, sessionOfPublic: seedPublic}
	for _, recipient := range decodeBody[broadcastResponse](t, sent).Recipients {
		seed.asSession(t, recipient.SessionID, projectOf[recipient.SessionID], http.MethodPost,
			"/api/v1/messages/"+recipient.Message.ID+"/reply",
			map[string]any{"attempt": 1, "body": "Green on " + recipient.SessionID + "."}, http.StatusCreated)
	}
	for _, session := range []string{sessionOfPrivate, sessionOfPublic} {
		seed.mustRequest(t, callerCarol, http.MethodPut, "/api/v1/me/agents/"+session+"/state",
			map[string]any{"pinned": true}, http.StatusOK)
	}

	both := []string{sessionOfPrivate, sessionOfPublic}
	public := []string{sessionOfPublic}
	requireSessions := func(t *testing.T, caller privateCaller, want []string) {
		t.Helper()
		// The Agent rows unread (the Inbox's badge) and in the Inbox, read whole and paged, which
		// counts the total.
		for _, query := range []string{"kind=agent", "kind=agent&state=inbox", "kind=agent&limit=10"} {
			read := decodeBody[model.MyRepliesResponse](t, seed.mustRequest(t, caller, http.MethodGet, "/api/v1/me/replies?"+query, nil, http.StatusOK))
			got := []string{}
			for _, row := range read.Rows {
				if row.Session != nil {
					got = append(got, row.Session.ID)
				}
			}
			slices.Sort(got)
			if !slices.Equal(got, want) || read.Total != len(want) {
				t.Errorf("%s: GET /api/v1/me/replies?%s holds the replies of %v, total %d; want %v, total %d",
					caller, query, got, read.Total, want, len(want))
			}
		}
		states := decodeBody[userAgentStates](t, seed.mustRequest(t, caller, http.MethodGet, "/api/v1/me/agents/state", nil, http.StatusOK))
		if got := slices.Sorted(maps.Keys(states)); !slices.Equal(got, want) {
			t.Errorf("%s: GET /api/v1/me/agents/state names %v, want %v", caller, got, want)
		}
		for _, session := range want {
			if state := states[session]; state.UnreadReplies != 1 || state.PinnedAt == nil {
				t.Errorf("%s: GET /api/v1/me/agents/state holds %s as %+v, want its one unread reply and its pin", caller, session, state)
			}
		}
		named := []string{}
		for _, event := range eventLogOf(t, seed, caller) {
			var payload model.UserAgentStateEventPayload
			if event.Type != "user_agent_state.updated" || json.Unmarshal(event.Payload, &payload) != nil {
				continue
			}
			named = append(named, payload.SessionID)
		}
		slices.Sort(named)
		if !slices.Equal(named, want) {
			t.Errorf("%s: the event log holds the user_agent_state.updated of %v, want %v", caller, named, want)
		}
	}

	t.Run("PRIV private", func(t *testing.T) {
		requireSessions(t, callerCarol, both)
		requireSessions(t, callerCarolToken, public)
		requireSessions(t, callerCarolPubToken, public)
	})
	seed.setVisibility(t, seedPrivate, model.ProjectPublic)
	t.Run("no private project", func(t *testing.T) {
		requireSessions(t, callerCarol, both)
		requireSessions(t, callerCarolToken, both)
	})
}
