package api

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/brokertoken"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/dispatch/text"
)

// The sessions knownSessionsFixture writes a record of, one kind of record each, and the two the
// stored registry copy lists. None of the ended sessions is in that copy: each is a session the
// live registry no longer lists, whose conversation Dispatch still holds.
const (
	knownTitled       = "s-known-titled"
	knownUntitled     = "s-known-untitled"
	knownActivity     = "s-ended-activity"
	knownConversation = "s-ended-conversation"
	knownDelivery     = "s-ended-delivery"
	knownMention      = "s-ended-mention"
	knownFile         = "s-ended-file"
	knownFollower     = "s-ended-follower"
	// knownPersonOnly is a person's login spelled like a session id: an event that person wrote
	// is no record of a session.
	knownPersonOnly = "s-person-only"
	knownNever      = "s-never-held"
)

// knownSessionsFixture writes one record of every kind GET /api/v1/agents/known counts, each
// naming a session of its own, and a registry copy listing two sessions, one titled and one not.
func knownSessionsFixture(t *testing.T, database *store.Store) {
	t.Helper()
	ctx := context.Background()
	for _, write := range []struct {
		what      string
		statement string
		args      []any
	}{
		{"the project", `insert into projects (key, name, default_assignee) values ('KNOWN', 'Known', 'alice')`, nil},
		{"the issue", `insert into issues (key, project_key, number, title, status, created_by, rank)
			values ('KNOWN-1', 'KNOWN', 1, 'Known sessions', 'todo', '{"kind":"user","id":"alice"}', 'U')`, nil},
		{"the registry copy", `insert into route_roster (answered, sessions, sent_at, expires_at)
			values (true, $1::jsonb, now(), now() + interval '5 seconds')
			on conflict (id) do update
			set answered = excluded.answered, sessions = excluded.sessions,
			    sent_at = excluded.sent_at, expires_at = excluded.expires_at`,
			[]any{fmt.Sprintf(`[{"session_id":%q,"title":"Planner","state":"live"},{"session_id":%q,"title":"","state":"unresponsive"}]`,
				knownTitled, knownUntitled)}},
		{"a session's event and a person's", `insert into events (issue_key, seq, type, actor, payload, notify) values
			('KNOWN-1', 1, 'message.created', jsonb_build_object('kind', 'session', 'id', $1::text), '{}', false),
			('KNOWN-1', 2, 'message.created', jsonb_build_object('kind', 'user', 'id', $2::text), '{}', false)`,
			[]any{knownActivity, knownPersonOnly}},
		{"a direct message to a session", `insert into messages (issue_key, author, body, target)
			values (null, '{"kind":"user","id":"alice"}', 'Are you still there?', 'session:' || $1::text)`,
			[]any{knownConversation}},
		{"a delivery of a role's message to a session", `with message as (
				insert into messages (issue_key, author, body, target)
				values ('KNOWN-1', '{"kind":"user","id":"alice"}', 'For the planner.', 'role:planner')
				returning id
			)
			insert into message_deliveries (message_id, attempt, delivery, session_id, state, requested_by)
			select id, 1, 'aside', $1, 'failed', '{"kind":"user","id":"alice"}' from message`,
			[]any{knownDelivery}},
		{"a mention delivered to a session", `with comment as (
				insert into comments (issue_key, author, body)
				values ('KNOWN-1', '{"kind":"user","id":"alice"}', 'Over to the reviewer.')
				returning id
			)
			insert into comment_deliveries (comment_id, target, attempt, delivery, session_id, state)
			select id, 'role:reviewer', 1, 'aside', $1, 'failed' from comment`,
			[]any{knownMention}},
		{"a file of a session's conversation", `insert into artifacts (session_id, slug, name, kind, created_by)
			values ($1, 'photo-png', 'photo.png', 'image', '{"kind":"user","id":"alice"}')`,
			[]any{knownFile}},
		{"an ask a session follows", `with ask as (
				insert into asks (issue_key, author, question, state)
				values ('KNOWN-1', '{"kind":"user","id":"alice"}', 'Which option?', 'open')
				returning id
			)
			insert into ask_followers (ask_id, session_id) select id, $1 from ask`,
			[]any{knownFollower}},
	} {
		if _, err := database.Pool.Exec(ctx, write.statement, write.args...); err != nil {
			t.Fatalf("write %s: %v", write.what, err)
		}
	}
}

// knownSessionsPath is the route's path asking about ids, one id parameter each.
func knownSessionsPath(ids ...string) string {
	return "/api/v1/agents/known?" + url.Values{"id": ids}.Encode()
}

// askKnownSessions asks the route about ids as alice in her browser and answers what it said,
// failing on anything but 200.
func askKnownSessions(t *testing.T, handler http.Handler, ids ...string) []knownSession {
	t.Helper()
	response := dispatchRequest(t, handler, http.MethodGet, knownSessionsPath(ids...), nil, "alice")
	if response.Code != http.StatusOK {
		t.Fatalf("ask about %v: status=%d body=%s", ids, response.Code, response.Body.String())
	}
	return decodeBody[knownSessions](t, response).Sessions
}

// One request carries every id a page holds, and the answer lists exactly those Dispatch holds a
// record of, in the order asked and each once: the registry copy's two sessions with the title
// it holds (null where it holds none), and each ended session by the one record it left, while an
// id no agent ever held and a person's login are absent.
func TestKnownSessionsAnswersTheAskedSessionsWithARecordInTheOrderAsked(t *testing.T) {
	handler, database := newTestHandlerWithStore(t)
	knownSessionsFixture(t, database)

	got := askKnownSessions(t, handler,
		knownNever, knownFollower, knownTitled, knownActivity, knownUntitled, knownPersonOnly,
		knownConversation, knownDelivery, knownMention, knownFile, knownTitled, knownFollower)
	want := []knownSession{
		{SessionID: knownFollower},
		{SessionID: knownTitled, Title: new("Planner")},
		{SessionID: knownActivity},
		{SessionID: knownUntitled},
		{SessionID: knownConversation},
		{SessionID: knownDelivery},
		{SessionID: knownMention},
		{SessionID: knownFile},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("known sessions = %s, want %s", knownSessionsText(got), knownSessionsText(want))
	}
}

// Each kind of record makes a session known on its own, so an ended session that left only that
// record is answered, and an id with none is answered an empty list rather than refused.
func TestKnownSessionsCountsEachKindOfRecordOnItsOwn(t *testing.T) {
	handler, database := newTestHandlerWithStore(t)
	knownSessionsFixture(t, database)

	for record, id := range map[string]string{
		"the registry copy, with a title":      knownTitled,
		"the registry copy, with none":         knownUntitled,
		"an event the session wrote":           knownActivity,
		"a message targeted at the session":    knownConversation,
		"a message delivery to the session":    knownDelivery,
		"a comment delivery to the session":    knownMention,
		"a file of the session's conversation": knownFile,
		"an ask the session follows":           knownFollower,
	} {
		t.Run(record, func(t *testing.T) {
			got := askKnownSessions(t, handler, id)
			if len(got) != 1 || got[0].SessionID != id {
				t.Fatalf("known sessions = %s, want %s alone", knownSessionsText(got), id)
			}
			if wantTitle := id == knownTitled; (got[0].Title != nil) != wantTitle {
				t.Fatalf("%s's title = %v, want one only where the registry copy holds one", id, got[0].Title)
			}
		})
	}
	for what, id := range map[string]string{
		"an id no agent ever held":                 knownNever,
		"a person's login that reads as a session": knownPersonOnly,
	} {
		t.Run(what, func(t *testing.T) {
			response := dispatchRequest(t, handler, http.MethodGet, knownSessionsPath(id), nil, "alice")
			if response.Code != http.StatusOK || strings.TrimSpace(response.Body.String()) != `{"sessions":[]}` {
				t.Fatalf("ask about %s: status=%d body=%s, want 200 and no session", id, response.Code, response.Body.String())
			}
		})
	}
}

// The whole batch is one statement, however many ids it names: the identity is read on the
// untraced pool, so what is counted is the route's own work.
func TestKnownSessionsReadsTheWholeBatchInOneStatement(t *testing.T) {
	database := storetest.Open(t)
	knownSessionsFixture(t, database)
	counter := &roundTrips{}
	traced := &store.Store{Pool: storetest.Traced(t, database.Pool, counter, nil)}
	handler, _, _ := newTestServer(t, testServerOptions{store: traced, identity: headerIdentity(database)})

	ids := make([]string, 0, contracts.KnownSessionsMax)
	ids = append(ids, knownTitled, knownActivity, knownFile, knownFollower)
	for len(ids) < contracts.KnownSessionsMax {
		ids = append(ids, fmt.Sprintf("s-unheld-%d", len(ids)))
	}
	counter.take()
	ctx := context.WithValue(context.Background(), countedRead{}, true)
	request := httptest.NewRequestWithContext(ctx, http.MethodGet, knownSessionsPath(ids...), nil)
	request.Header.Set("X-Dispatch-User", "alice")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	counted := counter.take()
	if response.Code != http.StatusOK {
		t.Fatalf("ask about %d sessions: status=%d body=%s", len(ids), response.Code, response.Body.String())
	}
	if got := decodeBody[knownSessions](t, response).Sessions; len(got) != 4 {
		t.Fatalf("known sessions = %s, want the four with a record", knownSessionsText(got))
	}
	if len(counted) != 1 {
		t.Fatalf("asking about %d sessions made %d round trips, want one:\n%s",
			len(ids), len(counted), strings.Join(roundTripLines(counted), "\n"))
	}
}

// Every parameter is held to its rule before anything is read, each refusal 400 INVALID_QUERY
// naming the parameter: a value that is no session id, one byte longer than any reference in
// text names, no id at all, one more than the bound, and a parameter the route does not take.
// Each bound itself is admitted, and so is the whole API's session parameter.
func TestKnownSessionsRefusesAQueryOutsideItsRule(t *testing.T) {
	handler, database := newTestHandlerWithStore(t)
	knownSessionsFixture(t, database)

	overBound := make([]string, contracts.KnownSessionsMax+1)
	for index := range overBound {
		overBound[index] = fmt.Sprintf("s-%d", index)
	}
	longest := strings.Repeat("s", knownSessionIDMax)
	for name, refusal := range map[string]struct {
		path  string
		names string
	}{
		"a value holding whitespace":           {knownSessionsPath("s one"), "id"},
		"a value holding a slash":              {knownSessionsPath("agent/s1"), "id"},
		"an empty value":                       {knownSessionsPath(""), "id"},
		"a value one byte past the longest id": {knownSessionsPath(knownTitled, longest+"s"), "id"},
		"no id at all":                         {"/api/v1/agents/known", "id"},
		"only the acting session":              {"/api/v1/agents/known?session=" + knownTitled, "id"},
		"one id more than the bound":           {knownSessionsPath(overBound...), "id"},
		"a parameter the route does not take":  {knownSessionsPath(knownTitled) + "&limit=5", "limit"},
		"the parameter misspelled":             {"/api/v1/agents/known?ids=" + knownTitled, "ids"},
	} {
		t.Run(name, func(t *testing.T) {
			response := dispatchRequest(t, handler, http.MethodGet, refusal.path, nil, "alice")
			body := decodeBody[refusalAnswer](t, response)
			if response.Code != http.StatusBadRequest || body.Code != "INVALID_QUERY" {
				t.Fatalf("status=%d body=%.400s, want 400 INVALID_QUERY", response.Code, response.Body.String())
			}
			if !strings.HasPrefix(body.Error, refusal.names+" ") {
				t.Fatalf("refusal %.400q does not lead with the parameter %s", body.Error, refusal.names)
			}
		})
	}
	if got := askKnownSessions(t, handler, overBound[:contracts.KnownSessionsMax]...); len(got) != 0 {
		t.Fatalf("known sessions = %s, want none of the unheld ids", knownSessionsText(got))
	}
	if got := askKnownSessions(t, handler, longest); len(got) != 0 {
		t.Fatalf("known sessions = %.400s, want none for an unheld id at the longest", knownSessionsText(got))
	}
	acting := dispatchRequest(t, handler, http.MethodGet, knownSessionsPath(knownTitled)+"&session=s-acting", nil, "alice")
	if acting.Code != http.StatusOK || len(decodeBody[knownSessions](t, acting).Sessions) != 1 {
		t.Fatalf("ask with the acting session named: status=%d body=%s, want 200 and the one session", acting.Code, acting.Body.String())
	}
}

// The longest id the route answers for is the longest a dispatch://agent/<session id> reference
// in text names: at it the text is a reference to that session, and one byte past it is none.
func TestKnownSessionsLongestIDIsTheLongestAReferenceNames(t *testing.T) {
	const server = "https://dispatch.test"
	longest := strings.Repeat("s", knownSessionIDMax)
	got := text.Extract("dispatch://agent/"+longest, server)
	if len(got) != 1 || got[0].Kind != "session" || got[0].ID != longest {
		t.Fatalf("the reference to a session id of %d bytes cites %.200v, want that session", len(longest), got)
	}
	if got := text.Extract("dispatch://agent/"+longest+"s", server); len(got) != 0 {
		t.Fatalf("the reference to a session id of %d bytes cites %.200v, want nothing", len(longest)+1, got)
	}
}

// The route answers only what its caller could already learn, so every kind of caller GET
// /api/v1/agents/{session_id}/messages tells apart is answered by it exactly as that read
// answers: no credential, a person's browser, a person's own token with and without the person
// scope, the deployment's shared token, and the broker's tokens for a person, for one project, for
// a service with no person, and for a machine login. Both read `session` as every person route
// does, so the broker's token for a person, which proved a session, naming another there is
// refused both, 403 ACTOR_SESSION_MISMATCH.
func TestKnownSessionsAdmitsExactlyTheCallersTheConversationReadAdmits(t *testing.T) {
	rig := newBrokerTokenRig(t)
	knownSessionsFixture(t, rig.database)
	caller := newBrokerCaller(t)

	broker := func(name string, says brokertoken.Introspection) string {
		token := "a-broker-token-" + name
		rig.broker.Says(token, says)
		return token
	}
	personScoped := liveNamedToken(caller.jkt)
	personScoped.ProjectScope, personScoped.Project = brokertoken.ProjectScopePerson, ""
	brokerTokens := map[string]string{
		"a broker token for a person":        broker("person", personScoped),
		"a broker token for one project":     broker("project", liveNamedToken(caller.jkt)),
		"a broker token for a service":       broker("service", serviceOnlyToken(caller.jkt)),
		"a broker token for a machine login": broker("machine", machineLoginToken(caller.jkt)),
	}
	bearers := map[string]string{
		"the deployment's shared token":             "agent-token",
		"a person's token holding the person scope": personalTokenHolding(t, rig.database, model.ScopePerson),
		"a person's token holding no scope":         personalTokenHolding(t, rig.database),
	}

	known := knownSessionsPath(knownActivity)
	conversation := "/api/v1/agents/" + knownActivity + "/messages"
	type answer struct {
		status int
		code   string
	}
	answerOf := func(response *httptest.ResponseRecorder) answer {
		if response.Code == http.StatusOK {
			return answer{status: http.StatusOK}
		}
		return answer{status: response.Code, code: decodeBody[refusalAnswer](t, response).Code}
	}
	plain := func(path string, set func(*http.Request)) answer {
		request := httptest.NewRequest(http.MethodGet, path, nil)
		set(request)
		return answerOf(rig.serve(request))
	}

	answers := map[string][2]answer{}
	answers["no credential"] = [2]answer{
		plain(conversation, func(*http.Request) {}), plain(known, func(*http.Request) {}),
	}
	browser := func(request *http.Request) { request.Header.Set("X-Dispatch-User", "alice") }
	answers["a person's browser"] = [2]answer{plain(conversation, browser), plain(known, browser)}
	for name, token := range bearers {
		bearer := func(request *http.Request) { request.Header.Set("Authorization", "Bearer "+token) }
		answers[name] = [2]answer{plain(conversation, bearer), plain(known, bearer)}
	}
	for name, token := range brokerTokens {
		answers[name] = [2]answer{
			answerOf(rig.serve(caller.presenting(t, http.MethodGet, conversation, token, nil))),
			answerOf(rig.serve(caller.presenting(t, http.MethodGet, known, token, nil))),
		}
	}
	mismatched := "a broker token for a person, naming another session"
	answers[mismatched] = [2]answer{
		answerOf(rig.serve(caller.presenting(t, http.MethodGet, conversation+"?session=s-another",
			brokerTokens["a broker token for a person"], nil))),
		answerOf(rig.serve(caller.presenting(t, http.MethodGet, known+"&session=s-another",
			brokerTokens["a broker token for a person"], nil))),
	}
	if refused := (answer{status: http.StatusForbidden, code: "ACTOR_SESSION_MISMATCH"}); answers[mismatched][1] != refused {
		t.Errorf("%s: the known-sessions read answers %+v, want %+v", mismatched, answers[mismatched][1], refused)
	}

	admitted := 0
	for name, pair := range answers {
		if pair[0] != pair[1] {
			t.Errorf("%s: the conversation read answers %+v and the known-sessions read %+v, want the same", name, pair[0], pair[1])
		}
		if pair[0].status == http.StatusOK {
			admitted++
		}
	}
	// The comparison proves nothing if every caller is refused both reads, or admitted to both.
	if admitted == 0 || admitted == len(answers) {
		t.Fatalf("the conversation read admits %d of %d callers, want some admitted and some refused: %+v", admitted, len(answers), answers)
	}
}

// knownSessionsText renders an answer for a failure message, a null title as -.
func knownSessionsText(sessions []knownSession) string {
	parts := make([]string, len(sessions))
	for index, session := range sessions {
		title := "-"
		if session.Title != nil {
			title = *session.Title
		}
		parts[index] = session.SessionID + "(" + title + ")"
	}
	return "[" + strings.Join(parts, " ") + "]"
}
