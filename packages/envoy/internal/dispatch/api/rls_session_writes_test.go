package api

// rls_session_writes_test.go holds the write rows of plan.md (INFRA-6168, "The outbox and write
// rows", W1-W6): a write that gives a session work of an issue, or sends it an item, is refused
// 403 SESSION_OUTSIDE_PROJECT when the session does not see the issue's project, and 503
// ENVOY_UNAVAILABLE when the verdict depends on a listener list that has expired and the
// listener does not answer the refresh. The outbox's rows of the same test name are package
// outbox's (rls_withhold_test.go).

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// The sessions the write rows name.
const (
	// writesOutside is a session tied to rlsMember, who sees rlsOpen and not rlsHush: it sees
	// rlsOpen alone, and holds the role reviewer.
	writesOutside = "ses_writes_outside"
	// writesListed is a session tied to nobody whose listener list names rlsHush: it sees rlsHush
	// while the stored listener answer that lists it has not expired.
	writesListed = "ses_writes_listed"
	// writesRole is the role writesOutside holds.
	writesRole = "reviewer"
)

// writesSessions is the stand-in listener's record of the two sessions, live and taking btw.
const writesSessions = `[` +
	`{"session_id":"` + writesOutside + `","title":"Bob's agent","capabilities":["btw","steer"],"roles":["` + writesRole + `"]},` +
	`{"session_id":"` + writesListed + `","title":"An agent of nobody's","capabilities":["btw","steer"],"projects":["` + rlsHush + `"]}]`

// writesListener is a stand-in Envoy listener: it serves the two sessions' records, their list
// (GET /v1/sessions, 503 while silent, as a listener that does not answer), the role's holder,
// and records every send it is asked to make.
type writesListener struct {
	mu     sync.Mutex
	silent bool
	sends  []string
}

func newWritesListener(t *testing.T) (*httptest.Server, *writesListener) {
	t.Helper()
	state := &writesListener{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/v1/registry/"):
			writeSessionRecord(t, w, writesSessions, strings.TrimPrefix(r.URL.Path, "/v1/registry/"))
		case r.Method == http.MethodGet && r.URL.Path == "/v1/sessions":
			state.mu.Lock()
			silent := state.silent
			state.mu.Unlock()
			if silent {
				w.WriteHeader(http.StatusServiceUnavailable)
				return
			}
			_, _ = w.Write([]byte(writesSessions))
		case r.Method == http.MethodGet && r.URL.Path == "/v1/roles/"+writesRole:
			_, _ = w.Write([]byte(`{"role":"` + writesRole + `","holder":"` + writesOutside + `","title":"Bob's agent","capabilities":["btw","steer"]}`))
		case r.Method == http.MethodPost && r.URL.Path == "/v1/messages/send":
			var request struct {
				TargetSession string `json:"target_session"`
			}
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Errorf("decode a send: %v", err)
			}
			state.mu.Lock()
			state.sends = append(state.sends, request.TargetSession)
			state.mu.Unlock()
			_, _ = w.Write([]byte(`{"event_id":"envelope-` + request.TargetSession + `","recipient":"` + request.TargetSession + `"}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(server.Close)
	return server, state
}

// silence makes the listener's session list answer 503 from now on.
func (l *writesListener) silence() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.silent = true
}

// sent is how many sends the listener was asked to make.
func (l *writesListener) sent() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.sends)
}

// writesWorld is the write rows' server, its listener, and each project's first issue.
type writesWorld struct {
	*rlsServer
	listener *writesListener
	issue    map[string]string
}

// newWritesWorld builds the write rows' world: with public, the deployment with no private
// project and no limited member (rlsOwner and rlsMember, rlsOpen), and otherwise the people of
// rls_world_test.go with rlsOpen and rlsHush, rlsHush private. writesOutside is tied to rlsMember.
func newWritesWorld(t *testing.T, public bool) *writesWorld {
	t.Helper()
	database := storetest.Open(t)
	projects := []string{rlsOpen}
	if public {
		seedPeople(t, database, rlsOwner, rlsMember)
	} else {
		seedRLSPeople(t, database)
		projects = append(projects, rlsHush)
	}
	if _, err := database.Pool.Exec(context.Background(), `insert into session_owners (session_id, login) values ($1, $2)`, writesOutside, rlsMember); err != nil {
		t.Fatalf("tie %s to %s: %v", writesOutside, rlsMember, err)
	}
	server, listener := newWritesListener(t)
	world := &writesWorld{rlsServer: newRLSServer(t, database, rlsServerConfig{envoyURL: server.URL}), listener: listener, issue: map[string]string{}}
	for _, project := range projects {
		world.asOK(t, rlsOwner, http.MethodPost, "/api/v1/projects", map[string]any{"key": project, "name": "The " + project + " project"}, http.StatusCreated)
		world.issue[project] = createdIssue(t, "file "+project+"'s issue", world.as(t, rlsOwner, http.MethodPost, "/api/v1/issues", map[string]any{
			"project": project, "title": "The " + rlsWord[project] + " issue",
		})).Key
	}
	if !public {
		makeRLSPrivate(t, database, world.deps.Events, rlsHush)
	}
	world.awaitWork(t)
	return world
}

// text is the one text a statement answers.
func (w *writesWorld) text(t *testing.T, sql string, args ...any) string {
	t.Helper()
	var text string
	if err := w.database.Pool.QueryRow(context.Background(), sql, args...).Scan(&text); err != nil {
		t.Fatalf("read %q: %v", sql, err)
	}
	return text
}

// ask is a new ask of rlsOwner's on project's issue.
func (w *writesWorld) ask(t *testing.T, project string) string {
	t.Helper()
	return decodeBody[model.Ask](t, w.asOK(t, rlsOwner, http.MethodPost, "/api/v1/issues/"+w.issue[project]+"/asks", map[string]any{
		"question": "Who works this?", "options": []map[string]string{{"label": "Me"}, {"label": "Not me"}},
	}, http.StatusCreated)).ID
}

// ownerToken is a personal token of rlsOwner's holding the person scope whose list names
// projects (agent_tokens.projects): an agent of hers, which names in a body the session it writes
// as.
func (w *writesWorld) ownerToken(t *testing.T, projects ...string) string {
	t.Helper()
	token := personalTokenOwnedBy(t, w.database, rlsOwner, model.ScopePerson)
	if projects == nil {
		projects = []string{}
	}
	if _, err := w.database.Pool.Exec(context.Background(), `update agent_tokens set projects = $2 where id = $1::uuid`,
		personalTokenID(t, w.database, token), projects); err != nil {
		t.Fatalf("write the token's list %v: %v", projects, err)
	}
	return token
}

// sessionWrite is one write that gives a session work of project's issue or sends it an item.
type sessionWrite struct {
	what string
	// send makes the write on project's issue, naming session.
	send func(t *testing.T, project, session string) *httptest.ResponseRecorder
	// ok is the status the write answers when the session sees the project.
	ok int
	// written reads whether the write left the session's work or item on project's issue.
	written func(t *testing.T, project, session string) bool
}

// sessionWrites is every write the rows make, on w.
func (w *writesWorld) sessionWrites(t *testing.T) []sessionWrite {
	t.Helper()
	asks := map[string]string{}
	for project := range w.issue {
		asks[project] = w.ask(t, project)
	}
	// The claim's writer is an agent of rlsOwner's whose list names every private project, so it
	// sees the issue it claims on either side of the session's sight.
	var listed []string
	if _, private := w.issue[rlsHush]; private {
		listed = append(listed, rlsHush)
	}
	token := w.ownerToken(t, listed...)
	mention := func(target string) func(t *testing.T, project, session string) *httptest.ResponseRecorder {
		return func(t *testing.T, project, _ string) *httptest.ResponseRecorder {
			return w.as(t, rlsOwner, http.MethodPost, "/api/v1/issues/"+w.issue[project]+"/comments", map[string]any{
				"body": "Over to " + target + ".", "mentions": []map[string]string{{"target": target}}, "delivery": "btw",
			})
		}
	}
	mentioned := func(target string) func(t *testing.T, project, _ string) bool {
		return func(t *testing.T, project, _ string) bool {
			return w.text(t, `select exists (select 1 from comments where issue_key = $1 and body = $2)::text`, w.issue[project], "Over to "+target+".") == "true"
		}
	}
	return []sessionWrite{
		{
			what: "a session route on the issue", ok: http.StatusOK,
			send: func(t *testing.T, project, session string) *httptest.ResponseRecorder {
				return w.as(t, rlsOwner, http.MethodPatch, "/api/v1/issues/"+w.issue[project], map[string]any{"route": "session:" + session})
			},
			written: func(t *testing.T, project, session string) bool {
				return w.text(t, `select (coalesce(route, '') = $2)::text from issues where key = $1`, w.issue[project], "session:"+session) == "true"
			},
		},
		{
			what: "a follower added on the issue's ask", ok: http.StatusNoContent,
			send: func(t *testing.T, project, session string) *httptest.ResponseRecorder {
				return w.as(t, rlsOwner, http.MethodPut, "/api/v1/asks/"+asks[project]+"/followers/"+session, nil)
			},
			written: func(t *testing.T, project, session string) bool {
				return w.text(t, `select exists (select 1 from ask_followers where ask_id = $1 and session_id = $2)::text`, asks[project], session) == "true"
			},
		},
		{
			what: "a claim made in the session's name", ok: http.StatusOK,
			send: func(t *testing.T, project, session string) *httptest.ResponseRecorder {
				return w.bearer(t, token, http.MethodPost, "/api/v1/issues/"+w.issue[project]+"/claim", map[string]any{
					"actor": map[string]any{"kind": "session", "id": session},
				})
			},
			written: func(t *testing.T, project, session string) bool {
				return w.text(t, `select (coalesce(claimed_by ->> 'id', '') = $2)::text from issues where key = $1`, w.issue[project], session) == "true"
			},
		},
		{
			what: "a comment's mention of the session", ok: http.StatusCreated,
			send: func(t *testing.T, project, session string) *httptest.ResponseRecorder {
				return mention("session:"+session)(t, project, session)
			},
			written: func(t *testing.T, project, session string) bool {
				return mentioned("session:"+session)(t, project, session)
			},
		},
		{
			what: "a comment's mention of a role the session holds", ok: http.StatusCreated,
			send: mention("role:" + writesRole), written: mentioned("role:" + writesRole),
		},
		{
			what: "a targeted message to the session", ok: http.StatusCreated,
			send: func(t *testing.T, project, session string) *httptest.ResponseRecorder {
				return w.as(t, rlsOwner, http.MethodPost, "/api/v1/issues/"+w.issue[project]+"/messages", map[string]any{
					"body": "A note for " + session + ".", "target": "session:" + session, "delivery": "btw",
				})
			},
			written: func(t *testing.T, project, session string) bool {
				return w.text(t, `select exists (select 1 from messages where issue_key = $1 and target = $2)::text`, w.issue[project], "session:"+session) == "true"
			},
		},
	}
}

// requireRefusedOutside fails t unless write, on rlsHush's issue and naming writesOutside, is
// refused 403 SESSION_OUTSIDE_PROJECT with nothing written and nothing sent, and the same write on
// rlsOpen's issue, which the session sees, is made: the control in the same run.
func (w *writesWorld) requireRefusedOutside(t *testing.T, write sessionWrite) {
	t.Helper()
	before := w.listener.sent()
	refused := write.send(t, rlsHush, writesOutside)
	w.awaitWork(t)
	if code, _ := answerCode(refused); refused.Code != http.StatusForbidden || code != "SESSION_OUTSIDE_PROJECT" {
		t.Errorf("%s on %s, which %s does not see: %d %s, want 403 SESSION_OUTSIDE_PROJECT",
			write.what, w.issue[rlsHush], writesOutside, refused.Code, cutBody(refused.Body.String()))
	}
	if write.written(t, rlsHush, writesOutside) {
		t.Errorf("%s on %s was refused and left it written", write.what, w.issue[rlsHush])
	}
	if sent := w.listener.sent(); sent != before {
		t.Errorf("%s on %s was refused and the listener was asked for %d sends", write.what, w.issue[rlsHush], sent-before)
	}
	accepted := write.send(t, rlsOpen, writesOutside)
	if accepted.Code != write.ok || !write.written(t, rlsOpen, writesOutside) {
		t.Errorf("%s on %s, which %s sees: %d %s, want %d and the write made", write.what, w.issue[rlsOpen], writesOutside,
			accepted.Code, cutBody(accepted.Body.String()), write.ok)
	}
	w.awaitWork(t)
}

// writeNamed is the one write of writes whose what starts with prefix.
func writeNamed(t *testing.T, writes []sessionWrite, prefix string) sessionWrite {
	t.Helper()
	for _, write := range writes {
		if strings.HasPrefix(write.what, prefix) {
			return write
		}
	}
	t.Fatalf("no write is %q", prefix)
	return sessionWrite{}
}

// TestOutboxAndRoutesWithholdFromHiddenSessions (plan R4, D11 "403 SESSION_OUTSIDE_PROJECT"): a
// write that routes an issue to a session, adds it to an ask's followers, claims in its name,
// mentions it or a role it holds, or sends it a targeted message, is refused when the session does
// not see the issue's project, and 503 ENVOY_UNAVAILABLE when that depends on a listener list the
// listener does not renew; with no private project and no limited member every such write is made
// as on main (R5, W6). The outbox's rows of this name are package outbox's.
func TestOutboxAndRoutesWithholdFromHiddenSessions(t *testing.T) {
	t.Parallel()
	t.Run("W1_route_to_a_session_outside_priv_is_refused", func(t *testing.T) {
		w := newWritesWorld(t, false)
		w.requireRefusedOutside(t, writeNamed(t, w.sessionWrites(t), "a session route"))
	})
	t.Run("W2_ask_follower_outside_priv_is_refused", func(t *testing.T) {
		w := newWritesWorld(t, false)
		w.requireRefusedOutside(t, writeNamed(t, w.sessionWrites(t), "a follower"))
	})
	t.Run("W3_claim_in_the_name_of_a_session_outside_priv_is_refused", func(t *testing.T) {
		w := newWritesWorld(t, false)
		w.requireRefusedOutside(t, writeNamed(t, w.sessionWrites(t), "a claim"))
	})
	t.Run("W4_mention_of_the_session_or_its_role_is_refused", func(t *testing.T) {
		w := newWritesWorld(t, false)
		writes := w.sessionWrites(t)
		w.requireRefusedOutside(t, writeNamed(t, writes, "a comment's mention of the session"))
		w.requireRefusedOutside(t, writeNamed(t, writes, "a comment's mention of a role"))
	})
	t.Run("W5_targeted_message_is_refused_and_an_unrenewed_list_is_503", func(t *testing.T) {
		w := newWritesWorld(t, false)
		message := writeNamed(t, w.sessionWrites(t), "a targeted message")
		w.requireRefusedOutside(t, message)
		// writesListed sees rlsHush through its listener list alone: while the stored listener
		// answer is fresh the message is made (the control), and once that answer has expired and
		// the listener does not answer the refresh, the verdict cannot be read and the message is
		// refused 503 ENVOY_UNAVAILABLE with nothing written.
		w.asOK(t, rlsOwner, http.MethodGet, "/api/v1/agents", nil, http.StatusOK)
		if listed := message.send(t, rlsHush, writesListed); listed.Code != message.ok {
			t.Fatalf("a message to %s, whose fresh listener list names %s: %d %s, want %d: the row proves nothing",
				writesListed, rlsHush, listed.Code, cutBody(listed.Body.String()), message.ok)
		}
		w.awaitWork(t)
		messages := func() string {
			return w.text(t, `select count(*)::text from messages where issue_key = $1 and target = $2`, w.issue[rlsHush], "session:"+writesListed)
		}
		before := messages()
		if _, err := w.database.Pool.Exec(context.Background(), `update route_roster set expires_at = now() - interval '1 minute'`); err != nil {
			t.Fatalf("expire the stored listener answer: %v", err)
		}
		w.listener.silence()
		unrenewed := message.send(t, rlsHush, writesListed)
		if code, _ := answerCode(unrenewed); unrenewed.Code != http.StatusServiceUnavailable || code != "ENVOY_UNAVAILABLE" {
			t.Errorf("a message to %s with its listener list expired and the listener silent: %d %s, want 503 ENVOY_UNAVAILABLE",
				writesListed, unrenewed.Code, cutBody(unrenewed.Body.String()))
		}
		if after := messages(); after != before {
			t.Errorf("the message refused for an unrenewed list was written: %s messages to %s, %s before it", after, writesListed, before)
		}
	})
	t.Run("W6_all_public_every_write_as_on_main", func(t *testing.T) {
		w := newWritesWorld(t, true)
		for _, write := range w.sessionWrites(t) {
			made := write.send(t, rlsOpen, writesOutside)
			if made.Code != write.ok || !write.written(t, rlsOpen, writesOutside) {
				t.Errorf("%s with no private project: %d %s, want %d and the write made", write.what, made.Code, cutBody(made.Body.String()), write.ok)
			}
			w.awaitWork(t)
		}
	})
}
