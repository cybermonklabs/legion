package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

// threadSeed is verification v16's seed (seedPrivateProject) holding the threads this file reads:
// in each project, carol's comment, ask and message on its issue; a session s1 that has acted in
// PRIV, so a member of PRIV sees it (access.SessionClause); and a direct message each of alice and
// bob sent to s1, two conversations that belong to no project. alice sends hers before s1 acts in
// PRIV, while she may still see s1, and bob his after, once PRIV's list lets him see it.
type threadSeed struct {
	privateSeed
	comment, ask, message  map[string]string
	aliceDirect, bobDirect string
}

// seedThreads stands up threadSeed through the server's own routes, on a test server whose fake
// Envoy listener holds s1 live (sessionListener), as a direct message needs.
func seedThreads(t *testing.T) threadSeed {
	t.Helper()
	live := true
	sent := []map[string]any{}
	listener := sessionListener(t, &live, &sent)
	t.Cleanup(listener.Close)
	seed := seedPrivateProjectWith(t, testServerOptions{envoyURL: listener.URL})
	threads := threadSeed{privateSeed: seed, comment: map[string]string{}, ask: map[string]string{}, message: map[string]string{}}
	for _, project := range []string{seedPublic, seedPrivate} {
		key := seed.issue[project]
		threads.comment[project] = createdID(t, seed.request(t, callerCarol, http.MethodPost, "/api/v1/issues/"+key+"/comments",
			map[string]any{"body": "The comment of " + project + "."}))
		threads.ask[project] = createdID(t, seed.request(t, callerCarol, http.MethodPost, "/api/v1/issues/"+key+"/asks",
			map[string]any{"question": "The question of " + project + "?"}))
		threads.message[project] = createdID(t, seed.request(t, callerCarol, http.MethodPost, "/api/v1/issues/"+key+"/messages",
			map[string]any{"body": "The message of " + project + "."}))
	}
	// alice writes to s1 while it has acted in no project, which an open viewer reaches only through
	// Dispatch's record of the listener's list (access.SessionClause): a list of the sessions stores it.
	seed.mustRequest(t, callerCarol, http.MethodGet, "/api/v1/agents", nil, http.StatusOK)
	threads.aliceDirect = createdID(t, postAgentMessage(t, seed.handler, "s1",
		map[string]any{"body": "alice asks s1 something.", "delivery": "aside"}, seedAlice))
	createdID(t, seed.asSession(t, "s1", seedPrivate, http.MethodPost, "/api/v1/issues/"+seed.issue[seedPrivate]+"/comments",
		map[string]any{"body": "s1 works on PRIV."}, http.StatusCreated))
	threads.bobDirect = createdID(t, postAgentMessage(t, seed.handler, "s1",
		map[string]any{"body": "bob asks s1 something.", "delivery": "aside"}, seedBob))
	return threads
}

// createdID is the id a create answered, failing the test unless the create succeeded.
func createdID(t *testing.T, response *httptest.ResponseRecorder) string {
	t.Helper()
	if response.Code != http.StatusOK && response.Code != http.StatusCreated {
		t.Fatalf("create: status=%d body=%s", response.Code, response.Body.String())
	}
	created := decodeBody[struct {
		ID string `json:"id"`
	}](t, response)
	if created.ID == "" {
		t.Fatalf("create answered no id: %s", response.Body.String())
	}
	return created.ID
}

// The callers of verification v16 that do not see PRIV - alice, alice's token that lists PRIV
// (alice is on no list of it), carol's token that lists no project (public projects only) and the
// shared token - and those that do, bob's token that lists PRIV among them.
var (
	callersBlindToPrivate = []privateCaller{callerAlice, callerAlicePrivToken, callerCarolToken, callerSharedToken}
	callersOfPrivate      = []privateCaller{callerBob, callerCarol, callerCarolPrivToken, callerBobPrivToken}
)

// TestThreadsOfAPrivateProjectAnswerAsMissing: an ask, a comment, a message and the event log of a
// private project are read, by every caller that does not see it, exactly as an id or a key no row
// holds is read - the same status and the same bytes - and found by every caller that does. A
// direct conversation is read by its parties alone: bob, a restricted member, reads his own and
// not alice's. A comment list kept for carol is never handed to alice, though the cache holds it.
func TestThreadsOfAPrivateProjectAnswerAsMissing(t *testing.T) {
	threads := seedThreads(t)
	missing := uuid.NewString()
	key := threads.issue[seedPrivate]
	comment, ask, message := threads.comment[seedPrivate], threads.ask[seedPrivate], threads.message[seedPrivate]

	byID := []struct{ name, hidden, absent string }{
		{"a comment", "/api/v1/comments/" + comment, "/api/v1/comments/" + missing},
		{"a comment's blocks", "/api/v1/comments/" + comment + "/blocks", "/api/v1/comments/" + missing + "/blocks"},
		{"an ask", "/api/v1/asks/" + ask, "/api/v1/asks/" + missing},
		{"an ask's followers", "/api/v1/asks/" + ask + "/followers", "/api/v1/asks/" + missing + "/followers"},
		{"a message's conversation", "/api/v1/messages/" + message, "/api/v1/messages/" + missing},
		{"a message's blocks", "/api/v1/messages/" + message + "/blocks", "/api/v1/messages/" + missing + "/blocks"},
		{"an issue's message", "/api/v1/issues/" + key + "/messages/" + message, "/api/v1/issues/" + key + "/messages/" + missing},
	}
	// Each list keyed by PRIV's issue answers as the list of a key no issue holds: the empty list,
	// under the ETag a missing owner answers where the list carries one.
	nobody := "NOPE-404"
	lists := []struct{ name, suffix string }{
		{"the issue's comments", "/comments"},
		{"the issue's asks", "/asks"},
		{"the issue's messages", "/messages"},
		{"the issue's event feed", "/events"},
	}
	for _, caller := range callersBlindToPrivate {
		t.Run(string(caller), func(t *testing.T) {
			for _, read := range byID {
				requireSameAnswer(t, threads.privateSeed, caller, read.name, read.hidden, read.absent)
			}
			for _, list := range lists {
				hidden := threads.request(t, caller, http.MethodGet, "/api/v1/issues/"+key+list.suffix, nil)
				absent := threads.request(t, caller, http.MethodGet, "/api/v1/issues/"+nobody+list.suffix, nil)
				if hidden.Code != absent.Code || hidden.Body.String() != absent.Body.String() {
					t.Errorf("%s reads %s of %s as %d %s; a key no issue holds answers %d %s",
						caller, list.name, key, hidden.Code, hidden.Body.String(), absent.Code, absent.Body.String())
				}
			}
			requireMissingOwnerETags(t, threads, caller, key)
			if open := threads.request(t, caller, http.MethodGet, "/api/v1/asks/open?project="+seedPrivate, nil); strings.Contains(open.Body.String(), ask) {
				t.Errorf("%s lists PRIV's open ask in /asks/open?project=PRIV: %s", caller, open.Body.String())
			}
			requireLogHoldsNothingOf(t, threads.privateSeed, caller, seedPrivate)
		})
	}
	for _, caller := range callersOfPrivate {
		t.Run(string(caller)+" finds each", func(t *testing.T) {
			for _, read := range byID {
				found := threads.request(t, caller, http.MethodGet, read.hidden, nil)
				if found.Code != http.StatusOK {
					t.Errorf("%s reads %s %s as %d %s, want 200", caller, read.name, read.hidden, found.Code, found.Body.String())
				}
			}
			for _, list := range lists[:3] {
				found := threads.request(t, caller, http.MethodGet, "/api/v1/issues/"+key+list.suffix, nil)
				if !strings.Contains(found.Body.String(), comment) && !strings.Contains(found.Body.String(), ask) &&
					!strings.Contains(found.Body.String(), message) {
					t.Errorf("%s reads %s of %s without PRIV's thread: %d %s", caller, list.name, key, found.Code, found.Body.String())
				}
			}
		})
	}

	t.Run("bob reads only his own direct messages", func(t *testing.T) {
		if own := threads.request(t, callerBob, http.MethodGet, "/api/v1/messages/"+threads.bobDirect, nil); own.Code != http.StatusOK {
			t.Errorf("bob reads his own direct message as %d %s, want 200", own.Code, own.Body.String())
		}
		requireSameAnswer(t, threads.privateSeed, callerBob, "alice's direct message",
			"/api/v1/messages/"+threads.aliceDirect, "/api/v1/messages/"+missing)
		window := threads.request(t, callerBob, http.MethodGet, "/api/v1/agents/s1/messages", nil)
		if window.Code != http.StatusOK || !strings.Contains(window.Body.String(), threads.bobDirect) ||
			strings.Contains(window.Body.String(), threads.aliceDirect) {
			t.Errorf("bob's conversations with s1 = %d %s; want his own and not alice's", window.Code, window.Body.String())
		}
		for _, event := range eventLogOf(t, threads.privateSeed, callerBob) {
			if event.Project == seedPublic || strings.Contains(string(event.Payload), threads.aliceDirect) {
				t.Errorf("bob's event log holds %s %d of %q: %s", event.Type, event.ID, event.Project, event.Payload)
			}
		}
		if inbox := threads.request(t, callerBob, http.MethodGet, "/api/v1/inbox", nil); !strings.Contains(inbox.Body.String(), ask) ||
			strings.Contains(inbox.Body.String(), threads.ask[seedPublic]) {
			t.Errorf("bob's inbox = %d %s; want PRIV's ask and not PUB's", inbox.Code, inbox.Body.String())
		}
	})

	t.Run("alice's inbox holds nothing of PRIV", func(t *testing.T) {
		inbox := threads.request(t, callerAlice, http.MethodGet, "/api/v1/inbox", nil)
		if inbox.Code != http.StatusOK || strings.Contains(inbox.Body.String(), ask) || !strings.Contains(inbox.Body.String(), threads.ask[seedPublic]) {
			t.Errorf("alice's inbox = %d %s; want PUB's ask and not PRIV's", inbox.Code, inbox.Body.String())
		}
	})

	t.Run("the kept comment list on a cache hit", func(t *testing.T) {
		path := "/api/v1/issues/" + key + "/comments"
		first := threads.request(t, callerCarol, http.MethodGet, path, nil)
		if first.Code != http.StatusOK || !strings.Contains(first.Body.String(), comment) {
			t.Fatalf("carol reads PRIV's comments as %d %s", first.Code, first.Body.String())
		}
		kept := threads.server.listAnswers.writes.Load()
		if again := threads.request(t, callerCarol, http.MethodGet, path, nil); again.Body.String() != first.Body.String() ||
			threads.server.listAnswers.writes.Load() != kept+1 {
			t.Fatalf("carol's second read was not answered from the kept list: %d writes, want %d", threads.server.listAnswers.writes.Load(), kept+1)
		}
		blind := threadsRequestWith(t, threads.privateSeed, callerAlice, path, first.Header().Get("ETag"))
		if blind.Code != http.StatusOK || strings.TrimSpace(blind.Body.String()) != "[]" || strings.Contains(blind.Body.String(), comment) {
			t.Errorf("alice, after carol's list was kept and naming its ETag, reads %d %q; want 200 [] as a missing owner", blind.Code, blind.Body.String())
		}
		if threads.server.listAnswers.writes.Load() != kept+1 {
			t.Errorf("alice was answered from the list kept for carol")
		}
	})
}

// requireSameAnswer fails unless caller reads hidden exactly as it reads absent: one status, one
// body.
func requireSameAnswer(t *testing.T, seed privateSeed, caller privateCaller, name, hidden, absent string) {
	t.Helper()
	got := seed.request(t, caller, http.MethodGet, hidden, nil)
	want := seed.request(t, caller, http.MethodGet, absent, nil)
	if got.Code != want.Code || got.Body.String() != want.Body.String() {
		t.Errorf("%s reads %s (%s) as %d %s; an id no row holds answers %d %s",
			caller, name, hidden, got.Code, got.Body.String(), want.Code, want.Body.String())
	}
}

// requireMissingOwnerETags fails unless caller's comment list and event feed of the issue key carry
// the ETag an issue no row holds is answered with, so no validator says the issue is there.
func requireMissingOwnerETags(t *testing.T, threads threadSeed, caller privateCaller, key string) {
	t.Helper()
	comments := threads.request(t, caller, http.MethodGet, "/api/v1/issues/"+key+"/comments", nil)
	if want := threads.server.commentListETag(issueOwner(key), "", 0, 0, false); comments.Header().Get("ETag") != want {
		t.Errorf("%s reads %s's comment list under ETag %s; a missing owner answers %s", caller, key, comments.Header().Get("ETag"), want)
	}
	feedPath := "/api/v1/issues/" + key + "/events"
	options, err := parseEventListOptions(httptest.NewRequest(http.MethodGet, feedPath, nil))
	if err != nil {
		t.Fatalf("parse the feed's default options: %v", err)
	}
	feed := threads.request(t, caller, http.MethodGet, feedPath, nil)
	if want := threads.server.eventFeedETag(0, time.Time{}, options, false); feed.Header().Get("ETag") != want {
		t.Errorf("%s reads %s's event feed under ETag %s; a missing issue answers %s", caller, key, feed.Header().Get("ETag"), want)
	}
}

// threadsRequestWith is a GET as caller that names etag in If-None-Match.
func threadsRequestWith(t *testing.T, seed privateSeed, caller privateCaller, target, etag string) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(http.MethodGet, target, nil)
	request.Header.Set("If-None-Match", etag)
	if login := caller.login(); login != "" {
		request.Header.Set("X-Dispatch-User", login)
	} else {
		request.Header.Set("Authorization", "Bearer "+seed.bearer(t, caller))
	}
	response := httptest.NewRecorder()
	seed.handler.ServeHTTP(response, request)
	return response
}

// privateLogRow is one event of GET /api/v1/events/log, as much of it as these tests read.
type privateLogRow struct {
	ID      int64           `json:"id"`
	Type    string          `json:"type"`
	Project string          `json:"project"`
	Payload json.RawMessage `json:"payload"`
}

// eventLogOf is the whole event log as caller pages it, once the seed's horizon is proven above
// every event committed before the call (safeHorizon): a read the horizon held back would answer
// short, and a check that the log holds nothing of a project would then pass over events it never
// read (awaitHorizonRead says what holds a read back).
func eventLogOf(t *testing.T, seed privateSeed, caller privateCaller) []privateLogRow {
	t.Helper()
	safeHorizon(t, seed.server)
	response := seed.request(t, caller, http.MethodGet, "/api/v1/events/log", nil)
	if response.Code != http.StatusOK {
		t.Fatalf("%s reads the event log as %d %s", caller, response.Code, response.Body.String())
	}
	return decodeBody[[]privateLogRow](t, response)
}

// requireLogHoldsNothingOf fails naming each event of project in caller's event log.
func requireLogHoldsNothingOf(t *testing.T, seed privateSeed, caller privateCaller, project string) {
	t.Helper()
	for _, event := range eventLogOf(t, seed, caller) {
		if event.Project == project {
			t.Errorf("%s's event log holds %s %d of %s", caller, event.Type, event.ID, project)
		}
	}
}

// TestNoStateEventOfAHiddenProjectReachesAnOpenViewer: carol marks a PRIV thread read, snoozes a
// PRIV ask and moves her read state on a PRIV issue. Each write appends a state event owned by
// PRIV - user_thread_state.updated, then user_state.updated twice - so alice, an open viewer who
// does not see PRIV, finds none of the three in her event log or on her live socket, and carol
// finds all three in both.
func TestNoStateEventOfAHiddenProjectReachesAnOpenViewer(t *testing.T) {
	threads := seedThreads(t)
	key := threads.issue[seedPrivate]
	url := liveURLOf(t, threads.handler)
	sockets := map[privateCaller]*socketClient{}
	for _, caller := range []privateCaller{callerAlice, callerCarol} {
		sockets[caller] = threads.dialLive(t, url, threads.liveHeader(t, caller))
		subscribeEvents(t, sockets[caller], "feed", nil)
	}
	now := time.Now().UTC()
	threads.mustRequest(t, callerCarol, http.MethodPut, "/api/v1/me/threads/"+threads.comment[seedPrivate]+"/read",
		map[string]any{"read_through": now.Format(time.RFC3339Nano)}, http.StatusNoContent)
	threads.mustRequest(t, callerCarol, http.MethodPut, "/api/v1/me/asks/"+threads.ask[seedPrivate]+"/snooze",
		map[string]any{"snoozed_until": now.Add(time.Hour).Format(time.RFC3339Nano)}, http.StatusOK)
	threads.mustRequest(t, callerCarol, http.MethodPut, "/api/v1/me/issues/"+key+"/state",
		map[string]any{"pinned": true}, http.StatusOK)

	stateTypes := []string{"user_thread_state.updated", "user_state.updated"}
	countCarols := func(events []privateLogRow) map[string]int {
		counted := map[string]int{}
		for _, event := range events {
			var payload struct {
				Login string `json:"login"`
			}
			if !slices.Contains(stateTypes, event.Type) || event.Project != seedPrivate ||
				json.Unmarshal(event.Payload, &payload) != nil || payload.Login != seedCarol {
				continue
			}
			counted[event.Type]++
		}
		return counted
	}
	// Each caller's socket is read through a pin of its own, written after carol's three writes, on
	// a session that acts in no project and the listener lists: s1 acts in PRIV, which alice does
	// not see.
	threads.markAgentState(t, seedAlice, "s-marker")
	threads.markAgentState(t, seedCarol, "s-marker")
	socketEvents := func(caller privateCaller, login string) []privateLogRow {
		var logged []privateLogRow
		for _, event := range liveEventsThrough(t, sockets[caller], "feed", agentStateOf(login)) {
			logged = append(logged, privateLogRow{ID: event.ID, Type: event.Type, Project: event.Project, Payload: event.Payload})
		}
		return logged
	}
	for _, read := range []struct {
		what   string
		events func(caller privateCaller, login string) []privateLogRow
	}{
		{"event log", func(caller privateCaller, _ string) []privateLogRow {
			return eventLogOf(t, threads.privateSeed, caller)
		}},
		{"live socket", socketEvents},
	} {
		if got := countCarols(read.events(callerCarol, seedCarol)); got["user_thread_state.updated"] != 1 || got["user_state.updated"] != 2 {
			t.Fatalf("carol's %s holds her PRIV state events %v; want user_thread_state.updated once and user_state.updated twice", read.what, got)
		}
		got := countCarols(read.events(callerAlice, seedAlice))
		for _, eventType := range stateTypes {
			if got[eventType] != 0 {
				t.Errorf("alice's %s holds %d of carol's %s of PRIV, a project alice does not see", read.what, got[eventType], eventType)
			}
		}
	}
}
