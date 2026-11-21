package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
	"time"
)

// registeredUnderPrivate is a session the stand-in listener knows by the token it registered with,
// which lists PRIV (K6), and which has written one event, a comment on PUB's issue, so its one pair
// from an event places it in PUB alone. idleUnderPrivate registered the same way and wrote nothing.
const (
	registeredUnderPrivate = "session-registered-priv"
	idleUnderPrivate       = "session-idle-priv"
	registrationRoster     = `[
	{"session_id":"session-registered-priv","title":"Registered","capabilities":["btw","steer"],"projects":["PRIV"],"last_seen":200},
	{"session_id":"session-idle-priv","title":"Idle","capabilities":["btw","steer"],"projects":["PRIV"],"last_seen":100}
]`
)

// TestADirectMessageToAPrivateSessionIsHiddenFromNonMembers (t-c-26, envoy.WithRegistrations,
// events.Registrations): a session that registered at the listener with a token listing PRIV, and
// whose only event is on PUB, is one of PRIV's agents from the moment Dispatch resolves it: carol,
// a member of PRIV, sends it a direct message, the send's read of the session records (session,
// PRIV, registration), and alice, an open viewer who does not see PRIV, the shared token and
// carol's token that lists no project each read neither the message nor its conversation nor its
// event in the event log, each answering as missing. carol, at her browser and with her token that
// lists PRIV, the conversation's party who sees PRIV, reads all three.
//
// A session that registered under PRIV and wrote no event has no pair, and #4177's rule judges it
// by its listener record (access.SessionClause): until a read of the listener records it, carol's
// own message to it answers 404, as for a session nobody holds. Her agent list's read of the
// listener (Sessions) records it under PRIV, and her message to it is then sent and is hidden
// from the same three.
//
// Control, in the test: with the pairs the listener's answers recorded taken away (today's
// recording, a client with no WithRegistrations), the session is placed in PUB alone by its event,
// so the shared token, carol's token that lists no project and alice each read the message to it,
// and carol's token that lists PRIV alone no longer does; the check fails naming all four.
func TestADirectMessageToAPrivateSessionIsHiddenFromNonMembers(t *testing.T) {
	listener, _ := newBroadcastListener(t, registrationRoster)
	seed := seedPrivateProjectWith(t, testServerOptions{envoyURL: listener.URL})
	seed.asSession(t, registeredUnderPrivate, seedPublic, http.MethodPost, "/api/v1/issues/"+seed.issue[seedPublic]+"/comments",
		map[string]any{"body": "Done on PUB."}, http.StatusCreated)
	ctx := context.Background()
	recordedUnderPrivate := func(session string) bool {
		var recorded int
		if err := seed.database.Pool.QueryRow(ctx, `
			select count(*) from session_projects
			where project_key = 'PRIV' and proven_by = 'registration' and session_id = $1
		`, session).Scan(&recorded); err != nil {
			t.Fatalf("read the recorded registrations: %v", err)
		}
		return recorded == 1
	}
	send := func(session string, status int) *httptest.ResponseRecorder {
		return seed.mustRequest(t, callerCarol, http.MethodPost, "/api/v1/agents/"+session+"/messages",
			map[string]any{"body": "Status on PRIV?", "delivery": "btw"}, status)
	}

	direct := map[string]string{registeredUnderPrivate: createdID(t, send(registeredUnderPrivate, http.StatusCreated))}
	waitUntil(t, 10*time.Second, "the send's read of "+registeredUnderPrivate+" recording its registration under PRIV", func() bool {
		return recordedUnderPrivate(registeredUnderPrivate)
	})
	send(idleUnderPrivate, http.StatusNotFound)
	seed.mustRequest(t, callerCarol, http.MethodGet, "/api/v1/agents", nil, http.StatusOK)
	if !recordedUnderPrivate(idleUnderPrivate) {
		t.Fatalf("carol's agent list recorded no registration of %s under PRIV", idleUnderPrivate)
	}
	direct[idleUnderPrivate] = createdID(t, send(idleUnderPrivate, http.StatusCreated))

	// reads is whether caller reads the direct message to session: by its id, in the session's
	// conversation, and as an event in the event log.
	reads := func(t *testing.T, caller privateCaller, session string) (byID, inConversation, inLog bool) {
		t.Helper()
		id := direct[session]
		byID = seed.request(t, caller, http.MethodGet, "/api/v1/messages/"+id, nil).Code == http.StatusOK
		if window := seed.request(t, caller, http.MethodGet, "/api/v1/agents/"+session+"/messages", nil); window.Code == http.StatusOK {
			inConversation = slices.ContainsFunc(decodeBody[[]messageRead](t, window), func(message messageRead) bool { return message.Message.ID == id })
		}
		for _, event := range eventLogOf(t, seed, caller) {
			var payload struct {
				ID string `json:"id"`
			}
			if json.Unmarshal(event.Payload, &payload) == nil && payload.ID == id {
				inLog = true
			}
		}
		return byID, inConversation, inLog
	}
	nonMembers := []privateCaller{callerAlice, callerSharedToken, callerCarolToken}
	members := []privateCaller{callerCarol, callerCarolPrivToken}
	// misread names each caller that reads a message it must not, or misses one it must.
	misread := func(t *testing.T, session string, hidden []privateCaller) []privateCaller {
		t.Helper()
		var wrong []privateCaller
		for _, caller := range append(slices.Clone(hidden), members...) {
			byID, inConversation, inLog := reads(t, caller, session)
			want := !slices.Contains(hidden, caller)
			if byID != want || inConversation != want || inLog != want {
				t.Logf("%s reads the message to %s by id %v, in its conversation %v, in the event log %v; want %v",
					caller, session, byID, inConversation, inLog, want)
				wrong = append(wrong, caller)
			}
		}
		return wrong
	}
	for _, session := range []string{registeredUnderPrivate, idleUnderPrivate} {
		if wrong := misread(t, session, nonMembers); len(wrong) != 0 {
			t.Errorf("the direct message to %s, which registered under PRIV, is misread by %v", session, wrong)
		}
	}

	// Control: the record of the listener's answers taken away.
	if _, err := seed.database.Pool.Exec(ctx, `delete from session_projects where proven_by = 'registration'`); err != nil {
		t.Fatalf("control: forget the recorded registrations: %v", err)
	}
	if wrong, want := misread(t, registeredUnderPrivate, nonMembers), append(slices.Clone(nonMembers), callerCarolPrivToken); !slices.Equal(wrong, want) {
		t.Fatalf("control: with no recorded registration the message to %s is misread by %v, want %v: every non-member reads it and the token that lists PRIV alone does not",
			registeredUnderPrivate, wrong, want)
	}
}
